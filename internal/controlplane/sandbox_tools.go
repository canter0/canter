package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

const sandboxExecOutputLimit = 64 << 10
const sandboxFileWriteLimit = 1 << 20
const sandboxFileReadLimit = 256 << 10

// workspaceSandboxes serves /v1/workspaces/{id}/sandboxes. Humans (any member)
// read; owner/operator write. Installations use the MCP tools with their
// workspace write grant.
func (h *HTTPServer) workspaceSandboxes(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	account := ""
	if p.Account != nil {
		account = p.Account.ID
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			items, err := h.service.Store.Sandboxes(r.Context(), workspace)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			used, _ := h.service.Store.SandboxFreeUsage(r.Context(), h.service.Store.pool, workspace, h.service.Store.now())
			writeJSON(w, http.StatusOK, map[string]any{
				"sandboxes": items, "enabled": h.config.Sandboxes.Enabled,
				"previewsEnabled": h.config.Sandboxes.PreviewDomain != "",
				"freeUsage":       map[string]any{"usedSeconds": used, "limitSeconds": h.config.Sandboxes.freeSecondsPerMonth()},
				"sizes":           sandboxSizeList(),
				"limits": map[string]any{
					"maxCommandSeconds": 86400, "maxFileBytes": int64(4) << 30,
					"uploadChunkBytes": 4 << 20, "defaultIdleSeconds": int(h.config.Sandboxes.idleSleep().Seconds()),
					"maxJobLogBytes": 32 << 20,
				},
			})
		case http.MethodPost:
			if account == "" && p.Installation == nil {
				writeStoreError(w, ErrForbidden)
				return
			}
			var input SandboxCreateInput
			if !decodeLimit(w, r, &input, 192<<10) {
				return
			}
			if header := strings.TrimSpace(r.Header.Get("Idempotency-Key")); header != "" {
				if input.IdempotencyKey != "" && input.IdempotencyKey != header {
					writeStoreError(w, fmt.Errorf("%w: Idempotency-Key header and body key differ", ErrConflict))
					return
				}
				input.IdempotencyKey = header
			}
			if err := h.allowWorkspace(r, p, workspace, true); err != nil {
				writeStoreError(w, err)
				return
			}
			v, err := h.CreateSandbox(r.Context(), workspace, account, p.Actor, input)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, v)
		default:
			methodNotAllowed(w)
		}
		return
	}
	v, err := h.service.Store.Sandbox(r.Context(), workspace, parts[0])
	if err != nil {
		writeStoreError(w, err)
		return
	}
	write := h.allowWorkspace(r, p, workspace, true) == nil
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, v)
		case http.MethodPatch:
			if !write {
				writeStoreError(w, ErrForbidden)
				return
			}
			var input struct {
				Name               *string           `json:"name"`
				Labels             map[string]string `json:"labels"`
				IdleTimeoutSeconds *int              `json:"idleTimeoutSeconds"`
			}
			if !decodeLimit(w, r, &input, 8192) {
				return
			}
			updated, err := h.sandboxPatch(r.Context(), v, input.Name, input.Labels, input.IdleTimeoutSeconds, p.Actor)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, updated)
		case http.MethodDelete:
			if !write {
				writeStoreError(w, ErrForbidden)
				return
			}
			if err = h.RequestSandboxDelete(r.Context(), workspace, v.ID, p.Actor); err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"deleting": true})
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) >= 2 && parts[1] == "terminals" {
		// Interactive terminal: owner/operator humans only.
		if p.Installation != nil || p.Account == nil || !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		h.sandboxTerminals(w, r, v, *p.Account, parts[1:])
		return
	}
	if len(parts) >= 2 && parts[1] == "previews" {
		h.sandboxPreviews(w, r, v, p, write, parts[1:])
		return
	}
	if len(parts) >= 2 && parts[1] == "desktop" {
		h.workspaceSandboxDesktop(w, r, p, v, write, parts[2:])
		return
	}
	if len(parts) == 2 && parts[1] == "restore" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		var input SandboxSnapshotSelection
		if !decodeLimit(w, r, &input, 8192) {
			return
		}
		updated, err := h.SelectSandboxSnapshot(r.Context(), workspace, account, v.ID, p.Actor, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 2 && (parts[1] == "sleep" || parts[1] == "wake") && r.Method == http.MethodPost {
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		if parts[1] == "sleep" {
			err = h.RequestSandboxSleep(r.Context(), workspace, v.ID, p.Actor)
		} else {
			err = h.RequestSandboxWake(r.Context(), workspace, v.ID, account, p.Actor)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"phase": parts[1]})
		return
	}
	if len(parts) == 2 && parts[1] == "exec" && r.Method == http.MethodPost {
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		var input SandboxExecInput
		if !decodeLimit(w, r, &input, 96<<10) {
			return
		}
		out, err := h.sandboxExec(r.Context(), v, input, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(parts) >= 2 && (parts[1] == "jobs" || parts[1] == "metrics" || parts[1] == "uploads") {
		h.sandboxWorkflows(w, r, v, p, write, parts)
		return
	}
	if len(parts) >= 2 && parts[1] == "files" {
		h.sandboxFiles(w, r, v, p, write, parts[1:])
		return
	}
	writeStoreError(w, ErrNotFound)
}

func sandboxJobView(job wrencomputer.Job) map[string]any {
	output := job.Output
	if len(output) > sandboxExecOutputLimit {
		output = output[len(output)-sandboxExecOutputLimit:]
	}
	view := workflowJobView(job)
	view["output"] = output
	view["outputTruncated"] = job.OutputTruncated || len(job.Output) > sandboxExecOutputLimit
	return view
}

// sandboxExec starts a durable job and waits up to 30 seconds for completion.
// An asleep sandbox stays asleep: waking is explicit so cost stays visible.
func (h *HTTPServer) sandboxExec(ctx context.Context, v Sandbox, input SandboxExecInput, actor sdk.ActorRef) (map[string]any, error) {
	job, err := h.startSandboxJob(ctx, v, input, actor, 600, false)
	if err != nil {
		return nil, err
	}
	if sandboxJobDone(job.Status) {
		return sandboxJobView(job), nil
	}
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		return nil, fmt.Errorf("inspect job %s for its result: %w", job.ID, err)
	}
	defer client.Close()
	deadline := time.Now().Add(30 * time.Second)
	for job.Status == "starting" || job.Status == "running" {
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		id := job.ID
		job, err = client.Job(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect job %s for its result: %w", id, err)
		}
	}
	if job.Status != "starting" && job.Status != "running" {
		h.service.Store.finishSandboxJob(ctx, v.ID, job.ID)
	}
	return sandboxJobView(job), nil
}

// sandboxFilePath normalizes a user-supplied file path to /workspace and
// enforces the user-file capability fence (excludes .canter/.browser data).
func sandboxFilePath(raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		target = "/workspace"
	}
	if !strings.HasPrefix(target, "/") {
		target = "/workspace/" + target
	}
	if !ComputerUserFileAllowed(target) {
		return "", ErrForbidden
	}
	return path.Clean(target), nil
}

func (h *HTTPServer) sandboxFiles(w http.ResponseWriter, r *http.Request, v Sandbox, p Principal, write bool, parts []string) {
	path, err := sandboxFilePath(r.URL.Query().Get("path"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(parts) == 2 && parts[1] == "content" && r.Method == http.MethodGet {
		h.sandboxFileDownload(w, r, v, p, path)
		return
	}
	switch r.Method {
	case http.MethodGet:
		offset, limit := int64(0), sandboxFileReadLimit
		if raw := r.URL.Query().Get("offset"); raw != "" {
			if n, err := strconv.ParseInt(raw, 10, 64); err != nil || n < 0 {
				writeStoreError(w, fmt.Errorf("%w: invalid offset", ErrConflict))
				return
			} else {
				offset = n
			}
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > sandboxFileReadLimit {
				writeStoreError(w, fmt.Errorf("%w: invalid limit", ErrConflict))
				return
			} else {
				limit = n
			}
		}
		client, err := h.sandboxWorkflowClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		defer h.sandboxActivity(r.Context(), v.ID)()
		entries, err := client.ListFiles(r.Context(), path)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": entries})
			return
		}
		var remote *wrencomputer.Error
		if !errors.As(err, &remote) {
			writeStoreError(w, err)
			return
		}
		chunk, err := client.ReadFile(r.Context(), path, offset, limit)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "content": string(chunk.Data), "size": chunk.Size, "offset": chunk.Offset, "nextOffset": chunk.NextOffset, "eof": chunk.EOF})
	case http.MethodPut:
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err := sandboxMutationAllowed(v); err != nil {
			writeStoreError(w, err)
			return
		}
		if r.ContentLength > sandboxFileWriteLimit+4096 {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("file writes are limited to 1 MiB"))
			return
		}
		var input struct {
			Content string `json:"content"`
		}
		if !decodeLimit(w, r, &input, sandboxFileWriteLimit+4096) {
			return
		}
		if len(input.Content) > sandboxFileWriteLimit {
			writeStoreError(w, fmt.Errorf("%w: file writes are limited to 1 MiB", ErrConflict))
			return
		}
		client, err := h.sandboxWorkflowClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		defer h.sandboxActivity(r.Context(), v.ID)()
		chunk, err := client.WriteFile(r.Context(), path, []byte(input.Content))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), v.WorkspaceID, p.Actor, "sandbox.file_written", v.ID, map[string]any{"path": path})
		writeJSON(w, http.StatusOK, map[string]any{"path": chunk.Path, "size": chunk.Size})
	default:
		methodNotAllowed(w)
	}
}

// Agent tools (home agent and connected agents over MCP) share one executor.
// Connected agents reach it through callMCPTool where allowWorkspace gates
// write; the home agent passes the conversation's requester account.
var sandboxToolDefs = append([]mcpTool{
	{Name: "canter_sandbox_create", Description: "Create a private Linux sandbox. Sizes: free-1x1, s-1x2, s-2x4, s-2x8, s-4x16. Free sandboxes share 50 awake-hours monthly and have mandatory idle sleep. Paid sizes require billing. Optionally supply snapshotId OR templateId, with templateVersion for a chosen template version; the destination disk must fit the save. A free sandbox's current save can be copied without history billing; other sources require billing. Labels and paid idleTimeoutSeconds configure the sandbox. Reuse idempotencyKey with identical inputs for a safe creation retry. Enable Sandboxes first. Set desktop=true for a graphical desktop on at least 2 vCPUs and 4096 MiB RAM. Desktop access does not grant root.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "name": map[string]string{"type": "string"}, "size": map[string]string{"type": "string"}, "labels": map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}}, "idleTimeoutSeconds": map[string]any{"type": "integer"}, "idempotencyKey": map[string]string{"type": "string"}, "snapshotId": map[string]any{"type": "string"}, "templateId": map[string]any{"type": "string"}, "templateVersion": map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}, "desktop": map[string]any{"type": "boolean"}, "environment": map[string]any{"type": "object", "additionalProperties": false, "description": "Optional repository and setup recipe for a new sandbox. Setup runs once. Environment values apply to setup and command jobs. secretRefs maps environment names to organization secret IDs explicitly enabled for sandbox use. Restored copies preserve files and do not rerun repository checkout or setup.", "properties": map[string]any{"repository": map[string]any{"type": "object", "additionalProperties": false, "description": "Public HTTPS repository. Pin a commit SHA in ref for a fixed source revision.", "required": []string{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string", "maxLength": 2048}, "ref": map[string]any{"type": "string", "maxLength": 256}}}, "setupCommand": map[string]any{"type": "string", "maxLength": 32768}, "setupTimeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}, "env": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 8192}, "maxProperties": 64}, "secretRefs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 128}, "maxProperties": 64}}}}, "required": []string{"name", "size"}, "additionalProperties": false}},
	{Name: "canter_sandbox_list", Description: "List this organization's sandboxes with phase (queued, ready, asleep, ...), size, creator, and free-hours usage.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}}, "additionalProperties": false}},
	{Name: "canter_sandbox_run", Description: "Run a command in an awake sandbox, waiting up to 30 seconds before returning its job ID and status. Use start for immediate background execution, job/logs to follow it, and stop to cancel. Directory defaults to /workspace; timeout is 1-86400 seconds, default 600. Commands run without root. Install supported system packages with sudo canter-apt install <name>. Never put credentials in shell command text.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "command": map[string]string{"type": "string"}, "directory": map[string]string{"type": "string"}, "env": map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}}, "timeoutSeconds": map[string]any{"type": "integer"}, "secretRefs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 128}, "maxProperties": 64, "description": "Optional environment-name to approved organization secret ID mapping for this job. Omit to inherit the sandbox's saved secret references; use {} to run without saved secrets."}}, "required": []string{"id", "command"}, "additionalProperties": false}},
	{Name: "canter_sandbox_job", Description: "Check a sandbox command's status and output by job id.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "jobId": map[string]string{"type": "string"}}, "required": []string{"id", "jobId"}, "additionalProperties": false}},
	{Name: "canter_sandbox_read_file", Description: "Read a text file or list a directory inside a sandbox.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "path": map[string]string{"type": "string"}}, "required": []string{"id", "path"}, "additionalProperties": false}},
	{Name: "canter_sandbox_write_file", Description: "Write a text file inside a sandbox (up to 1 MiB).", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "path": map[string]string{"type": "string"}, "content": map[string]string{"type": "string"}}, "required": []string{"id", "path", "content"}, "additionalProperties": false}},
	{Name: "canter_sandbox_sleep", Description: "Checkpoint a ready sandbox's files and shut down its VM to stop metered time. Files are restored on wake.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}},
	{Name: "canter_sandbox_wake", Description: "Provision a fresh VM for an asleep sandbox and restore its saved files. Waking resumes free-hour or paid usage.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}},
	{Name: "canter_sandbox_delete", Description: "Permanently delete a sandbox. Kept saves and saves used by another sandbox or template remain available; other saved data is removed after VM deletion is confirmed. This cannot restore the deleted machine.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}},
	{Name: "canter_sandbox_start", Description: "Start a sandbox command and return its job ID immediately. Use canter_sandbox_job or canter_sandbox_logs to observe completion and canter_sandbox_stop to cancel. Directory defaults to /workspace. Timeout is 1-86400 seconds, default 3600. Env values are ordinary process variables; this does not grant access to organization secrets. Never put credentials in shell command text. The sandbox must already be awake.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "command": map[string]string{"type": "string"}, "directory": map[string]string{"type": "string"}, "env": map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}}, "timeoutSeconds": map[string]any{"type": "integer"}, "secretRefs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 128}, "maxProperties": 64, "description": "Optional environment-name to approved organization secret ID mapping for this job. Omit to inherit the sandbox's saved secret references; use {} to run without saved secrets."}}, "required": []string{"id", "command"}, "additionalProperties": false}},
	{Name: "canter_sandbox_stop", Description: "Stop an existing command in this sandbox by its job ID. Inspect the returned status; stopping does not undo work or external effects.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "jobId": map[string]string{"type": "string"}}, "required": []string{"id", "jobId"}, "additionalProperties": false}},
	{Name: "canter_sandbox_logs", Description: "Read recorded command output from a byte offset. Streams are combined, stdout or stderr. Returns text and nextOffset; EOF only means the current log end, so also check job status. Recorded logs have a bounded size.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "jobId": map[string]string{"type": "string"}, "stream": map[string]string{"type": "string"}, "offset": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer"}}, "required": []string{"id", "jobId"}, "additionalProperties": false}},
	{Name: "canter_sandbox_metrics", Description: "Read this running sandbox's CPU load averages, memory usage, disk usage and uptime. CPU load is not a utilization percentage.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}},
	{Name: "canter_sandbox_update", Description: "Rename or label a sandbox, or change its paid idle policy. idleTimeoutSeconds -1 uses the server default, 0 disables idle sleep, and 60-86400 sets an idle timeout. Free sandboxes retain their fixed policy.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "name": map[string]string{"type": "string"}, "labels": map[string]any{"type": "object", "additionalProperties": map[string]string{"type": "string"}}, "idleTimeoutSeconds": map[string]any{"type": "integer"}}, "required": []string{"id"}, "additionalProperties": false}},
	{Name: "canter_sandbox_preview_create", Description: "Share a public preview link to a port (1024-65535) on the sandbox, so a person can open what runs there. Anyone with the link can open this port.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "port": map[string]any{"type": "integer"}}, "required": []string{"id", "port"}, "additionalProperties": false}},
	{Name: "canter_sandbox_preview_list", Description: "List a sandbox's active preview links.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false}},
	{Name: "canter_sandbox_preview_revoke", Description: "Revoke a sandbox preview link so it stops working.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"workspaceId": map[string]string{"type": "string"}, "id": map[string]string{"type": "string"}, "previewId": map[string]string{"type": "string"}}, "required": []string{"id", "previewId"}, "additionalProperties": false}},
}, append(sandboxLibraryTools(), sandboxDesktopTools()...)...)

func sandboxToolByName(name string) (mcpTool, bool) {
	for _, t := range sandboxToolDefs {
		if t.Name == name {
			return t, true
		}
	}
	return mcpTool{}, false
}

var sandboxMutatingTools = map[string]bool{
	"canter_sandbox_create":                  true,
	"canter_sandbox_run":                     true,
	"canter_sandbox_start":                   true,
	"canter_sandbox_stop":                    true,
	"canter_sandbox_update":                  true,
	"canter_sandbox_write_file":              true,
	"canter_sandbox_sleep":                   true,
	"canter_sandbox_wake":                    true,
	"canter_sandbox_delete":                  true,
	"canter_sandbox_preview_create":          true,
	"canter_sandbox_preview_revoke":          true,
	"canter_sandbox_snapshot_update":         true,
	"canter_sandbox_template_create":         true,
	"canter_sandbox_template_version_add":    true,
	"canter_sandbox_template_delete":         true,
	"canter_sandbox_template_version_delete": true,
	"canter_sandbox_restore":                 true,
	"canter_sandbox_input":                   true,
}

// callSandboxTool executes a sandbox tool for the given actor. writeGrant is
// the caller's resolved workspace-write permission (role or installation
// grant); read tools need membership only, which allowWorkspace established.
func (h *HTTPServer) callSandboxTool(ctx context.Context, workspace, account string, actor sdk.ActorRef, writeGrant bool, name string, raw json.RawMessage) (any, error) {
	var args struct {
		ID                 string            `json:"id"`
		JobID              string            `json:"jobId"`
		Name               string            `json:"name"`
		Size               string            `json:"size"`
		Command            string            `json:"command"`
		Directory          string            `json:"directory"`
		Env                map[string]string `json:"env"`
		Labels             map[string]string `json:"labels"`
		IdleTimeoutSeconds *int              `json:"idleTimeoutSeconds"`
		IdempotencyKey     string            `json:"idempotencyKey"`
		Stream             string            `json:"stream"`
		Offset             int64             `json:"offset"`
		Limit              int               `json:"limit"`
		TimeoutSeconds     int               `json:"timeoutSeconds"`
		Port               int               `json:"port"`
		PreviewID          string            `json:"previewId"`
		Path               string            `json:"path"`
		Content            string            `json:"content"`
		SnapshotID         string            `json:"snapshotId"`
		SourceSandboxID    string            `json:"sourceSandboxId"`
		Before             string            `json:"before"`
		Version            int               `json:"version"`
		ExpectedGeneration int               `json:"expectedGeneration"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	if sandboxMutatingTools[name] && !writeGrant {
		return nil, ErrForbidden
	}
	switch name {
	case "canter_sandbox_create":
		var input SandboxCreateInput
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
		return h.CreateSandbox(ctx, workspace, account, actor, input)
	case "canter_sandbox_list":
		items, err := h.service.Store.Sandboxes(ctx, workspace)
		if err != nil {
			return nil, err
		}
		used, _ := h.service.Store.SandboxFreeUsage(ctx, h.service.Store.pool, workspace, h.service.Store.now())
		return map[string]any{"sandboxes": items, "previewsEnabled": h.config.Sandboxes.PreviewDomain != "", "freeUsage": map[string]any{"usedSeconds": used, "limitSeconds": h.config.Sandboxes.freeSecondsPerMonth()}}, nil
	case "canter_sandbox_run":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		return h.sandboxExec(ctx, v, SandboxExecInput{Command: args.Command, Directory: args.Directory, Env: args.Env, TimeoutSeconds: args.TimeoutSeconds}, actor)
	case "canter_sandbox_start":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		job, err := h.sandboxJobStart(ctx, v, SandboxExecInput{Command: args.Command, Directory: args.Directory, Env: args.Env, TimeoutSeconds: args.TimeoutSeconds}, actor)
		if err != nil {
			return nil, err
		}
		return workflowJobView(job), nil
	case "canter_sandbox_stop":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		job, err := h.stopSandboxJob(ctx, v, args.JobID, actor)
		if err != nil {
			return nil, err
		}
		return workflowJobView(job), nil
	case "canter_sandbox_logs":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		if !h.service.Store.sandboxJobKnown(ctx, v.ID, args.JobID) {
			return nil, ErrNotFound
		}
		stream := args.Stream
		if stream == "" {
			stream = "combined"
		}
		limit := args.Limit
		if limit == 0 {
			limit = 65536
		}
		if args.Offset < 0 || limit < 1 || limit > 262144 || (stream != "combined" && stream != "stdout" && stream != "stderr") {
			return nil, fmt.Errorf("%w: invalid log range or stream", ErrConflict)
		}
		client, err := h.sandboxWorkflowClient(ctx, v)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		chunk, err := client.JobLogs(ctx, args.JobID, stream, args.Offset, limit)
		if err != nil {
			return nil, err
		}
		if err := validateSandboxLogChunk(chunk, args.JobID, stream, args.Offset, limit); err != nil {
			return nil, err
		}
		if sandboxJobDone(chunk.Status) {
			h.service.Store.finishSandboxJob(ctx, v.ID, args.JobID)
		}
		return map[string]any{"jobId": chunk.JobID, "stream": chunk.Stream, "text": string(chunk.Data), "offset": chunk.Offset, "nextOffset": chunk.NextOffset, "eof": chunk.EOF, "status": chunk.Status, "logTruncated": chunk.LogTruncated}, nil
	case "canter_sandbox_metrics":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		client, err := h.sandboxWorkflowClient(ctx, v)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return client.Metrics(ctx)
	case "canter_sandbox_update":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		var name *string
		if args.Name != "" {
			name = &args.Name
		}
		return h.sandboxPatch(ctx, v, name, args.Labels, args.IdleTimeoutSeconds, actor)
	case "canter_sandbox_job":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		if !h.service.Store.sandboxJobKnown(ctx, v.ID, args.JobID) {
			return nil, ErrNotFound
		}
		client, err := h.sandboxClient(ctx, v)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		job, err := client.Job(ctx, args.JobID)
		if err != nil {
			return nil, err
		}
		if job.Status != "starting" && job.Status != "running" {
			h.service.Store.finishSandboxJob(ctx, v.ID, args.JobID)
		}
		return sandboxJobView(job), nil
	case "canter_sandbox_read_file":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		path, err := sandboxFilePath(args.Path)
		if err != nil {
			return nil, err
		}
		client, err := h.sandboxWorkflowClient(ctx, v)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		defer h.sandboxActivity(ctx, v.ID)()
		entries, err := client.ListFiles(ctx, path)
		if err == nil {
			return map[string]any{"path": path, "entries": entries}, nil
		}
		var remote *wrencomputer.Error
		if !errors.As(err, &remote) {
			return nil, err
		}
		chunk, err := client.ReadFile(ctx, path, 0, sandboxFileReadLimit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "content": string(chunk.Data), "size": chunk.Size}, nil
	case "canter_sandbox_write_file":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		if err := sandboxMutationAllowed(v); err != nil {
			return nil, err
		}
		if len(args.Content) > sandboxFileWriteLimit {
			return nil, fmt.Errorf("%w: file writes are limited to 1 MiB", ErrConflict)
		}
		path, err := sandboxFilePath(args.Path)
		if err != nil {
			return nil, err
		}
		client, err := h.sandboxWorkflowClient(ctx, v)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		defer h.sandboxActivity(ctx, v.ID)()
		chunk, err := client.WriteFile(ctx, path, []byte(args.Content))
		if err != nil {
			return nil, err
		}
		_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, "sandbox.file_written", v.ID, map[string]any{"path": path})
		return map[string]any{"path": chunk.Path, "size": chunk.Size}, nil
	case "canter_sandbox_sleep":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sleeping": true}, h.RequestSandboxSleep(ctx, workspace, v.ID, actor)
	case "canter_sandbox_wake":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"waking": true}, h.RequestSandboxWake(ctx, workspace, v.ID, account, actor)
	case "canter_sandbox_delete":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"deleting": true}, h.RequestSandboxDelete(ctx, workspace, v.ID, actor)
	case "canter_sandbox_preview_create":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		if h.config.Sandboxes.PreviewDomain == "" {
			return nil, ErrConflict
		}
		pv, err := h.service.Store.CreateSandboxPreview(ctx, v, actor, args.Port)
		if err != nil {
			return nil, err
		}
		_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, "sandbox.preview_created", v.ID, map[string]any{"preview": pv.ID, "port": pv.Port})
		return map[string]any{"id": pv.ID, "sandboxId": pv.SandboxID, "port": pv.Port, "url": "https://" + pv.Token + "." + h.config.Sandboxes.PreviewDomain}, nil
	case "canter_sandbox_preview_list":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		list, err := h.service.Store.SandboxPreviews(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(list))
		for _, pv := range list {
			out = append(out, map[string]any{"id": pv.ID, "port": pv.Port, "url": "https://" + pv.Token + "." + h.config.Sandboxes.PreviewDomain, "createdBy": pv.CreatedBy, "createdAt": pv.CreatedAt})
		}
		return map[string]any{"previews": out}, nil
	case "canter_sandbox_preview_revoke":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, "sandbox.preview_revoked", v.ID, map[string]any{"preview": args.PreviewID})
		return map[string]any{"revoked": true}, h.service.Store.RevokeSandboxPreview(ctx, v.WorkspaceID, args.PreviewID)
	case "canter_sandbox_snapshots":
		return h.service.Store.ListSandboxSnapshots(ctx, workspace, args.SourceSandboxID, args.Before)
	case "canter_sandbox_snapshot_get":
		snapshot, err := h.service.Store.SandboxSnapshot(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		if snapshot.State != "ready" {
			return nil, ErrNotFound
		}
		return snapshot, nil
	case "canter_sandbox_snapshot_update":
		var input SandboxSnapshotUpdate
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		return h.UpdateSandboxSnapshot(ctx, workspace, account, args.ID, actor, input)
	case "canter_sandbox_templates":
		if args.ID != "" {
			return h.service.Store.SandboxTemplate(ctx, workspace, args.ID)
		}
		templates, err := h.service.Store.ListSandboxTemplates(ctx, workspace)
		if err != nil {
			return nil, err
		}
		var history bool
		if err := h.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_billing WHERE workspace_id=$1 AND status='active')`, workspace).Scan(&history); err != nil {
			return nil, err
		}
		return map[string]any{"templates": templates, "historyEnabled": history}, nil
	case "canter_sandbox_template_create":
		return h.CreateSandboxTemplate(ctx, workspace, account, actor, SandboxTemplateCreate{Name: args.Name, SnapshotID: args.SnapshotID})
	case "canter_sandbox_template_version_add":
		return h.AddSandboxTemplateVersion(ctx, workspace, account, args.ID, args.SnapshotID, actor)
	case "canter_sandbox_template_delete":
		if err := h.DeleteSandboxTemplate(ctx, workspace, account, args.ID, actor); err != nil {
			return nil, err
		}
		return map[string]bool{"deleted": true}, nil
	case "canter_sandbox_template_version_delete":
		if err := h.DeleteSandboxTemplateVersion(ctx, workspace, account, args.ID, args.Version, actor); err != nil {
			return nil, err
		}
		return map[string]bool{"deleted": true}, nil
	case "canter_sandbox_restore":
		return h.SelectSandboxSnapshot(ctx, workspace, account, args.ID, actor, SandboxSnapshotSelection{SnapshotID: args.SnapshotID, ExpectedGeneration: args.ExpectedGeneration})
	case "canter_sandbox_screenshot":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		png, width, height, err := h.sandboxDesktopScreenshot(ctx, v)
		if err != nil {
			return nil, err
		}
		captureID, err := newID("capture_")
		if err != nil {
			return nil, err
		}
		return sandboxDesktopScreenshotResult{
			ComputerID: v.ID, SandboxID: v.ID, Generation: v.Generation, Width: width, Height: height,
			Capture: OperatorAttachment{ID: captureID, Name: "Sandbox screenshot", MediaType: "image/png", Size: len(png), DataBase64: base64.StdEncoding.EncodeToString(png)},
		}, nil
	case "canter_sandbox_input":
		v, err := h.service.Store.Sandbox(ctx, workspace, args.ID)
		if err != nil {
			return nil, err
		}
		var input wrencomputer.DesktopInput
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
		if args.ExpectedGeneration <= 0 || args.ExpectedGeneration != v.Generation {
			return nil, ErrConflict
		}
		err = h.sandboxDesktopInput(ctx, v, input, sandboxDesktopInputClaim{})
		return map[string]any{"sandboxId": v.ID, "generation": v.Generation, "submitted": err == nil, "outcomeVerified": false, "action": input.Kind}, err
	}
	return nil, ErrNotFound
}
