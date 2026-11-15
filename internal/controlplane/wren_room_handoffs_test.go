package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func roomHandoffFixture(t *testing.T) (*Store, Conversation, Conversation, OperatorRun, []Wren) {
	t.Helper()
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	guest, _, _, err := s.Signup(ctx, "handoff-guest@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, guest.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	wrens := []Wren{}
	for _, name := range []string{"Lead", "Reviewer", "Checker"} {
		w, e := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_handoff_" + strings.ToLower(name), Name: name, Purpose: "Help with the requested stage", Shape: "circle"}, true)
		if e != nil {
			t.Fatal(e)
		}
		wrens = append(wrens, w)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_handoff", Kind: "group", Name: "Shared handoff", MemberIDs: []string{guest.ID}, WrenIDs: []string{wrens[0].ID, wrens[1].ID, wrens[2].ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	roomOwner, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	roomGuest, err := s.Conversation(ctx, c.WorkspaceID, guest.ID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.enqueueOperatorWithRecipients(ctx, roomGuest, "handoff-human-request", "@Lead prepare a draft and ask Reviewer to verify it", "test-model", OperatorModelOptions{}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	return s, roomGuest, roomOwner, parent, wrens
}

func TestWrenRoomHandoffDurableIdentityContextAndLoopBound(t *testing.T) {
	s, c, _, parent, wrens := roomHandoffFixture(t)
	ctx := context.Background()
	brief := "Review my Silver Heron draft. @Checker is a reference, not the recipient."
	child, err := s.AskRoomWren(ctx, parent, c, "handoff-op", wrens[1].ID, brief, "test-model")
	if err != nil || child.Speaker == nil || child.Speaker.ID != wrens[1].ID || child.RequesterAccountID != c.AccountID || child.TurnID != parent.TurnID || child.Status != "queued" {
		t.Fatalf("handoff %+v %v", child, err)
	}
	retry, err := s.AskRoomWren(ctx, parent, c, "handoff-op", wrens[1].ID, brief, "test-model")
	if err != nil || retry.ID != child.ID {
		t.Fatalf("retry %+v %v", retry, err)
	}
	if _, err = s.AskRoomWren(ctx, parent, c, "handoff-op", wrens[2].ID, "changed brief", "test-model"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay %v", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_room_handoffs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay created %d %v", count, err)
	}
	if _, ok, e := s.claimOperator(ctx); e != nil || ok {
		t.Fatalf("child began before parent finished %v %v", ok, e)
	}
	if err = s.finishOperator(ctx, parent, "completed", "My actual draft is Silver Heron."); err != nil {
		t.Fatal(err)
	}
	active, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || active.ID != child.ID {
		t.Fatalf("child claim %+v %v %v", active, ok, err)
	}
	history, err := s.operatorHistory(ctx, active, 80)
	if err != nil {
		t.Fatal(err)
	}
	context := operatorRecentContext(history, active)
	assignments := 0
	for _, m := range context {
		if strings.Contains(m.Content, "[Current assignment") {
			assignments++
		}
	}
	if assignments != 1 || !strings.Contains(context[len(context)-1].Content, "Handoff from Wren Lead") || !strings.Contains(context[len(context)-1].Content, brief) {
		t.Fatalf("wrong handoff assignment %+v", context)
	}
	joined := ""
	for _, m := range context {
		joined += m.Content
	}
	if !strings.Contains(joined, "My actual draft is Silver Heron.") {
		t.Fatal("parent's actual answer missing")
	}
	if _, err = s.AskRoomWren(ctx, active, c, "back-to-lead", wrens[0].ID, "again", "test-model"); err == nil {
		t.Fatal("cycle accepted")
	}
	if _, err = s.AskRoomWren(ctx, active, c, "self-handoff", wrens[1].ID, "again", "test-model"); err == nil {
		t.Fatal("self handoff accepted")
	}
	third, err := s.AskRoomWren(ctx, active, c, "next-stage", wrens[2].ID, "Check the public result only", "test-model")
	if err != nil || third.TurnID != parent.TurnID {
		t.Fatalf("next stage %+v %v", third, err)
	}
	if err = s.cancelOperatorRequest(ctx, c.WorkspaceID, c.AccountID, c.ID, active.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE turn_id=$1 AND status IN ('queued','running')`, parent.TurnID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stop left %d runs %v", count, err)
	}
	if _, err = s.AskRoomWren(ctx, active, c, "after-stop", wrens[2].ID, "keep going", "test-model"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled run handed off %v", err)
	}
}

func TestWrenRoomHandoffRejectsForeignRequesterStaleLeaseAndRemovedParticipant(t *testing.T) {
	s, c, owner, parent, wrens := roomHandoffFixture(t)
	ctx := context.Background()
	if _, err := s.AskRoomWren(ctx, parent, owner, "wrong-requester", wrens[1].ID, "review", "test-model"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner replaced requester %v", err)
	}
	stale := parent
	stale.Lease = "old-lease"
	if _, err := s.AskRoomWren(ctx, stale, c, "stale-parent", wrens[1].ID, "review", "test-model"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale lease %v", err)
	}
	if _, err := s.AskRoomWren(ctx, parent, c, "foreign-target", "wren_outside_room", "review", "test-model"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign target %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM wren_room_members WHERE room_id='room_handoff' AND account_id=$1`, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AskRoomWren(ctx, parent, c, "removed-requester", wrens[1].ID, "review", "test-model"); err == nil {
		t.Fatal("removed person handed off")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_room_handoffs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected requests persisted %d %v", count, err)
	}
}

func TestWrenRoomHandoffStopPreservesAnotherPersonsRequest(t *testing.T) {
	s, c, owner, parent, wrens := roomHandoffFixture(t)
	ctx := context.Background()
	child, err := s.AskRoomWren(ctx, parent, c, "stop-handoff", wrens[1].ID, "Review my draft", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	other := enqueueRoomWork(t, s, owner, "independent-owner", "Check a separate question", wrens[2])
	if err = s.cancelOperatorRequest(ctx, c.WorkspaceID, c.AccountID, c.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.OperatorMessages(ctx, c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range page.Messages {
		if m.RunID == child.ID && m.Role == "user" {
			found = true
			if m.HandoffStatus != "cancelled" || m.OriginKind != "handoff" || m.SenderName != "Lead" {
				t.Fatalf("handoff lost durable stopped identity %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("stopped handoff missing from reload")
	}
	var status string
	if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, other.ID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("other request %s %v", status, err)
	}
	active, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || active.ID != other.ID {
		t.Fatalf("independent claim %+v %v %v", active, ok, err)
	}
}

func TestWrenRoomHandoffRejectsAutomatedOrigins(t *testing.T) {
	for _, kind := range []string{"responsibility", "job"} {
		t.Run(kind, func(t *testing.T) {
			s, c, _, parent, wrens := roomHandoffFixture(t)
			ctx := context.Background()
			if _, err := s.pool.Exec(ctx, `UPDATE operator_runs SET origin_kind=$2 WHERE id=$1`, parent.ID, kind); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AskRoomWren(ctx, parent, c, "automated-handoff", wrens[1].ID, "Review", "test-model"); !errors.Is(err, ErrForbidden) {
				t.Fatalf("automated handoff %v", err)
			}
		})
	}
}
