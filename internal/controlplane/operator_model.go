package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type modelToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type modelToolCallBuilder struct {
	id        string
	typeName  string
	name      strings.Builder
	arguments strings.Builder
}

func (b *modelToolCallBuilder) modelToolCall() modelToolCall {
	return modelToolCall{
		ID:   b.id,
		Type: b.typeName,
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: b.name.String(), Arguments: b.arguments.String()},
	}
}

type modelMessage struct {
	Role             string               `json:"role"`
	Content          string               `json:"content"`
	Attachments      []OperatorAttachment `json:"attachments,omitempty"`
	ToolCalls        []modelToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	ReasoningDetails json.RawMessage      `json:"reasoning_details,omitempty"`
	Usage            *operatorModelUsage  `json:"usage,omitempty"`
}

type operatorModelUsage struct {
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	Cost             *float64 `json:"cost,omitempty"`
	PromptDetails    *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}
type OperatorConfig struct {
	APIKey          string
	ExaAPIKey       string
	exaTransport    http.RoundTripper // Test seam; the production destination is fixed.
	BaseURL         string
	Model           string
	TitleModel      string
	StaticBinary    string
	ShellRunner     string
	ShellNode       string
	ShellSocket     string
	ReasoningEffort string
}

func (c OperatorConfig) Ready() bool { return c.APIKey != "" && c.Model != "" && c.BaseURL != "" }

func (c OperatorConfig) complete(ctx context.Context, messages []modelMessage, tools []mcpTool, onText func(string) error) (modelMessage, error) {
	maxTokens := 4096
	if c.modelReasoningEffort() != "none" {
		maxTokens = 16384
	}
	return c.completeWithLimit(ctx, messages, tools, maxTokens, onText)
}

func (c OperatorConfig) completeWithLimit(ctx context.Context, messages []modelMessage, tools []mcpTool, maxTokens int, onText func(string) error) (out modelMessage, err error) {
	functions := make([]any, 0, len(tools))
	for _, tool := range tools {
		functions = append(functions, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
	}
	payload := make([]map[string]any, len(messages))
	for i, message := range messages {
		payload[i] = operatorModelPayload(message)
	}
	effort := c.modelReasoningEffort()
	reasoning := map[string]any{"effort": effort}
	if effort == "on" {
		reasoning = map[string]any{"enabled": true}
	}
	provider := map[string]any{"require_parameters": true}
	body, err := json.Marshal(map[string]any{"model": c.Model, "messages": payload, "tools": functions, "stream": true, "max_tokens": maxTokens, "reasoning": reasoning, "provider": provider})
	if err != nil {
		return modelMessage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return modelMessage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "Canter workspace operator")
	client := &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return modelMessage{}, ctx.Err()
		}
		return modelMessage{}, fmt.Errorf("could not reach the agent model provider")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return modelMessage{}, fmt.Errorf("model provider returned HTTP %d; check the configured model and provider account", response.StatusCode)
	}
	out = modelMessage{Role: "assistant"}
	var contentParts []string
	contentBytes := 0
	defer func() { out.Content = strings.Join(contentParts, "") }()
	var reasoningDetails []json.RawMessage
	calls := map[int]*modelToolCallBuilder{}
	var order []int
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 2<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	done := false
	lastSent := time.Time{}
	sentContentParts := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Error   json.RawMessage     `json:"error"`
			Usage   *operatorModelUsage `json:"usage"`
			Choices []struct {
				Delta struct {
					Content          string               `json:"content"`
					Attachments      []OperatorAttachment `json:"attachments,omitempty"`
					ReasoningDetails json.RawMessage      `json:"reasoning_details"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err = json.Unmarshal([]byte(data), &chunk); err != nil {
			return out, fmt.Errorf("invalid model stream")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return out, fmt.Errorf("the model provider interrupted the response; please retry")
		}
		if chunk.Usage != nil {
			out.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason == "length" {
				return out, fmt.Errorf("the agent reached its response limit; ask it to continue with a smaller step")
			}
			if choice.Delta.Content != "" {
				contentParts = append(contentParts, choice.Delta.Content)
				contentBytes += len(choice.Delta.Content)
			}
			if len(choice.Delta.ReasoningDetails) > 0 && string(choice.Delta.ReasoningDetails) != "null" {
				var parts []json.RawMessage
				if err = json.Unmarshal(choice.Delta.ReasoningDetails, &parts); err != nil {
					return out, fmt.Errorf("invalid model reasoning stream")
				}
				// Keep every block in order, including signatures for parallel tool
				// calls. Replacing this array loses earlier streamed reasoning.
				reasoningDetails = append(reasoningDetails, parts...)
			}
			for _, part := range choice.Delta.ToolCalls {
				call, ok := calls[part.Index]
				if !ok {
					call = &modelToolCallBuilder{typeName: "function"}
					calls[part.Index] = call
					order = append(order, part.Index)
				}
				if part.ID != "" {
					call.id = part.ID
				}
				_, _ = call.name.WriteString(part.Function.Name)
				_, _ = call.arguments.WriteString(part.Function.Arguments)
			}
		}
		if contentBytes > 0 && sentContentParts < len(contentParts) && time.Since(lastSent) > 300*time.Millisecond {
			// Progress events are retained individually. Send only the newly
			// received text so a bounded provider response cannot be amplified
			// into quadratic event storage.
			if err = onText(strings.Join(contentParts[sentContentParts:], "")); err != nil {
				return out, err
			}
			sentContentParts = len(contentParts)
			lastSent = time.Now()
		}
	}
	if err = scanner.Err(); err != nil {
		return out, fmt.Errorf("model response connection ended unexpectedly")
	}
	if !done {
		return out, fmt.Errorf("model response was incomplete; please retry")
	}
	if sentContentParts < len(contentParts) {
		if err = onText(strings.Join(contentParts[sentContentParts:], "")); err != nil {
			return out, err
		}
	}
	if len(reasoningDetails) > 0 {
		out.ReasoningDetails, _ = json.Marshal(reasoningDetails)
	}
	for _, i := range order {
		call := calls[i].modelToolCall()
		if call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return out, fmt.Errorf("model returned an incomplete tool request")
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	if contentBytes == 0 && len(out.ToolCalls) == 0 {
		return out, fmt.Errorf("the model returned an empty response")
	}
	return out, nil
}
