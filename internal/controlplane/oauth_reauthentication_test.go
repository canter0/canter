package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestOAuthConnectionReauthenticationPreservesDestination(t *testing.T) {
	s, c, human := operatorFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", GitHubOAuth: OAuthCredentials{ClientID: "github", ClientSecret: "secret"}, GitHubApp: OAuthCredentials{ClientID: "app", ClientSecret: "secret"}}).(*HTTPServer)
	if _, err := s.pool.Exec(ctx, `UPDATE human_sessions SET authenticated_at=$1`, s.now().Add(-6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"github", "github-app"} {
		for _, session := range []string{"", human} {
			params := url.Values{"mode": {"repository"}, "workspace": {c.WorkspaceID}, "next": {"/app?welcome=1"}}
			r := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/"+provider+"?"+params.Encode(), nil)
			if session != "" {
				r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			requireStatus(t, w, http.StatusSeeOther)
			signin, err := url.Parse(w.Header().Get("Location"))
			if err != nil || signin.Host != "canter.test" || signin.Path != "/sign-in" || signin.Query().Get("reauth") != "1" {
				t.Fatal("stale identity did not reach sign-in", signin, err)
			}
			resume, err := url.Parse(signin.Query().Get("next"))
			if err != nil || resume.IsAbs() || resume.Host != "" || resume.Path != "/api/canter/auth/oauth/"+provider || resume.Query().Get("workspace") != c.WorkspaceID || resume.Query().Get("mode") != "repository" || resume.Query().Get("next") != "/app?welcome=1" {
				t.Fatal("connection destination lost", resume, err)
			}
		}
	}
	var states int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_login_states`).Scan(&states); err != nil || states != 0 {
		t.Fatal("stale proof began a connection", states, err)
	}
	// Signing in again must allow the original connection with PKCE intact.
	if _, err := s.pool.Exec(ctx, `UPDATE human_sessions SET authenticated_at=$1`, s.now()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/github-app?mode=repository&workspace="+c.WorkspaceID+"&next=%2Fapp%3Fwelcome%3D1", nil)
	r.AddCookie(&http.Cookie{Name: "canter_session", Value: human})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	requireStatus(t, w, http.StatusSeeOther)
	provider, err := url.Parse(w.Header().Get("Location"))
	if err != nil || provider.Host != "github.com" || provider.Query().Get("code_challenge_method") != "S256" || provider.Query().Get("state") == "" {
		t.Fatal("reauthenticated connection did not resume securely", provider, err)
	}
}
