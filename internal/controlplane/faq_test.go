package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFAQModelRequestAndRateLimits(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct {
			Model    string         `json:"model"`
			Messages []modelMessage `json:"messages"`
			Tools    []any          `json:"tools"`
			Max      int            `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Model != "faq-model" || len(input.Tools) != 0 || input.Max != 420 || len(input.Messages) != 4 {
			t.Errorf("unexpected model request: %s, %d tools, %d tokens, %d messages", input.Model, len(input.Tools), input.Max, len(input.Messages))
		} else if input.Messages[0].Role != "system" || input.Messages[0].Content != faqInstructions || input.Messages[2].Role != "assistant" || input.Messages[3].Content != "What about each pull request?" {
			t.Error("FAQ lost its public instructions or follow-up context")
		}
		titleResponse(w, `{"answer":"Use a Canter runner with GitHub Actions.","source":"ci"}`)
	}))
	defer provider.Close()
	h := NewHTTPServer(nil, HTTPConfig{
		PublicURL: "https://canter.test",
		Operator:  OperatorConfig{APIKey: "test", Model: "workspace-model", BaseURL: provider.URL},
		FAQModel:  "faq-model",
	}).(*HTTPServer)
	const question = `{"question":"What about each pull request?","history":[{"question":"Where should I test?","answer":"Use a sandbox for a one-off test."}]}`
	request := func(ip, origin, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "https://canter.test/v1/faq", strings.NewReader(body))
		r.RemoteAddr = ip + ":4000"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("192.0.2.1", "https://other.test", question); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request returned %d", w.Code)
	}
	if w := request("192.0.2.1", "https://canter.test", `{"question":" "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty question returned %d", w.Code)
	}
	for range 6 {
		w := request("192.0.2.2", "https://canter.test", question)
		var reply faqReply
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil || reply.Source != "ci" || reply.Answer == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected reply: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request("192.0.2.2", "https://canter.test", question); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("IP limit returned %d", w.Code)
	}
	window := h.limits.windows["faq|site"]
	window.count = 300
	h.limits.windows["faq|site"] = window
	if w := request("192.0.2.3", "https://canter.test", question); w.Code != http.StatusTooManyRequests || calls.Load() != 6 {
		t.Fatalf("site limit returned %d after %d model calls", w.Code, calls.Load())
	}
}
