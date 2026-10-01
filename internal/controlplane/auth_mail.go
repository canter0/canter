package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type AuthEmail struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}
type AuthMailer interface {
	Send(context.Context, string, AuthEmail) (string, error)
}
type ResendMailer struct {
	APIKey, From string
	Client       *http.Client
}
type permanentMailError struct{}

func (permanentMailError) Error() string { return "email delivery rejected" }
func (m *ResendMailer) Send(ctx context.Context, id string, email AuthEmail) (string, error) {
	body, _ := json.Marshal(map[string]any{"from": m.From, "to": []string{email.To}, "subject": email.Subject, "text": email.Text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", id)
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", errors.New("email delivery temporarily unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == 429 || res.StatusCode >= 500 {
		return "", errors.New("email delivery temporarily unavailable")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", permanentMailError{}
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&out) != nil || out.ID == "" {
		return "", errors.New("invalid email delivery response")
	}
	return out.ID, nil
}
func (h *HTTPServer) queueEmailTx(ctx context.Context, tx pgx.Tx, account *string, to, subject, body string, ttl time.Duration) error {
	if !h.emailReady() {
		return errAuthUnavailable
	}
	id, err := newID("mail_")
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(AuthEmail{to, subject, body})
	key, sealed, err := h.config.Secrets.seal(raw, []byte("canter:auth-mail:"+id))
	clear(raw)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO auth_email_outbox(id,key_id,ciphertext,account_id,expires_at) VALUES($1,$2,$3,$4,$5)`, id, key, sealed, account, h.service.Store.now().Add(ttl))
	return err
}
func (h *HTTPServer) securityEventTx(ctx context.Context, tx pgx.Tx, a authAccount, action string) error {
	if err := authEventTx(ctx, tx, a.ID, action); err != nil {
		return err
	}
	if !h.emailReady() {
		return nil
	}
	return h.queueEmailTx(ctx, tx, &a.ID, a.Email, "Canter account security changed", action+" on your Canter account. If this wasn't you, sign in at Canter, review your sessions, and secure your account.", time.Hour)
}

// The outbox lock prevents concurrent dispatch. Resend's idempotency key covers
// a process crash after acceptance but before the transaction commits.
func (h *HTTPServer) deliverAuthEmail(ctx context.Context) (bool, error) {
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, key string
	var sealed []byte
	var attempts int
	err = tx.QueryRow(ctx, `SELECT id,key_id,ciphertext,attempts FROM auth_email_outbox WHERE status='queued' AND next_attempt_at<=now() AND expires_at>now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &key, &sealed, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := h.config.Secrets.open(key, sealed, []byte("canter:auth-mail:"+id))
	if err != nil {
		return false, err
	}
	defer clear(raw)
	var mail AuthEmail
	if json.Unmarshal(raw, &mail) != nil {
		return false, errors.New("invalid queued email")
	}
	var suppressed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM auth_email_suppressions WHERE email_hash=$1)`, secretHash(strings.ToLower(mail.To))).Scan(&suppressed); err != nil {
		return false, err
	}
	status, provider := "suppressed", ""
	if !suppressed {
		provider, err = h.config.Auth.Mailer.Send(ctx, id, mail)
		status = "accepted"
	}
	attempts++
	if err != nil {
		status = "queued"
		var permanent permanentMailError
		if errors.As(err, &permanent) || attempts >= 6 {
			status = "failed"
		}
	}
	var next any = sealed
	if status != "queued" {
		next = nil
	}
	_, err = tx.Exec(ctx, `UPDATE auth_email_outbox SET status=$2,provider_id=NULLIF($3,''),attempts=$4,next_attempt_at=now()+$5*interval '1 second',ciphertext=$6 WHERE id=$1`, id, status, provider, attempts, 5*(1<<attempts), next)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func (h *HTTPServer) RunAuthMaintenance(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if h.emailReady() {
				for i := 0; i < 10; i++ {
					ok, err := h.deliverAuthEmail(ctx)
					if err != nil || !ok {
						break
					}
				}
			}
			_, err := h.service.Store.pool.Exec(ctx, `DELETE FROM auth_challenges WHERE expires_at<now(); DELETE FROM account_totp WHERE enabled_at IS NULL AND pending_until<now(); DELETE FROM auth_rate_windows WHERE expires_at<now(); UPDATE auth_email_outbox SET status='expired',ciphertext=NULL WHERE status='queued' AND expires_at<=now(); DELETE FROM auth_email_outbox WHERE created_at<now()-interval '30 days'; DELETE FROM account_security_events WHERE created_at<now()-interval '90 days'`)
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
}
func (h *HTTPServer) authEmailWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	if !validResendSignature(h.config.Auth.WebhookSecret, r.Header, body, time.Now()) {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	var event struct {
		Type string `json:"type"`
		Data struct {
			EmailID string   `json:"email_id"`
			To      []string `json:"to"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &event) != nil {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	status := map[string]string{"email.delivered": "delivered", "email.bounced": "bounced", "email.complained": "complained", "email.failed": "failed"}[event.Type]
	if status != "" {
		tx, err := h.service.Store.pool.Begin(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		result, err := tx.Exec(r.Context(), `UPDATE auth_email_outbox SET status=$2 WHERE provider_id=$1 AND status NOT IN ('bounced','complained')`, event.Data.EmailID, status)
		if err == nil && result.RowsAffected() > 0 && (status == "bounced" || status == "complained") {
			for _, email := range event.Data.To {
				_, err = tx.Exec(r.Context(), `INSERT INTO auth_email_suppressions(email_hash) VALUES($1) ON CONFLICT DO NOTHING`, secretHash(strings.ToLower(email)))
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true})
}
func validResendSignature(secret string, headers http.Header, body []byte, now time.Time) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) < 16 {
		return false
	}
	stamp := headers.Get("svix-timestamp")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || now.Sub(time.Unix(seconds, 0)) > 5*time.Minute || time.Unix(seconds, 0).Sub(now) > 5*time.Minute {
		return false
	}
	id := headers.Get("svix-id")
	if id == "" {
		return false
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%s.", id, stamp)
	mac.Write(body)
	for _, signature := range strings.Fields(headers.Get("svix-signature")) {
		parts := strings.SplitN(signature, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(parts[1])
		if err == nil && hmac.Equal(got, mac.Sum(nil)) {
			return true
		}
	}
	return false
}
