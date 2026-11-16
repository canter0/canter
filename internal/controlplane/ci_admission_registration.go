package controlplane

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ciAdmissionID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ciAdmissionScopeValid enforces the registration scope contract before
// any database work and normalizes CreatedAt to UTC microseconds.
func ciAdmissionScopeValid(scope ciRunnerJobScope, now time.Time) (ciRunnerJobScope, error) {
	if scope.RepositoryID <= 0 || scope.RunnerID <= 0 {
		return ciRunnerJobScope{}, fmt.Errorf("%w: invalid admission scope identity", ErrConflict)
	}
	if !ciSourceRunnerName.MatchString(scope.RunnerName) {
		return ciRunnerJobScope{}, fmt.Errorf("%w: invalid runner name in admission scope", ErrConflict)
	}
	if _, ok := pricing.FindCI(scope.SizeID); !ok {
		return ciRunnerJobScope{}, fmt.Errorf("%w: unknown CI size in admission scope", ErrConflict)
	}
	if scope.CreatedAt.IsZero() || scope.CreatedAt.After(now.Add(time.Minute)) {
		return ciRunnerJobScope{}, fmt.Errorf("%w: invalid admission scope timestamp", ErrConflict)
	}
	scope.CreatedAt = scope.CreatedAt.UTC().Truncate(time.Microsecond)
	return scope, nil
}

func ciAdmissionUniqueConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

// registerCIRunnerAdmission durably binds a fresh admission nonce to the
// persisted runner identity so the existing source-proof verifier can later
// reload it. The nonce is a protocol binding, not authentication; no secrets
// or provider material is stored here.
func (s *Store) registerCIRunnerAdmission(ctx context.Context, workspace, runner, admissionID string, scope ciRunnerJobScope) error {
	if workspace == "" || runner == "" || !ciAdmissionID.MatchString(admissionID) {
		return fmt.Errorf("%w: invalid admission registration request", ErrConflict)
	}
	scope, err := ciAdmissionScopeValid(scope, s.now())
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var installationID string
	var githubRunnerID *int64
	var phase, sizeID string
	var createdAt time.Time
	var deleteRequested, repoEnabled bool
	var triggerRepoID int64
	var installationStatus string
	err = tx.QueryRow(ctx, `SELECT r.installation_id,r.github_runner_id,r.phase,r.size_id,r.created_at,r.delete_requested,j.github_repository_id,i.status,p.enabled FROM ci_runners r JOIN ci_jobs j ON j.workspace_id=r.workspace_id AND j.id=r.trigger_job_id JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id JOIN ci_repositories p ON p.workspace_id=r.workspace_id AND p.installation_id=r.installation_id AND p.github_repository_id=j.github_repository_id WHERE r.workspace_id=$1 AND r.id=$2 FOR UPDATE OF r,i,p`, workspace, runner).Scan(&installationID, &githubRunnerID, &phase, &sizeID, &createdAt, &deleteRequested, &triggerRepoID, &installationStatus, &repoEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	enabled, err := s.productEnabled(ctx, tx, workspace, "ci")
	if err != nil {
		return err
	}
	if !enabled || installationStatus != "active" || !repoEnabled || deleteRequested {
		return fmt.Errorf("%w: CI admission registration is not currently allowed", ErrConflict)
	}
	if sizeID != scope.SizeID {
		return fmt.Errorf("%w: runner size does not match admission scope", ErrConflict)
	}
	if !createdAt.UTC().Truncate(time.Microsecond).Equal(scope.CreatedAt) {
		return fmt.Errorf("%w: runner creation time does not match admission scope", ErrConflict)
	}
	if triggerRepoID != scope.RepositoryID {
		return fmt.Errorf("%w: trigger repository does not match admission scope", ErrConflict)
	}
	var existingNonce, existingName string
	var existingRepo int64
	err = tx.QueryRow(ctx, `SELECT admission_id,github_repository_id,runner_name FROM ci_runner_admissions WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&existingNonce, &existingRepo, &existingName)
	switch {
	case err == nil:
		if existingNonce == admissionID && existingRepo == scope.RepositoryID &&
			existingName == scope.RunnerName && githubRunnerID != nil &&
			*githubRunnerID > 0 && *githubRunnerID == scope.RunnerID &&
			(phase == "idle" || phase == "running") {
			return nil
		}
		return fmt.Errorf("%w: admission registration conflicts with the persisted identity", ErrConflict)
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return err
	}
	if phase != "registering" || (githubRunnerID != nil && *githubRunnerID != scope.RunnerID) {
		return fmt.Errorf("%w: runner is not in a registerable phase", ErrConflict)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ci_runner_admissions(workspace_id,runner_id,admission_id,github_repository_id,runner_name) VALUES($1,$2,$3,$4,$5)`, workspace, runner, admissionID, scope.RepositoryID, scope.RunnerName); err != nil {
		return ciAdmissionUniqueConflict(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runners SET github_runner_id=$3,phase='idle',updated_at=$4 WHERE workspace_id=$1 AND id=$2`, workspace, runner, scope.RunnerID, s.now()); err != nil {
		return ciAdmissionUniqueConflict(err)
	}
	return tx.Commit(ctx)
}

// ciRunnerAdmissionScope reloads only the persisted admission identity for
// the existing source-proof verifier. It performs no locking snapshot; any
// later admission must revalidate inside its own transaction.
func (s *Store) ciRunnerAdmissionScope(ctx context.Context, workspace, runner, admissionID string) (ciRunnerJobScope, error) {
	if workspace == "" || runner == "" || !ciAdmissionID.MatchString(admissionID) {
		return ciRunnerJobScope{}, fmt.Errorf("%w: invalid admission scope request", ErrConflict)
	}
	var scope ciRunnerJobScope
	var githubRunnerID int64
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `SELECT a.github_repository_id,r.github_runner_id,a.runner_name,r.size_id,r.created_at FROM ci_runner_admissions a JOIN ci_runners r ON r.workspace_id=a.workspace_id AND r.id=a.runner_id JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id JOIN ci_repositories p ON p.workspace_id=r.workspace_id AND p.installation_id=r.installation_id AND p.github_repository_id=a.github_repository_id WHERE a.workspace_id=$1 AND a.runner_id=$2 AND a.admission_id=$3 AND r.phase IN ('idle','running') AND NOT r.delete_requested AND r.github_runner_id IS NOT NULL AND i.status='active' AND p.enabled AND EXISTS(SELECT 1 FROM workspace_products wp WHERE wp.workspace_id=r.workspace_id AND wp.product='ci')`, workspace, runner, admissionID).Scan(&scope.RepositoryID, &githubRunnerID, &scope.RunnerName, &scope.SizeID, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciRunnerJobScope{}, ErrNotFound
	}
	if err != nil {
		return ciRunnerJobScope{}, err
	}
	scope.RunnerID = githubRunnerID
	scope.CreatedAt = createdAt
	return ciAdmissionScopeValid(scope, s.now())
}
