package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

type appOAuthFixture struct {
	h                               *HTTPServer
	remote                          *httptest.Server
	private, room                   Conversation
	member, ownerToken, memberToken string
	challenge                       string
	exchanges, refreshes, calls     atomic.Int32
	refreshFailure                  atomic.Int32
	toolReject                      atomic.Bool
}

func newAppOAuthFixture(t *testing.T) *appOAuthFixture {
	t.Helper()
	s, private, _, room, member := knowledgeFixture(t)
	ctx := context.Background()
	var ownerEmail string
	if err := s.pool.QueryRow(ctx, `SELECT email FROM accounts WHERE id=$1`, private.AccountID).Scan(&ownerEmail); err != nil {
		t.Fatal(err)
	}
	_, _, ownerToken, err := s.Signin(ctx, ownerEmail, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	_, _, memberToken, err := s.Signin(ctx, "knowledge-member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	f := &appOAuthFixture{private: private, room: room, member: member, ownerToken: ownerToken, memberToken: memberToken}
	server := mcp.NewServer(&mcp.Implementation{Name: "Fixture account", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "list_issues", Description: "List issues", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f.calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Authenticated fixture result"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	f.remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		base := f.remote.URL
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp/readonly":
			json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp/readonly", "authorization_servers": []string{base}, "scopes_supported": []string{"read"}})
		case "/.well-known/oauth-authorization-server":
			json.NewEncoder(w).Encode(map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "registration_endpoint": base + "/register", "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"read", "write"}, "authorization_response_iss_parameter_supported": true})
		case "/register":
			var data map[string]any
			json.NewDecoder(r.Body).Decode(&data)
			if data["scope"] != "read" || data["token_endpoint_auth_method"] != "none" {
				t.Error("unexpected client permissions")
			}
			json.NewEncoder(w).Encode(map[string]any{"client_id": "fixture-client", "token_endpoint_auth_method": "none", "redirect_uris": []string{f.h.config.PublicURL + "/api/canter/auth/app-oauth/callback"}})
		case "/token":
			r.ParseForm()
			if r.Form.Get("resource") != base+"/mcp/readonly" || r.Form.Get("client_id") != "fixture-client" {
				t.Error("lost resource/client binding")
			}
			access := "fixture_access_1"
			refresh := "fixture_refresh_1"
			if r.Form.Get("grant_type") == "refresh_token" {
				f.refreshes.Add(1)
				if mode := f.refreshFailure.Load(); mode != 0 {
					if mode == 1 {
						w.WriteHeader(503)
						json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "sensitive fixture detail"})
					} else {
						w.WriteHeader(400)
						json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "sensitive fixture detail"})
					}
					return
				}
				if r.Form.Get("refresh_token") != "fixture_refresh_1" {
					t.Error("reused or wrong refresh token")
				}
				access = "fixture_access_2"
				refresh = "fixture_refresh_2"
			} else {
				f.exchanges.Add(1)
				if oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != f.challenge || r.Form.Get("redirect_uri") != f.h.config.PublicURL+"/api/canter/auth/app-oauth/callback" {
					t.Error("PKCE/callback binding lost")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "scope": "read"})
		case "/mcp/readonly":
			if f.toolReject.Load() {
				w.WriteHeader(401)
				return
			}
			auth := r.Header.Get("Authorization")
			if auth != "Bearer fixture_access_1" && auth != "Bearer fixture_access_2" {
				t.Error("missing account token")
				w.WriteHeader(401)
				return
			}
			handler.ServeHTTP(w, r)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.remote.Close)
	provider := wrenMCPProvider{Name: "Linear fixture", Endpoint: f.remote.URL + "/mcp/readonly", ResourceMetadata: f.remote.URL + "/.well-known/oauth-protected-resource/mcp/readonly", Issuer: f.remote.URL}
	f.h = NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Extensions: WrenExtensionConfig{PreviewOrigins: []string{f.remote.URL}, appProviders: map[string]wrenMCPProvider{"linear": provider}}}).(*HTTPServer)
	return f
}
func (f *appOAuthFixture) request(method, path, token string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://canter.test"+path, bytes.NewReader(raw))
	r.Header.Set("Origin", "http://canter.test")
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func (f *appOAuthFixture) start(t *testing.T) (string, []*http.Cookie) {
	t.Helper()
	w := f.request("POST", "/v1/workspaces/"+f.private.WorkspaceID+"/app-accounts/connect", f.ownerToken, map[string]string{"provider": "linear", "next": "/app/wrens/rooms/room_test?keep=1"})
	if w.Code != 200 {
		t.Fatalf("start %d %s", w.Code, w.Body.String())
	}
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	u, _ := url.Parse(result["authorizationUrl"])
	if u.Query().Get("scope") != "read" || u.Query().Get("resource") != f.remote.URL+"/mcp/readonly" {
		t.Fatal("wrong OAuth authority")
	}
	f.challenge = u.Query().Get("code_challenge")
	return "/v1/auth/app-oauth/callback?code=fixture-code&state=" + url.QueryEscape(u.Query().Get("state")) + "&iss=" + url.QueryEscape(f.remote.URL), w.Result().Cookies()
}
func (f *appOAuthFixture) connect(t *testing.T) WrenAppAccount {
	t.Helper()
	path, cookies := f.start(t)
	w := f.request("GET", path, f.ownerToken, nil, cookies...)
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "appConnection=connected") {
		t.Fatalf("callback %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	accounts, err := f.h.service.Store.WrenAppAccounts(context.Background(), f.private.WorkspaceID, f.private.AccountID)
	if err != nil || len(accounts) != 1 {
		t.Fatal("account not saved", err)
	}
	return accounts[0]
}
func TestWrenAppOAuthBindsBrowserAccountIssuerAndSingleUse(t *testing.T) {
	f := newAppOAuthFixture(t)
	path, cookies := f.start(t)
	for _, w := range []*httptest.ResponseRecorder{f.request("GET", path, f.ownerToken, nil), f.request("GET", path, f.memberToken, nil, cookies...)} {
		if !strings.Contains(w.Header().Get("Location"), "expired") {
			t.Fatal("unbound callback accepted")
		}
	}
	if f.exchanges.Load() != 0 {
		t.Fatal("invalid callback reached token endpoint")
	}
	w := f.request("GET", path, f.ownerToken, nil, cookies...)
	if !strings.Contains(w.Header().Get("Location"), "connected") || f.exchanges.Load() != 1 {
		t.Fatal("valid callback rejected", w.Header().Get("Location"))
	}
	replay := f.request("GET", path, f.ownerToken, nil, cookies...)
	if !strings.Contains(replay.Header().Get("Location"), "expired") || f.exchanges.Load() != 1 {
		t.Fatal("callback replay")
	}
	accounts, err := f.h.service.Store.WrenAppAccounts(context.Background(), f.private.WorkspaceID, f.member)
	if err != nil || len(accounts) != 0 {
		t.Fatal("peer can see account")
	}
	var installs int
	f.h.service.Store.pool.QueryRow(context.Background(), `SELECT count(*) FROM wren_extension_installations`).Scan(&installs)
	if installs != 0 {
		t.Fatal("OAuth implicitly granted room access")
	}
	raw := f.request("GET", "/v1/workspaces/"+f.private.WorkspaceID+"/app-accounts", f.ownerToken, nil).Body.String()
	for _, secret := range []string{"fixture_access", "fixture_refresh", "ciphertext", "clientId"} {
		if strings.Contains(raw, secret) {
			t.Fatal("account metadata contains secret")
		}
	}
	badPath, badCookies := f.start(t)
	badPath = strings.Replace(badPath, url.QueryEscape(f.remote.URL), url.QueryEscape("https://attacker.example"), 1)
	f.request("GET", badPath, f.ownerToken, nil, badCookies...)
	if f.exchanges.Load() != 1 {
		t.Fatal("issuer mix-up redeemed code")
	}
}
func TestWrenAppAccountReuseRefreshIsolationAndDisconnect(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	ctx := context.Background()
	s := f.h.service.Store
	discovery, err := f.h.appAccountDiscovery(ctx, f.private.WorkspaceID, f.private.AccountID, account.ID)
	if err != nil || len(discovery.Tools) != 1 || discovery.Tools[0].Impact != "read" {
		t.Fatal("account discovery", err)
	}
	manifest := WrenExtensionManifest{Name: "Fixture app", Origin: f.remote.URL, Auth: "bearer", Tools: discovery.Tools}
	extension, err := s.saveWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInput{ID: "extension_account_fixture", Manifest: manifest}, f.h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := s.InstallWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInstallationInput{ID: "extinstall_account_fixture", WrenKnowledgeScope: WrenKnowledgeScope{Scope: "room", ConversationID: f.room.ID}, ExtensionID: extension.ID, Version: 1, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	input := WrenExtensionCredentialInput{AppAccountID: account.ID}
	if _, err = f.h.ConfigureWrenExtensionCredential(ctx, f.private.WorkspaceID, f.member, installation.ID, input); err == nil {
		t.Fatal("peer linked another account")
	}
	installation, err = f.h.ConfigureWrenExtensionCredential(ctx, f.private.WorkspaceID, f.private.AccountID, installation.ID, input)
	if err != nil || !installation.CredentialConfigured {
		t.Fatal("link failed", err)
	}
	if _, err = f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.member, installation.ID, "list_issues", "peer-no-token", json.RawMessage(`{}`)); err == nil {
		t.Fatal("peer inherited token")
	}
	call, err := f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, installation.ID, "list_issues", "account-read", json.RawMessage(`{}`))
	if err != nil || call.Status != "completed" || f.calls.Load() != 1 {
		t.Fatalf("authenticated call %+v %v", call, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_app_accounts SET expires_at=now()-interval '1 minute' WHERE id=$1`, account.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.h.appAccountDiscovery(ctx, f.private.WorkspaceID, f.private.AccountID, account.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.refreshes.Load() != 1 {
		t.Fatalf("refresh token raced %d", f.refreshes.Load())
	}
	var sealed []byte
	s.pool.QueryRow(ctx, `SELECT ciphertext FROM wren_app_accounts WHERE id=$1`, account.ID).Scan(&sealed)
	if bytes.Contains(sealed, []byte("fixture_access")) || bytes.Contains(sealed, []byte("fixture_refresh")) {
		t.Fatal("account secrets stored in plaintext")
	}
	path := "/v1/workspaces/" + f.private.WorkspaceID + "/app-accounts/" + account.ID
	if w := f.request("DELETE", path, f.memberToken, map[string]int{"expectedRevision": 1}); w.Code != 404 {
		t.Fatal("peer disconnected account", w.Code)
	}
	if w := f.request("DELETE", path, f.ownerToken, map[string]int{"expectedRevision": 1}); w.Code != 200 {
		t.Fatal("disconnect failed", w.Code, w.Body.String())
	}
	current, err := s.WrenExtensionInstallation(ctx, f.private.WorkspaceID, f.private.AccountID, installation.ID)
	if err != nil || current.CredentialConfigured {
		t.Fatal("disconnect not reflected in room")
	}
	if _, err = f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, installation.ID, "list_issues", "after-disconnect", json.RawMessage(`{}`)); err == nil || f.calls.Load() != 1 {
		t.Fatal("disconnected account invoked")
	}
}
func TestWrenAppOAuthCancelledExpiredAndDestination(t *testing.T) {
	f := newAppOAuthFixture(t)
	path, cookies := f.start(t)
	w := f.request("GET", path+"&error=access_denied", f.ownerToken, nil, cookies...)
	if !strings.Contains(w.Header().Get("Location"), "appConnection=cancelled") || f.exchanges.Load() != 0 {
		t.Fatal("cancel handled incorrectly")
	}
	path, cookies = f.start(t)
	f.h.service.Store.pool.Exec(context.Background(), `UPDATE wren_app_oauth_states SET expires_at=$1`, time.Now().Add(-time.Minute))
	w = f.request("GET", path, f.ownerToken, nil, cookies...)
	if !strings.Contains(w.Header().Get("Location"), "expired") {
		t.Fatal("expired state accepted")
	}
	for _, value := range []string{"https://evil.example/app/wrens", "//evil.example/app/wrens", "/app/wrens\\evil", "/other", "/app/customizeX", "/app/customize?conversation=nope"} {
		if appNextPath(value) != "/app/customize?app=linear" {
			t.Fatal("unsafe return accepted", value)
		}
	}
	if appNextPath("/app/customize?conversation=conv_x&evil=1") != "/app/customize?app=linear&conversation=conv_x" {
		t.Fatal("customize return lost conversation")
	}
	if appNextPath("/app/wrens/rooms/room_test?keep=1") != "/app/wrens/rooms/room_test?apps=1" {
		t.Fatal("wrens return lost destination")
	}
}

func TestWrenAppAccountCannotFollowChangedEndpointOrShareCredential(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	ctx := context.Background()
	s := f.h.service.Store
	manifest := WrenExtensionManifest{Name: "Changed destination", Origin: f.remote.URL, Auth: "bearer", Tools: []WrenExtensionTool{{Name: "read", Method: "MCP", Endpoint: f.remote.URL + "/different", Description: "Read", Impact: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	extension, err := s.saveWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInput{ID: "extension_other_destination", Manifest: manifest}, f.h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	install, err := s.InstallWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInstallationInput{ID: "extinstall_other_destination", WrenKnowledgeScope: WrenKnowledgeScope{Scope: "room", ConversationID: f.room.ID}, ExtensionID: extension.ID, Version: 1, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.h.ConfigureWrenExtensionCredential(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, WrenExtensionCredentialInput{AppAccountID: account.ID}); err == nil {
		t.Fatal("token authorized for different endpoint")
	}
	if _, err = f.h.ConfigureWrenExtensionCredential(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, WrenExtensionCredentialInput{AppAccountID: account.ID, SharedService: true}); err == nil {
		t.Fatal("personal OAuth account shared as service credential")
	}
}

func TestWrenAppOAuthLiveLinearRegistration(t *testing.T) {
	if os.Getenv("CANTER_TEST_LIVE_APP_OAUTH") != "1" {
		t.Skip("opt-in live registration, no account authorization")
	}
	s, c, human := operatorFixture(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://localhost:3028", Secrets: testVault(t)}).(*HTTPServer)
	r := httptest.NewRequest("POST", "http://localhost:3028/v1/workspaces/"+c.WorkspaceID+"/app-accounts/connect", strings.NewReader(`{"provider":"linear","next":"/app/wrens"}`))
	r.Header.Set("Origin", "http://localhost:3028")
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: human})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("live registration %d %s", w.Code, w.Body.String())
	}
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	u, err := url.Parse(result["authorizationUrl"])
	if err != nil || u.Scheme != "https" || u.Host != "mcp.linear.app" || u.Query().Get("scope") != "read" || u.Query().Get("resource") != "https://mcp.linear.app/mcp/readonly" {
		t.Fatal("unexpected live authorization destination")
	}
	t.Log("Linear registered the client and issued a read-only PKCE authorization URL. No person authorized an account.")
}

func TestWrenAppAccountsFailClosedWithoutVault(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	path, cookies := f.start(t)
	f.h.config.Secrets = nil
	if _, err := f.h.appAccountDiscovery(context.Background(), f.private.WorkspaceID, f.private.AccountID, account.ID); err == nil {
		t.Fatal("discovery succeeded without the secret vault")
	}
	response := f.request("GET", path, f.ownerToken, nil, cookies...)
	if response.Code != 303 || !strings.Contains(response.Header().Get("Location"), "appConnection=failed") {
		t.Fatal("callback did not recover from unavailable vault")
	}
}

func TestWrenAppAccountPinChangeCannotReusePriorAudience(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	ctx := context.Background()
	s := f.h.service.Store
	manifest := WrenExtensionManifest{Name: "Account pin check", Origin: f.remote.URL, Auth: "bearer", Tools: []WrenExtensionTool{{Name: "read", Method: "MCP", Endpoint: account.Endpoint, Description: "Read", Impact: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	extension, err := s.saveWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInput{ID: "extension_account_pin", Manifest: manifest}, f.h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	input := WrenExtensionInstallationInput{ID: "extinstall_account_pin", WrenKnowledgeScope: WrenKnowledgeScope{Scope: "room", ConversationID: f.room.ID}, ExtensionID: extension.ID, Version: 1, Enabled: true, CredentialMode: "personal"}
	install, err := s.InstallWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	install, err = f.h.ConfigureWrenExtensionCredential(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, WrenExtensionCredentialInput{AppAccountID: account.ID})
	if err != nil || !install.CredentialConfigured {
		t.Fatal("original account link", err)
	}
	manifest.Tools[0].Endpoint = f.remote.URL + "/different"
	_, err = s.saveWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInput{ID: extension.ID, ExpectedVersion: 1, Manifest: manifest}, f.h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	input.Version = 2
	input.ExpectedRevision = install.Revision
	install, err = s.InstallWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	if install.CredentialConfigured {
		t.Fatal("changed resource incorrectly shown as connected")
	}
	if _, err = f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, "read", "changed-resource", json.RawMessage(`{}`)); err == nil {
		t.Fatal("changed resource inherited authorization")
	}
	if f.calls.Load() != 0 {
		t.Fatal("request reached the changed resource")
	}
}

// The same account is used through real local OAuth/MCP HTTP exchanges. These
// fixtures verify lifecycle behavior, not authorization of a live Linear user.
func (f *appOAuthFixture) reconnect(t *testing.T, account WrenAppAccount) (string, []*http.Cookie) {
	t.Helper()
	w := f.request("POST", "/v1/workspaces/"+f.private.WorkspaceID+"/app-accounts/connect", f.ownerToken, map[string]any{"provider": "linear", "next": "/app/wrens/rooms/room_test", "reconnectId": account.ID, "expectedRevision": account.Revision})
	if w.Code != 200 {
		t.Fatalf("reconnect start %d %s", w.Code, w.Body.String())
	}
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	u, _ := url.Parse(result["authorizationUrl"])
	f.challenge = u.Query().Get("code_challenge")
	return "/v1/auth/app-oauth/callback?code=fixture-code&state=" + url.QueryEscape(u.Query().Get("state")) + "&iss=" + url.QueryEscape(f.remote.URL), w.Result().Cookies()
}
func (f *appOAuthFixture) linked(t *testing.T, account WrenAppAccount) WrenExtensionInstallation {
	t.Helper()
	ctx := context.Background()
	s := f.h.service.Store
	manifest := WrenExtensionManifest{Name: "Recovery fixture", Origin: f.remote.URL, Auth: "bearer", Tools: []WrenExtensionTool{{Name: "list_issues", Method: "MCP", Endpoint: account.Endpoint, Description: "Read issues", Impact: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	ext, e := s.saveWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInput{ID: "extension_recovery_fixture", Manifest: manifest}, f.h.config.Extensions)
	if e != nil {
		t.Fatal(e)
	}
	installation, e := s.InstallWrenExtension(ctx, f.private.WorkspaceID, f.private.AccountID, WrenExtensionInstallationInput{ID: "extinstall_recovery_fixture", WrenKnowledgeScope: WrenKnowledgeScope{Scope: "room", ConversationID: f.room.ID}, ExtensionID: ext.ID, Version: 1, Enabled: true, CredentialMode: "personal"})
	if e != nil {
		t.Fatal(e)
	}
	installation, e = f.h.ConfigureWrenExtensionCredential(ctx, f.private.WorkspaceID, f.private.AccountID, installation.ID, WrenExtensionCredentialInput{AppAccountID: account.ID})
	if e != nil {
		t.Fatal(e)
	}
	return installation
}
func TestWrenAppAccountRefreshFailureAndReconnectRetainsLinks(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	install := f.linked(t, account)
	ctx := context.Background()
	s := f.h.service.Store
	if _, e := s.pool.Exec(ctx, `UPDATE wren_app_accounts SET expires_at=now()-interval '1 minute' WHERE id=$1`, account.ID); e != nil {
		t.Fatal(e)
	}
	f.refreshFailure.Store(1)
	_, e := f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, "list_issues", "recovery-outage", json.RawMessage(`{}`))
	if e == nil || errors.Is(e, errAppReconnect) || strings.Contains(e.Error(), "sensitive") {
		t.Fatal("outage incorrectly requires reconnect", e)
	}
	accounts, e := s.WrenAppAccounts(ctx, f.private.WorkspaceID, f.private.AccountID)
	if e != nil || len(accounts) != 1 || accounts[0].Reconnect || accounts[0].Revision != 1 {
		t.Fatal("outage changed authorization state", e)
	}
	f.refreshFailure.Store(2)
	_, e = f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, "list_issues", "recovery-rejected", json.RawMessage(`{}`))
	if !errors.Is(e, errAppReconnect) || f.calls.Load() != 0 {
		t.Fatal("invalid grant reached remote tool", e)
	}
	accounts, e = s.WrenAppAccounts(ctx, f.private.WorkspaceID, f.private.AccountID)
	if e != nil || len(accounts) != 1 || !accounts[0].Reconnect || accounts[0].Revision != 2 {
		t.Fatal("failure not persisted", e)
	}
	current, e := s.WrenExtensionInstallation(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID)
	if e != nil || current.CredentialConfigured || current.AppAccountID != account.ID {
		t.Fatal("room did not reflect failed account", e)
	}
	_, e = f.h.appAccountDiscovery(ctx, f.private.WorkspaceID, f.private.AccountID, account.ID)
	if !errors.Is(e, errAppReconnect) || f.refreshes.Load() != 2 {
		t.Fatal("failed account repeatedly refreshed", e)
	}
	path, cookies := f.reconnect(t, accounts[0])
	w := f.request("GET", path, f.ownerToken, nil, cookies...)
	if !strings.Contains(w.Header().Get("Location"), "appConnection=reconnected") {
		t.Fatal("reconnect callback failed", w.Header().Get("Location"))
	}
	accounts, e = s.WrenAppAccounts(ctx, f.private.WorkspaceID, f.private.AccountID)
	if e != nil || len(accounts) != 1 || accounts[0].ID != account.ID || accounts[0].Reconnect || accounts[0].Revision != 3 || accounts[0].Label != account.Label {
		t.Fatal("reconnect replaced account identity", e)
	}
	current, e = s.WrenExtensionInstallation(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID)
	if e != nil || !current.CredentialConfigured || current.CredentialRevision != install.CredentialRevision {
		t.Fatal("existing account link lost", e)
	}
	call, e := f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, "list_issues", "after-reconnection", json.RawMessage(`{}`))
	if e != nil || call.Status != "completed" || f.calls.Load() != 1 {
		t.Fatal("reconnected account unusable", e)
	}
	if _, e = f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.member, install.ID, "list_issues", "peer-reconnect-check", json.RawMessage(`{}`)); e == nil || f.calls.Load() != 1 {
		t.Fatal("peer inherited reconnected account")
	}
}
func TestWrenAppAccountDiscoveryPersistsPermanentRefreshFailure(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	ctx := context.Background()
	if _, e := f.h.service.Store.pool.Exec(ctx, `UPDATE wren_app_accounts SET expires_at=now()-interval '1 minute' WHERE id=$1`, account.ID); e != nil {
		t.Fatal(e)
	}
	f.refreshFailure.Store(2)
	if _, e := f.h.appAccountDiscovery(ctx, f.private.WorkspaceID, f.private.AccountID, account.ID); !errors.Is(e, errAppReconnect) {
		t.Fatal(e)
	}
	accounts, e := f.h.service.Store.WrenAppAccounts(ctx, f.private.WorkspaceID, f.private.AccountID)
	if e != nil || len(accounts) != 1 || !accounts[0].Reconnect {
		t.Fatal("discovery rolled back reconnect state", e)
	}
}
func TestWrenAppAccountReconnectCannotReviveChangedOrDisconnectedAccount(t *testing.T) {
	for _, method := range []string{"PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			f := newAppOAuthFixture(t)
			account := f.connect(t)
			path, cookies := f.reconnect(t, account)
			resource := "/v1/workspaces/" + f.private.WorkspaceID + "/app-accounts/" + account.ID
			w := f.request(method, resource, f.ownerToken, map[string]any{"expectedRevision": account.Revision, "label": "Renamed during authorization"})
			if w.Code != 200 {
				t.Fatal("concurrent account change failed", w.Code)
			}
			w = f.request("GET", path, f.ownerToken, nil, cookies...)
			if !strings.Contains(w.Header().Get("Location"), "appConnection=changed") {
				t.Fatal("stale reconnect applied", w.Header().Get("Location"))
			}
			accounts, e := f.h.service.Store.WrenAppAccounts(context.Background(), f.private.WorkspaceID, f.private.AccountID)
			if e != nil {
				t.Fatal(e)
			}
			if method == "DELETE" && len(accounts) != 0 {
				t.Fatal("disconnect revived")
			}
			if method == "PATCH" && (len(accounts) != 1 || accounts[0].Label != "Renamed during authorization" || accounts[0].Revision != 2) {
				t.Fatal("concurrent label overwritten")
			}
		})
	}
}
func TestWrenAppAccountReconnectRejectsPeerStaleRevisionAndCancellation(t *testing.T) {
	f := newAppOAuthFixture(t)
	account := f.connect(t)
	base := "/v1/workspaces/" + f.private.WorkspaceID + "/app-accounts/connect"
	input := map[string]any{"provider": "linear", "reconnectId": account.ID, "expectedRevision": account.Revision}
	if w := f.request("POST", base, f.memberToken, input); w.Code != 404 {
		t.Fatal("peer reconnect allowed", w.Code)
	}
	input["expectedRevision"] = 0
	if w := f.request("POST", base, f.ownerToken, input); w.Code != 409 {
		t.Fatal("stale reconnect allowed", w.Code)
	}
	path, cookies := f.reconnect(t, account)
	w := f.request("GET", path+"&error=access_denied", f.ownerToken, nil, cookies...)
	if !strings.Contains(w.Header().Get("Location"), "appConnection=cancelled") {
		t.Fatal("cancel failed")
	}
	accounts, e := f.h.service.Store.WrenAppAccounts(context.Background(), f.private.WorkspaceID, f.private.AccountID)
	if e != nil || len(accounts) != 1 || accounts[0].Revision != 1 || accounts[0].Reconnect {
		t.Fatal("cancel changed working account", e)
	}
}

func TestWrenAppAccountRemoteUnauthorizedAndLateRejection(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		t.Run(fmt.Sprint(discovery), func(t *testing.T) {
			f := newAppOAuthFixture(t)
			account := f.connect(t)
			install := f.linked(t, account)
			ctx := context.Background()
			s := f.h.service.Store
			var oldCipher []byte
			if e := s.pool.QueryRow(ctx, `SELECT ciphertext FROM wren_app_accounts WHERE id=$1`, account.ID).Scan(&oldCipher); e != nil {
				t.Fatal(e)
			}
			f.toolReject.Store(true)
			if discovery {
				if _, e := f.h.appAccountDiscovery(ctx, f.private.WorkspaceID, f.private.AccountID, account.ID); !errors.Is(e, errAppReconnect) {
					t.Fatal("discovery unauthorized", e)
				}
			} else {
				call, e := f.h.TestWrenExtensionTool(ctx, f.private.WorkspaceID, f.private.AccountID, install.ID, "list_issues", "unauthorized-remote", json.RawMessage(`{}`))
				if e != nil || call.Status != "failed" || call.HTTPStatus != 401 {
					t.Fatal("401 receipt", call, e)
				}
			}
			accounts, e := s.WrenAppAccounts(ctx, f.private.WorkspaceID, f.private.AccountID)
			if e != nil || len(accounts) != 1 || !accounts[0].Reconnect {
				t.Fatal("remote rejection not reflected", e)
			}
			path, cookies := f.reconnect(t, accounts[0])
			w := f.request("GET", path, f.ownerToken, nil, cookies...)
			if !strings.Contains(w.Header().Get("Location"), "appConnection=reconnected") {
				t.Fatal("reconnect failed")
			}
			if e = f.h.markRejectedAppAccount(ctx, f.private.WorkspaceID, f.private.AccountID, account.ID, oldCipher); e != nil {
				t.Fatal(e)
			}
			accounts, e = s.WrenAppAccounts(ctx, f.private.WorkspaceID, f.private.AccountID)
			if e != nil || accounts[0].Reconnect || accounts[0].Revision != 3 {
				t.Fatal("late old-token rejection disabled new connection", e)
			}
		})
	}
}
