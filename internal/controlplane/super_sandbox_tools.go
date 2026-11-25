package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/canter0/canter/sdk"
)

func superSandboxTools() []mcpTool {
	str := map[string]any{"type": "string"}
	name := map[string]any{"type": "string", "minLength": 1, "maxLength": 100}
	bounded := func(min, max int) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "maximum": max}
	}
	object := func(properties map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []mcpTool{
		{Name: "canter_super_sandbox_create",
			Description: "Create one durable organization batch over 1-16 explicit EXISTING ready sandbox worker IDs. Prepare workers separately with sandbox tools. Tasks use worker-local files, not shared RAM or a shared filesystem. Requires a linked human owner/operator. maxConcurrency is bounded by worker count. maxWorkerSeconds is a conservative sum-of-task-timeouts execution allocation, NOT a financial/VM billing cap; existing VM billing continues independently. All task timeouts (default 300, maximum 3600 seconds) must fit the allocation up front. Supply an idempotencyKey; same normalized payload returns the same batch, a changed payload conflicts. Never put secrets in commands. Results reference existing sandbox job/log endpoints. Workers must have completed setup. Recipe env and secretRefs are not inherited; workers configured with either are rejected.",
			InputSchema: object(map[string]any{
				"workspaceId": str, "name": name,
				"workerIds":      map[string]any{"type": "array", "items": str, "minItems": 1, "maxItems": 16, "uniqueItems": true},
				"maxConcurrency": bounded(1, 16), "maxWorkerSeconds": bounded(1, 86400),
				"idempotencyKey": map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
				"tasks": map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "items": object(map[string]any{
					"name": name, "command": map[string]any{"type": "string", "minLength": 1, "maxLength": 64000},
					"directory": str, "timeoutSeconds": bounded(1, 3600),
				}, "name", "command")},
			}, "name", "workerIds", "maxConcurrency", "maxWorkerSeconds", "idempotencyKey", "tasks")},
		{Name: "canter_super_sandbox_list", Description: "List durable Super Sandboxes in this organization; passive observation never dispatches work.",
			InputSchema: object(map[string]any{"workspaceId": str})},
		{Name: "canter_super_sandbox_inspect", Description: "Inspect a durable batch, tasks, worker IDs and existing job IDs. Unknown/needs_attention is not completion; uncertain jobs retain their worker reservation.",
			InputSchema: object(map[string]any{"workspaceId": str, "id": str}, "id")},
		{Name: "canter_super_sandbox_cancel", Description: "Request cancellation of this batch's jobs only. Unstarted tasks are cancelled and owned active job IDs are stopped asynchronously. Cancellation is not complete until positive terminal receipts; it never deletes or sleeps workers and does not undo effects or stop VM billing.",
			InputSchema: object(map[string]any{"workspaceId": str, "id": str}, "id")},
	}
}

func (h *HTTPServer) callSuperSandboxTool(ctx context.Context, workspace, account string, actor sdk.ActorRef, name string, raw json.RawMessage) (any, error) {
	if len(raw) > 256<<10 {
		return nil, fmt.Errorf("%w: Super Sandbox input exceeds 256 KiB", ErrConflict)
	}
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	// The Operator's conversation binding is not a durable read grant after
	// membership withdrawal. Installation membership is resolved by MCP.
	if account != "" {
		if _, err := workspaceRole(ctx, h.service.Store.pool, workspace, account, false); err != nil {
			return nil, ErrForbidden
		}
	}
	// Deliberately ignore any workspaceId in the payload: the caller resolved
	// membership and bound the workspace (especially the Operator conversation).
	switch name {
	case "canter_super_sandbox_create":
		var input SuperSandboxCreateInput
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		return h.CreateSuperSandbox(ctx, workspace, account, actor, input)
	case "canter_super_sandbox_list":
		items, err := h.service.Store.SuperSandboxes(ctx, workspace)
		return map[string]any{"superSandboxes": items, "enabled": h.config.Sandboxes.Enabled}, err
	case "canter_super_sandbox_inspect":
		return h.service.Store.SuperSandbox(ctx, workspace, args.ID)
	case "canter_super_sandbox_cancel":
		return h.CancelSuperSandbox(ctx, workspace, account, args.ID, actor)
	}
	return nil, ErrNotFound
}
