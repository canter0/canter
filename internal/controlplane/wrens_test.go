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

func TestWrenValidation(t *testing.T) {
	valid := Wren{ID: "wren_example", Name: "  My Wren  ", Shape: "pebble", Palette: 0}
	if err := validateWren(&valid); err != nil || valid.Name != "My Wren" {
		t.Fatalf("valid profile: %v %#v", err, valid)
	}
	for _, invalid := range []Wren{{ID: "wren_invalid/path", Name: "Wren", Shape: "pebble"}, {ID: "not-wren", Name: "Wren", Shape: "pebble"}, {ID: "wren_example", Name: "", Shape: "pebble"}, {ID: "wren_example", Name: "Wren", Shape: "unknown"}, {ID: "wren_example", Name: "Wren", Shape: "pebble", Palette: 8}, {ID: "wren_example", Name: "Wren", Shape: "pebble", Purpose: strings.Repeat("x", 3001)}} {
		if validateWren(&invalid) == nil {
			t.Fatalf("accepted invalid profile: %#v", invalid)
		}
	}
}
func TestWrensHTTPPersistenceAndIsolation(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	other, _, otherToken, err := s.Signup(ctx, "other-wren@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	request := func(method, path, body, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/wrens"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: cookie})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"id":"wren_example","name":"My researcher","purpose":"Help me understand distributed systems","shape":"flower","palette":3,"role":"Research","section":"My project"}`
	created := request("POST", "", body, token)
	if created.Code != 200 {
		t.Fatalf("create %d %s", created.Code, created.Body)
	}
	var wren Wren
	if err = json.Unmarshal(created.Body.Bytes(), &wren); err != nil {
		t.Fatal(err)
	}
	if wren.ConversationID == "" || wren.Status != "idle" || wren.Role != "Research" || wren.Section != "My project" {
		t.Fatalf("new Wren: %#v", wren)
	}
	if code := request("POST", "", body, token).Code; code != 200 {
		t.Fatalf("idempotent retry %d", code)
	}
	list, err := s.Wrens(ctx, c.WorkspaceID, c.AccountID)
	if err != nil || len(list) != 1 {
		t.Fatalf("retry duplicated: %#v %v", list, err)
	}
	conversation, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, wren.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := s.Conversations(ctx, c.WorkspaceID, c.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == wren.ConversationID {
			t.Fatalf("Wren chat listed as a session: %#v", session)
		}
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, other.ID, wren.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared private history: %v", err)
	}
	profile, err := s.wrenContext(ctx, conversation)
	if err != nil || !strings.Contains(profile, wren.Purpose) {
		t.Fatalf("role context missing: %q %v", profile, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,status,model) VALUES('wren_run',$1,'request_wren','running','test')`, conversation.ID); err != nil {
		t.Fatal(err)
	}
	items, err := s.Wrens(ctx, c.WorkspaceID, c.AccountID)
	if err != nil || items[0].Status != "running" {
		t.Fatalf("activity not real: %#v %v", items, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES('wren_message',$1,'wren_run','assistant','A real saved conversation result.')`, conversation.ID); err != nil {
		t.Fatal(err)
	}
	previews, err := s.Wrens(ctx, c.WorkspaceID, c.AccountID)
	if err != nil || previews[0].LastMessage != "A real saved conversation result." || previews[0].LastMessageAt == nil {
		t.Fatalf("message preview missing: %#v %v", previews, err)
	}
	if code := request("GET", "/wren_example", "", otherToken).Code; code != 404 {
		t.Fatalf("foreign read %d", code)
	}
	if code := request("PATCH", "/wren_example", body, otherToken).Code; code != 403 {
		t.Fatalf("foreign update %d", code)
	}
	if code := request("POST", "", body, otherToken).Code; code != 403 {
		t.Fatalf("foreign ID collision %d", code)
	}
	foreignList := request("GET", "", "", otherToken)
	if foreignList.Code != 200 || !strings.Contains(foreignList.Body.String(), `"wrens":[]`) {
		t.Fatalf("foreign list %d %s", foreignList.Code, foreignList.Body)
	}
	edited := request("PATCH", "/wren_example", `{"name":"My renamed Wren","purpose":"Help with my notes","shape":"square","palette":1}`, token)
	if edited.Code != 200 {
		t.Fatalf("edit %d %s", edited.Code, edited.Body)
	}
	saved, err := s.Wren(ctx, c.WorkspaceID, c.AccountID, wren.ID)
	if err != nil || saved.Name != "My renamed Wren" || saved.ConversationID != conversation.ID {
		t.Fatalf("edit lost conversation: %#v %v", saved, err)
	}
	if text, err := s.wrenContext(ctx, conversation); err != nil || !strings.Contains(text, "Help with my notes") {
		t.Fatalf("changed purpose not used: %q %v", text, err)
	}
	// Deleting a normal chat must not leave a broken Wren entry behind.
	if _, err = s.pool.Exec(ctx, `DELETE FROM operator_conversations WHERE id=$1`, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wren(ctx, c.WorkspaceID, c.AccountID, wren.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan Wren: %v", err)
	}
}
