package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func replyHydrationAnswer(t *testing.T, s *Store, c Conversation) (OperatorRun, string) {
	t.Helper()
	ctx := context.Background()
	queued, err := s.EnqueueOperator(ctx, c, "hydration-answer", "Share an answer.", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || run.ID != queued.ID {
		t.Fatalf("answer run %+v %v %v", run, ok, err)
	}
	if err = s.finishOperator(ctx, run, "completed", "The answer being quoted."); err != nil {
		t.Fatal(err)
	}
	var target string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM operator_messages WHERE run_id=$1 AND role='assistant'`, run.ID).Scan(&target); err != nil {
		t.Fatal(err)
	}
	return run, target
}
func replyHydrationReply(t *testing.T, s *Store, c Conversation, target string, postOnly bool) string {
	t.Helper()
	run, err := s.enqueueOperatorWithRecipientsAndReply(context.Background(), c, "hydration-reply", "Clarify this answer.", "test", OperatorModelOptions{}, nil, nil, postOnly, target)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err = s.pool.QueryRow(context.Background(), `SELECT id FROM operator_messages WHERE run_id=$1 AND role='user'`, run.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func requireHydratedReply(t *testing.T, s *Store, c Conversation, reply, target, wren, name string, wake bool) {
	t.Helper()
	ctx := context.Background()
	resolved, err := s.WrenReplyTarget(ctx, c.WorkspaceID, c.AccountID, c.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	replies, err := s.WrenMessageReplies(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := replies[reply]
	if !ok {
		t.Fatal("saved reply missing")
	}
	for _, v := range []WrenReplyTarget{resolved, saved} {
		if v.MessageID != target || v.WrenID != wren || v.SenderName != name || v.CanWake != wake {
			t.Fatalf("inconsistent reply %+v; want %s/%s wake=%v", v, wren, name, wake)
		}
	}
	page, err := s.OperatorMessages(ctx, c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range page.Messages {
		if message.ID == reply {
			found = true
			if message.ReplyTo == nil || *message.ReplyTo != saved {
				t.Fatalf("history reply differs %+v vs %+v", message.ReplyTo, saved)
			}
		}
	}
	if !found {
		t.Fatal("reply missing in history")
	}
}

func TestWrenReplyHydrationPrivateAssistantWithoutSpeaker(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	w.Name = "Reading"
	var err error
	w, err = s.SaveWren(ctx, c.WorkspaceID, c.AccountID, w, false)
	if err != nil {
		t.Fatal(err)
	}
	run, target := replyHydrationAnswer(t, s, c)
	if run.Speaker != nil {
		t.Fatalf("expected legacy/private assistant without speaker: %+v", run.Speaker)
	}
	reply := replyHydrationReply(t, s, c, target, false)
	requireHydratedReply(t, s, c, reply, target, w.ID, "Reading", true)
	_, _, token, err := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	req := httptest.NewRequest(http.MethodGet, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wren-replies?conversationId="+c.ID, nil)
	req.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	var body struct {
		Replies map[string]WrenReplyTarget `json:"replies"`
	}
	if response.Code != http.StatusOK {
		t.Fatalf("saved reply GET %d", response.Code)
	}
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Replies[reply].SenderName != "Reading" || !body.Replies[reply].CanWake {
		t.Fatalf("saved reply GET author %+v %v", body.Replies[reply], err)
	}

	archiveLifecycle(t, s, c, 0, true)
	requireHydratedReply(t, s, c, reply, target, w.ID, "Reading", false)
}

func TestWrenReplyHydrationRoomSnapshotSurvivesRemoval(t *testing.T) {
	s, private, w, c, _ := knowledgeFixture(t)
	ctx := context.Background()
	w.Name = "Reading"
	var err error
	w, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, w, false)
	if err != nil {
		t.Fatal(err)
	}
	run, target := replyHydrationAnswer(t, s, c)
	if run.Speaker == nil || run.Speaker.Name != "Reading" || run.SenderName == "Reading" {
		t.Fatalf("room author and requester fixture %+v", run)
	}
	reply := replyHydrationReply(t, s, c, target, true)
	requireHydratedReply(t, s, c, reply, target, w.ID, "Reading", true)
	rooms, err := s.Rooms(ctx, c.WorkspaceID, c.AccountID)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("room %+v %v", rooms, err)
	}
	room := rooms[0]
	room.WrenIDs = nil
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	w.Name = "Renamed elsewhere"
	if _, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, w, false); err != nil {
		t.Fatal(err)
	}
	requireHydratedReply(t, s, c, reply, target, w.ID, "Reading", false)
}

func TestWrenReplyHydrationSharedPrivateUsesReviewedProfileAndRevocation(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	w.Name = "Reading"
	var err error
	w, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, w, false)
	if err != nil {
		t.Fatal(err)
	}
	published := publishWren(t, s, private, w)
	chat, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Conversation(ctx, private.WorkspaceID, member, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	_, target := replyHydrationAnswer(t, s, c)
	reply := replyHydrationReply(t, s, c, target, false)
	requireHydratedReply(t, s, c, reply, target, w.ID, "Reading", true)
	if _, err = s.SaveWrenPublication(ctx, private.WorkspaceID, private.AccountID, w.ID, WrenPublicationInput{Published: false, ExpectedRevision: published.Revision}); err != nil {
		t.Fatal(err)
	}
	w.Name = "Publisher private name"
	if _, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, w, false); err != nil {
		t.Fatal(err)
	}
	requireHydratedReply(t, s, c, reply, target, w.ID, "Reading", false)
}

func TestWrenReplyHydrationEarlierHistoryPageRetainsAssistantName(t *testing.T) {
	s, c, w := responsibilityFixture(t)
	ctx := context.Background()
	w.Name = "Reading"
	var err error
	w, err = s.SaveWren(ctx, c.WorkspaceID, c.AccountID, w, false)
	if err != nil {
		t.Fatal(err)
	}
	_, target := replyHydrationAnswer(t, s, c)
	reply := replyHydrationReply(t, s, c, target, false)
	if _, err = s.pool.Exec(ctx, `WITH runs AS (INSERT INTO operator_runs(id,conversation_id,request_id,model,status,completed_at) SELECT 'run_page_'||n,$1,'page_'||n,'test','completed',now() FROM generate_series(1,55) n RETURNING id) INSERT INTO operator_messages(id,conversation_id,run_id,role,content,created_at) SELECT 'msg_'||id,$1,id,'user','Later message',now()+interval '1 second' FROM runs`, c.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.OperatorMessages(ctx, c.ID, "")
	if err != nil || !page.HasMore {
		t.Fatalf("first page %+v %v", page, err)
	}
	earlier, err := s.OperatorMessages(ctx, c.ID, page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range earlier.Messages {
		if message.ID == reply {
			found = true
			if message.ReplyTo == nil || message.ReplyTo.SenderName != "Reading" || message.ReplyTo.WrenID != w.ID || !message.ReplyTo.CanWake {
				t.Fatalf("paged reply %+v", message.ReplyTo)
			}
		}
	}
	if !found {
		t.Fatal("reply missing from earlier history page")
	}
}
