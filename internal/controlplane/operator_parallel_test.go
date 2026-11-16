package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOperatorDifferentConversationsStayResponsive(t *testing.T) {
	s, first, _ := operatorFixture(t)
	ctx := context.Background()
	second, err := s.CreateConversation(ctx, first.WorkspaceID, first.AccountID, "conv_foreground_parallel", "foreground reply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_conversations SET title_state='manual' WHERE id=ANY($1)`, []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	slowStarted := make(chan struct{}, 1)
	releaseSlow := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []modelMessage `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			w.WriteHeader(400)
			return
		}
		slow := false
		for _, m := range input.Messages {
			if strings.Contains(m.Content, "slow worker sentinel") {
				slow = true
			}
		}
		if slow {
			select {
			case slowStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseSlow:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"A real test response.\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	if _, err = s.EnqueueOperator(ctx, first, "parallel-slow-request", "slow worker sentinel", "test", nil); err != nil {
		t.Fatal(err)
	}
	config := OperatorConfig{APIKey: "test", BaseURL: provider.URL, Model: "test"}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Operator: config}).(*HTTPServer)
	runtimeCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- (&OperatorRuntime{Server: h, Config: config}).Run(runtimeCtx) }()
	defer func() {
		close(releaseSlow)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("runtime did not stop its workers")
		}
	}()
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow conversation did not start")
	}
	if _, err = s.EnqueueOperator(ctx, second, "parallel-foreground-request", "foreground reply", "test", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, e := s.LatestOperatorRun(ctx, second.ID)
		if e != nil {
			t.Fatal(e)
		}
		if run != nil && run.Status == "completed" {
			slow, e := s.LatestOperatorRun(ctx, first.ID)
			if e != nil || slow.Status != "running" {
				t.Fatalf("slow worker didn't remain independent: %#v %v", slow, e)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("foreground conversation waited for a different conversation's slow work")
}
