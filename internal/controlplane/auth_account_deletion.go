package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const accountDeletionPurpose = "delete-account"

type accountDeletionProof struct {
	Factor string `json:"factor"`
}

type accountDeletionBlocked struct{ message string }

func (e accountDeletionBlocked) Error() string { return e.message }

func (h *HTTPServer) accountDeletion(w http.ResponseWriter, r *http.Request, route string) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if r.Header.Get("Authorization") != "" {
		writeStoreError(w, ErrForbidden)
		return
	}
	if !(route == "" && (r.Method == http.MethodGet || r.Method == http.MethodDelete)) &&
		!((route == "/start" || route == "/finish") && r.Method == http.MethodPost) {
		methodNotAllowed(w)
		return
	}
	if r.Method != http.MethodGet && !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return
	}
	var in struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if r.Method == http.MethodPost {
		if !decodeLimit(w, r, &in, 8192) || !h.allowAuthBudget(w, r, "delete-account", p.Account.ID, 15, 15*time.Minute) ||
			!h.allowAuthBudget(w, r, "delete-ip", requestIP(r), 60, 15*time.Minute) {
			return
		}
	}
	token := h.challengeToken(r, "deletion")
	if route == "/finish" {
		if err = h.attemptChallenge(r.Context(), token, accountDeletionPurpose); err != nil {
			writeError(w, http.StatusGone, err)
			return
		}
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
	var authenticatedAt time.Time
	err = tx.QueryRow(ctx, `SELECT authenticated_at FROM human_sessions WHERE id=$1 AND account_id=$2 AND revoked_at IS NULL AND expires_at>$3 AND auth_version=$4 AND (mfa_verified_at IS NOT NULL OR NOT $5) FOR UPDATE`, p.Actor.SessionID, a.ID, h.service.Store.now(), a.Version, a.MFA).Scan(&authenticatedAt)
	if err != nil {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	recent := authenticatedAt.After(h.service.Store.now().Add(-5 * time.Minute))
	if r.Method == http.MethodDelete {
		_, err = tx.Exec(ctx, `DELETE FROM auth_challenges WHERE account_id=$1 AND session_id=$2 AND purpose=$3`, a.ID, p.Actor.SessionID, accountDeletionPurpose)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		h.setAuthCookie(w, "deletion", "", -1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	factor := "email"
	if a.MFA {
		factor = "mfa"
	}
	hasPassword := strings.HasPrefix(a.Password, "$argon2id$")
	workspaces, blocked := h.service.Store.accountDeletionWorkspacesTx(ctx, tx, a.ID)
	if blocked != nil {
		var policy accountDeletionBlocked
		if !errors.As(blocked, &policy) {
			writeStoreError(w, blocked)
			return
		}
	}
	if r.Method == http.MethodGet {
		var passkey bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_passkeys WHERE account_id=$1)`, a.ID).Scan(&passkey); err != nil {
			writeStoreError(w, err)
			return
		}
		providers := []string{}
		rows, queryErr := tx.Query(ctx, `SELECT provider FROM oauth_identities WHERE account_id=$1 ORDER BY provider`, a.ID)
		if queryErr != nil {
			writeStoreError(w, queryErr)
			return
		}
		for rows.Next() {
			var provider string
			if err = rows.Scan(&provider); err != nil {
				break
			}
			if h.oauth[provider] != nil {
				providers = append(providers, provider)
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
		message := ""
		if blocked != nil {
			message = blocked.Error()
		} else if factor == "email" && !h.emailReady() {
			message = "Email verification is temporarily unavailable. Try again later."
		}
		writeJSON(w, http.StatusOK, map[string]any{"hasPassword": hasPassword, "factor": factor, "email": a.Email, "blocked": message,
			"recentAuth": recent, "hasPasskey": passkey && h.passkeys != nil && h.config.Secrets != nil, "providers": providers})
		return
	}
	if blocked != nil {
		writeError(w, http.StatusConflict, blocked)
		return
	}
	if route == "/start" {
		if hasPassword {
			valid, verifyErr := verifyPasswordLimited(a.Password, in.Password)
			if verifyErr != nil {
				writeAuthStoreError(w, verifyErr)
				return
			}
			if !valid {
				writeError(w, http.StatusUnauthorized, errors.New("incorrect password"))
				return
			}
		} else if !recent {
			writeError(w, http.StatusPreconditionRequired, errors.New("confirm your identity with your existing sign-in method before continuing"))
			return
		}
		if factor == "email" {
			if !h.emailReady() {
				writeError(w, http.StatusServiceUnavailable, errors.New("email verification is temporarily unavailable"))
				return
			}
			if !h.authBudgetQuery(ctx, tx, "delete-email-cooldown", a.ID, 1, time.Minute) || !h.authBudgetQuery(ctx, tx, "delete-email-hour", a.ID, 5, time.Hour) {
				w.Header().Set("Retry-After", "60")
				writeError(w, http.StatusTooManyRequests, errors.New("wait before requesting another deletion code"))
				return
			}
		}
		// A new password proof supersedes only this session's deletion proof.
		if _, err = tx.Exec(ctx, `DELETE FROM auth_challenges WHERE account_id=$1 AND session_id=$2 AND purpose=$3`, a.ID, p.Actor.SessionID, accountDeletionPurpose); err != nil {
			writeStoreError(w, err)
			return
		}
		payload, _ := json.Marshal(accountDeletionProof{Factor: factor})
		c := authChallenge{Purpose: accountDeletionPurpose, AccountID: &a.ID, Email: a.Email, Version: a.Version, SessionID: &p.Actor.SessionID, Payload: payload}
		if factor == "mfa" {
			token, err = newChallengeTx(ctx, tx, c, 10*time.Minute, h.service.Store.now())
		} else {
			token, err = h.queueCodeTx(ctx, tx, c, "")
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		h.setAuthCookie(w, "deletion", token, 600)
		writeJSON(w, http.StatusAccepted, map[string]string{"factor": factor, "email": a.Email})
		return
	}
	c, err := challengeTx(ctx, tx, token, accountDeletionPurpose, h.service.Store.now())
	var proof accountDeletionProof
	if err != nil || c.AccountID == nil || *c.AccountID != a.ID || c.SessionID == nil || *c.SessionID != p.Actor.SessionID ||
		c.Version != a.Version || c.Email != a.Email || json.Unmarshal(c.Payload, &proof) != nil || proof.Factor != factor {
		writeError(w, http.StatusGone, errAuthExpired)
		return
	}
	code := strings.TrimSpace(in.Code)
	if factor == "mfa" {
		err = h.verifyFactorTx(ctx, tx, a.ID, code)
	} else if !validEmailCode(c, token, code) {
		err = ErrUnauthorized
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid verification code"))
		return
	}
	if err = h.deleteAccountTx(ctx, tx, a, workspaces); err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.setHumanCookie(w, "", -time.Hour)
	h.setAuthCookie(w, "deletion", "", -1)
	h.setAuthCookie(w, "challenge", "", -1)
	writeJSON(w, http.StatusOK, map[string]string{"stage": "deleted"})
}

// Lock every membership's workspace before checking ownership or resources. FK
// inserts cannot introduce new resources while final deletion holds these locks.
func (s *Store) accountDeletionWorkspacesTx(ctx context.Context, tx pgx.Tx, account string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT w.id,w.name,m.role FROM workspaces w JOIN memberships m ON m.workspace_id=w.id WHERE m.account_id=$1 ORDER BY w.id FOR UPDATE OF w`, account)
	if err != nil {
		return nil, err
	}
	type workspace struct{ id, name, role string }
	var memberships []workspace
	for rows.Next() {
		var item workspace
		if err = rows.Scan(&item.id, &item.name, &item.role); err != nil {
			break
		}
		memberships = append(memberships, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	var private []string
	for _, item := range memberships {
		var members, owners int
		if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE m.role='owner' AND a.disabled_at IS NULL) FROM memberships m JOIN accounts a ON a.id=m.account_id WHERE m.workspace_id=$1 AND m.account_id<>$2`, item.id, account).Scan(&members, &owners); err != nil {
			return nil, err
		}
		if members > 0 {
			if item.role == "owner" && owners == 0 {
				return nil, accountDeletionBlocked{fmt.Sprintf("Transfer ownership of workspace %q before deleting your account. Contact support if you need help.", item.name)}
			}
			continue
		}
		// These rows may represent real resources even when an execution failed.
		var resources bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM systems WHERE workspace_id=$1) OR EXISTS(SELECT 1 FROM node_installations WHERE workspace_id=$1 AND revoked_at IS NULL) OR EXISTS(SELECT 1 FROM initial_deployment_executions WHERE workspace_id=$1 AND phase IN ('queued','running')) OR EXISTS(SELECT 1 FROM workspace_vps WHERE workspace_id=$1 AND phase NOT IN ('drafted','deleted'))`, item.id).Scan(&resources); err != nil {
			return nil, err
		}
		if resources {
			return nil, accountDeletionBlocked{fmt.Sprintf("Close or transfer hosted resources in workspace %q before deleting your account. Contact support to arrange removal.", item.name)}
		}
		if _, err = tx.Exec(ctx, `SELECT workspace_id FROM workspace_billing WHERE workspace_id=$1 FOR UPDATE`, item.id); err != nil {
			return nil, err
		}
		var billing bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_billing WHERE workspace_id=$1 AND ((subscription_id IS NOT NULL AND status NOT IN ('canceled','incomplete_expired')) OR checkout_expires_at>$2)) OR EXISTS(SELECT 1 FROM billing_usage_events WHERE workspace_id=$1 AND sent_at IS NULL)`, item.id, s.now()).Scan(&billing); err != nil {
			return nil, err
		}
		if billing {
			return nil, accountDeletionBlocked{fmt.Sprintf("Close active billing for workspace %q before deleting your account. Contact support if a payment still needs reconciliation.", item.name)}
		}
		private = append(private, item.id)
	}
	return private, nil
}
