package controlplane

import (
	"context"
	"net/http"
)

func (s *Store) onboardingComplete(ctx context.Context, accountID string) (bool, error) {
	var complete bool
	err := s.pool.QueryRow(ctx, `SELECT onboarding_completed_at IS NOT NULL FROM accounts WHERE id=$1`, accountID).Scan(&complete)
	return complete, err
}

func (h *HTTPServer) completeOnboarding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err := h.service.Store.pool.Exec(r.Context(), `UPDATE accounts SET onboarding_completed_at=COALESCE(onboarding_completed_at,$2) WHERE id=$1`, p.Account.ID, h.service.Store.now()); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
