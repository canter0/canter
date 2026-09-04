package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/canter0/canter/internal/nodeclient"
	"github.com/canter0/canter/internal/runtime/driver"
	"github.com/canter0/canter/sdk"
)

type process struct {
	cmd     *exec.Cmd
	version string
	port    int
	done    chan struct{}
	mu      sync.Mutex
	exitErr error
}

type nodeControl interface {
	Snapshot(context.Context) (sdk.NodeSnapshot, error)
	Artifact(context.Context, string) ([]byte, error)
	PutObserved(context.Context, sdk.ObservedRelease) error
	AckControl(context.Context, string) error
	PutRuntimeActionResult(context.Context, string, sdk.RuntimeActionResult) error
}

func (p *process) setExit(err error) { p.mu.Lock(); p.exitErr = err; p.mu.Unlock(); close(p.done) }
func (p *process) err() error        { p.mu.Lock(); defer p.mu.Unlock(); return p.exitErr }

type node struct {
	control                 nodeControl
	system                  string
	publicPort              int
	routingMu               sync.Mutex
	inflight                map[string]int
	targets                 []string
	nextTarget              uint64
	active                  []*process
	failedVersion           string
	restarts                int
	lastControl             string
	hostname                string
	lastObservedFingerprint string
	lastObservedAt          time.Time
	drivers                 *driver.Registry
	serviceBindings         map[string]string
	services                []sdk.ObservedService
	runtimePlan             sdk.RuntimePlan
	nextServiceCheck        time.Time
	restartRequested        bool
	startReplica            func(context.Context, sdk.ReleaseManifest, int) (*process, error)
	releaseRoot             string
}

func main() {
	system := flag.String("system", "", "system name")
	gatewayURL := flag.String("gateway", "", "HTTPS node capability gateway URL")
	tokenFile := flag.String("token-file", "/etc/canter/node.token", "node credential file")
	publicPort := flag.Int("public-port", 8080, "public proxy port")
	flag.Parse()
	if *system == "" || *gatewayURL == "" || *tokenFile == "" || *publicPort < 1 || *publicPort > 65535 {
		log.Fatal("system, gateway, token-file, and a valid public-port are required")
	}
	control, err := nodeclient.New(*gatewayURL, *tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	hostname, _ := os.Hostname()
	drivers := driver.NewRegistry()
	drivers.Register("database", "postgres", driver.Postgres{})
	drivers.Register("database", "postgresql", driver.Postgres{})
	n := &node{control: control, system: *system, publicPort: *publicPort, hostname: hostname, drivers: drivers, inflight: make(map[string]int)}
	go func() {
		addr := fmt.Sprintf(":%d", n.publicPort)
		if err := http.ListenAndServe(addr, n); err != nil {
			log.Fatalf("proxy: %v", err)
		}
	}()
	if err := n.run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func (n *node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, release := n.acquireTarget()
	if target == "" {
		http.Error(w, "canter release is not ready", http.StatusServiceUnavailable)
		return
	}
	defer release()
	u, _ := url.Parse(target)
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "canter release unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

func (n *node) run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := n.reconcile(ctx); err != nil {
			log.Printf("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (n *node) reconcile(ctx context.Context) error {
	snapshot, err := n.control.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot.SchemaVersion != "v1" || snapshot.System != n.system {
		return fmt.Errorf("gateway snapshot does not belong to node system")
	}
	if n.reapExited() > 0 {
		n.failedVersion = ""
		_ = n.writeObserved(ctx, sdk.ObservedRelease{Phase: "recovering", Restarts: n.restarts, ReadyReplicas: len(n.active), Message: "one or more application replicas exited"})
	}
	if err := n.applyControl(ctx, snapshot.Control); err != nil {
		return err
	}
	if err := n.reconcileServices(ctx, snapshot.RuntimePlan); err != nil {
		_ = n.writeObserved(ctx, sdk.ObservedRelease{Phase: "provisioning-services", Restarts: n.restarts, Message: err.Error()})
		return err
	}
	if err := n.reconcileRuntimeAction(ctx, snapshot.RuntimeAction); err != nil {
		log.Printf("runtime action: %v", err)
	}
	if snapshot.Desired == nil {
		return n.writeObserved(ctx, sdk.ObservedRelease{Phase: "waiting", Restarts: n.restarts, Message: "no desired release"})
	}
	d := snapshot.Desired
	desired := sdk.ReleaseManifest{SchemaVersion: d.SchemaVersion, System: d.System, Version: d.Version, ArtifactSHA: d.ArtifactSHA, Command: d.Command, Environment: d.Environment, HealthPath: d.HealthPath, PublicPort: d.PublicPort, Replicas: d.Replicas, CapacityLease: d.CapacityLease, RequestedAt: d.RequestedAt}
	if desired.System != n.system || desired.SchemaVersion != "v1" {
		return fmt.Errorf("desired release does not belong to node system")
	}
	if desired.Replicas < 1 {
		desired.Replicas = 1
	}
	if lease := desired.CapacityLease; lease != nil {
		if lease.ExpiresAt.IsZero() || lease.RestoreReplicas < 1 {
			return fmt.Errorf("desired release contains an invalid capacity lease")
		}
		if !time.Now().UTC().Before(lease.ExpiresAt) {
			desired.Replicas = lease.RestoreReplicas
		}
	}
	if n.failedVersion == desired.Version && !n.restartRequested {
		return nil
	}
	if err := n.reconcileReleaseFleet(ctx, desired); err != nil {
		if len(n.active) == 0 {
			n.failedVersion = desired.Version
		}
		return n.writeObserved(ctx, n.observed(desired, "release-failed", len(n.active) > 0, err.Error()))
	}
	return n.writeObserved(ctx, n.observed(desired, "running", len(n.active) == desired.Replicas, ""))
}

func (n *node) reconcileReleaseFleet(ctx context.Context, desired sdk.ReleaseManifest) error {
	versionMatches := len(n.active) > 0
	for _, current := range n.active {
		versionMatches = versionMatches && current.version == desired.Version
	}
	if n.restartRequested || !versionMatches {
		old := append([]*process(nil), n.active...)
		candidates := make([]*process, 0, desired.Replicas)
		for len(candidates) < desired.Replicas {
			candidate, err := n.launchReplica(ctx, desired, n.nextProcessPort(candidates))
			if err != nil {
				for _, started := range candidates {
					n.stop(started)
				}
				n.restartRequested = false
				return err
			}
			candidates = append(candidates, candidate)
		}
		n.active = candidates
		n.setTargets(processTargets(candidates))
		for _, previous := range old {
			n.waitForDrain(ctx, processTarget(previous), 10*time.Second)
			n.stop(previous)
		}
		if n.restartRequested {
			n.restarts++
		}
		n.restartRequested = false
		n.failedVersion = ""
		return nil
	}
	if len(n.active) < desired.Replicas {
		added := make([]*process, 0, desired.Replicas-len(n.active))
		for len(n.active)+len(added) < desired.Replicas {
			candidate, err := n.launchReplica(ctx, desired, n.nextProcessPort(added))
			if err != nil {
				for _, started := range added {
					n.stop(started)
				}
				return err
			}
			added = append(added, candidate)
		}
		n.active = append(n.active, added...)
		n.setTargets(processTargets(n.active))
	}
	if len(n.active) > desired.Replicas {
		removed := append([]*process(nil), n.active[desired.Replicas:]...)
		n.active = append([]*process(nil), n.active[:desired.Replicas]...)
		n.setTargets(processTargets(n.active))
		for _, previous := range removed {
			n.waitForDrain(ctx, processTarget(previous), 10*time.Second)
			n.stop(previous)
		}
	}
	return nil
}

func (n *node) reapExited() int {
	survivors := make([]*process, 0, len(n.active))
	exited := 0
	for _, current := range n.active {
		select {
		case <-current.done:
			log.Printf("release %s replica on port %d exited: %v", current.version, current.port, current.err())
			n.restarts++
			exited++
		default:
			survivors = append(survivors, current)
		}
	}
	if exited > 0 {
		n.active = survivors
		n.setTargets(processTargets(survivors))
	}
	return exited
}

func (n *node) nextProcessPort(extra []*process) int {
	used := make(map[int]struct{}, len(n.active)+len(extra))
	for _, current := range n.active {
		used[current.port] = struct{}{}
	}
	for _, current := range extra {
		used[current.port] = struct{}{}
	}
	for port := 18080; port <= 65535; port++ {
		if _, exists := used[port]; !exists {
			return port
		}
	}
	return 0
}

func (n *node) startRelease(ctx context.Context, desired sdk.ReleaseManifest, port int) (*process, error) {
	dir, err := n.materialize(ctx, desired)
	if err != nil {
		return nil, err
	}
	if port < 1 {
		return nil, fmt.Errorf("no private application port is available")
	}
	command := desired.Command[0]
	if strings.HasPrefix(command, "./") {
		command = filepath.Join(dir, strings.TrimPrefix(command, "./"))
	}
	cmd := exec.Command(command, desired.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = releaseProcessEnvironment(os.Environ(), desired.Environment, n.serviceBindings, port, desired.Version)
	logFile, err := os.OpenFile(filepath.Join(dir, "application.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	p := &process{cmd: cmd, version: desired.Version, port: port, done: make(chan struct{})}
	go func() { err := cmd.Wait(); _ = logFile.Close(); p.setExit(err) }()
	if err := waitHealthy(ctx, port, desired.HealthPath, p); err != nil {
		n.stop(p)
		return nil, err
	}
	return p, nil
}

func (n *node) launchReplica(ctx context.Context, desired sdk.ReleaseManifest, port int) (*process, error) {
	if n.startReplica != nil {
		return n.startReplica(ctx, desired, port)
	}
	return n.startRelease(ctx, desired, port)
}

// releaseProcessEnvironment makes the runtime boundary explicit. Release
// authors may set ordinary application variables, but they cannot redirect the
// candidate away from the node's private port, spoof its observed version, or
// replace a managed-service binding. Producing one value per key also avoids
// depending on platform-specific duplicate-environment behavior.
func releaseProcessEnvironment(base []string, desired, serviceBindings map[string]string, port int, version string) []string {
	values := make(map[string]string, len(base)+len(desired)+len(serviceBindings)+2)
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	for key, value := range desired {
		values[key] = value
	}
	for key, value := range serviceBindings {
		values[key] = value
	}
	values["PORT"] = strconv.Itoa(port)
	values["CANTER_RELEASE_VERSION"] = version
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func waitHealthy(ctx context.Context, port int, path string, process *process) error {
	deadline := time.NewTimer(30 * time.Second)
	tick := time.NewTicker(250 * time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case <-process.done:
			return fmt.Errorf("candidate exited before health: %v", process.err())
		case <-deadline.C:
			return fmt.Errorf("candidate did not become healthy within 30s")
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
			}
		}
	}
}

func (n *node) materialize(ctx context.Context, manifest sdk.ReleaseManifest) (string, error) {
	root := n.releaseRoot
	if root == "" {
		root = "/var/lib/canter-node/releases"
	}
	dir := filepath.Join(root, manifest.Version)
	marker := filepath.Join(dir, ".artifact-sha256")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == manifest.ArtifactSHA {
		return dir, nil
	}
	artifact, err := n.control.Artifact(ctx, manifest.ArtifactSHA)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(artifact)
	if actual := hex.EncodeToString(sum[:]); actual != manifest.ArtifactSHA {
		return "", fmt.Errorf("artifact digest mismatch: got %s", actual)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(root, ".incoming-")
	if err != nil {
		return "", err
	}
	if err := extractTarGz(artifact, temp); err != nil {
		_ = os.RemoveAll(temp)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(temp, ".artifact-sha256"), []byte(manifest.ArtifactSHA+"\n"), 0o640); err != nil {
		_ = os.RemoveAll(temp)
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(temp, dir); err != nil {
		_ = os.RemoveAll(temp)
		return "", err
	}
	return dir, nil
}

func extractTarGz(artifact []byte, destination string) error {
	gz, err := gzip.NewReader(bytes.NewReader(artifact))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe artifact path %q", header.Name)
		}
		path := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode) & 0o750
			f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, tr, header.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported artifact entry %q", header.Name)
		}
	}
}

func (n *node) stop(p *process) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (n *node) applyControl(ctx context.Context, control *sdk.RuntimeControl) error {
	if control == nil || control.ID == "" || control.ID == n.lastControl {
		return nil
	}
	n.lastControl = control.ID
	if control.Action != "restart" {
		return fmt.Errorf("unsupported runtime control %q", control.Action)
	}
	n.restartRequested = true
	return n.control.AckControl(ctx, control.ID)
}

func (n *node) observed(desired sdk.ReleaseManifest, phase string, healthy bool, message string) sdk.ObservedRelease {
	o := sdk.ObservedRelease{Phase: phase, DesiredVersion: desired.Version, DesiredReplicas: desired.Replicas, ReadyReplicas: len(n.active), Restarts: n.restarts, PublicPort: n.publicPort, Healthy: healthy, Message: message, Services: n.services}
	if lease := desired.CapacityLease; lease != nil {
		leasePhase := "active"
		if !time.Now().UTC().Before(lease.ExpiresAt) {
			leasePhase = "expiry-pending-restore"
			if desired.Replicas == lease.RestoreReplicas && len(n.active) == lease.RestoreReplicas {
				leasePhase = "expired-restored"
			}
		}
		o.CapacityLease = &sdk.ObservedCapacityLease{ExpiresAt: lease.ExpiresAt, RestoreReplicas: lease.RestoreReplicas, Phase: leasePhase}
	}
	if len(n.active) > 0 {
		o.RunningVersion = n.active[0].version
		o.PID = n.active[0].cmd.Process.Pid
		o.InternalPort = n.active[0].port
		for _, current := range n.active {
			o.ReplicaPIDs = append(o.ReplicaPIDs, current.cmd.Process.Pid)
			if current.version != o.RunningVersion {
				o.RunningVersion = "mixed"
			}
		}
	}
	return o
}

func (n *node) writeObserved(ctx context.Context, observed sdk.ObservedRelease) error {
	if observed.PublicPort == 0 {
		observed.PublicPort = n.publicPort
	}
	if observed.Services == nil {
		observed.Services = n.services
	}
	fingerprint := fmt.Sprintf("%s|%s|%s|%d|%d|%d|%d|%d|%v|%v|%t|%s|%v", observed.Phase, observed.DesiredVersion, observed.RunningVersion, observed.PID, observed.Restarts, observed.InternalPort, observed.DesiredReplicas, observed.ReadyReplicas, observed.ReplicaPIDs, observed.CapacityLease, observed.Healthy, observed.Message, observed.Services)
	if fingerprint == n.lastObservedFingerprint && time.Since(n.lastObservedAt) < 30*time.Second {
		return nil
	}
	observed.SchemaVersion = "v1"
	observed.System = n.system
	observed.Node = n.hostname
	observed.UpdatedAt = time.Now().UTC()
	if err := n.control.PutObserved(ctx, observed); err != nil {
		return err
	}
	n.lastObservedFingerprint = fingerprint
	n.lastObservedAt = observed.UpdatedAt
	return nil
}

func (n *node) acquireTarget() (string, func()) {
	n.routingMu.Lock()
	if len(n.targets) == 0 {
		n.routingMu.Unlock()
		return "", func() {}
	}
	target := n.targets[n.nextTarget%uint64(len(n.targets))]
	n.nextTarget++
	n.inflight[target]++
	n.routingMu.Unlock()
	return target, func() {
		n.routingMu.Lock()
		n.inflight[target]--
		n.routingMu.Unlock()
	}
}

func (n *node) setTargets(targets []string) {
	n.routingMu.Lock()
	n.targets = append([]string(nil), targets...)
	if len(n.targets) == 0 {
		n.nextTarget = 0
	} else {
		n.nextTarget %= uint64(len(n.targets))
	}
	n.routingMu.Unlock()
}

func processTarget(process *process) string {
	return fmt.Sprintf("http://127.0.0.1:%d", process.port)
}

func processTargets(processes []*process) []string {
	targets := make([]string, 0, len(processes))
	for _, process := range processes {
		targets = append(targets, processTarget(process))
	}
	return targets
}

func (n *node) waitForDrain(ctx context.Context, target string, timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		n.routingMu.Lock()
		active := n.inflight[target]
		n.routingMu.Unlock()
		if active == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func (n *node) reconcileServices(ctx context.Context, plan *sdk.RuntimePlan) error {
	if time.Now().Before(n.nextServiceCheck) {
		return nil
	}
	if plan == nil {
		n.nextServiceCheck = time.Now().Add(10 * time.Second)
		return nil
	}
	if err := plan.Validate(n.system); err != nil {
		return err
	}
	bindings, services, err := n.drivers.Ensure(ctx, *plan)
	n.services = services
	if err != nil {
		n.nextServiceCheck = time.Now().Add(time.Second)
		return err
	}
	n.serviceBindings = bindings
	n.runtimePlan = *plan
	n.nextServiceCheck = time.Now().Add(10 * time.Second)
	return nil
}

func (n *node) reconcileRuntimeAction(ctx context.Context, envelope *sdk.NodeRuntimeAction) error {
	if envelope == nil {
		return nil
	}
	action, lease := envelope.Action, envelope.Lease
	if action.SchemaVersion != "v1" || action.System != n.system || action.ID == "" || action.Service == "" || action.Kind == "" {
		return fmt.Errorf("invalid runtime action")
	}
	if lease.FencingToken != action.FencingToken || !lease.ExpiresAt.After(time.Now().UTC()) || !strings.HasPrefix(action.ID, lease.ChangeID+"-") {
		return nil
	}
	actionCtx, cancel := context.WithDeadline(ctx, lease.ExpiresAt)
	defer cancel()
	result, executeErr := n.drivers.Execute(actionCtx, n.runtimePlan, action)
	if executeErr != nil {
		result = sdk.RuntimeActionResult{SchemaVersion: "v1", ID: action.ID, System: n.system, Service: action.Service, Kind: action.Kind, Phase: "failed", Message: executeErr.Error(), CompletedAt: time.Now().UTC()}
	}
	if err := n.control.PutRuntimeActionResult(ctx, action.ID, result); err != nil {
		return err
	}
	return executeErr
}
