package controlplane

// Opt-in real-VM environment/setup integration test. Ordinary suites skip it;
// it requires BOTH CANTER_SANDBOX_LIVE_AUTHORIZED=true and
// CANTER_SANDBOX_ENV_LIVE_AUTHORIZED=true, a dedicated local
// canter_sandbox_live_environment_* database, and provider credentials from the
// local envfile. It provisions up to three HostVDS VM generations (one success
// sandbox with sleep/wake, one separate setup-failure sandbox), proves the
// durable setup lifecycle end-to-end, and deletes only the resources it created.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/envfile"
	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
	"github.com/canter0/canter/sdk/remote"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

// Pin the public repository revision so setup assertions are reproducible.
const liveEnvRepositoryURL = "https://github.com/octocat/Hello-World.git"
const liveEnvRepositoryRef = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"

// liveEnvSetupCommand runs inside /workspace/project after the pinned
// checkout. It sleeps briefly so the setup-fence window is observable,
// verifies the exact pin + repo contents, requires the environment values
// without printing them, records only a SHA-256 of the token, tracks the run
// count, and prints a fixed marker.
const liveEnvSetupCommand = `sleep 3
set -eu
test "$(git rev-parse HEAD)" = "` + liveEnvRepositoryRef + `"
test -f README
test "$PUBLIC_MODE" = "env-live"
test -n "${CANTER_LIVE_TOKEN:-}"
python3 -c 'import hashlib,os; open("/workspace/setup-token.sha","w").write(hashlib.sha256(os.environ["CANTER_LIVE_TOKEN"].encode()).hexdigest())'
c=0; test -f /workspace/setup-count && c=$(cat /workspace/setup-count); echo $((c+1)) > /workspace/setup-count
echo setup-ok`

func TestSandboxLiveEnvironment(t *testing.T) {
	if os.Getenv("CANTER_SANDBOX_ENV_LIVE_AUTHORIZED") != "true" || os.Getenv("CANTER_SANDBOX_LIVE_AUTHORIZED") != "true" {
		t.Skip("requires CANTER_SANDBOX_ENV_LIVE_AUTHORIZED=true AND CANTER_SANDBOX_LIVE_AUTHORIZED=true (real VMs, dedicated live DB)")
	}
	// Optional private relay: guest SSH is forwarded through a local OpenSSH
	// ControlMaster on the private production peer (native TCP forwarding
	// only). Unset means the exact existing direct path. A bad socket fails
	// here — before the provider preflight and any provisioning.
	var relay *sandboxLiveSSHRelay
	if controlPath := os.Getenv("CANTER_SANDBOX_LIVE_SSH_CONTROL_PATH"); controlPath != "" {
		if !filepath.IsAbs(controlPath) {
			t.Fatal("CANTER_SANDBOX_LIVE_SSH_CONTROL_PATH must be an absolute path")
		}
		info, err := os.Stat(controlPath)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatal("CANTER_SANDBOX_LIVE_SSH_CONTROL_PATH must be an existing unix socket")
		}
		relay = &sandboxLiveSSHRelay{controlPath: controlPath, forwards: map[string]int{}}
	}
	database := os.Getenv("CANTER_TEST_DATABASE_URL")
	parsed, err := url.Parse(database)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatal("CANTER_TEST_DATABASE_URL must be a postgres URL")
	}
	if !strings.HasPrefix(strings.TrimPrefix(parsed.Path, "/"), "canter_sandbox_live_environment_") {
		t.Fatal("CANTER_TEST_DATABASE_URL must name a dedicated canter_sandbox_live_environment_* database")
	}
	if _, err := envfile.Load(); err != nil {
		t.Fatalf("opt-in live test requires provider env: %v", err)
	}
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
	if !strings.HasPrefix(poolConfig.ConnConfig.Database, "canter_sandbox_live_environment_") {
		t.Fatalf("live test database must be canter_sandbox_live_environment_*, got %q", poolConfig.ConnConfig.Database)
	}
	engine, err := sdk.NewFromEnv()
	if err != nil {
		t.Fatalf("sdk engine: %v", err)
	}
	provider, err := compute.NewFromEnv()
	if err != nil {
		t.Fatalf("compute provider: %v", err)
	}
	// Check identity, compute, image and network APIs before provisioning.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 45*time.Second)
	probe := provider.Probe(probeCtx)
	probeCancel()
	if !probe.OK {
		t.Fatal("live provider API preflight failed (no provisioning)")
	}
	cidr := strings.TrimSpace(os.Getenv("CANTER_SANDBOX_LIVE_SSH_CIDR"))
	if cidr == "" {
		t.Fatal("CANTER_SANDBOX_LIVE_SSH_CIDR must be a /32 IPv4 CIDR")
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 {
		t.Fatal("CANTER_SANDBOX_LIVE_SSH_CIDR must be a valid IPv4 /32")
	}
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
	a, w, cookie := workspaceSession(t, s, "sandbox-live-environment@example.com")
	h := NewHTTPServer(&Service{Store: s, Engine: engine}, HTTPConfig{
		PublicURL: "http://canter.test", Secrets: testVault(t),
		Sandboxes: SandboxConfig{Enabled: true, SSHSourceCIDR: cidr, ImageID: image},
	}).(*HTTPServer)
	d := &SandboxDispatcher{Server: h}
	actor := sdk.ActorRef{Kind: "human", ID: a.ID}

	// Bounded probe recorder — populated by the wrapped sandboxDial below.
	// report logs each already-throttled event's safe fields so a provisioning
	// stall is never silent for 12 minutes.
	probes := &sandboxLiveProbeRecorder{now: time.Now, succeeded: map[string]bool{}, diagnose: sandboxLiveSSHDiagnostic}
	probes.report = func(event map[string]any) {
		raw, _ := json.Marshal(event)
		t.Logf("checkpoint: guest probe %s", raw)
	}

	// Evidence manifest: non-secret IDs, statuses and hashes only.
	// Registered BEFORE the cloud cleanup so it runs AFTER it (LIFO): the
	// manifest records the cleanup outcome the cleanup closure actually set.
	manifest := map[string]any{"startedAt": time.Now().UTC().Format(time.RFC3339), "repository": liveEnvRepositoryURL, "ref": liveEnvRepositoryRef}
	writeManifest := func() {
		if probes != nil {
			manifest["probeDiagnostics"] = probes.snapshot()
		}
		data, _ := json.MarshalIndent(manifest, "", "  ")
		if err := os.WriteFile(filepath.Join(artifactDir, "manifest.json"), data, 0600); err != nil {
			t.Logf("manifest write: %v", err)
		}
	}
	t.Cleanup(writeManifest)

	// Owned resources tracked up-front; cleanup runs on ANY exit path with an
	// independent context, destroys only the plans this run owned, garbage
	// collects only this workspace's snapshots, and confirms every tracked
	// provider ID is independently 404.
	var ownedIDs []string
	var ownedPlans []sdk.VPSPlan
	var snapshotIDs []string
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
		cleanupOK := true
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
			v, e := s.Sandbox(cleanCtx, w.ID, id)
			if e != nil || v.Phase != "deleted" {
				t.Errorf("cleanup sandbox %s ended phase=%s err=%v", id, v.Phase, e)
				cleanupOK = false
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
				cleanupOK = false
			}
		}
		// Snapshot GC scoped to this workspace only.
		for i := 0; i < 10 && cleanCtx.Err() == nil; i++ {
			if err := h.collectSandboxSnapshotGarbageInWorkspace(cleanCtx, w.ID, false); err != nil {
				t.Logf("cleanup snapshot gc: %v", err)
			}
			pending := false
			for _, id := range snapshotIDs {
				var state string
				if err := s.pool.QueryRow(cleanCtx, `SELECT state FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, w.ID, id).Scan(&state); err != nil || state != "deleted" {
					pending = true
				}
			}
			if !pending {
				break
			}
			if !pause() {
				break
			}
		}
		for _, id := range snapshotIDs {
			var state string
			if err := s.pool.QueryRow(cleanCtx, `SELECT state FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, w.ID, id).Scan(&state); err != nil || state != "deleted" {
				t.Errorf("cleanup snapshot %s ended state=%q err=%v", id, state, err)
				cleanupOK = false
			}
		}
		for id := range allProviderIDs {
			_, e := provider.Server(cleanCtx, id)
			if !compute.IsNotFound(e) {
				t.Errorf("cleanup provider %s not confirmed absent: %v", id, e)
				cleanupOK = false
			} else {
				t.Logf("cleanup provider %s confirmed absent (404)", id)
			}
		}
		if cleanupOK {
			manifest["cleanup"] = "verified: all owned sandboxes deleted, all owned plans destroyed, workspace snapshots collected, all tracked provider IDs 404"
		} else {
			manifest["cleanup"] = "incomplete: one or more cleanup confirmations failed; see test log"
		}
	})

	// Enable Sandboxes, then build the real remote SDK client over the live
	// router with an installation credential.
	req := requestJSON(t, h, http.MethodPut, fmt.Sprintf("/v1/workspaces/%s/products/sandboxes", w.ID), nil, cookie)
	if req.Code != http.StatusOK && req.Code != http.StatusNoContent {
		t.Fatalf("enable sandboxes: %d %s", req.Code, strings.TrimSpace(req.Body.String()))
	}
	pair := workspaceTestAgent(t, s, a, w.ID, "sandbox-env-live")
	if _, err := s.UpdateAgentAuthority(ctx, a.ID, w.ID, pair.Installation.ID, Authority{Inspect: true, Draft: true, ApplyMode: "automatic"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	client, err := remote.New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := client.Sandboxes(w.ID, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("checkpoint: workspace enabled, installation credential issued")

	// Test-only probe observability: wrap every guest client with a bounded
	// recorder and, on failure, an independent capped SSH diagnostic. The
	// private key bytes are parsed into an ssh.Signer HERE — the caller clears
	// config.PrivateKeyPEM as soon as dialSandbox returns. The diagnostic
	// never observes or stores key material, raw errors, or endpoint values.
	h.sandboxDial = func(config wrencomputer.Config) (sandboxAgentClient, error) {
		var signer ssh.Signer
		if s, parseErr := ssh.ParsePrivateKey(config.PrivateKeyPEM); parseErr == nil {
			signer = s
		}
		if relay != nil {
			localPort, relayErr := relay.forward(ctx, config.Address, config.Port)
			if relayErr != nil {
				return nil, relayErr
			}
			config.Address, config.Port = "127.0.0.1", localPort
		}
		inner, err := wrencomputer.NewSSH(config)
		if err != nil {
			return nil, err
		}
		return &sandboxLiveProbeClient{
			sandboxAgentClient: inner, recorder: probes, testCtx: ctx,
			address: config.Address, port: config.Port,
			fingerprint: config.HostKeySHA256, signer: signer,
		}, nil
	}

	// A stored+approved secret with a run-local dummy value: never prod,
	// never logged, never interpolated into any command.
	dummyBytes := make([]byte, 18)
	if _, err := rand.Read(dummyBytes); err != nil {
		t.Fatal(err)
	}
	dummyValue := "live-env-" + hex.EncodeToString(dummyBytes)
	expectedHash := sha256.Sum256([]byte(dummyValue))
	created := requestJSON(t, h, http.MethodPost, "/v1/workspaces/"+w.ID+"/secrets",
		map[string]any{"name": "CANTER_LIVE_TOKEN", "purpose": "stored", "value": dummyValue, "sandboxAllowed": true}, cookie)
	if created.Code != http.StatusOK && created.Code != http.StatusCreated {
		t.Fatalf("create stored secret: %d %s", created.Code, strings.TrimSpace(created.Body.String()))
	}
	var secretView struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &secretView); err != nil || secretView.ID == "" {
		t.Fatalf("secret view: %v %s", err, strings.TrimSpace(created.Body.String()))
	}
	secretID := secretView.ID
	secretVersion := 1
	// Secrets writes require a human session authenticated within the last
	// five minutes; provisioning can exceed that window, so renew the test
	// session's timestamp before each gated write.
	refreshSession := func() {
		if _, err := s.pool.Exec(ctx, `UPDATE human_sessions SET authenticated_at=now() WHERE account_id=$1 AND revoked_at IS NULL`, a.ID); err != nil {
			t.Fatalf("refresh session auth: %v", err)
		}
	}
	assertNoPlaintext := func(where string, payloads ...string) {
		for _, payload := range payloads {
			if payload != "" && strings.Contains(where, payload) {
				t.Fatal("secret plaintext leaked into an inspected response")
			}
		}
	}

	// Manual dispatcher ticks with a combined phase+setup target; tolerates the
	// transient guest-tools startup error like the existing live harness, and
	// fails fast if setup reaches a terminal state the caller doesn't expect.
	reconcileTo := func(id, wantPhase, wantSetup string, timeout time.Duration) Sandbox {
		deadline := time.Now().Add(timeout)
		lastProgress := time.Now()
		for time.Now().Before(deadline) {
			reconcileErr := d.Reconcile(ctx, w.ID, id)
			v, err := s.Sandbox(ctx, w.ID, id)
			if err != nil {
				t.Fatalf("load %s: %v", id, err)
			}
			waitingForTools := (v.Phase == "provisioning" || v.Phase == "waking") && v.Failure == "The VM is running; waiting for its tools."
			deletionPending := (v.Phase == "sleeping" || v.Phase == "deleting") && reconcileErr != nil && reconcileErr.Error() == "VM deletion is still in progress"
			if reconcileErr != nil && !waitingForTools && !deletionPending && !errors.Is(reconcileErr, ErrConflict) {
				t.Fatalf("reconcile %s -> %s/%s: %v", id, wantPhase, wantSetup, reconcileErr)
			}
			if v.Phase == "failed" || v.Phase == "save_failed" {
				t.Fatalf("%s entered %s: %s", id, v.Phase, v.Failure)
			}
			if wantSetup == "succeeded" && v.Setup.Status == "failed" {
				t.Fatalf("%s setup failed while expecting success: %s", id, v.Setup.Error)
			}
			if v.Phase == wantPhase && (wantSetup == "" || v.Setup.Status == wantSetup) {
				return v
			}
			if ctx.Err() != nil {
				t.Fatalf("context done reconciling %s: %v", id, ctx.Err())
			}
			if time.Since(lastProgress) >= 30*time.Second {
				t.Logf("checkpoint: %s still %s (setup %s)", id, v.Phase, v.Setup.Status)
				lastProgress = time.Now()
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("%s did not reach %s/%s in %v", id, wantPhase, wantSetup, timeout)
		return Sandbox{}
	}
	recordProviderIDs := func(v Sandbox) []string {
		trackPlan(v.Plan)
		resources, err := engine.VPSBillingResources(ctx, v.Plan)
		if err != nil {
			t.Fatalf("provider resources for %s: %v", v.ID, err)
		}
		var ids []string
		for _, r := range resources {
			if strings.HasPrefix(r.ID, "compute/") {
				id := strings.TrimPrefix(r.ID, "compute/")
				ids = append(ids, id)
				allProviderIDs[id] = true
			}
		}
		if len(ids) == 0 {
			t.Fatalf("no compute resource found for %s", v.ID)
		}
		return ids
	}
	waitJob := func(id, jobID string, timeout time.Duration) remote.SandboxJob {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			job, err := sandboxes.Job(ctx, id, jobID)
			if err != nil {
				t.Fatalf("job %s: %v", jobID, err)
			}
			switch job.Status {
			case "completed", "failed", "cancelled", "timed_out", "interrupted":
				return job
			}
			if ctx.Err() != nil {
				t.Fatalf("context done waiting job %s: %v", jobID, ctx.Err())
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("job %s not terminal in %v", jobID, timeout)
		return remote.SandboxJob{}
	}
	mustJob := func(id string, in remote.SandboxExecInput, label string) {
		job, err := sandboxes.Exec(ctx, id, in)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if done := waitJob(id, job.JobID, 90*time.Second); done.ExitCode == nil || *done.ExitCode != 0 {
			t.Fatalf("%s did not exit 0: %+v", label, done)
		}
	}
	download := func(id, path string) []byte {
		var buf bytes.Buffer
		if err := sandboxes.Download(ctx, id, path, &buf); err != nil {
			t.Fatalf("download %s: %v", path, err)
		}
		return buf.Bytes()
	}

	// --- Success sandbox: pinned repo + env + approved secret ---
	createdSandbox, err := sandboxes.Create(ctx, remote.SandboxCreateInput{
		Name: "env-live-success", Size: "free-1x1", IdempotencyKey: "env-live-" + fmt.Sprint(time.Now().UnixNano()),
		Environment: &remote.SandboxEnvironment{
			Repository:          &remote.SandboxRepository{URL: liveEnvRepositoryURL, Ref: liveEnvRepositoryRef},
			SetupCommand:        liveEnvSetupCommand,
			SetupTimeoutSeconds: 120,
			Env:                 map[string]string{"PUBLIC_MODE": "env-live"},
			SecretRefs:          map[string]string{"CANTER_LIVE_TOKEN": secretID},
		},
	})
	if err != nil {
		t.Fatalf("create success sandbox: %v", err)
	}
	track(createdSandbox.ID)
	if tracked, loadErr := s.Sandbox(ctx, w.ID, createdSandbox.ID); loadErr != nil {
		t.Fatalf("reload created sandbox for plan: %v", loadErr)
	} else {
		trackPlan(tracked.Plan)
	}
	if createdSandbox.Setup.Status != "pending" {
		t.Fatalf("setup must start pending: %+v", createdSandbox.Setup)
	}
	if createdSandbox.Environment == nil || createdSandbox.Environment.SecretRefs["CANTER_LIVE_TOKEN"] != secretID {
		t.Fatalf("environment not persisted: %+v", createdSandbox.Environment)
	}
	// Public recipe and setup view carry secret IDs only, never plaintext.
	rawView, _ := json.Marshal(createdSandbox)
	assertNoPlaintext(string(rawView), dummyValue)
	t.Logf("checkpoint: success sandbox %s created, setup pending", createdSandbox.ID)

	// Prove SDK.Wait gates on setup: launch it before setup succeeds.
	waitResult := make(chan struct {
		remote.Sandbox
		error
	}, 1)
	go func() {
		v, err := sandboxes.Wait(ctx, createdSandbox.ID, "ready")
		waitResult <- struct {
			remote.Sandbox
			error
		}{v, err}
	}()

	// Drive reconcile until the machine is ready while setup is pending or
	// running. The fence applies to both, and the pending->running CAS can
	// complete inside one tick, so the externally observable state may be
	// either. An ordinary mutation must conflict and persist no job row.
	var fencedStatus string
	pendingDeadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(pendingDeadline) {
		v, err := s.Sandbox(ctx, w.ID, createdSandbox.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Phase == "ready" && (v.Setup.Status == "pending" || v.Setup.Status == "running") {
			fencedStatus = v.Setup.Status
			rowsBefore := sandboxJobCount(ctx, t, s, v.ID)
			_, jobErr := sandboxes.StartJob(ctx, v.ID, remote.SandboxExecInput{Command: "true"})
			var apiErr *remote.APIError
			if jobErr == nil || !errors.As(jobErr, &apiErr) || apiErr.Status != http.StatusConflict {
				t.Fatalf("ordinary job during %s setup must conflict, got %v", v.Setup.Status, jobErr)
			}
			if after := sandboxJobCount(ctx, t, s, v.ID); after != rowsBefore {
				t.Fatalf("conflicting job must not persist a row: %d -> %d", rowsBefore, after)
			}
			break
		}
		if err := d.Reconcile(ctx, w.ID, createdSandbox.ID); err != nil {
			fresh, loadErr := s.Sandbox(ctx, w.ID, createdSandbox.ID)
			if loadErr != nil {
				t.Fatalf("reload after reconcile: %v", loadErr)
			}
			waitingForTools := (fresh.Phase == "provisioning" || fresh.Phase == "waking") && fresh.Failure == "The VM is running; waiting for its tools."
			if !waitingForTools && !errors.Is(err, ErrConflict) {
				t.Fatalf("reconcile: %v", err)
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("context done waiting for setup window: %v", ctx.Err())
		}
		time.Sleep(2 * time.Second)
	}
	if fencedStatus == "" {
		t.Fatal("never observed the ready+pending/running setup window")
	}
	t.Logf("checkpoint: setup fence probed while status=%s (409, no durable row)", fencedStatus)
	select {
	case <-waitResult:
		t.Fatal("SDK.Wait returned before setup succeeded")
	default:
	}

	// Drive setup to success; record provider IDs for this generation.
	successSandbox := reconcileTo(createdSandbox.ID, "ready", "succeeded", 12*time.Minute)
	firstGenIDs := recordProviderIDs(successSandbox)
	setupJobID := successSandbox.Setup.JobID
	if setupJobID == "" || successSandbox.Setup.ExitCode == nil || *successSandbox.Setup.ExitCode != 0 {
		t.Fatalf("setup must finish succeeded exit 0: %+v", successSandbox.Setup)
	}
	manifest["success"] = map[string]any{"sandboxId": successSandbox.ID, "setupJobId": setupJobID, "providerIds": firstGenIDs}
	t.Logf("checkpoint: setup succeeded (job %s, gen-1 providers %v)", setupJobID, firstGenIDs)

	// Concurrent Wait now resolves.
	select {
	case result := <-waitResult:
		if result.error != nil || result.Sandbox.Setup.Status != "succeeded" {
			t.Fatalf("Wait after success: %+v %v", result.Sandbox.Setup, result.error)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("SDK.Wait did not return after setup succeeded")
	}

	// Setup log: setup-ok marker, terminal completed with explicit exit 0,
	// and no secret material.
	var logText strings.Builder
	var setupStatus string
	var setupExitCode *int
	if err := sandboxes.StreamLogs(ctx, successSandbox.ID, setupJobID, "combined", 0, func(event remote.SandboxLogEvent) error {
		if event.Event == "output" {
			logText.Write(event.Data)
		}
		if event.Event == "status" {
			setupStatus = event.Status
			setupExitCode = event.ExitCode
		}
		return nil
	}); err != nil {
		t.Fatalf("setup log stream: %v", err)
	}
	if setupStatus != "completed" || setupExitCode == nil || *setupExitCode != 0 {
		t.Fatalf("setup stream terminal %q exit=%v", setupStatus, setupExitCode)
	}
	if !strings.Contains(logText.String(), "setup-ok") {
		t.Fatalf("setup log missing marker: %q", logText.String())
	}
	assertNoPlaintext(logText.String(), dummyValue)

	// The setup-time injection wrote the token hash; verify it matches the
	// expected dummy value's hash.
	if got := string(download(successSandbox.ID, "/workspace/setup-token.sha")); got != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("setup token hash %q != expected", got)
	}

	// The same hash on a normal job proves the inherited value.
	mustJob(successSandbox.ID, remote.SandboxExecInput{
		Command: `python3 -c 'import hashlib,os;open("/workspace/job-token.sha","w").write(hashlib.sha256(os.environ["CANTER_LIVE_TOKEN"].encode()).hexdigest())'`,
	}, "inherit job")
	if got := string(download(successSandbox.ID, "/workspace/job-token.sha")); got != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("injected token hash %q != expected", got)
	}
	t.Log("checkpoint: inherited secret hash verified (no plaintext anywhere)")

	// After rotation, the next job sees the new value.
	dummyBytes2 := make([]byte, 18)
	if _, err := rand.Read(dummyBytes2); err != nil {
		t.Fatal(err)
	}
	rotatedValue := "live-env-rot-" + hex.EncodeToString(dummyBytes2)
	rotatedHash := sha256.Sum256([]byte(rotatedValue))
	secretVersion++
	refreshSession()
	rot := requestJSON(t, h, http.MethodPut, "/v1/workspaces/"+w.ID+"/secrets/"+secretID,
		map[string]any{"version": secretVersion - 1, "value": rotatedValue}, cookie)
	if rot.Code != http.StatusOK {
		t.Fatalf("rotate secret: %d", rot.Code)
	}
	mustJob(successSandbox.ID, remote.SandboxExecInput{
		Command: `python3 -c 'import hashlib,os;open("/workspace/job-token.sha","w").write(hashlib.sha256(os.environ["CANTER_LIVE_TOKEN"].encode()).hexdigest())'`,
	}, "post-rotation job")
	if got := string(download(successSandbox.ID, "/workspace/job-token.sha")); got != hex.EncodeToString(rotatedHash[:]) {
		t.Fatal("rotated token hash does not match")
	}
	t.Log("checkpoint: rotated secret hash verified")

	// Disable the grant: omitted refs are forbidden with no durable row;
	// explicit {} runs without the token but keeps PUBLIC_MODE.
	secretVersion++
	refreshSession()
	dis := requestJSON(t, h, http.MethodPatch, "/v1/workspaces/"+w.ID+"/secrets/"+secretID,
		map[string]any{"version": secretVersion - 1, "sandboxAllowed": false}, cookie)
	if dis.Code != http.StatusOK {
		t.Fatalf("disable sandbox access: %d", dis.Code)
	}
	deniedRows := sandboxJobCount(ctx, t, s, successSandbox.ID)
	_, deniedErr := sandboxes.StartJob(ctx, successSandbox.ID, remote.SandboxExecInput{Command: "true"})
	var deniedAPI *remote.APIError
	if deniedErr == nil || !errors.As(deniedErr, &deniedAPI) || deniedAPI.Status != http.StatusForbidden {
		t.Fatalf("omitted refs with disabled grant must be forbidden, got %v", deniedErr)
	}
	if after := sandboxJobCount(ctx, t, s, successSandbox.ID); after != deniedRows {
		t.Fatalf("forbidden job must not persist a row: %d -> %d", deniedRows, after)
	}
	mustJob(successSandbox.ID, remote.SandboxExecInput{
		Command:    `test -z "${CANTER_LIVE_TOKEN:-}" && test "$PUBLIC_MODE" = "env-live"`,
		SecretRefs: map[string]string{},
	}, "{} clear job")
	t.Log("checkpoint: disabled grant -> 403 + no row; {} runs without token")

	// Upload/download a deterministic payload.
	binary := make([]byte, 128*1024)
	for i := range binary {
		binary[i] = byte(i % 251)
	}
	if _, err := sandboxes.Upload(ctx, successSandbox.ID, "/workspace/blob.bin", bytes.NewReader(binary)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := download(successSandbox.ID, "/workspace/blob.bin"); !bytes.Equal(got, binary) {
		t.Fatal("downloaded bytes do not match uploaded payload")
	}
	t.Log("checkpoint: 128KiB upload/download byte-equal")

	// A stoppable async job: sleep 120 with {} refs, StopJob on the exact ID,
	// then poll for a cancelled/interrupted terminal.
	longJob, err := sandboxes.StartJob(ctx, successSandbox.ID, remote.SandboxExecInput{
		Command: "sleep 120", SecretRefs: map[string]string{},
	})
	if err != nil {
		t.Fatalf("start long job: %v", err)
	}
	if _, err := sandboxes.StopJob(ctx, successSandbox.ID, longJob.JobID); err != nil {
		t.Fatalf("stop %s: %v", longJob.JobID, err)
	}
	stopped := waitJob(successSandbox.ID, longJob.JobID, 60*time.Second)
	if stopped.Status != "cancelled" && stopped.Status != "interrupted" {
		t.Fatalf("stopped job must be cancelled/interrupted, got %+v", stopped)
	}
	t.Logf("checkpoint: exact job %s stopped -> %s", longJob.JobID, stopped.Status)

	// Plaintext-leak sweep: recipe/setup JSON, streamed logs so far, and the
	// audit metadata for this workspace. Neither value may appear anywhere.
	rawView, _ = json.Marshal(successSandbox)
	assertNoPlaintext(string(rawView), dummyValue, rotatedValue)
	var auditJSON string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(json_agg(metadata),'[]'::json)::text FROM audit_events WHERE workspace_id=$1`, w.ID).Scan(&auditJSON); err != nil {
		t.Fatalf("audit metadata query: %v", err)
	}
	assertNoPlaintext(auditJSON, dummyValue, rotatedValue)

	// Sleep while the grant is disabled, record the snapshot, wake, and prove
	// the persisted environment survives: same setup (count stays 1), pin
	// intact, files persist, and {} still runs without the token.
	if err := sandboxes.Sleep(ctx, successSandbox.ID); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	asleep := reconcileTo(successSandbox.ID, "asleep", "", 12*time.Minute)
	if asleep.CurrentSnapshotID != "" {
		snapshotIDs = append(snapshotIDs, asleep.CurrentSnapshotID)
	}
	for _, id := range firstGenIDs {
		if _, err := provider.Server(ctx, id); !compute.IsNotFound(err) {
			t.Fatalf("first-generation provider %s must be gone after sleep: %v", id, err)
		}
	}
	t.Logf("checkpoint: asleep, gen-1 providers 404, snapshot %s", asleep.CurrentSnapshotID)
	if err := sandboxes.Wake(ctx, asleep.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	// Track the new-generation plan immediately, before any provisioning.
	if preWake, loadErr := s.Sandbox(ctx, w.ID, asleep.ID); loadErr != nil {
		t.Fatalf("reload post-wake sandbox for plan: %v", loadErr)
	} else {
		trackPlan(preWake.Plan)
	}
	woken := reconcileTo(successSandbox.ID, "ready", "succeeded", 12*time.Minute)
	secondGenIDs := recordProviderIDs(woken)
	if woken.Setup.JobID != setupJobID {
		t.Fatalf("wake must not relaunch setup: %q -> %q", setupJobID, woken.Setup.JobID)
	}
	// The original setup hash and the uploaded binary persist across wake.
	if got := string(download(woken.ID, "/workspace/setup-token.sha")); got != hex.EncodeToString(expectedHash[:]) {
		t.Fatal("post-wake setup token hash changed")
	}
	if got := download(woken.ID, "/workspace/blob.bin"); !bytes.Equal(got, binary) {
		t.Fatal("post-wake blob does not match uploaded bytes")
	}
	mustJob(woken.ID, remote.SandboxExecInput{
		Command:    `cd /workspace/project && test "$(git rev-parse HEAD)" = "` + liveEnvRepositoryRef + `" && test "$(cat /workspace/setup-count)" = "1" && test -z "${CANTER_LIVE_TOKEN:-}"`,
		SecretRefs: map[string]string{},
	}, "post-wake verification job")
	manifest["wake"] = map[string]any{"providerIds": secondGenIDs, "snapshotIds": snapshotIDs, "setupJobRetained": woken.Setup.JobID == setupJobID}
	t.Logf("checkpoint: woken on new generation %v, setup-job retained, pin+files verified", secondGenIDs)

	// Delete the success sandbox; every tracked provider ID must 404.
	if err := sandboxes.Delete(ctx, woken.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reconcileTo(woken.ID, "deleted", "", 12*time.Minute)
	for _, id := range append(firstGenIDs, secondGenIDs...) {
		if _, err := provider.Server(ctx, id); !compute.IsNotFound(err) {
			t.Fatalf("provider %s must be gone after delete: %v", id, err)
		}
	}
	// Workspace-scoped snapshot GC must collect the sleep snapshot.
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
	manifest["snapshotIds"] = snapshotIDs
	t.Log("checkpoint: success sandbox deleted, providers 404, snapshots collected")

	// Skip the separate failure case when using a two-generation budget.
	if os.Getenv("CANTER_SANDBOX_ENV_LIVE_SKIP_FAILURE") == "true" {
		manifest["failure"] = "not_run_within_approved_vm_budget"
		manifest["finishedAt"] = time.Now().UTC().Format(time.RFC3339)
		t.Log("checkpoint: failure/repair VM case not run within the approved VM budget; local tests cover it")
		return
	}

	// --- Failure sandbox: exit-17 setup must fail but keep the VM usable ---
	failure, err := sandboxes.Create(ctx, remote.SandboxCreateInput{
		Name: "env-live-failure", Size: "free-1x1", IdempotencyKey: "env-live-fail-" + fmt.Sprint(time.Now().UnixNano()),
		Environment: &remote.SandboxEnvironment{
			SetupCommand:        "exit 17",
			SetupTimeoutSeconds: 30,
		},
	})
	if err != nil {
		t.Fatalf("create failure sandbox: %v", err)
	}
	track(failure.ID)
	if tracked, loadErr := s.Sandbox(ctx, w.ID, failure.ID); loadErr != nil {
		t.Fatalf("reload failure sandbox for plan: %v", loadErr)
	} else {
		trackPlan(tracked.Plan)
	}
	failed := reconcileTo(failure.ID, "ready", "failed", 12*time.Minute)
	thirdGenIDs := recordProviderIDs(failed)
	if failed.Setup.JobID == "" || failed.Setup.Error == "" {
		t.Fatalf("failed setup must carry a job handle and safe error: %+v", failed.Setup)
	}
	manifest["failure"] = map[string]any{"sandboxId": failed.ID, "setupJobId": failed.Setup.JobID, "providerIds": thirdGenIDs, "error": failed.Setup.Error}
	// Wait ready must surface the failure with the setup job handle.
	if _, err := sandboxes.Wait(ctx, failed.ID, "ready"); err == nil || !strings.Contains(err.Error(), failed.Setup.JobID) {
		t.Fatalf("Wait must error with the setup job ID %q: %v", failed.Setup.JobID, err)
	}
	// The failed VM stays ready and accepts a manual repair job.
	mustJob(failed.ID, remote.SandboxExecInput{Command: "echo repaired"}, "repair job")
	t.Logf("checkpoint: failure sandbox %s kept VM, Wait surfaced %s, repair job ok", failed.ID, failed.Setup.JobID)
	if err := sandboxes.Delete(ctx, failed.ID); err != nil {
		t.Fatalf("delete failure sandbox: %v", err)
	}
	reconcileTo(failed.ID, "deleted", "", 12*time.Minute)
	for _, id := range thirdGenIDs {
		if _, err := provider.Server(ctx, id); !compute.IsNotFound(err) {
			t.Fatalf("failure provider %s must be gone after delete: %v", id, err)
		}
	}
	manifest["finishedAt"] = time.Now().UTC().Format(time.RFC3339)
	t.Log("checkpoint: failure sandbox deleted, all providers 404; live environment flow complete")
}

// sandboxJobCount returns the durable sandbox_jobs row count, used to prove a
// fenced mutation never persists a job.
func sandboxJobCount(ctx context.Context, t *testing.T, s *Store, sandboxID string) int {
	t.Helper()
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_jobs WHERE sandbox_id=$1`, sandboxID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// ---- Test-only bounded probe observability ----

// sandboxLiveProbeClient wraps a real guest client: Probe is observed and the
// exact capabilities/error are returned unchanged; every other method is
// forwarded by the embedded interface.
type sandboxLiveProbeClient struct {
	sandboxAgentClient
	recorder    *sandboxLiveProbeRecorder
	testCtx     context.Context // overall test ctx for the bounded diagnostic
	address     string          // internal map key only — never recorded
	port        int
	fingerprint string // expected host key — compared, never recorded
	signer      ssh.Signer
}

func (c *sandboxLiveProbeClient) Probe(ctx context.Context) (wrencomputer.Capabilities, error) {
	caps, err := c.sandboxAgentClient.Probe(ctx)
	c.recorder.observe(c, ctx, caps, err)
	return caps, err
}

// The optional guest surfaces below forward to the wrapped client so this
// observer does not strip capabilities the sleep/wake and desktop paths
// require; a genuinely missing surface returns a controlled error.
func (c *sandboxLiveProbeClient) ExportSandboxSnapshot(ctx context.Context, opts wrencomputer.SandboxSnapshotOptions, dst io.Writer) (wrencomputer.SandboxSnapshotStats, error) {
	source, ok := c.sandboxAgentClient.(sandboxSnapshotSource)
	if !ok {
		return wrencomputer.SandboxSnapshotStats{}, errors.New("wrapped client cannot export snapshots")
	}
	return source.ExportSandboxSnapshot(ctx, opts, dst)
}

func (c *sandboxLiveProbeClient) RestoreSandboxSnapshot(ctx context.Context, opts wrencomputer.SandboxSnapshotOptions, stats wrencomputer.SandboxSnapshotStats, src io.Reader) error {
	target, ok := c.sandboxAgentClient.(sandboxSnapshotTarget)
	if !ok {
		return errors.New("wrapped client cannot restore snapshots")
	}
	return target.RestoreSandboxSnapshot(ctx, opts, stats, src)
}

func (c *sandboxLiveProbeClient) Screenshot(ctx context.Context) ([]byte, error) {
	agent, ok := c.sandboxAgentClient.(sandboxDesktopAgent)
	if !ok {
		return nil, errors.New("wrapped client has no desktop")
	}
	return agent.Screenshot(ctx)
}

func (c *sandboxLiveProbeClient) ScreenStream(ctx context.Context, emit func([]byte) error) error {
	agent, ok := c.sandboxAgentClient.(sandboxDesktopAgent)
	if !ok {
		return errors.New("wrapped client has no desktop")
	}
	return agent.ScreenStream(ctx, emit)
}

func (c *sandboxLiveProbeClient) Input(ctx context.Context, input wrencomputer.DesktopInput) error {
	agent, ok := c.sandboxAgentClient.(sandboxDesktopAgent)
	if !ok {
		return errors.New("wrapped client has no desktop")
	}
	return agent.Input(ctx, input)
}

var (
	_ sandboxAgentClient    = (*sandboxLiveProbeClient)(nil)
	_ sandboxSnapshotSource = (*sandboxLiveProbeClient)(nil)
	_ sandboxSnapshotTarget = (*sandboxLiveProbeClient)(nil)
	_ sandboxDesktopAgent   = (*sandboxLiveProbeClient)(nil)
	_ sandboxSnapshotSource = (*wrencomputer.Client)(nil)
	_ sandboxSnapshotTarget = (*wrencomputer.Client)(nil)
	_ sandboxDesktopAgent   = (*wrencomputer.Client)(nil)
)

// sandboxLiveProbeRecorder owns a mutex-guarded, 32-entry event buffer.
// Success is recorded once per address; flagged events (failure or missing
// required capabilities) at most once per 30s. Events carry only fixed
// non-secret fields — error TYPE (%T), context flags, capability booleans,
// and the SSH diagnostic's boolean map. The mutex is never held across the
// bounded network diagnostic; report is invoked once per recorded event with
// the same immutable map.
type sandboxLiveProbeRecorder struct {
	mu        sync.Mutex
	events    []map[string]any
	lastFlag  map[string]time.Time
	succeeded map[string]bool
	diagnose  func(ctx context.Context, address string, port int, fingerprint string, signer ssh.Signer) map[string]bool
	now       func() time.Time
	report    func(map[string]any)
}

func (r *sandboxLiveProbeRecorder) observe(c *sandboxLiveProbeClient, probeCtx context.Context, caps wrencomputer.Capabilities, probeErr error) {
	r.mu.Lock()
	if r.now == nil {
		r.now = time.Now
	}
	now := r.now()
	if r.lastFlag == nil {
		r.lastFlag = map[string]time.Time{}
	}
	port := c.port
	if port == 0 {
		port = 22
	}
	// Internal dedup key only — never recorded or reported.
	key := net.JoinHostPort(c.address, strconv.Itoa(port))
	ok := probeErr == nil && caps.PersistentFiles && caps.DurableJobs
	if ok {
		if r.succeeded[key] {
			r.mu.Unlock()
			return
		}
		r.succeeded[key] = true
	} else {
		if now.Sub(r.lastFlag[key]) < 30*time.Second {
			r.mu.Unlock()
			return
		}
		r.lastFlag[key] = now
	}
	event := map[string]any{
		"at":               now.UTC().Format(time.RFC3339),
		"operation":        "probe",
		"ok":               ok,
		"persistentFiles":  caps.PersistentFiles,
		"durableJobs":      caps.DurableJobs,
		"workflowProtocol": caps.WorkflowProtocol,
	}
	if probeErr != nil {
		event["errorType"] = fmt.Sprintf("%T", probeErr)
		event["contextDeadline"] = errors.Is(probeErr, context.DeadlineExceeded)
		event["contextCancelled"] = errors.Is(probeErr, context.Canceled)
		var rpcErr *wrencomputer.Error
		event["rpcError"] = errors.As(probeErr, &rpcErr)
	}
	// Reserve the throttle slot above, then run the bounded diagnostic
	// UNLOCKED so snapshot() is never blocked by network IO.
	runDiag := !ok && c.testCtx != nil && c.testCtx.Err() == nil && r.diagnose != nil
	r.mu.Unlock()
	if runDiag {
		event["sshDiagnostic"] = r.diagnose(c.testCtx, c.address, c.port, c.fingerprint, c.signer)
	}
	r.mu.Lock()
	r.events = append(r.events, event)
	if len(r.events) > 32 {
		r.events = r.events[len(r.events)-32:]
	}
	r.mu.Unlock()
	if r.report != nil {
		r.report(event)
	}
}

func (r *sandboxLiveProbeRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.events))
	copy(out, r.events)
	return out
}

// sandboxLiveSSHRelay is a test-only adapter that maps a guest VM's ip:22 to
// a loopback TCP forward owned by a local OpenSSH ControlMaster over the
// private tailnet. It validates the target is a literal IP on port 22,
// forwards only that port, caches per remote endpoint under its own mutex,
// never falls back to a direct path, and never surfaces remote output — the
// caller sees only a sanitised generic error.
type sandboxLiveSSHRelay struct {
	controlPath string
	mu          sync.Mutex
	forwards    map[string]int // "ip:port" -> loopback port
}

func (r *sandboxLiveSSHRelay) forward(ctx context.Context, address string, port int) (int, error) {
	if net.ParseIP(address) == nil {
		return 0, errors.New("private ssh relay requires a literal IP target")
	}
	if port == 0 {
		port = 22
	}
	if port != 22 {
		return 0, errors.New("private ssh relay only forwards port 22")
	}
	key := net.JoinHostPort(address, strconv.Itoa(port))
	r.mu.Lock()
	defer r.mu.Unlock()
	if local, ok := r.forwards[key]; ok {
		return local, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("private ssh relay listener failed")
	}
	local := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	// Ask the running ControlMaster for a new forward; a stolen port just
	// fails the command — no retry, no host-key downgrade.
	forwardCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(forwardCtx, "ssh",
		"-T", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes",
		"-S", r.controlPath, "-O", "forward",
		"-L", "127.0.0.1:"+strconv.Itoa(local)+":"+key,
		"canter-production")
	if err := cmd.Run(); err != nil {
		return 0, errors.New("private ssh relay forward failed")
	}
	r.forwards[key] = local
	return local, nil
}

// sandboxLiveSSHDiagnostic answers "how far does the same-identity connection
// get?" — TCP, host-key presentation, host-key match, SSH auth — as booleans.
// Hard 6s bound, strict pinned-fingerprint callback, no session, no command,
// every connection closed on all paths, nothing but booleans returned.
func sandboxLiveSSHDiagnostic(ctx context.Context, address string, port int, expectedFingerprint string, signer ssh.Signer) map[string]bool {
	result := map[string]bool{}
	if signer == nil || address == "" {
		return result
	}
	if port == 0 {
		port = 22
	}
	diag, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	deadline := time.Now().Add(6 * time.Second)
	target := net.JoinHostPort(address, strconv.Itoa(port))
	conn, err := (&net.Dialer{}).DialContext(diag, "tcp", target)
	if err != nil {
		var netErr net.Error
		result["timeout"] = errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()
		result["deadline"] = diag.Err() == context.DeadlineExceeded
		result["cancelled"] = diag.Err() == context.Canceled
		return result
	}
	defer conn.Close()
	result["tcpConnected"] = true
	if connDeadline, hasDeadline := diag.Deadline(); hasDeadline {
		deadline = connDeadline
	}
	_ = conn.SetDeadline(deadline)
	// Cancel-safe: closing the conn aborts an in-flight handshake when the
	// parent context ends, and every path closes it.
	stopClose := context.AfterFunc(diag, func() { conn.Close() })
	defer stopClose()
	var hostKeySeen, hostKeyMatched atomic.Bool
	sshConn, _, _, err := ssh.NewClientConn(conn, target, &ssh.ClientConfig{
		User:              "wren",
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			hostKeySeen.Store(true)
			if ssh.FingerprintSHA256(key) == expectedFingerprint {
				hostKeyMatched.Store(true)
				return nil
			}
			return errors.New("host key mismatch")
		},
		Timeout: 6 * time.Second,
	})
	result["hostKeySeen"], result["hostKeyMatched"] = hostKeySeen.Load(), hostKeyMatched.Load()
	if err != nil {
		// Client-side auth rejection surfaces as "unable to authenticate" —
		// classified locally; the raw error is never recorded.
		result["authRejected"] = strings.Contains(err.Error(), "unable to authenticate")
		result["deadline"] = diag.Err() == context.DeadlineExceeded
		result["cancelled"] = diag.Err() == context.Canceled
		return result
	}
	result["sshAuthenticated"] = true
	sshConn.Close()
	return result
}

// ---- Offline helper tests: zero network, zero DB ----

// fakeProbeClient returns fixed caps/error from Probe; other methods forward
// to the embedded fake so embedding/forwarding stays verified.
type fakeProbeClient struct {
	*sandboxFakeClient
	caps wrencomputer.Capabilities
	err  error
}

func (c *fakeProbeClient) Probe(context.Context) (wrencomputer.Capabilities, error) {
	return c.caps, c.err
}

func probeTestClient(recorder *sandboxLiveProbeRecorder, caps wrencomputer.Capabilities, probeErr error) (*sandboxLiveProbeClient, *sandboxFakeClient) {
	fake := &sandboxFakeClient{}
	return &sandboxLiveProbeClient{
		sandboxAgentClient: &fakeProbeClient{sandboxFakeClient: fake, caps: caps, err: probeErr},
		recorder:           recorder, testCtx: context.Background(),
		address: "10.0.0.9", port: 22, fingerprint: "SHA256:test", signer: nil,
	}, fake
}

func TestSandboxLiveProbeWrapsAndRecords(t *testing.T) {
	good := wrencomputer.Capabilities{PersistentFiles: true, DurableJobs: true, WorkflowProtocol: 1}
	r := &sandboxLiveProbeRecorder{now: time.Now, succeeded: map[string]bool{}, diagnose: func(context.Context, string, int, string, ssh.Signer) map[string]bool {
		t.Error("diagnostic must not run for a healthy probe")
		return nil
	}}
	c, _ := probeTestClient(r, good, nil)
	caps, err := c.Probe(context.Background())
	if err != nil || caps != good {
		t.Fatalf("exact caps must pass through: %+v %v", caps, err)
	}
	caps, err = c.Probe(context.Background()) // once per address
	if err != nil || caps != good {
		t.Fatal(err)
	}
	events := r.snapshot()
	if len(events) != 1 || events[0]["ok"] != true {
		t.Fatalf("one success event expected: %v", events)
	}
	// Embedding forwards other methods.
	c, fake := probeTestClient(r, good, nil)
	c.Close()
	if !fake.closed.Load() {
		t.Fatal("embedded Close must reach the inner client")
	}
}

func TestSandboxLiveProbeFailureEventIsSafe(t *testing.T) {
	sentinel := errors.New("sentinel-probe-failure-SECRET-MARKER")
	var diagCalls int
	r := &sandboxLiveProbeRecorder{now: time.Now, succeeded: map[string]bool{}, diagnose: func(context.Context, string, int, string, ssh.Signer) map[string]bool {
		diagCalls++
		return map[string]bool{"tcpConnected": true, "hostKeySeen": true}
	}}
	c, _ := probeTestClient(r, wrencomputer.Capabilities{}, sentinel)
	caps, err := c.Probe(context.Background())
	if err != sentinel || caps.PersistentFiles {
		t.Fatalf("exact error must pass through unchanged: %v", err)
	}
	if diagCalls != 1 {
		t.Fatalf("one diagnostic expected, got %d", diagCalls)
	}
	events := r.snapshot()
	if len(events) != 1 {
		t.Fatalf("one failure event expected: %v", events)
	}
	event := events[0]
	if event["ok"] != false || event["errorType"] != "*errors.errorString" || event["contextDeadline"] != false || event["rpcError"] != false {
		t.Fatalf("event fields: %v", event)
	}
	diag, _ := event["sshDiagnostic"].(map[string]bool)
	if !diag["tcpConnected"] {
		t.Fatalf("diagnostic booleans missing: %v", event)
	}
	raw, _ := json.Marshal(events)
	for _, forbidden := range []string{"SECRET-MARKER", "10.0.0.9", "SHA256:test", "PRIVATE KEY"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("event leaked %q: %s", forbidden, raw)
		}
	}
}

func TestSandboxLiveProbeThrottleAndBounds(t *testing.T) {
	now := time.Unix(1700000000, 0)
	clock := func() time.Time { return now }
	r := &sandboxLiveProbeRecorder{now: clock, succeeded: map[string]bool{}, diagnose: func(context.Context, string, int, string, ssh.Signer) map[string]bool {
		return map[string]bool{}
	}}
	c, _ := probeTestClient(r, wrencomputer.Capabilities{}, errors.New("x"))
	for i := 0; i < 5; i++ {
		_, _ = c.Probe(context.Background()) // all inside 30s → one event
	}
	if got := len(r.snapshot()); got != 1 {
		t.Fatalf("throttled events: %d", got)
	}
	now = now.Add(31 * time.Second)
	_, _ = c.Probe(context.Background())
	if got := len(r.snapshot()); got != 2 {
		t.Fatalf("post-window event expected: %d", got)
	}
	// Buffer is bounded at 32.
	for i := 0; i < 40; i++ {
		now = now.Add(31 * time.Second)
		_, _ = c.Probe(context.Background())
	}
	if got := len(r.snapshot()); got != 32 {
		t.Fatalf("buffer must cap at 32, got %d", got)
	}
}

func TestSandboxLiveProbeMissingCapsTriggersDiagnostic(t *testing.T) {
	var diagCalls int
	r := &sandboxLiveProbeRecorder{now: time.Now, succeeded: map[string]bool{}, diagnose: func(context.Context, string, int, string, ssh.Signer) map[string]bool {
		diagCalls++
		return map[string]bool{"tcpConnected": true}
	}}
	c, _ := probeTestClient(r, wrencomputer.Capabilities{PersistentFiles: true, DurableJobs: false, WorkflowProtocol: 1}, nil)
	caps, err := c.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.PersistentFiles != true {
		t.Fatal("caps must pass through even when incomplete")
	}
	if diagCalls != 1 {
		t.Fatalf("missing durableJobs must trigger one diagnostic, got %d", diagCalls)
	}
	events := r.snapshot()
	if len(events) != 1 || events[0]["ok"] != false {
		t.Fatalf("incomplete caps must record a flagged event: %v", events)
	}
}
