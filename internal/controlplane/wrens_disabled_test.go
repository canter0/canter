package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These cases share the retained private-schema super fixture; nothing here
// truncates or drops any shared database.

func wrenToolNames(tools []mcpTool) map[string]bool {
	names := map[string]bool{}
	for _, t := range tools {
		names[t.Name] = true
	}
	return names
}

func TestWrensDisabledCatalogAndProductGuards(t *testing.T) {
	h, actor := superSandboxFixture(t)
	ctx := context.Background()
	s := h.service.Store

	s.SetWrensEnabled(false)
	products, err := s.WorkspaceProducts(ctx, "ws")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]productEntry{}
	for _, p := range products {
		ids[p.ID] = p
	}
	if _, ok := ids["wrens"]; ok {
		t.Fatalf("wrens advertised while disabled: %+v", products)
	}
	if _, ok := ids["sandboxes"]; !ok {
		t.Fatalf("sandboxes missing while wrens disabled: %+v", products)
	}
	if _, ok := ids["ci"]; !ok {
		t.Fatalf("ci missing while wrens disabled: %+v", products)
	}
	if err = s.EnableWorkspaceProduct(ctx, "ws", "wrens", actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("enabling disabled wrens: %v", err)
	}
	if err = s.DisableWorkspaceProduct(ctx, "ws", "wrens"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabling disabled wrens: %v", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.requireWorkspaceProductForUseTx(ctx, tx, "ws", "wrens", "owner", actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("implicit enable of disabled wrens: %v", err)
	}
	if enabled, err := s.productEnabled(ctx, s.pool, "ws", "wrens"); err != nil || enabled {
		t.Fatalf("wrens reported enabled: %v %v", enabled, err)
	}
	// The compute-only flag must not disturb sandbox products.
	v := superSandboxCreate(t, h, actor, superSandboxInput("wrens-off", 1))
	if v.ID == "" {
		t.Fatal("super sandbox creation failed while wrens disabled")
	}

	s.SetWrensEnabled(true)
	products, err = s.WorkspaceProducts(ctx, "ws")
	if err != nil {
		t.Fatal(err)
	}
	var wrens *productEntry
	for i := range products {
		if products[i].ID == "wrens" {
			wrens = &products[i]
		}
	}
	if wrens == nil || !wrens.Enabled || !wrens.AlwaysOn {
		t.Fatalf("enabled catalog missing always-on wrens: %+v", products)
	}
	if err = s.EnableWorkspaceProduct(ctx, "ws", "wrens", actor); err != nil {
		t.Fatalf("enabling always-on wrens: %v", err)
	}
}

func wrensFixtureSession(t *testing.T, h *HTTPServer, token string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.service.Store.pool.Exec(ctx, `UPDATE accounts SET email_verified_at=now() WHERE id='owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Store.pool.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,created_at,last_seen_at,expires_at,auth_version,authenticated_at,mfa_verified_at) VALUES('hs_wrens_gate','owner',$1,now(),now(),now()+interval '1 hour',0,now(),now())`, secretHash(token)); err != nil {
		t.Fatal(err)
	}
}

func TestWrensDisabledRouteGuard(t *testing.T) {
	h, _ := superSandboxFixture(t)
	s := h.service.Store
	s.SetWrensEnabled(false)
	token := "wrens-gate-test-token"
	wrensFixtureSession(t, h, token)

	request := func(segment string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/"+segment, nil)
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.workspaces(w, r, []string{"ws", segment})
		return w
	}

	for _, segment := range []string{"wrens", "rooms", "computers", "wren-read", "wren-search", "wren-media", "wren-templates"} {
		if w := request(segment); w.Code != http.StatusNotFound {
			t.Fatalf("disabled wrens route %q: %d %s", segment, w.Code, w.Body.String())
		}
	}
	for _, segment := range []string{"sandboxes", "super-sandboxes", "conversations", "members", "wren-members", "ci", "app-accounts", "wren-extensions", "customizations", "plugins"} {
		if w := request(segment); w.Code != http.StatusOK {
			t.Fatalf("non-wrens route %q failed: %d %s", segment, w.Code, w.Body.String())
		}
	}

	// The same paths must reach their handlers once the flag is on, proving
	// the 404s above came from the gate rather than missing routes.
	s.SetWrensEnabled(true)
	for _, segment := range []string{"wrens", "rooms", "computers"} {
		if w := request(segment); w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "not found") {
			t.Fatalf("enabled wrens route %q still gated: %d %s", segment, w.Code, w.Body.String())
		}
	}
}

func TestSandboxCIReadinessWithoutWrens(t *testing.T) {
	h, actor := superSandboxFixture(t)
	s := h.service.Store
	s.SetWrensEnabled(false)
	const token = "sandbox-ci-readiness-test-token"
	wrensFixtureSession(t, h, token)
	ctx := context.Background()
	read := func(segment string, value any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/"+segment, nil)
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.workspaces(w, r, []string{"ws", segment})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", segment, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), value); err != nil {
			t.Fatalf("%s: %v", segment, err)
		}
	}
	for _, productEnabled := range []bool{false, true} {
		if productEnabled {
			for _, product := range []string{"sandboxes", "ci"} {
				if err := s.EnableWorkspaceProduct(ctx, "ws", product, actor); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, executionEnabled := range []bool{false, true} {
			h.config.Sandboxes.Enabled = executionEnabled
			var inventory struct {
				Enabled         bool      `json:"enabled"`
				PreviewsEnabled bool      `json:"previewsEnabled"`
				Sandboxes       []Sandbox `json:"sandboxes"`
				Sizes           []struct {
					ID string `json:"id"`
				} `json:"sizes"`
				FreeUsage struct {
					LimitSeconds int64 `json:"limitSeconds"`
				} `json:"freeUsage"`
			}
			read("sandboxes", &inventory)
			if inventory.Enabled != executionEnabled || inventory.PreviewsEnabled || len(inventory.Sandboxes) != 2 || len(inventory.Sizes) != len(sandboxSizes) || inventory.FreeUsage.LimitSeconds != h.config.Sandboxes.freeSecondsPerMonth() {
				t.Fatalf("incorrect sandbox readiness with product=%v execution=%v: %+v", productEnabled, executionEnabled, inventory)
			}
			for _, sandbox := range inventory.Sandboxes {
				if sandbox.WorkspaceID != "ws" || sandbox.ID == "sbx_other" {
					t.Fatalf("cross-organization sandbox in inventory: %+v", sandbox)
				}
			}
			var batches struct {
				Enabled        bool           `json:"enabled"`
				SuperSandboxes []SuperSandbox `json:"superSandboxes"`
			}
			read("super-sandboxes", &batches)
			if batches.Enabled != executionEnabled || batches.SuperSandboxes == nil || len(batches.SuperSandboxes) != 0 {
				t.Fatalf("incorrect batch readiness: %+v", batches)
			}
			var overview CIOverview
			read("ci", &overview)
			if overview.Enabled != productEnabled || overview.Pool.Ready || overview.Pool.Status != "not_configured" || overview.Pool.Message == "" {
				t.Fatalf("unconfigured CI advertised readiness: %+v", overview)
			}
			if overview.Installations == nil || len(overview.Installations) != 0 || overview.Jobs == nil || len(overview.Jobs) != 0 || len(overview.Pricing.Sizes) == 0 || overview.Pricing.FreeSecondsPerRun <= 0 || overview.Settings.PaidOverrunEnabled {
				t.Fatalf("incomplete or unsafe CI overview: %+v", overview)
			}
			features := map[string]string{}
			for _, feature := range overview.Features {
				features[feature.ID] = feature.Status
			}
			for _, feature := range []string{"runner-pool", "github-app", "job-ingest", "job-cancel", "paid-overrun", "cache"} {
				if features[feature] != "not_configured" {
					t.Fatalf("unconfigured CI feature %q advertised as %q", feature, features[feature])
				}
			}
			if features["agent-inspect"] != "available" || features["job-history"] != "available" {
				t.Fatalf("read-only CI features unavailable: %v", features)
			}
			var products, work int
			if err := s.pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM workspace_products WHERE workspace_id='ws'),
				(SELECT count(*) FROM ci_runners)+(SELECT count(*) FROM sandbox_jobs)+(SELECT count(*) FROM super_sandboxes)+(SELECT count(*) FROM ci_settings)`).Scan(&products, &work); err != nil {
				t.Fatal(err)
			}
			expectedProducts := 0
			if productEnabled {
				expectedProducts = 2
			}
			if products != expectedProducts || work != 0 {
				t.Fatalf("inspection activated products or work: products=%d want=%d work=%d", products, expectedProducts, work)
			}
		}
	}
}

func TestWrensDisabledHarnessTools(t *testing.T) {
	h, _ := superSandboxFixture(t)
	s := h.service.Store
	runtime := &OperatorRuntime{Server: h}

	s.SetWrensEnabled(false)
	names := wrenToolNames(runtime.tools())
	for _, excluded := range []string{"canter_delegate_job", "canter_inspect_job", "canter_computer_status", "canter_ask_room_wren"} {
		if names[excluded] {
			t.Fatalf("wrens tool %s advertised while disabled", excluded)
		}
	}
	for _, kept := range []string{"canter_save_context", "canter_sandbox_list", "canter_super_sandbox_list", "canter_inspect_ci", "canter_list_products"} {
		if !names[kept] {
			t.Fatalf("non-wrens tool %s missing while disabled", kept)
		}
	}

	s.SetWrensEnabled(true)
	names = wrenToolNames(runtime.tools())
	for _, wanted := range []string{"canter_delegate_job", "canter_computer_status"} {
		if !names[wanted] {
			t.Fatalf("wrens tool %s missing while enabled", wanted)
		}
	}
}
