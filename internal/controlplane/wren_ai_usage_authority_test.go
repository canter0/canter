package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func aiReserveRun(t *testing.T, s *Store, c Conversation, run OperatorRun, key, purpose string) WrenAIReservation {
	t.Helper()
	in := aiTestInput(c, key, "operator", 50)
	in.RunID = run.ID
	in.RunLease = run.Lease
	in.Purpose = purpose
	if purpose == "title" {
		in.Estimate = json.RawMessage(`{"maxOutputTokens":64}`)
	}
	r, e := s.ReserveWrenAIUsage(context.Background(), in)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func requireAIDispatchDenied(t *testing.T, s *Store, c Conversation, id string) {
	t.Helper()
	if e := s.DispatchWrenAIUsage(context.Background(), c.WorkspaceID, id); e == nil {
		t.Fatal("revoked/stale work reached paid dispatch")
	}
	var state string
	var dispatched bool
	if e := s.pool.QueryRow(context.Background(), `SELECT state,dispatched_at IS NOT NULL FROM wren_ai_reservations WHERE id=$1`, id).Scan(&state, &dispatched); e != nil || state != "reserved" || dispatched {
		t.Fatalf("dispatch marker changed %s %v %v", state, dispatched, e)
	}
}
func TestWrenAIDispatchFencesCancelledExpiredAndStaleRunLeases(t *testing.T) {
	for _, mode := range []string{"cancelled", "expired", "stale"} {
		t.Run(mode, func(t *testing.T) {
			s, c, run := wrenJobFixture(t)
			ctx := context.Background()
			s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
			r := aiReserveRun(t, s, c, run, "before-revoke", "")
			switch mode {
			case "cancelled":
				if e := s.finishOperator(ctx, run, "cancelled", ""); e != nil {
					t.Fatal(e)
				}
			case "expired":
				s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, run.ID)
			case "stale":
				s.pool.Exec(ctx, `UPDATE operator_runs SET lease_token='new-owner' WHERE id=$1`, run.ID)
			}
			requireAIDispatchDenied(t, s, c, r.ID)
		})
	}
}
func TestWrenAIDispatchFencesCancelledOrArchivedACPWithoutFalseTerminalState(t *testing.T) {
	for _, mode := range []string{"cancelled", "archived"} {
		t.Run(mode, func(t *testing.T) {
			h, c, run, job, _, _ := harnessFixture(t)
			s := h.service.Store
			ctx := context.Background()
			s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
			in := aiTestInput(c, "queued-before-revocation", "acp", 50)
			in.RunID = run.ID
			in.RunLease = run.Lease
			r, e := s.ReserveWrenAIUsage(ctx, in)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "cancelled" {
				if _, e = s.CancelWrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID); e != nil {
					t.Fatal(e)
				}
			} else {
				archiveLifecycle(t, s, c, 0, true)
			}
			var status string
			s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, run.ID).Scan(&status)
			if status != "running" {
				t.Fatalf("repro must retain actual ACP stopping state, got%s", status)
			}
			requireAIDispatchDenied(t, s, c, r.ID)
		})
	}
}
func TestWrenAIDispatchResponsibilityRevocationFenced(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
	routine, e := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if e != nil {
		t.Fatal(e)
	}
	s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, routine.ID, "ai-dispatch-test", "", nil)
	if _, e = s.DispatchWrenResponsibilities(ctx, "test", 10); e != nil {
		t.Fatal(e)
	}
	run, ok, e := s.claimOperator(ctx)
	if e != nil || !ok {
		t.Fatal(e)
	}
	r := aiReserveRun(t, s, c, run, "routine-before-revoke", "")
	edit := responsibilityEdit(routine)
	edit.State = "revoked"
	if _, e = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit); e != nil {
		t.Fatal(e)
	}
	requireAIDispatchDenied(t, s, c, r.ID)
}
func TestWrenAITitleExceptionIsClaimedBoundedAndSuccessfulOnly(t *testing.T) {
	for _, mode := range []string{"valid", "ordinary", "cancelled", "unclaimed", "oversized", "old"} {
		t.Run(mode, func(t *testing.T) {
			s, c, _ := operatorFixture(t)
			ctx := context.Background()
			s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
			s.EnqueueOperator(ctx, c, "title-source", "first request", "test", nil)
			run, ok, e := s.claimOperator(ctx)
			if e != nil || !ok {
				t.Fatal(e)
			}
			if mode != "unclaimed" {
				if _, e = s.claimConversationTitle(ctx, run); e != nil {
					t.Fatal(e)
				}
			}
			purpose := "title"
			if mode == "ordinary" {
				purpose = ""
			}
			r := aiReserveRun(t, s, c, run, "async-title", purpose)
			if mode == "oversized" {
				s.pool.Exec(ctx, `UPDATE wren_ai_reservations SET estimate='{"maxOutputTokens":4096}' WHERE id=$1`, r.ID)
			}
			status := "completed"
			if mode == "cancelled" {
				status = "cancelled"
			}
			if e = s.finishOperator(ctx, run, status, "a real completed reply"); e != nil {
				t.Fatal(e)
			}
			if mode == "old" {
				s.pool.Exec(ctx, `UPDATE operator_runs SET completed_at=$2 WHERE id=$1`, run.ID, time.Now().Add(-2*time.Minute))
			}
			if mode == "valid" {
				if e = s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID); e != nil {
					t.Fatalf("legitimate async title blocked %v", e)
				}
			} else {
				requireAIDispatchDenied(t, s, c, r.ID)
			}
		})
	}
}

func TestWrenAIDispatchWaitsForConcurrentJobRevocationFence(t *testing.T) {
	h, c, run, job, _, _ := harnessFixture(t)
	s := h.service.Store
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.ConfigureWrenAIAllowance(ctx, c.WorkspaceID, 1000)
	in := aiTestInput(c, "concurrent-cancel", "acp", 50)
	in.RunID = run.ID
	in.RunLease = run.Lease
	r, e := s.ReserveWrenAIUsage(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if e = authorizeWrenJobOriginTx(ctx, tx, c.WorkspaceID, c.AccountID, c.ID); e != nil {
		t.Fatal(e)
	}
	if e = cancelWrenJobRowTx(ctx, tx, job); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.DispatchWrenAIUsage(ctx, c.WorkspaceID, r.ID) }()
	select {
	case e := <-done:
		t.Fatalf("dispatchpasseduncommittedrevocationfence %v", e)
	case <-time.After(40 * time.Millisecond):
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("concurrentcancelstilldispatched")
		}
	case <-ctx.Done():
		t.Fatal("revocationdispatchlockdeadlocked")
	}
	requireAIDispatchDenied(t, s, c, r.ID)
}
