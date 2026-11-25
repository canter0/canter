package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func apiKeyTestAccount(t *testing.T, store *Store, email string) (Account, Workspace, string) {
	t.Helper()
	account, workspace, session, err := store.Signup(context.Background(), email, "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return account, workspace, session
}

// apiKeyHTTP issues a request against the test server. Any bearer token is
// sent verbatim so tests can cover key, agent and malformed Authorization
// values alongside the browser cookie.
func apiKeyHTTP(t *testing.T, h *HTTPServer, method, path, session, bearer, origin string, data any) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if data != nil {
		raw, _ := json.Marshal(data)
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, "http://canter.test/v1"+path, body)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/json")
	if session != "" {
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, r)
	return out
}

// recentSession marks the signup session freshly authenticated so step-up
// checks pass, mirroring a completed Confirm identity flow.
func recentSession(t *testing.T, s *Store, session string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `UPDATE human_sessions SET authenticated_at=now() WHERE token_hash=$1`, secretHash(session)); err != nil {
		t.Fatal(err)
	}
}

func securityEventCount(t *testing.T, s *Store, account, action string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM account_security_events WHERE account_id=$1 AND action=$2`, account, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type apiKeyCreateResult struct {
	Key    PersonalAPIKey `json:"key"`
	Secret string         `json:"secret"`
}

func createTestAPIKey(t *testing.T, h *HTTPServer, s *Store, session, name string, days int) apiKeyCreateResult {
	t.Helper()
	recentSession(t, s, session)
	got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": name, "expiresInDays": days})
	if got.Code != http.StatusCreated {
		t.Fatalf("create key %d %s", got.Code, got.Body)
	}
	var out apiKeyCreateResult
	if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
		t.Fatalf("create body %s %v", got.Body, err)
	}
	return out
}

func TestPersonalAPIKeyCreateListAndResolve(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	account, workspace, session := apiKeyTestAccount(t, s, "keys-owner@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)

	created := createTestAPIKey(t, h, s, session, "laptop cli", 0)
	if !strings.HasPrefix(created.Secret, "canter_pk_") || len(created.Secret) != len("canter_pk_")+43 {
		t.Fatalf("unexpected secret shape %q", created.Secret)
	}
	if created.Key.Prefix != created.Secret[:18] {
		t.Fatalf("prefix %q does not lead the secret", created.Key.Prefix)
	}
	// Roughly the 90-day default; the exact instant is not contractual.
	if delta := time.Until(created.Key.ExpiresAt); delta < 89*24*time.Hour || delta > 91*24*time.Hour {
		t.Fatalf("default expiry %v out of range", delta)
	}

	// Only the prefix and hash exist anywhere: the plaintext secret is never
	// persisted and is not echoed by list.
	var storedHash []byte
	var storedPrefix string
	if err := s.pool.QueryRow(ctx, `SELECT token_hash,prefix FROM personal_api_keys WHERE id=$1`, created.Key.ID).Scan(&storedHash, &storedPrefix); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(created.Secret))
	if !bytes.Equal(storedHash, sum[:]) || bytes.Contains(storedHash, []byte(created.Secret[10:20])) {
		t.Fatalf("stored hash does not match SHA-256 of secret")
	}
	if storedPrefix != created.Key.Prefix {
		t.Fatalf("stored prefix %q", storedPrefix)
	}

	list := apiKeyHTTP(t, h, http.MethodGet, "/me/api-keys", session, "", "http://canter.test", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list %d %s", list.Code, list.Body)
	}
	if bytes.Contains(list.Body.Bytes(), []byte(created.Secret)) {
		t.Fatal("list leaked the plaintext secret")
	}
	var listed struct {
		Keys []PersonalAPIKey `json:"keys"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed.Keys) != 1 {
		t.Fatalf("list body %s %v", list.Body, err)
	}
	if listed.Keys[0].ID != created.Key.ID || listed.Keys[0].Name != "laptop cli" || listed.Keys[0].Prefix != created.Key.Prefix {
		t.Fatalf("list metadata %+v", listed.Keys[0])
	}
	if securityEventCount(t, s, account.ID, "API key created") != 1 {
		t.Fatal("missing creation security event")
	}

	// The bearer key immediately resolves the account identity and its
	// memberships, with the caller's own permissions.
	me := apiKeyHTTP(t, h, http.MethodGet, "/me", "", created.Secret, "http://canter.test", nil)
	if me.Code != http.StatusOK {
		t.Fatalf("key /me %d %s", me.Code, me.Body)
	}
	var identity struct {
		Account    Account `json:"account"`
		Workspaces []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil || identity.Account.ID != account.ID || len(identity.Workspaces) != 1 || identity.Workspaces[0].ID != workspace.ID {
		t.Fatalf("identity %s %v", me.Body, err)
	}
	read := apiKeyHTTP(t, h, http.MethodGet, "/workspaces/"+workspace.ID+"/bootstrap", "", created.Secret, "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("key workspace read %d %s", read.Code, read.Body)
	}
	write := apiKeyHTTP(t, h, http.MethodPatch, "/workspaces/"+workspace.ID, "", created.Secret, "", map[string]string{"name": "key-renamed"})
	if write.Code != http.StatusOK {
		t.Fatalf("key workspace write %d %s", write.Code, write.Body)
	}

	// A stranger's key cannot reach the workspace at all.
	stranger, _, strangerSession := apiKeyTestAccount(t, s, "keys-stranger@example.com")
	_ = stranger
	strangerKey := createTestAPIKey(t, h, s, strangerSession, "stranger key", 30)
	denied := apiKeyHTTP(t, h, http.MethodGet, "/workspaces/"+workspace.ID+"/bootstrap", "", strangerKey.Secret, "", nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("outsider workspace read %d %s", denied.Code, denied.Body)
	}
}

func TestPersonalAPIKeyViewerWriteForbiddenAndMembershipInvalidation(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, workspace, _ := apiKeyTestAccount(t, s, "keys-viewer-owner@example.com")
	viewer, _, viewerSession := apiKeyTestAccount(t, s, "keys-viewer@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewer.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	key := createTestAPIKey(t, h, s, viewerSession, "viewer key", 30)

	read := apiKeyHTTP(t, h, http.MethodGet, "/workspaces/"+workspace.ID+"/bootstrap", "", key.Secret, "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("viewer read %d %s", read.Code, read.Body)
	}
	// allowWorkspace(write) denies viewers before the body is consumed.
	upload := apiKeyHTTP(t, h, http.MethodPost, "/workspaces/"+workspace.ID+"/artifacts", "", key.Secret, "", nil)
	if upload.Code != http.StatusForbidden {
		t.Fatalf("viewer write %d %s", upload.Code, upload.Body)
	}
	rename := apiKeyHTTP(t, h, http.MethodPatch, "/workspaces/"+workspace.ID, "", key.Secret, "", map[string]string{"name": "nope"})
	if rename.Code != http.StatusForbidden {
		t.Fatalf("viewer rename %d %s", rename.Code, rename.Body)
	}

	// Removing the membership cuts workspace access immediately, while the
	// account identity still resolves.
	if _, err := s.pool.Exec(ctx, `DELETE FROM memberships WHERE account_id=$1 AND workspace_id=$2`, viewer.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	read = apiKeyHTTP(t, h, http.MethodGet, "/workspaces/"+workspace.ID+"/bootstrap", "", key.Secret, "", nil)
	if read.Code != http.StatusForbidden {
		t.Fatalf("removed-member read %d %s", read.Code, read.Body)
	}
	me := apiKeyHTTP(t, h, http.MethodGet, "/me", "", key.Secret, "", nil)
	if me.Code != http.StatusOK {
		t.Fatalf("removed-member /me %d %s", me.Code, me.Body)
	}
}

func TestPersonalAPIKeyRevocationIsIdempotentWithSingleEvent(t *testing.T) {
	s := integrationStore(t)
	account, _, session := apiKeyTestAccount(t, s, "keys-revoke@example.com")
	_, _, _ = apiKeyTestAccount(t, s, "keys-revoke-other@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	key := createTestAPIKey(t, h, s, session, "revoke me", 30)

	first := apiKeyHTTP(t, h, http.MethodDelete, "/me/api-keys/"+key.Key.ID, session, "", "http://canter.test", nil)
	if first.Code != http.StatusNoContent {
		t.Fatalf("revoke %d %s", first.Code, first.Body)
	}
	second := apiKeyHTTP(t, h, http.MethodDelete, "/me/api-keys/"+key.Key.ID, session, "", "http://canter.test", nil)
	if second.Code != http.StatusNoContent {
		t.Fatalf("idempotent revoke %d %s", second.Code, second.Body)
	}
	if n := securityEventCount(t, s, account.ID, "API key revoked"); n != 1 {
		t.Fatalf("revocation events %d", n)
	}
	me := apiKeyHTTP(t, h, http.MethodGet, "/me", "", key.Secret, "", nil)
	if me.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key /me %d", me.Code)
	}

	// A foreign account sees the same not-found as a missing key.
	stranger, _, strangerSession := apiKeyTestAccount(t, s, "keys-revoke-stranger@example.com")
	_ = stranger
	foreign := apiKeyHTTP(t, h, http.MethodDelete, "/me/api-keys/"+key.Key.ID, strangerSession, "", "http://canter.test", nil)
	missing := apiKeyHTTP(t, h, http.MethodDelete, "/me/api-keys/apk_missing", strangerSession, "", "http://canter.test", nil)
	if foreign.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("foreign %d missing %d", foreign.Code, missing.Code)
	}
}

func TestPersonalAPIKeyExpiryAuthVersionAndAccountState(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	account, _, session := apiKeyTestAccount(t, s, "keys-state@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)

	expired := createTestAPIKey(t, h, s, session, "expired", 30)
	if _, err := s.pool.Exec(ctx, `UPDATE personal_api_keys SET created_at=$1,expires_at=$2 WHERE id=$3`, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour), expired.Key.ID); err != nil {
		t.Fatal(err)
	}
	if got := apiKeyHTTP(t, h, http.MethodGet, "/me", "", expired.Secret, "", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("expired key %d", got.Code)
	}

	versioned := createTestAPIKey(t, h, s, session, "versioned", 30)
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET auth_version=auth_version+1 WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}
	if got := apiKeyHTTP(t, h, http.MethodGet, "/me", "", versioned.Secret, "", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("rotated-auth key %d", got.Code)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET auth_version=auth_version-1 WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}
	if got := apiKeyHTTP(t, h, http.MethodGet, "/me", "", versioned.Secret, "", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("disabled-account key %d", got.Code)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=NULL,email_verified_at=NULL WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}
	if got := apiKeyHTTP(t, h, http.MethodGet, "/me", "", versioned.Secret, "", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("unverified-account key %d", got.Code)
	}
}

func TestPersonalAPIKeyManagementIsCookieOnly(t *testing.T) {
	s := integrationStore(t)
	account, _, session := apiKeyTestAccount(t, s, "keys-mgmt@example.com")
	_ = account
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	key := createTestAPIKey(t, h, s, session, "manage me", 30)

	// A valid personal key cannot mint, list, or revoke keys -- even alongside
	// a valid browser cookie.
	for _, bearer := range []string{key.Secret, "agent-token-looks-real"} {
		if got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, bearer, "http://canter.test", map[string]any{"name": "escalation"}); got.Code != http.StatusUnauthorized {
			t.Fatalf("bearer+cookie create %d %s", got.Code, got.Body)
		}
		if got := apiKeyHTTP(t, h, http.MethodGet, "/me/api-keys", session, bearer, "http://canter.test", nil); got.Code != http.StatusUnauthorized {
			t.Fatalf("bearer+cookie list %d %s", got.Code, got.Body)
		}
		if got := apiKeyHTTP(t, h, http.MethodDelete, "/me/api-keys/"+key.Key.ID, session, bearer, "http://canter.test", nil); got.Code != http.StatusUnauthorized {
			t.Fatalf("bearer+cookie delete %d %s", got.Code, got.Body)
		}
		if got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", "", bearer, "http://canter.test", map[string]any{"name": "escalation"}); got.Code != http.StatusUnauthorized {
			t.Fatalf("bearer-only create %d %s", got.Code, got.Body)
		}
	}
	// Malformed Authorization values must not fall back to the cookie.
	r := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/me/api-keys", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
	basic := httptest.NewRecorder()
	h.ServeHTTP(basic, r)
	if basic.Code != http.StatusUnauthorized {
		t.Fatalf("basic auth fallback %d %s", basic.Code, basic.Body)
	}
	// The key cannot touch account security or device approvals either.
	if got := apiKeyHTTP(t, h, http.MethodGet, "/auth/security", session, key.Secret, "http://canter.test", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("key on auth security %d %s", got.Code, got.Body)
	}
	if got := apiKeyHTTP(t, h, http.MethodPost, "/device/authorizations/ABCD-EFGH/approve", session, key.Secret, "http://canter.test", map[string]string{"workspaceId": "wrk_x"}); got.Code != http.StatusUnauthorized {
		t.Fatalf("key device approve %d %s", got.Code, got.Body)
	}
	// A forged Origin is rejected before reaching the handler.
	if got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "https://evil.example", map[string]any{"name": "origin trick"}); got.Code != http.StatusForbidden {
		t.Fatalf("foreign origin %d %s", got.Code, got.Body)
	}
	if got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test.evil.example", map[string]any{"name": "suffix trick"}); got.Code != http.StatusForbidden {
		t.Fatalf("suffix origin %d %s", got.Code, got.Body)
	}
}

func TestPersonalAPIKeyCreateRequiresRecentAuthAndValidInput(t *testing.T) {
	s := integrationStore(t)
	account, _, session := apiKeyTestAccount(t, s, "keys-recent@example.com")
	_ = account
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)

	// A session whose last step-up is outside the 5-minute window must be
	// refused with 428 before anything is inserted.
	if _, err := s.pool.Exec(context.Background(), `UPDATE human_sessions SET authenticated_at=now()-interval '10 minutes' WHERE token_hash=$1`, secretHash(session)); err != nil {
		t.Fatal(err)
	}
	stale := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": "stale", "expiresInDays": 30})
	if stale.Code != http.StatusPreconditionRequired {
		t.Fatalf("stale-auth create %d %s", stale.Code, stale.Body)
	}
	var count int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM personal_api_keys WHERE account_id=$1`, account.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale create left %d keys %v", count, err)
	}

	// Validation failures are 400s, never 500s.
	recentSession(t, s, session)
	for _, body := range []map[string]any{
		{"name": "   "},
		{"name": "has\nnewline"},
		{"name": strings.Repeat("x", 81)},
		{"name": "ok", "expiresInDays": -1},
		{"name": "ok", "expiresInDays": 366},
	} {
		got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", body)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("invalid body %v -> %d %s", body, got.Code, got.Body)
		}
	}
	if got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": "ok", "expiresInDays": 365}); got.Code != http.StatusCreated {
		t.Fatalf("boundary expiry %d %s", got.Code, got.Body)
	}
}

func TestPersonalAPIKeyActiveCap(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	account, _, session := apiKeyTestAccount(t, s, "keys-cap@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)

	// Revoked and expired rows do not count toward the cap.
	for i := 0; i < personalAPIKeyMaxActive; i++ {
		expiry := "now() + interval '1 day'"
		if i == personalAPIKeyMaxActive-1 {
			expiry = "now() - interval '1 day'"
		}
		if _, err := s.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO personal_api_keys(id,account_id,name,token_hash,prefix,auth_version,created_at,expires_at,revoked_at) VALUES($1,$2,$3,$4,$5,(SELECT auth_version FROM accounts WHERE id=$2),now()-interval '2 days',%s,CASE WHEN $6 THEN now() ELSE NULL END)`, expiry), fmt.Sprintf("apk_cap_%02d", i), account.ID, "cap", secretHash(fmt.Sprintf("cap-token-%d", i)), "cap", i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	got := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": "over"})
	// Half the rows are revoked and one expired: 24 remain active, so creation
	// must succeed until the true cap is reached.
	if got.Code != http.StatusCreated {
		t.Fatalf("under-cap create %d %s", got.Code, got.Body)
	}
	// Fill the remaining slots directly.
	var active int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM personal_api_keys WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>now()`, account.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	for i := active; i < personalAPIKeyMaxActive; i++ {
		if _, err := s.pool.Exec(ctx, `INSERT INTO personal_api_keys(id,account_id,name,token_hash,prefix,auth_version,created_at,expires_at) VALUES($1,$2,'cap',$3,'cap',(SELECT auth_version FROM accounts WHERE id=$2),now(),now()+interval '1 day')`, fmt.Sprintf("apk_fill_%02d", i), account.ID, secretHash(fmt.Sprintf("fill-token-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	over := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": "over"})
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("cap overflow %d %s", over.Code, over.Body)
	}
}

func TestPersonalAPIKeyStoreNeverLogsSecretOnFailure(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, _, session := apiKeyTestAccount(t, s, "keys-quiet@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	key := createTestAPIKey(t, h, s, session, "quiet", 30)

	// A failed resolve and a failed management call surface generic errors
	// without the token value.
	if _, err := s.ResolvePersonalAPIKey(ctx, key.Secret[:len(key.Secret)-1]+"x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("corrupted token %v", err)
	}
	if _, err := s.ResolvePersonalAPIKey(ctx, "canter_pk_"+strings.Repeat("a", 43)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown token %v", err)
	}
	if _, err := s.ResolvePersonalAPIKey(ctx, key.Secret+"\n"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("control-suffixed token %v", err)
	}
	bad := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": "x", "expiresInDays": 9999})
	if bad.Code != http.StatusBadRequest || strings.Contains(bad.Body.String(), key.Secret) {
		t.Fatalf("error leaked token %d %s", bad.Code, bad.Body)
	}
}

func TestPersonalAPIKeyListRetainsOldActiveKey(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	account, _, session := apiKeyTestAccount(t, s, "keys-history@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)

	// An old still-live key must stay discoverable and revocable even when
	// more than a hundred newer revoked rows crowd the history.
	oldID := "apk_old_active"
	if _, err := s.pool.Exec(ctx, `INSERT INTO personal_api_keys(id,account_id,name,token_hash,prefix,auth_version,created_at,expires_at) VALUES($1,$2,'old active',$3,'canter_pk_oldActive',(SELECT auth_version FROM accounts WHERE id=$2),now()-interval '40 days',now()+interval '30 days')`, oldID, account.ID, secretHash("canter_pk_"+strings.Repeat("o", 43))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < personalAPIKeyListLimit+1; i++ {
		if _, err := s.pool.Exec(ctx, `INSERT INTO personal_api_keys(id,account_id,name,token_hash,prefix,auth_version,created_at,expires_at,revoked_at) VALUES($1,$2,'revoked history',$3,'hist',(SELECT auth_version FROM accounts WHERE id=$2),now()-interval '1 day'+(make_interval(secs => $4)),now()+interval '30 days',now())`, fmt.Sprintf("apk_hist_%03d", i), account.ID, secretHash(fmt.Sprintf("hist-token-%d", i)), i); err != nil {
			t.Fatal(err)
		}
	}

	got := apiKeyHTTP(t, h, http.MethodGet, "/me/api-keys", session, "", "http://canter.test", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("list %d %s", got.Code, got.Body)
	}
	var listed struct {
		Keys []PersonalAPIKey `json:"keys"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list body %v", err)
	}
	if len(listed.Keys) != personalAPIKeyListLimit {
		t.Fatalf("list length %d", len(listed.Keys))
	}
	if listed.Keys[0].ID != oldID {
		t.Fatalf("old active key not listed first: %+v", listed.Keys[0])
	}
	del := apiKeyHTTP(t, h, http.MethodDelete, "/me/api-keys/"+oldID, session, "", "http://canter.test", nil)
	if del.Code != http.StatusNoContent {
		t.Fatalf("old key revoke %d %s", del.Code, del.Body)
	}
}

func TestPersonalAPIKeySecurityChangeRevokesKeys(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	account, _, session := apiKeyTestAccount(t, s, "keys-epoch@example.com")
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	real := createTestAPIKey(t, h, s, session, "epoch key", 30)

	// A full cap of active rows must not survive a credential change, or the
	// account could never mint keys again.
	for i := 0; i < personalAPIKeyMaxActive; i++ {
		if _, err := s.pool.Exec(ctx, `INSERT INTO personal_api_keys(id,account_id,name,token_hash,prefix,auth_version,created_at,expires_at) VALUES($1,$2,'epoch seed',$3,'seed',(SELECT auth_version FROM accounts WHERE id=$2),now(),now()+interval '1 day')`, fmt.Sprintf("apk_epoch_%02d", i), account.ID, secretHash(fmt.Sprintf("epoch-token-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	var sessionID string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM human_sessions WHERE token_hash=$1`, secretHash(session)).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.invalidateAuthTx(ctx, tx, account.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var active int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM personal_api_keys WHERE account_id=$1 AND revoked_at IS NULL`, account.ID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active keys after security change %d %v", active, err)
	}
	listed := apiKeyHTTP(t, h, http.MethodGet, "/me/api-keys", session, "", "http://canter.test", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list %d %s", listed.Code, listed.Body)
	}
	var history struct {
		Keys []PersonalAPIKey `json:"keys"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &history); err != nil {
		t.Fatalf("list body %v", err)
	}
	for _, k := range history.Keys {
		if k.RevokedAt == nil {
			t.Fatalf("key %s still listed live after security change", k.ID)
		}
	}
	if got := apiKeyHTTP(t, h, http.MethodGet, "/me", "", real.Secret, "", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("pre-change key %d", got.Code)
	}
	// The kept browser session survives and can mint a fresh key; the bulk
	// revocation emits no per-key security events.
	recentSession(t, s, session)
	fresh := apiKeyHTTP(t, h, http.MethodPost, "/me/api-keys", session, "", "http://canter.test", map[string]any{"name": "fresh"})
	if fresh.Code != http.StatusCreated {
		t.Fatalf("post-change create %d %s", fresh.Code, fresh.Body)
	}
	if n := securityEventCount(t, s, account.ID, "API key revoked"); n != 0 {
		t.Fatalf("epoch change emitted %d per-key events", n)
	}
}

func TestPersonalAPIKeyTokenShape(t *testing.T) {
	canonical := personalAPIKeyPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, personalAPIKeySecretBytes))
	if !validPersonalAPIToken(canonical) {
		t.Fatal("canonical token rejected")
	}
	for _, bad := range []string{
		canonical + "x",
		canonical[:len(canonical)-1],
		canonical[:len(personalAPIKeyPrefix)+10] + "\n" + canonical[len(personalAPIKeyPrefix)+10:],
		canonical[:len(personalAPIKeyPrefix)+10] + "\r" + canonical[len(personalAPIKeyPrefix)+10:],
		canonical + "=",
		canonical[:len(personalAPIKeyPrefix)] + strings.Repeat("!", 43),
		"canter_pk_" + strings.Repeat("a", 43) + "\x00",
		"pk_" + canonical[len(personalAPIKeyPrefix):],
	} {
		if validPersonalAPIToken(bad) {
			t.Fatalf("malformed token accepted %q", bad)
		}
	}
}
