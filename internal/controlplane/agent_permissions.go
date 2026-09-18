package controlplane

import (
	"context"
	"fmt"
)

func validateAgentAuthority(authority Authority) error {
	if !authority.Inspect {
		return fmt.Errorf("workspace read access is required")
	}
	if !authority.Draft && authority.ApplyMode != "never" {
		return fmt.Errorf("read-only access cannot apply changes")
	}
	switch authority.ApplyMode {
	case "never", "human-approval-required", "automatic":
		return nil
	default:
		return fmt.Errorf("unsupported apply mode")
	}
}

func (s *Store) UpdateAgentAuthority(ctx context.Context, accountID, workspaceID, id string, authority Authority) (Installation, error) {
	if err := validateAgentAuthority(authority); err != nil {
		return Installation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Installation{}, err
	}
	defer tx.Rollback(ctx)
	var role string
	if err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE account_id=$1 AND workspace_id=$2 FOR SHARE`, accountID, workspaceID).Scan(&role); err != nil || role != "owner" {
		return Installation{}, ErrForbidden
	}
	result, err := tx.Exec(ctx, `UPDATE agent_installations SET inspect_allowed=$1,draft_allowed=$2,apply_mode=$3 WHERE id=$4 AND workspace_id=$5 AND harness<>'canter-hosted' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$6)`, authority.Inspect, authority.Draft, authority.ApplyMode, id, workspaceID, s.now())
	if err != nil {
		return Installation{}, err
	}
	if result.RowsAffected() == 0 {
		return Installation{}, ErrNotFound
	}
	// Old approval links must not outlive a change to the originating grant.
	if _, err = tx.Exec(ctx, `UPDATE change_approval_capabilities SET revoked_at=$1 WHERE requested_by_installation=$2 AND consumed_at IS NULL AND revoked_at IS NULL`, s.now(), id); err != nil {
		return Installation{}, err
	}
	installation, err := installationByID(ctx, tx, id)
	if err != nil {
		return Installation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Installation{}, err
	}
	return installation, nil
}

// A worker cannot inherit the orchestrator's right to authorize deployments.
func agentCanApply(p Principal) bool {
	return p.Installation != nil && p.Session != nil && p.Session.ParentSessionID == "" && p.Installation.Authority.Inspect && p.Installation.Authority.Draft && p.Installation.Authority.ApplyMode == "automatic"
}
