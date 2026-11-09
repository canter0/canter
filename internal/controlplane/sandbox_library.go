package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// SandboxSnapshotPage is the snapshot-library list payload — always a slice,
// never null, so the UI's iteration never branches on absent data.
type SandboxSnapshotPage struct {
	Snapshots      []SandboxSnapshot `json:"snapshots"`
	NextCursor     string            `json:"nextCursor,omitempty"`
	HistoryEnabled bool              `json:"historyEnabled"`
}

// SandboxSnapshotUpdate is the PATCH shape — name and retained are the only
// user-editable fields; storage pointers, keys and bytes never change.
type SandboxSnapshotUpdate struct {
	Name     *string `json:"name,omitempty"`
	Retained *bool   `json:"retained,omitempty"`
}

// ListSandboxSnapshots returns ready snapshots the workspace may still use —
// retained saves plus snapshots still referenced by a live machine/template —
// newest-first with a ready_at/id cursor.
func (s *Store) ListSandboxSnapshots(ctx context.Context, workspace, source, before string) (SandboxSnapshotPage, error) {
	page := SandboxSnapshotPage{Snapshots: []SandboxSnapshot{}}
	if len(source) > 128 || len(before) > 128 {
		return page, ErrConflict
	}
	var history bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_billing WHERE workspace_id=$1 AND status='active')`, workspace).Scan(&history); err != nil {
		return page, err
	}
	page.HistoryEnabled = history
	cursorTime := time.Time{}
	if before != "" {
		// Cursor metadata is allowed to be deleted — only its ready_at matters.
		var readyAt *time.Time
		err := s.pool.QueryRow(ctx, `SELECT ready_at FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, workspace, before).Scan(&readyAt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && readyAt == nil) {
			return page, ErrNotFound
		}
		if err != nil {
			return page, err
		}
		cursorTime = *readyAt
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sandboxSnapshotColumns+` FROM sandbox_snapshots p
		WHERE p.workspace_id=$1 AND p.state='ready' AND p.ready_at IS NOT NULL
		 AND (p.retained OR `+sandboxSnapshotHasReferences+`)
		 AND ($2='' OR p.source_sandbox_id=$2)
		 AND ($3='' OR (p.ready_at,p.id)<($4::timestamptz,$3))
		ORDER BY p.ready_at DESC,p.id DESC LIMIT 101`, workspace, source, before, cursorTime)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		snapshot, err := scanSandboxSnapshot(rows)
		if err != nil {
			return page, err
		}
		page.Snapshots = append(page.Snapshots, snapshot)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Snapshots) > 100 {
		page.Snapshots = page.Snapshots[:100]
		page.NextCursor = page.Snapshots[len(page.Snapshots)-1].ID
	}
	return page, nil
}

// UpdateSandboxSnapshot renames or pins a ready snapshot. Pinning new history
// needs active billing; re-pinning an already-retained snapshot, renaming and
// unpinning do not. The budget hold is reserved inside the same workspace-locked
// transaction AFTER the reference change so a capacity failure rolls both back.
func (h *HTTPServer) UpdateSandboxSnapshot(ctx context.Context, workspace, account, id string, actor sdk.ActorRef, input SandboxSnapshotUpdate) (SandboxSnapshot, error) {
	s := h.service.Store
	var snapshot SandboxSnapshot
	if input.Name == nil && input.Retained == nil {
		return snapshot, fmt.Errorf("%w: nothing to update", ErrConflict)
	}
	name := ""
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if len(name) < 1 || len(name) > 80 || strings.ContainsAny(name, "\x00\r\n") {
			return snapshot, fmt.Errorf("%w: snapshot names are 1-80 characters of single-line text", ErrConflict)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshot, ErrNotFound
		}
		return snapshot, err
	}
	if account != "" {
		if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
			return snapshot, err
		}
	}
	snapshot, err = scanSandboxSnapshot(tx.QueryRow(ctx, `SELECT `+sandboxSnapshotColumns+` FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshot, ErrNotFound
		}
		return snapshot, err
	}
	if snapshot.State != "ready" {
		return snapshot, ErrNotFound
	}
	if input.Retained != nil && *input.Retained && !snapshot.Retained {
		// New retention is a billable commitment.
		if err = requireSandboxHistoryBilling(ctx, tx, workspace); err != nil {
			return snapshot, err
		}
		enabled, e := s.productEnabled(ctx, tx, workspace, "sandboxes")
		if e != nil {
			return snapshot, e
		}
		if !enabled {
			return snapshot, fmt.Errorf("%w: Enable Sandboxes first", ErrConflict)
		}
	}
	if input.Name != nil {
		snapshot.Name = name
	}
	retainedChanged := false
	if input.Retained != nil {
		retainedChanged = *input.Retained != snapshot.Retained
		snapshot.Retained = *input.Retained
	}
	snapshot, err = scanSandboxSnapshot(tx.QueryRow(ctx, `UPDATE sandbox_snapshots SET name=$3,retained=$4,updated_at=now()
		WHERE workspace_id=$1 AND id=$2 AND state='ready'
		RETURNING `+sandboxSnapshotColumns, workspace, id, snapshot.Name, snapshot.Retained))
	if err != nil {
		return snapshot, err
	}
	if retainedChanged {
		if err = s.reserveSandboxHistoryTx(ctx, tx, workspace); err != nil {
			return snapshot, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	_ = s.Audit(ctx, workspace, actor, "sandbox-snapshot.updated", id, map[string]any{"name": snapshot.Name, "retained": snapshot.Retained})
	return snapshot, nil
}

// workspaceSandboxSnapshots serves /v1/workspaces/{ws}/sandbox-snapshots[/{id}]:
// GET lists or fetches, PATCH renames/pins. No archive download yet.
func (h *HTTPServer) workspaceSandboxSnapshots(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	s := h.service.Store
	account := ""
	if p.Account != nil {
		account = p.Account.ID
	}
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		page, err := s.ListSandboxSnapshots(r.Context(), workspace, r.URL.Query().Get("sourceSandboxId"), r.URL.Query().Get("before"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		snapshot, err := s.SandboxSnapshot(r.Context(), workspace, id)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if snapshot.State != "ready" {
			writeStoreError(w, ErrNotFound)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	case http.MethodPatch:
		if err := h.allowWorkspace(r, p, workspace, true); err != nil {
			writeStoreError(w, err)
			return
		}
		var input SandboxSnapshotUpdate
		if !decodeLimit(w, r, &input, 8192) {
			return
		}
		snapshot, err := h.UpdateSandboxSnapshot(r.Context(), workspace, account, id, p.Actor, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	default:
		methodNotAllowed(w)
	}
}
