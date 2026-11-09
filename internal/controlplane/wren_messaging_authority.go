package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Resolve the instruction from authoritative rows, never a model-supplied key.
// A rejected standing instruction remains fenced across subsequent wakes until
// a human changes that instruction's saved revision or makes a fresh request.
func messageProposalAuthorityTx(ctx context.Context, tx pgx.Tx, workspace, account, conversation string, run OperatorRun) (string, string, error) {
	if run.ConversationID != conversation {
		return "", "", ErrForbidden
	}
	var origin, responsibility, turn string
	e := tx.QueryRow(ctx, `SELECT origin_kind,COALESCE(responsibility_id,''),turn_id FROM operator_runs WHERE id=$1 AND conversation_id=$2`, run.ID, conversation).Scan(&origin, &responsibility, &turn)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", "", ErrForbidden
	}
	if e != nil {
		return "", "", e
	}
	authority, instruction := "", ""
	switch origin {
	case "human":
		if turn == "" {
			turn = run.ID
		}
		authority = "human:" + turn
		instruction = authority
	case "responsibility":
		var revision int
		e = tx.QueryRow(ctx, `SELECT r.revision FROM wren_responsibilities r JOIN wren_responsibility_wakes w ON w.responsibility_id=r.id WHERE r.id=$1 AND r.workspace_id=$2 AND r.owner_id=$3 AND r.conversation_id=$4 AND r.state='active' AND w.run_id=$5 AND w.accepted_revision=r.revision AND w.state='dispatched' FOR SHARE OF r`, responsibility, workspace, account, conversation, run.ID).Scan(&revision)
		if errors.Is(e, pgx.ErrNoRows) {
			return "", "", ErrForbidden
		}
		if e != nil {
			return "", "", e
		}
		authority = fmt.Sprintf("standing:%s:%d", responsibility, revision)
		instruction = "wake:" + run.ID
	default:
		return "", "", ErrForbidden
	}
	var id string
	e = tx.QueryRow(ctx, `SELECT r.id FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.id=$1 AND r.conversation_id=$2 AND c.workspace_id=$3 AND COALESCE(r.requester_account_id,c.account_id)=$4 AND r.lease_token=$5 AND r.status='running' AND r.lease_expires_at>now() FOR SHARE OF r`, run.ID, conversation, workspace, account, run.Lease).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", "", ErrForbidden
	}
	if e != nil {
		return "", "", e
	}
	return authority, instruction, nil
}
