package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ciWorkspaceProtectedTx(ctx context.Context, tx pgx.Tx, workspace string) (bool, error) {
	var locked string
	err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	var protected bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ci_runners WHERE workspace_id=$1 AND (phase<>'deleted' OR (paid_overrun_enabled AND execution_started_at IS NOT NULL AND execution_stopped_at IS NULL))) OR EXISTS(SELECT 1 FROM ci_budget_reservations WHERE workspace_id=$1) OR EXISTS(SELECT 1 FROM ci_provisioning_reservations WHERE workspace_id=$1 AND released_at IS NULL)`, workspace).Scan(&protected)
	return protected, err
}

func (s *Store) ciFinancialHistoryTx(ctx context.Context, tx pgx.Tx, workspace string) (bool, error) {
	var financial bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ci_usage_ledger WHERE workspace_id=$1) OR EXISTS(SELECT 1 FROM ci_usage_balances WHERE workspace_id=$1 AND remainder_units>0)`, workspace).Scan(&financial)
	return financial, err
}

func (s *Store) closeCIWorkspaceTx(ctx context.Context, tx pgx.Tx, workspace string) error {
	protected, err := s.ciWorkspaceProtectedTx(ctx, tx, workspace)
	if err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("%w: CI cleanup and billing reconciliation must finish before closing the workspace", ErrConflict)
	}
	for _, query := range []string{
		`DELETE FROM ci_runner_lifecycles WHERE workspace_id=$1`,
		`DELETE FROM ci_runner_admissions WHERE workspace_id=$1`,
		`UPDATE ci_runners SET installation_id=NULL,trigger_job_id=NULL,assigned_job_id=NULL,github_runner_id=NULL,document='{}'::jsonb WHERE workspace_id=$1 AND phase='deleted'`,
		`DELETE FROM ci_jobs WHERE workspace_id=$1`,
		`DELETE FROM ci_runners r WHERE workspace_id=$1 AND NOT EXISTS(SELECT 1 FROM ci_usage_ledger l WHERE l.workspace_id=r.workspace_id AND l.runner_id=r.id)`,
		`DELETE FROM ci_github_installations WHERE workspace_id=$1`,
		`DELETE FROM ci_connect_states WHERE workspace_id=$1`,
		`DELETE FROM ci_settings WHERE workspace_id=$1`,
		`DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`,
	} {
		if _, err = tx.Exec(ctx, query, workspace); err != nil {
			return err
		}
	}
	return nil
}
