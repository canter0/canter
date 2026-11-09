package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// Match revocation order: workspace -> member -> source/child conversations ->
// responsibility -> job -> run. The marker commit is the submission fence;
// revocation cannot retroactively recall an already accepted supplier request.
func (s *Store) lockWrenAIDispatchAuthority(ctx context.Context, tx pgx.Tx, workspace, account, conversation, runID, lease, purpose, kind string, estimate json.RawMessage) error {
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR SHARE`, workspace).Scan(&locked); err != nil {
		return ErrForbidden
	}
	if err := requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return err
	}
	if conversation != "" {
		if err := artifactAccess(ctx, tx, workspace, account, conversation, true); err != nil {
			return err
		}
		if err := validateWrenLifecycleTx(ctx, tx, conversation, ""); err != nil {
			return err
		}
	}
	if runID == "" {
		return nil
	}
	var runConversation, requester, speaker, originKind, responsibility string
	err := tx.QueryRow(ctx, `SELECT r.conversation_id,COALESCE(r.requester_account_id,c.account_id),COALESCE(r.speaker->>'id',''),r.origin_kind,COALESCE(r.responsibility_id,'') FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.id=$1 AND c.workspace_id=$2`, runID, workspace).Scan(&runConversation, &requester, &speaker, &originKind, &responsibility)
	if err != nil || requester != account {
		return ErrForbidden
	}
	// Child grants are billed to their origin but fenced by the actual child run.
	var jobID, jobOrigin, jobWren, parent string
	err = tx.QueryRow(ctx, `SELECT id,origin_conversation_id,wren_id,parent_run_id FROM wren_jobs WHERE child_run_id=$1`, runID).Scan(&jobID, &jobOrigin, &jobWren, &parent)
	child := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if conversation != runConversation && (!child || conversation != jobOrigin) {
		return ErrForbidden
	}
	if child {
		if err = artifactAccess(ctx, tx, workspace, account, jobOrigin, true); err != nil {
			return err
		}
	}
	if runConversation != conversation {
		if err = artifactAccess(ctx, tx, workspace, account, runConversation, true); err != nil {
			return err
		}
	}
	if err = validateWrenLifecycleTx(ctx, tx, runConversation, speaker); err != nil {
		return err
	}
	if err = validateWrenProjectRunTx(ctx, tx, workspace, account, runID, runConversation, true); err != nil {
		return err
	}
	if child {
		// Parent responsibility revocation cancels descendants even when an ACP
		// process is still stopping and its run is truthfully still 'running'.
		var parentKind, parentResponsibility string
		if err = tx.QueryRow(ctx, `SELECT origin_kind,COALESCE(responsibility_id,'') FROM operator_runs WHERE id=$1`, parent).Scan(&parentKind, &parentResponsibility); err != nil {
			return ErrForbidden
		}
		if err = lockWrenAIResponsibility(ctx, tx, workspace, account, jobOrigin, parent, parentKind, parentResponsibility); err != nil {
			return err
		}
	} else {
		if err = lockWrenAIResponsibility(ctx, tx, workspace, account, runConversation, runID, originKind, responsibility); err != nil {
			return err
		}
	}
	if child {
		var owner string
		var revoked bool
		if err = tx.QueryRow(ctx, `SELECT COALESCE(requester_account_id,''),cancelled_at IS NOT NULL OR return_suppressed FROM wren_jobs WHERE id=$1 AND child_run_id=$2 AND workspace_id=$3 FOR SHARE`, jobID, runID, workspace).Scan(&owner, &revoked); err != nil || owner != account || revoked {
			return ErrForbidden
		}
		if _, err = responsibilityWren(ctx, tx, workspace, jobOrigin, jobWren); err != nil {
			return ErrForbidden
		}
		if err = validateWrenLifecycleTx(ctx, tx, jobOrigin, jobWren); err != nil {
			return err
		}
	} else if speaker != "" {
		if _, err = responsibilityWren(ctx, tx, workspace, runConversation, speaker); err != nil {
			return ErrForbidden
		}
	}
	var status, currentLease string
	var expires, completed *time.Time
	if err = tx.QueryRow(ctx, `SELECT status,COALESCE(lease_token,''),lease_expires_at,completed_at FROM operator_runs WHERE id=$1 FOR SHARE`, runID).Scan(&status, &currentLease, &expires, &completed); err != nil {
		return ErrForbidden
	}
	titleAllowed := false
	if purpose == "title" && kind == "operator" {
		var policy struct {
			MaxOutput int `json:"maxOutputTokens"`
		}
		if json.Unmarshal(estimate, &policy) != nil || policy.MaxOutput < 1 || policy.MaxOutput > 64 {
			return ErrForbidden
		}
		var claimed bool
		if err = tx.QueryRow(ctx, `SELECT title_state='requested' AND $2=(SELECT run_id FROM operator_messages WHERE conversation_id=$1 AND role='user' ORDER BY created_at,id LIMIT 1) FROM operator_conversations WHERE id=$1`, runConversation, runID).Scan(&claimed); err != nil || !claimed {
			return ErrForbidden
		}
		titleAllowed = true
	}
	if status == "running" && lease != "" && lease == wrenAIRunLeaseHash(currentLease) && expires != nil && expires.After(s.now()) {
		return nil
	}
	// A server-claimed, bounded first-message title can finish just after its
	// successful source turn. Cancelled/failed/old runs never use this exception.
	if titleAllowed && status == "completed" && completed != nil && completed.After(s.now().Add(-time.Minute)) {
		return nil
	}

	return ErrForbidden
}
func lockWrenAIResponsibility(ctx context.Context, tx pgx.Tx, workspace, account, conversation, runID, originKind, id string) error {
	if originKind != "responsibility" && id == "" {
		return nil
	}
	if originKind != "responsibility" || id == "" {
		return ErrForbidden
	}
	var state, owner, scope string
	err := tx.QueryRow(ctx, `SELECT state,COALESCE(owner_id,''),conversation_id FROM wren_responsibilities WHERE id=$1 AND workspace_id=$2 FOR SHARE`, id, workspace).Scan(&state, &owner, &scope)
	if err != nil || state == "revoked" || owner != account || scope != conversation {
		return ErrForbidden
	}
	var wren string
	if err = tx.QueryRow(ctx, `SELECT wren_snapshot->>'id' FROM wren_responsibility_wakes WHERE run_id=$1 AND responsibility_id=$2 AND state='dispatched' FOR SHARE`, runID, id).Scan(&wren); err != nil {
		return ErrForbidden
	}
	if _, err = responsibilityWren(ctx, tx, workspace, conversation, wren); err != nil {
		return ErrForbidden
	}
	return nil
}
