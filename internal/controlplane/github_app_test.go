package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestGitHubAppRefreshIsolationAndConcurrency(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", GitHubApp: OAuthCredentials{ClientID: "app", ClientSecret: "app-secret"}, GitHubAppSlug: "canter-deploy"}).(*HTTPServer)
	token := (&oauth2.Token{AccessToken: "initial", RefreshToken: "refresh-first", Expiry: time.Now().Add(time.Hour)}).WithExtra(map[string]any{"refresh_token_expires_in": 15897600})
	if err := h.saveGitHubProviderConnection(ctx, c.AccountID, c.WorkspaceID, "123", "octocat", "", "github-app", token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE github_repository_connections SET expires_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-first" {
			t.Error("invalid refresh request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"rotated","refresh_token":"refresh-second","token_type":"bearer","expires_in":28800,"refresh_token_expires_in":15897600}`)
	}))
	defer provider.Close()
	h.oauth["github-app"].config.Endpoint.TokenURL = provider.URL
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, value, err := h.githubAccess(ctx, c.AccountID, c.WorkspaceID)
			if err != nil || value != "rotated" || !state.Connected || state.Provider != "github-app" {
				t.Errorf("refresh failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh redeemed %d times", calls.Load())
	}
	var sealed, refresh []byte
	if err := s.pool.QueryRow(ctx, `SELECT encrypted_token,encrypted_refresh_token FROM github_repository_connections WHERE account_id=$1 AND workspace_id=$2`, c.AccountID, c.WorkspaceID).Scan(&sealed, &refresh); err != nil {
		t.Fatal(err)
	}
	aead, _ := h.githubCipher("github-app")
	if _, err := openGitHubToken(aead, sealed, githubTokenBinding("other", c.WorkspaceID)); err == nil {
		t.Fatal("cross-account token accepted")
	}
	if _, err := openGitHubToken(aead, refresh, githubTokenBinding(c.AccountID, c.WorkspaceID)); err == nil {
		t.Fatal("refresh token accepted as access token")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE github_repository_connections SET expires_at=now()-interval '1 minute',refresh_expires_at=now()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	state, value, err := h.githubAccess(ctx, c.AccountID, c.WorkspaceID)
	if err != nil || !state.Reconnect || value != "" || calls.Load() != 1 {
		t.Fatal("expired refresh token reused")
	}
}

func TestGitHubAppCannotBeUsedForSignIn(t *testing.T) {
	h := NewHTTPServer(&Service{}, HTTPConfig{PublicURL: "http://canter.test", GitHubApp: OAuthCredentials{ClientID: "app", ClientSecret: "secret"}}).(*HTTPServer)
	for _, mode := range []string{"login", "signup", "link"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://canter.test/v1/auth/oauth/github-app?mode="+mode, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("unexpected rejected sign-in: %d", w.Code)
		}
	}
}
