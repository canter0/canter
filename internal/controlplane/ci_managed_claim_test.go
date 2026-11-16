package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func ciManagedClaimProfile() ciManagedRunnerProfile {
	profile := ciManagedProfileFixture()
	// Reviewed sealed guest artifact; changes require a new authorization.
	profile.GuestBundleSHA256 = ciManagedTestBundleSHA
	return profile
}

func ciManagedQueuedJob(t *testing.T, s *Store, workspace, size string, number int) string {
	t.Helper()
	id := fmt.Sprintf("ci_managed_job_%d", number)
	doc, err := json.Marshal(ciJobDocument{Job: CIJob{ID: id, Size: size, Status: "queued"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document,created_at) VALUES($1,$2,'ci_financial_installation',789,$3,$4,'queued',$5,$6)`,
		id, workspace, number+7000, number+8000, doc, s.now().Add(time.Duration(number)*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ciManagedClaimCounts(t *testing.T, s *Store, workspace string, want int) {
	t.Helper()
	var runners, plans, grants, funding int
	err := s.pool.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM ci_runners WHERE workspace_id=$1),
 (SELECT count(*) FROM ci_runner_lifecycles WHERE workspace_id=$1),
 (SELECT count(*) FROM ci_runner_bootstrap_grants g JOIN ci_runner_lifecycles l ON l.operation_id=g.operation_id WHERE l.workspace_id=$1),
 (SELECT count(*) FROM ci_provisioning_reservations WHERE workspace_id=$1 AND released_at IS NULL)`,
		workspace).Scan(&runners, &plans, &grants, &funding)
	if err != nil || runners != want || plans != want || grants != want || funding != want {
		t.Fatalf("atomic claim counts runners=%d plans=%d grants=%d funding=%d want=%d err=%v",
			runners, plans, grants, funding, want, err)
	}
}

func TestCIManagedClaim(t *testing.T) {
	t.Run("no authorization creates no lifecycle or allocation", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		ciManagedQueuedJob(t, s, workspace, "linux-x64-2", 1)
		_, err := s.claimCIManagedRunner(context.Background(), ciManagedClaimProfile())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("unfunded claim: %v", err)
		}
		ciManagedClaimCounts(t, s, workspace, 0)
		ciBudgetTotalsZero(t, s, workspace)
	})

	t.Run("concurrent claims obey caps and preserve the execution clock", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		profile := ciManagedClaimProfile()
		policy := ciBudgetPolicy(s.now())
		policy.ProfileDigest, _ = profile.digest()
		ciBudgetGrant(t, s, "credit_claim", 1000, policy)
		for n := 1; n <= 12; n++ {
			ciManagedQueuedJob(t, s, workspace, "linux-x64-2", n)
		}
		type result struct {
			claim ciManagedClaim
			err   error
		}
		results := make(chan result, 12)
		var wg sync.WaitGroup
		for n := 0; n < 12; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				claim, err := s.claimCIManagedRunner(context.Background(), profile)
				results <- result{claim, err}
			}()
		}
		wg.Wait()
		close(results)
		successes := 0
		seen := map[string]bool{}
		for result := range results {
			if result.err != nil {
				if !errors.Is(result.err, ErrCapacity) {
					t.Fatalf("unexpected claim failure: %v", result.err)
				}
				continue
			}
			successes++
			claim := result.claim
			if claim.Workspace != workspace || seen[claim.Runner] ||
				claim.Spec.SizeID != "linux-x64-2" || claim.Spec.GitHubInstallationID != 123 ||
				claim.Spec.GitHubRepositoryID != 789 || !claim.Spec.Plan.newPlanValid(s.now()) {
				t.Fatal("claim identity or immutable deadline mismatch")
			}
			seen[claim.Runner] = true
			scope, err := s.ciBootstrapScope(context.Background(), workspace, claim.Runner, policy.ProfileDigest, claim.Spec.Plan, true)
			if err != nil || !ciBootstrapGrantMatches(scope) || scope.Grant.AttemptedAt != nil ||
				scope.Snapshot.CreateAttemptedAt != nil || scope.Snapshot.ExecutionStartedAt != nil {
				t.Fatal("claim started creation, consumed credentials or reset execution")
			}
		}
		if successes != 2 {
			t.Fatalf("successful claims=%d want=2", successes)
		}
		ciManagedClaimCounts(t, s, workspace, 2)
		state := ciBudgetPeek(t, s, "credit_claim", 1000, policy)
		if state.Active != 2 || state.CommittedMicros != 200 || state.AuthorizedMicros != 1000 {
			t.Fatal("claim escaped infrastructure envelope")
		}
		ciBudgetTotalsZero(t, s, workspace)
	})

	t.Run("bootstrap failure rolls back runner plan and infrastructure commitment", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		profile := ciManagedClaimProfile()
		policy := ciBudgetPolicy(s.now())
		policy.ProfileDigest, _ = profile.digest()
		ciBudgetGrant(t, s, "credit_claim", 1000, policy)
		ciManagedQueuedJob(t, s, workspace, "linux-x64-2", 1)
		ctx := context.Background()
		_, err := s.pool.Exec(ctx, `ALTER TABLE ci_runner_bootstrap_grants ADD CONSTRAINT ci_claim_test_refuse CHECK (false) NOT VALID`)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := s.pool.Exec(context.Background(), `ALTER TABLE ci_runner_bootstrap_grants DROP CONSTRAINT ci_claim_test_refuse`); err != nil {
				t.Error(err)
			}
		})
		if _, err = s.claimCIManagedRunner(ctx, profile); err == nil {
			t.Fatal("injected bootstrap refusal was ignored")
		}
		ciManagedClaimCounts(t, s, workspace, 0)
		state := ciBudgetPeek(t, s, "credit_claim", 1000, policy)
		if state.Active != 0 || state.CommittedMicros != 0 {
			t.Fatal("failed atomic claim consumed capacity or funding")
		}
		ciBudgetTotalsZero(t, s, workspace)
	})

	t.Run("queue eligibility cannot consume capacity or money", func(t *testing.T) {
		cases := []struct {
			name string
			sql  string
			want error
		}{
			{"disabled product", `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, errCIRunnerJobPending},
			{"suspended installation", `UPDATE ci_github_installations SET status='suspended' WHERE workspace_id=$1`, errCIRunnerJobPending},
			{"disabled repository", `UPDATE ci_repositories SET enabled=false WHERE workspace_id=$1`, errCIRunnerJobPending},
			{"cancelled job", `UPDATE ci_jobs SET status='cancelled' WHERE workspace_id=$1`, errCIRunnerJobPending},
			{"unsupported size", "", errCIRunnerJobPending},
			{"authorization window too short", "", ErrConflict},
			{"wrong authorization profile", "", ErrConflict},
			{"insufficient authorization", "", ErrCapacity},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, workspace, _ := ciFinancialFixture(t)
				ciBudgetReset(t, s)
				ciAdmissionEnable(t, s, workspace)
				profile := ciManagedClaimProfile()
				size := "linux-x64-2"
				if tc.name == "unsupported size" {
					profile.Machines = profile.Machines[:1]
					size = "linux-x64-4"
				}
				ciManagedQueuedJob(t, s, workspace, size, 1)
				policy := ciBudgetPolicy(s.now())
				policy.ProfileDigest, _ = profile.digest()
				amount := int64(1000)
				switch tc.name {
				case "authorization window too short":
					policy.AdmitBefore = s.now().Add(899 * time.Second)
				case "wrong authorization profile":
					policy.ProfileDigest = strings.Repeat("d", 64)
				case "insufficient authorization":
					amount = 50
				}
				ciBudgetGrant(t, s, "credit_claim", amount, policy)
				if tc.sql != "" {
					if _, err := s.pool.Exec(context.Background(), tc.sql, workspace); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.claimCIManagedRunner(context.Background(), profile); !errors.Is(err, tc.want) {
					t.Fatalf("claim denial: %v want=%v", err, tc.want)
				}
				ciManagedClaimCounts(t, s, workspace, 0)
				state := ciBudgetPeek(t, s, "credit_claim", amount, policy)
				if state.Active != 0 || state.CommittedMicros != 0 || state.AuthorizedMicros != amount {
					t.Fatal("ineligible demand consumed funding")
				}
			})
		}
	})

	t.Run("matching spare capacity follows actual adoption not trigger jobs", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		profile := ciManagedClaimProfile()
		policy := ciBudgetPolicy(s.now())
		policy.ProfileDigest, _ = profile.digest()
		policy.MaxConcurrent, policy.MaxPerWorkspace = 3, 3
		ciBudgetGrant(t, s, "credit_claim", 1000, policy)
		jobA := ciManagedQueuedJob(t, s, workspace, "linux-x64-2", 1)
		jobB := ciManagedQueuedJob(t, s, workspace, "linux-x64-2", 2)
		ctx := context.Background()
		first, err := s.claimCIManagedRunner(ctx, profile)
		if err != nil {
			t.Fatal(err)
		}
		second, err := s.claimCIManagedRunner(ctx, profile)
		if err != nil {
			t.Fatal(err)
		}
		// Synthetic source-committed adoption: A's runner accepted B. The
		// other unadmitted runner still covers the one remaining queued job.
		_, err = s.pool.Exec(ctx, `UPDATE ci_runners SET assigned_job_id=$3,phase='running',execution_started_at=$4 WHERE workspace_id=$1 AND id=$2`,
			workspace, first.Runner, jobB, s.now())
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.pool.Exec(ctx, `UPDATE ci_jobs SET status='running',runner_id=$3 WHERE workspace_id=$1 AND id=$2`,
			workspace, jobB, first.Runner)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.claimCIManagedRunner(ctx, profile); !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("matching spare capacity was double provisioned: %v", err)
		}
		if _, err = s.requestCIRunnerStop(ctx, workspace, second.Runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		third, err := s.claimCIManagedRunner(ctx, profile)
		if err != nil {
			t.Fatal(err)
		}
		var trigger string
		if err = s.pool.QueryRow(ctx, `SELECT trigger_job_id FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, third.Runner).Scan(&trigger); err != nil || trigger != jobA {
			t.Fatal("replacement did not serve the still-queued job")
		}
		ciManagedClaimCounts(t, s, workspace, 3)
	})
}
