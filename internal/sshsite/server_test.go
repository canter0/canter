package sshsite

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

type transcript struct {
	sync.Mutex
	buffer bytes.Buffer
}

func (b *transcript) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.buffer.Write(p)
}
func (b *transcript) text() string { b.Lock(); defer b.Unlock(); return b.buffer.String() }

func testClient(t *testing.T) *gossh.Client {
	return testClientWithInputClock(t, time.Now)
}

func testClientWithInputClock(t *testing.T, inputClock func() time.Time) *gossh.Client {
	t.Helper()
	s, err := newServer("127.0.0.1:0", filepath.Join(t.TempDir(), "host_key"), inputClock)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Close() })
	c, err := gossh.Dial("tcp", l.Addr().String(), &gossh.ClientConfig{User: "visitor", HostKeyCallback: gossh.FixedHostKey(s.HostSigners[0].PublicKey()), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSSHHostKeyPermissionsAreRestricted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")
	server, err := NewServer("127.0.0.1:0", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	server, err = NewServer("127.0.0.1:0", path)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("host key mode = %o, want 600", got)
	}
}

func TestSSHHostKeyRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	server, err := NewServer("127.0.0.1:0", target)
	if err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	link := filepath.Join(directory, "host_key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer("127.0.0.1:0", link); err == nil {
		t.Fatal("accepted a symbolic-link host key")
	}
}

func TestSSHConnectionConcurrencyIsBounded(t *testing.T) {
	server, err := NewServer("127.0.0.1:0", filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	connections := make([]net.Conn, 0, maxSSHConnections)
	clients := make([]net.Conn, 0, maxSSHConnections)
	for i := 0; i < maxSSHConnections; i++ {
		client, peer := net.Pipe()
		wrapped := server.ConnCallback(nil, peer)
		if wrapped == nil {
			t.Fatalf("connection %d was rejected before the limit", i)
		}
		clients = append(clients, client)
		connections = append(connections, wrapped)
	}
	defer func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	client, peer := net.Pipe()
	if wrapped := server.ConnCallback(nil, peer); wrapped != nil {
		_ = wrapped.Close()
		_ = client.Close()
		t.Fatal("connection limit admitted an excess connection")
	}
	_ = client.Close()
	if err := connections[0].Close(); err != nil {
		t.Fatal(err)
	}
	client, peer = net.Pipe()
	if wrapped := server.ConnCallback(nil, peer); wrapped == nil {
		_ = client.Close()
		t.Fatal("closed connection did not release its slot")
	} else {
		_ = wrapped.Close()
		_ = client.Close()
	}
}

func TestSSHSessionChannelsAreBounded(t *testing.T) {
	c := testClient(t)
	sessions := make([]*gossh.Session, 0, maxSSHChannels)
	for i := 0; i < maxSSHChannels; i++ {
		s, err := c.NewSession()
		if err != nil {
			t.Fatalf("session channel %d was rejected before the limit: %v", i, err)
		}
		sessions = append(sessions, s)
	}
	defer func() {
		for _, s := range sessions {
			_ = s.Close()
		}
	}()
	if s, err := c.NewSession(); err == nil {
		_ = s.Close()
		t.Fatal("session channel limit admitted an excess channel")
	}
	if err := sessions[0].Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s, err := c.NewSession()
		if err == nil {
			_ = s.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("closed session channel did not release its slot")
}

func TestSSHVisitorSession(t *testing.T) {
	c := testClient(t)
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out transcript
	s.Stdout, s.Stderr = &out, &out
	in, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequestPty("xterm-256color", 35, 100, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err = s.Shell(); err != nil {
		t.Fatal(err)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(out.text(), want) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("SSH output did not contain %q: %q", want, out.text())
	}
	waitFor("Infrastructure")
	if _, err = io.WriteString(in, "2"); err != nil {
		t.Fatal(err)
	}
	waitFor("Start small. Room to grow.")
	if err = s.WindowChange(18, 40); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(in, "q"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q did not terminate the SSH session")
	}
}

func TestSSHRejectsNonApplicationAccess(t *testing.T) {
	c := testClient(t)
	for _, command := range []string{"id", "sh", "scp -t /tmp/upload"} {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Run(command); err == nil {
			t.Errorf("command %q was accepted", command)
		}
		_ = s.Close()
	}
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequestSubsystem("sftp"); err == nil {
		t.Error("SFTP was accepted")
	}
	_ = s.Close()
	if conn, err := c.Dial("tcp", "127.0.0.1:80"); err == nil {
		conn.Close()
		t.Error("local forwarding was accepted")
	}
	if l, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		l.Close()
		t.Error("remote forwarding was accepted")
	}
	s, err = c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RequestPty("xterm", 1<<25, 1<<25, gossh.TerminalModes{}); err == nil {
		t.Error("oversized PTY was accepted")
	}
}

func TestSSHExplainsMissingPTY(t *testing.T) {
	c := testClient(t)
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out bytes.Buffer
	s.Stdout = &out
	if err = s.Shell(); err != nil {
		t.Fatal(err)
	}
	if err = s.Wait(); err == nil {
		t.Fatal("missing PTY should exit unsuccessfully")
	}
	if !strings.Contains(out.String(), "ssh -t") {
		t.Fatal("missing PTY must explain how to reconnect")
	}
}

func TestSSHInputBudgetClosesFloodingSession(t *testing.T) {
	// Freeze refill across the live SSH transport so this checks that a known
	// over-budget read closes the channel, independent of race detector speed.
	now := time.Unix(100, 0)
	c := testClientWithInputClock(t, func() time.Time { return now })
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequestPty("xterm-256color", 35, 100, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err = s.Shell(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(in, strings.Repeat("x", maxSessionInputBurst*2)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Wait() }()
	select {
	case err := <-done:
		exit, ok := err.(*gossh.ExitError)
		if !ok || exit.ExitStatus() != 1 {
			t.Fatalf("over-budget session exit = %v; want status 1", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("input flood did not close the session promptly")
	}
}
