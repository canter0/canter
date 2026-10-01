package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
)

type passkeyUser struct {
	account     authAccount
	credentials []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { return []byte(u.account.ID) }
func (u passkeyUser) WebAuthnName() string                       { return u.account.Email }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.account.Email }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
func newPasskeys(publicURL string) (*webauthn.WebAuthn, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("invalid passkey origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackOriginHost(u.Hostname())) {
		return nil, errors.New("passkeys require HTTPS")
	}
	return webauthn.New(&webauthn.Config{RPDisplayName: "Canter", RPID: u.Hostname(), RPOrigins: []string{u.Scheme + "://" + u.Host}, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}, AttestationPreference: protocol.PreferNoAttestation})
}
func (h *HTTPServer) passkeyUserTx(ctx context.Context, tx pgx.Tx, account string) (passkeyUser, error) {
	a, err := authAccountTx(ctx, tx, account)
	u := passkeyUser{account: a}
	if err != nil {
		return u, err
	}
	rows, err := tx.Query(ctx, `SELECT id,key_id,ciphertext FROM account_passkeys WHERE account_id=$1 ORDER BY created_at`, account)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key string
		var sealed []byte
		if err = rows.Scan(&id, &key, &sealed); err != nil {
			return u, err
		}
		raw, e := h.config.Secrets.open(key, sealed, []byte("canter:passkey:"+account+":"+id))
		if e != nil {
			return u, e
		}
		var c webauthn.Credential
		e = json.Unmarshal(raw, &c)
		clear(raw)
		if e != nil {
			return u, e
		}
		u.credentials = append(u.credentials, c)
	}
	return u, rows.Err()
}
func (h *HTTPServer) passkeyAuth(w http.ResponseWriter, r *http.Request, route string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return
	}
	if h.passkeys == nil || h.config.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("passkeys are not configured"))
		return
	}
	if !h.allowAuthBudget(w, r, "passkey-ip", requestIP(r), 60, 10*time.Minute) {
		return
	}
	register := strings.HasPrefix(route, "register/")
	reauth := strings.HasPrefix(route, "reauth/")
	if route != "login/begin" && route != "login/finish" && route != "register/begin" && route != "register/finish" && route != "reauth/begin" && route != "reauth/finish" {
		writeStoreError(w, ErrNotFound)
		return
	}
	var p Principal
	var err error
	if register || reauth {
		p, err = h.human(r)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if register && !h.requireRecent(w, r, p) {
			return
		}
	}
	ctx := r.Context()
	purpose := "passkey-login"
	if register {
		purpose = "passkey-register"
	}
	if reauth {
		purpose = "passkey-reauth"
	}
	token := h.challengeToken(r, "passkey")
	if strings.HasSuffix(route, "/finish") {
		if err = h.attemptChallenge(ctx, token, purpose); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if strings.HasSuffix(route, "/begin") {
		var in struct {
			Name string `json:"name"`
		}
		if !decodeLimit(w, r, &in, 1024) {
			return
		}
		var options any
		var session *webauthn.SessionData
		c := authChallenge{Purpose: purpose}
		if register || reauth {
			user, e := h.passkeyUserTx(ctx, tx, p.Account.ID)
			if e != nil {
				writeStoreError(w, e)
				return
			}
			c.AccountID = &user.account.ID
			c.Version = user.account.Version
			c.SessionID = &p.Actor.SessionID
			if register {
				if len(user.credentials) >= 20 {
					writeError(w, http.StatusBadRequest, errors.New("remove a passkey before adding another"))
					return
				}
				options, session, err = h.passkeys.BeginRegistration(user, webauthn.WithExclusions(webauthn.Credentials(user.credentials).CredentialDescriptors()))
			} else {
				options, session, err = h.passkeys.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
			}
		} else {
			options, session, err = h.passkeys.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("could not start passkey verification"))
			return
		}
		if in.Name == "" {
			in.Name = "Passkey"
		}
		if len(in.Name) > 80 {
			writeError(w, http.StatusBadRequest, errors.New("passkey name is too long"))
			return
		}
		c.Payload, _ = json.Marshal(struct {
			Session *webauthn.SessionData `json:"session"`
			Name    string                `json:"name"`
		}{session, in.Name})
		// A separate cookie keeps conditional passkey autofill from overwriting an
		// email or MFA challenge. All ceremonies are browser-bound and one-use.
		if old := h.challengeToken(r, "passkey"); old != "" {
			if err = consumeChallengeTx(ctx, tx, old); err != nil {
				writeStoreError(w, err)
				return
			}
		}
		token, e := newChallengeTx(ctx, tx, c, 5*time.Minute, h.service.Store.now())
		if e == nil {
			e = tx.Commit(ctx)
		}
		if e != nil {
			writeStoreError(w, e)
			return
		}
		h.setAuthCookie(w, "passkey", token, 300)
		writeJSON(w, http.StatusOK, options)
		return
	}
	c, err := challengeTx(ctx, tx, token, purpose, h.service.Store.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var payload struct {
		Session webauthn.SessionData `json:"session"`
		Name    string               `json:"name"`
	}
	if json.Unmarshal(c.Payload, &payload) != nil {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var user passkeyUser
	var credential *webauthn.Credential
	if register || reauth {
		if c.AccountID == nil || c.SessionID == nil || *c.AccountID != p.Account.ID || *c.SessionID != p.Actor.SessionID {
			writeStoreError(w, ErrUnauthorized)
			return
		}
		user, err = h.passkeyUserTx(ctx, tx, p.Account.ID)
		if err != nil || user.account.Version != c.Version {
			writeStoreError(w, ErrUnauthorized)
			return
		}
		var live bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM human_sessions WHERE id=$1 AND revoked_at IS NULL AND expires_at>$2 AND auth_version=$3)`, p.Actor.SessionID, h.service.Store.now(), user.account.Version).Scan(&live)
		if err != nil || !live {
			writeStoreError(w, ErrUnauthorized)
			return
		}
		if register {
			credential, err = h.passkeys.FinishRegistration(user, payload.Session, r)
		} else {
			credential, err = h.passkeys.FinishLogin(user, payload.Session, r)
		}
	} else {
		var resolved webauthn.User
		resolved, credential, err = h.passkeys.FinishPasskeyLogin(func(rawID, handle []byte) (webauthn.User, error) {
			var account string
			e := tx.QueryRow(ctx, `SELECT account_id FROM account_passkeys WHERE credential_id=$1`, rawID).Scan(&account)
			if e != nil || !bytes.Equal(handle, []byte(account)) {
				return nil, ErrUnauthorized
			}
			u, e := h.passkeyUserTx(ctx, tx, account)
			if e != nil || u.account.Verified == nil {
				return nil, ErrUnauthorized
			}
			return u, nil
		}, payload.Session, r)
		if err == nil {
			user = resolved.(passkeyUser)
		}
	}
	if err != nil || credential == nil || credential.Authenticator.CloneWarning {
		writeError(w, http.StatusBadRequest, errors.New("passkey verification failed; try again"))
		return
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer clear(raw)
	var id string
	if register {
		id, err = newID("pk_")
	} else {
		err = tx.QueryRow(ctx, `SELECT id FROM account_passkeys WHERE credential_id=$1 AND account_id=$2`, credential.ID, user.account.ID).Scan(&id)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	key, sealed, err := h.config.Secrets.seal(raw, []byte("canter:passkey:"+user.account.ID+":"+id))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if register {
		_, err = tx.Exec(ctx, `INSERT INTO account_passkeys(id,account_id,credential_id,name,key_id,ciphertext) VALUES($1,$2,$3,$4,$5,$6)`, id, user.account.ID, credential.ID, payload.Name, key, sealed)
		if err == nil {
			err = h.invalidateAuthTx(ctx, tx, user.account.ID, p.Actor.SessionID)
		}
		if err == nil {
			err = h.securityEventTx(ctx, tx, user.account, "Passkey added")
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE account_passkeys SET key_id=$2,ciphertext=$3,last_used_at=$4 WHERE id=$1`, id, key, sealed, h.service.Store.now())
	}
	var authToken string
	if err == nil && !register {
		if reauth {
			authToken, err = h.rotateSessionTx(ctx, tx, p.Actor.SessionID, true)
		} else {
			authToken, err = h.issueSessionTx(ctx, tx, user.account, true, r)
		}
	}
	if err == nil {
		err = consumeChallengeTx(ctx, tx, token)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.setAuthCookie(w, "passkey", "", -1)
	if authToken != "" {
		h.authSuccess(w, r, authToken)
	} else {
		writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
	}
}
