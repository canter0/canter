package controlplane

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"path"
	"strings"
	"time"
)

var errComputerHumanControl = fmt.Errorf("%w: a person is controlling this computer; hand control over and finish your reply without polling or retrying", ErrConflict)

// ComputerUserFileAllowed fences managed capability material from user/model
// file surfaces. The private runtime client deliberately retains its own access.
func ComputerUserFileAllowed(raw string) bool {
	if strings.ContainsAny(raw, "\x00\r\n") {
		return false
	}
	if raw == "" || raw == "." {
		raw = "/workspace"
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/workspace/" + raw
	}
	clean := path.Clean(raw)
	if clean != "/workspace" && !strings.HasPrefix(clean, "/workspace/") {
		return false
	}
	relative := strings.TrimPrefix(clean, "/workspace/")
	first := strings.Split(relative, "/")[0]
	return first != ".canter" && first != ".browser"
}

func activeComputerACP(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, computer, exceptRun string) (bool, error) {
	var busy bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_harness_executions e WHERE e.computer_id=$1 AND e.run_id<>$2 AND NOT e.process_stopped AND (e.process_id<>'' OR NOT EXISTS(SELECT 1 FROM wren_harness_executions own WHERE own.run_id=$2 AND own.computer_id=$1) OR (e.created_at,e.job_id)<(SELECT own.created_at,own.job_id FROM wren_harness_executions own WHERE own.run_id=$2)))`, computer, exceptRun).Scan(&busy)
	return busy, err
}
func liveComputerMutation(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, v WrenComputer) (bool, error) {
	if v.MutationRunID == "" || v.MutationExpiresAt == nil || !v.MutationExpiresAt.After(time.Now()) {
		return false, nil
	}
	var live bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_runs WHERE id=$1 AND status='running' AND lease_token=$2 AND lease_expires_at>now())`, v.MutationRunID, v.MutationLease).Scan(&live)
	return live, err
}

// AcquireComputerMutationLease is fenced by the actual operator run lease. It
// serializes Canter API mutations; arbitrary authorized guest shell processes
// retain the explicitly granted whole-VM scope and their own stop handles.
func (h *HTTPServer) AcquireComputerMutationLease(ctx context.Context, c Conversation, run OperatorRun) (WrenComputer, error) {
	if err := h.ComputerAgentWriteAllowed(ctx, c); err != nil {
		return WrenComputer{}, err
	}
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenComputer{}, err
	}
	defer tx.Rollback(ctx)
	v, err := scanWrenComputer(tx.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND conversation_id=$2 AND phase='ready' AND NOT delete_requested FOR UPDATE`, c.WorkspaceID, c.ID))
	if err != nil {
		return v, err
	}
	var live bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_runs r JOIN operator_conversations rc ON rc.id=r.conversation_id WHERE r.id=$1 AND r.lease_token=$2 AND r.status='running' AND r.lease_expires_at>now() AND rc.workspace_id=$3 AND COALESCE(r.requester_account_id,rc.account_id)=$4 AND (r.conversation_id=$5 OR EXISTS(SELECT 1 FROM wren_jobs j WHERE j.child_run_id=r.id AND j.origin_conversation_id=$5 AND j.requester_account_id=$4)))`, run.ID, run.Lease, c.WorkspaceID, c.AccountID, c.ID).Scan(&live)
	if err != nil {
		return v, err
	}
	if !live {
		return v, ErrConflict
	}
	if h.activeComputerHumanControl(ctx, v) {
		return v, errComputerHumanControl
	}
	busy, err := activeComputerACP(ctx, tx, v.ID, run.ID)
	if err != nil {
		return v, err
	}
	if busy {
		return v, fmt.Errorf("%w: another coding worker is using this computer; wait for it to finish or stop it", ErrConflict)
	}
	held, err := liveComputerMutation(ctx, tx, v)
	if err != nil {
		return v, err
	}
	if held && (v.MutationRunID != run.ID || v.MutationLease != run.Lease) {
		return v, fmt.Errorf("%w: another agent is using this computer", ErrConflict)
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_computers SET mutation_run_id=$2,mutation_lease_token=$3,mutation_expires_at=now()+interval '60 seconds',updated_at=now() WHERE id=$1`, v.ID, run.ID, run.Lease); err != nil {
		return v, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v, err
	}
	return s.WrenComputer(ctx, c.WorkspaceID, c.AccountID, v.ID)
}
func (h *HTTPServer) ReleaseComputerMutationLease(ctx context.Context, computer, run, lease string) error {
	result, err := h.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET mutation_run_id=NULL,mutation_lease_token=NULL,mutation_expires_at=NULL,updated_at=now() WHERE id=$1 AND mutation_run_id=$2 AND mutation_lease_token=$3`, computer, run, lease)
	if err == nil && result.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
func (h *HTTPServer) computerControlOwnerAllowed(ctx context.Context, v WrenComputer, account string) error {
	current, err := h.service.Store.wrenComputerPrivate(ctx, v.WorkspaceID, v.ID)
	if err != nil {
		return err
	}
	if h.activeComputerHumanControl(ctx, current) && current.ControlAccountID != account {
		return fmt.Errorf("%w: another person is controlling this computer", ErrConflict)
	}
	return nil
}
func (h *HTTPServer) computerHumanMutationAllowed(ctx context.Context, v WrenComputer, account string) error {
	if err := h.computerControlOwnerAllowed(ctx, v, account); err != nil {
		return err
	}
	if saving, e := activeComputerCheckpoint(ctx, h.service.Store.pool, v.ID); e != nil {
		return e
	} else if saving {
		return fmt.Errorf("%w: this computer is saving its workspace", ErrConflict)
	}
	busy, err := activeComputerACP(ctx, h.service.Store.pool, v.ID, "")
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("%w: stop the active coding worker before changing this computer", ErrConflict)
	}
	current, err := h.service.Store.wrenComputerPrivate(ctx, v.WorkspaceID, v.ID)
	if err != nil {
		return err
	}
	held, err := liveComputerMutation(ctx, h.service.Store.pool, current)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("%w: an agent is using this computer; take control first", ErrConflict)
	}
	return nil
}
