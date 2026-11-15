package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const ciAdmissionRepoName = "financial-tests/repo"

func ciAdmissionJobRequest() ciAdmissionRequest {
	return ciAdmissionRequest{
		SchemaVersion:      1,
		AdmissionID:        ciAdmissionNonce("a"),
		GitHubRunnerID:     ciAdmissionRunnerID,
		GitHubRepositoryID: ciAdmissionRepoID,
		Repository:         ciAdmissionRepoName,
		GitHubRunID:        1001,
		RunAttempt:         1,
		JobKey:             "build",
		RunnerName:         ciAdmissionName,
	}
}

func ciAdmissionJobProof(req ciAdmissionRequest, jobID int64, startedAt time.Time) ciRunnerJobProof {
	return ciRunnerJobProof{
		RepositoryID: req.GitHubRepositoryID,
		Repository:   req.Repository,
		JobID:        jobID,
		RunID:        req.GitHubRunID,
		Attempt:      req.RunAttempt,
		RunnerID:     req.GitHubRunnerID,
		RunnerName:   req.RunnerName,
		StartedAt:    startedAt,
	}
}

// ciAdmissionJobFixture returns a registered runner whose store clock sits
// just after the source-proof start so budget deadlines stay in the future.
func ciAdmissionJobFixture(t *testing.T) (*Store, string, string, time.Time, ciAdmissionRequest, ciRunnerJobProof) {
	t.Helper()
	s, workspace, runner, start, _ := ciAdmissionRegistered(t)
	s.now = func() time.Time { return start.Add(10 * time.Second) }
	req := ciAdmissionJobRequest()
	proof := ciAdmissionJobProof(req, 1001, start.Add(5*time.Second))
	return s, workspace, runner, start, req, proof
}

func ciAdmissionRunnerJobState(t *testing.T, s *Store, workspace, runner string) (assigned *string, started, deadline *time.Time, phase string) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(), `SELECT assigned_job_id,execution_started_at,deadline_at,phase FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&assigned, &started, &deadline, &phase)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func ciAdmissionReservedUnits(t *testing.T, s *Store, workspace, runner string) int64 {
	t.Helper()
	var n int64
	err := s.pool.QueryRow(context.Background(), `SELECT COALESCE(SUM(reserved_units),0) FROM ci_budget_reservations WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ciAdmissionDecisionRaw(t *testing.T, s *Store, workspace, runner string) []byte {
	t.Helper()
	var raw []byte
	err := s.pool.QueryRow(context.Background(), `SELECT decision FROM ci_runner_admissions WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ciAdmissionJobRow(t *testing.T, s *Store, workspace string, githubJobID int64) (id, status string, runnerID *string, doc ciJobDocument) {
	t.Helper()
	var raw []byte
	err := s.pool.QueryRow(context.Background(), `SELECT id,status,runner_id,document FROM ci_jobs WHERE workspace_id=$1 AND github_job_id=$2`, workspace, githubJobID).Scan(&id, &status, &runnerID, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return
}

func ciAdmissionJobCount(t *testing.T, s *Store, workspace string, githubJobID int64) int {
	t.Helper()
	var n int
	err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_jobs WHERE workspace_id=$1 AND github_job_id=$2`, workspace, githubJobID).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ciAdmissionSetJobStatus(t *testing.T, s *Store, workspace, jobID, status string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `UPDATE ci_jobs SET status=$3 WHERE workspace_id=$1 AND id=$2`, workspace, jobID, status)
	if err != nil {
		t.Fatal(err)
	}
}

// ciAdmissionAssertDenied verifies a rejected admission left no clock, no
// budget reservation, no decision, and no runner binding anywhere.
func ciAdmissionAssertDenied(t *testing.T, s *Store, workspace, runner string, wantPhase string, githubJobID int64) {
	t.Helper()
	assigned, started, deadline, phase := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if assigned != nil || started != nil || deadline != nil || phase != wantPhase {
		t.Fatalf("runner mutated by denial: assigned=%v started=%v deadline=%v phase=%s", assigned, started, deadline, phase)
	}
	if n := ciAdmissionReservedUnits(t, s, workspace, runner); n != 0 {
		t.Fatalf("denial reserved %d units", n)
	}
	if raw := ciAdmissionDecisionRaw(t, s, workspace, runner); raw != nil {
		t.Fatalf("denial persisted decision %s", raw)
	}
	_, status, runnerID, _ := ciAdmissionJobRow(t, s, workspace, githubJobID)
	if status != "queued" || runnerID != nil {
		t.Fatalf("job mutated by denial: status=%s runner=%v", status, runnerID)
	}
}

func TestCIAdmissionJobAtomicBudgetAndRecovery(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, req, proof := ciAdmissionJobFixture(t)

	// A nil proof is a read-only recovery attempt: pending, zero writes.
	if _, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200); !errors.Is(err, errCIRunnerJobPending) {
		t.Fatalf("nil proof admission: %v", err)
	}
	ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)

	receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
	if err != nil {
		t.Fatal(err)
	}
	wantStart := start.Add(5 * time.Second)
	wantDeadline := wantStart.Add(7200 * time.Second)
	if receipt.Decision != "allow" || receipt.ciAdmissionRequest != req ||
		!time.UnixMilli(receipt.ExecutionDeadlineUnixMs).Equal(wantDeadline) {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}

	assigned, started, deadline, phase := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if phase != "running" || assigned == nil || *assigned != "ci_financial_job_1" {
		t.Fatalf("runner state after admit: phase=%s assigned=%v", phase, assigned)
	}
	// The stored clock is the source-proof start, not the store wall clock.
	if started == nil || !started.Equal(wantStart) || deadline == nil || !deadline.Equal(wantDeadline) {
		t.Fatalf("clock is not source-derived: started=%v deadline=%v", started, deadline)
	}
	if n := ciAdmissionReservedUnits(t, s, workspace, runner); n <= 0 {
		t.Fatalf("admission reserved no budget: %d", n)
	}
	raw := ciAdmissionDecisionRaw(t, s, workspace, runner)
	if raw == nil {
		t.Fatal("admission persisted no decision")
	}
	var stored ciAdmissionDecision
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Request != req || stored.Receipt != receipt ||
		stored.Proof.JobID != proof.JobID || stored.Proof.RunID != proof.RunID ||
		stored.Proof.RepositoryID != proof.RepositoryID || stored.Proof.RunnerID != proof.RunnerID ||
		stored.Proof.RunnerName != proof.RunnerName || stored.Proof.Attempt != proof.Attempt ||
		stored.Proof.Repository != proof.Repository || !stored.Proof.StartedAt.Equal(proof.StartedAt) {
		t.Fatalf("stored decision mismatch: %+v", stored)
	}
	jobID, status, runnerID, doc := ciAdmissionJobRow(t, s, workspace, 1001)
	if jobID != "ci_financial_job_1" || status != "running" || runnerID == nil || *runnerID != runner {
		t.Fatalf("job binding wrong: id=%s status=%s runner=%v", jobID, status, runnerID)
	}
	if doc.Job.StartedAt == nil || !doc.Job.StartedAt.Equal(wantStart) || doc.Job.Size != ciAdmissionSize {
		t.Fatalf("job document lost source clock or size: %+v", doc.Job)
	}

	// An identical nil-proof retry recovers the committed receipt without
	// duplicating the quote, clock, or decision.
	decisionBefore := ciAdmissionDecisionRaw(t, s, workspace, runner)
	reservedBefore := ciAdmissionReservedUnits(t, s, workspace, runner)
	retry, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200)
	if err != nil {
		t.Fatal(err)
	}
	if retry != receipt {
		t.Fatalf("recovery receipt changed: %+v vs %+v", retry, receipt)
	}
	if got := ciAdmissionReservedUnits(t, s, workspace, runner); got != reservedBefore {
		t.Fatalf("recovery duplicated reservation: %d -> %d", reservedBefore, got)
	}
	if got := ciAdmissionDecisionRaw(t, s, workspace, runner); !bytes.Equal(got, decisionBefore) {
		t.Fatal("recovery rewrote the decision")
	}

	// A supplied proof that disagrees with the committed decision conflicts.
	for name, mutate := range map[string]func(*ciRunnerJobProof){
		"job_id": func(p *ciRunnerJobProof) { p.JobID = 9999 },
		"start":  func(p *ciRunnerJobProof) { p.StartedAt = start.Add(6 * time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := proof
			mutate(&bad)
			if _, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &bad, 7200); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicting proof: %v", err)
			}
		})
	}
}

func TestCIAdmissionJobAdoptionAndTriggerRestore(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
	req := ciAdmissionJobRequest()
	req.GitHubRunID = 2002
	proof := ciAdmissionJobProof(req, 2002, start.Add(5*time.Second))

	// The webhook has not arrived for the actual job; the trigger job is
	// mid-provisioning and must be restored to queued, not lost.
	ciAdmissionSetJobStatus(t, s, workspace, "ci_financial_job_1", "provisioning")
	receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline := proof.StartedAt.Add(7200 * time.Second)
	if receipt.Decision != "allow" || !time.UnixMilli(receipt.ExecutionDeadlineUnixMs).Equal(wantDeadline) {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	if n := ciAdmissionJobCount(t, s, workspace, 2002); n != 1 {
		t.Fatalf("adopted job rows = %d", n)
	}
	adoptedID, status, runnerID, doc := ciAdmissionJobRow(t, s, workspace, 2002)
	if adoptedID == "ci_financial_job_1" || !strings.HasPrefix(adoptedID, "cijob_") {
		t.Fatalf("adopted job id = %s", adoptedID)
	}
	if status != "running" || runnerID == nil || *runnerID != runner {
		t.Fatalf("adopted job binding: status=%s runner=%v", status, runnerID)
	}
	if doc.Job.GitHubJobID != 2002 || doc.Job.GitHubRunID != 2002 || doc.Job.RunnerID == nil || *doc.Job.RunnerID != runner {
		t.Fatalf("adopted job document: %+v", doc.Job)
	}
	triggerID, triggerStatus, triggerRunner, _ := ciAdmissionJobRow(t, s, workspace, 1001)
	if triggerID != "ci_financial_job_1" || triggerStatus != "queued" || triggerRunner != nil {
		t.Fatalf("trigger demand lost: id=%s status=%s runner=%v", triggerID, triggerStatus, triggerRunner)
	}
	assigned, _, _, phase := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if phase != "running" || assigned == nil || *assigned != adoptedID {
		t.Fatalf("runner not bound to adopted job: phase=%s assigned=%v", phase, assigned)
	}
}

func TestCIAdmissionJobConcurrent(t *testing.T) {
	ctx := context.Background()
	t.Run("identical_requests", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		receipts := make([]ciAdmissionReceipt, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				receipts[i], errs[i] = s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent admit %d: %v", i, err)
			}
		}
		if receipts[0] != receipts[1] {
			t.Fatalf("concurrent receipts differ: %+v vs %+v", receipts[0], receipts[1])
		}
		// A sequential admit on a fresh fixture produces the same quote.
		// Capture the concurrent reservation first: building the second
		// fixture truncates the shared test schema.
		concurrentUnits := ciAdmissionReservedUnits(t, s, workspace, runner)
		s2, workspace2, runner2, _, req2, proof2 := ciAdmissionJobFixture(t)
		single, err := s2.admitCIRunnerJob(ctx, workspace2, runner2, req2, &proof2, 7200)
		if err != nil {
			t.Fatal(err)
		}
		if single != receipts[0] {
			t.Fatalf("concurrent receipt %+v != sequential %+v", receipts[0], single)
		}
		if want := ciAdmissionReservedUnits(t, s2, workspace2, runner2); concurrentUnits != want {
			t.Fatalf("concurrent reservation %d != single %d", concurrentUnits, want)
		}
	})
	t.Run("distinct_actual_jobs", func(t *testing.T) {
		s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
		req := ciAdmissionJobRequest()
		req.GitHubRunID = 2002
		proofs := []ciRunnerJobProof{
			ciAdmissionJobProof(req, 2002, start.Add(5*time.Second)),
			ciAdmissionJobProof(req, 2003, start.Add(5*time.Second)),
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range proofs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = s.admitCIRunnerJob(ctx, workspace, runner, req, &proofs[i], 7200)
			}(i)
		}
		wg.Wait()
		successes, conflicts := 0, 0
		for _, err := range errs {
			if err == nil {
				successes++
			} else if errors.Is(err, ErrConflict) {
				conflicts++
			} else {
				t.Fatalf("unexpected concurrent error: %v", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("concurrent admits: %d success %d conflict", successes, conflicts)
		}
		raw := ciAdmissionDecisionRaw(t, s, workspace, runner)
		if raw == nil {
			t.Fatal("no decision persisted")
		}
		var stored ciAdmissionDecision
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		winnerJob, loserJob := stored.Proof.JobID, int64(2002)
		if winnerJob == 2002 {
			loserJob = 2003
		} else if winnerJob != 2003 {
			t.Fatalf("decision bound unexpected job %d", winnerJob)
		}
		if n := ciAdmissionJobCount(t, s, workspace, loserJob); n != 0 {
			t.Fatalf("losing admission left %d job rows", n)
		}
		_, status, runnerID, _ := ciAdmissionJobRow(t, s, workspace, winnerJob)
		if status != "running" || runnerID == nil || *runnerID != runner {
			t.Fatalf("winning job binding: status=%s runner=%v", status, runnerID)
		}
	})
}

func TestCIAdmissionJobDenials(t *testing.T) {
	ctx := context.Background()

	t.Run("pending_without_proof", func(t *testing.T) {
		s, workspace, runner, _, req, _ := ciAdmissionJobFixture(t)
		if _, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200); !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("nil proof: %v", err)
		}
		ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)
	})

	t.Run("foreign_workspace", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		_, other, _, err := s.Signup(ctx, "admission-foreign@example.com", "correct horse battery staple", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.admitCIRunnerJob(ctx, other.ID, runner, req, &proof, 7200); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign workspace admit: %v", err)
		}
		ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)
	})

	cases := []struct {
		name       string
		mutateReq  func(*ciAdmissionRequest)
		mutatePrf  func(*ciRunnerJobProof, time.Time)
		setup      func(t *testing.T, s *Store, workspace, runner string)
		wantErr    error
		wantPhase  string
		withoutPrf bool
	}{
		{"schema_version", func(r *ciAdmissionRequest) { r.SchemaVersion = 2 }, nil, nil, ErrConflict, "idle", false},
		{"bad_nonce_format", func(r *ciAdmissionRequest) { r.AdmissionID = "not-hex" }, nil, nil, ErrConflict, "idle", false},
		{"unknown_nonce", func(r *ciAdmissionRequest) { r.AdmissionID = ciAdmissionNonce("b") }, nil, nil, ErrNotFound, "idle", false},
		{"zero_runner_id", func(r *ciAdmissionRequest) { r.GitHubRunnerID = 0 }, nil, nil, ErrConflict, "idle", false},
		{"bad_repository", func(r *ciAdmissionRequest) { r.Repository = "no-slash" }, nil, nil, ErrConflict, "idle", false},
		{"bad_job_key", func(r *ciAdmissionRequest) { r.JobKey = "has space" }, nil, nil, ErrConflict, "idle", false},
		{"bad_runner_name", func(r *ciAdmissionRequest) { r.RunnerName = "bad name!" }, nil, nil, ErrConflict, "idle", false},
		{"proof_repo", nil, func(p *ciRunnerJobProof, _ time.Time) { p.RepositoryID = 790 }, nil, ErrConflict, "idle", false},
		{"proof_runner", nil, func(p *ciRunnerJobProof, _ time.Time) { p.RunnerID = 32 }, nil, ErrConflict, "idle", false},
		{"proof_name", nil, func(p *ciRunnerJobProof, _ time.Time) { p.RunnerName = "other-runner" }, nil, ErrConflict, "idle", false},
		{"proof_run", nil, func(p *ciRunnerJobProof, _ time.Time) { p.RunID = 1002 }, nil, ErrConflict, "idle", false},
		{"proof_attempt", nil, func(p *ciRunnerJobProof, _ time.Time) { p.Attempt = 2 }, nil, ErrConflict, "idle", false},
		{"proof_start_before_window", nil, func(p *ciRunnerJobProof, start time.Time) { p.StartedAt = start.Add(-2 * time.Minute) }, nil, ErrConflict, "idle", false},
		{"product_disabled", nil, nil, func(t *testing.T, s *Store, w, r string) { ciAdmissionProductOff(t, s, w) }, ErrConflict, "idle", false},
		{"repo_disabled", nil, nil, func(t *testing.T, s *Store, w, r string) { ciAdmissionRepoEnabled(t, s, w, false) }, ErrNotFound, "idle", false},
		{"installation_suspended", nil, nil, func(t *testing.T, s *Store, w, r string) { ciAdmissionInstallationStatus(t, s, w, "suspended") }, ErrNotFound, "idle", false},
		{"delete_requested", nil, nil, func(t *testing.T, s *Store, w, r string) { ciAdmissionPhase(t, s, w, r, "idle", true) }, ErrNotFound, "idle", false},
		{"phase_deleted", nil, nil, func(t *testing.T, s *Store, w, r string) { ciAdmissionPhase(t, s, w, r, "deleted", false) }, ErrNotFound, "deleted", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, workspace, runner, start, req, proof := ciAdmissionJobFixture(t)
			if tc.mutateReq != nil {
				tc.mutateReq(&req)
			}
			if tc.mutatePrf != nil {
				tc.mutatePrf(&proof, start)
			}
			if tc.setup != nil {
				tc.setup(t, s, workspace, runner)
			}
			var proofPtr *ciRunnerJobProof
			if !tc.withoutPrf {
				proofPtr = &proof
			}
			_, err := s.admitCIRunnerJob(ctx, workspace, runner, req, proofPtr, 7200)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			ciAdmissionAssertDenied(t, s, workspace, runner, tc.wantPhase, 1001)
		})
	}
}

func TestCIAdmissionJobTerminalRecoveryDenials(t *testing.T) {
	ctx := context.Background()
	admit := func(t *testing.T) (*Store, string, string, ciAdmissionRequest) {
		t.Helper()
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		if _, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200); err != nil {
			t.Fatal(err)
		}
		return s, workspace, runner, req
	}
	t.Run("job_cancelled", func(t *testing.T) {
		s, workspace, runner, req := admit(t)
		ciAdmissionSetJobStatus(t, s, workspace, "ci_financial_job_1", "cancelled")
		receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200)
		if !errors.Is(err, ErrConflict) || receipt != (ciAdmissionReceipt{}) {
			t.Fatalf("cancelled job recovery: receipt=%+v err=%v", receipt, err)
		}
	})
	t.Run("job_completed", func(t *testing.T) {
		s, workspace, runner, req := admit(t)
		ciAdmissionSetJobStatus(t, s, workspace, "ci_financial_job_1", "completed")
		receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200)
		if !errors.Is(err, ErrConflict) || receipt != (ciAdmissionReceipt{}) {
			t.Fatalf("completed job recovery: receipt=%+v err=%v", receipt, err)
		}
	})
	t.Run("delete_requested_after_decision", func(t *testing.T) {
		s, workspace, runner, req := admit(t)
		ciAdmissionPhase(t, s, workspace, runner, "running", true)
		receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200)
		if !errors.Is(err, ErrNotFound) || receipt != (ciAdmissionReceipt{}) {
			t.Fatalf("delete-requested recovery: receipt=%+v err=%v", receipt, err)
		}
	})
}

func TestCIAdmissionJobAtomicRollback(t *testing.T) {
	ctx := context.Background()
	rejectDecision := func(t *testing.T, s *Store) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, `ALTER TABLE ci_runner_admissions ADD CONSTRAINT ci_test_reject_decision CHECK (decision IS NULL)`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := s.pool.Exec(context.Background(), `ALTER TABLE ci_runner_admissions DROP CONSTRAINT ci_test_reject_decision`); err != nil {
				t.Errorf("drop ci_test_reject_decision: %v", err)
			}
		})
	}
	assertCheckViolation := func(t *testing.T, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("expected decision check violation, got %v", err)
		}
	}
	t.Run("existing_job", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		rejectDecision(t, s)
		_, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
		assertCheckViolation(t, err)
		ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)
	})
	t.Run("adopted_stub", func(t *testing.T) {
		s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
		rejectDecision(t, s)
		req := ciAdmissionJobRequest()
		req.GitHubRunID = 2002
		proof := ciAdmissionJobProof(req, 2002, start.Add(5*time.Second))
		_, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
		assertCheckViolation(t, err)
		// The stub insert, job binding, runner clock, and reservation all
		// rolled back with the rejected decision.
		if n := ciAdmissionJobCount(t, s, workspace, 2002); n != 0 {
			t.Fatalf("rolled-back admission left %d stub jobs", n)
		}
		ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)
	})
}

func ciAdmissionSourceJobJSON(id int64, startedAt time.Time, mutate func(map[string]any)) string {
	job := map[string]any{
		"id": id, "run_id": 1001, "run_attempt": 1,
		"runner_id": ciAdmissionRunnerID, "runner_name": ciAdmissionName,
		"status": "in_progress", "started_at": startedAt.Format(time.RFC3339),
		"completed_at": nil, "conclusion": nil,
		"labels": []any{"canter-2vcpu-ubuntu-2404"},
	}
	if mutate != nil {
		mutate(job)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// ciAdmissionSourceSetup wires a registered runner to a mock GitHub App
// transport that mints a repo-scoped token and serves one jobs page.
func ciAdmissionSourceSetup(t *testing.T, jobJSON string) (*ciSourceFixture, *Store, string, string, time.Time, ciAdmissionRequest) {
	t.Helper()
	s, workspace, runner, start, _ := ciAdmissionRegistered(t)
	s.now = func() time.Time { return start.Add(10 * time.Second) }
	req := ciAdmissionJobRequest()
	page := `{"total_count":1,"jobs":[` + jobJSON + `]}`
	f := newCISourceFixture(t)
	f.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/123/access_tokens":
			fmt.Fprint(w, `{"token":"source-token-admission","expires_at":"2099-01-01T00:00:00Z"}`)
		case r.URL.Path == "/repositories/789":
			fmt.Fprint(w, `{"id":789,"full_name":"financial-tests/repo","private":true}`)
		case strings.HasPrefix(r.URL.Path, "/repos/financial-tests/repo/actions/runs/1001/attempts/1/jobs"):
			fmt.Fprint(w, page)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
	return f, s, workspace, runner, start, req
}

func TestCIAdmissionSourceRecoveryAndFirstProof(t *testing.T) {
	ctx := context.Background()

	t.Run("recovery_mints_nothing", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		first, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
		if err != nil {
			t.Fatal(err)
		}
		f := newCISourceFixture(t)
		again, err := f.client.admitRunnerJob(ctx, s, workspace, runner, req, 3600)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("recovery receipt changed: %+v vs %+v", again, first)
		}
		if f.count() != 0 {
			t.Fatalf("committed recovery made %d GitHub requests", f.count())
		}
	})

	t.Run("first_proof_binds_actual_job", func(t *testing.T) {
		started := time.Date(2026, time.November, 30, 23, 0, 3, 0, time.UTC)
		f, s, workspace, runner, _, req := ciAdmissionSourceSetup(t,
			ciAdmissionSourceJobJSON(5001, started, nil))
		receipt, err := f.client.admitRunnerJob(ctx, s, workspace, runner, req, 7200)
		if err != nil {
			t.Fatal(err)
		}
		wantDeadline := started.Add(7200 * time.Second)
		if receipt.Decision != "allow" || !time.UnixMilli(receipt.ExecutionDeadlineUnixMs).Equal(wantDeadline) {
			t.Fatalf("unexpected receipt: %+v", receipt)
		}
		if f.count() != 3 {
			t.Fatalf("requests = %d", f.count())
		}
		tokenReq := f.request(0)
		if tokenReq.Method != http.MethodPost || tokenReq.Path != "/app/installations/123/access_tokens" {
			t.Fatalf("token request = %s %s", tokenReq.Method, tokenReq.Path)
		}
		if !strings.HasPrefix(tokenReq.Authorization, "Bearer eyJ") {
			t.Fatalf("token request is not an App JWT: %q", tokenReq.Authorization)
		}
		wantBody := `{"repository_ids":[789],"permissions":{"actions":"read","metadata":"read"}}`
		if tokenReq.Body != wantBody {
			t.Fatalf("token body = %s", tokenReq.Body)
		}
		for i := 1; i < 3; i++ {
			if got := f.request(i).Authorization; got != "Bearer source-token-admission" {
				t.Fatalf("request %d used non-repo-scoped auth: %q", i, got)
			}
		}
		// The GitHub assignment id (5001) differs from the trigger job id
		// (1001): a new stub is bound and the trigger stays queued.
		if n := ciAdmissionJobCount(t, s, workspace, 5001); n != 1 {
			t.Fatalf("adopted stub rows = %d", n)
		}
		jobID, status, runnerID, doc := ciAdmissionJobRow(t, s, workspace, 5001)
		if !strings.HasPrefix(jobID, "cijob_") || status != "running" || runnerID == nil || *runnerID != runner {
			t.Fatalf("adopted stub binding: id=%s status=%s runner=%v", jobID, status, runnerID)
		}
		if doc.Job.GitHubJobID != 5001 || doc.Job.RunnerID == nil || *doc.Job.RunnerID != runner {
			t.Fatalf("adopted stub document: %+v", doc.Job)
		}
		_, triggerStatus, triggerRunner, _ := ciAdmissionJobRow(t, s, workspace, 1001)
		if triggerStatus != "queued" || triggerRunner != nil {
			t.Fatalf("trigger demand lost: status=%s runner=%v", triggerStatus, triggerRunner)
		}
		assigned, execStart, _, phase := ciAdmissionRunnerJobState(t, s, workspace, runner)
		if phase != "running" || assigned == nil || execStart == nil || !execStart.Equal(started) {
			t.Fatalf("runner state: phase=%s assigned=%v started=%v", phase, assigned, execStart)
		}
	})

	t.Run("proof_denied", func(t *testing.T) {
		started := time.Date(2026, time.November, 30, 23, 0, 3, 0, time.UTC)
		for name, jobJSON := range map[string]string{
			"runner_name_mismatch": ciAdmissionSourceJobJSON(5001, started, func(j map[string]any) {
				j["runner_name"] = "other-runner"
			}),
			"job_still_unassigned": ciAdmissionSourceJobJSON(5001, started, func(j map[string]any) {
				j["runner_id"] = nil
				j["status"] = "queued"
				j["started_at"] = nil
			}),
		} {
			t.Run(name, func(t *testing.T) {
				f, s, workspace, runner, _, req := ciAdmissionSourceSetup(t, jobJSON)
				receipt, err := f.client.admitRunnerJob(ctx, s, workspace, runner, req, 7200)
				if receipt != (ciAdmissionReceipt{}) {
					t.Fatalf("denied admission returned receipt %+v", receipt)
				}
				if name == "job_still_unassigned" {
					if !errors.Is(err, errCIRunnerJobPending) {
						t.Fatalf("expected pending, got %v", err)
					}
				} else if !errors.Is(err, ErrConflict) {
					t.Fatalf("expected conflict, got %v", err)
				}
				ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)
			})
		}
	})
}

func TestCIAdmissionJobWebhookMerge(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, req, proof := ciAdmissionJobFixture(t)
	if _, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200); err != nil {
		t.Fatal(err)
	}
	// A later in_progress webhook must merge onto the bound job without
	// losing the admission source clock, size, or runner identity, and must
	// not print the unmanaged-runner queue warning.
	webhookStart := start.Add(7 * time.Second)
	startedText := webhookStart.Format(time.RFC3339)
	payload := &ciWorkflowJobPayload{
		ID: 1001, RunID: 1001, Name: "build", Status: "in_progress",
		StartedAt: &startedText, Labels: []string{"canter-2vcpu-ubuntu-2404"},
	}
	webhookJob := CIJob{
		Repository:  ciAdmissionRepoName,
		Name:        "build",
		GitHubJobID: 1001, GitHubRunID: 1001,
		GitHubURL:   "https://github.com/financial-tests/repo/actions/runs/1001/job/1001",
		Status:      "running",
		Size:        "linux-x64-4",
		CreatedAt:   time.Now().UTC(),
		StartedAt:   &webhookStart,
		QueueReason: "Reported by GitHub but not running on a Canter-managed runner; it is not billed.",
	}
	h := &HTTPServer{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ciUpsertJob(ctx, tx, workspace, "ci_financial_installation", ciAdmissionRepoID, payload, webhookJob, []CIJobStep{}, "running"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, status, runnerID, doc := ciAdmissionJobRow(t, s, workspace, 1001)
	if status != "running" || runnerID == nil || *runnerID != runner {
		t.Fatalf("merge lost runner binding: status=%s runner=%v", status, runnerID)
	}
	if doc.Job.RunnerID == nil || *doc.Job.RunnerID != runner {
		t.Fatalf("merge document runner = %v", doc.Job.RunnerID)
	}
	wantStart := start.Add(5 * time.Second)
	if doc.Job.StartedAt == nil || !doc.Job.StartedAt.Equal(wantStart) {
		t.Fatalf("merge replaced source clock: %v", doc.Job.StartedAt)
	}
	if doc.Job.Size != ciAdmissionSize {
		t.Fatalf("merge replaced admitted size: %q", doc.Job.Size)
	}
	if doc.Job.QueueReason != "" {
		t.Fatalf("merge printed unmanaged warning: %q", doc.Job.QueueReason)
	}
}

func TestCIAdmissionProtocolDecode(t *testing.T) {
	nonce := ciAdmissionNonce("a")
	valid := fmt.Sprintf(`{"schemaVersion":1,"admissionId":"%s","githubRunnerId":31,"githubRepositoryId":789,"repository":"financial-tests/repo","githubRunId":1001,"runAttempt":1,"jobKey":"build","runnerName":"canter-test-1"}`, nonce)
	req, err := decodeCIAdmissionRequest([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	want := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: nonce, GitHubRunnerID: 31,
		GitHubRepositoryID: 789, Repository: ciAdmissionRepoName,
		GitHubRunID: 1001, RunAttempt: 1, JobKey: "build", RunnerName: ciAdmissionName,
	}
	if req != want {
		t.Fatalf("decoded request = %+v", req)
	}
	// The receipt is exactly the nine request fields plus decision and
	// executionDeadlineUnixMs.
	receipt := ciAdmissionReceipt{ciAdmissionRequest: req, Decision: "allow", ExecutionDeadlineUnixMs: 123456789}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var receiptFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &receiptFields); err != nil {
		t.Fatal(err)
	}
	if len(receiptFields) != 11 {
		t.Fatalf("receipt fields = %d", len(receiptFields))
	}
	for _, key := range []string{
		"schemaVersion", "admissionId", "githubRunnerId", "githubRepositoryId",
		"repository", "githubRunId", "runAttempt", "jobKey", "runnerName",
		"decision", "executionDeadlineUnixMs",
	} {
		if _, ok := receiptFields[key]; !ok {
			t.Fatalf("receipt missing key %s", key)
		}
	}

	replace := func(old, new string) string { return strings.Replace(valid, old, new, 1) }
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"null_doc", "null"},
		{"array_doc", "[]"},
		{"string_doc", `"x"`},
		{"number_doc", "5"},
		{"bool_doc", "true"},
		{"empty_object", "{}"},
		{"trailing_object", valid + " {}"},
		{"trailing_number", valid + " 5"},
		{"trailing_garbage", valid + " x"},
		{"duplicate_key", valid[:len(valid)-1] + `,"schemaVersion":1}`},
		{"unknown_key", valid[:len(valid)-1] + `,"extra":1}`},
		{"case_key", replace(`"schemaVersion"`, `"SchemaVersion"`)},
		{"missing_key", strings.Replace(valid, `,"runnerName":"canter-test-1"`, ``, 1)},
		{"null_string", replace(`"jobKey":"build"`, `"jobKey":null`)},
		{"null_int", replace(`"githubRunId":1001`, `"githubRunId":null`)},
		{"bool_int", replace(`"githubRunId":1001`, `"githubRunId":true`)},
		{"bool_schema", replace(`"schemaVersion":1`, `"schemaVersion":true`)},
		{"string_int", replace(`"githubRunId":1001`, `"githubRunId":"1001"`)},
		{"float_int", replace(`"githubRunId":1001`, `"githubRunId":1001.0`)},
		{"fraction", replace(`"runAttempt":1`, `"runAttempt":1.5`)},
		{"exponent", replace(`"githubRunnerId":31`, `"githubRunnerId":3.1e1`)},
		{"overflow", replace(`"githubRunnerId":31`, `"githubRunnerId":9223372036854775808`)},
		{"negative_id", replace(`"githubRunnerId":31`, `"githubRunnerId":-5`)},
		{"zero_id", replace(`"githubRunnerId":31`, `"githubRunnerId":0`)},
		{"schema_v2", replace(`"schemaVersion":1`, `"schemaVersion":2`)},
		{"schema_string", replace(`"schemaVersion":1`, `"schemaVersion":"1"`)},
		{"schema_32bit_alias", replace(`"schemaVersion":1`, `"schemaVersion":4294967297`)},
		{"attempt_zero", replace(`"runAttempt":1`, `"runAttempt":0`)},
		{"bad_nonce_len", replace(nonce, strings.Repeat("a", 63))},
		{"bad_nonce_case", replace(nonce, strings.Repeat("A", 64))},
		{"bad_nonce_hex", replace(nonce, strings.Repeat("g", 64))},
		{"bad_name", replace(`"runnerName":"canter-test-1"`, `"runnerName":"bad name!"`)},
		{"bad_jobkey_space", replace(`"jobKey":"build"`, `"jobKey":"has space"`)},
		{"bad_jobkey_digit", replace(`"jobKey":"build"`, `"jobKey":"1build"`)},
		{"bad_repo_noslash", replace(`"repository":"financial-tests/repo"`, `"repository":"repo"`)},
		{"bad_repo_space", replace(`"repository":"financial-tests/repo"`, `"repository":"bad repo/x"`)},
		{"repo_object", replace(`"repository":"financial-tests/repo"`, `"repository":{}`)},
		{"jobkey_array", replace(`"jobKey":"build"`, `"jobKey":[]`)},
		{"name_number", replace(`"runnerName":"canter-test-1"`, `"runnerName":5`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeCIAdmissionRequest([]byte(tc.body)); !errors.Is(err, errCIAdmissionRequestInvalid) {
				t.Fatalf("body %q: err = %v, want invalid", tc.body, err)
			}
		})
	}
	// The byte bound is inclusive: exactly 16384 bytes still decodes.
	padded := strings.Repeat(" ", ciAdmissionRequestMaxBytes-len(valid)) + valid
	if _, err := decodeCIAdmissionRequest([]byte(padded)); err != nil {
		t.Fatalf("max-size body rejected: %v", err)
	}
	if _, err := decodeCIAdmissionRequest([]byte(padded + " ")); !errors.Is(err, errCIAdmissionRequestInvalid) {
		t.Fatal("16385-byte body accepted")
	}
}
