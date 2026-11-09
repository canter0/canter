package controlplane

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
)

const maxInitialWrenAIAllowanceMicroUSD int64 = 1_000_000_000_000

// SetInitialWrenAIAllowanceMicroUSD is startup/operator policy only. No human
// or model endpoint calls it; changing policy never modifies an existing cap.
func (s *Store) SetInitialWrenAIAllowanceMicroUSD(amount int64) error {
	if amount < 0 || amount > maxInitialWrenAIAllowanceMicroUSD {
		return fmt.Errorf("invalid initial workspace AI allowance policy")
	}
	s.initialAIAllowance.Store(amount)
	return nil
}
func (s *Store) initializeWrenAIAllowanceTx(ctx context.Context, tx pgx.Tx, workspace string) error {
	amount := s.initialAIAllowance.Load()
	if amount == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO wren_ai_allowances(workspace_id,limit_micro_usd)VALUES($1,$2) ON CONFLICT(workspace_id)DO NOTHING`, workspace, amount)
	return err
}

type WrenAIUsageSnapshot struct {
	Configured                 bool   `json:"configured"`
	Enforced                   bool   `json:"enforced"`
	Currency                   string `json:"currency"`
	Unit                       string `json:"unit"`
	LimitMicroUSD              int64  `json:"limitMicroUsd"`
	HeldMicroUSD               int64  `json:"heldMicroUsd"`
	SpentMicroUSD              int64  `json:"spentMicroUsd"`
	RemainingMicroUSD          int64  `json:"remainingMicroUsd"`
	EstimatedHoldsMicroUSD     int64  `json:"estimatedHoldsMicroUsd"`
	ReservedEstimateMicroUSD   int64  `json:"reservedEstimateMicroUsd"`
	DispatchedEstimateMicroUSD int64  `json:"dispatchedEstimateMicroUsd"`
	UncertainEstimateMicroUSD  int64  `json:"uncertainEstimateMicroUsd"`
	Closed                     bool   `json:"closed"`
	ComputeIncluded            bool   `json:"computeIncluded"`
}

// One statement yields a consistent snapshot while other requests reserve or
// settle. Holds are estimates; only spentMicroUsd is settled provider cost.
func (s *Store) WrenAIUsageSnapshot(ctx context.Context, workspace string) (WrenAIUsageSnapshot, error) {
	out := WrenAIUsageSnapshot{Currency: "USD", Unit: "micro_usd"}
	err := s.pool.QueryRow(ctx, `SELECT a.limit_micro_usd,a.held_micro_usd,a.spent_micro_usd,a.closed_at IS NOT NULL,COALESCE(x.reserved,0),COALESCE(x.dispatched,0),COALESCE(x.uncertain,0) FROM wren_ai_allowances a LEFT JOIN LATERAL (SELECT SUM(amount_micro_usd)FILTER(WHERE state='reserved') AS reserved,SUM(amount_micro_usd)FILTER(WHERE state='dispatched') AS dispatched,SUM(amount_micro_usd)FILTER(WHERE state='uncertain') AS uncertain FROM wren_ai_reservations WHERE workspace_id=a.workspace_id)x ON true WHERE a.workspace_id=$1`, workspace).Scan(&out.LimitMicroUSD, &out.HeldMicroUSD, &out.SpentMicroUSD, &out.Closed, &out.ReservedEstimateMicroUSD, &out.DispatchedEstimateMicroUSD, &out.UncertainEstimateMicroUSD)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Configured = true
	out.EstimatedHoldsMicroUSD = out.HeldMicroUSD
	if !out.Closed && out.SpentMicroUSD <= out.LimitMicroUSD && out.HeldMicroUSD <= out.LimitMicroUSD-out.SpentMicroUSD {
		out.RemainingMicroUSD = out.LimitMicroUSD - out.SpentMicroUSD - out.HeldMicroUSD
	}
	return out, nil
}

func (h *HTTPServer) workspaceWrenAIUsage(w http.ResponseWriter, r *http.Request, p Principal, workspace string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if _, err := h.service.Store.Membership(r.Context(), p.Account.ID, workspace); err != nil {
		writeStoreError(w, err)
		return
	}
	out, err := h.service.Store.WrenAIUsageSnapshot(r.Context(), workspace)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out.Enforced = h.config.AIUsage.Enforce
	writeJSON(w, http.StatusOK, out)
}
