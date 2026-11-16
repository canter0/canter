package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCodingTakeoverRequiresReviewAndConfirmedProcessStop(t *testing.T) {
	h, c, run, job, grant, token := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	var review *computerCommandsRunning
	if _, err := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, grant.ComputerID, "human"); !errors.As(err, &review) || len(review.CodingJobs) != 1 || review.CodingJobs[0].ID != job.ID {
		t.Fatalf("missing exact coding review: %#v %v", review, err)
	}
	var stopping bool
	if err := s.pool.QueryRow(ctx, `SELECT stop_requested FROM wren_harness_executions WHERE job_id=$1`, job.ID).Scan(&stopping); err != nil || stopping {
		t.Fatalf("review stopped work: %v %v", stopping, err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := h.setComputerControl(ctx, c.WorkspaceID, c.AccountID, grant.ComputerID, "human", []string{job.ID})
		result <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !stopping && time.Now().Before(deadline) {
		if err := s.pool.QueryRow(ctx, `SELECT stop_requested FROM wren_harness_executions WHERE job_id=$1`, job.ID).Scan(&stopping); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !stopping {
		t.Fatal("stop never requested")
	}
	select {
	case err := <-result:
		t.Fatalf("ownership changed before process ack: %v", err)
	default:
	}
	var holder, status string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(control_account_id,'') FROM wren_computers WHERE id=$1`, grant.ComputerID).Scan(&holder); err != nil || holder != "" {
		t.Fatalf("premature ownership: %s %v", holder, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, run.ID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("premature terminal status: %s %v", status, err)
	}
	if got := brokerRequest(h, token, `{"messages":[{"role":"user","content":"continue"}]}`); got.Code < 400 {
		t.Fatal("stopping worker retained billing grant")
	}
	// Fixture models the dispatcher's terminal receipt; native acceptance is separate.
	if _, err := s.pool.Exec(ctx, `UPDATE wren_harness_executions SET process_stopped=true,status='stopped' WHERE job_id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("takeover did not finish after acknowledgement")
	}
	if err := h.requireHumanComputerControl(ctx, WrenComputer{ID: grant.ComputerID, WorkspaceID: c.WorkspaceID}, c.AccountID); err != nil {
		t.Fatal(err)
	}
}

func TestCodingTakeoverRejectsStaleReviewWithoutStoppingAnyWorker(t *testing.T) {
	h, c, _, job, grant, _ := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	var parent OperatorRun
	if err := s.pool.QueryRow(ctx, `SELECT id,lease_token FROM operator_runs WHERE id=$1`, job.ParentRunID).Scan(&parent.ID, &parent.Lease); err != nil {
		t.Fatal(err)
	}
	newer, err := h.CreateHarnessWrenJob(ctx, parent, c, "harness-call-2", "New independent work", "Build a second scoped page.")
	if err != nil {
		t.Fatal(err)
	}
	var review *computerCommandsRunning
	if _, err = h.setComputerControl(ctx, c.WorkspaceID, c.AccountID, grant.ComputerID, "human", []string{job.ID}); !errors.As(err, &review) || len(review.CodingJobs) != 2 {
		t.Fatalf("new work not reviewed: %#v %v", review, err)
	}
	var stopped int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_harness_executions WHERE job_id=ANY($1) AND stop_requested`, []string{job.ID, newer.ID}).Scan(&stopped); err != nil || stopped != 0 {
		t.Fatalf("stale review cancelled work: %d %v", stopped, err)
	}
}

func TestCodingStopWaitCannotTreatPendingOrTimeoutAsAcknowledgement(t *testing.T) {
	h, c, _, job, grant, _ := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	if err := s.requestComputerCodingStops(ctx, Conversation{ID: "wrong-origin", WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}, grant.ComputerID, []string{job.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong origin authorized: %v", err)
	}
	work, err := s.pendingComputerCodingWork(ctx, grant.ComputerID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.requestComputerCodingStops(ctx, c, grant.ComputerID, []string{job.ID}); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err = s.waitComputerCodingStops(short, grant.ComputerID, work); !errors.Is(err, ErrConflict) {
		t.Fatalf("unconfirmed stop accepted: %v", err)
	}
	var holder string
	if err = s.pool.QueryRow(ctx, `SELECT COALESCE(control_account_id,'') FROM wren_computers WHERE id=$1`, grant.ComputerID).Scan(&holder); err != nil || holder != "" {
		t.Fatalf("unconfirmed stop changed owner: %s %v", holder, err)
	}
}

func TestCodingCleanupPreservesCompletedDeliverable(t *testing.T) {
	h, c, run, job, grant, _ := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE operator_runs SET status='completed',completed_at=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.requestComputerCodingStops(ctx, c, grant.ComputerID, []string{job.ID}); err != nil {
		t.Fatal(err)
	}
	current, err := s.WrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID)
	if err != nil || current.Status != "completed" || current.ReturnSuppressed || current.CancelledAt != nil {
		t.Fatalf("cleanup erased completion: %#v %v", current, err)
	}
	var terminal string
	if err = s.pool.QueryRow(ctx, `SELECT terminal_status FROM wren_harness_executions WHERE job_id=$1`, job.ID).Scan(&terminal); err != nil || terminal != "completed" {
		t.Fatalf("cleanup changed terminal outcome: %s %v", terminal, err)
	}
}
