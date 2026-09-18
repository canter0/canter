package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pairing invitations carry no workspace authority. Claiming one produces a
// separate device secret that is useful only after the initiating human approves.
type AgentPairing struct {
	ID             string    `json:"id"`
	WorkspaceID    string    `json:"workspaceId"`
	Status         string    `json:"status"`
	Token          string    `json:"token,omitempty"`
	Name           string    `json:"name,omitempty"`
	Harness        string    `json:"harness,omitempty"`
	InstallationID string    `json:"installationId,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

func (s *Store) CreateAgentPairing(ctx context.Context, accountID, workspaceID string) (AgentPairing, error) {
	role, err := s.Membership(ctx, accountID, workspaceID)
	if err != nil || role != "owner" {
		return AgentPairing{}, ErrForbidden
	}
	id, err := newID("pair_")
	if err != nil {
		return AgentPairing{}, err
	}
	token, err := newSecret("cp_", 32)
	if err != nil {
		return AgentPairing{}, err
	}
	now := s.now()
	out := AgentPairing{ID: id, WorkspaceID: workspaceID, Status: "waiting", Token: token, ExpiresAt: now.Add(10 * time.Minute)}
	_, err = s.pool.Exec(ctx, `INSERT INTO agent_pairings(id,workspace_id,created_by,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, id, workspaceID, accountID, secretHash(token), now, out.ExpiresAt)
	return out, err
}

func (s *Store) AgentPairing(ctx context.Context, id, accountID string) (AgentPairing, error) {
	var out AgentPairing
	err := s.pool.QueryRow(ctx, `SELECT p.id,p.workspace_id,p.expires_at,COALESCE(d.requested_name,''),COALESCE(d.harness,''),COALESCE(d.installation_id,''),CASE WHEN p.cancelled_at IS NOT NULL THEN 'cancelled' WHEN i.revoked_at IS NOT NULL OR i.expires_at<=$3 THEN 'disconnected' WHEN d.exchanged_at IS NOT NULL THEN 'connected' WHEN p.expires_at<=$3 THEN 'expired' WHEN d.authorized_at IS NOT NULL THEN 'approved' WHEN p.device_id IS NOT NULL THEN 'ready' ELSE 'waiting' END FROM agent_pairings p LEFT JOIN device_authorizations d ON d.id=p.device_id LEFT JOIN agent_installations i ON i.id=d.installation_id JOIN memberships m ON m.workspace_id=p.workspace_id AND m.account_id=p.created_by WHERE p.id=$1 AND p.created_by=$2 AND m.role='owner'`, id, accountID, s.now()).Scan(&out.ID, &out.WorkspaceID, &out.ExpiresAt, &out.Name, &out.Harness, &out.InstallationID, &out.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

func (s *Store) ClaimAgentPairing(ctx context.Context, token, name, harness, publicURL string) (DeviceAuthorization, error) {
	name, harness = strings.TrimSpace(name), strings.TrimSpace(harness)
	if !validAgentLabel(name, 120) || !validAgentLabel(harness, 60) {
		return DeviceAuthorization{}, fmt.Errorf("agent name and harness are required")
	}
	if len(token) > 128 || !strings.HasPrefix(token, "cp_") {
		return DeviceAuthorization{}, ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	var expires time.Time
	var claimed, cancelled bool
	err = tx.QueryRow(ctx, `SELECT id,expires_at,device_id IS NOT NULL,cancelled_at IS NOT NULL FROM agent_pairings WHERE token_hash=$1 FOR UPDATE`, secretHash(token)).Scan(&id, &expires, &claimed, &cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceAuthorization{}, ErrUnauthorized
	}
	if err != nil {
		return DeviceAuthorization{}, err
	}
	if cancelled {
		return DeviceAuthorization{}, ErrDeviceDenied
	}
	if !expires.After(s.now()) {
		return DeviceAuthorization{}, ErrDeviceExpired
	}
	if claimed {
		return DeviceAuthorization{}, ErrConflict
	}
	deviceID, err := newID("dev_")
	if err != nil {
		return DeviceAuthorization{}, err
	}
	secret, err := newSecret("cdc_", 32)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	code, err := newUserCode()
	if err != nil {
		return DeviceAuthorization{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO device_authorizations(id,device_hash,user_code,requested_name,harness,requested_inspect,requested_draft,requested_apply_mode,created_at,expires_at) VALUES($1,$2,$3,$4,$5,true,true,'human-approval-required',$6,$7)`, deviceID, secretHash(secret), code, name, harness, s.now(), expires)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_pairings SET device_id=$1 WHERE id=$2`, deviceID, id); err != nil {
		return DeviceAuthorization{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return DeviceAuthorization{}, err
	}
	return DeviceAuthorization{DeviceCode: secret, UserCode: code, VerificationURI: strings.TrimRight(publicURL, "/") + "/app", ExpiresAt: expires, IntervalSeconds: 2}, nil
}

func (s *Store) ApproveAgentPairing(ctx context.Context, id, accountID string, remember bool, requested ...Authority) (Installation, error) {
	authority := Authority{Inspect: true, Draft: true, ApplyMode: "human-approval-required"}
	if len(requested) > 0 {
		authority = requested[0]
	}
	if err := validateAgentAuthority(authority); err != nil {
		return Installation{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Installation{}, err
	}
	defer tx.Rollback(ctx)
	var workspaceID string
	var deviceID *string
	var expires time.Time
	var cancelled bool
	err = tx.QueryRow(ctx, `SELECT workspace_id,device_id,expires_at,cancelled_at IS NOT NULL FROM agent_pairings WHERE id=$1 AND created_by=$2 FOR UPDATE`, id, accountID).Scan(&workspaceID, &deviceID, &expires, &cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, ErrNotFound
	}
	if err != nil {
		return Installation{}, err
	}
	var role string
	if err = tx.QueryRow(ctx, `SELECT role FROM memberships WHERE account_id=$1 AND workspace_id=$2 FOR SHARE`, accountID, workspaceID).Scan(&role); err != nil || role != "owner" {
		return Installation{}, ErrForbidden
	}
	if cancelled {
		return Installation{}, ErrDeviceDenied
	}
	if !expires.After(s.now()) {
		return Installation{}, ErrDeviceExpired
	}
	if deviceID == nil {
		return Installation{}, ErrDevicePending
	}
	var name, harness string
	var authorized, denied bool
	err = tx.QueryRow(ctx, `SELECT requested_name,harness,authorized_at IS NOT NULL,denied_at IS NOT NULL FROM device_authorizations WHERE id=$1 FOR UPDATE`, *deviceID).Scan(&name, &harness, &authorized, &denied)
	if err != nil {
		return Installation{}, err
	}
	if authorized || denied {
		return Installation{}, ErrConflict
	}
	installationID, err := newID("agt_")
	if err != nil {
		return Installation{}, err
	}
	now := s.now()
	var connectionExpiry *time.Time
	if !remember {
		expiry := now.Add(8 * time.Hour)
		connectionExpiry = &expiry
	}
	out := Installation{ID: installationID, WorkspaceID: workspaceID, Name: name, Harness: harness, Authority: authority, CreatedBy: accountID, CreatedAt: now, ExpiresAt: connectionExpiry}
	_, err = tx.Exec(ctx, `INSERT INTO agent_installations(id,workspace_id,name,harness,inspect_allowed,draft_allowed,apply_mode,created_by,created_at,expires_at) VALUES($1,$2,$3,$4,$8,$9,$10,$5,$6,$7)`, out.ID, workspaceID, name, harness, accountID, now, connectionExpiry, authority.Inspect, authority.Draft, authority.ApplyMode)
	if err != nil {
		return Installation{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE device_authorizations SET workspace_id=$1,authorized_by=$2,installation_id=$3,authorized_at=$4 WHERE id=$5`, workspaceID, accountID, out.ID, now, *deviceID); err != nil {
		return Installation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Installation{}, err
	}
	return out, nil
}

func (s *Store) CancelAgentPairing(ctx context.Context, id, accountID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var deviceID *string
	err = tx.QueryRow(ctx, `UPDATE agent_pairings SET cancelled_at=COALESCE(cancelled_at,$3) WHERE id=$1 AND created_by=$2 RETURNING device_id`, id, accountID, s.now()).Scan(&deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if deviceID != nil {
		var installationID *string
		if err = tx.QueryRow(ctx, `UPDATE device_authorizations SET denied_at=COALESCE(denied_at,$2) WHERE id=$1 RETURNING installation_id`, *deviceID, s.now()).Scan(&installationID); err != nil {
			return err
		}
		if installationID != nil {
			if err = s.revokeInstallationTx(ctx, tx, *installationID); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) revokeInstallationTx(ctx context.Context, tx pgx.Tx, id string) error {
	for _, query := range []string{`UPDATE agent_installations SET revoked_at=COALESCE(revoked_at,$2) WHERE id=$1`, `UPDATE agent_credentials SET revoked_at=COALESCE(revoked_at,$2) WHERE installation_id=$1`, `UPDATE agent_sessions SET ended_at=COALESCE(ended_at,$2) WHERE installation_id=$1`} {
		if _, err := tx.Exec(ctx, query, id, s.now()); err != nil {
			return err
		}
	}
	return nil
}

func (h *HTTPServer) agentPairings(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 1 && parts[0] == "instructions" && r.Method == http.MethodGet {
		base := strings.TrimRight(h.config.PublicURL, "/") + "/api/canter"
		writeJSON(w, http.StatusOK, map[string]any{"apiBaseURL": base, "steps": []string{
			"POST " + base + "/agent-pairings/claim with JSON {token: the invitation code supplied by the user, name: your recognizable agent name, harness: your agent software}. The invitation is single-use and expires after 10 minutes. Do not send it to another service.",
			"Keep the returned deviceCode private. Poll POST " + base + "/device/token with JSON {deviceCode, clientInstance: a unique name for this conversation} every intervalSeconds until the user clicks Connect in their existing Canter window. Retry only HTTP 428 when error.message is device authorization pending. Stop on denied, expired, or conflict. Do not open another approval page or approve yourself.",
			"The successful response contains accessToken, refreshToken, installation, and session. Keep credentials in process memory or a local file with mode 0600; never put them in chat, source control, task text, or logs. Access tokens expire at session.expiresAt. Before expiry POST " + base + "/agent/token/refresh with {refreshToken,clientInstance} and replace both credentials; each refresh token is single-use. Installation expiresAt, when present, is a hard deadline.",
			"Use Authorization: Bearer <accessToken> for GET " + base + "/agent/bootstrap and subsequent Canter API requests, or connect Streamable HTTP MCP at " + base + "/mcp. Begin by reading bootstrap and inspecting available tasks. Task preferences are requests, not proof of which model executes. Check installation.authority: inspect grants reads, draft grants writes, and applyMode automatic permits canter_apply_change and canter_apply_initial_deployment with the exact proposal digest without another human approval. With human-approval-required, request human approval; with never, do not apply. Permissions may change during the session.",
			"For subagents, POST " + base + "/agent/workers with {name,clientInstance,draft:false} using the orchestrator's access token. Give each worker only its returned accessToken. Set draft:true only if the worker must prepare changes. Workers cannot delegate, refresh, claim, or finish tasks. They share the parent installation's current task for activity attribution. Their access expires with the parent session; reissue after refreshing. Never pass the orchestrator's refresh credential to workers.",
			"Finish a claimed task with canter_finish_task. Temporary connections end when their task finishes, or at the installation expiry (8 hours maximum). Remembered installations persist until revoked. POST " + base + "/agent/disconnect to end a temporary connection or the current remembered session when you are done.",
		}})
		return
	}
	if len(parts) == 1 && parts[0] == "claim" && r.Method == http.MethodPost {
		var in struct {
			Token   string `json:"token"`
			Name    string `json:"name"`
			Harness string `json:"harness"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := h.service.Store.ClaimAgentPairing(r.Context(), in.Token, in.Name, in.Harness, h.config.PublicURL)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		var in struct {
			WorkspaceID string `json:"workspaceId"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := h.service.Store.CreateAgentPairing(r.Context(), p.Account.ID, in.WorkspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		out, err := h.service.Store.AgentPairing(r.Context(), parts[0], p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := h.service.Store.CancelAgentPairing(r.Context(), parts[0], p.Account.ID); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && parts[1] == "approve" && r.Method == http.MethodPost {
		var in struct {
			Remember  bool       `json:"remember"`
			Authority *Authority `json:"authority"`
		}
		if !decode(w, r, &in) {
			return
		}
		var requested []Authority
		if in.Authority != nil {
			if err := validateAgentAuthority(*in.Authority); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			requested = append(requested, *in.Authority)
		}
		out, err := h.service.Store.ApproveAgentPairing(r.Context(), parts[0], p.Account.ID, in.Remember, requested...)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), out.WorkspaceID, p.Actor, "agent.authorized", out.ID, map[string]any{"harness": out.Harness})
		writeJSON(w, http.StatusOK, map[string]any{"installation": out})
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}
