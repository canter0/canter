package controlplane

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/canter0/canter/internal/provider/compute"
)

// ciManagedTestBundleSHA is the root-frozen guest bundle digest verified by
// the artifact gate; the profile pin must equal the embedded bundle exactly.
const ciManagedTestBundleSHA = "5d431bba4165be3e3e6c0a76dcb989cbb45ffbf7ac3f09a6b9a3cd0f74b4bb8d"

// fakeCIManagedCompute wraps the shared enforcer fake with the one-shot SDK
// create seam. It performs no retries, no deduplication, and records only
// in-memory request fields for assertions; userdata is never logged.
type fakeCIManagedCompute struct {
	*fakeCICompute
	mu        sync.Mutex
	creates   int
	scope     compute.CIRunnerScope
	userdata  string
	flavorID  string
	imageID   string
	vcpus     int
	memoryMiB int
	diskGiB   int
	block     chan struct{}
	onEnter   func()
	afterAck  func(stage compute.CIRunnerCreateStage)
	ackLost   map[compute.CIRunnerCreateStage]bool
	sdkErr    error
	sgID      string
	portID    string
	serverID  string
}

func newFakeCIManagedCompute(op string) *fakeCIManagedCompute {
	return &fakeCIManagedCompute{
		fakeCICompute: newFakeCICompute(),
		ackLost:       map[compute.CIRunnerCreateStage]bool{},
		sgID:          "sg_" + op,
		portID:        "port_" + op,
		serverID:      "srv_" + op,
	}
}

func (f *fakeCIManagedCompute) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *fakeCIManagedCompute) captured() (string, compute.CIRunnerScope, string, string, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userdata, f.scope, f.flavorID, f.imageID, f.vcpus, f.memoryMiB, f.diskGiB
}

func (f *fakeCIManagedCompute) addStageResource(kind, id string, scope compute.CIRunnerScope) {
	if kind == "server" {
		f.addServer(scope, id, "ACTIVE")
		return
	}
	f.fakeCICompute.mu.Lock()
	defer f.fakeCICompute.mu.Unlock()
	key := kind + "/" + id
	f.resources[key] = compute.CIRunnerResource{Kind: kind, ID: id, OperationID: scope.OperationID}
	f.reads[key] = 200
}

// DeleteCIRunner mirrors the real provider adapter: every acknowledged kind
// is removed, not only servers.
func (f *fakeCIManagedCompute) DeleteCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error {
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

func (f *fakeCIManagedCompute) CreateCIRunner(ctx context.Context, in compute.CIRunnerCreateRequest) (compute.CIRunnerCreateResult, error) {
	f.mu.Lock()
	f.creates++
	f.scope = in.Scope
	f.userdata = in.UserData
	f.flavorID, f.imageID, f.vcpus, f.memoryMiB, f.diskGiB = in.FlavorID, in.ImageID, in.VCPUs, in.MemoryMiB, in.DiskGiB
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return compute.CIRunnerCreateResult{CreationSettled: true}, ctx.Err()
		}
	}
	if f.onEnter != nil {
		f.onEnter()
	}
	if err := ctx.Err(); err != nil {
		return compute.CIRunnerCreateResult{CreationSettled: true}, err
	}
	f.mu.Lock()
	sdkErr := f.sdkErr
	f.mu.Unlock()
	if sdkErr != nil {
		return compute.CIRunnerCreateResult{CreationSettled: true}, sdkErr
	}
	result := compute.CIRunnerCreateResult{CreationSettled: true}
	stages := []struct {
		stage compute.CIRunnerCreateStage
		kind  string
		id    string
	}{
		{compute.CIRunnerCreateSecurityGroup, "security_group", f.sgID},
		{compute.CIRunnerCreatePort, "port", f.portID},
		{compute.CIRunnerCreateServer, "server", f.serverID},
	}
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
		f.addStageResource(st.kind, st.id, in.Scope)
		f.mu.Lock()
		lost := f.ackLost[st.stage]
		hook := f.afterAck
		f.mu.Unlock()
		if lost {
			return result, fmt.Errorf("compute CI runner create %s failed", st.stage)
		}
		result.Resources = append(result.Resources, compute.CIRunnerResource{
			Kind: st.kind, ID: st.id, OperationID: in.Scope.OperationID,
		})
		if hook != nil {
			hook(st.stage)
		}
	}
	result.CreationSettled = true
	return result, nil
}

type ciManagedFixture struct {
	t         *testing.T
	s         *Store
	workspace string
	profile   ciManagedRunnerProfile
	digest    string
	key       []byte
	token     []byte
	runner    string
	plan      ciRunnerLifecyclePlan
	spec      ciEnforcerSpec
	policy    ciProvisioningBudgetPolicy
	compute   *fakeCIManagedCompute
	engine    *ciEnforcerEngine
	client    *ciEnforcerClient
	clk       *fakeClock
	wallMu    sync.Mutex
	wall      time.Time
	prov      *ciManagedProvisioner
}

func newCIManagedFixture(t *testing.T, op string, number int) *ciManagedFixture {
	t.Helper()
	s, workspace, _ := ciFinancialFixture(t)
	fx := &ciManagedFixture{t: t, s: s, workspace: workspace}
	fx.wall = s.now()
	s.now = func() time.Time {
		fx.wallMu.Lock()
		defer fx.wallMu.Unlock()
		return fx.wall
	}
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	fx.profile = ciManagedProfileFixture()
	fx.profile.GuestBundleSHA256 = ciManagedTestBundleSHA
	var err error
	fx.digest, err = fx.profile.digest()
	if err != nil {
		t.Fatal(err)
	}
	now := fx.wall
	fx.policy = ciBudgetPolicy(now)
	fx.policy.ProfileDigest = fx.digest
	ciBudgetGrant(t, s, "credit_1", 1000, fx.policy)
	fx.runner = ciFinancialRunner(t, s, workspace, "linux-x64-2", number, false, now)
	lease, err := ciEnforcerNewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	fx.plan, err = fx.profile.planFor("linux-x64-2", op, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.trackCIRunnerLifecycle(context.Background(), workspace, fx.runner, fx.plan); err != nil {
		t.Fatalf("track lifecycle: %v", err)
	}
	fx.spec = ciEnforcerSpec{
		Plan:                 fx.plan,
		GitHubInstallationID: 123,
		GitHubRepositoryID:   789,
		SizeID:               "linux-x64-2",
	}
	fx.key = []byte(strings.Repeat("k", 32))
	fx.token = []byte(strings.Repeat("t", 40))
	fx.compute = newFakeCIManagedCompute(op)
	fx.clk = &fakeClock{wall: now, mono: now}
	fx.engine, _ = newTestEngineAt(t, fx.compute, nil, fx.clk)
	srv := &CIEnforcerServer{engine: fx.engine, token: fx.token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	fx.client, err = newCIEnforcerClient(httpSrv.URL, fx.token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	fx.prov, err = newCIManagedProvisioner(s, fx.profile, fx.key, fx.client, fx.compute)
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *ciManagedFixture) now() time.Time {
	fx.wallMu.Lock()
	defer fx.wallMu.Unlock()
	return fx.wall
}

func (fx *ciManagedFixture) advance(d time.Duration) {
	fx.wallMu.Lock()
	fx.wall = fx.wall.Add(d)
	fx.wallMu.Unlock()
	fx.clk.advance(d)
}

func (fx *ciManagedFixture) snapshot(t *testing.T) ciRunnerLifecycleSnapshot {
	t.Helper()
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func (fx *ciManagedFixture) journal(t *testing.T) ciProvisioningCreationJournal {
	t.Helper()
	j, err := fx.s.ciProvisioningCreationState(context.Background(), fx.workspace, fx.runner, fx.digest, fx.plan)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (fx *ciManagedFixture) budget(t *testing.T) ciProvisioningBudgetState {
	t.Helper()
	return ciBudgetPeek(t, fx.s, "credit_1", 1000, fx.policy)
}

func (fx *ciManagedFixture) enforcerStatus(t *testing.T) ciEnforcerStatus {
	t.Helper()
	st, err := fx.client.Status(context.Background(), fx.plan)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (fx *ciManagedFixture) grantRow(t *testing.T) (string, int64, *time.Time, *time.Time) {
	t.Helper()
	var id string
	var maximum int64
	var attempted, delivered *time.Time
	err := fx.s.pool.QueryRow(context.Background(), `SELECT bootstrap_id,maximum_seconds,attempted_at,delivered_at FROM ci_runner_bootstrap_grants WHERE operation_id=$1`, fx.plan.OperationID).Scan(&id, &maximum, &attempted, &delivered)
	if err != nil {
		t.Fatal(err)
	}
	return id, maximum, attempted, delivered
}

func (fx *ciManagedFixture) grantCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := fx.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_runner_bootstrap_grants WHERE operation_id=$1`, fx.plan.OperationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (fx *ciManagedFixture) worker() *ciRunnerLifecycleWorker {
	return &ciRunnerLifecycleWorker{
		Store:   fx.s,
		Backend: &ciLifecycleProviderBackend{Compute: fx.compute, Enforcer: fx.client},
	}
}

var ciManagedTokenPattern = regexp.MustCompile(`v1\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func ciManagedUserdataToken(t *testing.T, userdata string) string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(userdata))
	if err != nil {
		t.Fatal("userdata is not the opaque gzip payload")
	}
	plain, err := io.ReadAll(io.LimitReader(gz, 2<<20))
	gz.Close()
	if err != nil || len(plain) == 0 || len(plain) >= 2<<20 {
		t.Fatal("userdata exceeds bounded decode")
	}
	match := ciManagedTokenPattern.FindString(string(plain))
	if match == "" {
		t.Fatal("userdata carries no bootstrap capability")
	}
	return match
}

func TestCIManagedProvisionerHappyPath(t *testing.T) {
	fx := newCIManagedFixture(t, "prov_happy_1", 1)
	if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("sdk create calls=%d want 1", n)
	}
	userdata, scope, flavor, image, vcpus, memory, disk := fx.compute.captured()
	if scope.OperationID != fx.plan.OperationID || scope.ServerName != fx.plan.ServerName ||
		scope.Region != "r1" || scope.NetworkID != "n1" {
		t.Fatalf("provider scope drift: %+v", scope)
	}
	if flavor != "f2" || image != "i1" || vcpus != 2 || memory != 8192 || disk != 80 {
		t.Fatalf("machine fields: %s %s %d %d %d", flavor, image, vcpus, memory, disk)
	}
	token := ciManagedUserdataToken(t, userdata)
	claims, err := parseCIBootstrapCapability(fx.key, token, fx.now())
	if err != nil {
		t.Fatalf("bootstrap capability parse: %v", err)
	}
	planDigest, _ := fx.spec.digests()
	if claims.Workspace != fx.workspace || claims.Runner != fx.runner ||
		claims.OperationID != fx.plan.OperationID || claims.GitHubRepositoryID != 789 ||
		claims.PlanDigest != planDigest || claims.MaximumSeconds != 3600 ||
		claims.ExpiresAt != fx.plan.SetupDeadlineAt.Unix() {
		t.Fatalf("capability claims drift: %+v", claims)
	}
	grantID, grantMax, attempted, delivered := fx.grantRow(t)
	if grantID != claims.BootstrapID || grantMax != 3600 || attempted != nil || delivered != nil {
		t.Fatalf("grant row: %s %d %v %v", grantID, grantMax, attempted, delivered)
	}
	if n := fx.grantCount(t); n != 1 {
		t.Fatalf("grant rows=%d want 1", n)
	}
	snap := fx.snapshot(t)
	if snap.Phase != "registering" || !snap.CreateSettled || snap.CreateAttemptedAt == nil ||
		snap.StopRequestedAt != nil || snap.ExecutionStartedAt != nil || snap.DeadlineAt != nil {
		t.Fatalf("lifecycle snapshot: %+v", snap)
	}
	journal := fx.journal(t)
	if journal.FinishedAt == nil || journal.Stage != "server" || journal.Outcome == nil ||
		!journal.Outcome.MutationAttempted || !journal.Outcome.CreationSettled || len(journal.Outcome.Resources) != 3 {
		t.Fatalf("creation journal: %+v", journal)
	}
	wantIDs := []string{fx.compute.sgID, fx.compute.portID, fx.compute.serverID}
	for i, r := range journal.Outcome.Resources {
		if r.ID != wantIDs[i] || r.OperationID != fx.plan.OperationID {
			t.Fatalf("journal resource %d: %+v", i, r)
		}
	}
	status := fx.enforcerStatus(t)
	if !status.CreateSettled || status.StopRequested || status.RegistrationAttempted ||
		status.RegistrationSettled || status.GitHubRunnerID != 0 || status.Settled {
		t.Fatalf("enforcer status: %+v", status)
	}
	state := fx.budget(t)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("budget state: %+v", state)
	}
	units, cents, reserved := ciFinancialTotals(t, fx.s, fx.workspace)
	if units != 0 || cents != 0 || reserved != 0 {
		t.Fatalf("customer financials moved: %d %d %d", units, cents, reserved)
	}
}

func TestCIManagedProvisionerNoFundingAndConstructor(t *testing.T) {
	t.Run("constructor rejects invalid inputs without IO", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_ctor_1", 1)
		cases := map[string]func() (*ciManagedProvisioner, error){
			"nil store": func() (*ciManagedProvisioner, error) {
				return newCIManagedProvisioner(nil, fx.profile, fx.key, fx.client, fx.compute)
			},
			"nil pool": func() (*ciManagedProvisioner, error) {
				return newCIManagedProvisioner(&Store{}, fx.profile, fx.key, fx.client, fx.compute)
			},
			"nil enforcer": func() (*ciManagedProvisioner, error) {
				return newCIManagedProvisioner(fx.s, fx.profile, fx.key, nil, fx.compute)
			},
			"nil compute": func() (*ciManagedProvisioner, error) {
				return newCIManagedProvisioner(fx.s, fx.profile, fx.key, fx.client, nil)
			},
			"short key": func() (*ciManagedProvisioner, error) {
				return newCIManagedProvisioner(fx.s, fx.profile, []byte("short"), fx.client, fx.compute)
			},
			"invalid profile": func() (*ciManagedProvisioner, error) {
				bad := fx.profile
				bad.Version = 2
				return newCIManagedProvisioner(fx.s, bad, fx.key, fx.client, fx.compute)
			},
			"bundle mismatch": func() (*ciManagedProvisioner, error) {
				bad := fx.profile
				bad.GuestBundleSHA256 = strings.Repeat("d", 64)
				return newCIManagedProvisioner(fx.s, bad, fx.key, fx.client, fx.compute)
			},
		}
		for name, build := range cases {
			prov, err := build()
			if err == nil || prov != nil {
				t.Fatalf("%s accepted: %v", name, err)
			}
		}
		if n := fx.compute.createCount(); n != 0 {
			t.Fatalf("constructor performed provider IO: %d", n)
		}
	})

	t.Run("no funding fails before lease or provider", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_nofund_1", 1)
		ciBudgetReset(t, fx.s)
		err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("unfunded provision: %v", err)
		}
		if n := fx.compute.createCount(); n != 0 {
			t.Fatalf("provider called without funding: %d", n)
		}
		status, err := fx.client.Status(context.Background(), fx.plan)
		if err == nil && (status.LeaseID != "" || len(status.Resources) != 0) {
			t.Fatalf("enforcer lease exists without funding: %+v", status)
		}
	})

	t.Run("caller mutations cannot change sealed profile or key", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_sealed_1", 1)
		originalKey := append([]byte(nil), fx.key...)
		fx.profile.Machines[0].FlavorID = "f9"
		fx.key[0] ^= 0xff
		if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err != nil {
			t.Fatalf("provision with mutated caller inputs: %v", err)
		}
		_, _, flavor, _, _, _, _ := fx.compute.captured()
		if flavor != "f2" {
			t.Fatalf("sealed profile changed: %s", flavor)
		}
		userdata, _, _, _, _, _, _ := fx.compute.captured()
		token := ciManagedUserdataToken(t, userdata)
		if _, err := parseCIBootstrapCapability(originalKey, token, fx.now()); err != nil {
			t.Fatalf("signing key was not sealed: %v", err)
		}
	})
}

func TestCIManagedProvisionerConcurrentSingleCreate(t *testing.T) {
	fx := newCIManagedFixture(t, "prov_race_1", 1)
	fx.compute.block = make(chan struct{})
	const callers = 12
	errs := make([]error, callers)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fx.compute.block) }) }
	// Every failure path must join the blocked callers before fixture
	// cleanup touches the store; deferred joins run before t.Cleanup.
	defer func() {
		cancel()
		release()
		wg.Wait()
	}()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fx.prov.Provision(ctx, fx.workspace, fx.runner, fx.spec)
		}(i)
	}
	deadline := time.Now().Add(10 * time.Second)
	for fx.compute.createCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("first SDK create never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("sdk create calls=%d want 1", n)
	}
	snap := fx.snapshot(t)
	if snap.StopRequestedAt != nil {
		t.Fatal("duplicate caller stopped a live creation")
	}
	state := fx.budget(t)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("budget state: %+v", state)
	}
	if n := fx.grantCount(t); n != 1 {
		t.Fatalf("grant rows=%d want 1", n)
	}
	if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err != nil {
		t.Fatalf("idempotent provision after registering: %v", err)
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("sdk create calls after idempotent retry=%d want 1", n)
	}
}

func TestCIManagedProvisionerCancelBeforeFirstCallback(t *testing.T) {
	t.Run("cancelled parent records no mutation and rejects", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_cancel_1", 1)
		ctx, cancel := context.WithCancel(context.Background())
		fx.compute.onEnter = cancel
		err := fx.prov.Provision(ctx, fx.workspace, fx.runner, fx.spec)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provision error: %v", err)
		}
		journal := fx.journal(t)
		if journal.FinishedAt == nil || journal.Outcome == nil || journal.Outcome.MutationAttempted ||
			!journal.Outcome.CreationSettled || len(journal.Outcome.Resources) != 0 {
			t.Fatalf("journal: %+v", journal)
		}
		snap := fx.snapshot(t)
		if snap.StopRequestedAt == nil || snap.Phase != "deleting" {
			t.Fatalf("stop not committed: %+v", snap)
		}
		status := fx.enforcerStatus(t)
		if !status.CreateSettled || !status.StopRequested || len(status.Resources) != 0 {
			t.Fatalf("enforcer lease not rejected: %+v", status)
		}
		if len(fx.compute.fakeCICompute.servers) != 0 {
			t.Fatal("server materialized before first callback")
		}
	})

	t.Run("sanitized SDK error never leaks provider detail", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_secret_1", 1)
		fx.compute.sdkErr = errors.New("provider response SECRET_MARKER_XYZ body")
		err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec)
		if err == nil {
			t.Fatal("expected provisioning failure")
		}
		if strings.Contains(err.Error(), "SECRET_MARKER_XYZ") || strings.Contains(fmt.Sprintf("%+v", err), "SECRET_MARKER_XYZ") {
			t.Fatalf("provider detail leaked: %v", err)
		}
		for e := err; e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), "SECRET_MARKER_XYZ") {
				t.Fatalf("provider detail leaked in chain: %v", e)
			}
		}
	})
}

func TestCIManagedProvisionerCancelBetweenStages(t *testing.T) {
	fx := newCIManagedFixture(t, "prov_mid_1", 1)
	var stopErr error
	fx.compute.afterAck = func(stage compute.CIRunnerCreateStage) {
		if stage != compute.CIRunnerCreateSecurityGroup {
			return
		}
		// Commit the durable stop through the existing API after the SG ack
		// but before the port fence, exactly as an external cancel would.
		_, stopErr = fx.s.requestCIRunnerStop(context.Background(), fx.workspace, fx.runner, "cancelled", nil)
	}
	err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec)
	if stopErr != nil {
		t.Fatalf("commit stop: %v", stopErr)
	}
	if err == nil {
		t.Fatal("expected provisioning failure after committed stop")
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("sdk calls=%d want 1", n)
	}
	fx.compute.fakeCICompute.mu.Lock()
	_, hasPort := fx.compute.resources["port/"+fx.compute.portID]
	_, hasServer := fx.compute.resources["server/"+fx.compute.serverID]
	fx.compute.fakeCICompute.mu.Unlock()
	if hasPort || hasServer {
		t.Fatal("port or server POST ran after committed stop")
	}
	// Before physical discovery the retained inventory is empty, so recovery
	// correctly holds pending and funding stays committed.
	state := fx.budget(t)
	if state.Active != 1 {
		t.Fatalf("funding released before core cleanup: %+v", state)
	}
	leaseID := fx.plan.EnforcerLease
	ctx := context.Background()
	fx.engine.reconcilePhysical(ctx, leaseID)
	fx.engine.reconcileGitHub(ctx, leaseID)
	if err := fx.prov.Recover(ctx, fx.workspace, fx.runner, fx.spec); err != nil {
		t.Fatalf("recover after witness: %v", err)
	}
	status := fx.enforcerStatus(t)
	if !status.CreateSettled || status.Settled {
		t.Fatalf("enforcer status: %+v", status)
	}
	var retained []string
	for _, r := range status.Resources {
		retained = append(retained, r.Kind+"/"+r.ID)
	}
	if strings.Join(retained, ",") != "security_group/"+fx.compute.sgID {
		t.Fatalf("retained witness: %v", retained)
	}
	driveCleanup(t, fx.engine, leaseID, 2)
	if err := fx.worker().Reconcile(ctx, fx.workspace, fx.runner); err != nil {
		t.Fatalf("lifecycle reconcile: %v", err)
	}
	if err := fx.s.releaseCIProvisioningCapacity(ctx, fx.workspace, fx.runner); err != nil {
		t.Fatalf("release capacity: %v", err)
	}
	state = fx.budget(t)
	if state.Active != 0 || state.CommittedMicros != 100 {
		t.Fatalf("budget after cleanup: %+v", state)
	}
}

func TestCIManagedProvisionerAmbiguousServerRecovery(t *testing.T) {
	t.Run("late owned server closes created from retained witness", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_late_1", 1)
		fx.compute.ackLost[compute.CIRunnerCreateServer] = true
		if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err == nil {
			t.Fatal("expected ambiguous create failure")
		}
		if n := fx.compute.createCount(); n != 1 {
			t.Fatalf("sdk calls=%d want 1", n)
		}
		journal := fx.journal(t)
		if journal.FinishedAt == nil || journal.Outcome == nil || journal.Outcome.CreationSettled ||
			len(journal.Outcome.Resources) != 2 || journal.Stage != "server" {
			t.Fatalf("journal: %+v", journal)
		}
		ctx := context.Background()
		// No positive witness yet: recovery must hold pending, never settle.
		if err := fx.prov.Recover(ctx, fx.workspace, fx.runner, fx.spec); !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("recover before discovery: %v", err)
		}
		snap := fx.snapshot(t)
		if snap.CreateSettled {
			t.Fatal("lifecycle settled without enforcer acknowledgement")
		}
		if state := fx.budget(t); state.Active != 1 {
			t.Fatalf("funding released early: %+v", state)
		}
		leaseID := fx.plan.EnforcerLease
		fx.engine.reconcilePhysical(ctx, leaseID)
		fx.engine.reconcileGitHub(ctx, leaseID)
		if err := fx.prov.Recover(ctx, fx.workspace, fx.runner, fx.spec); err != nil {
			t.Fatalf("recover with witness: %v", err)
		}
		if n := fx.compute.createCount(); n != 1 {
			t.Fatalf("sdk replayed: %d", n)
		}
		status := fx.enforcerStatus(t)
		if !status.CreateSettled {
			t.Fatalf("creation not closed: %+v", status)
		}
		driveCleanup(t, fx.engine, leaseID, 2)
		if err := fx.worker().Reconcile(ctx, fx.workspace, fx.runner); err != nil {
			t.Fatalf("lifecycle reconcile: %v", err)
		}
		if err := fx.s.releaseCIProvisioningCapacity(ctx, fx.workspace, fx.runner); err != nil {
			t.Fatalf("release capacity: %v", err)
		}
		if state := fx.budget(t); state.Active != 0 || state.CommittedMicros != 100 {
			t.Fatalf("budget after cleanup: %+v", state)
		}
	})

	t.Run("mismatched retained witness denies close", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_mismatch_1", 1)
		fx.compute.ackLost[compute.CIRunnerCreateServer] = true
		if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err == nil {
			t.Fatal("expected ambiguous create failure")
		}
		// Replace the acknowledged SG with a different provider ID before
		// discovery: the retained witness can no longer match the journal.
		fx.compute.fakeCICompute.mu.Lock()
		delete(fx.compute.resources, "security_group/"+fx.compute.sgID)
		delete(fx.compute.reads, "security_group/"+fx.compute.sgID)
		other := "sg_" + fx.plan.OperationID + "_x"
		fx.compute.resources["security_group/"+other] = compute.CIRunnerResource{Kind: "security_group", ID: other, OperationID: fx.plan.OperationID}
		fx.compute.reads["security_group/"+other] = 200
		fx.compute.fakeCICompute.mu.Unlock()
		ctx := context.Background()
		fx.engine.reconcilePhysical(ctx, fx.plan.EnforcerLease)
		fx.engine.reconcileGitHub(ctx, fx.plan.EnforcerLease)
		if err := fx.prov.Recover(ctx, fx.workspace, fx.runner, fx.spec); !errors.Is(err, ErrConflict) {
			t.Fatalf("mismatched witness recover: %v", err)
		}
		if status := fx.enforcerStatus(t); status.CreateSettled {
			t.Fatal("fictional close committed")
		}
	})

	t.Run("duplicate server witness denies close", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_dup_1", 1)
		fx.compute.ackLost[compute.CIRunnerCreateServer] = true
		if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err == nil {
			t.Fatal("expected ambiguous create failure")
		}
		// A second same-operation server is a foreign duplicate, never a
		// settleable witness.
		fx.compute.addServer(ciProviderScope(fx.plan), "srv_foreign_1", "ACTIVE")
		ctx := context.Background()
		fx.engine.reconcilePhysical(ctx, fx.plan.EnforcerLease)
		fx.engine.reconcileGitHub(ctx, fx.plan.EnforcerLease)
		if err := fx.prov.Recover(ctx, fx.workspace, fx.runner, fx.spec); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate witness recover: %v", err)
		}
		if status := fx.enforcerStatus(t); status.CreateSettled {
			t.Fatal("fictional close committed")
		}
	})
}

func TestCIManagedProvisionerCrashBeforeFence(t *testing.T) {
	for _, advanceBy := range []time.Duration{901 * time.Second, 22600 * time.Second} {
		name := "past_setup"
		if advanceBy > 20000*time.Second {
			name = "past_hard"
		}
		t.Run(name, func(t *testing.T) {
			fx := newCIManagedFixture(t, "prov_crash_"+name, 1)
			// Funded and tracked, but the creator crashed before Put, grant,
			// or the create fence: no SDK, no grant, no fence.
			if _, err := fx.s.reserveCIProvisioningBudget(context.Background(), fx.workspace, fx.runner, fx.digest, fx.plan); err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if _, err := fx.s.requestCIRunnerStop(context.Background(), fx.workspace, fx.runner, "setup_failed", nil); err != nil {
				t.Fatalf("stop: %v", err)
			}
			fx.advance(advanceBy)
			if err := fx.prov.Recover(context.Background(), fx.workspace, fx.runner, fx.spec); err != nil {
				t.Fatalf("recover: %v", err)
			}
			if n := fx.compute.createCount(); n != 0 {
				t.Fatalf("sdk invoked during recovery: %d", n)
			}
			journal := fx.journal(t)
			if journal.FinishedAt == nil || journal.Outcome == nil || journal.Outcome.MutationAttempted ||
				!journal.Outcome.CreationSettled || len(journal.Outcome.Resources) != 0 {
				t.Fatalf("journal: %+v", journal)
			}
			status := fx.enforcerStatus(t)
			if !status.CreateSettled || !status.StopRequested || len(status.Resources) != 0 {
				t.Fatalf("rejected lease: %+v", status)
			}
			ctx := context.Background()
			leaseID := fx.plan.EnforcerLease
			driveCleanup(t, fx.engine, leaseID, 2)
			if err := fx.worker().Reconcile(ctx, fx.workspace, fx.runner); err != nil {
				t.Fatalf("lifecycle reconcile: %v", err)
			}
			if err := fx.s.releaseCIProvisioningCapacity(ctx, fx.workspace, fx.runner); err != nil {
				t.Fatalf("release capacity: %v", err)
			}
			if state := fx.budget(t); state.Active != 0 || state.CommittedMicros != 100 {
				t.Fatalf("budget after cleanup: %+v", state)
			}
		})
	}
}

type ciManagedDropCloseTransport struct {
	base  http.RoundTripper
	armed int32
}

func (d *ciManagedDropCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if strings.HasSuffix(req.URL.Path, "/close-creation") && atomic.CompareAndSwapInt32(&d.armed, 1, 0) {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, errors.New("dropped response")
	}
	return resp, nil
}

func TestCIManagedProvisionerCloseAckLostAndDrift(t *testing.T) {
	fx := newCIManagedFixture(t, "prov_ack_1", 1)
	fx.client.hc.Transport = &ciManagedDropCloseTransport{base: http.DefaultTransport, armed: 1}
	if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err == nil {
		t.Fatal("expected close acknowledgement loss")
	}
	// The first close committed server-side; the durable outcome is intact.
	journal := fx.journal(t)
	if journal.FinishedAt == nil || !ciManagedJournalFullSuccess(journal) {
		t.Fatalf("journal lost full outcome: %+v", journal)
	}
	if status := fx.enforcerStatus(t); !status.CreateSettled {
		t.Fatal("enforcer close did not commit")
	}
	// The retry performs only the acknowledgement: no second SDK call.
	if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err != nil {
		t.Fatalf("ack retry: %v", err)
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("sdk replayed on ack retry: %d", n)
	}
	snap := fx.snapshot(t)
	if snap.Phase != "registering" || !snap.CreateSettled {
		t.Fatalf("phase after ack retry: %+v", snap)
	}
	// Spec drift is rejected before any provider write.
	drift := fx.spec
	drift.GitHubRepositoryID = 790
	if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, drift); !errors.Is(err, ErrConflict) {
		t.Fatalf("drifted spec: %v", err)
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("provider write on drift: %d", n)
	}
}

func TestCIManagedProvisionerMaxConnsOne(t *testing.T) {
	fx := newCIManagedFixture(t, "prov_max1", 1)
	old := fx.s.pool
	cfg := *old.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.s.pool = pool
	defer func() {
		fx.s.pool = old
		pool.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fx.prov.Provision(ctx, fx.workspace, fx.runner, fx.spec); err != nil {
		t.Fatalf("provision under MaxConns=1: %v", err)
	}
	// Recovery of a crashed-before-fence runner under the same single
	// connection pool.
	now := fx.now()
	runner2 := ciFinancialRunner(t, fx.s, fx.workspace, "linux-x64-2", 2, false, now)
	lease2, err := ciEnforcerNewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	plan2, err := fx.profile.planFor("linux-x64-2", "prov_max1_r", lease2, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = fx.s.trackCIRunnerLifecycle(ctx, fx.workspace, runner2, plan2); err != nil {
		t.Fatalf("track second lifecycle: %v", err)
	}
	if _, err = fx.s.reserveCIProvisioningBudget(ctx, fx.workspace, runner2, fx.digest, plan2); err != nil {
		t.Fatalf("reserve second: %v", err)
	}
	spec2 := ciEnforcerSpec{
		Plan:                 plan2,
		GitHubInstallationID: 123,
		GitHubRepositoryID:   789,
		SizeID:               "linux-x64-2",
	}
	if _, err = fx.s.requestCIRunnerStop(ctx, fx.workspace, runner2, "setup_failed", nil); err != nil {
		t.Fatalf("stop second: %v", err)
	}
	fx.advance(901 * time.Second)
	if err := fx.prov.Recover(ctx, fx.workspace, runner2, spec2); err != nil {
		t.Fatalf("recover under MaxConns=1: %v", err)
	}
	if n := fx.compute.createCount(); n != 1 {
		t.Fatalf("sdk calls=%d want 1", n)
	}
	// The lifecycle worker's advisory session is dedicated: reconciling the
	// recovered runner under the same single connection must not hang, and
	// releasing verified capacity keeps the committed ceiling intact.
	driveCleanup(t, fx.engine, plan2.EnforcerLease, 2)
	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 5*time.Second)
	err = fx.worker().Reconcile(reconcileCtx, fx.workspace, runner2)
	reconcileCancel()
	if err != nil {
		t.Fatalf("reconcile under MaxConns=1: %v", err)
	}
	if err = fx.s.releaseCIProvisioningCapacity(ctx, fx.workspace, runner2); err != nil {
		t.Fatalf("release second capacity: %v", err)
	}
	if state := fx.budget(t); state.Active != 1 || state.CommittedMicros != 200 {
		t.Fatalf("budget after second release: %+v", state)
	}
}

// ciManagedCancelCloseTransport forwards the real close-creation request
// (the enforcer commits it server-side), then cancels the parent context
// before the acknowledgement returns to the caller.
type ciManagedCancelCloseTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
	once   sync.Once
}

func (d *ciManagedCancelCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(req)
	if strings.HasSuffix(req.URL.Path, "/close-creation") {
		d.once.Do(d.cancel)
	}
	return resp, err
}

func TestCIManagedProvisionerCancelAfterServerAck(t *testing.T) {
	assertCancelledCleanup := func(t *testing.T, fx *ciManagedFixture) {
		t.Helper()
		journal := fx.journal(t)
		if !ciManagedJournalFullSuccess(journal) {
			t.Fatalf("durable outcome lost: %+v", journal)
		}
		snap := fx.snapshot(t)
		if snap.StopRequestedAt == nil || snap.Phase == "registering" ||
			snap.ExecutionStartedAt != nil || snap.CreateSettled {
			t.Fatalf("snapshot after cancelled ack: %+v", snap)
		}
		status := fx.enforcerStatus(t)
		if !status.StopRequested || !status.CreateSettled || status.RegistrationAttempted ||
			status.RegistrationSettled || status.GitHubRunnerID != 0 || status.Settled {
			t.Fatalf("enforcer status after cancelled ack: %+v", status)
		}
		if n := fx.compute.createCount(); n != 1 {
			t.Fatalf("sdk calls=%d want 1", n)
		}
		// A retry may finish durable bookkeeping but must never replay the
		// provider write or reach registering.
		_ = fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec)
		if n := fx.compute.createCount(); n != 1 {
			t.Fatalf("sdk replayed on retry: %d", n)
		}
		snap = fx.snapshot(t)
		if snap.Phase == "registering" {
			t.Fatal("cancelled creation reached registering")
		}
	}

	t.Run("cancel inside server ack", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_ackcancel_1", 1)
		ctx, cancel := context.WithCancel(context.Background())
		fx.compute.afterAck = func(stage compute.CIRunnerCreateStage) {
			if stage == compute.CIRunnerCreateServer {
				cancel()
			}
		}
		err := fx.prov.Provision(ctx, fx.workspace, fx.runner, fx.spec)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provision error: %v", err)
		}
		assertCancelledCleanup(t, fx)
	})

	t.Run("cancel between close ack and mark", func(t *testing.T) {
		fx := newCIManagedFixture(t, "prov_closecancel_1", 1)
		ctx, cancel := context.WithCancel(context.Background())
		fx.client.hc.Transport = &ciManagedCancelCloseTransport{
			base: http.DefaultTransport, cancel: cancel,
		}
		err := fx.prov.Provision(ctx, fx.workspace, fx.runner, fx.spec)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("provision error: %v", err)
		}
		assertCancelledCleanup(t, fx)
	})
}

func TestCIManagedProvisionerLateMaintenanceGuard(t *testing.T) {
	fx := newCIManagedFixture(t, "prov_guard_1", 1)
	if err := fx.prov.Provision(context.Background(), fx.workspace, fx.runner, fx.spec); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// A late admission commits execution after creation: pre-start
	// maintenance must never stop the control plane or the enforcer lease
	// of an adopted job.
	if _, err := fx.s.pool.Exec(context.Background(),
		`UPDATE ci_runners SET execution_started_at=$3,phase='running' WHERE workspace_id=$1 AND id=$2`,
		fx.workspace, fx.runner, fx.now()); err != nil {
		t.Fatal(err)
	}
	err := fx.prov.stopAndRecover(context.Background(), fx.workspace, fx.runner, fx.spec, errors.New("late maintenance failure"))
	if !errors.Is(err, errCIManagedProvisioningUnavailable) {
		t.Fatalf("stopAndRecover on adopted runner: %v", err)
	}
	snap := fx.snapshot(t)
	if snap.StopRequestedAt != nil || snap.ExecutionStartedAt == nil || snap.Phase != "running" {
		t.Fatalf("adopted runner changed: %+v", snap)
	}
	status := fx.enforcerStatus(t)
	if status.StopRequested || !status.CreateSettled {
		t.Fatalf("enforcer touched an adopted job: %+v", status)
	}
}
