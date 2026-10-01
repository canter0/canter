package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

func (h *HTTPServer) verifyFactorTx(ctx context.Context, tx pgx.Tx, account, code string) error {
	var key string
	var sealed []byte
	var last int64
	err := tx.QueryRow(ctx, `SELECT key_id,ciphertext,last_step FROM account_totp WHERE account_id=$1 AND enabled_at IS NOT NULL FOR UPDATE`, account).Scan(&key, &sealed, &last)
	if err != nil {
		return ErrUnauthorized
	}
	if len(code) != 6 {
		result, err := tx.Exec(ctx, `UPDATE account_recovery_codes SET used_at=$3 WHERE account_id=$1 AND code_hash=$2 AND used_at IS NULL`, account, recoveryHash(code), h.service.Store.now())
		if err == nil && result.RowsAffected() == 1 {
			return authEventTx(ctx, tx, account, "Recovery code used")
		}
		return ErrUnauthorized
	}
	if h.config.Secrets == nil {
		return errAuthUnavailable
	}
	raw, err := h.config.Secrets.open(key, sealed, []byte("canter:totp:"+account))
	if err != nil {
		return err
	}
	defer clear(raw)
	step, ok := acceptedTOTPStep(string(raw), code, h.service.Store.now(), last)
	if !ok {
		return ErrUnauthorized
	}
	_, err = tx.Exec(ctx, `UPDATE account_totp SET last_step=$2 WHERE account_id=$1`, account, step)
	return err
}
func acceptedTOTPStep(secret, code string, now time.Time, last int64) (int64, bool) {
	current := now.Unix() / 30
	for _, step := range []int64{current, current - 1, current + 1} {
		if step <= last {
			continue
		}
		valid, err := totp.ValidateCustom(code, secret, time.Unix(step*30, 0), totp.ValidateOpts{Period: 30, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && valid {
			return step, true
		}
	}
	return 0, false
}
func (h *HTTPServer) accountSecurity(w http.ResponseWriter, r *http.Request, parts []string) {
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	route := strings.Join(parts, "/")
	if r.Method == http.MethodGet && route == "" {
		h.securityOverview(w, r, p)
		return
	}
	if !isUnsafeMethod(r.Method) || !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return
	}
	if !h.allowAuthBudget(w, r, "security-account", p.Account.ID, 30, 10*time.Minute) {
		return
	}
	if route != "reauth" && !strings.HasPrefix(route, "sessions/") && !h.requireRecent(w, r, p) {
		return
	}
	var in struct {
		Password string `json:"password"`
		Code     string `json:"code"`
		Name     string `json:"name"`
	}
	if !decodeLimit(w, r, &in, 8192) {
		return
	}
	if route == "reauth" && !h.allowAuthBudget(w, r, "reauth-account", p.Account.ID, 10, 15*time.Minute) {
		return
	}
	ctx := r.Context()
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	a, err := authAccountTx(ctx, tx, p.Account.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var current bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM human_sessions WHERE id=$1 AND account_id=$2 AND revoked_at IS NULL AND expires_at>$3 AND auth_version=$4)`, p.Actor.SessionID, a.ID, h.service.Store.now(), a.Version).Scan(&current)
	if err != nil || !current {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	response := map[string]any{"saved": true}
	action := ""
	var rotatedToken string
	switch {
	case route == "reauth" && r.Method == http.MethodPost:
		if strings.HasPrefix(a.Password, "$argon2id$") {
			if !verifyPassword(a.Password, in.Password) {
				writeStoreError(w, ErrUnauthorized)
				return
			}
		} else if !a.MFA {
			writeError(w, http.StatusBadRequest, errors.New("use a passkey or sign in again with your connected provider"))
			return
		}
		if a.MFA {
			if err = h.verifyFactorTx(ctx, tx, a.ID, in.Code); err != nil {
				writeStoreError(w, ErrUnauthorized)
				return
			}
		}
		rotatedToken, err = h.rotateSessionTx(ctx, tx, p.Actor.SessionID, a.MFA)
	case route == "totp/start" && r.Method == http.MethodPost:
		if h.config.Secrets == nil {
			writeError(w, http.StatusServiceUnavailable, errAuthUnavailable)
			return
		}
		if a.MFA {
			writeError(w, http.StatusConflict, errors.New("remove the existing authenticator before adding another"))
			return
		}
		key, e := totp.Generate(totp.GenerateOpts{Issuer: "Canter", AccountName: a.Email, Period: 30, SecretSize: 20, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if e != nil {
			writeStoreError(w, e)
			return
		}
		keyID, sealed, e := h.config.Secrets.seal([]byte(key.Secret()), []byte("canter:totp:"+a.ID))
		if e != nil {
			writeStoreError(w, e)
			return
		}
		_, err = tx.Exec(ctx, `INSERT INTO account_totp(account_id,key_id,ciphertext,pending_until) VALUES($1,$2,$3,$4) ON CONFLICT(account_id) DO UPDATE SET key_id=$2,ciphertext=$3,pending_until=$4,last_step=-1 WHERE account_totp.enabled_at IS NULL`, a.ID, keyID, sealed, h.service.Store.now().Add(10*time.Minute))
		img, e := key.Image(256, 256)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		var buf bytes.Buffer
		if e = png.Encode(&buf, img); e != nil {
			writeStoreError(w, e)
			return
		}
		response = map[string]any{"secret": key.Secret(), "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())}
	case route == "totp/confirm" && r.Method == http.MethodPost:
		var keyID string
		var sealed []byte
		err = tx.QueryRow(ctx, `SELECT key_id,ciphertext FROM account_totp WHERE account_id=$1 AND enabled_at IS NULL AND pending_until>$2 FOR UPDATE`, a.ID, h.service.Store.now()).Scan(&keyID, &sealed)
		if err != nil {
			writeError(w, http.StatusBadRequest, errAuthExpired)
			return
		}
		raw, e := h.config.Secrets.open(keyID, sealed, []byte("canter:totp:"+a.ID))
		if e != nil {
			writeStoreError(w, e)
			return
		}
		step, ok := acceptedTOTPStep(string(raw), in.Code, h.service.Store.now(), -1)
		clear(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("invalid authenticator code"))
			return
		}
		_, err = tx.Exec(ctx, `UPDATE account_totp SET enabled_at=$2,pending_until=NULL,last_step=$3 WHERE account_id=$1`, a.ID, h.service.Store.now(), step)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE human_sessions SET mfa_verified_at=$2 WHERE id=$1`, p.Actor.SessionID, h.service.Store.now())
		}
		if err == nil {
			response["recoveryCodes"], err = recoveryCodesTx(ctx, tx, a.ID)
		}
		action = "Authenticator enabled"
	case route == "totp" && r.Method == http.MethodDelete:
		_, err = tx.Exec(ctx, `DELETE FROM account_totp WHERE account_id=$1`, a.ID)
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM account_recovery_codes WHERE account_id=$1`, a.ID)
		}
		action = "Authenticator removed"
	case route == "recovery-codes" && r.Method == http.MethodPost:
		if !a.MFA {
			writeError(w, http.StatusBadRequest, errors.New("enable an authenticator first"))
			return
		}
		response["recoveryCodes"], err = recoveryCodesTx(ctx, tx, a.ID)
		action = "Recovery codes replaced"
	case route == "password" && r.Method == http.MethodPost:
		if e := h.checkPassword(in.Password); e != nil {
			writeError(w, http.StatusBadRequest, e)
			return
		}
		password, e := hashPassword(in.Password)
		if e != nil {
			writeError(w, http.StatusBadRequest, e)
			return
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET password_hash=$2 WHERE id=$1`, a.ID, password)
		action = "Password changed"
	case strings.HasPrefix(route, "passkeys/") && r.Method == http.MethodDelete:
		result, e := tx.Exec(ctx, `DELETE FROM account_passkeys WHERE id=$1 AND account_id=$2`, strings.TrimPrefix(route, "passkeys/"), a.ID)
		err = e
		if err == nil && result.RowsAffected() != 1 {
			err = ErrNotFound
		}
		action = "Passkey removed"
	case strings.HasPrefix(route, "passkeys/") && r.Method == http.MethodPatch:
		name := strings.TrimSpace(in.Name)
		if name == "" || len(name) > 80 {
			writeError(w, http.StatusBadRequest, errors.New("use a passkey name between 1 and 80 characters"))
			return
		}
		result, e := tx.Exec(ctx, `UPDATE account_passkeys SET name=$3 WHERE id=$1 AND account_id=$2`, strings.TrimPrefix(route, "passkeys/"), a.ID, name)
		err = e
		if err == nil && result.RowsAffected() != 1 {
			err = ErrNotFound
		}
	case strings.HasPrefix(route, "sessions/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(route, "sessions/")
		if id == "others" {
			_, err = tx.Exec(ctx, `UPDATE human_sessions SET revoked_at=$3 WHERE account_id=$1 AND id<>$2 AND revoked_at IS NULL`, a.ID, p.Actor.SessionID, h.service.Store.now())
		} else {
			result, e := tx.Exec(ctx, `UPDATE human_sessions SET revoked_at=$3 WHERE account_id=$1 AND id=$2`, a.ID, id, h.service.Store.now())
			err = e
			if err == nil && result.RowsAffected() != 1 {
				err = ErrNotFound
			}
		}
		if err == nil {
			err = authEventTx(ctx, tx, a.ID, "Sessions signed out")
		}
	default:
		writeStoreError(w, ErrNotFound)
		return
	}
	if err == nil && action != "" {
		err = h.invalidateAuthTx(ctx, tx, a.ID, p.Actor.SessionID)
		if err == nil {
			err = h.securityEventTx(ctx, tx, a, action)
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if rotatedToken != "" {
		h.setHumanCookie(w, rotatedToken, authLifetime)
	}
	writeJSON(w, http.StatusOK, response)
}
func (h *HTTPServer) securityOverview(w http.ResponseWriter, r *http.Request, p Principal) {
	ctx := r.Context()
	var enabled, hasPassword bool
	var remaining int
	err := h.service.Store.pool.QueryRow(ctx, `SELECT password_hash LIKE '$argon2id$%',EXISTS(SELECT 1 FROM account_totp WHERE account_id=a.id AND enabled_at IS NOT NULL),(SELECT count(*) FROM account_recovery_codes WHERE account_id=a.id AND used_at IS NULL) FROM accounts a WHERE id=$1`, p.Account.ID).Scan(&hasPassword, &enabled, &remaining)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	sessions := []map[string]any{}
	rows, err := h.service.Store.pool.Query(ctx, `SELECT id,created_at,last_seen_at,expires_at,user_agent,ip_address FROM human_sessions WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>$2 AND auth_version=(SELECT auth_version FROM accounts WHERE id=$1) AND (mfa_verified_at IS NOT NULL OR NOT $3) ORDER BY last_seen_at DESC LIMIT 100`, p.Account.ID, h.service.Store.now(), enabled)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	for rows.Next() {
		var id, ua, ip string
		var created, seen, expires time.Time
		if err = rows.Scan(&id, &created, &seen, &expires, &ua, &ip); err != nil {
			break
		}
		sessions = append(sessions, map[string]any{"id": id, "createdAt": created, "lastSeenAt": seen, "expiresAt": expires, "userAgent": ua, "ip": ip, "current": id == p.Actor.SessionID})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	passkeys := []map[string]any{}
	rows, err = h.service.Store.pool.Query(ctx, `SELECT id,name,created_at,last_used_at FROM account_passkeys WHERE account_id=$1 ORDER BY created_at`, p.Account.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	for rows.Next() {
		var id, name string
		var created time.Time
		var used *time.Time
		if err = rows.Scan(&id, &name, &created, &used); err != nil {
			break
		}
		passkeys = append(passkeys, map[string]any{"id": id, "name": name, "createdAt": created, "lastUsedAt": used})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	events := []map[string]any{}
	rows, err = h.service.Store.pool.Query(ctx, `SELECT action,created_at FROM account_security_events WHERE account_id=$1 ORDER BY created_at DESC LIMIT 30`, p.Account.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	for rows.Next() {
		var action string
		var at time.Time
		if err = rows.Scan(&action, &at); err != nil {
			break
		}
		events = append(events, map[string]any{"action": action, "at": at})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	providers := []string{}
	rows, err = h.service.Store.pool.Query(ctx, `SELECT provider FROM oauth_identities WHERE account_id=$1 ORDER BY provider`, p.Account.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		if h.oauth[name] != nil {
			providers = append(providers, name)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"totp": enabled, "totpAvailable": h.config.Secrets != nil, "passkeysAvailable": h.passkeys != nil && h.config.Secrets != nil, "hasPassword": hasPassword, "recoveryCodesRemaining": remaining, "recentAuth": h.recentAuth(ctx, p), "sessions": sessions, "passkeys": passkeys, "events": events, "providers": providers})
}
