package controlplane

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
)

func requireSandboxHistoryBilling(ctx context.Context, tx pgx.Tx, workspace string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_billing WHERE workspace_id=$1 AND status='active')`, workspace).Scan(&active); err != nil {
		return err
	}
	if !active {
		return fmt.Errorf("%w: templates and extra snapshot history require active billing", ErrConflict)
	}
	return nil
}

// reserveSandboxHistoryTx checks an estimated 720-hour storage cost against
// the existing organization budget. It is an admission reservation, not an
// invoice: actual retained byte-seconds are metered separately. One aggregate
// reservation avoids reserving a whole cent for every tiny saved copy.
//
// The caller MUST hold the workspace row lock. Change the history/template
// references first, then call this before committing; a capacity error rolls
// back both the reference change and its budget effect. Removing references is
// always allowed, including after billing is inactive. Mandatory current sleep
// saves do not participate in this extra-history admission check.
func (s *Store) reserveSandboxHistoryTx(ctx context.Context, tx pgx.Tx, workspace string) error {
	var wanted int64
	if err := tx.QueryRow(ctx, `SELECT ceil(COALESCE(sum(p.stored_bytes),0)::numeric*14/10000000000)::bigint
FROM sandbox_snapshots p WHERE p.workspace_id=$1 AND p.state='ready' AND
(p.retained OR EXISTS(
 SELECT 1 FROM sandbox_template_versions v JOIN sandbox_templates t
  ON t.workspace_id=v.workspace_id AND t.id=v.template_id
 WHERE v.workspace_id=p.workspace_id AND v.snapshot_id=p.id
  AND v.deleted_at IS NULL AND t.deleted_at IS NULL
))`, workspace).Scan(&wanted); err != nil {
		return err
	}
	if wanted < 0 || wanted > math.MaxInt32 {
		return fmt.Errorf("%w: saved history exceeds the organization budget", ErrCapacity)
	}
	var limit, reserved, spent int64
	if err := tx.QueryRow(ctx, `SELECT limit_cents,reserved_cents,spent_cents FROM workspace_usage_caps WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&limit, &reserved, &spent); err != nil {
		return err
	}
	var prior int64
	err := tx.QueryRow(ctx, `SELECT CASE WHEN phase='reserved' THEN amount_cents ELSE 0 END
FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox_history' AND subject_id=$1 FOR UPDATE`, workspace).Scan(&prior)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	delta := wanted - prior
	if delta > 0 && spent+reserved+delta > limit {
		return fmt.Errorf("%w: saved history exceeds the organization budget", ErrCapacity)
	}
	if wanted == 0 {
		if _, err := tx.Exec(ctx, `UPDATE usage_reservations SET phase='released',updated_at=now()
WHERE workspace_id=$1 AND subject_kind='sandbox_history' AND subject_id=$1 AND phase='reserved'`, workspace); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO usage_reservations(id,workspace_id,subject_kind,subject_id,amount_cents,phase)
VALUES($1,$2,'sandbox_history',$2,$3,'reserved')
ON CONFLICT(workspace_id,subject_kind,subject_id) DO UPDATE SET
 amount_cents=EXCLUDED.amount_cents,phase='reserved',updated_at=now()`, "ushist_"+workspace, workspace, wanted); err != nil {
			return err
		}
	}
	if delta != 0 {
		if _, err := tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=greatest(0,reserved_cents+$2),updated_at=now() WHERE workspace_id=$1`, workspace, delta); err != nil {
			return err
		}
	}
	return nil
}
