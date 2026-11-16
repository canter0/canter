package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

type CreateWorkspaceInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func cleanWorkspaceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("choose a name between 1 and 64 characters without control characters")
	}
	return name, nil
}

// createWorkspaceTx provisions a workspace the same way signup does: the base
// row, compute allowance, AI allowance and the creator's owner membership.
func (s *Store) createWorkspaceTx(ctx context.Context, tx pgx.Tx, id, name, account string) (Workspace, error) {
	workspace := Workspace{ID: id, Name: name, Role: "owner"}
	if err := tx.QueryRow(ctx, `INSERT INTO workspaces(id,name,created_at) VALUES($1,$2,$3) RETURNING revision,agent_authority`, id, name, s.now()).Scan(&workspace.Revision, &workspace.AgentAuthority); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_usage_caps(workspace_id,limit_cents,created_at,updated_at) VALUES($1,500,$2,$2)`, id, s.now()); err != nil {
		return Workspace{}, err
	}
	if err := s.initializeWrenAIAllowanceTx(ctx, tx, id); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, account, id); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

// CreateWorkspace provisions an additional organization for an existing human
// account. A client-supplied ID makes a retried submission idempotent: the same
// account replaying the same name receives the original record.
func (s *Store) CreateWorkspace(ctx context.Context, account string, input CreateWorkspaceInput) (Workspace, bool, error) {
	name, err := cleanWorkspaceName(input.Name)
	if err != nil {
		return Workspace{}, false, err
	}
	id := input.ID
	if id == "" {
		if id, err = newID("wrk_"); err != nil {
			return Workspace{}, false, err
		}
	} else if !knowledgeID(id, "wrk_") {
		return Workspace{}, false, fmt.Errorf("invalid workspace ID")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Workspace{}, false, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 FOR UPDATE`, account).Scan(&locked); err != nil {
		return Workspace{}, false, err
	}
	var existing Workspace
	err = tx.QueryRow(ctx, `SELECT w.id,w.name,w.revision,w.agent_authority,COALESCE(m.role,'') FROM workspaces w LEFT JOIN memberships m ON m.workspace_id=w.id AND m.account_id=$2 WHERE w.id=$1`, id, account).Scan(&existing.ID, &existing.Name, &existing.Revision, &existing.AgentAuthority, &existing.Role)
	if err == nil {
		if existing.Role == "" || existing.Name != name {
			return Workspace{}, false, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return Workspace{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, false, err
	}
	var owned int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE account_id=$1 AND role='owner'`, account).Scan(&owned); err != nil {
		return Workspace{}, false, err
	}
	if owned >= 10 {
		return Workspace{}, false, fmt.Errorf("%w: up to 10 owned workspaces", ErrCapacity)
	}
	workspace, err := s.createWorkspaceTx(ctx, tx, id, name, account)
	if err != nil {
		return Workspace{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Workspace{}, false, err
	}
	return workspace, true, nil
}

func (s *Store) RenameWorkspace(ctx context.Context, workspace, account, name string) (Workspace, error) {
	name, err := cleanWorkspaceName(name)
	if err != nil {
		return Workspace{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Workspace{}, err
	}
	defer tx.Rollback(ctx)
	role, err := s.lockWorkspace(ctx, tx, workspace, account)
	if err != nil {
		return Workspace{}, err
	}
	if role != "owner" {
		return Workspace{}, ErrForbidden
	}
	var out Workspace
	if err = tx.QueryRow(ctx, `UPDATE workspaces SET name=$2 WHERE id=$1 RETURNING id,name,revision,agent_authority`, workspace, name).Scan(&out.ID, &out.Name, &out.Revision, &out.AgentAuthority); err != nil {
		return Workspace{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Workspace{}, err
	}
	out.Role = "owner"
	return out, nil
}

type WorkspaceInvitation struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspaceId"`
	WorkspaceName string    `json:"workspaceName"`
	InviterEmail  string    `json:"inviterEmail"`
	Role          string    `json:"role"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// AccountInvites lists pending, unexpired invitations addressed to the
// account's verified email so they can be answered inside the app.
func (s *Store) AccountInvites(ctx context.Context, account string) ([]WorkspaceInvitation, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id,i.workspace_id,w.name,COALESCE(a.email,''),i.role,i.expires_at FROM workspace_invites i JOIN workspaces w ON w.id=i.workspace_id JOIN accounts me ON me.id=$1 AND me.email=i.email AND me.email_verified_at IS NOT NULL AND me.disabled_at IS NULL LEFT JOIN accounts a ON a.id=i.inviter_id WHERE i.state='pending' AND i.expires_at>$2 ORDER BY i.created_at DESC LIMIT 100`, account, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkspaceInvitation{}
	for rows.Next() {
		var v WorkspaceInvitation
		if err = rows.Scan(&v.ID, &v.WorkspaceID, &v.WorkspaceName, &v.InviterEmail, &v.Role, &v.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (h *HTTPServer) createWorkspace(w http.ResponseWriter, r *http.Request) {
	p, err := h.principal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	var input CreateWorkspaceInput
	if !decodeLimit(w, r, &input, 4096) {
		return
	}
	workspace, created, err := h.service.Store.CreateWorkspace(r.Context(), p.Account.ID, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if created {
		_ = h.service.Store.Audit(r.Context(), workspace.ID, p.Actor, "workspace.created", workspace.ID, map[string]any{"name": workspace.Name})
		writeJSON(w, http.StatusCreated, workspace)
		return
	}
	writeJSON(w, http.StatusOK, workspace)
}

func (h *HTTPServer) renameWorkspace(w http.ResponseWriter, r *http.Request, workspace string) {
	p, err := h.principal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	var input struct {
		Name string `json:"name"`
	}
	if !decodeLimit(w, r, &input, 4096) {
		return
	}
	out, err := h.service.Store.RenameWorkspace(r.Context(), workspace, p.Account.ID, input.Name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = h.service.Store.Audit(r.Context(), workspace, p.Actor, "workspace.renamed", workspace, map[string]any{"name": out.Name})
	writeJSON(w, http.StatusOK, out)
}

func (h *HTTPServer) meInvites(w http.ResponseWriter, r *http.Request, parts []string) {
	p, err := h.principal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		invites, err := h.service.Store.AccountInvites(r.Context(), p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"invites": invites})
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost && (parts[1] == "accept" || parts[1] == "decline") {
		out, err := h.service.Store.RespondWorkspaceInvite(r.Context(), p.Account.ID, parts[0], parts[1] == "accept")
		if err != nil {
			writeStoreError(w, err)
			return
		}
		action := "invite.declined"
		if parts[1] == "accept" {
			action = "invite.accepted"
		}
		_ = h.service.Store.Audit(r.Context(), out.WorkspaceID, p.Actor, action, out.ID, map[string]any{"email": out.Email})
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

// workspaceMemory handles GET/POST /v1/workspaces/{id}/memory and
// PATCH/DELETE /v1/workspaces/{id}/memory/{memoryId}. Any member reads;
// owners and operators write, and writes require a human session.
func (h *HTTPServer) workspaceMemory(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		if p.Account == nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		items, err := h.service.Store.WorkspaceMemory(r.Context(), workspaceID, p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"memory": items})
		return
	}
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		var input WorkspaceMemoryInput
		if !decodeLimit(w, r, &input, 32<<10) {
			return
		}
		if input.ID != "" && !knowledgeID(input.ID, "wmemo_") {
			writeStoreError(w, fmt.Errorf("%w: invalid memory ID", ErrConflict))
			return
		}
		entry, err := h.service.Store.CreateWorkspaceMemory(r.Context(), workspaceID, p.Account.ID, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "memory.created", entry.ID, map[string]any{"title": entry.Title})
		writeJSON(w, http.StatusCreated, entry)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var input WorkspaceMemoryInput
		if !decodeLimit(w, r, &input, 32<<10) {
			return
		}
		entry, err := h.service.Store.UpdateWorkspaceMemory(r.Context(), workspaceID, p.Account.ID, parts[0], input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "memory.updated", entry.ID, map[string]any{"title": entry.Title, "revision": entry.Revision})
		writeJSON(w, http.StatusOK, entry)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		var input struct {
			BaseRevision int `json:"baseRevision"`
		}
		_ = decodeLimit(w, r, &input, 4<<10)
		entry, err := h.service.Store.DeleteWorkspaceMemory(r.Context(), workspaceID, p.Account.ID, parts[0], input.BaseRevision)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "memory.deleted", entry.ID, map[string]any{"title": entry.Title})
		writeJSON(w, http.StatusOK, entry)
		return
	}
	methodNotAllowed(w)
}

func (h *HTTPServer) workspaceAudit(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string) {
	if p.Account == nil || r.Method != http.MethodGet {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeStoreError(w, ErrForbidden)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, next, err := h.service.Store.AuditEvents(r.Context(), workspaceID, p.Account.ID, r.URL.Query().Get("before"), limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "nextCursor": next})
}
