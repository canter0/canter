package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

type computerCodingWork struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	RunID string `json:"-"`
}

func (s *Store) pendingComputerCodingWork(ctx context.Context, computer string) ([]computerCodingWork, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.job_id,COALESCE(NULLIF(j.title,''),'Coding work'),e.run_id FROM wren_harness_executions e LEFT JOIN wren_jobs j ON j.id=e.job_id WHERE e.computer_id=$1 AND NOT e.process_stopped ORDER BY e.created_at,e.job_id LIMIT 65`, computer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []computerCodingWork{}
	for rows.Next() {
		var v computerCodingWork
		if err = rows.Scan(&v.ID, &v.Title, &v.RunID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) > 64 {
		return nil, fmt.Errorf("%w: too much coding work to hand over safely; stop current work first", ErrConflict)
	}
	return out, rows.Err()
}

// The caller holds the computer operation barrier, also used by harness
// creation. Cancel only the reviewed set and retain terminal execution receipts.
// Live dispatchers own stopping their processes; ownership waits for their ack.
func (s *Store) requestComputerCodingStops(ctx context.Context, origin Conversation, computer string, approved []string) error {
	current, err := s.WrenComputer(ctx, origin.WorkspaceID, origin.AccountID, computer)
	if err != nil {
		return err
	}
	if current.ConversationID != origin.ID {
		return ErrForbidden
	}
	work, err := s.pendingComputerCodingWork(ctx, computer)
	if err != nil {
		return err
	}
	for _, v := range work {
		if !slices.Contains(approved, v.ID) {
			return &computerCommandsRunning{CodingJobs: work}
		}
	}
	if len(work) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = authorizeWrenJobOriginTx(ctx, tx, origin.WorkspaceID, origin.AccountID, origin.ID); err != nil {
		return err
	}
	for _, v := range work {
		j, e := scanWrenJob(tx.QueryRow(ctx, `SELECT `+wrenJobColumns+` FROM wren_jobs j JOIN operator_runs r ON r.id=j.child_run_id WHERE j.id=$1 AND j.origin_conversation_id=$2 FOR UPDATE OF j`, v.ID, origin.ID))
		if e != nil && !errors.Is(e, ErrNotFound) {
			return e
		}
		if e == nil && (j.Status == "queued" || j.Status == "running" || j.Status == "stop_requested") {
			if err = cancelWrenJobRowTx(ctx, tx, j); err != nil {
				return err
			}
		} else {
			// Completed jobs may still have an idle process; stopping it must not
			// relabel or suppress a successfully completed deliverable.
			if _, err = tx.Exec(ctx, `UPDATE wren_harness_executions SET stop_requested=true,terminal_status=COALESCE((SELECT status FROM operator_runs WHERE id=$2 AND status IN ('completed','failed','cancelled')),'cancelled'),next_attempt_at=now(),updated_at=now() WHERE job_id=$1 AND computer_id=$3 AND NOT process_stopped`, v.ID, v.RunID, computer); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE wren_model_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE job_id=$1`, v.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) waitComputerCodingStops(ctx context.Context, computer string, work []computerCodingWork) error {
	ids := make([]string, 0, len(work))
	for _, v := range work {
		ids = append(ids, v.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		var pending bool
		if err := s.pool.QueryRow(wait, `SELECT EXISTS(SELECT 1 FROM wren_harness_executions WHERE computer_id=$1 AND job_id=ANY($2) AND NOT process_stopped)`, computer, ids).Scan(&pending); err != nil {
			if wait.Err() != nil {
				break
			}
			return err
		}
		if !pending {
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("%w: coding work is still stopping. Control has not changed; try taking over again after it stops", ErrConflict)
		case <-tick.C:
		}
	}
	return fmt.Errorf("%w: could not confirm that coding work stopped. Control has not changed", ErrConflict)
}
