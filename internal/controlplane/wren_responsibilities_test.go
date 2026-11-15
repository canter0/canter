package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func responsibilityFixture(t *testing.T) (*Store, Conversation, Wren) {
	t.Helper()
	s, c, _ := operatorFixture(t)
	w, err := s.SaveWren(context.Background(), c.WorkspaceID, c.AccountID, Wren{ID: "wren_routine_test", Name: "Scout", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Conversation(context.Background(), c.WorkspaceID, c.AccountID, w.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, w
}
func responsibilityInput(c Conversation, w Wren) WrenResponsibilityInput {
	return WrenResponsibilityInput{ConversationID: c.ID, WrenID: w.ID, Name: "Morning research", Instruction: "Check the authorized sources and report changes.", Schedule: WrenResponsibilitySchedule{Kind: "interval", EverySeconds: 3600, TimeZone: "America/Toronto"}}
}
func responsibilityEdit(r WrenResponsibility) WrenResponsibilityInput {
	return WrenResponsibilityInput{ID: r.ID, ConversationID: r.ConversationID, WrenID: r.WrenID, Name: r.Name, Instruction: r.Instruction, ReportPolicy: r.ReportPolicy, Schedule: r.Schedule, State: r.State, ExpectedRevision: r.Revision}
}
func TestWrenResponsibilityScheduleValidationAndDST(t *testing.T) {
	base := WrenResponsibilityInput{ID: "resp_schedule", Name: "Research", Instruction: "Research", Schedule: WrenResponsibilitySchedule{Kind: "cron", Cron: "30 2 * * *", TimeZone: "America/Toronto"}}
	if err := validateResponsibilityInput(&base); err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	next, err := nextResponsibilityTime(base.Schedule, after)
	if err != nil || next == nil || !next.Equal(time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("spring gap next=%v err=%v", next, err)
	}
	base.Schedule.Cron = "30 1 * * *"
	after = time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	first, _ := nextResponsibilityTime(base.Schedule, after)
	second, _ := nextResponsibilityTime(base.Schedule, *first)
	if !first.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) || !second.Equal(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("fall repeated hour: %v %v", first, second)
	}
	for _, change := range []func(*WrenResponsibilityInput){
		func(v *WrenResponsibilityInput) {
			v.Schedule.Kind = "interval"
			v.Schedule.Cron = ""
			v.Schedule.EverySeconds = 59
		},
		func(v *WrenResponsibilityInput) { v.Schedule.TimeZone = "Local" },
		func(v *WrenResponsibilityInput) { v.Schedule.TimeZone = "not/a-zone" },
		func(v *WrenResponsibilityInput) { v.Schedule.Cron = "@every 5m" },
		func(v *WrenResponsibilityInput) { v.Schedule.Cron = "0 0 0 * * *" },
		func(v *WrenResponsibilityInput) {
			v.Schedule.Kind = "event"
			v.Schedule.Cron = ""
			v.Schedule.EventName = ""
		},
	} {
		v := base
		change(&v)
		if err := validateResponsibilityInput(&v); err == nil {
			t.Fatalf("accepted invalid schedule %+v", v)
		}
	}
}
func TestWrenResponsibilityOutboxConcurrentReplayAndSnapshot(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "manual-operation-001", "", json.RawMessage(`{"source":"human requested test"}`))
	if err != nil {
		t.Fatal(err)
	}
	dup, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "manual-operation-001", "", nil)
	if err != nil || dup.ID != wake.ID {
		t.Fatalf("trigger replay %v %v", dup, err)
	}
	// A fresh Store instance observes the same durable outbox after a restart.
	reopened := NewStore(s.pool)
	reopened.now = s.now
	var group sync.WaitGroup
	fail := make(chan error, 4)
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, e := reopened.DispatchWrenResponsibilities(ctx, "test", 16); fail <- e }()
	}
	group.Wait()
	close(fail)
	for e := range fail {
		if e != nil {
			t.Fatal(e)
		}
	}
	history, err := s.WrenResponsibilityHistory(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || len(history) != 1 || history[0].RunID == "" {
		t.Fatalf("history %+v %v", history, err)
	}
	run := OperatorRun{ID: history[0].RunID}
	var count, messages int
	var kind, respID string
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE responsibility_id=$1`, r.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_messages WHERE conversation_id=$1 AND role='user'`, c.ID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT origin_kind,responsibility_id FROM operator_runs WHERE id=$1`, run.ID).Scan(&kind, &respID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || messages != 0 || kind != "responsibility" || respID != r.ID {
		t.Fatalf("duplicate or false human attribution %d %d %s %s", count, messages, kind, respID)
	}
	if err = s.ValidateResponsibilityRun(ctx, c, run); err != nil {
		t.Fatal(err)
	}
	edit := responsibilityEdit(r)
	edit.Instruction = "New instruction"
	edit.Schedule.TimeZone = "Asia/Tokyo"
	r, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.ResponsibilityRunContext(ctx, run)
	if err != nil || !strings.Contains(text, "America/Toronto") || !strings.Contains(text, "Check the authorized sources") || strings.Contains(text, "Standing instruction: New instruction") {
		t.Fatalf("accepted snapshot changed: %s %v", text, err)
	}
	edit = responsibilityEdit(r)
	edit.State = "paused"
	r, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateResponsibilityRun(ctx, c, run); err != nil {
		t.Fatalf("pause stopped accepted run: %v", err)
	}
	edit = responsibilityEdit(r)
	edit.State = "revoked"
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateResponsibilityRun(ctx, c, run); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked run accepted: %v", err)
	}
	var status string
	s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, run.ID).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("revoked run not cancelled: %s", status)
	}
}
func TestWrenResponsibilityCatchupPauseOnceAndEvent(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	due := now.Add(-25 * time.Hour)
	if _, err = s.pool.Exec(ctx, `UPDATE wren_responsibilities SET next_run_at=$2 WHERE id=$1`, r.ID, due); err != nil {
		t.Fatal(err)
	}
	if err = s.stageResponsibilityDue(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || r.NextRunAt == nil || !r.NextRunAt.After(now) || r.NextRunAt.After(now.Add(time.Hour)) {
		t.Fatalf("bad catchup %+v %v", r, err)
	}
	// A still pending occurrence suppresses another automatic wake, even across downtime.
	now = now.Add(48 * time.Hour)
	if err = s.stageResponsibilityDue(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	h, err := s.WrenResponsibilityHistory(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || len(h) != 1 {
		t.Fatalf("catchup storm: %+v %v", h, err)
	}
	edit := responsibilityEdit(r)
	edit.State = "paused"
	r, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if r.NextRunAt != nil {
		t.Fatal("paused schedule retained future wake")
	}
	if _, err = s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "paused-manual-001", "", nil); err != nil {
		t.Fatalf("explicit paused test: %v", err)
	}
	edit = responsibilityEdit(r)
	edit.State = "active"
	edit.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "lead.received", TimeZone: "UTC"}
	r, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "webhook-source-001", "wrong.event", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong event accepted %v", err)
	}
	if _, err = s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "webhook-source-002", "lead.received", nil); err != nil {
		t.Fatal(err)
	}
	edit = responsibilityEdit(r)
	at := now.Add(time.Minute)
	edit.Schedule = WrenResponsibilitySchedule{Kind: "once", At: &at, TimeZone: "UTC"}
	r, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err = s.stageResponsibilityDue(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || r.NextRunAt != nil {
		t.Fatalf("one-time repeated %+v %v", r, err)
	}
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit accepted %v", err)
	}
}
func TestWrenResponsibilityRoomPermissionsAndDispatchRevocation(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	member, _, _, err := s.Signup(ctx, "routine-member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, member.ID, c.WorkspaceID)
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_routine_test", Kind: "group", Name: "Research team", WrenIDs: []string{w.ID}, MemberIDs: []string{member.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenResponsibility(ctx, c.WorkspaceID, member.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, member.ID, responsibilityEdit(r)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member edited owner's grant: %v", err)
	}
	if _, err = s.TriggerWrenResponsibility(ctx, c.WorkspaceID, member.ID, r.ID, "member-manual-001", "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member triggered owner's identity: %v", err)
	}
	if _, err = s.WrenResponsibility(ctx, "other-workspace", member.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross workspace %+v", err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "remove-wren-001", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_room_wrens WHERE room_id=$1 AND wren_id=$2`, room.ID, w.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 0 {
		t.Fatalf("removed Wren dispatched %d %v", n, err)
	}
	got, err := s.responsibilityWake(ctx, wake.ID)
	if err != nil || got.Status != "failed" || got.RunID != "" {
		t.Fatalf("failed wake %+v %v", got, err)
	}
	r, err = s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || r.State != "paused" {
		t.Fatalf("invalid scope not paused %+v %v", r, err)
	}
	edit := responsibilityEdit(r)
	edit.State = "revoked"
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit); err != nil {
		t.Fatalf("cannot revoke removed Wren: %v", err)
	}
}
func TestWrenResponsibilityConfigurationLimit(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	for i := 0; i < WrenResponsibilityLimit; i++ {
		input := responsibilityInput(c, w)
		input.ID = fmt.Sprintf("resp_limit_%03d", i)
		if _, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input); err != nil {
			t.Fatal(err)
		}
	}
	input := responsibilityInput(c, w)
	if _, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input); !errors.Is(err, ErrCapacity) {
		t.Fatalf("cap not enforced %v", err)
	}
}

func TestWrenResponsibilityRevokeStopsOwnChildNotHumanWork(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "revoke-child-001", "", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 1 {
		t.Fatalf("dispatch %d %v", n, err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	job, err := s.CreateWrenJob(ctx, parent, c, "routine-child-operation", "Produce report", "Write the report from current sources.", "test")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.EnqueueOperator(ctx, c, "unrelated-human-request", "A separate human instruction", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	edit := responsibilityEdit(r)
	edit.State = "revoked"
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, edit); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent.ID, job.ChildRunID} {
		var status string
		if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, id).Scan(&status); err != nil || status != "cancelled" {
			t.Fatalf("own run %s %s %v", id, status, err)
		}
	}
	var status string
	if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, human.ID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("unrelated human stopped %s %v", status, err)
	}
	var suppressed bool
	if err = s.pool.QueryRow(ctx, `SELECT return_suppressed FROM wren_jobs WHERE id=$1`, job.ID).Scan(&suppressed); err != nil || !suppressed {
		t.Fatalf("return not fenced %v %v", suppressed, err)
	}
}

func TestWrenResponsibilityMembershipLossFencesRecoveredAndPending(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "accepted-before-revocation", "", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 1 {
		t.Fatalf("dispatch %d %v", n, err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err = s.operatorCheckpoint(ctx, run, []modelMessage{{Role: "system", Content: "Recovered checkpoint"}}); err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "pending-before-revocation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateResponsibilityRun(ctx, c, run); !errors.Is(err, ErrForbidden) {
		t.Fatalf("checkpoint bypassed membership: %v", err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 0 {
		t.Fatalf("revoked member dispatched %d %v", n, err)
	}
	got, err := s.responsibilityWake(ctx, wake.ID)
	if err != nil || got.Status != "failed" || got.RunID != "" {
		t.Fatalf("pending grant not fenced %+v %v", got, err)
	}
}

func TestWrenResponsibilityHTTPContract(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	w, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_http_routine", Name: "Scout", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, w.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	request := func(method, path, session string, data any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(data)
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-responsibilities"+path, bytes.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
		}
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	created := request("POST", "", token, responsibilityInput(c, w))
	if created.Code != 200 {
		t.Fatalf("create %d %s", created.Code, created.Body)
	}
	var r WrenResponsibility
	if err = json.Unmarshal(created.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	got := request("GET", "?conversationId="+c.ID, token, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), r.ID) {
		t.Fatalf("list %d %s", got.Code, got.Body)
	}
	edit := responsibilityEdit(r)
	edit.State = "paused"
	got = request("PATCH", "/"+r.ID, token, edit)
	if got.Code != 200 {
		t.Fatalf("pause %d %s", got.Code, got.Body)
	}
	got = request("PATCH", "/"+r.ID, token, edit)
	if got.Code != 409 {
		t.Fatalf("stale patch %d %s", got.Code, got.Body)
	}
	got = request("POST", "/"+r.ID+"/trigger", token, map[string]string{"eventKey": "http-test-operation"})
	if got.Code != 202 {
		t.Fatalf("trigger %d %s", got.Code, got.Body)
	}
	got = request("GET", "/"+r.ID+"/history", token, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), "http-test-operation") {
		t.Fatalf("history %d %s", got.Code, got.Body)
	}
	got = request("GET", "/"+r.ID, "", nil)
	if got.Code != 401 {
		t.Fatalf("anonymous read %d", got.Code)
	}
}

func TestWrenResponsibilityCreateReplayAndQueueBackoff(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	input := responsibilityInput(c, w)
	input.ID = "resp_stable_create"
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, input)
	if err != nil || replay.ID != r.ID || replay.Revision != 1 {
		t.Fatalf("lost create replay %+v %v", replay, err)
	}
	changed := input
	changed.Instruction = "Different create payload"
	if _, err = s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("different payload overwrote create %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err = s.EnqueueOperator(ctx, c, fmt.Sprintf("queued-human-%d", i), "Earlier human work", "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "backoff-queue-operation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 0 {
		t.Fatalf("queue limit bypass %d %v", n, err)
	}
	wake, err = s.responsibilityWake(ctx, wake.ID)
	if err != nil || wake.Status != "queued" || wake.RunID != "" || !strings.Contains(wake.Failure, "earlier work") {
		t.Fatalf("backoff lost or fabricated run %+v %v", wake, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE responsibility_id=$1`, r.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("backoff created run %d %v", count, err)
	}
}

func TestWrenResponsibilityRoleDowngradePausesPendingAndFencesRecovery(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "role-accepted-operation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 1 {
		t.Fatalf("dispatch %d %v", n, err)
	}
	accepted, err = s.responsibilityWake(ctx, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "role-pending-operation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Direct role updates exercise the runtime fence independently of the new
	// membership endpoint's proactive cancellation and pause transaction.
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateResponsibilityRun(ctx, c, OperatorRun{ID: accepted.RunID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("downgraded checkpoint accepted %v", err)
	}
	if n, err := s.DispatchWrenResponsibilities(ctx, "test", 16); err != nil || n != 0 {
		t.Fatalf("readonly owner dispatched %d %v", n, err)
	}
	pending, err = s.responsibilityWake(ctx, pending.ID)
	if err != nil || pending.Status != "failed" || pending.RunID != "" {
		t.Fatalf("role loss produced phantomrun %+v %v", pending, err)
	}
	r, err = s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || r.State != "paused" || r.NextRunAt != nil {
		t.Fatalf("downgrade not paused %+v %v", r, err)
	}
}

func TestWrenResponsibilityDispatchLocksAuthorityBeforeResource(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	wake, err := s.TriggerWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, r.ID, "authority-lock-order", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	revoke, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer revoke.Rollback(context.Background())
	var role string
	if err = revoke.QueryRow(ctx, `SELECT role FROM memberships WHERE workspace_id=$1 AND account_id=$2 FOR UPDATE`, c.WorkspaceID, c.AccountID).Scan(&role); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		started, e := s.dispatchResponsibilityWake(ctx, wake.ID, "test")
		if e == nil && started {
			e = errors.New("readonly owner dispatch accepted")
		}
		result <- e
	}()
	deadline := time.Now().Add(2 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%SELECT m.role FROM memberships%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("dispatcher did not wait for its authority lock before resource locking")
	}
	var id string
	if err = revoke.QueryRow(ctx, `SELECT id FROM wren_responsibilities WHERE id=$1 FOR UPDATE NOWAIT`, r.ID).Scan(&id); err != nil {
		t.Fatalf("dispatcher holds resource while waiting for membership (lock inversion): %v", err)
	}
	if _, err = revoke.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = revoke.Exec(ctx, `UPDATE wren_responsibilities SET state='paused',next_run_at=NULL WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err = revoke.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	history, err := s.WrenResponsibilityHistory(ctx, c.WorkspaceID, c.AccountID, r.ID)
	if err != nil || len(history) != 1 || history[0].RunID != "" {
		t.Fatalf("role race created paid run %+v %v", history, err)
	}
}
