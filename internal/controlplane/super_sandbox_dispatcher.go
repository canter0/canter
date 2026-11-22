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
	// observeTimeout bounds one guest observation; a test seam, default 10s.
	observeTimeout time.Duration
	// pendingRuns is a test seam for queue-scan failures; nil uses pending.
	pendingRuns func(context.Context) ([][2]string, error)
}

// Task-state reason codes persisted on uncertain or evidence-closed tasks.
// Keep them generic: never raw guest errors or command output.
const (
	superSandboxReasonMissingExitCode  = "missing_exit_code"
	superSandboxReasonWorkerInterrupt  = "worker_interrupted"
	superSandboxReasonUnconfirmed      = "receipt_unconfirmed"
	superSandboxReasonUnobservable     = "worker_unobservable"
	superSandboxReasonGenerationChange = "generation_changed"
	superSandboxReasonWorkerRemoved    = "worker_removed"
	superSandboxReasonStartRejected    = "start_rejected"
)

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
	// A failing queue scan is a degraded scheduler, not a fatal server error:
	// back off (2s doubling to 30s) and keep joining in-flight runs.
	var backoff time.Duration
	for {
		// Even with the feature flag withdrawn, observe and cancel persisted
		// active work. Do not scan/hot-spin while all four slots are occupied.
		if poll && len(inflight) < concurrency {
			pending := d.pending
			if d.pendingRuns != nil {
				pending = d.pendingRuns
			}
			items, err := pending(workerCtx)
			if err != nil {
				if workerCtx.Err() != nil {
					return workerCtx.Err()
				}
				if backoff == 0 {
					slog.Error("super sandbox queue scan failed; scheduling degraded until recovery", "error", err)
					backoff = 2 * time.Second
				} else if backoff < 30*time.Second {
					backoff *= 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
				timer := time.NewTimer(backoff)
				waiting := true
				for waiting {
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case id := <-completed:
						delete(inflight, id)
					case <-timer.C:
						waiting = false
					}
				}
				poll = true
				continue
			}
			if backoff != 0 {
				slog.Info("super sandbox queue scan recovered")
				backoff = 0
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

// claim preserves the original signature for existing callers; admission is
// always enabled through this path.
func (d *SuperSandboxDispatcher) claim(ctx context.Context, workspace, id string) (*superSandboxClaim, error) {
	return d.gateClaim(ctx, workspace, id, true)
}

// gateClaim checks authority and cancellation in the same transaction that
// may assign a worker and record BOTH the batch task and existing
// sandbox_jobs handle. The workspace lock coordinates different runs and
// product/role mutations; the partial unique index additionally enforces
// cross-replica exclusion. With admit=false it only refreshes
// authority/cancellation state and never assigns new work.
func (d *SuperSandboxDispatcher) gateClaim(ctx context.Context, workspace, id string, admit bool) (*superSandboxClaim, error) {
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
	if err = tx.QueryRow(ctx, `SELECT id FROM super_sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT state,creator_account_id,max_concurrency,finished_at FROM super_sandboxes WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&state, &account, &maxConcurrency, &finished); err != nil {
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
	if !admit {
		return nil, tx.Commit(ctx)
	}
	// A persisted-but-unstarted claim is never launched while another task in
	// this run is uncertain or in a rejection close: read the live task rows
	// under this same lock, never the pre-aggregated batch state alone.
	var blocked bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM super_sandbox_tasks WHERE run_id=$1 AND (state='unknown' OR (resolution_mode IS NOT NULL AND state IN ('starting','running'))))`, id).Scan(&blocked); err != nil {
		return nil, err
	}
	if blocked {
		return nil, tx.Commit(ctx)
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM super_sandbox_tasks WHERE run_id=$1 AND state IN ('starting','running')`, id).Scan(&active); err != nil {
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
	// Rotate by least recently attempted, then task index, so a bounded pass
	// cannot permanently starve late-index tasks.
	rows, err := d.Server.service.Store.pool.Query(ctx, `SELECT id,worker_id,generation,job_request FROM super_sandbox_tasks WHERE run_id=$1 AND state IN ('starting','running','unknown') ORDER BY COALESCE(last_attempt_at,'-infinity'::timestamptz),task_index LIMIT 16`, id)
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
	// stops owned active jobs. Refresh only: no new admission this step.
	if _, err = d.gateClaim(ctx, workspace, id, false); err != nil {
		return err
	}
	claims, err := d.activeClaims(ctx, id)
	if err != nil {
		return err
	}
	// Observe independent tasks in up to two in-flight guest sequences. One
	// failed or expired observation records its own uncertainty and must not
	// abort the other peer observations in this pass.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 2)
	errCh := make(chan error, len(claims))
	for _, c := range claims {
		c := c
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		if ctx.Err() != nil {
			<-slots
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			errCh <- d.observe(ctx, workspace, id, c)
		}()
	}
	wg.Wait()
	close(errCh)
	var obsErr error
	for e := range errCh {
		obsErr = errors.Join(obsErr, e)
	}
	err = errors.Join(err, obsErr)
	if err != nil && ctx.Err() != nil {
		return err
	}
	// Claim serially; observe each newly committed claim before the next so a
	// committed-but-uncertain start blocks further launches this same pass.
	for i := 0; i < 16; i++ {
		c, cerr := d.claim(ctx, workspace, id)
		if cerr != nil {
			return errors.Join(err, cerr)
		}
		if c == nil {
			break
		}
		if cerr = d.observe(ctx, workspace, id, *c); cerr != nil {
			return errors.Join(err, cerr)
		}
	}
	return errors.Join(err, d.aggregate(ctx, workspace, id))
}

// withSuperSandboxRunLock serializes every receipt write behind the run's row
// lock so committed evidence and the claim gate order correctly against each
// other. The write deadline is derived from the RUN context: the observation
// RPC may consume its own deadline without aborting peer work.
func (d *SuperSandboxDispatcher) withSuperSandboxRunLock(ctx context.Context, workspace, id string, fn func(context.Context, pgx.Tx) error) error {
	return d.withSuperSandboxLocks(ctx, workspace, id, false, fn)
}

// withSuperSandboxLocks optionally takes the workspace row lock first, then
// the run row lock, matching gateClaim/CancelSuperSandbox lock order so a
// mayStart gate can never deadlock against an in-flight claim or cancel.
func (d *SuperSandboxDispatcher) withSuperSandboxLocks(ctx context.Context, workspace, id string, lockWorkspace bool, fn func(context.Context, pgx.Tx) error) error {
	write, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx, err := d.Server.service.Store.pool.Begin(write)
	if err != nil {
		return err
	}
	defer tx.Rollback(write)
	if lockWorkspace {
		var ws string
		if err = tx.QueryRow(write, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&ws); err != nil {
			return err
		}
	}
	var locked string
	if err = tx.QueryRow(write, `SELECT id FROM super_sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id).Scan(&locked); err != nil {
		return err
	}
	if err = fn(write, tx); err != nil {
		return err
	}
	return tx.Commit(write)
}

func (d *SuperSandboxDispatcher) observe(ctx context.Context, workspace, id string, c superSandboxClaim) error {
	s := d.Server.service.Store
	// Stamp the attempt before any guest contact so rotation and starvation
	// accounting reflect this pass even if the RPC hangs to its deadline.
	if _, err := s.pool.Exec(ctx, `UPDATE super_sandbox_tasks SET last_attempt_at=now() WHERE id=$1`, c.TaskID); err != nil {
		return err
	}
	timeout := d.observeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	obsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Confirmed removal of the exact assigned provider generation is the only
	// receipt-free release: task and job handle settle in one transaction.
	var removed bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_destroyed_generations WHERE workspace_id=$1 AND sandbox_id=$2 AND generation=$3)`, workspace, c.WorkerID, c.Generation).Scan(&removed); err != nil {
		return err
	}
	if removed {
		return d.workerRemoved(ctx, workspace, id, c)
	}
	v, err := s.Sandbox(obsCtx, workspace, c.WorkerID)
	if err != nil {
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnobservable, "Worker could not be observed")
	}
	// Phase/generation alone is never release evidence; hold the reservation.
	if v.Generation != c.Generation || v.Phase == "deleted" || v.Phase == "failed" {
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonGenerationChange, "Worker died or changed generation; the old command will not be replayed on a replacement")
	}
	var client sandboxAgentClient
	if d.client != nil {
		client, err = d.client(obsCtx, v)
	} else {
		client, err = d.Server.sandboxClient(obsCtx, v)
	}
	if err != nil {
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnobservable, "Worker connection is uncertain")
	}
	defer client.Close()
	var state, taskState, resolution string
	if err = s.pool.QueryRow(ctx, `SELECT r.state,t.state,COALESCE(t.resolution_mode,'') FROM super_sandboxes r JOIN super_sandbox_tasks t ON t.run_id=r.id WHERE r.workspace_id=$1 AND r.id=$2 AND t.id=$3`, workspace, id, c.TaskID).Scan(&state, &taskState, &resolution); err != nil {
		return err
	}
	// Another replica may have positively finished this claim since the active
	// snapshot was taken. Never replay a claim already known to be terminal.
	if taskState != "starting" && taskState != "running" && taskState != "unknown" {
		return nil
	}
	var job wrencomputer.Job
	switch {
	case resolution == "close_rejected_start":
		// Rejection close honors the persisted mode: retry only the close,
		// never the launch. A matching cancelled receipt closes as failed; a
		// matching terminal receipt is preserved instead.
		job, err = client.StopJob(obsCtx, c.Request.ID)
		if err != nil || job.ID != c.Request.ID {
			return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnconfirmed, "Rejection close receipt could not be confirmed; worker remains reserved")
		}
		if job.Status == "interrupted" {
			return d.unknownReason(ctx, workspace, id, c, superSandboxReasonWorkerInterrupt, "Rejection close was interrupted; worker remains reserved")
		}
		// Known guest terminal statuses only; ordinary release rules do not
		// apply inside a rejection close.
		receiptState, receiptTerminal := superSandboxJobState(job)
		if job.Status == "completed" && job.ExitCode == nil {
			return d.unknownReason(ctx, workspace, id, c, superSandboxReasonMissingExitCode, "Rejection close reported completion without an exit code; worker remains reserved")
		}
		if job.Status == "cancelled" {
			return d.settle(ctx, workspace, id, c, "failed", superSandboxReasonStartRejected, "Start rejected; job closed", nil, "")
		}
		if receiptTerminal {
			return d.settle(ctx, workspace, id, c, receiptState, "", superSandboxGuestError(job.Error), job.ExitCode, "")
		}
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnconfirmed, "Rejection close has not produced a terminal receipt; worker remains reserved")
	case state == "cancelling":
		// StopJob writes a guest cancellation tombstone even before StartJob.
		// Thus a late in-flight start cannot resurrect the command.
		job, err = client.StopJob(obsCtx, c.Request.ID)
	default:
		job, err = client.Job(obsCtx, c.Request.ID)
		if err != nil || job.Status == "unknown" {
			var remote *wrencomputer.Error
			switch {
			case err != nil && errors.As(err, &remote) && remote.Code == "not_found":
				// Positive absence of the exact persisted ID: replay the
				// original stored request, never a newly minted job.
				if probeErr := requireSandboxWorkflow(obsCtx, client); probeErr != nil {
					return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnobservable, "Worker workflow capability could not be confirmed")
				}
				// The Probe may have blocked while authority, cancellation, or
				// sibling uncertainty changed. Re-gate under the run lock; no
				// database handle is held across the guest call itself.
				allowed, gateErr := d.mayStart(ctx, workspace, id, c)
				if gateErr != nil {
					return gateErr
				}
				if !allowed {
					return nil
				}
				job, err = client.StartJob(obsCtx, c.Request)
				if err != nil {
					var rejected *wrencomputer.Error
					if errors.As(err, &rejected) && rejected.Code == "invalid_directory" {
						return d.rejectStart(ctx, workspace, id, c)
					}
				}
			default:
				// A transport/read error or an unknown status is uncertainty,
				// never absence: keep the reservation, retry the read next pass.
				return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnconfirmed, "Guest job receipt could not be confirmed; worker remains reserved")
			}
		}
		// Cancellation can race a start; close it with the same ID, never with
		// a worker-wide stop or lifecycle operation.
		var nowState string
		if readErr := s.pool.QueryRow(ctx, `SELECT state FROM super_sandboxes WHERE id=$1`, id).Scan(&nowState); readErr != nil {
			return readErr
		}
		if nowState == "cancelling" {
			job, err = client.StopJob(obsCtx, c.Request.ID)
		}
	}
	if err != nil || job.ID != c.Request.ID {
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnconfirmed, "Guest job receipt could not be confirmed; worker remains reserved")
	}
	receiptState, _ := superSandboxJobState(job)
	switch {
	case job.Status == "interrupted":
		// The guest noticed the supervisor disappeared, not that the separate
		// command process exited; keep the reservation explicitly unresolved.
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonWorkerInterrupt, "Guest reported an interrupted job; worker remains reserved")
	case job.Status == "completed" && job.ExitCode == nil:
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonMissingExitCode, "Guest reported completion without an exit code; worker remains reserved")
	case receiptState == "unknown":
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnconfirmed, "Guest job status is unknown; worker remains reserved")
	case receiptState == "running" || receiptState == "starting":
		err = d.withSuperSandboxRunLock(ctx, workspace, id, func(wctx context.Context, tx pgx.Tx) error {
			// A stale observation must not revert a persisted close mode.
			_, e := tx.Exec(wctx, `UPDATE super_sandbox_tasks SET state=$2,error='',reason=NULL WHERE id=$1 AND state IN ('starting','running','unknown') AND resolution_mode IS NULL`, c.TaskID, receiptState)
			return e
		})
		s.touchSandbox(ctx, c.WorkerID)
		return err
	case sandboxJobTerminal(job.Status):
		return d.settle(ctx, workspace, id, c, receiptState, "", superSandboxGuestError(job.Error), job.ExitCode, "")
	default:
		return d.unknownReason(ctx, workspace, id, c, superSandboxReasonUnconfirmed, "Guest job receipt could not be confirmed; worker remains reserved")
	}
}

// rejectStart records an explicit typed pre-launch rejection. The first pass
// persists the close mode only; the rejection-path stop is sent on the next
// pass and the persisted ID is never started again.
func (d *SuperSandboxDispatcher) rejectStart(ctx context.Context, workspace, id string, c superSandboxClaim) error {
	return d.withSuperSandboxRunLock(ctx, workspace, id, func(wctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(wctx, `UPDATE super_sandbox_tasks SET state='unknown',reason=$2,resolution_mode='close_rejected_start',error=$3 WHERE id=$1 AND state IN ('starting','running','unknown')`, c.TaskID, superSandboxReasonStartRejected, "Start rejected; closing the persisted job ID")
		return err
	})
}

// mayStart re-gates a positive-absence replay immediately before the guest
// call: workspace lock, then the run lock, rechecking authority, the exact
// worker generation, this task's mode, batch cancellation, and sibling
// uncertainty. No database handle is held across the subsequent RPC.
func (d *SuperSandboxDispatcher) mayStart(ctx context.Context, workspace, id string, c superSandboxClaim) (allowed bool, err error) {
	err = d.withSuperSandboxLocks(ctx, workspace, id, true, func(wctx context.Context, tx pgx.Tx) error {
		enabled, err := d.Server.service.Store.productEnabled(wctx, tx, workspace, "sandboxes")
		if err != nil {
			return err
		}
		var account *string
		var state string
		var finished *time.Time
		if err = tx.QueryRow(wctx, `SELECT creator_account_id,state,finished_at FROM super_sandboxes WHERE id=$1`, id).Scan(&account, &state, &finished); err != nil {
			return err
		}
		var role string
		if account != nil {
			role, err = workspaceRole(wctx, tx, workspace, *account, true)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if finished != nil || state == "cancelling" || !d.Server.config.Sandboxes.Enabled || !enabled || (role != "owner" && role != "operator") {
			return nil
		}
		return tx.QueryRow(wctx, `SELECT t.state IN ('starting','running','unknown') AND t.resolution_mode IS NULL
			AND EXISTS(SELECT 1 FROM sandboxes s WHERE s.workspace_id=$2 AND s.id=$3 AND s.generation=$4 AND s.phase='ready' AND NOT s.sleep_requested AND NOT s.delete_requested)
			AND NOT EXISTS(SELECT 1 FROM super_sandbox_tasks o WHERE o.run_id=$1 AND o.id<>t.id AND (o.state='unknown' OR (o.resolution_mode IS NOT NULL AND o.state IN ('starting','running'))))
			FROM super_sandbox_tasks t WHERE t.id=$5 AND t.run_id=$1`, id, workspace, c.WorkerID, c.Generation, c.TaskID).Scan(&allowed)
	})
	return allowed, err
}

// workerRemoved settles a task whose exact provider generation was confirmed
// destroyed. It keeps the recorded worker/job IDs, fabricates no exit code,
// and releases the reservation by closing the handle in the same transaction.
func (d *SuperSandboxDispatcher) workerRemoved(ctx context.Context, workspace, id string, c superSandboxClaim) error {
	return d.settle(ctx, workspace, id, c, "outcome_unavailable", superSandboxReasonWorkerRemoved, "Worker was removed before the job outcome was recorded", nil, "A worker was removed before its command outcome was recorded")
}

// settle commits a terminal receipt for the exact persisted job ID and closes
// the ordinary sandbox_jobs handle in the same transaction. runFailure, when
// nonempty, is recorded on the batch under the same lock so a terminal worker
// removal is never misread as an ordinary success after a rollback.
func (d *SuperSandboxDispatcher) settle(ctx context.Context, workspace, id string, c superSandboxClaim, state, reason, message string, exitCode *int, runFailure string) error {
	return d.withSuperSandboxRunLock(ctx, workspace, id, func(wctx context.Context, tx pgx.Tx) error {
		var reasonArg any
		if reason != "" {
			reasonArg = reason
		}
		updated, err := tx.Exec(wctx, `UPDATE super_sandbox_tasks SET state=$2,reason=$3,exit_code=$4,error=$5,finished_at=now() WHERE id=$1 AND run_id=$6 AND job_id=$7 AND state IN ('starting','running','unknown')`, c.TaskID, state, reasonArg, exitCode, message, id, c.Request.ID)
		if err != nil {
			return err
		}
		// A stale observation (e.g. delayed destruction evidence) must not
		// close the job or rewrite run failure after a terminal receipt won.
		if updated.RowsAffected() == 0 {
			return nil
		}
		if _, err := tx.Exec(wctx, `UPDATE sandbox_jobs SET finished_at=now() WHERE sandbox_id=$1 AND job_id=$2 AND finished_at IS NULL`, c.WorkerID, c.Request.ID); err != nil {
			return err
		}
		if runFailure != "" {
			if _, err := tx.Exec(wctx, `UPDATE super_sandboxes SET failure=$2 WHERE id=$1 AND failure=''`, id, runFailure); err != nil {
				return err
			}
		}
		return nil
	})
}

// Positive guest enums only. Do not use sandboxJobDone: unknown is not done.
func superSandboxJobState(job wrencomputer.Job) (state string, terminal bool) {
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

func (d *SuperSandboxDispatcher) unknownReason(ctx context.Context, workspace, id string, c superSandboxClaim, reason, message string) error {
	// The RPC may have consumed its deadline. Use the caller's bounded parent
	// on the next pass if this write fails; starting also retains the lease.
	// Close mode, once persisted, is never cleared by a nonterminal write.
	return d.withSuperSandboxRunLock(ctx, workspace, id, func(wctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(wctx, `UPDATE super_sandbox_tasks SET state='unknown',reason=$2,error=$3 WHERE id=$1 AND state IN ('starting','running','unknown')`, c.TaskID, reason, message)
		return err
	})
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
	var queued, active, unknown, failed, cancelled, unavailable int
	if err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='queued'),count(*) FILTER (WHERE state IN ('starting','running','unknown')),count(*) FILTER (WHERE state='unknown'),count(*) FILTER (WHERE state='failed'),count(*) FILTER (WHERE state='cancelled'),count(*) FILTER (WHERE state='outcome_unavailable') FROM super_sandbox_tasks WHERE run_id=$1`, id).Scan(&queued, &active, &unknown, &failed, &cancelled, &unavailable); err != nil {
		return err
	}
	done := queued == 0 && active == 0
	if done {
		switch {
		case unavailable > 0:
			state = "failed"
			if failure == "" {
				failure = "A worker was removed before its command outcome was recorded"
			}
		case failure != "" || failed > 0:
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
	if _, err = tx.Exec(ctx, `UPDATE super_sandboxes SET state=$2,failure=$3,finished_at=CASE WHEN $4 THEN now() ELSE NULL END WHERE id=$1`, id, state, failure, done); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) superSandboxWorkerBusy(ctx context.Context, worker string) (bool, error) {
	var busy bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM super_sandbox_tasks WHERE worker_id=$1 AND state IN ('starting','running','unknown'))`, worker).Scan(&busy)
	return busy, err
}
