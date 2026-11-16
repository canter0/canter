package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ciBootstrapClaims struct {
	Version            int    `json:"version"`
	Workspace          string `json:"workspace"`
	Runner             string `json:"runner"`
	OperationID        string `json:"operationId"`
	BootstrapID        string `json:"bootstrapId"`
	GitHubRepositoryID int64  `json:"githubRepositoryId"`
	PlanDigest         string `json:"planDigest"`
	MaximumSeconds     int64  `json:"maximumSeconds"`
	ExpiresAt          int64  `json:"expiresAt"`
}

type ciBootstrapGrant struct {
	BootstrapID    string
	MaximumSeconds int64
	ExpiresAt      time.Time
	AttemptedAt    *time.Time
	DeliveredAt    *time.Time
}

type ciBootstrapScope struct {
	Grant     ciBootstrapGrant
	Spec      ciEnforcerSpec
	CreatedAt time.Time
	Snapshot  ciRunnerLifecycleSnapshot
}

func (scope ciBootstrapScope) claims(workspace, runner string) ciBootstrapClaims {
	pd, _ := scope.Spec.digests()
	return ciBootstrapClaims{
		Version: 1, Workspace: workspace, Runner: runner,
		OperationID: scope.Spec.Plan.OperationID, BootstrapID: scope.Grant.BootstrapID,
		GitHubRepositoryID: scope.Spec.GitHubRepositoryID, PlanDigest: pd,
		MaximumSeconds: scope.Grant.MaximumSeconds, ExpiresAt: scope.Grant.ExpiresAt.Unix(),
	}
}

func (s *Store) ciBootstrapBaseTx(ctx context.Context, tx pgx.Tx, workspace, runner string, snap ciRunnerLifecycleSnapshot) (ciBootstrapScope, error) {
	out := ciBootstrapScope{Snapshot: snap, Spec: ciEnforcerSpec{Plan: snap.Plan}}
	if snap.GitHubInstallationID == nil || *snap.GitHubInstallationID <= 0 {
		return out, ErrConflict
	}
	out.Spec.GitHubInstallationID = *snap.GitHubInstallationID
	err := tx.QueryRow(ctx, `SELECT r.size_id,r.created_at,j.github_repository_id FROM ci_runners r JOIN ci_jobs j ON j.workspace_id=r.workspace_id AND j.id=r.trigger_job_id AND j.installation_id=r.installation_id WHERE r.workspace_id=$1 AND r.id=$2`, workspace, runner).Scan(
		&out.Spec.SizeID, &out.CreatedAt, &out.Spec.GitHubRepositoryID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt = out.CreatedAt.UTC().Truncate(time.Microsecond)
	if !out.Spec.valid() {
		return out, ErrConflict
	}
	return out, nil
}

func loadCIBootstrapGrantTx(ctx context.Context, tx pgx.Tx, operation string) (ciBootstrapGrant, error) {
	var g ciBootstrapGrant
	err := tx.QueryRow(ctx, `SELECT bootstrap_id,maximum_seconds,expires_at,attempted_at,delivered_at FROM ci_runner_bootstrap_grants WHERE operation_id=$1`, operation).Scan(
		&g.BootstrapID, &g.MaximumSeconds, &g.ExpiresAt, &g.AttemptedAt, &g.DeliveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return g, err
}

func ciBootstrapGrantMatches(scope ciBootstrapScope) bool {
	maximum := int64(3600)
	if scope.Snapshot.PaidOverrunEnabled {
		maximum = ciMaxRuntimeSeconds
	}
	g := scope.Grant
	return ciAdmissionID.MatchString(g.BootstrapID) && g.MaximumSeconds == maximum &&
		g.ExpiresAt.Equal(time.Unix(scope.Spec.Plan.SetupDeadlineAt.Unix(), 0)) &&
		(g.DeliveredAt == nil || g.AttemptedAt != nil)
}

// The stored nonce is not a credential. A retry recovers the same claims and
// signs the same token; it never replaces the nonce or extends its expiry.
func (s *Store) ensureCIBootstrapGrant(ctx context.Context, workspace, runner, profile string, plan ciRunnerLifecyclePlan, proposedNonce string) (ciBootstrapScope, error) {
	var out ciBootstrapScope
	if !ciAdmissionID.MatchString(proposedNonce) {
		return out, ErrConflict
	}
	err := s.withCIProvisioningCreationTx(ctx, workspace, runner, profile, plan, true, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		var err error
		out, err = s.ciBootstrapBaseTx(ctx, tx, workspace, runner, snap)
		if err != nil {
			return err
		}
		out.Grant, err = loadCIBootstrapGrantTx(ctx, tx, plan.OperationID)
		if errors.Is(err, ErrNotFound) {
			maximum := int64(3600)
			if snap.PaidOverrunEnabled {
				maximum = ciMaxRuntimeSeconds
			}
			expiry := time.Unix(plan.SetupDeadlineAt.Unix(), 0)
			if !expiry.After(s.now()) {
				return ErrConflict
			}
			_, err = tx.Exec(ctx, `INSERT INTO ci_runner_bootstrap_grants(operation_id,bootstrap_id,maximum_seconds,expires_at,created_at) VALUES($1,$2,$3,$4,$5)`, plan.OperationID, proposedNonce, maximum, expiry, s.now())
			if err != nil {
				return ciAdmissionUniqueConflict(err)
			}
			out.Grant = ciBootstrapGrant{BootstrapID: proposedNonce, MaximumSeconds: maximum, ExpiresAt: expiry}
		} else if err != nil {
			return err
		}
		if !ciBootstrapGrantMatches(out) {
			return ErrConflict
		}
		return nil
	})
	return out, err
}

func (s *Store) ciBootstrapScope(ctx context.Context, workspace, runner, profile string, plan ciRunnerLifecyclePlan, ready bool) (ciBootstrapScope, error) {
	var out ciBootstrapScope
	err := s.withCIProvisioningCreationTx(ctx, workspace, runner, profile, plan, ready, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		var err error
		out, err = s.ciBootstrapBaseTx(ctx, tx, workspace, runner, snap)
		if err != nil {
			return err
		}
		out.Grant, err = loadCIBootstrapGrantTx(ctx, tx, plan.OperationID)
		if err == nil && !ciBootstrapGrantMatches(out) {
			err = ErrConflict
		}
		return err
	})
	return out, err
}

func (s *Store) claimCIBootstrap(ctx context.Context, profile string, plan ciRunnerLifecyclePlan, claims ciBootstrapClaims) (ciBootstrapScope, error) {
	var out ciBootstrapScope
	err := s.withCIProvisioningCreationTx(ctx, claims.Workspace, claims.Runner, profile, plan, true, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		var err error
		out, err = s.ciBootstrapBaseTx(ctx, tx, claims.Workspace, claims.Runner, snap)
		if err != nil {
			return err
		}
		out.Grant, err = loadCIBootstrapGrantTx(ctx, tx, plan.OperationID)
		if err != nil {
			return err
		}
		if !ciBootstrapGrantMatches(out) || out.claims(claims.Workspace, claims.Runner) != claims ||
			!out.Grant.ExpiresAt.After(s.now()) || out.Grant.AttemptedAt != nil {
			return ErrConflict
		}
		if !snap.CreateSettled || snap.Phase != "registering" || snap.CreateAttemptedAt == nil {
			return errCIRunnerJobPending
		}
		j, err := loadCIProvisioningCreationTx(ctx, tx, plan.OperationID)
		if err != nil {
			return err
		}
		if j.Stage != "server" || j.Outcome == nil || !j.Outcome.CreationSettled ||
			!j.Outcome.MutationAttempted || len(j.Outcome.Resources) != 3 {
			return errCIRunnerJobPending
		}
		now := s.now()
		tag, err := tx.Exec(ctx, `UPDATE ci_runner_bootstrap_grants SET attempted_at=$2 WHERE operation_id=$1 AND attempted_at IS NULL`, plan.OperationID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		out.Grant.AttemptedAt = &now
		return nil
	})
	return out, err
}

// Registration already committed separately. Recheck scope, stop, and exact
// runner identity before committing the delivery marker; never store the JIT
// material or capability itself.
func (s *Store) markCIBootstrapDelivered(ctx context.Context, profile string, plan ciRunnerLifecyclePlan, claims ciBootstrapClaims, githubRunnerID int64) error {
	return s.withCIProvisioningCreationTx(ctx, claims.Workspace, claims.Runner, profile, plan, true, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		scope, err := s.ciBootstrapBaseTx(ctx, tx, claims.Workspace, claims.Runner, snap)
		if err != nil {
			return err
		}
		scope.Grant, err = loadCIBootstrapGrantTx(ctx, tx, plan.OperationID)
		if err != nil {
			return err
		}
		if !ciBootstrapGrantMatches(scope) || scope.claims(claims.Workspace, claims.Runner) != claims ||
			!scope.Grant.ExpiresAt.After(s.now()) || scope.Grant.AttemptedAt == nil ||
			!snap.CreateSettled || snap.Phase != "idle" || githubRunnerID <= 0 {
			return ErrConflict
		}
		var admissionID, runnerName string
		var repositoryID, runnerID int64
		err = tx.QueryRow(ctx, `SELECT a.admission_id,a.github_repository_id,a.runner_name,r.github_runner_id FROM ci_runner_admissions a JOIN ci_runners r ON r.workspace_id=a.workspace_id AND r.id=a.runner_id WHERE a.workspace_id=$1 AND a.runner_id=$2`, claims.Workspace, claims.Runner).Scan(&admissionID, &repositoryID, &runnerName, &runnerID)
		if err != nil {
			return err
		}
		if admissionID != claims.BootstrapID || repositoryID != claims.GitHubRepositoryID ||
			runnerName != plan.ServerName || runnerID != githubRunnerID {
			return ErrConflict
		}
		tag, err := tx.Exec(ctx, `UPDATE ci_runner_bootstrap_grants SET delivered_at=COALESCE(delivered_at,$2) WHERE operation_id=$1`, plan.OperationID, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}

// Only the authenticated enforcer's exact live creation acknowledgement can
// make the guest registerable. An SDK return is not this acknowledgement.
func (s *Store) markCIProvisioningRegistering(ctx context.Context, workspace, runner, profile string, plan ciRunnerLifecyclePlan, status ciEnforcerStatus) error {
	return s.withCIProvisioningCreationTx(ctx, workspace, runner, profile, plan, true, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		if snap.CreateAttemptedAt == nil || (snap.Phase != "provisioning" && snap.Phase != "registering") {
			return ErrConflict
		}
		scope, err := s.ciBootstrapBaseTx(ctx, tx, workspace, runner, snap)
		if err != nil {
			return err
		}
		j, err := loadCIProvisioningCreationTx(ctx, tx, plan.OperationID)
		if err != nil {
			return err
		}
		if j.Stage != "server" || j.Outcome == nil || !j.Outcome.MutationAttempted ||
			!j.Outcome.CreationSettled || len(j.Outcome.Resources) != 3 {
			return ErrConflict
		}
		pd, sd := scope.Spec.digests()
		if status.LeaseID != plan.EnforcerLease || status.OperationID != plan.OperationID ||
			status.PlanDigest != pd || status.SpecDigest != sd ||
			!status.CreateSettled || status.StopRequested || status.Settled ||
			status.DeadlineUnixMs != plan.SetupDeadlineAt.UnixMilli() ||
			status.GitHubRepositoryID != scope.Spec.GitHubRepositoryID ||
			status.GitHubRunnerID != 0 || status.RunnerName != plan.ServerName ||
			status.RegistrationAttempted || status.RegistrationSettled || status.Resources == nil {
			return ErrConflict
		}
		expected := make(map[string]string, 3)
		for _, r := range j.Outcome.Resources {
			expected[r.Kind] = r.ID
		}
		serverKnown := false
		for _, r := range status.Resources {
			if !ciResourceValid(r, plan.OperationID) || expected[r.Kind] != r.ID {
				return ErrConflict
			}
			serverKnown = serverKnown || r.Kind == "server"
		}
		if !serverKnown {
			return ErrConflict
		}
		combined, err := ciProvisioningMergeResources(plan.OperationID, snap.Resources, status.Resources)
		if err != nil {
			return err
		}
		if err = s.ciLifecycleSaveEvidenceWith(ctx, tx, workspace, runner, snap, combined, true, false); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE ci_runners SET phase='registering',updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, runner, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		return nil
	})
}
