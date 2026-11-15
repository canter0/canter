package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func ciManagedWorkFixture(t *testing.T, count int) (*Store, string, ciProvisioningBudgetPolicy, []ciManagedClaim) {
	t.Helper()
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	profile := ciManagedClaimProfile()
	policy := ciBudgetPolicy(s.now())
	policy.ProfileDigest, _ = profile.digest()
	policy.MaxConcurrent, policy.MaxPerWorkspace = count, count
	ciBudgetGrant(t, s, "credit_work", int64(count)*100, policy)
	for n := 1; n <= count; n++ {
		ciManagedQueuedJob(t, s, workspace, "linux-x64-2", n)
	}
	var claims []ciManagedClaim
	for n := 0; n < count; n++ {
		claim, err := s.claimCIManagedRunner(context.Background(), profile)
		if err != nil {
			t.Fatal(err)
		}
		claims = append(claims, claim)
	}
	return s, workspace, policy, claims
}

func TestCIManagedWork(t *testing.T) {
	t.Run("bounded cursor visits every pending identity and wraps", func(t *testing.T) {
		s, workspace, policy, claims := ciManagedWorkFixture(t, 4)
		expected := map[string]bool{}
		for _, claim := range claims {
			expected[claim.Runner] = true
		}
		seen := map[string]bool{}
		afterWorkspace, afterRunner := "", ""
		for n := 0; n < 2; n++ {
			batch, err := s.listCIManagedWork(context.Background(), policy.ProfileDigest, afterWorkspace, afterRunner)
			if err != nil || len(batch) != 2 {
				t.Fatalf("bounded work batch length=%d err=%v", len(batch), err)
			}
			for _, item := range batch {
				if item.Workspace != workspace || !expected[item.Runner] || seen[item.Runner] {
					t.Fatal("work rotation duplicated or crossed an identity")
				}
				seen[item.Runner] = true
				afterWorkspace, afterRunner = item.Workspace, item.Runner
			}
		}
		wrapped, err := s.listCIManagedWork(context.Background(), policy.ProfileDigest, afterWorkspace, afterRunner)
		if err != nil || len(wrapped) != 2 || len(seen) != 4 {
			t.Fatal("work cursor did not wrap across all operations")
		}
	})

	t.Run("closed but unacknowledged creation and deleted capacity remain recoverable", func(t *testing.T) {
		s, workspace, policy, claims := ciManagedWorkFixture(t, 2)
		ctx := context.Background()
		for _, claim := range claims {
			if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET create_settled=true WHERE workspace_id=$1 AND runner_id=$2`, workspace, claim.Runner); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='provisioning' WHERE workspace_id=$1 AND id=$2`, workspace, claims[0].Runner); err != nil {
			t.Fatal(err)
		}
		ciBudgetProofs(t, s, workspace, claims[1].Runner, s.now())
		batch, err := s.listCIManagedWork(ctx, policy.ProfileDigest, "", "")
		if err != nil || len(batch) != 2 {
			t.Fatal("lost acknowledgement or verified capacity was stranded")
		}
		if err := s.releaseCIProvisioningCapacity(ctx, workspace, claims[1].Runner); err != nil {
			t.Fatal(err)
		}
		batch, err = s.listCIManagedWork(ctx, policy.ProfileDigest, "", "")
		if err != nil || len(batch) != 1 || batch[0].Runner != claims[0].Runner {
			t.Fatal("released capacity was dispatched again")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='registering' WHERE workspace_id=$1 AND id=$2`, workspace, claims[0].Runner); err != nil {
			t.Fatal(err)
		}
		batch, err = s.listCIManagedWork(ctx, policy.ProfileDigest, "", "")
		if err != nil || len(batch) != 0 {
			t.Fatal("healthy registered runner received creator work")
		}
	})

	t.Run("late setup failure never stops adopted execution", func(t *testing.T) {
		s, workspace, _, claims := ciManagedWorkFixture(t, 2)
		ctx := context.Background()
		first := claims[0]
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='running',execution_started_at=$3 WHERE workspace_id=$1 AND id=$2`,
			workspace, first.Runner, s.now()); err != nil {
			t.Fatal(err)
		}
		stopped, err := s.stopUnstartedCIManagedRunner(ctx, workspace, first.Runner, first.Spec.Plan)
		if err != nil || stopped {
			t.Fatal("late eligibility failure stopped an adopted job")
		}
		snap, err := s.loadCIRunnerLifecycle(ctx, workspace, first.Runner)
		if err != nil || snap.StopRequestedAt != nil || snap.Phase != "running" {
			t.Fatal("adopted execution changed")
		}
		second := claims[1]
		stopped, err = s.stopUnstartedCIManagedRunner(ctx, workspace, second.Runner, second.Spec.Plan)
		if err != nil || !stopped {
			t.Fatal("unstarted operation was not fenced for cleanup")
		}
		before, _ := s.loadCIRunnerLifecycle(ctx, workspace, second.Runner)
		now := s.now()
		s.now = func() time.Time { return now.Add(time.Minute) }
		stopped, err = s.stopUnstartedCIManagedRunner(ctx, workspace, second.Runner, second.Spec.Plan)
		after, loadErr := s.loadCIRunnerLifecycle(ctx, workspace, second.Runner)
		if err != nil || loadErr != nil || !stopped || before.StopRequestedAt == nil ||
			after.StopRequestedAt == nil || !before.StopRequestedAt.Equal(*after.StopRequestedAt) ||
			after.ExecutionStartedAt != nil || after.BillStopAt != nil {
			t.Fatal("stop retry changed the original clock or invented execution")
		}
		wrong := second.Spec.Plan
		wrong.FlavorID = "other"
		if _, err := s.stopUnstartedCIManagedRunner(ctx, workspace, second.Runner, wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("wrong lifecycle identity was accepted")
		}
	})
}
