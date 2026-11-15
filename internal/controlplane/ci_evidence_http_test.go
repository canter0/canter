package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newCIEvidenceHTTPFixture is the admission fixture promoted to managed run
// mode with the evidence slot pool the production constructor allocates.
func newCIEvidenceHTTPFixture(t *testing.T) *ciAdmissionHTTPFixture {
	t.Helper()
	fx := newCIAdmissionHTTPFixture(t, "")
	fx.h.config.CI.Managed = &CIManagedRuntime{mode: CIManagedRun}
	fx.h.ciEvidenceSlots = make(chan struct{}, ciEvidenceHTTPConcurrency)
	return fx
}

func (fx *ciAdmissionHTTPFixture) evidenceServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/ci/"), "/")
		fx.h.ciGitHubPublic(w, r, strings.Split(path, "/"))
	}))
}

func ciEvidencePost(t *testing.T, srv *httptest.Server, token string, body []byte) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ci/runner-evidence", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("evidence POST: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw, resp.Header
}

func ciEvidenceValidBody(t *testing.T, fx *ciAdmissionHTTPFixture) []byte {
	t.Helper()
	return ciEvidenceBody(t, ciEvidenceBatchID(100),
		[]any{ciEvidenceMetric(fx.s.now().UTC().Truncate(time.Microsecond), map[string]any{"cpuPercent": 3.0})},
		[]any{ciEvidenceTest(strings.Repeat("2", 64), map[string]any{"durationMs": nil})})
}

func TestCIEvidenceHTTPRouteGating(t *testing.T) {
	// Plain admission config without managed runtime: route absent.
	fx := newCIAdmissionHTTPFixture(t, "")
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, httptest.NewRequest(http.MethodPost, "/v1/ci/runner-evidence", strings.NewReader("{}")), []string{"runner-evidence"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("unmanaged evidence route must 404, got %d", w.Code)
	}
	// Managed recover and off modes: route absent.
	for _, mode := range []CIManagedMode{CIManagedRecover, CIManagedOff} {
		fx.h.config.CI.Managed = &CIManagedRuntime{mode: mode}
		fx.h.ciEvidenceSlots = make(chan struct{}, ciEvidenceHTTPConcurrency)
		w := httptest.NewRecorder()
		fx.h.ciGitHubPublic(w, httptest.NewRequest(http.MethodPost, "/v1/ci/runner-evidence", strings.NewReader("{}")), []string{"runner-evidence"})
		if w.Code != http.StatusNotFound {
			t.Fatalf("mode %v evidence route must 404, got %d", mode, w.Code)
		}
	}
	// Managed run without admission config: route absent.
	fx.h.config.CI.Managed = &CIManagedRuntime{mode: CIManagedRun}
	fx.h.config.CI.Admission = nil
	fx.h.ciEvidenceSlots = make(chan struct{}, ciEvidenceHTTPConcurrency)
	w = httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, httptest.NewRequest(http.MethodPost, "/v1/ci/runner-evidence", strings.NewReader("{}")), []string{"runner-evidence"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured admission evidence route must 404, got %d", w.Code)
	}
}

func TestCIEvidenceHTTPAuth(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)

	// Missing and malformed bearer fail before method or body work.
	for _, tok := range []string{"", "garbage", "Bearer", "v1.abc.def"} {
		status, body, _ := ciEvidencePost(t, srv, tok, []byte(`{}`))
		if status != http.StatusUnauthorized {
			t.Fatalf("token %q: want 401, got %d", tok, status)
		}
		var out map[string]string
		if err := json.Unmarshal(body, &out); err != nil || out["error"] != "unauthorized" {
			t.Fatalf("token %q: bad 401 body %s", tok, body)
		}
	}
	// An admission capability is the wrong purpose.
	admissionTok := fx.capability(t, 3600, nil)
	status, _, _ := ciEvidencePost(t, srv, admissionTok, []byte(`{}`))
	if status != http.StatusUnauthorized {
		t.Fatalf("admission token must 401, got %d", status)
	}
	// A capability for a different workspace is a valid token but an
	// unauthorized scope: bare 401 with no body.
	foreign := ciEvidenceTestClaims(fx.s.now())
	foreign.Workspace = "ws_other"
	foreignTok, err := mintCIEvidenceCapability(fx.signingKey, foreign, fx.s.now())
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ := ciEvidencePost(t, srv, foreignTok, []byte(`{}`))
	if status != http.StatusUnauthorized || len(body) != 0 {
		t.Fatalf("foreign scope must be bare 401, got %d body %q", status, body)
	}
	// Auth precedes method: GET with an invalid token is 401, with a valid
	// token is 405.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/ci/runner-evidence", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET invalid auth must 401, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/ci/runner-evidence", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET valid auth must 405 Allow POST, got %d allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

func TestCIEvidenceHTTPPositive(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)
	status, body, _ := ciEvidencePost(t, srv, token, ciEvidenceValidBody(t, fx))
	if status != http.StatusNoContent {
		t.Fatalf("evidence POST: %d body %s", status, body)
	}
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	scope, err := fx.s.loadCIEvidenceScope(context.Background(), capability)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := fx.s.CIJobMetrics(context.Background(), fx.workspace, scope.Job)
	if err != nil || len(metrics) != 1 || metrics[0].Source != "guest" {
		t.Fatalf("metrics readback: %v %+v", err, metrics)
	}
	tests, err := fx.s.CIJobTests(context.Background(), fx.workspace, scope.Job)
	if err != nil || len(tests) != 1 || tests[0].Source != "workflow" || tests[0].DurationMs != nil {
		t.Fatalf("tests readback: %v %+v", err, tests)
	}
}

func TestCIEvidenceHTTPErrorMapping(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)

	// Schema violation -> fixed 400.
	status, body, _ := ciEvidencePost(t, srv, token, []byte(`{"schemaVersion":2}`))
	if status != http.StatusBadRequest || !strings.Contains(string(body), "invalid CI evidence") {
		t.Fatalf("schema violation: %d %s", status, body)
	}
	// Oversize -> 413.
	status, _, _ = ciEvidencePost(t, srv, token, bytes.Repeat([]byte(" "), ciEvidenceRequestMaxBytes+1))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", status)
	}
	// Capability minted for a different job id -> unauthorized scope.
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	scope, err := fx.s.loadCIEvidenceScope(context.Background(), capability)
	if err != nil {
		t.Fatal(err)
	}
	capability.GitHubJobID = 999999
	wrongJobTok, err := mintCIEvidenceCapability(fx.signingKey, capability, fx.s.now())
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ = ciEvidencePost(t, srv, wrongJobTok, ciEvidenceValidBody(t, fx))
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong job scope: %d %s", status, body)
	}
	// Conflict: same batch id, different bytes.
	first := ciEvidenceBody(t, ciEvidenceBatchID(110),
		[]any{ciEvidenceMetric(fx.s.now().UTC().Truncate(time.Microsecond), nil)}, nil)
	if status, _, _ := ciEvidencePost(t, srv, token, first); status != http.StatusNoContent {
		t.Fatalf("seed batch: %d", status)
	}
	second := ciEvidenceBody(t, ciEvidenceBatchID(110),
		[]any{ciEvidenceMetric(fx.s.now().UTC().Truncate(time.Microsecond).Add(time.Second), map[string]any{"cpuPercent": 1.0})}, nil)
	status, body, _ = ciEvidencePost(t, srv, token, second)
	if status != http.StatusConflict || !strings.Contains(string(body), "conflict") {
		t.Fatalf("hash conflict: %d %s", status, body)
	}
	// Quota: fill the usage row, then a new batch must 429.
	if _, err := fx.s.pool.Exec(context.Background(),
		`UPDATE ci_job_evidence_usage SET batch_count=4096 WHERE workspace_id=$1 AND job_id=$2`,
		fx.workspace, scope.Job); err != nil {
		t.Fatal(err)
	}
	third := ciEvidenceBody(t, ciEvidenceBatchID(111),
		[]any{ciEvidenceMetric(fx.s.now().UTC().Truncate(time.Microsecond).Add(2*time.Second), nil)}, nil)
	status, body, _ = ciEvidencePost(t, srv, token, third)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "quota") {
		t.Fatalf("quota: %d %s", status, body)
	}
}

func TestCIEvidenceHTTPStoreFailure(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)
	// A closed pool makes every store call fail -> fixed 503, no internals.
	fx.s.pool.Close()
	status, body, _ := ciEvidencePost(t, srv, token, ciEvidenceValidBody(t, fx))
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "unavailable") {
		t.Fatalf("store failure: %d %s", status, body)
	}
	if strings.Contains(string(body), token[:16]) || strings.Contains(string(body), fx.workspace) {
		t.Fatalf("503 leaked internals: %s", body)
	}
}

func TestCIEvidenceHTTPConcurrencySlots(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)
	// Occupy every slot: the next valid request fails fast with an empty 503.
	for i := 0; i < ciEvidenceHTTPConcurrency; i++ {
		fx.h.ciEvidenceSlots <- struct{}{}
	}
	status, body, _ := ciEvidencePost(t, srv, token, ciEvidenceValidBody(t, fx))
	if status != http.StatusServiceUnavailable || len(body) != 0 {
		t.Fatalf("overloaded slot: %d body %q", status, body)
	}
	for i := 0; i < ciEvidenceHTTPConcurrency; i++ {
		<-fx.h.ciEvidenceSlots
	}
	if status, _, _ := ciEvidencePost(t, srv, token, ciEvidenceValidBody(t, fx)); status != http.StatusNoContent {
		t.Fatalf("freed slot must accept, got %d", status)
	}
}

func TestCIEvidenceHTTPStallAndCancel(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)

	// A client that vanishes mid-body cannot hold a slot: cancellation joins
	// the deadline watcher and frees the slot promptly.
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ci/runner-evidence", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	// Dribble one byte so the handler is inside ReadAll.
	if _, err := pw.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	pw.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not join")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(fx.h.ciEvidenceSlots) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(fx.h.ciEvidenceSlots) != 0 {
		t.Fatal("canceled request leaked its concurrency slot")
	}

	// A dribbling body hits the five-second bound instead of parking.
	pr2, pw2 := io.Pipe()
	req2, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ci/runner-evidence", pr2)
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Authorization", "Bearer "+token)
	respCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req2)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		respCh <- err
	}()
	go func() {
		for i := 0; i < 15; i++ {
			if _, err := pw2.Write([]byte(" ")); err != nil {
				return
			}
			time.Sleep(400 * time.Millisecond)
		}
		pw2.Close()
	}()
	select {
	case err := <-respCh:
		// A fixed error status or a closed connection are both bounded
		// outcomes; a hang would hit the test timeout instead.
		_ = err
	case <-time.After(9 * time.Second):
		t.Fatal("dribbling body held the request past the bound")
	}
}

func TestCIEvidenceHTTPAdmissionHeader(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	// Pending (advisory busy) carries no header — checked before any commit.
	conn, err := fx.s.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(hashtextextended($1,948))`, "ci/"+fx.workspace+"/"+fx.runner); err != nil {
		t.Fatal(err)
	}
	pendingTok := fx.capability(t, 3600, nil)
	wPending := fx.post(t, fx.req, pendingTok, nil)
	if wPending.Code != http.StatusAccepted {
		t.Fatalf("busy advisory must 202, got %d", wPending.Code)
	}
	if h := wPending.Header().Get(ciEvidenceHeader); h != "" {
		t.Fatal("pending response carried evidence header")
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,948))`, "ci/"+fx.workspace+"/"+fx.runner); err != nil {
		t.Fatal(err)
	}
	// Deny carries no header either: a scope-mismatched request with matching
	// claims earns the durable deny receipt.
	denyReq := fx.req
	denyReq.RunnerName = "canter-other"
	wDeny := fx.post(t, denyReq, fx.capabilityFor(t, denyReq, 3600), nil)
	if wDeny.Code != http.StatusForbidden {
		t.Fatalf("deny: %d", wDeny.Code)
	}
	if h := wDeny.Header().Get(ciEvidenceHeader); h != "" {
		t.Fatal("deny response carried evidence header")
	}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admission: %d %s", w.Code, w.Body.String())
	}
	// Receipt JSON stays exactly the eleven approved keys.
	var receiptMap map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &receiptMap); err != nil {
		t.Fatal(err)
	}
	if len(receiptMap) != 11 {
		t.Fatalf("receipt must keep exactly 11 keys, got %d: %s", len(receiptMap), w.Body.String())
	}
	header := w.Header().Get(ciEvidenceHeader)
	if header == "" {
		t.Fatal("managed admission missing evidence header")
	}
	capability, err := parseCIEvidenceCapability(fx.signingKey, header, fx.s.now())
	if err != nil {
		t.Fatalf("header token parse: %v", err)
	}
	var receipt ciAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if capability.Workspace != fx.workspace || capability.Runner != fx.runner ||
		capability.AdmissionID != fx.nonce || capability.GitHubRepositoryID != fx.req.GitHubRepositoryID ||
		capability.GitHubRunnerID != fx.req.GitHubRunnerID ||
		capability.DeadlineUnixMs != receipt.ExecutionDeadlineUnixMs || capability.GitHubJobID <= 0 {
		t.Fatalf("header claims mismatch: %+v", capability)
	}
	if capability.GitHubInstallationID != 123 {
		t.Fatalf("installation id %d want 123", capability.GitHubInstallationID)
	}
	// The header token authorizes a real evidence upload.
	srv := fx.evidenceServer(t)
	defer srv.Close()
	status, _, _ := ciEvidencePost(t, srv, header, ciEvidenceValidBody(t, fx))
	if status != http.StatusNoContent {
		t.Fatalf("header token upload: %d", status)
	}
	// A retried admission returns the identical receipt and the identical
	// header — the execution clock is never reset.
	w2 := fx.post(t, fx.req, tok, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("admission retry: %d", w2.Code)
	}
	if w2.Header().Get(ciEvidenceHeader) != header {
		t.Fatal("retry minted a different evidence token")
	}
	if w2.Body.String() != w.Body.String() {
		t.Fatal("retry changed the receipt")
	}
	// Plain admission config (no managed runtime): allow receipt, no header.
	fxPlain := newCIAdmissionHTTPFixture(t, "")
	plainTok := fxPlain.capability(t, 3600, nil)
	w5 := fxPlain.post(t, fxPlain.req, plainTok, nil)
	if w5.Code != http.StatusOK {
		t.Fatalf("plain admission: %d", w5.Code)
	}
	if h := w5.Header().Get(ciEvidenceHeader); h != "" {
		t.Fatal("unmanaged admission carried evidence header")
	}
}

func TestCIEvidenceHTTPNoRawLeak(t *testing.T) {
	fx := newCIEvidenceHTTPFixture(t)
	srv := fx.evidenceServer(t)
	defer srv.Close()
	receipt := fx.allow(t, 3600)
	_, token := ciEvidenceAdmittedCapability(t, fx, receipt)
	// Every error body is fixed text; token material and workspace ids never
	// appear in responses.
	for _, body := range [][]byte{
		[]byte(`{bad`),
		bytes.Repeat([]byte("x"), ciEvidenceRequestMaxBytes+1),
	} {
		status, raw, _ := ciEvidencePost(t, srv, token, body)
		if status == http.StatusNoContent {
			t.Fatal("invalid body accepted")
		}
		if strings.Contains(string(raw), token[:20]) || strings.Contains(string(raw), fx.workspace) ||
			strings.Contains(string(raw), "runner_") {
			t.Fatalf("error leaked internals: %d %s", status, raw)
		}
	}
	// Oversize specifically is 413.
	status, _, _ := ciEvidencePost(t, srv, token, bytes.Repeat([]byte("x"), ciEvidenceRequestMaxBytes+1))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status %d", status)
	}
}

// TestCIEvidenceAvailability covers the read-only overview metadata gate:
// job-metrics is available only while a managed Run runtime is configured,
// admission is configured and the runtime is actually running.
func TestCIEvidenceAvailability(t *testing.T) {
	newServer := func(t *testing.T) *HTTPServer {
		fx := newCIEvidenceHTTPFixture(t)
		return fx.h
	}
	cases := []struct {
		name   string
		mutate func(h *HTTPServer)
		want   bool
	}{
		{"managed run configured but not running", func(h *HTTPServer) {}, false},
		{"running managed run", func(h *HTTPServer) {
			h.config.CI.Managed.running.Store(true)
		}, true},
		{"off mode running", func(h *HTTPServer) {
			h.config.CI.Managed.mode = CIManagedOff
			h.config.CI.Managed.running.Store(true)
		}, false},
		{"recover mode running", func(h *HTTPServer) {
			h.config.CI.Managed.mode = CIManagedRecover
			h.config.CI.Managed.running.Store(true)
		}, false},
		{"nil managed", func(h *HTTPServer) {
			h.config.CI.Managed = nil
		}, false},
		{"nil admission", func(h *HTTPServer) {
			h.config.CI.Admission = nil
		}, false},
		{"short signing key", func(h *HTTPServer) {
			h.config.CI.Admission.SigningKey = []byte("short")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newServer(t)
			tc.mutate(h)
			if got := h.ciEvidenceAvailable(); got != tc.want {
				t.Fatalf("ciEvidenceAvailable() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("overview reports job-metrics only while running", func(t *testing.T) {
		fx := newCIEvidenceHTTPFixture(t)
		jobMetrics := func() (string, string) {
			view, err := fx.h.ciOverviewView(context.Background(), Principal{}, fx.workspace)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range view.Features {
				if f.ID == "job-metrics" {
					return f.Status, f.Description
				}
			}
			t.Fatal("job-metrics feature missing")
			return "", ""
		}
		status, desc := jobMetrics()
		if status != "not_configured" {
			t.Fatalf("configured-but-stopped job-metrics: %q", status)
		}
		// A running flag makes Pool() probe the enforcer; give the bare
		// fixture runtime a valid client bound to a dead loopback URL so the
		// probe fails closed instead of dereferencing nil.
		enforcer, err := newCIEnforcerClient("http://127.0.0.1:1",
			[]byte("0123456789abcdef0123456789abcdef"), http.DefaultClient, true)
		if err != nil {
			t.Fatal(err)
		}
		fx.h.config.CI.Managed.enforcer = enforcer
		fx.h.config.CI.Managed.running.Store(true)
		status, desc = jobMetrics()
		if status != "available" || desc != "Guest-reported VM metrics are collected after admission. Submit JUnit files with canter-ci-report junit FILE." {
			t.Fatalf("running job-metrics: %q %q", status, desc)
		}
		fx.h.config.CI.Managed.running.Store(false)
		fx.h.config.CI.Managed.mode = CIManagedOff
		status, _ = jobMetrics()
		if status == "available" {
			t.Fatal("off-mode runtime reported job-metrics available")
		}
	})
}
