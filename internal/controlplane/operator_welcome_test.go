package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/oauth2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOperatorWelcomeFreshUnsavedAndRateLimited(t *testing.T) {
	s, c, token := operatorFixture(t)
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input struct {
			Messages  []modelMessage `json:"messages"`
			Tools     []any          `json:"tools"`
			MaxTokens int            `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if len(input.Messages) != 1 || input.Messages[0].Role != "system" || len(input.Tools) != 0 || input.MaxTokens != 512 {
			t.Errorf("unbounded or non-isolated welcome request: %+v", input)
		}
		titleResponse(w, fmt.Sprintf("Welcome %d. What would you like to work on?", calls))
	}))
	defer provider.Close()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", Model: "test", BaseURL: provider.URL}})
	request := func(cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/welcome", nil)
		r.Header.Set("Origin", origin)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	_, _, foreign, err := s.Signup(context.Background(), "welcome-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ cookie, origin string }{{"", "http://canter.test"}, {foreign, "http://canter.test"}, {token, "http://foreign.test"}} {
		if w := request(tc.cookie, tc.origin); w.Code < 400 {
			t.Fatalf("unauthorized request accepted: %d", w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized request reached model")
	}
	for i := 1; i <= 5; i++ {
		w := request(token, "http://canter.test")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), fmt.Sprintf("Welcome %d.", i)) {
			t.Fatalf("fresh introduction missing: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request(token, "http://canter.test"); w.Code != 429 || w.Header().Get("Retry-After") != "60" || calls != 5 {
		t.Fatalf("minute limit failed: %d calls=%d", w.Code, calls)
	}
	// Simulate the next minute with the hourly account budget exhausted.
	ctx := context.Background()
	if _, err = s.pool.Exec(ctx, `UPDATE auth_rate_windows SET expires_at=now()-interval '1 second' WHERE bucket_hash=$1`, secretHash("operator-welcome-minute\x00"+c.AccountID)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE auth_rate_windows SET count=20 WHERE bucket_hash=$1`, secretHash("operator-welcome-hour\x00"+c.AccountID)); err != nil {
		t.Fatal(err)
	}
	if w := request(token, "http://canter.test"); w.Code != 429 || w.Header().Get("Retry-After") != "3600" || calls != 5 {
		t.Fatalf("hour limit failed: %d calls=%d", w.Code, calls)
	}
	var chats, runs, messages int
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM operator_conversations),(SELECT count(*) FROM operator_runs),(SELECT count(*) FROM operator_messages)`).Scan(&chats, &runs, &messages); err != nil {
		t.Fatal(err)
	}
	if chats != 1 || runs != 0 || messages != 0 {
		t.Fatalf("welcome persisted conversation data: chats=%d runs=%d messages=%d", chats, runs, messages)
	}
}

func TestOperatorWelcomeFailureAndCancellation(t *testing.T) {
	s, c, token := operatorFixture(t)
	cancelled := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail/chat/completions" {
			w.WriteHeader(503)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(3 * time.Second):
		}
	}))
	defer provider.Close()
	for _, path := range []string{"/fail", "/cancel"} {
		h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", Model: "test", BaseURL: provider.URL + path}})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/welcome", nil).WithContext(ctx)
		r.Header.Set("Origin", "http://canter.test")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		cancel()
		if path == "/fail" && (w.Code != 502 || strings.Contains(w.Body.String(), `"text"`)) {
			t.Fatalf("failure fabricated welcome: %d %s", w.Code, w.Body.String())
		}
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("leaving welcome did not cancel provider")
	}
	var count int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM operator_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed welcome was persisted: %d %v", count, err)
	}
}

func TestOperatorWelcomeProjectsUseAuthorizedGitHubMetadata(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input struct {
			Messages []modelMessage `json:"messages"`
			Tools    []any          `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if len(input.Messages) != 2 || len(input.Tools) != 0 || !strings.Contains(input.Messages[0].Content, "not source code") || !strings.Contains(input.Messages[1].Content, "octocat/reading-list") {
			t.Errorf("project facts or guardrails missing: %+v", input)
		}
		titleResponse(w, "I noticed octocat/reading-list. What would you like to work on?")
	}))
	defer provider.Close()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", GitHubApp: OAuthCredentials{ClientID: "client", ClientSecret: "secret"}, Operator: OperatorConfig{APIKey: "test", Model: "test", BaseURL: provider.URL}}).(*HTTPServer)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/welcome?stage=projects", nil)
		r.Header.Set("Origin", "http://canter.test")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != 409 || calls != 0 {
		t.Fatalf("unconnected projects fabricated: %d", w.Code)
	}
	if err := h.saveGitHubProviderConnection(ctx, c.AccountID, c.WorkspaceID, "123", "octocat", "", "github-app", (&oauth2.Token{AccessToken: "test-repository-token", TokenType: "bearer", RefreshToken: "test-refresh", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"refresh_token_expires_in": 86400})); err != nil {
		t.Fatal(err)
	}
	old := githubHTTPClient
	githubHTTPClient = &http.Client{Transport: githubRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" || r.URL.Path != "/user/repos" || r.Header.Get("Authorization") != "Bearer test-repository-token" {
			t.Errorf("unexpected source access: %s", r.URL)
		}
		return webTestResponse(200, `[{"full_name":"octocat/reading-list","description":"Track books and reading notes","private":true}]`), nil
	})}
	t.Cleanup(func() { githubHTTPClient = old })
	if w := request(); w.Code != 200 || calls != 1 {
		t.Fatalf("project introduction failed: %d %s", w.Code, w.Body.String())
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("project welcome saved: %d %v", count, err)
	}
}

func TestOperatorWelcomeProjectMetadataBoundedAndOAuthReturn(t *testing.T) {
	repos := []githubRepository{}
	for i := 0; i < 50; i++ {
		repos = append(repos, githubRepository{Name: fmt.Sprintf("owner/project-%d", i), Description: strings.Repeat("x", 1000)})
	}
	messages := operatorProjectWelcomeMessages(repos)
	if len(messages) != 2 || strings.Contains(messages[1].Content, "owner/project-3") || len(messages[1].Content) > 1300 {
		t.Fatal("project metadata not bounded")
	}
	h := &HTTPServer{config: HTTPConfig{PublicURL: "http://canter.test"}}
	r := httptest.NewRequest(http.MethodGet, "http://canter.test/callback", nil)
	w := httptest.NewRecorder()
	h.githubReturn(w, r, "/app?welcome=1", "connected")
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil || target.Path != "/app" || target.Query().Get("welcome") != "1" || target.Query().Get("github") != "connected" {
		t.Fatalf("OAuth lost onboarding: %s %v", target, err)
	}
}
