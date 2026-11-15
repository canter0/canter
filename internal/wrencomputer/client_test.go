package wrencomputer

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func keyPair(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
}

func helperSSH(t *testing.T, sandbox ...bool) (Config, string, *atomic.Int32) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	marker := ""
	if len(sandbox) > 0 && sandbox[0] {
		marker = filepath.Join(root, "sandbox-marker")
		if err := os.WriteFile(marker, []byte("1"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	helper, err := filepath.Abs("guest.py")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := keyPair(t)
	client, key := keyPair(t)
	config := &ssh.ServerConfig{PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if conn.User() != "wren" || string(key.Marshal()) != string(client.PublicKey().Marshal()) {
			return nil, io.EOF
		}
		return &ssh.Permissions{}, nil
	}}
	alternate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	alternateSigner, err := ssh.NewSignerFromKey(alternate)
	if err != nil {
		t.Fatal(err)
	}
	config.AddHostKey(alternateSigner)
	config.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	calls := &atomic.Int32{}
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				connection, channels, requests, err := ssh.NewServerConn(raw, config)
				if err != nil {
					return
				}
				defer connection.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					if channel.ChannelType() != "session" {
						channel.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					session, requests, err := channel.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer session.Close()
						for request := range requests {
							if request.Type != "exec" {
								request.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if err := ssh.Unmarshal(request.Payload, &payload); err != nil || payload.Command != "python3 /opt/canter/computer-agent.py" {
								request.Reply(false, nil)
								continue
							}
							request.Reply(true, nil)
							calls.Add(1)
							command := exec.Command("python3", helper)
							env := append(os.Environ(), "CANTER_COMPUTER_ROOT="+workspace, "CANTER_COMPUTER_STATE="+filepath.Join(root, "state"))
							if marker != "" {
								env = append(env, "CANTER_SANDBOX_MARKER="+marker)
							}
							command.Env = env
							command.Stdin = session
							command.Stdout = session
							command.Stderr = io.Discard
							status := uint32(0)
							if command.Run() != nil {
								status = 1
							}
							session.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	return Config{Address: "127.0.0.1", Port: addr.Port, Username: "wren", PrivateKeyPEM: key, HostKeySHA256: ssh.FingerprintSHA256(host.PublicKey())}, workspace, calls
}
func waitJob(t *testing.T, c *Client, id string) Job {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		job, err := c.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != "starting" && job.Status != "running" {
			return job
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not settle")
	return Job{}
}
func TestComputerPinnedSSHAndDurableJobs(t *testing.T) {
	config, workspace, calls := helperSSH(t)
	ctx := context.Background()
	c, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := c.Probe(ctx)
	if err != nil || !capability.PersistentFiles || !capability.DurableJobs {
		t.Fatalf("probe: %#v %v", capability, err)
	}
	request := JobRequest{ID: "job_once_123", Command: "printf 'one\n' >> counter.txt; printf 'real result'; printf 'saved file' > report.txt", TimeoutSeconds: 5}
	if _, err = c.StartJob(ctx, request); err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, c, request.ID)
	if job.Status != "completed" || job.Output != "real result" || job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("job result: %#v", job)
	}
	// A new SSH connection and client recover the same persisted job.
	reconnected, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reconnected.StartJob(ctx, request); err != nil {
		t.Fatal(err)
	}
	count, err := os.ReadFile(filepath.Join(workspace, "counter.txt"))
	if err != nil || string(count) != "one\n" {
		t.Fatalf("idempotent retry executed twice: %q %v", count, err)
	}
	conflict := request
	conflict.Command = "echo changed"
	if _, err = c.StartJob(ctx, conflict); err == nil {
		t.Fatal("accepted changed command under existing identity")
	}
	var missing *Error
	if _, err := c.ReadFile(ctx, "/workspace/does-not-exist.txt", 0, 10); !errors.As(err, &missing) || missing.Code != "not_found" {
		t.Fatalf("missing-file semantics: %v", err)
	}
	file, err := c.ReadFile(ctx, "/workspace/report.txt", 0, 5)
	if err != nil || string(file.Data) != "saved" || file.EOF || file.NextOffset != 5 {
		t.Fatalf("file chunk: %#v %v", file, err)
	}
	if _, err = c.WriteFile(ctx, "notes/test.txt", []byte("upload")); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(workspace, "notes", "test.txt"))
	if err != nil || string(saved) != "upload" {
		t.Fatalf("file upload: %q %v", saved, err)
	}
	if _, err = c.ReadFile(ctx, "/etc/passwd", 0, 10); err == nil {
		t.Fatal("read outside workspace")
	}
	if _, err = c.WriteFile(ctx, "../escape", []byte("no")); err == nil {
		t.Fatal("write outside workspace")
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = c.WriteFile(ctx, "escape/file", []byte("no")); err == nil {
		t.Fatal("followed symlink out of workspace")
	}
	entries, err := c.ListFiles(ctx, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	foundLink := false
	for _, entry := range entries {
		if entry.Name == "escape" {
			foundLink = entry.Symlink
		}
	}
	if !foundLink {
		t.Fatal("directory listing did not preserve an inaccessible symlink safely")
	}
	before := calls.Load()
	wrong := config
	host, _ := keyPair(t)
	wrong.HostKeySHA256 = ssh.FingerprintSHA256(host.PublicKey())
	bad, err := NewSSH(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bad.Probe(ctx); err == nil {
		t.Fatal("accepted changed computer host key")
	}
	if calls.Load() != before {
		t.Fatal("executed helper before validating host identity")
	}
}
func TestComputerCancellationTimeoutAndBoundedOutput(t *testing.T) {
	config, _, _ := helperSSH(t)
	c, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = c.StartJob(ctx, JobRequest{ID: "job_cancel_123", Command: "sleep 60", TimeoutSeconds: 90}); err != nil {
		t.Fatal(err)
	}
	stopped, err := c.StopJob(ctx, "job_cancel_123")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status == "starting" || stopped.Status == "running" {
		stopped = waitJob(t, c, stopped.ID)
	}
	if stopped.Status != "cancelled" {
		t.Fatalf("cancel result: %#v", stopped)
	}
	if _, err = c.StartJob(ctx, JobRequest{ID: "job_timeout_123", Command: "sleep 30", TimeoutSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, c, "job_timeout_123"); job.Status != "timed_out" {
		t.Fatalf("timeout result: %#v", job)
	}
	if _, err = c.StartJob(ctx, JobRequest{ID: "job_output_123", Command: `python3 -c 'import sys;sys.stdout.write("x"*1100000)'`, TimeoutSeconds: 5}); err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, c, "job_output_123"); job.Status != "completed" || !job.OutputTruncated || len(job.Output) != 1<<20 {
		t.Fatalf("output limit: status=%s bytes=%d truncated=%v", job.Status, len(job.Output), job.OutputTruncated)
	}
}
func TestBootstrapRestrictedGuestAndValidation(t *testing.T) {
	key, _ := keyPair(t)
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key.PublicKey())))
	script, err := Bootstrap(BootstrapConfig{SSHPublicKey: public, SSHSourceCIDR: "203.0.113.9/32", Desktop: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"AllowUsers wren", "PermitRootLogin no", "AllowTcpForwarding no", "ufw allow from 203.0.113.9/32", "User=wren", "169.254.169.254", "python3 -m py_compile", "-E sha256"} {
		if !strings.Contains(script, fragment) {
			t.Errorf("bootstrap lacks %s", fragment)
		}
	}
	// Inspect executable text, not accidental substrings in encoded data.
	// Keep the decoded helper covered by the same restriction.
	inspected := strings.ReplaceAll(script, compressedBase64(guestHelper), string(guestHelper))
	inspected = strings.ReplaceAll(inspected, base64.StdEncoding.EncodeToString(ssh.MarshalAuthorizedKey(key.PublicKey())), "")
	if strings.Contains(inspected, "NOPASSWD") || strings.Contains(inspected, "--no-sandbox") || strings.Contains(inspected, "vnc") {
		t.Fatal("guest bootstrap widens access")
	}
	for _, source := range []string{"0.0.0.0/0", "::/0", "not-cidr"} {
		if _, err = Bootstrap(BootstrapConfig{SSHPublicKey: public, SSHSourceCIDR: source}); err == nil {
			t.Fatalf("accepted source %s", source)
		}
	}
	if strings.Index(script, "install -d -m 755 -o root -g root /run/sshd") < 0 || strings.Index(script, "install -d -m 755 -o root -g root /run/sshd") > strings.Index(script, "sshd -t") {
		t.Fatal("bootstrap validates SSH before creating its required privilege directory")
	}
	if len(script) > 60000 {
		t.Fatalf("bootstrap too large: %s", strconv.Itoa(len(script)))
	}
	if _, err = NewSSH(Config{Address: "127.0.0.1", PrivateKeyPEM: []byte("invalid"), HostKeySHA256: "SHA256:expected"}); err == nil {
		t.Fatal("accepted malformed SSH key")
	}
}

func TestComputerDoubleClickRejectsInvalidCoordinates(t *testing.T) {
	config, _, calls := helperSSH(t)
	c, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Input(context.Background(), DesktopInput{Kind: "double_click", X: -1, Y: 10}); err == nil {
		t.Fatal("negative double-click coordinate accepted")
	}
	if err = c.Input(context.Background(), DesktopInput{Kind: "double_click", X: 10, Y: 4096}); err == nil {
		t.Fatal("off-screen double-click coordinate accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached guest execution")
	}
}

func TestComputerOpenURLAliasReachesActualLegacyGuestBrowserAdapter(t *testing.T) {
	config, workspace, _ := helperSSH(t)
	bin := t.TempDir()
	captured := filepath.Join(workspace, "browser-arguments.txt")
	script := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CANTER_TEST_BROWSER_ARGUMENTS\"\n")
	// Test executable records browser invocation; it never opens a host browser.
	if err := os.WriteFile(filepath.Join(bin, "google-chrome"), script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CANTER_TEST_BROWSER_ARGUMENTS", captured)
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Input(context.Background(), DesktopInput{Kind: "open_url", URL: "https://example.com/verified"}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		got, _ = os.ReadFile(captured)
		if len(got) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(got), "--new-tab\nhttps://example.com/verified") {
		t.Fatalf("browser did not receive normalized URL: %q", got)
	}
	if err = client.Input(context.Background(), DesktopInput{Kind: "open_url", URL: "file:///etc/passwd"}); err == nil {
		t.Fatal("URL alias bypassed guest scheme validation")
	}
}

func TestComputerDoubleClickJobUsesRealDesktopDisplay(t *testing.T) {
	config, workspace, _ := helperSSH(t)
	bin := t.TempDir()
	captured := filepath.Join(workspace, "double-click-arguments.txt")
	script := []byte("#!/bin/sh\n[ \"$DISPLAY\" = ':99' ] || exit 3\nprintf '%s\\n' \"$DISPLAY\" \"$@\" > \"$CANTER_TEST_INPUT_ARGUMENTS\"\n")
	if err := os.WriteFile(filepath.Join(bin, "xdotool"), script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CANTER_TEST_INPUT_ARGUMENTS", captured)
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Input(context.Background(), DesktopInput{Kind: "double_click", X: 200, Y: 150}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(captured)
	if !strings.HasPrefix(string(got), ":99\n") || !strings.Contains(string(got), "--repeat\n2\n--delay\n100") {
		t.Fatalf("desktop double click invocation: %q", got)
	}
}

func TestRepeatedPointerInputAvoidsSyncWaitOnNewAndExistingGuests(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			config, workspace, _ := helperSSH(t)
			bin := t.TempDir()
			captured := filepath.Join(workspace, "pointer.txt")
			// Emulate the guest package's known bad --sync path; same-position clicks
			// must never reach it, for either the new RPC or running-guest fallback.
			script := []byte("#!/bin/sh\n[ \"$DISPLAY\" = ':99' ] || exit 3\nfor arg do [ \"$arg\" != '--sync' ] || exit 88; done\nprintf '%s\\n' \"$*\" >> \"$CANTER_TEST_INPUT_ARGUMENTS\"\n")
			if err := os.WriteFile(filepath.Join(bin, "xdotool"), script, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CANTER_TEST_INPUT_ARGUMENTS", captured)
			client, err := NewSSH(config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if legacy {
				client.pointerProtocol.Store(1)
			}
			for _, input := range []DesktopInput{{Kind: "click", X: 200, Y: 150}, {Kind: "click", X: 200, Y: 150}, {Kind: "right_click", X: 200, Y: 150}, {Kind: "double_click", X: 200, Y: 150}, {Kind: "drag", X: 200, Y: 150, X2: 200, Y2: 150}} {
				if err = client.Input(context.Background(), input); err != nil {
					t.Fatalf("%s failed: %v", input.Kind, err)
				}
			}
			raw, err := os.ReadFile(captured)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{"mousemove 200 150", "click 1", "click 3", "--repeat 2 --delay 100", "mouseup 1"} {
				if !strings.Contains(string(raw), expected) {
					t.Fatalf("missing %q from %q", expected, raw)
				}
			}
		})
	}
}

func TestComputerCancelBeforeStartLeavesDurableTombstone(t *testing.T) {
	config, workspace, _ := helperSSH(t)
	c, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	job, err := c.StopJob(ctx, "command_tombstone")
	if err != nil || job.Status != "cancelled" {
		t.Fatalf("cancel unknown: %+v %v", job, err)
	}
	job, err = c.StartJob(ctx, JobRequest{ID: "command_tombstone", Command: "touch should-not-exist.txt", TimeoutSeconds: 5})
	if err != nil || job.Status != "cancelled" {
		t.Fatalf("late launch: %+v %v", job, err)
	}
	if _, err = os.Stat(filepath.Join(workspace, "should-not-exist.txt")); !os.IsNotExist(err) {
		t.Fatalf("cancelled command ran: %v", err)
	}
}
