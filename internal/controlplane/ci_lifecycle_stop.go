package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// requestCIRunnerStop freezes a server-authoritative billing ceiling, but does
// not claim the guest stopped. Settlement waits for independent stop proof
// and uses an earlier source-proved completion when available. Shutdown and
// cleanup latency after a cancellation is absorbed by Canter.
func (s *Store) requestCIRunnerStop(ctx context.Context, workspace, runner, reason string, completedAt *time.Time) (time.Time, error) {
	switch reason {
	case "cancelled", "deadline", "completed", "setup_failed":
	default:
		return time.Time{}, ErrConflict
	}
	if workspace == "" || runner == "" || (reason == "completed") != (completedAt != nil) {
		return time.Time{}, ErrConflict
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if completedAt != nil && (completedAt.IsZero() || completedAt.After(now.Add(time.Minute))) {
		return time.Time{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return time.Time{}, err
	}
	var started, deadline, prior, priorBill *time.Time
	var hardDeadline time.Time
	var phase string
	err = tx.QueryRow(ctx, `SELECT r.execution_started_at,r.deadline_at,r.phase,l.stop_requested_at,l.bill_stop_at,l.hard_deadline_at FROM ci_runners r JOIN ci_runner_lifecycles l ON l.workspace_id=r.workspace_id AND l.runner_id=r.id WHERE r.workspace_id=$1 AND r.id=$2 FOR UPDATE OF r,l`, workspace, runner).Scan(&started, &deadline, &phase, &prior, &priorBill, &hardDeadline)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	if prior != nil {
		// Subsequent retries cannot move the already frozen boundary.
		if priorBill != nil {
			return priorBill.UTC(), nil
		}
		return time.Time{}, nil
	}
	if phase == "deleted" {
		return time.Time{}, ErrConflict
	}
	var billStop *time.Time
	if started != nil {
		at := now
		if reason == "completed" {
			at = completedAt.UTC().Truncate(time.Microsecond)
			if at.Before(*started) {
				return time.Time{}, ErrConflict
			}
			if at.After(now) {
				at = now
			}
		}
		if reason == "deadline" {
			limit := hardDeadline
			if deadline != nil && deadline.Before(limit) {
				limit = *deadline
			}
			if now.Before(limit) {
				return time.Time{}, ErrConflict
			}
			at = limit.UTC()
		}
		if at.Before(*started) {
			at = started.UTC()
		}
		billStop = &at
	} else if reason == "completed" {
		return time.Time{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runner_lifecycles SET stop_requested_at=$3,bill_stop_at=$4,stop_reason=$5,next_attempt_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, now, billStop, reason); err != nil {
		return time.Time{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runners SET delete_requested=true,phase='deleting',updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, runner, now); err != nil {
		return time.Time{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return time.Time{}, err
	}
	if billStop != nil {
		return *billStop, nil
	}
	return time.Time{}, nil
}
