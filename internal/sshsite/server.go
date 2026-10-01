package sshsite

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	"charm.land/wish/v2/bubbletea"
)

// NewServer creates an anonymous public application. It has no account, shell,
// provider, database, forwarding, or agent credentials. Each session owns a model.
func NewServer(address, hostKey string) (*ssh.Server, error) {
	if hostKey == "" {
		return nil, fmt.Errorf("a persistent host key path is required")
	}
	if err := os.MkdirAll(filepath.Dir(hostKey), 0700); err != nil {
		return nil, fmt.Errorf("create host key directory: %w", err)
	}
	slots := make(chan struct{}, 64)
	server, err := wish.NewServer(
		wish.WithAddress(address),
		wish.WithHostKeyPath(hostKey),
		wish.WithIdleTimeout(5*time.Minute),
		wish.WithMaxTimeout(30*time.Minute),
		wish.WithMiddleware(
			bubbletea.MiddlewareWithProgramHandler(func(s ssh.Session) *tea.Program {
				pty, _, _ := s.Pty()
				opts := append(bubbletea.MakeOptions(s), tea.WithFPS(30), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
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
	// Only interactive application sessions are accepted. Empty handler maps
	// explicitly disable global forwarding requests and all subsystems.
	server.SessionRequestCallback = func(_ ssh.Session, request string) bool { return request == "shell" }
	server.HandshakeTimeout = 10 * time.Second
	server.ChannelHandlers = map[string]ssh.ChannelHandler{"session": ssh.DefaultSessionHandler}
	server.RequestHandlers = map[string]ssh.RequestHandler{}
	server.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	server.PtyCallback = func(_ ssh.Context, pty ssh.Pty) bool {
		return pty.Window.Width >= 0 && pty.Window.Width <= 500 && pty.Window.Height >= 0 && pty.Window.Height <= 200
	}
	return server, nil
}
