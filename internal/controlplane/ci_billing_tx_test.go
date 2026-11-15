package controlplane

import (
	"context"
	"testing"
)

func ciAssertBudgetUncommitted(t *testing.T, s *Store, workspace, runner string) {
	t.Helper()
	var unset bool
	var reserved int64
	err := s.pool.QueryRow(context.Background(), `SELECT execution_started_at IS NULL AND deadline_at IS NULL FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&unset)
	if err != nil {
		t.Fatal(err)
	}
	err = s.pool.QueryRow(context.Background(), `SELECT COALESCE(SUM(reserved_units),0) FROM ci_budget_reservations WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&reserved)
	if err != nil {
		t.Fatal(err)
	}
	if !unset || reserved != 0 {
		t.Fatalf("reservation escaped caller transaction: unset=%v reserved=%d", unset, reserved)
	}
}

func TestCIReserveBudgetTxRollback(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	quote, err := s.reserveCIBudgetTx(ctx, tx, workspace, runner, start, 7200)
	if err != nil || !quote.PaidOverrunEnabled || quote.MaxChargeUnits != 7200000 {
		t.Fatalf("unexpected transactional quote: %+v %v", quote, err)
	}
	var reserved int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_units),0) FROM ci_budget_reservations WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&reserved); err != nil || reserved != quote.MaxChargeUnits {
		t.Fatalf("reservation not visible inside caller transaction: %d %v", reserved, err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatalf("helper committed caller transaction: %v", err)
	}
	ciAssertBudgetUncommitted(t, s, workspace, runner)
}

func TestCIReserveBudgetTxCommitAndRetry(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	quote, err := s.reserveCIBudgetTx(ctx, tx, workspace, runner, start, 7200)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.reserveCIBudgetTx(ctx, tx, workspace, runner, start, 21600)
	if err != nil || retry != quote {
		t.Fatalf("retry changed uncommitted clock or budget: %+v %+v %v", quote, retry, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	retry, err = s.reserveCIBudget(ctx, workspace, runner, start, 21600)
	if err != nil || retry != quote {
		t.Fatalf("committed clock or budget was not recovered: %+v %+v %v", quote, retry, err)
	}
	var matchingClock bool
	var reserved int64
	err = s.pool.QueryRow(ctx, `SELECT execution_started_at=$3 AND deadline_at=$4 FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner, quote.StartedAt, quote.DeadlineAt).Scan(&matchingClock)
	if err != nil || !matchingClock {
		t.Fatalf("wrong committed execution clock: %v %v", matchingClock, err)
	}
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_units),0) FROM ci_budget_reservations WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&reserved)
	if err != nil || reserved != quote.MaxChargeUnits {
		t.Fatalf("retry duplicated budget: %d %v", reserved, err)
	}
}

func TestCIReserveBudgetTxSiblingFailureRollsBack(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = s.reserveCIBudgetTx(ctx, tx, workspace, runner, start, 7200); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT 1/0`); err == nil {
		t.Fatal("expected the sibling lifecycle write to fail")
	}
	if err = tx.Commit(ctx); err == nil {
		t.Fatal("committed an aborted caller transaction")
	}
	ciAssertBudgetUncommitted(t, s, workspace, runner)
}
