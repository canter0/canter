package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/pricing"
)

func ciFinancialFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	s, conversation, _ := operatorFixture(t)
	start := time.Date(2026, time.November, 30, 22, 59, 58, 0, time.UTC)
	s.now = func() time.Time { return start.Add(6 * time.Hour) }
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,created_by) VALUES('ci_financial_installation',$1,123,456,'financial-tests','Organization',$2)`, conversation.WorkspaceID, conversation.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name) VALUES($1,'ci_financial_installation',789,'financial-tests/repo')`, conversation.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO ci_settings(workspace_id,paid_overrun_enabled) VALUES($1,true)`, conversation.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end) VALUES($1,'cus_ci','sub_ci','active',$2,$3) ON CONFLICT(workspace_id) DO UPDATE SET customer_id=EXCLUDED.customer_id,subscription_id=EXCLUDED.subscription_id,status=EXCLUDED.status,period_start=EXCLUDED.period_start,period_end=EXCLUDED.period_end`, conversation.WorkspaceID, start.Add(-24*time.Hour), start.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return s, conversation.WorkspaceID, start
}

func ciFinancialRunner(t *testing.T, s *Store, workspace, size string, number int, paid bool, createdAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	job, runner := fmt.Sprintf("ci_financial_job_%d", number), fmt.Sprintf("ci_financial_runner_%d", number)
	_, err := s.pool.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document) VALUES($1,$2,'ci_financial_installation',789,$3,$3,'queued','{}'::jsonb)`, job, workspace, number+1000)
	if err != nil {
		t.Fatal(err)
	}
	price, ok := pricing.FindCI(size)
	if !ok {
		t.Fatal("invalid test size")
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,phase,document,rate_version,rate_micros_per_minute,paid_overrun_enabled,created_at) VALUES($1,$2,'ci_financial_installation',$3,$4,'queued','{}'::jsonb,$5,$6,$7,$8)`, runner, workspace, size, job, pricing.CIRateVersion, price.MicrosPerMinute, paid, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func ciFinancialTotals(t *testing.T, s *Store, workspace string) (units, cents, reserved int64) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT SUM(delta_units) FROM ci_usage_ledger WHERE workspace_id=$1),0),COALESCE((SELECT SUM(amount_cents) FROM billing_usage_events WHERE workspace_id=$1 AND resource='ci'),0),COALESCE((SELECT SUM(reserved_units) FROM ci_budget_reservations WHERE workspace_id=$1),0)`, workspace).Scan(&units, &cents, &reserved)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestCIBillingFreeHourAndUnapprovedOverrun(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-4", 1, false, start)
	quote, err := s.reserveCIBudget(context.Background(), workspace, runner, start, 7200)
	if err != nil || quote.PaidOverrunEnabled || quote.MaxElapsedSeconds != 3600 || quote.MaxChargeUnits != 0 {
		t.Fatalf("unexpected free quote: %+v %v", quote, err)
	}
	if err = s.settleCIBudget(context.Background(), workspace, runner, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 0 || cents != 0 || reserved != 0 {
		t.Fatalf("charged unapproved runtime: %d %d %d", units, cents, reserved)
	}
}

func TestCIBillingReserveSettleAndRetry(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	quote, err := s.reserveCIBudget(ctx, workspace, runner, start, 7200)
	if err != nil || !quote.PaidOverrunEnabled || quote.MaxChargeUnits != 7200000 {
		t.Fatalf("unexpected quote: %+v %v", quote, err)
	}
	retry, err := s.reserveCIBudget(ctx, workspace, runner, start, 21600)
	if err != nil || retry != quote {
		t.Fatalf("reservation retry changed quote: %+v %v", retry, err)
	}
	if _, err = s.reserveCIBudget(ctx, workspace, runner, start.Add(time.Hour), 7200); !errors.Is(err, ErrConflict) {
		t.Fatal("allowed the free-hour clock to reset")
	}
	stop := start.Add(2 * time.Hour)
	for i := 0; i < 2; i++ {
		if err = s.settleCIBudget(ctx, workspace, runner, stop); err != nil {
			t.Fatal(err)
		}
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 7200000 || cents != 12 || reserved != 0 {
		t.Fatalf("incorrect settlement: units=%d cents=%d reserved=%d", units, cents, reserved)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if err = s.settleCIBudget(ctx, workspace, runner, stop); err != nil {
		t.Fatalf("settled retry failed after billing changed: %v", err)
	}
	if err = s.settleCIBudget(ctx, workspace, runner, stop.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatal("changed a settled duration")
	}
}

func TestCIBillingMonthlySplitAndFractionalCarry(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3604); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(3604*time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT to_char(period_start,'YYYY-MM-DD'),delta_units FROM ci_usage_ledger WHERE workspace_id=$1 AND runner_id=$2 ORDER BY period_start`, workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	expected := []struct {
		period string
		units  int64
	}{{"2026-11-01", 2000}, {"2026-12-01", 6000}}
	index := 0
	for rows.Next() {
		var period string
		var units int64
		if err = rows.Scan(&period, &units); err != nil {
			t.Fatal(err)
		}
		if index >= len(expected) || period != expected[index].period || units != expected[index].units {
			t.Fatalf("incorrect monthly segment: %s %d", period, units)
		}
		index++
	}
	rows.Close()
	if rows.Err() != nil || index != len(expected) {
		t.Fatalf("missing monthly segments: %v", rows.Err())
	}
	for i := 2; i <= 15; i++ {
		runner = ciFinancialRunner(t, s, workspace, "linux-x64-2", i, true, start)
		if _, err = s.reserveCIBudget(ctx, workspace, runner, start, 3620); err != nil {
			t.Fatal(err)
		}
		if err = s.settleCIBudget(ctx, workspace, runner, start.Add(3620*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	runner = ciFinancialRunner(t, s, workspace, "linux-x64-2", 16, true, start)
	if _, err = s.reserveCIBudget(ctx, workspace, runner, start, 3616); err != nil {
		t.Fatal(err)
	}
	if err = s.settleCIBudget(ctx, workspace, runner, start.Add(3616*time.Second)); err != nil {
		t.Fatal(err)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 600000 || cents != 1 || reserved != 0 {
		t.Fatalf("lost cross-runner fractions: %d %d %d", units, cents, reserved)
	}
}

func TestCIBillingConcurrentReservationsRespectCap(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE ci_settings SET monthly_spend_limit_cents=1 WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	start = start.Add(2 * time.Hour)
	first := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	second := ciFinancialRunner(t, s, workspace, "linux-x64-2", 2, true, start)
	results := make(chan ciBudgetQuote, 2)
	failures := make(chan error, 2)
	var wg sync.WaitGroup
	for _, runner := range []string{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			quote, err := s.reserveCIBudget(ctx, workspace, runner, start, 7200)
			results <- quote
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total, paid int64
	for quote := range results {
		total += quote.MaxChargeUnits
		if quote.PaidOverrunEnabled {
			paid++
			if quote.MaxElapsedSeconds != 3900 {
				t.Fatalf("wrong one-cent runtime limit: %+v", quote)
			}
		} else if quote.MaxElapsedSeconds != 3600 {
			t.Fatalf("capacity consumed the free hour: %+v", quote)
		}
	}
	if total != 600000 || paid != 1 {
		t.Fatalf("overcommitted cap: %d %d", total, paid)
	}
}

func TestCIBillingCapsCleanupTimeAndRejectsForeignScope(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-4", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3660); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, "another-workspace", runner, start.Add(3660*time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign workspace accessed billing")
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 240000 || cents != 0 || reserved != 0 {
		t.Fatalf("charged cleanup beyond deadline: %d %d %d", units, cents, reserved)
	}
}

func TestCIBillingInactiveAccountRollsBackSettlement(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 7200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(2*time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatal("charged inactive billing")
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 0 || cents != 0 || reserved != 7200000 {
		t.Fatalf("failed settlement was partially committed: %d %d %d", units, cents, reserved)
	}
}

func TestCIBillingSpendLimitIncludesReservedCommitments(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	start = start.Add(2 * time.Hour)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-4", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 7200); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.checkCISpendLimit(ctx, tx, workspace, 23); !errors.Is(err, ErrConflict) {
		t.Fatal("lowered cap below existing commitments")
	}
	if err = s.checkCISpendLimit(ctx, tx, workspace, 24); err != nil {
		t.Fatal(err)
	}
}
