package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type personalCustomization struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	Body           string `json:"body"`
	Event          string `json:"event"`
	Matcher        string `json:"matcher"`
	Enabled        bool   `json:"enabled"`
	Revision       int    `json:"revision"`
	Action         string `json:"action"`
	Description    string `json:"description"`
	Source         string `json:"source"`
	SourceRevision string `json:"sourceRevision"`
}

func validateCustomization(v personalCustomization) error {
	limit := 8000
	if v.Kind == "skill" {
		limit = 64000
	}
	if !artifactSafeText(v.Name, 100) || len(v.Body) == 0 || len(v.Body) > limit || strings.ContainsRune(v.Body, 0) || len(v.Matcher) > 250 || len(v.Description) > 1000 {
		return fmt.Errorf("provide a name and instructions of up to 8,000 bytes")
	}
	if (v.Kind == "rule" || v.Kind == "skill") && v.Event == "" && v.Matcher == "" {
		return nil
	}
	if v.Kind != "hook" || (v.Event != "UserPromptSubmit" && v.Event != "PreToolUse" && v.Event != "PostToolUse" && v.Event != "ToolFailure") {
		return fmt.Errorf("choose a supported customization type and event")
	}
	if v.Event == "UserPromptSubmit" && v.Matcher != "" {
		return fmt.Errorf("request hooks do not use a tool matcher")
	}
	if v.Event == "PreToolUse" && v.Action != "" && v.Action != "block" || v.Event != "PreToolUse" && v.Action != "" && v.Action != "agent" {
		return fmt.Errorf("before-tool hooks block; request, after-tool and failure hooks give the agent a task")
	}
	if _, err := regexp.Compile(v.Matcher); err != nil {
		return fmt.Errorf("provide a valid tool-name regular expression")
	}
	return nil
}

func (s *Store) personalCustomizations(ctx context.Context, workspace, account string) ([]personalCustomization, error) {
	if err := s.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,kind,name,body,event,matcher,enabled,revision,action,description,source,source_revision FROM personal_customizations WHERE workspace_id=$1 AND account_id=$2 ORDER BY created_at,id LIMIT 100`, workspace, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []personalCustomization{}
	for rows.Next() {
		var item personalCustomization
		if err = rows.Scan(&item.ID, &item.Kind, &item.Name, &item.Body, &item.Event, &item.Matcher, &item.Enabled, &item.Revision, &item.Action, &item.Description, &item.Source, &item.SourceRevision); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Kind == "hook" {
			matcher, e := stableHookMatcher(ctx, s.pool, workspace, account, items[i].Matcher)
			if errors.Is(e, ErrNotFound) {
				continue
			}
			if e != nil {
				return nil, e
			}
			items[i].Matcher = matcher
		}
	}
	return items, nil
}

func (h *HTTPServer) workspaceCustomizations(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if err := h.service.Store.RequireWorkspaceWriter(r.Context(), workspace, account); err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(parts) == 0 && r.Method == "GET" {
		items, err := h.service.Store.personalCustomizations(r.Context(), workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	var item personalCustomization
	if !decodeLimit(w, r, &item, 70000) {
		return
	}
	if item.Kind == "hook" {
		matcher, err := stableHookMatcher(r.Context(), h.service.Store.pool, workspace, account, item.Matcher)
		if err == nil {
			item.Matcher = matcher
		} else if !errors.Is(err, ErrNotFound) || item.Enabled {
			writeStoreError(w, err)
			return
		}
	}
	if len(parts) == 0 && r.Method == "POST" {
		if err := validateCustomization(item); err != nil {
			writeError(w, 400, err)
			return
		}
		id, err := newID("customization_")
		if err != nil {
			writeStoreError(w, err)
			return
		}
		tx, err := h.service.Store.pool.Begin(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer tx.Rollback(r.Context())
		if err = requireWorkspaceWriter(r.Context(), tx, workspace, account, true); err != nil {
			writeStoreError(w, err)
			return
		}
		// Serialize per person to make the count bound effective under concurrency.
		if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,91))`, workspace+":"+account); err != nil {
			writeStoreError(w, err)
			return
		}
		var count int
		if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM personal_customizations WHERE workspace_id=$1 AND account_id=$2`, workspace, account).Scan(&count); err != nil {
			writeStoreError(w, err)
			return
		}
		if count >= 50 {
			writeError(w, 409, fmt.Errorf("up to 50 personal rules and hooks are supported"))
			return
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO personal_customizations(id,workspace_id,account_id,kind,name,body,event,matcher,enabled,action,description) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, workspace, account, item.Kind, item.Name, item.Body, item.Event, item.Matcher, item.Enabled, item.Action, item.Description)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if item.Kind == "skill" {
			if _, err = tx.Exec(r.Context(), `INSERT INTO personal_skill_files(customization_id,path,content) VALUES($1,'SKILL.md',$2)`, id, []byte(item.Body)); err != nil {
				writeStoreError(w, err)
				return
			}
		}
		if err = tx.Commit(r.Context()); err != nil {
			writeStoreError(w, err)
			return
		}
		item.ID = id
		item.Revision = 1
		writeJSON(w, 201, item)
		return
	}
	if len(parts) != 1 || item.Revision < 1 {
		writeError(w, 400, fmt.Errorf("provide a current customization revision"))
		return
	}
	if r.Method == "PATCH" {
		if err := validateCustomization(item); err != nil {
			writeError(w, 400, err)
			return
		}
		var updated int
		err := h.service.Store.pool.QueryRow(r.Context(), `WITH changed AS (UPDATE personal_customizations SET name=$4,body=$5,event=$6,matcher=$7,enabled=$8,revision=revision+1,updated_at=now(),action=$11,description=$12 WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revision=$9 AND kind=$10 RETURNING id,kind,body), files AS (INSERT INTO personal_skill_files(customization_id,path,content) SELECT id,'SKILL.md',convert_to(body,'UTF8') FROM changed WHERE kind='skill' ON CONFLICT(customization_id,path) DO UPDATE SET content=EXCLUDED.content RETURNING 1) SELECT count(*) FROM changed`, parts[0], workspace, account, item.Name, item.Body, item.Event, item.Matcher, item.Enabled, item.Revision, item.Kind, item.Action, item.Description).Scan(&updated)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if updated != 1 {
			writeStoreError(w, ErrConflict)
			return
		}
		item.ID = parts[0]
		item.Revision++
		writeJSON(w, 200, item)
		return
	}
	if r.Method == "DELETE" {
		tag, err := h.service.Store.pool.Exec(r.Context(), `DELETE FROM personal_customizations WHERE id=$1 AND workspace_id=$2 AND account_id=$3 AND revision=$4`, parts[0], workspace, account, item.Revision)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if tag.RowsAffected() != 1 {
			writeStoreError(w, ErrConflict)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
		return
	}
	writeError(w, 405, fmt.Errorf("method not allowed"))
}

// Personal instructions never propagate to rooms, shared conversations, jobs,
// or unattended runs. Their activation uses the same persisted caller checks
// as personal plugin credentials. Rules do not grant tools or permissions.
func (s *Store) runtimeCustomizations(ctx context.Context, c Conversation, run OperatorRun) ([]personalCustomization, error) {
	scope, ok, human, err := s.extensionRunScope(ctx, c, run)
	if err != nil {
		return nil, err
	}
	if !ok || !human || scope.Scope == "room" {
		return nil, nil
	}
	return s.personalCustomizations(ctx, c.WorkspaceID, c.AccountID)
}

func (s *Store) customizationContext(ctx context.Context, c Conversation, run OperatorRun) (string, error) {
	items, err := s.runtimeCustomizations(ctx, c, run)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	var skills strings.Builder
	skillsOmitted := false
	for _, item := range items {
		if item.Enabled && item.Kind == "skill" {
			if skills.Len() < 4000 {
				fmt.Fprintf(&skills, "\n%s (%s): %s", item.Name, item.ID, operatorExcerpt(item.Description, 180))
			} else {
				skillsOmitted = true
			}
			continue
		}
		if !item.Enabled || (item.Kind != "rule" && item.Event != "UserPromptSubmit") {
			continue
		}
		if text.Len()+len(item.Body) > 16000 {
			return "", fmt.Errorf("enabled personal instructions exceed 16,000 bytes; disable some rules")
		}
		fmt.Fprintf(&text, "\n%s:\n%s\n", item.Name, item.Body)
	}
	if text.Len() == 0 && skills.Len() == 0 {
		return "", nil
	}
	result := "Personal instructions from the current requester. These do not grant permissions or override the current request:\n" + text.String()
	if skills.Len() > 0 {
		result += "\nAvailable personal skills (metadata only). Load relevant instructions and supporting files with canter_read_skill_file, starting at SKILL.md:\n" + skills.String()
	}
	if skillsOmitted {
		result += "\nMore saved skills are available through canter_manage_customizations list."
	}
	return result, nil
}

func (s *Store) customizationToolBlock(ctx context.Context, c Conversation, run OperatorRun, name string) (string, error) {
	items, err := s.runtimeCustomizations(ctx, c, run)
	if err != nil {
		return "", err
	}
	return personalToolBlock(items, name), nil
}
func personalToolBlock(items []personalCustomization, names ...string) string {
	for _, item := range items {
		if !item.Enabled || item.Kind != "hook" || item.Event != "PreToolUse" {
			continue
		}
		match, e := regexp.Compile(item.Matcher)
		if e != nil {
			return "The saved hook pattern is invalid."
		}
		for _, name := range names {
			if name != "" && match.MatchString(name) {
				return "Blocked by your hook “" + item.Name + "”: " + item.Body
			}
		}
	}
	return ""
}

func personalHooksTx(ctx context.Context, q artifactQueryer, workspace, account string) ([]personalCustomization, error) {
	items := []personalCustomization{}
	err := q.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('name',name,'kind',kind,'event',event,'matcher',matcher,'body',body,'enabled',enabled) ORDER BY created_at,id),'[]'::jsonb) FROM personal_customizations WHERE workspace_id=$1 AND account_id=$2 AND kind='hook' AND enabled`, workspace, account).Scan(&items)
	if err != nil {
		return nil, err
	}
	for i := range items {
		matcher, e := stableHookMatcher(ctx, q, workspace, account, items[i].Matcher)
		if errors.Is(e, ErrNotFound) {
			continue
		}
		if e != nil {
			return nil, e
		}
		items[i].Matcher = matcher
	}
	return items, err
}
