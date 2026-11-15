package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

// registeredLeaseForTest builds a fully registered lease (created VM + JIT).
func registeredLeaseForTest(t *testing.T, e *ciEnforcerEngine, fc *fakeCICompute, gh *ciGitHubClient, spec ciEnforcerSpec, serverID string) int64 {
	t.Helper()
	putAndCreate(t, e, fc, spec, serverID)
	_, runnerID, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
	if err != nil {
		t.Fatalf("registerRunner: %v", err)
	}
	return runnerID
}

func TestCIEnforcerConcurrentApproveFirstWinner(t *testing.T) {
	_, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, dir := newTestEngineAt(t, fc, client, clk)
	spec := enforcerTestSpecAt("op-appr-race", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	registeredLeaseForTest(t, e, fc, client, spec, "srv-appr-race")
	d1 := clk.wall.Add(3600 * time.Second).UnixMilli()
	d2 := clk.wall.Add(7200 * time.Second).UnixMilli()
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, d := range []int64{d1, d2} {
		wg.Add(1)
		go func(i int, d int64) {
			defer wg.Done()
			_, err := e.approve(leaseID, d)
			results[i] = err
		}(i, d)
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, errCIEnforcerConflict) {
			t.Fatalf("unexpected approve error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one approve must win, got %d", wins)
	}
	l := leaseState(t, e, leaseID)
	winner := l.DeadlineUnixMs
	if winner != d1 && winner != d2 {
		t.Fatalf("persisted winner deadline invalid: %d", winner)
	}
	// Exact retry after wall rollback + mono advance: same cutoff, no rewrite.
	before, _ := os.ReadFile(filepath.Join(dir, "lease-"+leaseID+".json"))
	beforeMono := leaseState(t, e, leaseID).cutoffMono
	clk.rollbackWall(30 * time.Second)
	clk.advanceMono(time.Second)
	if _, err := e.approve(leaseID, winner); err != nil {
		t.Fatalf("exact retry after rollback must succeed: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "lease-"+leaseID+".json"))
	if string(before) != string(after) {
		t.Fatalf("exact retry must not rewrite the journal")
	}
	if !leaseState(t, e, leaseID).cutoffMono.Equal(beforeMono) {
		t.Fatal("exact retry must not reset the in-memory monotonic cutoff")
	}
	// Stop vs approve: final stop wins, later approves conflict.
	if _, err := e.stop(leaseID); err != nil {
		t.Fatal(err)
	}
	l = leaseState(t, e, leaseID)
	if !l.StopRequested || l.ApprovedDeadlineUnixMs != 0 {
		t.Fatalf("stop must clear approval: %+v", l)
	}
	if _, err := e.approve(leaseID, winner); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("approve after stop must conflict")
	}
}

func TestCIEnforcerConcurrentPutAndCloseFirstWinner(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-put-race", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	// Concurrent identical PUT: both may succeed idempotently.
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.putLease(leaseID, spec)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("identical PUT must be idempotent: %v", err)
		}
	}
	// Same op, different lease: exactly one allocation wins.
	alt := spec
	altLease, _ := ciEnforcerNewLeaseID()
	alt.Plan.EnforcerLease = altLease
	var wg2 sync.WaitGroup
	res := make([]error, 2)
	for i, sp := range []ciEnforcerSpec{spec, alt} {
		wg2.Add(1)
		go func(i int, sp ciEnforcerSpec) {
			defer wg2.Done()
			_, res[i] = e.putLease(sp.Plan.EnforcerLease, sp)
		}(i, sp)
	}
	wg2.Wait()
	// One is the existing lease (idempotent), the other must conflict.
	conflicts := 0
	for _, err := range res {
		if errors.Is(err, errCIEnforcerConflict) {
			conflicts++
		} else if err != nil {
			t.Fatalf("unexpected put error: %v", err)
		}
	}
	if conflicts != 1 {
		t.Fatalf("second lease for same op must conflict exactly once, got %d", conflicts)
	}
	// Concurrent close with different server IDs: one wins.
	spec2 := enforcerTestSpecAt("op-close-race", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err := e.putLease(spec2.Plan.EnforcerLease, spec2); err != nil {
		t.Fatal(err)
	}
	scope2 := ciEnforcerScope(spec2)
	fc.addServer(scope2, "srv-close-a", "ACTIVE")
	fc.addServer(scope2, "srv-close-b", "ACTIVE")
	var wg3 sync.WaitGroup
	cerr := make([]error, 2)
	for i, id := range []string{"srv-close-a", "srv-close-b"} {
		wg3.Add(1)
		go func(i int, id string) {
			defer wg3.Done()
			_, cerr[i] = e.closeCreation(context.Background(), spec2.Plan.EnforcerLease, "created", id)
		}(i, id)
	}
	wg3.Wait()
	wins := 0
	conflicts = 0
	for _, err := range cerr {
		if err == nil {
			wins++
		} else if errors.Is(err, errCIEnforcerConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected close error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("close race: want 1 win 1 conflict, got %d/%d", wins, conflicts)
	}
	l2 := leaseState(t, e, spec2.Plan.EnforcerLease)
	if !l2.CreateSettled || (l2.CreateServerID != "srv-close-a" && l2.CreateServerID != "srv-close-b") {
		t.Fatalf("first close result must be pinned: %+v", l2)
	}
}

func TestCIEnforcerConcurrentRegisterClaims(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-reg-race", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-reg-race")
	var wg sync.WaitGroup
	jitCalls := int32(0)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, jit, _ := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
			if jit != "" {
				atomic.AddInt32(&jitCalls, 1)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&gh.jitCalls) != 1 {
		t.Fatalf("concurrent register must POST exactly once, got %d", gh.jitCalls)
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if !l.RegistrationSettled || l.RegistrationConflict {
		t.Fatalf("successful registration must not latch own conflict: %+v", l)
	}
}

func TestCIEnforcerStopDuringPreflightForbidsPOST(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-pre-stop", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-pre-stop")
	// Block inside preflight (runner list) — token endpoint already done.
	gh.mu.Lock()
	gh.listBlock = make(chan struct{})
	gh.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		listing := atomic.LoadInt32(&gh.listCalls)
		if listing > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("preflight list never reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.stop(spec.Plan.EnforcerLease)
	gh.mu.Lock()
	close(gh.listBlock)
	gh.mu.Unlock()
	if err := <-done; err == nil {
		t.Fatalf("stop during preflight must forbid the POST")
	}
	if atomic.LoadInt32(&gh.jitCalls) != 0 {
		t.Fatalf("no JIT POST may follow a preflight stop")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.RegistrationAttempted {
		t.Fatalf("attempted must not persist when stop forbids POST")
	}
}

// blockingGitHub wraps the fixture handler and blocks runner DELETE calls.
type blockingGitHub struct {
	inner   http.Handler
	blocked chan struct{} // closed to release all
	entered chan struct{} // signaled per blocked call
	count   int32
}

func (b *blockingGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/actions/runners/") {
		atomic.AddInt32(&b.count, 1)
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-b.blocked:
		case <-r.Context().Done():
			return
		}
	}
	b.inner.ServeHTTP(w, r)
}

func TestCIEnforcerRunStarvationThirdLeaseDeadline(t *testing.T) {
	_, ghSrv, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, client, clk)
	// Two stopped registered leases whose GitHub DELETEs block. Lease IDs are
	// pinned so the round-robin order is deterministic: sorted 1..,2..,3..
	// rotates C to the head once the cursor passes B.
	var ids []string
	for i, op := range []string{"op-run-a", "op-run-b"} {
		spec := enforcerTestSpecAt(op, clk.wall, 800*time.Second, 20000*time.Second)
		spec.Plan.EnforcerLease = strings.Repeat(fmt.Sprintf("%x", i+1), 64)
		registeredLeaseForTest(t, e, fc, client, spec, fmt.Sprintf("srv-run-%d", i))
		e.stop(spec.Plan.EnforcerLease)
		ids = append(ids, spec.Plan.EnforcerLease)
	}
	// Wrap the GitHub fixture HTTP handler to block runner DELETEs.
	bg := &blockingGitHub{inner: ghSrv.Config.Handler, blocked: make(chan struct{}), entered: make(chan struct{}, 8)}
	ghSrv.Config.Handler = bg
	defer close(bg.blocked)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { e.Run(ctx); close(runDone) }()
	// Wait until BOTH GitHub DELETE handlers are blocked.
	for i := 0; i < 2; i++ {
		select {
		case <-bg.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("blocked GitHub DELETE %d never reached", i+1)
		}
	}
	// Give the old blocking scheduler a chance to stall (>1 tick).
	time.Sleep(1500 * time.Millisecond)
	// Add new demand only after the scheduler's GitHub slots are blocked.
	spec3 := enforcerTestSpecAt("op-run-c", clk.wall, 300*time.Second, 20000*time.Second)
	spec3.Plan.EnforcerLease = strings.Repeat("3", 64)
	fc.addServer(ciEnforcerScope(spec3), "srv-run-c", "ACTIVE")
	if _, err := e.putLease(spec3.Plan.EnforcerLease, spec3); err != nil {
		t.Fatal(err)
	}
	clk.advance(400 * time.Second)
	deadline := time.Now().Add(4 * time.Second)
	for {
		fc.mu.Lock()
		_, thirdExists := fc.servers["srv-run-c"]
		fc.mu.Unlock()
		if !thirdExists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("third VM must be deleted while GitHub calls remain blocked")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-runDone
}

func TestCIEnforcerCloseJoinsWorkAndFlock(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, dir := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-close-join", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-close-join")
	gh.mu.Lock()
	gh.jitBlock = make(chan struct{})
	gh.mu.Unlock()
	regDone := make(chan error, 1)
	go func() {
		_, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
		regDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&gh.jitCalls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("JIT POST never reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// close() must cancel the in-flight JIT work and join before returning.
	// Release the fixture block during cleanup regardless of outcome.
	defer func() {
		gh.mu.Lock()
		select {
		case <-gh.jitBlock:
		default:
			close(gh.jitBlock)
		}
		gh.mu.Unlock()
	}()
	closeResult := make(chan struct{})
	go func() { e.close(); close(closeResult) }()
	select {
	case <-closeResult:
	case <-time.After(10 * time.Second):
		t.Fatal("close must join in-flight work")
	}
	select {
	case err := <-regDone:
		t.Logf("registerRunner returned: %v", err)
		if err == nil {
			t.Fatal("cancelled registration must not succeed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registerRunner must return after close")
	}
	// Flock released: a second opener succeeds.
	e2, err := newCIEnforcerEngine(dir, fc, client, nil, nil)
	if err != nil {
		t.Fatalf("second opener must succeed after close: %v", err)
	}
	e2.close()
}

// delayObserve wraps compute to stall one armed ObserveCIRunner call: the
// block happens before delegating, so the observation reflects provider
// state at release time while `retained` was fixed at call time.
type delayObserve struct {
	inner   ciEnforcerCompute
	gate    chan struct{}
	armed   int32
	entered chan struct{}
}

func (d *delayObserve) ObserveCIRunner(ctx context.Context, scope compute.CIRunnerScope, retained []compute.CIRunnerResource) (compute.CIRunnerObservation, error) {
	if atomic.CompareAndSwapInt32(&d.armed, 1, 0) {
		close(d.entered)
		select {
		case <-d.gate:
		case <-ctx.Done():
			return compute.CIRunnerObservation{}, ctx.Err()
		}
	}
	return d.inner.ObserveCIRunner(ctx, scope, retained)
}
func (d *delayObserve) StopCIRunner(ctx context.Context, scope compute.CIRunnerScope, r []compute.CIRunnerResource) error {
	return d.inner.StopCIRunner(ctx, scope, r)
}
func (d *delayObserve) DeleteCIRunner(ctx context.Context, scope compute.CIRunnerScope, r []compute.CIRunnerResource) error {
	return d.inner.DeleteCIRunner(ctx, scope, r)
}
func (d *delayObserve) Server(ctx context.Context, id string) (compute.Server, error) {
	return d.inner.Server(ctx, id)
}
func (d *delayObserve) ValidateCIRunnerScope(scope compute.CIRunnerScope) error {
	return d.inner.ValidateCIRunnerScope(scope)
}

func TestCIEnforcerStaleInventoryCannotTombstone(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	dc := &delayObserve{inner: fc, gate: make(chan struct{}), entered: make(chan struct{})}
	e, _ := newTestEngineAt(t, dc, nil, clk)
	spec := enforcerTestSpecAt("op-stale", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	// Pass 0: lease expires with provider server A present but creation
	// never closed. The pass discovers A, deletes it, and merges A into the
	// durable retained inventory without tombstoning (creation open).
	fc.addServer(scope, "srv-stale-a", "ACTIVE")
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	clk.advance(400 * time.Second)
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	l0 := leaseState(t, e, spec.Plan.EnforcerLease)
	foundA := false
	for _, r := range l0.Resources {
		if r.Kind == "server" && r.ID == "srv-stale-a" {
			foundA = true
		}
	}
	if !foundA || l0.Settled || l0.CreateSettled {
		t.Fatalf("pass 0 must retain discovered A without settling: %+v", l0)
	}
	// Second owned server B appears; the first closeCreation will retain it.
	fc.addServer(scope, "srv-stale-b", "ACTIVE")
	// Arm the wrapper: the next physical pass blocks inside Observe AFTER
	// its retained snapshot ([A]) was taken but before provider reads.
	atomic.StoreInt32(&dc.armed, 1)
	done := make(chan struct{})
	go func() { e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease); close(done) }()
	select {
	case <-dc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observe never entered")
	}
	// Concurrent first close retains B while the observe is blocked.
	if _, err := e.closeCreation(context.Background(), spec.Plan.EnforcerLease, "created", "srv-stale-b"); err != nil {
		t.Fatalf("close B: %v", err)
	}
	// B disappears before the stale observation is released.
	fc.removeServer("srv-stale-b")
	close(dc.gate)
	<-done
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	// B was committed to the journal mid-pass and must survive the merge.
	foundB := false
	for _, r := range l.Resources {
		if r.Kind == "server" && r.ID == "srv-stale-b" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("concurrently retained B must stay in journal: %+v", l.Resources)
	}
	// The stale pass never read B directly: no absence proof, no tombstone.
	if l.PhysicalAbsent || l.Settled {
		t.Fatalf("stale inventory must not prove absence: %+v", l)
	}
	// Next pass reads B's fresh 404 with complete ports and may settle.
	driveCleanup(t, e, spec.Plan.EnforcerLease, 3)
	l = leaseState(t, e, spec.Plan.EnforcerLease)
	if !l.Settled {
		t.Fatalf("fresh pass must settle after real absence proof: %+v", l)
	}
}

func TestCIEnforcerTombstoneAdapterThroughBackendEvidence(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-tomb-adapter", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, "srv-tomb-1", "ACTIVE")
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := e.closeCreation(context.Background(), spec.Plan.EnforcerLease, "created", "srv-tomb-1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(400 * time.Second)
	driveCleanup(t, e, spec.Plan.EnforcerLease, 3)
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if !l.Settled {
		t.Fatalf("lease must tombstone: %+v", l)
	}
	// Real HTTP client + backend: CP retained has the server ID (404 now),
	// daemon tombstone reports empty inventory with proofs true.
	token := []byte(strings.Repeat("x", 40))
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	client, err := newCIEnforcerClient(httpSrv.URL, token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	backend := &ciLifecycleProviderBackend{Compute: fc, Enforcer: client}
	plan := spec.Plan
	fc.removeServer("srv-tomb-1")
	obs, err := backend.Observe(context.Background(), plan,
		[]ciOwnedResource{{Kind: "server", ID: "srv-tomb-1", OperationID: plan.OperationID}})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.CreateSettled || !obs.EnforcerSettled {
		t.Fatalf("tombstone status must report both proofs true: %+v", obs)
	}
	_, stopped, absent, err := ciResourceEvidence(plan.OperationID,
		[]ciOwnedResource{{Kind: "server", ID: "srv-tomb-1", OperationID: plan.OperationID}}, obs)
	if err != nil || !stopped || !absent {
		t.Fatalf("root evidence must prove stopped+absent: stopped=%v absent=%v err=%v", stopped, absent, err)
	}
}

func TestCIEnforcerRestartAttemptedNoGitHubClient(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, dir := newTestEngineAt(t, fc, client, clk)
	spec := enforcerTestSpecAt("op-nogh-restart", clk.wall, 300*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-nogh-1")
	gh.mu.Lock()
	gh.jitErr = http.StatusInternalServerError
	gh.mu.Unlock()
	if _, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease); err == nil {
		t.Fatal("ambiguous JIT must error")
	}
	// Restart with attempted registration but NO GitHub client: physical
	// cleanup works, GitHub pass does not panic and stays unsettled.
	e.close()
	clk.advance(400 * time.Second)
	wall, mono := clk.pair()
	e2, err := newCIEnforcerEngine(dir, fc, nil, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	driveCleanup(t, e2, spec.Plan.EnforcerLease, 3)
	l := leaseState(t, e2, spec.Plan.EnforcerLease)
	if l.Settled {
		t.Fatalf("attempted registration without GitHub proof must never settle")
	}
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("physical cleanup must work without GitHub client")
	}
}

func TestCIEnforcerJITResponseNegatives(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong-owner-label", func(r map[string]any) {
			r["labels"] = []map[string]any{{"id": 1, "name": "self-hosted", "type": "system"}}
		}},
		{"missing-labels", func(r map[string]any) { delete(r, "labels") }},
		{"wrong-name", func(r map[string]any) { r["name"] = "foreign-runner" }},
		{"bad-id", func(r map[string]any) { r["id"] = -5 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh, _, client := newGitHubFixture(t)
			fc := newFakeCICompute()
			e, _ := newTestEngine(t, fc, client)
			spec := enforcerTestSpec("op-jit-neg-"+tc.name, 800*time.Second, 20000*time.Second)
			putAndCreate(t, e, fc, spec, "srv-jit-neg")
			// Wrap generate-jitconfig to mutate the runner in the response.
			gh.mu.Lock()
			gh.jitMutate = tc.mutate
			gh.mu.Unlock()
			_, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
			if err == nil {
				t.Fatalf("%s must fail", tc.name)
			}
			l := leaseState(t, e, spec.Plan.EnforcerLease)
			if l.RegistrationSettled {
				t.Fatalf("%s must stay unsettled", tc.name)
			}
			if _, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease); err == nil ||
				atomic.LoadInt32(&gh.jitCalls) != 1 {
				t.Fatal("a malformed JIT response must never trigger another POST")
			}
		})
	}
	// Oversized encoded config (>64KiB) rejected before decode.
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-jit-oversize", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-jit-big")
	gh.mu.Lock()
	gh.jitEncoded = strings.Repeat("A", 65537)
	gh.mu.Unlock()
	_, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
	if err == nil {
		t.Fatal("oversized encoded config must fail")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.RegistrationSettled || atomic.LoadInt32(&gh.jitCalls) != 1 {
		t.Fatalf("oversize must be single ambiguous attempt: %+v", l)
	}
}

func TestCIEnforcerPartialRunnerIdentityUnresolved(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	e, _ := newTestEngine(t, newFakeCICompute(), client)
	spec := enforcerTestSpec("op-partial", 800*time.Second, 20000*time.Second)
	label := ciRunnerOwnerLabel(spec.Plan.OperationID)
	partial := ghRunnerJSON(9002, "different-name", []string{label})
	partial["labels"] = []map[string]any{{"name": label, "type": "read-only"}}
	gh.mu.Lock()
	gh.runners[9001] = ghRunnerJSON(9001, ciEnforcerRunnerName(spec.Plan.OperationID), []string{label})
	gh.runners[9002] = partial
	gh.mu.Unlock()
	if id, err := e.githubRecoverRunnerID(context.Background(), spec); err != nil || id != 0 {
		t.Fatalf("a partial second identity must prevent recovery: id=%d err=%v", id, err)
	}
	gh.mu.Lock()
	delete(gh.runners, 9001)
	gh.mu.Unlock()
	if absent, err := e.githubRunnerAbsent(context.Background(), "runner-token", "octo", "repo",
		9001, ciEnforcerRunnerName(spec.Plan.OperationID), spec.Plan.OperationID); err != nil || absent {
		t.Fatalf("a read-only owner-name match must block absence: absent=%v err=%v", absent, err)
	}
	for _, bad := range []map[string]any{
		{"id": 9002, "name": "different-name", "labels": nil},
		{"id": 9002, "labels": []any{}},
	} {
		gh.mu.Lock()
		gh.runners = map[int64]map[string]any{9002: bad}
		gh.mu.Unlock()
		if _, err := client.ciListRepoRunners(context.Background(), "runner-token", "octo", "repo"); err == nil {
			t.Fatal("a malformed runner record must not establish a complete listing")
		}
	}
}

type wrongRegionCompute struct{ *fakeCICompute }

func (w wrongRegionCompute) ValidateCIRunnerScope(scope compute.CIRunnerScope) error {
	return compute.ErrCIRunnerOwnership
}

func TestCIEnforcerWrongRegionRejected(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	// PUT must reject a spec whose region the provider cannot serve, before
	// any lease file is written.
	e, dir := newTestEngineAt(t, wrongRegionCompute{fc}, nil, clk)
	spec := enforcerTestSpecAt("op-wrong-region", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("wrong-region PUT must be rejected as conflict: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lease-"+spec.Plan.EnforcerLease+".json")); !os.IsNotExist(err) {
		t.Fatalf("rejected PUT must not write a lease file")
	}
	// Restart with a misconfigured region refuses to open the engine.
	e2, dir2 := newTestEngineAt(t, fc, nil, clk)
	spec2 := enforcerTestSpecAt("op-region-restart", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err := e2.putLease(spec2.Plan.EnforcerLease, spec2); err != nil {
		t.Fatal(err)
	}
	e2.close()
	wall, mono := clk.pair()
	if _, err := newCIEnforcerEngine(dir2, wrongRegionCompute{fc}, nil, wall, mono); err == nil {
		t.Fatal("restart with wrong configured region must refuse")
	}
	_ = dir
}

func TestCIEnforcerAbsenceProofNegatives(t *testing.T) {
	// Missing direct read for a retained ID must not prove absence.
	fc := newFakeCICompute()
	spec := enforcerTestSpec("op-absent-neg", 800*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	expected := []compute.CIRunnerResource{{Kind: "server", ID: "srv-neg-1", OperationID: spec.Plan.OperationID}}
	obs := compute.CIRunnerObservation{
		DiscoveryComplete: true,
		Reads:             []compute.CIRunnerRead{}, // missing the retained read
		ServerPorts:       []compute.CIRunnerServerPorts{{ServerID: "srv-neg-1", Complete: true, PortIDs: []string{}}},
	}
	if ciEnforcerAbsenceProved(spec.Plan.OperationID, expected, obs) {
		t.Fatal("missing retained read must not prove absence")
	}
	// Foreign 404 (wrong operation) must not prove absence.
	obs.Reads = []compute.CIRunnerRead{{
		Resource:   compute.CIRunnerResource{Kind: "server", ID: "srv-neg-1", OperationID: "op-foreign"},
		HTTPStatus: 404,
	}}
	if ciEnforcerAbsenceProved(spec.Plan.OperationID, expected, obs) {
		t.Fatal("foreign-operation read must not prove absence")
	}
	// Missing port list must not prove absence.
	obs.Reads = []compute.CIRunnerRead{{
		Resource:   compute.CIRunnerResource{Kind: "server", ID: "srv-neg-1", OperationID: spec.Plan.OperationID},
		HTTPStatus: 404,
	}}
	obs.ServerPorts = nil
	if ciEnforcerAbsenceProved(spec.Plan.OperationID, expected, obs) {
		t.Fatal("missing port list must not prove absence")
	}
	// Non-empty discovered must not prove absence.
	obs.ServerPorts = []compute.CIRunnerServerPorts{{ServerID: "srv-neg-1", Complete: true, PortIDs: []string{}}}
	obs.Discovered = []compute.CIRunnerResource{{Kind: "server", ID: "srv-neg-2", OperationID: spec.Plan.OperationID}}
	if ciEnforcerAbsenceProved(spec.Plan.OperationID, expected, obs) {
		t.Fatal("non-empty discovery must not prove absence")
	}
	_ = fc
	_ = scope
}

func TestCIEnforcerRequestRecursiveDuplicateRejected(t *testing.T) {
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, nil)
	token := []byte(strings.Repeat("x", 40))
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	leaseID := strings.Repeat("a", 64)
	body := `{"plan":{"operationId":"op","operationId":"op2"},"githubInstallationId":1,"githubRepositoryId":2,"sizeId":"ci-small"}`
	req, _ := http.NewRequest(http.MethodPut, httpSrv.URL+"/v1/leases/"+leaseID, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("nested duplicate plan key must be rejected, got %d", resp.StatusCode)
	}
	// Null nested field must also be rejected.
	body = `{"plan":{"operationId":null},"githubInstallationId":1,"githubRepositoryId":2,"sizeId":"ci-small"}`
	req, _ = http.NewRequest(http.MethodPut, httpSrv.URL+"/v1/leases/"+leaseID, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("nested null must be rejected, got %d", resp2.StatusCode)
	}
}

func TestCIEnforcerRoundRobin(t *testing.T) {
	in := []string{"c", "a", "b"}
	cases := []struct {
		name  string
		ids   []string
		after string
		want  []string
	}{
		{"empty_cursor_sorts", in, "", []string{"a", "b", "c"}},
		{"after_b_rotates", in, "b", []string{"c", "a", "b"}},
		{"after_c_wraps", in, "c", []string{"a", "b", "c"}},
		{"missing_cursor", []string{"a", "c"}, "b", []string{"c", "a"}},
		{"insert_before_cursor", []string{"a", "b", "c", "d"}, "b", []string{"c", "d", "a", "b"}},
		{"insert_after_cursor", []string{"a", "b", "c"}, "a", []string{"b", "c", "a"}},
		{"nil_input", nil, "", nil},
		{"empty_input", []string{}, "x", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ciEnforcerRoundRobin(tc.ids, tc.after); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("roundRobin(%v, %q) = %v, want %v", tc.ids, tc.after, got, tc.want)
			}
		})
	}
	// The caller's slice is never mutated or reordered in place.
	orig := []string{"c", "a", "b"}
	snapshot := append([]string(nil), orig...)
	ciEnforcerRoundRobin(orig, "b")
	if !reflect.DeepEqual(orig, snapshot) {
		t.Fatalf("input slice mutated: %v", orig)
	}
}

// ciBlockingObserveCompute wraps the fake provider and blocks
// ObserveCIRunner for the listed operation IDs until the gate opens or the
// call context ends. Each blocked operation signals the entered channel
// once, so repeat observes never pile up on the buffer.
type ciBlockingObserveCompute struct {
	ciEnforcerCompute
	mu       sync.Mutex
	block    map[string]bool
	gate     chan struct{}
	entered  chan string
	signaled map[string]bool
}

func (b *ciBlockingObserveCompute) ObserveCIRunner(ctx context.Context, scope compute.CIRunnerScope, retained []compute.CIRunnerResource) (compute.CIRunnerObservation, error) {
	b.mu.Lock()
	blocked := b.block[scope.OperationID]
	first := !b.signaled[scope.OperationID]
	if first {
		b.signaled[scope.OperationID] = true
	}
	b.mu.Unlock()
	if blocked {
		if first {
			select {
			case b.entered <- scope.OperationID:
			default:
			}
		}
		select {
		case <-b.gate:
		case <-ctx.Done():
			return compute.CIRunnerObservation{}, ctx.Err()
		}
	}
	return b.ciEnforcerCompute.ObserveCIRunner(ctx, scope, retained)
}

// TestCIEnforcerGitHubDispatchWithFullPhysicalPool proves the GitHub phase is
// independent of the physical pool: with both physical slots held by blocked
// provider observes, a stopped lease that already holds physical absence
// still gets its GitHub registration cleanup dispatched.
func TestCIEnforcerGitHubDispatchWithFullPhysicalPool(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	opA, opB, opC := "op-ghfull-a", "op-ghfull-b", "op-ghfull-c"
	specA := enforcerTestSpecAt(opA, clk.wall, 800*time.Second, 20000*time.Second)
	specA.Plan.EnforcerLease = strings.Repeat("1", 64)
	specB := enforcerTestSpecAt(opB, clk.wall, 800*time.Second, 20000*time.Second)
	specB.Plan.EnforcerLease = strings.Repeat("2", 64)
	specC := enforcerTestSpecAt(opC, clk.wall, 800*time.Second, 20000*time.Second)
	specC.Plan.EnforcerLease = strings.Repeat("3", 64)

	bc := &ciBlockingObserveCompute{
		ciEnforcerCompute: fc,
		block:             map[string]bool{opA: true, opB: true},
		gate:              make(chan struct{}),
		entered:           make(chan string, 2),
		signaled:          map[string]bool{},
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wall, mono := clk.pair()
	e, err := newCIEnforcerEngine(dir, bc, client, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()

	// A/B: created VMs, stopped, never registered. C: fully registered,
	// then stopped, with physical absence proven directly before Run.
	putAndCreate(t, e, fc, specA, "srv-ghfull-a")
	putAndCreate(t, e, fc, specB, "srv-ghfull-b")
	runnerIDC := registeredLeaseForTest(t, e, fc, client, specC, "srv-ghfull-c")
	for _, spec := range []ciEnforcerSpec{specA, specB, specC} {
		if _, err := e.stop(spec.Plan.EnforcerLease); err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	e.reconcilePhysical(context.Background(), specC.Plan.EnforcerLease)
	lc := leaseState(t, e, specC.Plan.EnforcerLease)
	if !lc.PhysicalAbsent || lc.RegAbsent || lc.Settled || lc.GitHubRunnerID != runnerIDC {
		t.Fatalf("C must hold physical absence with registration still present: %+v", lc)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { e.Run(ctx); close(runDone) }()
	defer func() {
		cancel()
		close(bc.gate)
		<-runDone
	}()

	// Both A and B provider observes must be inside the blocked gate, which
	// holds both physical pool slots for the whole assertion window.
	for i := 0; i < 2; i++ {
		select {
		case <-bc.entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("blocked provider observe %d never reached", i+1)
		}
	}
	// C's GitHub registration absence must be proven while the physical pool
	// is still full; no tombstone is required because physical passes for C
	// remain starved by design.
	deadline := time.Now().Add(3 * time.Second)
	for {
		lc = leaseState(t, e, specC.Plan.EnforcerLease)
		if lc.RegAbsent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("C registration absence must be proven while physical pool is full: %+v", lc)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The GitHub runner is durably deleted and the lease's runner identity
	// cleared by the cleanup commit.
	gh.mu.Lock()
	_, runnerPresent := gh.runners[runnerIDC]
	gh.mu.Unlock()
	if runnerPresent {
		t.Fatalf("C GitHub runner must be deleted while physical pool is full")
	}
	if lc.GitHubRunnerID != 0 {
		t.Fatalf("C runner identity must be cleared after absence proof: %+v", lc)
	}
	// A/B VMs remain present with no false absence proofs while blocked.
	for _, id := range []string{"srv-ghfull-a", "srv-ghfull-b"} {
		fc.mu.Lock()
		_, present := fc.servers[id]
		fc.mu.Unlock()
		if !present {
			t.Fatalf("%s must remain until the observe gate opens", id)
		}
	}
	for _, spec := range []ciEnforcerSpec{specA, specB} {
		l := leaseState(t, e, spec.Plan.EnforcerLease)
		if l.PhysicalAbsent || l.Settled {
			t.Fatalf("blocked lease must not gain false proofs: %+v", l)
		}
	}
}
