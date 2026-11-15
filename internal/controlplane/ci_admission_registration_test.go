package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ciAdmissionRepoID   = int64(789)
	ciAdmissionRunnerID = int64(31)
	ciAdmissionName     = "canter-test-1"
	ciAdmissionSize     = "linux-x64-2"
)

func ciAdmissionScope(createdAt time.Time) ciRunnerJobScope {
	return ciRunnerJobScope{
		RepositoryID: ciAdmissionRepoID,
		RunnerID:     ciAdmissionRunnerID,
		RunnerName:   ciAdmissionName,
		SizeID:       ciAdmissionSize,
		CreatedAt:    createdAt,
	}
}

func ciAdmissionNonce(letter string) string {
	return strings.Repeat(letter, 64)
}

func ciAdmissionEnable(t *testing.T, s *Store, workspace string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) SELECT $1,'ci','human',created_by FROM ci_github_installations WHERE id='ci_financial_installation' ON CONFLICT DO NOTHING`, workspace)
	if err != nil {
		t.Fatal(err)
	}
}

func ciAdmissionPhase(t *testing.T, s *Store, workspace, runner, phase string, deleteRequested bool) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `UPDATE ci_runners SET phase=$3,delete_requested=$4 WHERE workspace_id=$1 AND id=$2`, workspace, runner, phase, deleteRequested)
	if err != nil {
		t.Fatal(err)
	}
}

func ciAdmissionInstallationStatus(t *testing.T, s *Store, workspace, status string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `UPDATE ci_github_installations SET status=$2 WHERE workspace_id=$1`, workspace, status)
	if err != nil {
		t.Fatal(err)
	}
}

func ciAdmissionRepoEnabled(t *testing.T, s *Store, workspace string, enabled bool) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `UPDATE ci_repositories SET enabled=$2 WHERE workspace_id=$1`, workspace, enabled)
	if err != nil {
		t.Fatal(err)
	}
}

func ciAdmissionProductOff(t *testing.T, s *Store, workspace string) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, workspace)
	if err != nil {
		t.Fatal(err)
	}
}

func ciAdmissionRows(t *testing.T, s *Store, workspace, runner string) int {
	t.Helper()
	var n int
	err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_runner_admissions WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func ciAdmissionRunnerState(t *testing.T, s *Store, workspace, runner string) (string, *int64) {
	t.Helper()
	var phase string
	var ghid *int64
	err := s.pool.QueryRow(context.Background(), `SELECT phase,github_runner_id FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&phase, &ghid)
	if err != nil {
		t.Fatal(err)
	}
	return phase, ghid
}

func ciAdmissionRegistered(t *testing.T) (*Store, string, string, time.Time, string) {
	t.Helper()
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ciAdmissionEnable(t, s, workspace)
	ciAdmissionPhase(t, s, workspace, runner, "registering", false)
	nonce := ciAdmissionNonce("a")
	if err := s.registerCIRunnerAdmission(context.Background(), workspace, runner, nonce, ciAdmissionScope(start)); err != nil {
		t.Fatal(err)
	}
	return s, workspace, runner, start, nonce
}

func ciAdmissionScopeEqual(t *testing.T, got ciRunnerJobScope, want ciRunnerJobScope) {
	t.Helper()
	if got.RepositoryID != want.RepositoryID || got.RunnerID != want.RunnerID ||
		got.RunnerName != want.RunnerName || got.SizeID != want.SizeID ||
		!got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("scope mismatch: got %+v want %+v", got, want)
	}
}

func TestCIAdmissionRegistrationSuccessAndIdempotentRetry(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	nonce := ciAdmissionNonce("a")
	scope := ciAdmissionScope(start)
	ciAdmissionEnable(t, s, workspace)
	ciAdmissionPhase(t, s, workspace, runner, "registering", false)
	ctx := context.Background()

	if err := s.registerCIRunnerAdmission(ctx, workspace, runner, nonce, scope); err != nil {
		t.Fatal(err)
	}
	phase, ghid := ciAdmissionRunnerState(t, s, workspace, runner)
	if phase != "idle" || ghid == nil || *ghid != ciAdmissionRunnerID {
		t.Fatalf("unexpected runner state after registration: %s %v", phase, ghid)
	}
	if n := ciAdmissionRows(t, s, workspace, runner); n != 1 {
		t.Fatalf("expected one admission row, got %d", n)
	}

	// Exact retry is idempotent: no writes, still one row.
	if err := s.registerCIRunnerAdmission(ctx, workspace, runner, nonce, scope); err != nil {
		t.Fatal(err)
	}
	if n := ciAdmissionRows(t, s, workspace, runner); n != 1 {
		t.Fatalf("retry changed admission rows: %d", n)
	}

	// A runner that already moved to running still accepts the exact retry
	// without resetting its phase.
	ciAdmissionPhase(t, s, workspace, runner, "running", false)
	if err := s.registerCIRunnerAdmission(ctx, workspace, runner, nonce, scope); err != nil {
		t.Fatal(err)
	}
	phase, _ = ciAdmissionRunnerState(t, s, workspace, runner)
	if phase != "running" {
		t.Fatalf("idempotent retry reset phase to %s", phase)
	}

	got, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, nonce)
	if err != nil {
		t.Fatal(err)
	}
	ciAdmissionScopeEqual(t, got, scope)
}

func TestCIAdmissionRegistrationConflictingRetry(t *testing.T) {
	s, workspace, runner, start, nonce := ciAdmissionRegistered(t)
	ctx := context.Background()
	base := ciAdmissionScope(start)
	cases := []struct {
		name   string
		nonce  string
		mutate func(*ciRunnerJobScope)
	}{
		{"nonce", ciAdmissionNonce("b"), nil},
		{"repo", nonce, func(s *ciRunnerJobScope) { s.RepositoryID = 790 }},
		{"runner", nonce, func(s *ciRunnerJobScope) { s.RunnerID = 32 }},
		{"name", nonce, func(s *ciRunnerJobScope) { s.RunnerName = "canter-test-2" }},
		{"size", nonce, func(s *ciRunnerJobScope) { s.SizeID = "linux-x64-4" }},
		{"createdAt", nonce, func(s *ciRunnerJobScope) { s.CreatedAt = start.Add(time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := base
			if tc.mutate != nil {
				tc.mutate(&scope)
			}
			err := s.registerCIRunnerAdmission(ctx, workspace, runner, tc.nonce, scope)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict for %s, got %v", tc.name, err)
			}
		})
	}
	got, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, nonce)
	if err != nil {
		t.Fatal(err)
	}
	ciAdmissionScopeEqual(t, got, base)
	if n := ciAdmissionRows(t, s, workspace, runner); n != 1 {
		t.Fatalf("conflicting retries changed admission rows: %d", n)
	}
}

func TestCIAdmissionRegistrationPreInsertDenials(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s *Store, workspace, runner string)
	}{
		{"phase_queued", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "queued", false)
		}},
		{"phase_running", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "running", false)
		}},
		{"phase_deleting", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "deleting", false)
		}},
		{"phase_deleted", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "deleted", false)
		}},
		{"phase_cleanup_failed", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "cleanup_failed", false)
		}},
		{"delete_requested", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "registering", true)
		}},
		{"installation_suspended", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "registering", false)
			ciAdmissionInstallationStatus(t, s, w, "suspended")
		}},
		{"installation_removed", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "registering", false)
			ciAdmissionInstallationStatus(t, s, w, "removed")
		}},
		{"repo_disabled", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "registering", false)
			ciAdmissionRepoEnabled(t, s, w, false)
		}},
		{"product_disabled", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "registering", false)
			ciAdmissionProductOff(t, s, w)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, workspace, start := ciFinancialFixture(t)
			runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
			ciAdmissionEnable(t, s, workspace)
			tc.setup(t, s, workspace, runner)
			nonce := ciAdmissionNonce("a")
			err := s.registerCIRunnerAdmission(context.Background(), workspace, runner, nonce, ciAdmissionScope(start))
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict for %s, got %v", tc.name, err)
			}
			if n := ciAdmissionRows(t, s, workspace, runner); n != 0 {
				t.Fatalf("denied registration wrote %d admission rows", n)
			}
			_, ghid := ciAdmissionRunnerState(t, s, workspace, runner)
			if ghid != nil {
				t.Fatalf("denied registration set github_runner_id %d", *ghid)
			}
		})
	}
}

func TestCIAdmissionRegistrationScopeGetDenials(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, nonce := ciAdmissionRegistered(t)
	scope := ciAdmissionScope(start)

	// A second workspace in the same store has no CI rows at all.
	_, other, _, err := s.Signup(ctx, "cross-workspace@example.com",
		"correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ciRunnerAdmissionScope(ctx, other.ID, runner, nonce); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace scope get: %v", err)
	}
	if err = s.registerCIRunnerAdmission(ctx, other.ID, runner, nonce, scope); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace register: %v", err)
	}
	if _, err = s.ciRunnerAdmissionScope(ctx, workspace, runner, ciAdmissionNonce("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong nonce scope get: %v", err)
	}

	denials := []struct {
		name   string
		mutate func(t *testing.T, s *Store, workspace, runner string)
	}{
		{"delete_requested", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "idle", true)
		}},
		{"phase_deleted", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionPhase(t, s, w, r, "deleted", false)
		}},
		{"installation_removed", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionInstallationStatus(t, s, w, "removed")
		}},
		{"repo_disabled", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionRepoEnabled(t, s, w, false)
		}},
		{"product_disabled", func(t *testing.T, s *Store, w, r string) {
			ciAdmissionProductOff(t, s, w)
		}},
	}
	for _, tc := range denials {
		t.Run(tc.name, func(t *testing.T) {
			s2, w2, r2, _, n2 := ciAdmissionRegistered(t)
			tc.mutate(t, s2, w2, r2)
			if _, err := s2.ciRunnerAdmissionScope(ctx, w2, r2, n2); !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected not found for %s, got %v", tc.name, err)
			}
		})
	}
}

func TestCIAdmissionRegistrationConcurrentNonces(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ciAdmissionEnable(t, s, workspace)
	ciAdmissionPhase(t, s, workspace, runner, "registering", false)
	ctx := context.Background()
	scope := ciAdmissionScope(start)
	nonces := []string{ciAdmissionNonce("a"), ciAdmissionNonce("b")}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, nonce := range nonces {
		wg.Add(1)
		go func(i int, nonce string) {
			defer wg.Done()
			errs[i] = s.registerCIRunnerAdmission(ctx, workspace, runner, nonce, scope)
		}(i, nonce)
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatal("both concurrent registrations succeeded")
			}
			winner = i
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if winner < 0 {
		t.Fatal("neither concurrent registration succeeded")
	}
	if n := ciAdmissionRows(t, s, workspace, runner); n != 1 {
		t.Fatalf("expected exactly one admission row, got %d", n)
	}
	loser := 1 - winner
	if _, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, nonces[winner]); err != nil {
		t.Fatalf("winning nonce does not load: %v", err)
	}
	if _, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, nonces[loser]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("losing nonce unexpectedly loads: %v", err)
	}
}

func TestCIAdmissionRegistrationWorkspaceClosePrivacy(t *testing.T) {
	s, workspace, runner, start, _ := ciAdmissionRegistered(t)
	ctx := context.Background()

	// Legitimate financial history through the existing reserve/settle
	// path so the runner row is retained by the ledger tombstone guard.
	if _, err := s.reserveCIBudget(ctx, workspace, runner, start, 3601); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCIBudget(ctx, workspace, runner, start.Add(3601*time.Second)); err != nil {
		t.Fatal(err)
	}
	ciAdmissionPhase(t, s, workspace, runner, "deleted", false)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.closeCIWorkspaceTx(ctx, tx, workspace); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := ciAdmissionRows(t, s, workspace, runner); n != 0 {
		t.Fatalf("workspace close retained %d admission rows", n)
	}
	// The runner survives via ledger history; only the admission
	// identity must be gone.
	phase, _ := ciAdmissionRunnerState(t, s, workspace, runner)
	if phase != "deleted" {
		t.Fatalf("ledger-retained runner missing or wrong phase: %s", phase)
	}
}

func TestCIAdmissionRegistrationMalformedInputs(t *testing.T) {
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")
	base := ciAdmissionScope(start)
	withScope := func(mutate func(*ciRunnerJobScope)) ciRunnerJobScope {
		scope := base
		mutate(&scope)
		return scope
	}
	cases := []struct {
		name      string
		workspace string
		runner    string
		admission string
		scope     ciRunnerJobScope
	}{
		{"nonce_short", workspace, runner, strings.Repeat("a", 63), base},
		{"nonce_upper", workspace, runner, strings.Repeat("A", 64), base},
		{"nonce_nonhex", workspace, runner, strings.Repeat("g", 64), base},
		{"nonce_empty", workspace, runner, "", base},
		{"workspace_empty", "", runner, nonce, base},
		{"runner_empty", workspace, "", nonce, base},
		{"repo_zero", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.RepositoryID = 0 })},
		{"repo_negative", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.RepositoryID = -1 })},
		{"runnerid_zero", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.RunnerID = 0 })},
		{"runnerid_negative", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.RunnerID = -5 })},
		{"name_invalid", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.RunnerName = "bad name!" })},
		{"name_too_long", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.RunnerName = strings.Repeat("n", 65) })},
		{"size_unknown", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.SizeID = "linux-x64-9" })},
		{"created_zero", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.CreatedAt = time.Time{} })},
		{"created_future", workspace, runner, nonce, withScope(func(s *ciRunnerJobScope) { s.CreatedAt = start.Add(7 * time.Hour) })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.registerCIRunnerAdmission(ctx, tc.workspace, tc.runner, tc.admission, tc.scope)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict for %s, got %v", tc.name, err)
			}
		})
	}
	// The scope read applies the same fail-fast input validation.
	for _, bad := range []string{"", "bad", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if _, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("scope get accepted malformed nonce %q: %v", bad, err)
		}
	}
	if n := ciAdmissionRows(t, s, workspace, runner); n != 0 {
		t.Fatalf("malformed inputs wrote %d admission rows", n)
	}
	_, ghid := ciAdmissionRunnerState(t, s, workspace, runner)
	if ghid != nil {
		t.Fatalf("malformed inputs set github_runner_id %d", *ghid)
	}
}

func TestCIAdmissionRegistrationUniqueConflictsRollback(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, secondNonce string, secondRunnerID int64) {
		s, workspace, runner1, start, nonceA := ciAdmissionRegistered(t)
		runner2 := ciFinancialRunner(t, s, workspace, "linux-x64-2", 2, true, start)
		ciAdmissionPhase(t, s, workspace, runner2, "registering", false)
		scope2 := ciRunnerJobScope{
			RepositoryID: ciAdmissionRepoID,
			RunnerID:     secondRunnerID,
			RunnerName:   "canter-test-2",
			SizeID:       ciAdmissionSize,
			CreatedAt:    start,
		}
		err := s.registerCIRunnerAdmission(ctx, workspace, runner2, secondNonce, scope2)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected conflict, got %v", err)
		}
		if n := ciAdmissionRows(t, s, workspace, runner2); n != 0 {
			t.Fatalf("failed registration left %d admission rows", n)
		}
		phase, ghid := ciAdmissionRunnerState(t, s, workspace, runner2)
		if phase != "registering" || ghid != nil {
			t.Fatalf("runner2 mutated by failed registration: phase=%s ghid=%v", phase, ghid)
		}
		got, err := s.ciRunnerAdmissionScope(ctx, workspace, runner1, nonceA)
		if err != nil {
			t.Fatal(err)
		}
		ciAdmissionScopeEqual(t, got, ciAdmissionScope(start))
	}
	t.Run("same_workspace_nonce", func(t *testing.T) {
		run(t, ciAdmissionNonce("a"), ciAdmissionRunnerID+1)
	})
	t.Run("same_github_runner_id", func(t *testing.T) {
		run(t, ciAdmissionNonce("b"), ciAdmissionRunnerID)
	})
}
