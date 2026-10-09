package sshsite

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	"charm.land/wish/v2/bubbletea"
	gossh "golang.org/x/crypto/ssh"
)

const maxSSHConnections = 128
const maxSSHChannels = 128

// NewServer creates an anonymous public application. It has no account, shell,
// provider, database, forwarding, or agent credentials. Each session owns a model.
func NewServer(address, hostKey string) (*ssh.Server, error) {
	return newServer(address, hostKey, time.Now)
}

// newServer lets input-budget behavior use a controlled clock in transport
// tests while production always uses the wall clock through NewServer.
func newServer(address, hostKey string, inputClock func() time.Time) (*ssh.Server, error) {
	if inputClock == nil {
		inputClock = time.Now
	}
	if hostKey == "" {
		return nil, fmt.Errorf("a persistent host key path is required")
	}
	if err := os.MkdirAll(filepath.Dir(hostKey), 0700); err != nil {
		return nil, fmt.Errorf("create host key directory: %w", err)
	}
	if err := secureHostKey(hostKey); err != nil {
		return nil, err
	}
	slots := make(chan struct{}, 64)
	connections := make(chan struct{}, maxSSHConnections)
	channels := make(chan struct{}, maxSSHChannels)
	server, err := wish.NewServer(
		wish.WithAddress(address),
		wish.WithHostKeyPath(hostKey),
		wish.WithIdleTimeout(5*time.Minute),
		wish.WithMaxTimeout(30*time.Minute),
		wish.WithMiddleware(
			bubbletea.MiddlewareWithProgramHandler(func(s ssh.Session) *tea.Program {
				pty, _, _ := s.Pty()
				var input io.Reader = s
				if !s.EmulatedPty() {
					input = pty.Slave
				}
				opts := append(bubbletea.MakeOptions(s),
					tea.WithInput(newInputBudgetReader(input, inputClock, func() { _ = s.Exit(1) })),
					tea.WithFPS(30), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
						switch msg := msg.(type) {
						case tea.WindowSizeMsg:
							// Bound renderer allocations as well as our own layout.
							return tea.WindowSizeMsg{Width: max(1, min(msg.Width, 500)), Height: max(1, min(msg.Height, 200))}
						case tea.SuspendMsg:
							return tea.ResumeMsg{}
						}
						return msg
					}))
				return tea.NewProgram(NewModel(pty.Window.Width, pty.Window.Height), opts...)
			}),
			func(next ssh.Handler) ssh.Handler {
				return func(s ssh.Session) {
					if _, _, ok := s.Pty(); !ok {
						wish.Println(s, "Canter is an interactive terminal site. Reconnect with: ssh -t <host>")
						_ = s.Exit(1)
						return
					}
					select {
					case slots <- struct{}{}:
						defer func() { <-slots }()
						next(s)
					default:
						wish.Println(s, "The terminal is busy. Please reconnect in a moment.")
						_ = s.Exit(1)
					}
				}
			},
		),
	)
	if err != nil {
		return nil, err
	}
	// Wish generates a new host key with mode 0600. Apply the same protection
	// when the key did not exist during the preflight check, and verify that a
	// path replacement did not make the server load or retain a symlink.
	if err := secureHostKey(hostKey); err != nil {
		return nil, err
	}
	// Only interactive application sessions are accepted. Empty handler maps
	// explicitly disable global forwarding requests and all subsystems.
	server.SessionRequestCallback = func(_ ssh.Session, request string) bool { return request == "shell" }
	server.ConnCallback = func(_ ssh.Context, conn net.Conn) net.Conn {
		select {
		case connections <- struct{}{}:
			return &limitedConn{Conn: conn, release: func() { <-connections }}
		default:
			return nil
		}
	}
	server.HandshakeTimeout = 10 * time.Second
	server.ChannelHandlers = map[string]ssh.ChannelHandler{
		"session": func(srv *ssh.Server, conn *gossh.ServerConn, channel gossh.NewChannel, ctx ssh.Context) {
			select {
			case channels <- struct{}{}:
				defer func() { <-channels }()
				ssh.DefaultSessionHandler(srv, conn, channel, ctx)
			default:
				_ = channel.Reject(gossh.ResourceShortage, "too many session channels")
			}
		},
	}
	server.RequestHandlers = map[string]ssh.RequestHandler{}
	server.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	server.PtyCallback = func(_ ssh.Context, pty ssh.Pty) bool {
		return pty.Window.Width >= 0 && pty.Window.Width <= 500 && pty.Window.Height >= 0 && pty.Window.Height <= 200
	}
	return server, nil
}

type limitedConn struct {
	net.Conn
	release     func()
	releaseOnce sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)
	return err
}

func secureHostKey(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect SSH host key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("SSH host key must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("SSH host key must be a regular file")
	}
	if info.Mode().Perm() != 0600 {
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("restrict SSH host key permissions: %w", err)
		}
	}
	return nil
}
