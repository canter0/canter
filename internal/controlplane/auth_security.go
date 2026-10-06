package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const authLifetime = 7 * 24 * time.Hour

var errAuthExpired = errors.New("verification expired or invalid; start again")
var errAuthUnavailable = errors.New("account security is not configured")

// Authentication settings contain server-only credentials. Never serialize them.
type AuthConfig struct {
	PasswordDenylist map[[32]byte]struct{}
	Mailer           AuthMailer
	TurnstileSecret  string
	TurnstileSiteKey string
	WebhookSecret    string
}

type authChallenge struct {
	Purpose   string
	AccountID *string
	Email     string
	CodeHash  []byte
	Version   int64
	SessionID *string
	Payload   json.RawMessage
}

type authAccount struct {
	ID, Email, Password string
	Verified            *time.Time
	Version             int64
	MFA                 bool
}

func authAccountTx(ctx context.Context, tx pgx.Tx, id string) (a authAccount, err error) {
	err = tx.QueryRow(ctx, `SELECT id,email,password_hash,email_verified_at,auth_version,EXISTS(SELECT 1 FROM account_totp WHERE account_id=a.id AND enabled_at IS NOT NULL) FROM accounts a WHERE id=$1 AND disabled_at IS NULL FOR UPDATE`, id).Scan(&a.ID, &a.Email, &a.Password, &a.Verified, &a.Version, &a.MFA)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrUnauthorized
	}
	return
}
func authCodeHash(token, code string) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(code))
	return mac.Sum(nil)
}
func (h *HTTPServer) authCookieName(kind string) string {
	if h.config.CookieSecure {
		return "__Host-canter_" + kind
	}
	return "canter_" + kind
}
func (h *HTTPServer) setAuthCookie(w http.ResponseWriter, kind, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: h.authCookieName(kind), Value: value, Path: "/", HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (h *HTTPServer) challengeToken(r *http.Request, kind string) string {
	c, err := r.Cookie(h.authCookieName(kind))
	if err != nil {
		return ""
	}
	return c.Value
}
func newChallengeTx(ctx context.Context, tx pgx.Tx, c authChallenge, ttl time.Duration, now time.Time) (string, error) {
	token, err := newSecret("", 32)
	if err != nil {
		return "", err
	}
	if len(c.Payload) == 0 {
		c.Payload = json.RawMessage(`{}`)
	}
	_, err = tx.Exec(ctx, `INSERT INTO auth_challenges(token_hash,purpose,account_id,email,code_hash,auth_version,session_id,payload,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, secretHash(token), c.Purpose, c.AccountID, c.Email, c.CodeHash, c.Version, c.SessionID, c.Payload, now.Add(ttl))
	return token, err
}

// Charge attempts outside the validation transaction, so rolling back an invalid
// proof cannot roll back its attempt count. The proof is consumed under row lock.
func (h *HTTPServer) attemptChallenge(ctx context.Context, token, purpose string) error {
	var count int
	err := h.service.Store.pool.QueryRow(ctx, `UPDATE auth_challenges SET attempts=attempts+1 WHERE token_hash=$1 AND purpose=$2 AND expires_at>$3 AND attempts<5 RETURNING attempts`, secretHash(token), purpose, h.service.Store.now()).Scan(&count)
	if err != nil {
		return errAuthExpired
	}
	return nil
}
func challengeTx(ctx context.Context, tx pgx.Tx, token, purpose string, now time.Time) (c authChallenge, err error) {
	// Always lock the account before its challenge, matching credential changes.
	if _, err = tx.Exec(ctx, `SELECT id FROM accounts WHERE id=(SELECT account_id FROM auth_challenges WHERE token_hash=$1) FOR UPDATE`, secretHash(token)); err != nil {
		return c, err
	}
	err = tx.QueryRow(ctx, `SELECT purpose,account_id,email,code_hash,auth_version,session_id,payload FROM auth_challenges WHERE token_hash=$1 AND purpose=$2 AND expires_at>$3 AND attempts<=5 FOR UPDATE`, secretHash(token), purpose, now).Scan(&c.Purpose, &c.AccountID, &c.Email, &c.CodeHash, &c.Version, &c.SessionID, &c.Payload)
	if err != nil {
		err = errAuthExpired
	}
	return
}
func consumeChallengeTx(ctx context.Context, tx pgx.Tx, token string) error {
	_, err := tx.Exec(ctx, `DELETE FROM auth_challenges WHERE token_hash=$1`, secretHash(token))
	return err
}
func authEventTx(ctx context.Context, tx pgx.Tx, account, action string) error {
	id, err := newID("ase_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO account_security_events(id,account_id,action) VALUES($1,$2,$3)`, id, account, action)
	return err
}
func (h *HTTPServer) issueSessionTx(ctx context.Context, tx pgx.Tx, a authAccount, mfa bool, r *http.Request) (string, error) {
	if a.Verified == nil || (a.MFA && !mfa) {
		return "", ErrUnauthorized
	}
	token, err := newSecret("chs_", 32)
	if err != nil {
		return "", err
	}
	id, err := newID("hss_")
	if err != nil {
		return "", err
	}
	now := h.service.Store.now()
	var verified *time.Time
	if mfa {
		verified = &now
	}
	_, err = tx.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,created_at,last_seen_at,expires_at,auth_version,authenticated_at,mfa_verified_at,user_agent,ip_address) VALUES($1,$2,$3,$4,$4,$5,$6,$4,$7,$8,$9)`, id, a.ID, secretHash(token), now, now.Add(authLifetime), a.Version, verified, truncateAuth(r.UserAgent(), 300), requestIP(r))
	if err == nil {
		err = authEventTx(ctx, tx, a.ID, "Signed in")
	}
	return token, err
}
func truncateAuth(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Rotate the bearer token after step-up, so a copied pre-verification token
// cannot inherit the browser's newly elevated session.
func (h *HTTPServer) rotateSessionTx(ctx context.Context, tx pgx.Tx, session string, mfa bool) (string, error) {
	token, err := newSecret("chs_", 32)
	if err != nil {
		return "", err
	}
	now := h.service.Store.now()
	_, err = tx.Exec(ctx, `UPDATE human_sessions SET token_hash=$2,authenticated_at=$3,expires_at=$4,mfa_verified_at=CASE WHEN $5 THEN $3 ELSE mfa_verified_at END WHERE id=$1`, session, secretHash(token), now, now.Add(authLifetime), mfa)
	return token, err
}
func (h *HTTPServer) authSuccess(w http.ResponseWriter, r *http.Request, token string) {
	if old, err := h.humanCookie(r); err == nil && old.Value != token {
		_ = h.service.Store.RevokeHumanSession(r.Context(), old.Value)
	}
	h.setHumanCookie(w, token, authLifetime)
	h.setAuthCookie(w, "challenge", "", -1)
	h.claimAcquisition(r, token)
	writeJSON(w, http.StatusOK, map[string]any{"stage": "complete"})
}
func (h *HTTPServer) emailReady() bool { return h.config.Auth.Mailer != nil && h.config.Secrets != nil }

// Persistent budgets are shared by all control-plane instances. The original
// in-process limiter remains an inexpensive first layer. Never store raw emails.
func (h *HTTPServer) authBudget(ctx context.Context, bucket, key string, limit int, window time.Duration) bool {
	return h.authBudgetQuery(ctx, h.service.Store.pool, bucket, key, limit, window)
}

func (h *HTTPServer) authBudgetQuery(ctx context.Context, q querier, bucket, key string, limit int, window time.Duration) bool {
	now := h.service.Store.now()
	var n int
	err := q.QueryRow(ctx, `INSERT INTO auth_rate_windows(bucket_hash,count,expires_at) VALUES($1,1,$2) ON CONFLICT(bucket_hash) DO UPDATE SET count=CASE WHEN auth_rate_windows.expires_at<=$3 THEN 1 ELSE auth_rate_windows.count+1 END,expires_at=CASE WHEN auth_rate_windows.expires_at<=$3 THEN $2 ELSE auth_rate_windows.expires_at END RETURNING count`, secretHash(bucket+"\x00"+key), now.Add(window), now).Scan(&n)
	return err == nil && n <= limit
}
func (h *HTTPServer) allowAuthBudget(w http.ResponseWriter, r *http.Request, bucket, key string, limit int, window time.Duration) bool {
	if h.authBudget(r.Context(), bucket, key, limit, window) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	writeError(w, http.StatusTooManyRequests, errors.New("too many attempts; wait before trying again"))
	return false
}
func (h *HTTPServer) recentAuth(ctx context.Context, p Principal) bool {
	var ok bool
	err := h.service.Store.pool.QueryRow(ctx, `SELECT authenticated_at>$3 AND revoked_at IS NULL AND expires_at>$4 AND auth_version=(SELECT auth_version FROM accounts WHERE id=$2) FROM human_sessions WHERE id=$1 AND account_id=$2`, p.Actor.SessionID, p.Account.ID, h.service.Store.now().Add(-5*time.Minute), h.service.Store.now()).Scan(&ok)
	return err == nil && ok
}
func (h *HTTPServer) requireRecent(w http.ResponseWriter, r *http.Request, p Principal) bool {
	if h.recentAuth(r.Context(), p) {
		return true
	}
	writeError(w, http.StatusPreconditionRequired, errors.New("confirm your identity in Account security before continuing"))
	return false
}
func (h *HTTPServer) invalidateAuthTx(ctx context.Context, tx pgx.Tx, account, keepSession string) error {
	_, err := tx.Exec(ctx, `UPDATE accounts SET auth_version=auth_version+1 WHERE id=$1`, account)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE human_sessions SET revoked_at=CASE WHEN id=$2 THEN revoked_at ELSE $3 END,auth_version=CASE WHEN id=$2 THEN (SELECT auth_version FROM accounts WHERE id=$1) ELSE auth_version END WHERE account_id=$1`, account, keepSession, h.service.Store.now())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM auth_challenges WHERE account_id=$1`, account)
	return err
}
func (h *HTTPServer) queueCodeTx(ctx context.Context, tx pgx.Tx, c authChallenge, old string) (string, error) {
	if !h.emailReady() {
		return "", errAuthUnavailable
	}
	if old != "" {
		if err := consumeChallengeTx(ctx, tx, old); err != nil {
			return "", err
		}
	}
	token, err := newChallengeTx(ctx, tx, c, 10*time.Minute, h.service.Store.now())
	if err != nil {
		return "", err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	_, err = tx.Exec(ctx, `UPDATE auth_challenges SET code_hash=$2 WHERE token_hash=$1`, secretHash(token), authCodeHash(token, code))
	if err != nil {
		return "", err
	}
	subject := "Your Canter verification code"
	body := "Your Canter code is " + code + ". It expires in 10 minutes. Enter it only on Canter. If you did not request this, ignore this email."
	if c.Purpose == accountDeletionPurpose {
		subject = "Confirm deletion of your Canter account"
		body = "Your Canter account deletion code is " + code + ". It expires in 10 minutes. Entering this code and confirming on Canter permanently deletes your account. If you did not request deletion, do not share this code and review your account security."
	}
	err = h.queueEmailTx(ctx, tx, c.AccountID, c.Email, subject, body, 10*time.Minute)
	return token, err
}
func validEmailCode(c authChallenge, token, code string) bool {
	return len(code) == 6 && subtle.ConstantTimeCompare(c.CodeHash, authCodeHash(token, code)) == 1
}
func recoveryHash(code string) []byte {
	return secretHash(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", "")))
}
func recoveryCodesTx(ctx context.Context, tx pgx.Tx, account string) ([]string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM account_recovery_codes WHERE account_id=$1`, account); err != nil {
		return nil, err
	}
	codes := make([]string, 10)
	for i := range codes {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		s := strings.ToUpper(hex.EncodeToString(raw))
		codes[i] = s[:8] + "-" + s[8:16] + "-" + s[16:]
		if _, err := tx.Exec(ctx, `INSERT INTO account_recovery_codes(account_id,code_hash) VALUES($1,$2)`, account, recoveryHash(codes[i])); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

func (h *HTTPServer) clearPreviousLogin(w http.ResponseWriter, r *http.Request) {
	if old, err := h.humanCookie(r); err == nil {
		_ = h.service.Store.RevokeHumanSession(r.Context(), old.Value)
	}
	h.setHumanCookie(w, "", -time.Hour)
}
