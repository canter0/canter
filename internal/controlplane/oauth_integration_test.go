package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestGoogleSignupVerifiesRemoteKeysAndPreservesOnboarding(t *testing.T) {
	key, sign := testGoogleSigner(t)
	_, forgedSign := testGoogleSigner(t)
	for _, tc := range []struct {
		name, reason, errorCode string
		verified                any
		keysStatus              int
		wrongNonce, forged      bool
	}{
		{name: "verified", verified: true, keysStatus: http.StatusOK},
		{name: "verified string", verified: "true", keysStatus: http.StatusOK},
		{name: "unverified", verified: false, keysStatus: http.StatusOK, reason: "unverified_email", errorCode: "unverified_email"},
		{name: "key endpoint forbidden", verified: true, keysStatus: http.StatusForbidden, reason: "token_verification", errorCode: "sign_in_failed"},
		{name: "nonce mismatch", verified: true, keysStatus: http.StatusOK, wrongNonce: true, reason: "nonce_mismatch", errorCode: "sign_in_failed"},
		{name: "forged token", verified: true, keysStatus: http.StatusOK, forged: true, reason: "token_verification", errorCode: "sign_in_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := integrationStore(t)
			var nonce string
			keyRequests := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/keys" {
					keyRequests++
					if tc.keysStatus != http.StatusOK {
						w.WriteHeader(tc.keysStatus)
						fmt.Fprint(w, "private-provider-error-body")
						return
					}
					json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: key, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
					return
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("code") != "authorization-code" {
					t.Error("invalid code exchange", err)
				}
				claims := map[string]any{"iss": "https://accounts.google.com", "aud": "canter-client", "sub": "new-google-owner", "email": "new-owner@example.com", "email_verified": tc.verified, "nonce": nonce, "exp": time.Now().Add(time.Hour).Unix()}
				if tc.wrongNonce {
					claims["nonce"] = "another-browser"
				}
				signer := sign
				if tc.forged {
					signer = forgedSign
				}
				json.NewEncoder(w).Encode(map[string]string{"access_token": "private-access-token", "token_type": "Bearer", "id_token": signer(claims)})
			}))
			defer provider.Close()
			oldClient := googleSigningKeyHTTPClient
			defer func() { googleSigningKeyHTTPClient = oldClient }()
			googleSigningKeyHTTPClient = &http.Client{Timeout: time.Second, Transport: authRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://www.googleapis.com/oauth2/v3/certs" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected signing key request")
				}
				copy := r.Clone(r.Context())
				copy.URL, _ = url.Parse(provider.URL + "/keys")
				return provider.Client().Transport.RoundTrip(copy)
			})}
			h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", GoogleOAuth: OAuthCredentials{ClientID: "canter-client", ClientSecret: "private-client-secret"}}).(*HTTPServer)
			h.oauth["google"].config.Endpoint = oauth2.Endpoint{AuthURL: provider.URL + "/authorize", TokenURL: provider.URL, AuthStyle: oauth2.AuthStyleInParams}
			start := httptest.NewRecorder()
			h.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/google?mode=create-account", nil))
			requireStatus(t, start, http.StatusSeeOther)
			location, err := url.Parse(start.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			nonce = location.Query().Get("nonce")
			request := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/google/callback?code=authorization-code&state="+location.Query().Get("state"), nil)
			for _, cookie := range start.Result().Cookies() {
				request.AddCookie(cookie)
			}
			var logs bytes.Buffer
			oldLog := log.Writer()
			defer log.SetOutput(oldLog)
			log.SetOutput(&logs)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			requireStatus(t, response, http.StatusSeeOther)
			if keyRequests == 0 {
				t.Fatal("did not use the configured signing key client")
			}
			if tc.errorCode == "" {
				if response.Header().Get("Location") != "http://canter.test/onboarding/agent" {
					t.Fatal("lost onboarding destination", response.Header().Get("Location"))
				}
				if _, err := s.ResolveHuman(context.Background(), authCookie(t, h, response, "session").Value); err != nil {
					t.Fatal("missing authenticated session", err)
				}
			} else {
				if response.Header().Get("Location") != "http://canter.test/create-account?error="+tc.errorCode+"&next=%2Fonboarding%2Fagent" || !strings.Contains(logs.String(), "reason="+tc.reason) {
					t.Fatal("incorrect failure classification", response.Header().Get("Location"), logs.String())
				}
				var accounts, sessions int
				if err := s.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM human_sessions)`).Scan(&accounts, &sessions); err != nil || accounts != 0 || sessions != 0 {
					t.Fatal("invalid identity created an account or session", accounts, sessions, err)
				}
			}
			for _, sensitive := range []string{"new-owner@example.com", "private-access-token", "private-client-secret", "private-provider-error-body", nonce, "authorization-code"} {
				if sensitive != "" && strings.Contains(logs.String(), sensitive) {
					t.Fatal("identity diagnostics exposed private data")
				}
			}
			replay := httptest.NewRecorder()
			h.ServeHTTP(replay, request)
			if replay.Header().Get("Location") != "http://canter.test/sign-in?error=session_expired" {
				t.Fatal("OAuth callback replay accepted")
			}
		})
	}
}

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

func TestOAuthAccountsReuseVerifiedEmailAndHonorAccess(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	login := oauthLoginState{InviteHash: secretHash("")}
	id := oauthIdentity{Provider: "google", Subject: "google-one", Email: "owner@example.com"}
	token, err := s.signinOAuth(ctx, id, login, false, nil)
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
	repeat, err := s.signinOAuth(ctx, id, login, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	repeatPrincipal, err := s.ResolveHuman(ctx, repeat)
	if err != nil || repeatPrincipal.Actor.ID != principal.Actor.ID {
		t.Fatal(repeatPrincipal, err)
	}
	github := oauthIdentity{Provider: "github", Subject: "123", Email: "owner@example.com"}
	if linked, err := s.signinOAuth(ctx, github, login, true, nil); err != nil {
		t.Fatal("verified email could not reuse the account", err)
	} else if p, err := s.ResolveHuman(ctx, linked); err != nil || p.Actor.ID != principal.Actor.ID {
		t.Fatal("verified email resolved another account", p, err)
	}
	link := login
	link.LinkAccountID = &principal.Actor.ID
	// Explicitly authenticated linking also works when the user's provider email differs.
	github.Email = "github-owner@example.com"
	if _, err = s.signinOAuth(ctx, github, link, false, nil); err != nil {
		t.Fatal(err)
	}
	other := oauthIdentity{Provider: "google", Subject: "google-two", Email: "other@example.com"}
	if _, err = s.signinOAuth(ctx, other, login, true, nil); !errors.Is(err, ErrForbidden) {
		t.Fatal("invite bypass", err)
	}
	if err = s.SeedInvite(ctx, "invite-test", "OAuth test"); err != nil {
		t.Fatal(err)
	}
	inviteLogin := login
	inviteLogin.InviteHash = secretHash("invite-test")
	if _, err = s.signinOAuth(ctx, other, inviteLogin, true, nil); err != nil {
		t.Fatal(err)
	}
	other.Subject = "google-three"
	other.Email = "third@example.com"
	if _, err = s.signinOAuth(ctx, other, inviteLogin, true, nil); !errors.Is(err, ErrForbidden) {
		t.Fatal("reused invite", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, principal.Actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.signinOAuth(ctx, id, login, false, nil); !errors.Is(err, ErrForbidden) {
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
	visit := captureAcquisition(t, h)
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
	callback.AddCookie(visit)
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
	var source, landing string
	if err = s.pool.QueryRow(context.Background(), `SELECT source,landing_path FROM account_acquisition WHERE visit_hash=$1`, secretHash(visit.Value)).Scan(&source, &landing); err != nil || source != "google" || landing != "/pricing" {
		t.Fatalf("OAuth lost acquisition: %q %q %v", source, landing, err)
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, callback)
	if replay.Header().Get("Location") != "http://canter.test/sign-in?error=session_expired" {
		t.Fatal("callback replay accepted")
	}
}
