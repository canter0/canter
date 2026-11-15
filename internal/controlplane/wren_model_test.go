package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrenHTTPModelIsServerManaged(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	wren, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_model_policy", Name: "My Wren", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_model_policy", Kind: "channel", Name: "Product", WrenIDs: []string{wren.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", BaseURL: "http://unused", Model: "configured/normal"}})
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/conversations"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, conversation := range []string{wren.ConversationID, room.ConversationID} {
		sent := request("/"+conversation+"/messages", `{"requestId":"managed-forged-model","message":"hello","model":"not-a-permitted-model","modelOptions":{"reasoningEffort":"unbounded"}}`)
		if sent.Code != 202 {
			t.Fatalf("managed request: %d %s", sent.Code, sent.Body)
		}
		var run OperatorRun
		if err = json.Unmarshal(sent.Body.Bytes(), &run); err != nil || run.Model != defaultWrenModel || run.ModelOptions.ReasoningEffort != "" {
			t.Fatalf("model policy bypass: %#v %v", run, err)
		}
	}
	// The legacy collection route can append to an existing Wren conversation;
	// it must enforce exactly the same policy as /messages.
	sent := request("", `{"id":"`+wren.ConversationID+`","requestId":"managed-legacy-route","message":"continue","model":"openai/gpt-6.1-sol","modelOptions":{"reasoningEffort":"max"}}`)
	if sent.Code != 202 {
		t.Fatalf("legacy request: %d %s", sent.Code, sent.Body)
	}
	var created struct {
		Run OperatorRun `json:"run"`
	}
	if err = json.Unmarshal(sent.Body.Bytes(), &created); err != nil || created.Run.Model != defaultWrenModel || created.Run.ModelOptions.ReasoningEffort != "" {
		t.Fatalf("legacy policy bypass: %#v %v", created, err)
	}
}
func TestWrenModelFencesLegacyQueuedPreferences(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	wren, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_old_queued_model", Name: "Wren", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, wren.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EnqueueOperatorWithOptions(ctx, c, "old-model-request", "hello", "openai/gpt-6.1-sol", OperatorModelOptions{ReasoningEffort: "max"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err = s.enforceWrenRunModel(ctx, c, &run, OperatorConfig{WrenModel: "platform/model"}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.LatestOperatorRun(ctx, c.ID)
	if err != nil || saved.Model != "platform/model" || saved.ModelOptions.ReasoningEffort != "" {
		t.Fatalf("legacy preferences retained: %#v %v", saved, err)
	}
	if err = s.cancelOperator(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.enforceWrenRunModel(ctx, c, &run, OperatorConfig{}); err != ErrConflict {
		t.Fatalf("stale model rewrite bypassed lease: %v", err)
	}
}
