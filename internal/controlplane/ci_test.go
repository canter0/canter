package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

func ciTestServer(s *Store, cfg CIConfig) *HTTPServer {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "http://127.0.0.1:1"
	}
	return NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", CI: cfg}).(*HTTPServer)
}

func ciAuthedRequest(t *testing.T, method, path, token, body string) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	req.Header.Set("Origin", "http://canter.test")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestCIProductCatalogDisabledByDefault(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	products, err := s.WorkspaceProducts(ctx, c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	var ci *productEntry
	for i := range products {
		if products[i].ID == "ci" {
			ci = &products[i]
		}
	}
	if ci == nil {
		t.Fatal("ci product missing from catalog")
	}
	if ci.Enabled {
		t.Fatal("ci must be disabled by default")
	}
	if ci.Name != "CI" || !strings.Contains(ci.Description, "free first hour") {
		t.Fatalf("unexpected ci catalog entry: %+v", ci)
	}
	enabled, err := s.CIProductEnabled(ctx, c.WorkspaceID)
	if err != nil || enabled {
		t.Fatalf("ci enabled before opt-in: %v %v", enabled, err)
	}
	if err = s.EnableWorkspaceProduct(ctx, c.WorkspaceID, "ci", sdk.ActorRef{Kind: "human", ID: c.AccountID}); err != nil {
		t.Fatal(err)
	}
	enabled, err = s.CIProductEnabled(ctx, c.WorkspaceID)
	if err != nil || !enabled {
		t.Fatalf("ci not enabled independently of sandboxes: %v %v", enabled, err)
	}
}

func TestCISettingsDefaultsOwnerOnlyAndValidation(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	h := ciTestServer(s, CIConfig{})
	settings, err := s.CISettings(ctx, c.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.PaidOverrunEnabled || settings.MonthlySpendLimitCents != 1000 || !settings.CacheEnabled || settings.RetainFailedMinutes != 0 || settings.DefaultSize != "linux-x64-4" {
		t.Fatalf("bad defaults: %+v", settings)
	}
	req := ciAuthedRequest(t, http.MethodPatch, "/v1/workspaces/"+c.WorkspaceID+"/ci/settings", token, `{"paidOverrunEnabled":false,"monthlySpendLimitCents":2500,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-2"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner settings patch: %d %s", rec.Code, rec.Body.String())
	}
	settings, _ = s.CISettings(ctx, c.WorkspaceID)
	if settings.MonthlySpendLimitCents != 2500 || settings.DefaultSize != "linux-x64-2" {
		t.Fatalf("settings not saved: %+v", settings)
	}
	for _, body := range []string{
		`{"paidOverrunEnabled":true,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"}`,
		`{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":5,"defaultSize":"linux-x64-4"}`,
		`{"paidOverrunEnabled":false,"monthlySpendLimitCents":0,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"}`,
		`{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"}`,
		`{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"huge"}`,
		`{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4","extra":1}`,
	} {
		req = ciAuthedRequest(t, http.MethodPatch, "/v1/workspaces/"+c.WorkspaceID+"/ci/settings", token, body)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("invalid settings accepted: %s -> %d", body, rec.Code)
		}
	}
	other, otherWorkspace, otherToken, err := s.Signup(ctx, "other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	req = ciAuthedRequest(t, http.MethodPatch, "/v1/workspaces/"+c.WorkspaceID+"/ci/settings", otherToken, `{"paidOverrunEnabled":false,"monthlySpendLimitCents":1000,"cacheEnabled":true,"retainFailedMinutes":0,"defaultSize":"linux-x64-4"}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("cross-workspace settings patch succeeded")
	}
	_ = other
	_ = otherWorkspace
}

func TestCIOverviewUnconfiguredPool(t *testing.T) {
	s, c, token := operatorFixture(t)
	h := ciTestServer(s, CIConfig{})
	req := ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci", token, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["enabled"] != false {
		t.Fatal("overview enabled should be false before opt-in")
	}
	pool := out["pool"].(map[string]any)
	if pool["ready"] != false || pool["status"] != "not_configured" {
		t.Fatalf("pool must be unconfigured: %v", pool)
	}
	pricingMap := out["pricing"].(map[string]any)
	if pricingMap["monthlyMinuteLimit"] != nil {
		t.Fatal("monthlyMinuteLimit must be null")
	}
	if pricingMap["freeSecondsPerRun"] != float64(3600) {
		t.Fatalf("free seconds: %v", pricingMap["freeSecondsPerRun"])
	}
	for _, key := range []string{"installations", "jobs", "features"} {
		if out[key] == nil {
			t.Fatalf("%s must serialize as array", key)
		}
	}
	features := out["features"].([]any)
	for _, f := range features {
		fm := f.(map[string]any)
		if fm["status"] == "available" && strings.Contains(fmt.Sprint(fm["description"]), "fastest") {
			t.Fatal("unmeasured speed claim in feature description")
		}
	}
}

func ciWebhookRequest(t *testing.T, secret, delivery, event, body string, sign bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ci/github/webhook", bytes.NewReader([]byte(body)))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Event", event)
	if sign {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return req
}

func TestCIWebhookSignatureAndDedup(t *testing.T) {
	s, _, _ := operatorFixture(t)
	h := ciTestServer(s, CIConfig{WebhookSecret: []byte("whsec-whsec-whsec-whsec-whsec-0123")})
	body := `{"action":"ping"}`
	for _, tc := range []struct {
		name   string
		secret string
		sign   bool
		want   int
	}{
		{"missing signature", "whsec", false, http.StatusUnauthorized},
		{"bad signature", "whsec", true, http.StatusUnauthorized},
	} {
		req := ciWebhookRequest(t, tc.secret, "d-"+tc.name, "ping", body, tc.sign)
		if tc.name == "bad signature" {
			req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: got %d want %d", tc.name, rec.Code, tc.want)
		}
	}
	d1 := fmt.Sprintf("d-1-%d", time.Now().UnixNano())
	req := ciWebhookRequest(t, "whsec-whsec-whsec-whsec-whsec-0123", d1, "ping", body, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid webhook: %d %s", rec.Code, rec.Body.String())
	}
	var first map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first["duplicate"] == true {
		t.Fatalf("first delivery must not be a duplicate: %s", rec.Body.String())
	}
	req = ciWebhookRequest(t, "whsec-whsec-whsec-whsec-whsec-0123", d1, "ping", body, true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: %d", rec.Code)
	}
	var dup map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &dup)
	if dup["duplicate"] != true {
		t.Fatalf("replay not idempotent: %s", rec.Body.String())
	}
	req = ciWebhookRequest(t, "whsec-whsec-whsec-whsec-whsec-0123", d1, "ping", `{"action":"pong"}`, true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("changed-body replay: got %d want 409", rec.Code)
	}
	big := strings.Repeat("x", (1<<20)+10)
	req = ciWebhookRequest(t, "whsec-whsec-whsec-whsec-whsec-0123", "d-big", "ping", big, true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: got %d", rec.Code)
	}
}

func TestCIResolveJobLabels(t *testing.T) {
	size, blocked, has := ciResolveJobLabels([]string{"canter"})
	if !has || blocked != "" || size != "linux-x64-4" {
		t.Fatalf("alias: %q %q %v", size, blocked, has)
	}
	size, blocked, has = ciResolveJobLabels([]string{"canter-2vcpu-ubuntu-2404", "ubuntu-latest"})
	if !has || blocked != "" || size != "linux-x64-2" {
		t.Fatalf("label: %q %q %v", size, blocked, has)
	}
	_, blocked, has = ciResolveJobLabels([]string{"canter", "canter-2vcpu-ubuntu-2404"})
	if !has || blocked == "" {
		t.Fatalf("conflict not blocked: %q %v", blocked, has)
	}
	_, blocked, has = ciResolveJobLabels([]string{"canter-8vcpu-ubuntu"})
	if !has || blocked == "" {
		t.Fatalf("unsupported not blocked: %q %v", blocked, has)
	}
	_, _, has = ciResolveJobLabels([]string{"ubuntu-latest", "self-hosted"})
	if has {
		t.Fatal("non-canter labels must be ignored")
	}
}

func TestCIConnectStateLifecycle(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	raw := strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(raw))
	if err := s.CreateCIConnectState(ctx, c.WorkspaceID, c.AccountID, hex.EncodeToString(sum[:]), time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ws, acct, err := s.ConsumeCIConnectState(ctx, hex.EncodeToString(sum[:]))
	if err != nil || ws != c.WorkspaceID || acct != c.AccountID {
		t.Fatalf("consume: %v %q %q", err, ws, acct)
	}
	if _, _, err = s.ConsumeCIConnectState(ctx, hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("state replay must fail")
	}
	if err = s.CreateCIConnectState(ctx, c.WorkspaceID, c.AccountID, hex.EncodeToString(sum[:]), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ConsumeCIConnectState(ctx, hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("expired state must fail")
	}
}

func TestCIMigrationReapply(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate reapply: %v", err)
	}
	var versions int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version='077_ci_repository_history'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("077 migration row must appear exactly once, got %d", versions)
	}
	var nonUnique bool
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)=1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname='ci_repositories_name' AND indexdef NOT LIKE '%UNIQUE%'`).Scan(&nonUnique); err != nil {
		t.Fatal(err)
	}
	if !nonUnique {
		t.Fatal("077 must leave ci_repositories_name non-unique")
	}
}
