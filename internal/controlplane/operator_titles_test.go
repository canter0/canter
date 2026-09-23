package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func titleResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": content}}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
}

func TestConversationTitleSmallRequestAndValidation(t *testing.T) {
	for _, output := range []string{"\"Add conversation rename and delete\"", "", "Title\nExplanation", strings.Repeat("word ", 20)} {
		t.Run(output, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model     string         `json:"model"`
					MaxTokens int            `json:"max_tokens"`
					Messages  []modelMessage `json:"messages"`
					Tools     []any          `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Model != "small-title-model" || request.MaxTokens != 64 || len(request.Tools) != 0 || len(request.Messages) != 2 {
					t.Errorf("unexpected title request: %+v", request)
				}
				var input map[string]string
				if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
					t.Error(err)
				}
				if len([]rune(input["request"])) != 6000 || len([]rune(input["assistant_reply"])) != 1500 {
					t.Error("title input was not bounded")
				}
				titleResponse(w, output)
			}))
			defer provider.Close()
			title, err := (OperatorConfig{Model: "main", TitleModel: "small-title-model", APIKey: "test", BaseURL: provider.URL}).conversationTitle(context.Background(), strings.Repeat("你", 7000), strings.Repeat("a", 2000))
			if strings.HasPrefix(output, "\"") {
				if err != nil || title != "Add conversation rename and delete" {
					t.Fatalf("title=%q err=%v", title, err)
				}
			} else if err == nil {
				t.Fatalf("invalid output accepted: %q", title)
			}
		})
	}
}

func TestConversationRenameDeleteOwnershipAndTitleRaces(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	other, _, otherToken, err := s.Signup(ctx, "title-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	request := func(method, body, cookie, origin string) int {
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/conversations/"+c.ID, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: cookie})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		if code := request(method, `{"title":"Stolen"}`, otherToken, "http://canter.test"); code != 404 {
			t.Fatalf("other member %s: %d", method, code)
		}
		if code := request(method, `{"title":"Stolen"}`, token, "https://other.test"); code != 403 {
			t.Fatalf("cross-origin %s: %d", method, code)
		}
	}
	for _, title := range []string{" ", strings.Repeat("x", 101)} {
		body, _ := json.Marshal(map[string]string{"title": title})
		if code := request("PATCH", string(body), token, "http://canter.test"); code != 400 {
			t.Fatalf("invalid title accepted: %d", code)
		}
	}
	if _, err = s.EnqueueOperator(ctx, c, "request_title", "Check billing please", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if prompt, err := s.claimConversationTitle(ctx, run); err != nil || prompt != "Check billing please" {
		t.Fatalf("claim: %q %v", prompt, err)
	}
	if _, err = s.claimConversationTitle(ctx, run); err == nil {
		t.Fatal("duplicate title claim succeeded")
	}
	if code := request("PATCH", `{"title":"  My   chosen title  "}`, token, "http://canter.test"); code != 200 {
		t.Fatalf("rename: %d", code)
	}
	if err = s.saveGeneratedConversationTitle(ctx, c.ID, "Automatic title"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil || got.Title != "My chosen title" {
		t.Fatalf("manual title overwritten: %+v %v", got, err)
	}
	events, err := s.OperatorEvents(ctx, c.ID, 0)
	if err != nil || len(events) != 2 || events[1].Kind != "title" {
		t.Fatalf("missing rename event: %+v %v", events, err)
	}
	if code := request("DELETE", "", token, "http://canter.test"); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if err = s.operatorCheckpoint(ctx, run, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleted worker kept lease: %v", err)
	}
	if err = s.saveGeneratedConversationTitle(ctx, c.ID, "Late title"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted conversation returned: %v", err)
	}
	for _, table := range []string{"operator_runs", "operator_messages", "operator_events"} {
		var count int
		if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE conversation_id=$1", c.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s not removed: %d %v", table, count, err)
		}
	}
}

func TestConversationTitleUsesFirstPreambleWithoutBlockingMainReply(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	var titleCalls, mainCalls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseTitle := sync.OnceFunc(func() { close(release) })
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MaxTokens int            `json:"max_tokens"`
			Messages  []modelMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.MaxTokens == 64 {
			titleCalls.Add(1)
			if !strings.Contains(request.Messages[1].Content, "I'll check your workspace billing") || strings.Contains(request.Messages[1].Content, "not started") {
				t.Error("title did not use first preamble")
			}
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			titleResponse(w, "Check workspace billing")
			return
		}
		if mainCalls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"I'll check your workspace billing.\",\"tool_calls\":[{\"index\":0,\"id\":\"billing\",\"type\":\"function\",\"function\":{\"name\":\"canter_show_billing\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n")
		} else {
			titleResponse(w, "Your billing period has not started.")
		}
	}))
	defer provider.Close()
	defer releaseTitle()
	config := OperatorConfig{Model: "test", APIKey: "test", BaseURL: provider.URL}
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer), Config: config}
	if _, err := s.EnqueueOperator(ctx, c, "title_first", "Check my workspace", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.work(ctx, &run) }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("title blocked main reply")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("title was never requested")
	}
	got, _ := s.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if got.Title != c.Title {
		t.Fatal("initial snippet not preserved while generating")
	}
	releaseTitle()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID)
		if err == nil && got.Title == "Check workspace billing" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generated title was not saved: %+v %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = s.EnqueueOperator(ctx, c, "title_followup", "Explain that", "test", nil); err != nil {
		t.Fatal(err)
	}
	next, _, _ := s.claimOperator(ctx)
	if err = runtime.work(ctx, &next); err != nil {
		t.Fatal(err)
	}
	if titleCalls.Load() != 1 {
		t.Fatalf("generated %d titles", titleCalls.Load())
	}
}

func TestGeneratedConversationTitlePublishesAndKeepsRecency(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE operator_conversations SET title_state='requested' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.saveGeneratedConversationTitle(ctx, c.ID, "Check workspace billing"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil || got.Title != "Check workspace billing" || !got.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("title or recency: %+v %v", got, err)
	}
	events, err := s.OperatorEvents(ctx, c.ID, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "title" {
		t.Fatalf("title event: %+v %v", events, err)
	}
}
