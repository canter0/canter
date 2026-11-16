package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestStopKeepsOtherPeopleAndLaterRequests(t *testing.T) {
	s, private, wren, room, member := knowledgeFixture(t)
	ctx := context.Background()
	peer, err := s.Conversation(ctx, private.WorkspaceID, member, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveWren(ctx, private.WorkspaceID, private.AccountID, Wren{ID: "wren_stop_second", Name: "Second", Purpose: "Independent work", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.Room(ctx, private.WorkspaceID, private.AccountID, "room_knowledge_test")
	if err != nil {
		t.Fatal(err)
	}
	profile.WrenIDs = append(profile.WrenIDs, second.ID)
	if _, err = s.SaveRoom(ctx, private.WorkspaceID, private.AccountID, profile, false); err != nil {
		t.Fatal(err)
	}
	enqueue := func(c Conversation, key string, recipients []string) OperatorRun {
		t.Helper()
		r, e := s.enqueueOperatorWithRecipients(ctx, c, key, "Check the room memory", "test", OperatorModelOptions{}, nil, recipients, false)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	first := enqueue(room, "stop-owner", []string{wren.ID, second.ID})
	active, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || active.ID != first.ID {
		t.Fatalf("claim %v", err)
	}
	other := enqueue(peer, "stop-peer", []string{wren.ID})
	later := enqueue(room, "stop-owner-later", []string{wren.ID})
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test"})
	tokens := map[string]string{}
	for _, email := range []string{"operator@example.com", "knowledge-member@example.com"} {
		a, _, token, e := s.Signin(ctx, email, "correct horse battery staple")
		if e != nil {
			t.Fatal(e)
		}
		tokens[a.ID] = token
	}
	call := func(account, method, run string) *httptest.ResponseRecorder {
		t.Helper()
		body := ""
		parts := []string{room.ID}
		if method == http.MethodPost {
			parts = append(parts, "stop")
			body = `{"runId":"` + run + `"}`
		}
		r := httptest.NewRequest(method, "/v1/workspaces/"+room.WorkspaceID+"/conversations/"+strings.Join(parts, "/"), strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: tokens[account]})
		r.Header.Set("Origin", "https://canter.test")
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	for _, tc := range []struct{ account, want string }{{room.AccountID, first.ID}, {member, other.ID}} {
		out := call(tc.account, "GET", "")
		var detail struct {
			OwnRun      *OperatorRun        `json:"ownRun"`
			ActiveRuns  []OperatorActiveRun `json:"activeRuns"`
			RunSequence int64               `json:"runSequence"`
		}
		json.Unmarshal(out.Body.Bytes(), &detail)
		if out.Code != 200 || detail.OwnRun == nil || detail.OwnRun.ID != tc.want {
			t.Fatalf("wrong personal stop target: %d %s", out.Code, out.Body.String())
		}
		if len(detail.ActiveRuns) != 4 || detail.RunSequence == 0 {
			t.Fatalf("incomplete active snapshot: %d runs at %d", len(detail.ActiveRuns), detail.RunSequence)
		}
		for _, run := range detail.ActiveRuns {
			if run.Speaker == nil || run.CreatedAt.IsZero() || run.SenderName == "" || (run.Status != "queued" && run.Status != "running") {
				t.Fatalf("missing active run identity: %#v", run)
			}
		}
	}
	if out := call(member, "POST", first.ID); out.Code != 404 {
		t.Fatalf("peer stopped another request: %d %s", out.Code, out.Body.String())
	}
	for range 2 {
		if out := call(room.AccountID, "POST", first.ID); out.Code != 200 {
			t.Fatalf("stop/replay %d %s", out.Code, out.Body.String())
		}
	}
	var remaining int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE turn_id=$1 AND status<>'cancelled'`, first.TurnID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("multi-Wren turn was not stopped: %d %v", remaining, err)
	}
	for _, r := range []OperatorRun{other, later} {
		var status string
		s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, r.ID).Scan(&status)
		if status != "queued" {
			t.Fatalf("unrelated request %s became %s", r.ID, status)
		}
	}
	remainingRuns, boundary, err := s.activeOperatorRuns(ctx, room.ID)
	if err != nil || len(remainingRuns) != 2 || boundary == 0 {
		t.Fatalf("snapshot retained stopped work: %#v %d %v", remainingRuns, boundary, err)
	}
	if err = s.finishOperator(ctx, active, "completed", "Late stale output"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped lease still published: %v", err)
	}
	if out := call(member, "POST", "missing"); out.Code != 404 {
		t.Fatalf("unknown target %d", out.Code)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE account_id=$1 AND workspace_id=$2`, member, room.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if out := call(member, "POST", other.ID); out.Code != 403 {
		t.Fatalf("viewer stopped work %d", out.Code)
	}
	if err = s.cancelOperatorRequest(ctx, room.WorkspaceID, member, room.ID, other.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("store accepted revoked writer: %v", err)
	}
}

func TestRequestStopCancelsOnlyItsDelegation(t *testing.T) {
	s, _, wren, room, member := knowledgeFixture(t)
	ctx := context.Background()
	peer, err := s.Conversation(ctx, room.WorkspaceID, member, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(c Conversation, key string) OperatorRun {
		t.Helper()
		r, e := s.enqueueOperatorWithRecipients(ctx, c, key, "Delegate an independent outline", "test", OperatorModelOptions{}, nil, []string{wren.ID}, false)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	enqueue(room, "parent-owner")
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("parent %v", err)
	}
	other := enqueue(peer, "parent-peer")
	child := requireWrenJob(t, s, room, parent, "owner-child")
	if err = s.finishOperator(ctx, parent, "completed", "Delegated"); err != nil {
		t.Fatal(err)
	}
	peerParent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || peerParent.ID != other.ID {
		t.Fatalf("peer parent %+v %v", peerParent, err)
	}
	peerChild := requireWrenJob(t, s, peer, peerParent, "peer-child")
	if err = s.cancelOperatorRequest(ctx, room.WorkspaceID, room.AccountID, room.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, want string }{{child.ChildRunID, "cancelled"}, {peerChild.ChildRunID, "queued"}, {peerParent.ID, "running"}, {parent.ID, "completed"}} {
		var got string
		if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, tc.id).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("%s status %s want %s: %v", tc.id, got, tc.want, err)
		}
	}
	var suppressed bool
	s.pool.QueryRow(ctx, `SELECT return_suppressed FROM wren_jobs WHERE id=$1`, child.ID).Scan(&suppressed)
	if !suppressed {
		t.Fatal("cancelled worker could wake the lead")
	}
	if err = s.cancelOperator(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE id=ANY($1::text[]) AND status IN ('queued','running')`, []string{peerParent.ID, peerChild.ChildRunID}).Scan(&remaining)
	if remaining != 0 {
		t.Fatal("internal whole-conversation cancellation regressed")
	}
}
