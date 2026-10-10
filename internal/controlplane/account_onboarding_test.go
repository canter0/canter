package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAccountOnboardingCompletionIsAuthenticatedScopedAndPersistent(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	a, _, token, err := s.Signup(ctx, "onboarding@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _, err := s.Signup(ctx, "onboarding-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	request := func(method, path, session, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://canter.test/v1"+path, nil)
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
		}
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	status := func(want bool) {
		t.Helper()
		w := request(http.MethodGet, "/me", token, "")
		requireStatus(t, w, http.StatusOK)
		var body struct {
			Complete *bool `json:"onboardingComplete"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Complete == nil || *body.Complete != want {
			t.Fatalf("incorrect first-run state: %s %v", w.Body.String(), err)
		}
	}
	status(false)
	requireStatus(t, request(http.MethodPost, "/me/onboarding", "", "http://canter.test"), http.StatusUnauthorized)
	requireStatus(t, request(http.MethodPost, "/me/onboarding", token, "https://evil.example"), http.StatusForbidden)
	requireStatus(t, request(http.MethodPost, "/me/onboarding", token, ""), http.StatusForbidden)
	requireStatus(t, request(http.MethodGet, "/me/onboarding", token, ""), http.StatusMethodNotAllowed)
	requireStatus(t, request(http.MethodPost, "/me/onboarding/other", token, "http://canter.test"), http.StatusNotFound)
	status(false)
	requireStatus(t, request(http.MethodPost, "/me/onboarding", token, "http://canter.test"), http.StatusNoContent)
	status(true)
	var first time.Time
	if err := s.pool.QueryRow(ctx, `SELECT onboarding_completed_at FROM accounts WHERE id=$1`, a.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return first.Add(time.Minute) }
	requireStatus(t, request(http.MethodPost, "/me/onboarding", token, "http://canter.test"), http.StatusNoContent)
	var repeat time.Time
	if err := s.pool.QueryRow(ctx, `SELECT onboarding_completed_at FROM accounts WHERE id=$1`, a.ID).Scan(&repeat); err != nil || !repeat.Equal(first) {
		t.Fatal("completion was not idempotent", repeat, err)
	}
	if complete, err := s.onboardingComplete(ctx, other.ID); err != nil || complete {
		t.Fatal("completion changed another account", complete, err)
	}
	// A different session and server instance must observe the saved dismissal.
	_, _, newToken, err := s.Signin(ctx, a.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token = newToken
	h = NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	status(true)
}

func TestOAuthSignInCreatedAccountNeedsOnboarding(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token, err := s.signinOAuth(ctx, oauthIdentity{Provider: "google", Subject: "new-google-user", Email: "first-run@gmail.com"}, oauthLoginState{Mode: "sign-in"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ResolveHuman(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if complete, err := s.onboardingComplete(ctx, p.Account.ID); err != nil || complete {
		t.Fatal("SSO through sign-in skipped onboarding", complete, err)
	}
}

func TestAccountOnboardingMigrationPreservesEstablishedAccounts(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	accounts := make([]Account, 4)
	workspaces := make([]Workspace, 4)
	for i, email := range []string{"unused@example.com", "conversation@example.com", "agent@example.com", "system@example.com"} {
		var err error
		accounts[i], workspaces[i], _, err = s.Signup(ctx, email, "correct horse battery staple", "", false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateConversation(ctx, workspaces[1].ID, accounts[1].ID, "conv_existing", "Existing work"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_installations(id,workspace_id,name,harness,created_by) VALUES('existing-agent',$1,'Agent','test',$2)`, workspaces[2].ID, accounts[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO systems(workspace_id,name,contract,m1_prefix) VALUES($1,'existing-system','{"spec":{"m1":{"prefix":"onboarding-test/system"}}}','onboarding-test/system')`, workspaces[3].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version='034_account_onboarding'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i, account := range accounts {
		if complete, err := s.onboardingComplete(ctx, account.ID); err != nil || complete != (i > 0) {
			t.Fatal("migration lost first-run distinction", i, complete, err)
		}
	}
	// A later startup must not complete onboarding merely because activity was
	// added after the backfill. Completion is the user's explicit transition.
	if _, err := s.CreateConversation(ctx, workspaces[0].ID, accounts[0].ID, "conv_later", "Later work"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.onboardingComplete(ctx, accounts[0].ID); err != nil || complete {
		t.Fatal("onboarding backfill ran again", complete, err)
	}
}
