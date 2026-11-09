package controlplane

// Sandbox desktop: an optional graphical environment on >=2CPU/4GB sandboxes.
// One organization member can take human control (exclusive input + gated
// commands); everyone else shares the screen read-only. Mutations and input
// are fenced through sandbox_mutations + sandbox_desktop_control.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

type SandboxDesktopControl struct {
	Mode       string     `json:"mode"`
	Generation int        `json:"generation"`
	AccountID  string     `json:"accountId,omitempty"`
	Name       string     `json:"name,omitempty"`
	LeaseToken string     `json:"leaseToken,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

type SandboxDesktopControlInput struct {
	Mode               string `json:"mode"`
	ExpectedGeneration int    `json:"expectedGeneration"`
	LeaseToken         string `json:"leaseToken,omitempty"`
}

// sandboxDesktopInputClaim identifies the party driving desktop input through
// the mutation fence: nil means an ordinary mutation; nonnil with empty
// AccountID means agent desktop input; nonnil with both fields is human input.
type sandboxDesktopInputClaim struct {
	AccountID  string
	LeaseToken string
}

// sandboxDesktopAgent is the optional guest surface a desktop sandbox's client
// may implement; headless guests reject it.
type sandboxDesktopAgent interface {
	Probe(context.Context) (wrencomputer.Capabilities, error)
	Screenshot(context.Context) ([]byte, error)
	ScreenStream(context.Context, func([]byte) error) error
	Input(context.Context, wrencomputer.DesktopInput) error
	Close()
}

// sandboxDesktopControlState reads the active human holder. viewerAccount
// receives leaseToken only when it IS the holder — never to other members.
func sandboxDesktopControlState(ctx context.Context, q querier, workspace, id string, generation int, viewerAccount string) (SandboxDesktopControl, error) {
	out := SandboxDesktopControl{Mode: "agent", Generation: generation}
	var account, email, token string
	var expires time.Time
	err := q.QueryRow(ctx,
		`SELECT c.account_id,a.email,c.lease_token,c.expires_at
		 FROM sandbox_desktop_control c
		 JOIN memberships m ON m.account_id=c.account_id AND m.workspace_id=$2
		 JOIN accounts a ON a.id=c.account_id AND a.disabled_at IS NULL
		 WHERE c.sandbox_id=$1 AND c.generation=$3 AND c.expires_at>clock_timestamp()
		 AND m.role IN ('owner','operator')`,
		id, workspace, generation).Scan(&account, &email, &token, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Mode = "human"
	out.AccountID = account
	out.Name = strings.Split(email, "@")[0]
	out.ExpiresAt = &expires
	if viewerAccount == account {
		out.LeaseToken = token
	}
	return out, nil
}

// SandboxDesktopControl returns the current control presentation for a member.
func (s *Store) SandboxDesktopControl(ctx context.Context, workspace, id, account string) (SandboxDesktopControl, error) {
	v, err := s.Sandbox(ctx, workspace, id)
	if err != nil {
		return SandboxDesktopControl{}, err
	}
	if !v.Desktop {
		return SandboxDesktopControl{}, fmt.Errorf("%w: this sandbox has no desktop", ErrConflict)
	}
	if err := sandboxMutationAllowed(v); err != nil {
		return SandboxDesktopControl{}, err
	}
	return sandboxDesktopControlState(ctx, s.pool, workspace, id, v.Generation, account)
}

// SetSandboxDesktopControl acquires (mode=human) or releases (mode=agent) the
// human control lease under the workspace→sandbox lock order.
func (h *HTTPServer) SetSandboxDesktopControl(ctx context.Context, workspace, id, account string, actor sdk.ActorRef, input SandboxDesktopControlInput) (SandboxDesktopControl, error) {
	if account == "" {
		return SandboxDesktopControl{}, ErrForbidden
	}
	if input.Mode != "human" && input.Mode != "agent" {
		return SandboxDesktopControl{}, fmt.Errorf("%w: mode must be human or agent", ErrConflict)
	}
	if input.ExpectedGeneration <= 0 {
		return SandboxDesktopControl{}, fmt.Errorf("%w: expectedGeneration is required", ErrConflict)
	}
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SandboxDesktopControl{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := s.lockWorkspace(ctx, tx, workspace, account); err != nil {
		return SandboxDesktopControl{}, err
	}
	if err := requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return SandboxDesktopControl{}, err
	}
	v, err := scanSandbox(tx.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id))
	if err != nil {
		return SandboxDesktopControl{}, err
	}
	if !v.Desktop {
		return SandboxDesktopControl{}, fmt.Errorf("%w: this sandbox has no desktop", ErrConflict)
	}
	if v.Generation != input.ExpectedGeneration {
		return SandboxDesktopControl{}, fmt.Errorf("%w: sandbox changed; reload before taking control", ErrConflict)
	}
	if err := sandboxMutationAllowed(v); err != nil {
		return SandboxDesktopControl{}, err
	}
	current, err := sandboxDesktopControlState(ctx, tx, workspace, id, v.Generation, account)
	if err != nil {
		return SandboxDesktopControl{}, err
	}
	if current.Mode != "human" && input.LeaseToken != "" {
		return SandboxDesktopControl{}, fmt.Errorf("%w: desktop lease expired; take control again", ErrConflict)
	}

	if input.Mode == "agent" {
		// Release requires an active own matching lease — no in-flight mutation.
		if current.Mode != "human" || current.AccountID != account || current.LeaseToken == "" || current.LeaseToken != input.LeaseToken {
			return SandboxDesktopControl{}, fmt.Errorf("%w: no active desktop lease for this account", ErrConflict)
		}
		var busy bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM sandbox_mutations WHERE sandbox_id=$1 AND expires_at>clock_timestamp())`, id).Scan(&busy); err != nil {
			return SandboxDesktopControl{}, err
		}
		if busy {
			return SandboxDesktopControl{}, fmt.Errorf("%w: wait for the in-flight desktop operation before releasing control", ErrConflict)
		}
		result, err := tx.Exec(ctx,
			`DELETE FROM sandbox_desktop_control WHERE sandbox_id=$1 AND generation=$2 AND account_id=$3 AND lease_token=$4`,
			id, v.Generation, account, input.LeaseToken)
		if err != nil {
			return SandboxDesktopControl{}, err
		}
		if result.RowsAffected() != 1 {
			return SandboxDesktopControl{}, fmt.Errorf("%w: desktop lease changed", ErrConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return SandboxDesktopControl{}, err
		}
		_ = s.Audit(ctx, workspace, actor, "sandbox.desktop_control", id, map[string]any{"mode": "agent"})
		return SandboxDesktopControl{Mode: "agent", Generation: v.Generation}, nil
	}

	// mode=human
	token := input.LeaseToken
	renewal := current.Mode == "human" && current.AccountID == account && current.LeaseToken != "" && current.LeaseToken == input.LeaseToken
	if current.Mode == "human" && !renewal {
		return SandboxDesktopControl{}, fmt.Errorf("%w: another member controls the desktop", ErrConflict)
	}
	if !renewal {
		// New acquisition: no open jobs and no live mutations.
		var busy bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM sandbox_jobs WHERE sandbox_id=$1 AND finished_at IS NULL)
			 OR EXISTS(SELECT 1 FROM sandbox_mutations WHERE sandbox_id=$1 AND expires_at>clock_timestamp())`, id).Scan(&busy); err != nil {
			return SandboxDesktopControl{}, err
		}
		if busy {
			return SandboxDesktopControl{}, fmt.Errorf("%w: Stop running commands and close terminals or previews before taking desktop control.", ErrConflict)
		}
		token, err = newID("sdc_")
		if err != nil {
			return SandboxDesktopControl{}, err
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sandbox_desktop_control(sandbox_id,account_id,generation,lease_token,expires_at)
		 VALUES($1,$2,$3,$4,clock_timestamp()+interval '60 seconds')
		 ON CONFLICT(sandbox_id) DO UPDATE SET account_id=EXCLUDED.account_id,generation=EXCLUDED.generation,
		 lease_token=EXCLUDED.lease_token,expires_at=EXCLUDED.expires_at`,
		id, account, v.Generation, token); err != nil {
		return SandboxDesktopControl{}, err
	}
	state, err := sandboxDesktopControlState(ctx, tx, workspace, id, v.Generation, account)
	if err != nil {
		return SandboxDesktopControl{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SandboxDesktopControl{}, err
	}
	if !renewal {
		s.touchSandbox(ctx, id)
		_ = s.Audit(ctx, workspace, actor, "sandbox.desktop_control", id, map[string]any{"mode": "human"})
	}
	return state, nil
}

// sandboxDesktopClient returns the desktop-capable guest client after Desktop
// + phase/sleep/delete validation; the underlying agent must implement the
// optional desktop surface and report Desktop=true.
func (h *HTTPServer) sandboxDesktopClient(ctx context.Context, v Sandbox) (sandboxDesktopAgent, error) {
	if !v.Desktop {
		return nil, fmt.Errorf("%w: this sandbox has no desktop", ErrConflict)
	}
	if err := sandboxMutationAllowed(v); err != nil {
		return nil, err
	}
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		return nil, err
	}
	fenced, ok := client.(*fencedSandboxClient)
	if !ok {
		client.Close()
		return nil, fmt.Errorf("unexpected sandbox client")
	}
	agent, ok := fenced.sandboxAgentClient.(sandboxDesktopAgent)
	if !ok {
		fenced.sandboxAgentClient.Close()
		return nil, fmt.Errorf("%w: this guest has no desktop", ErrConflict)
	}
	caps, err := agent.Probe(ctx)
	if err != nil {
		agent.Close()
		return nil, err
	}
	if !caps.Desktop {
		agent.Close()
		return nil, fmt.Errorf("%w: this guest has no desktop", ErrConflict)
	}
	return agent, nil
}

// sandboxDesktopInput sends one human/agent desktop input under the mutation
// fence — the claim binds this RPC to the durable control lease.
func (h *HTTPServer) sandboxDesktopInput(ctx context.Context, v Sandbox, input wrencomputer.DesktopInput, claim sandboxDesktopInputClaim) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := h.sandboxDesktopClient(ctx, v)
	if err != nil {
		return err
	}
	defer client.Close()
	op, err := h.service.Store.beginSandboxMutationWithControl(ctx, v, 20*time.Second, &claim)
	if err != nil {
		return err
	}
	defer op.close()
	if err := op.run(func(active context.Context) error { return client.Input(active, input) }); err != nil {
		return err
	}
	h.service.Store.touchSandbox(ctx, v.ID)
	return nil
}

// sandboxDesktopScreenshot returns a bounded PNG with validated dimensions.
func (h *HTTPServer) sandboxDesktopScreenshot(ctx context.Context, v Sandbox) ([]byte, int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client, err := h.sandboxDesktopClient(ctx, v)
	if err != nil {
		return nil, 0, 0, err
	}
	defer client.Close()
	shot, err := client.Screenshot(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	if len(shot) > 1<<20 {
		return nil, 0, 0, fmt.Errorf("%w: screenshot exceeds 1 MiB", ErrConflict)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(shot))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: screenshot is not a valid PNG", ErrConflict)
	}
	if cfg.Width < 1 || cfg.Width > 4096 || cfg.Height < 1 || cfg.Height > 4096 {
		return nil, 0, 0, fmt.Errorf("%w: screenshot has invalid dimensions", ErrConflict)
	}
	return shot, cfg.Width, cfg.Height, nil
}

// workspaceSandboxDesktop routes /desktop/{control,input,screenshot,screen}.
func (h *HTTPServer) workspaceSandboxDesktop(w http.ResponseWriter, r *http.Request, p Principal, v Sandbox, write bool, parts []string) {
	if len(parts) == 0 {
		writeStoreError(w, ErrNotFound)
		return
	}
	if len(parts) != 1 {
		writeStoreError(w, ErrNotFound)
		return
	}
	account := ""
	if p.Account != nil && p.Installation == nil {
		account = p.Account.ID
	}
	switch parts[0] {
	case "control":
		switch r.Method {
		case http.MethodGet:
			state, err := h.service.Store.SandboxDesktopControl(r.Context(), v.WorkspaceID, v.ID, account)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, state)
		case http.MethodPost:
			if !write || p.Account == nil || p.Installation != nil {
				writeStoreError(w, ErrForbidden)
				return
			}
			var input SandboxDesktopControlInput
			if !decodeLimit(w, r, &input, 4096) {
				return
			}
			state, err := h.SetSandboxDesktopControl(r.Context(), v.WorkspaceID, v.ID, p.Account.ID, p.Actor, input)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, state)
		default:
			methodNotAllowed(w)
		}
	case "input":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if !write || p.Account == nil || p.Installation != nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		var body struct {
			wrencomputer.DesktopInput
			ExpectedGeneration int    `json:"expectedGeneration"`
			LeaseToken         string `json:"leaseToken"`
		}
		if !decodeLimit(w, r, &body, 32768) {
			return
		}
		if body.ExpectedGeneration <= 0 || body.ExpectedGeneration != v.Generation || body.LeaseToken == "" {
			writeStoreError(w, ErrConflict)
			return
		}
		if err := h.sandboxDesktopInput(r.Context(), v, body.DesktopInput, sandboxDesktopInputClaim{AccountID: p.Account.ID, LeaseToken: body.LeaseToken}); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
	case "screenshot":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		authCtx, stopAuth, err := h.sandboxReadStreamContext(r, v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer stopAuth()
		shot, width, height, err := h.sandboxDesktopScreenshot(authCtx, v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Desktop-Width", fmt.Sprint(width))
		w.Header().Set("X-Desktop-Height", fmt.Sprint(height))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(shot)
	case "screen":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		h.streamSandboxDesktop(w, r, v)
	default:
		writeStoreError(w, ErrNotFound)
	}
}

// streamSandboxDesktop reuses the Wren screen framing — uint32be length + JPEG
// per frame, a zero-length terminal frame only on clean end.
func (h *HTTPServer) streamSandboxDesktop(w http.ResponseWriter, r *http.Request, v Sandbox) {
	authCtx, stopAuth, err := h.sandboxReadStreamContext(r, v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer stopAuth()
	client, err := h.sandboxDesktopClient(authCtx, v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer client.Close()
	started := false
	response := http.NewResponseController(w)
	err = client.ScreenStream(authCtx, func(frame []byte) error {
		if err := authCtx.Err(); err != nil {
			return err
		}
		if !started {
			w.Header().Set("Content-Type", wrencomputer.ScreenContentType)
			w.Header().Set("Cache-Control", "no-store, no-transform")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		_ = response.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := binary.Write(w, binary.BigEndian, uint32(len(frame))); err != nil {
			return err
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		return response.Flush()
	})
	if !started {
		if errors.Is(err, wrencomputer.ErrScreenUnsupported) {
			writeError(w, http.StatusNotImplemented, err)
		} else if err != nil {
			writeStoreError(w, err)
		} else {
			writeStoreError(w, fmt.Errorf("%w: desktop feed ended without a frame", ErrConflict))
		}
		return
	}
	if err == nil {
		_ = response.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = binary.Write(w, binary.BigEndian, uint32(0))
		_ = response.Flush()
	}
}
