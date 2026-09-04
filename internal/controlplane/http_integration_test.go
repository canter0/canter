package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPHumanOnboardsFreshAgentAndAgentBootstraps(t *testing.T) {
	store := integrationStore(t)
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})

	signup := requestJSON(t, handler, http.MethodPost, "/v1/auth/signup", map[string]any{"email": "http-owner@example.com", "password": "correct horse battery staple"}, nil)
	if signup.Code != http.StatusCreated {
		t.Fatalf("signup status %d: %s", signup.Code, signup.Body.String())
	}
	cookies := signup.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "canter_session" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected human session cookie: %#v", cookies)
	}
	var signupBody struct {
		Workspace Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(signup.Body.Bytes(), &signupBody); err != nil {
		t.Fatal(err)
	}

	begin := requestJSON(t, handler, http.MethodPost, "/v1/device/authorizations", map[string]any{"name": "Blackout Codex", "harness": "codex", "authority": map[string]any{"inspect": true, "draft": true, "applyMode": "human-approval-required"}}, nil)
	if begin.Code != http.StatusCreated {
		t.Fatalf("begin device status %d: %s", begin.Code, begin.Body.String())
	}
	var device DeviceAuthorization
	if err := json.Unmarshal(begin.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}

	approve := requestJSON(t, handler, http.MethodPost, "/v1/device/authorizations/"+device.UserCode+"/approve", map[string]string{"workspaceId": signupBody.Workspace.ID}, cookies[0])
	if approve.Code != http.StatusOK {
		t.Fatalf("approve status %d: %s", approve.Code, approve.Body.String())
	}

	exchange := requestJSON(t, handler, http.MethodPost, "/v1/device/token", map[string]string{"deviceCode": device.DeviceCode, "clientInstance": "no-context-task"}, nil)
	if exchange.Code != http.StatusOK {
		t.Fatalf("exchange status %d: %s", exchange.Code, exchange.Body.String())
	}
	var pair TokenPair
	if err := json.Unmarshal(exchange.Body.Bytes(), &pair); err != nil {
		t.Fatal(err)
	}

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/v1/agent/bootstrap", nil)
	bootstrapRequest.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	bootstrap := httptest.NewRecorder()
	handler.ServeHTTP(bootstrap, bootstrapRequest)
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status %d: %s", bootstrap.Code, bootstrap.Body.String())
	}
	var bootstrapBody Bootstrap
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &bootstrapBody); err != nil {
		t.Fatal(err)
	}
	if bootstrapBody.Installation.ID != pair.Installation.ID || bootstrapBody.Session.ClientInstance != "no-context-task" || bootstrapBody.Workspace.ID != signupBody.Workspace.ID {
		t.Fatalf("fresh agent did not reconstruct durable identity: %#v", bootstrapBody)
	}

	mcp := requestJSONWithBearer(t, handler, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "canter_whoami", "arguments": map[string]any{}}}, pair.AccessToken)
	if mcp.Code != http.StatusOK || !bytes.Contains(mcp.Body.Bytes(), []byte(pair.Installation.ID)) {
		t.Fatalf("MCP did not use durable bearer identity: %d %s", mcp.Code, mcp.Body.String())
	}
}

func TestHTTPCookieMutationsRequireTheConfiguredOrigin(t *testing.T) {
	store := integrationStore(t)
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "https://canter.test"})
	signup := requestJSON(t, handler, http.MethodPost, "/v1/auth/signup", map[string]any{"email": "csrf-owner@example.com", "password": "correct horse battery staple"}, nil)
	if signup.Code != http.StatusCreated {
		t.Fatalf("signup status %d: %s", signup.Code, signup.Body.String())
	}
	cookie := signup.Result().Cookies()[0]
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/signout", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("originless cookie mutation status %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/auth/signout", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.test")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin cookie mutation status %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/auth/signout", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://canter.test")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("same-origin signout status %d: %s", response.Code, response.Body.String())
	}
}

func TestHTTPSUsesHostPrefixedSecureSessionCookie(t *testing.T) {
	store := integrationStore(t)
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "https://canter.test", CookieSecure: true})
	signup := requestJSON(t, handler, http.MethodPost, "/v1/auth/signup", map[string]any{"email": "secure-cookie@example.com", "password": "correct horse battery staple"}, nil)
	if signup.Code != http.StatusCreated {
		t.Fatalf("signup status %d: %s", signup.Code, signup.Body.String())
	}
	cookies := signup.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-canter_session" || !cookies[0].Secure || cookies[0].Path != "/" || cookies[0].Domain != "" {
		t.Fatalf("unexpected secure human session cookie: %#v", cookies)
	}
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
		request.Header.Set("Origin", "http://canter.test")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requestJSONWithBearer(t *testing.T, handler http.Handler, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
