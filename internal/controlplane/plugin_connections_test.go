package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

func TestPluginLiveMarketplace(t *testing.T) {
	if os.Getenv("CANTER_TEST_LIVE_MARKETPLACE") != "1" {
		t.Skip("set CANTER_TEST_LIVE_MARKETPLACE=1 for the public upstream network check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	catalog, err := fetchPluginMarketplace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Plugins) < 200 || catalog.Cached {
		t.Fatalf("live catalog invalid: %d plugins", len(catalog.Plugins))
	}
	t.Logf("read %d live manifests at %s", len(catalog.Plugins), catalog.Revision)
}

func TestPluginMarketplaceManifestInputs(t *testing.T) {
	var snapshot pluginSnapshot
	if err := json.Unmarshal(pluginMarketplaceSnapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	catalog, err := parsePluginSnapshot(snapshot)
	if err != nil || len(catalog.Plugins) < 200 {
		t.Fatalf("catalog count=%d err=%v", len(catalog.Plugins), err)
	}
	var amplitude pluginCatalogEntry
	for _, entry := range catalog.Plugins {
		if entry.Slug == "amplitude" {
			amplitude = entry
		}
	}
	secret, err := resolvePluginServer(amplitude, "", nil, "", WrenExtensionConfig{})
	if err != nil || secret.Endpoint != "https://mcp.amplitude.com/mcp" {
		t.Fatalf("manifest default lost: %s %v", secret.Endpoint, err)
	}
	entry := pluginCatalogEntry{Servers: []pluginServer{{Name: "test", URL: "https://${HOST}/mcp", Headers: map[string]string{"Authorization": "Bearer ${API_TOKEN}"}}}, Inputs: []pluginInput{{Name: "HOST", Required: true, Label: "Host"}, {Name: "API_TOKEN", Required: true, Sensitive: true, Label: "API token"}}}
	if _, err = resolvePluginServer(entry, "", map[string]string{"HOST": "example.com"}, "", WrenExtensionConfig{}); err == nil {
		t.Fatal("missing required secret accepted")
	}
	secret, err = resolvePluginServer(entry, "", map[string]string{"HOST": "example.com", "API_TOKEN": "test-secret-value"}, "", WrenExtensionConfig{})
	if err != nil || secret.Headers["Authorization"] != "Bearer test-secret-value" {
		t.Fatalf("header expansion: %v", err)
	}
	for _, host := range []string{"example.com@127.0.0.1", "example.com/x?token=a", "localhost"} {
		if _, err = resolvePluginServer(entry, "", map[string]string{"HOST": host, "API_TOKEN": "test-secret-value"}, "", WrenExtensionConfig{}); err == nil {
			t.Fatalf("unsafe host accepted: %s", host)
		}
	}
}

func TestPluginPersonalInstallationLifecycle(t *testing.T) {
	ctx := context.Background()
	calls := 0
	acceptToken := true
	handler := mcp.NewServer(&mcp.Implementation{Name: "Plugin fixture", Version: "1"}, nil)
	handler.AddTool(&mcp.Tool{Name: "lookup", Description: "Search fixture items", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls++
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fixture-secret-value should be redacted"}}}, nil
	})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return handler }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptToken || r.Header.Get("Authorization") != "Bearer fixture-secret-value" {
			w.WriteHeader(401)
			return
		}
		stream.ServeHTTP(w, r)
	}))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	s.SetWrensEnabled(false)
	entry := pluginCatalogEntry{Slug: "fixture", Name: "Fixture", Description: "Fixture plugin", Servers: []pluginServer{{Name: "fixture", URL: server.URL + "/mcp"}}, Inputs: []pluginInput{}}
	_, err := s.pool.Exec(ctx, `INSERT INTO plugin_connections(id,workspace_id,account_id,slug,catalog) VALUES('plugin_fixture','ws','owner','fixture',$1)`, entry)
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.getPluginConnection(ctx, "ws", "owner", "plugin_fixture")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := resolvePluginServer(entry, "", nil, "", h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	v, err = h.discoverPlugin(ctx, "ws", "owner", v, secret)
	if err != nil || v.State != "needs_auth" {
		t.Fatalf("auth prompt missing: %+v %v", v, err)
	}
	secret.Headers["Authorization"] = "Bearer fixture-secret-value"
	v, err = h.discoverPlugin(ctx, "ws", "owner", v, secret)
	if err != nil || v.State != "review" {
		t.Fatalf("tool review missing: %+v %v", v, err)
	}
	v, err = h.activatePlugin(ctx, "ws", "owner", v, []string{"lookup"})
	if err != nil || v.State != "ready" {
		t.Fatalf("activation: %+v %v", v, err)
	}
	if _, err = h.getPluginConnection(ctx, "ws", "member", v.ID); err == nil {
		t.Fatal("other account read private connection")
	}
	run, err := s.EnqueueOperator(ctx, c, "plugin-runtime", "Use my plugin", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 1 {
		t.Fatalf("global install missing: %+v %v", tools, err)
	}
	installs, err := s.WrenExtensionInstallations(ctx, "ws", "owner", WrenKnowledgeScope{Scope: "operator", ConversationID: c.ID})
	if err != nil || len(installs) != 1 {
		t.Fatalf("scope: %v %v", installs, err)
	}
	// This explicit human test invocation is against a fixture only.
	result, err := h.TestWrenExtensionTool(ctx, "ws", "owner", installs[0].ID, "lookup", "plugin-explicit-test", json.RawMessage(`{}`))
	if err != nil || result.Status != "completed" || calls != 1 {
		t.Fatalf("MCP execution: %+v %v calls=%d", result, err, calls)
	}
	if strings.Contains(string(result.Result), "fixture-secret-value") {
		t.Fatal("header credential leaked through tool output")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := extensionCredentialRevision(ctx, tx, installs[0], "owner")
	tx.Rollback(ctx)
	if err != nil || revision != v.Revision {
		t.Fatalf("review credential binding: %d %v", revision, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO personal_customizations(id,workspace_id,account_id,kind,name,body,event,matcher) VALUES('hook_execution','ws','owner','hook','Pause lookup','Paused during verification.','PreToolUse','lookup')`); err != nil {
		t.Fatal(err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, "ws", "owner", installs[0].ID, "lookup", "plugin-hook-blocked", json.RawMessage(`{}`)); err == nil || calls != 1 {
		t.Fatal("hook was bypassed at execution boundary")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE personal_customizations SET enabled=false WHERE id='hook_execution'`); err != nil {
		t.Fatal(err)
	}
	acceptToken = false
	result, err = h.TestWrenExtensionTool(ctx, "ws", "owner", installs[0].ID, "lookup", "plugin-token-rejected", json.RawMessage(`{}`))
	if err != nil || result.Status != "failed" {
		t.Fatalf("rejected provider token: %+v %v", result, err)
	}
	if current, e := h.getPluginConnection(ctx, "ws", "owner", v.ID); e != nil || current.State != "needs_auth" {
		t.Fatalf("rejected token did not offer reconnect: %+v %v", current, e)
	}
	sessionToken := "chs_plugin_delete_fixture"
	if _, err = s.pool.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,expires_at,authenticated_at) VALUES('hss_plugin_delete','owner',$1,now()+interval '1 day',now())`, secretHash(sessionToken)); err != nil {
		t.Fatal(err)
	}
	current, err := h.getPluginConnection(ctx, "ws", "owner", v.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "http://canter.test/v1/workspaces/ws/plugins/"+v.ID, strings.NewReader(fmt.Sprintf(`{"revision":%d}`, current.Revision)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://canter.test")
	req.AddCookie(&http.Cookie{Name: h.humanCookieName(), Value: sessionToken})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("disconnect API %d %s", recorder.Code, recorder.Body.String())
	}
	current, err = h.getPluginConnection(ctx, "ws", "owner", v.ID)
	if err != nil || current.Enabled || current.State != "installed" {
		t.Fatalf("disconnect state: %+v %v", current, err)
	}
	tools, err = h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 0 {
		t.Fatalf("revoked plugin advertised: %v %v", tools, err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, "ws", "owner", installs[0].ID, "lookup", "plugin-after-disconnect", json.RawMessage(`{}`)); err == nil {
		t.Fatal("revoked plugin executed")
	}
	if calls != 1 {
		t.Fatalf("revoked call reached provider: %d", calls)
	}
}

func TestPluginOAuthProtocolAndBinding(t *testing.T) {
	var origin string
	var exchanged bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+origin+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(401)
		case "/.well-known/oauth-protected-resource/mcp":
			fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, origin+"/mcp", origin)
		case "/.well-known/oauth-authorization-server":
			fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"registration_endpoint":%q,"code_challenge_methods_supported":["S256"]}`, origin, origin+"/authorize", origin+"/token", origin+"/register")
		case "/token":
			r.ParseForm()
			if r.Form.Get("resource") != origin+"/mcp" || r.Form.Get("code_verifier") != "fixture-verifier" {
				t.Error("token exchange lost resource or PKCE binding")
			}
			exchanged = true
			fmt.Fprint(w, `{"access_token":"oauth-fixture-token","token_type":"Bearer","refresh_token":"fixture-refresh","expires_in":3600}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	origin = server.URL
	h, _, _ := operatorConnectionFixture(t, origin)
	metadata, err := h.pluginOAuthMetadata(context.Background(), origin+"/mcp")
	if err != nil || metadata.Issuer != origin {
		t.Fatalf("metadata discovery: %+v %v", metadata, err)
	}
	secret := pluginSecret{Endpoint: origin + "/mcp", OAuth: &pluginOAuthSecret{Metadata: metadata, ClientID: "fixture-client"}}
	values := make(map[string][]string)
	values["resource"] = []string{origin + "/mcp"}
	values["code_verifier"] = []string{"fixture-verifier"}
	if err = h.exchangePluginToken(context.Background(), &secret, values); err != nil || !exchanged || secret.Headers["Authorization"] != "Bearer oauth-fixture-token" || !secret.OAuth.ExpiresAt.After(time.Now()) {
		t.Fatalf("token exchange failed: %v", err)
	}
}

func TestPluginRulesAndHooksRuntime(t *testing.T) {
	h, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19999")
	_ = h
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO personal_customizations(id,workspace_id,account_id,kind,name,body,event,matcher) VALUES
	('rule_owner','ws','owner','rule','Style','Use short sentences.','',''),
	('hook_owner','ws','owner','hook','Block deploy','Deployments are paused.','PreToolUse','deploy'),
	('rule_member','ws','member','rule','Other person','PRIVATE MEMBER RULE','','')`)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueOperator(ctx, c, "customization-runtime", "Hello", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.customizationContext(ctx, c, run)
	if err != nil || !strings.Contains(text, "Use short sentences.") || strings.Contains(text, "PRIVATE MEMBER") {
		t.Fatalf("rule scope: %s %v", text, err)
	}
	if reason, err := s.customizationToolBlock(ctx, c, run, "canter_deploy_app"); err != nil || reason == "" {
		t.Fatalf("hook did not block: %s %v", reason, err)
	}
	if reason, err := s.customizationToolBlock(ctx, c, run, "canter_list_apps"); err != nil || reason != "" {
		t.Fatalf("hook blocked unrelated tool: %s %v", reason, err)
	}
	_, err = s.pool.Exec(ctx, `UPDATE personal_customizations SET enabled=false WHERE account_id='owner'`)
	if err != nil {
		t.Fatal(err)
	}
	if text, err = s.customizationContext(ctx, c, run); err != nil || text != "" {
		t.Fatalf("disabled rule survived: %s %v", text, err)
	}
}

func TestPluginOAuthCallbackEndToEnd(t *testing.T) {
	ctx := context.Background()
	var origin, challenge string
	tokenCalls := 0
	remote := mcp.NewServer(&mcp.Implementation{Name: "OAuth fixture", Version: "1"}, nil)
	remote.AddTool(&mcp.Tool{Name: "lookup", Description: "Read items", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	stream := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer complete-fixture-token" {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+origin+`/.well-known/oauth-protected-resource/mcp"`)
				w.WriteHeader(401)
				return
			}
			stream.ServeHTTP(w, r)
		case "/.well-known/oauth-protected-resource/mcp":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"resource":%q,"authorization_servers":[%q]}`, origin+"/mcp", origin)
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"registration_endpoint":%q,"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true}`, origin, origin+"/authorize", origin+"/token", origin+"/register")
		case "/register":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"client_id":"public-fixture-client","token_endpoint_auth_method":"none"}`)
		case "/token":
			r.ParseForm()
			if r.Form.Get("resource") != origin+"/mcp" || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge || r.Form.Get("redirect_uri") != "http://canter.test/api/canter/auth/plugin-oauth/callback" {
				t.Error("OAuth exchange binding mismatch")
			}
			tokenCalls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"complete-fixture-token","refresh_token":"complete-fixture-refresh","token_type":"Bearer","expires_in":3600}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	origin = server.URL
	h, s, _ := operatorConnectionFixture(t, origin)
	sessionToken := "chs_plugin_oauth_fixture"
	_, err := s.pool.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,expires_at,authenticated_at) VALUES('hss_plugin','owner',$1,now()+interval '1 day',now())`, secretHash(sessionToken))
	if err != nil {
		t.Fatal(err)
	}
	entry := pluginCatalogEntry{Slug: "oauth-fixture", Name: "OAuth fixture", Servers: []pluginServer{{Name: "fixture", URL: origin + "/mcp"}}, Inputs: []pluginInput{}}
	_, err = s.pool.Exec(ctx, `INSERT INTO plugin_connections(id,workspace_id,account_id,slug,catalog) VALUES('plugin_oauth_fixture','ws','owner','oauth-fixture',$1)`, entry)
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.getPluginConnection(ctx, "ws", "owner", "plugin_oauth_fixture")
	if err != nil {
		t.Fatal(err)
	}
	v, err = h.discoverPlugin(ctx, "ws", "owner", v, pluginSecret{Endpoint: origin + "/mcp", Headers: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	authorize := func() (*url.URL, *http.Cookie) {
		t.Helper()
		request := httptest.NewRequest("POST", "http://canter.test/v1/workspaces/ws/plugins/"+v.ID+"/authorize", strings.NewReader(fmt.Sprintf(`{"revision":%d}`, v.Revision)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://canter.test")
		request.AddCookie(&http.Cookie{Name: h.humanCookieName(), Value: sessionToken})
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("authorize status %d %s", response.Code, response.Body.String())
		}
		var result struct {
			URL string `json:"authorizationUrl"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.URL == "" {
			t.Fatal("missing authorization URL")
		}
		authorization, err := url.Parse(result.URL)
		if err != nil {
			t.Fatal(err)
		}
		if authorization.Query().Get("code_challenge_method") != "S256" || authorization.Query().Get("resource") != origin+"/mcp" {
			t.Fatal("missing PKCE/resource")
		}
		challenge = authorization.Query().Get("code_challenge")
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == h.pluginOAuthCookieName() {
				if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
					t.Fatal("invalid state cookie")
				}
				return authorization, cookie
			}
		}
		t.Fatal("missing OAuth browser binding")
		return nil, nil
	}
	callback := func(auth *url.URL, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		query := url.Values{"state": {auth.Query().Get("state")}, "code": {"fixture-code"}, "iss": {origin}}
		req := httptest.NewRequest("GET", "http://canter.test/v1/auth/plugin-oauth/callback?"+query.Encode(), nil)
		req.AddCookie(&http.Cookie{Name: h.humanCookieName(), Value: sessionToken})
		req.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		return recorder
	}
	auth, cookie := authorize()
	wrong := *cookie
	wrong.Value = "wrong-browser"
	if response := callback(auth, &wrong); !strings.Contains(response.Header().Get("Location"), "connection=expired") || tokenCalls != 0 {
		t.Fatal("wrong browser exchanged token")
	}
	response := callback(auth, cookie)
	if response.Code != 303 || !strings.Contains(response.Header().Get("Location"), "connection=connected") || tokenCalls != 1 {
		t.Fatalf("callback did not connect: %d %s calls=%d", response.Code, response.Header().Get("Location"), tokenCalls)
	}
	v, err = h.getPluginConnection(ctx, "ws", "owner", v.ID)
	if err != nil || v.State != "review" {
		t.Fatalf("callback setup state: %+v %v", v, err)
	}
	if response = callback(auth, cookie); !strings.Contains(response.Header().Get("Location"), "connection=expired") || tokenCalls != 1 {
		t.Fatal("callback replay accepted")
	}
	auth, cookie = authorize()
	if _, err = s.pool.Exec(ctx, `UPDATE plugin_connections SET enabled=false,ciphertext=NULL,revision=revision+1 WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if response = callback(auth, cookie); !strings.Contains(response.Header().Get("Location"), "connection=expired") || tokenCalls != 1 {
		t.Fatal("disconnect resurrected a pending OAuth connection")
	}
}

func TestPluginToolSearchIsBoundedAndRevocable(t *testing.T) {
	h, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19998")
	_ = h
	ctx := context.Background()
	run, err := s.EnqueueOperator(ctx, c, "plugin-search", "Search issues", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	available := []mcpTool{}
	for i := 0; i < 30; i++ {
		available = append(available, mcpTool{Name: fmt.Sprintf("wext_search_%02d", i), Description: "Search Linear issues"})
	}
	selected := rankPluginTools(available, "Linear issues")
	if len(selected) != 8 {
		t.Fatalf("search unbounded: %d", len(selected))
	}
	if exposed, err := s.discoveredPluginTools(ctx, run, available); err != nil || len(exposed) != 0 {
		t.Fatalf("full catalog loaded before discovery: %d %v", len(exposed), err)
	}
	raw, _ := json.Marshal(map[string]any{"tools": []map[string]string{{"name": selected[0].Name}, {"name": "not_authorized"}}})
	_, err = s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result) VALUES($1,'search','canter_find_plugin_tools','{}',$2)`, run.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if exposed, err := s.discoveredPluginTools(ctx, run, available); err != nil || len(exposed) != 1 {
		t.Fatalf("wrong discovered surface: %d %v", len(exposed), err)
	}
	if exposed, err := s.discoveredPluginTools(ctx, run, available[1:]); err != nil || len(exposed) != 0 {
		t.Fatalf("stale discovered tool survived revocation: %d %v", len(exposed), err)
	}
}
