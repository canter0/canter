package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/canter0/canter/pricing"
)

// ciRunnerLifecycleBackend is the trusted provider seam. A real adapter must
// verify exact operation ownership immediately before Stop and Delete, act on
// the server first, and only then ports and security groups. A nil backend
// fails closed; managed runtime modes wire it explicitly at startup.
type ciRunnerLifecycleBackend interface {
	Observe(ctx context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) (ciResourceObservation, error)
	Stop(ctx context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) error
	Delete(ctx context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) error
	SettleEnforcer(ctx context.Context, plan ciRunnerLifecyclePlan) error
}

var errCILifecycleSettlementPending = errors.New("CI settlement awaits server completion proof")

type ciRunnerLifecycleWorker struct {
	Store   *Store
	Backend ciRunnerLifecycleBackend
	GitHub  *ciGitHubClient
	// UnstartedEnforcer, when set, lets Reconcile replay a stop-only creation
	// rejection for claimed-but-never-started runners before the session
	// advisory lock is taken. It is used for Stop only; Backend remains the
	// independent observer. Nil leaves the generic worker unchanged.
	UnstartedEnforcer *ciEnforcerClient
}

const (
	ciLifecyclePollInterval   = 2 * time.Second
	ciLifecycleReconcileBound = 2 * time.Minute
	ciLifecycleConcurrency    = 2
	ciLifecycleDueLimit       = 30
)

// Run polls due lifecycles until ctx ends. It is wired only through an
// explicitly configured managed runtime.
func (w *ciRunnerLifecycleWorker) Run(ctx context.Context) error {
	if w == nil || w.Store == nil {
		return errors.New("ci lifecycle worker requires a store")
	}
	ticker := time.NewTicker(ciLifecyclePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.drainDue(ctx)
		}
	}
}

func (w *ciRunnerLifecycleWorker) drainDue(ctx context.Context) {
	rows, err := w.Store.pool.Query(ctx, `SELECT l.workspace_id,l.runner_id FROM ci_runner_lifecycles l JOIN ci_runners r ON r.workspace_id=l.workspace_id AND r.id=l.runner_id WHERE l.next_attempt_at<=$1 AND (r.phase<>'deleted' OR (r.execution_started_at IS NOT NULL AND r.execution_stopped_at IS NULL)) ORDER BY l.next_attempt_at,l.runner_id LIMIT 30`, w.Store.now())
	if err != nil {
		return
	}
	type due struct{ workspace, runner string }
	var items []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.workspace, &d.runner); err != nil {
			rows.Close()
			return
		}
		items = append(items, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return
	}
	sem := make(chan struct{}, ciLifecycleConcurrency)
	var wg sync.WaitGroup
	for _, d := range items {
		wg.Add(1)
		go func(d due) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rctx, cancel := context.WithTimeout(ctx, ciLifecycleReconcileBound)
			defer cancel()
			if rerr := w.Reconcile(rctx, d.workspace, d.runner); rerr != nil {
				log.Printf("ci lifecycle reconcile %s/%s: %v", d.workspace, d.runner, rerr)
			}
		}(d)
	}
	wg.Wait()
}

// Reconcile advances one lifecycle under a session advisory lock so only one
// worker owns a runner at a time.
func (w *ciRunnerLifecycleWorker) Reconcile(ctx context.Context, workspace, runner string) error {
	if w == nil || w.Store == nil {
		return errors.New("ci lifecycle worker requires a store")
	}
	if w.UnstartedEnforcer != nil {
		if err := w.Store.recoverUnstartedCILifecycle(ctx, workspace, runner, w.UnstartedEnforcer); err != nil {
			return err
		}
	}
	conn, err := w.Store.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The advisory lock lives on a dedicated hijacked session outside pool
	// capacity: it is never returned to the pool, closing it releases the
	// lock, and Store transactions keep their own pool slots so a
	// MaxConns=1 store cannot deadlock behind the held session.
	session := conn.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = session.Close(closeCtx)
	}()
	key := "ci/" + workspace + "/" + runner
	var locked bool
	if err = session.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,948))`, key).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	return w.reconcileLocked(ctx, workspace, runner)
}

func ciLifecycleHasServer(resources []ciOwnedResource) bool {
	for _, r := range resources {
		if r.Kind == "server" {
			return true
		}
	}
	return false
}

// ciLifecycleRetry reschedules the next attempt without storing raw provider
// or source errors.
func (s *Store) ciLifecycleRetry(ctx context.Context, workspace, runner string, snap ciRunnerLifecycleSnapshot, markCleanupFailed bool) {
	now := s.now()
	if markCleanupFailed {
		_, _ = s.pool.Exec(ctx, `UPDATE ci_runners SET phase=CASE WHEN delete_requested THEN 'cleanup_failed' ELSE phase END,updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, runner, now)
	}
	_, _ = s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET next_attempt_at=$4 WHERE workspace_id=$1 AND runner_id=$2 AND operation_id=$3`, workspace, runner, snap.Plan.OperationID, now.Add(3*time.Second))
}

func (s *Store) ciLifecycleSaveEvidence(ctx context.Context, workspace, runner string, snap ciRunnerLifecycleSnapshot, combined []ciOwnedResource, createSettled, stopped bool) error {
	return s.ciLifecycleSaveEvidenceWith(ctx, s.pool, workspace, runner, snap, combined, createSettled, stopped)
}

func (s *Store) ciLifecycleSaveEvidenceWith(ctx context.Context, db ciLifecycleDB, workspace, runner string, snap ciRunnerLifecycleSnapshot, combined []ciOwnedResource, createSettled, stopped bool) error {
	raw, err := json.Marshal(combined)
	if err != nil {
		return err
	}
	now := s.now()
	tag, err := db.Exec(ctx, `UPDATE ci_runner_lifecycles SET resources=$4,create_settled=create_settled OR $5,stopped_verified_at=CASE WHEN $6 THEN COALESCE(stopped_verified_at,$7) ELSE stopped_verified_at END,next_attempt_at=$8 WHERE workspace_id=$1 AND runner_id=$2 AND operation_id=$3`, workspace, runner, snap.Plan.OperationID, raw, createSettled, stopped, now, now.Add(3*time.Second))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// ciLifecycleCompletion asks GitHub for the source-proved completion time of
// the admitted job. Pending answers and source errors are not completion.
func (w *ciRunnerLifecycleWorker) ciLifecycleCompletion(ctx context.Context, snap ciRunnerLifecycleSnapshot, now time.Time) (time.Time, bool) {
	if w.GitHub == nil || snap.GitHubInstallationID == nil || len(snap.Decision) == 0 {
		return time.Time{}, false
	}
	var decision ciAdmissionDecision
	if err := json.Unmarshal(snap.Decision, &decision); err != nil {
		return time.Time{}, false
	}
	proof := decision.Proof
	if proof.JobID <= 0 || proof.RepositoryID <= 0 || proof.RunID <= 0 || proof.Attempt <= 0 ||
		proof.RunnerID <= 0 || proof.StartedAt.IsZero() {
		return time.Time{}, false
	}
	token, err := w.GitHub.runnerSourceToken(ctx, *snap.GitHubInstallationID, proof.RepositoryID)
	if err != nil {
		return time.Time{}, false
	}
	at, err := w.GitHub.completedRunnerJob(ctx, token, proof, now)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// ciLifecycleObserve runs one fresh backend observation through the root
// evidence validator. The returned inventory is monotonic: retained and
// newly discovered verified IDs are merged and never lost.
func (w *ciRunnerLifecycleWorker) ciLifecycleObserve(ctx context.Context, snap ciRunnerLifecycleSnapshot, retained []ciOwnedResource) (ciResourceObservation, []ciOwnedResource, bool, bool, error) {
	obs, err := w.Backend.Observe(ctx, snap.Plan, retained)
	if err != nil {
		return ciResourceObservation{}, nil, false, false, err
	}
	combined, stopped, absent, err := ciResourceEvidence(snap.Plan.OperationID, retained, obs)
	if err != nil {
		return obs, nil, false, false, err
	}
	// A source-proven running job must have a known server identity before
	// stop, deletion, or settlement can be accepted; empty inventories never
	// prove anything for it.
	if snap.ExecutionStartedAt != nil && !ciLifecycleHasServer(combined) {
		stopped, absent = false, false
	}
	return obs, combined, stopped, absent, nil
}

// ciLifecycleSettle performs the only allowed financial write: the root
// settleCIBudget, and only after independent stop proof. The effective stop is
// the earlier of the frozen billing ceiling and a server-proved completion.
// Paid runs beyond the free hour without completion proof defer settlement —
// physical cleanup still proceeds.
func (w *ciRunnerLifecycleWorker) ciLifecycleSettle(ctx context.Context, workspace, runner string, snap ciRunnerLifecycleSnapshot, completedAt *time.Time, now time.Time) error {
	if snap.ExecutionStartedAt == nil || snap.ExecutionStoppedAt != nil || snap.BillStopAt == nil {
		return nil
	}
	effective := snap.BillStopAt.UTC().Truncate(time.Microsecond)
	started := snap.ExecutionStartedAt.UTC().Truncate(time.Microsecond)
	needProof := snap.PaidOverrunEnabled && effective.Sub(started) > time.Duration(pricing.CIFreeSeconds)*time.Second
	proof := completedAt
	if needProof && proof == nil {
		if at, ok := w.ciLifecycleCompletion(ctx, snap, now); ok {
			proof = &at
		}
	}
	if proof != nil {
		at := proof.UTC().Truncate(time.Microsecond)
		if at.Before(effective) {
			effective = at
		}
	}
	if needProof && proof == nil {
		return errCILifecycleSettlementPending
	}
	return w.Store.settleCIBudget(ctx, workspace, runner, effective)
}

// ciLifecycleStopLimit is the effective billing ceiling candidate: the
// earlier of the runner execution deadline and the plan hard deadline.
func ciLifecycleStopLimit(snap ciRunnerLifecycleSnapshot) time.Time {
	limit := snap.Plan.HardDeadlineAt
	if snap.DeadlineAt != nil && snap.DeadlineAt.Before(limit) {
		limit = *snap.DeadlineAt
	}
	return limit
}

// ciLifecycleRequestStop records a stop intent and refreshes the snapshot
// and clock. A conflict means another writer already owns the intent.
func (w *ciRunnerLifecycleWorker) ciLifecycleRequestStop(ctx context.Context, workspace, runner, reason string, completedAt *time.Time, snap *ciRunnerLifecycleSnapshot, now *time.Time) error {
	if _, err := w.Store.requestCIRunnerStop(ctx, workspace, runner, reason, completedAt); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	}
	fresh, err := w.Store.loadCIRunnerLifecycle(ctx, workspace, runner)
	if err != nil {
		return err
	}
	*snap = fresh
	*now = w.Store.now()
	return nil
}

func (w *ciRunnerLifecycleWorker) reconcileLocked(ctx context.Context, workspace, runner string) error {
	snap, err := w.Store.loadCIRunnerLifecycle(ctx, workspace, runner)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now := w.Store.now()

	// Physical cleanup already proven: only financial settlement may be
	// retried, with no provider calls.
	if snap.CleanupVerifiedAt != nil {
		return w.ciLifecycleSettle(ctx, workspace, runner, snap, nil, w.Store.now())
	}

	var completedAt *time.Time

	// Deadline and setup enforcement take priority over any source lookup:
	// a GitHub outage must never hold a known deadline open.
	if snap.StopRequestedAt == nil {
		reason := ""
		if snap.ExecutionStartedAt != nil {
			if !now.Before(ciLifecycleStopLimit(snap)) {
				reason = "deadline"
			}
		} else if !now.Before(snap.Plan.SetupDeadlineAt) {
			reason = "setup_failed"
		}
		if reason != "" {
			if err := w.ciLifecycleRequestStop(ctx, workspace, runner, reason, nil, &snap, &now); err != nil {
				return err
			}
		}
	}

	// Natural completion is consulted only while no stop intent exists.
	if snap.StopRequestedAt == nil && snap.ExecutionStartedAt != nil {
		if at, ok := w.ciLifecycleCompletion(ctx, snap, now); ok {
			completedAt = &at
			if err := w.ciLifecycleRequestStop(ctx, workspace, runner, "completed", &at, &snap, &now); err != nil {
				return err
			}
		} else {
			// Pending or unavailable source: refresh the clock — an RPC that
			// crossed a deadline boundary must not defer the stop.
			now = w.Store.now()
			if !now.Before(ciLifecycleStopLimit(snap)) {
				if err := w.ciLifecycleRequestStop(ctx, workspace, runner, "deadline", nil, &snap, &now); err != nil {
					return err
				}
			}
		}
	}

	var combined []ciOwnedResource
	if w.Backend != nil {
		obs, merged, isStopped, _, err := w.ciLifecycleObserve(ctx, snap, snap.Resources)
		if err != nil {
			w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, errors.Is(err, ErrConflict) && snap.StopRequestedAt != nil)
			return err
		}
		if err := w.Store.ciLifecycleSaveEvidence(ctx, workspace, runner, snap, merged, obs.CreateSettled, isStopped); err != nil {
			return err
		}
		combined = merged
		snap.Resources = merged
	} else {
		combined = snap.Resources
	}

	if snap.StopRequestedAt == nil || w.Backend == nil {
		// No stop intent: only the monotonic inventory was persisted. With
		// no backend there is no provider proof, so nothing else may run.
		return nil
	}

	// A source-proven running job must show at least one server identity
	// in the retained plus discovered inventory before stop, deletion, or
	// completion can be accepted. Observation keeps running so a late
	// discovery can still supply the missing ID.
	if snap.ExecutionStartedAt != nil && !ciLifecycleHasServer(combined) {
		return nil
	}

	// Stop is best-effort: physical deletion is itself a valid way to stop
	// the VM, so an ordinary stop failure must not strand a paid resource.
	// Only an ownership conflict aborts without attempting deletion, and a
	// stop acknowledgement alone never proves the guest stopped.
	if err := w.Backend.Stop(ctx, snap.Plan, combined); err != nil {
		if errors.Is(err, ErrConflict) {
			w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, true)
			return err
		}
	}
	obs, merged, isStopped, _, err := w.ciLifecycleObserve(ctx, snap, combined)
	if err != nil {
		w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, errors.Is(err, ErrConflict))
		return err
	}
	if err := w.Store.ciLifecycleSaveEvidence(ctx, workspace, runner, snap, merged, obs.CreateSettled, isStopped); err != nil {
		return err
	}
	combined = merged

	// Physical deletion. The adapter verifies operation ownership itself
	// and removes the server before ports and security groups.
	if err := w.Backend.Delete(ctx, snap.Plan, combined); err != nil {
		w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, true)
		return err
	}
	obs, merged, isStopped, _, err = w.ciLifecycleObserve(ctx, snap, combined)
	if err != nil {
		w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, errors.Is(err, ErrConflict))
		return err
	}
	if err := w.Store.ciLifecycleSaveEvidence(ctx, workspace, runner, snap, merged, obs.CreateSettled, isStopped); err != nil {
		return err
	}
	combined = merged

	// The enforcer may only settle once every retained resource reads 404,
	// discovery is complete and empty, and port lists are complete and
	// empty. Probe the validator with the enforcer clause forced so the
	// resource conditions are evaluated without requiring prior settlement.
	probe := obs
	probe.EnforcerSettled = true
	_, _, resourcesGone, err := ciResourceEvidence(snap.Plan.OperationID, combined, probe)
	if err != nil {
		w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, true)
		return err
	}
	if snap.ExecutionStartedAt != nil && !ciLifecycleHasServer(combined) {
		resourcesGone = false
	}
	if !resourcesGone {
		return nil
	}
	if err := w.Backend.SettleEnforcer(ctx, snap.Plan); err != nil {
		w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, true)
		return err
	}
	obs, merged, isStopped, isAbsent, err := w.ciLifecycleObserve(ctx, snap, combined)
	if err != nil {
		w.Store.ciLifecycleRetry(ctx, workspace, runner, snap, errors.Is(err, ErrConflict))
		return err
	}
	// The final stop flag is the fresh observation only; a historical
	// stopped_verified_at never substitutes for current proof.
	if err := w.Store.ciLifecycleSaveEvidence(ctx, workspace, runner, snap, merged, obs.CreateSettled, isStopped); err != nil {
		return err
	}
	combined = merged
	if !isAbsent {
		return nil
	}

	// Only a fully absent, enforcer-settled inventory may mark the runner
	// deleted, and only inside one transaction with the lifecycle proof.
	tx, err := w.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now = w.Store.now()
	tag, err := tx.Exec(ctx, `UPDATE ci_runner_lifecycles SET cleanup_verified_at=COALESCE(cleanup_verified_at,$4),next_attempt_at=$5 WHERE workspace_id=$1 AND runner_id=$2 AND operation_id=$3 AND stopped_verified_at IS NOT NULL`, workspace, runner, snap.Plan.OperationID, now, now.Add(30*time.Second))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE ci_runners SET phase='deleted',updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, runner, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}

	// Billing closes only after physical cleanup is proven, on a fresh
	// clock, so a GitHub or billing outage cannot delay deletion.
	return w.ciLifecycleSettle(ctx, workspace, runner, snap, completedAt, w.Store.now())
}
