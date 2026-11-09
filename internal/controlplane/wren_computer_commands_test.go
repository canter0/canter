package controlplane

import (
	"context"
	"errors"
	"github.com/canter0/canter/internal/wrencomputer"
	"testing"
)

type commandClientFake struct {
	jobs                  map[string]wrencomputer.Job
	started, stopped      []string
	startErr, errorOnRead error
	stopRunning           bool
}

func (f *commandClientFake) StartJob(_ context.Context, r wrencomputer.JobRequest) (wrencomputer.Job, error) {
	f.started = append(f.started, r.ID)
	j := wrencomputer.Job{ID: r.ID, Status: "running"}
	f.jobs[r.ID] = j
	return j, f.startErr
}
func (f *commandClientFake) Job(_ context.Context, id string) (wrencomputer.Job, error) {
	if f.errorOnRead != nil {
		return wrencomputer.Job{}, f.errorOnRead
	}
	j, ok := f.jobs[id]
	if !ok {
		return j, &wrencomputer.Error{Code: "not_found"}
	}
	return j, nil
}
func (f *commandClientFake) StopJob(_ context.Context, id string) (wrencomputer.Job, error) {
	f.stopped = append(f.stopped, id)
	j := wrencomputer.Job{ID: id, Status: "cancelled"}
	if f.stopRunning {
		j.Status = "running"
	}
	f.jobs[id] = j
	return j, nil
}
func TestComputerCommandSurvivesLostLaunchReceiptAndRequiresExactReview(t *testing.T) {
	h, _, v := operationComputer(t)
	s := h.service.Store
	ctx := context.Background()
	f := &commandClientFake{jobs: map[string]wrencomputer.Job{}, startErr: context.DeadlineExceeded}
	if _, e := s.startComputerCommand(ctx, f, v.ID, wrencomputer.JobRequest{ID: "command_first", Command: "sleep 30"}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	ids, e := s.pendingComputerCommands(ctx, v.ID)
	if e != nil || len(ids) != 1 {
		t.Fatalf("lost receipt lost command: %v %v", ids, e)
	}
	var review *computerCommandsRunning
	if e = s.settleComputerCommands(ctx, f, v.ID, ids, nil); !errors.As(e, &review) || len(f.stopped) != 0 {
		t.Fatalf("review not required: %v", e)
	}
	f.startErr = nil
	if _, e = s.startComputerCommand(ctx, f, v.ID, wrencomputer.JobRequest{ID: "command_second", Command: "sleep 30"}); e != nil {
		t.Fatal(e)
	}
	newer, _ := s.pendingComputerCommands(ctx, v.ID)
	if e = s.settleComputerCommands(ctx, f, v.ID, newer, ids); !errors.As(e, &review) || len(f.stopped) != 0 {
		t.Fatalf("stale review cancelled new work: %v", e)
	}
	if e = s.settleComputerCommands(ctx, f, v.ID, newer, newer); e != nil {
		t.Fatal(e)
	}
	if pending, e := s.pendingComputerCommands(ctx, v.ID); e != nil || len(pending) != 0 || len(f.stopped) != 2 {
		t.Fatalf("not settled: %v %v", pending, e)
	}
}
func TestComputerCommandHandoffDoesNotTrustStopRequestOrUnreachableGuest(t *testing.T) {
	h, _, v := operationComputer(t)
	s := h.service.Store
	ctx := context.Background()
	f := &commandClientFake{jobs: map[string]wrencomputer.Job{}}
	if _, e := s.startComputerCommand(ctx, f, v.ID, wrencomputer.JobRequest{ID: "command_wait", Command: "sleep 30"}); e != nil {
		t.Fatal(e)
	}
	ids, _ := s.pendingComputerCommands(ctx, v.ID)
	f.stopRunning = true
	var review *computerCommandsRunning
	if e := s.settleComputerCommands(ctx, f, v.ID, ids, ids); !errors.As(e, &review) {
		t.Fatalf("unconfirmed stop passed: %v", e)
	}
	f.errorOnRead = context.DeadlineExceeded
	if e := s.settleComputerCommands(ctx, f, v.ID, ids, ids); e == nil {
		t.Fatal("unreachable guest passed")
	}
	if pending, _ := s.pendingComputerCommands(ctx, v.ID); len(pending) != 1 {
		t.Fatal("unresolved command disappeared")
	}
}
func TestComputerMissingLaunchCanBeCancelledBeforeLateStart(t *testing.T) {
	h, _, v := operationComputer(t)
	s := h.service.Store
	ctx := context.Background()
	f := &commandClientFake{jobs: map[string]wrencomputer.Job{}}
	if _, e := s.pool.Exec(ctx, `INSERT INTO wren_computer_commands(computer_id,command_id) VALUES($1,'command_missing')`, v.ID); e != nil {
		t.Fatal(e)
	}
	ids, _ := s.pendingComputerCommands(ctx, v.ID)
	var review *computerCommandsRunning
	if e := s.settleComputerCommands(ctx, f, v.ID, ids, nil); !errors.As(e, &review) {
		t.Fatalf("unknown launch is not reviewable: %v", e)
	}
	if e := s.settleComputerCommands(ctx, f, v.ID, ids, ids); e != nil {
		t.Fatal(e)
	}
}

func TestComputerInvalidRetryCannotHideAnUnconfirmedLaunch(t *testing.T) {
	h, _, v := operationComputer(t)
	s := h.service.Store
	ctx := context.Background()
	f := &commandClientFake{jobs: map[string]wrencomputer.Job{}, startErr: context.DeadlineExceeded}
	request := wrencomputer.JobRequest{ID: "command_retry", Command: "sleep 30"}
	_, _ = s.startComputerCommand(ctx, f, v.ID, request)
	f.startErr = &wrencomputer.Error{Code: "invalid_command"}
	_, _ = s.startComputerCommand(ctx, f, v.ID, request)
	ids, e := s.pendingComputerCommands(ctx, v.ID)
	if e != nil || len(ids) != 1 {
		t.Fatalf("invalid retry cleared previous uncertain command: %v %v", ids, e)
	}
}

func TestComputerHumanCommandsCannotBypassAnotherController(t *testing.T) {
	h, c, v := operationComputer(t)
	ctx := context.Background()
	if _, err := h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, v.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if err := h.computerHumanMutationAllowed(ctx, v, "another-person"); !errors.Is(err, ErrConflict) {
		t.Fatalf("another person can mutate controlled computer: %v", err)
	}
	if err := h.computerControlOwnerAllowed(ctx, v, "another-person"); !errors.Is(err, ErrConflict) {
		t.Fatalf("another person can stop commands during control: %v", err)
	}
	if err := h.computerHumanMutationAllowed(ctx, v, c.AccountID); err != nil {
		t.Fatalf("holder blocked: %v", err)
	}
}
