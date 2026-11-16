package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This fixture deliberately does NOT call integrationStore (which truncates a
// shared database). Every test gets a new private schema and leaves it intact.
// search_path has ONLY that schema, never public.
func superSandboxFixture(t *testing.T) (*HTTPServer, sdk.ActorRef) {
	t.Helper()
	return superSandboxFixtureConnections(t, 4)
}

func superSandboxFixtureConnections(t *testing.T, connections int32) (*HTTPServer, sdk.ActorRef) {
	t.Helper()
	url := os.Getenv("CANTER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CANTER_TEST_DATABASE_URL is required for isolated-schema tests")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := newID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_super_sandbox_" + strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(suffix))
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	t.Logf("isolated schema (retained): %s", schema)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = connections
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := NewStore(pool)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// No actual session is needed: durable authority belongs to the account,
	// not an expiring login. This also exercises session-independent dispatch.
	_, err = pool.Exec(ctx, `
	INSERT INTO accounts(id,email,password_hash) VALUES ('owner','owner@example.test',''),('viewer','viewer@example.test',''),('outsider','outsider@example.test','');
	INSERT INTO workspaces(id,name) VALUES ('ws','Super Sandbox tests'),('other','Other organization');
	INSERT INTO memberships(account_id,workspace_id,role) VALUES ('owner','ws','owner'),('viewer','ws','viewer'),('outsider','other','owner');
	INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase) VALUES
	('sbx_one','ws','One','free-1x1','free','ready'),('sbx_two','ws','Two','free-1x1','free','ready'),('sbx_other','other','Other','free-1x1','free','ready');
	`)
	if err != nil {
		t.Fatal(err)
	}
	return &HTTPServer{service: &Service{Store: s}, config: HTTPConfig{Sandboxes: SandboxConfig{Enabled: true}}}, sdk.ActorRef{Kind: "human", ID: "owner"}
}

func superSandboxInput(key string, tasks int) SuperSandboxCreateInput {
	in := SuperSandboxCreateInput{Name: "Batch", WorkerIDs: []string{"sbx_one", "sbx_two"}, MaxConcurrency: 2, MaxWorkerSeconds: tasks * 300, IdempotencyKey: key}
	for i := 0; i < tasks; i++ {
		in.Tasks = append(in.Tasks, SuperSandboxTaskInput{Name: fmt.Sprintf("Task %d", i), Command: "echo test"})
	}
	return in
}

func superSandboxCreate(t *testing.T, h *HTTPServer, actor sdk.ActorRef, in SuperSandboxCreateInput) SuperSandbox {
	t.Helper()
	v, err := h.CreateSuperSandbox(context.Background(), "ws", "owner", actor, in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func superSandboxRead(t *testing.T, h *HTTPServer, id string) SuperSandbox {
	t.Helper()
	v, err := h.service.Store.SuperSandbox(context.Background(), "ws", id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func superSandboxExec(t *testing.T, h *HTTPServer, query string, args ...any) {
	t.Helper()
	if _, err := h.service.Store.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func superSandboxReconcile(t *testing.T, d *SuperSandboxDispatcher, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Reconcile(ctx, "ws", id); err != nil {
		t.Fatal(err)
	}
}

// Fake guest mirrors the existing per-ID request fingerprint/tombstone behavior.
// Embedding the unused interface makes accidental unrelated RPCs panic.
type superSandboxGuest struct {
	sandboxAgentClient
	mu          sync.Mutex
	requests    map[string]wrencomputer.JobRequest
	jobs        map[string]wrencomputer.Job
	starts      []wrencomputer.JobRequest
	stops       []string
	lost        bool
	readError   bool
	unknown     bool
	stopError   bool
	beforeStart func(wrencomputer.JobRequest)
	afterStart  func(wrencomputer.JobRequest)
}

func newSuperSandboxGuest() *superSandboxGuest {
	return &superSandboxGuest{requests: map[string]wrencomputer.JobRequest{}, jobs: map[string]wrencomputer.Job{}}
}
func (g *superSandboxGuest) Close() {}
func (g *superSandboxGuest) Probe(context.Context) (wrencomputer.Capabilities, error) {
	return wrencomputer.Capabilities{WorkflowProtocol: 1}, nil
}
func (g *superSandboxGuest) Job(_ context.Context, id string) (wrencomputer.Job, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.readError {
		return wrencomputer.Job{}, errors.New("lost read")
	}
	if g.unknown {
		return wrencomputer.Job{ID: id, Status: "unknown"}, nil
	}
	j, ok := g.jobs[id]
	if !ok {
		return j, &wrencomputer.Error{Code: "not_found"}
	}
	return j, nil
}
func (g *superSandboxGuest) StartJob(_ context.Context, r wrencomputer.JobRequest) (wrencomputer.Job, error) {
	if g.beforeStart != nil {
		g.beforeStart(r)
	}
	g.mu.Lock()
	g.starts = append(g.starts, r)
	if prior, ok := g.requests[r.ID]; ok && !reflect.DeepEqual(prior, r) {
		g.mu.Unlock()
		return wrencomputer.Job{}, &wrencomputer.Error{Code: "conflict"}
	}
	g.requests[r.ID] = r
	j, ok := g.jobs[r.ID]
	if !ok {
		j = wrencomputer.Job{ID: r.ID, Status: "running"}
		g.jobs[r.ID] = j
	}
	if g.unknown {
		j.Status = "unknown"
	}
	lost := g.lost
	g.mu.Unlock()
	if g.afterStart != nil {
		g.afterStart(r)
	}
	if lost {
		return wrencomputer.Job{}, errors.New("receipt lost after guest accepted command")
	}
	return j, nil
}
func (g *superSandboxGuest) StopJob(_ context.Context, id string) (wrencomputer.Job, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stops = append(g.stops, id)
	if g.stopError {
		return wrencomputer.Job{}, errors.New("stop receipt lost")
	}
	j := g.jobs[id]
	if j.Status != "completed" && j.Status != "failed" {
		j = wrencomputer.Job{ID: id, Status: "cancelled"}
		g.jobs[id] = j
	}
	return j, nil
}
func (g *superSandboxGuest) finish(id, status string, code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jobs[id] = wrencomputer.Job{ID: id, Status: status, ExitCode: &code}
}
func superSandboxDispatcher(h *HTTPServer, g *superSandboxGuest) *SuperSandboxDispatcher {
	return &SuperSandboxDispatcher{Server: h, client: func(context.Context, Sandbox) (sandboxAgentClient, error) { return g, nil }}
}

func TestSuperSandboxNormalization(t *testing.T) {
	in := superSandboxInput("normal", 2)
	normalized, reserved, hash, err := normalizeSuperSandbox(in)
	if err != nil || reserved != 600 || normalized.Tasks[0].Directory != "/workspace" || normalized.Tasks[0].TimeoutSeconds != 300 {
		t.Fatalf("normalization: %+v reserved=%d err=%v", normalized, reserved, err)
	}
	explicit := in
	explicit.Name = " Batch "
	explicit.IdempotencyKey = "different key"
	explicit.Tasks = append([]SuperSandboxTaskInput(nil), in.Tasks...)
	for i := range explicit.Tasks {
		explicit.Tasks[i].Directory = "/workspace/."
		explicit.Tasks[i].TimeoutSeconds = 300
	}
	_, _, hash2, err := normalizeSuperSandbox(explicit)
	if err != nil || hash != hash2 {
		t.Fatalf("normalized hashes differ: %s %s %v", hash, hash2, err)
	}
	tests := []struct {
		name   string
		change func(*SuperSandboxCreateInput)
	}{
		{"budget", func(v *SuperSandboxCreateInput) { v.MaxWorkerSeconds = 599 }},
		{"max budget", func(v *SuperSandboxCreateInput) { v.MaxWorkerSeconds = 86401 }},
		{"negative budget", func(v *SuperSandboxCreateInput) { v.MaxWorkerSeconds = -1 }},
		{"duplicate workers", func(v *SuperSandboxCreateInput) { v.WorkerIDs[1] = v.WorkerIDs[0] }},
		{"concurrency", func(v *SuperSandboxCreateInput) { v.MaxConcurrency = 3 }},
		{"zero concurrency", func(v *SuperSandboxCreateInput) { v.MaxConcurrency = 0 }},
		{"no workers", func(v *SuperSandboxCreateInput) { v.WorkerIDs = nil }},
		{"many workers", func(v *SuperSandboxCreateInput) { v.WorkerIDs = make([]string, 17) }},
		{"no tasks", func(v *SuperSandboxCreateInput) { v.Tasks = nil }},
		{"many tasks", func(v *SuperSandboxCreateInput) { v.Tasks = make([]SuperSandboxTaskInput, 257) }},
		{"long command", func(v *SuperSandboxCreateInput) { v.Tasks[0].Command = strings.Repeat("x", 64001) }},
		{"NUL command", func(v *SuperSandboxCreateInput) { v.Tasks[0].Command = "echo \x00" }},
		{"blank command", func(v *SuperSandboxCreateInput) { v.Tasks[0].Command = " \n" }},
		{"timeout", func(v *SuperSandboxCreateInput) { v.Tasks[0].TimeoutSeconds = 3601 }},
		{"negative timeout", func(v *SuperSandboxCreateInput) { v.Tasks[0].TimeoutSeconds = -1 }},
		{"huge timeout", func(v *SuperSandboxCreateInput) { v.Tasks[0].TimeoutSeconds = int(^uint(0) >> 1) }},
		{"name", func(v *SuperSandboxCreateInput) { v.Name = "\x00" }},
		{"long name", func(v *SuperSandboxCreateInput) { v.Name = strings.Repeat("x", 101) }},
		{"task name", func(v *SuperSandboxCreateInput) { v.Tasks[0].Name = "" }},
		{"directory", func(v *SuperSandboxCreateInput) { v.Tasks[0].Directory = "/etc" }},
		{"missing key", func(v *SuperSandboxCreateInput) { v.IdempotencyKey = "" }},
		{"total commands", func(v *SuperSandboxCreateInput) {
			v.Tasks = make([]SuperSandboxTaskInput, 4)
			v.MaxWorkerSeconds = 1200
			for i := range v.Tasks {
				v.Tasks[i] = SuperSandboxTaskInput{Name: "Task", Command: strings.Repeat("x", 64000)}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := superSandboxInput("bad", 2)
			tt.change(&v)
			if _, _, _, err := normalizeSuperSandbox(v); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	// Aggregate timeout overflow cannot wrap: each element is bounded before addition.
	v := superSandboxInput("overflow", 256)
	v.MaxWorkerSeconds = 86400
	for i := range v.Tasks {
		v.Tasks[i].TimeoutSeconds = 3600
	}
	if _, _, _, err := normalizeSuperSandbox(v); err == nil {
		t.Fatal("aggregate timeout cap not enforced")
	}
}

func TestSuperSandboxIdempotencyAndAuthority(t *testing.T) {
	h, actor := superSandboxFixture(t)
	ctx := context.Background()
	in := superSandboxInput("same", 3)
	v := superSandboxCreate(t, h, actor, in)
	if v.ReservedWorkerSeconds != 900 || v.State != "queued" || len(v.Tasks) != 3 {
		t.Fatalf("bad create: %+v", v)
	}
	second := superSandboxCreate(t, h, actor, in)
	if second.ID != v.ID || second.Tasks[0].ID != v.Tasks[0].ID {
		t.Fatal("idempotency did not return same resource")
	}
	in.Tasks[0].Command = "different"
	if _, err := h.CreateSuperSandbox(ctx, "ws", "owner", actor, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("different payload: %v", err)
	}
	for _, account := range []string{"viewer", "outsider", ""} {
		if _, err := h.CreateSuperSandbox(ctx, "ws", account, actor, superSandboxInput(account+"key", 1)); err == nil {
			t.Fatalf("account %q could create", account)
		}
		if _, err := h.CancelSuperSandbox(ctx, "ws", account, v.ID, actor); err == nil {
			t.Fatalf("account %q could cancel", account)
		}
	}
	if _, err := h.service.Store.SuperSandbox(ctx, "other", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-organization detail: %v", err)
	}
	in = superSandboxInput("cross-worker", 1)
	in.WorkerIDs[0] = "sbx_other"
	if _, err := h.CreateSuperSandbox(ctx, "ws", "owner", actor, in); err == nil {
		t.Fatal("cross-organization worker accepted")
	}
	h.config.Sandboxes.Enabled = false
	if _, err := h.CreateSuperSandbox(ctx, "ws", "owner", actor, superSandboxInput("disabled", 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("global flag create: %v", err)
	}
	if _, err := h.CancelSuperSandbox(ctx, "ws", "owner", v.ID, actor); err != nil {
		t.Fatalf("global flag cancel: %v", err)
	}
}

func TestSuperSandboxParallelClaimsAndCrossRunExclusion(t *testing.T) {
	h, actor := superSandboxFixture(t)
	a := superSandboxCreate(t, h, actor, superSandboxInput("a", 8))
	b := superSandboxCreate(t, h, actor, superSandboxInput("b", 8))
	d := &SuperSandboxDispatcher{Server: h}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := a.ID
			if i%2 == 0 {
				id = b.ID
			}
			_, err := d.claim(context.Background(), "ws", id)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var active, distinctWorkers, handles int
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT count(*),count(DISTINCT worker_id) FROM super_sandbox_tasks WHERE state='starting'`).Scan(&active, &distinctWorkers); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandbox_jobs WHERE finished_at IS NULL`).Scan(&handles); err != nil {
		t.Fatal(err)
	}
	if active != 2 || distinctWorkers != 2 || handles != 2 {
		t.Fatalf("claims=%d workers=%d handles=%d", active, distinctWorkers, handles)
	}
	// Both runs share the same physical workers, even across independent claims.
	for _, id := range []string{a.ID, b.ID} {
		v := superSandboxRead(t, h, id)
		count := 0
		for _, task := range v.Tasks {
			if task.State == "starting" {
				count++
				if task.JobID == "" {
					t.Fatal("claim lacks durable job ID")
				}
			}
		}
		if count > v.MaxConcurrency {
			t.Fatal("per-run concurrency exceeded")
		}
	}
}

func TestSuperSandboxStableReplayAfterLostReceipt(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("lost", 2)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	g.lost = true
	g.beforeStart = func(r wrencomputer.JobRequest) {
		if !h.service.Store.sandboxJobKnown(context.Background(), "sbx_one", r.ID) {
			t.Error("remote RPC preceded durable sandbox_jobs record")
		}
	}
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "needs_attention" || v.Tasks[0].State != "unknown" || v.Tasks[1].State != "queued" {
		t.Fatalf("lost receipt released worker or dispatched pending task: %+v", v)
	}
	first := g.starts[0]
	if first.Env["CANTER_SUPER_SANDBOX_ID"] != v.ID || first.Env["CANTER_SUPER_SANDBOX_TASK_INDEX"] != "0" || first.Env["CANTER_SUPER_SANDBOX_TASK_ID"] != v.Tasks[0].ID {
		t.Fatalf("missing task metadata: %+v", first)
	}
	// Generic readers must not release unknown batch-owned job handles.
	h.service.Store.finishSandboxJob(context.Background(), v.Tasks[0].WorkerID, v.Tasks[0].JobID)
	var finished *time.Time
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT finished_at FROM sandbox_jobs WHERE job_id=$1`, first.ID).Scan(&finished); err != nil || finished != nil {
		t.Fatalf("generic finish released uncertain batch job: %v %v", finished, err)
	}
	if busy, err := h.service.Store.superSandboxWorkerBusy(context.Background(), v.Tasks[0].WorkerID); err != nil || !busy {
		t.Fatalf("uncertain worker not busy: %v %v", busy, err)
	}
	// A new dispatcher (process restart), with lookup unavailable, must replay
	// identical ID+command+directory+timeout+env+sandbox flag, never a new task.
	g.lost = false
	g.readError = true
	d = superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	if len(g.starts) != 2 || !reflect.DeepEqual(first, g.starts[1]) || len(g.requests) != 1 {
		t.Fatalf("replay changed persisted request: %+v", g.starts)
	}
	g.readError = false
	g.finish(first.ID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "succeeded" || v.Tasks[1].State != "running" || v.Tasks[1].JobID == first.ID {
		t.Fatalf("worker was not correctly reused after positive receipt: %+v", v)
	}
	g.finish(v.Tasks[1].JobID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "succeeded" || v.FinishedAt == nil || v.ReservedWorkerSeconds != 600 {
		t.Fatalf("completion: %+v", v)
	}
}

func TestSuperSandboxUnknownAndGenerationChange(t *testing.T) {
	h, actor := superSandboxFixture(t)
	v := superSandboxCreate(t, h, actor, superSandboxInput("unknown", 3))
	g := newSuperSandboxGuest()
	g.unknown = true
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "needs_attention" || v.Tasks[0].State != "unknown" || v.FinishedAt != nil {
		t.Fatalf("unknown became terminal: %+v", v)
	}
	starts := len(g.starts)
	superSandboxExec(t, h, `UPDATE sandboxes SET generation=generation+1 WHERE id=$1`, v.Tasks[0].WorkerID)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" || !strings.Contains(v.Tasks[0].Error, "generation") || v.FinishedAt != nil {
		t.Fatalf("generation change misreported: %+v", v)
	}
	// Other active workers can still be observed, but the changed worker must
	// never receive any replay on its replacement generation.
	for _, r := range g.starts[starts:] {
		if r.ID == v.Tasks[0].JobID {
			t.Fatal("replayed old job on changed generation")
		}
	}
	g.stopError = true
	if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor); err != nil {
		t.Fatal(err)
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "cancelling" || v.FinishedAt != nil || v.Tasks[2].State != "cancelled" {
		t.Fatalf("uncertain cancellation became terminal: %+v", v)
	}
}

func TestSuperSandboxCancellationLateStartRace(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("cancel-race", 3)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	entered, resume := make(chan struct{}), make(chan struct{})
	g.beforeStart = func(wrencomputer.JobRequest) { close(entered); <-resume }
	done := make(chan error, 1)
	go func() { done <- d.Reconcile(context.Background(), "ws", v.ID) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("start never reached fake guest")
	}
	if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor); err != nil {
		t.Fatal(err)
	}
	// Another replica sees cancellation while the first StartJob is in flight.
	// It must persist a cancellation tombstone on the SAME known job ID.
	superSandboxReconcile(t, superSandboxDispatcher(h, g), v.ID)
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	v = superSandboxRead(t, h, v.ID)
	if v.State != "cancelled" || v.FinishedAt == nil || len(g.requests) != 1 {
		t.Fatalf("late start escaped cancellation: %+v starts=%+v", v, g.starts)
	}
	for _, task := range v.Tasks {
		if task.State != "cancelled" {
			t.Fatalf("task escaped cancellation: %+v", task)
		}
	}
	for _, id := range g.stops {
		if id != v.Tasks[0].JobID {
			t.Fatalf("stopped unowned job %s", id)
		}
	}
}

func TestSuperSandboxAuthorityWithdrawal(t *testing.T) {
	for _, mode := range []string{"role", "product", "global"} {
		t.Run(mode, func(t *testing.T) {
			h, actor := superSandboxFixture(t)
			in := superSandboxInput("withdraw", 3)
			in.MaxConcurrency = 1
			v := superSandboxCreate(t, h, actor, in)
			g := newSuperSandboxGuest()
			d := superSandboxDispatcher(h, g)
			superSandboxReconcile(t, d, v.ID)
			switch mode {
			case "role":
				superSandboxExec(t, h, `UPDATE memberships SET role='viewer' WHERE workspace_id='ws' AND account_id='owner'`)
			case "product":
				// All writes are confined to the test's new retained schema.
				superSandboxExec(t, h, `UPDATE workspace_products SET product='test-disabled-sandboxes' WHERE workspace_id='ws' AND product='sandboxes'`)
			case "global":
				h.config.Sandboxes.Enabled = false
			}
			superSandboxReconcile(t, d, v.ID)
			v = superSandboxRead(t, h, v.ID)
			if v.State != "failed" || v.FinishedAt == nil || v.Failure == "" || len(g.starts) != 1 || len(g.stops) != 1 {
				t.Fatalf("authority withdrawal not drained: %+v starts=%d stops=%d", v, len(g.starts), len(g.stops))
			}
		})
	}
}

func TestSuperSandboxQueuedGenerationChange(t *testing.T) {
	h, actor := superSandboxFixture(t)
	v := superSandboxCreate(t, h, actor, superSandboxInput("queued-generation", 1))
	superSandboxExec(t, h, `UPDATE sandboxes SET generation=generation+1 WHERE id='sbx_one'`)
	g := newSuperSandboxGuest()
	superSandboxReconcile(t, superSandboxDispatcher(h, g), v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "needs_attention" || v.Failure == "" || len(g.starts) != 0 || v.Tasks[0].JobID != "" {
		t.Fatalf("queued command assigned to replacement worker: %+v", v)
	}
}

func TestSuperSandboxSingleConnectionAndPersistedClaimRestart(t *testing.T) {
	h, actor := superSandboxFixtureConnections(t, 1)
	v := superSandboxCreate(t, h, actor, superSandboxInput("single-connection", 1))
	// Crash boundary: claim commits the request/handle, but no remote RPC ran.
	claim, err := (&SuperSandboxDispatcher{Server: h}).claim(context.Background(), "ws", v.ID)
	if err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	g := newSuperSandboxGuest()
	superSandboxReconcile(t, superSandboxDispatcher(h, g), v.ID)
	if len(g.starts) != 1 || !reflect.DeepEqual(g.starts[0], claim.Request) {
		t.Fatalf("restart did not resume committed request: %+v %+v", claim, g.starts)
	}
	g.finish(claim.Request.ID, "failed", 7)
	superSandboxReconcile(t, superSandboxDispatcher(h, g), v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "failed" || v.Tasks[0].ExitCode == nil || *v.Tasks[0].ExitCode != 7 || v.FinishedAt == nil {
		t.Fatalf("failure receipt: %+v", v)
	}
	g.readError = true
	starts := len(g.starts)
	if err := superSandboxDispatcher(h, g).observe(context.Background(), "ws", v.ID, *claim); err != nil {
		t.Fatal(err)
	}
	if len(g.starts) != starts {
		t.Fatal("stale replica replayed an already-terminal claim")
	}
}

func TestSuperSandboxJobStateAndToolSurface(t *testing.T) {
	zero, one := 0, 1
	for _, tt := range []struct {
		status string
		code   *int
		state  string
		done   bool
	}{
		{"completed", &zero, "succeeded", true}, {"completed", &one, "failed", true},
		{"completed", nil, "unknown", false},
		{"failed", nil, "failed", true}, {"timed_out", nil, "failed", true},
		{"cancelled", nil, "cancelled", true}, {"running", nil, "running", false},
		{"starting", nil, "starting", false}, {"unknown", nil, "unknown", false},
		{"not_found", nil, "unknown", false}, {"invented-terminal", nil, "unknown", false},
	} {
		state, done := superSandboxJobState(wrencomputer.Job{Status: tt.status, ExitCode: tt.code})
		if state != tt.state || done != tt.done {
			t.Fatalf("%s: state=%s done=%v", tt.status, state, done)
		}
	}
	for _, suffix := range []string{"create", "list", "inspect", "cancel"} {
		name := "canter_super_sandbox_" + suffix
		tool, ok := sandboxToolByName(name)
		if !ok || tool.InputSchema == nil {
			t.Fatalf("tool not discoverable: %s", name)
		}
		policy, ok := operatorPolicy(name)
		if !ok {
			t.Fatalf("missing policy: %s", name)
		}
		write := suffix == "create" || suffix == "cancel"
		if sandboxMutatingTools[name] != write || policy.RequireDraft != write || policy.RetrySafe == write {
			t.Fatalf("bad policy %s: %+v", name, policy)
		}
		v := SuperSandbox{ID: "ssb_abc-_", WorkspaceID: "ws"}
		surface, ok := sandboxResultSurface(name, v, "ws")
		if !ok || validateOperatorSurface(&surface) != nil {
			t.Fatalf("bad surface: %+v %v", surface, ok)
		}
		if write && surface.Kind != "super-sandbox" {
			t.Fatalf("bad detail: %+v", surface)
		}
	}
	for _, id := range []string{"", "ssb_", "sbx_abc", "ssb_a/b", "ssb_a b", "ssb_a\n", strings.Repeat("a", 161)} {
		if validateOperatorSurface(&OperatorSurface{Kind: "super-sandbox", ID: id}) == nil {
			t.Fatalf("invalid surface ID accepted: %q", id)
		}
	}
	if _, ok := sandboxResultSurface("canter_super_sandbox_inspect", SuperSandbox{ID: "ssb_abc", WorkspaceID: "other"}, "ws"); ok {
		t.Fatal("cross-organization surface projected")
	}
}

func TestSuperSandboxBase64URLIDRoundTrip(t *testing.T) {
	h, _ := superSandboxFixture(t)
	const id = "ssb_a-b_c"
	// Deterministic coverage: do not rely on a randomly generated newID
	// containing both URL-safe punctuation characters on any particular run.
	superSandboxExec(t, h, `INSERT INTO super_sandboxes(id,workspace_id,creator_account_id,name,max_concurrency,max_worker_seconds,reserved_worker_seconds,key_hash,request_hash) VALUES($1,'ws','owner','Base64 URL ID',1,300,300,'base64-key','base64-payload')`, id)
	superSandboxExec(t, h, `INSERT INTO super_sandbox_workers(run_id,worker_id,sandbox_id,generation,worker_index) VALUES($1,'sbx_one','sbx_one',1,0)`, id)
	superSandboxExec(t, h, `INSERT INTO super_sandbox_tasks(id,run_id,task_index,name,command,directory,timeout_seconds) VALUES('sst_a-b_c',$1,0,'Task','true','/workspace',300)`, id)
	v := superSandboxRead(t, h, id)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SuperSandbox
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded.ID != id || decoded.Tasks[0].ID != "sst_a-b_c" {
		t.Fatalf("base64url round trip: %+v %v", decoded, err)
	}
	surface, ok := sandboxResultSurface("canter_super_sandbox_inspect", decoded, "ws")
	if !ok || surface.ID != id || validateOperatorSurface(&surface) != nil {
		t.Fatalf("base64url surface rejected: %+v %v", surface, ok)
	}
}

func TestSuperSandboxDeletionForeignKeysAndCreatorWithdrawal(t *testing.T) {
	h, actor := superSandboxFixture(t)
	ctx := context.Background()
	// Assert catalog actions instead of executing DELETE or TRUNCATE, even in
	// this private schema. Creator removal must retain active job evidence;
	// actual workspace removal cascades children; sandbox cleanup detaches only
	// the live link, preserving immutable worker identity and task receipts.
	rows, err := h.service.Store.pool.Query(ctx, `
	SELECT c.conrelid::regclass::text,
	  (SELECT string_agg(a.attname,',' ORDER BY k.ord)
	   FROM unnest(c.conkey) WITH ORDINALITY AS k(num,ord)
	   JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num),
	  c.confrelid::regclass::text,c.confdeltype::text
	FROM pg_constraint c WHERE c.contype='f'
	  AND c.conrelid IN ('super_sandboxes'::regclass,'super_sandbox_workers'::regclass,'super_sandbox_tasks'::regclass)`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var table, columns, target, action string
		if err = rows.Scan(&table, &columns, &target, &action); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got[table+"."+columns] = target + ":" + action
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"super_sandboxes.workspace_id":         "workspaces:c",
		"super_sandboxes.creator_account_id":   "accounts:n",
		"super_sandbox_workers.run_id":         "super_sandboxes:c",
		"super_sandbox_workers.sandbox_id":     "sandboxes:n",
		"super_sandbox_tasks.run_id":           "super_sandboxes:c",
		"super_sandbox_tasks.run_id,worker_id": "super_sandbox_workers:c",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deletion FK actions:\ngot  %v\nwant %v", got, want)
	}
	var nullable bool
	if err = h.service.Store.pool.QueryRow(ctx, `SELECT NOT attnotnull FROM pg_attribute WHERE attrelid='super_sandboxes'::regclass AND attname='creator_account_id'`).Scan(&nullable); err != nil || !nullable {
		t.Fatalf("creator is not nullable: %v %v", nullable, err)
	}
	in := superSandboxInput("removed-creator", 3)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	ownedJobID := superSandboxRead(t, h, v.ID).Tasks[0].JobID
	// Simulate ON DELETE SET NULL without deleting an account/table.
	superSandboxExec(t, h, `UPDATE super_sandboxes SET creator_account_id=NULL WHERE id=$1`, v.ID)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "failed" || v.FinishedAt == nil || v.Failure == "" || len(v.Tasks) != 3 || len(g.starts) != 1 || !reflect.DeepEqual(g.stops, []string{ownedJobID}) {
		t.Fatalf("removed creator did not retain/drain owned work: %+v starts=%v stops=%v", v, g.starts, g.stops)
	}
	for _, task := range v.Tasks {
		if task.State != "cancelled" {
			t.Fatalf("creator withdrawal left work pending: %+v", task)
		}
	}
	// A physically removed worker's detached link cannot be silently rebound.
	w := superSandboxCreate(t, h, actor, superSandboxInput("removed-worker-link", 1))
	superSandboxExec(t, h, `UPDATE super_sandbox_workers SET sandbox_id=NULL WHERE run_id=$1 AND worker_id='sbx_one'`, w.ID)
	superSandboxReconcile(t, d, w.ID)
	w = superSandboxRead(t, h, w.ID)
	if w.State != "needs_attention" || len(w.WorkerIDs) != 2 || w.Tasks[0].JobID != "" || len(g.starts) != 1 {
		t.Fatalf("detached worker identity was lost/reassigned: %+v", w)
	}
}

func TestSuperSandboxCancelAfterFlagWithdrawal(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("cancel-disabled", 3)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	h.config.Sandboxes.Enabled = false
	for _, account := range []string{"viewer", "outsider", ""} {
		if _, err := h.CancelSuperSandbox(context.Background(), "ws", account, v.ID, actor); err == nil {
			t.Fatalf("disabled flag granted cancellation to %q", account)
		}
	}
	cancelled, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor)
	if err != nil || cancelled.State != "cancelling" {
		t.Fatalf("authorized cancellation blocked after flag withdrawal: %+v %v", cancelled, err)
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.FinishedAt == nil || len(g.starts) != 1 || len(g.stops) != 1 {
		t.Fatalf("disabled flag cancellation did not drain: %+v", v)
	}
}

func TestSuperSandboxMissingExitCodeRetainsReservation(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("missing-exit-code", 2)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	jobID, workerID := v.Tasks[0].JobID, v.Tasks[0].WorkerID
	g.mu.Lock()
	g.jobs[jobID] = wrencomputer.Job{ID: jobID, Status: "completed"}
	g.mu.Unlock()
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "needs_attention" || v.Tasks[0].State != "unknown" || v.Tasks[0].FinishedAt != nil || v.Tasks[1].State != "queued" {
		t.Fatalf("missing exit code freed an uncertain task: %+v", v)
	}
	var finished *time.Time
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT finished_at FROM sandbox_jobs WHERE job_id=$1`, jobID).Scan(&finished); err != nil || finished != nil {
		t.Fatalf("missing exit code closed job handle: %v %v", finished, err)
	}
	if busy, err := h.service.Store.superSandboxWorkerBusy(context.Background(), workerID); err != nil || !busy {
		t.Fatalf("missing exit code released worker: %v %v", busy, err)
	}
}

func TestSuperSandboxFairPollingAcrossThirtyThreeRuns(t *testing.T) {
	h, actor := superSandboxFixture(t)
	d := &SuperSandboxDispatcher{Server: h}
	var lastID string
	for i := 0; i < 33; i++ {
		in := superSandboxInput(fmt.Sprintf("fair-%d", i), 1)
		workspace, account, creator := "ws", "owner", actor
		if i%2 != 0 {
			workspace, account = "other", "outsider"
			creator = sdk.ActorRef{Kind: "human", ID: account}
			in.WorkerIDs, in.MaxConcurrency = []string{"sbx_other"}, 1
		}
		v, err := h.CreateSuperSandbox(context.Background(), workspace, account, creator, in)
		if err != nil {
			t.Fatal(err)
		}
		// Persisted unknown/attention resources stay unfinished, but must not
		// monopolize the bounded poll window across organizations.
		superSandboxExec(t, h, `UPDATE super_sandboxes SET state='needs_attention',failure='Waiting for worker observation',created_at='2020-01-01'::timestamptz+($2::int*interval '1 second') WHERE id=$1`, v.ID, i)
		lastID = v.ID
	}
	first, err := d.pending(context.Background())
	if err != nil || len(first) != 32 {
		t.Fatalf("bounded pending window: %d %v", len(first), err)
	}
	for i, item := range first {
		if item[1] == lastID {
			t.Fatal("deterministic initial ordering unexpectedly included run 33")
		}
		if i == 0 {
			// A failed attempt must still rotate to the back, even when its
			// original context is cancelled. No remote RPCs are necessary.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err = d.Reconcile(ctx, item[0], item[1]); !errors.Is(err, context.Canceled) {
				t.Fatalf("expected failed attempt, got %v", err)
			}
		} else if err = d.Reconcile(context.Background(), item[0], item[1]); err != nil {
			t.Fatal(err)
		}
	}
	next, err := d.pending(context.Background())
	if err != nil || len(next) != 32 || next[0][1] != lastID {
		t.Fatalf("run 33 starved after successful and failed attempts: %+v %v", next, err)
	}
}

func TestSuperSandboxHTTPReadWriteAndBodyLimits(t *testing.T) {
	h, actor := superSandboxFixture(t)
	v := superSandboxCreate(t, h, actor, superSandboxInput("http", 1))
	viewer := Principal{Account: &Account{ID: "viewer"}, Actor: sdk.ActorRef{Kind: "human", ID: "viewer"}}
	// The parent workspace router supplies the membership gate; exercise it
	// explicitly here before using the resource handler.
	read := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/super-sandboxes", nil)
	if err := h.allowWorkspace(read, viewer, "ws", false); err != nil {
		t.Fatal(err)
	}
	for _, parts := range [][]string{nil, {v.ID}} {
		w := httptest.NewRecorder()
		h.workspaceSuperSandboxes(w, read, viewer, "ws", parts)
		if w.Code != http.StatusOK {
			t.Fatalf("viewer read: %d %s", w.Code, w.Body.String())
		}
	}
	if err := h.allowWorkspace(read, viewer, "other", false); err == nil {
		t.Fatal("cross-organization member gate allowed read")
	}
	w := httptest.NewRecorder()
	h.workspaceSuperSandboxes(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)), viewer, "ws", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer POST: %d", w.Code)
	}
	owner := Principal{Account: &Account{ID: "owner"}, Actor: actor}
	w = httptest.NewRecorder()
	body := `{"name":"` + strings.Repeat("x", 256<<10) + `"}`
	h.workspaceSuperSandboxes(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), owner, "ws", nil)
	if w.Code < 400 {
		t.Fatalf("oversized body accepted: %d", w.Code)
	}
	raw, _ := json.Marshal(map[string]string{"workspaceId": "other", "id": v.ID})
	value, err := h.callSandboxTool(context.Background(), "ws", "owner", actor, true, "canter_super_sandbox_inspect", raw)
	if err != nil || value.(SuperSandbox).WorkspaceID != "ws" {
		t.Fatalf("payload overrode conversation scope: %v %v", value, err)
	}
	if _, err = h.callSandboxTool(context.Background(), "ws", "outsider", actor, false, "canter_super_sandbox_inspect", raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member tool read: %v", err)
	}
	// Passive GET/tool reads must not create sandbox job rows.
	var jobs int
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandbox_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("reads dispatched work: jobs=%d err=%v", jobs, err)
	}
}

type superSandboxPoolTestClient struct {
	sandboxAgentClient
	close func()
}

func (c *superSandboxPoolTestClient) Close() { c.close() }

func TestSuperSandboxRunIsolatesSlowBatchesAndJoinsWorkers(t *testing.T) {
	h, actor := superSandboxFixture(t)
	var healthyID string
	for i := 0; i < 8; i++ {
		workspace, account, worker := "ws", "owner", "sbx_one"
		if i == 1 {
			workspace, account, worker = "other", "outsider", "sbx_other"
		} else if i > 1 {
			worker = fmt.Sprintf("sbx_pool_%d", i)
			superSandboxExec(t, h, `INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase) VALUES($1,'ws','Pool worker','free-1x1','free','ready')`, worker)
		}
		in := superSandboxInput(fmt.Sprintf("pool-%d", i), 1)
		in.WorkerIDs, in.MaxConcurrency = []string{worker}, 1
		run, err := h.CreateSuperSandbox(context.Background(), workspace, account, actor, in)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			healthyID = run.ID
		}
		superSandboxExec(t, h, `UPDATE super_sandboxes SET created_at='2020-01-01'::timestamptz+($2::int*interval '1 second') WHERE id=$1`, run.ID, i)
	}
	var mu sync.Mutex
	active, maximum, blocked, exited := 0, 0, 0, 0
	byWorker := map[string]int{}
	duplicate := false
	firstBlocked := make(chan struct{})
	full := make(chan struct{}, 1)
	g := newSuperSandboxGuest()
	d := &SuperSandboxDispatcher{Server: h}
	d.client = func(ctx context.Context, worker Sandbox) (sandboxAgentClient, error) {
		mu.Lock()
		active++
		byWorker[worker.ID]++
		if byWorker[worker.ID] > 1 {
			duplicate = true
		}
		if active > maximum {
			maximum = active
		}
		release := func() {
			mu.Lock()
			active--
			byWorker[worker.ID]--
			mu.Unlock()
		}
		if worker.ID == "sbx_other" {
			mu.Unlock()
			return &superSandboxPoolTestClient{sandboxAgentClient: g, close: release}, nil
		}
		blocked++
		if worker.ID == "sbx_one" {
			close(firstBlocked)
		}
		if blocked >= 4 {
			select {
			case full <- struct{}{}:
			default:
			}
		}
		mu.Unlock()
		<-ctx.Done()
		release()
		mu.Lock()
		exited++
		mu.Unlock()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(cancel)
	select {
	case <-firstBlocked:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("first batch never reached blocked guest")
	}
	select {
	case <-full:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("healthy batch failed to free its slot while first was blocked")
	}
	healthy, err := h.service.Store.SuperSandbox(context.Background(), "other", healthyID)
	if err != nil || healthy.Tasks[0].State != "running" {
		cancel()
		t.Fatalf("other organization's batch did not progress: %+v %v", healthy, err)
	}
	// Leave the pool full across a polling tick. No extra local reconcile of
	// the blocked runs may begin, and shutdown must cancel/join all four.
	time.Sleep(2100 * time.Millisecond)
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not cancel and join its blocked workers")
	}
	mu.Lock()
	defer mu.Unlock()
	if maximum != 4 || active != 0 || blocked != 4 || exited != blocked || duplicate {
		t.Fatalf("pool isolation/join failed: max=%d active=%d blocked=%d exited=%d duplicate=%v", maximum, active, blocked, exited, duplicate)
	}
}

func TestSuperSandboxWorkerRecipeEligibility(t *testing.T) {
	h, actor := superSandboxFixture(t)
	ctx := context.Background()
	input := func(key string, workers ...string) SuperSandboxCreateInput {
		in := superSandboxInput(key, 1)
		in.WorkerIDs = workers
		in.MaxConcurrency = len(workers)
		return in
	}

	superSandboxExec(t, h, `UPDATE sandboxes SET environment='{"env":{"MODE":"test-mode-value"}}' WHERE id='sbx_one'`)
	_, err := h.CreateSuperSandbox(ctx, "ws", "owner", actor, input("recipe-env-worker", "sbx_one"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("env worker accepted or failed differently: %v", err)
	}
	if strings.Contains(err.Error(), "test-mode-value") {
		t.Fatalf("recipe env value leaked into error: %v", err)
	}

	superSandboxExec(t, h, `UPDATE sandboxes SET environment='{"secretRefs":{"TOKEN":"sec_test"}}' WHERE id='sbx_one'`)
	_, err = h.CreateSuperSandbox(ctx, "ws", "owner", actor, input("recipe-secret-worker", "sbx_one"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("secretRefs worker accepted or failed differently: %v", err)
	}
	if strings.Contains(err.Error(), "sec_test") {
		t.Fatalf("secret reference leaked into error: %v", err)
	}

	superSandboxExec(t, h, `UPDATE sandboxes SET environment='{}', setup='{"status":"pending"}' WHERE id='sbx_one'`)
	if _, err = h.CreateSuperSandbox(ctx, "ws", "owner", actor, input("setup-pending-worker", "sbx_one")); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "workers must finish setup before joining a Super Sandbox") {
		t.Fatalf("pending setup worker accepted or failed differently: %v", err)
	}

	superSandboxExec(t, h, `UPDATE sandboxes SET environment='{"repository":{"url":"https://example.test/repo.git","ref":"main"}}', setup='{"status":"succeeded"}' WHERE id='sbx_one'`)
	if _, err = h.CreateSuperSandbox(ctx, "ws", "owner", actor, input("repository-setup-worker", "sbx_one", "sbx_two")); err != nil {
		t.Fatalf("completed repository-only setup worker rejected: %v", err)
	}
}
