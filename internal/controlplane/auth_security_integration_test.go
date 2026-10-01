package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

type authTestMailer struct {
	mu       sync.Mutex
	messages []AuthEmail
	fail     bool
}

func (m *authTestMailer) Send(_ context.Context, id string, email AuthEmail) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return "", errors.New("temporary")
	}
	m.messages = append(m.messages, email)
	return "sent-" + id, nil
}
func newAuthTestServer(t *testing.T) (*Store, *HTTPServer, *authTestMailer) {
	t.Helper()
	s := integrationStore(t)
	m := &authTestMailer{}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test", CookieSecure: true, Secrets: testVault(t), Auth: AuthConfig{Mailer: m}}).(*HTTPServer)
	return s, h, m
}
func authRequest(t *testing.T, h *HTTPServer, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return authMethod(t, h, http.MethodPost, path, body, cookies...)
}
func authMethod(t *testing.T, h *HTTPServer, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, "/v1/auth/"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", h.config.PublicURL)
	for _, c := range cookies {
		if c != nil {
			r.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func requireStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
	}
}
func authCookie(t *testing.T, h *HTTPServer, w *httptest.ResponseRecorder, kind string) *http.Cookie {
	t.Helper()
	name := h.authCookieName(kind)
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge > 0 {
			return c
		}
	}
	t.Fatalf("missing %s cookie in %s", kind, w.Body.String())
	return nil
}
func lastEmailCode(t *testing.T, h *HTTPServer) string {
	t.Helper()
	tx, err := h.service.Store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var id, key string
	var sealed []byte
	if err = tx.QueryRow(context.Background(), `SELECT id,key_id,ciphertext FROM auth_email_outbox WHERE status='queued' ORDER BY created_at DESC LIMIT 1`).Scan(&id, &key, &sealed); err != nil {
		t.Fatal(err)
	}
	raw, err := h.config.Secrets.open(key, sealed, []byte("canter:auth-mail:"+id))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	var mail AuthEmail
	if err = json.Unmarshal(raw, &mail); err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`\b[0-9]{6}\b`).FindString(mail.Text)
	if code == "" {
		t.Fatal("missing email code")
	}
	return code
}

// Exercise the actual public signup proof when another integration test needs
// an authenticated browser. No test-only bypass is exposed in production.
func signupHTTP(t *testing.T, handler http.Handler, email string, extras ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	h := handler.(*HTTPServer)
	if h.config.Secrets == nil {
		h.config.Secrets = testVault(t)
	}
	if h.config.Auth.Mailer == nil {
		h.config.Auth.Mailer = &authTestMailer{}
	}
	start := authRequest(t, h, "signup/start", authInput{Email: email}, extras...)
	requireStatus(t, start, http.StatusAccepted)
	c := authCookie(t, h, start, "challenge")
	cookies := append([]*http.Cookie{c}, extras...)
	return authRequest(t, h, "signup/finish", authInput{Code: lastEmailCode(t, h), Password: "correct horse battery staple"}, cookies...)
}
func enrollTOTP(t *testing.T, h *HTTPServer, cookie *http.Cookie) (string, []string) {
	t.Helper()
	w := authRequest(t, h, "security/totp/start", map[string]any{}, cookie)
	requireStatus(t, w, 200)
	var setup struct{ Secret string }
	json.Unmarshal(w.Body.Bytes(), &setup)
	code, err := totp.GenerateCode(setup.Secret, h.service.Store.now())
	if err != nil {
		t.Fatal(err)
	}
	w = authRequest(t, h, "security/totp/confirm", map[string]string{"code": code}, cookie)
	requireStatus(t, w, 200)
	var result struct {
		Codes []string `json:"recoveryCodes"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Codes) != 10 {
		t.Fatal("missing recovery codes")
	}
	return setup.Secret, result.Codes
}
func TestAuthSignupRequiresBrowserBoundEmailProof(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	w := authRequest(t, h, "signup", authInput{Email: "new@example.com", Password: "correct horse battery staple"})
	requireStatus(t, w, 202)
	c := authCookie(t, h, w, "challenge")
	code := lastEmailCode(t, h)
	var count int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&count)
	if count != 0 {
		t.Fatal("signup created an account before proof")
	}
	w = authRequest(t, h, "signup/finish", authInput{Code: code, Password: "correct horse battery staple"})
	requireStatus(t, w, 400)
	w = authRequest(t, h, "password/reset/finish", authInput{Code: code, Password: "correct horse battery staple"}, c)
	requireStatus(t, w, 400)
	w = authRequest(t, h, "signup/finish", authInput{Code: code, Password: "correct horse battery staple"}, c)
	requireStatus(t, w, 201)
	session := authCookie(t, h, w, "session")
	if _, err := s.ResolveHuman(ctx, session.Value); err != nil {
		t.Fatal(err)
	}
	w = authRequest(t, h, "signup/finish", authInput{Code: code, Password: "correct horse battery staple"}, c)
	requireStatus(t, w, 400)
}
func TestAuthEmailAttemptLimitAndExpiry(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := authRequest(t, h, "signup/start", authInput{Email: "limit@example.com"})
	requireStatus(t, w, 202)
	c := authCookie(t, h, w, "challenge")
	correct := lastEmailCode(t, h)
	wrong := "000000"
	if correct == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		w = authRequest(t, h, "signup/finish", authInput{Code: wrong, Password: "correct horse battery staple"}, c)
		requireStatus(t, w, 400)
	}
	w = authRequest(t, h, "signup/finish", authInput{Code: correct, Password: "correct horse battery staple"}, c)
	requireStatus(t, w, 400)
	w = authRequest(t, h, "signup/start", authInput{Email: "expires@example.com"})
	requireStatus(t, w, 202)
	c = authCookie(t, h, w, "challenge")
	correct = lastEmailCode(t, h)
	now := s.now()
	s.now = func() time.Time { return now.Add(11 * time.Minute) }
	w = authRequest(t, h, "signup/finish", authInput{Code: correct, Password: "correct horse battery staple"}, c)
	requireStatus(t, w, 400)
}
func TestAuthMFAEnforcedForPasswordOAuthAndRecovery(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	w := signupHTTP(t, h, "mfa@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	p, err := s.ResolveHuman(ctx, cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	_, _, old, err := s.Signin(ctx, p.Account.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	secret, codes := enrollTOTP(t, h, cookie)
	if _, err = s.ResolveHuman(ctx, old); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old session survived enrollment", err)
	}
	_, _, partial, err := s.Signin(ctx, p.Account.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveHuman(ctx, partial); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("password bypassed MFA", err)
	}
	oauth, err := s.signinOAuth(ctx, oauthIdentity{Provider: "github", Subject: "mfa-test", Email: p.Account.Email}, oauthLoginState{LinkAccountID: &p.Account.ID}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveHuman(ctx, oauth); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("OAuth bypassed MFA", err)
	}
	w = authRequest(t, h, "signin", authInput{Email: p.Account.Email, Password: "correct horse battery staple"})
	requireStatus(t, w, 200)
	challenge := authCookie(t, h, w, "challenge")
	// The enrollment step is already spent and cannot be replayed to log in.
	code, _ := totp.GenerateCode(secret, s.now())
	w = authRequest(t, h, "mfa/finish", authInput{Code: code}, challenge)
	requireStatus(t, w, 400)
	now := s.now()
	s.now = func() time.Time { return now.Add(30 * time.Second) }
	code, _ = totp.GenerateCode(secret, s.now())
	w = authRequest(t, h, "mfa/finish", authInput{Code: code}, challenge)
	requireStatus(t, w, 200)
	session := authCookie(t, h, w, "session")
	if _, err = s.ResolveHuman(ctx, session.Value); err != nil {
		t.Fatal(err)
	}
	// Recovery codes are accepted only after a first factor, and only once.
	w = authRequest(t, h, "mfa/finish", authInput{Code: codes[0]})
	requireStatus(t, w, 400)
	for i := 0; i < 2; i++ {
		w = authRequest(t, h, "signin", authInput{Email: p.Account.Email, Password: "correct horse battery staple"})
		requireStatus(t, w, 200)
		challenge = authCookie(t, h, w, "challenge")
		w = authRequest(t, h, "mfa/finish", authInput{Code: codes[0]}, challenge)
		if i == 0 {
			requireStatus(t, w, 200)
		} else {
			requireStatus(t, w, 400)
		}
	}
}
func TestAuthRecoveryRaceHasOneWinner(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := signupHTTP(t, h, "race@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	_, codes := enrollTOTP(t, h, cookie)
	challenges := make([]*http.Cookie, 2)
	for i := range challenges {
		w = authRequest(t, h, "signin", authInput{Email: "race@example.com", Password: "correct horse battery staple"})
		requireStatus(t, w, 200)
		challenges[i] = authCookie(t, h, w, "challenge")
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, c := range challenges {
		wg.Add(1)
		go func(cookie *http.Cookie) {
			defer wg.Done()
			result := authRequest(t, h, "mfa/finish", authInput{Code: codes[0]}, cookie)
			results <- result.Code
		}(c)
	}
	wg.Wait()
	close(results)
	success := 0
	for status := range results {
		if status == 200 {
			success++
		} else if status != 400 {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if success != 1 {
		t.Fatalf("%d recovery race winners", success)
	}
	var used int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM account_recovery_codes WHERE used_at IS NOT NULL`).Scan(&used); err != nil || used != 1 {
		t.Fatal(used, err)
	}
}
func TestAuthResetKeepsMFAButRevokesSessions(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	w := signupHTTP(t, h, "reset@example.com")
	requireStatus(t, w, 201)
	cookie := authCookie(t, h, w, "session")
	_, codes := enrollTOTP(t, h, cookie)
	// Expire the signup email cooldown; reset uses the same shared mail budget.
	now := s.now()
	s.now = func() time.Time { return now.Add(2 * time.Minute) }
	w = authRequest(t, h, "password/reset/start", authInput{Email: "reset@example.com"})
	requireStatus(t, w, 202)
	challenge := authCookie(t, h, w, "challenge")
	code := lastEmailCode(t, h)
	body := authInput{Code: code, Password: "a completely new secure passphrase"}
	w = authRequest(t, h, "password/reset/finish", body, challenge)
	requireStatus(t, w, 400)
	body.Factor = codes[0]
	w = authRequest(t, h, "password/reset/finish", body, challenge)
	requireStatus(t, w, 200)
	if _, err := s.ResolveHuman(ctx, cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("reset retained old session", err)
	}
	w = authRequest(t, h, "signin", authInput{Email: "reset@example.com", Password: body.Password})
	requireStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"mfa"`) {
		t.Fatal("reset disabled MFA")
	}
	w = authRequest(t, h, "password/reset/finish", body, challenge)
	requireStatus(t, w, 400)
}
func TestAuthLegacyUnverifiedAccountsNeedNewPasswordAndProof(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	a, _, old, err := s.Signup(ctx, "legacy@example.com", "previous attacker chosen password", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE accounts SET email_verified_at=NULL WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveHuman(ctx, old); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("unverified session accepted")
	}
	w := authRequest(t, h, "signin", authInput{Email: a.Email, Password: "previous attacker chosen password"})
	requireStatus(t, w, 200)
	c := authCookie(t, h, w, "challenge")
	if !strings.Contains(w.Body.String(), `"verify"`) {
		t.Fatal(w.Body.String())
	}
	w = authRequest(t, h, "verify/finish", authInput{Code: lastEmailCode(t, h), Password: "new mailbox owner passphrase"}, c)
	requireStatus(t, w, 200)
	if _, _, _, err = s.Signin(ctx, a.Email, "previous attacker chosen password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old unverified password survived")
	}
	if _, err = s.ResolveHuman(ctx, old); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old unverified session survived")
	}
}
func TestAuthCrossAccountSessionAndRecentAuthentication(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	a, _, token, err := s.Signup(ctx, "owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, other, err := s.Signup(ctx, "other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ResolveHuman(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: h.humanCookieName(), Value: token}
	w := authMethod(t, h, http.MethodDelete, "security/sessions/"+p.Actor.SessionID, map[string]string{}, cookie)
	requireStatus(t, w, 404)
	if _, err = s.ResolveHuman(ctx, other); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	s.now = func() time.Time { return now.Add(6 * time.Minute) }
	w = authRequest(t, h, "security/totp/start", map[string]string{}, cookie)
	requireStatus(t, w, 428)
	w = authRequest(t, h, "security/reauth", map[string]string{"password": "correct horse battery staple"}, cookie)
	requireStatus(t, w, 200)
	if _, err = s.ResolveHuman(ctx, cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("pre-reauth session token survived")
	}
	cookie = authCookie(t, h, w, "session")
	w = authRequest(t, h, "security/totp/start", map[string]string{}, cookie)
	requireStatus(t, w, 200)
	if a.ID == p.Account.ID {
		t.Fatal("bad fixture")
	}
}
func TestAuthBudgetsSharedAndEmailQueueRetries(t *testing.T) {
	s, h, mail := newAuthTestServer(t)
	ctx := context.Background()
	h2 := NewHTTPServer(&Service{Store: s}, h.config).(*HTTPServer)
	if !h.authBudget(ctx, "test", "account", 1, time.Minute) || h2.authBudget(ctx, "test", "account", 1, time.Minute) {
		t.Fatal("rate budgets not shared")
	}
	w := authRequest(t, h, "signup/start", authInput{Email: "mail@example.com"})
	requireStatus(t, w, 202)
	cookie := authCookie(t, h, w, "challenge")
	code := lastEmailCode(t, h)
	w = authRequest(t, h, "signup/start", authInput{Email: "mail@example.com"}, cookie)
	requireStatus(t, w, 429)
	// Cooldown must not invalidate the earlier emailed code.
	w = authRequest(t, h, "signup/finish", authInput{Code: code, Password: "correct horse battery staple"}, cookie)
	requireStatus(t, w, 201)
	mail.fail = true
	ok, err := h.deliverAuthEmail(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	var status string
	var sealed []byte
	if err = s.pool.QueryRow(ctx, `SELECT status,ciphertext FROM auth_email_outbox LIMIT 1`).Scan(&status, &sealed); err != nil || status != "queued" || len(sealed) == 0 {
		t.Fatal(status, err)
	}
	mail.fail = false
	s.pool.Exec(ctx, `UPDATE auth_email_outbox SET next_attempt_at=now()`)
	ok, err = h.deliverAuthEmail(ctx)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT status,ciphertext FROM auth_email_outbox LIMIT 1`).Scan(&status, &sealed); err != nil || status != "accepted" || sealed != nil {
		t.Fatal(status, err)
	}
}
func TestAuthLoginAndProofRejectForeignOrMissingOrigin(t *testing.T) {
	_, h, _ := newAuthTestServer(t)
	for _, path := range []string{"signin", "signup/start", "signup/finish", "mfa/finish", "password/reset/start", "passkeys/login/begin"} {
		for _, origin := range []string{"", "https://evil.test"} {
			r := httptest.NewRequest(http.MethodPost, "/v1/auth/"+path, strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal(path, origin, w.Code)
			}
		}
	}
}
func TestAuthRegistrationDoesNotRevealExistingEmail(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	_, _, _, err := s.Signup(context.Background(), "existing@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"existing@example.com", "new@example.com"} {
		w := authRequest(t, h, "signup/start", authInput{Email: email})
		requireStatus(t, w, 202)
		if !strings.Contains(w.Body.String(), "If this request can be completed") {
			t.Fatal(w.Body.String())
		}
	}
}
func TestAuthMigrationDoesNotVerifyPasswordAccounts(t *testing.T) {
	s, _, _ := newAuthTestServer(t)
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO accounts(id,email,password_hash) VALUES('legacy-password','migration@example.com','!test')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var verified *time.Time
	if err = s.pool.QueryRow(ctx, `SELECT email_verified_at FROM accounts WHERE id='legacy-password'`).Scan(&verified); err != nil || verified != nil {
		t.Fatal("migration fabricated verification", verified, err)
	}
}
func TestAuthOutboxSecretIsNotPlaintext(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := authRequest(t, h, "signup/start", authInput{Email: "private@example.com"})
	requireStatus(t, w, 202)
	var cipher []byte
	if err := s.pool.QueryRow(context.Background(), `SELECT ciphertext FROM auth_email_outbox`).Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte("private@example.com")) {
		t.Fatal("plaintext email outbox")
	}
	if fmt.Sprint(h.config.Auth.Mailer) == "" {
		t.Fatal("missing fixture")
	}
}

func TestAuthOAuthReauthenticationClearsPreviousSessionBeforeMFA(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	w := signupHTTP(t, h, "oauth-mfa@example.com")
	cookie := authCookie(t, h, w, "session")
	p, err := s.ResolveHuman(context.Background(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	enrollTOTP(t, h, cookie)
	token, err := s.signinOAuth(context.Background(), oauthIdentity{Provider: "github", Subject: "oauth-reauth", Email: p.Account.Email}, oauthLoginState{LinkAccountID: &p.Account.ID}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/auth/oauth/github/callback", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	if h.finishOAuthSecurity(w, r, token, oauthLoginState{Next: "/app/account/security"}) {
		t.Fatal("MFA bypassed")
	}
	requireStatus(t, w, http.StatusSeeOther)
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == cookie.Name && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("previous browser cookie causes redirect loop")
	}
	if _, err = s.ResolveHuman(context.Background(), cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("previous session survived")
	}
	authCookie(t, h, w, "challenge")
}

func TestAuthOAuthLinkGuardRechecksSessionAndInvalidatesOthers(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	w := signupHTTP(t, h, "link-guard@example.com")
	cookie := authCookie(t, h, w, "session")
	p, err := s.ResolveHuman(ctx, cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	_, _, other, err := s.Signin(ctx, p.Account.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/v1/auth/oauth/github/callback", nil)
	r.AddCookie(cookie)
	login := oauthLoginState{LinkAccountID: &p.Account.ID}
	_, err = s.signinOAuth(ctx, oauthIdentity{Provider: "github", Subject: "link-new", Email: p.Account.Email}, login, false, h.oauthLinkGuard(r))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveHuman(ctx, other); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("other session survived link")
	}
	var notified bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_security_events WHERE account_id=$1 AND action='Connected sign-in account added')`, p.Account.ID).Scan(&notified); err != nil || !notified {
		t.Fatal("missing link notification", err)
	}
	if err = s.RevokeHumanSession(ctx, cookie.Value); err != nil {
		t.Fatal(err)
	}
	_, err = s.signinOAuth(ctx, oauthIdentity{Provider: "google", Subject: "link-revoked", Email: p.Account.Email}, login, false, h.oauthLinkGuard(r))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked linking session accepted", err)
	}
	var exists bool
	if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM oauth_identities WHERE provider='google' AND subject='link-revoked')`).Scan(&exists); err != nil || exists {
		t.Fatal("failed link was not rolled back", err)
	}
}
