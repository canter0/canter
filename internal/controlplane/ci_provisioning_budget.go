package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ciProvisioningID     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	ciProvisioningDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// This is an operator-authorized infrastructure envelope, NOT customer CI
// billing, a provider invoice, or proof that money was transferred. One
// reservation consumes a conservative, explicitly authorized allocation
// ceiling. Releasing capacity never refunds that ceiling automatically:
// unknown provider outcomes cannot manufacture fresh spending authority.
// No public route, environment wiring, or automatic authorization exists.
type ciProvisioningBudgetPolicy struct {
	Currency          string
	ProfileDigest     string
	MaxConcurrent     int
	MaxPerWorkspace   int
	ReservationMicros int64
	AdmitBefore       time.Time
}

func (p ciProvisioningBudgetPolicy) normalized() ciProvisioningBudgetPolicy {
	p.AdmitBefore = p.AdmitBefore.UTC().Truncate(time.Microsecond)
	return p
}

func (p ciProvisioningBudgetPolicy) valid() bool {
	return (p.Currency == "USD" || p.Currency == "EUR" || p.Currency == "CAD") &&
		ciProvisioningDigest.MatchString(p.ProfileDigest) &&
		p.MaxConcurrent >= 1 && p.MaxConcurrent <= 1000 &&
		p.MaxPerWorkspace >= 1 && p.MaxPerWorkspace <= p.MaxConcurrent &&
		p.ReservationMicros > 0 && p.AdmitBefore.Year() >= 1970 && p.AdmitBefore.Year() <= 9999
}

type ciProvisioningBudgetState struct {
	Policy           ciProvisioningBudgetPolicy
	AuthorizedMicros int64
	CommittedMicros  int64
	Active           int
}

type ciProvisioningAuthorization struct {
	ID           string
	Policy       ciProvisioningBudgetPolicy
	AmountMicros int64
}

func ciProvisioningJSONDigest(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", ErrConflict
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ciProvisioningPolicyEqual(a, b ciProvisioningBudgetPolicy) bool {
	return a.Currency == b.Currency && a.ProfileDigest == b.ProfileDigest &&
		a.MaxConcurrent == b.MaxConcurrent && a.MaxPerWorkspace == b.MaxPerWorkspace &&
		a.ReservationMicros == b.ReservationMicros && a.AdmitBefore.Equal(b.AdmitBefore)
}

func loadCIProvisioningBudgetTx(ctx context.Context, tx pgx.Tx) (ciProvisioningBudgetState, error) {
	var state ciProvisioningBudgetState
	err := tx.QueryRow(ctx, `SELECT currency,profile_digest,max_concurrent,max_per_workspace,reservation_micros,admit_before,authorized_micros,committed_micros FROM ci_provisioning_budget WHERE singleton FOR UPDATE`).Scan(
		&state.Policy.Currency, &state.Policy.ProfileDigest, &state.Policy.MaxConcurrent,
		&state.Policy.MaxPerWorkspace, &state.Policy.ReservationMicros, &state.Policy.AdmitBefore,
		&state.AuthorizedMicros, &state.CommittedMicros)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, err
	}
	state.Policy = state.Policy.normalized()
	if !state.Policy.valid() || state.CommittedMicros < 0 || state.AuthorizedMicros < state.CommittedMicros {
		return state, ErrConflict
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`).Scan(&state.Active)
	return state, err
}

// authorizeCIProvisioningBudget is an operator-only programming seam. It is
// deliberately not called by a config loader, scheduler, or public handler.
// Authorization IDs are permanent idempotency keys: an old retry cannot
// replenish the pool or roll its policy back after a later authorization.
func (s *Store) authorizeCIProvisioningBudget(ctx context.Context, input ciProvisioningAuthorization) (ciProvisioningBudgetState, error) {
	input.Policy = input.Policy.normalized()
	if !ciProvisioningID.MatchString(input.ID) || !input.Policy.valid() || input.AmountMicros <= 0 {
		return ciProvisioningBudgetState{}, ErrConflict
	}
	digest, err := ciProvisioningJSONDigest(input)
	if err != nil {
		return ciProvisioningBudgetState{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciProvisioningBudgetState{}, err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	p := input.Policy
	_, err = tx.Exec(ctx, `INSERT INTO ci_provisioning_budget(singleton,currency,profile_digest,max_concurrent,max_per_workspace,reservation_micros,admit_before,updated_at) VALUES(true,$1,$2,$3,$4,$5,$6,$7) ON CONFLICT(singleton) DO NOTHING`,
		p.Currency, p.ProfileDigest, p.MaxConcurrent, p.MaxPerWorkspace, p.ReservationMicros, p.AdmitBefore, now)
	if err != nil {
		return ciProvisioningBudgetState{}, err
	}
	state, err := loadCIProvisioningBudgetTx(ctx, tx)
	if err != nil {
		return state, err
	}
	var oldDigest string
	err = tx.QueryRow(ctx, `SELECT request_digest FROM ci_provisioning_authorizations WHERE id=$1`, input.ID).Scan(&oldDigest)
	if err == nil {
		if oldDigest != digest {
			return state, ErrConflict
		}
		return state, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	if !p.AdmitBefore.After(now) || state.Policy.Currency != p.Currency ||
		(state.Active != 0 && !ciProvisioningPolicyEqual(state.Policy, p)) ||
		state.AuthorizedMicros > math.MaxInt64-input.AmountMicros {
		return state, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO ci_provisioning_authorizations(id,request_digest,amount_micros,created_at) VALUES($1,$2,$3,$4)`, input.ID, digest, input.AmountMicros, now)
	if err != nil {
		return state, err
	}
	_, err = tx.Exec(ctx, `UPDATE ci_provisioning_budget SET profile_digest=$1,max_concurrent=$2,max_per_workspace=$3,reservation_micros=$4,admit_before=$5,authorized_micros=authorized_micros+$6,updated_at=$7 WHERE singleton`,
		p.ProfileDigest, p.MaxConcurrent, p.MaxPerWorkspace, p.ReservationMicros, p.AdmitBefore, input.AmountMicros, now)
	if err != nil {
		return state, err
	}
	state.Policy, state.AuthorizedMicros = p, state.AuthorizedMicros+input.AmountMicros
	if err = tx.Commit(ctx); err != nil {
		return ciProvisioningBudgetState{}, err
	}
	return state, nil
}

type ciProvisioningReservation struct {
	OperationID, Workspace, Runner, PlanDigest, ProfileDigest, Currency string
	ReservedMicros                                                      int64
	ReservedAt                                                          time.Time
	ReleasedAt                                                          *time.Time
}

// Creation guards are separate from reservation identity: cleanup must still
// record a creator's outcome after cancellation, expiry, or product disabling.
func (s *Store) ciProvisioningReadyScopeTx(ctx context.Context, tx pgx.Tx, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan, state ciProvisioningBudgetState) (ciRunnerLifecycleSnapshot, error) {
	var empty ciRunnerLifecycleSnapshot
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return empty, err
	}
	enabled, err := s.productEnabled(ctx, tx, workspace, "ci")
	if err != nil {
		return empty, err
	}
	if !enabled {
		return empty, ErrConflict
	}
	var phase, jobStatus, installationStatus string
	var started *time.Time
	var deleting, repositoryEnabled bool
	err = tx.QueryRow(ctx, `SELECT r.phase,r.execution_started_at,r.delete_requested,j.status,i.status,p.enabled FROM ci_runners r JOIN ci_jobs j ON j.workspace_id=r.workspace_id AND j.id=r.trigger_job_id AND j.installation_id=r.installation_id JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id JOIN ci_repositories p ON p.workspace_id=j.workspace_id AND p.installation_id=j.installation_id AND p.github_repository_id=j.github_repository_id WHERE r.workspace_id=$1 AND r.id=$2 FOR UPDATE OF r,j,i,p`, workspace, runner).Scan(
		&phase, &started, &deleting, &jobStatus, &installationStatus, &repositoryEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	if started != nil || deleting || (phase != "queued" && phase != "provisioning" && phase != "registering" && phase != "idle") ||
		(jobStatus != "queued" && jobStatus != "provisioning") || installationStatus != "active" || !repositoryEnabled {
		return empty, ErrConflict
	}
	snap, err := s.loadCIRunnerLifecycleWith(ctx, tx, workspace, runner)
	if err != nil {
		return empty, err
	}
	now := s.now()
	if !ciRunnerLifecyclePlanEqual(snap.Plan, plan) || snap.StopRequestedAt != nil ||
		snap.CleanupVerifiedAt != nil || snap.ExecutionStoppedAt != nil ||
		!plan.SetupDeadlineAt.After(now) || !plan.HardDeadlineAt.After(now) ||
		plan.SetupDeadlineAt.After(state.Policy.AdmitBefore) ||
		!state.Policy.AdmitBefore.After(now) || state.Policy.ProfileDigest != profileDigest {
		return empty, ErrConflict
	}
	return snap, nil
}

func loadCIProvisioningReservationTx(ctx context.Context, tx pgx.Tx, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan, state ciProvisioningBudgetState) (ciProvisioningReservation, error) {
	planDigest, _ := (ciEnforcerSpec{Plan: plan}).digests()
	var previous ciProvisioningReservation
	var oldWorkspace, oldRunner *string
	err := tx.QueryRow(ctx, `SELECT operation_id,workspace_id,runner_id,plan_digest,profile_digest,currency,reserved_micros,reserved_at,released_at FROM ci_provisioning_reservations WHERE operation_id=$1`, plan.OperationID).Scan(
		&previous.OperationID, &oldWorkspace, &oldRunner, &previous.PlanDigest, &previous.ProfileDigest,
		&previous.Currency, &previous.ReservedMicros, &previous.ReservedAt, &previous.ReleasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciProvisioningReservation{}, ErrNotFound
	}
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	if oldWorkspace == nil || oldRunner == nil || *oldWorkspace != workspace || *oldRunner != runner ||
		previous.PlanDigest != planDigest || previous.ProfileDigest != profileDigest ||
		previous.Currency != state.Policy.Currency || previous.ReservedMicros != state.Policy.ReservationMicros ||
		previous.ReleasedAt != nil {
		return ciProvisioningReservation{}, ErrConflict
	}
	previous.Workspace, previous.Runner = workspace, runner
	return previous, nil
}

// reserveCIProvisioningBudget serializes the global budget, workspace quota,
// and durable operation identity in one transaction. There is no subscription
// requirement: the first free hour remains a per-job customer billing rule.
func (s *Store) reserveCIProvisioningBudget(ctx context.Context, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan) (ciProvisioningReservation, error) {
	plan = plan.normalize()
	if !ciAdmissionScopeIDValid(workspace) || !ciAdmissionScopeIDValid(runner) ||
		!ciProvisioningDigest.MatchString(profileDigest) || !plan.shapeValid() ||
		plan.ServerName != ciEnforcerRunnerName(plan.OperationID) || !ciLifecycleLeaseID.MatchString(plan.EnforcerLease) {
		return ciProvisioningReservation{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	defer tx.Rollback(ctx)
	state, err := loadCIProvisioningBudgetTx(ctx, tx)
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	res, err := s.reserveCIProvisioningBudgetTx(ctx, tx, workspace, runner, profileDigest, plan, state)
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ciProvisioningReservation{}, err
	}
	return res, nil
}

// The caller holds the singleton budget lock. Queue admission uses this
// same gate so a runner, its plan and its funding either all commit or none do.
func (s *Store) reserveCIProvisioningBudgetTx(ctx context.Context, tx pgx.Tx, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan, state ciProvisioningBudgetState) (ciProvisioningReservation, error) {
	var err error
	if _, err = s.ciProvisioningReadyScopeTx(ctx, tx, workspace, runner, profileDigest, plan, state); err != nil {
		return ciProvisioningReservation{}, err
	}
	previous, err := loadCIProvisioningReservationTx(ctx, tx, workspace, runner, profileDigest, plan, state)
	if err == nil {
		return previous, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return ciProvisioningReservation{}, err
	}
	var workspaceActive int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM ci_provisioning_reservations WHERE workspace_id=$1 AND released_at IS NULL`, workspace).Scan(&workspaceActive)
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	if state.Active >= state.Policy.MaxConcurrent || workspaceActive >= state.Policy.MaxPerWorkspace ||
		state.AuthorizedMicros-state.CommittedMicros < state.Policy.ReservationMicros {
		return ciProvisioningReservation{}, ErrCapacity
	}
	now := s.now()
	planDigest, _ := (ciEnforcerSpec{Plan: plan}).digests()
	res := ciProvisioningReservation{
		OperationID: plan.OperationID, Workspace: workspace, Runner: runner, PlanDigest: planDigest,
		ProfileDigest: profileDigest, Currency: state.Policy.Currency,
		ReservedMicros: state.Policy.ReservationMicros, ReservedAt: now,
	}
	_, err = tx.Exec(ctx, `INSERT INTO ci_provisioning_reservations(operation_id,workspace_id,runner_id,plan_digest,profile_digest,currency,reserved_micros,reserved_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		res.OperationID, workspace, runner, planDigest, profileDigest, res.Currency, res.ReservedMicros, now)
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE ci_provisioning_budget SET committed_micros=committed_micros+$1,updated_at=$2 WHERE singleton`, res.ReservedMicros, now)
	if err != nil {
		return ciProvisioningReservation{}, err
	}
	return res, nil
}

// releaseCIProvisioningCapacity releases only the concurrency slot, and only
// after the existing lifecycle worker has committed verified cleanup. The
// authorization commitment is retained until an explicit operator accounting
// adjustment exists; this method never estimates or refunds provider spend.
func (s *Store) releaseCIProvisioningCapacity(ctx context.Context, workspace, runner string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = loadCIProvisioningBudgetTx(ctx, tx); err != nil {
		return err
	}
	var released *time.Time
	var operationID, planDigest string
	err = tx.QueryRow(ctx, `SELECT operation_id,plan_digest,released_at FROM ci_provisioning_reservations WHERE workspace_id=$1 AND runner_id=$2 FOR UPDATE`, workspace, runner).Scan(&operationID, &planDigest, &released)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if released != nil {
		return nil
	}
	snap, err := s.loadCIRunnerLifecycleWith(ctx, tx, workspace, runner)
	if err != nil {
		return err
	}
	freshDigest, _ := (ciEnforcerSpec{Plan: snap.Plan}).digests()
	if snap.Plan.OperationID != operationID || freshDigest != planDigest ||
		snap.Phase != "deleted" || snap.CleanupVerifiedAt == nil || snap.StoppedVerifiedAt == nil || !snap.CreateSettled {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE ci_provisioning_reservations SET released_at=GREATEST(reserved_at,$3) WHERE workspace_id=$1 AND runner_id=$2 AND released_at IS NULL`, workspace, runner, s.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
