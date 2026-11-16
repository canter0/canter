package controlplane

import "context"

// RUNNER_NAME is supplied by GitHub in every self-hosted job; numeric GitHub
// job IDs are not. Treat the name only as a hint, then bind the signed check
// run to the actual assigned job. No extra workflow actions:read is needed.
func (s *Store) ciCacheJobForRunner(ctx context.Context, q ciCacheQueryer, runnerName string, token ciCacheToken) (ciCacheActiveJob, error) {
	if ctx == nil || ctx.Err() != nil || !token.verified || token.identity.bound ||
		!ciSourceRunnerName.MatchString(runnerName) ||
		!ciCacheClockOK(token.identity.IssuedAt, token.identity.ExpiresAt, token.notBefore, s.now()) {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	rows, err := q.Query(ctx, `
SELECT j.github_job_id
FROM ci_jobs j
JOIN ci_runners r ON r.workspace_id=j.workspace_id AND r.installation_id=j.installation_id
    AND r.assigned_job_id=j.id AND j.runner_id=r.id
JOIN ci_runner_admissions a ON a.workspace_id=r.workspace_id AND a.runner_id=r.id
JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id
WHERE a.runner_name=$1 AND j.github_repository_id=$2 AND j.github_run_id=$3
  AND i.github_account_id=$4 AND i.status='active'
  AND j.status='running' AND r.phase='running' AND NOT r.delete_requested
LIMIT 2`, runnerName, token.identity.RepositoryID, token.identity.RunID, token.identity.RepositoryOwnerID)
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
	var jobID int64
	if err := rows.Scan(&jobID); err != nil {
		return ciCacheActiveJob{}, err
	}
	if rows.Next() {
		return ciCacheActiveJob{}, errCICacheUnauthorized
	}
	if err := rows.Err(); err != nil {
		return ciCacheActiveJob{}, err
	}
	// Release a pooled connection before the full lookup, including MaxConns=1.
	rows.Close()
	return s.ciCacheActiveJob(ctx, q, jobID, token, false)
}
