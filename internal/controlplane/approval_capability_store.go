package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

const changeApprovalCapabilityLifetime = 10 * time.Minute

func (s *Store) CreateChangeApprovalCapability(ctx context.Context, workspaceID, systemName, changeID, digest string, p Principal, publicURL string) (ChangeApprovalCapability, error) {
	if p.Installation == nil || p.Session == nil || p.WorkspaceID != workspaceID || p.Installation.WorkspaceID != workspaceID || p.Installation.RevokedAt != nil {
		return ChangeApprovalCapability{}, ErrForbidden
	}
	if !p.Installation.Authority.Draft || p.Installation.Authority.ApplyMode != "human-approval-required" {
		return ChangeApprovalCapability{}, ErrForbidden
	}
	if strings.TrimSpace(publicURL) == "" {
		return ChangeApprovalCapability{}, fmt.Errorf("public URL is required")
	}
	id, _ := newID("apr_")
	token, _ := newSecret("cap_", 32)
	now := s.now()
	capability := ChangeApprovalCapability{
		ID: id, WorkspaceID: workspaceID, System: systemName, ChangeID: changeID,
		Digest: digest, Action: "authorize-and-apply", RequestedBy: *p.Installation,
		CreatedAt: now, ExpiresAt: now.Add(changeApprovalCapabilityLifetime),
		ReviewURL: strings.TrimRight(publicURL, "/") + "/approve/change/" + token,
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ChangeApprovalCapability{}, err
	}
	defer tx.Rollback(ctx)
	var activeInstallation string
	if err = tx.QueryRow(ctx, `SELECT id FROM agent_installations WHERE id=$1 AND workspace_id=$2 AND revoked_at IS NULL FOR UPDATE`, p.Installation.ID, workspaceID).Scan(&activeInstallation); errors.Is(err, pgx.ErrNoRows) {
		return ChangeApprovalCapability{}, ErrForbidden
	} else if err != nil {
		return ChangeApprovalCapability{}, err
	}
	var phase, currentDigest string
	if err = tx.QueryRow(ctx, `SELECT phase,digest FROM change_records WHERE workspace_id=$1 AND system_name=$2 AND change_id=$3 FOR SHARE`, workspaceID, systemName, changeID).Scan(&phase, &currentDigest); errors.Is(err, pgx.ErrNoRows) {
		return ChangeApprovalCapability{}, ErrNotFound
	} else if err != nil {
		return ChangeApprovalCapability{}, err
	}
	if phase != "drafted" || currentDigest != digest || len(digest) != 64 {
		return ChangeApprovalCapability{}, fmt.Errorf("%w: approval capability must bind the current drafted Change digest", ErrConflict)
	}
	// A requesting installation gets one live route to this exact Change. An
	// older unconsumed route is revoked so copied links cannot accumulate.
	if _, err = tx.Exec(ctx, `UPDATE change_approval_capabilities SET revoked_at=$1 WHERE workspace_id=$2 AND system_name=$3 AND change_id=$4 AND requested_by_installation=$5 AND consumed_at IS NULL AND revoked_at IS NULL`, now, workspaceID, systemName, changeID, p.Installation.ID); err != nil {
		return ChangeApprovalCapability{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO change_approval_capabilities(id,token_hash,workspace_id,system_name,change_id,digest,requested_by_installation,requested_session_id,action,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, secretHash(token), workspaceID, systemName, changeID, digest, p.Installation.ID, p.Session.ID, capability.Action, now, capability.ExpiresAt); err != nil {
		return ChangeApprovalCapability{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ChangeApprovalCapability{}, err
	}
	return capability, nil
}

func (s *Store) ReviewChangeApprovalCapability(ctx context.Context, token, accountID string) (ChangeApprovalReview, error) {
	capability, change, role, err := s.readChangeApprovalCapability(ctx, s.pool, token, accountID, false)
	if err != nil {
		return ChangeApprovalReview{}, err
	}
	return ChangeApprovalReview{Capability: capability, Change: change, CanApprove: role != "viewer"}, nil
}

// ConsumeChangeApprovalCapability atomically spends the bearer route before
// calling the execution engine. If the later engine step fails, the safe
// recovery is a newly requested capability or the ordinary dashboard; replay
// of this token never gains a second chance to authorize broader state.
func (s *Store) ConsumeChangeApprovalCapability(ctx context.Context, token, accountID string) (ChangeApprovalReview, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ChangeApprovalReview{}, err
	}
	defer tx.Rollback(ctx)
	capability, change, role, err := s.readChangeApprovalCapability(ctx, tx, token, accountID, true)
	if err != nil {
		return ChangeApprovalReview{}, err
	}
	if role == "viewer" {
		return ChangeApprovalReview{}, ErrForbidden
	}
	now := s.now()
	result, err := tx.Exec(ctx, `UPDATE change_approval_capabilities SET consumed_at=$1,consumed_by=$2 WHERE id=$3 AND consumed_at IS NULL AND revoked_at IS NULL`, now, accountID, capability.ID)
	if err != nil {
		return ChangeApprovalReview{}, err
	}
	if result.RowsAffected() != 1 {
		return ChangeApprovalReview{}, ErrNotFound
	}
	if err = tx.Commit(ctx); err != nil {
		return ChangeApprovalReview{}, err
	}
	capability.ConsumedAt = &now
	capability.ConsumedBy = accountID
	return ChangeApprovalReview{Capability: capability, Change: change, CanApprove: true}, nil
}

type approvalCapabilityQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) readChangeApprovalCapability(ctx context.Context, q approvalCapabilityQuerier, token, accountID string, lock bool) (ChangeApprovalCapability, sdk.Change, string, error) {
	var capability ChangeApprovalCapability
	var change sdk.Change
	var raw []byte
	var currentPhase, currentDigest, role string
	var revokedAt *time.Time
	query := `SELECT c.id,c.workspace_id,c.system_name,c.change_id,c.digest,c.action,c.created_at,c.expires_at,c.consumed_at,COALESCE(c.consumed_by,''),COALESCE(c.execution_id,''),c.revoked_at,
		i.id,i.workspace_id,i.name,i.harness,i.inspect_allowed,i.draft_allowed,i.apply_mode,i.created_by,i.created_at,i.last_seen_at,i.revoked_at,
		cr.phase,cr.digest,cr.document,m.role
		FROM change_approval_capabilities c
		JOIN agent_installations i ON i.id=c.requested_by_installation
		JOIN change_records cr ON cr.workspace_id=c.workspace_id AND cr.system_name=c.system_name AND cr.change_id=c.change_id
		JOIN memberships m ON m.workspace_id=c.workspace_id AND m.account_id=$2
		WHERE c.token_hash=$1`
	if lock {
		query += ` FOR UPDATE OF c`
	}
	err := q.QueryRow(ctx, query, secretHash(token), accountID).Scan(
		&capability.ID, &capability.WorkspaceID, &capability.System, &capability.ChangeID, &capability.Digest, &capability.Action,
		&capability.CreatedAt, &capability.ExpiresAt, &capability.ConsumedAt, &capability.ConsumedBy, &capability.ExecutionID, &revokedAt,
		&capability.RequestedBy.ID, &capability.RequestedBy.WorkspaceID, &capability.RequestedBy.Name, &capability.RequestedBy.Harness,
		&capability.RequestedBy.Authority.Inspect, &capability.RequestedBy.Authority.Draft, &capability.RequestedBy.Authority.ApplyMode,
		&capability.RequestedBy.CreatedBy, &capability.RequestedBy.CreatedAt, &capability.RequestedBy.LastSeenAt, &capability.RequestedBy.RevokedAt,
		&currentPhase, &currentDigest, &raw, &role,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return capability, change, "", ErrNotFound
	}
	if err != nil {
		return capability, change, "", err
	}
	if revokedAt != nil || capability.ConsumedAt != nil || !capability.ExpiresAt.After(s.now()) {
		return capability, change, "", ErrNotFound
	}
	if currentPhase != "drafted" || currentDigest != capability.Digest {
		return capability, change, "", fmt.Errorf("%w: Change no longer matches this approval capability", ErrConflict)
	}
	if err = json.Unmarshal(raw, &change); err != nil {
		return capability, change, "", err
	}
	if change.ID != capability.ChangeID || change.System != capability.System || change.Digest != capability.Digest {
		return capability, change, "", fmt.Errorf("%w: approval capability record is inconsistent", ErrConflict)
	}
	return capability, change, role, nil
}

func (s *Store) RecordChangeApprovalExecution(ctx context.Context, capabilityID, executionID string) error {
	result, err := s.pool.Exec(ctx, `UPDATE change_approval_capabilities SET execution_id=$1 WHERE id=$2 AND consumed_at IS NOT NULL AND execution_id IS NULL`, executionID, capabilityID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
