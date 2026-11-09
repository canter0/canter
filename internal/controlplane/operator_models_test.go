package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelectedOperatorModel(t *testing.T) {
	for _, requested := range []string{"", "configured/custom", "openai/gpt-6-luna", "openai/gpt-6.1-sol", "z-ai/glm-5.3-flash", "deepseek/deepseek-v4.1-flash", "qwen/qwen3.8-flash", "google/gemini-3.8-flash"} {
		got, err := selectedOperatorModel(requested, "configured/custom")
		want := requested
		if want == "" {
			want = "configured/custom"
		}
		if err != nil || got != want {
			t.Fatalf("%q: got %q, %v", requested, got, err)
		}
	}
	for _, requested := range []string{"openai/gpt-6-astra", "anthropic/claude-sonnet-5", "openai/gpt-6.1-sol:extended", " gpt-6.1-sol", "https://example.com/model"} {
		if _, err := selectedOperatorModel(requested, "configured/custom"); err == nil {
			t.Fatalf("unexpected model allowed: %q", requested)
		}
	}
}

func TestOperatorHTTPModelSelectionIsManagedAcrossLegacyRoutes(t *testing.T) {
	s, c, token := operatorFixture(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", BaseURL: "http://unused", Model: "old/default", WrenModel: "platform/managed"}})
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+c.WorkspaceID+"/conversations"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	created := request("", `{"id":"conv_managed_legacy","requestId":"managed-legacy-new","message":"hello","model":"openai/gpt-6.1-sol","modelOptions":{"reasoningEffort":"max"}}`)
	if created.Code != 202 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	var initial struct {
		Run OperatorRun `json:"run"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &initial); err != nil || initial.Run.Model != "platform/managed" || initial.Run.ModelOptions.ReasoningEffort != "" {
		t.Fatalf("client model/options escaped policy: %#v %v", initial, err)
	}
	for i, model := range []string{"unlisted/model", "google/gemini-3.8-flash", ""} {
		body, _ := json.Marshal(map[string]any{"requestId": fmt.Sprintf("managed-followup-%d", i), "message": "continue", "model": model, "modelOptions": OperatorModelOptions{ReasoningEffort: "invalid"}})
		sent := request("/conv_managed_legacy/messages", string(body))
		if sent.Code != 202 {
			t.Fatalf("followup: %d %s", sent.Code, sent.Body)
		}
		var run OperatorRun
		if err := json.Unmarshal(sent.Body.Bytes(), &run); err != nil || run.Model != "platform/managed" || run.ModelOptions.ReasoningEffort != "" {
			t.Fatalf("followup bypass: %#v %v", run, err)
		}
	}
	repeated := request("/conv_managed_legacy/messages", `{"requestId":"managed-legacy-new","message":"hello","model":"another/model"}`)
	var replay OperatorRun
	if err := json.Unmarshal(repeated.Body.Bytes(), &replay); repeated.Code != 202 || err != nil || replay.ID != initial.Run.ID || replay.Model != initial.Run.Model {
		t.Fatalf("retry altered managed run: %d %s", repeated.Code, repeated.Body)
	}
	claimed, ok, err := s.claimOperator(context.Background())
	if err != nil || !ok || claimed.ID != initial.Run.ID || claimed.Model != "platform/managed" {
		t.Fatalf("claim lost policy: %#v %v", claimed, err)
	}
}

func TestOperatorModelSettingsProviderPayload(t *testing.T) {
	for _, tc := range []struct {
		model, effort, want string
	}{
		{"openai/gpt-6-luna", "none", "none"},
		{"openai/gpt-6.1-sol", "", "low"},
		// Persisted runs and configured defaults may still disable reasoning.
		{"openai/gpt-6.1-sol", "none", "low"},
		{"openai/gpt-6.1-sol", "high", "high"},
		{"z-ai/glm-5.3-flash", "", "low"},
		{"google/gemini-3.8-flash", "medium", "medium"},
		{"qwen/qwen3.8-flash", "on", "on"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var input struct {
					Reasoning map[string]any `json:"reasoning"`
					Provider  map[string]any `json:"provider"`
					Messages  []modelMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					return
				}
				if tc.want == "on" {
					if input.Reasoning["enabled"] != true {
						t.Error("reasoning not enabled")
					}
				} else if input.Reasoning["effort"] != tc.want {
					t.Errorf("wrong effort: %v", input.Reasoning)
				}
				if input.Provider["sort"] != nil || input.Provider["require_parameters"] != true {
					t.Errorf("wrong routing: %v", input.Provider)
				}
				if calls == 1 {
					fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"first","id":"call_a"}]}}]}`)
					fmt.Fprintln(w, `data: {"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"second","id":"call_b"}],"content":"ready"}}]}`)
					fmt.Fprintln(w, "data: [DONE]")
				} else {
					var blocks []map[string]string
					if len(input.Messages) != 1 || json.Unmarshal(input.Messages[0].ReasoningDetails, &blocks) != nil || len(blocks) != 2 || blocks[0]["data"] != "first" || blocks[1]["data"] != "second" {
						t.Error("streamed reasoning blocks lost in followup")
					}
					titleResponse(w, "ready")
				}
			}))
			defer server.Close()
			config := OperatorConfig{APIKey: "test", BaseURL: server.URL, Model: tc.model, ReasoningEffort: tc.effort}
			first, err := config.complete(context.Background(), nil, nil, func(string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err = config.complete(context.Background(), []modelMessage{first}, nil, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct{ model, effort string }{{"openai/gpt-6.1-sol", "none"}, {"google/gemini-3.8-flash", "none"}, {"z-ai/glm-5.3-flash", "medium"}, {"qwen/qwen3.8-flash", "high"}, {"openai/gpt-6-luna", "invalid"}} {
		if validateOperatorModelOptions(tc.model, OperatorModelOptions{ReasoningEffort: tc.effort}) == nil {
			t.Fatalf("accepted unsupported settings: %+v", tc)
		}
	}
}
