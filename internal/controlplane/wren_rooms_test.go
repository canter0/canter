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

func TestWrenRoomMentionsRunInMessageOrder(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	first, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_order_first", Name: "First Writer", Purpose: "Draft", Shape: "square"}, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_order_second", Name: "Second+", Purpose: "Review", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_order", Kind: "group", Name: "Order check", WrenIDs: []string{first.ID, second.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "@Second+ draft first.\n@First Writer review that draft. @Second+ owns the draft."
	run, err := s.enqueueOperatorWithRecipients(ctx, origin, "reverse-order", prompt, "test-model", OperatorModelOptions{}, nil, []string{first.ID, second.ID}, false)
	if err != nil || run.Speaker == nil || run.Speaker.ID != second.ID {
		t.Fatalf("first recipient: %+v %v", run, err)
	}
	replay, err := s.enqueueOperatorWithRecipients(ctx, origin, "reverse-order", prompt, "test-model", OperatorModelOptions{}, nil, []string{first.ID, second.ID}, false)
	if err != nil || replay.ID != run.ID {
		t.Fatalf("retry: %+v %v", replay, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE conversation_id=$1`, origin.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate mentions/retry created %d runs: %v", count, err)
	}
	draft, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || draft.ID != run.ID {
		t.Fatalf("draft claim: %+v %v %v", draft, ok, err)
	}
	if _, ok, err := s.claimOperator(ctx); err != nil || ok {
		t.Fatalf("review began before draft completed: %v %v", ok, err)
	}
	if err = s.finishOperator(ctx, draft, "completed", "The launch label is Silver Heron."); err != nil {
		t.Fatal(err)
	}
	review, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || review.Speaker == nil || review.Speaker.ID != first.ID || review.TurnID != draft.TurnID {
		t.Fatalf("review claim: %+v %v %v", review, ok, err)
	}
	history, err := s.operatorHistory(ctx, review, 80)
	if err != nil {
		t.Fatal(err)
	}
	context := operatorRecentContext(history, review)
	if len(context) != 2 || !strings.Contains(context[0].Content, "[Wren Second+]") || !strings.Contains(context[0].Content, "Silver Heron") || !strings.Contains(context[1].Content, "[Current assignment for Wren First Writer]") {
		t.Fatalf("review lacks actual preceding answer: %+v", context)
	}
}

func TestWrenRoomsMembershipAndTeamTurns(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	other, _, _, err := s.Signup(ctx, "room-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	a, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_room_writer", Name: "Writer", Purpose: "Draft the proposal", Shape: "square"}, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_room_reviewer", Name: "Reviewer", Purpose: "Review the draft", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_private", Kind: "group", Name: "Private project", WrenIDs: []string{a.ID, b.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, other.ID, group.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("group leaked: %v", err)
	}
	if rooms, err := s.Rooms(ctx, c.WorkspaceID, other.ID); err != nil || len(rooms) != 0 {
		t.Fatalf("foreign roster: %#v %v", rooms, err)
	}
	group.MemberIDs = []string{other.ID}
	group, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, group, false)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := s.Conversation(ctx, c.WorkspaceID, other.ID, group.ConversationID)
	if err != nil || guest.AccountID != other.ID {
		t.Fatalf("requester authority: %#v %v", guest, err)
	}
	if _, err = s.Wren(ctx, c.WorkspaceID, other.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared private Wren: %v", err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, other.ID, a.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared private DM: %v", err)
	}
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, other.ID, group, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("guest edited roster: %v", err)
	}
	run, err := s.enqueueOperatorWithRecipients(ctx, guest, "room-request-team", "@team draft and review one proposal", "test-model", OperatorModelOptions{}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.Speaker == nil || run.Speaker.ID != a.ID || run.RequesterAccountID != other.ID {
		t.Fatalf("wrong actor: %#v", run)
	}
	retry, err := s.enqueueOperatorWithRecipients(ctx, guest, "room-request-team", "@team draft and review one proposal", "test-model", OperatorModelOptions{}, nil, nil, false)
	if err != nil || retry.ID != run.ID {
		t.Fatalf("retry: %#v %v", retry, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE conversation_id=$1`, guest.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("fanout/retry count %d %v", count, err)
	}
	first, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || first.ID != run.ID || first.Speaker == nil || first.Speaker.ID != a.ID {
		t.Fatalf("first claim: %#v %v %v", first, ok, err)
	}
	if pending, err := s.operatorHasFollowup(ctx, first); err != nil || pending {
		t.Fatalf("teammate incorrectly interrupted first: %v %v", pending, err)
	}
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, group, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("edited live membership: %v", err)
	}
	if err = s.finishOperator(ctx, first, "completed", "Proposal with a blue door."); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || second.Speaker == nil || second.Speaker.ID != b.ID || second.TurnID != first.TurnID {
		t.Fatalf("second claim: %#v %v %v", second, ok, err)
	}
	history, err := s.operatorHistory(ctx, second, 80)
	if err != nil {
		t.Fatal(err)
	}
	transcript := operatorRecentContext(history, second)
	if len(transcript) != 2 || !strings.Contains(transcript[0].Content, "[Wren Writer]") || !strings.Contains(transcript[0].Content, "blue door") || !strings.Contains(transcript[1].Content, "[Current assignment for Wren Reviewer]") || !strings.Contains(transcript[1].Content, "@team draft and review one proposal") {
		t.Fatalf("reviewer lacks shared draft: %#v", transcript)
	}
	contextText, err := s.roomRunContext(ctx, guest, second)
	if err != nil || !strings.Contains(contextText, "You are Reviewer") || !strings.Contains(contextText, "Review the draft") {
		t.Fatalf("identity: %q %v", contextText, err)
	}
	// An expired claim is recovered with the same Wren and requester. A stale
	// worker cannot publish after another runtime has reclaimed its lease.
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || recovered.ID != second.ID || recovered.Speaker == nil || recovered.Speaker.ID != b.ID || recovered.RequesterAccountID != other.ID {
		t.Fatalf("lost team identity on reclaim: %#v %v", recovered, err)
	}
	if err = s.finishOperator(ctx, second, "completed", "stale reply"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker wrote: %v", err)
	}
	second = recovered
	if err = s.finishOperator(ctx, second, "completed", "I reviewed Writer’s blue door proposal."); err != nil {
		t.Fatal(err)
	}
	page, err := s.OperatorMessages(ctx, guest.ID, "")
	if err != nil || len(page.Messages) != 3 || page.Messages[0].Speaker == nil || page.Messages[0].Speaker.ID != b.ID || page.Messages[2].SenderName != "room-other@example.com" {
		t.Fatalf("authorship: %#v %v", page, err)
	}
	// Explicit mention overrides an all-team composer selection.
	addressed, err := s.enqueueOperatorWithRecipients(ctx, guest, "room-request-mention", "@Reviewer check this", "test-model", OperatorModelOptions{}, nil, []string{a.ID, b.ID}, false)
	if err != nil || addressed.Speaker.ID != b.ID {
		t.Fatalf("mention: %#v %v", addressed, err)
	}
	if _, err = s.enqueueOperatorWithRecipients(ctx, guest, "room-request-forged", "hello", "test-model", OperatorModelOptions{}, nil, []string{"wren_foreign"}, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forged recipient: %v", err)
	}
	if _, err = s.enqueueOperatorWithRecipients(ctx, guest, "room-request-context", "hello", "test-model", OperatorModelOptions{}, &OperatorSurface{Kind: "conversation", ID: guest.ID}, nil, false); err == nil {
		t.Fatal("private-context attachment accepted in room")
	}
	if err = s.cancelOperator(ctx, guest.ID); err != nil {
		t.Fatal(err)
	}
	if run, err := s.LatestOperatorRun(ctx, guest.ID); err != nil || run.Status != "cancelled" {
		t.Fatalf("stop: %#v %v", run, err)
	}
	group.MemberIDs = nil
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, group, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, other.ID, group.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked guest still reads: %v", err)
	}
	if _, err = s.enqueueOperatorWithRecipients(ctx, guest, "room-request-revoked", "hello", "test-model", OperatorModelOptions{}, nil, nil, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale guest can send: %v", err)
	}
	channel, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_channel", Kind: "channel", Name: "Product", WrenIDs: []string{a.ID, b.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, other.ID, channel.ConversationID); err != nil {
		t.Fatalf("workspace channel unavailable: %v", err)
	}
	// Membership removal applies to channels and to delayed execution.
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE account_id=$1 AND workspace_id=$2`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, other.ID, channel.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("former member reads channel: %v", err)
	}
	if _, err = s.enqueueOperatorWithRecipients(ctx, guest, "room-request-removed", "hello", "test-model", OperatorModelOptions{}, nil, nil, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed member sends: %v", err)
	}
}

func TestRoomHTTPMembershipAndPrivateHistoryBoundary(t *testing.T) {
	s, c, ownerToken := operatorFixture(t)
	ctx := context.Background()
	guest, _, guestToken, err := s.Signup(ctx, "room-http-guest@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, guest.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	wren, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_http_room", Name: "Researcher", Shape: "flower"}, true)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", BaseURL: "http://unused", Model: "test"}}).(*HTTPServer)
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"id":"room_http_private","kind":"group","name":"Review","wrenIds":["` + wren.ID + `"]}`
	created := request("POST", "/rooms", body, ownerToken)
	if created.Code != 200 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	var room WrenRoom
	if err = json.Unmarshal(created.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	if got := request("GET", "/rooms/"+room.ID, "", guestToken); got.Code != 404 {
		t.Fatalf("outsider reads group: %d", got.Code)
	}
	if got := request("GET", "/conversations/"+room.ConversationID, "", guestToken); got.Code != 404 {
		t.Fatalf("outsider reads transcript: %d", got.Code)
	}
	room.MemberIDs = []string{guest.ID}
	if _, err = s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, room, false); err != nil {
		t.Fatal(err)
	}
	if got := request("GET", "/conversations/"+room.ConversationID, "", guestToken); got.Code != 200 {
		t.Fatalf("member transcript: %d %s", got.Code, got.Body)
	}
	sent := request("POST", "/conversations/"+room.ConversationID+"/messages", `{"requestId":"room-http-send","message":"Please review this","recipientIds":["`+wren.ID+`"]}`, guestToken)
	if sent.Code != 202 {
		t.Fatalf("member send: %d %s", sent.Code, sent.Body)
	}
	var run OperatorRun
	if err = json.Unmarshal(sent.Body.Bytes(), &run); err != nil || run.Speaker == nil || run.Speaker.ID != wren.ID {
		t.Fatalf("run actor: %#v %v", run, err)
	}
	var requester string
	if err = s.pool.QueryRow(ctx, `SELECT requester_account_id FROM operator_runs WHERE id=$1`, run.ID).Scan(&requester); err != nil || requester != guest.ID {
		t.Fatalf("borrowed owner authority: %s %v", requester, err)
	}
	runtime := OperatorRuntime{Server: h}
	for _, name := range []string{"canter_search_history", "canter_read_history"} {
		if _, err = runtime.callTool(ctx, OperatorRun{Speaker: &wren}, c, Principal{}, name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("room reached private history through %s", name)
		}
	}
	if got := request("PATCH", "/conversations/"+room.ConversationID, `{"title":"stolen"}`, guestToken); got.Code != 403 {
		t.Fatalf("guest renamed group via legacy API: %d", got.Code)
	}
	if got := request("POST", "/conversations/"+room.ConversationID+"/stop", "{}", guestToken); got.Code != 200 {
		t.Fatalf("member stop: %d %s", got.Code, got.Body)
	}
	// People can chat even when inference is disconnected. A mention in a
	// deliberate people-only post never wakes a Wren or creates a fake reply.
	h.config.Operator = OperatorConfig{}
	posted := request("POST", "/conversations/"+room.ConversationID+"/messages", `{"requestId":"room-human-post","postOnly":true,"message":"@Researcher this is just a note for people."}`, guestToken)
	if posted.Code != 202 {
		t.Fatalf("human post without model: %d %s", posted.Code, posted.Body)
	}
	var humanRun OperatorRun
	if err = json.Unmarshal(posted.Body.Bytes(), &humanRun); err != nil || humanRun.Status != "completed" || humanRun.Speaker != nil {
		t.Fatalf("human post queued inference: %#v %v", humanRun, err)
	}
	page, err := s.OperatorMessages(ctx, room.ConversationID, "")
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Role != "user" || page.Messages[0].SenderName != "room-http-guest@example.com" {
		t.Fatalf("human post history: %#v %v", page, err)
	}
	if _, ok, err := s.claimOperator(ctx); err != nil || ok {
		t.Fatalf("human post produced background work: %v %v", ok, err)
	}

}
