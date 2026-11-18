package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/jackc/pgx/v5"
)

const (
	sandboxHistoryJobBytes       = 1 << 20
	sandboxHistoryWorkspaceBytes = 100 << 20
	sandboxHistoryChunkBytes     = 64 << 10
)

type sandboxHistoryJob struct {
	ID            string     `json:"jobId"`
	Generation    *int64     `json:"generation,omitempty"`
	Kind          string     `json:"kind"`
	ActorKind     string     `json:"actorKind,omitempty"`
	ActorID       string     `json:"actorId,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
	ObservedAt    *time.Time `json:"observedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Status        string     `json:"status"`
	ExitCode      *int       `json:"exitCode,omitempty"`
	Output        string     `json:"outputState"`
	RetainedBytes int        `json:"retainedBytes"`
	Stale         bool       `json:"stale"`
}

const sandboxHistoryColumns = `j.job_id,j.generation,j.history_kind,j.actor_kind,j.actor_id,j.started_at,
	j.history_observed_at,j.history_terminal_at,j.history_status,j.history_exit_code,
	CASE WHEN j.history_next_offset>0 AND COALESCE(j.history_terminal_at,j.history_observed_at)<=now()-interval '7 days' THEN 'expired' ELSE j.history_output_state END,
	CASE WHEN j.history_next_offset>0 AND COALESCE(j.history_terminal_at,j.history_observed_at)<=now()-interval '7 days' THEN 0 ELSE j.history_next_offset END,
	j.history_terminal_at IS NULL AND (j.history_observed_at IS NULL OR j.history_observed_at<now()-interval '45 seconds'
		OR j.generation<>s.generation OR s.phase NOT IN ('ready','save_failed'))`

func (h *HTTPServer) sandboxHistoryEnabled(workspace string) bool {
	return h.config.Secrets != nil && h.service.Store.canaryFeatureEnabled(workspace, h.config.Sandboxes.CollectJobHistory)
}

func (h *HTTPServer) sandboxJobHistory(w http.ResponseWriter, r *http.Request, v Sandbox, p Principal, parts []string) {
	if v.Phase == "deleted" {
		writeStoreError(w, ErrNotFound)
		return
	}
	ctx := r.Context()
	s := h.service.Store
	w.Header().Set("Cache-Control", "private, no-store")
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		var optIn bool
		err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT retain_output FROM sandbox_job_history_settings WHERE workspace_id=$1),true)`, v.WorkspaceID).Scan(&optIn)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		rows, err := s.pool.Query(ctx, `SELECT `+sandboxHistoryColumns+` FROM sandbox_jobs j
			JOIN sandboxes s ON s.id=j.sandbox_id WHERE s.workspace_id=$1 AND s.id=$2 AND s.phase<>'deleted'
			ORDER BY j.started_at DESC,j.job_id DESC LIMIT 101`, v.WorkspaceID, v.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		jobs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sandboxHistoryJob])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs[:min(len(jobs), 100)], "more": len(jobs) > 100, "retentionEnabled": optIn, "collectorEnabled": h.sandboxHistoryEnabled(v.WorkspaceID), "retentionDays": 7, "jobByteLimit": sandboxHistoryJobBytes})
	case len(parts) == 2 && parts[1] == "retention" && r.Method == http.MethodPatch:
		if p.Account == nil || p.Installation != nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		role, err := s.Membership(ctx, p.Account.ID, v.WorkspaceID)
		if err != nil || role != "owner" {
			writeStoreError(w, ErrForbidden)
			return
		}
		var input struct {
			Retain *bool `json:"retainOutput"`
		}
		if !decodeLimit(w, r, &input, 1024) {
			return
		}
		if input.Retain == nil {
			writeError(w, http.StatusBadRequest, errors.New("Specify retainOutput."))
			return
		}
		if err = s.setSandboxHistoryRetention(ctx, v.WorkspaceID, *input.Retain); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && r.Method == http.MethodGet:
		offset := 0
		if raw := r.URL.Query().Get("offset"); raw != "" {
			var err error
			offset, err = strconv.Atoi(raw)
			if err != nil || offset < 0 || offset > sandboxHistoryJobBytes {
				writeError(w, http.StatusBadRequest, errors.New("Invalid output offset."))
				return
			}
		}
		job, data, err := h.readSandboxJobHistory(ctx, v, parts[1], offset)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job, "dataBase64": data, "nextOffset": offset + len(data)})
	default:
		writeStoreError(w, ErrNotFound)
	}
}

func (s *Store) setSandboxHistoryRetention(ctx context.Context, workspace string, retain bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sandbox_job_history_settings VALUES($1,$2)
		ON CONFLICT(workspace_id) DO UPDATE SET retain_output=EXCLUDED.retain_output`, workspace, retain); err != nil {
		return err
	}
	if !retain {
		if _, err := tx.Exec(ctx, `UPDATE sandbox_jobs SET history_output_state=CASE WHEN history_next_offset>0 THEN 'incomplete' ELSE 'not_collected' END,
			history_nonce='',history_lease_until=NULL WHERE workspace_id=$1 AND history_output_state='collecting'`, workspace); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func sandboxHistoryBinding(workspace, sandbox, job string, generation int64, offset int) []byte {
	return []byte(fmt.Sprintf("canter:sandbox-job-output:v1\x00%s\x00%s\x00%s\x00%d\x00%d", workspace, sandbox, job, generation, offset))
}

func (h *HTTPServer) readSandboxJobHistory(ctx context.Context, v Sandbox, id string, offset int) (sandboxHistoryJob, []byte, error) {
	var job sandboxHistoryJob
	tx, err := h.service.Store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return job, nil, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT `+sandboxHistoryColumns+` FROM sandbox_jobs j JOIN sandboxes s ON s.id=j.sandbox_id
		WHERE s.workspace_id=$1 AND j.sandbox_id=$2 AND j.job_id=$3 AND s.phase<>'deleted'`, v.WorkspaceID, v.ID, id).Scan(
		&job.ID, &job.Generation, &job.Kind, &job.ActorKind, &job.ActorID, &job.StartedAt, &job.ObservedAt,
		&job.FinishedAt, &job.Status, &job.ExitCode, &job.Output, &job.RetainedBytes, &job.Stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, nil, ErrNotFound
	}
	if err != nil {
		return job, nil, err
	}
	if job.Generation == nil || offset >= job.RetainedBytes {
		return job, []byte{}, nil
	}
	var key string
	var sealed []byte
	var start int
	err = tx.QueryRow(ctx, `SELECT byte_offset,key_id,ciphertext FROM sandbox_job_history_chunks
		WHERE sandbox_id=$1 AND job_id=$2 AND byte_offset<=$3 ORDER BY byte_offset DESC LIMIT 1`, v.ID, id, offset).Scan(&start, &key, &sealed)
	if err != nil {
		return job, nil, err
	}
	plain, err := h.config.Secrets.open(key, sealed, sandboxHistoryBinding(v.WorkspaceID, v.ID, id, *job.Generation, start))
	if err != nil {
		return job, nil, err
	}
	if offset-start >= len(plain) {
		return job, nil, ErrConflict
	}
	return job, plain[offset-start:], nil
}

// This receipt updates diagnostic evidence only. Existing lifecycle guards
// remain owned by the submission, setup and sleep paths.
func (s *Store) observeSandboxHistory(ctx context.Context, v Sandbox, job wrencomputer.Job) {
	if !sandboxJobTerminal(job.Status) && job.Status != "starting" && job.Status != "running" {
		return
	}
	operation, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	terminal := sandboxJobTerminal(job.Status)
	_, _ = s.pool.Exec(operation, `UPDATE sandbox_jobs SET history_status=$5,history_observed_at=now(),
		history_terminal_at=CASE WHEN $6 THEN COALESCE(history_terminal_at,now()) ELSE history_terminal_at END,
		history_exit_code=CASE WHEN $6 THEN $7 ELSE history_exit_code END
		WHERE workspace_id=$1 AND sandbox_id=$2 AND generation=$3 AND job_id=$4 AND history_terminal_at IS NULL`,
		v.WorkspaceID, v.ID, v.Generation, job.ID, job.Status, terminal, job.ExitCode)
}

type sandboxHistoryClaim struct {
	Job, Nonce, State string
	Offset            int
}

func (s *Store) claimSandboxHistory(ctx context.Context, v Sandbox) (sandboxHistoryClaim, error) {
	var claim sandboxHistoryClaim
	nonce, err := newID("collect_")
	if err != nil {
		return claim, err
	}
	claim.Nonce = nonce
	err = s.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT j.job_id FROM sandbox_jobs j JOIN sandboxes s ON s.id=j.sandbox_id
		WHERE j.workspace_id=$1 AND j.sandbox_id=$2 AND j.generation=$3 AND s.generation=$3
		AND s.phase IN ('ready','saving','save_failed') AND NOT s.delete_requested
		AND (j.history_output_state='collecting' OR (j.history_output_state='truncated' AND j.history_terminal_at IS NULL))
		AND j.history_next_attempt<=now()
		AND (j.history_lease_until IS NULL OR j.history_lease_until<=now())
		ORDER BY j.history_next_attempt,j.job_id LIMIT 1 FOR UPDATE OF j SKIP LOCKED
	)
	UPDATE sandbox_jobs j SET history_nonce=$4,history_lease_until=now()+interval '20 seconds'
	FROM candidate WHERE j.sandbox_id=$2 AND j.job_id=candidate.job_id
	RETURNING j.job_id,j.history_next_offset,j.history_output_state`, v.WorkspaceID, v.ID, v.Generation, nonce).Scan(&claim.Job, &claim.Offset, &claim.State)
	return claim, err
}

func (h *HTTPServer) collectSandboxHistory(ctx context.Context, v Sandbox, client sandboxAgentClient) bool {
	s := h.service.Store
	claim, err := s.claimSandboxHistory(ctx, v)
	if err != nil {
		return false
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
		defer cancel()
		_, _ = s.pool.Exec(cleanup, `UPDATE sandbox_jobs SET history_nonce='',history_lease_until=NULL,history_next_attempt=now()+interval '3 seconds'
			WHERE sandbox_id=$1 AND job_id=$2 AND generation=$3 AND history_nonce=$4`, v.ID, claim.Job, v.Generation, claim.Nonce)
	}()
	if claim.State == "truncated" {
		receipt, err := client.Job(ctx, claim.Job)
		if err != nil || receipt.ID != claim.Job || !sandboxJobTerminal(receipt.Status) && receipt.Status != "running" && receipt.Status != "starting" {
			return false
		}
		result, err := s.pool.Exec(ctx, `UPDATE sandbox_jobs j SET history_status=$6,history_observed_at=now(),
			history_terminal_at=CASE WHEN $7 THEN now() END,history_exit_code=CASE WHEN $7 THEN $8 END
			FROM sandboxes s WHERE s.id=j.sandbox_id AND s.generation=$3 AND NOT s.delete_requested
			AND j.workspace_id=$1 AND j.sandbox_id=$2 AND j.generation=$3 AND j.job_id=$4 AND j.history_nonce=$5
			AND j.history_lease_until>now() AND j.history_terminal_at IS NULL`,
			v.WorkspaceID, v.ID, v.Generation, claim.Job, claim.Nonce, receipt.Status, sandboxJobTerminal(receipt.Status), receipt.ExitCode)
		return err == nil && result.RowsAffected() == 1
	}
	limit := min(sandboxHistoryChunkBytes, sandboxHistoryJobBytes-claim.Offset)
	if limit <= 0 {
		return false
	}
	chunk, err := client.JobLogs(ctx, claim.Job, "combined", int64(claim.Offset), limit)
	if err != nil || validateSandboxLogChunk(chunk, claim.Job, "combined", int64(claim.Offset), limit) != nil {
		return false
	}
	if !sandboxJobTerminal(chunk.Status) && chunk.Status != "starting" && chunk.Status != "running" {
		return false
	}
	defer clear(chunk.Data)
	return h.publishSandboxHistory(ctx, v, claim, chunk) == nil
}

func (h *HTTPServer) publishSandboxHistory(ctx context.Context, v Sandbox, claim sandboxHistoryClaim, chunk wrencomputer.JobLogChunk) error {
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var workspace string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, v.WorkspaceID).Scan(&workspace); err != nil {
		return err
	}
	var current int
	err = tx.QueryRow(ctx, `SELECT generation FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND phase IN ('ready','saving','save_failed') AND NOT delete_requested FOR SHARE`,
		v.WorkspaceID, v.ID).Scan(&current)
	if err != nil || current != v.Generation {
		return firstErr(err, ErrConflict)
	}

	var offset int
	var retained bool
	err = tx.QueryRow(ctx, `SELECT j.history_next_offset,COALESCE((SELECT retain_output FROM sandbox_job_history_settings WHERE workspace_id=$1),true)
		FROM sandbox_jobs j JOIN sandboxes s ON s.id=j.sandbox_id WHERE j.workspace_id=$1 AND j.sandbox_id=$2 AND j.job_id=$3
		AND j.generation=$4 AND s.generation=$4 AND s.phase IN ('ready','saving','save_failed') AND NOT s.delete_requested
		AND j.history_nonce=$5 AND j.history_lease_until>now() AND j.history_output_state='collecting' FOR UPDATE OF j`,
		v.WorkspaceID, v.ID, claim.Job, v.Generation, claim.Nonce).Scan(&offset, &retained)
	if err != nil {
		return err
	}
	if !retained || offset != claim.Offset {
		return ErrConflict
	}
	state := "collecting"
	terminal := sandboxJobTerminal(chunk.Status)
	if terminal && chunk.EOF {
		state = "complete"
	}
	if (chunk.NextOffset >= sandboxHistoryJobBytes && !(terminal && chunk.EOF)) || (terminal && chunk.EOF && chunk.LogTruncated) {
		state = "truncated"
	}
	if len(chunk.Data) > 0 {
		available, err := makeSandboxHistoryRoom(ctx, tx, v.WorkspaceID, v.ID, claim.Job, len(chunk.Data))
		if err != nil {
			return err
		}
		if available {
			key, sealed, err := h.config.Secrets.seal(chunk.Data, sandboxHistoryBinding(v.WorkspaceID, v.ID, claim.Job, int64(v.Generation), offset))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO sandbox_job_history_chunks VALUES($1,$2,$3,$4,$5,$6)`, v.ID, claim.Job, offset, len(chunk.Data), key, sealed); err != nil {
				return err
			}
			offset += len(chunk.Data)
		} else {
			state = "truncated"
		}
	}
	_, err = tx.Exec(ctx, `UPDATE sandbox_jobs SET history_next_offset=$3,history_output_state=$4,
		history_status=CASE WHEN history_terminal_at IS NULL THEN $5 ELSE history_status END,history_observed_at=now(),
		history_exit_code=CASE WHEN $6 AND history_terminal_at IS NULL THEN $7 ELSE history_exit_code END,
		history_terminal_at=CASE WHEN $6 THEN COALESCE(history_terminal_at,now()) ELSE history_terminal_at END
		WHERE sandbox_id=$1 AND job_id=$2`, v.ID, claim.Job, offset, state, chunk.Status, terminal, chunk.ExitCode)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func makeSandboxHistoryRoom(ctx context.Context, tx pgx.Tx, workspace, sandbox, job string, additional int) (bool, error) {
	var total int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(history_next_offset),0) FROM sandbox_jobs WHERE workspace_id=$1`, workspace).Scan(&total); err != nil {
		return false, err
	}
	for total+int64(additional) > sandboxHistoryWorkspaceBytes {
		var victimSandbox, victimJob string
		var count int
		err := tx.QueryRow(ctx, `SELECT sandbox_id,job_id,history_next_offset FROM sandbox_jobs WHERE workspace_id=$1
			AND history_terminal_at IS NOT NULL AND history_output_state<>'collecting' AND history_next_offset>0
			AND (sandbox_id,job_id)<>($2,$3) ORDER BY history_terminal_at,job_id LIMIT 1 FOR UPDATE`, workspace, sandbox, job).Scan(&victimSandbox, &victimJob, &count)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := expireSandboxHistory(ctx, tx, victimSandbox, victimJob); err != nil {
			return false, err
		}
		total -= int64(count)
	}
	return true, nil
}

func expireSandboxHistory(ctx context.Context, tx pgx.Tx, sandbox, job string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM sandbox_job_history_chunks WHERE sandbox_id=$1 AND job_id=$2`, sandbox, job); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE sandbox_jobs SET history_output_state='expired',history_next_offset=0 WHERE sandbox_id=$1 AND job_id=$2`, sandbox, job)
	return err
}

func (d *SandboxDispatcher) runSandboxHistory(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		operation, cancel := context.WithTimeout(ctx, 2*time.Second)
		d.collectSandboxHistoryPass(operation)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *SandboxDispatcher) collectSandboxHistoryPass(ctx context.Context) {
	h := d.Server
	s := h.service.Store
	_, _ = s.pool.Exec(ctx, `UPDATE sandbox_jobs j SET history_output_state=CASE WHEN j.history_next_offset>0 THEN 'incomplete' ELSE 'unavailable' END,
		history_nonce='',history_lease_until=NULL FROM sandboxes s WHERE s.id=j.sandbox_id AND j.history_output_state='collecting'
		AND (s.phase IN ('asleep','failed','deleted') OR s.generation<>j.generation)`)
	_, _ = s.pool.Exec(ctx, `WITH due AS (
		SELECT sandbox_id,job_id FROM sandbox_jobs WHERE history_next_offset>0 AND COALESCE(history_terminal_at,history_observed_at)<=now()-interval '7 days'
		ORDER BY history_terminal_at LIMIT 20 FOR UPDATE SKIP LOCKED
	), erased AS (
		DELETE FROM sandbox_job_history_chunks c USING due WHERE c.sandbox_id=due.sandbox_id AND c.job_id=due.job_id
	)
	UPDATE sandbox_jobs j SET history_output_state='expired',history_next_offset=0,history_nonce='',history_lease_until=NULL FROM due
	WHERE j.sandbox_id=due.sandbox_id AND j.job_id=due.job_id`)
	_, _ = s.pool.Exec(ctx, `WITH due AS (
		SELECT t.id FROM sandbox_transfers t JOIN sandboxes s ON s.id=t.sandbox_id
		WHERE t.expires_at<=now()-interval '7 days' OR s.phase='deleted'
		ORDER BY t.expires_at LIMIT 20 FOR UPDATE OF t SKIP LOCKED
	) DELETE FROM sandbox_transfers t USING due WHERE t.id=due.id`)
	if !h.sandboxHistoryEnabled(s.featureCanaryWorkspace) {
		return
	}
	var workspace, id string
	err := s.pool.QueryRow(ctx, `SELECT j.workspace_id,j.sandbox_id FROM sandbox_jobs j JOIN sandboxes s ON s.id=j.sandbox_id
		WHERE (j.history_output_state='collecting' OR (j.history_output_state='truncated' AND j.history_terminal_at IS NULL))
		AND ($1 = '' OR j.workspace_id=$1) AND j.history_next_attempt<=now()
		AND (j.history_lease_until IS NULL OR j.history_lease_until<=now())
		AND s.phase IN ('ready','save_failed') AND s.generation=j.generation AND NOT s.delete_requested AND NOT s.sleep_requested
		ORDER BY j.history_next_attempt,j.job_id LIMIT 1`, s.featureCanaryWorkspace).Scan(&workspace, &id)
	if err != nil {
		return
	}
	v, err := s.Sandbox(ctx, workspace, id)
	if err != nil {
		return
	}
	client, err := d.sandboxDirectClient(v)
	if err != nil {
		_, _ = s.pool.Exec(ctx, `UPDATE sandbox_jobs SET history_next_attempt=now()+interval '30 seconds'
			WHERE workspace_id=$1 AND sandbox_id=$2 AND generation=$3 AND history_next_attempt<=now()
			AND history_output_state IN ('collecting','truncated')`, v.WorkspaceID, v.ID, v.Generation)
		return
	}
	defer client.Close()
	h.collectSandboxHistory(ctx, v, client)
}
