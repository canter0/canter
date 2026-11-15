package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// admitRunnerJob drives one admission request through the trusted
// source-proof path: a read-only recovery attempt first, and only while
// GitHub has not yet assigned the job, a repo-scoped installation token, the
// source proof, and the atomic admit. The minted token is never persisted,
// logged, or sent to a guest; no HTTP endpoint or provider channel is
// created here.
func (c *ciGitHubClient) admitRunnerJob(ctx context.Context, s *Store, workspace, runner string, req ciAdmissionRequest, maximumSeconds int64) (ciAdmissionReceipt, error) {
	return c.admitRunnerJobBefore(ctx, s, workspace, runner, req, maximumSeconds, time.Time{})
}

// admitRunnerJobBefore also caps the quote at an externally enforced hard
// deadline. The bound uses the source-proved execution start, never guest time.
func (c *ciGitHubClient) admitRunnerJobBefore(ctx context.Context, s *Store, workspace, runner string, req ciAdmissionRequest, maximumSeconds int64, hardDeadline time.Time) (ciAdmissionReceipt, error) {
	receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, maximumSeconds)
	if err == nil {
		if !hardDeadline.IsZero() && receipt.ExecutionDeadlineUnixMs > hardDeadline.UnixMilli() {
			return ciAdmissionReceipt{}, ErrConflict
		}
		return receipt, nil
	}
	if !errors.Is(err, errCIRunnerJobPending) {
		return ciAdmissionReceipt{}, err
	}
	scope, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, req.AdmissionID)
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	var installationID int64
	err = s.pool.QueryRow(ctx, `SELECT i.github_installation_id FROM ci_runners r JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id WHERE r.workspace_id=$1 AND r.id=$2 AND i.status='active'`, workspace, runner).Scan(&installationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciAdmissionReceipt{}, ErrNotFound
	}
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	token, err := c.runnerSourceToken(ctx, installationID, scope.RepositoryID)
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	proof, err := c.proveRunnerJob(ctx, token, scope, ciRunnerJobHint{RunID: req.GitHubRunID, Attempt: req.RunAttempt}, s.now())
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	if !hardDeadline.IsZero() {
		limit := hardDeadline
		wallLimit := s.now().Add(time.Duration(ciMaxRuntimeSeconds) * time.Second)
		if wallLimit.Before(limit) {
			limit = wallLimit
		}
		remaining := int64(limit.Sub(proof.StartedAt) / time.Second)
		if remaining < 1 {
			return ciAdmissionReceipt{}, ErrConflict
		}
		if remaining < maximumSeconds {
			maximumSeconds = remaining
		}
	}
	return s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, maximumSeconds)
}
