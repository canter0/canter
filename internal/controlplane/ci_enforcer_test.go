package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

// fakeCICompute is an in-memory provider fixture. Every retained or
// discovered server — including 404 ones — gets a complete non-nil port
// list with independent direct read statuses.
type fakeCICompute struct {
	mu           sync.Mutex
	servers      map[string]compute.Server
	resources    map[string]compute.CIRunnerResource
	reads        map[string]int
	ports        map[string][]string
	observeErr   error
	deleteErr    error
	stopCalls    int32
	deleteCalls  int32
	observeCalls int32
	lateServer   *compute.Server // materializes on next observe when set
}

func newFakeCICompute() *fakeCICompute {
	return &fakeCICompute{
		servers: map[string]compute.Server{}, resources: map[string]compute.CIRunnerResource{},
		reads: map[string]int{}, ports: map[string][]string{},
	}
}

func (f *fakeCICompute) ValidateCIRunnerScope(scope compute.CIRunnerScope) error {
	if scope.Region != "r1" {
		return compute.ErrCIRunnerOwnership
	}
	return nil
}

func (f *fakeCICompute) addServer(scope compute.CIRunnerScope, id string, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[id] = compute.Server{ID: id, Name: scope.ServerName, Status: state, Metadata: map[string]string{
		"canter.managed": "true", "canter.sandbox": "ci-" + scope.OperationID,
		"canter.operation": scope.OperationID, "canter.resource": scope.ServerName,
	}}
	f.resources["server/"+id] = compute.CIRunnerResource{Kind: "server", ID: id, OperationID: scope.OperationID}
	f.reads["server/"+id] = 200
	f.ports[id] = []string{}
}

func (f *fakeCICompute) removeServer(id string) {
	delete(f.servers, id)
	delete(f.resources, "server/"+id)
	delete(f.ports, id)
	f.reads["server/"+id] = 404
}

func (f *fakeCICompute) ObserveCIRunner(ctx context.Context, scope compute.CIRunnerScope, retained []compute.CIRunnerResource) (compute.CIRunnerObservation, error) {
	atomic.AddInt32(&f.observeCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lateServer != nil {
		s := *f.lateServer
		f.servers[s.ID] = s
		f.resources["server/"+s.ID] = compute.CIRunnerResource{Kind: "server", ID: s.ID, OperationID: scope.OperationID}
		f.reads["server/"+s.ID] = 200
		f.ports[s.ID] = []string{}
		f.lateServer = nil
	}
	if f.observeErr != nil {
		return compute.CIRunnerObservation{}, f.observeErr
	}
	obs := compute.CIRunnerObservation{DiscoveryComplete: true}
	known := map[string]bool{}
	for _, r := range retained {
		known[r.Kind+"/"+r.ID] = true
	}
	for k, r := range f.resources {
		if r.OperationID == scope.OperationID && !known[k] {
			known[k] = true
			obs.Discovered = append(obs.Discovered, r)
		}
	}
	serverIDs := map[string]bool{}
	for k := range known {
		parts := strings.SplitN(k, "/", 2)
		status := f.reads[k]
		if status == 0 {
			status = 404
		}
		obs.Reads = append(obs.Reads, compute.CIRunnerRead{
			Resource:   compute.CIRunnerResource{Kind: parts[0], ID: parts[1], OperationID: scope.OperationID},
			HTTPStatus: status,
			ServerState: func() string {
				if parts[0] == "server" {
					if s, ok := f.servers[parts[1]]; ok {
						return s.Status
					}
				}
				return ""
			}(),
		})
		if parts[0] == "server" {
			serverIDs[parts[1]] = true
		}
	}
	// Every known server — live or 404 — gets a complete non-nil port list.
	for id := range serverIDs {
		obs.ServerPorts = append(obs.ServerPorts, compute.CIRunnerServerPorts{
			ServerID: id, Complete: true, PortIDs: append([]string{}, f.ports[id]...),
		})
	}
	return obs, nil
}

func (f *fakeCICompute) StopCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error {
	atomic.AddInt32(&f.stopCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range resources {
		if r.Kind == "server" {
			if s, ok := f.servers[r.ID]; ok {
				s.Status = "SHUTOFF"
				f.servers[r.ID] = s
			}
		}
	}
	return nil
}

func (f *fakeCICompute) DeleteCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error {
	atomic.AddInt32(&f.deleteCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for _, r := range resources {
		if r.Kind == "server" {
			f.removeServer(r.ID)
		}
	}
	return nil
}

func (f *fakeCICompute) Server(ctx context.Context, id string) (compute.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return compute.Server{}, &compute.HTTPError{StatusCode: 404}
}

// fakeClock drives the engine's wall and monotonic clocks independently.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Time
}

func newFakeClock() *fakeClock {
	n := time.Now().UTC()
	return &fakeClock{wall: n, mono: n}
}

func (c *fakeClock) pair() (func() time.Time, func() time.Time) {
	return func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.wall
		}, func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.mono
		}
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.mono = c.mono.Add(d)
}

func (c *fakeClock) advanceMono(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono = c.mono.Add(d)
}

func (c *fakeClock) rollbackWall(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(-d)
}

func enforcerTestSpecAt(opID string, wall time.Time, setupIn, hardIn time.Duration) ciEnforcerSpec {
	lease, err := ciEnforcerNewLeaseID()
	if err != nil {
		panic(err)
	}
	return ciEnforcerSpec{
		Plan: ciRunnerLifecyclePlan{
			OperationID: opID, ServerName: ciEnforcerRunnerName(opID), Region: "r1",
			FlavorID: "f1", ImageID: "i1", NetworkID: "n1", EnforcerLease: lease,
			SetupDeadlineAt: wall.Add(setupIn), HardDeadlineAt: wall.Add(hardIn),
		},
		GitHubInstallationID: 42, GitHubRepositoryID: 777, SizeID: "linux-x64-2",
	}
}

func enforcerTestSpec(opID string, setupIn, hardIn time.Duration) ciEnforcerSpec {
	return enforcerTestSpecAt(opID, time.Now().UTC(), setupIn, hardIn)
}

func newTestEngineAt(t *testing.T, computeClient ciEnforcerCompute, github *ciGitHubClient, clk *fakeClock) (*ciEnforcerEngine, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wall, mono := clk.pair()
	e, err := newCIEnforcerEngine(dir, computeClient, github, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.close)
	return e, dir
}

func newTestEngine(t *testing.T, computeClient ciEnforcerCompute, github *ciGitHubClient) (*ciEnforcerEngine, string) {
	return newTestEngineAt(t, computeClient, github, newFakeClock())
}

// putAndCreate drives put + owned close-creation so the lease has a created
// VM in its retained inventory.
func putAndCreate(t *testing.T, e *ciEnforcerEngine, fc *fakeCICompute, spec ciEnforcerSpec, serverID string) {
	t.Helper()
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatalf("putLease: %v", err)
	}
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, serverID, "ACTIVE")
	if _, err := e.closeCreation(context.Background(), spec.Plan.EnforcerLease, "created", serverID); err != nil {
		t.Fatalf("closeCreation: %v", err)
	}
}

// driveCleanup runs physical then GitHub passes until tombstone or pass cap.
func driveCleanup(t *testing.T, e *ciEnforcerEngine, leaseID string, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		e.reconcilePhysical(context.Background(), leaseID)
		e.reconcileGitHub(context.Background(), leaseID)
	}
	e.reconcilePhysical(context.Background(), leaseID)
}

func leaseState(t *testing.T, e *ciEnforcerEngine, leaseID string) *ciEnforcerLease {
	t.Helper()
	st, err := e.stateFor(leaseID)
	if err != nil || st == nil {
		t.Fatalf("stateFor: %v", err)
	}
	return st.snapshot()
}

func TestCIEnforcerLeaseLifecycleAndDeadlines(t *testing.T) {
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, newFakeCICompute(), nil, clk)
	spec := enforcerTestSpecAt("op-lease-1", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease

	l, err := e.putLease(leaseID, spec)
	if err != nil {
		t.Fatalf("putLease: %v", err)
	}
	if l.DeadlineUnixMs != spec.Plan.SetupDeadlineAt.UnixMilli() {
		t.Fatalf("initial cutoff must be setup deadline")
	}
	// Exact retry is idempotent even before expiry.
	if _, err = e.putLease(leaseID, spec); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	// Drifted spec conflicts.
	bad := spec
	bad.GitHubRepositoryID = 999
	if _, err = e.putLease(leaseID, bad); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("drifted spec must conflict, got %v", err)
	}
	// A different lease for the same operation conflicts.
	other := spec
	otherLease, _ := ciEnforcerNewLeaseID()
	other.Plan.EnforcerLease = otherLease
	if _, err = e.putLease(otherLease, other); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("second lease for same operation must conflict")
	}
	// Approve before creation/registration readiness must conflict.
	if _, err = e.approve(leaseID, clk.wall.Add(3600*time.Second).UnixMilli()); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("approve before readiness must conflict, got %v", err)
	}
	// Stop is durable and irrevocable; exact PUT retry after expiry stays a
	// no-op rather than re-validating current time.
	if _, err = e.stop(leaseID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err = e.approve(leaseID, clk.wall.Add(3600*time.Second).UnixMilli()); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("approval after stop must conflict")
	}
	clk.advance(900 * time.Second)
	if _, err = e.putLease(leaseID, spec); err != nil {
		t.Fatalf("exact PUT retry after expiry must remain a no-op: %v", err)
	}
}

func TestCIEnforcerExpiredLeaseCleanupAfterRestart(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	spec := enforcerTestSpecAt("op-expired", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, "srv-expired-1", "ACTIVE")

	e, dir := newTestEngineAt(t, fc, nil, clk)
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	// Simulate control-plane death: rebuild on the same dir with the clock
	// advanced past the setup deadline; the window is never renewed.
	e.close()
	clk.advance(400 * time.Second)
	wall, mono := clk.pair()
	e2, err := newCIEnforcerEngine(dir, fc, nil, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	st, err := e2.stateFor(spec.Plan.EnforcerLease)
	if err != nil || st == nil {
		t.Fatalf("lease reload: %v", err)
	}
	l := st.snapshot()
	w2, m2 := e2.now()
	if !l.expired(w2, m2) {
		t.Fatalf("reloaded lease must be expired")
	}
	e2.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("expired lease must delete owned VM")
	}
	l = st.snapshot()
	if !l.StopRequested {
		t.Fatalf("expiry must persist irrevocable stop")
	}
}

func TestCIEnforcerJournalGuards(t *testing.T) {
	e, dir := newTestEngine(t, newFakeCICompute(), nil)
	defer e.close()
	// Private state dir enforced (non-0700 refused).
	bad := t.TempDir()
	if err := os.Chmod(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newCIEnforcerEngine(bad, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatalf("non-private state dir must be refused")
	}
	// Corrupt lease file refuses startup in an ISOLATED dir (first engine's
	// flock must not mask the corrupt-state failure).
	dir2 := t.TempDir()
	os.Chmod(dir2, 0o700)
	badLease := strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(dir2, "lease-"+badLease+".json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCIEnforcerEngine(dir2, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatalf("corrupt lease state must refuse startup")
	}
	// Symlinked lease file refused.
	dir3 := t.TempDir()
	os.Chmod(dir3, 0o700)
	target := filepath.Join(dir3, "target.json")
	os.WriteFile(target, []byte("{}"), 0o600)
	os.Symlink(target, filepath.Join(dir3, "lease-"+strings.Repeat("c", 64)+".json"))
	if _, err := newCIEnforcerEngine(dir3, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatalf("symlinked lease state must refuse startup")
	}
	// Wrong mode refused.
	dir4 := t.TempDir()
	os.Chmod(dir4, 0o700)
	leaseD := strings.Repeat("d", 64)
	os.WriteFile(filepath.Join(dir4, "lease-"+leaseD+".json"), []byte("{}"), 0o644)
	if _, err := newCIEnforcerEngine(dir4, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatalf("wrong-mode lease state must refuse startup")
	}
	// Hardlinked VALID durable record refused (nlink > 1).
	dir5 := t.TempDir()
	os.Chmod(dir5, 0o700)
	e5, err := newCIEnforcerEngine(dir5, newFakeCICompute(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec5 := enforcerTestSpec("op-hardlink", 800*time.Second, 20000*time.Second)
	if _, err = e5.putLease(spec5.Plan.EnforcerLease, spec5); err != nil {
		t.Fatal(err)
	}
	e5.close()
	lp := filepath.Join(dir5, "lease-"+spec5.Plan.EnforcerLease+".json")
	if err = os.Link(lp, filepath.Join(dir5, "alias.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = newCIEnforcerEngine(dir5, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatalf("hardlinked lease state must refuse startup")
	}
	// Oversized lease file refused before read.
	dir6 := t.TempDir()
	os.Chmod(dir6, 0o700)
	leaseF := strings.Repeat("f", 64)
	os.WriteFile(filepath.Join(dir6, "lease-"+leaseF+".json"), make([]byte, ciEnforcerStateMax+1), 0o600)
	if _, err := newCIEnforcerEngine(dir6, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatalf("oversized lease state must refuse startup")
	}
	dir7 := t.TempDir()
	if err := os.Chmod(dir7, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir7, "lease-"+strings.Repeat("a", 64)+".json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newCIEnforcerEngine(dir7, newFakeCICompute(), nil, nil, nil); err == nil {
		t.Fatal("FIFO lease state must refuse startup without waiting for a writer")
	}
	_ = dir
}

func TestCIEnforcerCloseCreationAndAmbiguous(t *testing.T) {
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, nil)
	spec := enforcerTestSpec("op-create", 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	if _, err := e.putLease(leaseID, spec); err != nil {
		t.Fatal(err)
	}
	// Rejected must not accept a server ID.
	if _, err := e.closeCreation(context.Background(), leaseID, "rejected", "srv-x"); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("rejected close with serverID must conflict")
	}
	// Unknown server ID cannot close creation.
	if _, err := e.closeCreation(context.Background(), leaseID, "created", "ghost-1"); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("unowned created close must conflict, got %v", err)
	}
	// Owned server closes creation.
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, "srv-real-1", "BUILD")
	st, err := e.closeCreation(context.Background(), leaseID, "created", "srv-real-1")
	if err != nil {
		t.Fatalf("closeCreation: %v", err)
	}
	if !st.CreateSettled {
		t.Fatalf("creation must be settled")
	}
	if _, err = e.closeCreation(context.Background(), leaseID, "created", "srv-real-1"); err != nil {
		t.Fatalf("exact close retry: %v", err)
	}
	if _, err = e.closeCreation(context.Background(), leaseID, "created", "srv-other"); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("mismatched close retry must conflict")
	}
}

// gitHubFixture is a scripted GitHub API fixture emitting literal wire JSON
// (label objects, not production DTOs).
type gitHubFixture struct {
	t              *testing.T
	mu             sync.Mutex
	runners        map[int64]map[string]any
	nextID         int64
	jitCalls       int32
	jitBlock       chan struct{}
	jitErr         int
	tokenErr       int
	deleteKeep     bool // DELETE ACKs but runner stays (tests absence proof)
	deleteCalls    int32
	totalOverride  *int64
	truncated      bool
	dupeID         bool
	lastPerms      map[string]string
	listBlock      chan struct{} // when non-nil, runner LIST blocks until closed
	listCalls      int32
	jitMutate      func(map[string]any) // mutates runner object in JIT response
	jitEncoded     string               // override encoded_jit_config when non-empty
	installationID int64
	repoID         int64
	repoOwner      string
	repoName       string
}

func ghRunnerJSON(id int64, name string, labels []string) map[string]any {
	objs := make([]map[string]any, 0, len(labels))
	for i, l := range labels {
		objs = append(objs, map[string]any{"id": int64(i + 1), "name": l, "type": "custom"})
	}
	return map[string]any{"id": id, "name": name, "status": "offline", "busy": false, "labels": objs}
}

func newGitHubFixture(t *testing.T) (*gitHubFixture, *httptest.Server, *ciGitHubClient) {
	t.Helper()
	return newGitHubFixtureRepo(t, 42, 777, "octo", "repo")
}

// newGitHubFixtureRepo is the parameterized fixture: installation, repository
// and canonical repo path are explicit so control-plane fixtures can pin the
// same numeric identities the durable admission scope records.
func newGitHubFixtureRepo(t *testing.T, installationID, repoID int64, owner, name string) (*gitHubFixture, *httptest.Server, *ciGitHubClient) {
	t.Helper()
	f := &gitHubFixture{t: t, runners: map[int64]map[string]any{}, nextID: 5000,
		installationID: installationID, repoID: repoID, repoOwner: owner, repoName: name}
	f.jitBlock = make(chan struct{})
	close(f.jitBlock)
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/app/installations/%d/access_tokens", installationID), func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastPerms = body.Permissions
		f.mu.Unlock()
		if f.tokenErr > 0 {
			w.WriteHeader(f.tokenErr)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "runner-token", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc(fmt.Sprintf("/repositories/%d", repoID), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": repoID, "full_name": owner + "/" + name, "private": true})
	})
	jitPath := fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig", owner, name)
	listPath := fmt.Sprintf("/repos/%s/%s/actions/runners", owner, name)
	runnerPrefix := fmt.Sprintf("/repos/%s/%s/actions/runners/", owner, name)
	mux.HandleFunc(jitPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.jitCalls, 1)
		f.mu.Lock()
		block := f.jitBlock
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.jitErr > 0 {
			w.WriteHeader(f.jitErr)
			return
		}
		var body struct {
			Name          string   `json:"name"`
			RunnerGroupID int64    `json:"runner_group_id"`
			WorkFolder    string   `json:"work_folder"`
			Labels        []string `json:"labels"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.RunnerGroupID != 1 || body.WorkFolder != "_work" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := f.nextID
		f.nextID++
		f.runners[id] = ghRunnerJSON(id, body.Name, body.Labels)
		respRunner := ghRunnerJSON(id, body.Name, body.Labels)
		if f.jitMutate != nil {
			f.jitMutate(respRunner)
		}
		encoded := base64.StdEncoding.EncodeToString([]byte("jit-config"))
		if f.jitEncoded != "" {
			encoded = f.jitEncoded
		}
		json.NewEncoder(w).Encode(map[string]any{
			"runner":             respRunner,
			"encoded_jit_config": encoded,
		})
	})
	mux.HandleFunc(listPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.listCalls, 1)
		f.mu.Lock()
		block := f.listBlock
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		total := int64(len(f.runners))
		if f.totalOverride != nil {
			total = *f.totalOverride
		}
		list := []map[string]any{}
		for _, r := range f.runners {
			list = append(list, r)
		}
		if f.dupeID && len(list) > 1 {
			list[1]["id"] = list[0]["id"]
		}
		if f.truncated && len(list) > 1 {
			list = list[:1]
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "runners": list})
	})
	mux.HandleFunc(runnerPrefix, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var id int64
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, runnerPrefix), "%d", &id)
		runner, ok := f.runners[id]
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&f.deleteCalls, 1)
			if ok && !f.deleteKeep {
				delete(f.runners, id)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(runner)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	client := &ciGitHubClient{appID: 42, key: key, base: srv.URL, hc: srv.Client()}
	return f, srv, client
}

// setupRegisteredLease drives put + created VM + successful JIT registration.
func setupRegisteredLease(t *testing.T, e *ciEnforcerEngine, fc *fakeCICompute, gh *ciGitHubClient, spec ciEnforcerSpec, serverID string) (int64, string) {
	t.Helper()
	putAndCreate(t, e, fc, spec, serverID)
	_, runnerID, jit, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
	if err != nil {
		t.Fatalf("registerRunner: %v", err)
	}
	return runnerID, jit
}

func TestCIEnforcerJITRegisterFullCleanupChain(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, dir := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-jit", 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	runnerID, jit := setupRegisteredLease(t, e, fc, client, spec, "srv-jit-1")
	if runnerID <= 0 || jit == "" {
		t.Fatalf("JIT registration failed")
	}
	// Token permissions are scoped exactly.
	gh.mu.Lock()
	perms := gh.lastPerms
	gh.mu.Unlock()
	if perms["administration"] != "write" || perms["metadata"] != "read" || len(perms) != 2 {
		t.Fatalf("runner token permissions must be administration:write+metadata:read, got %v", perms)
	}
	// Full cleanup: stop -> physical -> github -> physical -> tombstone.
	e.stop(leaseID)
	driveCleanup(t, e, leaseID, 3)
	l := leaseState(t, e, leaseID)
	if !l.Settled {
		t.Fatalf("lease must tombstone after full proofs, got %+v", l)
	}
	if atomic.LoadInt32(&gh.jitCalls) != 1 || atomic.LoadInt32(&gh.deleteCalls) == 0 {
		t.Fatalf("JIT POST once and runner DELETE required")
	}
	// No token, raw or encoded JIT, or runner ID in any state file.
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		s := string(raw)
		for _, leak := range []string{"runner-token", "jit-config", "encodedJit", base64.StdEncoding.EncodeToString([]byte("jit-config")), fmt.Sprint(runnerID)} {
			if strings.Contains(s, leak) {
				t.Fatalf("state file %s leaks %q", ent.Name(), leak)
			}
		}
	}
	// Tombstone status retains identity but zeroes private metadata.
	st, err := e.status(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Settled || st.OperationID != spec.Plan.OperationID || st.PlanDigest == "" ||
		len(st.Resources) != 0 || st.GitHubRunnerID != 0 || st.RunnerName != "" || !st.StopRequested {
		t.Fatalf("bad tombstone status: %+v", st)
	}
	// Restart proves the durable tombstone.
	e.close()
	wall, mono := newFakeClock().pair()
	e2, err := newCIEnforcerEngine(dir, fc, client, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	st2, _ := e2.status(leaseID)
	if !st2.Settled || st2.OperationID != spec.Plan.OperationID {
		t.Fatalf("tombstone must survive restart: %+v", st2)
	}
}

func TestCIEnforcerJITNameConflictLatches(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-conflict", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-conf-1")
	existing := ciEnforcerRunnerName(spec.Plan.OperationID)
	gh.mu.Lock()
	gh.runners[9001] = ghRunnerJSON(9001, existing, []string{"canter-operation-" + spec.Plan.OperationID})
	gh.mu.Unlock()
	if _, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing canonical name must conflict, got %v", err)
	}
	if atomic.LoadInt32(&gh.jitCalls) != 0 {
		t.Fatalf("conflict must never POST")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if !l.RegistrationConflict {
		t.Fatalf("name conflict must latch permanently")
	}
	// Foreign runner is never adopted or deleted.
	gh.mu.Lock()
	_, stillThere := gh.runners[9001]
	gh.mu.Unlock()
	if !stillThere {
		t.Fatalf("foreign runner must never be deleted")
	}
	e.stop(spec.Plan.EnforcerLease)
	driveCleanup(t, e, spec.Plan.EnforcerLease, 3)
	if leaseState(t, e, spec.Plan.EnforcerLease).Settled {
		t.Fatal("registration conflict must remain unresolved after physical cleanup")
	}
}

func TestCIEnforcerAmbiguousJITRecoveryFullChain(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, dir := newTestEngineAt(t, fc, client, clk)
	spec := enforcerTestSpecAt("op-ambig", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	putAndCreate(t, e, fc, spec, "srv-amb-1")
	// Ambiguous POST: 500 with no definitive outcome.
	gh.mu.Lock()
	gh.jitErr = http.StatusInternalServerError
	gh.mu.Unlock()
	if _, _, _, err := e.registerRunner(context.Background(), leaseID); err == nil {
		t.Fatalf("ambiguous JIT outcome must error")
	}
	l := leaseState(t, e, leaseID)
	if l.RegistrationSettled || l.RegistrationAttempted == false {
		t.Fatalf("attempt must persist, outcome must stay unsettled: %+v", l)
	}
	// Retry must not repeat the POST.
	if _, _, _, err := e.registerRunner(context.Background(), leaseID); !errors.Is(err, errCIRunnerJobPending) {
		t.Fatalf("ambiguous retry must be pending, got %v", err)
	}
	if atomic.LoadInt32(&gh.jitCalls) != 1 {
		t.Fatalf("ambiguous JIT must not repeat POST")
	}
	// The POST actually created a runner server-side (fixture 500 after
	// create is simulated by injecting the runner directly).
	gh.mu.Lock()
	gh.jitErr = 0
	gh.runners[6001] = ghRunnerJSON(6001, ciEnforcerRunnerName(spec.Plan.OperationID),
		[]string{"canter-operation-" + spec.Plan.OperationID, "linux", "x64"})
	gh.mu.Unlock()
	// Live recovery resolves identity.
	e.recoverLiveRegistration(context.Background(), leaseID)
	l = leaseState(t, e, leaseID)
	if !l.RegistrationSettled || l.GitHubRunnerID != 6001 {
		t.Fatalf("recovery must settle positive identity: %+v", l)
	}
	// Full cleanup chain with delete-ACK-still-200 proof first.
	gh.mu.Lock()
	gh.deleteKeep = true
	gh.mu.Unlock()
	e.stop(leaseID)
	e.reconcilePhysical(context.Background(), leaseID)
	e.reconcileGitHub(context.Background(), leaseID)
	l = leaseState(t, e, leaseID)
	if l.RegAbsent {
		t.Fatalf("DELETE ACK while runner still 200 must not prove absence")
	}
	// Now let the delete take effect; next pass proves 404 + empty discovery.
	gh.mu.Lock()
	gh.deleteKeep = false
	gh.mu.Unlock()
	driveCleanup(t, e, leaseID, 3)
	l = leaseState(t, e, leaseID)
	if !l.Settled {
		t.Fatalf("full chain must tombstone: %+v", l)
	}
	// Restart proves durable tombstone.
	e.close()
	wall, mono := newFakeClock().pair()
	e2, err := newCIEnforcerEngine(dir, fc, client, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	st, _ := e2.status(leaseID)
	if !st.Settled {
		t.Fatalf("tombstone must survive restart")
	}
}

func TestCIEnforcerExpiryDuringBlockedJITDeletesVM(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, client, clk)
	spec := enforcerTestSpecAt("op-blocked", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, "srv-blocked-1", "ACTIVE")
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := e.closeCreation(context.Background(), spec.Plan.EnforcerLease, "created", "srv-blocked-1"); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.jitBlock = make(chan struct{})
	gh.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&gh.jitCalls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("JIT POST never reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Expire while the GitHub POST is blocked: physical cleanup must not wait.
	clk.advance(400 * time.Second)
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("expiry must delete VM while GitHub registration is blocked")
	}
	gh.mu.Lock()
	close(gh.jitBlock)
	gh.mu.Unlock()
	if err := <-result; err == nil {
		t.Fatalf("blocked-then-expired registration must not deliver JIT")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.GitHubRunnerID <= 0 {
		t.Fatalf("returned runner ID must be retained for cleanup")
	}
}

func TestCIEnforcerGitHubOutageDoesNotBlockPhysicalCleanup(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-ghdown", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, "srv-gh-1", "ACTIVE")
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	// No GitHub client at all: never-attempted registration must not block.
	clk.advance(400 * time.Second)
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	e.reconcileGitHub(context.Background(), spec.Plan.EnforcerLease)
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("physical cleanup must not depend on GitHub availability")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.Settled {
		t.Fatalf("unsettled lease must not tombstone")
	}
	if !l.RegAbsent {
		t.Fatalf("never-attempted registration must prove GitHub absence")
	}
}

func TestCIEnforcerAmbiguousJITStaysUnsettled(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-ambig2", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-amb2-1")
	gh.mu.Lock()
	gh.jitErr = http.StatusInternalServerError
	gh.mu.Unlock()
	if _, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease); err == nil {
		t.Fatalf("ambiguous JIT outcome must error")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.RegistrationSettled {
		t.Fatalf("ambiguous registration must stay unsettled")
	}
	// Unknown POST with zero source matches stays unsettled forever.
	e.stop(spec.Plan.EnforcerLease)
	driveCleanup(t, e, spec.Plan.EnforcerLease, 3)
	l = leaseState(t, e, spec.Plan.EnforcerLease)
	if l.Settled {
		t.Fatalf("unknown registration must never tombstone")
	}
}

func TestCIEnforcerPreflightFailureRetries(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, client)
	spec := enforcerTestSpec("op-preflight", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-pre-1")
	gh.mu.Lock()
	gh.tokenErr = http.StatusInternalServerError
	gh.mu.Unlock()
	if _, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease); err == nil {
		t.Fatalf("preflight failure must error")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.RegistrationAttempted {
		t.Fatalf("preflight failure must NOT persist attempted (no POST happened)")
	}
	// Retry succeeds once preflight recovers.
	gh.mu.Lock()
	gh.tokenErr = 0
	gh.mu.Unlock()
	_, runnerID, jit, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
	if err != nil || runnerID <= 0 || jit == "" {
		t.Fatalf("retry after preflight failure must succeed: %v", err)
	}
	if atomic.LoadInt32(&gh.jitCalls) != 1 {
		t.Fatalf("exactly one POST across retry")
	}
}

func TestCIEnforcerFullObservationErrorDeletesRetainedOnly(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-obserr", clk.wall, 300*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-retained-1")
	fc.mu.Lock()
	fc.observeErr = errors.New("provider down")
	fc.mu.Unlock()
	clk.advance(400 * time.Second)
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	fc.mu.Lock()
	fc.observeErr = nil
	fc.mu.Unlock()
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("observation failure must still best-effort delete retained IDs")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.Settled {
		t.Fatalf("unsettled evidence must never tombstone")
	}
}

func TestCIEnforcerApproveShortDeadlineAndReadiness(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, client, clk)
	spec := enforcerTestSpecAt("op-approve", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	putAndCreate(t, e, fc, spec, "srv-appr-1")
	_, _, _ = setupRegisteredLeaseRetry(t, e, spec)
	// Short maximumSeconds legitimately shortens the setup cutoff.
	short := clk.wall.Add(60 * time.Second).UnixMilli()
	st, err := e.approve(leaseID, short)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if st.DeadlineUnixMs != short {
		t.Fatalf("approved cutoff must equal requested deadline")
	}
	// Exact retry idempotent while future.
	if _, err = e.approve(leaseID, short); err != nil {
		t.Fatalf("approval retry: %v", err)
	}
	// Different cutoff conflicts.
	if _, err = e.approve(leaseID, short+1000); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("second approval must conflict")
	}
	_ = gh
	_ = client
}

func setupRegisteredLeaseRetry(t *testing.T, e *ciEnforcerEngine, spec ciEnforcerSpec) (int64, string, string) {
	t.Helper()
	_, runnerID, jit, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease)
	if err != nil {
		t.Fatalf("registerRunner: %v", err)
	}
	return runnerID, jit, ""
}

func TestCIEnforcerWallRollbackMonotonicAndRestart(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, dir := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-mono", clk.wall, 300*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-mono-1")
	// Wall clock rolls back but monotonic keeps advancing: earlier wins.
	clk.rollbackWall(100 * time.Second)
	clk.advanceMono(400 * time.Second)
	st := leaseState(t, e, spec.Plan.EnforcerLease)
	w, m := e.now()
	l := st
	if !l.expired(w, m) {
		t.Fatalf("monotonic cutoff must expire even when wall rolls back")
	}
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("mono-expired lease must delete VM")
	}
	// Restart rebuilds remaining monotonic duration from the absolute cutoff,
	// never a fresh window: with clocks past the deadline it stays expired.
	e.close()
	clk.advance(500 * time.Second)
	wall, mono := clk.pair()
	e2, err := newCIEnforcerEngine(dir, fc, nil, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.close()
	l2 := leaseState(t, e2, spec.Plan.EnforcerLease)
	w2, m2 := e2.now()
	if l2.DeadlineUnixMs != spec.Plan.SetupDeadlineAt.UnixMilli() {
		t.Fatalf("restart must keep original absolute deadline")
	}
	if !l2.expired(w2, m2) {
		t.Fatalf("restart must not renew the window")
	}
}

func TestCIEnforcerStorageFailureLatchesUnavailable(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, dir := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-storage", clk.wall, 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-sto-1")
	// Make the state dir read-only so persist fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	// A mutating transition fails and latches the lease unavailable.
	if _, err := e.stop(spec.Plan.EnforcerLease); err == nil {
		t.Fatalf("stop with failing storage must error")
	}
	// Matching approval/JIT/settled acknowledgement must fail too.
	if _, err := e.approve(spec.Plan.EnforcerLease, time.Now().Add(time.Hour).UnixMilli()); err == nil {
		t.Fatalf("approve must be latched unavailable")
	}
	if _, _, _, err := e.registerRunner(context.Background(), spec.Plan.EnforcerLease); err == nil {
		t.Fatalf("register must be latched unavailable")
	}
	st, _ := e.status(spec.Plan.EnforcerLease)
	if st.Settled {
		t.Fatalf("settled must never be claimed on storage failure")
	}
	// A damaged lease drives immediate best-effort cleanup from the last
	// committed scope, without waiting for its original setup cutoff.
	os.Chmod(dir, 0o700)
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("best-effort physical cleanup must use last committed scope")
	}
}

func TestCIEnforcerUnknownCreationLateMaterialization(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-late", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	// Creation was never closed; provider VM materializes late.
	fc.mu.Lock()
	fc.lateServer = &compute.Server{ID: "srv-late-1", Name: scope.ServerName, Status: "ACTIVE", Metadata: map[string]string{
		"canter.managed": "true", "canter.sandbox": "ci-" + scope.OperationID,
		"canter.operation": scope.OperationID, "canter.resource": scope.ServerName,
	}}
	fc.mu.Unlock()
	if _, err := e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	clk.advance(400 * time.Second)
	e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
	// Late VM discovered and deleted even though creation never settled.
	if atomic.LoadInt32(&fc.deleteCalls) == 0 {
		t.Fatalf("late materialized VM must be discovered and deleted")
	}
	l := leaseState(t, e, spec.Plan.EnforcerLease)
	if l.Settled {
		t.Fatalf("settlement must stay blocked until creator closes creation")
	}
}

func TestCIEnforcerBlockedGitHubDoesNotDelayThirdLease(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, client, clk)
	// Two leases with positive runner IDs block inside GitHub delete.
	var leaseIDs []string
	for i, op := range []string{"op-blk-a", "op-blk-b"} {
		spec := enforcerTestSpecAt(op, clk.wall, 800*time.Second, 20000*time.Second)
		putAndCreate(t, e, fc, spec, fmt.Sprintf("srv-blk-%d", i))
		setupRegisteredLease(t, e, fc, client, spec, fmt.Sprintf("srv-blk-%d", i))
		e.stop(spec.Plan.EnforcerLease)
		leaseIDs = append(leaseIDs, spec.Plan.EnforcerLease)
	}
	// Third lease: known VM, never registered, expires on its own.
	spec3 := enforcerTestSpecAt("op-blk-c", clk.wall, 300*time.Second, 20000*time.Second)
	scope3 := ciEnforcerScope(spec3)
	fc.addServer(scope3, "srv-blk-c", "ACTIVE")
	if _, err := e.putLease(spec3.Plan.EnforcerLease, spec3); err != nil {
		t.Fatal(err)
	}
	// Block GitHub runner GET/DELETE for the first two.
	gh.mu.Lock()
	gh.deleteKeep = true
	gh.mu.Unlock()
	clk.advance(400 * time.Second)
	// Physical passes for all three run; blocked GitHub work must not hold
	// physical slots — third lease's VM is deleted regardless.
	for _, id := range leaseIDs {
		e.reconcilePhysical(context.Background(), id)
	}
	e.reconcilePhysical(context.Background(), spec3.Plan.EnforcerLease)
	if atomic.LoadInt32(&fc.deleteCalls) < 3 {
		t.Fatalf("blocked GitHub cleanups must not delay third VM deletion")
	}
}

func TestCIEnforcerNoOverlappingSameLeaseReconcile(t *testing.T) {
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, nil)
	spec := enforcerTestSpec("op-nooverlap", 300*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec, "srv-noov-1")
	leaseID := spec.Plan.EnforcerLease
	st, _ := e.stateFor(leaseID)
	st.physicalMu.Lock()
	done := make(chan struct{})
	go func() {
		e.reconcilePhysical(context.Background(), leaseID)
		close(done)
	}()
	select {
	case <-done:
		t.Fatalf("reconcile must serialize on physicalMu")
	case <-time.After(100 * time.Millisecond):
	}
	st.physicalMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("reconcile did not complete after unlock")
	}
}

func TestCIEnforcerConcurrentHTTPAndJournal(t *testing.T) {
	token := []byte(strings.Repeat("t", 40))
	fc := newFakeCICompute()
	clk := newFakeClock()
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	wall, mono := clk.pair()
	e, err := newCIEnforcerEngine(dir, fc, nil, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	client, err := newCIEnforcerClient(httpSrv.URL, token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	spec := enforcerTestSpecAt("op-race", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err = client.PutLease(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				client.Status(context.Background(), spec.Plan)
			case 1:
				client.Stop(context.Background(), spec.Plan)
			case 2:
				client.PutLease(context.Background(), spec)
			case 3:
				e.reconcilePhysical(context.Background(), spec.Plan.EnforcerLease)
			}
		}(i)
	}
	wg.Wait()
}

func TestCIEnforcerRunTickerExpiredAfterRestart(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	spec := enforcerTestSpecAt("op-ticker", clk.wall, 300*time.Second, 20000*time.Second)
	scope := ciEnforcerScope(spec)
	fc.addServer(scope, "srv-tick-1", "ACTIVE")
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	wall, mono := clk.pair()
	e, err := newCIEnforcerEngine(dir, fc, nil, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatal(err)
	}
	// Simulate restart with no HTTP/control plane/App: clock already past
	// expiry; Run's ticker must reconcile on its own.
	e.close()
	clk.advance(400 * time.Second)
	wall2, mono2 := clk.pair()
	e2, err := newCIEnforcerEngine(dir, fc, nil, wall2, mono2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { e2.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&fc.deleteCalls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run ticker must reconcile expired lease without HTTP")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	e2.close()
}

func TestCIEnforcerStrictJSON(t *testing.T) {
	token := []byte(strings.Repeat("t", 40))
	e, _ := newTestEngine(t, newFakeCICompute(), nil)
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	leaseID := strings.Repeat("f", 64)
	post := func(path, body string) int {
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+string(token))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Stop accepts empty or exact {} only.
	if c := post("/v1/leases/"+leaseID+"/stop", ""); c == http.StatusBadRequest {
		t.Fatalf("empty stop body must not be malformed (may 404/409)")
	}
	if c := post("/v1/leases/"+leaseID+"/stop", "{}"); c == http.StatusBadRequest {
		t.Fatalf("exact {} stop body must not be malformed")
	}
	for _, bad := range []string{`{"x":1}`, `null`, `{"a":1} {"b":2}`, `{"a":`, `{"a":null}`} {
		if c := post("/v1/leases/"+leaseID+"/stop", bad); c != http.StatusBadRequest {
			t.Fatalf("stop body %q must be 400, got %d", bad, c)
		}
	}
	// Duplicate keys rejected on PUT.
	spec := enforcerTestSpec("op-strict", 800*time.Second, 20000*time.Second)
	raw, _ := json.Marshal(ciEnforcerLeaseSpecDTO(spec))
	dup := strings.Replace(string(raw), `"githubRepositoryId":777`, `"githubRepositoryId":777,"githubRepositoryId":777`, 1)
	req, _ := http.NewRequest(http.MethodPut, httpSrv.URL+"/v1/leases/"+spec.Plan.EnforcerLease, strings.NewReader(dup))
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate field must be 400, got %d", resp.StatusCode)
	}
	// Unknown field rejected.
	unk := strings.Replace(string(raw), `"sizeId"`, `"bogus":1,"sizeId"`, 1)
	req, _ = http.NewRequest(http.MethodPut, httpSrv.URL+"/v1/leases/"+spec.Plan.EnforcerLease, strings.NewReader(unk))
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field must be 400, got %d", resp.StatusCode)
	}
	// no-store header on JSON responses.
	req, _ = http.NewRequest(http.MethodGet, httpSrv.URL+"/v1/leases/"+strings.Repeat("0", 64), nil)
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("responses must set Cache-Control: no-store")
	}
}

func TestCIEnforcerHTTPAuthAndClientScope(t *testing.T) {
	token := []byte(strings.Repeat("t", 40))
	e, _ := newTestEngine(t, newFakeCICompute(), nil)
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	client, err := newCIEnforcerClient(httpSrv.URL, token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(httpSrv.URL + "/v1/leases/" + strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request must be 401, got %d", resp.StatusCode)
	}
	spec := enforcerTestSpec("op-http", 800*time.Second, 20000*time.Second)
	st, err := client.PutLease(context.Background(), spec)
	if err != nil {
		t.Fatalf("PutLease: %v", err)
	}
	if st.OperationID != spec.Plan.OperationID || st.PlanDigest == "" || st.SpecDigest == "" {
		t.Fatalf("bad status: %+v", st)
	}
	st2, err := client.Status(context.Background(), spec.Plan)
	if err != nil || st2.DeadlineUnixMs != st.DeadlineUnixMs {
		t.Fatalf("Status: %v %+v", err, st2)
	}
	// A plan with a different operation/digest/lease must hard-conflict.
	wrong := spec.Plan
	wrong.OperationID = "op-other"
	if _, err = client.Status(context.Background(), wrong); !errors.Is(err, errCIEnforcerScope) {
		t.Fatalf("wrong-operation status must be scope conflict, got %v", err)
	}
	// A lease the enforcer does not know is availability (404), never an
	// authenticated scope conflict.
	wrongLease := spec.Plan
	wrongLease.EnforcerLease = strings.Repeat("9", 64)
	if _, err = client.Status(context.Background(), wrongLease); err == nil || errors.Is(err, errCIEnforcerScope) {
		t.Fatalf("missing lease must be availability, got %v", err)
	}
	// Stop then approve must conflict.
	if _, err = client.Stop(context.Background(), spec.Plan); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Approve(context.Background(), spec.Plan, time.Now().Add(3600*time.Second).UnixMilli()); err == nil {
		t.Fatalf("approve after stop must fail")
	}
}

func TestCIGitHubRunnerPaginationBounds(t *testing.T) {
	gh, _, client := newGitHubFixture(t)
	ctx := context.Background()
	// Token + repo resolution helpers.
	token, err := client.ciRunnerManagementToken(ctx, 42, 777)
	if err != nil || token != "runner-token" {
		t.Fatalf("token: %v", err)
	}
	owner, name, err := client.ciManagementRepository(ctx, token, 777)
	if err != nil || owner != "octo" || name != "repo" {
		t.Fatalf("repo: %v %s %s", err, owner, name)
	}
	// MaxInt64 total must be rejected before page math.
	maxT := int64(math.MaxInt64)
	gh.mu.Lock()
	gh.totalOverride = &maxT
	gh.mu.Unlock()
	if _, err = client.ciListRepoRunners(ctx, token, owner, name); err == nil {
		t.Fatalf("MaxInt64 total must fail closed")
	}
	// 10001 entries must be rejected.
	over := int64(10001)
	gh.mu.Lock()
	gh.totalOverride = &over
	gh.mu.Unlock()
	if _, err = client.ciListRepoRunners(ctx, token, owner, name); err == nil {
		t.Fatalf("10001 total must fail closed")
	}
	// Truncated page fails.
	gh.mu.Lock()
	gh.totalOverride = nil
	gh.truncated = true
	gh.runners[1] = ghRunnerJSON(1, "r1", nil)
	gh.runners[2] = ghRunnerJSON(2, "r2", nil)
	gh.mu.Unlock()
	if _, err = client.ciListRepoRunners(ctx, token, owner, name); err == nil {
		t.Fatalf("truncated page must fail closed")
	}
	// Duplicate IDs fail.
	gh.mu.Lock()
	gh.truncated = false
	gh.dupeID = true
	gh.mu.Unlock()
	if _, err = client.ciListRepoRunners(ctx, token, owner, name); err == nil {
		t.Fatalf("duplicate runner IDs must fail closed")
	}
}

func TestCIRunnerOwnerLabelTypes(t *testing.T) {
	const op = "op-label-types"
	want := ciRunnerOwnerLabel(op)
	runner := ciGitHubRunner{ID: 1, Name: "r"}
	for _, typ := range []string{"custom", "read-only"} {
		runner.Labels = []ciGitHubLabel{{Name: want, Type: typ}}
		if !ciRunnerHasOwnerLabel(runner, op) {
			t.Fatalf("owner label type %q must identify ownership", typ)
		}
		if !ciJITRunnerHasOwnerLabel(runner.Labels, op) {
			t.Fatalf("JIT owner label type %q must identify ownership", typ)
		}
	}
	for _, labels := range [][]ciGitHubLabel{
		{{Name: want, Type: ""}},
		{{Name: want, Type: "system"}},
		{{Name: ciRunnerOwnerLabel("op-other"), Type: "read-only"}},
	} {
		runner.Labels = labels
		if ciRunnerHasOwnerLabel(runner, op) {
			t.Fatalf("labels %+v must not identify ownership", labels)
		}
	}
}
