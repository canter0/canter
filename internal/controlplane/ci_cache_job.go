package controlplane

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type ciCacheQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type ciCacheActiveJob struct {
	WorkspaceID          string
	InstallationID       string
	RunnerID             string
	JobID                string
	GitHubInstallationID int64
	Epoch                int64
	Proof                ciRunnerJobProof
	Scope                ciCacheIdentityScope
}

// The job ID is only a hint. Signed repository/owner/run claims and the actual
// admitted assignment select the server-side job, never a workspace from the
// request. The caller must still resolve its GitHub check-run ID and bind it.
func (s *Store) ciCacheActiveJob(ctx context.Context, q ciCacheQueryer, githubJobID int64, token ciCacheToken, lock bool) (ciCacheActiveJob, error) {
	if !token.verified || token.identity.bound || githubJobID <= 0 ||
		ctx == nil || ctx.Err() != nil {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	now := s.now()
	if !ciCacheClockOK(token.identity.IssuedAt, token.identity.ExpiresAt, token.notBefore, now) {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	query := `
SELECT r.workspace_id,r.installation_id,r.id,j.id,i.github_installation_id,
       j.github_repository_id,j.github_run_id,j.github_job_id,r.github_runner_id,
       a.runner_name,r.size_id,r.created_at,r.execution_started_at,
       r.deadline_at,l.hard_deadline_at,a.decision,COALESCE(st.cache_epoch,1)
FROM ci_jobs j
JOIN ci_runners r ON r.workspace_id=j.workspace_id
    AND r.assigned_job_id=j.id AND j.runner_id=r.id
    AND r.installation_id=j.installation_id
JOIN ci_runner_admissions a ON a.workspace_id=r.workspace_id AND a.runner_id=r.id
JOIN ci_runner_lifecycles l ON l.workspace_id=r.workspace_id AND l.runner_id=r.id
JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id
JOIN ci_repositories p ON p.workspace_id=j.workspace_id
    AND p.installation_id=j.installation_id AND p.github_repository_id=j.github_repository_id
LEFT JOIN ci_settings st ON st.workspace_id=r.workspace_id
WHERE j.github_job_id=$1 AND j.github_repository_id=$2 AND j.github_run_id=$3
  AND i.github_account_id=$4 AND i.status='active' AND p.enabled
  AND j.status='running' AND r.phase='running' AND NOT r.delete_requested
  AND r.execution_started_at IS NOT NULL AND r.execution_stopped_at IS NULL
  AND r.deadline_at>$5 AND l.hard_deadline_at>$5
  AND l.stop_requested_at IS NULL AND l.stopped_verified_at IS NULL
  AND a.github_repository_id=j.github_repository_id AND a.decision IS NOT NULL
  AND COALESCE(st.cache_enabled,true)
  AND EXISTS(SELECT 1 FROM workspace_products wp
      WHERE wp.workspace_id=r.workspace_id AND wp.product='ci')
LIMIT 2`
	if lock {
		// Mutations already hold the workspace lock. Lock the authoritative
		// assignment and lifecycle too, so cancellation cannot cross commit.
		query += ` FOR UPDATE OF r,j,a,l,i,p`
	}
	rows, err := q.Query(ctx, query,
		githubJobID, token.identity.RepositoryID, token.identity.RunID,
		token.identity.RepositoryOwnerID, now)
	if err != nil {
		return ciCacheActiveJob{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ciCacheActiveJob{}, err
		}
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	var job ciCacheActiveJob
	var repositoryID, runID, jobID, runnerID int64
	var runnerName, sizeID string
	var createdAt, startedAt, deadline, hardDeadline time.Time
	var decisionRaw []byte
	err = rows.Scan(&job.WorkspaceID, &job.InstallationID, &job.RunnerID, &job.JobID,
		&job.GitHubInstallationID, &repositoryID, &runID, &jobID, &runnerID,
		&runnerName, &sizeID, &createdAt, &startedAt, &deadline, &hardDeadline,
		&decisionRaw, &job.Epoch)
	if err != nil {
		return ciCacheActiveJob{}, err
	}
	if rows.Next() {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	if err := rows.Err(); err != nil {
		return ciCacheActiveJob{}, err
	}
	var decision ciAdmissionDecision
	if ciEnforcerStrictJSON(decisionRaw, &decision) != nil || !decision.Request.valid() ||
		!ciAdmissionProofMatches(decision.Request, ciRunnerJobScope{
			RepositoryID: repositoryID,
			RunnerID:     runnerID,
			RunnerName:   runnerName,
			SizeID:       sizeID,
			CreatedAt:    createdAt,
		}, decision.Proof, s.now()) ||
		decision.Proof.JobID != jobID || decision.Proof.RunID != runID ||
		decision.Proof.Attempt != token.identity.RunAttempt ||
		!decision.Proof.StartedAt.Equal(startedAt) ||
		decision.Receipt.Decision != "allow" ||
		decision.Receipt.ciAdmissionRequest != decision.Request ||
		decision.Receipt.ExecutionDeadlineUnixMs != deadline.UnixMilli() ||
		job.Epoch < 1 {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	if hardDeadline.Before(deadline) {
		deadline = hardDeadline
	}
	now = s.now()
	if ctx.Err() != nil || !deadline.After(now) ||
		!ciCacheClockOK(token.identity.IssuedAt, token.identity.ExpiresAt, token.notBefore, now) {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	job.Proof = decision.Proof
	job.Scope = ciCacheIdentityScope{
		RepositoryID:      repositoryID,
		RunID:             runID,
		RunAttempt:        decision.Proof.Attempt,
		Audience:          token.identity.Audience,
		ExecutionDeadline: deadline,
	}
	return job, nil
}
