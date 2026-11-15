package controlplane

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/canter0/canter/pricing"
)

// ciGitHubLabel is one runner label object; GitHub returns labels as
// [{id,name,type}], never bare strings.
type ciGitHubLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ciGitHubRunner is the management-view runner record used for JIT
// registration and cleanup proofs.
type ciGitHubRunner struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Busy   bool            `json:"busy"`
	Labels []ciGitHubLabel `json:"labels"`
}

type ciGitHubRunnerPage struct {
	TotalCount *int64           `json:"total_count"`
	Runners    []ciGitHubRunner `json:"runners"`
}

type ciJITConfigResponse struct {
	Runner struct {
		ID     int64           `json:"id"`
		Name   string          `json:"name"`
		Labels []ciGitHubLabel `json:"labels"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

const (
	ciRunnerListPerPage = 100
	ciRunnerListMaxPage = 100
	ciJITConfigMaxBytes = 65536
)

// ciRunnerManagementToken mints a repository-scoped installation token with
// administration:write + metadata:read for runner management only.
func (c *ciGitHubClient) ciRunnerManagementToken(ctx context.Context, installationID, repositoryID int64) (string, error) {
	if installationID <= 0 || repositoryID <= 0 {
		return "", fmt.Errorf("%w: invalid installation or repository", ErrConflict)
	}
	body := struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}{
		RepositoryIDs: []int64{repositoryID},
		Permissions:   map[string]string{"administration": "write", "metadata": "read"},
	}
	var out ciInstallationToken
	if err := c.ciSourceJSON(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", installationID),
		"app", body, &out); err != nil {
		return "", err
	}
	if !ciSourceTokenUsable(out.Token) {
		return "", fmt.Errorf("%w: GitHub returned an unusable runner token", ErrConflict)
	}
	return out.Token, nil
}

// ciManagementRepository resolves the numeric repository identity and
// requires canonical full-name plus privacy proof.
func (c *ciGitHubClient) ciManagementRepository(ctx context.Context, token string, repositoryID int64) (owner, name string, err error) {
	var repo ciSourceRepository
	if err = c.ciSourceJSON(ctx, http.MethodGet,
		fmt.Sprintf("/repositories/%d", repositoryID), token, nil, &repo); err != nil {
		return "", "", err
	}
	if repo.ID != repositoryID || repo.Private == nil {
		return "", "", fmt.Errorf("%w: repository identity or privacy is not provable", ErrConflict)
	}
	owner, name, ok := ciCanonicalRepoName(repo.FullName)
	if !ok {
		return "", "", fmt.Errorf("%w: repository canonical name is invalid", ErrConflict)
	}
	return owner, name, nil
}

// ciListRepoRunners returns the complete paginated runner list for one
// repository. Truncated or inconsistent pagination is an error, never an
// empty list.
func (c *ciGitHubClient) ciListRepoRunners(ctx context.Context, token, owner, name string) ([]ciGitHubRunner, error) {
	var all []ciGitHubRunner
	seen := make(map[int64]bool)
	baseline := int64(-1)
	pages := 1
	for page := 1; page <= ciRunnerListMaxPage; page++ {
		path := fmt.Sprintf("/repos/%s/%s/actions/runners?per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(name), ciRunnerListPerPage, page)
		var pg ciGitHubRunnerPage
		if err := c.ciSourceJSON(ctx, http.MethodGet, path, token, nil, &pg); err != nil {
			return nil, err
		}
		if pg.TotalCount == nil || *pg.TotalCount < 0 {
			return nil, fmt.Errorf("%w: runner count metadata is invalid", ErrConflict)
		}
		total := *pg.TotalCount
		if total > ciRunnerListMaxPage*ciRunnerListPerPage {
			return nil, fmt.Errorf("%w: runner count exceeds bound", ErrConflict)
		}
		if baseline < 0 {
			baseline = total
			pages = int((total + ciRunnerListPerPage - 1) / ciRunnerListPerPage)
			if pages < 1 {
				pages = 1
			}
		} else if total != baseline {
			return nil, fmt.Errorf("%w: runner list changed during pagination", ErrConflict)
		}
		if pg.Runners == nil {
			return nil, fmt.Errorf("%w: runner list is missing", ErrConflict)
		}
		remaining := baseline - int64((page-1)*ciRunnerListPerPage)
		expected := int64(ciRunnerListPerPage)
		if remaining < expected {
			expected = remaining
		}
		if int64(len(pg.Runners)) != expected {
			return nil, fmt.Errorf("%w: runner page is truncated", ErrConflict)
		}
		for _, r := range pg.Runners {
			if r.ID <= 0 || seen[r.ID] || strings.TrimSpace(r.Name) == "" || r.Labels == nil {
				return nil, fmt.Errorf("%w: runner identity is invalid", ErrConflict)
			}
			for _, label := range r.Labels {
				if label.Name == "" || (label.Type != "" && label.Type != "custom" && label.Type != "read-only") {
					return nil, fmt.Errorf("%w: runner label is invalid", ErrConflict)
				}
			}
			seen[r.ID] = true
			all = append(all, r)
		}
		if page >= pages {
			return all, nil
		}
	}
	return nil, fmt.Errorf("%w: runner pagination exceeded bound", ErrConflict)
}

// ciRunnerOwnerLabel is the custom label binding a GitHub runner to exactly
// one Canter operation.
func ciRunnerOwnerLabel(operationID string) string {
	return "canter-operation-" + operationID
}

func ciRunnerLabelsForSize(sizeID, operationID string) ([]string, error) {
	size, ok := pricing.FindCI(sizeID)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported CI size", ErrConflict)
	}
	allowed, ok := ciSourceSizeLabels(size)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported CI size", ErrConflict)
	}
	labels := make([]string, 0, len(allowed)+1)
	for label := range allowed {
		labels = append(labels, label)
	}
	sortStrings(labels)
	labels = append(labels, ciRunnerOwnerLabel(operationID))
	return labels, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func ciRunnerHasOwnerLabel(r ciGitHubRunner, operationID string) bool {
	want := ciRunnerOwnerLabel(operationID)
	for _, l := range r.Labels {
		// GitHub reports JIT-provisioned labels as read-only; only the name
		// binds a runner to the operation.
		if l.Name == want && (l.Type == "custom" || l.Type == "read-only") {
			return true
		}
	}
	return false
}

func ciRunnerHasOperationLabelName(r ciGitHubRunner, operationID string) bool {
	for _, label := range r.Labels {
		if label.Name == ciRunnerOwnerLabel(operationID) {
			return true
		}
	}
	return false
}

// githubManagementAuth resolves the repo-scoped management token and
// canonical repository path for one lease spec.
func (e *ciEnforcerEngine) githubManagementAuth(ctx context.Context, spec ciEnforcerSpec) (token, owner, name string, err error) {
	token, err = e.github.ciRunnerManagementToken(ctx, spec.GitHubInstallationID, spec.GitHubRepositoryID)
	if err != nil {
		return "", "", "", err
	}
	owner, name, err = e.github.ciManagementRepository(ctx, token, spec.GitHubRepositoryID)
	if err != nil {
		return "", "", "", err
	}
	return token, owner, name, nil
}

// ciJITPreflight is everything needed for the sole JIT POST, resolved before
// the durable attempt marker is written.
type ciJITPreflight struct {
	token      string
	owner      string
	name       string
	runnerName string
	labels     []string
}

// githubJITPreflight performs token, repository, and complete runner-list
// preflight. A canonical-name conflict latches permanent conflict; transient
// failures are retryable because no POST has happened yet.
func (e *ciEnforcerEngine) githubJITPreflight(ctx context.Context, l *ciEnforcerLease) (*ciJITPreflight, error) {
	spec := l.Spec
	token, owner, name, err := e.githubManagementAuth(ctx, spec)
	if err != nil {
		return nil, err
	}
	runnerName := ciEnforcerRunnerName(spec.Plan.OperationID)
	labels, err := ciRunnerLabelsForSize(spec.SizeID, spec.Plan.OperationID)
	if err != nil {
		return nil, err
	}
	// An existing canonical name conflicts; never overwrite or adopt it.
	runners, err := e.github.ciListRepoRunners(ctx, token, owner, name)
	if err != nil {
		return nil, err
	}
	for _, r := range runners {
		if r.Name == runnerName || ciRunnerHasOperationLabelName(r, spec.Plan.OperationID) {
			return nil, fmt.Errorf("%w: canonical runner name already exists", errCIEnforcerRunnerConflict)
		}
	}
	return &ciJITPreflight{token: token, owner: owner, name: name, runnerName: runnerName, labels: labels}, nil
}

// githubJITPost performs the sole JIT POST. definitive=false means the POST
// outcome is unknown and must be recovered via discovery; definitive with
// runnerID=0 means GitHub rejected creation and registration may close.
func (e *ciEnforcerEngine) githubJITPost(ctx context.Context, l *ciEnforcerLease, pre *ciJITPreflight) (int64, string, bool, error) {
	spec := l.Spec
	body := struct {
		Name          string   `json:"name"`
		RunnerGroupID int64    `json:"runner_group_id"`
		WorkFolder    string   `json:"work_folder"`
		Labels        []string `json:"labels"`
	}{Name: pre.runnerName, RunnerGroupID: 1, WorkFolder: "_work", Labels: pre.labels}
	var jit ciJITConfigResponse
	if err := e.github.ciSourceJSON(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig",
			url.PathEscape(pre.owner), url.PathEscape(pre.name)),
		pre.token, body, &jit); err != nil {
		var ghErr *ciGitHubError
		if errors.As(err, &ghErr) && ghErr.status >= 400 && ghErr.status < 500 && ghErr.status != 408 && ghErr.status != 429 {
			return 0, "", true, err
		}
		return 0, "", false, err
	}
	if jit.Runner.ID <= 0 || jit.Runner.Name != pre.runnerName {
		return 0, "", false, fmt.Errorf("%w: JIT runner identity mismatch", ErrConflict)
	}
	// The JIT response runner must carry the custom owner label.
	if !ciJITRunnerHasOwnerLabel(jit.Runner.Labels, spec.Plan.OperationID) {
		return 0, "", false, fmt.Errorf("%w: JIT runner owner label missing", ErrConflict)
	}
	// Bound the ENCODED config before decoding; it must be nonempty base64.
	if len(jit.EncodedJITConfig) == 0 || len(jit.EncodedJITConfig) > ciJITConfigMaxBytes {
		return 0, "", false, fmt.Errorf("%w: JIT config is invalid", ErrConflict)
	}
	decoded, derr := base64.StdEncoding.DecodeString(jit.EncodedJITConfig)
	if derr != nil || len(decoded) == 0 || len(decoded) > ciJITConfigMaxBytes {
		return 0, "", false, fmt.Errorf("%w: JIT config is invalid", ErrConflict)
	}
	return jit.Runner.ID, jit.EncodedJITConfig, true, nil
}

func ciJITRunnerHasOwnerLabel(labels []ciGitHubLabel, operationID string) bool {
	want := ciRunnerOwnerLabel(operationID)
	for _, l := range labels {
		if l.Name == want && (l.Type == "custom" || l.Type == "read-only") {
			return true
		}
	}
	return false
}

// githubRecoverRunnerID resolves an ambiguous JIT POST via complete
// discovery: exactly one runner matching canonical name AND owner label.
// Zero matches stays unresolved; multiple or partial matches never prove.
func (e *ciEnforcerEngine) githubRecoverRunnerID(ctx context.Context, spec ciEnforcerSpec) (int64, error) {
	token, owner, name, err := e.githubManagementAuth(ctx, spec)
	if err != nil {
		return 0, err
	}
	runners, err := e.github.ciListRepoRunners(ctx, token, owner, name)
	if err != nil {
		return 0, err
	}
	runnerName := ciEnforcerRunnerName(spec.Plan.OperationID)
	var found int64
	count := 0
	for _, r := range runners {
		if r.Name == runnerName || ciRunnerHasOperationLabelName(r, spec.Plan.OperationID) {
			count++
			if r.Name == runnerName && ciRunnerHasOwnerLabel(r, spec.Plan.OperationID) {
				found = r.ID
			}
		}
	}
	if count != 1 || found <= 0 {
		return 0, nil
	}
	return found, nil
}

// githubDeleteRunner removes an exact numeric runner ID after verifying
// identity/name/owner label, then proves absence via direct 404 plus complete
// discovery. DELETE acceptance alone is never proof.
func (e *ciEnforcerEngine) githubDeleteRunner(ctx context.Context, spec ciEnforcerSpec, runnerID int64) (bool, error) {
	token, owner, name, err := e.githubManagementAuth(ctx, spec)
	if err != nil {
		return false, err
	}
	runnerName := ciEnforcerRunnerName(spec.Plan.OperationID)
	path := fmt.Sprintf("/repos/%s/%s/actions/runners/%d",
		url.PathEscape(owner), url.PathEscape(name), runnerID)
	var runner ciGitHubRunner
	if err = e.github.ciSourceJSON(ctx, http.MethodGet, path, token, nil, &runner); err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			// Already absent at GitHub; still require discovery absence.
			return e.githubRunnerAbsent(ctx, token, owner, name, runnerID, runnerName, spec.Plan.OperationID)
		}
		return false, err
	}
	if runner.ID != runnerID || runner.Name != runnerName || !ciRunnerHasOwnerLabel(runner, spec.Plan.OperationID) {
		return false, fmt.Errorf("%w: runner identity mismatch before delete", ErrConflict)
	}
	if err = e.github.call(ctx, http.MethodDelete, path, token, nil, nil); err != nil {
		return false, err
	}
	return e.githubRunnerAbsent(ctx, token, owner, name, runnerID, runnerName, spec.Plan.OperationID)
}

func isGitHubStatus(err error, status int) bool {
	var ghErr *ciGitHubError
	return errors.As(err, &ghErr) && ghErr.status == status
}

// githubRunnerAbsent proves the runner is gone: direct GET 404 AND complete
// scoped discovery contains no matching identity.
func (e *ciEnforcerEngine) githubRunnerAbsent(ctx context.Context, token, owner, name string, runnerID int64, runnerName, operationID string) (bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runners/%d",
		url.PathEscape(owner), url.PathEscape(name), runnerID)
	var probe ciGitHubRunner
	if err := e.github.ciSourceJSON(ctx, http.MethodGet, path, token, nil, &probe); err == nil {
		return false, nil
	} else if !isGitHubStatus(err, http.StatusNotFound) {
		return false, err
	}
	runners, err := e.github.ciListRepoRunners(ctx, token, owner, name)
	if err != nil {
		return false, err
	}
	for _, r := range runners {
		if r.ID == runnerID || r.Name == runnerName || ciRunnerHasOperationLabelName(r, operationID) {
			return false, nil
		}
	}
	return true, nil
}
