package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/canter0/canter/internal/computeclass"
	"github.com/canter0/canter/sdk"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (h *HTTPServer) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request mcpRequest
	if !decodeLimit(w, r, &request, 96<<20) {
		return
	}
	if request.JSONRPC != "2.0" {
		h.mcpError(w, request.ID, -32600, "invalid JSON-RPC request")
		return
	}
	switch request.Method {
	case "initialize":
		h.mcpResult(w, request.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]string{"name": "canter", "version": "0.1.0"}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "ping":
		h.mcpResult(w, request.ID, map[string]any{})
	case "tools/list":
		h.mcpResult(w, request.ID, map[string]any{"tools": mcpTools()})
	case "tools/call":
		principal, err := h.principal(r)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			h.mcpError(w, request.ID, -32602, "invalid tool call parameters")
			return
		}
		result, err := h.callMCPTool(r, principal, params.Name, params.Arguments)
		if err != nil {
			h.mcpResult(w, request.ID, mcpToolResult(publicMCPToolError(err), true))
			return
		}
		h.mcpResult(w, request.ID, mcpToolResult(result, false))
	default:
		h.mcpError(w, request.ID, -32601, "method not found")
	}
}

func publicMCPToolError(err error) map[string]any {
	result := map[string]any{"error": err.Error()}
	var unsupported *computeclass.UnsupportedClassError
	if errors.As(err, &unsupported) {
		result["errorCode"] = computeclass.UnsupportedCode
		result["retryable"] = false
		result["supportedHostClasses"] = sdk.SupportedHostClasses()
	}
	return result
}

func mcpTools() []mcpTool {
	object := func(properties map[string]any, required ...string) map[string]any {
		if properties == nil {
			properties = map[string]any{}
		}
		out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	hostClass := map[string]any{
		"type":        "string",
		"enum":        sdk.SupportedHostClasses(),
		"description": "Provider-neutral Canter compute class. Use exactly one advertised value; c1 is the smallest class.",
	}
	service := object(map[string]any{
		"name": str, "kind": str, "engine": str,
		"isolation":  map[string]any{"type": "string", "enum": []string{"process", "firecracker"}},
		"instances":  integer,
		"dependsOn":  map[string]any{"type": "array", "items": str},
		"resources":  object(map[string]any{"vcpu": integer, "memoryMiB": integer}, "vcpu", "memoryMiB"),
		"readiness":  object(map[string]any{"protocol": str, "port": integer}, "protocol", "port"),
		"networking": map[string]any{"type": "string", "description": "Use public for the single HTTP application service exposed by the current node runtime."},
	}, "name", "kind", "isolation", "instances", "resources", "readiness")
	system := object(map[string]any{
		"apiVersion": map[string]any{"type": "string", "const": sdk.APIVersion},
		"kind":       map[string]any{"type": "string", "const": "System"},
		"metadata":   object(map[string]any{"name": str}, "name"),
		"spec": object(map[string]any{
			"intent":      str,
			"constraints": object(map[string]any{"host": object(map[string]any{"class": hostClass, "count": integer, "memoryMiB": integer, "systemReserveMiB": integer}, "class", "count", "memoryMiB", "systemReserveMiB")}, "host"),
			"services":    map[string]any{"type": "array", "minItems": 1, "items": service},
			"m1": object(map[string]any{
				"prefix": map[string]any{"type": "string", "description": "Optional safe client suggestion; the control plane replaces it with a workspace-scoped server-derived prefix."},
			}),
		}, "intent", "constraints", "services"),
	}, "apiVersion", "kind", "metadata", "spec")
	initialProposal := object(map[string]any{
		"summary":        str,
		"system":         system,
		"artifactSha256": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
		"release": object(map[string]any{
			"command":     map[string]any{"type": "array", "minItems": 1, "items": str, "description": "argv: command[0] must be a ./-prefixed executable file in the tar.gz; remaining entries are exact arguments; no shell"},
			"environment": map[string]any{"type": "object", "additionalProperties": str},
			"healthPath":  str,
			"publicPort":  integer,
		}, "command", "healthPath", "publicPort"),
		"verification": object(map[string]any{
			"method": map[string]any{"type": "string", "const": "GET"}, "path": str, "expectedStatus": integer, "bodyContains": str,
		}, "method", "path", "expectedStatus"),
	}, "summary", "system", "artifactSha256", "release", "verification")
	var changeRequest map[string]any
	if err := json.Unmarshal(sdk.ChangeRequestSchemaJSON(), &changeRequest); err != nil {
		changeRequest = map[string]any{"type": "object"}
	}
	return []mcpTool{
		{Name: "canter_whoami", Description: "Return the authenticated human or durable agent installation and current session.", InputSchema: object(nil)},
		{Name: "canter_bootstrap", Description: "Reconstruct the current durable workspace state without relying on conversation history.", InputSchema: object(map[string]any{"workspaceId": str})},
		{Name: "canter_list_changes", Description: "List durable Changes in a workspace.", InputSchema: object(map[string]any{"workspaceId": str}, "workspaceId")},
		{Name: "canter_inspect_system", Description: "Inspect a System's declared contract, deterministic graph, bindings, and observed state.", InputSchema: object(map[string]any{"workspaceId": str, "system": str}, "workspaceId", "system")},
		{Name: "canter_draft_change", Description: "Draft a governed release or typed application replica Change through the real Canter engine. Replica targets are validated against existing host capacity; no provider resources are exposed or silently created. This never authorizes or applies it.", InputSchema: object(map[string]any{"workspaceId": str, "system": str, "request": changeRequest}, "workspaceId", "system", "request")},
		{Name: "canter_inspect_change", Description: "Inspect a durable Change, its exact digest, authorization, operation ledger, and evidence.", InputSchema: object(map[string]any{"workspaceId": str, "system": str, "changeId": str}, "workspaceId", "system", "changeId")},
		{Name: "canter_inspect_change_execution", Description: "Inspect the durable execution that was enqueued for a Change, including its stable ID, requester, attempts, phase, and timestamps.", InputSchema: object(map[string]any{"workspaceId": str, "system": str, "changeId": str}, "workspaceId", "system", "changeId")},
		{Name: "canter_list_standing_policies", Description: "List the human-authored standing policy envelopes and their revocation or expiry state for a System. Agents cannot create or widen policies.", InputSchema: object(map[string]any{"workspaceId": str, "system": str}, "workspaceId", "system")},
		{Name: "canter_apply_change_under_policy", Description: "Evaluate one exact drafted Change digest against active human-authored standing policies. If a policy matches, Canter authorizes and queues it under the immutable policy record; otherwise nothing is authorized and the result requires human approval.", InputSchema: object(map[string]any{"workspaceId": str, "system": str, "changeId": str, "digest": str}, "workspaceId", "system", "changeId", "digest")},
		{Name: "canter_request_change_approval", Description: "Request a ten-minute, single-use human review URL bound to one exact drafted Change digest. The URL grants no agent authorization and must be shown only to the human who will review it.", InputSchema: object(map[string]any{"workspaceId": str, "system": str, "changeId": str, "digest": str}, "workspaceId", "system", "changeId", "digest")},
		{Name: "canter_upload_artifact", Description: "Upload a base64 tar.gz application bundle through Canter into durable content-addressed storage. Provider credentials are never returned.", InputSchema: object(map[string]any{"workspaceId": str, "filename": str, "contentType": str, "dataBase64": map[string]any{"type": "string", "contentEncoding": "base64"}}, "workspaceId", "filename", "dataBase64")},
		{Name: "canter_draft_initial_deployment", Description: "Draft an immutable governed proposal for a System's first real deployment. This never provisions or publishes anything.", InputSchema: object(map[string]any{"workspaceId": str, "proposal": initialProposal}, "workspaceId", "proposal")},
		{Name: "canter_list_initial_deployments", Description: "List governed first-deployment proposals in a workspace.", InputSchema: object(map[string]any{"workspaceId": str}, "workspaceId")},
		{Name: "canter_inspect_initial_deployment", Description: "Inspect a first-deployment proposal, exact digest, human authorization, operations, and evidence.", InputSchema: object(map[string]any{"workspaceId": str, "deploymentId": str}, "workspaceId", "deploymentId")},
		{Name: "canter_inspect_initial_deployment_execution", Description: "Inspect server-owned execution state for a first deployment.", InputSchema: object(map[string]any{"executionId": str}, "executionId")},
	}
}

type mcpArguments struct {
	WorkspaceID  string          `json:"workspaceId"`
	System       string          `json:"system"`
	ChangeID     string          `json:"changeId"`
	Digest       string          `json:"digest"`
	Request      json.RawMessage `json:"request"`
	DeploymentID string          `json:"deploymentId"`
	ExecutionID  string          `json:"executionId"`
	Filename     string          `json:"filename"`
	ContentType  string          `json:"contentType"`
	DataBase64   string          `json:"dataBase64"`
	Proposal     json.RawMessage `json:"proposal"`
}

func (h *HTTPServer) callMCPTool(r *http.Request, p Principal, name string, raw json.RawMessage) (any, error) {
	var args mcpArguments
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	switch name {
	case "canter_whoami":
		return map[string]any{"actor": p.Actor, "account": p.Account, "installation": p.Installation, "session": p.Session}, nil
	case "canter_bootstrap":
		if p.Installation != nil {
			return h.service.Bootstrap(r.Context(), p)
		}
		if args.WorkspaceID == "" {
			return nil, fmt.Errorf("workspaceId is required for a human session")
		}
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		workspace, err := h.service.Store.Workspace(r.Context(), args.WorkspaceID)
		if err != nil {
			return nil, err
		}
		systems, err := h.service.Store.ListSystems(r.Context(), args.WorkspaceID)
		if err != nil {
			return nil, err
		}
		changes, err := h.service.Store.ListChanges(r.Context(), args.WorkspaceID)
		if err != nil {
			return nil, err
		}
		deployments, err := h.service.Store.ListInitialDeployments(r.Context(), args.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"protocolVersion": "v1", "workspace": workspace, "systems": systems, "changes": changes, "initialDeployments": deployments, "capabilities": initialDeploymentCapabilities(args.WorkspaceID), "incidents": []any{}}, nil
	case "canter_list_changes":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		changes, err := h.service.Store.ListChanges(r.Context(), args.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"changes": changes}, nil
	case "canter_inspect_system":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		return h.service.InspectSystem(r.Context(), args.WorkspaceID, args.System)
	case "canter_draft_change":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, true); err != nil {
			return nil, err
		}
		if p.Installation != nil && !p.Installation.Authority.Draft {
			return nil, ErrForbidden
		}
		var request sdk.ChangeRequest
		if err := json.Unmarshal(args.Request, &request); err != nil {
			return nil, fmt.Errorf("invalid Change request: %w", err)
		}
		change, err := h.service.DraftChange(r.Context(), args.WorkspaceID, args.System, request, p.Actor)
		return publicChange(change), err
	case "canter_inspect_change":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		return h.service.InspectChangeWithExecution(r.Context(), args.WorkspaceID, args.System, args.ChangeID)
	case "canter_inspect_change_execution":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		return h.service.Store.ExecutionForChange(r.Context(), args.WorkspaceID, args.System, args.ChangeID)
	case "canter_list_standing_policies":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		policies, err := h.service.Store.ListStandingPolicies(r.Context(), args.WorkspaceID, args.System)
		if err != nil {
			return nil, err
		}
		return map[string]any{"policies": policies}, nil
	case "canter_apply_change_under_policy":
		if p.Installation == nil || p.Session == nil {
			return nil, ErrForbidden
		}
		if err := h.allowWorkspace(r, p, args.WorkspaceID, true); err != nil {
			return nil, err
		}
		result, err := h.service.ApplyChangeUnderPolicy(r.Context(), args.WorkspaceID, args.System, args.ChangeID, args.Digest, p)
		return publicPolicyApplyResult(result), err
	case "canter_request_change_approval":
		if p.Installation == nil || p.Session == nil {
			return nil, ErrForbidden
		}
		if err := h.allowWorkspace(r, p, args.WorkspaceID, true); err != nil {
			return nil, err
		}
		if !p.Installation.Authority.Draft {
			return nil, ErrForbidden
		}
		capability, err := h.service.Store.CreateChangeApprovalCapability(r.Context(), args.WorkspaceID, args.System, args.ChangeID, args.Digest, p, h.config.PublicURL)
		if err != nil {
			return nil, err
		}
		_ = h.service.Store.Audit(r.Context(), args.WorkspaceID, p.Actor, "change.approval-capability.created", capability.ID, map[string]any{"system": args.System, "changeId": args.ChangeID, "digest": args.Digest, "expiresAt": capability.ExpiresAt})
		return capability, nil
	case "canter_upload_artifact":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, true); err != nil {
			return nil, err
		}
		if p.Installation != nil && !p.Installation.Authority.Draft {
			return nil, ErrForbidden
		}
		data, err := base64.StdEncoding.DecodeString(args.DataBase64)
		if err != nil {
			return nil, fmt.Errorf("dataBase64 is invalid: %w", err)
		}
		if len(data) == 0 || len(data) > 64<<20 {
			return nil, fmt.Errorf("artifact must be between 1 byte and 64 MiB")
		}
		return h.service.UploadDeploymentArtifact(r.Context(), args.WorkspaceID, data, args.Filename, args.ContentType, p.Actor)
	case "canter_draft_initial_deployment":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, true); err != nil {
			return nil, err
		}
		if p.Installation != nil && !p.Installation.Authority.Draft {
			return nil, ErrForbidden
		}
		var input DraftInitialDeploymentInput
		if err := json.Unmarshal(args.Proposal, &input); err != nil {
			return nil, fmt.Errorf("invalid initial deployment proposal: %w", err)
		}
		deployment, err := h.service.DraftInitialDeployment(r.Context(), args.WorkspaceID, input, p.Actor)
		return publicInitialDeployment(deployment), err
	case "canter_list_initial_deployments":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		items, err := h.service.Store.ListInitialDeployments(r.Context(), args.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"initialDeployments": items}, nil
	case "canter_inspect_initial_deployment":
		if err := h.allowWorkspace(r, p, args.WorkspaceID, false); err != nil {
			return nil, err
		}
		deployment, err := h.service.Store.InitialDeployment(r.Context(), args.WorkspaceID, args.DeploymentID)
		return publicInitialDeployment(deployment), err
	case "canter_inspect_initial_deployment_execution":
		execution, err := h.service.Store.InitialDeploymentExecution(r.Context(), args.ExecutionID)
		if err != nil {
			return nil, err
		}
		if err := h.allowWorkspace(r, p, execution.WorkspaceID, false); err != nil {
			return nil, err
		}
		return execution, nil
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func mcpToolResult(value any, isError bool) map[string]any {
	raw, _ := json.Marshal(value)
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}, "structuredContent": value, "isError": isError}
}

func (h *HTTPServer) mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (h *HTTPServer) mcpError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
