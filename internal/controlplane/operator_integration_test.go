package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func operatorFixture(t *testing.T) (*Store, Conversation, string) {
	t.Helper()
	s := integrationStore(t)
	a, w, token, err := s.Signup(context.Background(), "operator@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation(context.Background(), w.ID, a.ID, "conv_test", "Check my workspace")
	if err != nil {
		t.Fatal(err)
	}
	return s, c, token
}
func operatorTestCall(name, args string) modelToolCall {
	call := modelToolCall{ID: "call_1", Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = args
	return call
}

func TestOperatorConversationMentionUsesOwnedHistory(t *testing.T) {
	s, current, _ := operatorFixture(t)
	ctx := context.Background()
	previous, err := s.CreateConversation(ctx, current.WorkspaceID, current.AccountID, "conv_previous", "Plan the database migration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueOperator(ctx, previous, "request_previous", "Keep the old tables until verification finishes", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueOperator(ctx, current, "request_context", "Use the earlier plan", "test", &OperatorSurface{Kind: "conversation", ID: previous.ID}); err != nil {
		t.Fatal(err)
	}
	selected, err := s.operatorAttachedConversation(ctx, current.WorkspaceID, current.AccountID, previous.ID)
	if err != nil || !strings.Contains(selected, "Keep the old tables until verification finishes") {
		t.Fatalf("selected history missing: %q %v", selected, err)
	}
	if _, err = s.operatorAttachedConversation(ctx, current.WorkspaceID, "another-account", previous.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign account read history: %v", err)
	}
	if _, err = s.EnqueueOperator(ctx, current, "request_foreign", "Read another account", "test", &OperatorSurface{Kind: "conversation", ID: "conv_other"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation attached: %v", err)
	}
}
func TestOperatorIdempotencyRecoveryAndCancellation(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	first, err := s.EnqueueOperator(ctx, c, "request_1", "Show billing", "test", &OperatorSurface{Kind: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.EnqueueOperator(ctx, c, "request_1", "Show billing", "test", nil)
	if err != nil || again.ID != first.ID {
		t.Fatalf("duplicate request created a second run: %v", err)
	}
	if next, err := s.EnqueueOperator(ctx, c, "request_2", "Another message", "test", nil); err != nil || next.Status != "queued" {
		t.Fatalf("follow-up was not queued: %v", err)
	}
	messages, err := s.OperatorMessages(ctx, c.ID)
	if err != nil || len(messages) != 2 || messages[0].Surface == nil || messages[0].Surface.Kind != "billing" {
		t.Fatalf("message or view context was not persisted: %+v %v", messages, err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	run.Steps = 3
	checkpoint := []modelMessage{{Role: "user", Content: "Show billing"}, {Role: "assistant", ToolCalls: []modelToolCall{operatorTestCall("canter_show_billing", "{}")}}}
	if err = s.operatorCheckpoint(ctx, run, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || recovered.Lease == run.Lease || recovered.Steps != 3 || len(recovered.Checkpoint) != 2 {
		t.Fatalf("lease recovery lost state: %+v %v", recovered, err)
	}
	if err = s.finishOperator(ctx, run, "completed", "stale worker"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker could finish: %v", err)
	}
	if err = s.operatorEvent(ctx, run, "text", map[string]string{"content": "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale event accepted: %v", err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)
	runtime := OperatorRuntime{Server: h}
	p, err := s.operatorPrincipal(ctx, c, recovered.ID)
	if err != nil {
		t.Fatal(err)
	}
	call := operatorTestCall("canter_show_billing", "{}")
	result, err := runtime.executeTool(ctx, recovered, c, p, call)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := runtime.executeTool(ctx, recovered, c, p, call)
	var originalValue, cachedValue any
	_ = json.Unmarshal(result, &originalValue)
	_ = json.Unmarshal(cached, &cachedValue)
	if err != nil || !reflect.DeepEqual(originalValue, cachedValue) {
		t.Fatalf("tool replay did not use the stored result: %v", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_events WHERE run_id=$1 AND kind='surface'`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("replay duplicated UI events: %d %v", count, err)
	}
	write := operatorTestCall("canter_prepare_repository_deployment", `{"repository":"mdn/beginner-html-site-styled"}`)
	write.ID = "interrupted_write"
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments) VALUES($1,$2,$3,'{}')`, run.ID, write.ID, write.Function.Name); err != nil {
		t.Fatal(err)
	}
	ambiguous, err := runtime.executeTool(ctx, recovered, c, p, write)
	if err != nil || !strings.Contains(string(ambiguous), "was not repeated") {
		t.Fatalf("ambiguous write was repeated: %s %v", ambiguous, err)
	}
	if err = s.cancelOperator(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.operatorCheckpoint(ctx, recovered, checkpoint); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled worker checkpoint accepted: %v", err)
	}
	if err = s.finishOperator(ctx, recovered, "completed", "late response"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled worker completion accepted: %v", err)
	}
	if _, err = s.EnqueueOperator(ctx, c, "request_3", "Continue", "test", nil); err != nil {
		t.Fatalf("could not continue after stop: %v", err)
	}
}

func TestOperatorHTTPConversationIsolationAndGrantRevocation(t *testing.T) {
	s, c, token := operatorFixture(t)
	ctx := context.Background()
	other, _, otherToken, err := s.Signup(ctx, "other-operator@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", BaseURL: "http://unused", Model: "test"}}).(*HTTPServer)
	request := func(method, path, body, cookie string) int {
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/conversations"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: cookie})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := request("GET", "/"+c.ID, "", otherToken); code != 404 {
		t.Fatalf("another workspace member read private conversation: %d", code)
	}
	if code := request("POST", "/"+c.ID+"/messages", `{"requestId":"request_1","message":"hello"}`, otherToken); code != 404 {
		t.Fatalf("another member continued private conversation: %d", code)
	}
	if code := request("GET", "/"+c.ID, "", token); code != 200 {
		t.Fatalf("owner cannot read conversation: %d", code)
	}
	p, err := s.operatorPrincipal(ctx, c, "run_test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Account != nil || p.Installation == nil || p.Session == nil || p.Actor.Kind != "agent" {
		t.Fatal("hosted operator inherited human identity")
	}
	runtime := OperatorRuntime{Server: h}
	for _, tool := range runtime.tools() {
		if strings.Contains(tool.Name, "authorize") || strings.Contains(tool.Name, "checkout") || strings.Contains(tool.Name, "approve") {
			t.Fatalf("human-only tool exposed to model: %s", tool.Name)
		}
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = runtime.checkPrincipal(ctx, c, p); err != nil || p.Installation.Authority.Draft {
		t.Fatalf("role downgrade did not remove draft authority: %v", err)
	}
	_, _, err = runtime.localTool(ctx, OperatorRun{}, c, p, "canter_prepare_repository_deployment", json.RawMessage(`{"repository":"mdn/beginner-html-site-styled","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"blocked"}`))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer could prepare a deployment: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE agent_installations SET revoked_at=now() WHERE id=$1`, p.Installation.ID); err != nil {
		t.Fatal(err)
	}
	if err = runtime.checkPrincipal(ctx, c, p); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked grant still used: %v", err)
	}
	if _, err = s.operatorPrincipal(ctx, c, "later_run"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked hosted grant recreated: %v", err)
	}
}

// The deterministic provider here verifies protocol and database behavior. The
// separate live verification runs against the configured external model.
func TestOperatorRuntimeExecutesToolAndPersistsFollowup(t *testing.T) {
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	// This test counts main-model calls; title generation is covered separately.
	if _, err := s.RenameConversation(ctx, c.WorkspaceID, c.AccountID, c.ID, c.Title); err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages  []modelMessage `json:"messages"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		calls++
		if request.Reasoning.Effort != "none" {
			t.Error("tool calling must explicitly disable unsupported reasoning")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(delta any) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if calls == 1 {
			emit(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "billing_1", "type": "function", "function": map[string]string{"name": "canter_show_billing", "arguments": "{}"}}}})
		} else {
			if calls == 2 {
				last := request.Messages[len(request.Messages)-1]
				if last.Role != "tool" || last.ToolCallID != "billing_1" || !strings.Contains(last.Content, "not_started") {
					t.Error("actual billing result was not returned to model")
				}
			}
			if calls == 3 {
				if len(request.Messages) < 4 || request.Messages[len(request.Messages)-2].Content != "Your billing period has not started." {
					t.Error("follow-up lost conversation history")
				}
			}
			emit(map[string]string{"content": "Your billing period has not started."})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	config := OperatorConfig{APIKey: "protocol-test", Model: "test", BaseURL: provider.URL}
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer), Config: config}
	for index, prompt := range []string{"Show billing", "And what does that mean?"} {
		if _, err := s.EnqueueOperator(ctx, c, fmt.Sprintf("request_%d", index), prompt, "test", nil); err != nil {
			t.Fatal(err)
		}
		run, ok, err := s.claimOperator(ctx)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if err = runtime.work(ctx, &run); err != nil {
			t.Fatal(err)
		}
		latest, err := s.LatestOperatorRun(ctx, c.ID)
		if err != nil || latest.Status != "completed" {
			t.Fatalf("run did not complete: %+v %v", latest, err)
		}
	}
	messages, err := s.OperatorMessages(ctx, c.ID)
	if err != nil || len(messages) != 4 || calls != 3 {
		t.Fatalf("conversation not durably continued: messages=%d calls=%d err=%v", len(messages), calls, err)
	}
}
