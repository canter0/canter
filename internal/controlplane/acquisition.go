package controlplane

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

const acquisitionLifetime = 30 * 24 * time.Hour

func (h *HTTPServer) acquisitionCookieName() string {
	if h.config.CookieSecure {
		return "__Host-canter_acquisition"
	}
	return "canter_acquisition"
}

func acquisitionOptOut(r *http.Request) bool {
	return r.Header.Get("DNT") == "1" || r.Header.Get("Sec-GPC") == "1"
}

func (h *HTTPServer) acquisition(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/acquisition" {
		writeStoreError(w, ErrNotFound)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !h.trustedHumanOrigin(r) {
		writeStoreError(w, ErrForbidden)
		return
	}
	if acquisitionOptOut(r) {
		http.SetCookie(w, &http.Cookie{Name: h.acquisitionCookieName(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Existing customers are not new acquisition visitors.
	if _, err := h.humanCookie(r); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var in struct {
		Source      string `json:"source"`
		LandingPath string `json:"landingPath"`
	}
	if !decodeLimit(w, r, &in, 512) {
		return
	}
	validSource := false
	for _, source := range []string{"direct", "google", "bing", "duckduckgo", "yahoo", "referral", "paid", "campaign"} {
		validSource = validSource || in.Source == source
	}
	if !validSource || (in.LandingPath != "/" && in.LandingPath != "/pricing") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid acquisition category"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if cookie, err := r.Cookie(h.acquisitionCookieName()); err == nil && len(cookie.Value) <= 100 {
		var exists bool
		err = h.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM acquisition_visits WHERE token_hash=$1 AND expires_at>$2)`, secretHash(cookie.Value), h.service.Store.now()).Scan(&exists)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if exists {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	token, err := newSecret("cav_", 32)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	now := h.service.Store.now()
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	// Expired identifiers are removed on capture; durable reports retain only
	// aggregate visitor counts and attribution for accounts that actually signed up.
	if _, err = tx.Exec(ctx, `DELETE FROM acquisition_visits WHERE expires_at<=$1`, now); err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO acquisition_visits(token_hash,source,landing_path,occurred_at,expires_at) VALUES($1,$2,$3,$4,$5)`, secretHash(token), in.Source, in.LandingPath, now, now.Add(acquisitionLifetime))
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO acquisition_daily(day,source,landing_path,visitors) VALUES(($1::timestamptz AT TIME ZONE 'UTC')::date,$2,$3,1) ON CONFLICT(day,source,landing_path) DO UPDATE SET visitors=acquisition_daily.visitors+1`, now, in.Source, in.LandingPath)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: h.acquisitionCookieName(), Value: token, Path: "/", HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: int(acquisitionLifetime.Seconds()), Expires: now.Add(acquisitionLifetime)})
	w.WriteHeader(http.StatusNoContent)
}

// Attribution is best effort and never changes authentication's outcome. The
// session identifies the account; a client cannot submit an account/workspace ID.
// Only a visit predating a newly created account can claim its original workspace.
func (h *HTTPServer) claimAcquisition(r *http.Request, session string) {
	if acquisitionOptOut(r) {
		return
	}
	cookie, err := r.Cookie(h.acquisitionCookieName())
	if err != nil || len(cookie.Value) > 100 {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	_, err = h.service.Store.pool.Exec(ctx, `INSERT INTO account_acquisition(account_id,workspace_id,visit_hash,source,landing_path,occurred_at,signed_up_at)
        SELECT a.id,w.id,v.token_hash,v.source,v.landing_path,v.occurred_at,a.created_at
        FROM acquisition_visits v CROSS JOIN human_sessions s
        JOIN accounts a ON a.id=s.account_id
        JOIN LATERAL (SELECT ws.id FROM workspaces ws JOIN memberships m ON m.workspace_id=ws.id
            WHERE m.account_id=a.id AND m.role='owner' AND ws.created_at>=a.created_at
            ORDER BY ws.created_at,ws.id LIMIT 1) w ON true
        WHERE v.token_hash=$1 AND s.token_hash=$2 AND s.revoked_at IS NULL AND s.expires_at>$3
          AND v.expires_at>$3 AND v.occurred_at<=a.created_at AND a.created_at>=$3-interval '5 minutes'
        ON CONFLICT DO NOTHING`, secretHash(cookie.Value), secretHash(session), h.service.Store.now())
	if err != nil {
		log.Print("acquisition attribution unavailable")
	}
}
