package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

// fakeCISchedulerCompute is the per-create provider fixture for scheduler
// tests: resource IDs derive locally from each call's scope operation, so
// concurrent creates never share mutable fixture fields.
type fakeCISchedulerCompute struct {
	*fakeCICompute
	mu      sync.Mutex
	creates int
	block   chan struct{}
}

func newFakeCISchedulerCompute() *fakeCISchedulerCompute {
	return &fakeCISchedulerCompute{fakeCICompute: newFakeCICompute()}
}

func (f *fakeCISchedulerCompute) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// DeleteCIRunner mirrors the real provider adapter: every acknowledged kind
// is removed, not only servers.
func (f *fakeCISchedulerCompute) DeleteCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error {
	err := f.fakeCICompute.DeleteCIRunner(ctx, scope, resources)
	f.fakeCICompute.mu.Lock()
	defer f.fakeCICompute.mu.Unlock()
	for _, r := range resources {
		if r.Kind == "security_group" || r.Kind == "port" {
			key := r.Kind + "/" + r.ID
			delete(f.resources, key)
			f.reads[key] = 404
		}
	}
	return err
}

func (f *fakeCISchedulerCompute) CreateCIRunner(ctx context.Context, in compute.CIRunnerCreateRequest) (compute.CIRunnerCreateResult, error) {
	f.mu.Lock()
	f.creates++
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return compute.CIRunnerCreateResult{CreationSettled: true}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return compute.CIRunnerCreateResult{CreationSettled: true}, err
	}
	op := in.Scope.OperationID
	stages := []struct {
		stage compute.CIRunnerCreateStage
		kind  string
		id    string
	}{
		{compute.CIRunnerCreateSecurityGroup, "security_group", "sg_" + op},
		{compute.CIRunnerCreatePort, "port", "port_" + op},
		{compute.CIRunnerCreateServer, "server", "srv_" + op},
	}
	result := compute.CIRunnerCreateResult{CreationSettled: true}
	for _, st := range stages {
		snapshot := append([]compute.CIRunnerResource(nil), result.Resources...)
		if err := in.BeforeMutation(ctx, st.stage, snapshot); err != nil {
			return result, fmt.Errorf("compute CI runner create %s admission fence failed", st.stage)
		}
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("compute CI runner create %s admission fence failed: %w", st.stage, err)
		}
		result.MutationAttempted = true
		result.CreationSettled = false
		if st.kind == "server" {
			f.addServer(in.Scope, st.id, "ACTIVE")
		} else {
			f.fakeCICompute.mu.Lock()
			key := st.kind + "/" + st.id
			f.resources[key] = compute.CIRunnerResource{Kind: st.kind, ID: st.id, OperationID: op}
			f.reads[key] = 200
			f.fakeCICompute.mu.Unlock()
		}
		result.Resources = append(result.Resources, compute.CIRunnerResource{
			Kind: st.kind, ID: st.id, OperationID: op,
		})
	}
	result.CreationSettled = true
	return result, nil
}

type ciSchedulerRuntime struct {
	compute  *fakeCISchedulerCompute
	engine   *ciEnforcerEngine
	client   *ciEnforcerClient
	github   *ciGitHubClient
	gh       *gitHubFixture
	prov     *ciManagedProvisioner
	sched    *ciManagedScheduler
	stopOnce sync.Once
	cancel   context.CancelFunc
	done     chan error
}

// attachCISchedulerRuntime wires the real enforcer engine over HTTP, the
// provisioner, and the disabled-by-default scheduler onto an existing
// funded store fixture. The profile must digest-match the fixture policy.
func attachCISchedulerRuntime(t *testing.T, s *Store, workspace string, profile ciManagedRunnerProfile) *ciSchedulerRuntime {
	t.Helper()
	rt := &ciSchedulerRuntime{}
	// The store clock and the enforcer clock both advance in real time from
	// the fixture base: lifecycle retries schedule next_attempt_at against
	// the store clock, and the engine validates plan deadlines against its
	// own wall clock. A frozen clock would strand a pending settlement
	// proof forever, or reject a plan minted microseconds after the base.
	base := s.now()
	started := time.Now()
	now := func() time.Time { return base.Add(time.Since(started)) }
	s.now = now
	rt.compute = newFakeCISchedulerCompute()
	rt.gh, _, rt.github = newGitHubFixtureRepo(t, 123, 789, "financial-tests", "repo")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	rt.engine, err = newCIEnforcerEngine(dir, rt.compute, rt.github,
		func() time.Time { return now().UTC() }, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.engine.close)
	token := []byte(strings.Repeat("t", 40))
	srv := &CIEnforcerServer{engine: rt.engine, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	rt.client, err = newCIEnforcerClient(httpSrv.URL, token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	rt.prov, err = newCIManagedProvisioner(s, profile, []byte(strings.Repeat("k", 32)), rt.client, rt.compute)
	if err != nil {
		t.Fatal(err)
	}
	rt.sched, err = newCIManagedScheduler(rt.prov, rt.github)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

type ciSchedulerFixture struct {
	t         *testing.T
	s         *Store
	workspace string
	policy    ciProvisioningBudgetPolicy
	credit    int64
	rt        *ciSchedulerRuntime
}

func newCISchedulerFixture(t *testing.T, credit int64, adjust func(*ciProvisioningBudgetPolicy), jobs int) *ciSchedulerFixture {
	t.Helper()
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	profile := ciManagedClaimProfile()
	digest, err := profile.digest()
	if err != nil {
		t.Fatal(err)
	}
	policy := ciBudgetPolicy(s.now())
	policy.ProfileDigest = digest
	if adjust != nil {
		adjust(&policy)
	}
	ciBudgetGrant(t, s, "credit_sched", credit, policy)
	for n := 1; n <= jobs; n++ {
		ciManagedQueuedJob(t, s, workspace, "linux-x64-2", n)
	}
	return &ciSchedulerFixture{
		t: t, s: s, workspace: workspace, policy: policy, credit: credit,
		rt: attachCISchedulerRuntime(t, s, workspace, profile),
	}
}

func ciSchedulerCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ciSchedulerBudgetState(t *testing.T, s *Store) (active int, committed int64) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL),
 (SELECT committed_micros FROM ci_provisioning_budget WHERE singleton)`).Scan(&active, &committed)
	if err != nil {
		t.Fatal(err)
	}
	return active, committed
}

func ciSchedulerWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// start enables the scheduler, launches Run, and registers a cleanup that
// always cancels and joins it, so a failed wait can never leave workers or
// the lifecycle goroutine running into fixture teardown.
func (rt *ciSchedulerRuntime) start(t *testing.T) {
	t.Helper()
	rt.sched.enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	rt.cancel, rt.done = cancel, done
	go func() { done <- rt.sched.Run(ctx) }()
	t.Cleanup(func() { rt.stop(t) })
}

// stop cancels Run and joins it exactly once; the cleanup registered by
// start makes a second call a no-op.
func (rt *ciSchedulerRuntime) stop(t *testing.T) {
	t.Helper()
	rt.stopOnce.Do(func() {
		rt.cancel()
		select {
		case err := <-rt.done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("scheduler run exit: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("scheduler run did not join")
		}
	})
}

func TestCIManagedSchedulerDisabled(t *testing.T) {
	fx := newCISchedulerFixture(t, 1000, nil, 1)
	if fx.rt.sched.enabled {
		t.Fatal("factory enabled the scheduler")
	}
	if _, err := newCIManagedScheduler(nil, fx.rt.github); !errors.Is(err, ErrConflict) {
		t.Fatalf("nil provisioner accepted: %v", err)
	}
	if err := fx.rt.sched.Run(context.Background()); !errors.Is(err, errCIManagedSchedulingDisabled) {
		t.Fatalf("disabled run: %v", err)
	}
	if n := ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners WHERE workspace_id=$1`, fx.workspace); n != 0 {
		t.Fatalf("disabled scheduler created %d runners", n)
	}
	active, committed := ciSchedulerBudgetState(t, fx.s)
	if active != 0 || committed != 0 {
		t.Fatalf("disabled scheduler consumed funding: active=%d committed=%d", active, committed)
	}
	if n := fx.rt.compute.createCount(); n != 0 {
		t.Fatalf("disabled scheduler called provider: %d", n)
	}
}

func TestCIManagedSchedulerSingleAllocation(t *testing.T) {
	fx := newCISchedulerFixture(t, 100, nil, 2)
	rt := fx.rt
	rt.start(t)
	ciSchedulerWait(t, "one registering runner", func() bool {
		return ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners WHERE workspace_id=$1 AND phase='registering'`, fx.workspace) == 1 &&
			rt.compute.createCount() == 1
	})
	// The money cap admits exactly one reservation: the second queued job
	// stays unclaimed and no second runner or provider write exists.
	if n := ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners WHERE workspace_id=$1`, fx.workspace); n != 1 {
		t.Fatalf("runners=%d want 1", n)
	}
	active, committed := ciSchedulerBudgetState(t, fx.s)
	if active != 1 || committed != 100 {
		t.Fatalf("funding: active=%d committed=%d want 1/100", active, committed)
	}
	if err := rt.sched.Run(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent scheduler run: %v", err)
	}
	rt.stop(t)
	// Registration belongs to the guest bootstrap endpoint; nothing reached
	// GitHub JIT while the execution clock never started.
	if n := atomic.LoadInt32(&rt.gh.jitCalls); n != 0 {
		t.Fatalf("github jit calls=%d want 0", n)
	}
}

func TestCIManagedSchedulerConcurrencyBound(t *testing.T) {
	fx := newCISchedulerFixture(t, 1000, func(p *ciProvisioningBudgetPolicy) {
		p.MaxConcurrent, p.MaxPerWorkspace = 3, 3
	}, 3)
	rt := fx.rt
	rt.compute.block = make(chan struct{})
	var blockOnce sync.Once
	releaseBlock := func() { blockOnce.Do(func() { close(rt.compute.block) }) }
	rt.start(t)
	// The provider block is released before the scheduler join on every
	// path, so a failed wait can never leave creates blocked into fixture
	// teardown; ctx cancellation also unblocks the fake select safely.
	t.Cleanup(releaseBlock)
	ciSchedulerWait(t, "two inflight creates", func() bool {
		return rt.compute.createCount() == 2
	})
	// With both worker slots owned, no third claim may be taken.
	if n := ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners WHERE workspace_id=$1`, fx.workspace); n != 2 {
		t.Fatalf("runners while blocked=%d want 2", n)
	}
	if active, _ := ciSchedulerBudgetState(t, fx.s); active != 2 {
		t.Fatalf("active reservations while blocked=%d want 2", active)
	}
	unclaimed := ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_jobs j WHERE j.workspace_id=$1 AND j.status='queued' AND NOT EXISTS(SELECT 1 FROM ci_runners r WHERE r.workspace_id=j.workspace_id AND r.trigger_job_id=j.id)`, fx.workspace)
	if unclaimed != 1 {
		t.Fatalf("unclaimed jobs while blocked=%d want 1", unclaimed)
	}
	releaseBlock()
	ciSchedulerWait(t, "all three runners registering", func() bool {
		return ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners WHERE workspace_id=$1 AND phase='registering'`, fx.workspace) == 3
	})
	rt.stop(t)
}

func TestCIManagedSchedulerAmbiguousWorkRotation(t *testing.T) {
	s, workspace, policy, claims := ciManagedWorkFixture(t, 4)
	ctx := context.Background()
	// Pre-fence one operation through the existing durable gate, then lose
	// the creator: fenced but no provider stage was ever authorized.
	fenced, err := s.fenceCIRunnerCreate(ctx, workspace, claims[0].Runner)
	if err != nil || !fenced {
		t.Fatalf("pre-fence: fenced=%v err=%v", fenced, err)
	}
	rt := attachCISchedulerRuntime(t, s, workspace, ciManagedClaimProfile())
	rt.start(t)
	// The three unfenced claims provision normally; the ambiguous pending
	// identity never starves them behind the rotating cursor.
	ciSchedulerWait(t, "three fresh creates", func() bool {
		return rt.compute.createCount() == 3
	})
	ciSchedulerWait(t, "three runners registering", func() bool {
		return ciSchedulerCount(t, s, `SELECT count(*) FROM ci_runners WHERE workspace_id=$1 AND phase='registering'`, workspace) == 3
	})
	ambiguous := claims[0]
	snap, err := s.loadCIRunnerLifecycle(ctx, workspace, ambiguous.Runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Phase != "provisioning" || snap.CreateSettled || snap.StopRequestedAt != nil {
		t.Fatalf("ambiguous identity advanced: %+v", snap)
	}
	journal, err := s.ciProvisioningCreationState(ctx, workspace, ambiguous.Runner, policy.ProfileDigest, ambiguous.Spec.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if journal.FinishedAt != nil {
		t.Fatalf("ambiguous journal finished: %+v", journal)
	}
	// Pending work holds its funding: all four reservations stay committed.
	if active, committed := ciSchedulerBudgetState(t, s); active != 4 || committed != 400 {
		t.Fatalf("funding while pending: active=%d committed=%d want 4/400", active, committed)
	}
	rt.stop(t)
}

func TestCIManagedSchedulerStoppedClaimCleanup(t *testing.T) {
	s, workspace, _, claims := ciManagedWorkFixture(t, 1)
	claim := claims[0]
	ctx := context.Background()
	// The atomic claim already committed runner, plan, reservation, and
	// grant; stop it before any Put or create fence.
	stopped, err := s.stopUnstartedCIManagedRunner(ctx, workspace, claim.Runner, claim.Spec.Plan)
	if err != nil || !stopped {
		t.Fatalf("guarded stop: stopped=%v err=%v", stopped, err)
	}
	rt := attachCISchedulerRuntime(t, s, workspace, ciManagedClaimProfile())
	// The enforcer watchdog settles the rejected lease independently. Its
	// cleanup is registered before start so the scheduler always joins
	// first, ahead of the engine, HTTP server, and store teardown.
	engCtx, engCancel := context.WithCancel(context.Background())
	engDone := make(chan struct{})
	go func() {
		defer close(engDone)
		rt.engine.Run(engCtx)
	}()
	t.Cleanup(func() { engCancel(); <-engDone })
	rt.start(t)
	ciSchedulerWait(t, "verified cleanup and released capacity", func() bool {
		active, _ := ciSchedulerBudgetState(t, s)
		return active == 0
	})
	snap, err := s.loadCIRunnerLifecycle(ctx, workspace, claim.Runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Phase != "deleted" || snap.CleanupVerifiedAt == nil || snap.StoppedVerifiedAt == nil {
		t.Fatalf("cleanup not proven: %+v", snap)
	}
	// Capacity release frees the slot only; the committed ceiling is never
	// refunded by cleanup.
	if active, committed := ciSchedulerBudgetState(t, s); active != 0 || committed != 100 {
		t.Fatalf("funding after release: active=%d committed=%d want 0/100", active, committed)
	}
	ciBudgetTotalsZero(t, s, workspace)
	rt.stop(t)
}
