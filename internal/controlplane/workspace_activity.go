package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// WorkspaceTask records intent. It never grants deployment or policy authority.
type WorkspaceTask struct {
	ID                   string        `json:"id"`
	WorkspaceID          string        `json:"workspaceId"`
	Prompt               string        `json:"prompt"`
	Status               string        `json:"status"`
	RequestedBy          string        `json:"requestedBy"`
	TargetInstallationID string        `json:"targetInstallationId,omitempty"`
	ClaimedBy            string        `json:"claimedBy,omitempty"`
	Result               string        `json:"result,omitempty"`
	Model                string        `json:"model"`
	Reasoning            string        `json:"reasoning"`
	Context              []TaskContext `json:"context,omitempty"`
	CreatedAt            time.Time     `json:"createdAt"`
	UpdatedAt            time.Time     `json:"updatedAt"`
}

type TaskContext struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	ReferenceID string `json:"referenceId,omitempty"`
	URL         string `json:"url,omitempty"`
	MediaType   string `json:"mediaType,omitempty"`
	DataBase64  string `json:"dataBase64,omitempty"`
	Size        int    `json:"size,omitempty"`
}

type TaskInput struct {
	Prompt               string        `json:"prompt"`
	TargetInstallationID string        `json:"targetInstallationId"`
	Model                string        `json:"model"`
	Reasoning            string        `json:"reasoning"`
	Context              []TaskContext `json:"context"`
}

const taskColumns = `id,workspace_id,prompt,status,requested_by,COALESCE(target_installation_id,''),COALESCE(claimed_by,''),result,model,reasoning,created_at,updated_at`

func scanTask(row pgx.Row) (WorkspaceTask, error) {
	var task WorkspaceTask
	err := row.Scan(&task.ID, &task.WorkspaceID, &task.Prompt, &task.Status, &task.RequestedBy, &task.TargetInstallationID, &task.ClaimedBy, &task.Result, &task.Model, &task.Reasoning, &task.CreatedAt, &task.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return task, ErrNotFound
	}
	return task, err
}

func (s *Store) ListWorkspaceTasks(ctx context.Context, workspaceID string) ([]WorkspaceTask, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM workspace_tasks WHERE workspace_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkspaceTask{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

func (s *Store) WorkspaceTask(ctx context.Context, workspaceID, id string) (WorkspaceTask, error) {
	task, err := scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM workspace_tasks WHERE workspace_id=$1 AND id=$2`, workspaceID, id))
	if err != nil {
		return task, err
	}
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(item - 'dataBase64'),'[]'::jsonb) FROM workspace_tasks, jsonb_array_elements(context) item WHERE workspace_id=$1 AND id=$2`, workspaceID, id).Scan(&task.Context)
	return task, err
}

func (s *Store) CreateWorkspaceTask(ctx context.Context, workspaceID, accountID string, input TaskInput) (WorkspaceTask, error) {
	prompt := strings.TrimSpace(input.Prompt)
	targetID := input.TargetInstallationID
	if prompt == "" || len(prompt) > 12000 {
		return WorkspaceTask{}, fmt.Errorf("task must contain 1 to 12000 bytes")
	}
	if input.Model == "" {
		input.Model = "gpt-5.6-luna"
	}
	if input.Reasoning == "" {
		input.Reasoning = "medium"
	}
	if input.Model != "gpt-5.6-luna" && input.Model != "deepseek-v4.1-flash" && input.Model != "glm-5.3-flash" {
		return WorkspaceTask{}, fmt.Errorf("unsupported model selection")
	}
	if input.Reasoning != "low" && input.Reasoning != "medium" && input.Reasoning != "high" {
		return WorkspaceTask{}, fmt.Errorf("unsupported reasoning selection")
	}
	if len(input.Context) > 12 {
		return WorkspaceTask{}, fmt.Errorf("at most 12 context items are allowed")
	}
	totalSize := 0
	for i := range input.Context {
		item := &input.Context[i]
		if strings.TrimSpace(item.Name) == "" || len(item.Name) > 255 {
			return WorkspaceTask{}, fmt.Errorf("context name is required and must be under 256 bytes")
		}
		item.ID, _ = newID("ctx_")
		switch item.Kind {
		case "attachment":
			if len(item.DataBase64) > 3<<20 {
				return WorkspaceTask{}, fmt.Errorf("attachments must be at most 2 MiB each")
			}
			bytes, err := base64.StdEncoding.DecodeString(item.DataBase64)
			if err != nil || len(bytes) > 2<<20 {
				return WorkspaceTask{}, fmt.Errorf("invalid attachment or attachment exceeds 2 MiB")
			}
			item.Size = len(bytes)
			totalSize += len(bytes)
			item.URL = ""
			item.ReferenceID = ""
		case "repository":
			parsed, err := url.Parse(item.URL)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || len(item.URL) > 2048 {
				return WorkspaceTask{}, fmt.Errorf("a valid HTTP or HTTPS repository URL without credentials is required")
			}
			item.DataBase64 = ""
			item.ReferenceID = ""
			item.Size = 0
		case "app", "task":
			var valid bool
			query := `SELECT EXISTS(SELECT 1 FROM systems WHERE workspace_id=$1 AND name=$2)`
			if item.Kind == "task" {
				query = `SELECT EXISTS(SELECT 1 FROM workspace_tasks WHERE workspace_id=$1 AND id=$2)`
			}
			if err := s.pool.QueryRow(ctx, query, workspaceID, item.ReferenceID).Scan(&valid); err != nil {
				return WorkspaceTask{}, err
			}
			if !valid {
				return WorkspaceTask{}, ErrNotFound
			}
			item.DataBase64 = ""
			item.URL = ""
			item.Size = 0
		default:
			return WorkspaceTask{}, fmt.Errorf("unsupported context kind")
		}
	}
	if totalSize > 8<<20 {
		return WorkspaceTask{}, fmt.Errorf("attachments must total at most 8 MiB")
	}
	if targetID != "" {
		var valid bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_installations WHERE id=$1 AND workspace_id=$2 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>now()))`, targetID, workspaceID).Scan(&valid); err != nil {
			return WorkspaceTask{}, err
		}
		if !valid {
			return WorkspaceTask{}, ErrForbidden
		}
	}
	id, err := newID("task_")
	if err != nil {
		return WorkspaceTask{}, err
	}
	if input.Context == nil {
		input.Context = []TaskContext{}
	}
	raw, err := json.Marshal(input.Context)
	if err != nil {
		return WorkspaceTask{}, err
	}
	return scanTask(s.pool.QueryRow(ctx, `INSERT INTO workspace_tasks(id,workspace_id,prompt,requested_by,target_installation_id,created_at,updated_at,model,reasoning,context) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,$6,$7,$8,$9) RETURNING `+taskColumns, id, workspaceID, prompt, accountID, targetID, s.now(), input.Model, input.Reasoning, raw))
}

func (s *Store) ClaimWorkspaceTask(ctx context.Context, workspaceID, id, installationID string) (WorkspaceTask, error) {
	task, err := scanTask(s.pool.QueryRow(ctx, `UPDATE workspace_tasks SET status='working',claimed_by=$3,updated_at=$4 WHERE workspace_id=$1 AND id=$2 AND (status='queued' OR (status='working' AND claimed_by=$3)) AND (target_installation_id IS NULL OR target_installation_id=$3) RETURNING `+taskColumns, workspaceID, id, installationID, s.now()))
	var pgErr *pgconn.PgError
	if errors.Is(err, ErrNotFound) || (errors.As(err, &pgErr) && pgErr.Code == "23505") {
		return task, ErrConflict
	}
	return task, err
}

func (s *Store) FinishWorkspaceTask(ctx context.Context, workspaceID, id, installationID, status, result string) (WorkspaceTask, error) {
	if (status != "completed" && status != "failed") || strings.TrimSpace(result) == "" || len(result) > 12000 {
		return WorkspaceTask{}, fmt.Errorf("a completed or failed status and a result of 1 to 12000 bytes are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WorkspaceTask{}, err
	}
	defer tx.Rollback(ctx)
	task, err := scanTask(tx.QueryRow(ctx, `UPDATE workspace_tasks SET status=$4,result=$5,updated_at=$6 WHERE workspace_id=$1 AND id=$2 AND status='working' AND claimed_by=$3 RETURNING `+taskColumns, workspaceID, id, installationID, status, result, s.now()))
	if errors.Is(err, ErrNotFound) {
		return task, ErrConflict
	}
	if err != nil {
		return task, err
	}
	var temporary bool
	if err = tx.QueryRow(ctx, `SELECT expires_at IS NOT NULL FROM agent_installations WHERE id=$1`, installationID).Scan(&temporary); err != nil {
		return task, err
	}
	if temporary {
		if err = s.revokeInstallationTx(ctx, tx, installationID); err != nil {
			return task, err
		}
	}
	return task, tx.Commit(ctx)
}

func (s *Store) ListWorkspaceActions(ctx context.Context, workspaceID string) ([]AuditEvent, error) {
	// Expose only known display metadata, never request arguments, credentials, or arbitrary audit payloads.
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.workspace_id,e.actor_kind,e.actor_id,e.session_id,CASE WHEN sess.worker_name<>'' THEN sess.worker_name || ' · ' || COALESCE(a.name,'Agent') ELSE COALESCE(a.name,'') END,e.action,e.subject,jsonb_strip_nulls(jsonb_build_object('system',e.metadata->'system','tool',e.metadata->'tool','outcome',e.metadata->'outcome','taskId',e.metadata->'taskId','deploymentId',e.metadata->'deploymentId','changeId',e.metadata->'changeId')),e.occurred_at FROM audit_events e LEFT JOIN agent_installations a ON a.id=e.actor_id AND a.workspace_id=e.workspace_id LEFT JOIN agent_sessions sess ON sess.id=e.session_id AND sess.installation_id=a.id WHERE e.workspace_id=$1 ORDER BY e.occurred_at DESC,e.id DESC LIMIT 100`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.WorkspaceID, &event.Actor.Kind, &event.Actor.ID, &event.Actor.SessionID, &event.Actor.DisplayName, &event.Action, &event.Subject, &event.Metadata, &event.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (h *HTTPServer) workspaceTasks(w http.ResponseWriter, r *http.Request, p Principal, workspaceID string, parts []string) {
	if len(parts) == 3 && parts[1] == "context" && r.Method == http.MethodGet {
		item, err := h.service.Store.WorkspaceTaskContext(r.Context(), workspaceID, parts[0], parts[2])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if item.Kind != "attachment" {
			writeJSON(w, http.StatusOK, item)
			return
		}
		data, err := base64.StdEncoding.DecodeString(item.DataBase64)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": item.Name}))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodGet {
		tasks, err := h.service.Store.ListWorkspaceTasks(r.Context(), workspaceID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		task, err := h.service.Store.WorkspaceTask(r.Context(), workspaceID, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
		return
	}
	if len(parts) == 0 && r.Method == http.MethodPost {
		if p.Account == nil {
			writeStoreError(w, ErrForbidden)
			return
		}
		if err := h.allowWorkspace(r, p, workspaceID, true); err != nil {
			writeStoreError(w, err)
			return
		}
		var input TaskInput
		if !decodeLimit(w, r, &input, 16<<20) {
			return
		}
		task, err := h.service.Store.CreateWorkspaceTask(r.Context(), workspaceID, p.Account.ID, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "task.created", task.ID, map[string]any{"taskId": task.ID})
		writeJSON(w, http.StatusCreated, task)
		return
	}
	methodNotAllowed(w)
}

func (h *HTTPServer) changeTask(r *http.Request, p Principal, workspaceID, id, status, result string) (WorkspaceTask, error) {
	if p.Session != nil && p.Session.ParentSessionID != "" {
		return WorkspaceTask{}, ErrForbidden
	}
	if p.Installation == nil || !p.Installation.Authority.Draft {
		return WorkspaceTask{}, ErrForbidden
	}
	if err := h.allowWorkspace(r, p, workspaceID, true); err != nil {
		return WorkspaceTask{}, err
	}
	var task WorkspaceTask
	var err error
	if status == "working" {
		task, err = h.service.Store.ClaimWorkspaceTask(r.Context(), workspaceID, id, p.Installation.ID)
	} else {
		task, err = h.service.Store.FinishWorkspaceTask(r.Context(), workspaceID, id, p.Installation.ID, status, result)
	}
	if err == nil {
		_ = h.service.Store.Audit(r.Context(), workspaceID, p.Actor, "task."+status, task.ID, map[string]any{"taskId": task.ID})
	}
	return task, err
}

func (h *HTTPServer) recordMCPAction(ctx context.Context, p Principal, name, taskID string, failed bool) {
	if p.Installation == nil {
		return
	}
	for _, tool := range mcpTools() {
		if tool.Name != name {
			continue
		}
		outcome := "completed"
		if failed {
			outcome = "failed"
		}
		_ = h.service.Store.Audit(ctx, p.Installation.WorkspaceID, sdk.ActorRef{Kind: "agent", ID: p.Installation.ID, SessionID: p.Actor.SessionID}, "agent.tool", name, map[string]any{"tool": name, "outcome": outcome, "taskId": taskID})
		return
	}
}

func (s *Store) WorkingTaskID(ctx context.Context, installationID string) string {
	var id string
	_ = s.pool.QueryRow(ctx, `SELECT id FROM workspace_tasks WHERE claimed_by=$1 AND status='working'`, installationID).Scan(&id)
	return id
}

func (s *Store) WorkspaceTaskContext(ctx context.Context, workspaceID, taskID, contextID string) (TaskContext, error) {
	var item TaskContext
	err := s.pool.QueryRow(ctx, `SELECT item FROM workspace_tasks, jsonb_array_elements(context) item WHERE workspace_id=$1 AND id=$2 AND item->>'id'=$3`, workspaceID, taskID, contextID).Scan(&item)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return item, err
}
