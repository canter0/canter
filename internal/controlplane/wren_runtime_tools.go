package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Re-read identity and control at each model boundary. This is runtime state, not a
// remembered tool result or an instruction embedded in somebody's screen.
// Keep it out of the durable transcript so returning control removes it.
func (o *OperatorRuntime) wrenComputerRuntimeNotice(ctx context.Context, c Conversation, runID string) (string, error) {
	origin, _, err := o.wrenWorkOrigin(ctx, c)
	if err != nil {
		return "", err
	}
	v, err := scanWrenComputer(o.Server.service.Store.pool.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND conversation_id=$2 AND phase='ready' AND NOT delete_requested AND expires_at>now()`, origin.WorkspaceID, origin.ID))
	if errors.Is(err, ErrNotFound) {
		return "Current computer context: no ready computer is registered for this conversation. Earlier computer observations do not establish current files, processes or browser state. Use canter_computer_status when the request needs computer access; independent chat and saved-artifact work can continue.", nil
	}
	if err != nil {
		return "", err
	}
	state, err := json.Marshal(struct {
		ID        string `json:"computerId"`
		Scope     string `json:"scope"`
		CreatedAt string `json:"createdAt"`
		Root      string `json:"workspaceRoot"`
		Restored  bool   `json:"restoredSavedFiles"`
	}{v.ID, v.ScopeKind, v.CreatedAt.UTC().Format(time.RFC3339Nano), "/workspace", v.RestoreCheckpointID != ""})
	if err != nil {
		return "", err
	}
	notice := fmt.Sprintf("Current computer context (re-read from runtime): %s\nFile paths, command IDs and browser observations belong to the computer that produced them. Results from another computer, or from before this computer's creation, are historical. Saved memories, project notes and chat messages do not mount their mentioned files here. Before reusing an old project directory, inspect it on THIS computer with canter_computer_files. If no working directory is requested or verified, omit directory to use /workspace. Do not silently recreate a missing project directory or claim its old contents were restored. Restored saved files do not resume running processes; inspect actual files before relying on them. This context does not grant permission or establish guest reachability. Keep computer IDs and infrastructure details internal unless needed to explain a problem.", state)
	if !o.Server.activeComputerHumanControl(ctx, v) {
		return notice, nil
	}
	// Use this run's durable receipts, not remembered prose from earlier turns.
	// Project only bounded input metadata: never elevate typed text, URLs, or
	// screen contents into this trusted control-state instruction.
	var receipts []byte
	err = o.Server.service.Store.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object(
		'action',t.result->'action','amount',t.result->'amount','submitted',true,
		'outcomeVerified',false) ORDER BY t.created_at,t.call_id),'[]'::jsonb)
		FROM operator_tool_calls t JOIN operator_runs r ON r.id=t.run_id
		WHERE r.id=$1 AND r.conversation_id=$2 AND t.name='canter_computer_input'
		AND t.result->'submitted'='true'::jsonb AND t.result->>'computerId'=$3`, runID, c.ID, v.ID).Scan(&receipts)
	if err != nil {
		return "", err
	}
	return notice + "\n" + fmt.Sprintf("Current computer control: a person has taken over this conversation's computer. You cannot operate it. Do not poll screenshots or status waiting for control, and do not bypass takeover with shell commands, delegated work, or another input path. If this request needs further computer actions, end your reply now with the progress actually verified and say you have handed control over. Resume computer actions only on a new request after control returns. A specifically requested read-only observation may be answered once; unrelated work can continue. Taking over does not establish that arbitrary background processes stopped.\nAccepted computer input receipts for THIS request, in order: %s\nThis list is exhaustive for submitted computer_input calls in this request. Count only these entries; do not add intended, rejected, pending, or earlier-request actions. A receipt proves input submission, not a visible outcome. Describe screen outcomes only when supported by an actual screenshot from this request.", receipts), nil
}

// This is an affordance filter, not an authorization grant. Every execution
// still checks the live scope, lease and dependencies immediately before use.
func (o *OperatorRuntime) wrenConversationTools(ctx context.Context, c Conversation, tools []mcpTool) ([]mcpTool, error) {
	origin, jobID, err := o.wrenWorkOrigin(ctx, c)
	if err != nil {
		return nil, err
	}
	ready, writable := false, false
	// NewSSH validates scoped credentials locally; it does not dial the guest.
	// Missing/broken compute must not prevent independent inline content work.
	if o.Server.config.Secrets != nil {
		client, computer, e := o.Server.ClientForConversation(ctx, origin)
		if e == nil && client != nil && computer.Phase == "ready" {
			ready = true
			writable = o.Server.ComputerAgentWriteAllowed(ctx, origin) == nil
		}
	}
	externalQueue, err := o.Server.service.Store.hasExternalTaskAgent(ctx, c.WorkspaceID, "")
	if err != nil {
		return nil, err
	}
	acp := ready && writable && o.Server.HarnessConfigured()
	var teammates []Wren
	if strings.HasPrefix(c.ID, "conv_room_") && jobID == "" {
		room, e := o.Server.service.Store.Room(ctx, c.WorkspaceID, c.AccountID, strings.TrimPrefix(c.ID, "conv_"))
		if e != nil {
			return nil, e
		}
		teammates = room.Wrens
	}
	out := make([]mcpTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "canter_ask_room_wren" {
			if len(teammates) < 2 {
				continue
			}
			tool = cloneWrenToolSchema(tool)
			ids, names := []string{}, []string{}
			for _, w := range teammates {
				ids = append(ids, w.ID)
				names = append(names, w.Name+" = "+w.ID)
			}
			tool.InputSchema["properties"].(map[string]any)["wrenId"] = map[string]any{"type": "string", "enum": ids, "description": strings.Join(names, "; ")}
		}
		if tool.Name == "canter_create_task" && !externalQueue {
			continue
		}
		if jobID != "" && (tool.Name == "canter_prepare_message" || tool.Name == "canter_read_inbox" || strings.HasPrefix(tool.Name, "canter_list_message_")) {
			continue
		}
		if strings.HasPrefix(tool.Name, "canter_computer_") && tool.Name != "canter_computer_status" {
			if !ready {
				continue
			}
			policy, known := operatorPolicy(tool.Name)
			if !writable && (!known || policy.Effect != "read") {
				continue
			}
		}
		// The current media tool deliberately requires its accepted lead/routine
		// run; a temporary child cannot authorize image generation itself.
		if jobID != "" && tool.Name == "canter_generate_image" {
			continue
		}
		if tool.Name == "canter_delegate_job" && !acp {
			tool = cloneWrenToolSchema(tool)
			tool.Description += " The external ACP coding executor is unavailable in this conversation. If specifically requested, explain that limitation; do not substitute an unstarted external queue task or a different executor without the user choosing it."
			if properties, ok := tool.InputSchema["properties"].(map[string]any); ok {
				if executor, ok := properties["executor"].(map[string]any); ok {
					executor["enum"] = []string{"operator"}
					executor["description"] = "Use operator for a worker with the capabilities currently available in this conversation."
				}
			}
		}
		if tool.Name == "canter_publish_artifact" && !ready {
			tool = cloneWrenToolSchema(tool)
			if properties, ok := tool.InputSchema["properties"].(map[string]any); ok {
				delete(properties, "path")
			}
			tool.InputSchema["required"] = []string{"name", "mediaType", "content"}
			tool.Description = "Save nonempty UTF-8 content as a reusable output with an authenticated download in this conversation. Use a real text format such as CSV, Markdown, HTML or JSON. Supply content directly; this conversation has no ready computer file path. Do not label text as an Excel workbook, PDF or Word document. Existing edits require id and expectedRevision. Worker outputs publish to the origin conversation; open their existing artifact IDs instead of duplicating them."
		}
		out = append(out, tool)
	}
	return out, nil
}
func cloneWrenToolSchema(tool mcpTool) mcpTool {
	raw, _ := json.Marshal(tool.InputSchema)
	var schema map[string]any
	_ = json.Unmarshal(raw, &schema)
	tool.InputSchema = schema
	return tool
}
