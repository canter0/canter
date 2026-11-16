package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	ciLifecycleSetupWindow = 900 * time.Second
	ciLifecycleHardWindow  = 22500 * time.Second
)

// ciRunnerLifecyclePlan is the nonsecret pinned allocation recorded before a
// runner create is fenced. It never carries userdata, JIT configuration,
// provider credentials, or App credentials.
type ciRunnerLifecyclePlan struct {
	OperationID     string    `json:"operationId"`
	ServerName      string    `json:"serverName"`
	Region          string    `json:"region"`
	FlavorID        string    `json:"flavorId"`
	ImageID         string    `json:"imageId"`
	NetworkID       string    `json:"networkId"`
	EnforcerLease   string    `json:"enforcerLease"`
	SetupDeadlineAt time.Time `json:"setupDeadlineAt"`
	HardDeadlineAt  time.Time `json:"hardDeadlineAt"`
}

func (p ciRunnerLifecyclePlan) normalize() ciRunnerLifecyclePlan {
	p.SetupDeadlineAt = p.SetupDeadlineAt.UTC().Truncate(time.Microsecond)
	p.HardDeadlineAt = p.HardDeadlineAt.UTC().Truncate(time.Microsecond)
	return p
}

func (p ciRunnerLifecyclePlan) shapeValid() bool {
	p = p.normalize()
	return ciLifecycleID.MatchString(p.OperationID) &&
		ciSourceRunnerName.MatchString(p.ServerName) &&
		ciLifecycleID.MatchString(p.Region) &&
		ciLifecycleID.MatchString(p.FlavorID) &&
		ciLifecycleID.MatchString(p.ImageID) &&
		ciLifecycleID.MatchString(p.NetworkID) &&
		p.EnforcerLease != "" &&
		!p.SetupDeadlineAt.IsZero() && !p.HardDeadlineAt.IsZero() &&
		p.HardDeadlineAt.After(p.SetupDeadlineAt)
}

// newPlanValid additionally bounds the deadlines against the current clock;
// it applies only when no lifecycle row exists yet, so an exact retry of a
// recorded plan stays valid even after its deadlines have passed.
func (p ciRunnerLifecyclePlan) newPlanValid(now time.Time) bool {
	p = p.normalize()
	now = now.UTC()
	return p.shapeValid() &&
		p.SetupDeadlineAt.After(now) && !p.SetupDeadlineAt.After(now.Add(ciLifecycleSetupWindow)) &&
		!p.HardDeadlineAt.After(now.Add(ciLifecycleHardWindow))
}

func ciRunnerLifecyclePlanEqual(a, b ciRunnerLifecyclePlan) bool {
	return a.OperationID == b.OperationID && a.ServerName == b.ServerName &&
		a.Region == b.Region && a.FlavorID == b.FlavorID && a.ImageID == b.ImageID &&
		a.NetworkID == b.NetworkID && a.EnforcerLease == b.EnforcerLease &&
		a.SetupDeadlineAt.Equal(b.SetupDeadlineAt) && a.HardDeadlineAt.Equal(b.HardDeadlineAt)
}

// trackCIRunnerLifecycle records the pinned allocation for a runner that has
// not started executing. An exact retry of the recorded plan is a no-op; any
// different plan or operation conflicts.
func (s *Store) trackCIRunnerLifecycle(ctx context.Context, workspace, runner string, plan ciRunnerLifecyclePlan) error {
	plan = plan.normalize()
	if workspace == "" || runner == "" || !plan.shapeValid() {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return err
	}
	var phase string
	var started *time.Time
	err = tx.QueryRow(ctx, `SELECT phase,execution_started_at FROM ci_runners WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, runner).Scan(&phase, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return err
	}
	var operationID string
	var planRaw []byte
	var lease string
	err = tx.QueryRow(ctx, `SELECT operation_id,plan,enforcer_lease FROM ci_runner_lifecycles WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&operationID, &planRaw, &lease)
	if err == nil {
		var stored ciRunnerLifecyclePlan
		if json.Unmarshal(planRaw, &stored) != nil {
			return ErrConflict
		}
		stored = stored.normalize()
		if operationID == plan.OperationID && lease == plan.EnforcerLease && ciRunnerLifecyclePlanEqual(stored, plan) {
			return nil
		}
		return ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if started != nil {
		return ErrConflict
	}
	switch phase {
	case "queued", "provisioning", "registering", "idle":
	default:
		return ErrConflict
	}
	if !plan.newPlanValid(s.now()) {
		return ErrConflict
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ci_runner_lifecycles(workspace_id,runner_id,operation_id,plan,enforcer_lease,setup_deadline_at,hard_deadline_at,next_attempt_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, workspace, runner, plan.OperationID, planJSON, plan.EnforcerLease, plan.SetupDeadlineAt, plan.HardDeadlineAt, s.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// fenceCIRunnerCreate is the durable one-shot gate in front of a runner
// create. The first successful fence marks the attempt and moves the runner
// to provisioning; every later call reports false so a create is never
// retried. It performs no provider call itself.
func (s *Store) fenceCIRunnerCreate(ctx context.Context, workspace, runner string) (bool, error) {
	if workspace == "" || runner == "" {
		return false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return false, err
	}
	var phase string
	var deleteRequested bool
	var attempted, stopRequested *time.Time
	var setupDeadline, hardDeadline time.Time
	var lease string
	err = tx.QueryRow(ctx, `SELECT r.phase,r.delete_requested,l.create_attempted_at,l.stop_requested_at,l.setup_deadline_at,l.hard_deadline_at,l.enforcer_lease FROM ci_runners r JOIN ci_runner_lifecycles l ON l.workspace_id=r.workspace_id AND l.runner_id=r.id WHERE r.workspace_id=$1 AND r.id=$2 FOR UPDATE OF r,l`, workspace, runner).Scan(&phase, &deleteRequested, &attempted, &stopRequested, &setupDeadline, &hardDeadline, &lease)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if attempted != nil {
		return false, nil
	}
	now := s.now()
	if (phase != "queued" && phase != "provisioning") || deleteRequested ||
		stopRequested != nil || lease == "" ||
		!setupDeadline.After(now) || !hardDeadline.After(now) {
		return false, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runner_lifecycles SET create_attempted_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, now); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runners SET phase='provisioning',updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, runner, now); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ciRunnerLifecycleSnapshot is the reconciler's view of one lifecycle row
// joined with its runner clocks, admission decision, and installation.
type ciRunnerLifecycleSnapshot struct {
	Plan                 ciRunnerLifecyclePlan
	Resources            []ciOwnedResource
	CreateAttemptedAt    *time.Time
	CreateSettled        bool
	StopRequestedAt      *time.Time
	BillStopAt           *time.Time
	StoppedVerifiedAt    *time.Time
	CleanupVerifiedAt    *time.Time
	Phase                string
	ExecutionStartedAt   *time.Time
	ExecutionStoppedAt   *time.Time
	DeadlineAt           *time.Time
	PaidOverrunEnabled   bool
	Decision             []byte
	GitHubInstallationID *int64
}

type ciLifecycleDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (s *Store) loadCIRunnerLifecycle(ctx context.Context, workspace, runner string) (ciRunnerLifecycleSnapshot, error) {
	return s.loadCIRunnerLifecycleWith(ctx, s.pool, workspace, runner)
}

func (s *Store) loadCIRunnerLifecycleWith(ctx context.Context, db ciLifecycleDB, workspace, runner string) (ciRunnerLifecycleSnapshot, error) {
	var snap ciRunnerLifecycleSnapshot
	var planRaw, resourcesRaw, decisionRaw []byte
	var operationID, lease string
	var setupDeadline, hardDeadline time.Time
	err := db.QueryRow(ctx, `SELECT l.operation_id,l.plan,l.resources,l.create_attempted_at,l.create_settled,l.stop_requested_at,l.bill_stop_at,l.stopped_verified_at,l.cleanup_verified_at,l.setup_deadline_at,l.hard_deadline_at,l.enforcer_lease,r.phase,r.execution_started_at,r.execution_stopped_at,r.deadline_at,r.paid_overrun_enabled,a.decision,i.github_installation_id FROM ci_runner_lifecycles l JOIN ci_runners r ON r.workspace_id=l.workspace_id AND r.id=l.runner_id LEFT JOIN ci_runner_admissions a ON a.workspace_id=r.workspace_id AND a.runner_id=r.id LEFT JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id WHERE l.workspace_id=$1 AND l.runner_id=$2`, workspace, runner).Scan(&operationID, &planRaw, &resourcesRaw, &snap.CreateAttemptedAt, &snap.CreateSettled, &snap.StopRequestedAt, &snap.BillStopAt, &snap.StoppedVerifiedAt, &snap.CleanupVerifiedAt, &setupDeadline, &hardDeadline, &lease, &snap.Phase, &snap.ExecutionStartedAt, &snap.ExecutionStoppedAt, &snap.DeadlineAt, &snap.PaidOverrunEnabled, &decisionRaw, &snap.GitHubInstallationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return snap, ErrNotFound
	}
	if err != nil {
		return snap, err
	}
	if err = json.Unmarshal(planRaw, &snap.Plan); err != nil {
		return snap, ErrConflict
	}
	snap.Plan = snap.Plan.normalize()
	// The enforced columns are authoritative where they duplicate plan JSON.
	snap.Plan.OperationID = operationID
	snap.Plan.EnforcerLease = lease
	snap.Plan.SetupDeadlineAt = setupDeadline.UTC().Truncate(time.Microsecond)
	snap.Plan.HardDeadlineAt = hardDeadline.UTC().Truncate(time.Microsecond)
	if resourcesRaw != nil {
		if err = json.Unmarshal(resourcesRaw, &snap.Resources); err != nil {
			return snap, ErrConflict
		}
	}
	if snap.Resources == nil {
		snap.Resources = []ciOwnedResource{}
	}
	snap.Decision = decisionRaw
	return snap, nil
}
