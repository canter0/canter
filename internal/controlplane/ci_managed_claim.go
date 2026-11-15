package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/jackc/pgx/v5"
)

type ciManagedClaim struct {
	Workspace string
	Runner    string
	Spec      ciEnforcerSpec
}

// Demand is repository/installation/size scoped, not webhook-job ownership:
// GitHub may assign a matching runner a different job from its trigger.
const ciManagedCandidate = `
 FROM ci_jobs j
 JOIN ci_github_installations i ON i.workspace_id=j.workspace_id AND i.id=j.installation_id
 JOIN ci_repositories p ON p.workspace_id=j.workspace_id AND p.installation_id=j.installation_id AND p.github_repository_id=j.github_repository_id
 WHERE j.status='queued' AND i.status='active' AND p.enabled
 AND EXISTS(SELECT 1 FROM workspace_products wp WHERE wp.workspace_id=j.workspace_id AND wp.product='ci')
 AND j.document#>>'{job,size}'=ANY($1::text[])
 AND (SELECT count(*) FROM ci_provisioning_reservations f WHERE f.workspace_id=j.workspace_id AND f.released_at IS NULL)<$2
 AND NOT EXISTS(
   SELECT 1 FROM ci_runners r WHERE r.workspace_id=j.workspace_id AND r.trigger_job_id=j.id
   AND r.phase IN ('queued','provisioning','registering','idle')
   AND r.assigned_job_id IS NULL AND r.execution_started_at IS NULL AND NOT r.delete_requested)
 AND (SELECT count(*) FROM ci_jobs q
      WHERE q.workspace_id=j.workspace_id AND q.installation_id=j.installation_id
      AND q.github_repository_id=j.github_repository_id AND q.status='queued'
      AND q.document#>>'{job,size}'=j.document#>>'{job,size}') >
     (SELECT count(*) FROM ci_runners r
      JOIN ci_jobs tr ON tr.workspace_id=r.workspace_id AND tr.id=r.trigger_job_id AND tr.installation_id=r.installation_id
      WHERE r.workspace_id=j.workspace_id AND r.installation_id=j.installation_id
      AND tr.github_repository_id=j.github_repository_id AND r.size_id=j.document#>>'{job,size}'
      AND r.phase IN ('queued','provisioning','registering','idle')
      AND r.assigned_job_id IS NULL AND r.execution_started_at IS NULL AND NOT r.delete_requested)
`

// claimCIManagedRunner performs no network I/O, authorizes no spending, and
// starts no execution clock. The explicit infrastructure envelope must exist.
func (s *Store) claimCIManagedRunner(ctx context.Context, profile ciManagedRunnerProfile) (ciManagedClaim, error) {
	var empty ciManagedClaim
	profileDigest, err := profile.digest()
	if err != nil {
		return empty, err
	}
	bundle, err := newCIGuestBundle()
	if err != nil || bundle.SHA256 != profile.GuestBundleSHA256 {
		return empty, ErrConflict
	}
	sizes := make([]string, 0, len(profile.Machines))
	for _, machine := range profile.Machines {
		sizes = append(sizes, machine.SizeID)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	state, err := loadCIProvisioningBudgetTx(ctx, tx)
	if err != nil {
		return empty, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if state.Policy.ProfileDigest != profileDigest ||
		!state.Policy.AdmitBefore.After(now) ||
		now.Add(ciLifecycleSetupWindow).After(state.Policy.AdmitBefore) {
		return empty, ErrConflict
	}
	if state.Active >= state.Policy.MaxConcurrent ||
		state.AuthorizedMicros-state.CommittedMicros < state.Policy.ReservationMicros {
		return empty, ErrCapacity
	}
	var workspace, job string
	err = tx.QueryRow(ctx, `SELECT j.workspace_id,j.id`+ciManagedCandidate+` ORDER BY j.created_at,j.id LIMIT 1`,
		sizes, state.Policy.MaxPerWorkspace).Scan(&workspace, &job)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errCIRunnerJobPending
	}
	if err != nil {
		return empty, err
	}
	// Read the candidate without locking it, then take the workspace lock
	// first, matching webhook/settings/admission lock order. Recheck every
	// eligibility predicate under that lock before creating any state.
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		return empty, err
	}
	var installation string
	var installationID, repositoryID int64
	var document []byte
	err = tx.QueryRow(ctx, `SELECT j.installation_id,i.github_installation_id,j.github_repository_id,j.document`+
		ciManagedCandidate+` AND j.workspace_id=$3 AND j.id=$4 FOR UPDATE OF j,i,p`,
		sizes, state.Policy.MaxPerWorkspace, workspace, job).Scan(&installation, &installationID, &repositoryID, &document)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errCIRunnerJobPending
	}
	if err != nil {
		return empty, err
	}
	var doc ciJobDocument
	if json.Unmarshal(document, &doc) != nil {
		return empty, ErrConflict
	}
	size, ok := pricing.FindCI(doc.Job.Size)
	if !ok || size.ID != doc.Job.Size {
		return empty, ErrConflict
	}
	runner, err := newID("cir_")
	if err != nil {
		return empty, err
	}
	operation, err := newID("ciop_")
	if err != nil {
		return empty, err
	}
	lease, err := ciEnforcerNewLeaseID()
	if err != nil {
		return empty, err
	}
	nonce, err := ciEnforcerNewLeaseID()
	if err != nil {
		return empty, err
	}
	plan, err := profile.planFor(size.ID, operation, lease, now)
	if err != nil {
		return empty, err
	}
	spec := ciEnforcerSpec{Plan: plan, GitHubInstallationID: installationID, GitHubRepositoryID: repositoryID, SizeID: size.ID}
	if _, err = profile.machineFor(spec); err != nil {
		return empty, err
	}
	var paid bool
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT paid_overrun_enabled FROM ci_settings WHERE workspace_id=$1),false)`, workspace).Scan(&paid); err != nil {
		return empty, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,phase,document,rate_version,rate_micros_per_minute,paid_overrun_enabled,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'queued','{}'::jsonb,$6,$7,$8,$9,$9)`,
		runner, workspace, installation, size.ID, job, pricing.CIRateVersion, size.MicrosPerMinute, paid, now)
	if err != nil {
		return empty, err
	}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		return empty, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO ci_runner_lifecycles(workspace_id,runner_id,operation_id,plan,enforcer_lease,setup_deadline_at,hard_deadline_at,next_attempt_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		workspace, runner, operation, planRaw, lease, plan.SetupDeadlineAt, plan.HardDeadlineAt, now)
	if err != nil {
		return empty, err
	}
	if _, err = s.reserveCIProvisioningBudgetTx(ctx, tx, workspace, runner, profileDigest, plan, state); err != nil {
		return empty, err
	}
	maximum := pricing.CIFreeSeconds
	if paid {
		maximum = ciMaxRuntimeSeconds
	}
	_, err = tx.Exec(ctx, `INSERT INTO ci_runner_bootstrap_grants(operation_id,bootstrap_id,maximum_seconds,expires_at,created_at) VALUES($1,$2,$3,$4,$5)`,
		operation, nonce, maximum, time.Unix(plan.SetupDeadlineAt.Unix(), 0), now)
	if err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return ciManagedClaim{Workspace: workspace, Runner: runner, Spec: spec}, nil
}
