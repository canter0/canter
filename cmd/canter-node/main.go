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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/canter0/canter/internal/provider/m1"
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

func (p *process) setExit(err error) { p.mu.Lock(); p.exitErr = err; p.mu.Unlock(); close(p.done) }
func (p *process) err() error        { p.mu.Lock(); defer p.mu.Unlock(); return p.exitErr }

type node struct {
	store                   *m1.Client
	system                  string
	prefix                  string
	publicPort              int
	target                  atomic.Value
	routingMu               sync.Mutex
	inflight                map[string]int
	active                  *process
	failedVersion           string
	restarts                int
	lastControl             string
	hostname                string
	lastObservedFingerprint string
	lastObservedAt          time.Time
	drivers                 *driver.Registry
	serviceBindings         map[string]string
	services                []sdk.ObservedService
	nextServiceCheck        time.Time
	restartRequested        bool
}

func main() {
	system := flag.String("system", "", "system name")
	prefix := flag.String("prefix", "", "m1 system prefix")
	publicPort := flag.Int("public-port", 8080, "public proxy port")
	flag.Parse()
	if *system == "" || *prefix == "" || *publicPort < 1 || *publicPort > 65535 {
		log.Fatal("system, prefix, and a valid public-port are required")
	}
	store, err := m1.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	hostname, _ := os.Hostname()
	drivers := driver.NewRegistry()
	drivers.Register("database", "postgres", driver.Postgres{})
	drivers.Register("database", "postgresql", driver.Postgres{})
	n := &node{store: store, system: *system, prefix: strings.TrimRight(*prefix, "/"), publicPort: *publicPort, hostname: hostname, drivers: drivers, inflight: make(map[string]int)}
	n.target.Store("")
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
	if n.active != nil {
		select {
		case <-n.active.done:
			err := n.active.err()
			log.Printf("release %s exited: %v", n.active.version, err)
			n.active = nil
			n.setTarget("")
			n.restarts++
			n.failedVersion = ""
			_ = n.writeObserved(ctx, sdk.ObservedRelease{Phase: "recovering", Restarts: n.restarts, Message: "application process exited"})
		default:
		}
	}
	if err := n.applyControl(ctx); err != nil {
		return err
	}
	if err := n.reconcileServices(ctx); err != nil {
		_ = n.writeObserved(ctx, sdk.ObservedRelease{Phase: "provisioning-services", Restarts: n.restarts, Message: err.Error()})
		return err
	}
	var desired sdk.ReleaseManifest
	found, err := n.store.GetOptional(ctx, n.prefix+"/desired.json", &desired)
	if err != nil {
		return err
	}
	if !found {
		return n.writeObserved(ctx, sdk.ObservedRelease{Phase: "waiting", Restarts: n.restarts, Message: "no desired release"})
	}
	if desired.System != n.system || desired.SchemaVersion != "v1" {
		return fmt.Errorf("desired release does not belong to node system")
	}
	if n.active != nil && n.active.version == desired.Version && !n.restartRequested {
		return n.writeObserved(ctx, n.observed(desired, "running", true, ""))
	}
	if n.failedVersion == desired.Version && !n.restartRequested {
		return nil
	}
	old := n.active
	candidate, err := n.startRelease(ctx, desired)
	if err != nil {
		if n.restartRequested && old != nil {
			n.restartRequested = false
			return n.writeObserved(ctx, n.observed(desired, "running", true, "requested replacement failed: "+err.Error()))
		}
		n.failedVersion = desired.Version
		observed := n.observed(desired, "release-failed", old != nil, err.Error())
		return n.writeObserved(ctx, observed)
	}
	n.active = candidate
	newTarget := fmt.Sprintf("http://127.0.0.1:%d", candidate.port)
	n.setTarget(newTarget)
	n.failedVersion = ""
	if old != nil {
		oldTarget := fmt.Sprintf("http://127.0.0.1:%d", old.port)
		n.waitForDrain(ctx, oldTarget, 10*time.Second)
		n.stop(old)
	}
	if n.restartRequested {
		n.restarts++
		n.restartRequested = false
	}
	return n.writeObserved(ctx, n.observed(desired, "running", true, ""))
}

func (n *node) startRelease(ctx context.Context, desired sdk.ReleaseManifest) (*process, error) {
	dir, err := n.materialize(ctx, desired)
	if err != nil {
		return nil, err
	}
	port := 18080
	if n.active != nil && n.active.port == port {
		port = 18081
	}
	command := desired.Command[0]
	if strings.HasPrefix(command, "./") {
		command = filepath.Join(dir, strings.TrimPrefix(command, "./"))
	}
	cmd := exec.Command(command, desired.Command[1:]...)
	cmd.Dir = dir
	env := append(os.Environ(), "PORT="+strconv.Itoa(port), "CANTER_RELEASE_VERSION="+desired.Version)
	for key, value := range desired.Environment {
		env = append(env, key+"="+value)
	}
	for key, value := range n.serviceBindings {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
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
	root := "/var/lib/canter-node/releases"
	dir := filepath.Join(root, manifest.Version)
	marker := filepath.Join(dir, ".artifact-sha256")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == manifest.ArtifactSHA {
		return dir, nil
	}
	artifact, err := n.store.GetBytes(ctx, manifest.ArtifactKey)
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

func (n *node) applyControl(ctx context.Context) error {
	var control sdk.RuntimeControl
	found, err := n.store.GetOptional(ctx, n.prefix+"/control.json", &control)
	if err != nil || !found || control.ID == "" || control.ID == n.lastControl {
		return err
	}
	var ack struct {
		ID string `json:"id"`
	}
	if ackFound, ackErr := n.store.GetOptional(ctx, n.prefix+"/control-ack.json", &ack); ackErr != nil {
		return ackErr
	} else if ackFound && ack.ID == control.ID {
		n.lastControl = control.ID
		return nil
	}
	n.lastControl = control.ID
	if control.Action != "restart" {
		return fmt.Errorf("unsupported runtime control %q", control.Action)
	}
	n.restartRequested = true
	return n.store.PutJSON(ctx, n.prefix+"/control-ack.json", map[string]any{"id": control.ID, "action": control.Action, "completedAt": time.Now().UTC()})
}

func (n *node) observed(desired sdk.ReleaseManifest, phase string, healthy bool, message string) sdk.ObservedRelease {
	o := sdk.ObservedRelease{Phase: phase, DesiredVersion: desired.Version, Restarts: n.restarts, PublicPort: n.publicPort, Healthy: healthy, Message: message, Services: n.services}
	if n.active != nil {
		o.RunningVersion = n.active.version
		o.PID = n.active.cmd.Process.Pid
		o.InternalPort = n.active.port
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
	fingerprint := fmt.Sprintf("%s|%s|%s|%d|%d|%d|%t|%s|%v", observed.Phase, observed.DesiredVersion, observed.RunningVersion, observed.PID, observed.Restarts, observed.InternalPort, observed.Healthy, observed.Message, observed.Services)
	if fingerprint == n.lastObservedFingerprint && time.Since(n.lastObservedAt) < 30*time.Second {
		return nil
	}
	observed.SchemaVersion = "v1"
	observed.System = n.system
	observed.Node = n.hostname
	observed.UpdatedAt = time.Now().UTC()
	if err := n.store.PutJSON(ctx, n.prefix+"/observed.json", observed); err != nil {
		return err
	}
	n.lastObservedFingerprint = fingerprint
	n.lastObservedAt = observed.UpdatedAt
	return nil
}

func (n *node) acquireTarget() (string, func()) {
	n.routingMu.Lock()
	target := n.target.Load().(string)
	if target == "" {
		n.routingMu.Unlock()
		return "", func() {}
	}
	n.inflight[target]++
	n.routingMu.Unlock()
	return target, func() {
		n.routingMu.Lock()
		n.inflight[target]--
		n.routingMu.Unlock()
	}
}

func (n *node) setTarget(target string) {
	n.routingMu.Lock()
	n.target.Store(target)
	n.routingMu.Unlock()
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

func (n *node) reconcileServices(ctx context.Context) error {
	if time.Now().Before(n.nextServiceCheck) {
		return nil
	}
	var plan sdk.RuntimePlan
	found, err := n.store.GetOptional(ctx, n.prefix+"/runtime-plan.json", &plan)
	if err != nil {
		return err
	}
	if !found {
		n.nextServiceCheck = time.Now().Add(10 * time.Second)
		return nil
	}
	if err := plan.Validate(n.system); err != nil {
		return err
	}
	bindings, services, err := n.drivers.Ensure(ctx, plan)
	n.services = services
	if err != nil {
		n.nextServiceCheck = time.Now().Add(time.Second)
		return err
	}
	n.serviceBindings = bindings
	n.nextServiceCheck = time.Now().Add(10 * time.Second)
	return nil
}
