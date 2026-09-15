package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type githubRoundTrip func(*http.Request) (*http.Response, error)

func (f githubRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubConnectionOAuthRepositoryAccessAndIsolation(t *testing.T) {
	s, c, human := operatorFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", GitHubOAuth: OAuthCredentials{ClientID: "client", ClientSecret: "secret"}}).(*HTTPServer)
	var challenge string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "secret" || r.Form.Get("redirect_uri") != "http://canter.test/api/canter/auth/oauth/github/callback" || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge {
			t.Error("repository OAuth lost PKCE or callback binding")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"private-repository-token","token_type":"bearer","scope":"repo"}`)
	}))
	defer provider.Close()
	h.oauth["github"].config.Endpoint.TokenURL = provider.URL
	oldClient := githubHTTPClient
	revoked := false
	githubHTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer private-repository-token" {
			t.Error("GitHub repository request lost its server-held connection")
		}
		status, body := 200, ""
		switch r.URL.Path {
		case "/user":
			body = `{"id":123,"login":"octocat"}`
		case "/user/repos":
			body = `[{"full_name":"octocat/private-site","private":true,"default_branch":"main"}]`
		case "/repos/octocat/private-site":
			body = `{"description":"Private website","default_branch":"main"}`
		case "/repos/octocat/private-site/commits/main":
			body = `{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
		case "/repos/octocat/private-site/git/trees/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			body = `{"tree":[{"path":"index.html","type":"blob"}]}`
		default:
			t.Errorf("unexpected GitHub path %s", r.URL.Path)
		}
		if revoked {
			status = 401
			body = `{"message":"Bad credentials"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	t.Cleanup(func() { githubHTTPClient = oldClient })
	request := func(method, path, token string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://canter.test"+path, nil)
		r.Header.Set("Origin", "http://canter.test")
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	base := "/v1/workspaces/" + c.WorkspaceID + "/github"
	if w := request("GET", base+"/repositories", human); w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":false`) {
		t.Fatalf("disconnected: %d %s", w.Code, w.Body.String())
	}
	startPath := "/v1/auth/oauth/github?mode=repository&workspace=" + c.WorkspaceID + "&next=" + url.QueryEscape("/app/conversations/"+c.ID+"?keep=yes")
	if w := request("GET", startPath, ""); w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "session_expired") {
		t.Fatal("anonymous connection allowed")
	}
	start := request("GET", startPath, human)
	authURL, _ := url.Parse(start.Header().Get("Location"))
	if start.Code != 303 || authURL.Host != "github.com" || authURL.Query().Get("scope") != "repo" {
		t.Fatalf("start: %d %s", start.Code, start.Header().Get("Location"))
	}
	challenge = authURL.Query().Get("code_challenge")
	callbackPath := "/v1/auth/oauth/github/callback?code=authorization-code&state=" + authURL.Query().Get("state")
	callback := request("GET", callbackPath, human, start.Result().Cookies()...)
	if callback.Code != 303 || callback.Header().Get("Location") != "http://canter.test/app/conversations/"+c.ID+"?github=connected&keep=yes" {
		t.Fatalf("callback: %d %s", callback.Code, callback.Header().Get("Location"))
	}
	if w := request("GET", callbackPath, human, start.Result().Cookies()...); !strings.Contains(w.Header().Get("Location"), "session_expired") {
		t.Fatal("callback replay accepted")
	}
	var ciphertext []byte
	if err := s.pool.QueryRow(ctx, `SELECT encrypted_token FROM github_repository_connections WHERE account_id=$1 AND workspace_id=$2`, c.AccountID, c.WorkspaceID).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte("private-repository-token")) {
		t.Fatal("token not encrypted", err)
	}
	w := request("GET", base+"/repositories", human)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "octocat/private-site") || strings.Contains(w.Body.String(), "private-repository-token") {
		t.Fatalf("repositories: %d %s", w.Code, w.Body.String())
	}
	// Same-workspace collaborators must never inherit another member's connection.
	other, _, otherHuman, err := s.Signup(ctx, "other-github@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if w := request("GET", base, otherHuman); w.Code == 200 {
		t.Fatal("cross-workspace connection exposed")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", base, otherHuman); w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":false`) {
		t.Fatal("collaborator inherited connection")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO github_repository_connections(account_id,workspace_id,github_id,login,encrypted_token,scopes) VALUES($1,$2,'123','octocat',$3,'repo')`, other.ID, c.WorkspaceID, ciphertext); err != nil {
		t.Fatal(err)
	}
	if state, token, err := h.githubAccess(ctx, other.ID, c.WorkspaceID); err != nil || !state.Reconnect || token != "" {
		t.Fatal("ciphertext was transferable between accounts")
	}
	// Exercise the hosted tool boundary: real grant, persisted UI event, private read.
	if _, err := s.EnqueueOperator(ctx, c, "github-request", "Deploy something", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	p, err := s.operatorPrincipal(ctx, c, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	o := OperatorRuntime{Server: h}
	result, err := o.executeTool(ctx, run, c, p, operatorTestCall("canter_show_repositories", "{}"))
	if err != nil || !strings.Contains(string(result), "private-site") || strings.Contains(string(result), "private-repository-token") {
		t.Fatalf("picker tool: %s %v", result, err)
	}
	call := operatorTestCall("canter_inspect_repository", `{"repository":"octocat/private-site"}`)
	call.ID = "inspect-private"
	result, err = o.executeTool(ctx, run, c, p, call)
	if err != nil || !strings.Contains(string(result), "index.html") {
		t.Fatalf("private inspection: %s %v", result, err)
	}
	var surface string
	if err := s.pool.QueryRow(ctx, `SELECT data->>'kind' FROM operator_events WHERE run_id=$1 AND kind='surface' ORDER BY sequence LIMIT 1`, run.ID).Scan(&surface); err != nil || surface != "github" {
		t.Fatal("missing native GitHub view", err)
	}
	revoked = true
	w = request("GET", base+"/repositories", human)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reconnect":true`) || strings.Contains(w.Body.String(), "private-site") {
		t.Fatal("revoked connection did not prompt reconnection")
	}
	if w := request("DELETE", base, human); w.Code != 200 {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body.String())
	}
	if state, token, err := h.githubAccess(ctx, c.AccountID, c.WorkspaceID); err != nil || state.Connected || token != "" {
		t.Fatal("disconnect retained usable credential")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_identities`).Scan(&count); err != nil || count != 0 {
		t.Fatal("repository connection changed sign-in identity")
	}
}

func TestGitHubArchiveRedirectNeverForwardsCredential(t *testing.T) {
	oldClient := githubHTTPClient
	t.Cleanup(func() { githubHTTPClient = oldClient })
	for _, destination := range []string{"https://codeload.github.com/owner/repo/legacy.tar.gz/commit?token=signed", "https://attacker.test/archive", "http://codeload.github.com/archive", "https://codeload.github.com.attacker.test/archive"} {
		requests := 0
		githubHTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
			requests++
			response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("archive")), Request: r}
			if r.URL.Host == "api.github.com" {
				if r.Header.Get("Authorization") != "Bearer private-token" {
					t.Error("missing API credential")
				}
				response.StatusCode = 302
				response.Header.Set("Location", destination)
			} else if r.URL.Host != "codeload.github.com" || r.Header.Get("Authorization") != "" {
				t.Error("credential or request escaped GitHub API boundary")
			}
			return response, nil
		})}
		body, err := githubBytes(context.WithValue(context.Background(), githubTokenKey{}, "private-token"), "https://api.github.com/repos/owner/repo/tarball/commit", 100)
		if strings.HasPrefix(destination, "https://codeload.github.com/") {
			if err != nil || string(body) != "archive" || requests != 2 {
				t.Fatal("valid private archive failed", err)
			}
		} else if err == nil || requests != 1 {
			t.Fatal("unsafe redirect accepted")
		}
	}
}

func TestGitHubConnectionResponsesContainNoCredentialFields(t *testing.T) {
	raw, err := json.Marshal(githubConnection{Connected: true, Enabled: true, Login: "octocat"})
	if err != nil || strings.Contains(string(raw), "token") {
		t.Fatal("credential field in public connection state")
	}
}
