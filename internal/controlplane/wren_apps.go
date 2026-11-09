package controlplane

import (
	"context"
	"errors"
)

type wrenAppConnectionStatus struct {
	ID        string                        `json:"id"`
	Name      string                        `json:"name"`
	State     string                        `json:"state"`
	Reason    string                        `json:"reason"`
	Version   int                           `json:"version"`
	CanManage bool                          `json:"canManage"`
	Tools     []WrenExtensionToolDiagnostic `json:"tools"`
}

func (h *HTTPServer) wrenAppConnectionStatuses(ctx context.Context, c Conversation, run OperatorRun) ([]wrenAppConnectionStatus, error) {
	s := h.service.Store
	out := []wrenAppConnectionStatus{}
	scope, ok, err := s.knowledgeRunScope(ctx, c, run)
	if err != nil || !ok {
		return out, err
	}
	installs, err := s.WrenExtensionInstallations(ctx, c.WorkspaceID, c.AccountID, scope)
	if err != nil {
		return nil, err
	}
	for _, v := range installs {
		tools, e := h.WrenExtensionToolDiagnostics(ctx, c.WorkspaceID, c.AccountID, v.ID)
		if e != nil {
			return nil, e
		}
		manageErr := extensionManage(ctx, s.pool, v, c.AccountID)
		if manageErr != nil && !errors.Is(manageErr, ErrForbidden) && !errors.Is(manageErr, ErrNotFound) {
			return nil, manageErr
		}
		status := wrenAppConnectionStatus{ID: v.ID, Name: v.Extension.Manifest.Name, Version: v.Version, CanManage: manageErr == nil && v.RevokedAt == nil, Tools: tools, State: "available", Reason: "Saved access permits the listed tools for this requester. This is not a live service health check."}
		available := false
		for _, tool := range tools {
			available = available || tool.CanInvoke
		}
		switch {
		case v.RevokedAt != nil:
			status.State, status.Reason = "removed", "This connection was removed. A human must add a new connection before using it again."
		case v.Extension.Disabled:
			status.State, status.Reason = "publisher_disabled", "The publisher disabled this app. The room cannot resume it until the publisher makes it available."
		case !v.Enabled:
			status.State, status.Reason = "paused", "New calls are paused for this conversation. The installer or workspace owner can resume it in Apps. Calls already sent retain their own outcomes."
		case v.Extension.Manifest.Auth == "bearer" && !v.CredentialConfigured:
			status.State, status.Reason = "needs_sign_in", "The requesting person needs to connect their account in Apps. Another participant's personal sign-in is not reused."
			if v.CredentialMode == "service" {
				status.Reason = "A manager needs to connect the room's shared service credential in Apps."
			}
		case !available:
			status.State, status.Reason = "unavailable", "No listed tools can be invoked by this requester. Inspect their reasons; do not claim that the service was tested."
		}
		out = append(out, status)
	}
	return out, nil
}

// Discovery describes available access without installing anything or exposing
// account identifiers and credentials in a shared conversation's tool result.
func (o *OperatorRuntime) showWrenApps(ctx context.Context, c Conversation, run OperatorRun) (any, error) {
	if run.ConversationID != c.ID || (run.RequesterAccountID != "" && run.RequesterAccountID != c.AccountID) {
		return nil, ErrForbidden
	}
	if _, err := o.Server.service.Store.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID); err != nil {
		return nil, err
	}
	connection, _, err := o.Server.githubAccess(ctx, c.AccountID, c.WorkspaceID)
	if err != nil {
		return nil, err
	}
	installed, err := o.Server.wrenAppConnectionStatuses(ctx, c, run)
	if err != nil {
		return nil, err
	}
	sources := []map[string]string{}
	for _, entry := range wrenConnectorCatalog() {
		sources = append(sources, map[string]string{"slug": entry.Slug, "name": entry.Manifest.Name, "description": entry.Manifest.Description, "limitation": entry.Limitation})
	}
	if err := o.surface(ctx, run, OperatorSurface{Kind: "wren-extensions", Attention: "input"}); err != nil {
		return nil, err
	}
	return map[string]any{
		"github":                  map[string]bool{"connectionAvailable": connection.Enabled, "connectedForRequester": connection.Connected, "reconnectNeeded": connection.Reconnect},
		"accountApps":             []map[string]any{{"name": "Linear", "connectionAvailable": o.Server.config.Secrets != nil, "access": "Read-only issues and projects. The human signs in, reviews tools, and adds them to this conversation. Each participant uses their own account."}},
		"publicSources":           sources,
		"conversationConnections": installed,
		"next":                    "conversationConnections is the actual saved installation and requester access state for this conversation. publicSources and accountApps are catalog choices, not proof that they are installed or connected. Use the explicit connection state and tool canInvoke fields when diagnosing access. No live service health check was performed. A human can connect or resume access in Apps, then continue in chat. This lookup did not grant access or install anything. Do not request passwords or API keys in messages. Never reuse another participant's sign-in or claim unsupported services are connected.",
	}, nil
}
