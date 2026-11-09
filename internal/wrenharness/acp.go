// Package wrenharness speaks the real Agent Client Protocol to a coding agent
// running in an authorized Wren computer. It does not implement a second model
// loop or repurpose a consumer subscription's authentication credentials.
package wrenharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

var ErrUnsupported = errors.New("harness capability is not supported")

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("ACP %d: %s", e.Code, e.Message) }

type Received struct {
	Cursor  int64
	Message Message
}
type Batch struct {
	Events []Received
	Cursor int64
}
type Transport interface {
	Send(context.Context, string, json.RawMessage) error
	Poll(context.Context, int64) (Batch, error)
	Stop(context.Context) error
}
type AgentCapabilities struct {
	LoadSession         bool                       `json:"loadSession"`
	PromptCapabilities  map[string]bool            `json:"promptCapabilities,omitempty"`
	SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities,omitempty"`
}
type PendingRequest struct {
	ID     int64           `json:"id"`
	Params json.RawMessage `json:"params"`
}
type Snapshot struct {
	Pending         map[string]PendingRequest `json:"pending,omitempty"`
	Replies         map[string]Message        `json:"replies,omitempty"`
	ProtocolVersion int                       `json:"protocolVersion"`
	ProcessID       string                    `json:"processId"`
	SessionID       string                    `json:"sessionId"`
	Directory       string                    `json:"directory"`
	Capabilities    AgentCapabilities         `json:"capabilities"`
	NextRequestID   int64                     `json:"nextRequestId"`
	NextDeliveryID  int64                     `json:"nextDeliveryId"`
	Cursor          int64                     `json:"cursor"`
	PromptRequestID int64                     `json:"promptRequestId,omitempty"`
}
type SessionUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind       string          `json:"sessionUpdate"`
		Content    json.RawMessage `json:"content,omitempty"`
		ToolCallID string          `json:"toolCallId,omitempty"`
		Status     string          `json:"status,omitempty"`
	} `json:"update"`
}
type PermissionOption struct {
	ID   string `json:"optionId"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}
type PromptResult struct {
	StopReason string `json:"stopReason"`
}
type Client struct {
	transport  Transport
	mu         sync.Mutex
	reader     sync.Mutex
	snapshot   Snapshot
	persist    func(Snapshot) error
	onUpdate   func(context.Context, int64, json.RawMessage) error
	permission func(context.Context, PermissionRequest) (string, error)
	results    map[string]Message
}

// NewClient requires durable snapshot persistence. Restore a snapshot only for
// its original process; if that process was lost, start/initialize a new process
// and LoadSession only when the agent advertises support for it.
func NewClient(transport Transport, snapshot Snapshot, persist func(Snapshot) error, onUpdate func(context.Context, int64, json.RawMessage) error, permission func(context.Context, PermissionRequest) (string, error)) (*Client, error) {
	if transport == nil || persist == nil {
		return nil, fmt.Errorf("ACP transport and durable snapshot persistence are required")
	}
	if snapshot.NextRequestID < 1 {
		snapshot.NextRequestID = 1
	}
	if snapshot.NextDeliveryID < 1 {
		snapshot.NextDeliveryID = 1
	}
	if snapshot.Pending == nil {
		snapshot.Pending = map[string]PendingRequest{}
	}
	if snapshot.Replies == nil {
		snapshot.Replies = map[string]Message{}
	}
	return &Client{transport: transport, snapshot: snapshot, persist: persist, onUpdate: onUpdate, permission: permission, results: snapshot.Replies}, nil
}
func (c *Client) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copySnapshot(c.snapshot)
}
func (c *Client) send(ctx context.Context, method string, params any, request bool) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	message := Message{JSONRPC: "2.0", Method: method}
	raw, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}
	message.Params = raw
	if request && method != "session/prompt" {
		if pending, ok := c.snapshot.Pending[method]; ok {
			if !bytes.Equal(pending.Params, raw) {
				return 0, fmt.Errorf("an earlier ACP request is unresolved; inspect it before changing parameters")
			}
			return pending.ID, nil
		}
	}
	id := int64(0)
	if request {
		id = c.snapshot.NextRequestID
		c.snapshot.NextRequestID++
		message.ID = json.RawMessage(strconv.FormatInt(id, 10))
	}
	delivery := c.snapshot.NextDeliveryID
	c.snapshot.NextDeliveryID++
	if request && method != "session/prompt" {
		c.snapshot.Pending[method] = PendingRequest{ID: id, Params: raw}
	}
	if method == "session/prompt" {
		if c.snapshot.PromptRequestID != 0 {
			return 0, fmt.Errorf("this harness session already has an active prompt")
		}
		c.snapshot.PromptRequestID = id
	}
	if err = c.persist(copySnapshot(c.snapshot)); err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return 0, err
	}
	// A transport error is an uncertain send. Snapshot retains the prompt ID;
	// callers must inspect/reconnect, not silently issue another coding turn.
	if err = c.transport.Send(ctx, fmt.Sprintf("%020d", delivery), encoded); err != nil {
		return id, err
	}
	return id, nil
}
func (c *Client) Initialize(ctx context.Context) error {
	if c.Snapshot().ProtocolVersion != 0 {
		return fmt.Errorf("ACP connection is already initialized")
	}
	id, err := c.send(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "canter", "title": "Canter", "version": "wren-acp-v1"}}, true)
	if err != nil {
		return err
	}
	var result struct {
		ProtocolVersion int               `json:"protocolVersion"`
		Capabilities    AgentCapabilities `json:"agentCapabilities"`
	}
	if err = c.await(ctx, id, &result); err != nil {
		return err
	}
	if result.ProtocolVersion != 1 {
		return fmt.Errorf("%w: ACP protocol version %d", ErrUnsupported, result.ProtocolVersion)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.snapshot.Pending, "initialize")
	c.snapshot.ProtocolVersion = 1
	c.snapshot.Capabilities = result.Capabilities
	return c.persist(copySnapshot(c.snapshot))
}
func (c *Client) NewSession(ctx context.Context, directory string) (string, error) {
	return c.setup(ctx, "session/new", "", directory)
}
func (c *Client) LoadSession(ctx context.Context, id, directory string) error {
	if !c.Snapshot().Capabilities.LoadSession {
		return fmt.Errorf("%w: session history loading", ErrUnsupported)
	}
	_, err := c.setup(ctx, "session/load", id, directory)
	return err
}
func (c *Client) setup(ctx context.Context, method, session, directory string) (string, error) {
	snapshot := c.Snapshot()
	if snapshot.ProtocolVersion != 1 {
		return "", fmt.Errorf("initialize the ACP connection first")
	}
	if snapshot.PromptRequestID != 0 {
		return "", fmt.Errorf("finish or cancel the active prompt before changing sessions")
	}
	if !validDirectory(directory) {
		return "", fmt.Errorf("harness working directory must stay inside /workspace")
	}
	params := map[string]any{"cwd": directory, "mcpServers": []any{}}
	if session != "" {
		params["sessionId"] = session
	}
	id, err := c.send(ctx, method, params, true)
	if err != nil {
		return "", err
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err = c.await(ctx, id, &result); err != nil {
		return "", err
	}
	if result.SessionID == "" {
		result.SessionID = session
	}
	if result.SessionID == "" {
		return "", fmt.Errorf("ACP agent returned no session identity")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.snapshot.Pending, method)
	c.snapshot.SessionID = result.SessionID
	c.snapshot.Directory = directory
	return result.SessionID, c.persist(copySnapshot(c.snapshot))
}
func (c *Client) StartPrompt(ctx context.Context, text string) (int64, error) {
	snapshot := c.Snapshot()
	if snapshot.ProtocolVersion != 1 || snapshot.SessionID == "" {
		return 0, fmt.Errorf("create or load an ACP session first")
	}
	if len(text) == 0 || len(text) > 64000 {
		return 0, fmt.Errorf("a coding prompt of up to 64000 bytes is required")
	}
	return c.send(ctx, "session/prompt", map[string]any{"sessionId": snapshot.SessionID, "prompt": []map[string]string{{"type": "text", "text": text}}}, true)
}
func (c *Client) AwaitPrompt(ctx context.Context, id int64) (PromptResult, error) {
	var result PromptResult
	err := c.await(ctx, id, &result)
	if err == nil || isRPCError(err) {
		c.mu.Lock()
		if c.snapshot.PromptRequestID == id {
			c.snapshot.PromptRequestID = 0
			if e := c.persist(copySnapshot(c.snapshot)); err == nil {
				err = e
			}
		}
		c.mu.Unlock()
	}
	return result, err
}
func isRPCError(err error) bool { var rpc *RPCError; return errors.As(err, &rpc) }
func (c *Client) Prompt(ctx context.Context, text string) (PromptResult, error) {
	id, err := c.StartPrompt(ctx, text)
	if err != nil {
		return PromptResult{}, err
	}
	return c.AwaitPrompt(ctx, id)
}

// Cancel acknowledges only that the notification was queued. The caller waits
// for AwaitPrompt's cancelled stopReason before claiming execution has stopped.
func (c *Client) Cancel(ctx context.Context) error {
	s := c.Snapshot()
	if s.SessionID == "" {
		return fmt.Errorf("no ACP session is active")
	}
	_, err := c.send(ctx, "session/cancel", map[string]string{"sessionId": s.SessionID}, false)
	return err
}
func (c *Client) Steer(context.Context, string) error {
	return fmt.Errorf("%w: this ACP v1 harness requires cancellation and a follow-up prompt; it has no concurrent steering", ErrUnsupported)
}
func (c *Client) Close(ctx context.Context) error {
	s := c.Snapshot()
	if _, ok := s.Capabilities.SessionCapabilities["close"]; ok && s.SessionID != "" {
		id, err := c.send(ctx, "session/close", map[string]string{"sessionId": s.SessionID}, true)
		if err != nil {
			return err
		}
		if err = c.await(ctx, id, nil); err != nil {
			return err
		}
	}
	return c.transport.Stop(ctx)
}
func (c *Client) await(ctx context.Context, id int64, out any) error {
	c.reader.Lock()
	defer c.reader.Unlock()
	key := strconv.FormatInt(id, 10)
	for {
		c.mu.Lock()
		message, found := c.results[key]

		cursor := c.snapshot.Cursor
		c.mu.Unlock()
		if found {
			if message.Error != nil {
				return message.Error
			}
			if out != nil && json.Unmarshal(message.Result, out) != nil {
				return fmt.Errorf("ACP response has an invalid result")
			}
			return nil
		}
		batch, err := c.transport.Poll(ctx, cursor)
		if err != nil {
			return err
		}
		for _, event := range batch.Events {
			if err = c.receive(ctx, event); err != nil {
				return err
			}
		}
		if len(batch.Events) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}
func (c *Client) receive(ctx context.Context, event Received) error {
	message := event.Message
	if message.JSONRPC != "2.0" {
		return fmt.Errorf("agent returned an invalid ACP message")
	}
	if message.Method == "session/update" {
		var update SessionUpdate
		if json.Unmarshal(message.Params, &update) != nil {
			return fmt.Errorf("agent returned an invalid session update")
		}
		if update.Update.Kind != "agent_thought_chunk" && c.onUpdate != nil {
			if err := c.onUpdate(ctx, event.Cursor, message.Params); err != nil {
				return err
			}
		}
	} else if len(message.ID) > 0 && message.Method != "" {
		response := Message{JSONRPC: "2.0", ID: message.ID}
		if message.Method == "session/request_permission" {
			var request PermissionRequest
			if json.Unmarshal(message.Params, &request) != nil {
				return fmt.Errorf("invalid ACP permission request")
			}
			selected := ""
			var err error
			if c.permission != nil {
				selected, err = c.permission(ctx, request)
				if err != nil {
					return err
				}
			}
			allowed := false
			for _, option := range request.Options {
				if option.ID == selected && option.Kind == "allow_once" {
					allowed = true
				}
			}
			result := map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
			if allowed {
				result["outcome"] = map[string]string{"outcome": "selected", "optionId": selected}
			}
			response.Result, _ = json.Marshal(result)
		} else {
			response.Error = &RPCError{Code: -32601, Message: "Client capability was not advertised"}
		}
		c.mu.Lock()
		delivery := c.snapshot.NextDeliveryID
		c.snapshot.NextDeliveryID++
		err := c.persist(copySnapshot(c.snapshot))
		c.mu.Unlock()
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(response)
		if err = c.transport.Send(ctx, fmt.Sprintf("%020d", delivery), encoded); err != nil {
			return err
		}
	} else if len(message.ID) > 0 {
		c.mu.Lock()
		c.results[string(message.ID)] = message
		c.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if event.Cursor > c.snapshot.Cursor {
		c.snapshot.Cursor = event.Cursor
	}
	return c.persist(copySnapshot(c.snapshot))
}

func copySnapshot(value Snapshot) Snapshot {
	raw, _ := json.Marshal(value)
	var clone Snapshot
	_ = json.Unmarshal(raw, &clone)
	return clone
}

// Acknowledge discards a saved response only after its result has been durably
// folded into the owning Canter job. Keeping it until then makes a crash after
// receipt but before the caller's commit recoverable without another prompt.
func (c *Client) Acknowledge(id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.results, strconv.FormatInt(id, 10))
	return c.persist(copySnapshot(c.snapshot))
}
