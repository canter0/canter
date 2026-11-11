package controlplane

// Loopback tests for sandboxLiveSSHDiagnostic: real TCP listeners and real
// SSH handshakes on 127.0.0.1; no database, provider or internet.
// They verify the diagnostic's own semantics: pinned host-key matching,
// client-side auth-rejection classification, and parent-context cancellation.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"golang.org/x/crypto/ssh"
)

func probeTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// serveSSH accepts one connection and runs a real SSH handshake: the server
// presents hostSigner and accepts only allowKey; it then closes without any
// session or command. Cleanup closes the listener FIRST so a pending Accept
// never blocks the wait for the serve goroutine.
func serveSSH(t *testing.T, hostSigner ssh.Signer, allowKey ssh.PublicKey) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	t.Cleanup(func() {
		listener.Close()
		<-serverDone
	})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		config := &ssh.ServerConfig{
			PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
				if allowKey != nil && string(key.Marshal()) == string(allowKey.Marshal()) {
					return &ssh.Permissions{}, nil
				}
				return nil, ssh.ErrNoAuth
			},
		}
		config.AddHostKey(hostSigner)
		server, _, _, err := ssh.NewServerConn(conn, config)
		if err != nil {
			return
		}
		server.Close()
	}()
	host, rawPort, _ := net.SplitHostPort(listener.Addr().String())
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("port %q: %v", rawPort, err)
	}
	return host, port
}

func TestSandboxLiveProbeSSHSuccess(t *testing.T) {
	hostSigner := probeTestSigner(t)
	clientSigner := probeTestSigner(t)
	host, port := serveSSH(t, hostSigner, clientSigner.PublicKey())
	result := sandboxLiveSSHDiagnostic(context.Background(), host, port,
		ssh.FingerprintSHA256(hostSigner.PublicKey()), clientSigner)
	for _, key := range []string{"tcpConnected", "hostKeySeen", "hostKeyMatched", "sshAuthenticated"} {
		if !result[key] {
			t.Fatalf("expected %s in %+v", key, result)
		}
	}
}

func TestSandboxLiveProbeSSHWrongHostKey(t *testing.T) {
	hostSigner := probeTestSigner(t)
	clientSigner := probeTestSigner(t)
	other := probeTestSigner(t)
	host, port := serveSSH(t, hostSigner, clientSigner.PublicKey())
	result := sandboxLiveSSHDiagnostic(context.Background(), host, port,
		ssh.FingerprintSHA256(other.PublicKey()), clientSigner)
	if !result["tcpConnected"] || !result["hostKeySeen"] {
		t.Fatalf("connection and banner expected: %+v", result)
	}
	if result["hostKeyMatched"] || result["sshAuthenticated"] {
		t.Fatalf("wrong pin must not match or authenticate: %+v", result)
	}
}

func TestSandboxLiveProbeSSHAuthRejected(t *testing.T) {
	hostSigner := probeTestSigner(t)
	clientSigner := probeTestSigner(t)
	other := probeTestSigner(t)
	// Correct pin, but the server accepts only `other`'s key.
	host, port := serveSSH(t, hostSigner, other.PublicKey())
	result := sandboxLiveSSHDiagnostic(context.Background(), host, port,
		ssh.FingerprintSHA256(hostSigner.PublicKey()), clientSigner)
	if !result["hostKeyMatched"] {
		t.Fatalf("host pin must match: %+v", result)
	}
	if !result["authRejected"] || result["sshAuthenticated"] {
		t.Fatalf("client-side rejection must classify as authRejected: %+v", result)
	}
}

func TestSandboxLiveProbeSSHParentCancel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientHello := make(chan struct{})
	release := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "SSH-") {
			return
		}
		close(clientHello)
		<-release // hold the connection open until the test finishes
	}()
	defer func() {
		listener.Close()
		close(release)
		<-serverDone
	}()
	host, rawPort, _ := net.SplitHostPort(listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-clientHello: // client finished Dial and sent its SSH banner
			cancel()
		case <-serverDone: // accept/read failed — still cancel, no leak
			cancel()
		}
	}()
	start := time.Now()
	port, _ := strconv.Atoi(rawPort)
	result := sandboxLiveSSHDiagnostic(ctx, host, port,
		"SHA256:irrelevant", probeTestSigner(t))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("parent cancel must abort the handshake promptly, took %v", elapsed)
	}
	if !result["tcpConnected"] {
		t.Fatalf("tcp must connect before cancel: %+v", result)
	}
	if !result["cancelled"] {
		t.Fatalf("cancelled must be recorded: %+v", result)
	}
}

func TestSandboxLiveProbeSSHAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := sandboxLiveSSHDiagnostic(ctx, "127.0.0.1", 22,
		"SHA256:irrelevant", probeTestSigner(t))
	if result["tcpConnected"] {
		t.Fatalf("pre-cancelled ctx must not connect: %+v", result)
	}
	if !result["cancelled"] {
		t.Fatalf("cancelled must be recorded on the dial branch: %+v", result)
	}
}

// ---- Optional-interface contract tests: the probe wrapper must forward
// every optional guest surface, not hide it behind the base embedding. ----

func liveProbeWrapper(inner sandboxAgentClient) *sandboxLiveProbeClient {
	return &sandboxLiveProbeClient{
		sandboxAgentClient: inner,
		recorder:           &sandboxLiveProbeRecorder{now: time.Now, succeeded: map[string]bool{}},
		testCtx:            context.Background(),
	}
}

func liveSnapshotOptions() wrencomputer.SandboxSnapshotOptions {
	return wrencomputer.SandboxSnapshotOptions{MaxUnpackedBytes: 1 << 30, MaxEntries: wrencomputer.SandboxSnapshotMaxEntries}
}

func TestSandboxLiveProbeSnapshotRoundTrip(t *testing.T) {
	archive := snapshotStoreArchive(t, 1024)
	fake := &sandboxFakeClient{archive: archive}
	wrapped := liveProbeWrapper(fake)
	options := liveSnapshotOptions()
	source, ok := interface{}(wrapped).(sandboxSnapshotSource)
	if !ok {
		t.Fatal("wrapper hides sandboxSnapshotSource")
	}
	var buf bytes.Buffer
	stats, err := source.ExportSandboxSnapshot(context.Background(), options, &buf)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), archive) {
		t.Fatal("export bytes differ from fixture archive")
	}
	if stats.RawBytes <= 0 || stats.Entries <= 0 {
		t.Fatalf("invalid export stats %+v", stats)
	}
	target, ok := interface{}(wrapped).(sandboxSnapshotTarget)
	if !ok {
		t.Fatal("wrapper hides sandboxSnapshotTarget")
	}
	if err := target.RestoreSandboxSnapshot(context.Background(), options, stats, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(fake.restored) != 1 || !bytes.Equal(fake.restored[0], archive) {
		t.Fatalf("inner restore saw %d payloads, not the exact archive", len(fake.restored))
	}
}

type desktopRecordingFake struct {
	*fakeProbeClient
	screenshot []byte
	frames     [][]byte
	inputs     []wrencomputer.DesktopInput
}

func (f *desktopRecordingFake) Screenshot(context.Context) ([]byte, error) {
	return f.screenshot, nil
}

func (f *desktopRecordingFake) ScreenStream(_ context.Context, emit func([]byte) error) error {
	for _, frame := range f.frames {
		if err := emit(frame); err != nil {
			return err
		}
	}
	return nil
}

func (f *desktopRecordingFake) Input(_ context.Context, input wrencomputer.DesktopInput) error {
	f.inputs = append(f.inputs, input)
	return nil
}

// agentOnlyFake hides the optional interfaces behind the base embedding so
// the wrapper must surface a controlled error, never a panic.
type agentOnlyFake struct {
	sandboxAgentClient
}

func TestSandboxLiveProbeDesktopForwarding(t *testing.T) {
	fake := &sandboxFakeClient{}
	inner := &desktopRecordingFake{
		fakeProbeClient: &fakeProbeClient{sandboxFakeClient: fake},
		screenshot:      []byte{0x89, 0x50, 0x4e, 0x47},
		frames:          [][]byte{{1, 2, 3}, {4, 5, 6}},
	}
	wrapped := liveProbeWrapper(inner)
	agent, ok := interface{}(wrapped).(sandboxDesktopAgent)
	if !ok {
		t.Fatal("wrapper hides sandboxDesktopAgent")
	}
	shot, err := agent.Screenshot(context.Background())
	if err != nil || !bytes.Equal(shot, inner.screenshot) {
		t.Fatalf("screenshot %x err=%v", shot, err)
	}
	sentinel := errors.New("stream callback sentinel")
	var got [][]byte
	if err := agent.ScreenStream(context.Background(), func(frame []byte) error {
		got = append(got, frame)
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("callback sentinel must pass through unchanged, got %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0], inner.frames[0]) {
		t.Fatalf("stream frames %v", got)
	}
	input := wrencomputer.DesktopInput{Kind: "move", X: 10, Y: 20, X2: 30, Y2: 40, Button: 1, Amount: 2, Text: "t", Key: "k", URL: "u"}
	if err := agent.Input(context.Background(), input); err != nil {
		t.Fatalf("input: %v", err)
	}
	if len(inner.inputs) != 1 || !reflect.DeepEqual(inner.inputs[0], input) {
		t.Fatalf("input did not reach inner exactly: %+v", inner.inputs)
	}

	// Optional surface genuinely missing on the inner: controlled error, no panic.
	stub := liveProbeWrapper(agentOnlyFake{sandboxAgentClient: &fakeProbeClient{sandboxFakeClient: &sandboxFakeClient{}}})
	if _, err := stub.ExportSandboxSnapshot(context.Background(), liveSnapshotOptions(), io.Discard); err == nil {
		t.Fatal("missing optional surface must return a controlled error")
	}
}
