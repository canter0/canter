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
type modelMessage struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ToolCalls        []modelToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}
type OperatorConfig struct {
	APIKey       string
	BaseURL      string
	Model        string
	StaticBinary string
}

func (c OperatorConfig) Ready() bool { return c.APIKey != "" && c.Model != "" && c.BaseURL != "" }

func (c OperatorConfig) complete(ctx context.Context, messages []modelMessage, tools []mcpTool, onText func(string) error) (modelMessage, error) {
	functions := make([]any, 0, len(tools))
	for _, tool := range tools {
		functions = append(functions, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
	}
	body, err := json.Marshal(map[string]any{"model": c.Model, "messages": messages, "tools": functions, "stream": true, "max_tokens": 4096, "reasoning": map[string]any{"effort": "none"}, "provider": map[string]any{"require_parameters": true}})
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
	out := modelMessage{Role: "assistant"}
	calls := map[int]*modelToolCall{}
	var order []int
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 2<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	done := false
	lastSent := time.Time{}
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
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content          string          `json:"content"`
					ReasoningDetails json.RawMessage `json:"reasoning_details"`
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
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason == "length" {
				return out, fmt.Errorf("the agent reached its response limit; ask it to continue with a smaller step")
			}
			out.Content += choice.Delta.Content
			if len(choice.Delta.ReasoningDetails) > 0 && string(choice.Delta.ReasoningDetails) != "null" {
				out.ReasoningDetails = choice.Delta.ReasoningDetails
			}
			for _, part := range choice.Delta.ToolCalls {
				call, ok := calls[part.Index]
				if !ok {
					call = &modelToolCall{Type: "function"}
					calls[part.Index] = call
					order = append(order, part.Index)
				}
				if part.ID != "" {
					call.ID = part.ID
				}
				call.Function.Name += part.Function.Name
				call.Function.Arguments += part.Function.Arguments
			}
		}
		if out.Content != "" && time.Since(lastSent) > 300*time.Millisecond {
			if err = onText(out.Content); err != nil {
				return out, err
			}
			lastSent = time.Now()
		}
	}
	if err = scanner.Err(); err != nil {
		return out, fmt.Errorf("model response connection ended unexpectedly")
	}
	if !done {
		return out, fmt.Errorf("model response was incomplete; please retry")
	}
	for _, i := range order {
		call := calls[i]
		if call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return out, fmt.Errorf("model returned an incomplete tool request")
		}
		out.ToolCalls = append(out.ToolCalls, *call)
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return out, fmt.Errorf("the model returned an empty response")
	}
	return out, nil
}
