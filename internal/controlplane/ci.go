package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

type CISettings struct {
	PaidOverrunEnabled     bool   `json:"paidOverrunEnabled"`
	MonthlySpendLimitCents int64  `json:"monthlySpendLimitCents"`
	CacheEnabled           bool   `json:"cacheEnabled"`
	RetainFailedMinutes    int    `json:"retainFailedMinutes"`
	DefaultSize            string `json:"defaultSize"`
}

type CIJobStep struct {
	Name        string     `json:"name"`
	Number      int        `json:"number"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type CIJob struct {
	ID              string     `json:"id"`
	Repository      string     `json:"repository"`
	Name            string     `json:"name"`
	WorkflowName    string     `json:"workflowName"`
	Ref             string     `json:"ref"`
	HeadSHA         string     `json:"headSha"`
	GitHubJobID     int64      `json:"githubJobId"`
	GitHubRunID     int64      `json:"githubRunId"`
	GitHubURL       string     `json:"githubURL"`
	Status          string     `json:"status"`
	Conclusion      string     `json:"conclusion,omitempty"`
	Size            string     `json:"size"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
	ElapsedSeconds  int64      `json:"elapsedSeconds"`
	BillableSeconds int64      `json:"billableSeconds"`
	CostMicros      int64      `json:"costMicros"`
	QueueReason     string     `json:"queueReason"`
	RunnerID        *string    `json:"runnerId,omitempty"`
}

type CIInstallation struct {
	ID              string `json:"id"`
	InstallationID  int64  `json:"installationId"`
	Owner           string `json:"owner"`
	Status          string `json:"status"`
	RepositoryCount int    `json:"repositoryCount"`
}

type CIFeature struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description"`
}

type CIPool struct {
	Ready   bool   `json:"ready"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type CILogEntry struct {
	Sequence  int64     `json:"sequence"`
	Stream    string    `json:"stream"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type CIMetric struct {
	At               time.Time `json:"at"`
	Source           string    `json:"source,omitempty"`
	CPUPercent       *float64  `json:"cpuPercent"`
	MemoryBytes      *int64    `json:"memoryBytes"`
	MemoryLimitBytes *int64    `json:"memoryLimitBytes"`
	NetworkRxBytes   *int64    `json:"networkRxBytes"`
	NetworkTxBytes   *int64    `json:"networkTxBytes"`
	OOMKills         *int64    `json:"oomKills"`
}

type CITestResult struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Suite      string `json:"suite"`
	Status     string `json:"status"`
	DurationMs *int64 `json:"durationMs"`
	Message    string `json:"message"`
	Source     string `json:"source,omitempty"`
}

type CIJobDetail struct {
	Job               CIJob                `json:"job"`
	Steps             []CIJobStep          `json:"steps"`
	Logs              []CILogEntry         `json:"logs"`
	Metrics           []CIMetric           `json:"metrics"`
	Tests             []CITestResult       `json:"tests"`
	RunnerDiagnostics *CIRunnerDiagnostics `json:"runnerDiagnostics"`
	LogCollection     *CILogCollection     `json:"logCollection,omitempty"`
}

type CIOverview struct {
	Enabled       bool              `json:"enabled"`
	Pool          CIPool            `json:"pool"`
	Pricing       pricing.CICatalog `json:"pricing"`
	Settings      CISettings        `json:"settings"`
	Installations []CIInstallation  `json:"installations"`
	Jobs          []CIJob           `json:"jobs"`
	Features      []CIFeature       `json:"features"`
}

type ciJobDocument struct {
	Job   CIJob       `json:"job"`
	Steps []CIJobStep `json:"steps"`
}

type ciJobRow struct {
	Job                CIJob
	Repository         string
	GitHubRepositoryID int64
}

var ciPoolOffline = CIPool{Ready: false, Status: "not_configured", Message: "The CI runner backend has not been activated."}

func ciFeatureList(configured, cacheReady bool) []CIFeature {
	ingest := "not_configured"
	if configured {
		ingest = "available"
	}
	cache := "not_configured"
	cacheDesc := "Colocated caching is not configured."
	if cacheReady {
		cache = "available"
		cacheDesc = "Immutable cache blobs are scoped to admitted GitHub jobs and repository trust."
	}
	return []CIFeature{
		{ID: "job-ingest", Name: "Job ingest", Status: ingest, Description: "Signed GitHub App webhooks record workflow job metadata for this workspace."},
		{ID: "github-app", Name: "GitHub App", Status: ingest, Description: "Connect a GitHub App installation to select repositories for CI."},
		{ID: "job-history", Name: "Job history", Status: "available", Description: "Reported workflow job status, timing, and conclusions are listed per workspace."},
		{ID: "job-cancel", Name: "Job cancellation", Status: "not_configured", Description: "Cancel jobs in GitHub; Canter-side cancellation is not activated."},
		{ID: "agent-inspect", Name: "Agent inspection", Status: "available", Description: "Agents can inspect repository-visible job records and runner lifecycle evidence. Private repository access is not delegated to agent sessions."},
		{ID: "runner-pool", Name: "Runner pool", Status: "not_configured", Description: "The CI runner backend has not been activated."},
		{ID: "paid-overrun", Name: "Paid overrun", Status: "not_configured", Description: "Overrun billing is not yet available; every run's first hour is free."},
		{ID: "job-logs", Name: "Job logs", Status: "not_configured", Description: "GitHub job logs are not configured."},
		{ID: "job-metrics", Name: "Job metrics", Status: "not_configured", Description: "Resource sampling is not collected yet."},
		{ID: "cache", Name: "Runner cache", Status: cache, Description: cacheDesc},
	}
}

func defaultCISettings() CISettings {
	return CISettings{PaidOverrunEnabled: false, MonthlySpendLimitCents: 1000, CacheEnabled: true, RetainFailedMinutes: 0, DefaultSize: "linux-x64-4"}
}

func (s *Store) CISettings(ctx context.Context, workspaceID string) (CISettings, error) {
	out := defaultCISettings()
	err := s.pool.QueryRow(ctx, `SELECT paid_overrun_enabled,monthly_spend_limit_cents,cache_enabled,retain_failed_minutes,default_size FROM ci_settings WHERE workspace_id=$1`, workspaceID).Scan(&out.PaidOverrunEnabled, &out.MonthlySpendLimitCents, &out.CacheEnabled, &out.RetainFailedMinutes, &out.DefaultSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	return out, err
}

func (s *Store) SaveCISettings(ctx context.Context, workspaceID, accountID string, st CISettings) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var accountEnabled bool
	if err = tx.QueryRow(ctx, `SELECT disabled_at IS NULL FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&accountEnabled); err != nil {
		return err
	}
	if !accountEnabled {
		return ErrForbidden
	}
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&locked); err != nil {
		return err
	}
	role, err := workspaceRole(ctx, tx, workspaceID, accountID, true)
	if err != nil {
		return err
	}
	if role != "owner" {
		return ErrForbidden
	}
	if err = s.checkCISpendLimit(ctx, tx, workspaceID, st.MonthlySpendLimitCents); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ci_settings(workspace_id,paid_overrun_enabled,monthly_spend_limit_cents,cache_enabled,cache_epoch,retain_failed_minutes,default_size,updated_at) VALUES($1,$2,$3,$4,CASE WHEN $4 THEN 1 ELSE 2 END,$5,$6,now()) ON CONFLICT(workspace_id) DO UPDATE SET paid_overrun_enabled=EXCLUDED.paid_overrun_enabled,monthly_spend_limit_cents=EXCLUDED.monthly_spend_limit_cents,cache_enabled=EXCLUDED.cache_enabled,cache_epoch=ci_settings.cache_epoch+CASE WHEN ci_settings.cache_enabled IS DISTINCT FROM EXCLUDED.cache_enabled THEN 1 ELSE 0 END,retain_failed_minutes=EXCLUDED.retain_failed_minutes,default_size=EXCLUDED.default_size,updated_at=now()`, workspaceID, st.PaidOverrunEnabled, st.MonthlySpendLimitCents, st.CacheEnabled, st.RetainFailedMinutes, st.DefaultSize); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListCIInstallations(ctx context.Context, workspaceID string) ([]CIInstallation, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id,i.github_installation_id,i.owner,i.status,(SELECT count(*) FROM ci_repositories r WHERE r.installation_id=i.id AND r.workspace_id=i.workspace_id AND r.enabled) FROM ci_github_installations i WHERE i.workspace_id=$1 ORDER BY i.created_at,i.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CIInstallation{}
	for rows.Next() {
		var item CIInstallation
		if err = rows.Scan(&item.ID, &item.InstallationID, &item.Owner, &item.Status, &item.RepositoryCount); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanCIJobRow(row pgx.Row) (ciJobRow, []CIJobStep, error) {
	var id string
	var raw []byte
	var repository, status string
	var repoID, runID, githubJobID int64
	var createdAt time.Time
	if err := row.Scan(&id, &raw, &repository, &repoID, &runID, &githubJobID, &createdAt, &status); err != nil {
		return ciJobRow{}, nil, err
	}
	var doc ciJobDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ciJobRow{}, nil, err
	}
	doc.Job.ID = id
	doc.Job.Repository = repository
	doc.Job.GitHubJobID = githubJobID
	doc.Job.GitHubRunID = runID
	doc.Job.Status = status
	doc.Job.GitHubURL = ""
	if repository != "" && runID > 0 && githubJobID > 0 {
		doc.Job.GitHubURL = fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", repository, runID, githubJobID)
	}
	if doc.Job.CreatedAt.IsZero() {
		doc.Job.CreatedAt = createdAt
	}
	if doc.Steps == nil {
		doc.Steps = []CIJobStep{}
	}
	return ciJobRow{Job: doc.Job, Repository: repository, GitHubRepositoryID: repoID}, doc.Steps, nil
}

func (s *Store) listCIJobRows(ctx context.Context, workspaceID string, limit int) ([]ciJobRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT j.id,j.document,COALESCE(r.full_name,''),j.github_repository_id,j.github_run_id,j.github_job_id,j.created_at,j.status FROM ci_jobs j LEFT JOIN ci_repositories r ON r.workspace_id=j.workspace_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.github_repository_id WHERE j.workspace_id=$1 ORDER BY j.created_at DESC,j.id DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ciJobRow{}
	for rows.Next() {
		row, _, err := scanCIJobRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) CIJobRow(ctx context.Context, workspaceID, jobID string) (ciJobRow, []CIJobStep, error) {
	row, steps, err := scanCIJobRow(s.pool.QueryRow(ctx, `SELECT j.id,j.document,COALESCE(r.full_name,''),j.github_repository_id,j.github_run_id,j.github_job_id,j.created_at,j.status FROM ci_jobs j LEFT JOIN ci_repositories r ON r.workspace_id=j.workspace_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.github_repository_id WHERE j.workspace_id=$1 AND j.id=$2`, workspaceID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ciJobRow{}, nil, ErrNotFound
	}
	if err != nil {
		return ciJobRow{}, nil, err
	}
	return row, steps, nil
}

func (s *Store) CIJobLogs(ctx context.Context, workspaceID, jobID string) ([]CILogEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT sequence,stream,content,created_at FROM ci_job_logs WHERE workspace_id=$1 AND job_id=$2 ORDER BY sequence DESC LIMIT 500`, workspaceID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CILogEntry{}
	total := 0
	for rows.Next() {
		var entry CILogEntry
		if err = rows.Scan(&entry.Sequence, &entry.Stream, &entry.Text, &entry.CreatedAt); err != nil {
			return nil, err
		}
		total += len(entry.Text)
		if total > 1<<20 {
			break
		}
		out = append(out, entry)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (s *Store) CIJobMetrics(ctx context.Context, workspaceID, jobID string) ([]CIMetric, error) {
	rows, err := s.pool.Query(ctx, `SELECT sampled_at,document FROM ci_job_metrics WHERE workspace_id=$1 AND job_id=$2 ORDER BY sampled_at DESC LIMIT 500`, workspaceID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CIMetric{}
	for rows.Next() {
		var at time.Time
		var raw []byte
		if err = rows.Scan(&at, &raw); err != nil {
			return nil, err
		}
		var metric CIMetric
		if err = json.Unmarshal(raw, &metric); err != nil {
			return nil, err
		}
		metric.At = at
		out = append(out, metric)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (s *Store) CIJobTests(ctx context.Context, workspaceID, jobID string) ([]CITestResult, error) {
	rows, err := s.pool.Query(ctx, `SELECT document FROM ci_job_tests WHERE workspace_id=$1 AND job_id=$2 ORDER BY id LIMIT 1000`, workspaceID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CITestResult{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var test CITestResult
		if err = json.Unmarshal(raw, &test); err != nil {
			return nil, err
		}
		out = append(out, test)
	}
	return out, rows.Err()
}

func (s *Store) CreateCIConnectState(ctx context.Context, workspaceID, accountID, stateHash string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO ci_connect_states(state_hash,workspace_id,account_id,expires_at) VALUES($1,$2,$3,$4)`, stateHash, workspaceID, accountID, expiresAt)
	return err
}

func (s *Store) ConsumeCIConnectState(ctx context.Context, stateHash string) (workspaceID, accountID string, err error) {
	err = s.pool.QueryRow(ctx, `DELETE FROM ci_connect_states WHERE state_hash=$1 AND expires_at>now() RETURNING workspace_id,account_id`, stateHash).Scan(&workspaceID, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return workspaceID, accountID, err
}

func (s *Store) GetCIConnectState(ctx context.Context, stateHash string) (workspaceID, accountID string, expiresAt time.Time, err error) {
	err = s.pool.QueryRow(ctx, `SELECT workspace_id,account_id,expires_at FROM ci_connect_states WHERE state_hash=$1 AND expires_at>now()`, stateHash).Scan(&workspaceID, &accountID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", time.Time{}, ErrNotFound
	}
	return workspaceID, accountID, expiresAt, err
}

func (s *Store) CIInstallationByGitHubID(ctx context.Context, githubInstallationID int64) (id, workspaceID, owner, status string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT id,workspace_id,owner,status FROM ci_github_installations WHERE github_installation_id=$1`, githubInstallationID).Scan(&id, &workspaceID, &owner, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", "", ErrNotFound
	}
	return id, workspaceID, owner, status, err
}

func (s *Store) SetCIInstallationStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE ci_github_installations SET status=$2,updated_at=now() WHERE id=$1`, id, status)
	return err
}

func (s *Store) CIRepositoryForJob(ctx context.Context, installationID string, githubRepositoryID int64) (fullName string, private, enabled bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT full_name,private,enabled FROM ci_repositories WHERE installation_id=$1 AND github_repository_id=$2`, installationID, githubRepositoryID).Scan(&fullName, &private, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, ErrNotFound
	}
	return fullName, private, enabled, err
}

func (s *Store) CIProductEnabled(ctx context.Context, workspaceID string) (bool, error) {
	return s.productEnabled(ctx, s.pool, workspaceID, "ci")
}

var ErrCICancelActive = errors.New("CI job cancellation is not yet available; cancel the job in GitHub instead.")

func (s *Store) CancelCIJob(ctx context.Context, workspaceID, jobID string) (CIJob, bool, error) {
	row, _, err := s.CIJobRow(ctx, workspaceID, jobID)
	if err != nil {
		return CIJob{}, false, err
	}
	if row.Job.Status == "completed" || row.Job.Status == "cancelled" {
		return row.Job, false, nil
	}
	return CIJob{}, false, ErrCICancelActive
}

var (
	errCIConnectState      = errors.New("CI connect state is invalid or expired")
	errCINotWorkspaceOwner = errors.New("only the workspace owner can connect the CI GitHub App")
	errCIAccountDisabled   = errors.New("account is disabled")
)

func (s *Store) BindCIInstallation(ctx context.Context, workspaceID, accountID, stateHash string, githubInstallationID, githubAccountID int64, owner, accountType string, repos []ciGitHubRepo) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var accountEnabled bool
	if err = tx.QueryRow(ctx, `SELECT disabled_at IS NULL FROM accounts WHERE id=$1 FOR UPDATE`, accountID).Scan(&accountEnabled); err != nil {
		return "", err
	}
	if !accountEnabled {
		return "", errCIAccountDisabled
	}
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&locked); err != nil {
		return "", err
	}
	var stateWorkspace, stateAccount string
	err = tx.QueryRow(ctx, `SELECT workspace_id,account_id FROM ci_connect_states WHERE state_hash=$1 AND workspace_id=$2 AND account_id=$3 AND expires_at>clock_timestamp() FOR UPDATE`, stateHash, workspaceID, accountID).Scan(&stateWorkspace, &stateAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errCIConnectState
	}
	if err != nil {
		return "", err
	}
	role, err := workspaceRole(ctx, tx, workspaceID, accountID, true)
	if err != nil {
		return "", err
	}
	if role != "owner" {
		return "", errCINotWorkspaceOwner
	}
	if err := s.requireWorkspaceProductForUseTx(ctx, tx, workspaceID, "ci", accountID, sdk.ActorRef{Kind: "human", ID: accountID}); err != nil {
		return "", err
	}
	var existingID, existingWorkspace string
	var existingAccountID int64
	err = tx.QueryRow(ctx, `SELECT id,workspace_id,github_account_id FROM ci_github_installations WHERE github_installation_id=$1 FOR UPDATE`, githubInstallationID).Scan(&existingID, &existingWorkspace, &existingAccountID)
	bound := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	installationID := existingID
	if !bound {
		installationID, err = newID("ciinst_")
		if err != nil {
			return "", err
		}
		var inserted string
		err = tx.QueryRow(ctx, `INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,status,created_by) VALUES($1,$2,$3,$4,$5,$6,'active',$7) ON CONFLICT(github_installation_id) DO NOTHING RETURNING id`, installationID, workspaceID, githubInstallationID, githubAccountID, owner, accountType, accountID).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			if qerr := tx.QueryRow(ctx, `SELECT id,workspace_id,github_account_id FROM ci_github_installations WHERE github_installation_id=$1 FOR UPDATE`, githubInstallationID).Scan(&existingID, &existingWorkspace, &existingAccountID); qerr != nil {
				return "", qerr
			}
			bound = true
			installationID = existingID
		} else if err != nil {
			return "", err
		}
	}
	if bound {
		if existingWorkspace != workspaceID || existingAccountID != githubAccountID {
			return "", errCIInstallationBound
		}
		if _, err = tx.Exec(ctx, `UPDATE ci_github_installations SET owner=$2,account_type=$3,status='active',updated_at=now() WHERE id=$1`, installationID, owner, accountType); err != nil {
			return "", err
		}
	}
	ids := make([]int64, 0, len(repos))
	for _, repo := range repos {
		ids = append(ids, repo.ID)
		branch := repo.DefaultBranch
		if branch == "" {
			branch = "main"
		}
		if _, err = tx.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name,private,default_branch,enabled,updated_at) VALUES($1,$2,$3,$4,$5,$6,true,now()) ON CONFLICT(installation_id,github_repository_id) DO UPDATE SET full_name=EXCLUDED.full_name,private=EXCLUDED.private,default_branch=EXCLUDED.default_branch,enabled=true,updated_at=now()`, workspaceID, installationID, repo.ID, repo.FullName, repo.Private, branch); err != nil {
			return "", err
		}
	}
	if bound {
		if _, err = tx.Exec(ctx, `UPDATE ci_repositories SET enabled=false,updated_at=now() WHERE installation_id=$1 AND github_repository_id<>ALL($2)`, installationID, ids); err != nil {
			return "", err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM ci_connect_states WHERE state_hash=$1`, stateHash); err != nil {
		return "", err
	}
	return installationID, tx.Commit(ctx)
}
