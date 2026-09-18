package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type WorkerInput struct {
	Name           string `json:"name"`
	ClientInstance string `json:"clientInstance"`
	Draft          bool   `json:"draft"`
}

type WorkerToken struct {
	AccessToken string       `json:"accessToken"`
	TokenType   string       `json:"tokenType"`
	ExpiresAt   time.Time    `json:"expiresAt"`
	Session     AgentSession `json:"session"`
}

func (s *Store) CreateAgentWorker(ctx context.Context, p Principal, in WorkerInput) (WorkerToken, error) {
	if p.Installation == nil || p.Session == nil || p.Session.ParentSessionID != "" || !p.Installation.Authority.Inspect || (in.Draft && !p.Installation.Authority.Draft) {
		return WorkerToken{}, ErrForbidden
	}
	if !validAgentLabel(in.Name, 120) {
		return WorkerToken{}, fmt.Errorf("worker name is required")
	}
	instance, err := validatedClientInstance(in.ClientInstance)
	if err != nil {
		return WorkerToken{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkerToken{}, err
	}
	defer tx.Rollback(ctx)
	var expiry time.Time
	// Synchronize issuance with refresh/disconnect and installation revocation.
	err = tx.QueryRow(ctx, `SELECT s.expires_at FROM agent_sessions s JOIN agent_installations i ON i.id=s.installation_id WHERE s.id=$1 AND s.ended_at IS NULL AND s.expires_at>$2 AND i.revoked_at IS NULL AND (i.expires_at IS NULL OR i.expires_at>$2) FOR SHARE OF s,i`, p.Session.ID, s.now()).Scan(&expiry)
	if err != nil {
		return WorkerToken{}, ErrUnauthorized
	}
	id, err := newID("ass_")
	if err != nil {
		return WorkerToken{}, err
	}
	token, err := newSecret("ca_", 32)
	if err != nil {
		return WorkerToken{}, err
	}
	if cap := s.now().Add(time.Hour); cap.Before(expiry) {
		expiry = cap
	}
	session := AgentSession{ID: id, InstallationID: p.Installation.ID, ParentSessionID: p.Session.ID, WorkerName: in.Name, WorkerDraft: in.Draft, ClientInstance: instance, CreatedAt: s.now(), LastSeenAt: s.now(), ExpiresAt: expiry}
	_, err = tx.Exec(ctx, `INSERT INTO agent_sessions(id,installation_id,access_hash,client_instance,created_at,last_seen_at,expires_at,parent_session_id,worker_name,worker_draft) VALUES($1,$2,$3,$4,$5,$5,$6,$7,$8,$9)`, id, p.Installation.ID, secretHash(token), instance, s.now(), expiry, p.Session.ID, in.Name, in.Draft)
	if err != nil {
		return WorkerToken{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WorkerToken{}, err
	}
	return WorkerToken{AccessToken: token, TokenType: "Bearer", ExpiresAt: expiry, Session: session}, nil
}

func (s *Store) DisconnectAgent(ctx context.Context, p Principal) error {
	if p.Installation == nil || p.Session == nil {
		return ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if p.Installation.ExpiresAt != nil && p.Session.ParentSessionID == "" {
		err = s.revokeInstallationTx(ctx, tx, p.Installation.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE agent_sessions SET ended_at=COALESCE(ended_at,$2) WHERE id=$1 OR parent_session_id=$1`, p.Session.ID, s.now())
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AgentWorkers(ctx context.Context, installationID string) ([]AgentSession, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id,s.installation_id,s.parent_session_id,s.worker_name,s.worker_draft,s.client_instance,s.created_at,s.last_seen_at,s.expires_at FROM agent_sessions s JOIN agent_sessions p ON p.id=s.parent_session_id WHERE s.installation_id=$1 AND s.ended_at IS NULL AND s.expires_at>$2 AND p.ended_at IS NULL AND p.expires_at>$2 ORDER BY s.created_at LIMIT 100`, installationID, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentSession{}
	for rows.Next() {
		var v AgentSession
		if err = rows.Scan(&v.ID, &v.InstallationID, &v.ParentSessionID, &v.WorkerName, &v.WorkerDraft, &v.ClientInstance, &v.CreatedAt, &v.LastSeenAt, &v.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (h *HTTPServer) workerRequest(w http.ResponseWriter, r *http.Request, p Principal) {
	var in WorkerInput
	if !decode(w, r, &in) {
		return
	}
	out, err := h.service.Store.CreateAgentWorker(r.Context(), p, in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = h.service.Store.Audit(r.Context(), p.WorkspaceID, p.Actor, "agent.worker-connected", out.Session.ID, map[string]any{})
	writeJSON(w, http.StatusCreated, out)
}
