package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func testVault(t *testing.T) *SecretVault {
	t.Helper()
	raw := fmt.Sprintf(`{"active":"test-v1","keys":{"test-v1":%q}}`, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	v, err := NewSecretVault([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestSecretVaultAuthenticatedEncryption(t *testing.T) {
	v := testVault(t)
	value := []byte("test-only-secret-123")
	binding := secretBinding("workspace-a", "secret-a", "stored")
	key, sealed, err := v.seal(value, binding)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := v.seal(value, binding)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sealed, second) || bytes.Contains(sealed, value) {
		t.Fatal("ciphertexts must be randomized and conceal plaintext")
	}
	plain, err := v.open(key, sealed, binding)
	if err != nil || !bytes.Equal(plain, value) {
		t.Fatal("roundtrip failed")
	}
	for _, bad := range [][]byte{secretBinding("workspace-b", "secret-a", "stored"), secretBinding("workspace-a", "secret-b", "stored"), secretBinding("workspace-a", "secret-a", "openrouter")} {
		if _, err = v.open(key, sealed, bad); err == nil {
			t.Fatal("ciphertext is transferable")
		}
	}
	sealed[len(sealed)-1] ^= 1
	if _, err = v.open(key, sealed, binding); err == nil {
		t.Fatal("tampering accepted")
	}
	if _, err = v.open("missing", second, binding); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err = (*SecretVault)(nil).open(key, second, binding); err == nil {
		t.Fatal("disabled vault accepted")
	}
	for _, bad := range []string{`{}`, `{"active":"a","keys":{"a":"short"}}`, `{"active":"missing","keys":{}}`} {
		if _, err = NewSecretVault([]byte(bad)); err == nil {
			t.Fatal("bad keyring accepted")
		}
	}
	// A new active key can coexist with the old decryption key during rotation.
	raw := fmt.Sprintf(`{"active":"test-v2","keys":{"test-v1":%q,"test-v2":%q}}`, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	rotated, err := NewSecretVault([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rotated.open(key, second, binding); err != nil {
		t.Fatal("old key lost during rotation")
	}
	if id, _, err := rotated.seal(value, binding); err != nil || id != "test-v2" {
		t.Fatal("writes did not use active key")
	}
}

func TestWorkspaceSecretsAuthorizationLifecycleAndProviderBinding(t *testing.T) {
	s, c, human := operatorFixture(t)
	ctx := context.Background()
	v := testVault(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: v, Operator: OperatorConfig{Model: "test-model"}}).(*HTTPServer)
	base := "/v1/workspaces/" + c.WorkspaceID + "/secrets"
	request := func(method, path, token, bearer, origin string, input any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		r := httptest.NewRequest(method, "http://canter.test"+path, bytes.NewReader(raw))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	call := func(method, path, token string, input any) *httptest.ResponseRecorder {
		return request(method, path, token, "", "http://canter.test", input)
	}
	input := map[string]any{"name": "OPENROUTER_KEY", "purpose": "openrouter", "value": "test-only-provider-secret-123", "note": "test connection"}
	if w := call("POST", base, "", input); w.Code != 401 {
		t.Fatalf("anonymous write %d", w.Code)
	}
	if w := request("POST", base, human, "", "https://evil.test", input); w.Code != 403 {
		t.Fatal("cross-origin secret write allowed")
	}
	w := call("POST", base, human, input)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var saved struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", base, human, input); w.Code != 409 {
		t.Fatal("duplicate provider accepted")
	}
	var ciphertext []byte
	if err := s.pool.QueryRow(ctx, `SELECT ciphertext FROM workspace_secrets WHERE id=$1`, saved.ID).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte(input["value"].(string))) {
		t.Fatal("value stored plainly")
	}
	w = call("GET", base, human, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), input["value"].(string)) || strings.Contains(w.Body.String(), "ciphertext") {
		t.Fatal("secret leaked in metadata")
	}
	if w = call("GET", base+"/"+saved.ID, human, nil); w.Code == 200 {
		t.Fatal("secret reveal endpoint exists")
	}
	other, _, otherToken, err := s.Signup(ctx, "secret-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if w = call("GET", base, otherToken, nil); w.Code != 403 {
		t.Fatal("cross-workspace metadata exposed")
	}
	for _, role := range []string{"viewer", "operator"} {
		if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,$3) ON CONFLICT(account_id,workspace_id) DO UPDATE SET role=$3`, other.ID, c.WorkspaceID, role); err != nil {
			t.Fatal(err)
		}
		if w = call("GET", base, otherToken, nil); w.Code != 200 {
			t.Fatal("member cannot inspect metadata")
		}
		for _, method := range []string{"PUT", "DELETE"} {
			if w = call(method, base+"/"+saved.ID, otherToken, map[string]any{"version": 1, "value": "another-test-secret"}); w.Code != 403 {
				t.Fatal("non-owner modified secret")
			}
		}
	}
	device, err := s.BeginDeviceAuthorization(ctx, "test agent", "test", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDevice(ctx, device.UserCode, c.AccountID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	pair, err := s.ExchangeDevice(ctx, device.DeviceCode, "secret-test")
	if err != nil {
		t.Fatal(err)
	}
	if w = request("GET", base, "", pair.AccessToken, "", nil); w.Code != 200 || strings.Contains(w.Body.String(), input["value"].(string)) {
		t.Fatal("agent metadata boundary failed")
	}
	if w = request("PUT", base+"/"+saved.ID, human, pair.AccessToken, "http://canter.test", map[string]any{"version": 1, "value": "agent-write-attempt"}); w.Code != 403 {
		t.Fatal("agent escalated through human cookie")
	}
	fallback := OperatorConfig{APIKey: "platform-key", BaseURL: "https://untrusted.test", Model: "test"}
	config, err := h.workspaceModelConfig(ctx, c.WorkspaceID, sdk.ActorRef{Kind: "agent", ID: pair.Installation.ID}, fallback)
	if err != nil || config.APIKey != input["value"] || config.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatal("provider was not bound to the fixed OpenRouter endpoint")
	}
	if !h.workspaceOperatorReady(ctx, c.WorkspaceID) {
		t.Fatal("workspace connection did not enable operator")
	}
	h.config.Secrets = nil
	if _, err = h.workspaceModelConfig(ctx, c.WorkspaceID, sdk.ActorRef{}, fallback); err == nil {
		t.Fatal("missing vault fell back silently")
	}
	h.config.Secrets = v
	w = call("PUT", base+"/"+saved.ID, human, map[string]any{"version": 1, "value": "test-only-rotated-secret"})
	if w.Code != 200 {
		t.Fatalf("rotation %d %s", w.Code, w.Body.String())
	}
	if w = call("PUT", base+"/"+saved.ID, human, map[string]any{"version": 1, "value": "stale-write-value"}); w.Code != 409 {
		t.Fatal("stale rotation overwritten")
	}
	config, err = h.workspaceModelConfig(ctx, c.WorkspaceID, sdk.ActorRef{Kind: "human", ID: c.AccountID}, fallback)
	if err != nil || config.APIKey != "test-only-rotated-secret" {
		t.Fatal("next use ignored rotation")
	}
	if w = call("DELETE", base+"/"+saved.ID, human, map[string]any{"version": 1}); w.Code != 409 {
		t.Fatal("stale removal accepted")
	}
	h.config.Secrets = nil // Owners can remove unusable ciphertext without the encryption key.
	if w = call("POST", base, human, input); w.Code != 503 {
		t.Fatal("disabled vault accepted a write")
	}
	if w = call("DELETE", base+"/"+saved.ID, human, map[string]any{"version": 2}); w.Code != 200 {
		t.Fatalf("remove %d", w.Code)
	}
	var removed bool
	if err = s.pool.QueryRow(ctx, `SELECT ciphertext IS NULL AND revoked_at IS NOT NULL FROM workspace_secrets WHERE id=$1`, saved.ID).Scan(&removed); err != nil || !removed {
		t.Fatal("removed ciphertext retained")
	}
	config, err = h.workspaceModelConfig(ctx, c.WorkspaceID, sdk.ActorRef{}, fallback)
	if err != nil || config.APIKey != fallback.APIKey {
		t.Fatal("removed key still used")
	}
	var audit string
	if err = s.pool.QueryRow(ctx, `SELECT string_agg(action || metadata::text,',') FROM audit_events WHERE subject=$1`, saved.ID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"secret.created", "secret.used", "secret.rotated", "secret.revoked"} {
		if !strings.Contains(audit, action) {
			t.Fatal("missing audit event", action)
		}
	}
	if strings.Contains(audit, "test-only") {
		t.Fatal("audit contains secret values")
	}
}
