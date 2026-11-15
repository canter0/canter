package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
)

func wrenJobTools() []mcpTool {
	str := map[string]any{"type": "string"}
	schema := func(p map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": p, "required": required, "additionalProperties": false}
	}
	return []mcpTool{
		{Name: "canter_ask_room_wren", Description: "Pass a bounded part of the person's current request to another named Wren already in this room. Use when that teammate should own the next stage or review your findings. Include the actual findings, desired outcome and limits in prompt; never send credentials or private DM context. The receiving Wren posts its own answer here after earlier contributions finish. Each Wren contributes at most once per request, up to four total; no automatic back-and-forth loops. This preserves the original person's permissions and grants no new access. Returns saved queued status, not a completed handoff. Finish your own reply after assigning the work.", InputSchema: schema(map[string]any{"wrenId": str, "prompt": str}, "wrenId", "prompt")},
		{Name: "canter_delegate_job", Description: "Start a real, bounded temporary worker for a specific part of this Wren's work. The worker has a separate private execution thread and receives only your explicit brief. Use for research, drafting, analysis, or code work that can run while you stay available. Provide context, concrete outcome and acceptance checks without credentials. Returns a durable job ID and actual queued/running state; do not call queued work completed. The result returns automatically to this conversation. No further delegation from a worker or completion callback. Computer/code tools are available only with their actual existing grants.", InputSchema: schema(map[string]any{"projectIds": map[string]any{"type": "array", "items": str, "maxItems": 8, "description": "Explicitly pass only attached project knowledge needed by this worker. Omit for no project access; private memories never inherit."}, "title": str, "prompt": str, "executor": map[string]any{"type": "string", "enum": []string{"operator", "acp-opencode"}, "description": "Use acp-opencode for an actual coding agent on an already-ready computer; otherwise operator."}}, "title", "prompt")},
		{Name: "canter_steer_job", Description: "Forward a correction to this conversation’s active operator worker at its next model checkpoint. It cannot interrupt an external operation already submitted. ACP coding workers do not support concurrent steering: stop and wait for confirmed exit before a corrected delegation. Never report accepted as already applied.", InputSchema: schema(map[string]any{"id": str, "prompt": str}, "id", "prompt")},
		{Name: "canter_inspect_job", Description: "Read a delegated worker's actual status, result or failure in this conversation. Use the job_ ID returned by canter_delegate_job, including external coding workers; computer command IDs are inspected with canter_computer_job.", InputSchema: schema(map[string]any{"id": str}, "id")},
		{Name: "canter_list_jobs", Description: "Read this conversation's delegated jobs and their actual progress.", InputSchema: schema(map[string]any{})},
		{Name: "canter_cancel_job", Description: "Stop this conversation's delegated job and suppress automatic follow-up. Completed external operations keep their own state.", InputSchema: schema(map[string]any{"id": str}, "id")},
	}
}
func (o *OperatorRuntime) wrenJobTool(ctx context.Context, run OperatorRun, c Conversation, name string, raw json.RawMessage) (any, bool, error) {
	if name == "canter_ask_room_wren" {
		value, err := o.askRoomWrenTool(ctx, run, c, raw)
		return value, true, err
	}
	var in struct {
		ID, Title, Prompt, OperationKey, Executor string
		ProjectIDs                                []string
	}
	if name != "canter_delegate_job" && name != "canter_inspect_job" && name != "canter_list_jobs" && name != "canter_cancel_job" && name != "canter_steer_job" {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, true, err
	}
	s := o.Server.service.Store
	if name == "canter_delegate_job" {
		if in.Executor == "acp-opencode" {
			j, err := o.Server.CreateHarnessWrenJob(ctx, run, c, in.OperationKey, in.Title, in.Prompt, in.ProjectIDs)
			return j, true, err
		}
		if in.Executor != "" && in.Executor != "operator" {
			return nil, true, ErrForbidden
		}
		j, err := s.CreateWrenJob(ctx, run, c, in.OperationKey, in.Title, in.Prompt, o.Config.wrenModel(), in.ProjectIDs)
		return j, true, err
	}
	if name == "canter_list_jobs" {
		v, err := s.WrenJobs(ctx, c.WorkspaceID, c.AccountID, c.ID)
		return v, true, err
	}
	j, err := s.WrenJob(ctx, c.WorkspaceID, c.AccountID, in.ID)
	if err != nil {
		return nil, true, err
	}
	if j.OriginConversationID != c.ID {
		return nil, true, ErrForbidden
	}
	if name == "canter_steer_job" {
		input, err := s.SteerWrenJob(ctx, c.WorkspaceID, c.AccountID, in.ID, in.OperationKey, in.Prompt)
		return map[string]any{"input": input, "accepted": err == nil, "delivery": "next_model_checkpoint", "interrupted": false}, true, err
	}
	if name == "canter_cancel_job" {
		j, err = s.CancelWrenJob(ctx, c.WorkspaceID, c.AccountID, in.ID)
	}
	return j, true, err
}
func (h *HTTPServer) workspaceWrenJobs(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		v, err := h.service.Store.WrenJobs(r.Context(), workspace, p.Account.ID, r.URL.Query().Get("conversationId"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"jobs": v})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		v, err := h.service.Store.WrenJob(r.Context(), workspace, p.Account.ID, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if len(parts) == 2 && parts[1] == "steer" && r.Method == http.MethodPost {
		var input struct {
			OperationKey string `json:"operationKey"`
			Content      string `json:"content"`
		}
		if !decodeLimit(w, r, &input, 12<<10) {
			return
		}
		v, err := h.service.Store.SteerWrenJob(r.Context(), workspace, p.Account.ID, parts[0], input.OperationKey, input.Content)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"input": v, "accepted": true, "delivery": "next_model_checkpoint", "interrupted": false})
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost {
		v, err := h.service.Store.CancelWrenJob(r.Context(), workspace, p.Account.ID, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	methodNotAllowed(w)
}
