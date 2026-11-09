package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/canter0/canter/sdk"
)

// terminalIO is the subset of the guest terminal the manager needs; the real
// implementation is *wrencomputer.Terminal, tests use fakes.
type terminalIO interface {
	Write([]byte) error
	Resize(cols, rows int) error
	Read([]byte) (int, error)
	Close()
}

type sandboxTerminal struct {
	id        string
	sandboxID string
	accountID string
	term      terminalIO
	lease     *sandboxLiveLease
	out       chan []byte
	done      chan struct{}
	lastRead  time.Time
	mu        sync.Mutex
	exited    bool
}

// terminalManager owns live guest pty sessions. Single-process: sessions do
// not survive a restart, which the UI treats as a reconnect.
type terminalManager struct {
	mu       sync.Mutex
	sessions map[string]*sandboxTerminal
	openTerm func(context.Context, Sandbox, int, int) (terminalIO, error)
	store    *Store
}

func newTerminalManager(store *Store, open func(context.Context, Sandbox, int, int) (terminalIO, error)) *terminalManager {
	manager := &terminalManager{sessions: map[string]*sandboxTerminal{}, openTerm: open, store: store}
	go manager.sweep()
	return manager
}

func (m *terminalManager) sweep() {
	for range time.Tick(30 * time.Second) {
		m.mu.Lock()
		for id, session := range m.sessions {
			session.mu.Lock()
			stale := !session.exited && session.lastRead.Before(time.Now().Add(-10*time.Minute))
			exited := session.exited
			session.mu.Unlock()
			if exited || stale {
				if session.lease != nil {
					session.lease.Close()
				} else {
					session.term.Close()
				}
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
	}
}

// dropSandbox closes every session whose sandbox left the ready phase.
func (m *terminalManager) dropSandbox(sandboxID string, phase string) {
	if phase == "ready" {
		return
	}
	m.mu.Lock()
	for id, session := range m.sessions {
		if session.sandboxID == sandboxID {
			if session.lease != nil {
				session.lease.Close()
			} else {
				session.term.Close()
			}
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
}

func (m *terminalManager) open(ctx context.Context, v Sandbox, accountID string, validate func(context.Context) error) (*sandboxTerminal, error) {
	m.mu.Lock()
	total := len(m.sessions)
	mine := 0
	for _, s := range m.sessions {
		if s.sandboxID == v.ID {
			mine++
		}
	}
	if total >= 20 || mine >= 3 {
		m.mu.Unlock()
		return nil, ErrCapacity
	}
	m.mu.Unlock()

	// Durable mutation claim for the open itself; promoted to a live lease
	// once the remote terminal is actually up.
	op, err := m.store.beginSandboxMutation(ctx, v, 45*time.Second)
	if err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(op.ctx); err != nil {
			op.close()
			return nil, err
		}
	}
	var term terminalIO
	err = op.run(func(active context.Context) error {
		var err error
		term, err = m.openTerm(active, v, 120, 32)
		return err
	})
	if err != nil {
		op.close()
		return nil, err
	}
	if err := op.ctx.Err(); err != nil {
		term.Close()
		op.close()
		return nil, err
	}
	id, err := newID("trm_")
	if err != nil {
		term.Close()
		op.close()
		return nil, err
	}
	session := &sandboxTerminal{
		id:        id,
		sandboxID: v.ID,
		accountID: accountID,
		term:      term,
		out:       make(chan []byte, 64),
		done:      make(chan struct{}),
		lastRead:  time.Now(),
	}
	// Initialize the lease before publishing the session or starting its reader.
	session.lease = keepSandboxMutation(op, v, validate, session.term.Close)
	m.mu.Lock()
	m.sessions[session.id] = session
	m.mu.Unlock()
	go func() {
		buffer := make([]byte, 8192)
		defer func() {
			session.lease.Close()
			session.mu.Lock()
			session.exited = true
			session.mu.Unlock()
			close(session.done)
		}()
		for {
			n, err := session.term.Read(buffer)
			if n > 0 {
				session.lease.Activity()
				chunk := make([]byte, n)
				copy(chunk, buffer[:n])
				select {
				case session.out <- chunk:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return session, nil
}

func (m *terminalManager) get(sandboxID, accountID, id string) *sandboxTerminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session == nil || session.sandboxID != sandboxID || session.accountID != accountID {
		return nil
	}
	return session
}

func (m *terminalManager) close(id string) {
	m.mu.Lock()
	session := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if session != nil {
		if session.lease != nil {
			session.lease.Close() // closes remote then removes claim
		} else {
			session.term.Close()
		}
	}
}

// sandboxTerminals handles /terminals[/{tid}/(output|input|resize)].
func (h *HTTPServer) sandboxTerminals(w http.ResponseWriter, r *http.Request, v Sandbox, a Account, parts []string) {
	if v.Phase != "ready" {
		writeStoreError(w, ErrConflict)
		return
	}
	h.terminalsOnce.Do(func() {
		h.terminals = newTerminalManager(h.service.Store, h.openSandboxTerminal)
	})
	// POST /terminals → new session
	if len(parts) == 1 && r.Method == http.MethodPost {
		authorization := r.Clone(context.Background())
		authorization.Body = nil
		validate := func(ctx context.Context) error {
			request := authorization.WithContext(ctx)
			fresh, err := h.principal(request)
			if err != nil {
				return err
			}
			if fresh.Account.ID != a.ID {
				return ErrForbidden
			}
			return h.allowWorkspace(request, fresh, v.WorkspaceID, true)
		}
		session, err := h.terminals.open(r.Context(), v, a.ID, validate)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), v.WorkspaceID, sdk.ActorRef{Kind: "human", ID: a.ID}, "sandbox.terminal_opened", v.ID, map[string]any{"terminal": session.id})
		writeJSON(w, http.StatusCreated, map[string]any{"id": session.id})
		return
	}
	if len(parts) < 2 {
		writeStoreError(w, ErrNotFound)
		return
	}
	session := h.terminals.get(v.ID, a.ID, parts[1])
	if session == nil {
		writeStoreError(w, ErrNotFound)
		return
	}
	action := ""
	if len(parts) == 3 {
		action = parts[2]
	}
	switch {
	case r.Method == http.MethodDelete && action == "":
		h.terminals.close(session.id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && action == "output":
		h.sandboxTerminalOutput(w, r, session, v)
	case r.Method == http.MethodPost && action == "input":
		var body struct {
			D string `json:"d"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
		data, err := base64.StdEncoding.DecodeString(body.D)
		if err != nil || len(data) > 64<<10 {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
		err = func() error {
			op, e := h.service.Store.beginSandboxMutation(r.Context(), v, 45*time.Second)
			if e != nil {
				return e
			}
			defer op.close()
			return op.run(func(active context.Context) error {
				stop := context.AfterFunc(active, session.lease.Close)
				defer stop()
				return session.term.Write(data)
			})
		}()
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if session.lease != nil {
			session.lease.Activity()
		}
		h.service.Store.touchSandbox(r.Context(), v.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodPost && action == "resize":
		var body struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
		err := func() error {
			op, e := h.service.Store.beginSandboxMutation(r.Context(), v, 45*time.Second)
			if e != nil {
				return e
			}
			defer op.close()
			return op.run(func(active context.Context) error {
				stop := context.AfterFunc(active, session.lease.Close)
				defer stop()
				return session.term.Resize(body.Cols, body.Rows)
			})
		}()
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeStoreError(w, ErrNotFound)
	}
}

// sandboxTerminalOutput streams NDJSON {"d":"<base64>"} chunks, then {"exit":true}.
func (h *HTTPServer) sandboxTerminalOutput(w http.ResponseWriter, r *http.Request, session *sandboxTerminal, v Sandbox) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	write := func(value map[string]any) bool {
		line, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if _, err = w.Write(append(line, '\n')); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for {
		select {
		case chunk := <-session.out:
			session.mu.Lock()
			session.lastRead = time.Now()
			session.mu.Unlock()
			h.service.Store.touchSandbox(r.Context(), v.ID)
			if !write(map[string]any{"d": base64.StdEncoding.EncodeToString(chunk)}) {
				return
			}
		case <-session.done:
			write(map[string]any{"exit": true})
			return
		case <-r.Context().Done():
			return
		case <-time.After(25 * time.Second):
			if !write(map[string]any{"d": ""}) {
				return
			}
		}
	}
}

var errBadRequest = errors.New("invalid request")

// openSandboxTerminal dials the guest and starts a pty on a fresh session.
// Tests replace this on the server via sandboxTerminalOpen.
func (h *HTTPServer) openSandboxTerminal(ctx context.Context, v Sandbox, cols, rows int) (terminalIO, error) {
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		return nil, err
	}
	// Own the client for the open handshake: a stalled SSH channel open must
	// not leak the connection past the caller's cancellation.
	stop := context.AfterFunc(ctx, client.Close)
	var term terminalIO
	if h.sandboxTerminalOpen != nil {
		term, err = h.sandboxTerminalOpen(ctx, v, cols, rows)
	} else {
		raw, openErr := client.OpenTerminal(ctx, cols, rows)
		err = openErr
		if raw != nil {
			term = raw
		}
	}
	stop()
	if err != nil {
		client.Close()
		if term != nil {
			term.Close()
		}
		return nil, err
	}
	if term == nil {
		client.Close()
		return nil, ErrConflict
	}
	if ctx.Err() != nil {
		client.Close()
		term.Close()
		return nil, ctx.Err()
	}
	return &ownedTerminal{term: term, client: client}, nil
}

// ownedTerminal returns a terminalIO whose Close tears down the client first
// (releasing the SSH channel) then the raw terminal — no leaked connection.
type ownedTerminal struct {
	term   terminalIO
	client sandboxAgentClient
	once   sync.Once
}

func (o *ownedTerminal) Read(b []byte) (int, error) { return o.term.Read(b) }
func (o *ownedTerminal) Write(b []byte) error       { return o.term.Write(b) }
func (o *ownedTerminal) Resize(c, r int) error      { return o.term.Resize(c, r) }
func (o *ownedTerminal) Close() {
	o.once.Do(func() {
		o.client.Close()
		o.term.Close()
	})
}

var _ = strings.TrimSpace
