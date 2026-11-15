package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/canter0/canter/pricing"
)

var errCIRunnerJobPending = errors.New("CI runner assignment is not yet visible in GitHub.")

var (
	ciSourceRunnerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	ciSourceRepoName   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`)
)

const (
	ciSourceJobsPerPage = 100
	ciSourceMaxJobs     = 10000
)

type ciRunnerJobScope struct {
	RepositoryID int64
	RunnerID     int64
	RunnerName   string
	SizeID       string
	CreatedAt    time.Time
}

type ciRunnerJobHint struct {
	RunID   int64
	Attempt int64
}

type ciRunnerJobProof struct {
	RepositoryID int64
	Repository   string
	JobID        int64
	RunID        int64
	Attempt      int64
	RunnerID     int64
	RunnerName   string
	StartedAt    time.Time
}

type ciSourceRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  *bool  `json:"private"`
}

type ciSourceJob struct {
	ID          int64           `json:"id"`
	RunID       int64           `json:"run_id"`
	RunAttempt  json.RawMessage `json:"run_attempt"`
	RunnerID    *int64          `json:"runner_id"`
	RunnerName  string          `json:"runner_name"`
	Status      string          `json:"status"`
	StartedAt   *time.Time      `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at"`
	Conclusion  *string         `json:"conclusion"`
	Labels      []string        `json:"labels"`
}

type ciSourceJobsPage struct {
	TotalCount *int64        `json:"total_count"`
	Jobs       []ciSourceJob `json:"jobs"`
}

func ciSourceTokenUsable(token string) bool {
	if token == "" || token == "app" {
		return false
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (c *ciGitHubClient) runnerSourceToken(ctx context.Context, installationID, repositoryID int64) (string, error) {
	if installationID <= 0 || repositoryID <= 0 {
		return "", fmt.Errorf("%w: invalid installation or repository", ErrConflict)
	}
	body := struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}{
		RepositoryIDs: []int64{repositoryID},
		Permissions:   map[string]string{"actions": "read", "metadata": "read"},
	}
	var out ciInstallationToken
	if err := c.ciSourceJSON(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", installationID),
		"app", body, &out); err != nil {
		return "", err
	}
	if !ciSourceTokenUsable(out.Token) {
		return "", fmt.Errorf("%w: GitHub returned an unusable installation token", ErrConflict)
	}
	return out.Token, nil
}

func (c *ciGitHubClient) ciSourceJSON(ctx context.Context, method, path, bearer string, body, out any) error {
	var raw json.RawMessage
	if err := c.call(ctx, method, path, bearer, body, &raw); err != nil {
		var syntax *json.SyntaxError
		var unmarshal *json.UnmarshalTypeError
		if errors.As(err, &syntax) || errors.As(err, &unmarshal) {
			return fmt.Errorf("%w: malformed GitHub response", ErrConflict)
		}
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("%w: empty GitHub response", ErrConflict)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: malformed GitHub response", ErrConflict)
	}
	return nil
}

func ciCanonicalRepoName(fullName string) (owner, name string, ok bool) {
	if !ciSourceRepoName.MatchString(fullName) {
		return "", "", false
	}
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 || parts[1] == "." || parts[1] == ".." {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func ciSourceSizeLabels(size pricing.CISize) (map[string]bool, bool) {
	switch size.ID {
	case "linux-x64-2":
		return map[string]bool{"canter-2vcpu-ubuntu-2404": true}, true
	case "linux-x64-4":
		return map[string]bool{"canter-4vcpu-ubuntu-2404": true, "canter": true}, true
	default:
		return nil, false
	}
}

func (c *ciGitHubClient) proveRunnerJob(ctx context.Context, token string, scope ciRunnerJobScope, hint ciRunnerJobHint, now time.Time) (ciRunnerJobProof, error) {
	if scope.RepositoryID <= 0 || scope.RunnerID <= 0 || hint.RunID <= 0 || hint.Attempt <= 0 {
		return ciRunnerJobProof{}, fmt.Errorf("%w: invalid runner job scope or hint", ErrConflict)
	}
	if !ciSourceRunnerName.MatchString(scope.RunnerName) {
		return ciRunnerJobProof{}, fmt.Errorf("%w: invalid runner name in scope", ErrConflict)
	}
	size, ok := pricing.FindCI(scope.SizeID)
	if !ok {
		return ciRunnerJobProof{}, fmt.Errorf("%w: unknown CI size in scope", ErrConflict)
	}
	if now.IsZero() || scope.CreatedAt.IsZero() || scope.CreatedAt.After(now.Add(time.Minute)) {
		return ciRunnerJobProof{}, fmt.Errorf("%w: invalid runner job timestamps", ErrConflict)
	}
	if !ciSourceTokenUsable(token) {
		return ciRunnerJobProof{}, fmt.Errorf("%w: unusable runner source token", ErrConflict)
	}

	var repo ciSourceRepository
	if err := c.ciSourceJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repositories/%d", scope.RepositoryID), token, nil, &repo); err != nil {
		return ciRunnerJobProof{}, err
	}
	if repo.ID != scope.RepositoryID || repo.Private == nil {
		return ciRunnerJobProof{}, fmt.Errorf("%w: repository identity or privacy is not provable", ErrConflict)
	}
	owner, name, ok := ciCanonicalRepoName(repo.FullName)
	if !ok {
		return ciRunnerJobProof{}, fmt.Errorf("%w: repository canonical name is invalid", ErrConflict)
	}

	var matches []ciSourceJob
	seen := make(map[int64]bool)
	baseline := int64(-1)
	pages := 1
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d/jobs?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(name), hint.RunID, hint.Attempt,
			ciSourceJobsPerPage, page)
		var pg ciSourceJobsPage
		if err := c.ciSourceJSON(ctx, http.MethodGet, path, token, nil, &pg); err != nil {
			return ciRunnerJobProof{}, err
		}
		if pg.TotalCount == nil || *pg.TotalCount < 0 || *pg.TotalCount > ciSourceMaxJobs {
			return ciRunnerJobProof{}, fmt.Errorf("%w: job count metadata is invalid", ErrConflict)
		}
		total := *pg.TotalCount
		if baseline < 0 {
			baseline = total
			pages = int((total + ciSourceJobsPerPage - 1) / ciSourceJobsPerPage)
			if pages < 1 {
				pages = 1
			}
		} else if total != baseline {
			return ciRunnerJobProof{}, errCIRunnerJobPending
		}
		if pg.Jobs == nil {
			return ciRunnerJobProof{}, fmt.Errorf("%w: job list is missing", ErrConflict)
		}
		remaining := baseline - int64((page-1)*ciSourceJobsPerPage)
		expected := int64(ciSourceJobsPerPage)
		if remaining < expected {
			expected = remaining
		}
		if int64(len(pg.Jobs)) != expected {
			return ciRunnerJobProof{}, errCIRunnerJobPending
		}
		for i := range pg.Jobs {
			job := pg.Jobs[i]
			if job.ID <= 0 || job.RunID != hint.RunID {
				return ciRunnerJobProof{}, fmt.Errorf("%w: job identity does not match run", ErrConflict)
			}
			if len(job.RunAttempt) > 0 {
				var attempt int64
				if err := json.Unmarshal(job.RunAttempt, &attempt); err != nil ||
					attempt <= 0 || attempt != hint.Attempt {
					return ciRunnerJobProof{}, fmt.Errorf("%w: job attempt does not match run attempt", ErrConflict)
				}
			}
			if seen[job.ID] {
				return ciRunnerJobProof{}, errCIRunnerJobPending
			}
			seen[job.ID] = true
			if job.RunnerID != nil && *job.RunnerID == scope.RunnerID {
				matches = append(matches, job)
			}
		}
		if page >= pages {
			break
		}
	}

	if len(matches) == 0 {
		return ciRunnerJobProof{}, errCIRunnerJobPending
	}
	if len(matches) > 1 {
		return ciRunnerJobProof{}, fmt.Errorf("%w: multiple jobs claim the same runner", ErrConflict)
	}
	job := matches[0]
	if job.RunnerName != scope.RunnerName {
		return ciRunnerJobProof{}, fmt.Errorf("%w: runner name does not match scope", ErrConflict)
	}
	switch job.Status {
	case "in_progress":
	case "queued", "waiting", "requested", "pending":
		return ciRunnerJobProof{}, errCIRunnerJobPending
	default:
		return ciRunnerJobProof{}, fmt.Errorf("%w: job status cannot prove execution", ErrConflict)
	}
	if job.StartedAt == nil || job.StartedAt.IsZero() || job.CompletedAt != nil || job.Conclusion != nil {
		return ciRunnerJobProof{}, fmt.Errorf("%w: job timestamps are inconsistent with running state", ErrConflict)
	}
	started := job.StartedAt.UTC()
	if started.Before(scope.CreatedAt.Add(-time.Minute)) || started.After(now.Add(time.Minute)) {
		return ciRunnerJobProof{}, fmt.Errorf("%w: job start time is outside the admitted window", ErrConflict)
	}
	allowed, ok := ciSourceSizeLabels(size)
	if !ok || len(job.Labels) == 0 {
		return ciRunnerJobProof{}, fmt.Errorf("%w: job labels do not match the admitted size", ErrConflict)
	}
	for _, label := range job.Labels {
		if !allowed[strings.ToLower(label)] {
			return ciRunnerJobProof{}, fmt.Errorf("%w: job labels do not match the admitted size", ErrConflict)
		}
	}
	return ciRunnerJobProof{
		RepositoryID: scope.RepositoryID,
		Repository:   repo.FullName,
		JobID:        job.ID,
		RunID:        hint.RunID,
		Attempt:      hint.Attempt,
		RunnerID:     scope.RunnerID,
		RunnerName:   scope.RunnerName,
		StartedAt:    started,
	}, nil
}
