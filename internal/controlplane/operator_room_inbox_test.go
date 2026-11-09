package controlplane

import (
	"context"
	"testing"
)

func TestRoomInboxDoesNotSupersedeAnotherHumanOrWren(t *testing.T) {
	s, private, firstWren, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	secondWren, err := s.SaveWren(ctx, private.WorkspaceID, private.AccountID, Wren{ID: "wren_second_inbox", Name: "Second", Purpose: "Independently verify", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.Room(ctx, private.WorkspaceID, private.AccountID, "room_knowledge_test")
	if err != nil {
		t.Fatal(err)
	}
	room.WrenIDs = append(room.WrenIDs, secondWren.ID)
	if _, err = s.SaveRoom(ctx, private.WorkspaceID, private.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	enqueue := func(c Conversation, key string, recipient string) OperatorRun {
		t.Helper()
		r, e := s.enqueueOperatorWithRecipients(ctx, c, key, "Read the saved launch decision", "test", OperatorModelOptions{}, nil, []string{recipient}, false)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	first := enqueue(shared, "room-inbox-owner-first", firstWren.ID)
	claimed, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || claimed.ID != first.ID {
		t.Fatalf("claim %v", err)
	}
	peer, err := s.Conversation(ctx, private.WorkspaceID, member, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	enqueue(peer, "room-inbox-other-human", firstWren.ID)
	enqueue(shared, "room-inbox-other-wren", secondWren.ID)
	if pending, err := s.operatorHasFollowup(ctx, claimed); err != nil || pending {
		t.Fatalf("independent work interrupted the active Wren: %v", err)
	}
	correction := enqueue(shared, "room-inbox-same-human-wren", firstWren.ID)
	if pending, err := s.operatorHasFollowup(ctx, claimed); err != nil || !pending {
		t.Fatalf("actual correction not recognized: %v", err)
	}
	if pending, err := s.operatorHasFollowup(ctx, correction); err != nil || pending {
		t.Fatalf("older queued work superseded newer correction: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET origin_kind='responsibility' WHERE id=$1`, correction.ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.operatorHasFollowup(ctx, claimed); err != nil || pending {
		t.Fatalf("automated wake superseded human: %v", err)
	}
}
