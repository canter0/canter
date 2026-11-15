package controlplane

// TestSandboxLiveArchiveFork is an explicitly opted-in live integration test:
// it creates real HostVDS free-1x1 sandboxes, seeds a real archive, forks from
// the save, deletes the source, and proves archive independence end to end.
// Ordinary suites skip it — it requires CANTER_SANDBOX_LIVE_AUTHORIZED=true,
// a dedicated canter_sandbox_live_* test DB, and configured provider env.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/envfile"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/canter0/canter/internal/provider/compute"
	wrencomputer "github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

func TestSandboxLiveArchiveFork(t *testing.T) {
	if os.Getenv("CANTER_SANDBOX_LIVE_AUTHORIZED") != "true" {
		t.Skip("requires CANTER_SANDBOX_LIVE_AUTHORIZED=true (real VMs, dedicated live DB)")
	}
	runSandboxLiveArchiveFork(t, false)
}

// TestSandboxLiveDesktop replays the archive/fork flow on a desktop-enabled
// s-2x4 sandbox, additionally proving screenshot/stream/input/human-control.
func TestSandboxLiveDesktop(t *testing.T) {
	if os.Getenv("CANTER_SANDBOX_LIVE_DESKTOP_AUTHORIZED") != "true" {
		t.Skip("requires CANTER_SANDBOX_LIVE_DESKTOP_AUTHORIZED=true (real desktop VM, dedicated live DB)")
	}
	runSandboxLiveArchiveFork(t, true)
}

func runSandboxLiveArchiveFork(t *testing.T, desktop bool) {
	if os.Getenv("CANTER_SANDBOX_LIVE_AUTHORIZED") != "true" {
		t.Skip("requires CANTER_SANDBOX_LIVE_AUTHORIZED=true (real VMs, dedicated live DB)")
	}
	database := os.Getenv("CANTER_TEST_DATABASE_URL")
	// Guard: integrationStore truncates — must be a dedicated live DB.
	parsed, err := url.Parse(database)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatal("CANTER_TEST_DATABASE_URL must be a postgres URL")
	}
	if !strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), "canter_sandbox_live_") {
		t.Fatal("CANTER_TEST_DATABASE_URL must name a dedicated canter_sandbox_live_* database")
	}
	if _, err := envfile.Load(); err != nil {
		t.Fatalf("opt-in live test requires provider env: %v", err)
	}
	// Query params and PGHOST can override host/db — verify the resolved config.
	poolConfig, err := pgxpool.ParseConfig(database)
	if err != nil {
		t.Fatalf("invalid test database URL: %v", err)
	}
	isLocal := func(host string) bool {
		ip := net.ParseIP(host)
		return host == "" || host == "localhost" || strings.HasPrefix(host, "/") || (ip != nil && ip.IsLoopback())
	}
	if !isLocal(poolConfig.ConnConfig.Host) {
		t.Fatalf("live test database must be local, got host %q", poolConfig.ConnConfig.Host)
	}
	for _, fallback := range poolConfig.ConnConfig.Fallbacks {
		if !isLocal(fallback.Host) {
			t.Fatalf("live test fallback must be local, got host %q", fallback.Host)
		}
	}
	if !strings.HasPrefix(poolConfig.ConnConfig.Database, "canter_sandbox_live_") {
		t.Fatalf("live test database must be canter_sandbox_live_*, got %q", poolConfig.ConnConfig.Database)
	}
	engine, err := sdk.NewFromEnv()
	if err != nil {
		t.Fatalf("sdk engine: %v", err)
	}
	provider, err := compute.NewFromEnv()
	if err != nil {
		t.Fatalf("compute provider: %v", err)
	}

	// Scoped HTTP proxy: only provider API hosts relay through the tunnel;
	// everything else stays direct. Restored on cleanup.
	var baseTransport http.RoundTripper = http.DefaultTransport
	if proxyURL := os.Getenv("CANTER_SANDBOX_LIVE_PROXY"); proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Scheme != "socks5" || parsed.User != nil {
			t.Fatal("CANTER_SANDBOX_LIVE_PROXY must be a userinfo-free socks5:// loopback URL")
		}
		host, _, _ := net.SplitHostPort(parsed.Host)
		if !net.ParseIP(host).IsLoopback() {
			t.Fatal("CANTER_SANDBOX_LIVE_PROXY must target loopback")
		}
		direct := http.DefaultTransport.(*http.Transport)
		clone := direct.Clone()
		clone.Proxy = func(req *http.Request) (*url.URL, error) {
			h := req.URL.Hostname()
			if h == "hostvds.com" || strings.HasSuffix(h, ".hostvds.com") {
				return parsed, nil
			}
			return nil, nil
		}
		http.DefaultTransport = clone
		t.Cleanup(func() { http.DefaultTransport = baseTransport })
	}

	cidr := strings.TrimSpace(os.Getenv("CANTER_SANDBOX_LIVE_SSH_CIDR"))
	if cidr == "" {
		t.Fatal("CANTER_SANDBOX_LIVE_SSH_CIDR must be a /32 IPv4 CIDR")
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 {
		t.Fatal("CANTER_SANDBOX_LIVE_SSH_CIDR must be a valid IPv4 /32")
	}
	// Optional: CANTER_SANDBOX_LIVE_IMAGE pins a prebuilt image for faster
	// provisioning; the current bootstrap installs the CNSNAP2 helper either
	// way, so the image is a speed optimization, not a protocol requirement.
	image := os.Getenv("CANTER_SANDBOX_LIVE_IMAGE")

	artifactDir := os.Getenv("CANTER_SANDBOX_LIVE_ARTIFACT_DIR")
	if artifactDir == "" {
		artifactDir = t.TempDir()
	} else if !filepath.IsAbs(artifactDir) {
		t.Fatal("CANTER_SANDBOX_LIVE_ARTIFACT_DIR must be an absolute path")
	}
	if err := os.MkdirAll(artifactDir, 0700); err != nil {
		t.Fatalf("artifact dir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	s := integrationStore(t)
	a, w, cookie := workspaceSession(t, s, "sandbox-live-archive@example.com")
	if desktop {
		if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, w.ID); err != nil {
			t.Fatalf("workspace billing: %v", err)
		}
	}
	h := NewHTTPServer(&Service{Store: s, Engine: engine}, HTTPConfig{
		PublicURL: "http://canter.test", Secrets: testVault(t),
		Sandboxes: SandboxConfig{Enabled: true, SSHSourceCIDR: cidr, ImageID: image},
	}).(*HTTPServer)
	d := &SandboxDispatcher{Server: h}
	actor := sdk.ActorRef{Kind: "human", ID: a.ID}

	// Owned resources tracked up-front; cleanup runs on ANY exit path.
	var ownedIDs []string
	var ownedPlans []sdk.VPSPlan
	var snapshotIDs []string
	var readyProviderIDs []string
	allProviderIDs := map[string]bool{}
	track := func(id string) { ownedIDs = append(ownedIDs, id) }
	trackPlan := func(plan sdk.VPSPlan) {
		if plan.Spec.Metadata.Name == "" {
			return
		}
		for _, existing := range ownedPlans {
			if existing.Spec.Metadata.Name == plan.Spec.Metadata.Name {
				return
			}
		}
		ownedPlans = append(ownedPlans, plan)
	}
	t.Cleanup(func() {
		cleanCtx, stop := context.WithTimeout(context.Background(), 15*time.Minute)
		defer stop()
		pause := func() bool {
			select {
			case <-cleanCtx.Done():
				return false
			case <-time.After(2 * time.Second):
				return true
			}
		}
		for _, id := range ownedIDs {
			if v, err := s.Sandbox(cleanCtx, w.ID, id); err == nil && v.Phase != "deleted" {
				resources, resourceErr := engine.VPSBillingResources(cleanCtx, v.Plan)
				if resourceErr != nil {
					t.Errorf("cleanup resource discovery for %s: %v", id, resourceErr)
				}
				for _, resource := range resources {
					if strings.HasPrefix(resource.ID, "compute/") {
						allProviderIDs[strings.TrimPrefix(resource.ID, "compute/")] = true
					}
				}
			}
			if err := h.RequestSandboxDelete(cleanCtx, w.ID, id, actor); err != nil && !errors.Is(err, ErrNotFound) {
				t.Logf("cleanup request %s: %v", id, err)
			}
			until := time.Now().Add(3 * time.Minute)
			for cleanCtx.Err() == nil && time.Now().Before(until) {
				v, e := s.Sandbox(cleanCtx, w.ID, id)
				if e == nil && v.Phase == "deleted" {
					break
				}
				_ = d.Reconcile(cleanCtx, w.ID, id)
				if !pause() {
					break
				}
			}
		}
		for _, plan := range ownedPlans {
			var last error
			done := false
			for cleanCtx.Err() == nil {
				st, e := engine.DestroyVPS(cleanCtx, plan)
				for _, r := range st.Resources {
					if r.ID != "" {
						allProviderIDs[r.ID] = true
					}
				}
				if e == nil && st.Phase == "destroyed" {
					done = true
					break
				}
				last = e
				if e == nil {
					last = fmt.Errorf("phase %s", st.Phase)
				}
				if !pause() {
					break
				}
			}
			if !done {
				t.Errorf("cleanup plan %s not confirmed destroyed: %v", plan.Spec.Metadata.Name, last)
			}
		}
		for id := range allProviderIDs {
			_, e := provider.Server(cleanCtx, id)
			if !compute.IsNotFound(e) {
				t.Errorf("cleanup provider %s not confirmed absent: %v", id, e)
			} else {
				t.Logf("cleanup provider %s confirmed absent (404)", id)
			}
		}
		for _, id := range ownedIDs {
			if cleanCtx.Err() == nil {
				_ = d.Reconcile(cleanCtx, w.ID, id)
			}
			v, e := s.Sandbox(cleanCtx, w.ID, id)
			if e != nil || v.Phase != "deleted" {
				t.Errorf("cleanup sandbox %s state=%s err=%v", id, v.Phase, e)
			}
		}
	})

	// Enable Sandboxes through the authenticated product route.
	req := requestJSON(t, h, http.MethodPut, fmt.Sprintf("/v1/workspaces/%s/products/sandboxes", w.ID), nil, cookie)
	if req.Code != http.StatusOK && req.Code != http.StatusNoContent {
		t.Fatalf("enable sandboxes: %d %s", req.Code, strings.TrimSpace(req.Body.String()))
	}

	reconcileTo := func(id, want string, timeout time.Duration) Sandbox {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			reconcileErr := d.Reconcile(ctx, w.ID, id)
			v, err := s.Sandbox(ctx, w.ID, id)
			if err != nil {
				t.Fatalf("load %s: %v", id, err)
			}
			waitingForTools := (v.Phase == "provisioning" || v.Phase == "waking") && v.Failure == "The VM is running; waiting for its tools."
			if reconcileErr != nil && !waitingForTools {
				t.Fatalf("reconcile %s -> %s: %v", id, want, reconcileErr)
			}
			if reconcileErr != nil {
				t.Logf("%s waiting for guest tools; retrying within startup deadline", id)
			}
			if v.Phase == "failed" || v.Phase == "save_failed" {
				t.Fatalf("%s entered %s: %s", id, v.Phase, v.Failure)
			}
			if v.Phase == want {
				return v
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("%s did not reach %s in %v", id, want, timeout)
		return Sandbox{}
	}
	// At READY, record the concrete provider IDs for this generation — after
	// sleep/delete those exact IDs must independently 404.
	recordProviderIDs := func(v Sandbox) {
		resources, err := engine.VPSBillingResources(ctx, v.Plan)
		if err != nil {
			t.Fatalf("provider resources failed at ready for %s: %v", v.ID, err)
		}
		computeIDs := 0
		for _, r := range resources {
			if strings.HasPrefix(r.ID, "compute/") {
				computeIDs++
				id := strings.TrimPrefix(r.ID, "compute/")
				readyProviderIDs = append(readyProviderIDs, id)
				allProviderIDs[id] = true
			}
		}
		if computeIDs == 0 {
			t.Fatalf("no compute IDs at ready for %s", v.ID)
		}
	}
	checkProviderGone := func(v Sandbox) {
		for _, id := range readyProviderIDs {
			if _, err := provider.Server(ctx, id); !compute.IsNotFound(err) {
				t.Fatalf("provider server %s still present after %s: %v", id, v.Phase, err)
			}
		}
	}

	runJob := func(v Sandbox, command string, timeout time.Duration) {
		job, err := h.sandboxJobStart(ctx, v, SandboxExecInput{Command: "set -eu\n" + command, TimeoutSeconds: 180}, actor)
		if err != nil {
			t.Fatalf("job start: %v", err)
		}
		client, err := h.sandboxWorkflowClient(ctx, v)
		if err != nil {
			t.Fatalf("job client: %v", err)
		}
		defer client.Close()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			fresh, err := client.Job(ctx, job.ID)
			if err != nil {
				t.Fatalf("job poll: %v", err)
			}
			if fresh.Status == "completed" || fresh.Status == "failed" || fresh.Status == "stopped" {
				s.finishSandboxJob(ctx, v.ID, job.ID)
				if fresh.Status != "completed" {
					t.Fatalf("job %s ended %s", job.ID, fresh.Status)
				}
				if fresh.ExitCode == nil || *fresh.ExitCode != 0 {
					t.Fatalf("job %s exit code %v", job.ID, fresh.ExitCode)
				}
				return
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("job %s did not finish in %v", job.ID, timeout)
	}

	// ---- 1) Create source, seed, download, sleep, snapshot assertions ----
	createSize, createDesktop := "free-1x1", false
	if desktop {
		createSize, createDesktop = "s-2x4", true
	}
	created, err := h.CreateSandbox(ctx, w.ID, a.ID, actor, SandboxCreateInput{
		Name: "live-archive-source", Size: createSize, Desktop: createDesktop, IdempotencyKey: fmt.Sprintf("live-archive-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	track(created.ID)
	trackPlan(created.Plan)
	source := reconcileTo(created.ID, "ready", 12*time.Minute)
	recordProviderIDs(source)

	if desktop {
		runSandboxLiveDesktopProof(t, ctx, h, s, w.ID, a.ID, actor, source, artifactDir)
	}

	const seedCommand = `python3 -m venv --without-pip /workspace/.venv
python3 - <<'PY'
import hashlib,json,os,pathlib,stat
w=pathlib.Path('/workspace'); h=pathlib.Path.home()
(w/'project/node_modules/canter-live').mkdir(parents=True,exist_ok=True)
(w/'project/node_modules/canter-live/index.js').write_text('module.exports = 73;\n')
(h/'.local/bin').mkdir(parents=True,exist_ok=True)
p=h/'.local/bin/canter-live-helper'; p.write_text('#!/bin/sh\nprintf "home-tool-ok\\n"\n'); p.chmod(0o755)
b=w/'project/data.bin'; b.write_bytes(os.urandom(40*1024*1024)); b.chmod(0o640)
os.utime(b,ns=(1730000000123456789,1730000000123456789))
os.symlink('data.bin',w/'project/data-link')
os.link(b,w/'project/data-hardlink')
(w/'project/hello-世界.txt').write_text('persistent unicode ✓\n')
expected={'sha256':hashlib.sha256(b.read_bytes()).hexdigest(),'size':b.stat().st_size,'mode':stat.S_IMODE(b.stat().st_mode),'mtimeNs':b.stat().st_mtime_ns}
(w/'live-expected.json').write_text(json.dumps(expected,sort_keys=True))
print(json.dumps(expected,sort_keys=True))
PY
/workspace/.venv/bin/python -c 'print("venv-ok")'
node -e 'if(require("/workspace/project/node_modules/canter-live")!==73) process.exit(1)'
$HOME/.local/bin/canter-live-helper`
	runJob(source, seedCommand, 3*time.Minute)

	// Capture expected JSON once from the guest's own seeded file.
	dlReq := requestJSON(t, h, http.MethodGet, fmt.Sprintf("/v1/workspaces/%s/sandboxes/%s/files/content?path=%%2Fworkspace%%2Flive-expected.json", w.ID, source.ID), nil, cookie)
	if dlReq.Code != http.StatusOK {
		t.Fatalf("expected download: %d", dlReq.Code)
	}
	var expected struct {
		SHA256  string `json:"sha256"`
		Size    int64  `json:"size"`
		Mode    int    `json:"mode"`
		MtimeNs int64  `json:"mtimeNs"`
	}
	if err := json.NewDecoder(dlReq.Body).Decode(&expected); err != nil {
		t.Fatalf("decode expected: %v", err)
	}
	if expected.Size != 40*1024*1024 || expected.Mode != 0640 || expected.MtimeNs != 1730000000123456789 || len(expected.SHA256) != 64 {
		t.Fatalf("unexpected seeded expectations: %v", expected)
	}
	// Real binary download: sha256 must equal captured expectation.
	binReq := requestJSON(t, h, http.MethodGet, fmt.Sprintf("/v1/workspaces/%s/sandboxes/%s/files/content?path=%%2Fworkspace%%2Fproject%%2Fdata.bin", w.ID, source.ID), nil, cookie)
	if binReq.Code != http.StatusOK {
		t.Fatalf("binary download: %d", binReq.Code)
	}
	binBody := binReq.Body.Bytes()
	if int64(len(binBody)) != expected.Size {
		t.Fatalf("download size %d != %d", len(binBody), expected.Size)
	}
	if hex.EncodeToString(func() []byte { h := sha256.Sum256(binBody); return h[:] }()) != expected.SHA256 {
		t.Fatal("downloaded binary sha256 != seeded expectation")
	}

	if err := h.RequestSandboxSleep(ctx, w.ID, source.ID, actor); err != nil {
		t.Fatalf("request sleep: %v", err)
	}
	sourceAsleep := reconcileTo(source.ID, "asleep", 12*time.Minute)
	snap, err := s.SandboxSnapshot(ctx, w.ID, sourceAsleep.CurrentSnapshotID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.State != "ready" || snap.Stats.Format != "canter-sandbox-files-v2" || snap.StoredBytes <= 32*1024*1024 || sourceAsleep.CurrentSnapshotID == "" {
		t.Fatalf("bad source snapshot: state=%s format=%s bytes=%d", snap.State, snap.Stats.Format, snap.StoredBytes)
	}
	snapshotIDs = append(snapshotIDs, snap.ID)
	// Source provider server gone — check the IDs recorded while it was ready.
	checkProviderGone(sourceAsleep)
	readyProviderIDs = nil

	// ---- 2) Fork from save, delete source, reconcile fork ready, validate ----
	fork, err := h.CreateSandbox(ctx, w.ID, a.ID, actor, SandboxCreateInput{
		Name: "live-archive-fork", Size: createSize, Desktop: createDesktop, SnapshotID: snap.ID,
		IdempotencyKey: fmt.Sprintf("live-fork-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("create fork: %v", err)
	}
	track(fork.ID)
	trackPlan(fork.Plan)
	if fork.CurrentSnapshotID != snap.ID || fork.Phase != "waking" {
		t.Fatalf("fork snap=%s phase=%s", fork.CurrentSnapshotID, fork.Phase)
	}
	// Delete the source before the fork's restore completes — shared save survives.
	if err := h.RequestSandboxDelete(ctx, w.ID, source.ID, actor); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	reconcileTo(source.ID, "deleted", 12*time.Minute)
	still, err := s.SandboxSnapshot(ctx, w.ID, snap.ID)
	if err != nil || still.State != "ready" {
		t.Fatal("shared snapshot not ready after source delete")
	}
	forkReady := reconcileTo(fork.ID, "ready", 12*time.Minute)
	recordProviderIDs(forkReady)

	validateCommand := fmt.Sprintf(`python3 - <<'PY'
import hashlib,json,os,pathlib,stat
w=pathlib.Path('/workspace'); b=w/'project/data.bin'
expected=%s
assert b.stat().st_size==expected['size']
assert hashlib.sha256(b.read_bytes()).hexdigest()==expected['sha256']
assert stat.S_IMODE(b.stat().st_mode)==expected['mode']
assert b.stat().st_mtime_ns==expected['mtimeNs']
assert os.readlink(w/'project/data-link')=='data.bin'
assert os.stat(b).st_ino==os.stat(w/'project/data-hardlink').st_ino
assert (w/'project/hello-世界.txt').read_text()=='persistent unicode ✓\n'
print('archive-verified')
PY
/workspace/.venv/bin/python -c 'print("venv-ok")'
node -e 'if(require("/workspace/project/node_modules/canter-live")!==73) process.exit(1)'
$HOME/.local/bin/canter-live-helper`,
		fmt.Sprintf(`{"sha256":"%s","size":%d,"mode":%d,"mtimeNs":%d}`, expected.SHA256, expected.Size, expected.Mode, expected.MtimeNs))
	runJob(forkReady, validateCommand, 3*time.Minute)

	// ---- 3) Fork writes after-fork, sleeps, wakes — revalidate + new file ----
	runJob(forkReady, `printf 'fork roundtrip\n' > /workspace/after-fork.txt`, time.Minute)
	if err := h.RequestSandboxSleep(ctx, w.ID, fork.ID, actor); err != nil {
		t.Fatalf("fork sleep: %v", err)
	}
	forkAsleep := reconcileTo(fork.ID, "asleep", 12*time.Minute)
	checkProviderGone(forkAsleep)
	readyProviderIDs = nil
	if forkAsleep.CurrentSnapshotID == "" || forkAsleep.CurrentSnapshotID == snap.ID {
		t.Fatal("fork did not publish a new snapshot")
	}
	snapshotIDs = append(snapshotIDs, forkAsleep.CurrentSnapshotID)
	if err := h.RequestSandboxWake(ctx, w.ID, fork.ID, a.ID, actor); err != nil {
		t.Fatalf("fork wake: %v", err)
	}
	woken, err := s.Sandbox(ctx, w.ID, fork.ID)
	if err != nil {
		t.Fatalf("reload fork for plan: %v", err)
	}
	trackPlan(woken.Plan)
	forkWoken := reconcileTo(fork.ID, "ready", 12*time.Minute)
	recordProviderIDs(forkWoken)
	runJob(forkWoken, validateCommand+`
python3 -c "from pathlib import Path;assert Path('/workspace/after-fork.txt').read_bytes()==b'fork roundtrip\\n'"`, 3*time.Minute)
	if desktop {
		if !forkWoken.Desktop {
			t.Fatal("woken fork lost desktop flag")
		}
		runSandboxLiveDesktopScreen(t, ctx, h, forkWoken, artifactDir)
		runJob(forkWoken, `python3 -c "from pathlib import Path;assert Path('/workspace/desktop-proof-result.txt').read_bytes()==b'canter-human-control-ok'"`, time.Minute)
	}

	// ---- 4) Delete fork, GC, provider 404 + DestroyVPS idempotent ----
	if err := h.RequestSandboxDelete(ctx, w.ID, fork.ID, actor); err != nil {
		t.Fatalf("delete fork: %v", err)
	}
	reconcileTo(fork.ID, "deleted", 12*time.Minute)
	for i := 0; i < 10; i++ {
		if err := h.collectSandboxSnapshotGarbageInWorkspace(ctx, w.ID, false); err != nil {
			t.Fatalf("gc: %v", err)
		}
		pending := false
		for _, id := range snapshotIDs {
			var state string
			if err := s.pool.QueryRow(ctx, `SELECT state FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, w.ID, id).Scan(&state); err != nil {
				t.Fatalf("snapshot %s row: %v", id, err)
			}
			if state != "deleted" {
				pending = true
			}
		}
		if !pending {
			break
		}
		time.Sleep(2 * time.Second)
	}
	for _, id := range snapshotIDs {
		var state string
		if err := s.pool.QueryRow(ctx, `SELECT state FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, w.ID, id).Scan(&state); err != nil || state != "deleted" {
			t.Fatalf("snapshot %s ended state=%q err=%v", id, state, err)
		}
	}
	checkProviderGone(forkWoken)
	for id := range allProviderIDs {
		if _, err := provider.Server(ctx, id); !compute.IsNotFound(err) {
			t.Fatalf("provider server %s still present: %v", id, err)
		}
	}
	t.Log("live archive/fork validated and cleaned")
}

// sandboxLiveDesktopArtifactPath resolves an absolute artifact path outside
// the package tree — env override (must be absolute) or t.TempDir().
func sandboxLiveDesktopArtifactPath(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("CANTER_SANDBOX_LIVE_ARTIFACT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("CANTER_SANDBOX_LIVE_ARTIFACT_DIR must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

// runSandboxLiveDesktopScreen captures a screenshot + the first stream JPEG,
// asserting the 1280x800 desktop surface both ways. Writes the PNG artifact.
func runSandboxLiveDesktopScreen(t *testing.T, ctx context.Context, h *HTTPServer, v Sandbox, dir string) {
	t.Helper()
	pngData, width, height, err := h.sandboxDesktopScreenshot(ctx, v)
	if err != nil {
		t.Fatalf("desktop screenshot: %v", err)
	}
	if width != 1280 || height != 800 {
		t.Fatalf("desktop screenshot %dx%d want 1280x800", width, height)
	}
	if err := os.WriteFile(sandboxLiveDesktopArtifactPath(t, fmt.Sprintf("screenshot-%s.png", v.ID)), pngData, 0600); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}
	client, err := h.sandboxDesktopClient(ctx, v)
	if err != nil {
		t.Fatalf("desktop client: %v", err)
	}
	defer client.Close()
	streamCtx, stopStream := context.WithTimeout(ctx, 10*time.Second)
	defer stopStream()
	errDone := errors.New("frame captured")
	var frameW, frameH int
	streamErr := client.ScreenStream(streamCtx, func(frame []byte) error {
		cfg, e := jpeg.DecodeConfig(bytes.NewReader(frame))
		if e != nil {
			return fmt.Errorf("stream frame not JPEG: %w", e)
		}
		frameW, frameH = cfg.Width, cfg.Height
		return errDone
	})
	if !errors.Is(streamErr, errDone) {
		t.Fatalf("desktop stream: %v", streamErr)
	}
	if frameW != 1280 || frameH != 800 {
		t.Fatalf("desktop stream frame %dx%d want 1280x800", frameW, frameH)
	}
}

// runSandboxLiveDesktopProof exercises screenshot/stream, agent input under
// no claim, human take-control exclusivity, real input, and release.
func runSandboxLiveDesktopProof(t *testing.T, ctx context.Context, h *HTTPServer, s *Store, workspace, accountID string, actor sdk.ActorRef, v Sandbox, dir string) {
	t.Helper()
	runSandboxLiveDesktopScreen(t, ctx, h, v, dir)

	runJob := func(sandbox Sandbox, command string, timeout time.Duration) {
		job, err := h.sandboxJobStart(ctx, sandbox, SandboxExecInput{Command: "set -eu\n" + command, TimeoutSeconds: 180}, actor)
		if err != nil {
			t.Fatalf("job start: %v", err)
		}
		client, err := h.sandboxWorkflowClient(ctx, sandbox)
		if err != nil {
			t.Fatalf("job client: %v", err)
		}
		defer client.Close()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			fresh, err := client.Job(ctx, job.ID)
			if err != nil {
				t.Fatalf("job poll: %v", err)
			}
			if fresh.Status == "completed" || fresh.Status == "failed" || fresh.Status == "stopped" {
				s.finishSandboxJob(ctx, sandbox.ID, job.ID)
				if fresh.Status != "completed" || fresh.ExitCode == nil || *fresh.ExitCode != 0 {
					exit := "<nil>"
					if fresh.ExitCode != nil {
						exit = fmt.Sprint(*fresh.ExitCode)
					}
					if logs, logErr := client.JobLogs(ctx, job.ID, "stderr", 0, 4096); logErr == nil {
						t.Logf("desktop proof job stderr: %s", logs.Data)
					}
					t.Fatalf("job %s ended %s exit=%s", job.ID, fresh.Status, exit)
				}
				return
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("job %s did not finish in %v", job.ID, timeout)
	}

	// A loopback-only fixture records the browser's actual click and input.
	// This avoids depending on Chrome's download directory or download prompts.
	runJob(v, `python3 - <<'PY'
from pathlib import Path
Path('/workspace/desktop-proof.html').write_text('''<!doctype html><title>Canter desktop check ready</title><body style="margin:0;background:#163040;color:white;font:24px sans-serif"><h1 style="position:absolute;left:40px;top:0">Canter desktop check</h1><input id="proof" autofocus style="position:absolute;left:40px;top:80px;width:400px;height:40px"><button id="save" style="position:absolute;left:40px;top:150px;width:240px;height:50px">Save proof</button><pre id="events" style="position:absolute;left:40px;top:220px;font-size:14px"></pre><script>
const events = document.getElementById('events');
for (const kind of ['mousemove','mousedown','mouseup','click']) {
  document.addEventListener(kind, e => {
    events.textContent = kind + ' ' + e.clientX + ',' + e.clientY + ' target=' + e.target.id;
  }, true);
}
window.onerror = message => { events.textContent = String(message); };
document.getElementById('save').onclick = async function(event) {
  this.textContent = 'Saving';
  try {
    const response = await fetch('/proof', {method:'POST',body:JSON.stringify({value:document.getElementById('proof').value,x:event.clientX,y:event.clientY,trusted:event.isTrusted})});
    if (!response.ok) throw new Error('Proof write failed: ' + response.status);
    this.textContent = 'Saved';
    document.body.style.background = '#165b30';
    document.title = 'Canter desktop proof saved';
  } catch (error) { events.textContent = String(error); }
};
</script></body>''')
Path('/workspace/desktop-proof-server.py').write_text('''from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = Path('/workspace/desktop-proof.html').read_bytes()
        self.send_response(200)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_POST(self):
        size = int(self.headers.get('Content-Length', '0'))
        if self.path != '/proof' or not 0 < size <= 32000:
            self.send_error(400)
            return
        Path('/workspace/desktop-proof-event.json').write_bytes(self.rfile.read(size))
        self.send_response(204)
        self.end_headers()
HTTPServer(('127.0.0.1', 18765), Handler).serve_forever()
''')
PY
nohup python3 /workspace/desktop-proof-server.py > /workspace/desktop-proof-server.log 2>&1 < /dev/null &
for i in $(seq 1 30); do if curl -fsS http://127.0.0.1:18765/ >/dev/null; then break; fi; sleep 1; done
curl -fsS http://127.0.0.1:18765/ >/dev/null
for i in $(seq 1 60); do if DISPLAY=:99 xdotool search --onlyvisible --class google-chrome >/dev/null; then exit 0; fi; sleep 1; done
exit 1`, 90*time.Second)

	// Agent input (empty claim) — navigate Chrome to the proof page.
	agent := func(input wrencomputer.DesktopInput) {
		if err := h.sandboxDesktopInput(ctx, v, input, sandboxDesktopInputClaim{}); err != nil {
			t.Fatalf("agent input %s: %v", input.Kind, err)
		}
	}
	agent(wrencomputer.DesktopInput{Kind: "key", Key: "ctrl+l"})
	agent(wrencomputer.DesktopInput{Kind: "type", Text: "http://127.0.0.1:18765/"})
	agent(wrencomputer.DesktopInput{Kind: "key", Key: "Return"})
	runJob(v, `for i in $(seq 1 60); do if DISPLAY=:99 xdotool search --name 'Canter desktop check ready' >/dev/null; then exit 0; fi; sleep 1; done
exit 1`, 75*time.Second)
	agent(wrencomputer.DesktopInput{Kind: "key", Key: "F11"})
	time.Sleep(2 * time.Second)

	// Human takes control — agent input + new commands must now conflict.
	state, err := h.SetSandboxDesktopControl(ctx, workspace, v.ID, accountID, actor,
		SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation})
	if err != nil || state.LeaseToken == "" {
		t.Fatalf("take control: %v", err)
	}
	token := state.LeaseToken
	if err := h.sandboxDesktopInput(ctx, v, wrencomputer.DesktopInput{Kind: "key", Key: "a"}, sandboxDesktopInputClaim{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("agent input under human control: %v", err)
	}
	if _, err := h.sandboxJobStart(ctx, v, SandboxExecInput{Command: "true", TimeoutSeconds: 10}, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("job under human control: %v", err)
	}

	// Human claim — real keys/clipboard/mouse on the proof page.
	human := func(input wrencomputer.DesktopInput) {
		if err := h.sandboxDesktopInput(ctx, v, input, sandboxDesktopInputClaim{AccountID: accountID, LeaseToken: token}); err != nil {
			t.Fatalf("human input %s: %v", input.Kind, err)
		}
	}
	human(wrencomputer.DesktopInput{Kind: "click", X: 100, Y: 100})
	human(wrencomputer.DesktopInput{Kind: "key", Key: "ctrl+a"})
	human(wrencomputer.DesktopInput{Kind: "type", Text: "canter-human-control-ok"})
	human(wrencomputer.DesktopInput{Kind: "click", X: 160, Y: 175})
	time.Sleep(2 * time.Second)
	proof, _, _, err := h.sandboxDesktopScreenshot(ctx, v)
	if err != nil {
		t.Fatalf("post-input screenshot: %v", err)
	}
	if err := os.WriteFile(sandboxLiveDesktopArtifactPath(t, fmt.Sprintf("proof-%s.png", v.ID)), proof, 0600); err != nil {
		t.Fatal(err)
	}

	// Release with matching token, then verify the real browser event and save.
	if _, err := h.SetSandboxDesktopControl(ctx, workspace, v.ID, accountID, actor,
		SandboxDesktopControlInput{Mode: "agent", ExpectedGeneration: v.Generation, LeaseToken: token}); err != nil {
		t.Fatalf("release: %v", err)
	}
	runJob(v, `for i in $(seq 1 15); do if [ -f /workspace/desktop-proof-event.json ]; then break; fi; sleep 1; done
DISPLAY=:99 xdotool getmouselocation --shell >&2
python3 - <<'PY'
import json
from pathlib import Path
event = json.loads(Path('/workspace/desktop-proof-event.json').read_text())
assert event['trusted'] is True, event
assert (event['x'], event['y']) == (160, 175), event
b = event['value'].encode()
assert b == b'canter-human-control-ok', b
Path('/workspace/desktop-proof-result.txt').write_bytes(b)
PY`, 30*time.Second)
}
