package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var errWrenJobInputPending = errors.New("worker correction awaits next model checkpoint")

type WrenJobInput struct {
	ID        string     `json:"id"`
	JobID     string     `json:"jobId"`
	Content   string     `json:"content"`
	CreatedAt time.Time  `json:"createdAt"`
	AppliedAt *time.Time `json:"appliedAt,omitempty"`
}

// Steering is an explicit correction at the next model checkpoint, never a
// promise to interrupt an already submitted command or grant new authority.
func (s *Store) SteerWrenJob(ctx context.Context, workspace, account, job, key, content string) (WrenJobInput, error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > 8000 || key == "" || len(key) > 160 {
		return WrenJobInput{}, fmt.Errorf("a correction up to 8000 bytes and an operation key are required")
	}
	j, err := s.WrenJob(ctx, workspace, account, job)
	if err != nil {
		return WrenJobInput{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenJobInput{}, err
	}
	defer tx.Rollback(ctx)
	if err = authorizeWrenJobOriginTx(ctx, tx, workspace, account, j.OriginConversationID); err != nil {
		return WrenJobInput{}, err
	}
	var status, executor string
	err = tx.QueryRow(ctx, `SELECT r.status,r.executor_kind FROM wren_jobs j JOIN operator_runs r ON r.id=j.child_run_id WHERE j.id=$1 AND j.cancelled_at IS NULL AND NOT j.return_suppressed FOR UPDATE OF j,r`, job).Scan(&status, &executor)
	if errors.Is(err, pgx.ErrNoRows) {
		return WrenJobInput{}, ErrConflict
	}
	if err != nil {
		return WrenJobInput{}, err
	}
	if executor == "acp-opencode" {
		return WrenJobInput{}, fmt.Errorf("this coding harness cannot accept concurrent steering; stop it, wait for confirmed exit, then delegate a corrected brief")
	}
	var out WrenJobInput
	err = tx.QueryRow(ctx, `SELECT id,job_id,content,created_at,applied_at FROM wren_job_inputs WHERE job_id=$1 AND operation_key=$2`, job, key).Scan(&out.ID, &out.JobID, &out.Content, &out.CreatedAt, &out.AppliedAt)
	if err == nil {
		if out.Content != content {
			return WrenJobInput{}, ErrConflict
		}
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if status != "queued" && status != "running" {
		return out, ErrConflict
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM wren_job_inputs WHERE job_id=$1`, job).Scan(&count); err != nil {
		return out, err
	}
	if count >= 32 {
		return out, ErrCapacity
	}
	out.ID, err = newID("job_input_")
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO wren_job_inputs(id,job_id,operation_key,account_id,content) VALUES($1,$2,$3,$4,$5) RETURNING id,job_id,content,created_at,applied_at`, out.ID, job, key, account, content).Scan(&out.ID, &out.JobID, &out.Content, &out.CreatedAt, &out.AppliedAt)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) applyWrenJobInputs(ctx context.Context, run OperatorRun, messages []modelMessage) ([]modelMessage, error) {
	if !strings.HasPrefix(run.ConversationID, "conv_job_") {
		return messages, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM operator_runs WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now() FOR UPDATE`, run.ID, run.Lease).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrConflict
		}
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT i.id,i.content FROM wren_job_inputs i JOIN wren_jobs j ON j.id=i.job_id WHERE j.child_run_id=$1 AND i.applied_at IS NULL AND j.cancelled_at IS NULL AND NOT j.return_suppressed ORDER BY i.created_at,i.id`, run.ID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var input, text string
		if err = rows.Scan(&input, &text); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, input)
		messages = append(messages, modelMessage{Role: "user", Content: "Correction forwarded from the authorized origin conversation (" + input + "). Apply it to this job within existing authority; it does not authorize new resources or external actions.\n" + text})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		raw, e := json.Marshal(messages)
		if e != nil {
			return nil, e
		}
		if _, err = tx.Exec(ctx, `UPDATE operator_runs SET checkpoint=$2 WHERE id=$1`, run.ID, raw); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE wren_job_inputs SET applied_at=now() WHERE id=ANY($1)`, ids); err != nil {
			return nil, err
		}
	}
	return messages, tx.Commit(ctx)
}
