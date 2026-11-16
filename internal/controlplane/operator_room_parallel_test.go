package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func parallelRoomFixture(t *testing.T) (*Store, Conversation, Conversation, []Wren) {
	t.Helper()
	s, c, _ := operatorFixture(t)
	ctx := context.Background()
	peer, _, _, err := s.Signup(ctx, "parallel-peer@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, peer.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	wrens := []Wren{}
	for _, name := range []string{"Writer", "Reviewer", "Researcher"} {
		w, e := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_parallel_" + strings.ToLower(name), Name: name, Purpose: "Help with the current assignment", Shape: "circle"}, true)
		if e != nil {
			t.Fatal(e)
		}
		wrens = append(wrens, w)
	}
	room, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_parallel", Kind: "group", Name: "Parallel review", WrenIDs: []string{wrens[0].ID, wrens[1].ID, wrens[2].ID}, MemberIDs: []string{peer.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := s.Conversation(ctx, c.WorkspaceID, peer.ID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, guest, wrens
}

func enqueueRoomWork(t *testing.T, s *Store, c Conversation, id, prompt string, wrens ...Wren) OperatorRun {
	t.Helper()
	ids := []string{}
	for _, w := range wrens {
		ids = append(ids, w.ID)
	}
	run, err := s.enqueueOperatorWithRecipients(context.Background(), c, id, prompt, "test", OperatorModelOptions{}, nil, ids, false)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRoomParallelClaimsPreserveTeamAndWrenOrder(t *testing.T) {
	s, owner, peer, wrens := parallelRoomFixture(t)
	ctx := context.Background()
	team := enqueueRoomWork(t, s, owner, "parallel-team", "Draft, then review the draft.", wrens[0], wrens[1])
	separate := enqueueRoomWork(t, s, peer, "parallel-separate", "Research a separate question.", wrens[2])
	enqueueRoomWork(t, s, peer, "parallel-followup", "Another task for Writer.", wrens[0])
	// Competing dispatchers must claim exactly the first team member and the
	// independent researcher, never a reviewer before its writer or two Writers.
	type result struct {
		run OperatorRun
		ok  bool
		err error
	}
	results := make(chan result, 12)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; r, ok, e := s.claimOperator(ctx); results <- result{r, ok, e} }()
	}
	close(start)
	wg.Wait()
	close(results)
	claimed := map[string]OperatorRun{}
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.ok {
			if _, exists := claimed[r.run.ID]; exists {
				t.Fatal("double claim")
			}
			claimed[r.run.ID] = r.run
		}
	}
	if len(claimed) != 2 || claimed[team.ID].ID == "" || claimed[separate.ID].ID == "" {
		t.Fatalf("incorrect parallel claims: %#v", claimed)
	}
	// Startup migrations must not recreate the retired room-wide running index.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("restart with parallel work: %v", err)
	}
	if err := s.finishOperator(ctx, claimed[team.ID], "completed", "The shared draft."); err != nil {
		t.Fatal(err)
	}
	reviewer, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || reviewer.TurnID != team.ID || reviewer.Speaker.ID != wrens[1].ID {
		t.Fatalf("review sequence: %#v %v", reviewer, err)
	}
	writer, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || writer.Speaker.ID != wrens[0].ID {
		t.Fatalf("independent Writer follow-up: %#v %v", writer, err)
	}
	if err = s.cancelOperatorRequest(ctx, owner.WorkspaceID, owner.AccountID, owner.ID, reviewer.ID); err != nil {
		t.Fatal(err)
	}
	var active int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE id=ANY($1) AND status='running'`, []string{separate.ID, writer.ID}).Scan(&active); err != nil || active != 2 {
		t.Fatalf("stop affected peer: %d %v", active, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, separate.ID); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || recovered.ID != separate.ID || recovered.Lease == claimed[separate.ID].Lease {
		t.Fatalf("recovery: %#v %v", recovered, err)
	}
	if err = s.finishOperator(ctx, claimed[separate.ID], "completed", "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale worker published: %v", err)
	}
}

func TestRoomParallelPrivateWorkingDataAndEvidence(t *testing.T) {
	s, owner, peer, wrens := parallelRoomFixture(t)
	ctx := context.Background()
	first := enqueueRoomWork(t, s, owner, "private-owner", "Owner's assignment", wrens[0])
	second := enqueueRoomWork(t, s, peer, "private-peer", "Peer's assignment", wrens[1])
	claimed := map[string]OperatorRun{}
	for range 2 {
		r, ok, e := s.claimOperator(ctx)
		if e != nil || !ok {
			t.Fatalf("claim: %v", e)
		}
		claimed[r.ID] = r
	}
	first = claimed[first.ID]
	second = claimed[second.ID]
	if _, err := s.pool.Exec(ctx, `INSERT INTO operator_working_context(conversation_id,notes,scratch) VALUES($1,'{"objective":"unattributed legacy room notes"}','{"/scratch/legacy.txt":"unknown author"}')`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if notes, scratch, err := s.operatorWorkingData(ctx, first); err != nil || string(notes) != "{}" || len(scratch) != 0 {
		t.Fatalf("unattributed legacy context was assigned to a room member: %s %#v %v", notes, scratch, err)
	}
	for _, r := range []OperatorRun{first, second} {
		if err := s.saveOperatorNotes(ctx, r, operatorWorkingNotes{Objective: r.ID}); err != nil {
			t.Fatal(err)
		}
		if err := s.saveOperatorScratch(ctx, r, map[string]string{"/scratch/work.txt": r.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result,result_path) VALUES($1,'private_result','fixture','{}',$2,$3)`, r.ID, json.RawMessage(`{"marker":"`+r.ID+`"}`), operatorResultPath(r.ID, "private_result")); err != nil {
			t.Fatal(err)
		}
	}
	runtime := OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{}).(*HTTPServer)}
	for i, r := range []OperatorRun{first, second} {
		other := second
		c := owner
		if i == 1 {
			other = first
			c = peer
		}
		notes, scratch, e := s.operatorWorkingData(ctx, r)
		if e != nil || !strings.Contains(string(notes), r.ID) || strings.Contains(string(notes), other.ID) || scratch["/scratch/work.txt"] != r.ID {
			t.Fatalf("private working state: %s %#v %v", notes, scratch, e)
		}
		evidence, e := s.operatorRecentEvidence(ctx, r)
		if e != nil || !strings.Contains(string(evidence), r.ID) || strings.Contains(string(evidence), other.ID) {
			t.Fatalf("evidence leaked: %s %v", evidence, e)
		}
		files, _, e := s.operatorShellFiles(ctx, r, c)
		if e != nil || files[operatorResultPath(other.ID, "private_result")] != "" || files[operatorResultPath(r.ID, "private_result")] == "" {
			t.Fatalf("shell evidence scope: %v", e)
		}
		args, _ := json.Marshal(map[string]string{"path": operatorResultPath(other.ID, "private_result")})
		if _, _, e = runtime.harnessTool(ctx, r, c, Principal{}, "canter_read_result", args); !errors.Is(e, ErrNotFound) {
			t.Fatalf("read peer result: %v", e)
		}
		args, _ = json.Marshal(map[string]string{"path": operatorResultPath(r.ID, "private_result")})
		if _, _, e = runtime.harnessTool(ctx, r, c, Principal{}, "canter_read_result", args); e != nil {
			t.Fatalf("own result unreadable: %v", e)
		}
	}
	if err := s.finishOperator(ctx, first, "completed", "A deliberately shared finding."); err != nil {
		t.Fatal(err)
	}
	// The same Wren serving a different person must not inherit that person's
	// scratch or receipts. Its public answer remains in the room transcript.
	next := enqueueRoomWork(t, s, peer, "private-same-wren", "A new person's assignment", wrens[0])
	next, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	notes, scratch, err := s.operatorWorkingData(ctx, next)
	if err != nil || string(notes) != "{}" || len(scratch) != 0 {
		t.Fatalf("same-Wren requester leak: %s %#v %v", notes, scratch, err)
	}
	evidence, err := s.operatorRecentEvidence(ctx, next)
	if err != nil || string(evidence) != "[]" {
		t.Fatalf("prior requester's receipt leaked: %s %v", evidence, err)
	}
	history, err := s.operatorHistory(ctx, next, 80)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(history)
	if !strings.Contains(string(raw), "A deliberately shared finding.") {
		t.Fatal("lost shared answer")
	}
	if err = s.saveOperatorNotes(ctx, first, operatorWorkingNotes{Objective: "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("finished run wrote notes: %v", err)
	}
	if err = s.saveOperatorScratch(ctx, first, map[string]string{"/scratch/work.txt": "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("finished run wrote scratch: %v", err)
	}
	if err = s.finishOperator(ctx, next, "completed", "Peer work finished."); err != nil {
		t.Fatal(err)
	}
	resumed := enqueueRoomWork(t, s, owner, "private-resume-owner", "Resume my earlier assignment", wrens[0])
	notes, scratch, err = s.operatorWorkingData(ctx, resumed)
	if err != nil || !strings.Contains(string(notes), first.ID) || scratch["/scratch/work.txt"] != first.ID {
		t.Fatalf("original person's working context did not survive another member's turn: %s %#v %v", notes, scratch, err)
	}
}

func TestRoomParallelRuntimeFastReplyDoesNotWaitForSlowWren(t *testing.T) {
	s, owner, peer, wrens := parallelRoomFixture(t)
	ctx := context.Background()
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
			if strings.Contains(m.Content, "[Current assignment for Wren Writer]") {
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
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Independent reply.\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	slow := enqueueRoomWork(t, s, owner, "runtime-slow", "Work on a slow assignment.", wrens[0])
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
			t.Error("runtime did not stop")
		}
	}()
	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("slow Wren did not start")
	}
	fast := enqueueRoomWork(t, s, peer, "runtime-fast", "Answer this independent question.", wrens[1])
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var fastStatus, slowStatus string
		if err := s.pool.QueryRow(ctx, `SELECT (SELECT status FROM operator_runs WHERE id=$1),(SELECT status FROM operator_runs WHERE id=$2)`, fast.ID, slow.ID).Scan(&fastStatus, &slowStatus); err != nil {
			t.Fatal(err)
		}
		if fastStatus == "completed" {
			if slowStatus != "running" {
				t.Fatalf("slow assignment lost: %s", slowStatus)
			}
			return
		}
		if fastStatus == "failed" {
			t.Fatal("fast reply failed")
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("fast Wren waited behind a different Wren in the same room")
}
