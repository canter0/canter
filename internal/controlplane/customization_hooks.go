package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var exactExtensionHookMatcher = regexp.MustCompile(`^\^?wext_([a-f0-9]{12})v([0-9]+)_([a-zA-Z0-9_-]+)\$?$`)

func personalHookToolName(install WrenExtensionInstallation, tool string) string {
	if install.PluginConnectionID != "" {
		return "plugin:" + install.PluginConnectionID + ":" + tool
	}
	return "extension:" + install.ExtensionID + ":" + tool
}

// Runtime aliases contain a conversation installation ID. Bind an exact
// selection to its owned provider, so another conversation uses the same hook
// without accidentally matching another provider's same-named tool.
func stableHookMatcher(ctx context.Context, q artifactQueryer, workspace, account, matcher string) (string, error) {
	parts := exactExtensionHookMatcher.FindStringSubmatch(matcher)
	if parts == nil {
		return matcher, nil
	}
	var install WrenExtensionInstallation
	err := q.QueryRow(ctx, `SELECT extension_id,COALESCE(plugin_connection_id,'') FROM wren_extension_installations WHERE workspace_id=$1 AND owner_id=$2 AND encode(substring(sha256(convert_to(id,'UTF8')) FROM 1 FOR 6),'hex')=$3 AND scope_kind<>'room' LIMIT 1`, workspace, account, parts[1]).Scan(&install.ExtensionID, &install.PluginConnectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: that tool selection is no longer available; discover the current plugin tool before saving the hook", ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return "^" + regexp.QuoteMeta(personalHookToolName(install, parts[3])) + "$", nil
}

// Hook instructions accompany the completed tool response. The normal model
// loop performs the follow-up using its existing authority and write reviews.
// Firing records survive checkpoint replay; the original receipt is untouched.
func (s *Store) customizationToolResult(ctx context.Context, c Conversation, run OperatorRun, call modelToolCall, result json.RawMessage) (json.RawMessage, error) {
	items, err := s.runtimeCustomizations(ctx, c, run)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return result, nil
	}
	var receipt []byte
	err = s.pool.QueryRow(ctx, `SELECT result FROM operator_tool_calls WHERE run_id=$1 AND call_id=$2 AND result IS NOT NULL`, run.ID, call.ID).Scan(&receipt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	var outcome struct {
		Error          any    `json:"error"`
		Status         string `json:"status"`
		ToolName       string `json:"toolName"`
		InstallationID string `json:"installationId"`
	}
	_ = json.Unmarshal(receipt, &outcome)
	event := "PostToolUse"
	if outcome.Error != nil || outcome.Status == "failed" {
		event = "ToolFailure"
	}
	if strings.HasPrefix(call.Function.Name, "wext_") && outcome.Status != "completed" && outcome.Status != "failed" {
		return result, nil
	}
	stableName := ""
	if outcome.InstallationID != "" {
		install, e := s.WrenExtensionInstallation(ctx, c.WorkspaceID, c.AccountID, outcome.InstallationID)
		if e != nil {
			return nil, e
		}
		stableName = personalHookToolName(install, outcome.ToolName)
	}
	matches := []personalCustomization{}
	for _, item := range items {
		if !item.Enabled || item.Kind != "hook" || item.Event != event {
			continue
		}
		pattern, e := regexp.Compile(item.Matcher)
		if e != nil {
			return nil, e
		}
		if pattern.MatchString(call.Function.Name) || outcome.ToolName != "" && pattern.MatchString(outcome.ToolName) || stableName != "" && pattern.MatchString(stableName) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		return result, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var active bool
	if err = tx.QueryRow(ctx, `SELECT true FROM operator_runs WHERE id=$1 AND conversation_id=$2 AND lease_token=$3 AND status='running' AND lease_expires_at>now() FOR UPDATE`, run.ID, c.ID, run.Lease).Scan(&active); err != nil {
		return nil, ErrConflict
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM personal_hook_firings WHERE run_id=$1`, run.ID).Scan(&count); err != nil {
		return nil, err
	}
	for _, item := range matches {
		if count >= 16 {
			break
		}
		tag, e := tx.Exec(ctx, `INSERT INTO personal_hook_firings(run_id,call_id,customization_id,revision,instructions) SELECT $1,$2,id,revision,body FROM personal_customizations WHERE id=$3 AND revision=$4 AND enabled AND created_at<=(SELECT created_at FROM operator_tool_calls WHERE run_id=$1 AND call_id=$2) ON CONFLICT DO NOTHING`, run.ID, call.ID, item.ID, item.Revision)
		if e != nil {
			return nil, e
		}
		count += int(tag.RowsAffected())
	}
	rows, err := tx.Query(ctx, `SELECT p.name,f.instructions FROM personal_hook_firings f JOIN personal_customizations p ON p.id=f.customization_id AND p.revision=f.revision AND p.enabled WHERE f.run_id=$1 AND f.call_id=$2 ORDER BY f.created_at,p.id`, run.ID, call.ID)
	if err != nil {
		return nil, err
	}
	instructions := []string{}
	for rows.Next() {
		var name, body string
		if err = rows.Scan(&name, &body); err != nil {
			rows.Close()
			return nil, err
		}
		instructions = append(instructions, fmt.Sprintf("%s: %s", name, body))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if len(instructions) == 0 {
		return result, nil
	}
	return json.Marshal(map[string]any{"toolResult": result, "hookInstructions": instructions, "hookEvent": event, "trigger": run.ID + ":" + call.ID, "next": "Carry out these saved personal follow-up instructions once for this trigger, using ordinary authorized tools. Do not claim an action happened without its receipt. No new permissions were granted. Avoid repeating a completed action on replay."})
}

// Approval is an explicit human continuation. A pending, uncertain, declined or
// shared-room action never starts a personal-credential follow-up.
func (h *HTTPServer) continueReviewHooks(ctx context.Context, workspace, account string, review WrenExtensionReview) error {
	if review.Status != "accepted" || review.Receipt == nil || (review.Receipt.Status != "completed" && review.Receipt.Status != "failed") {
		return nil
	}
	s := h.service.Store
	install, err := s.WrenExtensionInstallation(ctx, workspace, account, review.InstallationID)
	if err != nil {
		return err
	}
	if install.Scope == "room" || install.OwnerID != account {
		return nil
	}
	if err = extensionScopeAccess(ctx, s.pool, workspace, account, install.WrenKnowledgeScope, false); err != nil {
		return err
	}
	items, err := s.personalCustomizations(ctx, workspace, account)
	if err != nil {
		return err
	}
	event := "PostToolUse"
	if review.Receipt.Status == "failed" {
		event = "ToolFailure"
	}
	tasks := []string{}
	for _, item := range items {
		if !item.Enabled || item.Kind != "hook" || item.Event != event {
			continue
		}
		pattern, e := regexp.Compile(item.Matcher)
		if e != nil {
			return e
		}
		if pattern.MatchString(review.Tool) || pattern.MatchString(extensionDynamicName(install.ID, install.Version, review.Tool)) || pattern.MatchString(personalHookToolName(install, review.Tool)) {
			tasks = append(tasks, item.Name+": "+item.Body)
		}
		if len(tasks) == 16 {
			break
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	c, err := s.Conversation(ctx, workspace, account, review.ConversationID)
	if err != nil {
		return err
	}
	model, options, err := h.conversationModel(ctx, workspace, c.ID, "", OperatorModelOptions{})
	if err != nil {
		return err
	}
	message := fmt.Sprintf("Continue my saved hooks after the %s action %s %s. Its receipt is %s. Do not repeat the original action. Follow-up work uses the usual permissions and action approvals.\n\n%s", review.App, review.Tool, review.Receipt.Status, review.Receipt.ID, strings.Join(tasks, "\n\n"))
	if len(message) > 23000 {
		return fmt.Errorf("the matching hook instructions exceed the follow-up limit")
	}
	_, err = s.EnqueueOperatorWithOptions(ctx, c, "hook_review_"+review.ID, message, model, options, nil)
	return err
}
