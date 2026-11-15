package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	lcServer = ciOwnedResource{Kind: "server", ID: "srv_lc_1", OperationID: "op_ci_test"}
	lcSG     = ciOwnedResource{Kind: "security_group", ID: "sg_lc_1", OperationID: "op_ci_test"}
	lcPort   = ciOwnedResource{Kind: "port", ID: "pt_lc_1", OperationID: "op_ci_test"}
)

func lcResources() []ciOwnedResource {
	return []ciOwnedResource{lcServer, lcSG, lcPort}
}

func lcPlan(now time.Time) ciRunnerLifecyclePlan {
	return ciRunnerLifecyclePlan{
		OperationID:     "op_ci_test",
		ServerName:      ciAdmissionName,
		Region:          "test",
		FlavorID:        "flavor1",
		ImageID:         "image1",
		NetworkID:       "net1",
		EnforcerLease:   "lease1",
		SetupDeadlineAt: now.Add(900 * time.Second),
		HardDeadlineAt:  now.Add(22500 * time.Second),
	}
}

func lcTrack(t *testing.T, s *Store, workspace, runner string, plan ciRunnerLifecyclePlan) {
	t.Helper()
	if err := s.trackCIRunnerLifecycle(context.Background(), workspace, runner, plan); err != nil {
		t.Fatal(err)
	}
}

type lcState struct {
	stopRequested, billStop, stoppedVerified, cleanupVerified *time.Time
	deleteRequested                                           bool
	phase                                                     string
	execStopped                                               *time.Time
}

func lcLifecycleState(t *testing.T, s *Store, workspace, runner string) lcState {
	t.Helper()
	var st lcState
	err := s.pool.QueryRow(context.Background(), `SELECT l.stop_requested_at,l.bill_stop_at,l.stopped_verified_at,l.cleanup_verified_at,r.delete_requested,r.phase,r.execution_stopped_at FROM ci_runner_lifecycles l JOIN ci_runners r ON r.workspace_id=l.workspace_id AND r.id=l.runner_id WHERE l.workspace_id=$1 AND l.runner_id=$2`, workspace, runner).Scan(&st.stopRequested, &st.billStop, &st.stoppedVerified, &st.cleanupVerified, &st.deleteRequested, &st.phase, &st.execStopped)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func lcLifecycleRows(t *testing.T, s *Store, workspace, runner string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_runner_lifecycles WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// lcFixture returns a registered runner plus a tracked lifecycle plan, before
// any admission or execution.
func lcFixture(t *testing.T) (*Store, string, string, time.Time, ciRunnerLifecyclePlan) {
	t.Helper()
	s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
	plan := lcPlan(s.now())
	lcTrack(t, s, workspace, runner, plan)
	return s, workspace, runner, start, plan
}

func lcAdmit(t *testing.T, s *Store, workspace, runner string, req ciAdmissionRequest, proof ciRunnerJobProof) {
	t.Helper()
	if _, err := s.admitCIRunnerJob(context.Background(), workspace, runner, req, &proof, 7200); err != nil {
		t.Fatal(err)
	}
}

func TestCILifecycleTrackScopeImmutableAndInvalid(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, _, _ := ciAdmissionJobFixture(t)
	now := s.now()
	plan := lcPlan(now)

	lcTrack(t, s, workspace, runner, plan)
	if n := lcLifecycleRows(t, s, workspace, runner); n != 1 {
		t.Fatalf("lifecycle rows = %d", n)
	}
	// Exact retry is unchanged even though deadlines are still valid.
	lcTrack(t, s, workspace, runner, plan)
	// A retry after the deadlines have passed is still an exact no-op.
	past := s.now
	s.now = func() time.Time { return now.Add(48 * time.Hour) }
	lcTrack(t, s, workspace, runner, plan)
	s.now = past

	// Different plan fields or operation conflict.
	for name, mutate := range map[string]func(*ciRunnerLifecyclePlan){
		"operation":  func(p *ciRunnerLifecyclePlan) { p.OperationID = "op_ci_other" },
		"server":     func(p *ciRunnerLifecyclePlan) { p.ServerName = "canter-test-2" },
		"region":     func(p *ciRunnerLifecyclePlan) { p.Region = "us-east" },
		"flavor":     func(p *ciRunnerLifecyclePlan) { p.FlavorID = "flavor2" },
		"image":      func(p *ciRunnerLifecyclePlan) { p.ImageID = "image2" },
		"network":    func(p *ciRunnerLifecyclePlan) { p.NetworkID = "net2" },
		"lease":      func(p *ciRunnerLifecyclePlan) { p.EnforcerLease = "lease2" },
		"setup_dead": func(p *ciRunnerLifecyclePlan) { p.SetupDeadlineAt = p.SetupDeadlineAt.Add(time.Second) },
		"hard_dead":  func(p *ciRunnerLifecyclePlan) { p.HardDeadlineAt = p.HardDeadlineAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			other := plan
			mutate(&other)
			if err := s.trackCIRunnerLifecycle(ctx, workspace, runner, other); !errors.Is(err, ErrConflict) {
				t.Fatalf("mutated plan: %v", err)
			}
		})
	}

	// Foreign workspace and missing runner are not found.
	_, other, _, err := s.Signup(ctx, "lc-foreign@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.trackCIRunnerLifecycle(ctx, other.ID, runner, plan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign workspace: %v", err)
	}
	if err := s.trackCIRunnerLifecycle(ctx, workspace, "ci_missing_runner", plan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing runner: %v", err)
	}

	// Invalid bounds and shapes fail before any row exists.
	s2, workspace2, runner2, _, _, _ := ciAdmissionJobFixture(t)
	bad := lcPlan(s2.now())
	cases := map[string]func(*ciRunnerLifecyclePlan){
		"setup_past":  func(p *ciRunnerLifecyclePlan) { p.SetupDeadlineAt = s2.now().Add(-time.Second) },
		"setup_far":   func(p *ciRunnerLifecyclePlan) { p.SetupDeadlineAt = s2.now().Add(901 * time.Second) },
		"hard_before": func(p *ciRunnerLifecyclePlan) { p.HardDeadlineAt = p.SetupDeadlineAt.Add(-time.Second) },
		"hard_far":    func(p *ciRunnerLifecyclePlan) { p.HardDeadlineAt = s2.now().Add(22501 * time.Second) },
		"zero_setup":  func(p *ciRunnerLifecyclePlan) { p.SetupDeadlineAt = time.Time{} },
		"bad_op":      func(p *ciRunnerLifecyclePlan) { p.OperationID = "bad op!" },
		"bad_server":  func(p *ciRunnerLifecyclePlan) { p.ServerName = "bad name!" },
		"bad_region":  func(p *ciRunnerLifecyclePlan) { p.Region = "" },
		"empty_lease": func(p *ciRunnerLifecyclePlan) { p.EnforcerLease = "" },
		"long_op":     func(p *ciRunnerLifecyclePlan) { p.OperationID = strings.Repeat("o", 129) },
	}
	for name, mutate := range cases {
		t.Run("invalid_"+name, func(t *testing.T) {
			p := bad
			mutate(&p)
			if err := s2.trackCIRunnerLifecycle(ctx, workspace2, runner2, p); !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid plan accepted: %v", err)
			}
		})
	}
	if n := lcLifecycleRows(t, s2, workspace2, runner2); n != 0 {
		t.Fatalf("invalid plans left %d rows", n)
	}

	// Denied phases other than queued/provisioning/registering/idle. This
	// runs before the next fixture because each fixture truncates the schema.
	for _, phase := range []string{"running", "retaining", "deleting", "deleted", "cleanup_failed"} {
		ciAdmissionPhase(t, s2, workspace2, runner2, phase, false)
		if err := s2.trackCIRunnerLifecycle(ctx, workspace2, runner2, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("phase %s: %v", phase, err)
		}
	}

	// Tracking is denied once execution has started.
	s3, workspace3, runner3, _, req3, proof3 := ciAdmissionJobFixture(t)
	lcAdmit(t, s3, workspace3, runner3, req3, proof3)
	if err := s3.trackCIRunnerLifecycle(ctx, workspace3, runner3, lcPlan(s3.now())); !errors.Is(err, ErrConflict) {
		t.Fatalf("track after execution start: %v", err)
	}
}

func TestCILifecycleCreationFenceOneShot(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, _ := lcFixture(t)

	// idle is not a creatable phase.
	if ok, err := s.fenceCIRunnerCreate(ctx, workspace, runner); !errors.Is(err, ErrConflict) || ok {
		t.Fatalf("idle fence: ok=%v err=%v", ok, err)
	}
	ciAdmissionPhase(t, s, workspace, runner, "provisioning", false)

	ok, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil || !ok {
		t.Fatalf("first fence: ok=%v err=%v", ok, err)
	}
	// One-shot: every later call reports false, never an error.
	for i := 0; i < 3; i++ {
		ok, err = s.fenceCIRunnerCreate(ctx, workspace, runner)
		if err != nil || ok {
			t.Fatalf("repeat fence %d: ok=%v err=%v", i, ok, err)
		}
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.phase != "provisioning" {
		t.Fatalf("phase after fence = %s", st.phase)
	}

	// Concurrent fences on a fresh runner: exactly one wins.
	s2, workspace2, runner2, _, _, _ := ciAdmissionJobFixture(t)
	lcTrack(t, s2, workspace2, runner2, lcPlan(s2.now()))
	ciAdmissionPhase(t, s2, workspace2, runner2, "queued", false)
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s2.fenceCIRunnerCreate(ctx, workspace2, runner2)
		}(i)
	}
	wg.Wait()
	wins := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent fence %d: %v", i, errs[i])
		}
		if results[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent fences won %d times", wins)
	}

	// Denials: delete requested, stop intent, expired deadlines, idle phase.
	deny := func(name string, mutate func(t *testing.T, s *Store, w, r string), advance func(*Store)) {
		t.Run(name, func(t *testing.T) {
			sd, wd, rd, _, _ := lcFixture(t)
			ciAdmissionPhase(t, sd, wd, rd, "provisioning", false)
			if mutate != nil {
				mutate(t, sd, wd, rd)
			}
			if advance != nil {
				advance(sd)
			}
			ok, err := sd.fenceCIRunnerCreate(ctx, wd, rd)
			if !errors.Is(err, ErrConflict) || ok {
				t.Fatalf("denied fence: ok=%v err=%v", ok, err)
			}
		})
	}
	deny("delete_requested", func(t *testing.T, s *Store, w, r string) {
		ciAdmissionPhase(t, s, w, r, "provisioning", true)
	}, nil)
	deny("stop_intent", func(t *testing.T, s *Store, w, r string) {
		if _, err := s.requestCIRunnerStop(ctx, w, r, "setup_failed", nil); err != nil {
			t.Fatal(err)
		}
	}, nil)
	deny("setup_deadline_passed", nil, func(s *Store) {
		base := s.now()
		s.now = func() time.Time { return base.Add(901 * time.Second) }
	})
	deny("idle_phase", func(t *testing.T, s *Store, w, r string) {
		ciAdmissionPhase(t, s, w, r, "idle", false)
	}, nil)
	deny("running_phase", func(t *testing.T, s *Store, w, r string) {
		ciAdmissionPhase(t, s, w, r, "running", false)
	}, nil)
}

// lcObservation builds a provider observation for the standard inventory.
func lcObservation(discovered []ciOwnedResource, reads []ciResourceRead, ports []ciServerPorts, discovery, createSettled, enforcerSettled bool) ciResourceObservation {
	return ciResourceObservation{
		OperationID: "op_ci_test", Discovered: discovered, Reads: reads,
		ServerPorts: ports, DiscoveryComplete: discovery,
		CreateSettled: createSettled, EnforcerSettled: enforcerSettled,
	}
}

func lcRead(r ciOwnedResource, status int, serverState string) ciResourceRead {
	state := ""
	if r.Kind == "server" {
		state = serverState
	}
	return ciResourceRead{Resource: r, HTTPStatus: status, ServerState: state}
}

func lcReads(resources []ciOwnedResource, status int, serverState string) []ciResourceRead {
	out := make([]ciResourceRead, 0, len(resources))
	for _, r := range resources {
		out = append(out, lcRead(r, status, serverState))
	}
	return out
}

func lcPortList(portIDs []string) []ciServerPorts {
	return []ciServerPorts{{ServerID: lcServer.ID, Complete: true, PortIDs: portIDs}}
}

// lcBackend scripts provider observations per Observe call.
type lcBackend struct {
	mu                  sync.Mutex
	observeCalls        int
	stopCalls           int
	deleteCalls         int
	settleEnforcerCalls int
	observeFn           func(resources []ciOwnedResource, call int) (ciResourceObservation, error)
	stopErr             error
	deleteErr           error
	settleErr           error
	stopSeen            [][]ciOwnedResource
	deleteSeen          [][]ciOwnedResource
}

func (b *lcBackend) Observe(_ context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) (ciResourceObservation, error) {
	b.mu.Lock()
	b.observeCalls++
	call := b.observeCalls
	fn := b.observeFn
	b.mu.Unlock()
	if fn != nil {
		return fn(resources, call)
	}
	return lcObservation(nil, nil, nil, true, true, false), nil
}

func (b *lcBackend) Stop(_ context.Context, _ ciRunnerLifecyclePlan, resources []ciOwnedResource) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopCalls++
	b.stopSeen = append(b.stopSeen, append([]ciOwnedResource(nil), resources...))
	return b.stopErr
}

func (b *lcBackend) Delete(_ context.Context, _ ciRunnerLifecyclePlan, resources []ciOwnedResource) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteCalls++
	b.deleteSeen = append(b.deleteSeen, append([]ciOwnedResource(nil), resources...))
	return b.deleteErr
}

func (b *lcBackend) SettleEnforcer(_ context.Context, _ ciRunnerLifecyclePlan) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settleEnforcerCalls++
	return b.settleErr
}

func (b *lcBackend) counts() (observe, stop, delete, settle int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observeCalls, b.stopCalls, b.deleteCalls, b.settleEnforcerCalls
}

// lcGitHub stubs the App token, repository, and single-job endpoints used by
// the completion proof path.
func lcGitHub(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *ciSourceFixture {
	t.Helper()
	f := newCISourceFixture(t)
	f.setRespond(respond)
	return f
}

func lcGitHubDefault(t *testing.T, jobBody string) *ciSourceFixture {
	t.Helper()
	return lcGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/123/access_tokens":
			fmt.Fprint(w, `{"token":"ghs_lifecycle_fixture","expires_at":"2099-01-01T00:00:00Z"}`)
		case r.URL.Path == "/repositories/789":
			fmt.Fprint(w, `{"id":789,"full_name":"financial-tests/repo","private":true}`)
		case r.URL.Path == "/repos/financial-tests/repo/actions/jobs/1001":
			fmt.Fprint(w, jobBody)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
}

func lcCompletedJobJSON(startedAt, completedAt time.Time) string {
	return fmt.Sprintf(`{"id":1001,"run_id":1001,"run_attempt":1,"runner_id":31,"runner_name":"canter-test-1","status":"completed","started_at":%q,"completed_at":%q,"conclusion":"success","labels":[]}`, startedAt.Format(time.RFC3339), completedAt.Format(time.RFC3339))
}

// lcHappyObservations returns the four-observation script for one full
// reconcile: active, shutoff, gone-without-enforcer, gone-settled.
func lcHappyObservations() []ciResourceObservation {
	res := lcResources()
	return []ciResourceObservation{
		lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false),
		lcObservation(res, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, false),
		lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, false),
		lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, true),
	}
}

func lcScriptedBackend(obs []ciResourceObservation) *lcBackend {
	return &lcBackend{observeFn: func(_ []ciOwnedResource, call int) (ciResourceObservation, error) {
		if call > len(obs) {
			call = len(obs)
		}
		return obs[call-1], nil
	}}
}

func TestCILifecycleReconcileCompletionStop(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)

	proofStart := proof.StartedAt
	completedAt := proofStart.Add(3601 * time.Second)
	f := lcGitHubDefault(t, lcCompletedJobJSON(proofStart, completedAt))
	backend := lcScriptedBackend(lcHappyObservations())
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend, GitHub: f.client}

	// GitHub reports the job completed; the reconciler requests stop itself.
	s.now = func() time.Time { return proofStart.Add(3610 * time.Second) }
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.cleanupVerified == nil {
		t.Fatalf("lifecycle not cleaned: %+v", st)
	}
	if st.stopRequested == nil || st.billStop == nil || !st.billStop.Equal(completedAt) {
		t.Fatalf("stop intent/bill stop: %+v", st)
	}
	if st.stoppedVerified == nil || st.execStopped == nil {
		t.Fatalf("stop/settlement not verified: %+v", st)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 2000 || cents != 0 || reserved != 0 {
		t.Fatalf("settlement totals units=%d cents=%d reserved=%d", units, cents, reserved)
	}
	// The settled runtime starts at the source-proved clock, not a VM clock.
	var execStart time.Time
	if err := s.pool.QueryRow(ctx, `SELECT execution_started_at FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&execStart); err != nil {
		t.Fatal(err)
	}
	if !execStart.Equal(proofStart) {
		t.Fatalf("execution start = %v want %v", execStart, proofStart)
	}
	// Backend ordering: stop before delete, enforcer settled exactly once.
	_, stops, deletes, settles := backend.counts()
	if stops != 1 || deletes != 1 || settles != 1 {
		t.Fatalf("backend calls stop=%d delete=%d settle=%d", stops, deletes, settles)
	}
	// Admission identity is revoked as soon as stop is requested.
	if _, err := s.ciRunnerAdmissionScope(ctx, workspace, runner, ciAdmissionNonce("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admission scope after stop: %v", err)
	}
}

func TestCILifecycleReconcileDeadlineStop(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)

	// GitHub never confirms completion; the execution deadline drives stop.
	backend := lcScriptedBackend(lcHappyObservations())
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}

	deadline := proof.StartedAt.Add(7200 * time.Second)
	// Just before the deadline nothing happens.
	s.now = func() time.Time { return deadline.Add(-time.Second) }
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.stopRequested != nil || st.phase == "deleted" {
		t.Fatalf("early deadline stop: %+v", st)
	}
	// At the exact deadline the stop is allowed and the ceiling is the cap.
	// The paid run is past the free hour with no GitHub proof, so settlement
	// defers while physical cleanup still completes.
	s.now = func() time.Time { return deadline }
	if err := worker.Reconcile(ctx, workspace, runner); !errors.Is(err, errCILifecycleSettlementPending) {
		t.Fatalf("expected deferred settlement, got %v", err)
	}
	st = lcLifecycleState(t, s, workspace, runner)
	if st.stopRequested == nil || st.billStop == nil || !st.billStop.Equal(deadline) {
		t.Fatalf("deadline stop: %+v", st)
	}
	if st.phase != "deleted" || st.execStopped != nil {
		t.Fatalf("cleanup/settlement state: %+v", st)
	}
	if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
		t.Fatalf("idempotent stop retry: %v", err)
	}
	st = lcLifecycleState(t, s, workspace, runner)
	if st.billStop == nil || !st.billStop.Equal(deadline) {
		t.Fatalf("frozen ceiling moved: %+v", st)
	}
}

func TestCILifecycleReconcileSetupFailedUnstarted(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))

	backend := lcScriptedBackend([]ciResourceObservation{
		lcObservation(nil, nil, nil, true, true, false),
		lcObservation(nil, nil, nil, true, true, true),
	})
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}

	// Before the setup deadline the runner is untouched.
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if st := lcLifecycleState(t, s, workspace, runner); st.stopRequested != nil {
		t.Fatalf("premature setup stop: %+v", st)
	}
	// After it, setup_failed stop intent drives the empty cleanup path.
	s.now = func() time.Time { return start.Add(911 * time.Second) }
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.cleanupVerified == nil || st.billStop != nil {
		t.Fatalf("setup_failed cleanup: %+v", st)
	}
	if st.execStopped != nil {
		t.Fatalf("unstarted runner settled billing: %+v", st)
	}
	_, stops, deletes, settles := backend.counts()
	if stops != 1 || deletes != 1 || settles != 1 {
		t.Fatalf("backend calls stop=%d delete=%d settle=%d", stops, deletes, settles)
	}
}

func TestCILifecycleReconcileNoBackendFailsClosed(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	worker := &ciRunnerLifecycleWorker{Store: s}

	// Idle runner with future deadlines: complete no-op.
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if st := lcLifecycleState(t, s, workspace, runner); st.stopRequested != nil || st.phase != "idle" {
		t.Fatalf("unexpected mutation: %+v", st)
	}
	// Expired setup deadline still records stop intent (DB-only), but no
	// provider work or deletion can happen without a backend.
	s.now = func() time.Time { return start.Add(911 * time.Second) }
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.stopRequested == nil || !st.deleteRequested || st.phase != "deleting" || st.cleanupVerified != nil {
		t.Fatalf("backend-less stop intent: %+v", st)
	}
}

func TestCILifecycleReconcileEvidenceDenials(t *testing.T) {
	ctx := context.Background()
	res := lcResources()

	setup := func(t *testing.T) (*Store, string, string, *lcBackend, *ciRunnerLifecycleWorker) {
		t.Helper()
		s, workspace, runner, start, req, proof := ciAdmissionJobFixture(t)
		lcTrack(t, s, workspace, runner, lcPlan(s.now()))
		lcAdmit(t, s, workspace, runner, req, proof)
		s.now = func() time.Time { return start.Add(4000 * time.Second) }
		res := lcResources()
		backend := &lcBackend{observeFn: func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		}}
		worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}
		// One inventory-only pass retains the discovered IDs before the
		// stop intent so later observations must cover them.
		if err := worker.Reconcile(ctx, workspace, runner); err != nil {
			t.Fatal(err)
		}
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		return s, workspace, runner, backend, worker
	}
	assertNotDeleted := func(t *testing.T, s *Store, workspace, runner string) {
		t.Helper()
		st := lcLifecycleState(t, s, workspace, runner)
		if st.phase == "deleted" || st.cleanupVerified != nil {
			t.Fatalf("denied observation marked deleted: %+v", st)
		}
	}

	t.Run("active_server", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("delete_without_proof", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		// Delete succeeds but every resource still reads 200.
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(res, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, true), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("missing_direct_read", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			reads := lcReads(res, 404, "")
			return lcObservation(nil, reads[:2], lcPortList([]string{}), true, true, true), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("incomplete_discovery", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), false, true, true), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("nil_port_list", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(nil, lcReads(res, 404, ""), []ciServerPorts{{ServerID: lcServer.ID, Complete: true, PortIDs: nil}}, true, true, true), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("missing_port_list", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(nil, lcReads(res, 404, ""), nil, true, true, true), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("late_owned_vm", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		late := ciOwnedResource{Kind: "server", ID: "srv_lc_late", OperationID: "op_ci_test"}
		call := 0
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			call++
			switch call {
			case 1:
				return lcObservation(nil, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
			case 2:
				return lcObservation(nil, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, false), nil
			case 3:
				return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, false), nil
			default:
				// A new owned VM appears after earlier absence: it must be
				// discovered, retained, and block deletion.
				reads := append(lcReads(res, 404, ""), lcRead(late, 200, "ACTIVE"))
				ports := append(lcPortList([]string{}), ciServerPorts{ServerID: late.ID, Complete: true, PortIDs: []string{}})
				return lcObservation([]ciOwnedResource{late}, reads, ports, true, true, true), nil
			}
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
		// The late ID was retained for the next pass.
		snap, err := s.loadCIRunnerLifecycle(ctx, w, r)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, res := range snap.Resources {
			if res.ID == "srv_lc_late" {
				found = true
			}
		}
		if !found {
			t.Fatal("late owned VM ID was not retained")
		}
	})

	t.Run("foreign_metadata", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		foreign := ciOwnedResource{Kind: "server", ID: "srv_foreign", OperationID: "op_other"}
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation([]ciOwnedResource{foreign}, append(lcReads(res, 200, "SHUTOFF"), lcRead(foreign, 200, "ACTIVE")), lcPortList([]string{lcPort.ID}), true, true, false), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("create_unsettled", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, false, true), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
	})

	t.Run("enforcer_unsettled", func(t *testing.T) {
		s, w, r, b, worker := setup(t)
		b.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, false), nil
		}
		_ = worker.Reconcile(ctx, w, r)
		assertNotDeleted(t, s, w, r)
		if _, _, _, settles := b.counts(); settles != 1 {
			t.Fatalf("enforcer settles = %d", settles)
		}
	})
}

func TestCILifecycleReconcilePreservesIDsAcrossPasses(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	// Unstarted setup_failed path keeps inventory across worker restarts.
	s.now = func() time.Time { return start.Add(911 * time.Second) }

	res := lcResources()
	backend := &lcBackend{observeFn: func(_ []ciOwnedResource, call int) (ciResourceObservation, error) {
		switch call {
		case 1:
			// First pass discovers the full inventory while still running.
			return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, false, false), nil
		case 2:
			// Setup deadline passes; stop intent exists. Discovery now
			// reports nothing new but reads still cover retained IDs.
			return lcObservation(nil, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		default:
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, call%2 == 0), nil
		}
	}}
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	snap, err := s.loadCIRunnerLifecycle(ctx, workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Resources) != 3 {
		t.Fatalf("retained resources = %d", len(snap.Resources))
	}
	// A fresh worker (simulated restart) keeps the same inventory and op.
	worker2 := &ciRunnerLifecycleWorker{Store: s, Backend: backend}
	if err := worker2.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	snap, err = s.loadCIRunnerLifecycle(ctx, workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Resources) != 3 || snap.Plan.OperationID != "op_ci_test" {
		t.Fatalf("inventory lost across restart: %+v", snap.Plan)
	}
}

func TestCILifecycleReconcileFailureRecoverySameOperation(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, _, _ := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	s.now = func() time.Time { return start.Add(911 * time.Second) }

	res := lcResources()
	backend := &lcBackend{
		stopErr:   errors.New("provider stop unavailable"),
		deleteErr: errors.New("provider delete unavailable"),
		observeFn: func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		},
	}
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}

	// Pass 1: setup_failed intent, inventory retained, ordinary stop failure
	// falls through to delete which also fails -> cleanup_failed, intent kept.
	if err := worker.Reconcile(ctx, workspace, runner); err == nil {
		t.Fatal("expected delete error")
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.stopRequested == nil || st.cleanupVerified != nil || st.phase != "cleanup_failed" {
		t.Fatalf("failed cleanup not marked: %+v", st)
	}
	// Pass 2: provider recovers; retained IDs read 404 and the same
	// operation completes cleanup.
	backend.stopErr = nil
	backend.deleteErr = nil
	backend.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
		return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, true), nil
	}
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st = lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.cleanupVerified == nil {
		t.Fatalf("recovery did not complete: %+v", st)
	}
	snap, err := s.loadCIRunnerLifecycle(ctx, workspace, runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan.OperationID != "op_ci_test" {
		t.Fatalf("operation changed: %s", snap.Plan.OperationID)
	}
}

func TestCILifecycleSettlementDeferredThenRecovered(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)

	proofStart := proof.StartedAt
	completedAt := proofStart.Add(3601 * time.Second)
	// Stop intent frozen at +3610 while GitHub is unavailable.
	s.now = func() time.Time { return proofStart.Add(3610 * time.Second) }
	if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	res := lcResources()
	call := 0
	backend := &lcBackend{}
	backend.observeFn = func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
		call++
		switch call {
		case 1:
			return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		case 2:
			return lcObservation(res, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		default:
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, true), nil
		}
	}
	// GitHub client exists but every call fails: no completion proof.
	dead := newCISourceFixture(t)
	dead.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"down"}`)
	})
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend, GitHub: dead.client}

	if err := worker.Reconcile(ctx, workspace, runner); !errors.Is(err, errCILifecycleSettlementPending) {
		t.Fatalf("expected settlement pending, got %v", err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.cleanupVerified == nil || st.execStopped != nil {
		t.Fatalf("cleanup should complete without settlement: %+v", st)
	}
	units, _, _ := ciFinancialTotals(t, s, workspace)
	if units != 0 {
		t.Fatalf("deferred settlement charged %d units", units)
	}
	// Account closure is blocked while a paid open clock is unsettled.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := s.closeCIWorkspaceTx(ctx, tx, workspace); !errors.Is(err, ErrConflict) {
		t.Fatalf("workspace close with open paid clock: %v", err)
	}
	tx.Rollback(ctx)

	// GitHub recovers: the next pass performs financial settlement only —
	// no new provider observations.
	f := lcGitHubDefault(t, lcCompletedJobJSON(proofStart, completedAt))
	worker.GitHub = f.client
	before := backend.observeCalls
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	if backend.observeCalls != before {
		t.Fatalf("financial retry made provider calls: %d -> %d", before, backend.observeCalls)
	}
	st = lcLifecycleState(t, s, workspace, runner)
	if st.execStopped == nil {
		t.Fatalf("settlement did not complete: %+v", st)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 2000 || cents != 0 || reserved != 0 {
		t.Fatalf("recovered settlement units=%d cents=%d reserved=%d", units, cents, reserved)
	}
	// With the clock settled the workspace may close and the lifecycle row
	// is erased even though financial history remains.
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := s.closeCIWorkspaceTx(ctx, tx, workspace); err != nil {
		t.Fatalf("workspace close after settlement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := lcLifecycleRows(t, s, workspace, runner); n != 0 {
		t.Fatalf("lifecycle rows after close = %d", n)
	}
}

func TestCILifecycleConcurrentStopIntentFirstWinner(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, start, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)
	s.now = func() time.Time { return start.Add(100 * time.Second) }

	completedAt := proof.StartedAt.Add(50 * time.Second)
	var wg sync.WaitGroup
	bills := make([]time.Time, 2)
	errs := make([]error, 2)
	for i, reason := range []string{"cancelled", "completed"} {
		wg.Add(1)
		go func(i int, reason string) {
			defer wg.Done()
			var at *time.Time
			if reason == "completed" {
				at = &completedAt
			}
			bills[i], errs[i] = s.requestCIRunnerStop(ctx, workspace, runner, reason, at)
		}(i, reason)
	}
	wg.Wait()
	st := lcLifecycleState(t, s, workspace, runner)
	if st.stopRequested == nil || st.billStop == nil {
		t.Fatalf("no frozen stop intent: %+v", st)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("concurrent stop %d: %v", i, errs[i])
		}
	}
	// The loser observes the winner's frozen ceiling, not its own request.
	if !st.billStop.Equal(bills[0]) || !st.billStop.Equal(bills[1]) {
		t.Fatalf("frozen ceiling mismatch: state=%v calls=%v", st.billStop, bills)
	}
	again, err := s.requestCIRunnerStop(ctx, workspace, runner, "deadline", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(*st.billStop) {
		t.Fatalf("frozen ceiling changed: %v vs %v", again, st.billStop)
	}
}

// A source-proven running job must never let an empty retained plus
// discovered inventory vacuously prove stop, deletion, settlement, or
// completion. Observation still runs so a late discovery can supply the
// missing server identity; until then every provider mutation stays off.
func TestCILifecycleRunningJobRequiresServerID(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)

	// Frozen stop well inside the free hour so settlement would need no
	// completion proof if it were ever reached.
	s.now = func() time.Time { return proof.StartedAt.Add(100 * time.Second) }
	if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
		t.Fatal(err)
	}

	res := lcResources()
	var mu sync.Mutex
	discover := false
	call := 0
	backend := &lcBackend{observeFn: func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
		mu.Lock()
		call++
		c := call
		d := discover
		mu.Unlock()
		if !d {
			// Every provider flag satisfied yet zero known or discovered
			// IDs: must fail closed for a running job.
			return lcObservation(nil, nil, nil, true, true, true), nil
		}
		switch c {
		case 2:
			return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		case 3:
			return lcObservation(res, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, false), nil
		case 4:
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, false), nil
		default:
			return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, true), nil
		}
	}}
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}

	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if _, stops, deletes, settles := backend.counts(); stops != 0 || deletes != 0 || settles != 0 {
		t.Fatalf("provider mutations on empty inventory: stop=%d delete=%d settle=%d", stops, deletes, settles)
	}
	if st.phase == "deleted" || st.cleanupVerified != nil || st.stoppedVerified != nil || st.execStopped != nil {
		t.Fatalf("empty inventory proved lifecycle state: %+v", st)
	}

	// Discovery supplying a server identity lets the same operation finish.
	mu.Lock()
	discover = true
	mu.Unlock()
	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st = lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.cleanupVerified == nil || st.execStopped == nil {
		t.Fatalf("server-ID recovery did not complete: %+v", st)
	}
	if _, stops, deletes, settles := backend.counts(); stops != 1 || deletes != 1 || settles != 1 {
		t.Fatalf("provider calls stop=%d delete=%d settle=%d", stops, deletes, settles)
	}
}

// Deletion is itself a valid way to stop a VM: an ordinary Stop failure or
// a stop acknowledgement followed by persistent ACTIVE reads must not
// strand the resource when Delete and every absence proof succeed.
func TestCILifecycleReconcileDeleteIsValidStop(t *testing.T) {
	ctx := context.Background()
	res := lcResources()

	t.Run("stop_error_delete_succeeds", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		lcTrack(t, s, workspace, runner, lcPlan(s.now()))
		lcAdmit(t, s, workspace, runner, req, proof)
		s.now = func() time.Time { return proof.StartedAt.Add(100 * time.Second) }
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		backend := &lcBackend{
			stopErr: errors.New("provider stop unavailable"),
			observeFn: func(_ []ciOwnedResource, call int) (ciResourceObservation, error) {
				if call <= 2 {
					return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
				}
				return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, call%2 == 0), nil
			},
		}
		worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}
		if err := worker.Reconcile(ctx, workspace, runner); err != nil {
			t.Fatal(err)
		}
		st := lcLifecycleState(t, s, workspace, runner)
		if st.phase != "deleted" || st.cleanupVerified == nil || st.execStopped == nil {
			t.Fatalf("delete-as-stop did not complete: %+v", st)
		}
		if _, stops, deletes, settles := backend.counts(); stops != 1 || deletes != 1 || settles != 1 {
			t.Fatalf("provider calls stop=%d delete=%d settle=%d", stops, deletes, settles)
		}
	})

	t.Run("stop_ack_active_then_delete", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		lcTrack(t, s, workspace, runner, lcPlan(s.now()))
		lcAdmit(t, s, workspace, runner, req, proof)
		s.now = func() time.Time { return proof.StartedAt.Add(100 * time.Second) }
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		backend := &lcBackend{observeFn: func(_ []ciOwnedResource, call int) (ciResourceObservation, error) {
			switch call {
			case 1, 2:
				// Stop acknowledged yet the guest still reports ACTIVE: the
				// acknowledgement alone is never proof and must not block
				// the delete path.
				return lcObservation(res, lcReads(res, 200, "ACTIVE"), lcPortList([]string{lcPort.ID}), true, true, false), nil
			case 3:
				return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, false), nil
			default:
				return lcObservation(nil, lcReads(res, 404, ""), lcPortList([]string{}), true, true, true), nil
			}
		}}
		worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}
		if err := worker.Reconcile(ctx, workspace, runner); err != nil {
			t.Fatal(err)
		}
		st := lcLifecycleState(t, s, workspace, runner)
		if st.phase != "deleted" || st.execStopped == nil {
			t.Fatalf("active-then-delete did not complete: %+v", st)
		}
	})

	t.Run("resources_remain_no_settle", func(t *testing.T) {
		s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
		lcTrack(t, s, workspace, runner, lcPlan(s.now()))
		lcAdmit(t, s, workspace, runner, req, proof)
		s.now = func() time.Time { return proof.StartedAt.Add(100 * time.Second) }
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		backend := &lcBackend{observeFn: func(_ []ciOwnedResource, _ int) (ciResourceObservation, error) {
			return lcObservation(res, lcReads(res, 200, "SHUTOFF"), lcPortList([]string{lcPort.ID}), true, true, true), nil
		}}
		worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}
		if err := worker.Reconcile(ctx, workspace, runner); err != nil {
			t.Fatal(err)
		}
		st := lcLifecycleState(t, s, workspace, runner)
		if st.phase == "deleted" || st.cleanupVerified != nil || st.execStopped != nil {
			t.Fatalf("remaining resources claimed cleanup: %+v", st)
		}
		if _, stops, deletes, settles := backend.counts(); stops != 1 || deletes != 1 || settles != 0 {
			t.Fatalf("provider calls stop=%d delete=%d settle=%d", stops, deletes, settles)
		}
	})
}

// A due deadline must be enforced before any GitHub call: the only source
// request happens inside settlement after physical cleanup has completed.
func TestCILifecycleDeadlinePreemptsGitHub(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)

	deadline := proof.StartedAt.Add(7200 * time.Second)
	s.now = func() time.Time { return deadline }
	backend := lcScriptedBackend(lcHappyObservations())
	var sawCleanup atomic.Bool
	f := lcGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _, deletes, settles := backend.counts()
		if deletes >= 1 && settles >= 1 {
			sawCleanup.Store(true)
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"down"}`)
	})
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend, GitHub: f.client}

	if err := worker.Reconcile(ctx, workspace, runner); !errors.Is(err, errCILifecycleSettlementPending) {
		t.Fatalf("expected settlement pending, got %v", err)
	}
	if !sawCleanup.Load() {
		t.Fatal("GitHub was consulted before physical cleanup completed")
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.cleanupVerified == nil || st.execStopped != nil {
		t.Fatalf("deadline cleanup state: %+v", st)
	}
	units, _, _ := ciFinancialTotals(t, s, workspace)
	if units != 0 {
		t.Fatalf("deferred settlement charged %d units", units)
	}
}

// A source RPC that crosses the deadline boundary mid-flight must not defer
// the stop to another tick: the worker refreshes its clock and enforces the
// original deadline cap in the same reconcile.
func TestCILifecycleSourceRPCCrossesDeadline(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	lcAdmit(t, s, workspace, runner, req, proof)

	deadline := proof.StartedAt.Add(7200 * time.Second)
	base := deadline.Add(-time.Second)
	var offset atomic.Int64
	s.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }

	var advanced atomic.Bool
	f := lcGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if advanced.CompareAndSwap(false, true) {
			offset.Add(int64(10 * time.Second))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/123/access_tokens":
			fmt.Fprint(w, `{"token":"ghs_lifecycle_fixture","expires_at":"2099-01-01T00:00:00Z"}`)
		case r.URL.Path == "/repositories/789":
			fmt.Fprint(w, `{"id":789,"full_name":"financial-tests/repo","private":true}`)
		case r.URL.Path == "/repos/financial-tests/repo/actions/jobs/1001":
			fmt.Fprint(w, ciStopSourceJobBody(proof, proof.StartedAt.Add(time.Hour), func(j map[string]any) {
				j["status"] = "in_progress"
				j["completed_at"] = nil
				j["conclusion"] = nil
			}))
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
	backend := lcScriptedBackend(lcHappyObservations())
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend, GitHub: f.client}

	if err := worker.Reconcile(ctx, workspace, runner); !errors.Is(err, errCILifecycleSettlementPending) {
		t.Fatalf("expected settlement pending, got %v", err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.stopRequested == nil || st.billStop == nil || !st.billStop.Equal(deadline) {
		t.Fatalf("crossed-deadline stop: %+v", st)
	}
	if st.phase != "deleted" || st.execStopped != nil {
		t.Fatalf("same-reconcile cleanup state: %+v", st)
	}
}

// The plan hard deadline binds the billing cap when it precedes the runner
// execution deadline.
func TestCILifecycleHardDeadlineCapsBilling(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	plan := lcPlan(s.now())
	plan.HardDeadlineAt = s.now().Add(5400 * time.Second)
	lcTrack(t, s, workspace, runner, plan)
	lcAdmit(t, s, workspace, runner, req, proof)

	s.now = func() time.Time { return plan.HardDeadlineAt }
	backend := lcScriptedBackend(lcHappyObservations())
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}

	if err := worker.Reconcile(ctx, workspace, runner); !errors.Is(err, errCILifecycleSettlementPending) {
		t.Fatalf("expected settlement pending, got %v", err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.billStop == nil || !st.billStop.Equal(plan.HardDeadlineAt.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("hard cap not binding: %+v want %v", st.billStop, plan.HardDeadlineAt)
	}
	if st.phase != "deleted" || st.execStopped != nil {
		t.Fatalf("hard-cap cleanup state: %+v", st)
	}
}

// Exactly at the free-hour boundary no source proof is needed: settlement
// closes the clock with zero units, cents and reservation.
func TestCILifecycleFreeBoundarySettlesWithoutProof(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	if _, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 3600); err != nil {
		t.Fatal(err)
	}

	deadline := proof.StartedAt.Add(3600 * time.Second)
	s.now = func() time.Time { return deadline }
	backend := lcScriptedBackend(lcHappyObservations())
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: backend}

	if err := worker.Reconcile(ctx, workspace, runner); err != nil {
		t.Fatal(err)
	}
	st := lcLifecycleState(t, s, workspace, runner)
	if st.phase != "deleted" || st.execStopped == nil || !st.execStopped.Equal(deadline) {
		t.Fatalf("free-boundary settlement: %+v", st)
	}
	units, cents, reserved := ciFinancialTotals(t, s, workspace)
	if units != 0 || cents != 0 || reserved != 0 {
		t.Fatalf("free boundary totals units=%d cents=%d reserved=%d", units, cents, reserved)
	}
}

// Admission and stop intent race on the workspace lock: whichever commits
// first wins, the runner always ends delete-requested, and a committed
// allow can never be recovered once stop intent exists.
func TestCILifecycleAdmissionStopRace(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))

	var wg sync.WaitGroup
	var admitReceipt ciAdmissionReceipt
	var admitErr, stopErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		admitReceipt, admitErr = s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
	}()
	go func() {
		defer wg.Done()
		_, stopErr = s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil)
	}()
	wg.Wait()
	_ = admitReceipt

	st := lcLifecycleState(t, s, workspace, runner)
	if !st.deleteRequested {
		t.Fatal("stop intent missing after race")
	}
	if stopErr != nil {
		t.Fatalf("stop intent failed: %v", stopErr)
	}
	// Recovery is denied in every interleaving.
	if receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, nil, 7200); err == nil {
		t.Fatalf("recovery allowed after stop intent: %+v", receipt)
	}

	assigned, started, _, _ := ciAdmissionRunnerJobState(t, s, workspace, runner)
	if started != nil {
		// Admission won the lock: clock, decision and reservation exist and
		// the frozen billing ceiling was applied afterwards.
		if admitErr != nil {
			t.Fatalf("admission reported error after winning: %v", admitErr)
		}
		if !started.Equal(proof.StartedAt) {
			t.Fatalf("execution start = %v want %v", started, proof.StartedAt)
		}
		if st.billStop == nil {
			t.Fatal("frozen ceiling missing after admission win")
		}
		if n := ciAdmissionReservedUnits(t, s, workspace, runner); n <= 0 {
			t.Fatalf("reservation missing after admission win: %d", n)
		}
		if assigned == nil {
			t.Fatal("job assignment missing after admission win")
		}
	} else {
		// Stop won the lock: admission is denied and nothing was reserved.
		if admitErr == nil {
			t.Fatal("admission succeeded after stop intent")
		}
		if n := ciAdmissionReservedUnits(t, s, workspace, runner); n != 0 {
			t.Fatalf("reservation leaked after stop win: %d", n)
		}
		var decision []byte
		if err := s.pool.QueryRow(ctx, `SELECT decision FROM ci_runner_admissions WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner).Scan(&decision); err != nil {
			t.Fatal(err)
		}
		if decision != nil {
			t.Fatal("decision persisted after stop win")
		}
	}
}
