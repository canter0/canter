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

func TestOperatorHTTPModelSelectionPersistsAndRejectsUnknown(t *testing.T) {
	s, existing, token := operatorFixture(t)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: OperatorConfig{APIKey: "test", BaseURL: "http://unused", Model: "configured/default"}})
	request := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+existing.WorkspaceID+"/conversations"+path, strings.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("", `{"id":"conv_model_selection","requestId":"model_new_1","message":"hello","model":"openai/gpt-6.1-sol","modelOptions":{"reasoningEffort":"low"}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Run OperatorRun `json:"run"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Run.Model != "openai/gpt-6.1-sol" || created.Run.ModelOptions.ReasoningEffort != "low" {
		t.Fatalf("selected model lost: %+v", created.Run)
	}
	w = request("/conv_model_selection/messages", `{"requestId":"model_followup_1","message":"continue","model":"deepseek/deepseek-v4.1-flash"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("followup: %d %s", w.Code, w.Body.String())
	}
	var followup OperatorRun
	if err := json.Unmarshal(w.Body.Bytes(), &followup); err != nil {
		t.Fatal(err)
	}
	var persistedModel string
	err := s.pool.QueryRow(context.Background(), `SELECT model FROM operator_runs WHERE id=$1`, followup.ID).Scan(&persistedModel)
	if err != nil || persistedModel != "deepseek/deepseek-v4.1-flash" || followup.Model != persistedModel {
		t.Fatalf("followup model lost: %+v, stored %q, %v", followup, persistedModel, err)
	}
	w = request("", `{"id":"conv_rejected_model","requestId":"model_invalid_1","message":"hello","model":"unlisted/model"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown model accepted: %d", w.Code)
	}
	if _, err := s.Conversation(context.Background(), existing.WorkspaceID, existing.AccountID, "conv_rejected_model"); err != ErrNotFound {
		t.Fatalf("invalid model created conversation: %v", err)
	}
	w = request("/conv_model_selection/messages", `{"requestId":"model_invalid_2","message":"continue","model":"unlisted/model"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown followup model accepted: %d", w.Code)
	}

	w = request("", `{"id":"conv_rejected_options","requestId":"model_invalid_options","message":"hello","model":"google/gemini-3.8-flash","modelOptions":{"reasoningEffort":"none"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported reasoning accepted: %d", w.Code)
	}
	if _, err := s.Conversation(context.Background(), existing.WorkspaceID, existing.AccountID, "conv_rejected_options"); err != ErrNotFound {
		t.Fatal("invalid options created conversation")
	}
	claimed, ok, err := s.claimOperator(context.Background())
	if err != nil || !ok || claimed.ID != created.Run.ID || claimed.ModelOptions != created.Run.ModelOptions {
		t.Fatalf("claimed run lost settings: %+v %v", claimed, err)
	}
	// Retrying the same request cannot change the model settings of the run.
	w = request("/conv_model_selection/messages", `{"requestId":"model_new_1","message":"hello","model":"openai/gpt-6.1-sol","modelOptions":{"reasoningEffort":"high"}}`)
	var repeated OperatorRun
	if err := json.Unmarshal(w.Body.Bytes(), &repeated); err != nil || w.Code != http.StatusAccepted || repeated.ModelOptions != created.Run.ModelOptions {
		t.Fatalf("retry altered options: %s", w.Body.String())
	}
	// Older clients that omit model still use the server's configured default.
	w = request("/"+existing.ID+"/messages", `{"requestId":"model_legacy_1","message":"continue"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("legacy client: %d %s", w.Code, w.Body.String())
	}
	run, err := s.LatestOperatorRun(context.Background(), existing.ID)
	if err != nil || run.Model != "configured/default" {
		t.Fatalf("configured default lost: %+v %v", run, err)
	}
}

func TestOperatorModelSettingsProviderPayload(t *testing.T) {
	for _, tc := range []struct {
		model, effort, want string
	}{
		{"openai/gpt-6-luna", "none", "none"},
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
	for _, tc := range []struct{ model, effort string }{{"google/gemini-3.8-flash", "none"}, {"z-ai/glm-5.3-flash", "medium"}, {"qwen/qwen3.8-flash", "high"}, {"openai/gpt-6-luna", "invalid"}} {
		if validateOperatorModelOptions(tc.model, OperatorModelOptions{ReasoningEffort: tc.effort}) == nil {
			t.Fatalf("accepted unsupported settings: %+v", tc)
		}
	}
}
