package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/internal/model"
	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/internal/provider/m1"
)

type fakeModel struct {
	mu           sync.Mutex
	compileCalls int
	plan         Plan
}

func (f *fakeModel) Probe(context.Context) model.ProbeResult { return model.ProbeResult{OK: true} }
func (f *fakeModel) Compile(_ context.Context, _ string, target any) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compileCalls++
	plan, ok := target.(*Plan)
	if !ok {
		return "", 0, fmt.Errorf("unexpected target %T", target)
	}
	*plan = f.plan
	return "fake-model", 1, nil
}

type fakeStore struct {
	mu                 sync.Mutex
	objects            map[string][]byte
	versions           map[string]int
	autoProof          bool
	versionReadBarrier chan struct{}
	versionReadTarget  int
	versionReadCount   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte), versions: make(map[string]int), autoProof: true}
}
func (f *fakeStore) Probe(context.Context) m1.ProbeResult { return m1.ProbeResult{OK: true} }
func (f *fakeStore) Put(_ context.Context, key string, data []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
	f.versions[key]++
	return nil
}
func (f *fakeStore) PutJSON(ctx context.Context, key string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return f.Put(ctx, key, payload, "application/json")
}
func (f *fakeStore) PutJSONIfAbsent(ctx context.Context, key string, value any) (string, bool, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, found := f.objects[key]
	if found {
		return "", false, nil
	}
	f.objects[key] = payload
	f.versions[key]++
	return fmt.Sprintf("etag-%d", f.versions[key]), true, nil
}
func (f *fakeStore) PutJSONIfMatch(_ context.Context, key, etag string, value any) (string, bool, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if etag != fmt.Sprintf("etag-%d", f.versions[key]) {
		return "", false, nil
	}
	f.objects[key] = payload
	f.versions[key]++
	return fmt.Sprintf("etag-%d", f.versions[key]), true, nil
}
func (f *fakeStore) Get(_ context.Context, key string, target any) error {
	f.mu.Lock()
	payload, found := f.objects[key]
	f.mu.Unlock()
	if !found {
		return errors.New("not found")
	}
	return json.Unmarshal(payload, target)
}
func (f *fakeStore) GetBytes(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, found := f.objects[key]
	if !found {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), payload...), nil
}
func (f *fakeStore) GetJSONVersion(ctx context.Context, key string, target any) (bool, string, error) {
	f.mu.Lock()
	payload, found := f.objects[key]
	version := f.versions[key]
	barrier := f.versionReadBarrier
	if barrier != nil {
		f.versionReadCount++
		if f.versionReadCount == f.versionReadTarget {
			close(barrier)
		}
	}
	f.mu.Unlock()
	if barrier != nil {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-barrier:
		}
	}
	if !found {
		return false, "", nil
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("etag-%d", version), nil
}
func (f *fakeStore) GetOptional(ctx context.Context, key string, target any) (bool, error) {
	f.mu.Lock()
	_, found := f.objects[key]
	f.mu.Unlock()
	if !found {
		return false, nil
	}
	return true, f.Get(ctx, key, target)
}
func (f *fakeStore) PresignPut(ctx context.Context, key string, _ time.Duration) (string, error) {
	if f.autoProof {
		_ = f.PutJSON(ctx, key, BootProof{Sandbox: "demo", Status: "booted", ExitCode: 0, Hostname: "fake"})
	}
	return "https://signed.invalid/" + key, nil
}
func (f *fakeStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://signed.invalid/" + key, nil
}

type fakeCompute struct {
	mu                     sync.Mutex
	store                  *fakeStore
	stateKey               string
	servers                map[string][]compute.Server
	resolveCalls           int
	createCalls            int
	exposeCalls            int
	findExposureCalls      int
	createAmbiguous        bool
	createNoResourceErr    error
	hideLookup             bool
	hideServer             bool
	hideLookupCalls        int
	lookupCalls            int
	lookupBarrier          chan struct{}
	lookupBarrierTarget    int
	lookupBarrierCount     int
	exposeErrors           []error
	exposurePolicy         *compute.SecurityPolicy
	hideExposureLookup     bool
	exposeErrorAfterPolicy bool
	createAccepted         chan struct{}
	createRelease          chan struct{}
	exposeAccepted         chan struct{}
	exposeRelease          chan struct{}
	deletedServers         []string
	deletedPolicies        []string
	waitActiveCalls        int
	waitActiveAccepted     chan struct{}
	waitActiveRelease      chan struct{}
	assertIntentOnCreate   func(State, compute.ManagedServerRequest) error
	assertExposure         func(State) error
}

func newFakeCompute(store *fakeStore, key string) *fakeCompute {
	return &fakeCompute{store: store, stateKey: key, servers: make(map[string][]compute.Server)}
}
func (f *fakeCompute) Probe(context.Context) compute.ProbeResult {
	return compute.ProbeResult{OK: true}
}
func (f *fakeCompute) Resolve(context.Context, string, string) (compute.Shape, string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls++
	return compute.Shape{ID: "shape-1", Name: "shape"}, "image-1", []string{"network-a", "network-b"}, nil
}
func (f *fakeCompute) CreateManaged(ctx context.Context, request compute.ManagedServerRequest) (compute.Server, error) {
	f.mu.Lock()
	f.createCalls++
	if f.assertIntentOnCreate != nil {
		var state State
		if err := f.store.Get(ctx, f.stateKey, &state); err != nil {
			f.mu.Unlock()
			return compute.Server{}, err
		}
		if err := f.assertIntentOnCreate(state, request); err != nil {
			f.mu.Unlock()
			return compute.Server{}, err
		}
	}
	if f.createNoResourceErr != nil {
		err := f.createNoResourceErr
		f.mu.Unlock()
		return compute.Server{}, err
	}
	server := compute.Server{ID: fmt.Sprintf("server-%d", f.createCalls), Name: request.Name, Status: "BUILD", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": request.Sandbox, "canter.operation": request.OperationID, "canter.resource": request.Name}}
	f.servers[request.Name] = append(f.servers[request.Name], server)
	ambiguous := f.createAmbiguous
	accepted := f.createAccepted
	release := f.createRelease
	call := f.createCalls
	f.mu.Unlock()
	if accepted != nil && call == 1 {
		close(accepted)
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return compute.Server{}, ctx.Err()
		case <-release:
		}
	}
	if ambiguous {
		return compute.Server{}, errors.New("provider response lost")
	}
	return server, nil
}
func (f *fakeCompute) FindManagedServers(_ context.Context, sandbox, operationID, name string) ([]compute.Server, error) {
	f.mu.Lock()
	f.lookupCalls++
	var matches []compute.Server
	if !f.hideLookup && f.lookupCalls > f.hideLookupCalls {
		for _, server := range f.servers[name] {
			if server.Name == name && server.Metadata["canter.sandbox"] == sandbox && server.Metadata["canter.operation"] == operationID && server.Metadata["canter.resource"] == name {
				matches = append(matches, server)
			}
		}
	}
	barrier := f.lookupBarrier
	if barrier != nil {
		f.lookupBarrierCount++
		if f.lookupBarrierCount == f.lookupBarrierTarget {
			close(barrier)
		}
	}
	f.mu.Unlock()
	if barrier != nil {
		<-barrier
	}
	return matches, nil
}
func (f *fakeCompute) Server(_ context.Context, id string) (compute.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hideServer {
		return compute.Server{}, &compute.HTTPError{StatusCode: 404, Body: "not found"}
	}
	for _, servers := range f.servers {
		for _, server := range servers {
			if server.ID == id {
				return server, nil
			}
		}
	}
	return compute.Server{}, &compute.HTTPError{StatusCode: 404, Body: "not found"}
}
func (f *fakeCompute) WaitActive(ctx context.Context, id string) (compute.Server, error) {
	server, err := f.Server(ctx, id)
	if err != nil {
		return compute.Server{}, err
	}
	f.mu.Lock()
	f.waitActiveCalls++
	accepted := f.waitActiveAccepted
	release := f.waitActiveRelease
	call := f.waitActiveCalls
	f.mu.Unlock()
	if accepted != nil && call == 1 {
		close(accepted)
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return compute.Server{}, ctx.Err()
		case <-release:
		}
	}
	server.Status = "ACTIVE"
	server.Addresses = map[string][]compute.Address{"public": {{Addr: "192.0.2.10", Version: 4}}}
	return server, nil
}
func (f *fakeCompute) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedServers = append(f.deletedServers, id)
	for name, servers := range f.servers {
		for index, server := range servers {
			if server.ID == id {
				f.servers[name] = append(servers[:index], servers[index+1:]...)
				return nil
			}
		}
	}
	return nil
}
func (f *fakeCompute) ExposeManagedTCP(ctx context.Context, input compute.ManagedTCPExposureRequest) (compute.SecurityPolicy, error) {
	f.mu.Lock()
	f.exposeCalls++
	if f.assertExposure != nil {
		var state State
		if err := f.store.Get(ctx, f.stateKey, &state); err != nil {
			f.mu.Unlock()
			return compute.SecurityPolicy{}, err
		}
		if err := f.assertExposure(state); err != nil {
			f.mu.Unlock()
			return compute.SecurityPolicy{}, err
		}
	}
	if len(f.exposeErrors) > 0 {
		err := f.exposeErrors[0]
		f.exposeErrors = f.exposeErrors[1:]
		if err != nil {
			if f.exposeErrorAfterPolicy {
				policy := compute.SecurityPolicy{ID: "policy-1", PortID: "port-1", RuleID: "rule-1", Port: input.Port}
				f.exposurePolicy = &policy
			}
			f.mu.Unlock()
			return compute.SecurityPolicy{}, err
		}
	}
	policy := compute.SecurityPolicy{ID: "policy-1", PortID: "port-1", RuleID: "rule-1", Port: input.Port}
	f.exposurePolicy = &policy
	accepted := f.exposeAccepted
	release := f.exposeRelease
	call := f.exposeCalls
	f.mu.Unlock()
	if accepted != nil && call == 1 {
		close(accepted)
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return compute.SecurityPolicy{}, ctx.Err()
		case <-release:
		}
	}
	return policy, nil
}
func (f *fakeCompute) FindManagedTCPExposure(_ context.Context, input compute.ManagedTCPExposureRequest) (compute.SecurityPolicy, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findExposureCalls++
	if f.hideExposureLookup || f.exposurePolicy == nil {
		return compute.SecurityPolicy{}, false, nil
	}
	return *f.exposurePolicy, true, nil
}
func (f *fakeCompute) DeleteSecurityPolicy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedPolicies = append(f.deletedPolicies, id)
	if f.exposurePolicy != nil && f.exposurePolicy.ID == id {
		f.exposurePolicy = nil
	}
	return nil
}

func testSpec() Spec {
	return Spec{APIVersion: APIVersion, Kind: "Sandbox", Metadata: Metadata{Name: "demo"}, Spec: Desired{
		Intent: "test durable creation", Compute: ComputeSpec{Class: "c1", Image: "ubuntu-24.04", Replicas: 1, Bootstrap: "true"},
		M1: M1Spec{Prefix: "systems/demo"}, Policy: Policy{MaxReplicas: 1},
	}}
}

func testPlan(spec Spec) Plan {
	return Plan{SchemaVersion: "v1", Summary: "test", Operations: []Operation{
		{Kind: "m1.ensure", Name: spec.Metadata.Name, Prefix: spec.Spec.M1.Prefix},
		{Kind: "compute.ensure", Name: spec.Metadata.Name, Class: spec.Spec.Compute.Class, Image: spec.Spec.Compute.Image, Replicas: spec.Spec.Compute.Replicas},
	}}
}

func creatingState(spec Spec, operationID string) State {
	proofKey := spec.Spec.M1.Prefix + "/proofs/" + operationID + "-1.json"
	createdAt := time.Now().UTC()
	return State{
		Sandbox: spec.Metadata.Name, Phase: "creating", Class: spec.Spec.Compute.Class, Image: spec.Spec.Compute.Image, ProofKeys: []string{proofKey}, CreatedAt: createdAt,
		CreationIntent: &CreationIntent{OperationID: operationID, DesiredClass: spec.Spec.Compute.Class, DesiredImage: spec.Spec.Compute.Image, ShapeID: "shape-1", ImageID: "image-1", NetworkIDs: []string{"network-a"}, Phase: "creating", CreatedAt: createdAt,
			Resources: []ComputeResourceIntent{{Replica: 1, Name: "canter-demo-1", ProofKey: proofKey, Phase: "pending"}}},
	}
}

func readyState(spec Spec, operationID, serverID string) State {
	state := creatingState(spec, operationID)
	attempted := time.Now().UTC()
	until := attempted.Add(time.Minute)
	resource := &state.CreationIntent.Resources[0]
	resource.ResourceID = serverID
	resource.Phase = "ready"
	resource.AttemptID = "attempt-" + operationID
	resource.CreateAttemptedAt = &attempted
	resource.ReconcileUntil = &until
	state.CreationIntent.Phase = "ready"
	state.Phase = "ready"
	state.Resources = []Resource{{ID: serverID, Name: resource.Name, Status: "ACTIVE", Address: "192.0.2.10"}}
	return state
}

func managedTestServer(operationID, serverID string) compute.Server {
	return compute.Server{ID: serverID, Name: "canter-demo-1", Status: "ACTIVE", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": operationID, "canter.resource": "canter-demo-1"}}
}

func testClient(spec Spec) (*Client, *fakeStore, *fakeCompute, *fakeModel) {
	store := newFakeStore()
	key := stateKey(spec)
	provider := newFakeCompute(store, key)
	planner := &fakeModel{plan: testPlan(spec)}
	return &Client{model: planner, compute: provider, m1: store}, store, provider, planner
}

func TestApplyPersistsCompleteCreationIntentBeforeProviderCreate(t *testing.T) {
	spec := testSpec()
	client, _, provider, _ := testClient(spec)
	provider.assertIntentOnCreate = func(state State, request compute.ManagedServerRequest) error {
		if state.Phase != "creating" || state.CreationIntent == nil || state.CreationIntent.OperationID == "" || len(state.CreationIntent.Resources) != 1 || len(state.ProofKeys) != 1 || state.CreationIntent.Resources[0].Phase != "creating" || state.CreationIntent.Resources[0].CreateAttemptedAt == nil || state.CreationIntent.Resources[0].ReconcileUntil == nil {
			return fmt.Errorf("incomplete durable state before create: %+v", state)
		}
		if request.OperationID != state.CreationIntent.OperationID || request.Name != state.CreationIntent.Resources[0].Name || request.NetworkID != state.CreationIntent.NetworkIDs[0] {
			return fmt.Errorf("provider request does not match durable intent")
		}
		return nil
	}
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != "ready" || result.State.CreationIntent.Phase != "ready" || provider.createCalls != 1 || provider.resolveCalls != 1 {
		t.Fatalf("result=%+v create=%d resolve=%d", result.State, provider.createCalls, provider.resolveCalls)
	}
}

func TestApplyRecoversAmbiguousCreateResponseByLookup(t *testing.T) {
	spec := testSpec()
	client, _, provider, _ := testClient(spec)
	provider.createAmbiguous = true
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != "ready" || len(result.State.Resources) != 1 || provider.createCalls != 1 {
		t.Fatalf("result=%+v create=%d", result.State, provider.createCalls)
	}
}

func TestApplyWaitsForAmbiguousCreateVisibilityWithoutDuplicate(t *testing.T) {
	spec := testSpec()
	client, _, provider, _ := testClient(spec)
	provider.createAmbiguous = true
	provider.hideLookupCalls = 4
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != "ready" || provider.createCalls != 1 || provider.lookupCalls < 5 {
		t.Fatalf("result=%+v create=%d lookup=%d", result.State, provider.createCalls, provider.lookupCalls)
	}
}

func TestApplyReclaimsInFlightCreateWithLookupOnlyGrace(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-in-flight")
	attempted := time.Now().UTC()
	until := attempted.Add(time.Minute)
	state.CreationIntent.Resources[0].Phase = "creating"
	state.CreationIntent.Resources[0].AttemptID = "attempt-in-flight"
	state.CreationIntent.Resources[0].CreateAttemptedAt = &attempted
	state.CreationIntent.Resources[0].ReconcileUntil = &until
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON(context.Background(), state.ProofKeys[0], BootProof{Status: "booted", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "eventually-visible", Name: "canter-demo-1", Status: "BUILD", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": "op-in-flight", "canter.resource": "canter-demo-1"}}}
	provider.hideLookupCalls = 3
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != "ready" || provider.createCalls != 0 || result.State.Resources[0].ID != "eventually-visible" {
		t.Fatalf("result=%+v create=%d lookup=%d", result.State, provider.createCalls, provider.lookupCalls)
	}
}

func TestApplyDoesNotCreateDuringUnresolvedVisibilityGrace(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-unresolved")
	attempted := time.Now().UTC()
	until := attempted.Add(time.Minute)
	state.CreationIntent.Resources[0].Phase = "creating"
	state.CreationIntent.Resources[0].AttemptID = "attempt-unresolved"
	state.CreationIntent.Resources[0].CreateAttemptedAt = &attempted
	state.CreationIntent.Resources[0].ReconcileUntil = &until
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result, err := client.Apply(ctx, spec)
	if !errors.Is(err, context.DeadlineExceeded) || result.State.Phase != "creating" || provider.createCalls != 0 {
		t.Fatalf("result=%+v err=%v create=%d", result.State, err, provider.createCalls)
	}
}

func TestApplyEscalatesExpiredInvisibleCreateWithoutRetry(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-expired")
	attempted := time.Now().Add(-time.Minute).UTC()
	until := time.Now().Add(-time.Second).UTC()
	state.CreationIntent.Resources[0].Phase = "creating"
	state.CreationIntent.Resources[0].AttemptID = "attempt-expired"
	state.CreationIntent.Resources[0].CreateAttemptedAt = &attempted
	state.CreationIntent.Resources[0].ReconcileUntil = &until
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	result, err := client.Apply(context.Background(), spec)
	if err == nil || result.State.Phase != "escalated" || provider.createCalls != 0 {
		t.Fatalf("result=%+v err=%v create=%d", result.State, err, provider.createCalls)
	}
}

func TestApplyEscalatesAmbiguousCreateAfterShortInjectedGrace(t *testing.T) {
	spec := testSpec()
	client, _, provider, _ := testClient(spec)
	client.createReconcileGrace = 2 * time.Millisecond
	client.managedLookupInterval = time.Millisecond
	provider.createNoResourceErr = errors.New("provider response lost")
	result, err := client.Apply(context.Background(), spec)
	if err == nil || result.State.Phase != "escalated" || result.State.CreationIntent.Phase != "escalated" || provider.createCalls != 1 {
		t.Fatalf("result=%+v err=%v create=%d", result.State, err, provider.createCalls)
	}
}

func TestApplyResumesCreatingStateByLookupWithoutCreate(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-resume")
	attempted := time.Now().UTC()
	until := attempted.Add(time.Minute)
	state.CreationIntent.Resources[0].Phase = "creating"
	state.CreationIntent.Resources[0].AttemptID = "attempt-resume"
	state.CreationIntent.Resources[0].CreateAttemptedAt = &attempted
	state.CreationIntent.Resources[0].ReconcileUntil = &until
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON(context.Background(), state.ProofKeys[0], BootProof{Status: "booted", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "existing-1", Name: "canter-demo-1", Status: "BUILD", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": "op-resume", "canter.resource": "canter-demo-1"}}}
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.OperationID != "op-resume" || provider.createCalls != 0 || provider.resolveCalls != 0 || result.State.Resources[0].ID != "existing-1" {
		t.Fatalf("result=%+v create=%d resolve=%d", result, provider.createCalls, provider.resolveCalls)
	}
}

func TestApplyResumesPersistedResourceIDWithoutRecreatingDuringListLag(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-attached")
	state.CreationIntent.Resources[0].ResourceID = "existing-1"
	state.CreationIntent.Resources[0].Phase = "attached"
	state.Resources = []Resource{{ID: "existing-1", Name: "canter-demo-1", Status: "BUILD"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON(context.Background(), state.ProofKeys[0], BootProof{Status: "booted", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "existing-1", Name: "canter-demo-1", Status: "BUILD", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": "op-attached", "canter.resource": "canter-demo-1"}}}
	provider.hideLookup = true
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Phase != "ready" || provider.createCalls != 0 || result.State.Resources[0].ID != "existing-1" {
		t.Fatalf("result=%+v create=%d", result.State, provider.createCalls)
	}
}

func TestApplyEscalatesDuplicateManagedMatches(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-duplicate")
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": "op-duplicate", "canter.resource": "canter-demo-1"}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "one", Name: "canter-demo-1", Metadata: metadata}, {ID: "two", Name: "canter-demo-1", Metadata: metadata}}
	result, err := client.Apply(context.Background(), spec)
	if !compute.IsDuplicateManagedResource(err) || result.State.Phase != "escalated" || provider.createCalls != 0 {
		t.Fatalf("result=%+v err=%v create=%d", result.State, err, provider.createCalls)
	}
	var persisted State
	if getErr := store.Get(context.Background(), stateKey(spec), &persisted); getErr != nil || persisted.Phase != "escalated" || persisted.CreationIntent.Phase != "escalated" {
		t.Fatalf("persisted=%+v err=%v", persisted, getErr)
	}
}

func TestApplyDoesNotReplayReadyState(t *testing.T) {
	spec := testSpec()
	client, store, provider, planner := testClient(spec)
	state := creatingState(spec, "op-ready")
	state.Phase = "ready"
	state.CreationIntent.Phase = "ready"
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Apply(context.Background(), spec); err == nil {
		t.Fatal("expected live-state rejection")
	}
	if provider.resolveCalls != 0 || provider.createCalls != 0 || planner.compileCalls != 0 {
		t.Fatalf("ready state replayed: resolve=%d create=%d plan=%d", provider.resolveCalls, provider.createCalls, planner.compileCalls)
	}
}

func TestApplyAfterDestroyedStateStartsFreshIntentWithoutReplayingResources(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "op-destroyed")
	state.Phase = "destroyed"
	state.CreationIntent.Phase = "ready"
	state.Resources = []Resource{{ID: "old-deleted", Name: "canter-demo-1", Status: "DELETED"}}
	destroyedAt := time.Now().UTC()
	state.DestroyedAt = &destroyedAt
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "old-deleted", Name: "canter-demo-1", Status: "DELETED", Metadata: map[string]string{"canter.managed": "true", "canter.sandbox": "demo", "canter.operation": "op-destroyed", "canter.resource": "canter-demo-1"}}}
	result, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.OperationID == "op-destroyed" || len(result.State.Resources) != 1 || result.State.Resources[0].ID == "old-deleted" || result.State.DestroyedAt != nil || provider.createCalls != 1 {
		t.Fatalf("result=%+v create=%d", result, provider.createCalls)
	}
}

func TestConcurrentAbsentApplyCannotOverwriteCreationIntent(t *testing.T) {
	testConcurrentCreationIntentCAS(t, false)
}

func TestConcurrentDestroyedApplyCannotOverwriteCreationIntent(t *testing.T) {
	testConcurrentCreationIntentCAS(t, true)
}

func TestConcurrentPendingIntentHasSingleCreateAttemptOwner(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	second := &Client{model: &fakeModel{plan: testPlan(spec)}, compute: provider, m1: store}
	state := creatingState(spec, "shared-operation")
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	store.versionReadBarrier = make(chan struct{})
	store.versionReadTarget = 2
	provider.lookupBarrier = make(chan struct{})
	provider.lookupBarrierTarget = 2
	type outcome struct {
		result ApplyResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, candidate := range []*Client{client, second} {
		wait.Add(1)
		go func(candidate *Client) {
			defer wait.Done()
			result, err := candidate.Apply(context.Background(), spec)
			outcomes <- outcome{result: result, err: err}
		}(candidate)
	}
	wait.Wait()
	close(outcomes)
	succeeded, contended := 0, 0
	for outcome := range outcomes {
		if outcome.err == nil {
			succeeded++
		} else if strings.Contains(outcome.err.Error(), "concurrently claimed") || strings.Contains(outcome.err.Error(), "already has live state in phase ready") || isLifecycleFenceError(outcome.err) {
			contended++
		} else {
			t.Fatalf("unexpected Apply error: %v", outcome.err)
		}
	}
	if succeeded != 1 || contended != 1 || provider.createCalls != 1 {
		t.Fatalf("succeeded=%d contended=%d creates=%d", succeeded, contended, provider.createCalls)
	}
}

func TestDestroyEscalatesInvisibleAcceptedCreateAndLateOwnerCleansUp(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	provider.hideLookup = true
	provider.createAccepted = make(chan struct{})
	provider.createRelease = make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		_, err := client.Apply(context.Background(), spec)
		applyDone <- err
	}()
	<-provider.createAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err == nil || destroyed.Phase != "escalated" || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	close(provider.createRelease)
	if err := <-applyDone; !isLifecycleFenceError(err) {
		t.Fatalf("late apply err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "escalated" || len(provider.deletedServers) != 1 || provider.deletedServers[0] != "server-1" {
		t.Fatalf("persisted=%+v deleted=%v", persisted, provider.deletedServers)
	}
}

func TestLateCreateOwnerCannotOverwriteDestroyedState(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	provider.createAccepted = make(chan struct{})
	provider.createRelease = make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		_, err := client.Apply(context.Background(), spec)
		applyDone <- err
	}()
	<-provider.createAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err != nil || destroyed.Phase != "destroyed" {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	close(provider.createRelease)
	if err := <-applyDone; !isLifecycleFenceError(err) {
		t.Fatalf("late apply err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "destroyed" || persisted.CreationIntent.OperationID != destroyed.CreationIntent.OperationID {
		t.Fatalf("late owner resurrected state: %+v", persisted)
	}
}

func TestReplacementApplySurvivesOldOwnerReturningAfterDestroy(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	provider.createAccepted = make(chan struct{})
	provider.createRelease = make(chan struct{})
	oldApplyDone := make(chan error, 1)
	go func() {
		_, err := client.Apply(context.Background(), spec)
		oldApplyDone <- err
	}()
	<-provider.createAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err != nil || destroyed.Phase != "destroyed" {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	provider.mu.Lock()
	oldRelease := provider.createRelease
	provider.createRelease = nil
	provider.createAccepted = nil
	provider.mu.Unlock()
	replacement, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("replacement apply: %v", err)
	}
	close(oldRelease)
	if err := <-oldApplyDone; !isLifecycleFenceError(err) {
		t.Fatalf("old apply err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "ready" || persisted.CreationIntent.OperationID != replacement.OperationID || persisted.Resources[0].ID != "server-2" {
		t.Fatalf("replacement was overwritten: replacement=%+v persisted=%+v", replacement.State, persisted)
	}
}

func testConcurrentCreationIntentCAS(t *testing.T, destroyed bool) {
	t.Helper()
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	second := &Client{model: &fakeModel{plan: testPlan(spec)}, compute: provider, m1: store}
	if destroyed {
		state := creatingState(spec, "old-operation")
		state.Phase = "destroyed"
		state.CreationIntent.Phase = "ready"
		destroyedAt := time.Now().UTC()
		state.DestroyedAt = &destroyedAt
		if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
			t.Fatal(err)
		}
	}
	store.versionReadBarrier = make(chan struct{})
	store.versionReadTarget = 2
	type outcome struct {
		result ApplyResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, candidate := range []*Client{client, second} {
		wait.Add(1)
		go func(candidate *Client) {
			defer wait.Done()
			result, err := candidate.Apply(context.Background(), spec)
			outcomes <- outcome{result: result, err: err}
		}(candidate)
	}
	wait.Wait()
	close(outcomes)
	succeeded, contended := 0, 0
	var winningOperation string
	for outcome := range outcomes {
		if outcome.err == nil {
			succeeded++
			winningOperation = outcome.result.OperationID
		} else if strings.Contains(outcome.err.Error(), "concurrently claimed") {
			contended++
		} else {
			t.Fatalf("unexpected Apply error: %v", outcome.err)
		}
	}
	if succeeded != 1 || contended != 1 || provider.createCalls != 1 {
		t.Fatalf("succeeded=%d contended=%d creates=%d", succeeded, contended, provider.createCalls)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.CreationIntent == nil || persisted.CreationIntent.OperationID != winningOperation || winningOperation == "old-operation" {
		t.Fatalf("persisted=%+v winner=%s", persisted.CreationIntent, winningOperation)
	}
}

func TestCreationIntentAllowsOnlyPendingExhaustedNetworkCursor(t *testing.T) {
	spec := testSpec()
	state := creatingState(spec, "op-exhausted")
	state.CreationIntent.Resources[0].NetworkIndex = len(state.CreationIntent.NetworkIDs)
	state.CreationIntent.Resources[0].Phase = "pending"
	if err := validateCreationIntent(spec, state); err != nil {
		t.Fatalf("persisted exhausted cursor should remain inspectable: %v", err)
	}
	state.CreationIntent.Resources[0].Phase = "creating"
	if err := validateCreationIntent(spec, state); err == nil {
		t.Fatal("creating resource cannot point beyond all network candidates")
	}
}

func testSystem() System {
	return System{APIVersion: APIVersion, Kind: "System", Metadata: Metadata{Name: "demo"}, Spec: SystemContract{
		Intent: "test durable endpoint", Constraints: Constraints{Host: HostConstraint{Class: "c1", Count: 1, MemoryMiB: 1024, SystemReserve: 512}}, M1: M1Spec{Prefix: "systems/demo"},
		Services: []SystemService{{Name: "web", Kind: "service", Engine: "http", Isolation: "process", Instances: 1, Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}, Networking: "public"}},
	}}
}

func TestExposureIntentIsDurableBeforeProviderAndReadyIsNotReplayed(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.assertExposure = func(persisted State) error {
		if persisted.ExposureIntent == nil || persisted.ExposureIntent.Phase != "creating" || persisted.ExposureIntent.ServerID != "server-1" || persisted.ExposureIntent.Port != 8080 || persisted.ExposureIntent.Ownership == "" {
			return fmt.Errorf("exposure intent not durable before provider call: %+v", persisted.ExposureIntent)
		}
		return nil
	}
	result := ApplyResult{OperationID: "create-op", State: state, ReceiptKey: "receipt"}
	if err := client.exposeSystemHost(context.Background(), system, &result); err != nil {
		t.Fatal(err)
	}
	if result.State.ExposureIntent.Phase != "ready" || len(result.State.NetworkPolicies) != 1 || provider.exposeCalls != 1 {
		t.Fatalf("state=%+v expose=%d", result.State, provider.exposeCalls)
	}
	if err := client.exposeSystemHost(context.Background(), system, &result); err != nil {
		t.Fatal(err)
	}
	if provider.exposeCalls != 1 {
		t.Fatalf("ready exposure replayed: calls=%d", provider.exposeCalls)
	}
}

func TestExposureRetryReconcilesPersistedIntent(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.exposeErrors = []error{errors.New("provider response lost"), nil}
	provider.exposeErrorAfterPolicy = true
	first := ApplyResult{State: state}
	if err := client.exposeSystemHost(context.Background(), system, &first); err == nil {
		t.Fatal("expected interrupted exposure")
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ExposureIntent == nil || persisted.ExposureIntent.Phase != "creating" {
		t.Fatalf("persisted=%+v", persisted)
	}
	second := ApplyResult{State: persisted}
	if err := client.exposeSystemHost(context.Background(), system, &second); err != nil {
		t.Fatal(err)
	}
	if provider.exposeCalls != 1 || provider.findExposureCalls != 1 || second.State.ExposureIntent.OperationID != persisted.ExposureIntent.OperationID || second.State.ExposureIntent.Phase != "ready" {
		t.Fatalf("state=%+v expose=%d find=%d", second.State, provider.exposeCalls, provider.findExposureCalls)
	}
}

func TestExposureEscalatesDuplicateProviderState(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.exposeErrors = []error{&compute.DuplicateManagedResourceError{Kind: "network policy", Identity: "canter-demo", Count: 2}}
	result := ApplyResult{State: state}
	err := client.exposeSystemHost(context.Background(), system, &result)
	if !compute.IsDuplicateManagedResource(err) || result.State.Phase != "escalated" || result.State.ExposureIntent.Phase != "escalated" {
		t.Fatalf("state=%+v err=%v", result.State, err)
	}
	var persisted State
	if getErr := store.Get(context.Background(), stateKey(spec), &persisted); getErr != nil || persisted.Phase != "escalated" || persisted.ExposureIntent.Phase != "escalated" {
		t.Fatalf("persisted=%+v err=%v", persisted, getErr)
	}
}

func TestEndpointPolicyIdentityIsCanonicalNamespaceScoped(t *testing.T) {
	first := testSystem()
	second := testSystem()
	second.Spec.M1.Prefix = "workspaces/other/demo/"
	firstName, firstOwner := systemEndpointPolicyIdentity(first)
	secondName, secondOwner := systemEndpointPolicyIdentity(second)
	if firstName == secondName || firstOwner == secondOwner || !strings.Contains(firstName, "canter-demo-") || !strings.HasPrefix(firstOwner, "sha256:") {
		t.Fatalf("first=%q/%q second=%q/%q", firstName, firstOwner, secondName, secondOwner)
	}
	first.Spec.M1.Prefix += "/"
	canonicalName, canonicalOwner := systemEndpointPolicyIdentity(first)
	if canonicalName != firstName || canonicalOwner != firstOwner {
		t.Fatalf("trailing slash changed canonical identity: %q/%q", canonicalName, canonicalOwner)
	}
}

func TestConcurrentExposureAttemptHasSingleCreateCapableOwner(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	second := &Client{model: &fakeModel{plan: testPlan(spec)}, compute: provider, m1: store}
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	store.versionReadBarrier = make(chan struct{})
	store.versionReadTarget = 2
	errorsOut := make(chan error, 2)
	var wait sync.WaitGroup
	for _, candidate := range []*Client{client, second} {
		wait.Add(1)
		go func(candidate *Client) {
			defer wait.Done()
			result := ApplyResult{State: state}
			errorsOut <- candidate.exposeSystemHost(context.Background(), system, &result)
		}(candidate)
	}
	wait.Wait()
	close(errorsOut)
	succeeded, contended := 0, 0
	for err := range errorsOut {
		if err == nil {
			succeeded++
		} else if strings.Contains(err.Error(), "concurrently claimed") {
			contended++
		} else {
			t.Fatalf("unexpected exposure error: %v", err)
		}
	}
	if succeeded != 1 || contended != 1 || provider.exposeCalls != 1 {
		t.Fatalf("succeeded=%d contended=%d expose=%d", succeeded, contended, provider.exposeCalls)
	}
}

func TestExposureCompletionCannotOverwriteDestroyedState(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := readyState(spec, "create-ready", "server-1")
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("create-ready", "server-1")}
	provider.exposeAccepted = make(chan struct{})
	provider.exposeRelease = make(chan struct{})
	exposeDone := make(chan error, 1)
	go func() {
		result := ApplyResult{State: state}
		exposeDone <- client.exposeSystemHost(context.Background(), system, &result)
	}()
	<-provider.exposeAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err != nil || destroyed.Phase != "destroyed" {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	close(provider.exposeRelease)
	if err := <-exposeDone; !isLifecycleFenceError(err) {
		t.Fatalf("late exposure err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "destroyed" || len(persisted.NetworkPolicies) != 0 || len(provider.deletedPolicies) < 1 {
		t.Fatalf("late exposure resurrected state: persisted=%+v deleted=%v", persisted, provider.deletedPolicies)
	}
}

func TestDestroyEscalatesInvisibleInFlightExposureAndLateOwnerCleansUp(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := readyState(spec, "create-ready", "server-1")
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("create-ready", "server-1")}
	provider.hideExposureLookup = true
	provider.exposeAccepted = make(chan struct{})
	provider.exposeRelease = make(chan struct{})
	exposeDone := make(chan error, 1)
	go func() {
		result := ApplyResult{State: state}
		exposeDone <- client.exposeSystemHost(context.Background(), system, &result)
	}()
	<-provider.exposeAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err == nil || destroyed.Phase != "escalated" || !strings.Contains(err.Error(), "still in flight") {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	retried, retryErr := client.Destroy(context.Background(), spec)
	if retryErr == nil || retried.Phase != "escalated" || !strings.Contains(retryErr.Error(), "still in flight") {
		t.Fatalf("retry destroyed=%+v err=%v", retried, retryErr)
	}
	close(provider.exposeRelease)
	if err := <-exposeDone; !isLifecycleFenceError(err) {
		t.Fatalf("late exposure err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "escalated" || persisted.ExposureIntent.MutationUnresolved || len(provider.deletedPolicies) != 1 || provider.deletedPolicies[0] != "policy-1" {
		t.Fatalf("persisted=%+v deleted=%v", persisted, provider.deletedPolicies)
	}
	final, finalErr := client.Destroy(context.Background(), spec)
	if finalErr != nil || final.Phase != "destroyed" {
		t.Fatalf("final destroy=%+v err=%v", final, finalErr)
	}
}

func TestOldExposureOwnerCannotDeleteReplacementAdoptedPolicy(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := readyState(spec, "old-create", "old-server")
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("old-create", "old-server")}
	provider.exposeAccepted = make(chan struct{})
	provider.exposeRelease = make(chan struct{})
	oldExposeDone := make(chan error, 1)
	go func() {
		result := ApplyResult{State: state}
		oldExposeDone <- client.exposeSystemHost(context.Background(), system, &result)
	}()
	<-provider.exposeAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err != nil || destroyed.Phase != "destroyed" {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	provider.mu.Lock()
	oldRelease := provider.exposeRelease
	provider.exposeRelease = nil
	provider.exposeAccepted = nil
	provider.mu.Unlock()
	replacement, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("replacement apply: %v", err)
	}
	replacementExposure := ApplyResult{State: replacement.State}
	if err := client.exposeSystemHost(context.Background(), system, &replacementExposure); err != nil {
		t.Fatalf("replacement exposure: %v", err)
	}
	newExposureOperation := replacementExposure.State.ExposureIntent.OperationID
	close(oldRelease)
	if err := <-oldExposeDone; !isLifecycleFenceError(err) {
		t.Fatalf("old exposure err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	policyStillExists := provider.exposurePolicy != nil && provider.exposurePolicy.ID == "policy-1"
	deletedPolicies := append([]string(nil), provider.deletedPolicies...)
	provider.mu.Unlock()
	if persisted.Phase != "ready" || persisted.ExposureIntent.OperationID != newExposureOperation || !policyStillExists || len(deletedPolicies) != 1 {
		t.Fatalf("replacement policy was affected: persisted=%+v policyExists=%t deleted=%v", persisted, policyStillExists, deletedPolicies)
	}
}

func TestDestroyEscalatesAttachedResourceIDWhenProviderVisibilityIsAmbiguous(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "attached-operation")
	state.CreationIntent.Resources[0].AttemptID = "attached-attempt"
	state.CreationIntent.Resources[0].ResourceID = "attached-server"
	state.CreationIntent.Resources[0].Phase = "attached"
	state.Resources = []Resource{{ID: "attached-server", Name: "canter-demo-1", Status: "BUILD"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("attached-operation", "attached-server")}
	provider.hideLookup = true
	provider.hideServer = true

	destroyed, err := client.Destroy(context.Background(), spec)
	if err == nil || destroyed.Phase != "escalated" || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	retried, retryErr := client.Destroy(context.Background(), spec)
	if retryErr == nil || retried.Phase != "escalated" {
		t.Fatalf("retry destroyed=%+v err=%v", retried, retryErr)
	}
	if len(provider.deletedServers) != 0 {
		t.Fatalf("ambiguous server was treated as absent: deleted=%v", provider.deletedServers)
	}
}

func TestLateWaitActiveCannotDeleteOrOverwriteReplacement(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "old-attached")
	state.CreationIntent.Resources[0].AttemptID = "old-attempt"
	state.CreationIntent.Resources[0].ResourceID = "old-server"
	state.CreationIntent.Resources[0].Phase = "attached"
	state.Resources = []Resource{{ID: "old-server", Name: "canter-demo-1", Status: "BUILD"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON(context.Background(), state.ProofKeys[0], BootProof{Status: "booted", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("old-attached", "old-server")}
	provider.waitActiveAccepted = make(chan struct{})
	provider.waitActiveRelease = make(chan struct{})
	oldApplyDone := make(chan error, 1)
	go func() {
		_, err := client.Apply(context.Background(), spec)
		oldApplyDone <- err
	}()
	<-provider.waitActiveAccepted

	destroyed, err := client.Destroy(context.Background(), spec)
	if err != nil || destroyed.Phase != "destroyed" {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	provider.mu.Lock()
	oldRelease := provider.waitActiveRelease
	provider.waitActiveRelease = nil
	provider.waitActiveAccepted = nil
	provider.mu.Unlock()
	replacement, err := client.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("replacement apply: %v", err)
	}
	close(oldRelease)
	if err := <-oldApplyDone; !isLifecycleFenceError(err) {
		t.Fatalf("old apply err=%v", err)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "ready" || persisted.CreationIntent.OperationID != replacement.OperationID || persisted.Resources[0].ID != "server-1" {
		t.Fatalf("replacement was affected: replacement=%+v persisted=%+v", replacement.State, persisted)
	}
	for _, deleted := range provider.deletedServers {
		if deleted == "server-1" {
			t.Fatalf("late old owner deleted replacement server: %v", provider.deletedServers)
		}
	}
}

func TestLateWaitActiveCleansUnresolvedOldServerBeforeDestroyRetry(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	state := creatingState(spec, "old-ambiguous")
	state.CreationIntent.Resources[0].AttemptID = "old-ambiguous-attempt"
	state.CreationIntent.Resources[0].ResourceID = "old-ambiguous-server"
	state.CreationIntent.Resources[0].Phase = "attached"
	state.Resources = []Resource{{ID: "old-ambiguous-server", Name: "canter-demo-1", Status: "BUILD"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("old-ambiguous", "old-ambiguous-server")}
	provider.waitActiveAccepted = make(chan struct{})
	provider.waitActiveRelease = make(chan struct{})
	oldApplyDone := make(chan error, 1)
	go func() {
		_, err := client.Apply(context.Background(), spec)
		oldApplyDone <- err
	}()
	<-provider.waitActiveAccepted
	provider.mu.Lock()
	provider.hideLookup = true
	provider.hideServer = true
	provider.mu.Unlock()

	destroyed, err := client.Destroy(context.Background(), spec)
	if err == nil || destroyed.Phase != "escalated" || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("destroyed=%+v err=%v", destroyed, err)
	}
	close(provider.waitActiveRelease)
	if err := <-oldApplyDone; !isLifecycleFenceError(err) {
		t.Fatalf("old apply err=%v", err)
	}
	provider.mu.Lock()
	provider.hideLookup = false
	provider.hideServer = false
	deleted := append([]string(nil), provider.deletedServers...)
	provider.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "old-ambiguous-server" {
		t.Fatalf("late active cleanup deleted=%v", deleted)
	}
	final, finalErr := client.Destroy(context.Background(), spec)
	if finalErr != nil || final.Phase != "destroyed" {
		t.Fatalf("final destroy=%+v err=%v", final, finalErr)
	}
}

func TestInFlightExposureRetryIsLookupOnlyAndRecoversExactPolicy(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	name, ownership := systemEndpointPolicyIdentity(system)
	now := time.Now().UTC()
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}, ExposureIntent: &ExposureIntent{OperationID: "expose-crash", ServerID: "server-1", Name: name, Ownership: ownership, Protocol: "tcp", Port: 8080, Phase: "creating", CreatedAt: now, AttemptedAt: &now}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	policy := compute.SecurityPolicy{ID: "recovered", PortID: "port-1", RuleID: "rule-1", Port: 8080}
	provider.exposurePolicy = &policy
	result := ApplyResult{State: state}
	if err := client.exposeSystemHost(context.Background(), system, &result); err != nil {
		t.Fatal(err)
	}
	if provider.exposeCalls != 0 || provider.findExposureCalls != 1 || result.State.ExposureIntent.Phase != "ready" || len(result.State.NetworkPolicies) != 1 || result.State.NetworkPolicies[0].ID != "recovered" {
		t.Fatalf("state=%+v expose=%d find=%d", result.State, provider.exposeCalls, provider.findExposureCalls)
	}
}

func TestInFlightExposureRetryEscalatesWhenExactPolicyInvisible(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	name, ownership := systemEndpointPolicyIdentity(system)
	now := time.Now().UTC()
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}, ExposureIntent: &ExposureIntent{OperationID: "expose-crash", ServerID: "server-1", Name: name, Ownership: ownership, Protocol: "tcp", Port: 8080, Phase: "creating", CreatedAt: now, AttemptedAt: &now}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	result := ApplyResult{State: state}
	err := client.exposeSystemHost(context.Background(), system, &result)
	if !compute.IsAmbiguousManagedResource(err) || result.State.Phase != "escalated" || provider.exposeCalls != 0 || provider.findExposureCalls != 1 {
		t.Fatalf("state=%+v err=%v expose=%d find=%d", result.State, err, provider.exposeCalls, provider.findExposureCalls)
	}
}

func TestExposureTerminalAmbiguityCannotRetryCreateCapableEnsure(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.exposeErrors = []error{&compute.AmbiguousManagedResourceError{Kind: "network policy", Identity: "scoped", Cause: errors.New("response lost")}}
	first := ApplyResult{State: state}
	if err := client.exposeSystemHost(context.Background(), system, &first); !compute.IsAmbiguousManagedResource(err) {
		t.Fatalf("first err=%v", err)
	}
	second := ApplyResult{State: first.State}
	if err := client.exposeSystemHost(context.Background(), system, &second); err == nil {
		t.Fatal("terminal exposure unexpectedly retried")
	}
	if provider.exposeCalls != 1 {
		t.Fatalf("create-capable exposure calls=%d", provider.exposeCalls)
	}
}

func TestExplicitExposureRecoveryFinishesMissingExactPolicy(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.exposeErrors = []error{&compute.AmbiguousManagedResourceError{Kind: "network policy attachment", Identity: "scoped", Cause: errors.New("response lost")}}
	first := ApplyResult{State: state}
	if err := client.exposeSystemHost(context.Background(), system, &first); !compute.IsAmbiguousManagedResource(err) {
		t.Fatalf("first err=%v", err)
	}
	recovered, err := client.RecoverSystemHostExposure(context.Background(), system)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != "ready" || recovered.ExposureIntent == nil || recovered.ExposureIntent.Phase != "ready" || recovered.ExposureIntent.MutationUnresolved || len(recovered.NetworkPolicies) != 1 {
		t.Fatalf("recovered=%+v", recovered)
	}
	if provider.findExposureCalls != 1 || provider.exposeCalls != 2 {
		t.Fatalf("find=%d expose=%d", provider.findExposureCalls, provider.exposeCalls)
	}
}

func TestExplicitExposureRecoveryAdoptsVisibleExactPolicyWithoutMutation(t *testing.T) {
	system := testSystem()
	spec := SystemHostSpec(system, "true")
	client, store, provider, _ := testClient(spec)
	state := State{Sandbox: "demo", Phase: "ready", Resources: []Resource{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}}
	provider.servers["canter-demo-1"] = []compute.Server{{ID: "server-1", Name: "canter-demo-1", Status: "ACTIVE"}}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	provider.exposeErrorAfterPolicy = true
	provider.exposeErrors = []error{&compute.AmbiguousManagedResourceError{Kind: "network policy attachment", Identity: "scoped", Cause: errors.New("response lost")}}
	first := ApplyResult{State: state}
	if err := client.exposeSystemHost(context.Background(), system, &first); !compute.IsAmbiguousManagedResource(err) {
		t.Fatalf("first err=%v", err)
	}
	recovered, err := client.RecoverSystemHostExposure(context.Background(), system)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != "ready" || len(recovered.NetworkPolicies) != 1 || recovered.NetworkPolicies[0].ID != "policy-1" {
		t.Fatalf("recovered=%+v", recovered)
	}
	if provider.findExposureCalls != 1 || provider.exposeCalls != 1 {
		t.Fatalf("find=%d expose=%d", provider.findExposureCalls, provider.exposeCalls)
	}
}
