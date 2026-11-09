package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWrenQuietOutcomeFencesHumanAndRetainsHistory(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)
	o := OperatorRuntime{Server: h}
	human, err := s.EnqueueOperator(ctx, c, "quiet-human", "hello", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || claimed.ID != human.ID {
		t.Fatalf("claim %v %v", ok, err)
	}
	advertised, e := s.wrenOutcomeToolsForRun(ctx, claimed.ID, wrenOutcomeTools())
	if e != nil || len(advertised) != 0 {
		t.Fatalf("human received responsibility outcomes: %v %v", advertised, e)
	}
	raw := json.RawMessage(`{"reason":"Authorized source checked; no actionable changes."}`)
	for _, name := range []string{"canter_finish_quiet", "canter_finish_blocked", "canter_finish_report"} {
		humanRaw := raw
		if name == "canter_finish_report" {
			humanRaw = json.RawMessage(`{"report":"Actual result"}`)
		}
		if _, _, err = o.wrenOutcomeTool(ctx, claimed, c, name, humanRaw); err == nil {
			t.Fatalf("human request ended by %s", name)
		}
	}
	if err = s.finishOperator(ctx, claimed, "completed", "hello"); err != nil {
		t.Fatal(err)
	}
	input := responsibilityInput(c, w)
	input.ReportPolicy = "changes_only"
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "quiet-once", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchWrenResponsibilities(ctx, "test", 8); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("routine claim %v %v", ok, err)
	}
	if _, _, err = o.wrenOutcomeTool(ctx, run, c, "canter_finish_quiet", raw); err != nil {
		t.Fatal(err)
	}
	if quiet, err := s.finishWrenOutcome(ctx, run); err != nil || !quiet {
		t.Fatalf("finish %v %v", quiet, err)
	}
	saved, err := s.responsibilityWake(ctx, wake.ID)
	if err != nil || saved.Status != "quiet" || saved.Reason == "" {
		t.Fatalf("history %+v %v", saved, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages WHERE run_id=$1`, run.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("quiet message spam %d %v", count, err)
	}
	if _, err = s.finishWrenOutcome(ctx, run); err == nil {
		t.Fatal("stale lease finalized twice")
	}
}

func TestWrenBlockedOutcomeRecoveryPublishesOnceAndRetainsSchedule(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	o := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)}
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "blocked-recovery", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchWrenResponsibilities(ctx, "test", 8); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	reason := "The release source is missing. Add it to this conversation, then test this responsibility again."
	raw, _ := json.Marshal(map[string]string{"reason": reason})
	for range 2 {
		if _, handled, e := o.wrenOutcomeTool(ctx, run, c, "canter_finish_blocked", raw); !handled || e != nil {
			t.Fatalf("save outcome %v %v", handled, e)
		}
	}
	if _, _, err = o.wrenOutcomeTool(ctx, run, c, "canter_finish_quiet", raw); err == nil {
		t.Fatal("accepted conflicting final outcome")
	}
	if before, e := s.responsibilityWake(ctx, wake.ID); e != nil || before.Status != "running" {
		t.Fatalf("persisted intent reported completion before finish: %+v %v", before, e)
	}
	// A crash after the tool stores its outcome must not lose the public blocker.
	recovered := run
	recovered.Lease = "recovered-outcome-lease"
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_token=$2,lease_expires_at=now()+interval '1 minute' WHERE id=$1`, run.ID, recovered.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finishWrenOutcome(ctx, run); err == nil {
		t.Fatal("stale worker published after recovery")
	}
	if done, e := NewStore(s.pool).finishWrenOutcome(ctx, recovered); e != nil || !done {
		t.Fatalf("recover outcome %v %v", done, e)
	}
	if _, err = s.finishWrenOutcome(ctx, recovered); err == nil {
		t.Fatal("finalized twice")
	}
	saved, err := s.responsibilityWake(ctx, wake.ID)
	if err != nil || saved.Status != "blocked" || saved.Reason != reason {
		t.Fatalf("history %+v %v", saved, err)
	}
	var messages, events int
	var content, outcome string
	if err = s.pool.QueryRow(ctx, `SELECT count(*),min(content) FROM operator_messages WHERE run_id=$1 AND role='assistant'`, run.ID).Scan(&messages, &content); err != nil || messages != 1 || content != reason {
		t.Fatalf("public reason %d %q %v", messages, content, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*),min(data->>'outcome') FROM operator_events WHERE run_id=$1 AND kind='finished'`, run.ID).Scan(&events, &outcome); err != nil || events != 1 || outcome != "blocked" {
		t.Fatalf("completion event %d %q %v", events, outcome, err)
	}
	counts, err := s.WrenUnreadCounts(ctx, c.WorkspaceID, c.AccountID)
	if err != nil || counts[c.ID] != 1 {
		t.Fatalf("blocker must notify, counts=%v err=%v", counts, err)
	}
	var state string
	var revision int
	var next *time.Time
	if err = s.pool.QueryRow(ctx, `SELECT state,revision,next_run_at FROM wren_responsibilities WHERE id=$1`, r.ID).Scan(&state, &revision, &next); err != nil || state != r.State || revision != r.Revision || next == nil || r.NextRunAt == nil || !next.Equal(*r.NextRunAt) {
		t.Fatalf("outcome changed schedule %s %d %v %v", state, revision, next, err)
	}
}

func TestWrenBlockedOutcomeCancelledBeforeFinishCannotPublish(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	o := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)}
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "blocked-cancelled", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchWrenResponsibilities(ctx, "test", 8); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	raw := json.RawMessage(`{"reason":"The source is missing. Supply it before another test."}`)
	if _, _, err = o.wrenOutcomeTool(ctx, run, c, "canter_finish_blocked", raw); err != nil {
		t.Fatal(err)
	}
	edit := responsibilityEdit(r)
	edit.State = "revoked"
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit); err != nil {
		t.Fatal(err)
	}
	if _, err = s.finishWrenOutcome(ctx, run); err == nil {
		t.Fatal("revoked worker published")
	}
	if _, _, err = o.wrenOutcomeTool(ctx, run, c, "canter_finish_blocked", raw); err == nil {
		t.Fatal("revoked worker recorded outcome")
	}
	saved, err := s.responsibilityWake(ctx, wake.ID)
	if err != nil || saved.Status != "cancelled" {
		t.Fatalf("revocation lost to outcome %+v %v", saved, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages WHERE run_id=$1`, run.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("revoked reply %d %v", count, err)
	}
}

func TestWrenQuietOutcomeRequiresAcceptedReportingPolicy(t *testing.T) {
	for _, policy := range []string{"always", "changes_only"} {
		t.Run(policy, func(t *testing.T) {
			s, c, w := responsibilityFixture(t)
			ctx := context.Background()
			o := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)}
			input := responsibilityInput(c, w)
			input.ReportPolicy = policy
			r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input)
			if err != nil {
				t.Fatal(err)
			}
			wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "report-policy-test", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.DispatchWrenResponsibilities(ctx, "test", 8); err != nil {
				t.Fatal(err)
			}
			run, ok, err := s.claimOperator(ctx)
			if err != nil || !ok {
				t.Fatalf("claim %v %v", ok, err)
			}
			edit := responsibilityEdit(r)
			edit.ReportPolicy = "always"
			if policy == "always" {
				edit.ReportPolicy = "changes_only"
			}
			updated, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit)
			if err != nil {
				t.Fatal(err)
			}
			// Older clients omitting the new field must preserve the owner's saved choice.
			omitted := responsibilityEdit(updated)
			omitted.ReportPolicy = ""
			kept, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, omitted)
			if err != nil || kept.ReportPolicy != updated.ReportPolicy {
				t.Fatalf("omitted policy changed setting %+v %v", kept, err)
			}
			advertised, e := s.wrenOutcomeToolsForRun(ctx, run.ID, wrenOutcomeTools())
			if e != nil {
				t.Fatal(e)
			}
			hasQuiet := false
			for _, tool := range advertised {
				if tool.Name == "canter_finish_quiet" {
					hasQuiet = true
				}
			}
			if hasQuiet != (policy == "changes_only") {
				t.Fatalf("wrong advertised quiet capability for accepted policy %s", policy)
			}
			raw := json.RawMessage(`{"reason":"The authorized source is unchanged."}`)
			_, _, err = o.wrenOutcomeTool(ctx, run, c, "canter_finish_quiet", raw)
			if policy == "always" && err == nil {
				t.Fatal("quiet suppressed an explicitly requested report")
			}
			if policy == "changes_only" && err != nil {
				t.Fatalf("accepted policy was replaced by later edit: %v", err)
			}
			history, err := s.responsibilityWake(ctx, wake.ID)
			if err != nil || history.ReportPolicy != policy {
				t.Fatalf("accepted policy snapshot %+v %v", history, err)
			}
			context, err := s.ResponsibilityRunContext(ctx, run)
			if err != nil || !strings.Contains(context, "Reporting policy: "+policy) {
				t.Fatalf("missing accepted policy in context %s %v", context, err)
			}
			invalid := responsibilityEdit(kept)
			invalid.ReportPolicy = "silent_on_failure"
			if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, invalid); err == nil {
				t.Fatal("invalid report policy accepted")
			}
		})
	}
}

func TestWrenReportOutcomePublishesRequestedResultAndMigratesAgain(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	o := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)}
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "report-outcome-test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchWrenResponsibilities(ctx, "test", 8); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	report := "Verified source: Amber Beacon.\n" + strings.Repeat("A sourced result with enough detail to be useful.\n", 30)
	raw, _ := json.Marshal(map[string]string{"report": report})
	if _, _, err = o.wrenOutcomeTool(ctx, run, c, "canter_finish_report", raw); err != nil {
		t.Fatal(err)
	}
	if done, e := s.finishWrenOutcome(ctx, run); e != nil || !done {
		t.Fatalf("finish report %v %v", done, e)
	}
	history, err := s.responsibilityWake(ctx, wake.ID)
	if err != nil || history.Status != "completed" || history.ReportPolicy != "always" || history.Reason != strings.TrimSpace(report) {
		t.Fatalf("successful history %+v %v", history, err)
	}
	var content string
	if err = s.pool.QueryRow(ctx, `SELECT content FROM operator_messages WHERE run_id=$1 AND role='assistant'`, run.ID).Scan(&content); err != nil || content != strings.TrimSpace(report) {
		t.Fatalf("published report %q %v", content, err)
	}
	// Startup reapplies idempotent migrations; earlier constraints must not reject new records.
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("restart migration rejected a persisted report: %v", err)
	}
}
