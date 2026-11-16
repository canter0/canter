package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func wrenOutcomeTools() []mcpTool {
	return []mcpTool{{Name: "canter_finish_report", Description: "Finish a responsibility successfully and post its requested result to the conversation. Put the actual result in report. Use this when the source exists and the task outcome is satisfied, including an unchanged check when reporting policy is always. This is the normal successful finish path; do not use canter_finish_blocked merely to publish a report.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"report": map[string]any{"type": "string"}}, "required": []string{"report"}, "additionalProperties": false}}, {Name: "canter_finish_quiet", Description: "Finish an actual scheduled/event responsibility without posting a chat message when you verified there is no actionable change and the accepted reporting policy is changes_only. Save a concise truthful reason in run history. Never use for failures, lost authentication, missing data, a human question or a result requiring attention. This ends the current wake; it does not stop the schedule.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string"}}, "required": []string{"reason"}, "additionalProperties": false}}, {Name: "canter_finish_blocked", Description: "End this responsibility run when missing data, unavailable access, a failed prerequisite, or a required human decision prevents the requested outcome. State the verified blocker and the next step in reason. Canter posts that exact reason to the conversation and marks this run Needs attention. This ends the current wake only; it does not pause the schedule or retry actions. Do not use for human turns, successful results, or unchanged quiet checks.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string"}}, "required": []string{"reason"}, "additionalProperties": false}}}
}

// Only advertise outcomes that this accepted wake may use. The tool handler
// still enforces the same boundary if a stale or fabricated call arrives.
func (s *Store) wrenOutcomeToolsForRun(ctx context.Context, runID string, tools []mcpTool) ([]mcpTool, error) {
	var policy string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT COALESCE(w.report_policy,'always') FROM wren_responsibility_wakes w JOIN operator_runs r ON r.id=w.run_id WHERE r.id=$1 AND r.origin_kind='responsibility'),'')`, runID).Scan(&policy); err != nil {
		return nil, err
	}
	out := make([]mcpTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "canter_finish_quiet" && policy != "changes_only" {
			continue
		}
		if (tool.Name == "canter_finish_blocked" || tool.Name == "canter_finish_report") && policy == "" {
			continue
		}
		out = append(out, tool)
	}
	return out, nil
}

func (o *OperatorRuntime) wrenOutcomeTool(ctx context.Context, run OperatorRun, c Conversation, name string, raw json.RawMessage) (any, bool, error) {
	if name != "canter_finish_quiet" && name != "canter_finish_blocked" && name != "canter_finish_report" {
		return nil, false, nil
	}
	outcome := "quiet"
	if name == "canter_finish_blocked" {
		outcome = "blocked"
	}
	if name == "canter_finish_report" {
		outcome = "report"
	}
	var in struct{ Reason, Report string }
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, true, err
	}
	limit := 1000
	if outcome == "report" {
		in.Reason = in.Report
		limit = 12000
	}
	in.Reason = strings.TrimSpace(in.Reason)
	validationText := in.Reason
	if outcome == "report" {
		validationText = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(validationText)
	}
	if !artifactSafeText(validationText, limit) || in.Reason == "" {
		return nil, true, fmt.Errorf("give a nonempty verified result, up to %d characters", limit)
	}
	if err := o.Server.service.Store.ValidateResponsibilityRun(ctx, c, run); err != nil {
		return nil, true, err
	}
	if outcome == "quiet" {
		var allowed bool
		if err := o.Server.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_responsibility_wakes WHERE run_id=$1 AND report_policy='changes_only')`, run.ID).Scan(&allowed); err != nil {
			return nil, true, err
		}
		if !allowed {
			return nil, true, fmt.Errorf("this responsibility reports every run; return the requested result in chat instead of finishing quietly")
		}
	}
	result, err := o.Server.service.Store.pool.Exec(ctx, `INSERT INTO wren_run_outcomes(run_id,outcome,reason) SELECT id,$3,$4 FROM operator_runs WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now() AND origin_kind='responsibility' AND responsibility_id IS NOT NULL ON CONFLICT(run_id) DO NOTHING`, run.ID, run.Lease, outcome, in.Reason)
	if err != nil {
		return nil, true, err
	}
	if result.RowsAffected() != 1 {
		var exists bool
		err = o.Server.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_run_outcomes q JOIN operator_runs r ON r.id=q.run_id WHERE q.run_id=$1 AND r.lease_token=$2 AND r.status='running' AND r.lease_expires_at>now() AND r.origin_kind='responsibility' AND q.outcome=$3 AND q.reason=$4)`, run.ID, run.Lease, outcome, in.Reason).Scan(&exists)
		if err != nil {
			return nil, true, err
		}
		if !exists {
			return nil, true, ErrForbidden
		}
	}
	if outcome != "quiet" {
		return map[string]any{"outcome": outcome, "message": "The result will be posted to this conversation and retained in run history. The schedule is unchanged."}, true, nil
	}
	return map[string]any{"outcome": "quiet", "message": "No chat notification will be posted. The reason is retained in responsibility history."}, true, nil
}
func (s *Store) finishWrenOutcome(ctx context.Context, run OperatorRun) (bool, error) {
	var outcome, reason string
	err := s.pool.QueryRow(ctx, `SELECT outcome,reason FROM wren_run_outcomes WHERE run_id=$1`, run.ID).Scan(&outcome, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if outcome != "quiet" && outcome != "blocked" && outcome != "report" {
		return true, fmt.Errorf("unknown Wren outcome")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	r, err := tx.Exec(ctx, `UPDATE operator_runs SET status='completed',completed_at=now(),failure='',lease_token=NULL,lease_expires_at=NULL WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now() AND origin_kind='responsibility'`, run.ID, run.Lease)
	if err != nil {
		return true, err
	}
	if r.RowsAffected() != 1 {
		return true, ErrConflict
	}
	content := ""
	if outcome != "quiet" {
		content = reason
		id, e := newID("msg_")
		if e != nil {
			return true, e
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES($1,$2,$3,'assistant',$4) ON CONFLICT(run_id,role) DO NOTHING`, id, run.ConversationID, run.ID, content); err != nil {
			return true, err
		}
		if _, err = tx.Exec(ctx, `UPDATE operator_conversations SET updated_at=now() WHERE id=$1`, run.ConversationID); err != nil {
			return true, err
		}
	}
	raw, _ := json.Marshal(map[string]string{"status": "completed", "content": content, "outcome": outcome})
	_, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'finished',$3)`, run.ConversationID, run.ID, raw)
	if err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}
