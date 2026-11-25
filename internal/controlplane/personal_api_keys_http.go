package controlplane

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// personalAPIKeys serves the browser-only management surface for account API
// keys. It deliberately authenticates through h.human: a bearer credential --
// including a valid personal key -- can never mint, list, or revoke keys.
func (h *HTTPServer) personalAPIKeys(w http.ResponseWriter, r *http.Request, parts []string) {
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if isUnsafeMethod(r.Method) && !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return
	}
	if isUnsafeMethod(r.Method) && !h.allowAuthBudget(w, r, "api-keys-account", p.Account.ID, 30, 10*time.Minute) {
		return
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			keys, err := h.service.Store.ListPersonalAPIKeys(r.Context(), p.Account.ID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
		case http.MethodPost:
			h.createPersonalAPIKey(w, r, p)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		// Revocation matches session sign-out usability: it does not require
		// step-up authentication, so a leaked session cannot block cleanup.
		if err := h.service.Store.RevokePersonalAPIKey(r.Context(), p.Account.ID, parts[0]); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) createPersonalAPIKey(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expiresInDays"`
	}
	if !decodeLimit(w, r, &in, 4096) {
		return
	}
	name, err := validatePersonalAPIKeyName(in.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	days, err := validatePersonalAPIKeyExpiry(in.ExpiresInDays)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !h.requireRecent(w, r, p) {
		return
	}
	cookie, err := h.humanCookie(r)
	if err != nil || cookie.Value == "" {
		writeStoreError(w, ErrUnauthorized)
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
	if a.Verified == nil {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	// Recheck the browser session inside the account lock, taking the session
	// row FOR UPDATE so a creation can never commit after the session was
	// revoked or credentials rotated in a concurrent transaction. The
	// recent-auth precondition is revalidated here, not trusted from the
	// handler precheck alone.
	var session string
	err = tx.QueryRow(ctx, `SELECT id FROM human_sessions WHERE id=$1 AND account_id=$2 AND token_hash=$3 AND revoked_at IS NULL AND expires_at>$4 AND authenticated_at>$5 AND auth_version=$6 AND (mfa_verified_at IS NOT NULL OR NOT $7) FOR UPDATE`, p.Actor.SessionID, a.ID, secretHash(cookie.Value), h.service.Store.now(), h.service.Store.now().Add(-5*time.Minute), a.Version, a.MFA).Scan(&session)
	if errors.Is(err, pgx.ErrNoRows) {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	key, secret, err := h.service.Store.createPersonalAPIKeyTx(ctx, tx, a, name, days)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
}
