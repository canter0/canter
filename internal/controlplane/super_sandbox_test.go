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
	"sync/atomic"
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
	// A new dispatcher (process restart), with lookup unavailable, must NOT
	// replay: a read error is uncertainty, never positive absence.
	g.lost = false
	g.readError = true
	d = superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if len(g.starts) != 1 || v.Tasks[0].State != "unknown" {
		t.Fatalf("read error replayed or released uncertain claim: %+v %+v", g.starts, v)
	}
	// Positive absence of the exact persisted ID replays identical
	// ID+command+directory+timeout+env+sandbox flag, never a new task.
	g.readError = false
	g.mu.Lock()
	delete(g.jobs, first.ID)
	g.mu.Unlock()
	superSandboxReconcile(t, d, v.ID)
	if len(g.starts) != 2 || !reflect.DeepEqual(first, g.starts[1]) || len(g.requests) != 1 {
		t.Fatalf("replay changed persisted request: %+v", g.starts)
	}
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

// A typed pre-launch rejection persists the close mode, sends the rejection
// stop on a later pass, and never retries the launch.
func TestSuperSandboxRejectedStartClosesWithoutRetries(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("rejected-start", 2)
	in.MaxConcurrency = 1
	in.Tasks[0].Directory = "missing"
	v := superSandboxCreate(t, h, actor, in)
	g := &superSandboxRejectingGuest{superSandboxGuest: newSuperSandboxGuest()}
	d := superSandboxDispatcher(h, g.superSandboxGuest)
	d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return g, nil }
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if g.rejected != 1 || v.Tasks[0].State != "unknown" || v.Tasks[0].Reason != "start_rejected" {
		t.Fatalf("first rejection not recorded: rejects=%d %+v", g.rejected, v.Tasks[0])
	}
	var mode string
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT COALESCE(resolution_mode,'') FROM super_sandbox_tasks WHERE id=$1`, v.Tasks[0].ID).Scan(&mode); err != nil || mode != "close_rejected_start" {
		t.Fatalf("close mode not persisted: %q %v", mode, err)
	}
	if v.Tasks[1].State != "queued" || len(g.starts) != 0 {
		t.Fatalf("rejection launched or dropped pending work: %+v", v)
	}
	// A lost close receipt keeps the reservation; only the close retries.
	g.stopError = true
	superSandboxReconcile(t, d, v.ID)
	if g.rejected != 1 || len(g.stops) != 1 || g.stops[0] != v.Tasks[0].JobID {
		t.Fatalf("close retried the launch or stopped another ID: rejects=%d stops=%v", g.rejected, g.stops)
	}
	if busy, err := h.service.Store.superSandboxWorkerBusy(context.Background(), v.Tasks[0].WorkerID); err != nil || !busy {
		t.Fatalf("lost close receipt released worker: %v %v", busy, err)
	}
	g.stopError = false
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if g.rejected != 1 || v.Tasks[0].State != "failed" || v.Tasks[0].Reason != "start_rejected" || v.Tasks[0].Error != "Start rejected; job closed" {
		t.Fatalf("rejection close did not settle: %+v", v.Tasks[0])
	}
	g.finish(v.Tasks[1].JobID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "failed" || v.FinishedAt == nil || v.Tasks[1].State != "succeeded" {
		t.Fatalf("batch did not settle after close: %+v", v)
	}
}

type superSandboxInterruptingGuest struct {
	*superSandboxGuest
	interrupted bool
}

func (g *superSandboxInterruptingGuest) Job(ctx context.Context, id string) (wrencomputer.Job, error) {
	if g.interrupted {
		return wrencomputer.Job{ID: id, Status: "interrupted"}, nil
	}
	return g.superSandboxGuest.Job(ctx, id)
}

func (g *superSandboxInterruptingGuest) StopJob(ctx context.Context, id string) (wrencomputer.Job, error) {
	if g.interrupted {
		return wrencomputer.Job{ID: id, Status: "interrupted"}, nil
	}
	return g.superSandboxGuest.StopJob(ctx, id)
}

type superSandboxRejectingGuest struct {
	*superSandboxGuest
	rejected int
}

func (g *superSandboxRejectingGuest) StartJob(ctx context.Context, r wrencomputer.JobRequest) (wrencomputer.Job, error) {
	if !strings.Contains(r.Directory, "missing") {
		return g.superSandboxGuest.StartJob(ctx, r)
	}
	g.mu.Lock()
	g.rejected++
	g.mu.Unlock()
	return wrencomputer.Job{}, &wrencomputer.Error{Code: "invalid_directory", Message: "directory does not exist"}
}

// A repeated interrupted receipt is explicitly unresolved: the reservation
// is never released by that evidence class.
func TestSuperSandboxInterruptedKeepsReservation(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("interrupted", 2)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	interrupt := &superSandboxInterruptingGuest{superSandboxGuest: g}
	d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return interrupt, nil }
	for i := 0; i < 2; i++ {
		interrupt.interrupted = true
		superSandboxReconcile(t, d, v.ID)
	}
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" || v.Tasks[0].Reason != "worker_interrupted" || v.Tasks[1].State != "queued" || v.FinishedAt != nil {
		t.Fatalf("interrupted released or settled work: %+v", v)
	}
	if busy, err := h.service.Store.superSandboxWorkerBusy(context.Background(), v.Tasks[0].WorkerID); err != nil || !busy {
		t.Fatalf("interrupted released worker: %v %v", busy, err)
	}
	// Cancellation with a still-interrupted job stays cancelling with the
	// reason visible; it is not promised a stop time.
	if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor); err != nil {
		t.Fatal(err)
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "cancelling" || v.FinishedAt != nil || v.Tasks[1].State != "cancelled" || v.Tasks[0].Reason != "worker_interrupted" {
		t.Fatalf("interrupted stop forced resolution: %+v", v)
	}
	interrupt.interrupted = false
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "cancelled" || v.FinishedAt == nil || v.Tasks[0].State != "cancelled" {
		t.Fatalf("terminal stop receipt ignored: %+v", v)
	}
}

// One unreachable worker must not hide a peer's already-finished job.
func TestSuperSandboxDeadlinedObservationLeavesPeerVisible(t *testing.T) {
	h, actor := superSandboxFixture(t)
	v := superSandboxCreate(t, h, actor, superSandboxInput("deadline-peer", 2))
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	d.observeTimeout = 200 * time.Millisecond
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "running" || v.Tasks[1].State != "running" {
		t.Fatalf("expected two running tasks: %+v", v.Tasks)
	}
	blocked := v.Tasks[0].WorkerID
	g.finish(v.Tasks[1].JobID, "completed", 0)
	d.client = func(ctx context.Context, worker Sandbox) (sandboxAgentClient, error) {
		if worker.ID == blocked {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return g, nil
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" || v.Tasks[0].Reason != "worker_unobservable" {
		t.Fatalf("deadlined worker not marked uncertain: %+v", v.Tasks[0])
	}
	if v.Tasks[1].State != "succeeded" || v.Tasks[1].FinishedAt == nil {
		t.Fatalf("peer completion hidden by deadlined worker: %+v", v.Tasks[1])
	}
}

// Confirmed removal evidence settles the outstanding task; a phase or
// generation change alone never releases the reservation.
func TestSuperSandboxWorkerRemovalEvidence(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("removed-worker", 1)
	in.WorkerIDs, in.MaxConcurrency = []string{"sbx_one"}, 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	jobID, generation := v.Tasks[0].JobID, v.Tasks[0].WorkerID
	// A synthetic generation bump is not evidence: nothing settles.
	superSandboxExec(t, h, `UPDATE sandboxes SET generation=generation+1 WHERE id=$1`, generation)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" || v.Tasks[0].Reason != "generation_changed" || len(g.starts) != 1 {
		t.Fatalf("synthetic generation bump released task: %+v", v.Tasks[0])
	}
	if busy, err := h.service.Store.superSandboxWorkerBusy(context.Background(), generation); err != nil || !busy {
		t.Fatalf("synthetic generation bump released worker: %v %v", busy, err)
	}
	// A failed/unfinished destroy (no evidence row) releases nothing either.
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" {
		t.Fatalf("missing evidence settled task: %+v", v.Tasks[0])
	}
	// Confirmed settle of the ORIGINAL generation writes evidence exactly once;
	// repeated writes are idempotent. The fence rejects a mismatched view.
	sd := &SandboxDispatcher{Server: h}
	mismatched := &Sandbox{ID: generation, WorkspaceID: "ws", Generation: 1, observedPhase: "ready"}
	if err := sd.settleStoppedSandbox(context.Background(), mismatched); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale generation settled evidence: %v", err)
	}
	superSandboxExec(t, h, `UPDATE sandboxes SET generation=1 WHERE id=$1`, generation)
	removed := &Sandbox{ID: generation, WorkspaceID: "ws", Generation: 1, observedPhase: "ready"}
	if err := sd.settleStoppedSandbox(context.Background(), removed); err != nil {
		t.Fatal(err)
	}
	if err := sd.settleStoppedSandbox(context.Background(), removed); err != nil {
		t.Fatalf("repeated settle not idempotent: %v", err)
	}
	var confirmed int
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandbox_destroyed_generations WHERE sandbox_id=$1 AND generation=1`, generation).Scan(&confirmed); err != nil || confirmed != 1 {
		t.Fatalf("evidence rows=%d err=%v", confirmed, err)
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "outcome_unavailable" || v.Tasks[0].Reason != "worker_removed" || v.Tasks[0].JobID != jobID || v.Tasks[0].ExitCode != nil {
		t.Fatalf("removal evidence not applied: %+v", v.Tasks[0])
	}
	var finished *time.Time
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT finished_at FROM sandbox_jobs WHERE job_id=$1`, jobID).Scan(&finished); err != nil || finished == nil {
		t.Fatalf("removal did not close job handle: %v %v", finished, err)
	}
	if v.State != "failed" || v.FinishedAt == nil || v.Failure == "" || v.BlockedReason != "" {
		t.Fatalf("removed-worker batch not settled as failed: %+v", v)
	}
	if len(g.starts) != 1 {
		t.Fatal("settled outcome replayed an old job")
	}
	// Only the confirmed generation carries evidence: a replacement row gets
	// nothing from the stale generation.
	in.IdempotencyKey = "after-removal"
	in.WorkerIDs = []string{"sbx_two"}
	next := superSandboxCreate(t, h, actor, in)
	superSandboxReconcile(t, d, next.ID)
	next = superSandboxRead(t, h, next.ID)
	if next.Tasks[0].State != "running" || next.Tasks[0].WorkerID != "sbx_two" {
		t.Fatalf("healthy replacement worker could not be claimed: %+v", next.Tasks[0])
	}
}

// An unknown task blocks a second claim on a DIFFERENT worker under the run
// lock, including after a dispatcher restart, without blocking observation or
// cancellation of an already-running peer.
func TestSuperSandboxUnknownBlocksCrossWorkerClaim(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("cross-worker-block", 3)
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "running" || v.Tasks[1].State != "running" {
		t.Fatalf("expected two running tasks: %+v", v.Tasks)
	}
	g.unknown = true
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" || v.Tasks[1].State != "unknown" || v.Tasks[2].State != "queued" {
		t.Fatalf("uncertain receipts not isolated: %+v", v.Tasks)
	}
	g.unknown = false
	g.mu.Lock()
	g.jobs[v.Tasks[1].JobID] = wrencomputer.Job{ID: v.Tasks[1].JobID, Status: "interrupted"}
	g.mu.Unlock()
	g.finish(v.Tasks[0].JobID, "completed", 0)
	superSandboxReconcile(t, superSandboxDispatcher(h, g), v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "succeeded" || v.Tasks[1].State != "unknown" || v.Tasks[2].State != "queued" {
		t.Fatalf("peer completion blocked or third task claimed: %+v", v.Tasks)
	}
	g.mu.Lock()
	delete(g.jobs, v.Tasks[1].JobID)
	g.mu.Unlock()
	if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor); err != nil {
		t.Fatal(err)
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "cancelled" || v.FinishedAt == nil || v.Tasks[2].State != "cancelled" || len(g.stops) == 0 {
		t.Fatalf("uncertain batch could not drain: %+v stops=%v", v, g.stops)
	}
}

// Queued work on a sleeping worker reports a blocker, and waking the worker
// under a new generation never rewrites the original batch.
func TestSuperSandboxSleepingWorkerBlocker(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("sleeping-worker", 1)
	in.WorkerIDs, in.MaxConcurrency = []string{"sbx_one"}, 1
	v := superSandboxCreate(t, h, actor, in)
	superSandboxExec(t, h, `UPDATE sandboxes SET phase='asleep' WHERE id='sbx_one'`)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "queued" || v.BlockedReason != "worker_unavailable" || len(g.starts) != 0 {
		t.Fatalf("sleeping worker blocker missing: %+v", v)
	}
	// Waking as a NEW generation is a replacement, not a resumption.
	superSandboxExec(t, h, `UPDATE sandboxes SET phase='ready',generation=generation+1 WHERE id='sbx_one'`)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "needs_attention" || v.BlockedReason != "worker_replaced" || len(g.starts) != 0 {
		t.Fatalf("replacement generation reused for queued work: %+v", v)
	}
}

// Summary pages are workspace-scoped, keyset-ordered without task bodies, and
// reject malformed cursors.
func TestSuperSandboxSummaryPagination(t *testing.T) {
	h, actor := superSandboxFixture(t)
	for i := 0; i < 101; i++ {
		in := superSandboxInput(fmt.Sprintf("summary-%03d", i), 2)
		in.MaxConcurrency = 1
		in.WorkerIDs = []string{"sbx_one"}
		v, err := h.CreateSuperSandbox(context.Background(), "ws", "owner", actor, in)
		if err != nil {
			t.Fatal(err)
		}
		// Equal timestamps exercise the deterministic (created_at,id) tiebreak.
		superSandboxExec(t, h, `UPDATE super_sandboxes SET created_at='2020-01-01'::timestamptz WHERE id=$1`, v.ID)
	}
	foreign, err := h.CreateSuperSandbox(context.Background(), "other", "outsider", sdk.ActorRef{Kind: "human", ID: "outsider"}, func() SuperSandboxCreateInput {
		in := superSandboxInput("foreign-summary", 1)
		in.WorkerIDs, in.MaxConcurrency = []string{"sbx_other"}, 1
		return in
	}())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page1, err := h.service.Store.SuperSandboxSummaries(ctx, "ws", 100, nil)
	if err != nil || len(page1) != 100 {
		t.Fatalf("first page: %d %v", len(page1), err)
	}
	seen := map[string]bool{}
	var cursor *superSandboxCursor
	for i, row := range page1 {
		if row.WorkerCount != 1 || row.Progress.Total != 2 || row.Progress.Settled != 0 || row.State != "queued" {
			t.Fatalf("bad summary row: %+v", row)
		}
		if i > 0 && page1[i-1].CreatedAt.Before(row.CreatedAt) {
			t.Fatalf("created_at ordering violated at %d", i)
		}
		if seen[row.ID] {
			t.Fatalf("duplicate page row %s", row.ID)
		}
		seen[row.ID] = true
	}
	last1 := page1[len(page1)-1]
	cursor = &superSandboxCursor{CreatedAt: last1.CreatedAt, ID: last1.ID}
	page2, err := h.service.Store.SuperSandboxSummaries(ctx, "ws", 100, cursor)
	if err != nil || len(page2) != 1 || seen[page2[0].ID] {
		t.Fatalf("second page: %+v %v", page2, err)
	}
	for _, rows := range [][]SuperSandboxSummary{page1, page2} {
		for _, row := range rows {
			if row.ID == foreign.ID {
				t.Fatal("cross-workspace batch in summary page")
			}
		}
	}
	// Malformed cursor and out-of-range limit are rejected over HTTP.
	owner := Principal{Account: &Account{ID: "owner"}, Actor: actor}
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/super-sandboxes?view=summary&cursor=not-a-cursor", nil)
	w := httptest.NewRecorder()
	h.workspaceSuperSandboxes(w, req, owner, "ws", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed cursor accepted: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/super-sandboxes?view=summary&limit=101", nil)
	w = httptest.NewRecorder()
	h.workspaceSuperSandboxes(w, req, owner, "ws", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range limit accepted: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/super-sandboxes?view=summary&limit=2", nil)
	w = httptest.NewRecorder()
	h.workspaceSuperSandboxes(w, req, owner, "ws", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("summary request: %d %s", w.Code, w.Body.String())
	}
	var payload struct {
		SuperSandboxes []SuperSandboxSummary `json:"superSandboxes"`
		Enabled        bool                  `json:"enabled"`
		NextCursor     string                `json:"nextCursor"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.SuperSandboxes) != 2 || payload.NextCursor == "" || !payload.Enabled {
		t.Fatalf("summary payload: %+v", payload)
	}
	if payload.SuperSandboxes[0].ID == payload.SuperSandboxes[1].ID {
		t.Fatalf("duplicate rows in a single page: %+v", payload.SuperSandboxes)
	}
	if _, err = decodeSuperSandboxCursor(payload.NextCursor); err != nil {
		t.Fatalf("nextCursor does not decode: %v", err)
	}
	raw := w.Body.String()
	if strings.Contains(raw, "echo test") || strings.Contains(raw, `"tasks"`) || strings.Contains(raw, "jobId") {
		t.Fatalf("summary leaked task detail: %s", raw)
	}
	// Legacy list unchanged and never silently switched to summary rows.
	req = httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/super-sandboxes", nil)
	w = httptest.NewRecorder()
	h.workspaceSuperSandboxes(w, req, owner, "ws", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tasks"`) {
		t.Fatalf("legacy list changed: %d", w.Code)
	}
}

// The admission gate blocks new batch creation while leaving reconciliation
// and cancellation of existing batches fully operational.
func TestSuperSandboxAdmissionGate(t *testing.T) {
	h, actor := superSandboxFixture(t)
	h.config.Sandboxes.SuperSandboxAdmissionDisabled = true
	if _, err := h.CreateSuperSandbox(context.Background(), "ws", "owner", actor, superSandboxInput("admission-off", 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admission accepted while disabled: %v", err)
	}
	owner := Principal{Account: &Account{ID: "owner"}, Actor: actor}
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws/super-sandboxes?view=summary", nil)
	w := httptest.NewRecorder()
	h.workspaceSuperSandboxes(w, req, owner, "ws", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("summary reported enabled with admission off: %d %s", w.Code, w.Body.String())
	}
	h.config.Sandboxes.SuperSandboxAdmissionDisabled = false
	v := superSandboxCreate(t, h, actor, superSandboxInput("admission-on", 1))
	h.config.Sandboxes.SuperSandboxAdmissionDisabled = true
	if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor); err != nil {
		t.Fatalf("admission gate blocked cancellation: %v", err)
	}
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.FinishedAt == nil || v.State != "cancelled" {
		t.Fatalf("admission gate blocked drain: %+v", v)
	}
}

// A full worker slate where the earliest workers answer too slowly must not
// hide peer completions; once they answer again, the persisted jobs resume
// from their recorded state.
func TestSuperSandboxSixteenTasksSlowEarlyWorkers(t *testing.T) {
	h, actor := superSandboxFixture(t)
	slow := map[string]bool{}
	in := superSandboxInput("sixteen", 16)
	in.MaxConcurrency = 16
	in.WorkerIDs = nil
	for i := 0; i < 16; i++ {
		worker := fmt.Sprintf("sbx_w%d", i)
		superSandboxExec(t, h, `INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase) VALUES($1,'ws','W','free-1x1','free','ready')`, worker)
		if i < 2 {
			slow[worker] = true
		}
		in.WorkerIDs = append(in.WorkerIDs, worker)
	}
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	d.observeTimeout = 250 * time.Millisecond
	superSandboxReconcile(t, d, v.ID)
	// A pass commits at most 15 new claims; the last slot takes a second pass.
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "running" || v.Tasks[15].State != "running" {
		t.Fatalf("full slate did not claim: %+v", v.Tasks)
	}
	// Early workers now answer too slowly; the rest complete in the same pass.
	d.client = func(ctx context.Context, worker Sandbox) (sandboxAgentClient, error) {
		if slow[worker.ID] {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return g, nil
	}
	for _, task := range v.Tasks[2:] {
		g.finish(task.JobID, "completed", 0)
	}
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	settled := 0
	for _, task := range v.Tasks {
		if task.State == "succeeded" {
			settled++
		}
	}
	if settled != 14 || v.Tasks[0].State != "unknown" || v.Tasks[1].State != "unknown" || v.State != "needs_attention" {
		t.Fatalf("slow workers hid peer completions: %+v", v.Tasks)
	}
	// Healthy again: the recorded jobs are read, not restarted, then finish.
	d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return g, nil }
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "running" || v.Tasks[1].State != "running" || len(g.starts) != 16 {
		t.Fatalf("uncertain jobs replayed or lost: %+v starts=%d", v.Tasks, len(g.starts))
	}
	g.finish(v.Tasks[0].JobID, "completed", 0)
	g.finish(v.Tasks[1].JobID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "succeeded" || v.FinishedAt == nil {
		t.Fatalf("full slate did not finish: %+v", v)
	}
}

// The replay gate re-reads authority under the run lock after a blocked
// workflow Probe: cancellation or sibling uncertainty during the block must
// prevent the launch entirely.
func TestSuperSandboxMayStartRechecksBlockedProbe(t *testing.T) {
	t.Run("cancelled during probe", func(t *testing.T) {
		h, actor := superSandboxFixture(t)
		in := superSandboxInput("gate-cancel", 1)
		in.MaxConcurrency = 1
		v := superSandboxCreate(t, h, actor, in)
		g := newSuperSandboxGuest()
		probe := &superSandboxProbeGateGuest{superSandboxGuest: g, entered: make(chan struct{}, 4), resume: make(chan struct{})}
		d := superSandboxDispatcher(h, g)
		d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return probe, nil }
		done := make(chan error, 1)
		go func() { done <- d.Reconcile(context.Background(), "ws", v.ID) }()
		select {
		case <-probe.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("replay probe never reached guest")
		}
		if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", v.ID, actor); err != nil {
			t.Fatal(err)
		}
		close(probe.resume)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(g.starts) != 0 {
			t.Fatalf("launch issued after cancellation: %+v", g.starts)
		}
		v = superSandboxRead(t, h, v.ID)
		if v.State != "cancelling" || v.Tasks[0].State == "succeeded" {
			t.Fatalf("cancel during blocked probe misrecorded: %+v", v)
		}
	})
	t.Run("sibling unknown during probe", func(t *testing.T) {
		h, actor := superSandboxFixture(t)
		v := superSandboxCreate(t, h, actor, superSandboxInput("gate-sibling", 3))
		g := newSuperSandboxGuest()
		// Let the FIRST replay Probe through so task 0 launches; the second
		// (task 1's replay) blocks while we mark task 0 unknown.
		probe := &superSandboxProbeGateGuest{superSandboxGuest: g, entered: make(chan struct{}, 4), resume: make(chan struct{}), allow: 1}
		d := superSandboxDispatcher(h, g)
		d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return probe, nil }
		done := make(chan error, 1)
		go func() { done <- d.Reconcile(context.Background(), "ws", v.ID) }()
		select {
		case <-probe.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("second replay probe never reached guest")
		}
		v = superSandboxRead(t, h, v.ID)
		if v.Tasks[0].State != "running" || v.Tasks[1].State != "starting" {
			t.Fatalf("fixture did not reach one running one starting: %+v", v.Tasks)
		}
		superSandboxExec(t, h, `UPDATE super_sandbox_tasks SET state='unknown',reason='worker_interrupted' WHERE id=$1`, v.Tasks[0].ID)
		close(probe.resume)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		v = superSandboxRead(t, h, v.ID)
		if len(g.starts) != 1 || v.Tasks[1].State != "starting" || v.Tasks[2].State != "queued" {
			t.Fatalf("sibling uncertainty still allowed launch: %+v starts=%+v", v.Tasks, g.starts)
		}
	})
}

type superSandboxProbeGateGuest struct {
	*superSandboxGuest
	entered chan struct{}
	resume  chan struct{}
	allow   int
	mu2     sync.Mutex
}

func (g *superSandboxProbeGateGuest) Probe(ctx context.Context) (wrencomputer.Capabilities, error) {
	g.mu2.Lock()
	if g.allow > 0 {
		g.allow--
		g.mu2.Unlock()
		return g.superSandboxGuest.Probe(ctx)
	}
	g.mu2.Unlock()
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.resume:
		return g.superSandboxGuest.Probe(ctx)
	case <-ctx.Done():
		return wrencomputer.Capabilities{}, ctx.Err()
	}
}

// Ordinary failures report failed without removal copy; an ordinary stop
// endpoint cancelling jobs reports cancelled, never success.
func TestSuperSandboxAggregateAttribution(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("attribution-fail", 1)
	in.MaxConcurrency = 1
	v := superSandboxCreate(t, h, actor, in)
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	g.finish(v.Tasks[0].JobID, "failed", 3)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "failed" || v.Failure != "" || v.BlockedReason != "" || v.Tasks[0].Reason != "" {
		t.Fatalf("ordinary failure misattributed: %+v", v)
	}
	in.IdempotencyKey = "attribution-cancel"
	next := superSandboxCreate(t, h, actor, in)
	superSandboxReconcile(t, d, next.ID)
	next = superSandboxRead(t, h, next.ID)
	// The ordinary stop endpoint cancels the guest job outside the batch.
	g.finish(next.Tasks[0].JobID, "cancelled", 0)
	if _, err := h.CancelSuperSandbox(context.Background(), "ws", "owner", next.ID, actor); err != nil {
		t.Fatal(err)
	}
	superSandboxReconcile(t, d, next.ID)
	next = superSandboxRead(t, h, next.ID)
	if next.State != "cancelled" || next.Failure != "" || next.Tasks[0].State != "cancelled" {
		t.Fatalf("ordinary cancellation misreported: %+v", next)
	}
}

// Blocker derivation: no blocker on terminal rows or when a ready free worker
// exists; workers_busy only when eligible workers are all reserved.
func TestSuperSandboxBlockerDerivation(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("blocker-free", 2)
	in.WorkerIDs, in.MaxConcurrency = []string{"sbx_one"}, 1
	v := superSandboxCreate(t, h, actor, in)
	v = superSandboxRead(t, h, v.ID)
	if v.BlockedReason != "" {
		t.Fatalf("ready free worker reported blocker: %q", v.BlockedReason)
	}
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "running" || v.BlockedReason != "" {
		t.Fatalf("batch at its own concurrency limit reported blocker: %+v", v)
	}
	g.finish(v.Tasks[0].JobID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "succeeded" || v.Tasks[1].State != "running" || v.BlockedReason != "" {
		t.Fatalf("in-progress batch at concurrency reported blocker: %+v", v)
	}
	g.finish(v.Tasks[1].JobID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "succeeded" || v.BlockedReason != "" {
		t.Fatalf("terminal batch reported blocker: %+v", v)
	}
	// All eligible workers reserved by another run: busy, not unavailable.
	in.IdempotencyKey = "blocker-busy"
	next := superSandboxCreate(t, h, actor, in)
	other := superSandboxInput("blocker-busy-owner", 1)
	other.WorkerIDs, other.MaxConcurrency = []string{"sbx_one"}, 1
	owner := superSandboxCreate(t, h, actor, other)
	superSandboxReconcile(t, d, owner.ID)
	owner = superSandboxRead(t, h, owner.ID)
	if owner.Tasks[0].State != "running" {
		t.Fatalf("owner task not running: %+v", owner.Tasks[0])
	}
	superSandboxReconcile(t, d, next.ID)
	next = superSandboxRead(t, h, next.ID)
	if next.Tasks[0].State != "queued" || next.BlockedReason != "workers_busy" {
		t.Fatalf("reserved worker slate not reported busy: %+v", next)
	}
}

type superSandboxCloseCompletedGuest struct {
	*superSandboxGuest
}

// StopJob returns a completed receipt without an exit code, the invalid
// terminal write that must hold rather than release inside a rejection close.
func (g *superSandboxCloseCompletedGuest) StopJob(_ context.Context, id string) (wrencomputer.Job, error) {
	return wrencomputer.Job{ID: id, Status: "completed"}, nil
}

// A rejection close that reports completed-without-exit-code stays active as
// missing_exit_code: the close keeps its mode, the reservation holds, and the
// next pass still only retries the close.
func TestSuperSandboxRejectionCloseCompletedWithoutExitCodeHolds(t *testing.T) {
	h, actor := superSandboxFixture(t)
	in := superSandboxInput("close-completed-no-code", 1)
	in.Tasks[0].Directory = "missing"
	v := superSandboxCreate(t, h, actor, in)
	g := &superSandboxRejectingGuest{superSandboxGuest: newSuperSandboxGuest()}
	d := superSandboxDispatcher(h, g.superSandboxGuest)
	d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return g, nil }
	superSandboxReconcile(t, d, v.ID)
	closer := &superSandboxCloseCompletedGuest{superSandboxGuest: g.superSandboxGuest}
	d.client = func(context.Context, Sandbox) (sandboxAgentClient, error) { return closer, nil }
	for i := 0; i < 2; i++ {
		superSandboxReconcile(t, d, v.ID)
	}
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "unknown" || v.Tasks[0].Reason != "missing_exit_code" {
		t.Fatalf("completed-without-code close released: %+v", v.Tasks[0])
	}
	if v.FinishedAt != nil || v.State == "failed" {
		t.Fatalf("batch wrongly finished from invalid close receipt: %+v", v)
	}
	var mode string
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT COALESCE(resolution_mode,'') FROM super_sandbox_tasks WHERE id=$1`, v.Tasks[0].ID).Scan(&mode); err != nil || mode != "close_rejected_start" {
		t.Fatalf("invalid receipt cleared close mode: %q %v", mode, err)
	}
	if busy, err := h.service.Store.superSandboxWorkerBusy(context.Background(), v.Tasks[0].WorkerID); err != nil || !busy {
		t.Fatalf("invalid close receipt released worker: %v %v", busy, err)
	}
}

// Delayed destruction evidence arriving after a terminal receipt must not
// rewrite the task, close the already-settled job, or fabricate run failure.
func TestSuperSandboxStaleRemovalCannotRewriteSuccess(t *testing.T) {
	h, actor := superSandboxFixture(t)
	v := superSandboxCreate(t, h, actor, superSandboxInput("stale-removal", 1))
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	g.finish(v.Tasks[0].JobID, "completed", 0)
	superSandboxReconcile(t, d, v.ID)
	v = superSandboxRead(t, h, v.ID)
	if v.State != "succeeded" || v.Tasks[0].State != "succeeded" {
		t.Fatalf("expected succeeded batch, got %+v", v)
	}
	var generation int
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT generation FROM super_sandbox_tasks WHERE id=$1`, v.Tasks[0].ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	superSandboxExec(t, h, `INSERT INTO sandbox_destroyed_generations(workspace_id,sandbox_id,generation,confirmed_at) VALUES('ws',$1,$2,now())`, v.Tasks[0].WorkerID, generation)
	if err := d.observe(context.Background(), "ws", v.ID, superSandboxClaim{TaskID: v.Tasks[0].ID, WorkerID: v.Tasks[0].WorkerID, Generation: generation, Request: wrencomputer.JobRequest{ID: v.Tasks[0].JobID}}); err != nil {
		t.Fatal(err)
	}
	v = superSandboxRead(t, h, v.ID)
	if v.Tasks[0].State != "succeeded" || v.State != "succeeded" || v.Failure != "" {
		t.Fatalf("stale removal rewrote terminal state: %+v", v)
	}
}

// A failing queue scan backs off and retries; the run stays alive until
// cancel and then joins workers cleanly.
func TestSuperSandboxRunRetriesAfterQueueScanFailure(t *testing.T) {
	h, actor := superSandboxFixture(t)
	v := superSandboxCreate(t, h, actor, superSandboxInput("queue-retry", 1))
	g := newSuperSandboxGuest()
	d := superSandboxDispatcher(h, g)
	var calls int32
	d.pendingRuns = func(context.Context) ([][2]string, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, errors.New("queue scan unavailable")
		}
		return [][2]string{{"ws", v.ID}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.After(10 * time.Second)
	for {
		g.mu.Lock()
		started := len(g.starts)
		g.mu.Unlock()
		if started > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("queue retry never reached the guest")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("queue scan did not retry: calls=%d", calls)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not join workers after cancel")
	}
}
