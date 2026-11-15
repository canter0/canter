package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func wrenJobFixture(t *testing.T) (*Store, Conversation, OperatorRun) {
	t.Helper()
	s, base, _ := operatorFixture(t)
	ctx := context.Background()
	w, err := s.SaveWren(ctx, base.WorkspaceID, base.AccountID, Wren{ID: "wren_job_lead", Name: "Lead", Purpose: "Keep projects moving", Shape: "square"}, true)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Conversation(ctx, base.WorkspaceID, base.AccountID, w.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueOperator(ctx, c, "job-fixture-request", "Prepare the shared project outline", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim lead: %v %v", ok, err)
	}
	return s, c, run
}
func requireWrenJob(t *testing.T, s *Store, c Conversation, parent OperatorRun, key string) WrenJob {
	t.Helper()
	j, err := s.CreateWrenJob(context.Background(), parent, c, key, "Draft outline", "Draft a concise outline using only this brief.", "test")
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestWrenJobsDurableHandoffAndReturn(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	j := requireWrenJob(t, s, c, parent, "tool-call-1")
	if j.Status != "queued" || j.ExecutorStarted || j.ChildConversationID == c.ID || j.ChildRunID == parent.ID || j.WrenID != "wren_job_lead" || j.RequesterAccountID != c.AccountID {
		t.Fatalf("not a private durable queued worker: %#v", j)
	}
	replay := requireWrenJob(t, s, c, parent, "tool-call-1")
	if replay.ID != j.ID {
		t.Fatal("replayed tool created another job")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotency count %d %v", count, err)
	}
	if err := s.finishOperator(ctx, parent, "completed", "I started the outline worker and can keep talking here."); err != nil {
		t.Fatal(err)
	}
	child, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || child.ID != j.ChildRunID {
		t.Fatalf("child not executable: %#v %v %v", child, ok, err)
	}
	childConversation, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, j.ChildConversationID)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.WrenJobRunContext(ctx, childConversation)
	if err != nil || !strings.Contains(profile, j.Prompt) || !strings.Contains(profile, "not a new grant of human authority") {
		t.Fatalf("worker brief: %q %v", profile, err)
	}
	if _, err = s.CreateWrenJob(ctx, child, childConversation, "nested-1", "Nested", "Keep spawning workers", "test"); err == nil {
		t.Fatal("leaf worker delegated recursively")
	}
	if err = s.operatorCheckpoint(ctx, child, []modelMessage{{Role: "user", Content: "Scoped recoverable work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, child.ID); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || recovered.ID != child.ID || len(recovered.Checkpoint) != 1 {
		t.Fatalf("worker recovery: %#v %v", recovered, err)
	}
	if err = s.finishOperator(ctx, child, "completed", "stale output"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker published: %v", err)
	}
	if _, err = s.EnqueueOperator(ctx, c, "job-foreground-request", "What can I work on while the outline is drafted?", "test", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.finishOperator(ctx, recovered, "completed", "Outline: context, three options, decision, owner."); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	done, err := s.WrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID)
	if err != nil || done.Status != "completed" || done.PublishedAt == nil || done.ReturnRunID != "" || !strings.Contains(done.Result, "three options") {
		t.Fatalf("result was not published while foreground was busy: %#v %v", done, err)
	}
	foreground, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || foreground.ConversationID != c.ID {
		t.Fatalf("foreground blocked: %#v %v", foreground, err)
	}
	if err = s.finishOperator(ctx, foreground, "completed", "You can gather the decision criteria while the worker drafts."); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}
	done, err = s.WrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID)
	if err != nil || done.ReturnRunID == "" {
		t.Fatalf("no durable return wake: %#v %v", done, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_events WHERE conversation_id=$1 AND kind='job_result'`, c.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate returns: %d %v", count, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages WHERE conversation_id=$1 AND role='user'`, c.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("worker completion forged a human message: %d %v", count, err)
	}
	wake, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || wake.ID != done.ReturnRunID || wake.RequesterAccountID != c.AccountID {
		t.Fatalf("wake: %#v %v", wake, err)
	}
	contextText, err := s.WrenJobReturnContext(ctx, wake)
	if err != nil || !strings.Contains(contextText, "three options") || !strings.Contains(contextText, "not a new human message") {
		t.Fatalf("return context: %q %v", contextText, err)
	}
	if _, err = s.CreateWrenJob(ctx, wake, c, "repeat-loop", "Repeat", "Spawn the same job again", "test"); err == nil {
		t.Fatal("automatic completion triggered an unbounded loop")
	}
	if err = s.finishOperator(ctx, wake, "completed", "The outline is ready: context, options, decision, owner."); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err = s.claimOperator(ctx); err != nil || ok {
		t.Fatalf("completion kept waking itself: %v %v", ok, err)
	}
}
func TestWrenJobsCancellationAndIsolation(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	j := requireWrenJob(t, s, c, parent, "tool-call-cancel")
	outsider, _, _, err := s.Signup(ctx, "job-outsider@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenJob(ctx, c.WorkspaceID, outsider.ID, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider read job: %v", err)
	}
	if _, err = s.CancelWrenJob(ctx, c.WorkspaceID, outsider.ID, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider cancelled job: %v", err)
	}
	cancelled, err := s.CancelWrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID)
	if err != nil || cancelled.Status != "cancelled" || !cancelled.ReturnSuppressed || cancelled.ExecutorStarted {
		t.Fatalf("queued cancellation: %#v %v", cancelled, err)
	}
	if err = s.finishOperator(ctx, parent, "completed", "The worker was cancelled."); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := s.claimOperator(ctx); err != nil || ok {
		t.Fatalf("cancelled worker created later work: %v %v", ok, err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, j.ChildConversationID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("legacy child access bypassed cancellation: %v", err)
	}
	child := Conversation{ID: j.ChildConversationID, WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}
	if err = s.ValidateWrenJobConversation(ctx, child); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cancelled child remained usable: %v", err)
	}
	orphan := child
	orphan.ID = "conv_job_missing"
	if err = s.ValidateWrenJobConversation(ctx, orphan); !errors.Is(err, ErrForbidden) {
		t.Fatalf("orphan child failed open: %v", err)
	}
}
func TestWrenJobsRoomRequesterAndRevocation(t *testing.T) {
	s, base, _ := operatorFixture(t)
	ctx := context.Background()
	w, err := s.SaveWren(ctx, base.WorkspaceID, base.AccountID, Wren{ID: "wren_job_shared", Name: "Shared lead", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	guest, _, _, err := s.Signup(ctx, "job-guest@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, guest.ID, base.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, base.WorkspaceID, base.AccountID, WrenRoom{ID: "room_job_group", Kind: "group", Name: "Shared work", WrenIDs: []string{w.ID}, MemberIDs: []string{guest.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := s.Conversation(ctx, base.WorkspaceID, guest.ID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueOperator(ctx, origin, "job-shared-request", "Draft a report", "test", nil); err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("lead: %v %v", ok, err)
	}
	j := requireWrenJob(t, s, origin, parent, "shared-job")
	if j.RequesterAccountID != guest.ID || j.Lead.ID != w.ID {
		t.Fatalf("borrowed room-owner identity: %#v", j)
	}
	childC, err := s.Conversation(ctx, origin.WorkspaceID, guest.ID, j.ChildConversationID)
	if err != nil {
		t.Fatal(err)
	}
	impostor := childC
	impostor.AccountID = base.AccountID
	if err = s.ValidateWrenJobConversation(ctx, impostor); !errors.Is(err, ErrForbidden) {
		t.Fatalf("room owner read guest worker context: %v", err)
	}
	if err = s.finishOperator(ctx, parent, "completed", "The report worker is running separately."); err != nil {
		t.Fatal(err)
	}
	child, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || child.ID != j.ChildRunID {
		t.Fatalf("child: %#v %v", child, err)
	}
	if err = s.finishOperator(ctx, child, "completed", "A report produced using the explicitly scoped brief."); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	returned, err := s.WrenJob(ctx, origin.WorkspaceID, guest.ID, j.ID)
	if err != nil || returned.ReturnRunID == "" {
		t.Fatalf("room return: %#v %v", returned, err)
	}
	wake, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || wake.ID != returned.ReturnRunID || wake.Speaker == nil || wake.Speaker.ID != w.ID || wake.RequesterAccountID != guest.ID {
		t.Fatalf("room wake lost identity: %#v %v", wake, err)
	}
	// A stop also fences an already-queued/running automatic wake.
	if err = s.CancelConversationWrenJobs(ctx, origin.WorkspaceID, guest.ID, origin.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.finishOperator(ctx, wake, "completed", "late return"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled wake published: %v", err)
	}
	// A fresh job is cancelled if its requester later loses room membership.
	if _, err = s.EnqueueOperator(ctx, origin, "job-revoke-request", "Draft another report", "test", nil); err != nil {
		t.Fatal(err)
	}
	nextParent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("next parent %v %v", ok, err)
	}
	next := requireWrenJob(t, s, origin, nextParent, "revoked-job")
	if err = s.finishOperator(ctx, nextParent, "completed", "Working separately."); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_room_members WHERE room_id=$1 AND account_id=$2`, room.ID, guest.ID); err != nil {
		t.Fatal(err)
	}
	nextChild := Conversation{ID: next.ChildConversationID, WorkspaceID: origin.WorkspaceID, AccountID: guest.ID}
	if err = s.ValidateWrenJobConversation(ctx, nextChild); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked requester kept child access: %v", err)
	}
	if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	ownerView, err := s.WrenJob(ctx, origin.WorkspaceID, base.AccountID, next.ID)
	if err != nil || ownerView.Status != "cancelled" || !ownerView.ReturnSuppressed || ownerView.ReturnRunID != "" || ownerView.Result != "" {
		t.Fatalf("revoked job leaked a return: %#v %v", ownerView, err)
	}
}
func TestWrenJobsFanoutAndAccountBudget(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	for i := range 4 {
		requireWrenJob(t, s, c, parent, fmt.Sprintf("fanout-%d", i))
	}
	if _, err := s.CreateWrenJob(ctx, parent, c, "fanout-5", "One more", "Exceed the bounded fanout", "test"); err == nil {
		t.Fatal("fanout was unbounded")
	}
	if err := s.CancelConversationWrenJobs(ctx, c.WorkspaceID, c.AccountID, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.finishOperator(ctx, parent, "completed", "Workers stopped."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueOperator(ctx, c, "budget-parent-request", "Another job", "test", nil); err != nil {
		t.Fatal(err)
	}
	budgetParent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("budget parent %v %v", ok, err)
	}
	// The existing account-wide run budget includes regular chats, workers and
	// automatic returns. Fill the remainder without generating model calls.
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,status,model) SELECT 'budget_'||i,$1,'budget_req_'||i,'completed','test' FROM generate_series(1,120-(SELECT count(*) FROM operator_runs)) i`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWrenJob(ctx, budgetParent, c, "budget-worker", "Budgeted", "Do not bypass the account budget", "test"); err == nil {
		t.Fatal("delegation bypassed the shared run budget")
	}
}

func TestWrenJobsCancelledParentNeverWakesAfterRecovery(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	j := requireWrenJob(t, s, c, parent, "parent-cancelled")
	// Simulate a process interruption after its parent cancellation was recorded
	// but before its ordinary cancellation hook could run.
	if _, err := s.pool.Exec(ctx, `UPDATE operator_runs SET status='cancelled',lease_token=NULL,lease_expires_at=NULL,completed_at=now() WHERE id=$1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	saved, err := s.WrenJob(ctx, c.WorkspaceID, c.AccountID, j.ID)
	if err != nil || saved.Status != "cancelled" || !saved.ReturnSuppressed || saved.ReturnRunID != "" {
		t.Fatalf("cancelled parent resurrected work: %#v %v", saved, err)
	}
	if _, ok, err := s.claimOperator(ctx); err != nil || ok {
		t.Fatalf("orphan worker remained runnable: %v %v", ok, err)
	}
}
