package sshsite

import (
	"bytes"
	"io"
	"net"
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
	t.Helper()
	s, err := NewServer("127.0.0.1:0", filepath.Join(t.TempDir(), "host_key"))
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
