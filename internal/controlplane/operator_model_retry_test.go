package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const operatorRetryTestAnswer = "data: {\"choices\":[{\"delta\":{\"content\":\"Recovered.\"}}]}\n\ndata: [DONE]\n\n"

func TestOperatorModelRetriesTransientHTTPFailures(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				fmt.Fprint(w, operatorRetryTestAnswer)
			}))
			defer provider.Close()
			var updates []string
			config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
			answer, err := config.complete(context.Background(), nil, nil, func(text string) error {
				updates = append(updates, text)
				return nil
			})
			if err != nil || answer.Content != "Recovered." || calls.Load() != 2 || strings.Join(updates, "") != answer.Content {
				t.Fatalf("recovery: answer=%+v calls=%d updates=%v err=%v", answer, calls.Load(), updates, err)
			}
		})
	}
}

func TestOperatorModelDoesNotRetryPermanentHTTPFailures(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				fmt.Fprint(w, "private provider response")
			}))
			defer provider.Close()
			config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
			_, err := config.complete(context.Background(), nil, nil, func(string) error { return nil })
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "private") {
				t.Fatalf("permanent failure retried or exposed: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestOperatorModelTimeoutsRetainCauseAndBoundRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer provider.Close()
			config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelTimeout: 50 * time.Millisecond, modelRetryDelay: time.Nanosecond}
			_, err := config.complete(context.Background(), nil, nil, func(string) error { return nil })
			var failure *operatorModelError
			if !errors.As(err, &failure) || failure.kind != "timeout" || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 3 {
				t.Fatalf("timeout classification/retries: calls=%d err=%v", calls.Load(), err)
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("timeout message: %v", err)
			}
		})
	}
}

func TestOperatorModelRetriesInterruptedToolStreamWithoutLeakingFragments(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"discarded\",\"function\":{\"name\":\"canter_\",\"arguments\":\"{\"}}]}}]}\n\n")
			w.(http.Flusher).Flush()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"recovered\",\"function\":{\"name\":\"canter_show_billing\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
	answer, err := config.complete(context.Background(), nil, nil, func(string) error {
		t.Error("tool-only attempts emitted text")
		return nil
	})
	if err != nil || calls.Load() != 2 || len(answer.ToolCalls) != 1 || answer.ToolCalls[0].ID != "recovered" || answer.ToolCalls[0].Function.Name != "canter_show_billing" {
		t.Fatalf("tool stream recovery: calls=%d answer=%+v err=%v", calls.Load(), answer, err)
	}
}

func TestOperatorModelDoesNotRetryPartialTextOrCallbackFailures(t *testing.T) {
	for _, callbackFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("callbackFails=%t", callbackFails), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Partial answer\"}}]}\n\n")
			}))
			defer provider.Close()
			callbackError := errors.New("persistence failed")
			var updates []string
			config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
			answer, err := config.complete(context.Background(), nil, nil, func(text string) error {
				updates = append(updates, text)
				if callbackFails {
					return callbackError
				}
				return nil
			})
			if err == nil || calls.Load() != 1 || answer.Content != "Partial answer" || len(updates) != 1 {
				t.Fatalf("partial/callback retry: calls=%d answer=%+v updates=%v err=%v", calls.Load(), answer, updates, err)
			}
			if callbackFails && !errors.Is(err, callbackError) {
				t.Fatalf("callback error changed: %v", err)
			}
		})
	}
}

func TestOperatorModelCancellationStopsRequestAndBackoff(t *testing.T) {
	for _, backoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("backoff=%t", backoff), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				if backoff {
					w.Header().Set("Retry-After", "10")
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			config := OperatorConfig{BaseURL: provider.URL, Model: "test"}
			_, err := config.complete(ctx, nil, nil, func(string) error { return nil })
			if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 || time.Since(started) > time.Second {
				t.Fatalf("cancellation ignored: calls=%d elapsed=%s err=%v", calls.Load(), time.Since(started), err)
			}
		})
	}
}

func TestOperatorModelProviderStreamFailuresUseStatusOnly(t *testing.T) {
	for _, status := range []int{401, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					fmt.Fprintf(w, "data: {\"error\":{\"code\":%d,\"message\":\"private request data\"}}\n\n", status)
					return
				}
				fmt.Fprint(w, operatorRetryTestAnswer)
			}))
			defer provider.Close()
			config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
			answer, err := config.complete(context.Background(), nil, nil, func(string) error { return nil })
			if status == 503 {
				if err != nil || calls.Load() != 2 || answer.Content != "Recovered." {
					t.Fatalf("transient provider error: calls=%d answer=%+v err=%v", calls.Load(), answer, err)
				}
			} else if err == nil || calls.Load() != 1 || strings.Contains(fmt.Sprint(errors.Unwrap(err)), "private") {
				t.Fatalf("permanent provider error: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestOperatorModelDiagnosticsExcludeProviderURL(t *testing.T) {
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(original)
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	provider.Close()
	config := OperatorConfig{BaseURL: strings.Replace(provider.URL, "http://", "http://private-user:private-password@", 1) + "/private-path?key=private-key", Model: "test", modelRetryDelay: time.Nanosecond}
	_, err := config.complete(context.Background(), nil, nil, func(string) error { return nil })
	if err == nil || errors.Unwrap(err) == nil || output.Len() == 0 {
		t.Fatalf("transport cause not retained/logged: %v", err)
	}
	if strings.Contains(output.String()+err.Error(), "private-") {
		t.Fatalf("provider URL exposed: %s %v", output.String(), err)
	}
}

func TestOperatorModelRespectsLongProviderCooldown(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer provider.Close()
	config := OperatorConfig{BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
	_, err := config.complete(context.Background(), nil, nil, func(string) error { return nil })
	if err == nil || calls.Load() != 1 {
		t.Fatalf("provider cooldown ignored: calls=%d err=%v", calls.Load(), err)
	}
	now := time.Now().Truncate(time.Second)
	if got := operatorModelRetryAfter(now.Add(5*time.Second).UTC().Format(http.TimeFormat), now); got != 5*time.Second {
		t.Fatalf("Retry-After date = %s", got)
	}
}

func TestOperatorRuntimeModelRetryDoesNotRepeatCompletedTask(t *testing.T) {
	s, conversation, _ := operatorFixture(t)
	ctx := context.Background()
	// Prevent asynchronous title requests from affecting the model call count.
	if _, err := s.RenameConversation(ctx, conversation.WorkspaceID, conversation.AccountID, conversation.ID, conversation.Title); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	requests := make(chan string, 3)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		requests <- string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"created_task\",\"function\":{\"name\":\"canter_create_task\",\"arguments\":\"{\\\"prompt\\\":\\\"Implement the requested upload feature and verify it.\\\"}\"}}]}}]}\n\ndata: [DONE]\n\n")
		case 2:
			// Even a complete tool fragment is untrusted until the stream finishes.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"discarded_task\",\"function\":{\"name\":\"canter_create_task\",\"arguments\":\"{\\\"prompt\\\":\\\"This interrupted fragment must never create a second task.\\\"}\"}}]}}]}\n\n")
		default:
			fmt.Fprint(w, operatorRetryTestAnswer)
		}
	}))
	defer provider.Close()
	config := OperatorConfig{APIKey: "protocol-test", BaseURL: provider.URL, Model: "test", modelRetryDelay: time.Nanosecond}
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer), Config: config}
	if _, err := s.EnqueueOperator(ctx, conversation, "retry-task", "Create the upload task", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%t err=%v", ok, err)
	}
	runtime.execute(ctx, run)
	latest, err := s.LatestOperatorRun(ctx, conversation.ID)
	if err != nil || latest.Status != "completed" || calls.Load() != 3 {
		t.Fatalf("runtime recovery: run=%+v calls=%d err=%v", latest, calls.Load(), err)
	}
	tasks, err := s.ListWorkspaceTasks(ctx, conversation.WorkspaceID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("completed or interrupted tool was replayed: tasks=%d err=%v", len(tasks), err)
	}
	<-requests
	second, third := <-requests, <-requests
	if second != third {
		t.Fatal("model retry changed the saved tool evidence or request options")
	}
	var request struct {
		Messages []modelMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(third), &request); err != nil {
		t.Fatal(err)
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "created_task" || !strings.Contains(last.Content, "executorStarted") {
		t.Fatalf("completed tool evidence missing on retry: %+v", last)
	}
}

func TestOperatorRuntimeCompletesAfterBrowserDisconnect(t *testing.T) {
	s, conversation, token := operatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.RenameConversation(ctx, conversation.WorkspaceID, conversation.AccountID, conversation.ID, conversation.Title); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{}, 1)
	defer close(release)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-release:
			fmt.Fprint(w, operatorRetryTestAnswer)
		case <-r.Context().Done():
		}
	}))
	defer provider.Close()
	config := OperatorConfig{APIKey: "protocol-test", BaseURL: provider.URL, Model: "test"}
	handler := NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer)
	principal, err := s.ResolveHuman(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	browserCtx, disconnect := context.WithCancel(ctx)
	defer disconnect()
	send := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(`{"requestId":"disconnect","message":"Please finish this response"}`)).WithContext(browserCtx)
	send.Header.Set("Content-Type", "application/json")
	accepted := httptest.NewRecorder()
	handler.conversations(accepted, send, principal, conversation.WorkspaceID, []string{conversation.ID, "messages"})
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("message was not accepted: %d %s", accepted.Code, accepted.Body.String())
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%t err=%v", ok, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&OperatorRuntime{Server: handler, Config: config}).execute(ctx, run)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("model request did not start")
	}
	// Losing the event poll must not revoke the server worker's context/lease.
	poll := httptest.NewRequest(http.MethodGet, "/events?after=9223372036854775807", nil).WithContext(browserCtx)
	poll.AddCookie(&http.Cookie{Name: handler.humanCookieName(), Value: token})
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		handler.conversations(httptest.NewRecorder(), poll, principal, conversation.WorkspaceID, []string{conversation.ID, "events"})
	}()
	disconnect()
	select {
	case <-pollDone:
	case <-ctx.Done():
		t.Fatal("disconnected poll did not stop")
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("server worker stopped with the browser")
	}
	latest, err := s.LatestOperatorRun(ctx, conversation.ID)
	if err != nil || latest.Status != "completed" {
		t.Fatalf("response did not finish after disconnect: %+v %v", latest, err)
	}
	page, err := s.OperatorMessages(ctx, conversation.ID, "")
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Content != "Recovered." {
		t.Fatalf("completed response not available on reconnect: %+v %v", page, err)
	}
}

func TestOperatorRuntimePersistsDistinctModelFailureMessages(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			s, conversation, _ := operatorFixture(t)
			ctx := context.Background()
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if timeout {
					<-r.Context().Done()
					return
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
			}))
			defer provider.Close()
			config := OperatorConfig{APIKey: "protocol-test", BaseURL: provider.URL, Model: "test", modelTimeout: 50 * time.Millisecond, modelRetryDelay: time.Nanosecond}
			runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{Operator: config}).(*HTTPServer), Config: config}
			if _, err := s.EnqueueOperator(ctx, conversation, "failed-model", "Please respond", "test", nil); err != nil {
				t.Fatal(err)
			}
			run, ok, err := s.claimOperator(ctx)
			if err != nil || !ok {
				t.Fatalf("claim: ok=%t err=%v", ok, err)
			}
			runtime.execute(ctx, run)
			latest, err := s.LatestOperatorRun(ctx, conversation.ID)
			wantMessage := "The connection to the model provider was interrupted. Your conversation and completed operations are saved."
			if timeout {
				wantMessage = "The model response timed out. Your conversation and completed operations are saved."
			}
			if err != nil || latest.Status != "failed" || latest.Failure != wantMessage || calls.Load() != 3 {
				t.Fatalf("persisted failure: run=%+v calls=%d want=%q err=%v", latest, calls.Load(), wantMessage, err)
			}
		})
	}
}
