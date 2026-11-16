package controlplane

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ciBudgetPolicy is the synthetic operator policy used across these tests.
// All amounts are arbitrary micro units, never real prices.
func ciBudgetPolicy(now time.Time) ciProvisioningBudgetPolicy {
	return ciProvisioningBudgetPolicy{
		Currency:          "EUR",
		ProfileDigest:     strings.Repeat("a", 64),
		MaxConcurrent:     2,
		MaxPerWorkspace:   2,
		ReservationMicros: 100,
		AdmitBefore:       now.Add(86400 * time.Second),
	}
}

func ciBudgetDigestB() string { return strings.Repeat("b", 64) }

func ciBudgetPlan(t *testing.T, op string, now time.Time) ciRunnerLifecyclePlan {
	t.Helper()
	lease, err := ciEnforcerNewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	return ciRunnerLifecyclePlan{
		OperationID:     op,
		ServerName:      ciEnforcerRunnerName(op),
		Region:          "r1",
		FlavorID:        "f1",
		ImageID:         "i1",
		NetworkID:       "n1",
		EnforcerLease:   lease,
		SetupDeadlineAt: now.Add(600 * time.Second),
		HardDeadlineAt:  now.Add(22000 * time.Second),
	}
}

// ciBudgetSeed creates one queued financial runner and pins its lifecycle
// plan exactly as the parent would before any funding reservation.
func ciBudgetSeed(t *testing.T, s *Store, workspace, op string, number int, now time.Time) (string, ciRunnerLifecyclePlan) {
	t.Helper()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", number, false, now)
	plan := ciBudgetPlan(t, op, now)
	if err := s.trackCIRunnerLifecycle(context.Background(), workspace, runner, plan); err != nil {
		t.Fatalf("track lifecycle %s: %v", op, err)
	}
	return runner, plan
}

func ciBudgetGrant(t *testing.T, s *Store, id string, amount int64, policy ciProvisioningBudgetPolicy) ciProvisioningBudgetState {
	t.Helper()
	state, err := s.authorizeCIProvisioningBudget(context.Background(), ciProvisioningAuthorization{ID: id, Policy: policy, AmountMicros: amount})
	if err != nil {
		t.Fatalf("authorize %s: %v", id, err)
	}
	return state
}

// ciBudgetPeek replays an identical authorization, which is a read-only
// idempotent retry returning the current singleton state.
func ciBudgetPeek(t *testing.T, s *Store, id string, amount int64, policy ciProvisioningBudgetPolicy) ciProvisioningBudgetState {
	t.Helper()
	state, err := s.authorizeCIProvisioningBudget(context.Background(), ciProvisioningAuthorization{ID: id, Policy: policy, AmountMicros: amount})
	if err != nil {
		t.Fatalf("peek %s: %v", id, err)
	}
	return state
}

func ciBudgetReserve(t *testing.T, s *Store, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan) ciProvisioningReservation {
	t.Helper()
	res, err := s.reserveCIProvisioningBudget(context.Background(), workspace, runner, profileDigest, plan)
	if err != nil {
		t.Fatalf("reserve %s/%s: %v", workspace, runner, err)
	}
	return res
}

// ciBudgetProofs commits the exact trusted lifecycle facts a verified
// cleanup produces, using only test SQL against the scratch database.
func ciBudgetProofs(t *testing.T, s *Store, workspace, runner string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted' WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET stopped_verified_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET cleanup_verified_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET create_settled=true WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
}

func ciBudgetTotalsZero(t *testing.T, s *Store, workspace string) {
	t.Helper()
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 0 || cents != 0 || reserved != 0 {
		t.Fatalf("customer financials moved: units=%d cents=%d reserved=%d", units, cents, reserved)
	}
}

// ciBudgetReset clears the global singleton budget. Authorization IDs are
// permanent keys and the singleton survives across fixtures, so every case
// starts from an empty authorization state.
func ciBudgetReset(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM ci_provisioning_reservations`,
		`DELETE FROM ci_provisioning_authorizations`,
		`DELETE FROM ci_provisioning_budget`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("budget reset: %v", err)
		}
	}
}

func TestCIProvisioningBudgetDisabledNoAuthorization(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)

	_, err := s.reserveCIProvisioningBudget(context.Background(), workspace, runner, policy.ProfileDigest, plan)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserve before grant: %v", err)
	}
	ciBudgetTotalsZero(t, s, workspace)
	snap, err := s.loadCIRunnerLifecycle(context.Background(), workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ExecutionStartedAt != nil {
		t.Fatal("funding denial started an execution clock")
	}

	state := ciBudgetGrant(t, s, "credit_1", 1000, policy)
	if state.AuthorizedMicros != 1000 || state.CommittedMicros != 0 || state.Active != 0 {
		t.Fatalf("unexpected grant state %+v", state)
	}
}

func TestCIProvisioningBudgetAuthorizationIdempotency(t *testing.T) {
	s, _, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	now := s.now()
	policy := ciBudgetPolicy(now)
	grant := ciProvisioningAuthorization{ID: "credit_1", Policy: policy, AmountMicros: 1000}

	first := ciBudgetGrant(t, s, "credit_1", 1000, policy)
	if first.AuthorizedMicros != 1000 {
		t.Fatalf("first grant %+v", first)
	}
	again := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if again.AuthorizedMicros != 1000 {
		t.Fatalf("sequential retry added funds: %+v", again)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.authorizeCIProvisioningBudget(context.Background(), grant)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent retry %d: %v", i, err)
		}
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.AuthorizedMicros != 1000 || state.CommittedMicros != 0 {
		t.Fatalf("concurrent retries changed totals: %+v", state)
	}

	changedAmount := grant
	changedAmount.AmountMicros = 2000
	if _, err := s.authorizeCIProvisioningBudget(context.Background(), changedAmount); !errors.Is(err, ErrConflict) {
		t.Fatalf("same ID changed amount: %v", err)
	}
	changedPolicy := grant
	changedPolicy.Policy.MaxConcurrent = 3
	if _, err := s.authorizeCIProvisioningBudget(context.Background(), changedPolicy); !errors.Is(err, ErrConflict) {
		t.Fatalf("same ID changed policy: %v", err)
	}

	topup := ciBudgetGrant(t, s, "credit_2", 500, policy)
	if topup.AuthorizedMicros != 1500 {
		t.Fatalf("topup state %+v", topup)
	}
	old := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if old.AuthorizedMicros != 1500 {
		t.Fatalf("old retry rewrote totals: %+v", old)
	}
}

func TestCIProvisioningBudgetAtomicReserve(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", 1000, policy)
	runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)

	res := ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)
	if res.ReservedMicros != 100 || res.Currency != "EUR" || res.PlanDigest == "" ||
		res.OperationID != plan.OperationID || res.Runner != runner {
		t.Fatalf("reservation %+v", res)
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("state after reserve %+v", state)
	}

	const n = 16
	results := make([]ciProvisioningReservation, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.reserveCIProvisioningBudget(context.Background(), workspace, runner, policy.ProfileDigest, plan)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("idempotent reserve %d: %v", i, errs[i])
		}
		r := results[i]
		if r.OperationID != res.OperationID || r.Currency != res.Currency ||
			r.ReservedMicros != res.ReservedMicros || r.PlanDigest != res.PlanDigest ||
			r.ProfileDigest != res.ProfileDigest || !r.ReservedAt.Equal(res.ReservedAt) {
			t.Fatalf("reserve %d diverged: %+v vs %+v", i, r, res)
		}
	}
	state = ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("concurrent reserves changed totals: %+v", state)
	}
	ciBudgetTotalsZero(t, s, workspace)
	snap, err := s.loadCIRunnerLifecycle(context.Background(), workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ExecutionStartedAt != nil {
		t.Fatal("fund reservation started an execution clock")
	}
}

// ciBudgetSecondWorkspace builds a second workspace with its own GitHub
// installation, repository, and CI product using only fixture SQL.
func ciBudgetSecondWorkspace(t *testing.T, s *Store, email, installID string, repoID int64) string {
	t.Helper()
	ctx := context.Background()
	acct, ws, _, err := s.Signup(ctx, email, "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,created_by) VALUES($1,$2,124,457,'budget-tests','Organization',$3)`, installID, ws.ID, acct.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name) VALUES($1,$2,$3,'budget-tests/repo-two')`, ws.ID, installID, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) VALUES($1,'ci','human',$2)`, ws.ID, acct.ID); err != nil {
		t.Fatal(err)
	}
	return ws.ID
}

// ciBudgetRunner mirrors ciFinancialRunner with an explicit installation and
// repository so second workspaces can seed runners.
func ciBudgetRunner(t *testing.T, s *Store, workspace, installation string, repoID int64, size string, number int, createdAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	job, runner := fmt.Sprintf("ci_financial_job_%d", number), fmt.Sprintf("ci_financial_runner_%d", number)
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document) VALUES($1,$2,$3,$4,$5,$5,'queued','{}'::jsonb)`, job, workspace, installation, repoID, number+1000); err != nil {
		t.Fatal(err)
	}
	price, ok := pricing.FindCI(size)
	if !ok {
		t.Fatal("invalid test size")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,phase,document,rate_version,rate_micros_per_minute,paid_overrun_enabled,created_at) VALUES($1,$2,$3,$4,$5,'queued','{}'::jsonb,$6,$7,false,$8)`, runner, workspace, installation, size, job, pricing.CIRateVersion, price.MicrosPerMinute, createdAt); err != nil {
		t.Fatal(err)
	}
	return runner
}

func ciBudgetSeedOn(t *testing.T, s *Store, workspace, installation string, repoID int64, op string, number int, now time.Time) (string, ciRunnerLifecyclePlan) {
	t.Helper()
	runner := ciBudgetRunner(t, s, workspace, installation, repoID, "linux-x64-2", number, now)
	plan := ciBudgetPlan(t, op, now)
	if err := s.trackCIRunnerLifecycle(context.Background(), workspace, runner, plan); err != nil {
		t.Fatalf("track lifecycle %s: %v", op, err)
	}
	return runner, plan
}

func TestCIProvisioningBudgetGlobalCap(t *testing.T) {
	s, w1, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, w1)
	now := s.now()
	policy := ciBudgetPolicy(now)
	policy.MaxConcurrent = 2
	policy.MaxPerWorkspace = 2
	ciBudgetGrant(t, s, "credit_1", 10000, policy)

	w2 := ciBudgetSecondWorkspace(t, s, "budget-w2@example.com", "ci_budget_install_2", 790)
	type target struct {
		workspace, runner string
		plan              ciRunnerLifecyclePlan
	}
	targets := make([]target, 0, 16)
	for i := 1; i <= 8; i++ {
		op := fmt.Sprintf("budget_op_%d", i)
		runner, plan := ciBudgetSeedOn(t, s, w1, "ci_financial_installation", 789, op, i, now)
		targets = append(targets, target{w1, runner, plan})
	}
	for i := 9; i <= 16; i++ {
		op := fmt.Sprintf("budget_op_%d", i)
		runner, plan := ciBudgetSeedOn(t, s, w2, "ci_budget_install_2", 790, op, i, now)
		targets = append(targets, target{w2, runner, plan})
	}

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.reserveCIProvisioningBudget(context.Background(), targets[i].workspace, targets[i].runner, policy.ProfileDigest, targets[i].plan)
		}(i)
	}
	wg.Wait()
	succeeded, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrCapacity):
			capped++
		default:
			t.Fatalf("unexpected reserve error: %v", err)
		}
	}
	if succeeded != 2 || capped != 14 {
		t.Fatalf("global cap: %d succeeded %d capped", succeeded, capped)
	}
	state := ciBudgetPeek(t, s, "credit_1", 10000, policy)
	if state.Active != 2 || state.CommittedMicros != 200 {
		t.Fatalf("state %+v", state)
	}
	for _, tg := range targets {
		snap, err := s.loadCIRunnerLifecycle(context.Background(), tg.workspace, tg.runner)
		if err != nil {
			t.Fatal(err)
		}
		if snap.ExecutionStartedAt != nil {
			t.Fatal("capped reserve started an execution")
		}
	}
}

func TestCIProvisioningBudgetPerWorkspaceCap(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	policy.MaxPerWorkspace = 1
	ciBudgetGrant(t, s, "credit_1", 1000, policy)

	runner1, plan1 := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	runner2, plan2 := ciBudgetSeed(t, s, workspace, "budget_op_2", 2, now)

	ciBudgetReserve(t, s, workspace, runner1, policy.ProfileDigest, plan1)
	if _, err := s.reserveCIProvisioningBudget(context.Background(), workspace, runner2, policy.ProfileDigest, plan2); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second same-workspace reserve: %v", err)
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("state %+v", state)
	}
}

func TestCIProvisioningBudgetMoneyCapIndependent(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", 150, policy)

	runner1, plan1 := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	runner2, plan2 := ciBudgetSeed(t, s, workspace, "budget_op_2", 2, now)

	ciBudgetReserve(t, s, workspace, runner1, policy.ProfileDigest, plan1)
	// Global concurrency allows two, but only 50 micro units remain for
	// the second 100-unit reservation: the money cap denies independently.
	if _, err := s.reserveCIProvisioningBudget(context.Background(), workspace, runner2, policy.ProfileDigest, plan2); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unfunded reserve: %v", err)
	}
	state := ciBudgetPeek(t, s, "credit_1", 150, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("state after money cap %+v", state)
	}
	// The denied transaction must not have consumed the operation identity.
	ciBudgetGrant(t, s, "credit_2", 50, policy)
	res := ciBudgetReserve(t, s, workspace, runner2, policy.ProfileDigest, plan2)
	if res.ReservedMicros != 100 {
		t.Fatalf("reservation %+v", res)
	}
	state = ciBudgetPeek(t, s, "credit_1", 150, policy)
	if state.Active != 2 || state.CommittedMicros != 200 || state.AuthorizedMicros != 200 {
		t.Fatalf("state after topup %+v", state)
	}
}

func TestCIProvisioningBudgetScopeDenials(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Store, string, ciProvisioningBudgetPolicy, string, ciRunnerLifecyclePlan) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		policy := ciBudgetPolicy(now)
		ciBudgetGrant(t, s, "credit_1", 1000, policy)
		runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
		return s, workspace, policy, runner, plan
	}
	assertDenied := func(t *testing.T, s *Store, workspace string, policy ciProvisioningBudgetPolicy, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected denial")
		}
		state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
		if state.Active != 0 || state.CommittedMicros != 0 {
			t.Fatalf("denial consumed budget: %+v", state)
		}
		ciBudgetTotalsZero(t, s, workspace)
	}

	t.Run("changed profile digest", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, ciBudgetDigestB(), plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("wrong workspace", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		other := ciBudgetSecondWorkspace(t, s, "budget-deny-w2@example.com", "ci_budget_install_3", 791)
		_, err := s.reserveCIProvisioningBudget(ctx, other, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("wrong runner", func(t *testing.T) {
		s, w, policy, _, plan := setup(t)
		_, err := s.reserveCIProvisioningBudget(ctx, w, "ci_financial_runner_999", policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("stop requested", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		if _, err := s.requestCIRunnerStop(ctx, w, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("ci product disabled", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		if _, err := s.pool.Exec(ctx, `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, w); err != nil {
			t.Fatal(err)
		}
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("suspended installation", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		if _, err := s.pool.Exec(ctx, `UPDATE ci_github_installations SET status='suspended' WHERE id='ci_financial_installation'`); err != nil {
			t.Fatal(err)
		}
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("disabled repository", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		if _, err := s.pool.Exec(ctx, `UPDATE ci_repositories SET enabled=false WHERE workspace_id=$1`, w); err != nil {
			t.Fatal(err)
		}
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("cancelled trigger job", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET status='cancelled' WHERE workspace_id=$1`, w); err != nil {
			t.Fatal(err)
		}
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("delete requested", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		ciAdmissionPhase(t, s, w, runner, "queued", true)
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("executed runner", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET execution_started_at=$3 WHERE workspace_id=$1 AND id=$2`, w, runner, s.now()); err != nil {
			t.Fatal(err)
		}
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, plan)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("changed plan field", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		bad := plan
		bad.FlavorID = "f2"
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, bad)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("changed plan deadline", func(t *testing.T) {
		s, w, policy, runner, plan := setup(t)
		bad := plan
		bad.HardDeadlineAt = bad.HardDeadlineAt.Add(time.Second)
		_, err := s.reserveCIProvisioningBudget(ctx, w, runner, policy.ProfileDigest, bad)
		assertDenied(t, s, w, policy, err)
	})
	t.Run("setup after admit before", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		policy := ciBudgetPolicy(now)
		policy.AdmitBefore = now.Add(500 * time.Second) // before plan.SetupDeadlineAt (now+600)
		ciBudgetGrant(t, s, "credit_1", 1000, policy)
		runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
		_, err := s.reserveCIProvisioningBudget(ctx, workspace, runner, policy.ProfileDigest, plan)
		if err == nil {
			t.Fatal("setup deadline beyond admit window allowed")
		}
		state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
		if state.Active != 0 || state.CommittedMicros != 0 {
			t.Fatalf("state %+v", state)
		}
	})
	t.Run("expired admit window", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		policy := ciBudgetPolicy(now)
		policy.AdmitBefore = now.Add(900 * time.Second)
		ciBudgetGrant(t, s, "credit_1", 1000, policy)
		runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
		// Start from a genuinely valid authorization and reservation.
		// Expiring the admit window necessarily also expires setup, since
		// setup must fit inside that window. Neither expiry may renew it.
		ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)
		s.now = func() time.Time { return now.Add(1000 * time.Second) }
		_, err := s.reserveCIProvisioningBudget(ctx, workspace, runner, policy.ProfileDigest, plan)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expired reservation revalidated: %v", err)
		}
		state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
		if state.Active != 1 || state.CommittedMicros != 100 {
			t.Fatalf("expiry reset the reservation: %+v", state)
		}
	})
}

func TestCIProvisioningBudgetExpiredAuthorizationRollsBack(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	policy.AdmitBefore = now.Add(-time.Second)
	input := ciProvisioningAuthorization{ID: "expired_credit", Policy: policy, AmountMicros: 1000}
	if _, err := s.authorizeCIProvisioningBudget(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired authorization accepted: %v", err)
	}
	// No overflow or active-allocation guard masks the expiry check here.
	// A corrected request with the same ID must be the first committed one.
	input.Policy.AdmitBefore = now.Add(24 * time.Hour)
	state, err := s.authorizeCIProvisioningBudget(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if state.AuthorizedMicros != 1000 || state.CommittedMicros != 0 || state.Active != 0 {
		t.Fatalf("rejected authorization left durable state: %+v", state)
	}
	ciBudgetTotalsZero(t, s, workspace)
}

func TestCIProvisioningBudgetPolicyChange(t *testing.T) {
	ctx := context.Background()

	// Currency is immutable even with zero active allocations: on a fresh
	// singleton a USD grant succeeds while an EUR grant conflicts, proving
	// there is no conversion path. The singleton is then reset for the main
	// EUR flow below.
	s2, _, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s2)
	now2 := s2.now()
	usd := ciBudgetPolicy(now2)
	usd.Currency = "USD"
	ciBudgetGrant(t, s2, "credit_usd", 1000, usd)
	eur := usd
	eur.Currency = "EUR"
	if _, err := s2.authorizeCIProvisioningBudget(ctx, ciProvisioningAuthorization{ID: "credit_eur", Policy: eur, AmountMicros: 100}); !errors.Is(err, ErrConflict) {
		t.Fatalf("currency change with no actives: %v", err)
	}
	ciBudgetReset(t, s2)

	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policyA := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", 1000, policyA)
	runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	ciBudgetReserve(t, s, workspace, runner, policyA.ProfileDigest, plan)

	for name, mutate := range map[string]func(*ciProvisioningBudgetPolicy){
		"profile digest": func(p *ciProvisioningBudgetPolicy) { p.ProfileDigest = ciBudgetDigestB() },
		"max concurrent": func(p *ciProvisioningBudgetPolicy) { p.MaxConcurrent = 3 },
		"max per ws":     func(p *ciProvisioningBudgetPolicy) { p.MaxPerWorkspace = 1 },
		"reservation":    func(p *ciProvisioningBudgetPolicy) { p.ReservationMicros = 50 },
		"admit before":   func(p *ciProvisioningBudgetPolicy) { p.AdmitBefore = p.AdmitBefore.Add(time.Hour) },
	} {
		p := policyA
		mutate(&p)
		if _, err := s.authorizeCIProvisioningBudget(ctx, ciProvisioningAuthorization{ID: "credit_x_" + strings.ReplaceAll(name, " ", "_"), Policy: p, AmountMicros: 100}); !errors.Is(err, ErrConflict) {
			t.Fatalf("policy change %s while active: %v", name, err)
		}
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policyA)
	if state.Active != 1 || state.CommittedMicros != 100 || state.AuthorizedMicros != 1000 {
		t.Fatalf("denied policy changes moved state: %+v", state)
	}

	// Identical policy under a new ID tops up without resetting anything.
	topup := ciBudgetGrant(t, s, "credit_2", 500, policyA)
	if topup.AuthorizedMicros != 1500 || topup.Policy.ProfileDigest != policyA.ProfileDigest {
		t.Fatalf("same-policy topup %+v", topup)
	}

	// After a fully verified release the policy may change explicitly.
	ciBudgetProofs(t, s, workspace, runner, now)
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	policyB := policyA
	policyB.ProfileDigest = ciBudgetDigestB()
	policyB.ReservationMicros = 50
	policyB.AdmitBefore = now.Add(90000 * time.Second)
	ciBudgetGrant(t, s, "credit_3", 100, policyB)

	// The original credit_1 retry is idempotent: it returns current state,
	// which now carries policy B and the retained commitment.
	old := ciBudgetPeek(t, s, "credit_1", 1000, policyA)
	if old.Policy.ProfileDigest != ciBudgetDigestB() || old.Policy.ReservationMicros != 50 ||
		old.AuthorizedMicros != 1600 || old.CommittedMicros != 100 || old.Active != 0 {
		t.Fatalf("old retry did not return current state: %+v", old)
	}

	// A new runner under profile B reserves at the new rate.
	runner2, plan2 := ciBudgetSeed(t, s, workspace, "budget_op_2", 2, now)
	resB, err := s.reserveCIProvisioningBudget(ctx, workspace, runner2, ciBudgetDigestB(), plan2)
	if err != nil {
		t.Fatal(err)
	}
	if resB.ReservedMicros != 50 || resB.ProfileDigest != ciBudgetDigestB() {
		t.Fatalf("policy B reservation %+v", resB)
	}
}

func TestCIProvisioningBudgetOverflowAndInvalidPolicy(t *testing.T) {
	s, _, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	now := s.now()
	policy := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", math.MaxInt64-50, policy)

	// 51 would overflow authorized_micros; the failed attempt must not
	// persist the authorization key.
	over := ciProvisioningAuthorization{ID: "credit_2", Policy: policy, AmountMicros: 51}
	if _, err := s.authorizeCIProvisioningBudget(context.Background(), over); !errors.Is(err, ErrConflict) {
		t.Fatalf("overflow grant: %v", err)
	}
	state := ciBudgetGrant(t, s, "credit_2", 50, policy)
	if state.AuthorizedMicros != math.MaxInt64 || state.CommittedMicros != 0 || state.Active != 0 {
		t.Fatalf("post-overflow state %+v", state)
	}

	invalid := map[string]ciProvisioningAuthorization{
		"bad currency": {ID: "bad_cur", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.Currency = "JPY"; return p }(), AmountMicros: 1},
		"zero admit":   {ID: "bad_time", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.AdmitBefore = time.Time{}; return p }(), AmountMicros: 1},
		"year 10000": {ID: "bad_year", Policy: func() ciProvisioningBudgetPolicy {
			p := policy
			p.AdmitBefore = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			return p
		}(), AmountMicros: 1},
		"expired admit":     {ID: "bad_exp", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.AdmitBefore = now.Add(-time.Second); return p }(), AmountMicros: 1},
		"zero amount":       {ID: "bad_amt0", Policy: policy, AmountMicros: 0},
		"negative amount":   {ID: "bad_amtn", Policy: policy, AmountMicros: -5},
		"bad digest":        {ID: "bad_dig", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.ProfileDigest = "zz"; return p }(), AmountMicros: 1},
		"max concurrent 0":  {ID: "bad_mc0", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.MaxConcurrent = 0; return p }(), AmountMicros: 1},
		"max concurrent hi": {ID: "bad_mchi", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.MaxConcurrent = 1001; return p }(), AmountMicros: 1},
		"per ws over max":   {ID: "bad_pwm", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.MaxPerWorkspace = 3; p.MaxConcurrent = 2; return p }(), AmountMicros: 1},
		"zero reserve":      {ID: "bad_rsv", Policy: func() ciProvisioningBudgetPolicy { p := policy; p.ReservationMicros = 0; return p }(), AmountMicros: 1},
		"bad id":            {ID: "bad id!", Policy: policy, AmountMicros: 1},
	}
	for name, in := range invalid {
		if _, err := s.authorizeCIProvisioningBudget(context.Background(), in); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	state = ciBudgetPeek(t, s, "credit_2", 50, policy)
	if state.AuthorizedMicros != math.MaxInt64 || state.CommittedMicros != 0 || state.Active != 0 {
		t.Fatalf("invalid policies moved state: %+v", state)
	}
}

func TestCIProvisioningBudgetReleaseNeedsProofs(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", 1000, policy)
	runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	res := ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)
	ctx := context.Background()

	// No proofs at all: release must conflict and keep the slot.
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); !errors.Is(err, ErrConflict) {
		t.Fatalf("release without proofs: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted' WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); !errors.Is(err, ErrConflict) {
		t.Fatalf("release with only phase deleted: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET stopped_verified_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, now); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); !errors.Is(err, ErrConflict) {
		t.Fatalf("release without cleanup proof: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET cleanup_verified_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, now); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); !errors.Is(err, ErrConflict) {
		t.Fatalf("release without create_settled: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET create_settled=true WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); err != nil {
		t.Fatalf("release with full proofs: %v", err)
	}
	// Idempotent retry.
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); err != nil {
		t.Fatalf("release retry: %v", err)
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 0 || state.CommittedMicros != 100 {
		t.Fatalf("post-release state %+v (commitment is non-refundable)", state)
	}
	ciBudgetTotalsZero(t, s, workspace)

	// Plan-digest corruption must block release even with all proofs.
	runner2, plan2 := ciBudgetSeed(t, s, workspace, "budget_op_2", 2, now)
	res2, err := s.reserveCIProvisioningBudget(ctx, workspace, runner2, policy.ProfileDigest, plan2)
	if err != nil {
		t.Fatal(err)
	}
	ciBudgetProofs(t, s, workspace, runner2, now)
	wrongDigest := strings.Repeat("f", 64)
	if _, err := s.pool.Exec(ctx, `UPDATE ci_provisioning_reservations SET plan_digest=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner2, wrongDigest); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner2); !errors.Is(err, ErrConflict) {
		t.Fatalf("release with corrupted plan digest: %v", err)
	}
	state = ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 {
		t.Fatalf("corrupted release freed capacity: %+v", state)
	}
	// Restoring the true digest lets the same proofs release normally.
	if _, err := s.pool.Exec(ctx, `UPDATE ci_provisioning_reservations SET plan_digest=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner2, res2.PlanDigest); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner2); err != nil {
		t.Fatalf("release after digest restore: %v", err)
	}
	_ = res
}

func TestCIProvisioningBudgetPermanentCeiling(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	policy.ReservationMicros = 100
	ciBudgetGrant(t, s, "credit_1", 100, policy)

	runner1, plan1 := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	ciBudgetReserve(t, s, workspace, runner1, policy.ProfileDigest, plan1)
	ciBudgetProofs(t, s, workspace, runner1, now)
	if err := s.releaseCIProvisioningCapacity(context.Background(), workspace, runner1); err != nil {
		t.Fatal(err)
	}
	state := ciBudgetPeek(t, s, "credit_1", 100, policy)
	if state.Active != 0 || state.CommittedMicros != 100 {
		t.Fatalf("state %+v", state)
	}

	// The freed concurrency slot cannot mint new spend authority.
	runner2, plan2 := ciBudgetSeed(t, s, workspace, "budget_op_2", 2, now)
	if _, err := s.reserveCIProvisioningBudget(context.Background(), workspace, runner2, policy.ProfileDigest, plan2); !errors.Is(err, ErrCapacity) {
		t.Fatalf("reserve beyond permanent ceiling: %v", err)
	}

	ciBudgetGrant(t, s, "credit_2", 100, policy)
	res := ciBudgetReserve(t, s, workspace, runner2, policy.ProfileDigest, plan2)
	if res.ReservedMicros != 100 {
		t.Fatalf("reservation %+v", res)
	}
	state = ciBudgetPeek(t, s, "credit_1", 100, policy)
	if state.CommittedMicros != 200 || state.Active != 1 {
		t.Fatalf("state %+v", state)
	}
}

func TestCIProvisioningBudgetClosureGuard(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", 1000, policy)
	runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)
	ctx := context.Background()

	// Fully proven cleanup but an active funding reservation still protects.
	ciBudgetProofs(t, s, workspace, runner, now)
	if !ciProtection(t, s, workspace) {
		t.Fatal("active provisioning reservation did not protect workspace")
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if ciProtection(t, s, workspace) {
		t.Fatal("workspace still protected after release")
	}
}

func TestCIProvisioningBudgetMaxConnsOne(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	ciBudgetReset(t, s)

	old := s.pool
	cfg := *old.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.pool = pool
	defer func() {
		s.pool = old
		pool.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = s.authorizeCIProvisioningBudget(ctx, ciProvisioningAuthorization{ID: "credit_1", Policy: policy, AmountMicros: 1000}); err != nil {
		t.Fatalf("authorize under MaxConns=1: %v", err)
	}
	runner, plan := ciBudgetSeed(t, s, workspace, "budget_op_1", 1, now)
	res, err := s.reserveCIProvisioningBudget(ctx, workspace, runner, policy.ProfileDigest, plan)
	if err != nil {
		t.Fatalf("reserve under MaxConns=1: %v", err)
	}
	ciBudgetProofs(t, s, workspace, runner, now)
	if err = s.releaseCIProvisioningCapacity(ctx, workspace, runner); err != nil {
		t.Fatalf("release under MaxConns=1: %v", err)
	}
	if res.ReservedMicros != 100 {
		t.Fatalf("reservation %+v", res)
	}
}
