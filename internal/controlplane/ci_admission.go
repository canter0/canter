package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ciAdmissionJobKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,255}$`)

type ciAdmissionRequest struct {
	SchemaVersion      int    `json:"schemaVersion"`
	AdmissionID        string `json:"admissionId"`
	GitHubRunnerID     int64  `json:"githubRunnerId"`
	GitHubRepositoryID int64  `json:"githubRepositoryId"`
	Repository         string `json:"repository"`
	GitHubRunID        int64  `json:"githubRunId"`
	RunAttempt         int64  `json:"runAttempt"`
	JobKey             string `json:"jobKey"`
	RunnerName         string `json:"runnerName"`
}

type ciAdmissionReceipt struct {
	ciAdmissionRequest
	Decision                string `json:"decision"`
	ExecutionDeadlineUnixMs int64  `json:"executionDeadlineUnixMs"`
}

type ciAdmissionDecision struct {
	Request ciAdmissionRequest
	Proof   ciRunnerJobProof
	Receipt ciAdmissionReceipt
}

func (r ciAdmissionRequest) valid() bool {
	_, _, repoOK := ciCanonicalRepoName(r.Repository)
	return r.SchemaVersion == 1 && ciAdmissionID.MatchString(r.AdmissionID) &&
		r.GitHubRunnerID > 0 && r.GitHubRepositoryID > 0 && r.GitHubRunID > 0 &&
		r.RunAttempt > 0 && ciSourceRunnerName.MatchString(r.RunnerName) &&
		ciAdmissionJobKey.MatchString(r.JobKey) && repoOK
}

func ciAdmissionProofMatches(req ciAdmissionRequest, scope ciRunnerJobScope, proof ciRunnerJobProof, now time.Time) bool {
	return proof.JobID > 0 && proof.RepositoryID == scope.RepositoryID &&
		proof.RepositoryID == req.GitHubRepositoryID &&
		proof.RunnerID == scope.RunnerID && proof.RunnerID == req.GitHubRunnerID &&
		proof.RunnerName == scope.RunnerName && proof.RunnerName == req.RunnerName &&
		proof.RunID == req.GitHubRunID && proof.Attempt == req.RunAttempt &&
		strings.EqualFold(proof.Repository, req.Repository) &&
		!proof.StartedAt.IsZero() && !proof.StartedAt.Before(scope.CreatedAt.Add(-time.Minute)) &&
		!proof.StartedAt.After(now.Add(time.Minute))
}

// admitCIRunnerJob accepts only server-produced source proof, never a proof
// supplied by a guest/API body. A nil proof is a read-only recovery attempt.
// Every allow is returned after the assignment, budget and decision commit.
func (s *Store) admitCIRunnerJob(ctx context.Context, workspace, runner string, req ciAdmissionRequest, proof *ciRunnerJobProof, maximumSeconds int64) (ciAdmissionReceipt, error) {
	if workspace == "" || runner == "" || !req.valid() || maximumSeconds < 1 || maximumSeconds > ciMaxRuntimeSeconds {
		return ciAdmissionReceipt{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return ciAdmissionReceipt{}, err
	}
	var scope ciRunnerJobScope
	var installation, trigger, phase string
	var assigned *string
	var started, stopped *time.Time
	var decisionRaw []byte
	err = tx.QueryRow(ctx, `SELECT a.github_repository_id,r.github_runner_id,a.runner_name,r.size_id,r.created_at,r.installation_id,r.trigger_job_id,r.assigned_job_id,r.phase,r.execution_started_at,r.execution_stopped_at,a.decision FROM ci_runner_admissions a JOIN ci_runners r ON r.workspace_id=a.workspace_id AND r.id=a.runner_id JOIN ci_github_installations i ON i.workspace_id=r.workspace_id AND i.id=r.installation_id JOIN ci_repositories p ON p.workspace_id=r.workspace_id AND p.installation_id=r.installation_id AND p.github_repository_id=a.github_repository_id WHERE a.workspace_id=$1 AND a.runner_id=$2 AND a.admission_id=$3 AND r.phase IN ('idle','running') AND NOT r.delete_requested AND r.github_runner_id IS NOT NULL AND i.status='active' AND p.enabled FOR UPDATE OF r,a,i,p`, workspace, runner, req.AdmissionID).Scan(&scope.RepositoryID, &scope.RunnerID, &scope.RunnerName, &scope.SizeID, &scope.CreatedAt, &installation, &trigger, &assigned, &phase, &started, &stopped, &decisionRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	enabled, err := s.productEnabled(ctx, tx, workspace, "ci")
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	if !enabled || stopped != nil {
		return ciAdmissionReceipt{}, ErrConflict
	}
	if scope, err = ciAdmissionScopeValid(scope, s.now()); err != nil {
		return ciAdmissionReceipt{}, err
	}
	if scope.RepositoryID != req.GitHubRepositoryID || scope.RunnerID != req.GitHubRunnerID || scope.RunnerName != req.RunnerName {
		return ciAdmissionReceipt{}, ErrConflict
	}
	var chosen ciRunnerJobProof
	var previous ciAdmissionDecision
	recovering := len(decisionRaw) != 0
	if recovering {
		if err = json.Unmarshal(decisionRaw, &previous); err != nil {
			return ciAdmissionReceipt{}, ErrConflict
		}
		chosen = previous.Proof
		if previous.Request != req || phase != "running" || started == nil || assigned == nil ||
			!started.Equal(chosen.StartedAt) {
			return ciAdmissionReceipt{}, ErrConflict
		}
		if proof != nil {
			candidate := *proof
			candidate.StartedAt = candidate.StartedAt.UTC().Truncate(time.Microsecond)
			if candidate != chosen {
				return ciAdmissionReceipt{}, ErrConflict
			}
		}
	} else {
		if proof == nil {
			return ciAdmissionReceipt{}, errCIRunnerJobPending
		}
		if phase != "idle" || assigned != nil || started != nil {
			return ciAdmissionReceipt{}, ErrConflict
		}
		chosen = *proof
		chosen.StartedAt = chosen.StartedAt.UTC().Truncate(time.Microsecond)
	}
	if !ciAdmissionProofMatches(req, scope, chosen, s.now()) {
		return ciAdmissionReceipt{}, ErrConflict
	}
	job, err := s.ciAdoptionJobTx(ctx, tx, workspace, installation, runner, scope.SizeID, chosen, recovering)
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	if recovering && *assigned != job.Job.ID {
		return ciAdmissionReceipt{}, ErrConflict
	}
	quote, err := s.reserveCIBudgetTx(ctx, tx, workspace, runner, chosen.StartedAt, maximumSeconds)
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	now := s.now()
	if !quote.DeadlineAt.After(now) || quote.DeadlineAt.After(now.Add(time.Duration(ciMaxRuntimeSeconds)*time.Second)) {
		return ciAdmissionReceipt{}, ErrConflict
	}
	receipt := ciAdmissionReceipt{ciAdmissionRequest: req, Decision: "allow", ExecutionDeadlineUnixMs: quote.DeadlineAt.UnixMilli()}
	if recovering {
		if previous.Receipt != receipt {
			return ciAdmissionReceipt{}, ErrConflict
		}
		return receipt, nil
	}
	job.Job.RunnerID, job.Job.StartedAt = &runner, &chosen.StartedAt
	job.Job.Status, job.Job.QueueReason, job.Job.Conclusion = "running", "", ""
	job.Job.Repository, job.Job.Size = chosen.Repository, scope.SizeID
	job.Job.CompletedAt = nil
	job.Job.GitHubJobID, job.Job.GitHubRunID = chosen.JobID, chosen.RunID
	job.Job.GitHubURL = fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", chosen.Repository, chosen.RunID, chosen.JobID)
	job.Job.BillableSeconds, job.Job.CostMicros = 0, 0
	jobRaw, err := json.Marshal(job)
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_jobs SET status='running',runner_id=$3,document=$4,updated_at=$5 WHERE workspace_id=$1 AND id=$2`, workspace, job.Job.ID, runner, jobRaw, now); err != nil {
		return ciAdmissionReceipt{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runners SET assigned_job_id=$3,phase='running',updated_at=$4 WHERE workspace_id=$1 AND id=$2`, workspace, runner, job.Job.ID, now); err != nil {
		return ciAdmissionReceipt{}, err
	}
	if trigger != job.Job.ID {
		if _, err = tx.Exec(ctx, `UPDATE ci_jobs SET status='queued',document=jsonb_set(document,'{job,status}','"queued"'::jsonb),updated_at=$3 WHERE workspace_id=$1 AND id=$2 AND status='provisioning' AND runner_id IS NULL`, workspace, trigger, now); err != nil {
			return ciAdmissionReceipt{}, err
		}
	}
	decisionRaw, err = json.Marshal(ciAdmissionDecision{Request: req, Proof: chosen, Receipt: receipt})
	if err != nil {
		return ciAdmissionReceipt{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_runner_admissions SET decision=$3 WHERE workspace_id=$1 AND runner_id=$2 AND decision IS NULL`, workspace, runner, decisionRaw); err != nil {
		return ciAdmissionReceipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ciAdmissionReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ciAdoptionJobTx(ctx context.Context, tx pgx.Tx, workspace, installation, runner, size string, proof ciRunnerJobProof, recovering bool) (ciJobDocument, error) {
	const query = `SELECT id,status,runner_id,document,created_at,github_repository_id,github_run_id FROM ci_jobs WHERE workspace_id=$1 AND installation_id=$2 AND github_job_id=$3 FOR UPDATE`
	var id, status string
	var assigned *string
	var raw []byte
	var created time.Time
	var repoID, runID int64
	scan := func() error {
		return tx.QueryRow(ctx, query, workspace, installation, proof.JobID).Scan(&id, &status, &assigned, &raw, &created, &repoID, &runID)
	}
	err := scan()
	if errors.Is(err, pgx.ErrNoRows) && !recovering {
		// A runner can adopt a different matching job before its webhook
		// arrives. Seed only metadata already established by source proof.
		id, err = newID("cijob_")
		if err != nil {
			return ciJobDocument{}, err
		}
		stub := ciJobDocument{Job: CIJob{ID: id, Repository: proof.Repository, GitHubJobID: proof.JobID, GitHubRunID: proof.RunID, Status: "running", Size: size, CreatedAt: s.now().UTC(), StartedAt: &proof.StartedAt}, Steps: []CIJobStep{}}
		raw, err = json.Marshal(stub)
		if err != nil {
			return ciJobDocument{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'running',$7,$8,$8) ON CONFLICT(installation_id,github_job_id) DO NOTHING`, id, workspace, installation, proof.RepositoryID, proof.JobID, proof.RunID, raw, s.now()); err != nil {
			return ciJobDocument{}, err
		}
		err = scan()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrConflict
	}
	if err != nil {
		return ciJobDocument{}, err
	}
	if repoID != proof.RepositoryID || runID != proof.RunID ||
		(status != "queued" && status != "provisioning" && status != "running") {
		return ciJobDocument{}, ErrConflict
	}
	if recovering {
		if status != "running" || assigned == nil || *assigned != runner {
			return ciJobDocument{}, ErrConflict
		}
	} else if assigned != nil {
		return ciJobDocument{}, ErrConflict
	}
	var doc ciJobDocument
	if err = json.Unmarshal(raw, &doc); err != nil {
		return ciJobDocument{}, err
	}
	doc.Job.ID = id
	if doc.Job.CreatedAt.IsZero() {
		doc.Job.CreatedAt = created
	}
	if doc.Steps == nil {
		doc.Steps = []CIJobStep{}
	}
	return doc, nil
}
