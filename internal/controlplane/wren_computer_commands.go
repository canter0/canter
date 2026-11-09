package controlplane

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/canter0/canter/internal/wrencomputer"
)

var computerCommandID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,99}$`)

type computerCommandClient interface {
	StartJob(context.Context, wrencomputer.JobRequest) (wrencomputer.Job, error)
	Job(context.Context, string) (wrencomputer.Job, error)
	StopJob(context.Context, string) (wrencomputer.Job, error)
}
type computerCommandsRunning struct {
	IDs        []string
	CodingJobs []computerCodingWork
}

func (e *computerCommandsRunning) Error() string {
	return "Work is still running on this computer. Stop it before taking control."
}
func (e *computerCommandsRunning) Unwrap() error { return ErrConflict }
func computerCommandFinished(status string) bool {
	return slices.Contains([]string{"completed", "cancelled", "failed", "timed_out", "not_started"}, status)
}

// Call under the per-computer operation barrier. Record before the remote
// launch so a lost response or completed model turn cannot hide live work.
func (s *Store) startComputerCommand(ctx context.Context, client computerCommandClient, computer string, request wrencomputer.JobRequest) (wrencomputer.Job, error) {
	if !computerCommandID.MatchString(request.ID) {
		return wrencomputer.Job{}, fmt.Errorf("invalid computer command ID")
	}
	inserted, err := s.pool.Exec(ctx, `INSERT INTO wren_computer_commands(computer_id,command_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, computer, request.ID)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	job, err := client.StartJob(ctx, request)
	if err == nil {
		err = s.recordComputerCommand(ctx, computer, job)
	} else {
		var rejected *wrencomputer.Error
		if inserted.RowsAffected() == 1 && errors.As(err, &rejected) && slices.Contains([]string{"invalid_command", "invalid_timeout", "invalid_directory", "invalid_job"}, rejected.Code) {
			// Validation rejected before remote execution. Preserve an earlier record
			// if this ID had previously launched successfully.
			_, _ = s.pool.Exec(ctx, `UPDATE wren_computer_commands SET status='not_started',updated_at=now() WHERE computer_id=$1 AND command_id=$2 AND status='unconfirmed'`, computer, request.ID)
		}
	}
	return job, err
}
func (s *Store) recordComputerCommand(ctx context.Context, computer string, job wrencomputer.Job) error {
	_, err := s.pool.Exec(ctx, `UPDATE wren_computer_commands SET status=$3,updated_at=now() WHERE computer_id=$1 AND command_id=$2`, computer, job.ID, job.Status)
	return err
}
func (s *Store) pendingComputerCommands(ctx context.Context, computer string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT command_id FROM wren_computer_commands WHERE computer_id=$1 AND status NOT IN ('completed','cancelled','failed','timed_out','not_started') ORDER BY created_at,command_id LIMIT 65`, computer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 64 {
		return nil, fmt.Errorf("%w: too many active commands to hand over safely; stop current work first", ErrConflict)
	}
	return ids, rows.Err()
}

// Reconcile before touching any command. The reviewed set must cover every
// live command; work added since the review requires a new review, not a wider
// implicit cancellation. Caller holds the operation barrier throughout.
func (s *Store) settleComputerCommands(ctx context.Context, client computerCommandClient, computer string, ids, approved []string) error {
	live := []string{}
	for _, id := range ids {
		job, err := client.Job(ctx, id)
		if err != nil {
			var missing *wrencomputer.Error
			if errors.As(err, &missing) && missing.Code == "not_found" {
				live = append(live, id)
				continue
			}
			return fmt.Errorf("could not verify running computer work: %w", err)
		}
		if err = s.recordComputerCommand(ctx, computer, job); err != nil {
			return err
		}
		if !computerCommandFinished(job.Status) {
			live = append(live, id)
		}
	}
	for _, id := range live {
		if !slices.Contains(approved, id) {
			return &computerCommandsRunning{IDs: live}
		}
	}
	for _, id := range live {
		job, err := client.StopJob(ctx, id)
		if err != nil {
			return fmt.Errorf("could not confirm that computer work stopped: %w", err)
		}
		if err = s.recordComputerCommand(ctx, computer, job); err != nil {
			return err
		}
		if !computerCommandFinished(job.Status) {
			return &computerCommandsRunning{IDs: live}
		}
	}
	return nil
}
