package controlplane

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	pngpkg "image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

type sandboxFakeEngine struct {
	checkpointTestEngine
	destroyErr  error
	applies     atomic.Int64
	lastRequest sdk.VPSRequest
	destroyed   map[string]bool
}

func (e *sandboxFakeEngine) PlanVPS(ctx context.Context, workspace, id string, r sdk.VPSRequest) (sdk.VPSPlan, error) {
	e.lastRequest = r
	return sdk.VPSPlan{Spec: sdk.Spec{Metadata: sdk.Metadata{Name: id}, Spec: sdk.Desired{M1: sdk.M1Spec{Prefix: "workspaces/" + workspace + "/sandboxes/" + id}}}, Shape: compute.Shape{ID: "sandbox-shape", VCPU: r.VCPUs, Memory: r.MemoryMiB, GB: r.DiskGiB}, ImageID: "private-image", Networks: []string{"private-network"}}, nil
}

func (e *sandboxFakeEngine) ApplyVPS(ctx context.Context, plan sdk.VPSPlan) (sdk.State, error) {
	e.applies.Add(1)
	if e.destroyed[plan.Spec.Metadata.Name] {
		return sdk.State{}, fmt.Errorf("Recovery cannot recreate a destroyed server")
	}
	return e.checkpointTestEngine.ApplyVPS(ctx, plan)
}
func (e *sandboxFakeEngine) DestroyVPS(ctx context.Context, plan sdk.VPSPlan) (sdk.State, error) {
	if e.destroyErr != nil {
		return sdk.State{}, e.destroyErr
	}
	if e.destroyed == nil {
		e.destroyed = map[string]bool{}
	}
	e.destroyed[plan.Spec.Metadata.Name] = true
	return e.checkpointTestEngine.DestroyVPS(ctx, plan)
}

type sandboxFakeClient struct {
	capabilities   wrencomputer.Capabilities
	archive        []byte
	manifest       wrencomputer.CheckpointManifest
	restored       [][]byte
	jobs           map[string]wrencomputer.Job
	dialAddr       string
	closed         atomic.Bool
	failCheckpoint bool
	failRestore    bool
	startHook      func(context.Context, wrencomputer.JobRequest) (wrencomputer.Job, error)
	stopHook       func(context.Context, string) (wrencomputer.Job, error)
	exportHook     func() error
	logsHook       func(context.Context, string, string, int64, int) (wrencomputer.JobLogChunk, error)
	readHook       func(context.Context, string, int64, int, string) (wrencomputer.FileChunk, error)
}

func (c *sandboxFakeClient) OpenTerminal(context.Context, int, int) (*wrencomputer.Terminal, error) {
	return nil, fmt.Errorf("terminals are faked through sandboxTerminalOpen")
}
func (c *sandboxFakeClient) DialPort(_ context.Context, port int) (net.Conn, error) {
	return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func (c *sandboxFakeClient) Probe(context.Context) (wrencomputer.Capabilities, error) {
	return c.capabilities, nil
}
func (c *sandboxFakeClient) StartJob(ctx context.Context, r wrencomputer.JobRequest) (wrencomputer.Job, error) {
	if c.startHook != nil {
		return c.startHook(ctx, r)
	}
	if c.jobs == nil {
		c.jobs = map[string]wrencomputer.Job{}
	}
	job := wrencomputer.Job{ID: r.ID, Status: "completed", Output: "ran: " + r.Command}
	c.jobs[r.ID] = job
	return job, nil
}
func (c *sandboxFakeClient) Job(_ context.Context, id string) (wrencomputer.Job, error) {
	if job, ok := c.jobs[id]; ok {
		return job, nil
	}
	return wrencomputer.Job{}, ErrNotFound
}
func (c *sandboxFakeClient) StopJob(ctx context.Context, id string) (wrencomputer.Job, error) {
	if c.stopHook != nil {
		return c.stopHook(ctx, id)
	}
	job, err := c.Job(context.Background(), id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return job, err
	}
	if err != nil || job.Status == "starting" || job.Status == "running" {
		job = wrencomputer.Job{ID: id, Status: "cancelled"}
		if c.jobs == nil {
			c.jobs = map[string]wrencomputer.Job{}
		}
		c.jobs[id] = job
	}
	return job, nil
}
func (c *sandboxFakeClient) ReadFile(_ context.Context, path string, offset int64, limit int) (wrencomputer.FileChunk, error) {
	return wrencomputer.FileChunk{Path: path, Data: []byte("file-data"), Size: 9, EOF: true}, nil
}
func (c *sandboxFakeClient) WriteFile(_ context.Context, path string, data []byte) (wrencomputer.FileChunk, error) {
	return wrencomputer.FileChunk{Path: path, Size: int64(len(data))}, nil
}
func (c *sandboxFakeClient) ListFiles(_ context.Context, path string) ([]wrencomputer.FileEntry, error) {
	if strings.HasSuffix(path, ".txt") {
		return nil, &wrencomputer.Error{Code: "not-a-directory"}
	}
	return []wrencomputer.FileEntry{{Name: "main.py", Size: 12}}, nil
}
func (c *sandboxFakeClient) CreateCheckpoint(_ context.Context, id string, includeBrowser bool, keepStopped ...bool) ([]byte, wrencomputer.CheckpointManifest, error) {
	if c.failCheckpoint {
		return nil, wrencomputer.CheckpointManifest{}, fmt.Errorf("checkpoint output exceeded the 128 MiB unpacked limit")
	}
	return bytes.Clone(c.archive), c.manifest, nil
}
func (c *sandboxFakeClient) RestoreCheckpoint(_ context.Context, id string, data []byte, includeBrowser bool) error {
	if c.failRestore {
		return fmt.Errorf("restore failed")
	}
	c.restored = append(c.restored, bytes.Clone(data))
	return nil
}
func (c *sandboxFakeClient) Close() { c.closed.Store(true) }
func (c *sandboxFakeClient) ReadFileVersion(ctx context.Context, path string, offset int64, limit int, version string) (wrencomputer.FileChunk, error) {
	if c.readHook != nil {
		return c.readHook(ctx, path, offset, limit, version)
	}
	data := []byte("file-data")
	if offset > int64(len(data)) {
		return wrencomputer.FileChunk{}, ErrConflict
	}
	end := min(int64(len(data)), offset+int64(limit))
	return wrencomputer.FileChunk{Path: path, Version: "fixture-v1", Data: data[offset:end], Offset: offset, NextOffset: end, Size: int64(len(data)), EOF: end == int64(len(data))}, nil
}
func (c *sandboxFakeClient) JobLogs(ctx context.Context, id, stream string, offset int64, limit int) (wrencomputer.JobLogChunk, error) {
	if c.logsHook != nil {
		return c.logsHook(ctx, id, stream, offset, limit)
	}
	return wrencomputer.JobLogChunk{JobID: id, Stream: stream, Offset: offset, NextOffset: offset, Status: "completed", EOF: true}, nil
}
func (c *sandboxFakeClient) BeginUpload(_ context.Context, id, path string, size int64, sha256 string) (wrencomputer.FileUpload, error) {
	return wrencomputer.FileUpload{ID: id, Path: path, Size: size}, nil
}
func (c *sandboxFakeClient) UploadChunk(_ context.Context, id string, offset int64, data []byte) (wrencomputer.FileUpload, error) {
	return wrencomputer.FileUpload{ID: id, Offset: offset + int64(len(data))}, nil
}
func (c *sandboxFakeClient) CompleteUpload(_ context.Context, id string) (wrencomputer.FileUpload, error) {
	return wrencomputer.FileUpload{ID: id, Complete: true}, nil
}
func (c *sandboxFakeClient) AbortUpload(_ context.Context, id string) error { return nil }
func (c *sandboxFakeClient) Metrics(_ context.Context) (wrencomputer.ResourceMetrics, error) {
	return wrencomputer.ResourceMetrics{}, nil
}

type sandboxFixtureT struct {
	h              *HTTPServer
	s              *Store
	engine         *sandboxFakeEngine
	clients        map[string]*sandboxFakeClient
	lastClient     *sandboxFakeClient
	failCheckpoint bool
	failRestore    bool
	jobs           map[string]wrencomputer.Job
	dialAddr       string
	terminals      []*fakeTerminal
	cookie         *http.Cookie
	account        Account
	space          Workspace
}

func sandboxFixture(t *testing.T) *sandboxFixtureT {
	t.Helper()
	s := integrationStore(t)
	a, w, cookie := workspaceSession(t, s, "sandbox-owner@example.com")
	engine := &sandboxFakeEngine{}
	f := &sandboxFixtureT{s: s, engine: engine, cookie: cookie, account: a, space: w, clients: map[string]*sandboxFakeClient{}, jobs: map[string]wrencomputer.Job{}}
	h := NewHTTPServer(&Service{Store: s, Engine: engine}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Sandboxes: SandboxConfig{Enabled: true, SSHSourceCIDR: "203.0.113.0/24", PreviewDomain: "preview.test"}}).(*HTTPServer)
	h.sandboxTerminalOpen = func(_ context.Context, _ Sandbox, _, _ int) (terminalIO, error) {
		term := newFakeTerminal()
		f.terminals = append(f.terminals, term)
		return term, nil
	}
	h.sandboxDial = func(config wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: snapshotStoreArchive(t, 1024), manifest: wrencomputer.CheckpointManifest{ArchiveSHA256: "fixture"}, failCheckpoint: f.failCheckpoint, failRestore: f.failRestore, jobs: f.jobs, dialAddr: f.dialAddr}
		f.clients[config.Address] = client
		f.lastClient = client
		return client, nil
	}
	f.h = h
	return f
}

func (f *sandboxFixtureT) enable(t *testing.T) {
	t.Helper()
	response := requestJSON(t, f.h, http.MethodPut, "/v1/workspaces/"+f.space.ID+"/products/sandboxes", nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
}

func (f *sandboxFixtureT) create(t *testing.T, name, size string, status int) Sandbox {
	t.Helper()
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", map[string]string{"name": name, "size": size}, f.cookie)
	requireStatus(t, response, status)
	var v Sandbox
	if status < 300 {
		if err := json.Unmarshal(response.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func (f *sandboxFixtureT) ready(t *testing.T, v Sandbox) Sandbox {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='ready',address='192.0.2.55',host_fingerprint='SHA256:test-host',awake_since=now() WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWorkspaceProductsEnableDisable(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	path := "/v1/workspaces/" + f.space.ID + "/products"
	response := requestJSON(t, f.h, http.MethodGet, path, nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var list struct {
		Products []productEntry `json:"products"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]productEntry{}
	for _, entry := range list.Products {
		if _, dup := byID[entry.ID]; dup {
			t.Fatalf("duplicate product id %s", entry.ID)
		}
		byID[entry.ID] = entry
	}
	wrens, ok := byID["wrens"]
	if !ok || !wrens.Enabled || !wrens.AlwaysOn {
		t.Fatalf("wrens entry wrong/missing: %#v", wrens)
	}
	sandboxes, ok := byID["sandboxes"]
	if !ok || sandboxes.Enabled || sandboxes.AlwaysOn {
		t.Fatalf("sandboxes entry wrong/missing: %#v", sandboxes)
	}
	// Viewer reads but cannot enable.
	viewer, _, viewerCookie := workspaceSession(t, f.s, "sandbox-viewer@example.com")
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewer.ID, f.space.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodGet, path, nil, viewerCookie), http.StatusOK)
	requireStatus(t, requestJSON(t, f.h, http.MethodPut, path+"/sandboxes", nil, viewerCookie), http.StatusForbidden)
	// Installation sessions cannot enable products.
	agent := workspaceTestAgent(t, f.s, f.account, f.space.ID, "Agent One")
	agentRequest := httptest.NewRequest(http.MethodPut, path+"/sandboxes", nil)
	agentRequest.Header.Set("Authorization", "Bearer "+agent.AccessToken)
	agentResponse := httptest.NewRecorder()
	f.h.ServeHTTP(agentResponse, agentRequest)
	requireStatus(t, agentResponse, http.StatusForbidden)
	// Enable + idempotent replay.
	requireStatus(t, requestJSON(t, f.h, http.MethodPut, path+"/sandboxes", nil, f.cookie), http.StatusOK)
	requireStatus(t, requestJSON(t, f.h, http.MethodPut, path+"/sandboxes", nil, f.cookie), http.StatusOK)
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, path+"/wrens", nil, f.cookie), http.StatusConflict)
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, path+"/nope", nil, f.cookie), http.StatusNotFound)
	// Live sandboxes block disabling.
	f.create(t, "dev", "free-1x1", http.StatusAccepted)
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, path+"/sandboxes", nil, f.cookie), http.StatusConflict)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleted'`); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, path+"/sandboxes", nil, f.cookie), http.StatusOK)
}

func TestSandboxCreateRequiresProductAndPolicy(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.create(t, "dev", "free-1x1", http.StatusConflict)
	f.enable(t)
	f.create(t, "dev", "s-2x4", http.StatusConflict) // paid without active billing
	f.create(t, "dev", "bogus", http.StatusConflict)
	viewer, _, viewerCookie := workspaceSession(t, f.s, "sandbox-viewer2@example.com")
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewer.ID, f.space.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", map[string]string{"name": "v", "size": "free-1x1"}, viewerCookie), http.StatusForbidden)
	v := f.create(t, "dev", "free-1x1", http.StatusAccepted)
	if v.ID == "" || v.Tier != "free" || v.Phase != "queued" || v.CreatedByName != "sandbox-owner@example.com" {
		t.Fatalf("create: %#v", v)
	}
	if !strings.Contains(f.engine.lastRequest.SSHPublicKey, "ssh-ed25519") {
		t.Fatal("plan lacks an ssh key")
	}
	bootstrap := f.engine.lastRequest // bootstrap lives on the plan, verified below
	_ = bootstrap
	private, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(private.Plan.Spec.Spec.Compute.Bootstrap, "ufw default deny outgoing") || strings.Contains(private.Plan.Spec.Spec.Compute.Bootstrap, "openbox") {
		t.Fatal("free sandbox bootstrap must be headless restricted egress")
	}
	for i := 1; i < 5; i++ {
		f.create(t, "dev-"+strings.Repeat("x", i), "free-1x1", http.StatusAccepted)
	}
	f.create(t, "over", "free-1x1", http.StatusTooManyRequests)
}

func TestSandboxPaidRequiresBillingAndMeters(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active') ON CONFLICT(workspace_id) DO UPDATE SET status='active'`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	free := f.create(t, "free", "free-1x1", http.StatusAccepted)
	paid := f.create(t, "paid", "s-1x2", http.StatusAccepted)
	if paid.Tier != "paid" || paid.MonthlyCents <= 0 {
		t.Fatalf("paid sandbox: %#v", paid)
	}
	if !strings.Contains(f.engine.lastRequest.SSHPublicKey, "ssh-ed25519") {
		t.Fatal("plan lacks ssh key")
	}
	private, err := f.s.Sandbox(ctx, f.space.ID, paid.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(private.Plan.Spec.Spec.Compute.Bootstrap, "ufw default deny outgoing") {
		t.Fatal("paid sandbox must keep standard egress")
	}
	f.ready(t, free)
	f.ready(t, paid)
	plans, err := f.s.SandboxPlans(ctx, f.space.ID)
	if err != nil || len(plans) != 1 {
		t.Fatalf("metered plans %d %v", len(plans), err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep' WHERE id=$1`, paid.ID); err != nil {
		t.Fatal(err)
	}
	if plans, err = f.s.SandboxPlans(ctx, f.space.ID); err != nil || len(plans) != 0 {
		t.Fatalf("asleep paid sandbox still metered: %d %v", len(plans), err)
	}
}

func TestSandboxSleepWakeRestoresCheckpoint(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "dev", "free-1x1", http.StatusAccepted))
	// Idle longer than the sleep policy forces checkpoint+destroy.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET awake_since=now()-interval '30 minutes' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || v.Phase != "asleep" || !v.HasCheckpoint {
		t.Fatalf("sleep: phase=%s checkpoint=%v err=%v", v.Phase, v.HasCheckpoint, err)
	}
	if v.CurrentSnapshotID == "" {
		t.Fatal("sleep did not publish a v2 snapshot")
	}
	var parts int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_snapshot_parts WHERE snapshot_id=$1`, v.CurrentSnapshotID).Scan(&parts); err != nil || parts < 1 {
		t.Fatalf("snapshot parts not stored: %d %v", parts, err)
	}
	if len(f.engine.destroyed) == 0 {
		t.Fatal("sleep did not destroy the VM")
	}
	var used int64
	if err := f.s.pool.QueryRow(ctx, `SELECT awake_seconds FROM sandbox_free_usage WHERE workspace_id=$1`, f.space.ID).Scan(&used); err != nil || used < 1700 {
		t.Fatalf("free usage not accrued: %d %v", used, err)
	}
	// Wake restores the sealed archive through the fake agent.
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || v.Phase != "ready" {
		t.Fatalf("wake: %#v %v", v, err)
	}
	var restored bool
	for _, client := range f.clients {
		for _, data := range client.restored {
			if len(data) > 0 {
				restored = true
			}
		}
	}
	if !restored {
		t.Fatal("wake did not restore the checkpoint archive")
	}
}

func TestSandboxFreeHoursExhaustionBlocksAndSleeps(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "dev", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES($1,$2,$3)`, f.space.ID, sandboxMonth(f.s.now()), 50*3600); err != nil {
		t.Fatal(err)
	}
	f.create(t, "more", "free-1x1", http.StatusTooManyRequests)
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || v.Phase != "asleep" {
		t.Fatalf("exhausted free sandbox must force-sleep: %#v %v", v, err)
	}
}

func TestSandboxExecAndFilesAndActorAudit(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "dev", "free-1x1", http.StatusAccepted))
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/exec", map[string]any{"command": "echo hi", "timeoutSeconds": 60}, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var out map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "completed" || !strings.Contains(out["output"].(string), "echo hi") {
		t.Fatalf("exec: %v", out)
	}
	response = requestJSON(t, f.h, http.MethodGet, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/files?path=/workspace", nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), "main.py") {
		t.Fatalf("files: %s", response.Body.String())
	}
	var audited bool
	if err := f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_events WHERE workspace_id=$1 AND action='sandbox.command' AND subject=$2)`, f.space.ID, v.ID).Scan(&audited); err != nil || !audited {
		t.Fatalf("command audit missing: %v", err)
	}
	// Asleep sandboxes refuse exec instead of waking silently.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',address=NULL WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/exec", map[string]any{"command": "echo hi"}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
}

func TestSandboxDisabledServer(t *testing.T) {
	s := integrationStore(t)
	_, w, cookie := workspaceSession(t, s, "sandbox-disabled@example.com")
	h := NewHTTPServer(&Service{Store: s, Engine: &sandboxFakeEngine{}}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Sandboxes: SandboxConfig{Enabled: false}})
	response := requestJSON(t, h, http.MethodPost, "/v1/workspaces/"+w.ID+"/sandboxes", map[string]string{"name": "dev", "size": "free-1x1"}, cookie)
	requireStatus(t, response, http.StatusBadRequest)
	if !strings.Contains(response.Body.String(), "aren't available") {
		t.Fatalf("disabled message: %s", response.Body.String())
	}
}

func TestSandboxWakeReplanAndPolicy(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "dev", "free-1x1", http.StatusAccepted))
	firstPlan := v.Plan.Spec.Metadata.Name
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "asleep" {
		t.Fatalf("sleep phase %s", v.Phase)
	}
	// Exhaust free hours: wake refused under policy.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES($1,$2,$3)`, f.space.ID, sandboxMonth(f.s.now()), 50*3600); err != nil {
		t.Fatal(err)
	}
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("exhausted wake: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx, `DELETE FROM sandbox_free_usage WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Generation != 2 || v.Plan.Spec.Metadata.Name != firstPlan+"-g2" || !strings.HasSuffix(v.Plan.Spec.Spec.M1.Prefix, "/g2") {
		t.Fatalf("wake replan: gen=%d name=%s prefix=%s", v.Generation, v.Plan.Spec.Metadata.Name, v.Plan.Spec.Spec.M1.Prefix)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "ready" {
		t.Fatalf("wake after replan: %#v", v)
	}
}

func TestSandboxWakeFailureRetainsCheckpoint(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "dev", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	snapshotID := v.CurrentSnapshotID
	if snapshotID == "" && v.CheckpointObject.Key == "" {
		t.Fatal("no checkpoint saved")
	}
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	f.failRestore = true
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "asleep" || v.CurrentSnapshotID != snapshotID || !strings.Contains(v.Failure, "retained") {
		t.Fatalf("wake failure must park asleep with checkpoint: %#v", v)
	}
	if v.CurrentSnapshotID == "" && len(f.engine.objects) != 1 {
		t.Fatalf("checkpoint object lost: %d", len(f.engine.objects))
	}
	// Wake again: restore succeeds with the retained checkpoint.
	f.failRestore = false
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "ready" || v.Generation != 3 {
		t.Fatalf("second wake: %#v", v)
	}
}

func TestSandboxFlushIsPerSandbox(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	one := f.ready(t, f.create(t, "one", "free-1x1", http.StatusAccepted))
	two := f.ready(t, f.create(t, "two", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',awake_since=now()-interval '10 minutes' WHERE id=$1`, one.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, one.ID); err != nil {
		t.Fatal(err)
	}
	two, err := f.s.Sandbox(ctx, f.space.ID, two.ID)
	if err != nil || two.AwakeSince == nil {
		t.Fatal("other sandbox stopped accruing")
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET awake_since=now()-interval '5 minutes' WHERE id=$1`, two.ID); err != nil {
		t.Fatal(err)
	}
	// Only the slept sandbox's ~10 minutes flushed; the other keeps its live
	// accrual, so total used stays under an hour while still counting it.
	var stored int64
	if err := f.s.pool.QueryRow(ctx, `SELECT awake_seconds FROM sandbox_free_usage WHERE workspace_id=$1`, f.space.ID).Scan(&stored); err != nil || stored < 560 || stored > 660 {
		t.Fatalf("only slept sandbox's time flushed: %d %v", stored, err)
	}
	used, err := f.s.SandboxFreeUsage(ctx, f.s.pool, f.space.ID, f.s.now())
	if err != nil || used <= stored {
		t.Fatalf("remaining sandbox not accruing: stored=%d used=%d", stored, used)
	}
}

func TestSandboxForcedSleepPreservesOnSaveFailure(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "dev", "free-1x1", http.StatusAccepted))
	// Even a cap-forced sleep past 24h must NOT destroy the VM on a failed
	// export — it preserves files and parks in save_failed.
	// Seed a legacy previous checkpoint that must survive the failed export.
	prevObj := sdk.ComputerCheckpointObject{Key: "prev-key", SHA256: strings.Repeat("ab", 32), Size: 10}
	prevRaw, _ := json.Marshal(prevObj)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET checkpoint_object_ref=$2,checkpoint_key_version='v1' WHERE id=$1`, v.ID, string(prevRaw)); err != nil {
		t.Fatal(err)
	}
	if f.engine.objects == nil {
		f.engine.objects = map[string][]byte{}
	}
	f.engine.objects[prevObj.Key] = []byte("prev-bytes")
	f.failCheckpoint = true
	forced := f.s.now().Add(-25 * time.Hour)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true,sleep_forced_at=$2,awake_since=now() WHERE id=$1`, v.ID, forced); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err == nil {
		t.Fatal("failed export should propagate")
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "save_failed" {
		t.Fatalf("phase=%s want save_failed", v.Phase)
	}
	if v.Address == "" || v.HostFingerprint == "" {
		t.Fatal("VM handles lost on save failure")
	}
	if !v.SleepRequested {
		t.Fatal("save_failed did not retain sleep intent")
	}
	var prevRef []byte
	if err := f.s.pool.QueryRow(ctx, `SELECT checkpoint_object_ref FROM sandboxes WHERE id=$1`, v.ID).Scan(&prevRef); err != nil {
		t.Fatal(err)
	}
	if len(prevRef) == 0 || string(prevRef) != string(prevRaw) {
		t.Fatal("previous checkpoint reference lost on save failure")
	}
	if len(f.engine.objects[prevObj.Key]) == 0 {
		t.Fatal("previous checkpoint object erased on save failure")
	}
	if v.Address != "192.0.2.55" {
		t.Fatalf("address changed on save failure: %s", v.Address)
	}
	if len(f.engine.destroyed) != 0 {
		t.Fatal("VM destroyed despite failed export")
	}
	// Downloads allowed (read stream), mutations refused.
	readReq := httptest.NewRequest(http.MethodGet, "/", nil)
	readReq.AddCookie(f.cookie)
	_, cancelStream, serr := f.h.sandboxReadStreamContext(readReq, v)
	if serr != nil {
		t.Fatalf("save_failed blocked reads: %v", serr)
	}
	defer cancelStream()
	if _, err := f.s.beginSandboxMutation(ctx, v, 45*time.Second); err == nil {
		t.Fatal("mutation allowed in save_failed")
	}
	// Free-cap resume is denied when the month is exhausted.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES($1,$2,$3) ON CONFLICT (workspace_id,month) DO UPDATE SET awake_seconds=$3`, f.space.ID, sandboxMonth(f.s.now()), int64(60*60*50)); err != nil {
		t.Fatal(err)
	}
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err == nil {
		t.Fatal("free-cap resume allowed")
	}
}

func TestSandboxSleepWakeSnapshotReexport(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "cycle", "free-1x1", http.StatusAccepted))
	d := &SandboxDispatcher{Server: f.h}
	// Sleep → snapshot v2 published → destroyed → asleep.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("sleep reconcile: %v", err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Phase != "asleep" || v.CurrentSnapshotID == "" {
		t.Fatalf("no v2 snapshot on sleep: phase=%s snap=%s", v.Phase, v.CurrentSnapshotID)
	}
	var snapshotState string
	if err := f.s.pool.QueryRow(ctx, `SELECT state FROM sandbox_snapshots WHERE id=$1`, v.CurrentSnapshotID).Scan(&snapshotState); err != nil || snapshotState != "ready" {
		t.Fatalf("snapshot state: %v %s", err, snapshotState)
	}
	// Wake → restore succeeds through the real archive → ready.
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatalf("wake: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("wake reconcile: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "ready" {
		t.Fatalf("wake did not reach ready: %s", v.Phase)
	}
	// Second sleep reexports at the NEW generation — not the same snapshot.
	firstSnapshot := v.CurrentSnapshotID
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("second sleep: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.CurrentSnapshotID == "" || v.CurrentSnapshotID == firstSnapshot {
		t.Fatalf("second sleep reused snapshot %s", v.CurrentSnapshotID)
	}
}

func TestSandboxSaveFailedResumeKeepsGenerationFence(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "resume", "free-1x1", http.StatusAccepted))
	// Force save_failed via failed export.
	f.failCheckpoint = true
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true,awake_since=now() WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	_ = d.Reconcile(ctx, f.space.ID, v.ID)
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "save_failed" {
		t.Fatalf("phase=%s", v.Phase)
	}
	oldGen := v.Generation
	plan := v.Plan
	f.failCheckpoint = false
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "ready" || v.Generation != oldGen+1 {
		t.Fatalf("resume: phase=%s gen=%d want ready/%d", v.Phase, v.Generation, oldGen+1)
	}
	if v.Plan.Spec.Metadata.Name != plan.Spec.Metadata.Name {
		t.Fatal("resume created a new allocation instead of reusing the VM")
	}
	if v.Address == "" {
		t.Fatal("resume lost the VM address")
	}
}

func TestSandboxSaveCASRejectsStaleView(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "cas", "free-1x1", http.StatusAccepted))
	d := &SandboxDispatcher{Server: f.h}
	// A stale generation cannot overwrite the row.
	stale := v
	stale.Generation++
	if err := d.save(ctx, &stale); err == nil {
		t.Fatal("stale generation save accepted")
	}
	// A stale observed phase cannot overwrite either — bump phase in DB.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.save(ctx, &v); err == nil {
		t.Fatal("stale observed-phase save accepted")
	}
	// A concurrent sleep request must survive a stale ready save.
	fresh, _ := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if fresh.Phase != "sleeping" {
		t.Fatalf("concurrent transition clobbered: %s", fresh.Phase)
	}

	// Independent proofs on a fresh sandbox: the CASE-guards preserve a
	// concurrent sleep request, a delete request, and the live storage
	// pointer — separate from the phase-mismatch rejection above.
	v2 := f.ready(t, f.create(t, "cas2", "free-1x1", http.StatusAccepted))
	// Concurrent sleep_requested must survive a same-phase ready save.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET sleep_requested=true WHERE id=$1`, v2.ID); err != nil {
		t.Fatal(err)
	}
	readySave := v2 // still observed ready, sleep_requested=false locally
	if err := d.save(ctx, &readySave); err != nil {
		t.Fatal(err)
	}
	fresh2, _ := f.s.Sandbox(ctx, f.space.ID, v2.ID)
	if !fresh2.SleepRequested {
		t.Fatal("concurrent sleep request cleared by stale ready save")
	}
	// Delete request survives a stale ready save (CAS refuses the write).
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET delete_requested=true WHERE id=$1`, v2.ID); err != nil {
		t.Fatal(err)
	}
	v2.SleepRequested = true // a later ready save can't clobber it back off
	if err := d.save(ctx, &v2); err == nil {
		t.Fatal("stale ready save overwrote a delete request")
	}
	fresh2, _ = f.s.Sandbox(ctx, f.space.ID, v2.ID)
	if !fresh2.DeleteRequested {
		t.Fatal("delete request cleared by stale save")
	}
	// Storage pointer survives a stale local pointer — publish a real
	// snapshot through sleep so current_snapshot_id satisfies the FK.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true,delete_requested=false WHERE id=$1`, v2.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v2.ID); err != nil {
		t.Fatalf("sleep for pointer test: %v", err)
	}
	v3, _ := f.s.Sandbox(ctx, f.space.ID, v2.ID)
	liveSnap := v3.CurrentSnapshotID
	if liveSnap == "" {
		t.Fatal("no live snapshot for pointer test")
	}
	v3.CurrentSnapshotID = "snap_stale" // stale local pointer must not write through
	v3.Phase = "asleep"
	v3.observedPhase = "asleep"
	if err := d.save(ctx, &v3); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := f.s.pool.QueryRow(ctx, `SELECT COALESCE(current_snapshot_id,'') FROM sandboxes WHERE id=$1`, v2.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != liveSnap {
		t.Fatalf("stale pointer overwrote live snapshot: %s", current)
	}
}

func TestSandboxTrackedJobStopsBeforeSave(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "jobsave", "free-1x1", http.StatusAccepted))
	// Record a running job — export must wait for its terminal stop receipt.
	if err := f.s.recordSandboxJob(ctx, v.ID, "job_running"); err != nil {
		t.Fatal(err)
	}
	stopOrder := &atomic.Int32{}
	exportOrder := &atomic.Int32{}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: snapshotStoreArchive(t, 1024), jobs: f.jobs}
		client.stopHook = func(_ context.Context, id string) (wrencomputer.Job, error) {
			stopOrder.Add(1)
			return wrencomputer.Job{ID: id, Status: "cancelled"}, nil
		}
		client.exportHook = func() error {
			exportOrder.Add(1)
			if stopOrder.Load() == 0 {
				return fmt.Errorf("export ran before stop receipt")
			}
			return nil
		}
		return client, nil
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("sleep with running job: %v", err)
	}
	if stopOrder.Load() == 0 || exportOrder.Load() != 1 {
		t.Fatalf("stop=%d export=%d", stopOrder.Load(), exportOrder.Load())
	}
	var finished bool
	if err := f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_jobs WHERE sandbox_id=$1 AND job_id='job_running' AND finished_at IS NULL)`, v.ID).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if finished {
		t.Fatal("job still tracked after terminal stop")
	}
}

// fakeTerminal is an in-memory pty: input is echoed back as output.
type fakeTerminal struct {
	mu     sync.Mutex
	out    chan []byte
	closed bool
	writes [][]byte
	resize [2]int
}

func newFakeTerminal() *fakeTerminal {
	return &fakeTerminal{out: make(chan []byte, 64)}
}

func (f *fakeTerminal) Write(input []byte) error {
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), input...))
	f.mu.Unlock()
	select {
	case f.out <- append([]byte("$ "), input...):
	default:
	}
	return nil
}

func (f *fakeTerminal) Resize(cols, rows int) error {
	f.mu.Lock()
	f.resize = [2]int{cols, rows}
	f.mu.Unlock()
	return nil
}

func (f *fakeTerminal) Read(buffer []byte) (int, error) {
	chunk, ok := <-f.out
	if !ok {
		return 0, fmt.Errorf("closed")
	}
	return copy(buffer, chunk), nil
}

func (f *fakeTerminal) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.out)
	}
}

func TestSandboxAddressIsPlainHost(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "addr", "free-1x1", http.StatusAccepted))
	reloaded, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(reloaded.Address, "/ \t\r\n") || reloaded.Address == "" {
		t.Fatalf("sandbox address must be a bare host for SSH: %q", reloaded.Address)
	}
	if _, err := netip.ParseAddr(reloaded.Address); err != nil {
		t.Fatalf("persisted address %q is not a bare IP for SSH dialing: %v", reloaded.Address, err)
	}
}

func TestSandboxIdleSleepRespectsRunningJobs(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "busy", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET last_used_at=now()-interval '30 minutes' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_jobs(sandbox_id,job_id) VALUES ($1,'job_running')`, v.ID); err != nil {
		t.Fatal(err)
	}
	f.jobs["job_running"] = wrencomputer.Job{ID: "job_running", Status: "running"}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || v.Phase != "ready" {
		t.Fatalf("running job should keep the sandbox awake: phase=%s err=%v", v.Phase, err)
	}
	if v.LastUsedAt == nil || time.Since(*v.LastUsedAt) > 15*time.Minute {
		t.Fatal("running job did not refresh last_used_at")
	}
	// Once the job finishes, the same reconcile sleeps the sandbox.
	f.jobs["job_running"] = wrencomputer.Job{ID: "job_running", Status: "completed"}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET last_used_at=now()-interval '30 minutes' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || v.Phase != "asleep" {
		t.Fatalf("finished job should allow sleep: phase=%s err=%v", v.Phase, err)
	}
	var finished bool
	if err := f.s.pool.QueryRow(ctx, `SELECT finished_at IS NOT NULL FROM sandbox_jobs WHERE sandbox_id=$1 AND job_id='job_running'`, v.ID).Scan(&finished); err != nil || !finished {
		t.Fatal("terminal status was not recorded in sandbox_jobs")
	}
}

func TestSandboxFreeCapForcesSleepDespiteRunningJob(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "capped", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES ($1,date_trunc('month',now())::date,$2)
		ON CONFLICT (workspace_id,month) DO UPDATE SET awake_seconds=$2`, f.space.ID, int64(f.h.config.Sandboxes.freeSecondsPerMonth())); err != nil {
		t.Fatal(err)
	}
	f.jobs["job_live"] = wrencomputer.Job{ID: "job_live", Status: "running"}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_jobs(sandbox_id,job_id) VALUES ($1,'job_live')`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || v.Phase != "asleep" {
		t.Fatalf("the free-hour cap must force sleep even with a running job: phase=%s err=%v", v.Phase, err)
	}
}

func TestSandboxTerminalEndpoints(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "term", "free-1x1", http.StatusAccepted))
	base := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID

	// Viewer (member role) cannot open a terminal.
	_, _, otherCookie := workspaceSession(t, f.s, "sandbox-viewer@example.com")
	if _, err := f.s.pool.Exec(context.Background(), `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id<>$2`, f.space.ID, f.account.ID); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, f.h, http.MethodPost, base+"/terminals", map[string]any{}, otherCookie)
	requireStatus(t, response, http.StatusForbidden)

	response = requestJSON(t, f.h, http.MethodPost, base+"/terminals", map[string]any{}, f.cookie)
	requireStatus(t, response, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("no terminal id: %v", err)
	}
	// Input echoes through the fake pty and arrives on the output stream.
	response = requestJSON(t, f.h, http.MethodPost, base+"/terminals/"+created.ID+"/input", map[string]any{"d": "bHMgLWxhCg=="}, f.cookie)
	requireStatus(t, response, http.StatusOK)
	response = requestJSON(t, f.h, http.MethodPost, base+"/terminals/"+created.ID+"/resize", map[string]any{"cols": 100, "rows": 40}, f.cookie)
	requireStatus(t, response, http.StatusOK)

	req := httptest.NewRequest(http.MethodGet, base+"/terminals/"+created.ID+"/output", nil)
	req.AddCookie(f.cookie)
	recorder := httptest.NewRecorder()
	var bodyMu sync.Mutex
	guarded := &guardedResponseWriter{ResponseWriter: recorder, mu: &bodyMu}
	done := make(chan struct{})
	go func() { defer close(done); f.h.ServeHTTP(guarded, req) }()
	deadline := time.After(5 * time.Second)
	for {
		bodyMu.Lock()
		body := recorder.Body.String()
		bodyMu.Unlock()
		if strings.Contains(body, "\"d\"") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no output chunk; body=%q", body)
		case <-time.After(20 * time.Millisecond):
		}
	}
	_ = done

	// Another writer's session is invisible (404), then close succeeds.
	operator, _, operatorCookie := workspaceSession(t, f.s, "sandbox-operator@example.com")
	if _, err := f.s.pool.Exec(context.Background(), `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, operator.ID, f.space.ID); err != nil {
		t.Fatal(err)
	}
	response = requestJSON(t, f.h, http.MethodDelete, base+"/terminals/"+created.ID, nil, operatorCookie)
	requireStatus(t, response, http.StatusNotFound)
	response = requestJSON(t, f.h, http.MethodDelete, base+"/terminals/"+created.ID, nil, f.cookie)
	requireStatus(t, response, http.StatusOK)

	// Three per sandbox is the cap.
	for i := 0; i < 3; i++ {
		response = requestJSON(t, f.h, http.MethodPost, base+"/terminals", map[string]any{}, f.cookie)
		requireStatus(t, response, http.StatusCreated)
	}
	response = requestJSON(t, f.h, http.MethodPost, base+"/terminals", map[string]any{}, f.cookie)
	requireStatus(t, response, http.StatusTooManyRequests)
}

func TestSandboxPreviewLifecycle(t *testing.T) {
	if err := ValidatePreviewDomain("preview.canter.dev", "https://canter.dev"); err == nil {
		t.Fatal("subdomain of the app domain must be rejected")
	}
	if err := ValidatePreviewDomain("canter.dev", "https://canter.dev"); err == nil {
		t.Fatal("the app domain itself must be rejected")
	}
	if err := ValidatePreviewDomain("canter-preview.dev", "https://canter.dev"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePreviewDomain("x.canter.dev.evil.test", "https://canter.dev"); err != nil {
		t.Fatalf("a different registrable domain must be allowed: %v", err)
	}

	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "site", "free-1x1", http.StatusAccepted))
	base := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID

	response := requestJSON(t, f.h, http.MethodPost, base+"/previews", map[string]any{"port": 22}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from sandbox"))
	}))
	defer backend.Close()
	backendPort, _ := strconv.Atoi(strings.TrimPrefix(backend.URL, "http://127.0.0.1:"))
	response = requestJSON(t, f.h, http.MethodPost, base+"/previews", map[string]any{"port": backendPort}, f.cookie)
	requireStatus(t, response, http.StatusCreated)
	var pv struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(pv.URL, "https://"), ".preview.test")

	req := httptest.NewRequest(http.MethodGet, "http://"+token+".preview.test/", nil)
	req.Host = token + ".preview.test"
	req.Header.Set("Authorization", "Bearer should-not-forward")
	req.AddCookie(&http.Cookie{Name: "canter_session", Value: "secret"})
	recorder := httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "hello from sandbox") {
		t.Fatalf("proxy failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatal("missing noindex")
	}

	// Unknown token → 404; never reaches API handlers.
	req = httptest.NewRequest(http.MethodGet, "http://deadbeef.preview.test/v1/workspaces", nil)
	req.Host = "deadbeef.preview.test"
	recorder = httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	requireStatus(t, recorder, http.StatusNotFound)

	// Loopback-only ask endpoint: active preview 200, unknown 404, external 404.
	req = httptest.NewRequest(http.MethodGet, "/internal/preview-ask?domain="+token+".preview.test", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	requireStatus(t, recorder, http.StatusOK)
	req = httptest.NewRequest(http.MethodGet, "/internal/preview-ask?domain=deadbeef.preview.test", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	requireStatus(t, recorder, http.StatusNotFound)
	req = httptest.NewRequest(http.MethodGet, "/internal/preview-ask?domain="+token+".preview.test", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	recorder = httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	requireStatus(t, recorder, http.StatusNotFound)

	// WebSocket upgrade: the transport must pump the hijacked conn both ways.
	wsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer wsListener.Close()
	go func() {
		for {
			conn, err := wsListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buffer := make([]byte, 8192)
				n, _ := c.Read(buffer)
				if !strings.Contains(string(buffer[:n]), "Upgrade: websocket") {
					return
				}
				c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
				for {
					n, err := c.Read(buffer)
					if err != nil {
						return
					}
					if _, err := c.Write(buffer[:n]); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	wsPort := wsListener.Addr().(*net.TCPAddr).Port
	response = requestJSON(t, f.h, http.MethodPost, base+"/previews", map[string]any{"port": wsPort}, f.cookie)
	requireStatus(t, response, http.StatusCreated)
	var wsPV struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wsPV); err != nil {
		t.Fatal(err)
	}
	wsToken := strings.TrimSuffix(strings.TrimPrefix(wsPV.URL, "https://"), ".preview.test")
	// ReverseProxy needs a real client; run an httptest server around h.
	proxy := httptest.NewServer(f.h)
	defer proxy.Close()
	wsRaw, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer wsRaw.Close()
	fmt.Fprintf(wsRaw, "GET /ws HTTP/1.1\r\nHost: %s.preview.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", wsToken)
	wsRaw.SetReadDeadline(time.Now().Add(10 * time.Second))
	reply := make([]byte, 4096)
	n, err := wsRaw.Read(reply)
	if err != nil || !strings.Contains(string(reply[:n]), "101") {
		t.Fatalf("websocket upgrade failed: %v %q", err, reply[:n])
	}
	if _, err := wsRaw.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	n, err = wsRaw.Read(reply)
	if err != nil || string(reply[:n]) != "ping" {
		t.Fatalf("websocket echo failed: %v %q", err, reply[:n])
	}

	// Asleep sandboxes get the notice page.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://"+token+".preview.test/", nil)
	req.Host = token + ".preview.test"
	recorder = httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "asleep") {
		t.Fatalf("expected the asleep page: %d", recorder.Code)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='ready' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}

	// Cap at five active previews (first + ws + 3), then revoke.
	for i := 0; i < 3; i++ {
		response = requestJSON(t, f.h, http.MethodPost, base+"/previews", map[string]any{"port": 3000 + i}, f.cookie)
		requireStatus(t, response, http.StatusCreated)
	}
	response = requestJSON(t, f.h, http.MethodPost, base+"/previews", map[string]any{"port": 4000}, f.cookie)
	requireStatus(t, response, http.StatusTooManyRequests)
	response = requestJSON(t, f.h, http.MethodDelete, base+"/previews/"+pv.ID, nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	req = httptest.NewRequest(http.MethodGet, "http://"+token+".preview.test/", nil)
	req.Host = token + ".preview.test"
	recorder = httptest.NewRecorder()
	f.h.ServeHTTP(recorder, req)
	requireStatus(t, recorder, http.StatusNotFound)
}

func TestSandboxPlanUsesPinnedImage(t *testing.T) {
	f := sandboxFixture(t)
	f.h.config.Sandboxes.ImageID = "img-pinned-123"
	f.enable(t)
	f.create(t, "pinned", "free-1x1", http.StatusAccepted)
	if f.engine.lastRequest.ImageID != "img-pinned-123" {
		t.Fatalf("plan did not receive the pinned image: %q", f.engine.lastRequest.ImageID)
	}
	// Without the config the request must not carry an override.
	f2 := sandboxFixture(t)
	f2.enable(t)
	f2.create(t, "stock", "free-1x1", http.StatusAccepted)
	if f2.engine.lastRequest.ImageID != "" {
		t.Fatalf("unpinned plan got an image override: %q", f2.engine.lastRequest.ImageID)
	}
}

func requestJSONHeaders(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
		req.Header.Set("Origin", "http://canter.test")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func TestSandboxCreateIdempotency(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	body := map[string]any{"name": "idem-test", "size": "free-1x1", "idempotencyKey": "retry-key-0001"}
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", body, f.cookie)
	requireStatus(t, response, http.StatusAccepted)
	var first Sandbox
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	// Same key + same body replays the original sandbox, not a new one.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", body, f.cookie)
	requireStatus(t, response, http.StatusAccepted)
	var replay Sandbox
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID {
		t.Fatalf("idempotent replay created a different sandbox: %s vs %s", replay.ID, first.ID)
	}
	var count int
	if err := f.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandboxes WHERE workspace_id=$1`, f.space.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one sandbox after replay, got %d", count)
	}
	// Same key + changed body conflicts.
	changed := map[string]any{"name": "different-name", "size": "free-1x1", "idempotencyKey": "retry-key-0001"}
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", changed, f.cookie)
	requireStatus(t, response, http.StatusConflict)
	// Header key must match body key when both present.
	response = requestJSONHeaders(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "x", "size": "free-1x1", "idempotencyKey": "body-key-01"},
		f.cookie, map[string]string{"Idempotency-Key": "header-key-02"})
	requireStatus(t, response, http.StatusConflict)
	// Header alone supplies the key when the body omits it.
	response = requestJSONHeaders(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "header-only", "size": "free-1x1"},
		f.cookie, map[string]string{"Idempotency-Key": "header-only-key"})
	requireStatus(t, response, http.StatusAccepted)
	// Replaying via header alone returns the same sandbox.
	response = requestJSONHeaders(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "header-only", "size": "free-1x1"},
		f.cookie, map[string]string{"Idempotency-Key": "header-only-key"})
	requireStatus(t, response, http.StatusAccepted)
	var hdrReplay Sandbox
	if err := json.Unmarshal(response.Body.Bytes(), &hdrReplay); err != nil {
		t.Fatal(err)
	}
	if hdrReplay.Name != "header-only" {
		t.Fatalf("header-key replay did not return original: %v", hdrReplay)
	}
}

func TestSandboxCreateLabelsIdle(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	// Valid labels + free tier with omitted idle.
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", map[string]any{
		"name": "labeled", "size": "free-1x1",
		"labels": map[string]string{"team": "platform", "env": "dev"},
	}, f.cookie)
	requireStatus(t, response, http.StatusAccepted)
	var v Sandbox
	if err := json.Unmarshal(response.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Labels["team"] != "platform" || v.Labels["env"] != "dev" {
		t.Fatalf("labels not persisted: %v", v.Labels)
	}
	// Free tier rejects a custom idle policy.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", map[string]any{
		"name": "idle-free", "size": "free-1x1", "idleTimeoutSeconds": 0,
	}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
	// Invalid label key rejected.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", map[string]any{
		"name": "bad-label", "size": "free-1x1", "labels": map[string]string{"bad key": "v"},
	}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
	// Invalid idle values rejected.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", map[string]any{
		"name": "bad-idle", "size": "free-1x1", "idleTimeoutSeconds": 42,
	}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
}

func TestSandboxPatch(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "patch-me", "free-1x1", http.StatusAccepted))
	response := requestJSON(t, f.h, http.MethodPatch, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID, map[string]any{
		"name": "renamed", "labels": map[string]string{"phase": "test"},
	}, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var updated Sandbox
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" || updated.Labels["phase"] != "test" {
		t.Fatalf("patch not applied: %+v", updated)
	}
}

func TestSandboxJobWorkflow(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "jobs", "free-1x1", http.StatusAccepted))
	// Async job start returns immediately with a job view.
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs", map[string]any{
		"command": "sleep 60", "directory": "/workspace", "timeoutSeconds": 3600,
	}, f.cookie)
	requireStatus(t, response, http.StatusAccepted)
	var started map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	jobID, _ := started["jobId"].(string)
	if jobID == "" {
		t.Fatal("no jobId returned")
	}
	// Stop returns the current receipt.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs/"+jobID+"/stop", nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var stopped map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &stopped); err != nil {
		t.Fatal(err)
	}
	if stopped["status"] == "running" || stopped["status"] == "starting" {
		t.Fatalf("stopped job still running: %v", stopped)
	}
	// Log read on a known job.
	response = requestJSON(t, f.h, http.MethodGet, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs/"+jobID+"/logs?stream=combined&offset=0&limit=1024", nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	// Unknown job ids are not found (checkpoint/internal jobs stay unreachable).
	response = requestJSON(t, f.h, http.MethodGet, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs/job_internal/logs", nil, f.cookie)
	requireStatus(t, response, http.StatusNotFound)
	// Invalid env rejected at the control-plane boundary.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs", map[string]any{
		"command": "true", "env": map[string]string{"bad key": "v"},
	}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
	// Timeout above the cap rejected, not silently replaced.
	response = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs", map[string]any{
		"command": "true", "timeoutSeconds": 99999,
	}, f.cookie)
	requireStatus(t, response, http.StatusConflict)
}

func TestSandboxCreateConcurrentIdempotency(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	if _, err := f.s.pool.Exec(context.Background(), `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active') ON CONFLICT(workspace_id) DO UPDATE SET status='active'`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"name": "race", "size": "s-1x2", "idempotencyKey": "concurrent-key-1"}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	statuses := make([]int, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes", body, f.cookie)
			statuses[i] = response.Code
			var v Sandbox
			if err := json.Unmarshal(response.Body.Bytes(), &v); err == nil {
				ids[i] = v.ID
			}
		}(i)
	}
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusAccepted || ids[i] == "" {
			t.Fatalf("request %d failed: status %d", i, status)
		}
	}
	var count int
	if err := f.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandboxes WHERE workspace_id=$1 AND phase<>'deleted'`, f.space.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent creates produced %d sandboxes, want 1 (statuses %v)", count, statuses)
	}
	// All replays returned the same sandbox id.
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			seen[id] = true
		}
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent creates returned %d distinct ids: %v", len(seen), ids)
	}
	var reservations int
	if err := f.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandbox_create_requests WHERE workspace_id=$1`, f.space.ID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("expected one create-request row, got %d", reservations)
	}
	if err := f.s.pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox' AND phase='reserved'`, f.space.ID).Scan(&reservations); err != nil || reservations != 1 {
		t.Fatalf("expected exactly one paid reservation: %d %v", reservations, err)
	}
}

func TestSandboxWorkflowCompatibilityAndRegistration(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "compatibility", "free-1x1", http.StatusAccepted))
	client := &sandboxFakeClient{jobs: map[string]wrencomputer.Job{}}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) { return client, nil }
	base := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID
	var launched []wrencomputer.JobRequest
	client.startHook = func(_ context.Context, input wrencomputer.JobRequest) (wrencomputer.Job, error) {
		launched = append(launched, input)
		return wrencomputer.Job{ID: input.ID, Status: "completed", Output: "legacy result"}, nil
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/exec", map[string]any{"command": "true"}, f.cookie), http.StatusOK)
	if len(launched) != 1 || launched[0].Sandbox || launched[0].TimeoutSeconds != 600 {
		t.Fatal("legacy exec changed protocol")
	}
	for _, route := range []string{"/jobs", "/exec"} {
		response := requestJSON(t, f.h, http.MethodPost, base+route, map[string]any{"command": "true", "env": map[string]string{"X": "value"}}, f.cookie)
		requireStatus(t, response, http.StatusConflict)
		if !strings.Contains(response.Body.String(), "wake") {
			t.Fatal("missing upgrade guidance")
		}
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodGet, base+"/metrics", nil, f.cookie), http.StatusConflict)
	if len(launched) != 1 {
		t.Fatal("unsupported request launched a job")
	}
	client.capabilities.WorkflowProtocol = 1
	ctx := context.Background()
	for _, directory := range []string{"/etc", "/workspace/.canter/private", "../outside"} {
		_, err := f.h.sandboxJobStart(ctx, v, SandboxExecInput{Command: "true", Directory: directory}, sdk.ActorRef{})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("unsafe cwd accepted: %q %v", directory, err)
		}
	}
	// A stale ready object cannot launch after the database changed state.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.sandboxJobStart(ctx, v, SandboxExecInput{Command: "true"}, sdk.ActorRef{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("registration fence: %v", err)
	}
	if len(launched) != 1 {
		t.Fatal("guest launched before durable registration succeeded")
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='ready' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	var uncertainID string
	client.startHook = func(_ context.Context, input wrencomputer.JobRequest) (wrencomputer.Job, error) {
		uncertainID = input.ID
		return wrencomputer.Job{}, context.DeadlineExceeded
	}
	response := requestJSON(t, f.h, http.MethodPost, base+"/jobs", map[string]any{"command": "true"}, f.cookie)
	if response.Code < 400 || uncertainID == "" || !strings.Contains(response.Body.String(), uncertainID) {
		t.Fatal("uncertain launch lost its recovery handle")
	}
	if !f.s.sandboxJobKnown(ctx, v.ID, uncertainID) {
		t.Fatal("uncertain job was not registered")
	}
	dispatcher := SandboxDispatcher{Server: f.h}
	if !dispatcher.jobsStillRunning(ctx, v) {
		t.Fatal("uncertain recent launch allowed sleep")
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_jobs SET started_at=now()-interval '1 minute' WHERE sandbox_id=$1 AND job_id=$2`, v.ID, uncertainID); err != nil {
		t.Fatal(err)
	}
	if dispatcher.jobsStillRunning(ctx, v) {
		t.Fatal("confirmed absent job prevents sleep forever")
	}
	if client.jobs[uncertainID].Status != "cancelled" {
		t.Fatal("missing cancellation tombstone for late launch")
	}
}

func TestSandboxDraftCreateAndDeniedReplay(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	pair := workspaceTestAgent(t, f.s, f.account, f.space.ID, "sandbox-cli")
	headers := map[string]string{"Authorization": "Bearer " + pair.AccessToken}
	base := "/v1/workspaces/" + f.space.ID + "/sandboxes"
	body := map[string]any{"name": "agent-created", "size": "free-1x1", "idempotencyKey": "scoped-retry-key"}
	first := requestJSONHeaders(t, f.h, http.MethodPost, base, body, nil, headers)
	requireStatus(t, first, http.StatusAccepted)
	var original Sandbox
	if err := json.Unmarshal(first.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateAgentAuthority(ctx, f.account.ID, f.space.ID, pair.Installation.ID, Authority{Inspect: true, Draft: false, ApplyMode: "never"}); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSONHeaders(t, f.h, http.MethodPost, base, body, nil, headers), http.StatusForbidden)
	viewer, _, viewerCookie := workspaceSession(t, f.s, "replay-viewer@example.com")
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewer.ID, f.space.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base, body, viewerCookie), http.StatusForbidden)
	_, other, otherCookie := workspaceSession(t, f.s, "other-sandbox-owner@example.com")
	requireStatus(t, requestJSON(t, f.h, http.MethodPut, "/v1/workspaces/"+other.ID+"/products/sandboxes", nil, otherCookie), http.StatusOK)
	second := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+other.ID+"/sandboxes", body, otherCookie)
	requireStatus(t, second, http.StatusAccepted)
	var independent Sandbox
	if err := json.Unmarshal(second.Body.Bytes(), &independent); err != nil {
		t.Fatal(err)
	}
	if independent.ID == original.ID || independent.WorkspaceID != other.ID {
		t.Fatal("idempotency escaped its organization")
	}
}

func TestSandboxPaidIdlePolicyDispatch(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	f.h.config.Sandboxes.IdleSleep = time.Hour
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active') ON CONFLICT(workspace_id) DO UPDATE SET status='active'`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		idle    int
		running bool
		phase   string
	}{{-1, false, "ready"}, {0, false, "ready"}, {60, false, "asleep"}, {60, true, "ready"}} {
		v := f.ready(t, f.create(t, fmt.Sprintf("idle-%d-%t", item.idle, item.running), "s-1x2", http.StatusAccepted))
		requireStatus(t, requestJSON(t, f.h, http.MethodPatch, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID, map[string]any{"idleTimeoutSeconds": item.idle}, f.cookie), http.StatusOK)
		if item.running {
			if err := f.s.recordSandboxJob(ctx, v.ID, "job_active_idle"); err != nil {
				t.Fatal(err)
			}
			f.jobs["job_active_idle"] = wrencomputer.Job{ID: "job_active_idle", Status: "running"}
		}
		if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET last_used_at=now()-interval '10 minutes' WHERE id=$1`, v.ID); err != nil {
			t.Fatal(err)
		}
		if err := (&SandboxDispatcher{Server: f.h}).Reconcile(ctx, f.space.ID, v.ID); err != nil {
			t.Fatal(err)
		}
		got, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
		if err != nil || got.Phase != item.phase {
			t.Fatalf("idle %d running=%t: phase %s error %v", item.idle, item.running, got.Phase, err)
		}
	}
}

func TestSandboxEventsResumeAndDrain(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "stream", "free-1x1", http.StatusAccepted))
	id := "job_stream_resume"
	if err := f.s.recordSandboxJob(context.Background(), v.ID, id); err != nil {
		t.Fatal(err)
	}
	full := []byte("héllo-world")
	client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1}}
	var offsets []int64
	client.logsHook = func(_ context.Context, id, stream string, offset int64, limit int) (wrencomputer.JobLogChunk, error) {
		offsets = append(offsets, offset)
		if offset > int64(len(full)) {
			return wrencomputer.JobLogChunk{}, &wrencomputer.Error{Code: "invalid_request"}
		}
		end := min(offset+3, int64(len(full)))
		return wrencomputer.JobLogChunk{JobID: id, Stream: stream, Data: full[offset:end], Offset: offset, NextOffset: end, EOF: end == int64(len(full)), Status: "completed", LogTruncated: true}, nil
	}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) { return client, nil }
	base := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID + "/jobs/" + id + "/events?offset=0"
	response := requestJSONHeaders(t, f.h, http.MethodGet, base, nil, f.cookie, map[string]string{"Last-Event-ID": "2"})
	requireStatus(t, response, http.StatusOK)
	if len(offsets) < 2 || offsets[0] != 2 {
		t.Fatalf("Last-Event-ID ignored: %v", offsets)
	}
	var output []byte
	terminal := false
	for _, block := range strings.Split(response.Body.String(), "\n\n") {
		lines := strings.Split(block, "\n")
		for _, line := range lines {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			raw := []byte(strings.TrimPrefix(line, "data: "))
			if strings.Contains(block, "event: output\n") {
				if terminal {
					t.Fatal("terminal status arrived before output was drained")
				}
				var chunk wrencomputer.JobLogChunk
				if err := json.Unmarshal(raw, &chunk); err != nil {
					t.Fatal(err)
				}
				output = append(output, chunk.Data...)
			} else {
				var view map[string]any
				if err := json.Unmarshal(raw, &view); err != nil {
					t.Fatal(err)
				}
				if view["status"] == "completed" {
					terminal = true
					if view["logTruncated"] != true {
						t.Fatal("final status lost truncation")
					}
				}
			}
		}
	}
	if !terminal || !bytes.Equal(output, full[2:]) {
		t.Fatalf("resumed stream data mismatch: %q", output)
	}
	requireStatus(t, requestJSONHeaders(t, f.h, http.MethodGet, base, nil, f.cookie, map[string]string{"Last-Event-ID": "bad"}), http.StatusConflict)
	bad := requestJSONHeaders(t, f.h, http.MethodGet, base, nil, f.cookie, map[string]string{"Last-Event-ID": "999"})
	if bad.Code < 400 || strings.Contains(bad.Header().Get("Content-Type"), "event-stream") {
		t.Fatal("invalid offset sent successful stream headers")
	}
}

func TestSandboxStreamsCancelOnRevocation(t *testing.T) {
	for _, surface := range []string{"events", "download"} {
		for _, cause := range []string{"session", "installation", "authority", "generation"} {
			t.Run(surface+"/"+cause, func(t *testing.T) {
				f := sandboxFixture(t)
				f.enable(t)
				v := f.ready(t, f.create(t, "revocation", "free-1x1", http.StatusAccepted))
				id := "job_stream_revoke"
				if err := f.s.recordSandboxJob(context.Background(), v.ID, id); err != nil {
					t.Fatal(err)
				}
				client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1}}
				blocked, cancelled := make(chan struct{}), make(chan struct{})
				calls := 0
				client.logsHook = func(ctx context.Context, id, stream string, offset int64, limit int) (wrencomputer.JobLogChunk, error) {
					calls++
					if calls == 1 {
						return wrencomputer.JobLogChunk{JobID: id, Stream: stream, Status: "running", EOF: true}, nil
					}
					close(blocked)
					<-ctx.Done()
					close(cancelled)
					return wrencomputer.JobLogChunk{}, ctx.Err()
				}
				client.readHook = func(ctx context.Context, path string, offset int64, limit int, version string) (wrencomputer.FileChunk, error) {
					if offset == 0 {
						return wrencomputer.FileChunk{Path: path, Data: bytes.Repeat([]byte("a"), 8192), Version: "v1", Size: 16384, NextOffset: 8192}, nil
					}
					close(blocked)
					<-ctx.Done()
					close(cancelled)
					return wrencomputer.FileChunk{}, ctx.Err()
				}
				f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) { return client, nil }
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				server := httptest.NewServer(f.h)
				defer func() { cancel(); server.Close() }()
				url := server.URL + "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID
				if surface == "events" {
					url += "/jobs/" + id + "/events"
				} else {
					url += "/files/content?path=/workspace/test.bin"
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if err != nil {
					t.Fatal(err)
				}
				var pair TokenPair
				if cause == "installation" || cause == "authority" {
					pair = workspaceTestAgent(t, f.s, f.account, f.space.ID, "stream-reader")
					request.Header.Set("Authorization", "Bearer "+pair.AccessToken)
				} else {
					request.AddCookie(f.cookie)
				}
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("stream status %d", response.StatusCode)
				}
				finished := make(chan struct{})
				go func() { io.Copy(io.Discard, response.Body); close(finished) }()
				select {
				case <-blocked:
				case <-ctx.Done():
					t.Fatal("guest did not enter pending read")
				}
				switch cause {
				case "session":
					_, err = f.s.pool.Exec(ctx, `UPDATE human_sessions SET revoked_at=now() WHERE account_id=$1`, f.account.ID)
				case "installation":
					err = f.s.RevokeInstallation(ctx, f.space.ID, pair.Installation.ID)
				case "authority":
					// Exercise persisted policy revocation independently of
					// the settings API (which requires Inspect on active grants).
					_, err = f.s.pool.Exec(ctx, `UPDATE agent_installations SET inspect_allowed=false,draft_allowed=false,apply_mode='never',use_workspace_authority=false WHERE id=$1`, pair.Installation.ID)
				case "generation":
					_, err = f.s.pool.Exec(ctx, `UPDATE sandboxes SET generation=generation+1 WHERE id=$1`, v.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-cancelled:
				case <-time.After(3 * time.Second):
					t.Fatal("revocation did not cancel pending guest RPC")
				}
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatal("revoked HTTP stream did not terminate")
				}
			})
		}
	}
}

func TestSandboxCreateViewerDenied(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	// A viewer session cannot create even with a valid key.
	a2, _, cookie2 := workspaceSession(t, f.s, "sandbox-viewer@example.com")
	if _, err := f.s.pool.Exec(context.Background(), `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, a2.ID, f.space.ID); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "viewer-try", "size": "free-1x1", "idempotencyKey": "viewer-key-1"}, cookie2)
	if response.Code == http.StatusAccepted {
		t.Fatal("viewer created a sandbox")
	}
}

func TestSandboxMetricsNoActivityTouch(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "metrics", "free-1x1", http.StatusAccepted))
	before := v.LastUsedAt
	response := requestJSON(t, f.h, http.MethodGet, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/metrics", nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	// Passive metrics polling must not keep the sandbox awake.
	current, err := f.s.Sandbox(context.Background(), f.space.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before == nil && current.LastUsedAt != nil {
		t.Fatal("metrics request bumped last_used_at")
	}
	if before != nil && current.LastUsedAt != nil && current.LastUsedAt.After(*before) {
		t.Fatal("metrics request bumped last_used_at")
	}
}

func TestSandboxJobStatusGet(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "status", "free-1x1", http.StatusAccepted))
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs", map[string]any{"command": "true"}, f.cookie)
	requireStatus(t, response, http.StatusAccepted)
	var started map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	jobID, _ := started["jobId"].(string)
	// The async-created job's actual URL is reachable (broad router no longer shadows it).
	response = requestJSON(t, f.h, http.MethodGet, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/jobs/"+jobID, nil, f.cookie)
	requireStatus(t, response, http.StatusOK)
	var view map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["jobId"] != jobID {
		t.Fatalf("status view wrong job: %v", view)
	}
}

func TestSandboxDeleteSchedulesSleepingAndFailed(t *testing.T) {
	for _, phase := range []string{"asleep", "failed"} {
		t.Run(phase, func(t *testing.T) {
			f := sandboxFixture(t)
			f.enable(t)
			v := f.create(t, "delete", "free-1x1", http.StatusAccepted)
			if _, err := f.s.pool.Exec(context.Background(), `UPDATE sandboxes SET phase=$2,awake_since=NULL WHERE id=$1`, v.ID, phase); err != nil {
				t.Fatal(err)
			}
			if err := f.h.RequestSandboxDelete(context.Background(), f.space.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
				t.Fatal(err)
			}
			current, err := f.s.Sandbox(context.Background(), f.space.ID, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Phase != "deleting" || !current.DeleteRequested {
				t.Fatalf("delete not scheduled: phase=%s requested=%v", current.Phase, current.DeleteRequested)
			}
			d := &SandboxDispatcher{Server: f.h}
			if err := d.Reconcile(context.Background(), f.space.ID, v.ID); err != nil {
				t.Fatal(err)
			}
			current, err = f.s.Sandbox(context.Background(), f.space.ID, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Phase != "deleted" {
				t.Fatalf("phase after reconcile: %s", current.Phase)
			}
		})
	}
}

// — Mutation fencing + sleep barrier acceptance tests —

func sandboxClaims(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sandbox_mutations WHERE sandbox_id=$1 AND expires_at>clock_timestamp()`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSandboxMutationConcurrencyAndSleepBarrier(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "mut", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var hookCalls atomic.Int32
	client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1}, jobs: f.jobs, startHook: func(c context.Context, input wrencomputer.JobRequest) (wrencomputer.Job, error) {
		hookCalls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-c.Done():
			return wrencomputer.Job{}, c.Err()
		}
		return wrencomputer.Job{ID: input.ID, Status: "running"}, nil
	}}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) { return client, nil }
	fenced, err := f.h.sandboxClient(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	defer fenced.Close()
	bound, cancelBound := context.WithTimeout(ctx, 15*time.Second)
	defer cancelBound()
	done := make(chan error, 2)
	// Stale-generation rejection BEFORE any sleep request — independent check.
	stale := v
	stale.Generation++
	staleClient, serr := f.h.sandboxClient(ctx, stale)
	if serr != nil {
		t.Fatal(serr)
	}
	if _, e := staleClient.StartJob(bound, wrencomputer.JobRequest{ID: "stale", Command: "x"}); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale generation: %v", e)
	}
	staleClient.Close()
	if hookCalls.Load() != 0 {
		t.Fatal("stale-generation call reached the hook")
	}
	for i := 0; i < 2; i++ {
		go func() {
			_, e := fenced.StartJob(bound, wrencomputer.JobRequest{ID: fmt.Sprintf("job_%d", i), Command: "sleep 5"})
			done <- e
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-bound.Done():
			t.Fatal("concurrent mutations did not both reach the guest")
		}
	}
	if got := sandboxClaims(t, f.s, v.ID); got != 2 {
		t.Fatalf("expected 2 concurrent claims, got %d", got)
	}
	// Sleep barrier commits sleep_requested but returns ErrConflict while claims live.
	if err := f.s.enterSandboxSleep(ctx, v); err != ErrConflict {
		t.Fatalf("sleep with claims: %v", err)
	}
	var sleepReq bool
	var forcedAt *time.Time
	if err := f.s.pool.QueryRow(ctx, `SELECT sleep_requested,sleep_forced_at FROM sandboxes WHERE id=$1`, v.ID).Scan(&sleepReq, &forcedAt); err != nil {
		t.Fatal(err)
	}
	if !sleepReq {
		t.Fatal("sleep_requested rolled back while claims blocked sleep")
	}
	if forcedAt != nil {
		t.Fatal("manual sleep became quota-forced")
	}
	// A fresh client built from the SAME stale ready-v is rejected pre-hook.
	c2, e2 := f.h.sandboxClient(ctx, v)
	if e2 != nil {
		t.Fatal(e2)
	}
	if _, e := c2.StartJob(bound, wrencomputer.JobRequest{ID: "stale2", Command: "x"}); !errors.Is(e, ErrConflict) {
		t.Fatalf("mutation after sleep requested: %v", e)
	}
	c2.Close()
	if hookCalls.Load() != 2 {
		t.Fatal("sleep request did not fence new mutations")
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("mutation RPC: %v", err)
			}
		case <-bound.Done():
			t.Fatal("mutation did not finish after release")
		}
	}
	if err := f.s.enterSandboxSleep(ctx, v); err != nil {
		t.Fatalf("sleep after claims released: %v", err)
	}
	var phase string
	if err := f.s.pool.QueryRow(ctx, `SELECT phase FROM sandboxes WHERE id=$1`, v.ID).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "sleeping" {
		t.Fatalf("phase=%s", phase)
	}
}

func TestSandboxMutationUncertainClaimExpiry(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "unc", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	// Unknown remote error → claim retained until bound expires.
	op, err := f.s.beginSandboxMutation(ctx, v, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = op.run(func(context.Context) error { return fmt.Errorf("transport lost") })
	op.close()
	if sandboxClaims(t, f.s, v.ID) != 1 {
		t.Fatal("uncertain operation released its claim early")
	}
	if err := f.s.enterSandboxSleep(ctx, v); err != ErrConflict {
		t.Fatalf("sleep should block on uncertain claim: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_mutations SET expires_at=clock_timestamp()-interval '1 second' WHERE sandbox_id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.enterSandboxSleep(ctx, v); err != nil {
		t.Fatalf("sleep after claim expiry: %v", err)
	}
	// Definitive invalid_request releases the claim immediately.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='ready',sleep_requested=false WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v.Phase = "ready"
	op2, err := f.s.beginSandboxMutation(ctx, v, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = op2.run(func(context.Context) error { return &wrencomputer.Error{Code: "invalid_request", Message: "bad"} })
	op2.close()
	if sandboxClaims(t, f.s, v.ID) != 0 {
		t.Fatal("definitive error did not release its claim")
	}
}

func TestSandboxTerminalLiveLease(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "term", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	// Open a terminal — produces a live lease claim.
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/terminals", nil, f.cookie)
	requireStatus(t, response, http.StatusCreated)
	if sandboxClaims(t, f.s, v.ID) != 1 {
		t.Fatal("terminal open did not produce a live claim")
	}
	if err := f.s.enterSandboxSleep(ctx, v); err != ErrConflict {
		t.Fatalf("sleep should block on live terminal: %v", err)
	}
	// Lease can't renew once sleep_requested — terminal closes within 5s.
	deadline := time.Now().Add(5 * time.Second)
	for sandboxClaims(t, f.s, v.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if sandboxClaims(t, f.s, v.ID) != 0 {
		t.Fatal("live terminal lease survived sleep request")
	}
	// Input after sleep request is refused.
	var tid struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &tid)
	input := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/terminals/"+tid.ID+"/input", map[string]string{"d": "eA=="}, f.cookie)
	if input.Code == http.StatusOK {
		t.Fatal("terminal input accepted after sleep requested")
	}
	if err := f.s.enterSandboxSleep(ctx, v); err != nil {
		t.Fatalf("retry sleep after terminal closed: %v", err)
	}
}

func TestSandboxPreviewLiveLease(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "prev", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "preview-body")
	}))
	defer target.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(target.URL, "http://127.0.0.1:"))
	f.dialAddr = "127.0.0.1"
	host := "tok1.preview.test"
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_previews(id,sandbox_id,workspace_id,port,token,created_by_kind,created_by_id) VALUES('prv_1',$1,$2,$3,'tok1','human',$4)`, v.ID, f.space.ID, port, f.account.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	f.h.servePreview(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "preview-body") {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	// Live lease released after completion — claim gone, client closed.
	deadline := time.Now().Add(5 * time.Second)
	for sandboxClaims(t, f.s, v.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := sandboxClaims(t, f.s, v.ID); got != 0 {
		t.Fatalf("preview left %d claims", got)
	}
	if f.lastClient == nil || !f.lastClient.closed.Load() {
		t.Fatal("preview client not closed after completion")
	}
}

func TestSandboxPreviewRevocation(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "prev-rev", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	targetEnded := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "ready\n")
		if f2, ok := w.(http.Flusher); ok {
			f2.Flush()
		}
		<-r.Context().Done()
		close(targetEnded)
	}))
	defer target.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(target.URL, "http://127.0.0.1:"))
	f.dialAddr = "127.0.0.1"
	host := "tok1.preview.test"
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_previews(id,sandbox_id,workspace_id,port,token,created_by_kind,created_by_id) VALUES('prv_1',$1,$2,$3,'tok1','human',$4)`, v.ID, f.space.ID, port, f.account.ID); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(f.h)
	defer proxy.Close()
	rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, "GET", proxy.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview status: %d", resp.StatusCode)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ready\n" {
		t.Fatalf("preview stream prefix: %q", buf)
	}
	if err := f.s.RevokeSandboxPreview(ctx, f.space.ID, "prv_1"); err != nil {
		t.Fatal(err)
	}
	deadlineTimer := time.NewTimer(5 * time.Second)
	defer deadlineTimer.Stop()
	bodyDone := make(chan struct{})
	go func() {
		defer close(bodyDone)
		io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-bodyDone:
	case <-deadlineTimer.C:
		cancel()
		t.Fatal("preview stream did not complete after revocation")
	}
	select {
	case <-targetEnded:
	case <-deadlineTimer.C:
		cancel()
		t.Fatal("revocation did not end the target request")
	}
	if rctx.Err() != nil {
		t.Fatal("request timeout, rather than revocation, ended the preview")
	}
	deadline := time.Now().Add(5 * time.Second)
	for sandboxClaims(t, f.s, v.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := sandboxClaims(t, f.s, v.ID); got != 0 {
		t.Fatalf("revoked preview left %d claims", got)
	}
	if f.lastClient == nil || !f.lastClient.closed.Load() {
		t.Fatal("preview client not closed after revocation")
	}
}

func TestSandboxMutationForcedSleep(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "forced", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	forced := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	v.SleepForcedAt = &forced
	if err := f.s.enterSandboxSleep(ctx, v); err != nil {
		t.Fatal(err)
	}
	var got *time.Time
	if err := f.s.pool.QueryRow(ctx, `SELECT sleep_forced_at FROM sandboxes WHERE id=$1`, v.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(forced) {
		t.Fatalf("sleep_forced_at %v != %v", got, forced)
	}
}

func TestSandboxFreeUsageAcrossUTCMonth(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "month-boundary", "free-1x1", http.StatusAccepted))
	ctx := context.Background()
	start := time.Date(2026, 1, 31, 23, 59, 30, 0, time.UTC)
	now := start.Add(time.Minute)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET awake_since=$2 WHERE id=$1`, v.ID, start); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES($1,'2026-01-01',7),($1,'2026-02-01',11)`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'America/Los_Angeles'`); err != nil {
		t.Fatal(err)
	}
	// Only February's 30 live seconds belong to February's allowance.
	used, err := f.s.SandboxFreeUsage(ctx, tx, f.space.ID, now)
	if err != nil || used != 41 {
		t.Fatalf("February live usage: %d, %v", used, err)
	}
	for i := 0; i < 2; i++ {
		if err := flushSandboxAwake(ctx, tx, f.space.ID, v.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	for month, expected := range map[string]int64{"2026-01-01": 37, "2026-02-01": 41} {
		var seconds int64
		if err := tx.QueryRow(ctx, `SELECT awake_seconds FROM sandbox_free_usage WHERE workspace_id=$1 AND month=$2`, f.space.ID, month).Scan(&seconds); err != nil || seconds != expected {
			t.Fatalf("%s ledger: %d, want %d (%v)", month, seconds, expected, err)
		}
	}
	used, err = f.s.SandboxFreeUsage(ctx, tx, f.space.ID, now)
	if err != nil || used != 41 {
		t.Fatalf("February flushed usage: %d, %v", used, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxTerminalRevocation(t *testing.T) {
	for _, kind := range []string{"session", "membership"} {
		t.Run(kind, func(t *testing.T) {
			f := sandboxFixture(t)
			f.enable(t)
			v := f.ready(t, f.create(t, "term-"+kind, "free-1x1", http.StatusAccepted))
			ctx := context.Background()
			response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/terminals", nil, f.cookie)
			requireStatus(t, response, http.StatusCreated)
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			session := f.h.terminals.get(v.ID, f.account.ID, created.ID)
			if session == nil {
				t.Fatal("terminal session not registered")
			}
			term := f.terminals[len(f.terminals)-1]
			t.Cleanup(func() { f.h.terminals.close(created.ID) })
			switch kind {
			case "session":
				if _, err := f.s.pool.Exec(ctx, `UPDATE human_sessions SET revoked_at=now() WHERE account_id=$1`, f.account.ID); err != nil {
					t.Fatal(err)
				}
			case "membership":
				if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-session.done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s revocation did not close terminal", kind)
			}
			if got := sandboxClaims(t, f.s, v.ID); got != 0 {
				t.Fatalf("%s revocation left %d claims", kind, got)
			}
			term.mu.Lock()
			closed := term.closed
			term.mu.Unlock()
			if !closed {
				t.Fatalf("%s revocation did not close the remote terminal", kind)
			}
		})
	}
}

func TestSandboxTerminalExitReleasesLease(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.ready(t, f.create(t, "term-exit", "free-1x1", http.StatusAccepted))
	response := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/terminals", nil, f.cookie)
	requireStatus(t, response, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	session := f.h.terminals.get(v.ID, f.account.ID, created.ID)
	if session == nil {
		t.Fatal("terminal session not registered")
	}
	term := f.terminals[len(f.terminals)-1]
	term.Close() // simulate natural shell EOF
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal exit did not propagate")
	}
	if got := sandboxClaims(t, f.s, v.ID); got != 0 {
		t.Fatalf("terminal exit left %d claims", got)
	}
}

// guardedResponseWriter makes a httptest recorder safe for concurrent read of
// the buffered body while the streaming handler is still writing.
type guardedResponseWriter struct {
	http.ResponseWriter
	mu *sync.Mutex
}

func (g *guardedResponseWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ResponseWriter.Write(b)
}
func (g *guardedResponseWriter) WriteHeader(code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ResponseWriter.WriteHeader(code)
}
func (g *guardedResponseWriter) Flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// v2 snapshot transport on the fake client — export validates the archive
// then copies to destination; restore validates stats/archive into restored.
func (c *sandboxFakeClient) ExportSandboxSnapshot(_ context.Context, options wrencomputer.SandboxSnapshotOptions, destination io.Writer) (wrencomputer.SandboxSnapshotStats, error) {
	var empty wrencomputer.SandboxSnapshotStats
	if c.exportHook != nil {
		if err := c.exportHook(); err != nil {
			return empty, err
		}
	}
	if c.failCheckpoint {
		return empty, fmt.Errorf("export failed")
	}
	if c.archive == nil {
		return empty, fmt.Errorf("no archive fixture")
	}
	stats, err := wrencomputer.ValidateSandboxArchive(bytes.NewReader(c.archive), options)
	if err != nil {
		return stats, err
	}
	if _, err := io.Copy(destination, bytes.NewReader(c.archive)); err != nil {
		return stats, err
	}
	return stats, nil
}

func (c *sandboxFakeClient) RestoreSandboxSnapshot(_ context.Context, options wrencomputer.SandboxSnapshotOptions, expected wrencomputer.SandboxSnapshotStats, source io.Reader) error {
	if c.failRestore {
		return fmt.Errorf("restore failed")
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return err
	}
	stats, err := wrencomputer.ValidateSandboxArchive(bytes.NewReader(data), options)
	stats.RuntimeSkipped = expected.RuntimeSkipped
	if err != nil {
		return err
	}
	if stats != expected {
		return fmt.Errorf("restore stats %v != expected %v", stats, expected)
	}
	c.restored = append(c.restored, data)
	return nil
}

func TestSandboxLargeArchiveSleepWake(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "large", "free-1x1", http.StatusAccepted))
	// Wire a >32MiB real archive through the controller.
	big := snapshotStoreArchive(t, 33<<20)
	if len(big) <= 32<<20 {
		t.Fatalf("fixture archive too small: %d", len(big))
	}
	var usedClients []*sandboxFakeClient
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: big, jobs: f.jobs}
		usedClients = append(usedClients, client)
		return client, nil
	}
	d := &SandboxDispatcher{Server: f.h}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.CurrentSnapshotID == "" {
		t.Fatal("large archive produced no snapshot")
	}
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "ready" {
		t.Fatalf("wake phase=%s", v.Phase)
	}
	var restored bool
	for _, client := range usedClients {
		for _, data := range client.restored {
			if bytes.Equal(data, big) {
				restored = true
			}
		}
	}
	if !restored {
		t.Fatal("large archive bytes not restored on wake")
	}
}

func TestSandboxPublicationThenDestroyRetryExportsOnce(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "once", "free-1x1", http.StatusAccepted))
	var exports atomic.Int32
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: snapshotStoreArchive(t, 1024), jobs: f.jobs}
		client.exportHook = func() error { exports.Add(1); return nil }
		return client, nil
	}
	d := &SandboxDispatcher{Server: f.h}
	f.engine.destroyErr = fmt.Errorf("provider down")
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err == nil {
		t.Fatal("destroy error should propagate")
	}
	if exports.Load() != 1 {
		t.Fatalf("exports=%d want 1", exports.Load())
	}
	// Retry — snapshot already published; must NOT export again.
	f.engine.destroyErr = nil
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if exports.Load() != 1 {
		t.Fatalf("retry re-exported: exports=%d", exports.Load())
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "asleep" {
		t.Fatalf("phase=%s", v.Phase)
	}
}

func TestSandboxLegacyWakeRestore(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "legacy", "free-1x1", http.StatusAccepted))
	// Seed a legacy checkpoint (no current_snapshot_id) and park asleep.
	var usedClients []*sandboxFakeClient
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: snapshotStoreArchive(t, 1024), jobs: f.jobs}
		usedClients = append(usedClients, client)
		return client, nil
	}
	version, ciphertext, err := f.h.config.Secrets.seal([]byte("legacy-bytes"), secretBinding(v.WorkspaceID, v.ID, "sandbox-checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.engine.PutComputerCheckpoint(ctx, v.checkpointScope(), ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(obj)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',checkpoint_object_ref=$2,checkpoint_key_version=$3 WHERE id=$1`, v.ID, string(raw), version); err != nil {
		t.Fatal(err)
	}
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("legacy wake: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "ready" {
		t.Fatalf("legacy wake phase=%s", v.Phase)
	}
	var legacyRestored bool
	for _, client := range usedClients {
		for _, data := range client.restored {
			if bytes.Equal(data, []byte("legacy-bytes")) {
				legacyRestored = true
			}
		}
	}
	if !legacyRestored {
		t.Fatal("legacy checkpoint bytes not restored on wake")
	}
}

func TestSandboxPaidResumeKeepsReservation(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_usage_caps(workspace_id,limit_cents,reserved_cents,spent_cents) VALUES($1,100000,2400,0) ON CONFLICT (workspace_id) DO UPDATE SET limit_cents=100000,reserved_cents=2400,spent_cents=0`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	v := f.ready(t, f.create(t, "paidres", "s-2x4", http.StatusAccepted))
	var beforeCents int
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&beforeCents); err != nil {
		t.Fatal(err)
	}
	var beforeCount int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1 AND subject_id=$2 AND phase='reserved'`, f.space.ID, v.ID).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	f.failCheckpoint = true
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	_ = d.Reconcile(ctx, f.space.ID, v.ID)
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "save_failed" {
		t.Fatalf("phase=%s", v.Phase)
	}
	f.failCheckpoint = false
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatalf("paid resume: %v", err)
	}
	var afterCents, afterCount int
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&afterCents); err != nil {
		t.Fatal(err)
	}
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1 AND subject_id=$2 AND phase='reserved'`, f.space.ID, v.ID).Scan(&afterCount); err != nil {
		t.Fatal(err)
	}
	if afterCents != beforeCents || afterCount != beforeCount {
		t.Fatalf("resume reserved again: cents %d→%d count %d→%d", beforeCents, afterCents, beforeCount, afterCount)
	}
}

func TestSandboxSaveFailedReconcileIsQuiet(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "quiet", "free-1x1", http.StatusAccepted))
	f.failCheckpoint = true
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	_ = d.Reconcile(ctx, f.space.ID, v.ID)
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "save_failed" {
		t.Fatalf("phase=%s", v.Phase)
	}
	// Second reconcile: early-return — no guest/provisioning/restore runs.
	var dialed atomic.Int32
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		dialed.Add(1)
		return &sandboxFakeClient{}, nil
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("quiet reconcile: %v", err)
	}
	if dialed.Load() != 0 {
		t.Fatalf("save_failed re-entered provider work: dials=%d", dialed.Load())
	}
}

func TestSandboxWakeRestoreFailureStaysWaking(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "wakerestore", "free-1x1", http.StatusAccepted))
	d := &SandboxDispatcher{Server: f.h}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	snap := v.CurrentSnapshotID
	if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	// Restore fails AND teardown destroy fails — must stay waking with handles.
	f.failRestore = true
	f.engine.destroyErr = fmt.Errorf("provider down")
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err == nil {
		t.Fatal("restore failure should propagate")
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "waking" {
		t.Fatalf("phase=%s want waking while teardown uncertain", v.Phase)
	}
	if v.Address == "" {
		t.Fatal("VM handles lost while teardown uncertain")
	}
	// Retry: cleanup only -> asleep, NO re-apply.
	f.failRestore = false
	f.engine.destroyErr = nil
	appliesBefore := f.engine.applies.Load()
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
		t.Fatalf("teardown retry: %v", err)
	}
	if f.engine.applies.Load() != appliesBefore {
		t.Fatalf("reconcile re-applied VM: %d→%d", appliesBefore, f.engine.applies.Load())
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "asleep" {
		t.Fatalf("final phase=%s want asleep", v.Phase)
	}
	if v.Address != "" || v.HostFingerprint != "" {
		t.Fatalf("VM handles retained after successful teardown: %s", v.Address)
	}
	if v.CurrentSnapshotID != snap {
		t.Fatal("snapshot lost on failed wake")
	}
}

func TestSandboxSettlementFencesAccounting(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active') ON CONFLICT(workspace_id) DO UPDATE SET status='active'`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_usage_caps SET limit_cents=100000 WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	v := f.ready(t, f.create(t, "settle", "s-2x4", http.StatusAccepted))
	var before int
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&before); err != nil || before <= 0 || v.AwakeSince == nil {
		t.Fatalf("paid fixture missing reservation or awake watermark: %d (%v)", before, err)
	}
	if _, err := f.engine.DestroyVPS(ctx, v.Plan); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	// Stale view can't touch accounting.
	stale := v
	stale.Generation++
	if err := d.settleStoppedSandbox(ctx, &stale); err == nil {
		t.Fatal("stale generation settled")
	}
	stale = v
	stale.observedPhase = "sleeping"
	if err := d.settleStoppedSandbox(ctx, &stale); err == nil {
		t.Fatal("stale phase settled")
	}
	var reserved int
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != before {
		t.Fatalf("stale settle changed reserved: %d", reserved)
	}
	current, err := f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || current.AwakeSince == nil || !current.AwakeSince.Equal(*v.AwakeSince) {
		t.Fatal("stale settlement changed the awake watermark", err)
	}
	// Fresh view settles once — reservation released, watermark cleared.
	if err := d.settleStoppedSandbox(ctx, &v); err != nil {
		t.Fatal(err)
	}
	if err := d.settleStoppedSandbox(ctx, &v); err != nil {
		t.Fatal("repeat settlement failed", err)
	}
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatalf("reservation not released: %d", reserved)
	}
	if v.AwakeSince != nil {
		t.Fatal("awake watermark not cleared")
	}
	current, err = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if err != nil || current.AwakeSince != nil {
		t.Fatal("database awake watermark not cleared", err)
	}
}

func TestSandboxUnknownStopStatusBlocksSave(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "badstatus", "free-1x1", http.StatusAccepted))
	if err := f.s.recordSandboxJob(ctx, v.ID, "job_unknown"); err != nil {
		t.Fatal(err)
	}
	var exports atomic.Int32
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: snapshotStoreArchive(t, 1024), jobs: f.jobs}
		client.stopHook = func(_ context.Context, id string) (wrencomputer.Job, error) {
			return wrencomputer.Job{ID: id, Status: "mystery"}, nil // unknown status fails closed
		}
		client.exportHook = func() error { exports.Add(1); return nil }
		return client, nil
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, f.space.ID, v.ID); err == nil {
		t.Fatal("unknown stop status should fail")
	}
	v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
	if v.Phase != "save_failed" {
		t.Fatalf("phase=%s want save_failed", v.Phase)
	}
	if exports.Load() != 0 {
		t.Fatal("export ran despite unknown stop status")
	}
	if len(f.engine.destroyed) != 0 {
		t.Fatal("VM destroyed despite unknown stop status")
	}
}

func TestSandboxSleepDuringWakeKeepsSavedFiles(t *testing.T) {
	run := func(t *testing.T, mode string) {
		f := sandboxFixture(t)
		ctx := context.Background()
		f.enable(t)
		v := f.ready(t, f.create(t, "durstops", "free-1x1", http.StatusAccepted))
		var snapID, ckptKey, ckptVersion string
		var archive []byte
		if mode == "legacy" {
			version, sealed, err := f.h.config.Secrets.seal([]byte("legacy bytes"), secretBinding(v.WorkspaceID, v.ID, "sandbox-checkpoint"))
			if err != nil {
				t.Fatal(err)
			}
			obj, err := f.engine.PutComputerCheckpoint(ctx, v.checkpointScope(), sealed)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(obj)
			if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET checkpoint_object_ref=$2,checkpoint_key_version=$3 WHERE id=$1`, v.ID, string(raw), version); err != nil {
				t.Fatal(err)
			}
			ckptKey, ckptVersion = obj.Key, version
		} else {
			archive = snapshotStoreArchive(t, 1024)
			if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
				t.Fatal(err)
			}
			v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
			saved, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: archive}, false)
			if err != nil {
				t.Fatal(err)
			}
			snapID = saved.ID
			v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
		}
		// Mid-wake: new generation, applied VM, awake watermark.
		if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='waking',generation=generation+1,sleep_requested=false,awake_since=now(),address='192.0.2.10'::inet,host_fingerprint='SHA256:x' WHERE id=$1`, v.ID); err != nil {
			t.Fatal(err)
		}
		if mode == "forcecap" {
			// Free-cap forced stop — no explicit sleep request; Reconcile forces it.
			if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_free_usage(workspace_id,month,awake_seconds) VALUES($1,$2,$3) ON CONFLICT(workspace_id,month) DO UPDATE SET awake_seconds=EXCLUDED.awake_seconds`, f.space.ID, sandboxMonth(f.s.now()), f.h.config.Sandboxes.freeSecondsPerMonth()); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := f.h.RequestSandboxSleep(ctx, v.WorkspaceID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
				t.Fatal(err)
			}
		}
		f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
			t.Fatal("guest dialed for unfinished wake stop")
			return nil, nil
		}
		baseline, _ := f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
		d := &SandboxDispatcher{Server: f.h}
		f.engine.destroyErr = fmt.Errorf("provider down")
		if err := d.Reconcile(ctx, v.WorkspaceID, v.ID); err == nil {
			t.Fatal("destroy error should propagate")
		}
		v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
		if v.Phase != "waking" || !v.SleepRequested {
			t.Fatalf("cancelled wake not durable: phase=%s sleep=%v", v.Phase, v.SleepRequested)
		}
		if v.AwakeSince == nil {
			t.Fatal("awake watermark lost before teardown confirmed")
		}
		if f.engine.applies.Load() != 0 {
			t.Fatalf("VM re-applied on stopped wake: %d", f.engine.applies.Load())
		}
		if mode != "legacy" && v.CurrentSnapshotID != snapID {
			t.Fatalf("snapshot pointer changed: %s→%s", snapID, v.CurrentSnapshotID)
		}
		f.engine.destroyErr = nil
		if err := d.Reconcile(ctx, v.WorkspaceID, v.ID); err != nil {
			t.Fatalf("cleanup reconcile: %v", err)
		}
		v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
		if v.Phase != "asleep" {
			t.Fatalf("phase=%s want asleep after cleanup", v.Phase)
		}
		if v.Address != "" || v.HostFingerprint != "" {
			t.Fatalf("VM handles retained after stop: %s", v.Address)
		}
		if v.AwakeSince != nil {
			t.Fatal("awake watermark not settled")
		}
		if f.engine.applies.Load() != 0 {
			t.Fatalf("VM re-applied during cleanup: %d", f.engine.applies.Load())
		}
		if mode == "legacy" {
			if ckptKey == "" || len(f.engine.objects[ckptKey]) == 0 {
				t.Fatal("legacy checkpoint erased on stopped wake")
			}
			var ref string
			var keyVer string
			if err := f.s.pool.QueryRow(ctx, `SELECT COALESCE(checkpoint_object_ref::text,''),COALESCE(checkpoint_key_version,'') FROM sandboxes WHERE id=$1`, v.ID).Scan(&ref, &keyVer); err != nil {
				t.Fatal(err)
			}
			if keyVer != ckptVersion || ref == "" {
				t.Fatalf("legacy ref/key version changed: %s %s", ref, keyVer)
			}
			sealed, err := f.engine.GetComputerCheckpoint(ctx, baseline.checkpointScope(), baseline.CheckpointObject)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := f.h.config.Secrets.open(baseline.CheckpointKey, sealed, secretBinding(v.WorkspaceID, v.ID, "sandbox-checkpoint"))
			if err != nil || !bytes.Equal(plain, []byte("legacy bytes")) {
				t.Fatalf("legacy decrypt: %v", err)
			}
		} else {
			if v.CurrentSnapshotID != snapID {
				t.Fatalf("snapshot pointer changed: %s→%s", snapID, v.CurrentSnapshotID)
			}
			reader, _, err := f.h.openSandboxSnapshot(ctx, v.WorkspaceID, snapID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(got, archive) {
				t.Fatalf("snapshot readback: %v", err)
			}
		}
	}
	t.Run("v2", func(t *testing.T) { run(t, "v2") })
	t.Run("legacy", func(t *testing.T) { run(t, "legacy") })
	t.Run("v2-freecap", func(t *testing.T) { run(t, "forcecap") })
}

func TestSandboxPaidRepeatedSleepWake(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_usage_caps(workspace_id,limit_cents,reserved_cents,spent_cents) VALUES($1,100000,0,0) ON CONFLICT (workspace_id) DO UPDATE SET limit_cents=100000,reserved_cents=0,spent_cents=0`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	f.enable(t)
	v := f.ready(t, f.create(t, "paidcycle", "s-1x2", http.StatusAccepted))
	var originalID string
	var originalAmount int
	if err := f.s.pool.QueryRow(ctx, `SELECT id,amount_cents FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox' AND subject_id=$2 AND phase='reserved'`, f.space.ID, v.ID).Scan(&originalID, &originalAmount); err != nil || originalAmount <= 0 {
		t.Fatalf("missing initial reservation: %v", err)
	}
	archive := snapshotStoreArchive(t, 1024)
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: archive, jobs: f.jobs}
		f.lastClient = client
		return client, nil
	}
	d := &SandboxDispatcher{Server: f.h}
	reservation := func() (string, int, string) {
		var rid, phase string
		var amount int
		if err := f.s.pool.QueryRow(ctx, `SELECT id,amount_cents,phase FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox' AND subject_id=$2 ORDER BY created_at DESC LIMIT 1`, f.space.ID, v.ID).Scan(&rid, &amount, &phase); err != nil {
			t.Fatal(err)
		}
		return rid, amount, phase
	}
	reserved := func() int {
		var cents int
		if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&cents); err != nil {
			t.Fatal(err)
		}
		return cents
	}
	totalRows := func() int {
		var n int
		if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox' AND subject_id=$2`, f.space.ID, v.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for cycle := 0; cycle < 2; cycle++ {
		if err := f.h.RequestSandboxSleep(ctx, f.space.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
			t.Fatalf("sleep request %d: %v", cycle, err)
		}
		if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
			t.Fatalf("sleep %d: %v", cycle, err)
		}
		v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
		if v.Phase != "asleep" {
			t.Fatalf("sleep %d phase=%s", cycle, v.Phase)
		}
		rid, amount, phase := reservation()
		if rid != originalID || amount != originalAmount || phase != "released" {
			t.Fatalf("sleep %d reservation id=%s amount=%d phase=%s", cycle, rid, amount, phase)
		}
		if reserved() != 0 {
			t.Fatalf("sleep %d reserved=%d want 0", cycle, reserved())
		}
		if err := f.h.RequestSandboxWake(ctx, f.space.ID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
			t.Fatalf("wake %d: %v", cycle, err)
		}
		v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
		if err := d.Reconcile(ctx, f.space.ID, v.ID); err != nil {
			t.Fatalf("wake reconcile %d: %v", cycle, err)
		}
		v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
		if v.Phase != "ready" {
			t.Fatalf("wake %d phase=%s", cycle, v.Phase)
		}
		rid, amount, phase = reservation()
		if rid != originalID || amount != originalAmount || phase != "reserved" {
			t.Fatalf("wake %d reservation id=%s amount=%d phase=%s", cycle, rid, amount, phase)
		}
		if reserved() != originalAmount {
			t.Fatalf("wake %d reserved=%d want %d", cycle, reserved(), originalAmount)
		}
		if totalRows() != 1 {
			t.Fatalf("wake %d has %d total reservation rows want 1", cycle, totalRows())
		}
		if f.lastClient == nil || len(f.lastClient.restored) != 1 || !bytes.Equal(f.lastClient.restored[0], archive) {
			t.Fatalf("wake %d restore missing or wrong bytes", cycle)
		}
		f.lastClient = nil
	}
}

func TestSandboxSaveCASPreservesConcurrentSleep(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	d := &SandboxDispatcher{Server: f.h}
	run := func(t *testing.T, phase string) {
		v := f.ready(t, f.create(t, "cassleep-"+phase, "free-1x1", http.StatusAccepted))
		if phase != "ready" {
			if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase=$2 WHERE id=$1`, v.ID, phase); err != nil {
				t.Fatal(err)
			}
			v, _ = f.s.Sandbox(ctx, f.space.ID, v.ID)
		}
		// Stale view carries sleep_requested=false; a real request flips it in DB.
		stale := v
		stale.SleepRequested = false
		if err := f.h.RequestSandboxSleep(ctx, f.space.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
			t.Fatal(err)
		}
		if err := d.save(ctx, &stale); err != nil {
			t.Fatalf("stale %s save rejected: %v", phase, err)
		}
		if !stale.SleepRequested {
			t.Fatalf("%s RETURNING sleep_requested lost the concurrent request", phase)
		}
		fresh, _ := f.s.Sandbox(ctx, f.space.ID, v.ID)
		if !fresh.SleepRequested {
			t.Fatalf("%s concurrent sleep request cleared by stale save", phase)
		}
	}
	t.Run("ready", func(t *testing.T) { run(t, "ready") })
	t.Run("waking", func(t *testing.T) { run(t, "waking") })
}

// ---- Desktop backend tests ----

// sandboxDesktopFakeClient is a desktop-capable fake agent: Probe reports
// Desktop=true; Screenshot/ScreenStream/Input implement the optional surface.
type sandboxDesktopFakeClient struct {
	*sandboxFakeClient
	screenshot []byte
	stream     chan []byte
	inputs     chan wrencomputer.DesktopInput
	inputFn    func(context.Context, wrencomputer.DesktopInput) error
	streamFn   func(context.Context, func([]byte) error) error
	entered    chan struct{}
	release    chan struct{}
}

func (c *sandboxDesktopFakeClient) Probe(context.Context) (wrencomputer.Capabilities, error) {
	caps, _ := c.sandboxFakeClient.Probe(context.Background())
	caps.Desktop = true
	return caps, nil
}
func (c *sandboxDesktopFakeClient) Screenshot(context.Context) ([]byte, error) {
	return c.screenshot, nil
}
func (c *sandboxDesktopFakeClient) ScreenStream(ctx context.Context, receive func([]byte) error) error {
	if c.streamFn != nil {
		return c.streamFn(ctx, receive)
	}
	for {
		select {
		case frame, ok := <-c.stream:
			if !ok {
				return nil
			}
			if err := receive(frame); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (c *sandboxDesktopFakeClient) Input(ctx context.Context, input wrencomputer.DesktopInput) error {
	if c.inputFn != nil {
		return c.inputFn(ctx, input)
	}
	return nil
}

func desktopFixtureClient(t *testing.T, f *sandboxFixtureT) *sandboxDesktopFakeClient {
	t.Helper()
	fake := &sandboxDesktopFakeClient{
		sandboxFakeClient: &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, jobs: f.jobs},
		screenshot:        mustPNG(t),
		stream:            make(chan []byte, 4),
		inputs:            make(chan wrencomputer.DesktopInput, 4),
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	f.h.sandboxDial = func(config wrencomputer.Config) (sandboxAgentClient, error) {
		return fake, nil
	}
	return fake
}

func TestSandboxDesktopCreation(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	// free-1x1 (1 vCPU / 1 GiB) and s-1x2 (1 vCPU / 2 GiB) too small — rejected
	// before any provider work.
	appliesBefore := f.engine.applies.Load()
	for _, size := range []string{"free-1x1", "s-1x2"} {
		resp := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
			map[string]any{"name": "desktop", "size": size, "desktop": true}, f.cookie)
		requireStatus(t, resp, http.StatusConflict)
	}
	if f.engine.applies.Load() != appliesBefore {
		t.Fatal("rejected desktop creation provisioned a VM")
	}
	// Paid s-2x4 needs active billing first.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active') ON CONFLICT(workspace_id) DO UPDATE SET status='active'`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	key := "desktop-key-1"
	resp := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "desktop", "size": "s-2x4", "desktop": true, "idempotencyKey": key}, f.cookie)
	requireStatus(t, resp, http.StatusAccepted)
	var created Sandbox
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, created.ID)
	if err != nil || !v.Desktop {
		t.Fatalf("desktop flag not persisted: %v", err)
	}
	// Same key+body → same ID; changed desktop → conflict.
	resp = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "desktop", "size": "s-2x4", "desktop": true, "idempotencyKey": key}, f.cookie)
	requireStatus(t, resp, http.StatusAccepted)
	var replay Sandbox
	if err := json.Unmarshal(resp.Body.Bytes(), &replay); err != nil || replay.ID != created.ID {
		t.Fatal("idempotent replay returned different sandbox")
	}
	resp = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "desktop", "size": "s-2x4", "desktop": false, "idempotencyKey": key}, f.cookie)
	requireStatus(t, resp, http.StatusConflict)
	// Headless creation unchanged.
	resp = requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "headless", "size": "s-2x4"}, f.cookie)
	requireStatus(t, resp, http.StatusAccepted)
}

func TestSandboxDesktopControl(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	if _, err := f.s.pool.Exec(context.Background(), `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	resp := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "desktop", "size": "s-2x4", "desktop": true}, f.cookie)
	requireStatus(t, resp, http.StatusAccepted)
	var created Sandbox
	_ = json.Unmarshal(resp.Body.Bytes(), &created)
	v := f.ready(t, created)
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	ctx := context.Background()

	// Non-owner member via a second workspaceSession on the same store.
	other, _, _ := workspaceSession(t, f.s, "desktop-member@example.com")
	// Make other a member of f.space via direct insert (default owner → demote).
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO memberships(workspace_id,account_id,role) VALUES($1,$2,'operator') ON CONFLICT DO NOTHING`, f.space.ID, other.ID); err != nil {
		t.Fatal(err)
	}

	// Initial state: agent mode, no token to non-holder.
	state, err := f.s.SandboxDesktopControl(ctx, f.space.ID, v.ID, other.ID)
	if err != nil || state.Mode != "agent" || state.LeaseToken != "" {
		t.Fatalf("initial state: %v", state)
	}
	// Take control.
	state, err = f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation})
	if err != nil || state.Mode != "human" || state.AccountID != f.account.ID || state.LeaseToken == "" {
		t.Fatalf("take control: %v err=%v", state, err)
	}
	token := state.LeaseToken
	// Other member reads without token.
	state, err = f.s.SandboxDesktopControl(ctx, f.space.ID, v.ID, other.ID)
	if err != nil || state.Mode != "human" || state.LeaseToken != "" {
		t.Fatal("other member saw lease token")
	}
	// Other can't steal.
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, other.ID, sdk.ActorRef{Kind: "human", ID: other.ID},
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation}); err == nil {
		t.Fatal("steal succeeded")
	}
	// Wrong token renew fails.
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation, LeaseToken: "wrong"}); err == nil {
		t.Fatal("wrong-token renew succeeded")
	}
	// Same token renews.
	state, err = f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation, LeaseToken: token})
	if err != nil || state.LeaseToken != token {
		t.Fatalf("renew: %v", err)
	}
	// Release needs own token.
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, other.ID, sdk.ActorRef{Kind: "human", ID: other.ID},
		SandboxDesktopControlInput{Mode: "agent", ExpectedGeneration: v.Generation, LeaseToken: token}); err == nil {
		t.Fatal("other released")
	}
	state, err = f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "agent", ExpectedGeneration: v.Generation, LeaseToken: token})
	if err != nil || state.Mode != "agent" {
		t.Fatalf("release: %v", err)
	}
	// Expired lease → agent presentation; stale token renew must fail
	// ErrConflict (not silent reacquire), then fresh empty-token claim works.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_desktop_control(sandbox_id,account_id,generation,lease_token,expires_at) VALUES($1,$2,$3,'stale',now()-interval '1 second')`, v.ID, f.account.ID, v.Generation); err != nil {
		t.Fatal(err)
	}
	state, err = f.s.SandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID)
	if err != nil || state.Mode != "agent" {
		t.Fatal("expired lease not agent")
	}
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation, LeaseToken: "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired-token renew: %v want ErrConflict", err)
	}
	state, err = f.s.SandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID)
	if err != nil || state.Mode != "agent" || state.LeaseToken != "" {
		t.Fatal("stale renew acquired a lease")
	}
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation}); err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
	// Viewer demotion invalidates holder role.
	if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID); err != nil {
		t.Fatal(err)
	}
	state, err = f.s.SandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID)
	if err != nil || state.Mode != "agent" {
		t.Fatal("demoted holder still human")
	}
	// Restore role for cleanup.
	_, _ = f.s.pool.Exec(ctx, `UPDATE memberships SET role='owner' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID)
}

func TestSandboxDesktopMutationFences(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	if _, err := f.s.pool.Exec(context.Background(), `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	resp := requestJSON(t, f.h, http.MethodPost, "/v1/workspaces/"+f.space.ID+"/sandboxes",
		map[string]any{"name": "desktop", "size": "s-2x4", "desktop": true}, f.cookie)
	requireStatus(t, resp, http.StatusAccepted)
	var created Sandbox
	_ = json.Unmarshal(resp.Body.Bytes(), &created)
	v := f.ready(t, created)
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	ctx := context.Background()

	// Open job blocks human claim.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_jobs(sandbox_id,job_id,started_at) VALUES($1,'job_1',now())`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation}); err == nil {
		t.Fatal("claim allowed with open job")
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_jobs SET finished_at=now() WHERE sandbox_id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	// Live mutation blocks human claim.
	op, err := f.s.beginSandboxMutation(ctx, v, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation}); err == nil {
		t.Fatal("claim allowed during live mutation")
	}
	op.close()
	// Claim succeeds.
	state, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation})
	if err != nil {
		t.Fatal(err)
	}
	token := state.LeaseToken
	fake := desktopFixtureClient(t, f)
	// Ordinary mutation blocked while human holds control — real helper calls.
	if _, err := f.s.beginSandboxMutation(ctx, v, 30*time.Second); err == nil {
		t.Fatal("ordinary mutation allowed under human control")
	}
	if _, err := f.s.beginSandboxMutationWithControl(ctx, v, 20*time.Second, &sandboxDesktopInputClaim{}); err == nil {
		t.Fatal("agent input allowed under human control")
	}
	// Human input with correct claim succeeds through the real helper.
	if err := f.h.sandboxDesktopInput(ctx, v, wrencomputer.DesktopInput{Kind: "click", X: 1, Y: 1},
		sandboxDesktopInputClaim{AccountID: f.account.ID, LeaseToken: token}); err != nil {
		t.Fatalf("human input: %v", err)
	}
	// Blocked input: entered/release channels hold the RPC deterministically.
	fake.inputFn = func(ctx context.Context, in wrencomputer.DesktopInput) error {
		close(fake.entered)
		select {
		case <-fake.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	inputDone := make(chan error, 1)
	go func() {
		inputDone <- f.h.sandboxDesktopInput(ctx, v, wrencomputer.DesktopInput{Kind: "click", X: 1, Y: 1},
			sandboxDesktopInputClaim{AccountID: f.account.ID, LeaseToken: token})
	}()
	<-fake.entered // input genuinely in flight
	// While input in flight: release + second input + ordinary mutation all conflict.
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "agent", ExpectedGeneration: v.Generation, LeaseToken: token}); !errors.Is(err, ErrConflict) {
		close(fake.release)
		<-inputDone
		t.Fatalf("release during input: %v", err)
	}
	for _, claim := range []sandboxDesktopInputClaim{{AccountID: f.account.ID, LeaseToken: token}, {}} {
		if err := f.h.sandboxDesktopInput(ctx, v, wrencomputer.DesktopInput{Kind: "click"}, claim); !errors.Is(err, ErrConflict) {
			close(fake.release)
			<-inputDone
			t.Fatalf("overlapping input: %v", err)
		}
	}
	if next, err := f.s.beginSandboxMutation(ctx, v, time.Second); !errors.Is(err, ErrConflict) {
		if next != nil {
			next.close()
		}
		close(fake.release)
		<-inputDone
		t.Fatalf("ordinary mutation during input: %v", err)
	}
	close(fake.release) // let the in-flight input finish
	if err := <-inputDone; err != nil {
		t.Fatalf("in-flight input: %v", err)
	}
	if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
		SandboxDesktopControlInput{Mode: "agent", ExpectedGeneration: v.Generation, LeaseToken: token}); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Ordinary mutation allowed again.
	op2, err := f.s.beginSandboxMutation(ctx, v, 30*time.Second)
	if err != nil {
		t.Fatalf("ordinary mutation after release: %v", err)
	}
	op2.close()

	// Uncertain-RPC failure: mutation row retained until expiry, blocks next.
	lostReply := errors.New("input reply lost")
	fake.inputFn = func(context.Context, wrencomputer.DesktopInput) error { return lostReply }
	if err := f.h.sandboxDesktopInput(ctx, v, wrencomputer.DesktopInput{Kind: "click"}, sandboxDesktopInputClaim{}); !errors.Is(err, lostReply) {
		t.Fatalf("uncertain input result: %v", err)
	}
	var uncertain int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_mutations WHERE sandbox_id=$1 AND desktop_input AND expires_at>clock_timestamp()`, v.ID).Scan(&uncertain); err != nil || uncertain != 1 {
		t.Fatalf("uncertain input claims=%d: %v", uncertain, err)
	}
	// A live agent-input row blocks ordinary mutations.
	if _, err := f.s.beginSandboxMutation(ctx, v, 30*time.Second); err == nil {
		t.Fatal("ordinary mutation allowed during in-flight agent input")
	}
	// Expire it locally (test-only); next ordinary mutation succeeds.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_mutations SET expires_at=clock_timestamp()-interval '1 second' WHERE sandbox_id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	op3, err := f.s.beginSandboxMutation(ctx, v, 30*time.Second)
	if err != nil {
		t.Fatalf("ordinary mutation after expiry: %v", err)
	}
	op3.close()
}

func TestSandboxDesktopScreenAuthorization(t *testing.T) {
	t.Run("framing_and_failures", func(t *testing.T) {
		f, v, fake := readyDesktopFixture(t)
		base := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID + "/desktop"
		viewer, _, cookie := workspaceSession(t, f.s, "desktop-viewer@example.com")
		if _, err := f.s.pool.Exec(context.Background(), `INSERT INTO memberships(workspace_id,account_id,role) VALUES($1,$2,'viewer')`, f.space.ID, viewer.ID); err != nil {
			t.Fatal(err)
		}
		shot := requestJSON(t, f.h, http.MethodGet, base+"/screenshot", nil, cookie)
		requireStatus(t, shot, http.StatusOK)
		if shot.Header().Get("Content-Type") != "image/png" || !bytes.Equal(shot.Body.Bytes(), fake.screenshot) || shot.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("viewer screenshot bytes or headers changed")
		}
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
			t.Fatal(err)
		}
		frame := encoded.Bytes()
		server := httptest.NewServer(f.h)
		defer server.Close()
		client := server.Client()
		client.Timeout = 5 * time.Second
		for _, ending := range []string{"clean", "failed", "unsupported"} {
			fake.streamFn = func(ctx context.Context, receive func([]byte) error) error {
				if ending == "unsupported" {
					return wrencomputer.ErrScreenUnsupported
				}
				if err := receive(frame); err != nil {
					return err
				}
				if ending == "failed" {
					return errors.New("stream interrupted")
				}
				return nil
			}
			request, err := http.NewRequest(http.MethodGet, server.URL+base+"/screen", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(cookie)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if ending == "unsupported" {
				if response.StatusCode != http.StatusNotImplemented {
					t.Fatalf("unsupported: HTTP %d", response.StatusCode)
				}
				continue
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != wrencomputer.ScreenContentType || response.Header.Get("Cache-Control") != "no-store, no-transform" {
				t.Fatalf("%s: incorrect stream status/headers: %d %v", ending, response.StatusCode, response.Header)
			}
			want := make([]byte, 4+len(frame))
			binary.BigEndian.PutUint32(want, uint32(len(frame)))
			copy(want[4:], frame)
			if ending == "clean" {
				want = append(want, 0, 0, 0, 0)
			}
			if !bytes.Equal(body, want) {
				t.Fatalf("%s: frame or end marker changed", ending)
			}
		}
		for _, invalid := range [][]byte{[]byte("not a PNG"), make([]byte, (1<<20)+1)} {
			fake.screenshot = invalid
			requireStatus(t, requestJSON(t, f.h, http.MethodGet, base+"/screenshot", nil, cookie), http.StatusConflict)
		}
		for _, suffix := range []string{"control", "input", "screenshot", "screen"} {
			requireStatus(t, requestJSON(t, f.h, http.MethodGet, base+"/"+suffix+"/extra", nil, cookie), http.StatusNotFound)
		}
		if _, err := f.s.pool.Exec(context.Background(), `UPDATE sandboxes SET desktop=false WHERE id=$1`, v.ID); err != nil {
			t.Fatal(err)
		}
		requireStatus(t, requestJSON(t, f.h, http.MethodGet, base+"/screenshot", nil, cookie), http.StatusConflict)
	})
	for _, revoke := range []string{"generation", "membership", "session", "sleep"} {
		t.Run(revoke, func(t *testing.T) {
			f, v, fake := readyDesktopFixture(t)
			viewer, _, cookie := workspaceSession(t, f.s, "desktop-stream-viewer@example.com")
			ctx := context.Background()
			if _, err := f.s.pool.Exec(ctx, `INSERT INTO memberships(workspace_id,account_id,role) VALUES($1,$2,'viewer')`, f.space.ID, viewer.ID); err != nil {
				t.Fatal(err)
			}
			entered, cancelled := make(chan struct{}), make(chan struct{})
			frame := []byte("frame")
			fake.streamFn = func(ctx context.Context, receive func([]byte) error) error {
				if err := receive(frame); err != nil {
					return err
				}
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			}
			server := httptest.NewServer(f.h)
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/workspaces/"+f.space.ID+"/sandboxes/"+v.ID+"/desktop/screen", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(cookie)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			<-entered
			first := make([]byte, 4+len(frame))
			if _, err := io.ReadFull(response.Body, first); err != nil {
				t.Fatal(err)
			}
			if response.Header.Get("Content-Type") != wrencomputer.ScreenContentType || binary.BigEndian.Uint32(first[:4]) != uint32(len(frame)) || !bytes.Equal(first[4:], frame) {
				t.Fatal("viewer did not receive first frame")
			}
			switch revoke {
			case "generation":
				_, err = f.s.pool.Exec(ctx, `UPDATE sandboxes SET generation=generation+1 WHERE id=$1`, v.ID)
			case "membership":
				_, err = f.s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, viewer.ID)
			case "session":
				_, err = f.s.pool.Exec(ctx, `UPDATE human_sessions SET revoked_at=now() WHERE account_id=$1`, viewer.ID)
			case "sleep":
				_, err = f.s.pool.Exec(ctx, `UPDATE sandboxes SET sleep_requested=true WHERE id=$1`, v.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("guest stream retained access after revocation/lifecycle change")
			}
		})
	}
}

func readyDesktopFixture(t *testing.T) (*sandboxFixtureT, Sandbox, *sandboxDesktopFakeClient) {
	t.Helper()
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	v, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxCreateInput{Name: "desktop", Size: "s-2x4", Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	v = f.ready(t, v)
	return f, v, desktopFixtureClient(t, f)
}

func TestSandboxDesktopHTTPAndConcurrentControl(t *testing.T) {
	f, v, fake := readyDesktopFixture(t)
	ctx := context.Background()
	other, foreign, otherCookie := workspaceSession(t, f.s, "desktop-operator@example.com")
	viewer, _, viewerCookie := workspaceSession(t, f.s, "desktop-reader@example.com")
	for _, member := range []struct{ id, role string }{{other.ID, "operator"}, {viewer.ID, "viewer"}} {
		if _, err := f.s.pool.Exec(ctx, `INSERT INTO memberships(workspace_id,account_id,role) VALUES($1,$2,$3)`, f.space.ID, member.id, member.role); err != nil {
			t.Fatal(err)
		}
	}
	base := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID + "/desktop"
	for _, path := range []string{"control", "input"} {
		requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/"+path, map[string]any{"mode": "human", "kind": "click", "expectedGeneration": v.Generation}, viewerCookie), http.StatusForbidden)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodGet, "/v1/workspaces/"+foreign.ID+"/sandboxes/"+v.ID+"/desktop/control", nil, otherCookie), http.StatusNotFound)
	type claimResult struct {
		account string
		state   SandboxDesktopControl
		err     error
	}
	start, results := make(chan struct{}), make(chan claimResult, 2)
	for _, account := range []string{f.account.ID, other.ID} {
		go func(account string) {
			<-start
			state, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, account, sdk.ActorRef{Kind: "human", ID: account}, SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation})
			results <- claimResult{account: account, state: state, err: err}
		}(account)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil {
		first, second = second, first
	}
	if first.err != nil || !errors.Is(second.err, ErrConflict) || first.state.AccountID != first.account || first.state.LeaseToken == "" {
		t.Fatalf("concurrent takeover results: %v / %v", first.err, second.err)
	}
	ownCookie, losingCookie := f.cookie, otherCookie
	if first.account == other.ID {
		ownCookie, losingCookie = otherCookie, f.cookie
	}
	for _, cookie := range []*http.Cookie{losingCookie, viewerCookie} {
		response := requestJSON(t, f.h, http.MethodGet, base+"/control", nil, cookie)
		requireStatus(t, response, http.StatusOK)
		var shown SandboxDesktopControl
		if err := json.Unmarshal(response.Body.Bytes(), &shown); err != nil || shown.Mode != "human" || shown.LeaseToken != "" {
			t.Fatal("non-holder saw a token or incorrect control state")
		}
	}
	var inputs atomic.Int32
	fake.inputFn = func(context.Context, wrencomputer.DesktopInput) error { inputs.Add(1); return nil }
	body := map[string]any{"kind": "click", "x": 1, "y": 2, "expectedGeneration": v.Generation, "leaseToken": first.state.LeaseToken}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/input", body, ownCookie), http.StatusOK)
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/input", body, losingCookie), http.StatusConflict)
	body["leaseToken"] = "wrong"
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/input", body, ownCookie), http.StatusConflict)
	body["leaseToken"] = first.state.LeaseToken
	for _, generation := range []int{0, v.Generation + 1} {
		body["expectedGeneration"] = generation
		requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/input", body, ownCookie), http.StatusConflict)
	}
	body["expectedGeneration"] = v.Generation
	if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, first.account); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/input", body, ownCookie), http.StatusForbidden)
	if inputs.Load() != 1 {
		t.Fatalf("unauthorized/stale input reached guest: %d calls", inputs.Load())
	}
}

func mustPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if err := pngpkg.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
