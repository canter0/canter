package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ciJobLogSeed inserts one job plus its bound runner using local fixture SQL.
// The runner is always assigned to the job and may or may not have started
// execution; status, assignment, and update time are caller-controlled so
// eligibility predicates can be exercised one at a time.
func ciJobLogSeed(t *testing.T, s *Store, workspace string, n int, status string, started, assigned bool, updatedAt time.Time) (jobID, runnerID string, ghJob, ghRun int64) {
	t.Helper()
	jobID = fmt.Sprintf("ci_logjob_%d", n)
	runnerID = fmt.Sprintf("ci_logrunner_%d", n)
	ghJob = int64(91000 + n)
	ghRun = int64(92000 + n)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document,updated_at) VALUES($1,$2,'ci_financial_installation',789,$3,$4,$5,'{}'::jsonb,$6)`,
		jobID, workspace, ghJob, ghRun, status, updatedAt); err != nil {
		t.Fatal(err)
	}
	var execStart, assignedJob any
	if started {
		execStart = s.now()
	}
	if assigned {
		assignedJob = jobID
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,assigned_job_id,phase,document,rate_version,rate_micros_per_minute,execution_started_at,created_at) VALUES($1,$2,'ci_financial_installation','linux-x64-2',$3,$4,'deleted','{}'::jsonb,$5,2000,$6,$7)`,
		runnerID, workspace, jobID, assignedJob, pricing.CIRateVersionV1, execStart, s.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET runner_id=$3 WHERE workspace_id=$1 AND id=$2`, workspace, jobID, runnerID); err != nil {
		t.Fatal(err)
	}
	return jobID, runnerID, ghJob, ghRun
}

func ciJobLogFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, workspace, _ := ciFinancialFixture(t)
	ciAdmissionEnable(t, s, workspace)
	return s, workspace
}

func ciJobLogNonce(t *testing.T) string {
	t.Helper()
	n, err := ciEnforcerNewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ciJobLogCandidatesSet(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	items, err := s.ciJobLogCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, item := range items {
		out[item.Job] = true
	}
	return out
}

func ciJobLogChunks(t *testing.T, s *Store, workspace, job string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `SELECT sequence,stream,content FROM ci_job_logs WHERE workspace_id=$1 AND job_id=$2 ORDER BY sequence`, workspace, job)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seq int64
		var stream, content string
		if err := rows.Scan(&seq, &stream, &content); err != nil {
			t.Fatal(err)
		}
		if stream != "stdout" {
			continue
		}
		out = append(out, content)
	}
	return out
}

func ciJobLogRowState(t *testing.T, s *Store, workspace, job string) (state, nonce string, lease, next time.Time, truncated bool, collected *time.Time) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(), `SELECT state,owner_nonce,lease_until,next_attempt_at,truncated,collected_at FROM ci_job_log_collections WHERE workspace_id=$1 AND job_id=$2`, workspace, job).
		Scan(&state, &nonce, &lease, &next, &truncated, &collected)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return state, nonce, lease, next, truncated, collected
}

func TestCIJobLogCandidatesEligibility(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	now := s.now()
	// Exactly two eligible jobs: candidates are limited to ciJobLogWorkers
	// rows per poll ordered by updated_at, so the eligible set must stay
	// within that bound for an exact assertion.
	nearWindow, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now.Add(-6*24*time.Hour))
	eligibleCancelled, _, _, _ := ciJobLogSeed(t, s, workspace, 2, "cancelled", true, true, now)
	// Negative fixtures sit earlier than both eligible rows but inside the
	// backfill window: candidates order by updated_at ASC with LIMIT 2, so a
	// wrongly-eligible negative would take a slot ahead of a legit job.
	negativeAt := now.Add(-6*24*time.Hour - time.Hour)
	ciJobLogSeed(t, s, workspace, 3, "queued", true, true, negativeAt)
	ciJobLogSeed(t, s, workspace, 4, "running", true, true, negativeAt)
	ciJobLogSeed(t, s, workspace, 5, "completed", false, true, negativeAt)
	ciJobLogSeed(t, s, workspace, 6, "completed", true, false, negativeAt)
	oldJob, _, _, _ := ciJobLogSeed(t, s, workspace, 7, "completed", true, true, now.Add(-8*24*time.Hour))

	got := ciJobLogCandidatesSet(t, s)
	want := map[string]bool{nearWindow: true, eligibleCancelled: true}
	if len(got) != len(want) {
		t.Fatalf("candidates: %#v want %#v", got, want)
	}
	for job := range want {
		if !got[job] {
			t.Fatalf("eligible job %s missing from candidates %#v", job, got)
		}
	}
	for _, excluded := range []string{oldJob} {
		if got[excluded] {
			t.Fatalf("job outside backfill window must not be a candidate: %s", excluded)
		}
	}

	// Repository disable removes every candidate.
	if _, err := s.pool.Exec(context.Background(), `UPDATE ci_repositories SET enabled=false WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if got := ciJobLogCandidatesSet(t, s); len(got) != 0 {
		t.Fatalf("disabled repository must yield no candidates: %#v", got)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE ci_repositories SET enabled=true WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	// Suspended installation removes every candidate.
	if _, err := s.pool.Exec(context.Background(), `UPDATE ci_github_installations SET status='suspended' WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if got := ciJobLogCandidatesSet(t, s); len(got) != 0 {
		t.Fatalf("suspended installation must yield no candidates: %#v", got)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE ci_github_installations SET status='active' WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	// Missing CI product removes every candidate.
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, workspace); err != nil {
		t.Fatal(err)
	}
	if got := ciJobLogCandidatesSet(t, s); len(got) != 0 {
		t.Fatalf("missing CI product must yield no candidates: %#v", got)
	}
}

func TestCIJobLogClaimScopeAndPublish(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, runnerID, ghJob, ghRun := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)

	items, err := s.ciJobLogCandidates(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("candidates: %v %#v", err, items)
	}
	nonce := ciJobLogNonce(t)
	claim, err := s.claimCIJobLog(ctx, workspace, jobID, nonce)
	if err != nil {
		t.Fatal(err)
	}
	want := ciJobLogScope{Workspace: workspace, Job: jobID, Runner: runnerID, GitHubInstallationID: 123, GitHubRepositoryID: 789, GitHubJobID: ghJob, GitHubRunID: ghRun}
	if claim.Scope != want {
		t.Fatalf("scope: %#v want %#v", claim.Scope, want)
	}
	if claim.Nonce != nonce || !claim.LeaseUntil.After(now) {
		t.Fatalf("claim fencing: %#v", claim)
	}

	// Invalid opaque IDs and nonces are rejected before any database work.
	for _, bad := range [][2]string{{"", jobID}, {workspace, ""}, {"bad id!", jobID}} {
		if _, err := s.claimCIJobLog(ctx, bad[0], bad[1], nonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("bad ids %v: %v", bad, err)
		}
	}
	for _, badNonce := range []string{"", "xyz", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if _, err := s.claimCIJobLog(ctx, workspace, jobID, badNonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("bad nonce %q: %v", badNonce, err)
		}
	}
	// Ineligible and unknown jobs are not found.
	queued, _, _, _ := ciJobLogSeed(t, s, workspace, 2, "queued", true, true, now)
	if _, err := s.claimCIJobLog(ctx, workspace, queued, ciJobLogNonce(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("queued job claim: %v", err)
	}
	unstarted, _, _, _ := ciJobLogSeed(t, s, workspace, 3, "completed", false, true, now)
	if _, err := s.claimCIJobLog(ctx, workspace, unstarted, ciJobLogNonce(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unstarted job claim: %v", err)
	}
	unassigned, _, _, _ := ciJobLogSeed(t, s, workspace, 4, "completed", true, false, now)
	if _, err := s.claimCIJobLog(ctx, workspace, unassigned, ciJobLogNonce(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unassigned job claim: %v", err)
	}
	if _, err := s.claimCIJobLog(ctx, workspace, "ci_logjob_missing", ciJobLogNonce(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job claim: %v", err)
	}
	// The live lease blocks a second claimant.
	if _, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("leased job reclaim: %v", err)
	}

	// Publish appends chunks and completes atomically.
	if err := s.publishCIJobLog(ctx, claim, "first line\nsecond line\n", false); err != nil {
		t.Fatal(err)
	}
	chunks := ciJobLogChunks(t, s, workspace, jobID)
	if len(chunks) != 1 || chunks[0] != "first line\nsecond line\n" {
		t.Fatalf("chunks: %#v", chunks)
	}
	state, rowNonce, _, _, truncated, collected := ciJobLogRowState(t, s, workspace, jobID)
	if state != "complete" || rowNonce != "" || truncated || collected == nil {
		t.Fatalf("collection row: %s nonce=%q truncated=%v collected=%v", state, rowNonce, truncated, collected)
	}
	view, err := s.CIJobLogCollection(ctx, workspace, jobID)
	if err != nil || view == nil || view.State != "complete" || view.Truncated || view.CollectedAt == nil {
		t.Fatalf("collection view: %v %#v", err, view)
	}
	// Re-publishing the same claim can never duplicate chunks.
	if err := s.publishCIJobLog(ctx, claim, "duplicate", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat publish: %v", err)
	}
	if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 1 {
		t.Fatalf("repeat publish duplicated chunks: %#v", chunks)
	}
	// Legacy jobs without a collection row return nil, not a fabricated state.
	if view, err := s.CIJobLogCollection(ctx, workspace, queued); err != nil || view != nil {
		t.Fatalf("legacy collection must be nil: %v %#v", err, view)
	}
}

func TestCIJobLogPublishEmptyAndTruncated(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()

	// An empty genuine log completes with zero chunks.
	emptyJob, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)
	claim, err := s.claimCIJobLog(ctx, workspace, emptyJob, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publishCIJobLog(ctx, claim, "", false); err != nil {
		t.Fatal(err)
	}
	if chunks := ciJobLogChunks(t, s, workspace, emptyJob); len(chunks) != 0 {
		t.Fatalf("empty log must have zero chunks: %#v", chunks)
	}
	state, _, _, _, _, collected := ciJobLogRowState(t, s, workspace, emptyJob)
	if state != "complete" || collected == nil {
		t.Fatalf("empty log must complete honestly: %s", state)
	}

	// A 1 MiB body sets truncation honestly and stays within chunk bounds.
	bigJob, _, _, _ := ciJobLogSeed(t, s, workspace, 2, "completed", true, true, now)
	bigClaim, err := s.claimCIJobLog(ctx, workspace, bigJob, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("z", ciJobLogMaxBytes)
	if err := s.publishCIJobLog(ctx, bigClaim, body, true); err != nil {
		t.Fatal(err)
	}
	chunks := ciJobLogChunks(t, s, workspace, bigJob)
	if len(chunks) != ciJobLogMaxBytes/ciJobLogChunkBytes {
		t.Fatalf("1MiB must split into %d chunks, got %d", ciJobLogMaxBytes/ciJobLogChunkBytes, len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) > ciJobLogChunkBytes {
			t.Fatalf("chunk exceeds 16KiB: %d", len(chunk))
		}
	}
	_, _, _, _, truncated, _ := ciJobLogRowState(t, s, workspace, bigJob)
	if !truncated {
		t.Fatal("1MiB truncated body must record truncated=true")
	}

	// Invalid input is rejected before any write.
	badJob, _, _, _ := ciJobLogSeed(t, s, workspace, 3, "completed", true, true, now)
	badClaim, err := s.claimCIJobLog(ctx, workspace, badJob, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"has\x00nul", string([]byte{0xff, 0xfe, 'x'}), strings.Repeat("y", ciJobLogMaxBytes+1)} {
		if err := s.publishCIJobLog(ctx, badClaim, bad, false); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid publish input must conflict: %q %.40q", bad[:min(len(bad), 8)], bad)
		}
	}
	if chunks := ciJobLogChunks(t, s, workspace, badJob); len(chunks) != 0 {
		t.Fatalf("invalid input must write nothing: %#v", chunks)
	}
}

func TestCIJobLogConcurrentClaimAndStaleOwner(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)
	nonceA := ciJobLogNonce(t)
	nonceB := ciJobLogNonce(t)

	// Two concurrent claimants: exactly one wins.
	var wg sync.WaitGroup
	results := make([]error, 2)
	claims := make([]ciJobLogClaim, 2)
	for i, nonce := range []string{nonceA, nonceB} {
		wg.Add(1)
		go func(i int, nonce string) {
			defer wg.Done()
			claims[i], results[i] = s.claimCIJobLog(ctx, workspace, jobID, nonce)
		}(i, nonce)
	}
	wg.Wait()
	winners := 0
	winnerIdx := -1
	for i, err := range results {
		if err == nil {
			winners++
			winnerIdx = i
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one claimant must win, got %d: %v", winners, results)
	}
	winner := claims[winnerIdx]
	loserNonce := []string{nonceA, nonceB}[1-winnerIdx]

	// Expire the winner's lease and let the loser reclaim with a new nonce.
	if _, err := s.pool.Exec(ctx, `UPDATE ci_job_log_collections SET lease_until=$3,next_attempt_at=$3 WHERE workspace_id=$1 AND job_id=$2`, workspace, jobID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.claimCIJobLog(ctx, workspace, jobID, loserNonce)
	if err != nil {
		t.Fatalf("expired lease must be reclaimable: %v", err)
	}
	if reclaimed.Nonce == winner.Nonce {
		t.Fatal("reclaim must install the new nonce")
	}

	// The stale owner can neither publish nor fail the new owner's attempt.
	if err := s.publishCIJobLog(ctx, winner, "stale output", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale publish: %v", err)
	}
	if err := s.failCIJobLog(ctx, winner); err != nil {
		t.Fatalf("stale fail should be a no-op success: %v", err)
	}
	state, rowNonce, _, _, _, _ := ciJobLogRowState(t, s, workspace, jobID)
	if state != "collecting" || rowNonce != loserNonce {
		t.Fatalf("stale owner must not affect new owner: %s nonce=%q", state, rowNonce)
	}
	if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 0 {
		t.Fatalf("stale owner must not append: %#v", chunks)
	}
	if err := s.publishCIJobLog(ctx, reclaimed, "fresh output\n", false); err != nil {
		t.Fatal(err)
	}
	chunks := ciJobLogChunks(t, s, workspace, jobID)
	if len(chunks) != 1 || chunks[0] != "fresh output\n" {
		t.Fatalf("new owner publish: %#v", chunks)
	}
}

func TestCIJobLogUnavailableRetryAndExistingChunks(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)

	// Pre-existing system chunks must be preserved and never overwritten.
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_job_logs(workspace_id,job_id,sequence,stream,content,created_at) VALUES($1,$2,0,'system','system event',$3)`, workspace, jobID, now); err != nil {
		t.Fatal(err)
	}
	claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.failCIJobLog(ctx, claim); err != nil {
		t.Fatal(err)
	}
	state, nonce, _, next, _, collected := ciJobLogRowState(t, s, workspace, jobID)
	if state != "unavailable" || nonce != "" || collected != nil {
		t.Fatalf("unavailable row: %s nonce=%q collected=%v", state, nonce, collected)
	}
	if next.Before(now.Add(4*time.Minute)) || next.After(now.Add(6*time.Minute)) {
		t.Fatalf("retry must be ~5m out, got %v (now %v)", next, now)
	}
	// Not a candidate until the retry time passes.
	if got := ciJobLogCandidatesSet(t, s); got[jobID] {
		t.Fatal("unavailable job must not be a candidate before retry time")
	}
	// Advance past the retry window: the same job becomes claimable again.
	s.now = func() time.Time { return now.Add(6 * time.Minute) }
	if got := ciJobLogCandidatesSet(t, s); !got[jobID] {
		t.Fatal("unavailable job must be a candidate after retry window")
	}
	reclaim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatalf("reclaim after retry: %v", err)
	}
	if err := s.publishCIJobLog(ctx, reclaim, "recovered\n", false); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT sequence,stream,content FROM ci_job_logs WHERE workspace_id=$1 AND job_id=$2 ORDER BY sequence`, workspace, jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seqs []int64
	var streams []string
	for rows.Next() {
		var seq int64
		var stream, content string
		if err := rows.Scan(&seq, &stream, &content); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
		streams = append(streams, stream)
		if stream == "system" && content != "system event" {
			t.Fatal("existing system chunk must be preserved verbatim")
		}
	}
	if len(seqs) != 2 || seqs[0] != 0 || streams[0] != "system" || seqs[1] != 1 || streams[1] != "stdout" {
		t.Fatalf("chunks must preserve prior logs and continue sequence: %v %v", seqs, streams)
	}
}

func TestCIJobLogScopeChangeBlocksPublish(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()

	// Job-level scope drift: GitHub IDs and runner assignment are re-read.
	for i, mutate := range []func(t *testing.T, s *Store, workspace, job, runner string){
		func(t *testing.T, s *Store, workspace, job, runner string) {
			if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET github_job_id=99999 WHERE workspace_id=$1 AND id=$2`, workspace, job); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, s *Store, workspace, job, runner string) {
			if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET assigned_job_id=NULL WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
				t.Fatal(err)
			}
		},
		func(t *testing.T, s *Store, workspace, job, runner string) {
			if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET status='running' WHERE workspace_id=$1 AND id=$2`, workspace, job); err != nil {
				t.Fatal(err)
			}
		},
	} {
		jobID, runnerID, _, _ := ciJobLogSeed(t, s, workspace, 10+i, "completed", true, true, now)
		claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
		if err != nil {
			t.Fatal(err)
		}
		mutate(t, s, workspace, jobID, runnerID)
		if err := s.publishCIJobLog(ctx, claim, "must not land", false); err == nil {
			t.Fatalf("case %d: scope change must block publish", i)
		}
		if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 0 {
			t.Fatalf("case %d: scope change wrote chunks: %#v", i, chunks)
		}
		state, _, _, _, _, _ := ciJobLogRowState(t, s, workspace, jobID)
		if state == "complete" {
			t.Fatalf("case %d: scope change must not complete", i)
		}
	}

	// Workspace-level scope drift.
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 20, "completed", true, true, now)
	claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_repositories SET enabled=false WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if err := s.publishCIJobLog(ctx, claim, "must not land", false); err == nil {
		t.Fatal("repository disable must block publish")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_repositories SET enabled=true WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	// Restoring eligibility lets the same still-valid claim finish.
	if err := s.publishCIJobLog(ctx, claim, "now it lands\n", false); err != nil {
		t.Fatalf("restored scope must allow publish: %v", err)
	}
	if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 1 {
		t.Fatalf("restored publish chunks: %#v", chunks)
	}
}

// Installation, product, and repository-binding drift are re-read inside the
// publish transaction. ErrNotFound means the job fell out of eligibility;
// ErrConflict means the job is still visible but under a different scope, so
// the stale claim must still write nothing.
func TestCIJobLogScopeChangeBinding(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, s *Store, workspace, job string)
		want   error
	}{
		{"ci product removed", func(t *testing.T, s *Store, workspace, job string) {
			if _, err := s.pool.Exec(context.Background(), `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, workspace); err != nil {
				t.Fatal(err)
			}
		}, ErrNotFound},
		{"installation suspended", func(t *testing.T, s *Store, workspace, job string) {
			if _, err := s.pool.Exec(context.Background(), `UPDATE ci_github_installations SET status='suspended' WHERE workspace_id=$1`, workspace); err != nil {
				t.Fatal(err)
			}
		}, ErrNotFound},
		{"installation github id changed", func(t *testing.T, s *Store, workspace, job string) {
			if _, err := s.pool.Exec(context.Background(), `UPDATE ci_github_installations SET github_installation_id=124 WHERE workspace_id=$1`, workspace); err != nil {
				t.Fatal(err)
			}
		}, ErrConflict},
		{"github run id changed", func(t *testing.T, s *Store, workspace, job string) {
			if _, err := s.pool.Exec(context.Background(), `UPDATE ci_jobs SET github_run_id=999999 WHERE workspace_id=$1 AND id=$2`, workspace, job); err != nil {
				t.Fatal(err)
			}
		}, ErrConflict},
		{"repository rebound to new enabled repo", func(t *testing.T, s *Store, workspace, job string) {
			if _, err := s.pool.Exec(context.Background(), `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name,enabled) VALUES($1,'ci_financial_installation',790,'financial-tests/repo-two',true)`, workspace); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(context.Background(), `UPDATE ci_jobs SET github_repository_id=790 WHERE workspace_id=$1 AND id=$2`, workspace, job); err != nil {
				t.Fatal(err)
			}
		}, ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, workspace := ciJobLogFixture(t)
			ctx := context.Background()
			jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, s.now())
			claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, s, workspace, jobID)
			if err := s.publishCIJobLog(ctx, claim, "must not land", false); !errors.Is(err, tc.want) {
				t.Fatalf("%s: publish must fail with %v, got %v", tc.name, tc.want, err)
			}
			if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 0 {
				t.Fatalf("%s: scope change wrote chunks: %#v", tc.name, chunks)
			}
			state, _, _, _, _, collected := ciJobLogRowState(t, s, workspace, jobID)
			if state == "complete" || collected != nil {
				t.Fatalf("%s: scope change must not complete: %s collected=%v", tc.name, state, collected)
			}
		})
	}
}

func TestCIJobLogJobDeleteCascade(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, runnerID, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)
	claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publishCIJobLog(ctx, claim, "doomed\n", false); err != nil {
		t.Fatal(err)
	}
	// Break the job↔runner FK cycle, then delete the job: both the collection
	// row and the log chunks must cascade away with it.
	if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET runner_id=NULL WHERE workspace_id=$1 AND id=$2`, workspace, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM ci_jobs WHERE workspace_id=$1 AND id=$2`, workspace, jobID); err != nil {
		t.Fatal(err)
	}
	view, err := s.CIJobLogCollection(ctx, workspace, jobID)
	if err != nil || view != nil {
		t.Fatalf("deleted job must not resurrect collection state: %v %#v", err, view)
	}
	var logs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_job_logs WHERE workspace_id=$1 AND job_id=$2`, workspace, jobID).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 0 {
		t.Fatalf("deleted job logs must cascade: %d", logs)
	}
}

func TestCIJobLogMaxConnsOne(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)

	cfg, err := pgxpool.ParseConfig(os.Getenv("CANTER_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := s.pool
	s.pool = pool
	t.Cleanup(func() {
		s.pool = old
		pool.Close()
	})

	claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatalf("single-conn claim: %v", err)
	}
	if err := s.publishCIJobLog(ctx, claim, "single conn\n", false); err != nil {
		t.Fatalf("single-conn publish: %v", err)
	}
	view, err := s.CIJobLogCollection(ctx, workspace, jobID)
	if err != nil || view == nil || view.State != "complete" {
		t.Fatalf("single-conn read: %v %#v", err, view)
	}
}

func TestCIJobLogExpiredLeaseRejectsPublish(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)
	claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	// Lease expiry alone never lets the stale owner write.
	if _, err := s.pool.Exec(ctx, `UPDATE ci_job_log_collections SET lease_until=$3 WHERE workspace_id=$1 AND job_id=$2`, workspace, jobID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.publishCIJobLog(ctx, claim, "too late\n", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired lease publish: %v", err)
	}
	if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 0 {
		t.Fatalf("expired lease wrote chunks: %#v", chunks)
	}
}

// A lease that expires while chunk INSERTs are in flight must still be
// fenced: the completion statement re-reads the clock, so the publish fails
// and the whole transaction — including already-inserted chunks — rolls back.
func TestCIJobLogExpiryDuringPublish(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx := context.Background()
	now := s.now()
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, now)
	claim, err := s.claimCIJobLog(ctx, workspace, jobID, ciJobLogNonce(t))
	if err != nil {
		t.Fatal(err)
	}
	// First clock read (the pre-insert lease check) sees a live lease; the
	// second read, taken immediately before the completion statement, lands
	// exactly on the lease boundary so lease_until>now is false.
	original := s.now
	calls := 0
	s.now = func() time.Time {
		calls++
		if calls == 1 {
			return now
		}
		return claim.LeaseUntil
	}
	t.Cleanup(func() { s.now = original })

	// Two chunks force INSERTs to run before the fenced completion.
	if err := s.publishCIJobLog(ctx, claim, strings.Repeat("a", ciJobLogChunkBytes+1), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("publish crossing lease expiry must conflict, got %v", err)
	}
	if chunks := ciJobLogChunks(t, s, workspace, jobID); len(chunks) != 0 {
		t.Fatalf("expired publish must roll back every chunk: %#v", chunks)
	}
	state, nonce, _, _, _, collected := ciJobLogRowState(t, s, workspace, jobID)
	if state != "collecting" || nonce != claim.Nonce || collected != nil {
		t.Fatalf("row must stay collecting under the same nonce: %s nonce=%q collected=%v", state, nonce, collected)
	}
}

func TestCIJobLogCollectorRun(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, s.now())

	calls := make(chan ciJobLogScope, 4)
	collector := &ciJobLogCollector{
		store: s,
		download: func(ctx context.Context, scope ciJobLogScope) (string, bool, error) {
			calls <- scope
			return "collected line\n", false, nil
		},
	}
	done := make(chan error, 1)
	go func() { defer close(done); done <- collector.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("collector did not join after cancel")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		view, err := s.CIJobLogCollection(ctx, workspace, jobID)
		if err == nil && view != nil && view.State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collection did not complete: %v %#v", err, view)
		}
		time.Sleep(25 * time.Millisecond)
	}
	select {
	case scope := <-calls:
		if scope.Job != jobID || scope.GitHubRepositoryID != 789 {
			t.Fatalf("download scope: %#v", scope)
		}
	default:
		t.Fatal("download was not invoked")
	}
	chunks := ciJobLogChunks(t, s, workspace, jobID)
	if len(chunks) != 1 || chunks[0] != "collected line\n" {
		t.Fatalf("collector chunks: %#v", chunks)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("collector Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("collector did not join after cancel")
	}
}

func TestCIJobLogCollectorFailureMarksUnavailable(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	jobID, _, _, _ := ciJobLogSeed(t, s, workspace, 1, "completed", true, true, s.now())

	collector := &ciJobLogCollector{
		store: s,
		download: func(ctx context.Context, scope ciJobLogScope) (string, bool, error) {
			return "", false, errCIJobLogUnavailable
		},
	}
	done := make(chan error, 1)
	go func() { defer close(done); done <- collector.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("collector did not join after cancel")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, _, _, _, _, _ := ciJobLogRowState(t, s, workspace, jobID)
		if state == "unavailable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collection did not become unavailable, state=%q", state)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestCIJobLogCollectorBlockedDownloadJoins(t *testing.T) {
	s, workspace := ciJobLogFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	ciJobLogSeed(t, s, workspace, 1, "completed", true, true, s.now())

	entered := make(chan struct{})
	collector := &ciJobLogCollector{
		store: s,
		download: func(ctx context.Context, scope ciJobLogScope) (string, bool, error) {
			close(entered)
			<-ctx.Done()
			return "", false, ctx.Err()
		},
	}
	done := make(chan error, 1)
	go func() { defer close(done); done <- collector.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("collector did not join after cancel")
		}
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("download was not entered")
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("collector did not join a blocked download")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("join took too long: %v", elapsed)
	}
}
