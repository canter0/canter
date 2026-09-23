package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

func (s *Store) UpdateAgentAuthority(ctx context.Context, accountID, workspaceID, id string, authority Authority, inherit ...bool) (Installation, error) {
	useDefault := len(inherit) > 0 && inherit[0]
	if err := validateAgentAuthority(authority); !useDefault && err != nil {
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
	var defaults Authority
	if err = tx.QueryRow(ctx, `SELECT agent_authority FROM workspaces WHERE id=$1 FOR SHARE`, workspaceID).Scan(&defaults); err != nil {
		return Installation{}, err
	}
	if useDefault {
		authority = defaults
	}
	result, err := tx.Exec(ctx, `UPDATE agent_installations SET inspect_allowed=$1,draft_allowed=$2,apply_mode=$3,use_workspace_authority=$7 WHERE id=$4 AND workspace_id=$5 AND harness<>'canter-hosted' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$6)`, authority.Inspect, authority.Draft, authority.ApplyMode, id, workspaceID, s.now(), useDefault)
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

func (s *Store) UpdateWorkspaceAgentAuthority(ctx context.Context, accountID, workspaceID string, authority Authority) error {
	if err := validateAgentAuthority(authority); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var role string
	if err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE account_id=$1 AND workspace_id=$2 FOR SHARE`, accountID, workspaceID).Scan(&role); err != nil || role != "owner" {
		return ErrForbidden
	}
	raw, err := json.Marshal(authority)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE workspaces SET agent_authority=$1 WHERE id=$2`, raw, workspaceID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_installations SET inspect_allowed=$1,draft_allowed=$2,apply_mode=$3 WHERE workspace_id=$4 AND use_workspace_authority AND harness<>'canter-hosted' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$5)`, authority.Inspect, authority.Draft, authority.ApplyMode, workspaceID, s.now()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE change_approval_capabilities SET revoked_at=$1 WHERE requested_by_installation IN (SELECT id FROM agent_installations WHERE workspace_id=$2 AND use_workspace_authority AND harness<>'canter-hosted') AND consumed_at IS NULL AND revoked_at IS NULL`, s.now(), workspaceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (h *HTTPServer) workspaceAgentSettings(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string) {
	if p.Account == nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	var in struct {
		Authority Authority `json:"authority"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := validateAgentAuthority(in.Authority); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.service.Store.UpdateWorkspaceAgentAuthority(r.Context(), p.Account.ID, workspaceID, in.Authority); err != nil {
		writeStoreError(w, err)
		return
	}
	_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "agent.defaults.updated", workspaceID, map[string]any{"authority": in.Authority})
	writeJSON(w, http.StatusOK, map[string]any{"authority": in.Authority})
}
