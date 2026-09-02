package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/runtime/driver"
	"github.com/canter0/canter/sdk"
)

type fakeNodeControl struct {
	acked    string
	observed sdk.ObservedRelease
	snapshot sdk.NodeSnapshot
	artifact []byte
}

func (f *fakeNodeControl) Snapshot(context.Context) (sdk.NodeSnapshot, error) {
	return f.snapshot, nil
}
func (f *fakeNodeControl) Artifact(context.Context, string) ([]byte, error) {
	return append([]byte(nil), f.artifact...), nil
}
func (f *fakeNodeControl) PutObserved(_ context.Context, observed sdk.ObservedRelease) error {
	f.observed = observed
	return nil
}
func (f *fakeNodeControl) AckControl(_ context.Context, id string) error { f.acked = id; return nil }
func (f *fakeNodeControl) PutRuntimeActionResult(context.Context, string, sdk.RuntimeActionResult) error {
	return nil
}

func archive(t *testing.T, name string, mode int64, content string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRouteSwitchDrainsPreviousTarget(t *testing.T) {
	n := &node{inflight: make(map[string]int)}
	n.setTargets([]string{"http://old"})
	target, release := n.acquireTarget()
	if target != "http://old" {
		t.Fatalf("target=%q", target)
	}
	n.setTargets([]string{"http://new"})
	var drained atomic.Bool
	go func() {
		n.waitForDrain(t.Context(), "http://old", time.Second)
		drained.Store(true)
	}()
	time.Sleep(30 * time.Millisecond)
	if drained.Load() {
		t.Fatal("old target drained while a request was still active")
	}
	release()
	deadline := time.Now().Add(time.Second)
	for !drained.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !drained.Load() {
		t.Fatal("old target did not drain")
	}
	newTarget, done := n.acquireTarget()
	done()
	if newTarget != "http://new" {
		t.Fatalf("new target=%q", newTarget)
	}
}

func TestRouteSelectionBalancesAcrossReadyReplicas(t *testing.T) {
	n := &node{inflight: make(map[string]int)}
	n.setTargets([]string{"http://one", "http://two", "http://three"})
	want := []string{"http://one", "http://two", "http://three", "http://one"}
	for index, expected := range want {
		target, done := n.acquireTarget()
		done()
		if target != expected {
			t.Fatalf("request %d target=%q want=%q", index, target, expected)
		}
	}
}

func TestObservedReleaseReportsEveryReadyReplica(t *testing.T) {
	n := &node{system: "api", publicPort: 8080, active: []*process{
		{cmd: &exec.Cmd{Process: &os.Process{Pid: 101}}, version: "v1", port: 18080},
		{cmd: &exec.Cmd{Process: &os.Process{Pid: 102}}, version: "v1", port: 18081},
	}}
	observed := n.observed(sdk.ReleaseManifest{Version: "v1", Replicas: 2}, "running", true, "")
	if observed.DesiredReplicas != 2 || observed.ReadyReplicas != 2 || len(observed.ReplicaPIDs) != 2 || !observed.Healthy {
		t.Fatalf("observed=%#v", observed)
	}
}

func TestReconcileReleaseFleetStartsAndRoutesExactReplicaCount(t *testing.T) {
	nextPID := 200
	n := &node{inflight: make(map[string]int)}
	n.startReplica = func(_ context.Context, desired sdk.ReleaseManifest, port int) (*process, error) {
		nextPID++
		return &process{cmd: &exec.Cmd{Process: &os.Process{Pid: nextPID}}, version: desired.Version, port: port, done: make(chan struct{})}, nil
	}
	desired := sdk.ReleaseManifest{Version: "release-two", Replicas: 3}
	if err := n.reconcileReleaseFleet(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	if len(n.active) != 3 || len(n.targets) != 3 {
		t.Fatalf("active=%d targets=%d", len(n.active), len(n.targets))
	}
	seen := map[string]bool{}
	for range 3 {
		target, done := n.acquireTarget()
		done()
		seen[target] = true
	}
	if len(seen) != 3 {
		t.Fatalf("ready replicas were not all placed in rotation: %#v", seen)
	}
}

func TestExpiredCapacityLeaseRestoresReplicasWithoutAgent(t *testing.T) {
	control := &fakeNodeControl{snapshot: sdk.NodeSnapshot{SchemaVersion: "v1", System: "api", Desired: &sdk.NodeDesiredRelease{SchemaVersion: "v1", System: "api", Version: "release-one", Replicas: 3, CapacityLease: &sdk.CapacityLease{ExpiresAt: time.Now().Add(-time.Minute), RestoreReplicas: 1}}}}
	n := &node{control: control, system: "api", publicPort: 8080, hostname: "node-one", inflight: make(map[string]int), drivers: driver.NewRegistry()}
	n.startReplica = func(_ context.Context, desired sdk.ReleaseManifest, port int) (*process, error) {
		return &process{cmd: &exec.Cmd{Process: &os.Process{Pid: 301}}, version: desired.Version, port: port, done: make(chan struct{})}, nil
	}
	if err := n.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(n.active) != 1 || control.observed.DesiredReplicas != 1 || control.observed.ReadyReplicas != 1 || control.observed.CapacityLease == nil || control.observed.CapacityLease.Phase != "expired-restored" {
		t.Fatalf("expired lease was not restored: active=%d observed=%#v", len(n.active), control.observed)
	}
}

func TestRealReplicaFleetServesTrafficAndScalesDown(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 runtime is unavailable")
	}
	application := `#!/usr/bin/python3
import http.server
import os

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/health":
            self.send_response(404)
            self.end_headers()
            return
        body = str(os.getpid()).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, format, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
`
	artifact := archive(t, "app", 0o750, application)
	digest := sha256.Sum256(artifact)
	control := &fakeNodeControl{artifact: artifact}
	n := &node{control: control, system: "real-replicas", publicPort: 8080, inflight: make(map[string]int), releaseRoot: t.TempDir()}
	desired := sdk.ReleaseManifest{SchemaVersion: "v1", System: "real-replicas", Version: "release-real", ArtifactSHA: hex.EncodeToString(digest[:]), Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080, Replicas: 3}
	if err := n.reconcileReleaseFleet(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, replica := range n.active {
			n.stop(replica)
		}
	})
	seen := map[string]bool{}
	for range 12 {
		request := httptest.NewRequest(http.MethodGet, "http://canter.test/health", nil)
		response := httptest.NewRecorder()
		n.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		seen[strings.TrimSpace(response.Body.String())] = true
	}
	if len(seen) != 3 {
		t.Fatalf("traffic did not reach all three real processes: %#v", seen)
	}
	desired.Replicas = 1
	if err := n.reconcileReleaseFleet(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	if len(n.active) != 1 || len(n.targets) != 1 {
		t.Fatalf("scale-down left active=%d targets=%d", len(n.active), len(n.targets))
	}
}

func TestRealReplicaScaleFailureKeepsOriginalServing(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 runtime is unavailable")
	}
	application := `#!/usr/bin/python3
import http.server
import os
import sys

port = int(os.environ["PORT"])
if port == 18082:
    sys.exit(12)

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(str(os.getpid()).encode())
    def log_message(self, format, *args):
        pass

http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
`
	artifact := archive(t, "app", 0o750, application)
	digest := sha256.Sum256(artifact)
	n := &node{control: &fakeNodeControl{artifact: artifact}, system: "failure-replicas", publicPort: 8080, inflight: make(map[string]int), releaseRoot: t.TempDir()}
	desired := sdk.ReleaseManifest{SchemaVersion: "v1", System: "failure-replicas", Version: "release-failure", ArtifactSHA: hex.EncodeToString(digest[:]), Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080, Replicas: 1}
	if err := n.reconcileReleaseFleet(t.Context(), desired); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, replica := range n.active {
			n.stop(replica)
		}
	})
	desired.Replicas = 3
	if err := n.reconcileReleaseFleet(t.Context(), desired); err == nil {
		t.Fatal("unhealthy third replica was accepted")
	}
	if len(n.active) != 1 || len(n.targets) != 1 {
		t.Fatalf("failed scale changed serving fleet: active=%d targets=%d", len(n.active), len(n.targets))
	}
	response := httptest.NewRecorder()
	n.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://canter.test/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("original replica stopped serving after failed scale: %d %s", response.Code, response.Body.String())
	}
}

func TestRealCapacityLeaseExpiresAndRestoresWithoutAgent(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 runtime is unavailable")
	}
	application := `#!/usr/bin/python3
import http.server
import os

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
    def log_message(self, format, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
`
	artifact := archive(t, "app", 0o750, application)
	digest := sha256.Sum256(artifact)
	expires := time.Now().Add(4 * time.Second)
	control := &fakeNodeControl{artifact: artifact, snapshot: sdk.NodeSnapshot{SchemaVersion: "v1", System: "leased-replicas", Desired: &sdk.NodeDesiredRelease{SchemaVersion: "v1", System: "leased-replicas", Version: "release-leased", ArtifactSHA: hex.EncodeToString(digest[:]), Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080, Replicas: 3, CapacityLease: &sdk.CapacityLease{ExpiresAt: expires, RestoreReplicas: 1}}}}
	n := &node{control: control, system: "leased-replicas", publicPort: 8080, hostname: "node-one", inflight: make(map[string]int), drivers: driver.NewRegistry(), releaseRoot: t.TempDir()}
	t.Cleanup(func() {
		for _, replica := range n.active {
			n.stop(replica)
		}
	})
	if err := n.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(n.active) != 3 || control.observed.CapacityLease == nil || control.observed.CapacityLease.Phase != "active" {
		t.Fatalf("lease did not begin at three replicas: active=%d observed=%#v", len(n.active), control.observed)
	}
	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	if err := n.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(n.active) != 1 || control.observed.DesiredReplicas != 1 || control.observed.ReadyReplicas != 1 || control.observed.CapacityLease == nil || control.observed.CapacityLease.Phase != "expired-restored" {
		t.Fatalf("lease expiry did not restore one real replica: active=%d observed=%#v", len(n.active), control.observed)
	}
}

func TestExtractTarGzPreservesExecutable(t *testing.T) {
	dir := t.TempDir()
	if err := extractTarGz(archive(t, "app", 0o750, "binary"), dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	if err := extractTarGz(archive(t, "../escape", 0o600, "bad"), t.TempDir()); err == nil {
		t.Fatal("path traversal archive was accepted")
	}
}

func TestNodeUsesTypedGatewayForControlAndObservedState(t *testing.T) {
	gateway := &fakeNodeControl{}
	n := &node{control: gateway, system: "api", publicPort: 8080, hostname: "node-one"}
	if err := n.applyControl(t.Context(), &sdk.RuntimeControl{ID: "ctl_one", Action: "restart"}); err != nil {
		t.Fatal(err)
	}
	if gateway.acked != "ctl_one" || !n.restartRequested {
		t.Fatalf("ack=%q restart=%v", gateway.acked, n.restartRequested)
	}
	if err := n.writeObserved(t.Context(), sdk.ObservedRelease{Phase: "waiting"}); err != nil {
		t.Fatal(err)
	}
	if gateway.observed.SchemaVersion != "v1" || gateway.observed.System != "api" || gateway.observed.Node != "node-one" {
		t.Fatalf("observed=%#v", gateway.observed)
	}
}

func TestReleaseProcessEnvironmentProtectsRuntimeOwnedValues(t *testing.T) {
	environment := releaseProcessEnvironment(
		[]string{"PATH=/usr/bin", "PORT=old", "DUPLICATE=first", "DUPLICATE=second"},
		map[string]string{
			"PORT":                        "8080",
			"CANTER_RELEASE_VERSION":      "spoofed",
			"CANTER_SERVICE_DATABASE_URL": "postgres://attacker",
			"APPLICATION_MODE":            "production",
		},
		map[string]string{"CANTER_SERVICE_DATABASE_URL": "postgres://managed"},
		18080,
		"release-actual",
	)
	got := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			t.Fatalf("invalid environment entry %q", entry)
		}
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("duplicate environment key %q", key)
		}
		got[key] = value
	}
	if got["PORT"] != "18080" || got["CANTER_RELEASE_VERSION"] != "release-actual" || got["CANTER_SERVICE_DATABASE_URL"] != "postgres://managed" {
		t.Fatalf("runtime-owned values were not protected: %#v", got)
	}
	if got["APPLICATION_MODE"] != "production" || got["PATH"] != "/usr/bin" || got["DUPLICATE"] != "second" {
		t.Fatalf("ordinary environment values were not preserved: %#v", got)
	}
}
