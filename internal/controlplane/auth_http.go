package controlplane

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type authInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
	Factor   string `json:"factor"`
	Invite   string `json:"inviteKey"`
	BotToken string `json:"botToken"`
}

func (h *HTTPServer) securityAuth(w http.ResponseWriter, r *http.Request, parts []string) bool {
	route := strings.Join(parts, "/")
	if route == "email-webhook" {
		h.authEmailWebhook(w, r)
		return true
	}
	if route == "config" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"email": h.emailReady(), "passkeys": h.passkeys != nil && h.config.Secrets != nil, "totp": h.config.Secrets != nil, "turnstileSiteKey": h.config.Auth.TurnstileSiteKey})
		return true
	}
	if route == "challenge" && r.Method == http.MethodGet {
		var purpose, email string
		err := h.service.Store.pool.QueryRow(r.Context(), `SELECT purpose,email FROM auth_challenges WHERE token_hash=$1 AND expires_at>$2 AND attempts<5`, secretHash(h.challengeToken(r, "challenge")), h.service.Store.now()).Scan(&purpose, &email)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]string{"stage": "start"})
		} else {
			writeJSON(w, http.StatusOK, map[string]string{"stage": purpose, "email": email})
		}
		return true
	}
	switch route {
	case "signup", "signup/start", "signup/finish", "signin", "verify/finish", "mfa/finish", "password/reset/start", "password/reset/finish":
	default:
		if strings.HasPrefix(route, "security") {
			h.accountSecurity(w, r, parts[1:])
			return true
		}
		if strings.HasPrefix(route, "passkeys/") {
			h.passkeyAuth(w, r, strings.TrimPrefix(route, "passkeys/"))
			return true
		}
		return false
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return true
	}
	if !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return true
	}
	var in authInput
	if !decodeLimit(w, r, &in, 8192) {
		return true
	}
	if !h.allowAuthBudget(w, r, "auth-ip", requestIP(r), 60, 10*time.Minute) {
		return true
	}
	switch route {
	case "signup", "signup/start", "password/reset/start":
		h.startEmailAuth(w, r, in, route == "password/reset/start")
	case "signup/finish", "verify/finish", "password/reset/finish":
		h.finishEmailAuth(w, r, in, strings.Split(route, "/")[0])
	case "signin":
		email, err := normalizeEmail(in.Email)
		if err != nil {
			email = strings.ToLower(in.Email)
		}
		if !h.allowAuthBudget(w, r, "password-account", email, 15, 15*time.Minute) {
			return true
		}
		if !h.verifyBot(w, r, in.BotToken) {
			return true
		}
		_, _, token, err := h.service.Store.Signin(r.Context(), email, in.Password)
		if err != nil {
			writeStoreError(w, err)
			return true
		}
		stage, challenge, err := h.afterPrimaryLogin(r, token)
		if err != nil {
			writeStoreError(w, err)
			return true
		}
		if stage == "complete" {
			h.authSuccess(w, r, challenge)
		} else {
			h.clearPreviousLogin(w, r)
			h.setAuthCookie(w, "challenge", challenge, 600)
			writeJSON(w, http.StatusOK, map[string]string{"stage": stage, "email": email})
		}
	case "mfa/finish":
		h.finishMFA(w, r, in.Code)
	}
	return true
}
func (h *HTTPServer) startEmailAuth(w http.ResponseWriter, r *http.Request, in authInput, reset bool) {
	if !h.emailReady() {
		writeError(w, http.StatusServiceUnavailable, errors.New("email verification is temporarily unavailable"))
		return
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !h.verifyBot(w, r, in.BotToken) {
		return
	}
	if !h.allowAuthBudget(w, r, "email-ip", requestIP(r), 20, time.Hour) {
		return
	}
	// A neutral response also covers the per-address cooldown, so it does not
	// disclose whether an account exists or whether another browser requested mail.
	allowed := h.authBudget(r.Context(), "email-cooldown", email, 1, time.Minute) && h.authBudget(r.Context(), "email-hour", email, 5, time.Hour)
	if !allowed {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, errors.New("wait one minute before requesting another code"))
		return
	}
	tx, err := h.service.Store.pool.Begin(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	var version int64
	lookup := tx.QueryRow(r.Context(), `SELECT id,auth_version FROM accounts WHERE email=$1 AND disabled_at IS NULL`, email).Scan(&id, &version)
	if lookup != nil && !errors.Is(lookup, pgx.ErrNoRows) {
		writeStoreError(w, lookup)
		return
	}
	purpose := "signup"
	if reset {
		purpose = "reset"
	}
	c := authChallenge{Purpose: purpose, Email: email, Version: version}
	if reset && id != "" {
		c.AccountID = &id
	}
	old := h.challengeToken(r, "challenge")
	var token string
	if allowed && ((reset && id != "") || (!reset && id == "")) {
		token, err = h.queueCodeTx(r.Context(), tx, c, old)
	} else {
		// Dummy browser challenge has the same shape and lifetime, but no usable code.
		if old != "" {
			err = consumeChallengeTx(r.Context(), tx, old)
		}
		if err == nil {
			token, err = newChallengeTx(r.Context(), tx, c, 10*time.Minute, h.service.Store.now())
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.setAuthCookie(w, "challenge", token, 600)
	writeJSON(w, http.StatusAccepted, map[string]string{"stage": purpose, "email": email, "message": "If this request can be completed, a code will arrive shortly. Wait one minute before requesting another."})
}
func (h *HTTPServer) finishEmailAuth(w http.ResponseWriter, r *http.Request, in authInput, purpose string) {
	if purpose == "password" {
		purpose = "reset"
	}
	token := h.challengeToken(r, "challenge")
	if err := h.attemptChallenge(r.Context(), token, purpose); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.checkPassword(in.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	password, err := hashPassword(in.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tx, err := h.service.Store.pool.Begin(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	c, err := challengeTx(r.Context(), tx, token, purpose, h.service.Store.now())
	if err != nil || !validEmailCode(c, token, in.Code) {
		writeError(w, http.StatusBadRequest, errAuthExpired)
		return
	}
	var session string
	var signupAccount Account
	var signupWorkspace Workspace
	if purpose == "signup" {
		signupAccount, signupWorkspace, session, err = h.service.Store.signupVerifiedTx(r.Context(), tx, c.Email, password, in.Invite, h.config.RequireInvite)
		if err == nil {
			err = authEventTx(r.Context(), tx, signupAccount.ID, "Email verified and account created")
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), `UPDATE human_sessions SET user_agent=$2,ip_address=$3 WHERE token_hash=$1`, secretHash(session), truncateAuth(r.UserAgent(), 300), requestIP(r))
		}
	} else {
		if c.AccountID == nil {
			writeError(w, http.StatusBadRequest, errAuthExpired)
			return
		}
		a, e := authAccountTx(r.Context(), tx, *c.AccountID)
		if e != nil || a.Version != c.Version {
			writeError(w, http.StatusBadRequest, errAuthExpired)
			return
		}
		if a.MFA {
			if e = h.verifyFactorTx(r.Context(), tx, a.ID, in.Factor); e != nil {
				writeError(w, http.StatusBadRequest, errors.New("enter a valid authenticator or recovery code"))
				return
			}
		}
		_, err = tx.Exec(r.Context(), `UPDATE accounts SET password_hash=$2,email_verified_at=COALESCE(email_verified_at,$3) WHERE id=$1`, a.ID, password, h.service.Store.now())
		if err == nil {
			err = h.invalidateAuthTx(r.Context(), tx, a.ID, "")
		}
		if err == nil {
			err = h.securityEventTx(r.Context(), tx, a, "Password changed")
		}
		if err == nil && purpose == "verify" {
			now := h.service.Store.now()
			a.Verified = &now
			a.Version++
			session, err = h.issueSessionTx(r.Context(), tx, a, a.MFA, r)
		}
	}
	if err == nil {
		err = consumeChallengeTx(r.Context(), tx, token)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("could not finish verification; sign in or request a new code"))
		return
	}
	if purpose == "reset" {
		h.setHumanCookie(w, "", -time.Hour)
		h.setAuthCookie(w, "challenge", "", -1)
		writeJSON(w, http.StatusOK, map[string]string{"stage": "reset-complete"})
		return
	}
	if purpose == "signup" {
		h.setHumanCookie(w, session, authLifetime)
		h.setAuthCookie(w, "challenge", "", -1)
		h.claimAcquisition(r, session)
		writeJSON(w, http.StatusCreated, map[string]any{"stage": "complete", "account": signupAccount, "workspace": signupWorkspace})
		return
	}
	h.authSuccess(w, r, session)
}

// Provisional sessions cannot resolve as humans until email and MFA policy pass.
func (h *HTTPServer) afterPrimaryLogin(r *http.Request, token string) (stage, next string, err error) {
	ctx := r.Context()
	var account, email string
	var version int64
	var unverified bool
	err = h.service.Store.pool.QueryRow(ctx, `SELECT hs.account_id,hs.auth_version,a.email,a.email_verified_at IS NULL FROM human_sessions hs JOIN accounts a ON a.id=hs.account_id WHERE hs.token_hash=$1 AND hs.revoked_at IS NULL AND hs.expires_at>$2`, secretHash(token), h.service.Store.now()).Scan(&account, &version, &email, &unverified)
	if err != nil {
		return "", "", ErrUnauthorized
	}
	if unverified && (!h.authBudget(ctx, "email-cooldown", email, 1, time.Minute) || !h.authBudget(ctx, "email-hour", email, 5, time.Hour)) {
		return "", "", errors.New("wait before requesting another verification email")
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	a, err := authAccountTx(ctx, tx, account)
	if err != nil || version != a.Version {
		return "", "", ErrUnauthorized
	}
	if _, err = tx.Exec(ctx, `DELETE FROM human_sessions WHERE token_hash=$1`, secretHash(token)); err != nil {
		return "", "", err
	}
	c := authChallenge{AccountID: &a.ID, Email: a.Email, Version: a.Version}
	switch {
	case a.Verified == nil:
		c.Purpose = "verify"
		stage = "verify"
		next, err = h.queueCodeTx(ctx, tx, c, h.challengeToken(r, "challenge"))
	case a.MFA:
		c.Purpose = "mfa"
		stage = "mfa"
		next, err = newChallengeTx(ctx, tx, c, 5*time.Minute, h.service.Store.now())
	default:
		stage = "complete"
		next, err = h.issueSessionTx(ctx, tx, a, false, r)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return
}
func (h *HTTPServer) finishMFA(w http.ResponseWriter, r *http.Request, code string) {
	token := h.challengeToken(r, "challenge")
	if err := h.attemptChallenge(r.Context(), token, "mfa"); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var account string
	if err := h.service.Store.pool.QueryRow(r.Context(), `SELECT account_id FROM auth_challenges WHERE token_hash=$1 AND purpose='mfa'`, secretHash(token)).Scan(&account); err != nil {
		writeError(w, http.StatusBadRequest, errAuthExpired)
		return
	}
	if !h.allowAuthBudget(w, r, "mfa-account", account, 15, 15*time.Minute) {
		return
	}
	tx, err := h.service.Store.pool.Begin(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	c, err := challengeTx(r.Context(), tx, token, "mfa", h.service.Store.now())
	if err != nil || c.AccountID == nil {
		writeError(w, http.StatusBadRequest, errAuthExpired)
		return
	}
	a, err := authAccountTx(r.Context(), tx, *c.AccountID)
	if err != nil || a.Version != c.Version {
		writeError(w, http.StatusBadRequest, errAuthExpired)
		return
	}
	if err = h.verifyFactorTx(r.Context(), tx, a.ID, code); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid authenticator or recovery code"))
		return
	}
	session, err := h.issueSessionTx(r.Context(), tx, a, true, r)
	if err == nil {
		err = consumeChallengeTx(r.Context(), tx, token)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.authSuccess(w, r, session)
}
func (h *HTTPServer) finishOAuthSecurity(w http.ResponseWriter, r *http.Request, token string, login oauthLoginState) bool {
	stage, next, err := h.afterPrimaryLogin(r, token)
	if err != nil {
		h.oauthFailure(w, r, "sign_in_failed", login.Mode, login.Next)
		return false
	}
	if stage != "complete" {
		h.clearPreviousLogin(w, r)
		h.setAuthCookie(w, "challenge", next, 600)
		w.Header().Del("Content-Type")
		http.Redirect(w, r, strings.TrimRight(h.config.PublicURL, "/")+"/sign-in?next="+url.QueryEscape(safeOAuthNext(login.Next, login.Mode)), http.StatusSeeOther)
		return false
	}
	if old, err := h.humanCookie(r); err == nil && old.Value != next {
		_ = h.service.Store.RevokeHumanSession(r.Context(), old.Value)
	}
	h.setHumanCookie(w, next, authLifetime)
	h.claimAcquisition(r, next)
	return true
}
