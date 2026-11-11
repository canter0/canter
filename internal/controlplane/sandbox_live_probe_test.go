package controlplane

// Loopback tests for sandboxLiveSSHDiagnostic: real TCP listeners and real
// SSH handshakes on 127.0.0.1; no database, provider or internet.
// They verify the diagnostic's own semantics: pinned host-key matching,
// client-side auth-rejection classification, and parent-context cancellation.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

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
