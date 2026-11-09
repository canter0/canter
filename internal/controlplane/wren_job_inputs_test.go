package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestWrenJobInputsDurableCorrectionAndReplay(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	j := requireWrenJob(t, s, c, parent, "steering-worker")
	first, err := s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID, "correction-1", "Use Tuesday, preserving the existing scope.")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID, "correction-1", first.Content)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay: %#v %v", again, err)
	}
	if _, err = s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID, "correction-1", "Different content"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if err = s.finishOperator(ctx, parent, "completed", "Worker started"); err != nil {
		t.Fatal(err)
	}
	child, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || child.ID != j.ChildRunID {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err = s.finishOperator(ctx, child, "completed", "Old result"); !errors.Is(err, errWrenJobInputPending) {
		t.Fatalf("lost accepted correction: %v", err)
	}
	messages, err := s.applyWrenJobInputs(ctx, child, []modelMessage{{Role: "user", Content: j.Prompt}})
	if err != nil || len(messages) != 2 || !strings.Contains(messages[1].Content, first.Content) {
		t.Fatalf("delivery: %#v %v", messages, err)
	}
	var applied bool
	var checkpoint []modelMessage
	if err = s.pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL FROM wren_job_inputs WHERE id=$1`, first.ID).Scan(&applied); err != nil || !applied {
		t.Fatalf("applied %v %v", applied, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT checkpoint FROM operator_runs WHERE id=$1`, child.ID).Scan(&checkpoint); err != nil || len(checkpoint) != 2 {
		t.Fatalf("checkpoint %#v %v", checkpoint, err)
	}
	replayed, err := s.applyWrenJobInputs(ctx, child, checkpoint)
	if err != nil || len(replayed) != 2 {
		t.Fatalf("redelivered: %#v %v", replayed, err)
	}
	child.Lease = "expired-generation"
	if _, err = s.applyWrenJobInputs(ctx, child, checkpoint); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale lease: %v", err)
	}
	if _, err = s.CancelWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID, "after-stop", "Continue"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled accepted %v", err)
	}
}

func TestWrenJobInputsPrivateAndUnsupportedHarness(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	j := requireWrenJob(t, s, c, parent, "private-worker")
	other, _, _, err := s.Signup(ctx, "steering-outsider@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SteerWrenJob(ctx, c.WorkspaceID, other.ID, j.ID, "outside", "Change this task"); err == nil {
		t.Fatal("private job accepted outsider")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET executor_kind='acp-opencode' WHERE id=$1`, j.ChildRunID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID, "unsupported", "Change the running code"); err == nil || !strings.Contains(err.Error(), "cannot accept concurrent steering") {
		t.Fatalf("unsupported hidden %v", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_job_inputs WHERE job_id=$1`, j.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unsupported persisted %d %v", count, err)
	}
}

func TestWrenJobInputsConcurrentFinishCannotDropAcceptedCorrection(t *testing.T) {
	for attempt := 0; attempt < 4; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			s, c, parent := wrenJobFixture(t)
			ctx := context.Background()
			j := requireWrenJob(t, s, c, parent, "finish-race")
			if err := s.finishOperator(ctx, parent, "completed", "Worker started"); err != nil {
				t.Fatal(err)
			}
			child, ok, err := s.claimOperator(ctx)
			if err != nil || !ok || child.ID != j.ChildRunID {
				t.Fatalf("claim: %v %v", ok, err)
			}
			start := make(chan struct{})
			steered, finished := make(chan error, 1), make(chan error, 1)
			go func() {
				<-start
				_, err := s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID, "concurrent", "Use the corrected date")
				steered <- err
			}()
			go func() { <-start; finished <- s.finishOperator(ctx, child, "completed", "Original date") }()
			close(start)
			steerErr, finishErr := <-steered, <-finished
			if steerErr == nil {
				if !errors.Is(finishErr, errWrenJobInputPending) {
					t.Fatalf("accepted correction was lost: finish=%v", finishErr)
				}
			} else if finishErr != nil || !errors.Is(steerErr, ErrConflict) {
				t.Fatalf("neither serialized outcome: steer=%v finish=%v", steerErr, finishErr)
			}
		})
	}
}
