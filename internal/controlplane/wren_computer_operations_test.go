package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
)

func operationComputer(t *testing.T) (*HTTPServer, Conversation, WrenComputer) {
	t.Helper()
	h, c, _, _ := computerFixture(t)
	w, e := h.service.Store.SaveWren(context.Background(), c.WorkspaceID, c.AccountID, Wren{ID: "wren_operation", Name: "Operation", Shape: "circle"}, true)
	if e != nil {
		t.Fatal(e)
	}
	c, e = h.service.Store.Conversation(context.Background(), c.WorkspaceID, c.AccountID, w.ConversationID)
	if e != nil {
		t.Fatal(e)
	}
	v, e := h.EnableWrenComputer(context.Background(), c.WorkspaceID, c.AccountID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	return h, c, readyComputer(t, h, v)
}

func TestComputerTakeoverWaitsForRemoteMutationReceipt(t *testing.T) {
	h, c, v := operationComputer(t)
	ctx := context.Background()
	op, e := h.service.Store.beginComputerOperation(ctx, v.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer op.close()
	started, finish, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { finished <- op.run(func(context.Context) error { close(started); <-finish; return nil }) }()
	<-started
	// A separate HTTP server must honor the same durable claim, not only a
	// process-local mutex or the original handler's view of control ownership.
	other := &HTTPServer{service: h.service, config: h.config}
	taken := make(chan error, 1)
	go func() { _, e := other.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); taken <- e }()
	select {
	case e := <-taken:
		close(finish)
		<-finished
		t.Fatalf("takeover acknowledged before remote action ended: %v", e)
	case <-time.After(200 * time.Millisecond):
	}
	close(finish)
	if e = <-finished; e != nil {
		t.Fatal(e)
	}
	op.close()
	select {
	case e = <-taken:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("takeover did not acquire after the receipt")
	}
	if e = h.ComputerAgentWriteAllowed(ctx, c); !errors.Is(e, ErrConflict) {
		t.Fatalf("next agent action admitted after takeover: %v", e)
	}
	if e = h.requireHumanComputerControl(ctx, v, c.AccountID); e != nil {
		t.Fatal(e)
	}
}

func TestComputerUncertainMutationKeepsBarrierAndOldCleanupCannotReleaseReplacement(t *testing.T) {
	h, _, v := operationComputer(t)
	s := h.service.Store
	ctx := context.Background()
	op, e := s.beginComputerOperation(ctx, v.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = op.run(func(context.Context) error { return context.DeadlineExceeded }); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	op.close()
	bounded, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	if _, e = s.beginComputerOperation(bounded, v.ID); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("uncertain remote operation allowed immediate replacement: %v", e)
	}
	if _, e = s.pool.Exec(ctx, `UPDATE wren_computer_operations SET expires_at=clock_timestamp()-interval '1 second' WHERE computer_id=$1`, v.ID); e != nil {
		t.Fatal(e)
	}
	next, e := s.beginComputerOperation(ctx, v.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer next.close()
	// Simulate a delayed successful receipt from the original holder.
	op.uncertain = false
	op.close()
	var token string
	if e = s.pool.QueryRow(ctx, `SELECT token FROM wren_computer_operations WHERE computer_id=$1`, v.ID).Scan(&token); e != nil || token != next.token {
		t.Fatalf("old cleanup removed replacement: %v", e)
	}
}

func TestComputerValidationAndCancelledWaitDoNotPoisonHandoff(t *testing.T) {
	h, _, v := operationComputer(t)
	s := h.service.Store
	ctx := context.Background()
	op, e := s.beginComputerOperation(ctx, v.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = op.run(func(context.Context) error {
		return &wrencomputer.Error{Code: "invalid_input", Message: "invalid coordinates"}
	}); e == nil {
		t.Fatal("expected validation error")
	}
	op.close()
	cancelled, cancel := context.WithCancel(ctx)
	next, e := s.beginComputerOperation(cancelled, v.ID)
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	if e = next.run(func(context.Context) error { t.Fatal("cancelled action invoked"); return nil }); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	next.close()
	last, e := s.beginComputerOperation(ctx, v.ID)
	if e != nil {
		t.Fatal(e)
	}
	last.close()
}

func TestComputerControlNoticeTracksLiveHandoffWithoutPollingInstructions(t *testing.T) {
	h, c, v := operationComputer(t)
	o := OperatorRuntime{Server: h}
	ctx := context.Background()
	if note, e := o.wrenComputerRuntimeNotice(ctx, c, "notice_run"); e != nil || !strings.Contains(note, v.ID) || strings.Contains(note, "a person has taken over") {
		t.Fatalf("idle computer has a takeover notice: %q %v", note, e)
	}
	if _, e := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); e != nil {
		t.Fatal(e)
	}
	if note, e := o.wrenComputerRuntimeNotice(ctx, c, "notice_run"); e != nil || !strings.Contains(note, "end your reply now") || !strings.Contains(note, "Do not poll") {
		t.Fatalf("missing handoff instruction: %q %v", note, e)
	}
	if _, e := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "agent"); e != nil {
		t.Fatal(e)
	}
	if note, e := o.wrenComputerRuntimeNotice(ctx, c, "notice_run"); e != nil || !strings.Contains(note, v.ID) || strings.Contains(note, "a person has taken over") {
		t.Fatalf("returned control retained an obsolete notice: %q %v", note, e)
	}
}

func TestComputerHandoffReceiptsExcludeOtherRunsRejectedInputsAndText(t *testing.T) {
	h, c, v := operationComputer(t)
	ctx := context.Background()
	s := h.service.Store
	if _, err := s.pool.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,status,model) VALUES('notice_run',$1,'notice_request','running','test'),('notice_old',$1,'notice_old_request','completed','test')`, c.ID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ run, call, result string }{
		{"notice_run", "accepted", `{"action":"scroll","amount":12,"submitted":true,"text":"private-text"}`},
		{"notice_run", "rejected", `{"action":"scroll","amount":-12,"error":"human control"}`},
		{"notice_old", "old", `{"action":"scroll","amount":99,"submitted":true}`},
	} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result) VALUES($1,$2,'canter_computer_input','{}',$3::jsonb||jsonb_build_object('computerId',$4::text))`, row.run, row.call, row.result, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); err != nil {
		t.Fatal(err)
	}
	o := OperatorRuntime{Server: h}
	note, err := o.wrenComputerRuntimeNotice(ctx, c, "notice_run")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(note, `"action": "scroll"`) != 1 || !strings.Contains(note, `"amount": 12`) || strings.Contains(note, "private-text") || strings.Contains(note, `"amount": 99`) || strings.Contains(note, `"amount": -12`) {
		t.Fatalf("incorrect receipt scope: %s", note)
	}
}
