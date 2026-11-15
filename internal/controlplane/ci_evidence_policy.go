package controlplane

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ciEvidenceRequestMaxBytes        = 256 << 10
	ciEvidenceAcceptedMaxBytes       = 8 << 20
	ciEvidenceMaxMetrics             = 4096
	ciEvidenceMaxTests               = 1000
	ciEvidenceMaxBatches             = 4096
	ciEvidenceHTTPConcurrency        = 4
	ciEvidenceMemoryMaxBytes   int64 = 16 << 30
	ciEvidenceRequestBound           = 5 * time.Second
	ciEvidenceUploadGrace            = 5 * time.Minute
	ciEvidenceClockSkew              = 30 * time.Second
	ciEvidenceMACDomain              = "canter-ci-evidence-v1."
	ciEvidenceTokenPrefix            = "ce1"
	ciEvidenceHeader                 = "X-Canter-CI-Evidence"
)

var errCIEvidenceInvalid = errors.New("invalid CI evidence")
var errCIEvidenceQuota = errors.New("CI evidence quota exceeded")
var errCIEvidenceUnauthorized = errors.New("unauthorized CI evidence")

type ciEvidenceCapability struct {
	verified             bool
	Version              int    `json:"version"`
	Workspace            string `json:"workspace"`
	Runner               string `json:"runner"`
	AdmissionID          string `json:"admissionId"`
	GitHubInstallationID int64  `json:"githubInstallationId"`
	GitHubRepositoryID   int64  `json:"githubRepositoryId"`
	GitHubRunnerID       int64  `json:"githubRunnerId"`
	GitHubJobID          int64  `json:"githubJobId"`
	DeadlineUnixMs       int64  `json:"deadlineUnixMs"`
}

type ciEvidenceBatch struct {
	SchemaVersion int            `json:"schemaVersion"`
	BatchID       string         `json:"batchId"`
	Metrics       []CIMetric     `json:"metrics"`
	Tests         []CITestResult `json:"tests"`
}

type ciEvidenceScope struct {
	Workspace, Job, Runner, SizeID                                     string
	GitHubInstallationID, GitHubRepositoryID, GitHubJobID, GitHubRunID int64
	AdmissionID, RunnerName                                            string
	AdmissionRepositoryID                                              int64
	CreatedAt, StartedAt, DeadlineAt, HardDeadlineAt                   time.Time
	StoppedAt, StopRequestedAt, BillStopAt                             *time.Time
	Decision                                                           []byte
	GitHubRunnerID                                                     *int64
}

const ciEvidenceWorkspaceLockSQL = `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`

// The durable assignment selects the job. Neither URL nor JSON may supply a
// workspace/job override. Nullable runner IDs permit a final report after
// registration retirement; the immutable admission decision still binds it.
const ciEvidenceScopeSQL = `
SELECT r.workspace_id,j.id,r.id,r.size_id,i.github_installation_id,
       j.github_repository_id,j.github_job_id,j.github_run_id,
       a.admission_id,a.runner_name,a.github_repository_id,r.created_at,
       r.execution_started_at,r.deadline_at,l.hard_deadline_at,
       r.execution_stopped_at,l.stop_requested_at,l.bill_stop_at,a.decision,r.github_runner_id
FROM ci_runners r
JOIN ci_jobs j ON j.workspace_id=r.workspace_id AND j.id=r.assigned_job_id
    AND j.runner_id=r.id AND j.installation_id=r.installation_id
JOIN ci_runner_admissions a ON a.workspace_id=r.workspace_id AND a.runner_id=r.id
JOIN ci_runner_lifecycles l ON l.workspace_id=r.workspace_id AND l.runner_id=r.id
JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id
JOIN ci_repositories p ON p.workspace_id=j.workspace_id AND p.installation_id=j.installation_id
    AND p.github_repository_id=j.github_repository_id
WHERE r.workspace_id=$1 AND r.id=$2
    AND r.execution_started_at IS NOT NULL AND r.deadline_at IS NOT NULL
    AND a.decision IS NOT NULL AND i.status='active' AND p.enabled
    AND j.status IN ('running','completed','cancelled')
    AND EXISTS(SELECT 1 FROM workspace_products wp
        WHERE wp.workspace_id=r.workspace_id AND wp.product='ci')
FOR UPDATE OF r,j,a,l,i,p`

const ciEvidenceBatchSQL = `
SELECT raw_sha256 FROM ci_job_evidence_batches
WHERE workspace_id=$1 AND job_id=$2 AND batch_id=$3`

const ciEvidenceUsageInitSQL = `
INSERT INTO ci_job_evidence_usage(workspace_id,job_id) VALUES($1,$2)
ON CONFLICT(job_id) DO NOTHING`

const ciEvidenceUsageSQL = `
SELECT metric_count,test_count,batch_count,accepted_bytes
FROM ci_job_evidence_usage WHERE workspace_id=$1 AND job_id=$2 FOR UPDATE`

const ciEvidenceMetricExistsSQL = `
SELECT document=$4::jsonb FROM ci_job_metrics
WHERE workspace_id=$1 AND job_id=$2 AND sampled_at=$3`

const ciEvidenceMetricInsertSQL = `
INSERT INTO ci_job_metrics(workspace_id,job_id,sampled_at,document) VALUES($1,$2,$3,$4)`

const ciEvidenceTestExistsSQL = `
SELECT document=$4::jsonb FROM ci_job_tests
WHERE workspace_id=$1 AND job_id=$2 AND id=$3`

const ciEvidenceTestInsertSQL = `
INSERT INTO ci_job_tests(workspace_id,job_id,id,document) VALUES($1,$2,$3,$4)`

const ciEvidenceUsageUpdateSQL = `
UPDATE ci_job_evidence_usage
SET metric_count=metric_count+$3,test_count=test_count+$4,batch_count=batch_count+1,
    accepted_bytes=accepted_bytes+$5
WHERE workspace_id=$1 AND job_id=$2
    AND metric_count+$3<=4096 AND test_count+$4<=1000
    AND batch_count+1<=4096 AND accepted_bytes+$5<=8388608`

const ciEvidenceBatchInsertSQL = `
INSERT INTO ci_job_evidence_batches(workspace_id,job_id,batch_id,raw_sha256,created_at)
VALUES($1,$2,$3,$4,$5)`

// Authentication uses server-produced admission proof, not guest timestamps,
// measurements or declared test results. Reports never change billing/phase.
func (s ciEvidenceScope) window(c ciEvidenceCapability, now time.Time) (time.Time, error) {
	if !c.verified || s.Workspace != c.Workspace || s.Runner != c.Runner ||
		s.AdmissionID != c.AdmissionID || s.GitHubInstallationID != c.GitHubInstallationID ||
		s.GitHubRepositoryID != c.GitHubRepositoryID || s.AdmissionRepositoryID != c.GitHubRepositoryID ||
		s.GitHubJobID != c.GitHubJobID || s.DeadlineAt.UnixMilli() != c.DeadlineUnixMs ||
		s.StartedAt.IsZero() || !s.DeadlineAt.After(s.StartedAt) ||
		s.DeadlineAt.After(s.HardDeadlineAt) ||
		(s.GitHubRunnerID != nil && *s.GitHubRunnerID != c.GitHubRunnerID) {
		return time.Time{}, errCIEvidenceUnauthorized
	}
	var decision ciAdmissionDecision
	if ciEnforcerStrictJSON(s.Decision, &decision) != nil || !decision.Request.valid() ||
		decision.Request.AdmissionID != c.AdmissionID ||
		decision.Receipt.Decision != "allow" || decision.Receipt.ciAdmissionRequest != decision.Request ||
		decision.Receipt.ExecutionDeadlineUnixMs != c.DeadlineUnixMs ||
		decision.Proof.JobID != c.GitHubJobID || decision.Proof.RunID != s.GitHubRunID ||
		!decision.Proof.StartedAt.Equal(s.StartedAt) ||
		!ciAdmissionProofMatches(decision.Request, ciRunnerJobScope{
			RepositoryID: s.GitHubRepositoryID, RunnerID: c.GitHubRunnerID,
			RunnerName: s.RunnerName, SizeID: s.SizeID, CreatedAt: s.CreatedAt,
		}, decision.Proof, now) {
		return time.Time{}, errCIEvidenceUnauthorized
	}
	cutoff := s.DeadlineAt
	for _, at := range []*time.Time{s.StoppedAt, s.StopRequestedAt, s.BillStopAt} {
		if at != nil && at.Before(cutoff) {
			cutoff = *at
		}
	}
	if !now.Before(cutoff.Add(ciEvidenceUploadGrace)) {
		return time.Time{}, errCIEvidenceUnauthorized
	}
	return cutoff, nil
}

func ciEvidenceText(value string, maximum int, required bool) bool {
	return (!required || value != "") && len(value) <= maximum &&
		utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

// Optional numeric values remain nil when not observed. Do not fill gaps,
// infer a job conclusion, or label guest measurements independently verified.
func normalizeCIEvidence(batch ciEvidenceBatch, scope ciEvidenceScope, cutoff, now time.Time) (ciEvidenceBatch, error) {
	if batch.SchemaVersion != 1 || !ciLifecycleLeaseID.MatchString(batch.BatchID) ||
		batch.Metrics == nil || batch.Tests == nil ||
		len(batch.Metrics)+len(batch.Tests) == 0 ||
		len(batch.Metrics) > ciEvidenceMaxMetrics || len(batch.Tests) > ciEvidenceMaxTests {
		return ciEvidenceBatch{}, errCIEvidenceInvalid
	}
	metrics := make([]CIMetric, len(batch.Metrics))
	copy(metrics, batch.Metrics)
	tests := make([]CITestResult, len(batch.Tests))
	copy(tests, batch.Tests)
	batch.Metrics, batch.Tests = metrics, tests
	for i := range batch.Metrics {
		m := &batch.Metrics[i]
		m.At = m.At.UTC().Truncate(time.Microsecond)
		if m.At.IsZero() || m.At.Before(scope.StartedAt.Add(-ciEvidenceClockSkew)) ||
			m.At.After(cutoff) || m.At.After(now.Add(ciEvidenceClockSkew)) ||
			m.Source != "" {
			return ciEvidenceBatch{}, errCIEvidenceInvalid
		}
		observed := false
		if m.CPUPercent != nil {
			v := *m.CPUPercent
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
				return ciEvidenceBatch{}, errCIEvidenceInvalid
			}
			observed = true
		}
		for _, value := range []*int64{m.MemoryBytes, m.MemoryLimitBytes, m.NetworkRxBytes, m.NetworkTxBytes, m.OOMKills} {
			if value != nil {
				if *value < 0 {
					return ciEvidenceBatch{}, errCIEvidenceInvalid
				}
				observed = true
			}
		}
		if !observed || (m.MemoryLimitBytes != nil && (*m.MemoryLimitBytes == 0 || *m.MemoryLimitBytes > ciEvidenceMemoryMaxBytes)) ||
			(m.MemoryBytes != nil && *m.MemoryBytes > ciEvidenceMemoryMaxBytes) ||
			(m.MemoryBytes != nil && m.MemoryLimitBytes != nil && *m.MemoryBytes > *m.MemoryLimitBytes) {
			return ciEvidenceBatch{}, errCIEvidenceInvalid
		}
		m.Source = "guest"
	}
	for i := range batch.Tests {
		t := &batch.Tests[i]
		if !ciLifecycleLeaseID.MatchString(t.ID) || !ciEvidenceText(t.Name, 4096, true) ||
			!ciEvidenceText(t.Suite, 1024, false) || !ciEvidenceText(t.Message, 8192, false) ||
			t.Source != "" || (t.Status != "passed" && t.Status != "failed" && t.Status != "skipped" && t.Status != "error") ||
			(t.DurationMs != nil && (*t.DurationMs < 0 || *t.DurationMs > ciMaxRuntimeSeconds*1000)) {
			return ciEvidenceBatch{}, errCIEvidenceInvalid
		}
		t.Source = "workflow"
	}
	return batch, nil
}
