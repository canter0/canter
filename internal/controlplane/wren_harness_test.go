package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type brokerTransport func(*http.Request) (*http.Response, error)

func (f brokerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func harnessFixture(t *testing.T) (*HTTPServer, Conversation, OperatorRun, WrenJob, wrenModelGrant, string) {
	t.Helper()
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	engine := &computerFakeEngine{}
	h := NewHTTPServer(&Service{Store: s, Engine: engine}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Operator: OperatorConfig{APIKey: "master-test-only", BaseURL: "https://provider.example/v1", Model: "test-worker", WrenModel: "test-worker"}, Computers: WrenComputerConfig{SSHSourceCIDR: "203.0.113.8/32", AllowedWorkspaces: []string{c.WorkspaceID}}, Harnesses: WrenHarnessConfig{BrokerBaseURL: "https://broker.example/v1/wren-model", MaxRequests: 3, MaxOutputTokens: 128, MaxAccountRequestsPerHour: 10}}).(*HTTPServer)
	computer, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	computer = readyComputer(t, h, computer)
	job, err := h.CreateHarnessWrenJob(ctx, parent, c, "harness-call-1", "Build the scoped page", "Create a simple accessible homepage in this job's work directory.")
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.claimOperator(ctx); err != nil || claimed {
		t.Fatalf("operator stole ACP executor run: %v %v", claimed, err)
	}
	run, claimed, err := s.claimWrenHarness(ctx)
	if err != nil || !claimed || run.ID != job.ChildRunID {
		t.Fatalf("ACP executor could not claim its real run: %#v %v %v", run, claimed, err)
	}
	job, err = s.WrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.ExecutorStarted {
		t.Fatal("claiming an ACP run falsely claimed guest execution began")
	}
	grant, token, err := h.mintWrenModelGrant(ctx, job, computer)
	if err != nil {
		t.Fatal(err)
	}
	return h, c, run, job, grant, token
}
func brokerRequest(h *HTTPServer, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://broker.example/v1/wren-model/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.wrenModelBroker(w, r)
	return w
}
func TestWrenModelBrokerScopedFixedModelAndUsage(t *testing.T) {
	h, c, run, job, grant, token := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	var calls atomic.Int32
	h.config.Harnesses.transport = brokerTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != "https://provider.example/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer master-test-only" {
			t.Error("broker changed destination or forwarded guest credential")
		}
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		var model string
		json.Unmarshal(body["model"], &model)
		var max int
		json.Unmarshal(body["max_tokens"], &max)
		if model != "test-worker" || max != 128 || body["base_url"] != nil {
			t.Errorf("broker did not enforce fixed routing/output: model=%s max=%d", model, max)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"id\":\"generation_test\",\"choices\":[{\"delta\":{\"content\":\"real upstream test stream\"}}]}\n\ndata: {\"id\":\"generation_test\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"cost\":0.001}}\n\ndata: [DONE]\n\n"))}, nil
	})
	wrong := brokerRequest(h, token, `{"model":"expensive/other-model","messages":[{"role":"user","content":"hello"}]}`)
	if wrong.Code != 403 || calls.Load() != 0 {
		t.Fatalf("caller selected arbitrary model: %d %s", wrong.Code, wrong.Body)
	}
	ok := brokerRequest(h, token, `{"model":"test-worker","messages":[{"role":"user","content":"private task content"}],"max_tokens":9000,"stream":true,"base_url":"https://attacker.example"}`)
	if ok.Code != 200 || !strings.Contains(ok.Body.String(), "[DONE]") {
		t.Fatalf("stream: %d %s", ok.Code, ok.Body)
	}
	var status string
	var usage []byte
	if err := s.pool.QueryRow(ctx, `SELECT status,usage FROM wren_model_requests WHERE grant_id=$1`, grant.ID).Scan(&status, &usage); err != nil || status != "completed" || !strings.Contains(string(usage), "0.001") {
		t.Fatalf("actual usage missing: %s %s %v", status, usage, err)
	}
	var sealed []byte
	if err := s.pool.QueryRow(ctx, `SELECT token_ciphertext FROM wren_model_grants WHERE id=$1`, grant.ID).Scan(&sealed); err != nil || bytes.Contains(sealed, []byte(token)) {
		t.Fatal("model broker token stored in plaintext")
	}
	existing, existingToken, err := h.mintWrenModelGrant(ctx, job, readyComputerForHarness(t, h, grant.ComputerID))
	if err != nil || existing.ID != grant.ID || existingToken != token {
		t.Fatalf("recovery minted a different budget/token: %s %v", existing.ID, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_model_grants SET revoked_at=now() WHERE id=$1`, grant.ID); err != nil {
		t.Fatal(err)
	}
	denied := brokerRequest(h, token, `{"model":"test-worker","messages":[{"role":"user","content":"retry"}]}`)
	if denied.Code != 403 || calls.Load() != 1 {
		t.Fatalf("revoked grant reached model: %d calls=%d", denied.Code, calls.Load())
	}
	_ = c
	_ = run
}
func readyComputerForHarness(t *testing.T, h *HTTPServer, id string) WrenComputer {
	t.Helper()
	var workspace string
	if err := h.service.Store.pool.QueryRow(context.Background(), `SELECT workspace_id FROM wren_computers WHERE id=$1`, id).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	v, err := h.service.Store.wrenComputerPrivate(context.Background(), workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestWrenModelBrokerConcurrentCapsExpiryAndRequesterRevocation(t *testing.T) {
	h, c, _, _, grant, token := harnessFixture(t)
	ctx := context.Background()
	s := h.service.Store
	if _, err := s.pool.Exec(ctx, `UPDATE wren_model_grants SET max_requests=2 WHERE id=$1`, grant.ID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h.config.Harnesses.transport = brokerTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"test","choices":[],"usage":{"total_tokens":2}}`))}, nil
	})
	var workers sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			codes <- brokerRequest(h, token, `{"model":"test-worker","messages":[{"role":"user","content":"hello"}]}`).Code
		}()
	}
	workers.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code != 403 {
			t.Fatalf("unexpected quota response: %d", code)
		}
	}
	if success != 2 || calls.Load() != 2 {
		t.Fatalf("concurrent requests overspent cap: success=%d calls=%d", success, calls.Load())
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wren_model_grants SET max_requests=3,expires_at=now()-interval '1 second' WHERE id=$1`, grant.ID); err != nil {
		t.Fatal(err)
	}
	if got := brokerRequest(h, token, `{"messages":[{"role":"user","content":"hello"}]}`); got.Code != 403 {
		t.Fatalf("expired grant accepted: %d", got.Code)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wren_model_grants SET expires_at=now()+interval '5 minutes' WHERE id=$1`, grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE account_id=$1 AND workspace_id=$2`, c.AccountID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if got := brokerRequest(h, token, `{"messages":[{"role":"user","content":"hello"}]}`); got.Code != 403 || calls.Load() != 2 {
		t.Fatalf("downgraded requester kept paid access: %d calls=%d", got.Code, calls.Load())
	}
}
func TestWrenModelBrokerSharedAccountQuotaAndRedirectSecrets(t *testing.T) {
	h, c, _, job, grant, token := harnessFixture(t)
	ctx := context.Background()
	s := h.service.Store
	h.config.Harnesses.MaxAccountRequestsPerHour = 1
	var calls atomic.Int32
	h.config.Harnesses.transport = brokerTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://attacker.example"}}, Body: io.NopCloser(strings.NewReader("sensitive upstream diagnostics must not be forwarded"))}, nil
	})
	first := brokerRequest(h, token, `{"messages":[{"role":"user","content":"hello"}]}`)
	if first.Code != 502 || strings.Contains(first.Body.String(), "sensitive upstream") {
		t.Fatalf("upstream redirect/error leaked: %d %s", first.Code, first.Body)
	}
	second := brokerRequest(h, token, `{"messages":[{"role":"user","content":"retry"}]}`)
	if second.Code != http.StatusTooManyRequests && second.Code != http.StatusConflict && second.Code != http.StatusForbidden && second.Code != http.StatusServiceUnavailable {
		t.Fatalf("shared quota response: %d %s", second.Code, second.Body)
	}
	if calls.Load() != 1 {
		t.Fatal("shared quota or redirect forwarded master key")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wren_harness_executions SET stop_requested=true WHERE job_id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.consumeWrenModelRequest(ctx, token, grant.Model)
	if err == nil {
		t.Fatal("stopping harness retained model capability")
	}
	_ = c
}
func TestHarnessRecoveryEventsAndFencedLease(t *testing.T) {
	h, c, run, job, _, _ := harnessFixture(t)
	ctx := context.Background()
	s := h.service.Store
	data := json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"A saved coding result."}}}`)
	if err := h.saveHarnessUpdate(ctx, run, job.ID, 100, data); err != nil {
		t.Fatal(err)
	}
	if err := h.saveHarnessUpdate(ctx, run, job.ID, 100, data); err != nil {
		t.Fatal(err)
	}
	execution, err := h.harnessExecution(ctx, job.ID)
	if err != nil || execution.ResponseText != "A saved coding result." {
		t.Fatalf("event replay duplicated result: %s %v", execution.ResponseText, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.claimWrenHarness(ctx)
	if err != nil || !ok || recovered.ID != run.ID || recovered.Lease == run.Lease {
		t.Fatalf("ACP run lease not recovered: %#v %v", recovered, err)
	}
	if err = h.saveHarnessUpdate(ctx, run, job.ID, 200, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale harness worker wrote: %v", err)
	}
	if err = h.saveHarnessUpdate(ctx, recovered, job.ID, 200, data); err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenJob(ctx, c.WorkspaceID, "foreign-account", job.ID); err == nil {
		t.Fatal("foreign user read delegated harness job")
	}
}

func TestHarnessCancellationWaitsForActualGuestStop(t *testing.T) {
	h, c, run, job, _, token := harnessFixture(t)
	ctx := context.Background()
	s := h.service.Store
	stopped, err := s.CancelWrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID)
	if err != nil || stopped.Status != "stop_requested" {
		t.Fatalf("cancellation falsely terminal: %s %v", stopped.Status, err)
	}
	var status string
	var stopRequested bool
	if err = s.pool.QueryRow(ctx, `SELECT r.status,e.stop_requested FROM operator_runs r JOIN wren_harness_executions e ON e.run_id=r.id WHERE r.id=$1`, run.ID).Scan(&status, &stopRequested); err != nil || status != "running" || !stopRequested {
		t.Fatalf("execution prematurely stopped: %s %v %v", status, stopRequested, err)
	}
	if got := brokerRequest(h, token, `{"messages":[{"role":"user","content":"continue"}]}`); got.Code < 400 {
		t.Fatal("cancelled job retained billing capability")
	}
	// A replay from the original parent must reject an executor change.
	var parent OperatorRun
	if err = s.pool.QueryRow(ctx, `SELECT id,lease_token FROM operator_runs WHERE id=$1`, job.ParentRunID).Scan(&parent.ID, &parent.Lease); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateWrenJobWithExecutor(ctx, parent, c, "harness-call-1", "different executor", "not replayable", "test-worker", "operator")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("changed executor replay accepted: %v", err)
	}
}

func TestHarnessWorkspaceDowngradeRequestsRealStop(t *testing.T) {
	h, c, run, job, _, token := harnessFixture(t)
	ctx := context.Background()
	s := h.service.Store
	owner, _, _, err := s.Signup(ctx, "harness-co-owner@example.test", "long-test-password-84", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(workspace_id,account_id,role) VALUES($1,$2,'owner')`, c.WorkspaceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateWorkspaceMember(ctx, c.WorkspaceID, owner.ID, c.AccountID, "viewer", false); err != nil {
		t.Fatal(err)
	}
	var status string
	var requested bool
	if err = s.pool.QueryRow(ctx, `SELECT r.status,e.stop_requested FROM operator_runs r JOIN wren_harness_executions e ON e.run_id=r.id WHERE r.id=$1`, run.ID).Scan(&status, &requested); err != nil || status != "running" || !requested {
		t.Fatalf("downgrade fabricated a remote stop: %s %v %v", status, requested, err)
	}
	var revoked bool
	if err = s.pool.QueryRow(ctx, `SELECT bool_and(revoked_at IS NOT NULL) FROM wren_model_grants WHERE job_id=$1`, job.ID).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("downgrade retained billing: %v %v", revoked, err)
	}
	if got := brokerRequest(h, token, `{"messages":[{"role":"user","content":"continue"}]}`); got.Code < 400 {
		t.Fatal("downgraded member retained broker authority")
	}
}

func TestHarnessToolContentBlocksDoNotCrashUpdateDecoder(t *testing.T) {
	h, _, run, job, _, _ := harnessFixture(t)
	ctx := context.Background()
	// Actual ACP tool content is an array; only agent_message_chunk has the
	// single ContentBlock shape used to assemble the assistant response.
	tool := json.RawMessage(`{"sessionId":"session","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-real-shape","status":"completed","content":[{"type":"content","content":{"type":"text","text":"real tool output"}}],"rawOutput":{"metadata":{"truncated":false}}}}`)
	if err := h.saveHarnessUpdate(ctx, run, job.ID, 123, tool); err != nil {
		t.Fatal(err)
	}
	execution, err := h.harnessExecution(ctx, job.ID)
	if err != nil || execution.ResponseText != "" {
		t.Fatalf("tool content became assistant text: %q %v", execution.ResponseText, err)
	}
	var status string
	if err = h.service.Store.pool.QueryRow(ctx, `SELECT data->>'status' FROM operator_events WHERE run_id=$1 AND kind='tool' ORDER BY sequence DESC LIMIT 1`, run.ID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("real completion missing: %s %v", status, err)
	}
}

func TestHarnessFollowupReadsSameEnvironmentDeliverables(t *testing.T) {
	own := "/workspace/jobs/job_current"
	for _, test := range []struct {
		kind, path string
		want       bool
	}{
		{"read", "/workspace/jobs/job_previous/index.html", true},
		{"search", "/workspace/jobs", true},
		{"edit", "/workspace/jobs/job_previous/index.html", false},
		{"delete", "/workspace/jobs/job_previous/index.html", false},
		{"read", "/workspace/.canter/harness/other/launch.json", false},
		{"read", "/workspace/.browser/Default/Cookies", false},
		{"read", "/workspace/jobs/../../../etc/passwd", false},
		{"edit", own + "/index.html", true},
	} {
		if got := harnessLocationAllowed(test.kind, test.path, own); got != test.want {
			t.Errorf("%s %s got%v want%v", test.kind, test.path, got, test.want)
		}
	}
}
