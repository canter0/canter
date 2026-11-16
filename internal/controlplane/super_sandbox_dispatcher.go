package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/jackc/pgx/v5"
)

// SuperSandboxDispatcher uses short database claims and stable guest requests.
// Replicas may observe/replay the same claim; the guest deduplicates its exact
// persisted ID+payload. No database connection is held across remote I/O.
type SuperSandboxDispatcher struct {
	Server *HTTPServer
	// client is only a test seam. Production always uses the generation-fenced
	// sandbox client (including secret dialing and setup gates).
	client func(context.Context, Sandbox) (sandboxAgentClient, error)
}

func (d *SuperSandboxDispatcher) Run(ctx context.Context) error {
	if d.Server == nil {
		return fmt.Errorf("super sandbox dispatcher has no server")
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancelWorkers()
		workers.Wait()
	}()
	// At most four run-level RPC sequences, never a goroutine per task. Keep
	// completed-but-not-yet-collected runs in the set to prevent local replays.
	const concurrency = 4
	inflight := make(map[string]bool, concurrency)
	completed := make(chan string, concurrency)
	var queue [][2]string
	poll := true
	for {
		// Even with the feature flag withdrawn, observe and cancel persisted
		// active work. Do not scan/hot-spin while all four slots are occupied.
		if poll && len(inflight) < concurrency {
			items, err := d.pending(workerCtx)
			if err != nil {
				return err
			}
			queue, poll = items, false
		}
		for len(queue) > 0 && len(inflight) < concurrency && workerCtx.Err() == nil {
			item := queue[0]
			queue = queue[1:]
			if inflight[item[1]] {
				continue
			}
			inflight[item[1]] = true
			workers.Add(1)
			go func() {
				defer workers.Done()
				rctx, cancel := context.WithTimeout(workerCtx, 30*time.Second)
				defer cancel()
				if err := d.Reconcile(rctx, item[0], item[1]); err != nil && workerCtx.Err() == nil {
					slog.Warn("super sandbox reconcile", "id", item[1], "error", err)
				}
				// Capacity matches the maximum outstanding runs, so shutdown
				// can join workers without needing to drain this channel.
				completed <- item[1]
			}()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-completed:
			delete(inflight, id)
		case <-ticker.C:
			poll = true
		}
	}
}

func (d *SuperSandboxDispatcher) pending(ctx context.Context) ([][2]string, error) {
	rows, err := d.Server.service.Store.pool.Query(ctx, `SELECT workspace_id,id FROM super_sandboxes WHERE finished_at IS NULL ORDER BY last_reconciled_at,created_at,id LIMIT 32`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := [][2]string{}
	for rows.Next() {
		var item [2]string
		if err = rows.Scan(&item[0], &item[1]); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type superSandboxClaim struct {
	TaskID     string
	WorkerID   string
	Generation int
	Request    wrencomputer.JobRequest
}

// claim checks authority and cancellation in the same transaction that assigns
// a worker and records BOTH the batch task and existing sandbox_jobs handle.
// The workspace lock coordinates different runs and product/role mutations;
// the partial unique index additionally enforces cross-replica exclusion.
func (d *SuperSandboxDispatcher) claim(ctx context.Context, workspace, id string) (*superSandboxClaim, error) {
	s := d.Server.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		return nil, err
	}
	var state string
	var account *string
	var maxConcurrency int
	var finished *time.Time
	if err = tx.QueryRow(ctx, `SELECT state,creator_account_id,max_concurrency,finished_at FROM super_sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id).Scan(&state, &account, &maxConcurrency, &finished); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if finished != nil {
		return nil, nil
	}
	enabled, err := s.productEnabled(ctx, tx, workspace, "sandboxes")
	if err != nil {
		return nil, err
	}
	var role string
	if account != nil {
		role, err = workspaceRole(ctx, tx, workspace, *account, true)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if !d.Server.config.Sandboxes.Enabled || !enabled || (role != "owner" && role != "operator") {
		state = "cancelling"
		if _, err = tx.Exec(ctx, `UPDATE super_sandboxes SET state='cancelling',failure='Creator authority or Sandboxes enablement was withdrawn' WHERE id=$1`, id); err != nil {
			return nil, err
		}
	}
	if state == "cancelling" {
		if _, err = tx.Exec(ctx, `UPDATE super_sandbox_tasks SET state='cancelled',finished_at=now() WHERE run_id=$1 AND state='queued'`, id); err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	// Uncertainty blocks further dispatch in this run rather than masking it
	// with more work. Existing reservations are never refunded/reassigned.
	if state == "needs_attention" {
		return nil, tx.Commit(ctx)
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM super_sandbox_tasks WHERE run_id=$1 AND state IN ('starting','running','unknown')`, id).Scan(&active); err != nil {
		return nil, err
	}
	if active >= maxConcurrency {
		return nil, tx.Commit(ctx)
	}
	claim := &superSandboxClaim{}
	var command, directory string
	var timeout, index int
	err = tx.QueryRow(ctx, `SELECT id,command,directory,timeout_seconds,task_index FROM super_sandbox_tasks WHERE run_id=$1 AND state='queued' ORDER BY task_index LIMIT 1 FOR UPDATE`, id).Scan(&claim.TaskID, &command, &directory, &timeout, &index)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Detect a dead/recreated generation before selecting any worker. Never
	// silently reinterpret the immutable worker list as replacement machines.
	var changed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM super_sandbox_workers w LEFT JOIN sandboxes s ON s.id=w.sandbox_id WHERE w.run_id=$1 AND (s.id IS NULL OR s.generation<>w.generation OR s.phase IN ('deleted','failed')))`, id).Scan(&changed); err != nil {
		return nil, err
	}
	if changed {
		if _, err = tx.Exec(ctx, `UPDATE super_sandboxes SET state='needs_attention',failure='A worker died or changed generation; no commands will be assigned to its replacement' WHERE id=$1`, id); err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	err = tx.QueryRow(ctx, `SELECT w.worker_id,w.generation
		FROM super_sandbox_workers w JOIN sandboxes s ON s.id=w.sandbox_id AND s.id=w.worker_id
		WHERE w.run_id=$1 AND s.workspace_id=$2 AND s.generation=w.generation
		AND s.phase='ready' AND NOT s.sleep_requested AND NOT s.delete_requested
		AND NOT EXISTS(SELECT 1 FROM super_sandbox_tasks t WHERE t.worker_id=w.worker_id AND t.state IN ('starting','running','unknown'))
		ORDER BY w.worker_index LIMIT 1 FOR UPDATE OF s`, id, workspace).Scan(&claim.WorkerID, &claim.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	jobID, err := newID("job_")
	if err != nil {
		return nil, err
	}
	claim.Request = wrencomputer.JobRequest{
		ID: jobID, Command: command, Directory: directory, TimeoutSeconds: timeout, Sandbox: true,
		Env: map[string]string{"CANTER_SUPER_SANDBOX_ID": id, "CANTER_SUPER_SANDBOX_TASK_ID": claim.TaskID, "CANTER_SUPER_SANDBOX_TASK_INDEX": strconv.Itoa(index)},
	}
	raw, err := json.Marshal(claim.Request)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE super_sandbox_tasks SET state='starting',worker_id=$2,generation=$3,job_id=$4,job_request=$5,started_at=now() WHERE id=$1`, claim.TaskID, claim.WorkerID, claim.Generation, jobID, raw); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sandbox_jobs(sandbox_id,job_id) VALUES($1,$2)`, claim.WorkerID, jobID); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE super_sandboxes SET state='running',started_at=COALESCE(started_at,now()) WHERE id=$1`, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claim, nil
}

func (d *SuperSandboxDispatcher) activeClaims(ctx context.Context, id string) ([]superSandboxClaim, error) {
	rows, err := d.Server.service.Store.pool.Query(ctx, `SELECT id,worker_id,generation,job_request FROM super_sandbox_tasks WHERE run_id=$1 AND state IN ('starting','running','unknown') ORDER BY task_index LIMIT 16`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []superSandboxClaim{}
	for rows.Next() {
		var c superSandboxClaim
		var raw []byte
		if err = rows.Scan(&c.TaskID, &c.WorkerID, &c.Generation, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &c.Request); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Reconcile is directly testable; its bounded sequential RPCs never spawn a
// goroutine per task. A run can claim at most its 16 worker slots per pass.
func (d *SuperSandboxDispatcher) Reconcile(ctx context.Context, workspace, id string) (err error) {
	// Rotate uncertain/long-running batches so they cannot permanently starve
	// later batches outside the bounded scan window. Stamp AFTER every attempt,
	// including failed/timed-out attempts, with a separate bounded DB deadline.
	defer func() {
		stampCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, stampErr := d.Server.service.Store.pool.Exec(stampCtx, `UPDATE super_sandboxes SET last_reconciled_at=clock_timestamp() WHERE workspace_id=$1 AND id=$2`, workspace, id)
		err = errors.Join(err, stampErr)
	}()
	// Revalidate authority even when there is no new work, so revocation also
	// stops owned active jobs. claim commits before any guest contact.
	_, err = d.claim(ctx, workspace, id)
	if err != nil {
		return err
	}
	claims, err := d.activeClaims(ctx, id)
	if err != nil {
		return err
	}
	for _, c := range claims {
		if err = d.observe(ctx, workspace, id, c); err != nil {
			return err
		}
	}
	for i := 0; i < 15; i++ {
		c, err := d.claim(ctx, workspace, id)
		if err != nil {
			return err
		}
		if c == nil {
			break
		}
		if err = d.observe(ctx, workspace, id, *c); err != nil {
			return err
		}
	}
	return d.aggregate(ctx, workspace, id)
}

func (d *SuperSandboxDispatcher) observe(ctx context.Context, workspace, id string, c superSandboxClaim) error {
	s := d.Server.service.Store
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := s.Sandbox(ctx, workspace, c.WorkerID)
	if err != nil {
		return d.unknown(ctx, c, "Worker could not be observed")
	}
	if v.Generation != c.Generation || v.Phase == "deleted" || v.Phase == "failed" {
		return d.unknown(ctx, c, "Worker died or changed generation; the old command will not be replayed on a replacement")
	}
	var client sandboxAgentClient
	if d.client != nil {
		client, err = d.client(ctx, v)
	} else {
		client, err = d.Server.sandboxClient(ctx, v)
	}
	if err != nil {
		return d.unknown(ctx, c, "Worker connection is uncertain")
	}
	defer client.Close()
	var state, taskState string
	if err = s.pool.QueryRow(ctx, `SELECT r.state,t.state FROM super_sandboxes r JOIN super_sandbox_tasks t ON t.run_id=r.id WHERE r.workspace_id=$1 AND r.id=$2 AND t.id=$3`, workspace, id, c.TaskID).Scan(&state, &taskState); err != nil {
		return err
	}
	// Another replica may have positively finished this claim since the active
	// snapshot was taken. Never replay a claim already known to be terminal.
	if taskState != "starting" && taskState != "running" && taskState != "unknown" {
		return nil
	}
	var job wrencomputer.Job
	if state == "cancelling" {
		// StopJob writes a guest cancellation tombstone even before StartJob.
		// Thus a late in-flight start cannot resurrect the command.
		job, err = client.StopJob(ctx, c.Request.ID)
	} else {
		job, err = client.Job(ctx, c.Request.ID)
		if err != nil || job.Status == "unknown" {
			// Replay only the exact persisted request on the original generation.
			// A transport error or not_found alone is never terminal evidence.
			if probeErr := requireSandboxWorkflow(ctx, client); probeErr != nil {
				return d.unknown(ctx, c, "Worker workflow capability could not be confirmed")
			}
			job, err = client.StartJob(ctx, c.Request)
		}
		// Cancellation can race a start; close it with the same ID, never with
		// a worker-wide stop or lifecycle operation.
		var nowState string
		if readErr := s.pool.QueryRow(ctx, `SELECT state FROM super_sandboxes WHERE id=$1`, id).Scan(&nowState); readErr != nil {
			return readErr
		}
		if nowState == "cancelling" {
			job, err = client.StopJob(ctx, c.Request.ID)
		}
	}
	if err != nil || job.ID != c.Request.ID {
		return d.unknown(ctx, c, "Guest job receipt could not be confirmed; worker remains reserved")
	}
	taskState, terminal := superSandboxJobState(job)
	if taskState == "unknown" {
		return d.unknown(ctx, c, "Guest job status is unknown; worker remains reserved")
	}
	if !terminal {
		_, err = s.pool.Exec(ctx, `UPDATE super_sandbox_tasks SET state=$2,error='' WHERE id=$1 AND state IN ('starting','running','unknown')`, c.TaskID, taskState)
		s.touchSandbox(ctx, c.WorkerID)
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE super_sandbox_tasks SET state=$2,exit_code=$3,error=$4,finished_at=now() WHERE id=$1 AND state IN ('starting','running','unknown')`, c.TaskID, taskState, job.ExitCode, superSandboxGuestError(job.Error)); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox_jobs SET finished_at=now() WHERE sandbox_id=$1 AND job_id=$2 AND finished_at IS NULL`, c.WorkerID, c.Request.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Positive guest enums only. Do not use sandboxJobDone: unknown is not done.
func superSandboxJobState(job wrencomputer.Job) (string, bool) {
	switch job.Status {
	case "starting":
		return "starting", false
	case "running":
		return "running", false
	case "completed":
		if job.ExitCode == nil {
			return "unknown", false
		}
		if *job.ExitCode == 0 {
			return "succeeded", true
		}
		return "failed", true
	case "failed", "timed_out":
		return "failed", true
	case "cancelled":
		return "cancelled", true
	default:
		return "unknown", false
	}
}

// Guest errors can contain command output/secrets. Keep durable batch errors
// generic; detailed job evidence remains on the existing authorized endpoints.
func superSandboxGuestError(message string) string {
	if message != "" {
		return "Guest job reported an error; inspect the job logs"
	}
	return ""
}

func (d *SuperSandboxDispatcher) unknown(ctx context.Context, c superSandboxClaim, message string) error {
	// The RPC may have consumed its deadline. Use the caller's bounded parent
	// on the next pass if this write fails; starting also retains the lease.
	_, err := d.Server.service.Store.pool.Exec(ctx, `UPDATE super_sandbox_tasks SET state='unknown',error=$2 WHERE id=$1 AND state IN ('starting','running','unknown')`, c.TaskID, message)
	return err
}

func (d *SuperSandboxDispatcher) aggregate(ctx context.Context, workspace, id string) error {
	s := d.Server.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state, failure string
	var finished *time.Time
	if err = tx.QueryRow(ctx, `SELECT state,failure,finished_at FROM super_sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id).Scan(&state, &failure, &finished); err != nil {
		return err
	}
	if finished != nil {
		return nil
	}
	var queued, active, unknown, failed, cancelled int
	if err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='queued'),count(*) FILTER (WHERE state IN ('starting','running','unknown')),count(*) FILTER (WHERE state='unknown'),count(*) FILTER (WHERE state='failed'),count(*) FILTER (WHERE state='cancelled') FROM super_sandbox_tasks WHERE run_id=$1`, id).Scan(&queued, &active, &unknown, &failed, &cancelled); err != nil {
		return err
	}
	done := queued == 0 && active == 0
	if done {
		switch {
		case failure != "", failed > 0:
			state = "failed"
		case state == "cancelling", cancelled > 0:
			state = "cancelled"
		default:
			state = "succeeded"
		}
	} else if state != "cancelling" {
		switch {
		case unknown > 0, failure != "":
			state = "needs_attention"
		case active > 0:
			state = "running"
		default:
			state = "queued"
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE super_sandboxes SET state=$2,finished_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1`, id, state, done); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) superSandboxWorkerBusy(ctx context.Context, worker string) (bool, error) {
	var busy bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM super_sandbox_tasks WHERE worker_id=$1 AND state IN ('starting','running','unknown'))`, worker).Scan(&busy)
	return busy, err
}
