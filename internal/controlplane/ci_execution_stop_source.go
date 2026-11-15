package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// completedRunnerJob obtains the stop timestamp from GitHub, not the guest,
// a DELETE acknowledgement, or a control-plane poll's observation time.
func (c *ciGitHubClient) completedRunnerJob(ctx context.Context, token string, expected ciRunnerJobProof, now time.Time) (time.Time, error) {
	if !ciSourceTokenUsable(token) || expected.RepositoryID <= 0 || expected.JobID <= 0 ||
		expected.RunID <= 0 || expected.Attempt <= 0 || expected.RunnerID <= 0 ||
		!ciSourceRunnerName.MatchString(expected.RunnerName) || expected.StartedAt.IsZero() ||
		now.IsZero() || expected.StartedAt.After(now.Add(time.Minute)) {
		return time.Time{}, ErrConflict
	}
	var repo ciSourceRepository
	if err := c.ciSourceJSON(ctx, http.MethodGet, fmt.Sprintf("/repositories/%d", expected.RepositoryID), token, nil, &repo); err != nil {
		return time.Time{}, err
	}
	owner, name, ok := ciCanonicalRepoName(repo.FullName)
	if !ok || repo.ID != expected.RepositoryID || repo.Private == nil {
		return time.Time{}, ErrConflict
	}
	var job ciSourceJob
	path := fmt.Sprintf("/repos/%s/%s/actions/jobs/%d", url.PathEscape(owner), url.PathEscape(name), expected.JobID)
	if err := c.ciSourceJSON(ctx, http.MethodGet, path, token, nil, &job); err != nil {
		return time.Time{}, err
	}
	if job.ID != expected.JobID || job.RunID != expected.RunID ||
		job.RunnerID == nil || *job.RunnerID != expected.RunnerID ||
		job.RunnerName != expected.RunnerName {
		return time.Time{}, ErrConflict
	}
	if len(job.RunAttempt) > 0 {
		var attempt int64
		if err := json.Unmarshal(job.RunAttempt, &attempt); err != nil || attempt != expected.Attempt {
			return time.Time{}, ErrConflict
		}
	}
	if job.StartedAt == nil || !job.StartedAt.UTC().Truncate(time.Microsecond).Equal(expected.StartedAt.UTC().Truncate(time.Microsecond)) {
		return time.Time{}, ErrConflict
	}
	switch job.Status {
	case "in_progress", "queued", "waiting", "requested", "pending":
		if job.CompletedAt != nil || job.Conclusion != nil {
			return time.Time{}, ErrConflict
		}
		return time.Time{}, errCIRunnerJobPending
	case "completed":
	default:
		return time.Time{}, ErrConflict
	}
	if job.CompletedAt == nil || job.CompletedAt.IsZero() || job.Conclusion == nil {
		return time.Time{}, ErrConflict
	}
	switch *job.Conclusion {
	case "success", "failure", "cancelled", "skipped", "timed_out", "neutral", "action_required", "startup_failure", "stale":
	default:
		return time.Time{}, ErrConflict
	}
	completed := job.CompletedAt.UTC().Truncate(time.Microsecond)
	if completed.Before(expected.StartedAt) || completed.After(now.Add(time.Minute)) {
		return time.Time{}, ErrConflict
	}
	return completed, nil
}
