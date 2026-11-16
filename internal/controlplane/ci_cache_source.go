package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A signature-valid token from another job in the same run is insufficient.
// Resolve the check-run ID from GitHub's actual admitted job, not a guest URL.
func (c *ciGitHubClient) ciCacheCheckRun(ctx context.Context, job ciCacheActiveJob, token ciCacheToken, now func() time.Time) (int64, error) {
	if ctx == nil || ctx.Err() != nil || !token.verified || token.identity.bound ||
		now == nil ||
		job.GitHubInstallationID <= 0 || job.Proof.JobID <= 0 ||
		job.Proof.RunnerID <= 0 || job.Proof.RunnerName == "" ||
		job.Proof.RepositoryID != token.identity.RepositoryID ||
		job.Proof.RunID != token.identity.RunID ||
		job.Proof.Attempt != token.identity.RunAttempt {
		return 0, errCICacheUnauthorized
	}
	live := func() bool {
		at := now()
		return ctx.Err() == nil &&
			ciCacheClockOK(token.identity.IssuedAt, token.identity.ExpiresAt, token.notBefore, at) &&
			job.Scope.ExecutionDeadline.After(at)
	}
	if !live() {
		return 0, errCICacheUnauthorized
	}
	bearer, err := c.runnerSourceToken(ctx, job.GitHubInstallationID, job.Proof.RepositoryID)
	if err != nil || !live() {
		return 0, errCICacheUnauthorized
	}
	var repo ciSourceRepository
	if err = c.ciSourceJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repositories/%d", job.Proof.RepositoryID), bearer, nil, &repo); err != nil ||
		repo.ID != job.Proof.RepositoryID || repo.Private == nil {
		return 0, errCICacheUnauthorized
	}
	owner, name, ok := ciCanonicalRepoName(repo.FullName)
	if !ok || !live() {
		return 0, errCICacheUnauthorized
	}
	var source struct {
		ciSourceJob
		CheckRunURL string `json:"check_run_url"`
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d",
		url.PathEscape(owner), url.PathEscape(name), job.Proof.JobID)
	if err = c.ciSourceJSON(ctx, http.MethodGet, path, bearer, nil, &source); err != nil {
		return 0, errCICacheUnauthorized
	}
	if source.ID != job.Proof.JobID || source.RunID != job.Proof.RunID ||
		source.RunnerID == nil || *source.RunnerID != job.Proof.RunnerID ||
		source.RunnerName != job.Proof.RunnerName || source.Status != "in_progress" ||
		source.StartedAt == nil || !source.StartedAt.Equal(job.Proof.StartedAt) ||
		source.CompletedAt != nil || source.Conclusion != nil {
		return 0, errCICacheUnauthorized
	}
	// Some GitHub job responses omit run_attempt. The unique admitted job ID
	// and source proof still bind its attempt; any supplied value must agree.
	if len(source.RunAttempt) != 0 {
		var attempt int64
		if json.Unmarshal(source.RunAttempt, &attempt) != nil || attempt != job.Proof.Attempt {
			return 0, errCICacheUnauthorized
		}
	}
	prefix := c.base + "/repos/" + url.PathEscape(owner) + "/" +
		url.PathEscape(name) + "/check-runs/"
	if !strings.HasPrefix(source.CheckRunURL, prefix) {
		return 0, errCICacheUnauthorized
	}
	rawID := strings.TrimPrefix(source.CheckRunURL, prefix)
	checkRunID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || checkRunID <= 0 || strconv.FormatInt(checkRunID, 10) != rawID ||
		checkRunID != token.identity.CheckRunID || !live() {
		return 0, errCICacheUnauthorized
	}
	// Only a number is returned. Never follow a URL obtained from a job or
	// send the repository-scoped installation token to a redirect target.
	return checkRunID, nil
}
