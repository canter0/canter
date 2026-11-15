package controlplane

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestCIAdmissionSourceBoundedHardDeadline(t *testing.T) {
	ctx := context.Background()
	sourceStart := time.Date(2026, time.November, 30, 23, 0, 3, 0, time.UTC)
	f, s, workspace, runner, _, req := ciAdmissionSourceSetup(t,
		ciAdmissionSourceJobJSON(5001, sourceStart, nil))
	hard := sourceStart.Add(1000 * time.Second)
	receipt, err := f.client.admitRunnerJobBefore(ctx, s, workspace, runner, req, 7200, hard)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Decision != "allow" || receipt.ExecutionDeadlineUnixMs != hard.UnixMilli() {
		t.Fatalf("hard cutoff not preserved: %+v", receipt)
	}
	_, started, deadline, _ := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if started == nil || !started.Equal(sourceStart) || deadline == nil || !deadline.Equal(hard) {
		t.Fatalf("clock mismatch: started=%v deadline=%v", started, deadline)
	}
	if units := ciAdmissionReservedUnits(t, s, workspace, runner); units != 0 {
		t.Fatalf("a 1000-second job must remain inside its free hour: %d", units)
	}
	firstDecision := ciAdmissionDecisionRaw(t, s, workspace, runner)
	firstCalls := f.count()
	again, err := f.client.admitRunnerJobBefore(ctx, s, workspace, runner, req, 7200, hard)
	if err != nil || again != receipt || f.count() != firstCalls {
		t.Fatalf("recovery changed receipt or repeated source work: receipt=%+v err=%v calls=%d", again, err, f.count())
	}
	if !bytes.Equal(firstDecision, ciAdmissionDecisionRaw(t, s, workspace, runner)) {
		t.Fatal("recovery rewrote the committed decision")
	}
}

func TestCIAdmissionSourceBoundedRejectsLongRecovery(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	first, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
	if err != nil {
		t.Fatal(err)
	}
	original := ciAdmissionDecisionRaw(t, s, workspace, runner)
	originalUnits := ciAdmissionReservedUnits(t, s, workspace, runner)
	f := newCISourceFixture(t)
	receipt, err := f.client.admitRunnerJobBefore(ctx, s, workspace, runner, req, 7200,
		proof.StartedAt.Add(1000*time.Second))
	if !errors.Is(err, ErrConflict) || receipt != (ciAdmissionReceipt{}) || f.count() != 0 {
		t.Fatalf("overlong recovery escaped its bound: receipt=%+v err=%v calls=%d", receipt, err, f.count())
	}
	if !bytes.Equal(original, ciAdmissionDecisionRaw(t, s, workspace, runner)) ||
		originalUnits != ciAdmissionReservedUnits(t, s, workspace, runner) {
		t.Fatal("denied recovery changed decision or reservation")
	}
	_, started, deadline, _ := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if started == nil || !started.Equal(proof.StartedAt) || deadline == nil ||
		deadline.UnixMilli() != first.ExecutionDeadlineUnixMs {
		t.Fatalf("denied recovery reset clock: started=%v deadline=%v", started, deadline)
	}
}

func TestCIAdmissionSourceBoundedFutureStartUsesWallCeiling(t *testing.T) {
	ctx := context.Background()
	// The fixture's current wall time is 23:00:08; GitHub is 30 seconds ahead.
	sourceStart := time.Date(2026, time.November, 30, 23, 0, 38, 0, time.UTC)
	f, s, workspace, runner, _, req := ciAdmissionSourceSetup(t,
		ciAdmissionSourceJobJSON(5001, sourceStart, nil))
	now := s.now()
	if !sourceStart.Equal(now.Add(30 * time.Second)) {
		t.Fatal("fixture no longer exercises a future source clock")
	}
	want := now.Add(time.Duration(ciMaxRuntimeSeconds) * time.Second)
	receipt, err := f.client.admitRunnerJobBefore(ctx, s, workspace, runner, req,
		ciMaxRuntimeSeconds, now.Add(ciLifecycleHardWindow))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Decision != "allow" || receipt.ExecutionDeadlineUnixMs != want.UnixMilli() {
		t.Fatalf("future source clock extended wall ceiling: %+v", receipt)
	}
	_, started, deadline, _ := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if started == nil || !started.Equal(sourceStart) || deadline == nil || !deadline.Equal(want) {
		t.Fatalf("bounded quote changed source clock: started=%v deadline=%v", started, deadline)
	}
}

func TestCIAdmissionSourceBoundedNoRemainingExecution(t *testing.T) {
	ctx := context.Background()
	sourceStart := time.Date(2026, time.November, 30, 23, 0, 38, 0, time.UTC)
	f, s, workspace, runner, _, req := ciAdmissionSourceSetup(t,
		ciAdmissionSourceJobJSON(5001, sourceStart, nil))
	receipt, err := f.client.admitRunnerJobBefore(ctx, s, workspace, runner, req, 7200, sourceStart)
	if !errors.Is(err, ErrConflict) || receipt != (ciAdmissionReceipt{}) {
		t.Fatalf("no remaining execution must deny: receipt=%+v err=%v", receipt, err)
	}
	ciAdmissionAssertDenied(t, s, workspace, runner, "idle", 1001)
	if count := ciAdmissionJobCount(t, s, workspace, 5001); count != 0 {
		t.Fatalf("denial adopted %d jobs", count)
	}
}
