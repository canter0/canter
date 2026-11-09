package wrencomputer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
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

// snapshotTransportFixture controls the fake helper's exec behavior for tests
// that need malformed output or forced exit codes.
type snapshotTransportFixture struct {
	stripReceipt  bool // drop the final receipt frame from export output
	forceExitCode int  // override the exit status (default 0)
	// holdExec ordinal (1-based): the server signals holdReached when that
	// exec request arrives, then holds Reply until holdRelease closes —
	// used to keep one exec "in Start" while a sibling streams.
	holdExec    int
	holdReached chan struct{}
	holdRelease chan struct{}
}

// snapshotTransportHelper is a fake SSH server that models the real forced
// authorized-key command: EVERY exec runs the patched guest.py under
// `python3 -I` regardless of the requested command — just like
// `restrict,command="python3 -I /opt/canter/computer-agent.py"` on a real guest.
// The embedded snapshot module's WORKSPACE_ROOT/HOME_ROOT are patched to temp
// dirs, and guest.py's fixed /opt/canter/sandbox-snapshot.py path is patched to
// that temp module. Returns config, home root, accepted/exec counters.
func snapshotTransportHelper(t *testing.T, fixture ...snapshotTransportFixture) (Config, string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	fx := snapshotTransportFixture{}
	if len(fixture) > 0 {
		fx = fixture[0]
	}
	ctx, cancelCtx := context.WithCancel(context.Background())
	t.Cleanup(cancelCtx)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("snapshot_stream.py")
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.ReplaceAll(string(source), `WORKSPACE_ROOT = "/workspace"`, "WORKSPACE_ROOT = "+strconv.Quote(workspace))
	patched = strings.ReplaceAll(patched, `HOME_ROOT = "/home/wren"`, "HOME_ROOT = "+strconv.Quote(home))
	stream := filepath.Join(root, "snapshot_stream_patched.py")
	if err := os.WriteFile(stream, []byte(patched), 0600); err != nil {
		t.Fatal(err)
	}
	guestSource, err := os.ReadFile("guest.py")
	if err != nil {
		t.Fatal(err)
	}
	patchedGuest := strings.ReplaceAll(string(guestSource), "/opt/canter/sandbox-snapshot.py", stream)
	guestPath := filepath.Join(root, "guest_patched.py")
	if err := os.WriteFile(guestPath, []byte(patchedGuest), 0600); err != nil {
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
	accepted := &atomic.Int32{}
	execs := &atomic.Int32{}
	var connsMu atomic.Pointer[[]net.Conn]
	connsMu.Store(&[]net.Conn{})
	trackConn := func(c net.Conn) {
		cur := connsMu.Load()
		next := append(append([]net.Conn{}, *cur...), c)
		connsMu.Store(&next)
	}
	go func() { // close all accepted connections on test cleanup
		<-ctx.Done()
		for _, c := range *connsMu.Load() {
			c.Close()
		}
	}()
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			trackConn(raw)
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
					session, reqs, err := channel.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer session.Close()
						for request := range reqs {
							if request.Type != "exec" {
								request.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
								request.Reply(false, nil)
								continue
							}
							// Forced command: the requested exec is ignored;
							// every session runs the patched guest.py.
							n := execs.Add(1)
							if fx.holdExec > 0 && int(n) == fx.holdExec {
								close(fx.holdReached)
								select {
								case <-fx.holdRelease:
								case <-ctx.Done():
									return
								}
							}
							request.Reply(true, nil)
							command := exec.CommandContext(ctx, "python3", "-I", guestPath)
							command.Stdin = session
							var stdout bytes.Buffer
							if fx.stripReceipt {
								command.Stdout = &stdout
							} else {
								command.Stdout = session
							}
							command.Stderr = io.Discard
							status := uint32(fx.forceExitCode)
							if err := command.Run(); err != nil && fx.forceExitCode == 0 {
								status = 1
							}
							if fx.stripReceipt {
								// Drop the final receipt frame so valid gzip data
								// arrives without its mandatory receipt.
								out := stdout.Bytes()
								if i := bytes.LastIndex(out, []byte{2}); i > 0 {
									out = out[:i]
								}
								session.Write(out)
							}
							session.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	return Config{
		Address:       "127.0.0.1",
		Port:          listener.Addr().(*net.TCPAddr).Port,
		Username:      "wren",
		PrivateKeyPEM: key,
		HostKeySHA256: ssh.FingerprintSHA256(host.PublicKey()),
	}, home, accepted, execs
}

func snapshotOptions() SandboxSnapshotOptions {
	return SandboxSnapshotOptions{MaxUnpackedBytes: 1 << 30, MaxEntries: 10000}
}

// gateWriter signals its first write then blocks until released or cancelled —
// used to hold an export mid-flight while a sibling also streams.
type gateWriter struct {
	ctx      context.Context
	ready    chan struct{}
	release  chan struct{}
	once     atomic.Bool
	received int64
}

func newGateWriter(ctx context.Context) *gateWriter {
	return &gateWriter{ctx: ctx, ready: make(chan struct{}), release: make(chan struct{})}
}

func (w *gateWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	w.received += int64(len(p))
	if w.once.CompareAndSwap(false, true) {
		close(w.ready)
	}
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	case <-w.release:
		return len(p), nil
	}
}

func (w *gateWriter) unblock() { close(w.release) }

// TestSandboxSnapshotTransportExportRestore round-trips ordinary binary/text,
// a Unicode name, and home content through the real stream source over SSH.
func TestSandboxSnapshotTransportExportRestore(t *testing.T) {
	config, home, _, _ := snapshotTransportHelper(t)
	src := filepath.Join(filepath.Dir(home), "workspace")
	if err := os.WriteFile(filepath.Join(src, "plain.txt"), []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin.dat"), []byte{0, 1, 2, 255, 128}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "héllo-.txt"), []byte("unicode content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"a":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	var archive bytes.Buffer
	stats, err := client.ExportSandboxSnapshot(ctx, snapshotOptions(), &archive)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if stats.Entries == 0 || stats.CompressedBytes != int64(archive.Len()) {
		t.Fatalf("stats wrong: %+v vs %d bytes", stats, archive.Len())
	}
	config2, home2, _, _ := snapshotTransportHelper(t)
	client2, err := NewSSH(config2)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	if err := client2.RestoreSandboxSnapshot(ctx, snapshotOptions(), stats, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}
	src2 := filepath.Join(filepath.Dir(home2), "workspace")
	for name, want := range map[string]string{"plain.txt": "hello world", "héllo-.txt": "unicode content"} {
		got, err := os.ReadFile(filepath.Join(src2, name))
		if err != nil || string(got) != want {
			t.Fatalf("restored %s: %v %q", name, err, got)
		}
	}
	bin, err := os.ReadFile(filepath.Join(src2, "bin.dat"))
	if err != nil || !bytes.Equal(bin, []byte{0, 1, 2, 255, 128}) {
		t.Fatalf("binary restore wrong: %v", err)
	}
	homeJSON, err := os.ReadFile(filepath.Join(home2, "settings.json"))
	if err != nil || string(homeJSON) != `{"a":1}` {
		t.Fatalf("home file not restored: %v %q", err, homeJSON)
	}
}

// TestSandboxSnapshotTransportRejectsPublicClient proves a UserFiles client
// cannot drive the private export — and never opens a TCP connection.
func TestSandboxSnapshotTransportRejectsPublicClient(t *testing.T) {
	config, _, accepted, _ := snapshotTransportHelper(t)
	config.UserFiles = true
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ExportSandboxSnapshot(context.Background(), snapshotOptions(), io.Discard); err == nil {
		t.Fatal("public client exported a snapshot")
	}
	if err := client.RestoreSandboxSnapshot(context.Background(), snapshotOptions(), SandboxSnapshotStats{}, bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("public client restored a snapshot")
	}
	if accepted.Load() != 0 {
		t.Fatalf("public client opened %d connections", accepted.Load())
	}
}

// TestSandboxSnapshotTransportRejectsBadExpected proves invalid expected
// metadata is rejected before any connection opens for restore.
func TestSandboxSnapshotTransportRejectsBadExpected(t *testing.T) {
	config, _, accepted, _ := snapshotTransportHelper(t)
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RestoreSandboxSnapshot(context.Background(), snapshotOptions(), SandboxSnapshotStats{}, bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("invalid expected receipt accepted")
	}
	if accepted.Load() != 0 {
		t.Fatalf("bad expected opened %d connections", accepted.Load())
	}
}

// TestSandboxSnapshotTransportWrongFingerprint rejects a mismatched host key.
func TestSandboxSnapshotTransportWrongFingerprint(t *testing.T) {
	config, _, _, _ := snapshotTransportHelper(t)
	wrong, _ := keyPair(t)
	config.HostKeySHA256 = ssh.FingerprintSHA256(wrong.PublicKey())
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ExportSandboxSnapshot(context.Background(), snapshotOptions(), io.Discard); err == nil {
		t.Fatal("export accepted the wrong host fingerprint")
	}
}

// TestSandboxSnapshotTransportCancelIsolatesSibling runs TWO concurrent
// exports on the SAME client; cancelling A leaves healthy sibling B to finish,
// all over one accepted connection.
func TestSandboxSnapshotTransportCancelIsolatesSibling(t *testing.T) {
	config, home, accepted, _ := snapshotTransportHelper(t)
	workspace := filepath.Join(filepath.Dir(home), "workspace")
	// >=3MiB incompressible input so the producer can't fit in the SSH window.
	random := make([]byte, 3<<20)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "big.bin"), random, 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	sinkA := newGateWriter(ctxA)
	sinkB := newGateWriter(ctxB)
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go func() { _, err := client.ExportSandboxSnapshot(ctxA, snapshotOptions(), sinkA); doneA <- err }()
	go func() { _, err := client.ExportSandboxSnapshot(ctxB, snapshotOptions(), sinkB); doneB <- err }()
	<-sinkA.ready
	<-sinkB.ready // BOTH exports actively streaming on the same client
	cancelA()
	select {
	case err := <-doneA:
		if err == nil {
			t.Fatal("cancelled export A reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled export A did not return promptly")
	}
	sinkB.unblock()
	if err := <-doneB; err != nil {
		t.Fatalf("healthy sibling B died with cancelled A: %v", err)
	}
	if accepted.Load() != 1 {
		t.Fatalf("expected one shared connection, got %d", accepted.Load())
	}
}

// TestSandboxSnapshotTransportDestinationFailure returns promptly when the
// caller's destination fails after first bytes — no producer deadlock.
func TestSandboxSnapshotTransportDestinationFailure(t *testing.T) {
	config, home, _, _ := snapshotTransportHelper(t)
	workspace := filepath.Join(filepath.Dir(home), "workspace")
	random := make([]byte, 3<<20)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "big.bin"), random, 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fail := &failingWriter{limit: 1 << 20}
	done := make(chan error, 1)
	go func() {
		_, err := client.ExportSandboxSnapshot(ctx, snapshotOptions(), fail)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("export succeeded with a failing destination")
		}
	case <-ctx.Done():
		t.Fatal("failing destination deadlocked the export")
	}
}

// failingWriter errors after receiving `limit` bytes — simulates a real
// destination that fails mid-stream rather than at start.
type failingWriter struct {
	limit    int64
	received int64
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.received += int64(len(p))
	if w.received > w.limit {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// TestSandboxSnapshotTransportMissingReceipt proves valid gzip data without a
// receipt frame is not accepted.
func TestSandboxSnapshotTransportMissingReceipt(t *testing.T) {
	config, home, _, _ := snapshotTransportHelper(t, snapshotTransportFixture{stripReceipt: true})
	workspace := filepath.Join(filepath.Dir(home), "workspace")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var archive bytes.Buffer
	if _, err := client.ExportSandboxSnapshot(context.Background(), snapshotOptions(), &archive); err == nil {
		t.Fatal("stream missing its receipt accepted")
	}
	// The gzip body itself is valid — only the receipt frame was stripped.
	if _, err := ValidateSandboxArchive(bytes.NewReader(archive.Bytes()), snapshotOptions()); err != nil {
		t.Fatalf("gzip body was invalid: %v", err)
	}
}

// TestSandboxSnapshotTransportExit1 proves a valid framed stream followed by a
// nonzero remote exit is not accepted.
func TestSandboxSnapshotTransportExit1(t *testing.T) {
	config, home, _, _ := snapshotTransportHelper(t, snapshotTransportFixture{forceExitCode: 1})
	workspace := filepath.Join(filepath.Dir(home), "workspace")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var archive bytes.Buffer
	if _, err := client.ExportSandboxSnapshot(context.Background(), snapshotOptions(), &archive); err == nil {
		t.Fatal("nonzero remote exit accepted")
	}
}

// TestSandboxSnapshotTransportRejectsTruncated sends valid exported stats with
// truncated bytes into a SECOND fresh helper — remote must reject, not crash.
func TestSandboxSnapshotTransportRejectsTruncated(t *testing.T) {
	config, home, _, _ := snapshotTransportHelper(t)
	workspace := filepath.Join(filepath.Dir(home), "workspace")
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var archive bytes.Buffer
	stats, err := client.ExportSandboxSnapshot(context.Background(), snapshotOptions(), &archive)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	config2, _, _, execs := snapshotTransportHelper(t)
	client2, err := NewSSH(config2)
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Close()
	if err := client2.RestoreSandboxSnapshot(context.Background(), snapshotOptions(), stats, bytes.NewReader(archive.Bytes()[:archive.Len()/2])); err == nil {
		t.Fatal("truncated archive accepted")
	}
	if execs.Load() == 0 {
		t.Fatal("restore never reached the remote")
	}
}

// TestSandboxSnapshotTransportCancelDuringStart cancels an operation while its
// Start handshake is still in flight; a sibling streaming on the same shared
// connection must finish undisturbed.
func TestSandboxSnapshotTransportCancelDuringStart(t *testing.T) {
	holdReached := make(chan struct{})
	holdRelease := make(chan struct{})
	defer close(holdRelease)
	config, home, accepted, _ := snapshotTransportHelper(t, snapshotTransportFixture{holdExec: 2, holdReached: holdReached, holdRelease: holdRelease})
	workspace := filepath.Join(filepath.Dir(home), "workspace")
	random := make([]byte, 3<<20)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "big.bin"), random, 0644); err != nil {
		t.Fatal(err)
	}
	client, err := NewSSH(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Sibling A starts first, gates on its destination mid-stream.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	sinkA := newGateWriter(ctxA)
	doneA := make(chan error, 1)
	go func() { _, err := client.ExportSandboxSnapshot(ctxA, snapshotOptions(), sinkA); doneA <- err }()
	<-sinkA.ready
	// Operation B begins: its Start handshake is held by the fixture.
	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan error, 1)
	go func() { _, err := client.ExportSandboxSnapshot(ctxB, snapshotOptions(), io.Discard); doneB <- err }()
	<-holdReached // B's exec request reached the server; Reply still held
	cancelB()
	select {
	case err := <-doneB:
		if err == nil {
			t.Fatal("cancelled-during-Start export reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled-during-Start export did not return promptly")
	}
	sinkA.unblock()
	if err := <-doneA; err != nil {
		t.Fatalf("sibling died when B was cancelled during Start: %v", err)
	}
	if accepted.Load() != 1 {
		t.Fatalf("expected one shared connection, got %d", accepted.Load())
	}
}

// TestSandboxSnapshotForcedCommandIgnoresExec proves the forced authorized-key
// model: an arbitrary exec request is never run — the session always enters the
// agent, which answers the probe op with a JSON error envelope.
func TestSandboxSnapshotForcedCommandIgnoresExec(t *testing.T) {
	config, home, _, _ := snapshotTransportHelper(t)
	root := filepath.Dir(home)
	marker := filepath.Join(root, "exec-probe-marker")
	signer, err := ssh.ParsePrivateKey(config.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", config.Address, config.Port), &ssh.ClientConfig{
		User: config.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = io.Discard
	// Arbitrary exec — the forced command ignores it entirely.
	if err := session.Start("touch " + marker); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(`{"operation":"probe"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	if err := session.Wait(); err != nil {
		t.Fatalf("forced session errored: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("arbitrary exec command was honored — marker exists")
	}
	var envelope struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("probe response not valid JSON: %v", stdout.String())
	}
	if !envelope.OK {
		t.Fatal("probe op failed — the session did not reach the agent")
	}
}

// TestSandboxSnapshotRPCRejectsBadInput proves malformed/oversized/userFiles
// snapshot requests exit nonzero and produce no frames through the real guest
// dispatcher.
func TestSandboxSnapshotRPCRejectsBadInput(t *testing.T) {
	config, _, _, _ := snapshotTransportHelper(t)
	signer, err := ssh.ParsePrivateKey(config.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", config.Address, config.Port), &ssh.ClientConfig{
		User: config.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cases := []struct {
		name    string
		payload string
	}{
		{"non-dict", `{"operation":"sandbox_snapshot","input":"not-a-dict"}` + "\n"},
		{"userFiles", `{"operation":"sandbox_snapshot","input":{"operation":"export","options":{"maxUnpackedBytes":1073741824,"maxEntries":10000,"includeBrowser":false}},"userFiles":true}` + "\n"},
		{"bogus op", `{"operation":"sandbox_snapshot","input":{"operation":"bogus","options":{}}}` + "\n"},
		{"oversize", `{"operation":"sandbox_snapshot","pad":"` + strings.Repeat("x", 16385) + `","input":{}}` + "\n"},
		{"no newline", `{"operation":"sandbox_snapshot","input":{}}`},
	}
	for _, tc := range cases {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := session.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		session.Stdout = &stdout
		session.Stderr = io.Discard
		if err := session.Start(sandboxSnapshotCommand()); err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write([]byte(tc.payload)); err != nil {
			t.Fatal(err)
		}
		stdin.Close()
		waitErr := session.Wait()
		var exitErr *ssh.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitStatus() == 0 {
			t.Fatalf("%s: expected nonzero exit, got %v", tc.name, waitErr)
		}
		if bytes.Contains(stdout.Bytes(), []byte("CNSNAP2")) {
			t.Fatalf("%s: bad input produced frames", tc.name)
		}
		session.Close()
	}
}
