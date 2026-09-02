package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
)

type HTTPConfig struct {
	PublicURL     string
	CookieSecure  bool
	RequireInvite bool
}

type HTTPServer struct {
	service *Service
	config  HTTPConfig
	limits  *requestLimiter
}

func NewHTTPServer(service *Service, config HTTPConfig) http.Handler {
	return &HTTPServer{service: service, config: config, limits: newRequestLimiter()}
}

func (h *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.allowRequest(w, r) {
		return
	}
	if isUnsafeMethod(r.Method) && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		if _, err := h.humanCookie(r); err == nil && !h.trustedHumanOrigin(r) {
			writeStoreError(w, ErrForbidden)
			return
		}
	}
	if r.URL.Path == "/mcp" {
		h.mcp(w, r)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/readyz" && r.Method == http.MethodGet {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.service.Store.Ready(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("not ready"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	switch parts[1] {
	case "auth":
		h.auth(w, r, parts[2:])
		return
	case "me":
		h.me(w, r)
		return
	case "device":
		h.device(w, r, parts[2:])
		return
	case "agent":
		h.agent(w, r, parts[2:])
		return
	case "change-approvals":
		h.changeApprovals(w, r, parts[2:])
		return
	case "node":
		h.nodeGateway(w, r, parts[2:])
		return
	case "installations":
		h.installations(w, r, parts[2:])
		return
	case "workspaces":
		h.workspaces(w, r, parts[2:])
		return
	case "executions":
		h.executions(w, r, parts[2:])
		return
	case "initial-deployment-executions":
		h.initialDeploymentExecutions(w, r, parts[2:])
		return
	default:
		writeError(w, http.StatusNotFound, ErrNotFound)
	}
}

func (h *HTTPServer) nodeGateway(w http.ResponseWriter, r *http.Request, parts []string) {
	if !nodeRequestIsHTTPS(r) {
		writeError(w, http.StatusUpgradeRequired, fmt.Errorf("node gateway requires HTTPS"))
		return
	}
	if len(parts) == 3 && parts[0] == "enrollments" && parts[2] == "exchange" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		token, ok := bearerValue(r, "ce_")
		if !ok {
			writeStoreError(w, ErrUnauthorized)
			return
		}
		credential, err := h.service.Store.ExchangeNodeEnrollment(r.Context(), parts[1], token)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, credential)
		return
	}
	token, ok := bearerValue(r, "cn_")
	if !ok {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	node, err := h.service.Store.ResolveNode(r.Context(), token)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 1 && parts[0] == "snapshot" && r.Method == http.MethodGet {
		snapshot, err := h.service.NodeSnapshot(r.Context(), node)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.Header().Set("ETag", `"`+snapshot.Generation+`"`)
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	if len(parts) == 2 && parts[0] == "artifacts" && r.Method == http.MethodGet {
		artifact, err := h.service.NodeArtifact(r.Context(), node, parts[1])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(artifact)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifact)
		return
	}
	if len(parts) == 1 && parts[0] == "observed" && r.Method == http.MethodPut {
		var observed sdk.ObservedRelease
		if !decodeLimit(w, r, &observed, 256<<10) {
			return
		}
		if err := h.service.PutNodeObserved(r.Context(), node, observed); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
		return
	}
	if len(parts) == 2 && parts[0] == "control-acks" && r.Method == http.MethodPut {
		if err := h.service.AckNodeControl(r.Context(), node, parts[1]); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
		return
	}
	if len(parts) == 3 && parts[0] == "runtime-actions" && parts[2] == "result" && r.Method == http.MethodPut {
		var result sdk.RuntimeActionResult
		if !decodeLimit(w, r, &result, 256<<10) {
			return
		}
		if err := h.service.PutNodeRuntimeResult(r.Context(), node, parts[1], result); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func bearerValue(r *http.Request, prefix string) (string, bool) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	return token, strings.HasPrefix(token, prefix)
}

// Forwarded scheme is trusted only from a loopback reverse proxy. A public
// peer cannot turn plain HTTP into an authenticated node channel with a header.
func nodeRequestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (h *HTTPServer) auth(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	switch parts[0] {
	case "signup":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var in struct {
			Email     string `json:"email"`
			Password  string `json:"password"`
			InviteKey string `json:"inviteKey"`
		}
		if !decode(w, r, &in) {
			return
		}
		account, workspace, token, err := h.service.Store.Signup(r.Context(), in.Email, in.Password, in.InviteKey, h.config.RequireInvite)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		h.setHumanCookie(w, token, 7*24*time.Hour)
		writeJSON(w, http.StatusCreated, map[string]any{"account": account, "workspace": workspace})
	case "signin":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		account, workspaces, token, err := h.service.Store.Signin(r.Context(), in.Email, in.Password)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		h.setHumanCookie(w, token, 7*24*time.Hour)
		writeJSON(w, http.StatusOK, map[string]any{"account": account, "workspaces": workspaces})
	case "signout":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var revokeErr error
		if cookie, err := h.humanCookie(r); err == nil {
			revokeErr = h.service.Store.RevokeHumanSession(r.Context(), cookie.Value)
		}
		h.setHumanCookie(w, "", -time.Hour)
		if revokeErr != nil {
			writeStoreError(w, revokeErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"signedOut": true})
	default:
		writeError(w, http.StatusNotFound, ErrNotFound)
	}
}

func (h *HTTPServer) me(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	workspaces, err := h.service.Store.WorkspacesForAccount(r.Context(), p.Account.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": p.Account, "workspaces": workspaces})
}

func (h *HTTPServer) device(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 1 && parts[0] == "authorizations" && r.Method == http.MethodPost {
		var in struct {
			Name      string    `json:"name"`
			Harness   string    `json:"harness"`
			Authority Authority `json:"authority"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := h.service.Store.BeginDeviceAuthorization(r.Context(), in.Name, in.Harness, in.Authority, h.config.PublicURL)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
		return
	}
	if len(parts) == 1 && parts[0] == "token" && r.Method == http.MethodPost {
		var in struct {
			DeviceCode     string `json:"deviceCode"`
			ClientInstance string `json:"clientInstance"`
		}
		if !decode(w, r, &in) {
			return
		}
		pair, err := h.service.Store.ExchangeDevice(r.Context(), in.DeviceCode, in.ClientInstance)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pair)
		return
	}
	if len(parts) >= 2 && parts[0] == "authorizations" {
		p, err := h.human(r)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		code := parts[1]
		if len(parts) == 2 && r.Method == http.MethodGet {
			d, err := h.service.Store.DeviceByUserCode(r.Context(), code)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			status := "pending"
			if d.AuthorizedAt != nil {
				status = "authorized"
			}
			if d.DeniedAt != nil {
				status = "denied"
			}
			if time.Now().After(d.ExpiresAt) {
				status = "expired"
			}
			writeJSON(w, http.StatusOK, map[string]any{"userCode": code, "name": d.Name, "harness": d.Harness, "authority": d.Authority, "status": status, "expiresAt": d.ExpiresAt})
			return
		}
		if len(parts) == 3 && parts[2] == "approve" && r.Method == http.MethodPost {
			var in struct {
				WorkspaceID string `json:"workspaceId"`
			}
			if !decode(w, r, &in) {
				return
			}
			installation, err := h.service.Store.ApproveDevice(r.Context(), code, p.Account.ID, in.WorkspaceID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			_ = h.service.Store.Audit(r.Context(), in.WorkspaceID, p.Actor, "agent.authorized", installation.ID, map[string]any{"harness": installation.Harness})
			writeJSON(w, http.StatusOK, map[string]any{"installation": installation})
			return
		}
		if len(parts) == 3 && parts[2] == "deny" && r.Method == http.MethodPost {
			if err := h.service.Store.DenyDevice(r.Context(), code, p.Account.ID); err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"denied": true})
			return
		}
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) agent(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 2 && parts[0] == "token" && parts[1] == "refresh" && r.Method == http.MethodPost {
		var in struct {
			RefreshToken   string `json:"refreshToken"`
			ClientInstance string `json:"clientInstance"`
		}
		if !decode(w, r, &in) {
			return
		}
		pair, err := h.service.Store.RefreshAgent(r.Context(), in.RefreshToken, in.ClientInstance)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pair)
		return
	}
	p, err := h.agentPrincipal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 1 && parts[0] == "whoami" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"installation": p.Installation, "session": p.Session})
		return
	}
	if len(parts) == 1 && parts[0] == "bootstrap" && r.Method == http.MethodGet {
		out, err := h.service.Bootstrap(r.Context(), p)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(parts) == 1 && parts[0] == "heartbeat" && r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]any{"session": p.Session, "authorized": true})
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) installations(w http.ResponseWriter, r *http.Request, parts []string) {
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	workspaceID := r.URL.Query().Get("workspaceId")
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("workspaceId is required"))
		return
	}
	if _, err = h.service.Store.Membership(r.Context(), p.Account.ID, workspaceID); err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.ListInstallations(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"installations": items})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		role, err := h.service.Store.Membership(r.Context(), p.Account.ID, workspaceID)
		if err != nil || role != "owner" {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err := h.service.Store.RevokeInstallation(r.Context(), workspaceID, parts[0]); err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "agent.revoked", parts[0], map[string]any{})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) changeApprovals(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 1 || len(parts) > 2 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	p, err := h.human(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	token := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		review, err := h.service.Store.ReviewChangeApprovalCapability(r.Context(), token, p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		review.Change = publicChange(review.Change)
		writeJSON(w, http.StatusOK, review)
		return
	}
	if len(parts) == 2 && parts[1] == "approve" && r.Method == http.MethodPost {
		var input struct{}
		if !decode(w, r, &input) {
			return
		}
		review, err := h.service.Store.ConsumeChangeApprovalCapability(r.Context(), token, p.Account.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		change, err := h.service.AuthorizeChange(r.Context(), review.Capability.WorkspaceID, review.Capability.System, review.Capability.ChangeID, review.Capability.Digest, p.Actor)
		if err != nil {
			_ = h.service.Store.Audit(r.Context(), review.Capability.WorkspaceID, p.Actor, "change.approval-capability.failed", review.Capability.ID, map[string]any{"changeId": review.Capability.ChangeID})
			writeStoreError(w, err)
			return
		}
		execution, err := h.service.Store.EnqueueExecution(r.Context(), review.Capability.WorkspaceID, review.Capability.System, review.Capability.ChangeID, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if err = h.service.Store.RecordChangeApprovalExecution(r.Context(), review.Capability.ID, execution.ID); err != nil {
			writeStoreError(w, err)
			return
		}
		review.Capability.ExecutionID = execution.ID
		_ = h.service.Store.Audit(r.Context(), review.Capability.WorkspaceID, p.Actor, "change.approval-capability.consumed", review.Capability.ID, map[string]any{"changeId": review.Capability.ChangeID, "digest": review.Capability.Digest, "executionId": execution.ID, "requestedBy": review.Capability.RequestedBy.ID})
		_ = h.service.Store.Audit(r.Context(), review.Capability.WorkspaceID, p.Actor, "execution.queued", execution.ID, map[string]any{"changeId": review.Capability.ChangeID, "approvalCapabilityId": review.Capability.ID})
		writeJSON(w, http.StatusAccepted, ChangeApprovalResult{Capability: review.Capability, Change: publicChange(change), Execution: execution})
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) workspaces(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 2 {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	workspaceID := parts[0]
	p, err := h.principal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err = h.allowWorkspace(r, p, workspaceID, false); err != nil {
		writeStoreError(w, err)
		return
	}
	if parts[1] == "bootstrap" && len(parts) == 2 && r.Method == http.MethodGet {
		workspace, err := h.service.Store.Workspace(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		systems, err := h.service.Store.ListSystems(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		changes, err := h.service.Store.ListChanges(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		deployments, err := h.service.Store.ListInitialDeployments(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"protocolVersion": "v1", "workspace": workspace, "systems": systems, "changes": changes, "initialDeployments": deployments, "capabilities": initialDeploymentCapabilities(workspaceID), "incidents": []any{}})
		return
	}
	if parts[1] == "changes" && len(parts) == 2 && r.Method == http.MethodGet {
		changes, err := h.service.Store.ListChanges(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
		return
	}
	if parts[1] == "artifacts" && len(parts) == 2 && r.Method == http.MethodPost {
		if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
			writeStoreError(w, err)
			return
		}
		if p.Installation != nil && !p.Installation.Authority.Draft {
			writeStoreError(w, ErrForbidden)
			return
		}
		const maxArtifact = 64 << 20
		r.Body = http.MaxBytesReader(w, r.Body, maxArtifact)
		data, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("artifact exceeds 64 MiB or could not be read: %w", readErr))
			return
		}
		artifact, uploadErr := h.service.UploadDeploymentArtifact(r.Context(), workspaceID, data, r.Header.Get("X-Canter-Filename"), r.Header.Get("Content-Type"), p.Actor)
		if uploadErr != nil {
			writeStoreError(w, uploadErr)
			return
		}
		writeJSON(w, http.StatusCreated, artifact)
		return
	}
	if parts[1] == "initial-deployments" {
		h.initialDeployments(w, r, p, workspaceID, parts[2:])
		return
	}
	if parts[1] != "systems" {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	if len(parts) == 2 {
		if r.Method == http.MethodGet {
			systems, err := h.service.Store.ListSystems(r.Context(), workspaceID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"systems": systems})
			return
		}
		if r.Method == http.MethodPost {
			if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
				writeStoreError(w, err)
				return
			}
			var system sdk.System
			if !decode(w, r, &system) {
				return
			}
			record, err := h.service.PutSystem(r.Context(), workspaceID, system)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "system.registered", system.Metadata.Name, map[string]any{"revision": record.Revision})
			writeJSON(w, http.StatusCreated, record)
			return
		}
	}
	if len(parts) >= 3 {
		systemName := parts[2]
		if len(parts) == 3 && r.Method == http.MethodGet {
			view, err := h.service.InspectSystem(r.Context(), workspaceID, systemName)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, view)
			return
		}
		if len(parts) == 4 && parts[3] == "policies" && r.Method == http.MethodGet {
			policies, err := h.service.Store.ListStandingPolicies(r.Context(), workspaceID, systemName)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
			return
		}
		if len(parts) == 4 && parts[3] == "policies" && r.Method == http.MethodPost {
			if p.Account == nil {
				writeStoreError(w, ErrForbidden)
				return
			}
			if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
				writeStoreError(w, err)
				return
			}
			var input CreateStandingPolicyInput
			if !decode(w, r, &input) {
				return
			}
			policy, err := h.service.Store.CreateStandingPolicy(r.Context(), workspaceID, systemName, p.Account.ID, input)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "standing-policy.created", policy.ID, map[string]any{"system": systemName, "digest": policy.Digest, "expiresAt": policy.ExpiresAt})
			writeJSON(w, http.StatusCreated, policy)
			return
		}
		if len(parts) == 5 && parts[3] == "policies" && r.Method == http.MethodGet {
			policy, err := h.service.Store.StandingPolicy(r.Context(), workspaceID, systemName, parts[4])
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, policy)
			return
		}
		if len(parts) == 6 && parts[3] == "policies" && parts[5] == "revoke" && r.Method == http.MethodPost {
			if p.Account == nil {
				writeStoreError(w, ErrForbidden)
				return
			}
			if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
				writeStoreError(w, err)
				return
			}
			policy, err := h.service.Store.RevokeStandingPolicy(r.Context(), workspaceID, systemName, parts[4], p.Account.ID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "standing-policy.revoked", policy.ID, map[string]any{"system": systemName, "digest": policy.Digest})
			writeJSON(w, http.StatusOK, policy)
			return
		}
		if len(parts) == 4 && parts[3] == "changes" && r.Method == http.MethodPost {
			if p.Installation != nil && !p.Installation.Authority.Draft {
				writeStoreError(w, ErrForbidden)
				return
			}
			var request sdk.ChangeRequest
			if !decode(w, r, &request) {
				return
			}
			change, err := h.service.DraftChange(r.Context(), workspaceID, systemName, request, p.Actor)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, publicChange(change))
			return
		}
		if len(parts) >= 5 && parts[3] == "changes" {
			changeID := parts[4]
			if len(parts) == 5 && r.Method == http.MethodGet {
				change, err := h.service.InspectChangeWithExecution(r.Context(), workspaceID, systemName, changeID)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, change)
				return
			}
			if len(parts) == 6 && parts[5] == "execution" && r.Method == http.MethodGet {
				execution, err := h.service.Store.ExecutionForChange(r.Context(), workspaceID, systemName, changeID)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, execution)
				return
			}
			if len(parts) == 6 && parts[5] == "approval-links" && r.Method == http.MethodPost {
				if p.Installation == nil || p.Session == nil {
					writeStoreError(w, ErrForbidden)
					return
				}
				if !p.Installation.Authority.Draft {
					writeStoreError(w, ErrForbidden)
					return
				}
				var input struct {
					Digest string `json:"digest"`
				}
				if !decode(w, r, &input) {
					return
				}
				capability, err := h.service.Store.CreateChangeApprovalCapability(r.Context(), workspaceID, systemName, changeID, input.Digest, p, h.config.PublicURL)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "change.approval-capability.created", capability.ID, map[string]any{"system": systemName, "changeId": changeID, "digest": input.Digest, "expiresAt": capability.ExpiresAt})
				writeJSON(w, http.StatusCreated, capability)
				return
			}
			if len(parts) == 6 && parts[5] == "apply-under-policy" && r.Method == http.MethodPost {
				if p.Installation == nil || p.Session == nil {
					writeStoreError(w, ErrForbidden)
					return
				}
				if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
					writeStoreError(w, err)
					return
				}
				var input struct {
					Digest string `json:"digest"`
				}
				if !decode(w, r, &input) {
					return
				}
				result, err := h.service.ApplyChangeUnderPolicy(r.Context(), workspaceID, systemName, changeID, input.Digest, p)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				status := http.StatusOK
				if result.Execution != nil {
					status = http.StatusAccepted
				}
				writeJSON(w, status, publicPolicyApplyResult(result))
				return
			}
			if len(parts) == 6 && parts[5] == "authorize" && r.Method == http.MethodPost {
				if p.Account == nil {
					writeStoreError(w, ErrForbidden)
					return
				}
				if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
					writeStoreError(w, err)
					return
				}
				var in struct {
					Digest string `json:"digest"`
				}
				if !decode(w, r, &in) {
					return
				}
				change, err := h.service.AuthorizeChange(r.Context(), workspaceID, systemName, changeID, in.Digest, p.Actor)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, publicChange(change))
				return
			}
			if len(parts) == 6 && parts[5] == "apply" && r.Method == http.MethodPost {
				if p.Account == nil {
					writeStoreError(w, ErrForbidden)
					return
				}
				if err = h.allowWorkspace(r, p, workspaceID, true); err != nil {
					writeStoreError(w, err)
					return
				}
				change, err := h.service.InspectChange(r.Context(), workspaceID, systemName, changeID)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				if change.Phase != "authorized" {
					writeError(w, http.StatusConflict, fmt.Errorf("change must be authorized before it can be queued"))
					return
				}
				execution, err := h.service.Store.EnqueueExecution(r.Context(), workspaceID, systemName, changeID, p.Actor)
				if err != nil {
					writeStoreError(w, err)
					return
				}
				_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "execution.queued", execution.ID, map[string]any{"changeId": changeID})
				writeJSON(w, http.StatusAccepted, execution)
				return
			}
		}
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) initialDeployments(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string, parts []string) {
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.ListInitialDeployments(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"initialDeployments": items})
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		if err := h.allowWorkspace(r, p, workspaceID, true); err != nil {
			writeStoreError(w, err)
			return
		}
		if p.Installation != nil && !p.Installation.Authority.Draft {
			writeStoreError(w, ErrForbidden)
			return
		}
		var input DraftInitialDeploymentInput
		if !decode(w, r, &input) {
			return
		}
		deployment, err := h.service.DraftInitialDeployment(r.Context(), workspaceID, input, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, publicInitialDeployment(deployment))
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		deployment, err := h.service.Store.InitialDeployment(r.Context(), workspaceID, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, publicInitialDeployment(deployment))
		return
	}
	if len(parts) == 2 && parts[1] == "authorize" && r.Method == http.MethodPost {
		if p.Account == nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err := h.allowWorkspace(r, p, workspaceID, true); err != nil {
			writeStoreError(w, err)
			return
		}
		var input struct {
			Digest string `json:"digest"`
		}
		if !decode(w, r, &input) {
			return
		}
		deployment, err := h.service.AuthorizeInitialDeployment(r.Context(), workspaceID, parts[0], input.Digest, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, publicInitialDeployment(deployment))
		return
	}
	if len(parts) == 2 && parts[1] == "apply" && r.Method == http.MethodPost {
		if p.Account == nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err := h.allowWorkspace(r, p, workspaceID, true); err != nil {
			writeStoreError(w, err)
			return
		}
		execution, err := h.service.Store.EnqueueInitialDeployment(r.Context(), workspaceID, parts[0], p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "initial-deployment.queued", execution.ID, map[string]any{"deploymentId": parts[0]})
		writeJSON(w, http.StatusAccepted, execution)
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (h *HTTPServer) executions(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 || r.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	p, err := h.principal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	execution, err := h.service.Store.Execution(r.Context(), parts[0])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err = h.allowWorkspace(r, p, execution.WorkspaceID, false); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, execution)
}

func (h *HTTPServer) initialDeploymentExecutions(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 1 || r.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	p, err := h.principal(r)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	execution, err := h.service.Store.InitialDeploymentExecution(r.Context(), parts[0])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err = h.allowWorkspace(r, p, execution.WorkspaceID, false); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, execution)
}

func (h *HTTPServer) principal(r *http.Request) (Principal, error) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return h.agentPrincipal(r)
	}
	return h.human(r)
}
func (h *HTTPServer) human(r *http.Request) (Principal, error) {
	cookie, err := h.humanCookie(r)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	return h.service.Store.ResolveHuman(r.Context(), cookie.Value)
}
func (h *HTTPServer) agentPrincipal(r *http.Request) (Principal, error) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return Principal{}, ErrUnauthorized
	}
	return h.service.Store.ResolveAgent(r.Context(), strings.TrimSpace(strings.TrimPrefix(value, "Bearer ")))
}
func (h *HTTPServer) allowWorkspace(r *http.Request, p Principal, workspaceID string, write bool) error {
	if p.Installation != nil {
		if p.WorkspaceID != workspaceID {
			return ErrForbidden
		}
		if !p.Installation.Authority.Inspect {
			return ErrForbidden
		}
		if write && !p.Installation.Authority.Draft {
			return ErrForbidden
		}
		return nil
	}
	if p.Account == nil {
		return ErrUnauthorized
	}
	role, err := h.service.Store.Membership(r.Context(), p.Account.ID, workspaceID)
	if err != nil {
		return err
	}
	if write && role == "viewer" {
		return ErrForbidden
	}
	return nil
}

func (h *HTTPServer) setHumanCookie(w http.ResponseWriter, value string, lifetime time.Duration) {
	maxAge := int(lifetime.Seconds())
	if lifetime < 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: h.humanCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: h.config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: time.Now().Add(lifetime)})
}

func (h *HTTPServer) humanCookieName() string {
	if h.config.CookieSecure {
		return "__Host-canter_session"
	}
	return "canter_session"
}

func (h *HTTPServer) humanCookie(r *http.Request) (*http.Cookie, error) {
	return r.Cookie(h.humanCookieName())
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func (h *HTTPServer) trustedHumanOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	requested, err := url.Parse(origin)
	if err != nil || requested.Scheme == "" || requested.Host == "" || requested.Path != "" || requested.RawQuery != "" || requested.Fragment != "" {
		return false
	}
	public, err := url.Parse(strings.TrimRight(h.config.PublicURL, "/"))
	if err != nil || public.Scheme == "" || public.Host == "" {
		return false
	}
	return strings.EqualFold(requested.Scheme, public.Scheme) && strings.EqualFold(requested.Host, public.Host)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeLimit(w, r, target, 2<<20)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("Content-Type must be application/json"))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, fmt.Errorf("request must contain one JSON object"))
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": err.Error()}})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}
func writeStoreError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, ErrCapacity):
		status = http.StatusTooManyRequests
	case errors.Is(err, ErrDevicePending):
		status = http.StatusPreconditionRequired
	case errors.Is(err, ErrDeviceDenied):
		status = http.StatusForbidden
	case errors.Is(err, ErrDeviceExpired):
		status = http.StatusGone
	}
	writeError(w, status, err)
}
