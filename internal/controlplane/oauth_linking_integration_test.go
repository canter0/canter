package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/oauth2"
)

// Exercise the real start/callback flow with a controlled provider response.
// Provider claim verification is covered separately by oauth_test.go.
func oauthCallbackForIdentity(t *testing.T, h *HTTPServer, identity oauthIdentity, mode string) *httptest.ResponseRecorder {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != "verified-provider-code" {
			t.Error("invalid authorization-code exchange", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"provider-test-token","token_type":"Bearer"}`)
	}))
	defer provider.Close()
	h.oauth[identity.Provider] = &oauthProvider{
		config:   oauth2.Config{ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: h.config.PublicURL + "/api/canter/auth/oauth/" + identity.Provider + "/callback", Endpoint: oauth2.Endpoint{AuthURL: provider.URL + "/authorize", TokenURL: provider.URL, AuthStyle: oauth2.AuthStyleInParams}},
		identity: func(_ context.Context, _ *oauth2.Token, _ string) (oauthIdentity, error) { return identity, nil },
	}
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/"+identity.Provider+"?mode="+mode+"&next=%2Fapp", nil))
	requireStatus(t, start, http.StatusSeeOther)
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil || location.Query().Get("state") == "" {
		t.Fatal("missing OAuth state", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/auth/oauth/"+identity.Provider+"/callback?code=verified-provider-code&state="+location.Query().Get("state"), nil)
	for _, cookie := range start.Result().Cookies() {
		request.AddCookie(cookie)
	}
	result := httptest.NewRecorder()
	h.ServeHTTP(result, request)
	return result
}

func TestOAuthVerifiedEmailLinksAcrossProvidersFromEitherEntry(t *testing.T) {
	for _, original := range []string{"google", "github"} {
		for _, mode := range []string{"sign-in", "create-account"} {
			t.Run(original+"/"+mode, func(t *testing.T) {
				s, h, _ := newAuthTestServer(t)
				ctx := context.Background()
				first := oauthIdentity{Provider: original, Subject: "original-owner", Email: "owner@example.com"}
				token, err := s.signinOAuth(ctx, first, oauthLoginState{}, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				owner, err := s.ResolveHuman(ctx, token)
				if err != nil {
					t.Fatal(err)
				}
				workspaces, err := s.WorkspacesForAccount(ctx, owner.Actor.ID)
				if err != nil || len(workspaces) != 1 {
					t.Fatal(workspaces, err)
				}
				provider := "github"
				if original == "github" {
					provider = "google"
				}
				identity := oauthIdentity{Provider: provider, Subject: "new-provider-owner", Email: "OWNER@EXAMPLE.COM"}
				// Invitations apply to account creation, not an existing owner.
				h.config.RequireInvite = true
				response := oauthCallbackForIdentity(t, h, identity, mode)
				requireStatus(t, response, http.StatusSeeOther)
				if response.Header().Get("Location") != h.config.PublicURL+"/app" {
					t.Fatal(response.Header().Get("Location"))
				}
				cookie := authCookie(t, h, response, "session")
				linked, err := s.ResolveHuman(ctx, cookie.Value)
				if err != nil || linked.Actor.ID != owner.Actor.ID {
					t.Fatal("created another account", linked, err)
				}
				current, err := s.WorkspacesForAccount(ctx, linked.Actor.ID)
				if err != nil || len(current) != 1 || current[0].ID != workspaces[0].ID {
					t.Fatal("lost existing workspace", current, err)
				}
				// Once attached, the stable subject remains authoritative even
				// when this provider's email changes.
				identity.Email = "changed@example.com"
				repeat := oauthCallbackForIdentity(t, h, identity, mode)
				repeated, err := s.ResolveHuman(ctx, authCookie(t, h, repeat, "session").Value)
				if err != nil || repeated.Actor.ID != owner.Actor.ID {
					t.Fatal("new identity was not persisted", repeated, err)
				}
				var notifications int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM account_security_events WHERE account_id=$1 AND action='Connected sign-in account added'`, owner.Actor.ID).Scan(&notifications); err != nil || notifications != 1 {
					t.Fatal("link notification duplicated or missing", notifications, err)
				}
			})
		}
	}
}

func TestOAuthAutomaticLinkWaitsForMFA(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	response := signupHTTP(t, h, "mfa-owner@example.com")
	cookie := authCookie(t, h, response, "session")
	owner, err := s.ResolveHuman(ctx, cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := enrollTOTP(t, h, cookie)
	now := s.now().Add(31 * time.Second)
	s.now = func() time.Time { return now }
	identity := oauthIdentity{Provider: "github", Subject: "mfa-github", Email: owner.Account.Email}
	response = oauthCallbackForIdentity(t, h, identity, "create-account")
	requireStatus(t, response, http.StatusSeeOther)
	if !strings.HasPrefix(response.Header().Get("Location"), h.config.PublicURL+"/sign-in?next=") {
		t.Fatal("MFA was skipped", response.Header().Get("Location"))
	}
	challenge := authCookie(t, h, response, "challenge")
	assertUnlinked := func() {
		t.Helper()
		var linked bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM oauth_identities WHERE provider='github' AND subject='mfa-github')`).Scan(&linked); err != nil || linked {
			t.Fatal("provider linked before MFA", linked, err)
		}
	}
	assertUnlinked()
	bad := authRequest(t, h, "mfa/finish", authInput{Code: "invalid-factor"}, challenge)
	requireStatus(t, bad, http.StatusBadRequest)
	assertUnlinked()
	code, err := totp.GenerateCode(secret, s.now())
	if err != nil {
		t.Fatal(err)
	}
	complete := authRequest(t, h, "mfa/finish", authInput{Code: code}, challenge)
	requireStatus(t, complete, http.StatusOK)
	principal, err := s.ResolveHuman(ctx, authCookie(t, h, complete, "session").Value)
	if err != nil || principal.Actor.ID != owner.Actor.ID {
		t.Fatal(principal, err)
	}
	identity.Email = "changed@example.com"
	if _, err := s.signinOAuth(ctx, identity, oauthLoginState{}, false, nil); err != nil {
		t.Fatal("provider not attached after MFA", err)
	}
	// A fresh login still has to satisfy the account's MFA policy.
	token, err := s.signinOAuth(ctx, identity, oauthLoginState{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveHuman(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("linked provider bypassed MFA", err)
	}
}

func TestOAuthAutomaticLinkRejectsProtectedUnverifiedDisabledAndConflictingAccounts(t *testing.T) {
	for _, state := range []string{"unverified", "disabled", "provider-conflict"} {
		t.Run(state, func(t *testing.T) {
			s, h, _ := newAuthTestServer(t)
			ctx := context.Background()
			identity := oauthIdentity{Provider: "google", Subject: "original", Email: "owner@example.com"}
			token, err := s.signinOAuth(ctx, identity, oauthLoginState{}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := s.ResolveHuman(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "unverified":
				_, err = s.pool.Exec(ctx, `UPDATE accounts SET email_verified_at=NULL WHERE id=$1`, owner.Actor.ID)
			case "disabled":
				_, err = s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, owner.Actor.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			identity.Subject = "new-owner"
			if state != "provider-conflict" {
				identity.Provider = "github"
			}
			response := oauthCallbackForIdentity(t, h, identity, "create-account")
			requireStatus(t, response, http.StatusSeeOther)
			if !strings.Contains(response.Header().Get("Location"), "error=") {
				t.Fatal("unsafe account linked", response.Header())
			}
			var accounts, identities int
			if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM oauth_identities)`).Scan(&accounts, &identities); err != nil || accounts != 1 || identities != 1 {
				t.Fatal("rejection changed accounts", accounts, identities, err)
			}
		})
	}
}

func TestOAuthVerifiesLegacyEmailAndRetiresUnverifiedCredentials(t *testing.T) {
	for _, mode := range []string{"sign-in", "create-account"} {
		t.Run(mode, func(t *testing.T) {
			s, h, _ := newAuthTestServer(t)
			ctx := context.Background()
			account, workspace, oldToken, err := s.Signup(ctx, "legacy@example.com", "unverified legacy password", "", false)
			if err != nil {
				t.Fatal(err)
			}
			// Internal Signup establishes trusted email ownership; historical
			// password registrations entered migration 027 without that proof.
			if _, err = s.pool.Exec(ctx, `UPDATE accounts SET email_verified_at=NULL WHERE id=$1`, account.ID); err != nil {
				t.Fatal(err)
			}
			response := oauthCallbackForIdentity(t, h, oauthIdentity{Provider: "github", Subject: "legacy-owner", Email: account.Email}, mode)
			requireStatus(t, response, http.StatusSeeOther)
			if response.Header().Get("Location") != h.config.PublicURL+"/app" {
				t.Fatal("legacy owner was not authenticated", response.Header().Get("Location"))
			}
			principal, err := s.ResolveHuman(ctx, authCookie(t, h, response, "session").Value)
			if err != nil || principal.Actor.ID != account.ID {
				t.Fatal("legacy account was replaced", principal, err)
			}
			workspaces, err := s.WorkspacesForAccount(ctx, principal.Actor.ID)
			if err != nil || len(workspaces) != 1 || workspaces[0].ID != workspace.ID {
				t.Fatal("legacy workspace was lost", workspaces, err)
			}
			if _, err = s.ResolveHuman(ctx, oldToken); !errors.Is(err, ErrUnauthorized) {
				t.Fatal("unverified registration session survived OAuth ownership proof", err)
			}
			if _, _, _, err = s.Signin(ctx, account.Email, "unverified legacy password"); !errors.Is(err, ErrUnauthorized) {
				t.Fatal("unverified registration password survived OAuth ownership proof", err)
			}
		})
	}
}

func TestOAuthLegacyVerificationDoesNotBypassEnrolledMFA(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	response := signupHTTP(t, h, "protected-legacy@example.com")
	cookie := authCookie(t, h, response, "session")
	owner, err := s.ResolveHuman(ctx, cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	enrollTOTP(t, h, cookie)
	if _, err = s.pool.Exec(ctx, `UPDATE accounts SET email_verified_at=NULL WHERE id=$1`, owner.Actor.ID); err != nil {
		t.Fatal(err)
	}
	response = oauthCallbackForIdentity(t, h, oauthIdentity{Provider: "github", Subject: "protected-legacy", Email: owner.Account.Email}, "create-account")
	requireStatus(t, response, http.StatusSeeOther)
	if !strings.Contains(response.Header().Get("Location"), "error=account_exists") {
		t.Fatal("legacy MFA was bypassed", response.Header().Get("Location"))
	}
	var linked bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM oauth_identities WHERE account_id=$1)`, owner.Actor.ID).Scan(&linked); err != nil || linked {
		t.Fatal("protected legacy account was linked", linked, err)
	}
}

func TestOAuthKnownSubjectDoesNotMergeAnotherMatchingEmailAccount(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	google := oauthIdentity{Provider: "google", Subject: "google-owner", Email: "google@example.com"}
	github := oauthIdentity{Provider: "github", Subject: "github-owner", Email: "github@example.com"}
	if _, err := s.signinOAuth(ctx, google, oauthLoginState{}, false, nil); err != nil {
		t.Fatal(err)
	}
	token, err := s.signinOAuth(ctx, github, oauthLoginState{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.ResolveHuman(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	github.Email = google.Email
	response := oauthCallbackForIdentity(t, h, github, "create-account")
	principal, err := s.ResolveHuman(ctx, authCookie(t, h, response, "session").Value)
	if err != nil || principal.Actor.ID != owner.Actor.ID {
		t.Fatal("email reassigned a bound provider identity", principal, err)
	}
}
