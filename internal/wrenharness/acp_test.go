package wrenharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type protocolAgent struct {
	mu               sync.Mutex
	events           []Received
	sent             []Message
	version          int
	failInitialize   bool
	failSession      bool
	promptID         json.RawMessage
	failPromptSend   bool
	waitForCancel    bool
	permissionResult string
}

func (a *protocolAgent) emit(message Message) {
	message.JSONRPC = "2.0"
	a.events = append(a.events, Received{Cursor: int64(len(a.events) + 1), Message: message})
}
func raw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
func (a *protocolAgent) Send(_ context.Context, _ string, data json.RawMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		return err
	}
	a.sent = append(a.sent, message)
	switch message.Method {
	case "initialize":
		version := a.version
		if version == 0 {
			version = 1
		}
		a.emit(Message{ID: message.ID, Result: raw(map[string]any{"protocolVersion": version, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"close": map[string]any{}}}})})
		if a.failInitialize {
			return errors.New("initialize delivery uncertain")
		}
	case "session/new":
		a.emit(Message{ID: message.ID, Result: raw(map[string]string{"sessionId": "session_real_protocol"})})
		if a.failSession {
			return errors.New("session delivery uncertain")
		}
	case "session/load":
		a.emit(Message{ID: message.ID, Result: raw(map[string]any{})})
	case "session/prompt":
		a.promptID = message.ID
		a.emit(Message{Method: "session/update", Params: raw(map[string]any{"sessionId": "session_real_protocol", "update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]string{"type": "text", "text": "private agent working text"}}})})
		a.emit(Message{Method: "session/update", Params: raw(map[string]any{"sessionId": "session_real_protocol", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "I will edit the scoped file."}}})})
		if !a.waitForCancel {
			a.emit(Message{ID: json.RawMessage(`"permission-1"`), Method: "session/request_permission", Params: raw(PermissionRequest{SessionID: "session_real_protocol", ToolCall: raw(map[string]string{"kind": "edit", "path": "/workspace/site/index.html"}), Options: []PermissionOption{{ID: "once", Kind: "allow_once"}, {ID: "always", Kind: "allow_always"}}})})
		}
		if a.failPromptSend {
			return errors.New("connection lost after delivery")
		}
	case "session/cancel":
		if a.waitForCancel {
			a.emit(Message{ID: a.promptID, Result: raw(PromptResult{StopReason: "cancelled"})})
		}
	case "session/close":
		a.emit(Message{ID: message.ID, Result: raw(map[string]any{})})
	case "":
		if string(message.ID) == `"permission-1"` {
			var result struct {
				Outcome struct{ Outcome, OptionID string }
			}
			json.Unmarshal(message.Result, &result)
			a.permissionResult = string(message.Result)
			a.emit(Message{ID: a.promptID, Result: raw(PromptResult{StopReason: "end_turn"})})
		}
	}
	return nil
}
func (a *protocolAgent) Poll(_ context.Context, cursor int64) (Batch, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	events := []Received{}
	for _, event := range a.events {
		if event.Cursor > cursor {
			events = append(events, event)
		}
	}
	return Batch{Events: events, Cursor: int64(len(a.events))}, nil
}
func (a *protocolAgent) Stop(context.Context) error { return nil }
func (a *protocolAgent) promptCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, m := range a.sent {
		if m.Method == "session/prompt" {
			count++
		}
	}
	return count
}
func TestACPActualProtocolLifecycleAndDurableResponseRecovery(t *testing.T) {
	agent := &protocolAgent{}
	var persisted Snapshot
	updates := []string{}
	persist := func(snapshot Snapshot) error { persisted = copySnapshot(snapshot); return nil }
	onUpdate := func(_ context.Context, _ int64, data json.RawMessage) error {
		updates = append(updates, string(data))
		return nil
	}
	permission := func(_ context.Context, request PermissionRequest) (string, error) {
		if request.SessionID != "session_real_protocol" {
			return "", fmt.Errorf("wrong permission scope")
		}
		return "once", nil
	}
	client, err := NewClient(agent, Snapshot{ProcessID: "harness_scope_123"}, persist, onUpdate, permission)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession(ctx, "/workspace/site")
	if err != nil || session == "" {
		t.Fatalf("session: %s %v", session, err)
	}
	id, err := client.StartPrompt(ctx, "Create the requested homepage.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.StartPrompt(ctx, "Concurrent unsupported prompt"); err == nil {
		t.Fatal("concurrent session prompt accepted")
	}
	result, err := client.AwaitPrompt(ctx, id)
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("prompt: %#v %v", result, err)
	}
	if len(updates) != 1 || strings.Contains(updates[0], "private agent working text") {
		t.Fatalf("public stream leaked thought or lost actual output: %#v", updates)
	}
	if !strings.Contains(agent.permissionResult, `"optionId":"once"`) {
		t.Fatalf("permission not bounded to one action: %s", agent.permissionResult)
	}
	if persisted.PromptRequestID != 0 || persisted.Cursor == 0 || len(persisted.Replies) == 0 {
		t.Fatalf("missing recovery state: %#v", persisted)
	}
	// Simulate a control-plane restart after receiving the terminal response but
	// before its caller commits the completed job. No new prompt is issued.
	restored, err := NewClient(agent, persisted, persist, onUpdate, permission)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restored.AwaitPrompt(ctx, id)
	if err != nil || recovered.StopReason != "end_turn" || agent.promptCount() != 1 {
		t.Fatalf("response recovery repeated work: %#v %v count=%d", recovered, err, agent.promptCount())
	}
	if err = restored.Acknowledge(id); err != nil {
		t.Fatal(err)
	}
	if _, found := persisted.Replies[fmt.Sprint(id)]; found {
		t.Fatal("acknowledged response retained")
	}
	if err = client.Steer(ctx, "Change an active command immediately"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("fabricated concurrent steering: %v", err)
	}
	if err = client.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestACPReconnectUncertainPromptAndNeverAutoGrantsAlways(t *testing.T) {
	agent := &protocolAgent{failPromptSend: true}
	snapshot := Snapshot{}
	persist := func(value Snapshot) error { snapshot = copySnapshot(value); return nil }
	client, _ := NewClient(agent, snapshot, persist, nil, func(context.Context, PermissionRequest) (string, error) { return "always", nil })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(ctx, "/workspace"); err != nil {
		t.Fatal(err)
	}
	id, err := client.StartPrompt(ctx, "Edit one file")
	if err == nil || snapshot.PromptRequestID != id {
		t.Fatalf("uncertain delivery lost durable identity: id=%d err=%v", id, err)
	}
	restored, _ := NewClient(agent, snapshot, persist, nil, func(context.Context, PermissionRequest) (string, error) { return "always", nil })
	if _, err = restored.AwaitPrompt(ctx, id); err != nil {
		t.Fatal(err)
	}
	if agent.promptCount() != 1 || strings.Contains(agent.permissionResult, `"selected"`) {
		t.Fatalf("uncertain prompt reran or permanent permission widened: prompts=%d permission=%s", agent.promptCount(), agent.permissionResult)
	}
}
func TestACPCancelRequiresRealStopAndCapabilityChecks(t *testing.T) {
	agent := &protocolAgent{waitForCancel: true}
	persist := func(Snapshot) error { return nil }
	client, _ := NewClient(agent, Snapshot{}, persist, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(ctx, "/workspace"); err != nil {
		t.Fatal(err)
	}
	id, err := client.StartPrompt(ctx, "Work until stopped")
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if client.Snapshot().PromptRequestID != id {
		t.Fatal("cancel notification was falsely treated as completed work")
	}
	result, err := client.AwaitPrompt(ctx, id)
	if err != nil || result.StopReason != "cancelled" {
		t.Fatalf("actual cancellation result: %#v %v", result, err)
	}
	unsupported, _ := NewClient(&protocolAgent{version: 2}, Snapshot{}, persist, nil, nil)
	if err = unsupported.Initialize(ctx); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported version accepted: %v", err)
	}
	noLoad, _ := NewClient(agent, Snapshot{ProtocolVersion: 1}, persist, nil, nil)
	if err = noLoad.LoadSession(ctx, "old", "/workspace"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("invented history loading: %v", err)
	}
	if _, err = client.NewSession(ctx, "/workspace/../etc"); err == nil {
		t.Fatal("ACP session escaped authorized directory")
	}
}
func TestOpenCodeBrokerCredentialNeverUsesMasterProviderKey(t *testing.T) {
	grant := BrokerCredential{BaseURL: "https://broker.example/v1", Token: "wren_broker_abcdefghijklmnopqrstuvwxyz0123456789", Model: "openai/gpt-6-luna", ExpiresAt: time.Now().Add(30 * time.Minute), MaxRequests: 20}
	config, err := OpenCodeConfig(grant)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), grant.Token) || !strings.Contains(string(config), `"enabled_providers":["canter"]`) || !strings.Contains(string(config), "CANTER_MODEL_BROKER_TOKEN") || !strings.Contains(string(config), `"share":"disabled"`) {
		t.Fatalf("unscoped provider config: %s", config)
	}
	for _, token := range []string{"sk-or-v1-master-provider-key", "app-subscription-session-cookie", "short"} {
		invalid := grant
		invalid.Token = token
		if _, err = OpenCodeConfig(invalid); err == nil {
			t.Fatal("accepted unscoped/master credential")
		}
	}
	invalid := grant
	invalid.BaseURL = "http://untrusted.example/v1"
	if _, err = OpenCodeConfig(invalid); err == nil {
		t.Fatal("accepted unencrypted broker transport")
	}
	invalid = grant
	invalid.ExpiresAt = time.Now().Add(24 * time.Hour)
	if _, err = OpenCodeConfig(invalid); err == nil {
		t.Fatal("accepted unbounded broker lifetime")
	}
	if _, err = NewClient(agentNil(), Snapshot{}, nil, nil, nil); err == nil {
		t.Fatal("ACP client without durable persistence accepted")
	}
}
func agentNil() Transport { return nil }

func TestACPRecoversUncertainInitializeAndSessionWithoutRepeating(t *testing.T) {
	agent := &protocolAgent{failInitialize: true, failSession: true}
	snapshot := Snapshot{}
	persist := func(value Snapshot) error { snapshot = copySnapshot(value); return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, _ := NewClient(agent, snapshot, persist, nil, nil)
	if err := first.Initialize(ctx); err == nil {
		t.Fatal("expected uncertain initialize delivery")
	}
	second, _ := NewClient(agent, snapshot, persist, nil, nil)
	if err := second.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := second.NewSession(ctx, "/workspace/site"); err == nil {
		t.Fatal("expected uncertain new-session delivery")
	}
	third, _ := NewClient(agent, snapshot, persist, nil, nil)
	if id, err := third.NewSession(ctx, "/workspace/site"); err != nil || id != "session_real_protocol" {
		t.Fatalf("session recovery: %s %v", id, err)
	}
	counts := map[string]int{}
	for _, message := range agent.sent {
		counts[message.Method]++
	}
	if counts["initialize"] != 1 || counts["session/new"] != 1 {
		t.Fatalf("recovery repeated protocol side effects: %#v", counts)
	}
}

func TestOpenCodeOutputLimitMatchesActualBrokerGrant(t *testing.T) {
	grant := BrokerCredential{BaseURL: "https://broker.example/v1", Token: "wren_broker_abcdefghijklmnopqrstuvwxyz0123456789", Model: "fixed", ExpiresAt: time.Now().Add(30 * time.Minute), MaxRequests: 10, MaxOutputTokens: 8192}
	raw, err := OpenCodeConfig(grant)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Provider map[string]struct {
			Models map[string]struct {
				Limit struct {
					Output int `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err = json.Unmarshal(raw, &config); err != nil || config.Provider["canter"].Models["fixed"].Limit.Output != 8192 {
		t.Fatalf("advertised a contradictory output ceiling: %s %v", raw, err)
	}
	grant.MaxOutputTokens = 16385
	if _, err = OpenCodeConfig(grant); err == nil {
		t.Fatal("accepted output above platform grant bound")
	}
}
