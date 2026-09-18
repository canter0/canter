package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestOAuthStateBindingExpiryAndReplay(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	login := oauthLoginState{Provider: "google", Verifier: "verifier", Nonce: "nonce", Mode: "sign-in", Next: "/app", InviteHash: secretHash("")}
	if err := s.beginOAuth(ctx, "state", "browser", login); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range [][3]string{{"state", "wrong-browser", "google"}, {"state", "browser", "github"}, {"wrong-state", "browser", "google"}} {
		if _, err := s.consumeOAuth(ctx, attempt[0], attempt[1], attempt[2]); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("accepted mismatched state: %v", err)
		}
	}
	if got, err := s.consumeOAuth(ctx, "state", "browser", "google"); err != nil || got.Verifier != "verifier" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.consumeOAuth(ctx, "state", "browser", "google"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("accepted replay", err)
	}
	if err := s.beginOAuth(ctx, "expired", "browser", login); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	s.now = func() time.Time { return now.Add(11 * time.Minute) }
	if _, err := s.consumeOAuth(ctx, "expired", "browser", "google"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("accepted expired state", err)
	}
}

func TestOAuthAccountsRequireExplicitLinkingAndHonorAccess(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	login := oauthLoginState{InviteHash: secretHash("")}
	id := oauthIdentity{Provider: "google", Subject: "google-one", Email: "owner@example.com"}
	token, err := s.signinOAuth(ctx, id, login, false)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := s.ResolveHuman(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := s.WorkspacesForAccount(ctx, principal.Actor.ID)
	if err != nil || len(workspaces) != 1 {
		t.Fatal(workspaces, err)
	}
	handler := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	for path, field := range map[string]string{
		"/v1/installations?workspaceId=" + workspaces[0].ID: "installations",
		"/v1/workspaces/" + workspaces[0].ID + "/systems":   "systems",
		"/v1/workspaces/" + workspaces[0].ID + "/changes":   "changes",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK || string(body[field]) != "[]" {
			t.Fatalf("new-account list %s: %d %s", field, response.Code, response.Body.String())
		}
	}
	var cap int64
	if err = s.pool.QueryRow(ctx, `SELECT limit_cents FROM workspace_usage_caps WHERE workspace_id=$1`, workspaces[0].ID).Scan(&cap); err != nil || cap != 500 {
		t.Fatal(cap, err)
	}
	if _, _, _, err = s.Signin(ctx, id.Email, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("SSO sentinel allowed password signin")
	}
	// The stable subject remains authoritative if the provider changes its email.
	id.Email = "changed@example.com"
	repeat, err := s.signinOAuth(ctx, id, login, true)
	if err != nil {
		t.Fatal(err)
	}
	repeatPrincipal, err := s.ResolveHuman(ctx, repeat)
	if err != nil || repeatPrincipal.Actor.ID != principal.Actor.ID {
		t.Fatal(repeatPrincipal, err)
	}
	github := oauthIdentity{Provider: "github", Subject: "123", Email: "owner@example.com"}
	if _, err = s.signinOAuth(ctx, github, login, false); !errors.Is(err, errOAuthAccountExists) {
		t.Fatal("automatically linked email", err)
	}
	link := login
	link.LinkAccountID = &principal.Actor.ID
	// Explicitly authenticated linking also works when the user's provider email differs.
	github.Email = "github-owner@example.com"
	if _, err = s.signinOAuth(ctx, github, link, false); err != nil {
		t.Fatal(err)
	}
	other := oauthIdentity{Provider: "google", Subject: "google-two", Email: "other@example.com"}
	if _, err = s.signinOAuth(ctx, other, login, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("invite bypass", err)
	}
	if err = s.SeedInvite(ctx, "invite-test", "OAuth test"); err != nil {
		t.Fatal(err)
	}
	inviteLogin := login
	inviteLogin.InviteHash = secretHash("invite-test")
	if _, err = s.signinOAuth(ctx, other, inviteLogin, true); err != nil {
		t.Fatal(err)
	}
	other.Subject = "google-three"
	other.Email = "third@example.com"
	if _, err = s.signinOAuth(ctx, other, inviteLogin, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("reused invite", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, principal.Actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.signinOAuth(ctx, id, login, false); !errors.Is(err, ErrForbidden) {
		t.Fatal("disabled account authenticated", err)
	}
}

func TestOAuthHTTPFlowSetsSessionAndPreservesDestination(t *testing.T) {
	s := integrationStore(t)
	var nonce, challenge string
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("code") != "authorization-code" || r.Form.Get("client_secret") != "test-secret" || r.Form.Get("redirect_uri") != "http://canter.test/api/canter/auth/oauth/google/callback" || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge {
			t.Errorf("invalid token exchange")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-token","token_type":"Bearer"}`)
	}))
	defer providerServer.Close()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	h.oauth["google"] = &oauthProvider{config: oauth2.Config{ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: "http://canter.test/api/canter/auth/oauth/google/callback", Endpoint: oauth2.Endpoint{AuthURL: providerServer.URL + "/authorize", TokenURL: providerServer.URL, AuthStyle: oauth2.AuthStyleInParams}}, identity: func(_ context.Context, token *oauth2.Token, wantNonce string) (oauthIdentity, error) {
		if wantNonce != nonce || token.AccessToken != "access-token" {
			t.Error("identity binding failed")
		}
		return oauthIdentity{Provider: "google", Subject: "subject", Email: "oauth@example.com"}, nil
	}}
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/google?next=%2Fonboarding%2Fauthorize%3Fcode%3DABCD-EFGH", nil))
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	authURL, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	nonce = authURL.Query().Get("nonce")
	challenge = authURL.Query().Get("code_challenge")
	if state == "" || nonce == "" || challenge == "" || authURL.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing flow protections")
	}
	cookies := start.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal(cookies)
	}
	callback := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/google/callback?code=authorization-code&state="+state, nil)
	callback.AddCookie(cookies[0])
	result := httptest.NewRecorder()
	h.ServeHTTP(result, callback)
	if result.Code != http.StatusSeeOther || result.Header().Get("Location") != "http://canter.test/onboarding/authorize?code=ABCD-EFGH" {
		t.Fatalf("callback: %d %s", result.Code, result.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, cookie := range result.Result().Cookies() {
		if cookie.Name == "canter_session" {
			session = cookie
		}
	}
	if session == nil || !session.HttpOnly {
		t.Fatal("missing session")
	}
	if _, err = s.ResolveHuman(context.Background(), session.Value); err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, callback)
	if replay.Header().Get("Location") != "http://canter.test/sign-in?error=session_expired" {
		t.Fatal("callback replay accepted")
	}
}
