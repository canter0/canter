package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func ciProtection(t *testing.T, s *Store, workspace string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	protected, err := s.ciWorkspaceProtectedTx(ctx, tx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return protected
}

func TestCIResourcesProtectLiveRunnersAndPendingFinancials(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	if !ciProtection(t, s, workspace) {
		t.Fatal("live CI runner did not protect workspace")
	}
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3601); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted' WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if !ciProtection(t, s, workspace) {
		t.Fatal("destroyed VM lost its unsettled financial protection")
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(3601*time.Second)); err != nil {
		t.Fatal(err)
	}
	if ciProtection(t, s, workspace) {
		t.Fatal("settled deleted runner still blocked closure")
	}
}

func TestCIResourcesProtectOrphanReservation(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3601); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted',paid_overrun_enabled=false,execution_started_at=NULL WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if !ciProtection(t, s, workspace) {
		t.Fatal("orphaned budget commitment did not protect workspace")
	}
}

func TestCIResourcesClosureRefusesLiveStateWithoutMutation(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, false, start)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.closeCIWorkspaceTx(ctx, tx, workspace); !errors.Is(err, ErrConflict) {
		t.Fatal("closure accepted a live runner")
	}
	var installation string
	if err = tx.QueryRow(ctx, `SELECT installation_id FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&installation); err != nil || installation == "" {
		t.Fatalf("failed closure changed runner ownership: %v", err)
	}
}

func TestCIResourcesClosureErasesUnbilledRunnerHistory(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, false, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3600); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted' WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.closeCIWorkspaceTx(ctx, tx, workspace); err != nil {
		t.Fatal(err)
	}
	financial, err := s.ciFinancialHistoryTx(ctx, tx, workspace)
	if err != nil || financial {
		t.Fatalf("unbilled runner invented financial history: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var present bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ci_runners WHERE workspace_id=$1) OR EXISTS(SELECT 1 FROM ci_jobs WHERE workspace_id=$1)`, workspace).Scan(&present)
	if err != nil || present {
		t.Fatalf("unbilled operational activity survived closure: %v", err)
	}
}

func TestCIResourcesClosureErasesOperationalMetadataKeepsExactLedger(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 7200); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(ctx, `UPDATE ci_runners SET phase='deleted',document='{"fixture":"erase operational data"}'::jsonb WHERE workspace_id=$1 AND id=$2`, workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `UPDATE ci_jobs SET runner_id=$2 WHERE workspace_id=$1 AND id='ci_financial_job_1'`, workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO ci_job_logs(workspace_id,job_id,sequence,stream,content) VALUES($1,'ci_financial_job_1',0,'stdout','fixture private log')`,
		`INSERT INTO ci_job_metrics(workspace_id,job_id,sampled_at,document) VALUES($1,'ci_financial_job_1',now(),'{}'::jsonb)`,
		`INSERT INTO ci_job_tests(workspace_id,job_id,id,document) VALUES($1,'ci_financial_job_1','test_fixture','{}'::jsonb)`,
		`INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) SELECT $1,p,'human',created_by FROM ci_github_installations CROSS JOIN (VALUES('ci'),('sandboxes')) products(p) WHERE id='ci_financial_installation' ON CONFLICT DO NOTHING`,
	} {
		if _, err = s.pool.Exec(ctx, query, workspace); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO ci_connect_states(state_hash,workspace_id,account_id,expires_at) SELECT $1,$2,created_by,now()+interval '10 minutes' FROM ci_github_installations WHERE id='ci_financial_installation'`, strings.Repeat("f", 64), workspace)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.closeCIWorkspaceTx(ctx, tx, workspace); err != nil {
		t.Fatal(err)
	}
	financial, err := s.ciFinancialHistoryTx(ctx, tx, workspace)
	if err != nil || !financial {
		t.Fatalf("closure lost financial-history protection: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ci_jobs", "ci_repositories", "ci_github_installations", "ci_connect_states", "ci_settings", "ci_job_logs", "ci_job_metrics", "ci_job_tests"} {
		var count int64
		if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE workspace_id=$1`, workspace).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained operational data: %d %v", table, count, err)
		}
	}
	var sanitized bool
	err = s.pool.QueryRow(ctx, `SELECT installation_id IS NULL AND trigger_job_id IS NULL AND assigned_job_id IS NULL AND github_runner_id IS NULL AND document='{}'::jsonb AND phase='deleted' FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&sanitized)
	if err != nil || !sanitized {
		t.Fatalf("financial runner tombstone retained private operational state: %v", err)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 7200000 || cents != 12 || reserved != 0 {
		t.Fatalf("closure altered exact usage: %d %d %d", units, cents, reserved)
	}
	var ciEnabled, sandboxesEnabled bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_products WHERE workspace_id=$1 AND product='ci'),EXISTS(SELECT 1 FROM workspace_products WHERE workspace_id=$1 AND product='sandboxes')`, workspace).Scan(&ciEnabled, &sandboxesEnabled)
	if err != nil || ciEnabled || !sandboxesEnabled {
		t.Fatalf("closure changed wrong product flags: ci=%v sandboxes=%v %v", ciEnabled, sandboxesEnabled, err)
	}
}

func TestCIResourcesPrivateWorkspaceClosureRetainsFractionalFinancialShell(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	ctx := context.Background()
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3601); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(3601*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE ci_runners SET phase='deleted' WHERE workspace_id=$1`,
		`UPDATE workspace_billing SET customer_id=NULL,subscription_id=NULL,status='canceled' WHERE workspace_id=$1`,
		`DELETE FROM wren_ai_allowances WHERE workspace_id=$1`,
	} {
		if _, err := s.pool.Exec(ctx, query, workspace); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = s.deletePrivateWorkspaceTx(ctx, tx, workspace); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var name string
	if err = s.pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id=$1`, workspace).Scan(&name); err != nil || name != "Closed workspace" {
		t.Fatalf("fractional CI charges lost their financial workspace shell: %q %v", name, err)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 2000 || cents != 0 || reserved != 0 {
		t.Fatalf("account closure erased a sub-cent charge: %d %d %d", units, cents, reserved)
	}
	var remainder int64
	if err = s.pool.QueryRow(ctx, `SELECT remainder_units FROM ci_usage_balances WHERE workspace_id=$1`, workspace).Scan(&remainder); err != nil || remainder != 2000 {
		t.Fatalf("account closure lost fractional carry: %d %v", remainder, err)
	}
	var sanitized bool
	if err = s.pool.QueryRow(ctx, `SELECT document='{}'::jsonb AND installation_id IS NULL AND trigger_job_id IS NULL FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&sanitized); err != nil || !sanitized {
		t.Fatalf("closed financial workspace retained private CI operational metadata: %v", err)
	}
	var installations, jobs, members int64
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ci_github_installations WHERE workspace_id=$1),(SELECT count(*) FROM ci_jobs WHERE workspace_id=$1),(SELECT count(*) FROM memberships WHERE workspace_id=$1)`, workspace).Scan(&installations, &jobs, &members)
	if err != nil || installations != 0 || jobs != 0 || members != 0 {
		t.Fatalf("closed workspace retained operational access: %d %d %d %v", installations, jobs, members, err)
	}
	newAccount, newWorkspace, _, err := s.Signup(ctx, "ci-reconnect-after-closure@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) VALUES($1,'ci','human',$2)`, newWorkspace.ID, newAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	stateHash := strings.Repeat("d", 64)
	if err = s.CreateCIConnectState(ctx, newWorkspace.ID, newAccount.ID, stateHash, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err = s.BindCIInstallation(ctx, newWorkspace.ID, newAccount.ID, stateHash, 123, 456, "financial-tests", "Organization", []ciGitHubRepo{{ID: 789, FullName: "financial-tests/repo", Private: true}})
	if err != nil {
		t.Fatalf("closed workspace prevented fresh authorized installation binding: %v", err)
	}
	units, cents, reserved = ciFinancialTotals(t, s, workspace)
	if units != 2000 || cents != 0 || reserved != 0 {
		t.Fatal("reconnecting an installation changed the old workspace's financial history")
	}
}
