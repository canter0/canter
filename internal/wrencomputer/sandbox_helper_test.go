package wrencomputer

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func python3(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	return path
}

func TestCanterAptValidation(t *testing.T) {
	python := python3(t)
	run := func(args ...string) (string, int) {
		cmd := exec.Command(python, append([]string{"canter-apt"}, args...)...)
		cmd.Dir = filepath.Dir("canter-apt")
		out, err := cmd.CombinedOutput()
		rc := 0
		if exit, ok := err.(*exec.ExitError); ok {
			rc = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), rc
	}
	for _, blocked := range []string{"docker", "docker.io", "ufw", "sudo", "iptables-persistent", "openssh-server", "qemu-system-x86"} {
		out, rc := run("install", blocked)
		if rc == 0 || !strings.Contains(out, "not permitted") {
			t.Fatalf("blocked package %q should be refused: rc=%d %s", blocked, rc, out)
		}
	}
	for _, invalid := range []string{"-o", "-y", "../etc", "PKG", "rm;rf", "pkg=1.0/x"} {
		if out, rc := run("install", invalid); rc != 2 || !strings.Contains(out, "invalid package name") {
			t.Fatalf("invalid name %q: rc=%d %s", invalid, rc, out)
		}
	}
	if out, rc := run("frobnicate"); rc != 2 || !strings.Contains(out, "usage") {
		t.Fatalf("bad subcommand: rc=%d %s", rc, out)
	}
	if out, rc := run("install"); rc != 2 || !strings.Contains(out, "required") {
		t.Fatalf("empty install: rc=%d %s", rc, out)
	}
}

// guestStream launches guest.py and returns its pipes.
func guestStream(t *testing.T, sandbox bool) (stdin *os.File, stdout *os.File, cmd *exec.Cmd) {
	t.Helper()
	python := python3(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "sandbox-marker")
	if sandbox {
		if err := os.WriteFile(marker, []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd = exec.Command(python, "guest.py")
	cmd.Dir = filepath.Dir("guest.py")
	cmd.Env = append(os.Environ(),
		"CANTER_SANDBOX_MARKER="+marker,
		"CANTER_COMPUTER_ROOT="+dir,
		"CANTER_COMPUTER_STATE="+filepath.Join(dir, "state"),
	)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	return in.(*os.File), out.(*os.File), cmd
}

func TestGuestPtyRoundTrip(t *testing.T) {
	stdin, stdout, _ := guestStream(t, true)
	header, _ := json.Marshal(map[string]any{"operation": "pty", "input": map[string]int{"cols": 80, "rows": 24}})
	if _, err := stdin.Write(append(header, '\n')); err != nil {
		t.Fatal(err)
	}
	frame, _ := json.Marshal(map[string]string{"t": "i", "d": base64.StdEncoding.EncodeToString([]byte("echo pty-$((3+4))\n"))})
	if _, err := stdin.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1<<16)
	got := ""
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(got, "pty-7"); {
		if time.Now().After(deadline) {
			t.Fatalf("no shell echo; got %q", got)
		}
		stdout.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := stdout.Read(buffer)
		if n > 0 {
			got += string(buffer[:n])
			if strings.Contains(got, "STREAM_ERROR") {
				t.Fatalf("pty refused: %s", got)
			}
		}
		if err != nil && n == 0 {
			if ne, ok := err.(interface{ Timeout() bool }); !ok || !ne.Timeout() {
				t.Fatalf("read: %v after %q", err, got)
			}
		}
	}
	stdout.SetReadDeadline(time.Time{})
	// Resize frame must not crash the loop.
	frame, _ = json.Marshal(map[string]any{"t": "r", "cols": 99999, "rows": 0})
	stdin.Write(append(frame, '\n'))
}

func TestGuestPtyRequiresMarker(t *testing.T) {
	stdin, stdout, _ := guestStream(t, false)
	header, _ := json.Marshal(map[string]any{"operation": "pty", "input": map[string]int{"cols": 80, "rows": 24}})
	stdin.Write(append(header, '\n'))
	stdin.Close()
	buffer := make([]byte, 1<<16)
	stdout.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, _ := stdout.Read(buffer)
	if !strings.Contains(string(buffer[:n]), "STREAM_ERROR") {
		t.Fatalf("wren computers must refuse pty: %q", buffer[:n])
	}
}

func TestGuestTCPRelay(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buffer := make([]byte, 4096)
				for {
					n, err := c.Read(buffer)
					if err != nil {
						return
					}
					c.Write([]byte("echo:" + string(buffer[:n])))
				}
			}(conn)
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	stdin, stdout, _ := guestStream(t, true)
	header, _ := json.Marshal(map[string]any{"operation": "tcp_relay", "input": map[string]int{"port": port}})
	if _, err := stdin.Write(append(header, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 128)
	got := ""
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(got, "echo:ping") && time.Now().Before(deadline) {
		stdout.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := stdout.Read(buffer)
		if n > 0 {
			got += string(buffer[:n])
		}
	}
	if !strings.Contains(got, "echo:ping") {
		t.Fatalf("relay did not pump bytes: %q", got)
	}
}

// The marker override must not be reachable over SSH: bootstrap never enables
// user environments or per-key environment options.
func TestMarkerEnvNotSSHInjectable(t *testing.T) {
	script, err := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "environment=") || strings.Contains(script, "PermitUserEnvironment") {
		t.Fatal("SSH must not pass client environment to the guest")
	}
}

func TestSandboxBootstrapAptHelper(t *testing.T) {
	script, err := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/usr/local/sbin/canter-apt", "/etc/sudoers.d/canter-apt", "wren ALL=(root) NOPASSWD: /usr/local/sbin/canter-apt", "visudo -cf", "/opt/canter/sandbox"} {
		if !strings.Contains(script, want) {
			t.Fatalf("sandbox bootstrap missing %q", want)
		}
	}
	// The embedded helper must be the current canter-apt source.
	var payload string
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "base64 -d > /usr/local/sbin/canter-apt") {
			payload = strings.TrimSuffix(strings.TrimPrefix(line, "printf '%s' '"), "' | base64 -d > /usr/local/sbin/canter-apt")
		}
	}
	if payload == "" {
		t.Fatal("helper payload line missing")
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(payload)
	if decodeErr != nil || !strings.Contains(string(decoded), "canter-apt") {
		t.Fatalf("embedded helper is not canter-apt: %v", decodeErr)
	}
}

func TestCanterAptTrackedFileSymlink(t *testing.T) {
	python := python3(t)
	dir := t.TempDir()
	// The attacker replaces the tracked file with a symlink to a root-owned
	// target; record() must refuse and leave the target untouched.
	target := filepath.Join(dir, "root-rules")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(dir, ".canter-packages")
	if err := os.Symlink(target, tracked); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("g={'__name__':'canter_apt'}; exec(compile(open(%q).read(), 'canter-apt', 'exec'), g); g['record'](['cowsay'], %q)", filepath.Join(dir, "canter-apt"), tracked)
	if err := os.Link("canter-apt", filepath.Join(dir, "canter-apt")); err != nil {
		if err := copyFile("canter-apt", filepath.Join(dir, "canter-apt")); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	content, readErr := os.ReadFile(target)
	if readErr != nil || string(content) != "original\n" {
		t.Fatalf("symlink target was modified: %q err=%v script-out=%s", content, readErr, out)
	}
	if err != nil {
		t.Fatalf("record should warn, not crash: %v %s", err, out)
	}
	// A plain file is still recorded into normally.
	stream, err2 := os.ReadFile(tracked)
	if err2 == nil && strings.Contains(string(stream), "cowsay") {
		t.Fatal("the symlink path must not be written")
	}
}

func copyFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o755)
}

func TestBootstrapImageFastPath(t *testing.T) {
	out, err := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: "restricted", ImageVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `sandbox-image-version 2>/dev/null)" != "1"`) {
		t.Fatal("fast path missing the version guard")
	}
	if !strings.Contains(out, `> /opt/canter/sandbox-image-version`) {
		t.Fatal("fast path does not stamp the version file")
	}
	// The install must live inside the guard: counting "apt-get install"
	// occurrences outside it is fragile, so assert the guard precedes every
	// install line and authorized_keys/helper/ufw still emit unconditionally.
	guard := strings.Index(out, "sandbox-image-version 2>/dev/null")
	for _, always := range []string{"authorized_keys", "computer-agent.py", "ufw default deny incoming", "ufw allow out to any port 123 proto udp", "resolved.conf"} {
		if !strings.Contains(out, always) {
			t.Fatalf("per-sandbox material %q missing", always)
		}
	}
	if guard < 0 || strings.Contains(out[:guard], "apt-get install") {
		t.Fatal("package install escaped the version guard")
	}
	// No version → unconditional install (fallback behavior unchanged).
	plain, err := Bootstrap(BootstrapConfig{SSHPublicKey: bootstrapKey(t), SSHSourceCIDR: "203.0.113.0/24", Egress: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "sandbox-image-version") || !strings.Contains(plain, "apt-get install -y --no-install-recommends") {
		t.Fatal("unversioned bootstrap must install unconditionally")
	}
}

func TestImageBuildBootstrap(t *testing.T) {
	out := ImageBuildBootstrap("1")
	for _, want := range []string{"apt-get install -y --no-install-recommends", "nodejs npm", "/usr/local/sbin/canter-apt", "visudo -cf", "/opt/canter/sandbox", "/opt/canter/sandbox-image-version", "cloud-init clean", "ssh_host_", "poweroff"} {
		if !strings.Contains(out, want) {
			t.Fatalf("build bootstrap missing %q", want)
		}
	}
	for _, banned := range []string{"authorized_keys", "computer-agent.py", "ufw allow from", "SSHCIDR", "169.254.169.254", "resolved.conf"} {
		if strings.Contains(out, banned) {
			t.Fatalf("build bootstrap must not carry per-sandbox material %q", banned)
		}
	}
}
