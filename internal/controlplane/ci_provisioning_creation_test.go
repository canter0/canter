package controlplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ciCreationResources returns the canonical candidate IDs a creator
// acknowledges, in SDK order: security group, port, server.
func ciCreationResources(operation string) (sg, port, server ciOwnedResource) {
	return ciOwnedResource{Kind: "security_group", ID: "sg_creation", OperationID: operation},
		ciOwnedResource{Kind: "port", ID: "port_creation", OperationID: operation},
		ciOwnedResource{Kind: "server", ID: "server_creation", OperationID: operation}
}

// ciCreationFunded grants, reserves, and lifecycle-pins one runner without
// consuming the durable one-shot create fence.
func ciCreationFunded(t *testing.T, s *Store, workspace, op string, number int, now time.Time, amount int64) (string, ciRunnerLifecyclePlan, ciProvisioningBudgetPolicy) {
	t.Helper()
	policy := ciBudgetPolicy(now)
	ciBudgetGrant(t, s, "credit_1", amount, policy)
	runner, plan := ciBudgetSeed(t, s, workspace, op, number, now)
	ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)
	return runner, plan, policy
}

// ciCreationSetup additionally consumes the global create fence, the state a
// trusted producer reaches immediately before its first provider mutation.
func ciCreationSetup(t *testing.T, s *Store, workspace, op string, number int, now time.Time, amount int64) (string, ciRunnerLifecyclePlan, ciProvisioningBudgetPolicy) {
	t.Helper()
	runner, plan, policy := ciCreationFunded(t, s, workspace, op, number, now, amount)
	fenced, err := s.fenceCIRunnerCreate(context.Background(), workspace, runner)
	if err != nil || !fenced {
		t.Fatalf("global create fence: fenced=%v err=%v", fenced, err)
	}
	return runner, plan, policy
}

func ciCreationFenceStage(t *testing.T, s *Store, workspace, runner string, policy ciProvisioningBudgetPolicy, plan ciRunnerLifecyclePlan, stage string, known []ciOwnedResource) {
	t.Helper()
	if err := s.fenceCIProvisioningStage(context.Background(), workspace, runner, policy.ProfileDigest, plan, stage, known); err != nil {
		t.Fatalf("fence stage %s: %v", stage, err)
	}
}

func ciCreationJournal(t *testing.T, s *Store, workspace, runner string, policy ciProvisioningBudgetPolicy, plan ciRunnerLifecyclePlan) ciProvisioningCreationJournal {
	t.Helper()
	j, err := s.ciProvisioningCreationState(context.Background(), workspace, runner, policy.ProfileDigest, plan)
	if err != nil {
		t.Fatalf("creation journal: %v", err)
	}
	return j
}

func ciCreationRecord(t *testing.T, s *Store, workspace, runner string, policy ciProvisioningBudgetPolicy, plan ciRunnerLifecyclePlan, outcome ciProvisioningCreateOutcome) {
	t.Helper()
	if err := s.recordCIProvisioningCreationResult(context.Background(), workspace, runner, policy.ProfileDigest, plan, outcome); err != nil {
		t.Fatalf("record creation result: %v", err)
	}
}

func ciCreationLifecycle(t *testing.T, s *Store, workspace, runner string) ciRunnerLifecycleSnapshot {
	t.Helper()
	snap, err := s.loadCIRunnerLifecycle(context.Background(), workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func ciCreationExpectConflict(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func ciCreationExpectDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil || (!errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) && !errors.Is(err, errCIRunnerJobPending)) {
		t.Fatalf("expected denial, got %v", err)
	}
}

// ciCreationComplete runs the full SG -> port -> server fence sequence and
// records the settled three-resource SDK outcome.
func ciCreationComplete(t *testing.T, s *Store, workspace, runner string, policy ciProvisioningBudgetPolicy, plan ciRunnerLifecyclePlan) []ciOwnedResource {
	t.Helper()
	sg, port, server := ciCreationResources(plan.OperationID)
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "server", []ciOwnedResource{sg, port})
	ciCreationRecord(t, s, workspace, runner, policy, plan, ciProvisioningCreateOutcome{
		MutationAttempted: true, CreationSettled: true,
		Resources: []ciOwnedResource{sg, port, server},
	})
	return []ciOwnedResource{sg, port, server}
}

func TestCIProvisioningCreationStageSequenceAndResult(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_seq", 1, now, 1000)
	sg, port, server := ciCreationResources(plan.OperationID)

	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "server", []ciOwnedResource{sg, port})

	j := ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "server" || j.FinishedAt != nil || j.Outcome != nil ||
		len(j.KnownResources) != 2 || j.KnownResources[0] != sg || j.KnownResources[1] != port {
		t.Fatalf("journal after stage fences: %+v", j)
	}
	snap := ciCreationLifecycle(t, s, workspace, runner)
	if snap.CreateSettled || snap.ExecutionStartedAt != nil || len(snap.Resources) != 2 ||
		snap.Resources[0] != sg || snap.Resources[1] != port {
		t.Fatalf("lifecycle after stage fences: %+v", snap)
	}

	outcome := ciProvisioningCreateOutcome{MutationAttempted: true, CreationSettled: true,
		Resources: []ciOwnedResource{sg, port, server}}
	ciCreationRecord(t, s, workspace, runner, policy, plan, outcome)

	j = ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.FinishedAt == nil || j.Outcome == nil || !j.Outcome.MutationAttempted ||
		!j.Outcome.CreationSettled || len(j.Outcome.Resources) != 3 || j.Outcome.Resources[2] != server {
		t.Fatalf("journal after result: %+v", j)
	}
	snap = ciCreationLifecycle(t, s, workspace, runner)
	if snap.CreateSettled || len(snap.Resources) != 3 {
		t.Fatalf("lifecycle retained wrong state: %+v", snap)
	}

	// An exact result retry is a durable no-op; funding is untouched.
	ciCreationRecord(t, s, workspace, runner, policy, plan, outcome)
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 || state.AuthorizedMicros != 1000 {
		t.Fatalf("funding moved: %+v", state)
	}
	ciBudgetTotalsZero(t, s, workspace)

	// Stages are never replayable once the creator finished.
	replays := []struct {
		stage string
		known []ciOwnedResource
	}{
		{"security_group", nil},
		{"port", []ciOwnedResource{sg}},
		{"server", []ciOwnedResource{sg, port}},
		{"bogus", nil},
	}
	for _, r := range replays {
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, r.stage, r.known))
	}
}

func TestCIProvisioningCreationRequiresFundingAndFence(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	policy := ciBudgetPolicy(now)
	runner, plan := ciBudgetSeed(t, s, workspace, "creation_op_fence", 1, now)

	// No grant at all: the singleton budget row is absent.
	if err := s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "security_group", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stage without budget: %v", err)
	}

	// Funded but never globally fenced: the stage conflicts while the
	// journal stays not_started.
	ciBudgetGrant(t, s, "credit_1", 1000, policy)
	ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)
	if err := s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "security_group", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stage without create fence: %v", err)
	}
	j := ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "not_started" || len(j.KnownResources) != 0 || j.FinishedAt != nil {
		t.Fatalf("journal moved without fence: %+v", j)
	}

	fenced, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil || !fenced {
		t.Fatalf("global fence: fenced=%v err=%v", fenced, err)
	}
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
	j = ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "security_group" {
		t.Fatalf("journal after SG: %+v", j)
	}
	again, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil || again {
		t.Fatalf("global fence replay: fenced=%v err=%v", again, err)
	}

	// A malformed lease is rejected at reserve before any claim.
	runner2 := ciFinancialRunner(t, s, workspace, "linux-x64-2", 2, false, now)
	badPlan := ciBudgetPlan(t, "creation_op_badlease", now)
	badPlan.EnforcerLease = "lease1"
	if err := s.trackCIRunnerLifecycle(ctx, workspace, runner2, badPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reserveCIProvisioningBudget(ctx, workspace, runner2, policy.ProfileDigest, badPlan); !errors.Is(err, ErrConflict) {
		t.Fatalf("bad lease reserve: %v", err)
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("bad lease consumed funding: %+v", state)
	}
}

func TestCIProvisioningCreationConcurrentSameStage(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_race", 1, now, 1000)
	ctx := context.Background()

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "security_group", nil)
		}(i)
	}
	wg.Wait()
	successes := 0
	for i, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict) || errors.Is(err, errCIRunnerJobPending):
		default:
			t.Fatalf("stage %d unexpected error: %v", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent same-stage fences: %d succeeded", successes)
	}
	j := ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "security_group" || j.FinishedAt != nil {
		t.Fatalf("journal: %+v", j)
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("funding moved: %+v", state)
	}
	snap := ciCreationLifecycle(t, s, workspace, runner)
	if snap.ExecutionStartedAt != nil || snap.CreateSettled {
		t.Fatalf("lifecycle moved: %+v", snap)
	}
	ciBudgetTotalsZero(t, s, workspace)
}

func TestCIProvisioningCreationStageAndScopeDenials(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_deny", 1, now, 1000)
	sg, port, server := ciCreationResources(plan.OperationID)
	digest := policy.ProfileDigest

	// Malformed stage and known-prefix shapes conflict before any write.
	bad := []struct {
		name  string
		stage string
		known []ciOwnedResource
	}{
		{"not_started stage", "not_started", nil},
		{"unknown stage", "bogus", nil},
		{"sg with known", "security_group", []ciOwnedResource{sg}},
		{"port without sg", "port", nil},
		{"server skipping all", "server", []ciOwnedResource{sg, port}},
		{"port wrong kind", "port", []ciOwnedResource{{Kind: "server", ID: "srv_x", OperationID: plan.OperationID}}},
		{"port foreign op", "port", []ciOwnedResource{{Kind: "security_group", ID: "sg_x", OperationID: "other_op"}}},
		{"server extra known", "server", []ciOwnedResource{sg, port, server}},
	}
	for _, c := range bad {
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, digest, plan, c.stage, c.known))
	}
	j := ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "not_started" {
		t.Fatalf("denials moved journal: %+v", j)
	}

	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
	for _, c := range []struct {
		stage string
		known []ciOwnedResource
	}{
		{"security_group", nil},                 // stage replay
		{"server", []ciOwnedResource{sg, port}}, // skipped port
		{"port", nil},                           // wrong known count
	} {
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, digest, plan, c.stage, c.known))
	}
	ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, digest, plan, "server",
		[]ciOwnedResource{sg, sg})) // duplicate kind in prefix
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})

	// A changed pinned SG ID cannot extend the recorded prefix.
	sgOther := ciOwnedResource{Kind: "security_group", ID: "sg_other", OperationID: plan.OperationID}
	ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, digest, plan, "server",
		[]ciOwnedResource{sgOther, port}))
	j = ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "port" || len(j.KnownResources) != 1 || j.KnownResources[0] != sg {
		t.Fatalf("journal after denials: %+v", j)
	}

	// Changed plan, profile, workspace, and runner identities all deny.
	changedPlan := plan
	changedPlan.FlavorID = "f2"
	ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, digest, changedPlan, "server", []ciOwnedResource{sg, port}))
	ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, ciBudgetDigestB(), plan, "server", []ciOwnedResource{sg, port}))
	other := ciBudgetSecondWorkspace(t, s, "creation-w2@example.com", "ci_creation_install_2", 792)
	ciCreationExpectDenied(t, s.fenceCIProvisioningStage(ctx, other, runner, digest, plan, "server", []ciOwnedResource{sg, port}))
	ciCreationExpectDenied(t, s.fenceCIProvisioningStage(ctx, workspace, "ci_financial_runner_999", digest, plan, "server", []ciOwnedResource{sg, port}))

	j = ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "port" || j.FinishedAt != nil {
		t.Fatalf("scope denials moved journal: %+v", j)
	}
}

func TestCIProvisioningCreationGuardsBlockAdvance(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Store, string, string, ciRunnerLifecyclePlan, ciProvisioningBudgetPolicy, ciOwnedResource, ciOwnedResource) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_guard", 1, now, 1000)
		sg, port, _ := ciCreationResources(plan.OperationID)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
		return s, workspace, runner, plan, policy, sg, port
	}

	t.Run("stop blocks next stage", func(t *testing.T) {
		s, w, runner, plan, policy, sg, _ := setup(t)
		if _, err := s.requestCIRunnerStop(ctx, w, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, w, runner, policy.ProfileDigest, plan, "port", []ciOwnedResource{sg}))
		j := ciCreationJournal(t, s, w, runner, policy, plan)
		if j.Stage != "security_group" || j.FinishedAt != nil {
			t.Fatalf("journal after stop: %+v", j)
		}
	})
	t.Run("product disabled blocks next stage", func(t *testing.T) {
		s, w, runner, plan, policy, sg, _ := setup(t)
		ciAdmissionProductOff(t, s, w)
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, w, runner, policy.ProfileDigest, plan, "port", []ciOwnedResource{sg}))
		j := ciCreationJournal(t, s, w, runner, policy, plan)
		if j.Stage != "security_group" || j.FinishedAt != nil {
			t.Fatalf("journal after disable: %+v", j)
		}
	})
	t.Run("setup expiry blocks next stage", func(t *testing.T) {
		s, w, runner, plan, policy, sg, _ := setup(t)
		now := s.now()
		s.now = func() time.Time { return now.Add(700 * time.Second) }
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, w, runner, policy.ProfileDigest, plan, "port", []ciOwnedResource{sg}))
		// The cleanup-identity getter ignores creation deadlines entirely.
		j := ciCreationJournal(t, s, w, runner, policy, plan)
		if j.Stage != "security_group" || j.FinishedAt != nil {
			t.Fatalf("journal after expiry: %+v", j)
		}
	})
}

func TestCIProvisioningCreationStagesUseHeldReservation(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	// Credit exactly covers one reservation: no headroom remains, proving
	// stages validate the held reservation instead of minting new money.
	runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_held", 1, now, 100)
	sg, port, server := ciCreationResources(plan.OperationID)

	state := ciBudgetPeek(t, s, "credit_1", 100, policy)
	if state.AuthorizedMicros != 100 || state.CommittedMicros != 100 || state.Active != 1 {
		t.Fatalf("pre-stage funding: %+v", state)
	}
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "server", []ciOwnedResource{sg, port})
	ciCreationRecord(t, s, workspace, runner, policy, plan, ciProvisioningCreateOutcome{
		MutationAttempted: true, CreationSettled: true,
		Resources: []ciOwnedResource{sg, port, server},
	})
	state = ciBudgetPeek(t, s, "credit_1", 100, policy)
	if state.AuthorizedMicros != 100 || state.CommittedMicros != 100 || state.Active != 1 {
		t.Fatalf("stages consumed or minted funding: %+v", state)
	}
	ciBudgetTotalsZero(t, s, workspace)
}

func TestCIProvisioningCreationRecordsUnknownAfterGuardsFail(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_unknown", 1, now, 1000)
	sg, _, _ := ciCreationResources(plan.OperationID)
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)

	// Stop, disable the product, and expire the setup window: the creator
	// outcome must still persist so cleanup is never stranded.
	if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	ciAdmissionProductOff(t, s, workspace)
	s.now = func() time.Time { return now.Add(700 * time.Second) }

	outcome := ciProvisioningCreateOutcome{MutationAttempted: true, CreationSettled: false,
		Resources: []ciOwnedResource{sg}}
	ciCreationRecord(t, s, workspace, runner, policy, plan, outcome)

	j := ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.FinishedAt == nil || j.Outcome == nil || !j.Outcome.MutationAttempted ||
		j.Outcome.CreationSettled || len(j.Outcome.Resources) != 1 || j.Outcome.Resources[0] != sg {
		t.Fatalf("unknown outcome journal: %+v", j)
	}
	if ciCreationLifecycle(t, s, workspace, runner).CreateSettled {
		t.Fatal("unknown outcome settled lifecycle")
	}
	ciCreationRecord(t, s, workspace, runner, policy, plan, outcome)
	changed := outcome
	changed.Resources = []ciOwnedResource{{Kind: "security_group", ID: "sg_other", OperationID: plan.OperationID}}
	ciCreationExpectConflict(t, s.recordCIProvisioningCreationResult(ctx, workspace, runner, policy.ProfileDigest, plan, changed))
	ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "port", []ciOwnedResource{sg}))
	// The cleanup-identity getter still reads the durable outcome.
	j = ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.FinishedAt == nil || j.Outcome == nil {
		t.Fatalf("getter after guards: %+v", j)
	}
}

func TestCIProvisioningCreationRecordsOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("preflight no mutation", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_pre", 1, now, 1000)
		ciCreationRecord(t, s, workspace, runner, policy, plan, ciProvisioningCreateOutcome{CreationSettled: true})
		j := ciCreationJournal(t, s, workspace, runner, policy, plan)
		if j.Stage != "not_started" || j.FinishedAt == nil || j.Outcome == nil ||
			j.Outcome.MutationAttempted || !j.Outcome.CreationSettled || len(j.Outcome.Resources) != 0 {
			t.Fatalf("preflight journal: %+v", j)
		}
		if err := s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "security_group", nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("stage after preflight finish: %v", err)
		}
		if ciCreationLifecycle(t, s, workspace, runner).CreateSettled {
			t.Fatal("creator outcome settled lifecycle")
		}
	})

	t.Run("empty no mutation after sg", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_sgfail", 1, now, 1000)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
		ciCreationRecord(t, s, workspace, runner, policy, plan, ciProvisioningCreateOutcome{CreationSettled: true})
		j := ciCreationJournal(t, s, workspace, runner, policy, plan)
		if j.Stage != "security_group" || j.FinishedAt == nil || j.Outcome == nil ||
			j.Outcome.MutationAttempted || !j.Outcome.CreationSettled || len(j.Outcome.Resources) != 0 {
			t.Fatalf("sg-fail journal: %+v", j)
		}
		if ciCreationLifecycle(t, s, workspace, runner).CreateSettled {
			t.Fatal("creator outcome settled lifecycle")
		}
	})

	t.Run("invalid outcomes denied", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_badout", 1, now, 1000)
		sg, port, server := ciCreationResources(plan.OperationID)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})

		invalid := []ciProvisioningCreateOutcome{
			{MutationAttempted: false, CreationSettled: false},
			{MutationAttempted: true, CreationSettled: true, Resources: []ciOwnedResource{sg}},
			{MutationAttempted: true, CreationSettled: false},
			{MutationAttempted: true, CreationSettled: false, Resources: []ciOwnedResource{
				{Kind: "security_group", ID: "sg_changed", OperationID: plan.OperationID}}},
			{MutationAttempted: true, CreationSettled: false, Resources: []ciOwnedResource{sg, port, server}},
		}
		for _, outcome := range invalid {
			ciCreationExpectConflict(t, s.recordCIProvisioningCreationResult(ctx, workspace, runner, policy.ProfileDigest, plan, outcome))
		}
		j := ciCreationJournal(t, s, workspace, runner, policy, plan)
		if j.Stage != "port" || j.FinishedAt != nil || j.Outcome != nil {
			t.Fatalf("invalid outcomes moved journal: %+v", j)
		}
	})

	t.Run("record before global fence", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "creation_op_prefence", 1, now, 1000)
		ciCreationExpectConflict(t, s.recordCIProvisioningCreationResult(ctx, workspace, runner, policy.ProfileDigest, plan,
			ciProvisioningCreateOutcome{CreationSettled: true}))
	})
}

func TestCIProvisioningCreationUnstartedRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("seal without stop conflicts", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "creation_op_seal0", 1, now, 1000)
		ciCreationExpectConflict(t, s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, policy.ProfileDigest, plan))
	})

	t.Run("seal before global fence", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "creation_op_seal1", 1, now, 1000)
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, policy.ProfileDigest, plan); err != nil {
			t.Fatalf("seal: %v", err)
		}
		j := ciCreationJournal(t, s, workspace, runner, policy, plan)
		if j.Stage != "not_started" || j.FinishedAt == nil || j.Outcome == nil ||
			j.Outcome.MutationAttempted || !j.Outcome.CreationSettled || len(j.Outcome.Resources) != 0 {
			t.Fatalf("sealed journal: %+v", j)
		}
		if err := s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, policy.ProfileDigest, plan); err != nil {
			t.Fatalf("seal retry: %v", err)
		}
	})

	t.Run("seal after global fence", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_seal2", 1, now, 1000)
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, policy.ProfileDigest, plan); err != nil {
			t.Fatalf("seal: %v", err)
		}
		j := ciCreationJournal(t, s, workspace, runner, policy, plan)
		if j.Stage != "not_started" || j.FinishedAt == nil || j.Outcome == nil || !j.Outcome.CreationSettled {
			t.Fatalf("sealed journal: %+v", j)
		}
	})

	t.Run("seal denied once a stage ran", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_seal3", 1, now, 1000)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
		// Even an empty unknown SDK outcome is a finished stage, never an
		// absence proof: inventory emptiness cannot justify unstarted seal.
		ciCreationRecord(t, s, workspace, runner, policy, plan,
			ciProvisioningCreateOutcome{MutationAttempted: true, CreationSettled: false})
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		ciCreationExpectConflict(t, s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, policy.ProfileDigest, plan))
	})

	t.Run("server stage blocks seal and replay", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_seal4", 1, now, 1000)
		sg, port, _ := ciCreationResources(plan.OperationID)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "server", []ciOwnedResource{sg, port})
		j := ciCreationJournal(t, s, workspace, runner, policy, plan)
		if j.Stage != "server" || len(j.KnownResources) != 2 || j.FinishedAt != nil || j.Outcome != nil {
			t.Fatalf("journal before result: %+v", j)
		}
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "server", []ciOwnedResource{sg, port}))
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		ciCreationExpectConflict(t, s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, policy.ProfileDigest, plan))
	})
}

func TestCIProvisioningCreationSessionLockInteroperation(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_lock", 1, now, 1000)
	ctx := context.Background()

	// A worker session holding the lifecycle advisory lock must make the
	// stage fence report pending, never deadlock or wait on row locks.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	key := "ci/" + workspace + "/" + runner
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,948))`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if !locked {
			return
		}
		uctx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ucancel()
		if _, uerr := conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtextextended($1,948))`, key); uerr != nil {
			// Never return a still-locked connection to the pool.
			conn.Hijack().Close(context.Background())
		}
	}()
	stageCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.fenceCIProvisioningStage(stageCtx, workspace, runner, policy.ProfileDigest, plan, "security_group", nil); !errors.Is(err, errCIRunnerJobPending) {
		t.Fatalf("stage under session lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,948))`, key); err != nil {
		t.Fatal(err)
	}
	locked = false
	ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
	j := ciCreationJournal(t, s, workspace, runner, policy, plan)
	if j.Stage != "security_group" {
		t.Fatalf("journal: %+v", j)
	}
}

func TestCIProvisioningCreationMaxConnsOne(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)

	old := s.pool
	cfg := *old.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.pool = pool
	defer func() {
		s.pool = old
		pool.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err = s.authorizeCIProvisioningBudget(ctx, ciProvisioningAuthorization{ID: "credit_1", Policy: policy, AmountMicros: 1000}); err != nil {
		t.Fatalf("authorize under MaxConns=1: %v", err)
	}
	runner, plan := ciBudgetSeed(t, s, workspace, "creation_op_max1", 1, now)
	if _, err = s.reserveCIProvisioningBudget(ctx, workspace, runner, policy.ProfileDigest, plan); err != nil {
		t.Fatalf("reserve under MaxConns=1: %v", err)
	}
	fenced, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil || !fenced {
		t.Fatalf("fence under MaxConns=1: fenced=%v err=%v", fenced, err)
	}
	sg, port, server := ciCreationResources(plan.OperationID)
	stages := []struct {
		stage string
		known []ciOwnedResource
	}{
		{"security_group", nil},
		{"port", []ciOwnedResource{sg}},
		{"server", []ciOwnedResource{sg, port}},
	}
	for _, st := range stages {
		if err = s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, st.stage, st.known); err != nil {
			t.Fatalf("stage %s under MaxConns=1: %v", st.stage, err)
		}
	}
	if err = s.recordCIProvisioningCreationResult(ctx, workspace, runner, policy.ProfileDigest, plan, ciProvisioningCreateOutcome{
		MutationAttempted: true, CreationSettled: true,
		Resources: []ciOwnedResource{sg, port, server},
	}); err != nil {
		t.Fatalf("record under MaxConns=1: %v", err)
	}
	j, err := s.ciProvisioningCreationState(ctx, workspace, runner, policy.ProfileDigest, plan)
	if err != nil || j.FinishedAt == nil || j.Outcome == nil || !j.Outcome.CreationSettled {
		t.Fatalf("getter under MaxConns=1: %+v err=%v", j, err)
	}
}

func TestCIProvisioningCreationLoaderRejectsCorruption(t *testing.T) {
	ctx := context.Background()

	t.Run("result digest mismatch", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_corrupt1", 1, now, 1000)
		ciCreationComplete(t, s, workspace, runner, policy, plan)
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_creation_journal SET result_digest=$2 WHERE operation_id=$1`, plan.OperationID, strings.Repeat("f", 64)); err != nil {
			t.Fatal(err)
		}
		_, err := s.ciProvisioningCreationState(ctx, workspace, runner, policy.ProfileDigest, plan)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("corrupt digest getter: %v", err)
		}
	})

	t.Run("foreign result resources", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationSetup(t, s, workspace, "creation_op_corrupt2", 1, now, 1000)
		ciCreationComplete(t, s, workspace, runner, policy, plan)
		foreign := `[{"kind":"server","id":"foreign","operationId":"wrong"}]`
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runner_creation_journal SET result_resources=$2::jsonb WHERE operation_id=$1`, plan.OperationID, foreign); err != nil {
			t.Fatal(err)
		}
		_, err := s.ciProvisioningCreationState(ctx, workspace, runner, policy.ProfileDigest, plan)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("foreign resources getter: %v", err)
		}
	})
}
