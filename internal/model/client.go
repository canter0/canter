package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const DefaultModel = "openai/gpt-5.6-luna"

const maxModelResponseBytes = 1 << 20

type Client struct {
	HTTP *http.Client
}

type ProbeResult struct {
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"latency"`
	Model   string        `json:"model,omitempty"`
	Error   string        `json:"error,omitempty"`
}

type completionRequest struct {
	Model          string            `json:"model"`
	Messages       []message         `json:"messages"`
	Temperature    int               `json:"temperature"`
	MaxTokens      int               `json:"max_tokens"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
}

func (c Client) Probe(ctx context.Context) ProbeResult {
	start := time.Now()
	var raw json.RawMessage
	modelName, err := c.complete(ctx, "Return a JSON object with exactly one field named ok whose value is true.", &raw)
	result := ProbeResult{Latency: time.Since(start), Model: modelName}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	var value struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || !value.OK {
		result.Error = "model did not return the required probe object"
		return result
	}
	result.OK = true
	return result
}

func (c Client) Compile(ctx context.Context, prompt string, target any) (string, int64, error) {
	start := time.Now()
	modelName, err := c.complete(ctx, prompt, target)
	if err != nil {
		return "", 0, err
	}
	return modelName, time.Since(start).Milliseconds(), nil
}

func (c Client) complete(ctx context.Context, prompt string, target any) (string, error) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return "", fmt.Errorf("OPENROUTER_API_KEY is not set")
	}
	body, _ := json.Marshal(completionRequest{
		Model: DefaultModel, Temperature: 0, MaxTokens: 800,
		Messages:       []message{{Role: "user", Content: prompt}},
		ResponseFormat: map[string]string{"type": "json_object"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 18 * time.Second}
	}
	// Keep injected transports and timeouts, but never let a provider redirect
	// this credential-bearing request to another host.
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxModelResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read model response")
	}
	if len(responseBody) > maxModelResponseBytes {
		return "", fmt.Errorf("model response exceeded the size limit")
	}
	if resp.StatusCode != http.StatusOK {
		// Provider error messages can echo request contents or other sensitive
		// data, so expose only the status code.
		return "", fmt.Errorf("model request failed with HTTP %d", resp.StatusCode)
	}
	var decoded completionResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("decode model response: %w", err)
	}
	if len(decoded.Choices) != 1 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("model returned no result")
	}
	result := strings.NewReader(decoded.Choices[0].Message.Content)
	decoder := json.NewDecoder(result)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return "", fmt.Errorf("model returned invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("model returned trailing JSON data")
	}
	return decoded.Model, nil
}
