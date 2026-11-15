package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ciAdmissionHTTPFixture wires one durable registered runner to a real
// enforcer daemon (engine + typed HTTP server + GitHub management fixture)
// and a real source-proof GitHub fixture behind the control-plane handler.
type ciAdmissionHTTPFixture struct {
	s           *Store
	workspace   string
	runner      string
	plan        ciRunnerLifecyclePlan
	spec        ciEnforcerSpec
	nonce       string
	req         ciAdmissionRequest
	runnerID    int64
	serverID    string
	engine      *ciEnforcerEngine
	fc          *fakeCICompute
	clk         *fakeClock
	gh          *gitHubFixture
	daemon      *httptest.Server
	daemonToken []byte
	signingKey  []byte
	source      *httptest.Server
	sourceCalls *int32
	enforcerN   *int32
	h           *HTTPServer
}

func newCIAdmissionHTTPFixture(t *testing.T, daemonSizeID string) *ciAdmissionHTTPFixture {
	t.Helper()
	ctx := context.Background()
	s, workspace, start := ciFinancialFixture(t)
	runner := ciFinancialRunner(t, s, workspace, "linux-x64-2", 1, true, start)
	ciAdmissionEnable(t, s, workspace)
	ciAdmissionInstallationStatus(t, s, workspace, "active")
	ciAdmissionRepoEnabled(t, s, workspace, true)
	now := s.now()

	opID := "op-admission-http"
	lease, err := ciEnforcerNewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	plan := ciRunnerLifecyclePlan{
		OperationID: opID, ServerName: ciEnforcerRunnerName(opID),
		Region: "r1", FlavorID: "f1", ImageID: "i1", NetworkID: "n1", EnforcerLease: lease,
		SetupDeadlineAt: now.Add(800 * time.Second), HardDeadlineAt: now.Add(20000 * time.Second),
	}
	if err := s.trackCIRunnerLifecycle(ctx, workspace, runner, plan); err != nil {
		t.Fatalf("trackCIRunnerLifecycle: %v", err)
	}
	created, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil || !created {
		t.Fatalf("fenceCIRunnerCreate: %v %v", created, err)
	}

	// Daemon side: real engine, fake compute, parameterized GitHub fixture
	// pinned to the same installation/repository identities as the DB scope.
	fc := newFakeCICompute()
	clk := &fakeClock{wall: now, mono: now}
	gh, _, ghClient := newGitHubFixtureRepo(t, 123, 789, "financial-tests", "repo")
	e, _ := newTestEngineAt(t, fc, ghClient, clk)
	sizeID := daemonSizeID
	if sizeID == "" {
		sizeID = "linux-x64-2"
	}
	spec := ciEnforcerSpec{Plan: plan, GitHubInstallationID: 123, GitHubRepositoryID: 789, SizeID: sizeID}
	serverID := "srv-admission-1"
	putAndCreate(t, e, fc, spec, serverID)
	_, runnerID, _, err := e.registerRunner(ctx, plan.EnforcerLease)
	if err != nil || runnerID <= 0 {
		t.Fatalf("registerRunner: %v id=%d", err, runnerID)
	}
	daemonToken := bytes.Repeat([]byte("d"), 40)
	dsrv := &CIEnforcerServer{engine: e, token: daemonToken}
	dsrv.handler = dsrv.routes()
	daemon := httptest.NewServer(dsrv)
	t.Cleanup(daemon.Close)

	// Source side: installation token, repository and one assigned in-progress
	// job carrying the real JIT runner identity.
	nonce := ciAdmissionNonce("a")
	ciAdmissionPhase(t, s, workspace, runner, "registering", false)
	scope := ciRunnerJobScope{
		RepositoryID: 789, RunnerID: runnerID, RunnerName: plan.ServerName,
		SizeID: "linux-x64-2", CreatedAt: start.UTC().Truncate(time.Microsecond),
	}
	if err := s.registerCIRunnerAdmission(ctx, workspace, runner, nonce, scope); err != nil {
		t.Fatalf("registerCIRunnerAdmission: %v", err)
	}

	sourceCalls := new(int32)
	startedAt := now.Add(-30 * time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/123/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(sourceCalls, 1)
		json.NewEncoder(w).Encode(map[string]any{"token": "source-token-0123456789abcdef", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/repositories/789", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(sourceCalls, 1)
		json.NewEncoder(w).Encode(map[string]any{"id": 789, "full_name": "financial-tests/repo", "private": true})
	})
	mux.HandleFunc("/repos/financial-tests/repo/actions/runs/4242/attempts/2/jobs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(sourceCalls, 1)
		job := map[string]any{
			"id": 5001, "run_id": 4242, "run_attempt": 2,
			"runner_id": runnerID, "runner_name": plan.ServerName,
			"status": "in_progress", "started_at": startedAt.Format(time.RFC3339Nano),
			"completed_at": nil, "conclusion": nil,
			"labels": []string{"canter-2vcpu-ubuntu-2404"},
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []any{job}})
	})
	source := httptest.NewServer(mux)
	t.Cleanup(source.Close)

	signingKey := bytes.Repeat([]byte("s"), 32)
	enforcerN := new(int32)
	h := &HTTPServer{
		service: &Service{Store: s},
		config: HTTPConfig{CI: CIConfig{
			AppID: 4321, AppSlug: "canter-ci", PrivateKeyPEM: ciTestKey(t),
			WebhookSecret: bytes.Repeat([]byte("w"), 32), APIBaseURL: source.URL,
			Admission: &CIAdmissionConfig{
				SigningKey: signingKey, EnforcerURL: daemon.URL,
				EnforcerToken: daemonToken, AllowLoopbackHTTP: true,
				HTTPClient: &http.Client{Transport: ciCountingTransport{base: http.DefaultTransport, n: enforcerN}},
			},
		}},
	}
	req := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: nonce, GitHubRunnerID: runnerID,
		GitHubRepositoryID: 789, Repository: "financial-tests/repo",
		GitHubRunID: 4242, RunAttempt: 2, JobKey: "build", RunnerName: plan.ServerName,
	}
	return &ciAdmissionHTTPFixture{
		s: s, workspace: workspace, runner: runner, plan: plan, spec: spec,
		nonce: nonce, req: req, runnerID: runnerID, serverID: serverID,
		engine: e, fc: fc, clk: clk, gh: gh, daemon: daemon, daemonToken: daemonToken,
		signingKey: signingKey, source: source, sourceCalls: sourceCalls,
		enforcerN: enforcerN, h: h,
	}
}

// ciCountingTransport counts every enforcer client request so tests can
// prove denied requests perform zero enforcer work.
type ciCountingTransport struct {
	base http.RoundTripper
	n    *int32
}

func (t ciCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(t.n, 1)
	return t.base.RoundTrip(r)
}

// ciFailApproveTransport fails the first enforcer Approve call (or runs a
// hook inside it) while passing everything else through.
type ciFailApproveTransport struct {
	base      http.RoundTripper
	fail      *int32
	onApprove func()
}

func (t *ciFailApproveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/approve") {
		if t.onApprove != nil {
			t.onApprove()
		}
		if t.fail != nil && atomic.CompareAndSwapInt32(t.fail, 1, 0) {
			return nil, errors.New("injected approve transport failure")
		}
	}
	return t.base.RoundTrip(r)
}

func (fx *ciAdmissionHTTPFixture) capability(t *testing.T, maxSeconds int64, mutate func(*ciAdmissionCapability)) string {
	t.Helper()
	claims := ciAdmissionCapability{
		Version: 1, Workspace: fx.workspace, Runner: fx.runner,
		AdmissionID: fx.nonce, GitHubRepositoryID: 789, GitHubRunnerID: fx.runnerID,
		RunnerName: fx.plan.ServerName, MaximumSeconds: maxSeconds,
		ExpiresAt: fx.plan.SetupDeadlineAt.Unix(),
	}
	if mutate != nil {
		mutate(&claims)
	}
	tok, err := mintCIAdmissionCapability(fx.signingKey, claims, fx.s.now(), fx.plan.SetupDeadlineAt)
	if err != nil {
		t.Fatalf("mintCIAdmissionCapability: %v", err)
	}
	return tok
}

func (fx *ciAdmissionHTTPFixture) post(t *testing.T, req ciAdmissionRequest, token string, rawBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if rawBody != nil {
		body = bytes.NewReader(rawBody)
	} else {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
	return w
}

func (fx *ciAdmissionHTTPFixture) allow(t *testing.T, maxSeconds int64) ciAdmissionReceipt {
	t.Helper()
	tok := fx.capability(t, maxSeconds, nil)
	w := fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admission status %d body %s", w.Code, w.Body.String())
	}
	var receipt ciAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("receipt decode: %v body %s", err, w.Body.String())
	}
	if receipt.Decision != "allow" || receipt.ExecutionDeadlineUnixMs <= 0 {
		t.Fatalf("bad receipt %+v", receipt)
	}
	return receipt
}

func (fx *ciAdmissionHTTPFixture) denyReceipt(t *testing.T, w *httptest.ResponseRecorder) ciAdmissionDenyReceipt {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected deny 403, got %d body %s", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 10 {
		t.Fatalf("deny receipt must have exactly 10 keys, got %d: %s", len(raw), w.Body.String())
	}
	var out ciAdmissionDenyReceipt
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("deny decode: %v", err)
	}
	if out.Decision != "deny" {
		t.Fatalf("deny decision %q", out.Decision)
	}
	return out
}

func TestCIAdmissionCapability(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	setup := now.Add(800 * time.Second)
	claims := ciAdmissionCapability{
		Version: 1, Workspace: "ws1", Runner: "r1",
		AdmissionID: strings.Repeat("a", 64), GitHubRepositoryID: 789,
		GitHubRunnerID: 5000, RunnerName: "canter-abc123", MaximumSeconds: 3600,
		ExpiresAt: setup.Unix(),
	}
	tok, err := mintCIAdmissionCapability(key, claims, now, setup)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) > ciAdmissionCapabilityMaxBytes {
		t.Fatal("token too large")
	}
	got, err := parseCIAdmissionCapability(key, tok, now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != claims {
		t.Fatalf("claims mismatch %+v vs %+v", got, claims)
	}
	// Tampered payload and foreign key both fail before/inside MAC check.
	parts := strings.Split(tok, ".")
	bad := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := parseCIAdmissionCapability(key, bad, now); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err := parseCIAdmissionCapability(bytes.Repeat([]byte("x"), 32), tok, now); err == nil {
		t.Fatal("foreign-key token accepted")
	}
	// Expired: mint far enough in the past that expiry precedes now.
	past := now.Add(-2000 * time.Second)
	old, err := mintCIAdmissionCapability(key, claims, past, past.Add(800*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseCIAdmissionCapability(key, old, now); err == nil {
		t.Fatal("expired capability accepted")
	}
	// Malformed wire shapes.
	for _, badTok := range []string{"", "v1", "v1..", "v2." + parts[1] + "." + parts[2],
		"v1." + strings.Repeat("A", ciAdmissionCapabilityMaxBytes) + "." + parts[2],
		"v1." + parts[1] + ".!!!", "v1." + parts[1] + "." + parts[2] + ".extra"} {
		if _, err := parseCIAdmissionCapability(key, badTok, now); err == nil {
			t.Fatalf("malformed token accepted: %.40q", badTok)
		}
	}
	// Claims with a VALID MAC but out-of-range fields are still rejected:
	// hand-sign raw claim JSON to bypass mint-side validation.
	signRaw := func(raw string) string {
		payload := base64.RawURLEncoding.EncodeToString([]byte(raw))
		mac := ciAdmissionMACForTest(key, payload)
		return "v1." + payload + "." + mac
	}
	for _, raw := range []string{
		`{"version":1,"workspace":"ws1","runner":"r1","admissionId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"githubRunnerId":5000,"runnerName":"canter-abc123","maximumSeconds":0,"expiresAt":` + fmt.Sprint(now.Add(500*time.Second).Unix()) + `}`,
		`{"version":2,"workspace":"ws1","runner":"r1","admissionId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"githubRunnerId":5000,"runnerName":"canter-abc123","maximumSeconds":3600,"expiresAt":` + fmt.Sprint(now.Add(500*time.Second).Unix()) + `}`,
		`{"version":1,"workspace":"ws1","runner":"r1","admissionId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"githubRunnerId":5000,"runnerName":"canter-abc123","maximumSeconds":3600,"expiresAt":` + fmt.Sprint(now.Add(500*time.Second).Unix()) + `,"extra":1}`,
		`{"version":1,"workspace":"ws1","runner":"r1","admissionId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"githubRunnerId":5000,"runnerName":"canter-abc123","maximumSeconds":3600,"expiresAt":` + fmt.Sprint(now.Add(500*time.Second).Unix()) + `,"maximumSeconds":7200}`,
		`{"version":1,"workspace":"ws 1","runner":"r1","admissionId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"githubRunnerId":5000,"runnerName":"canter-abc123","maximumSeconds":3600,"expiresAt":` + fmt.Sprint(now.Add(500*time.Second).Unix()) + `}`,
		`{"version":1,"workspace":"ws1","runner":"r1","admissionId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"githubRunnerId":5000,"runnerName":"canter-abc123","maximumSeconds":3600,"expiresAt":` + fmt.Sprint(now.Add(500*time.Second).Unix()) + `,"admissionId":null}`,
	} {
		if _, err := parseCIAdmissionCapability(key, signRaw(raw), now); err == nil {
			t.Fatalf("invalid claims accepted: %.80s", raw)
		}
	}
}

// ciAdmissionMACForTest reproduces the wire MAC for hand-built claim JSON.
func ciAdmissionMACForTest(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciAdmissionMACDomain + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestCIAdmissionHTTPUnconfigured(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	fx.h.config.CI.Admission = nil
	w := fx.post(t, fx.req, "whatever", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured admission must 404, got %d", w.Code)
	}
	// Invalid config variants also leave the route absent.
	for _, cfg := range []*CIAdmissionConfig{
		{SigningKey: []byte("short"), EnforcerToken: bytes.Repeat([]byte("e"), 32), EnforcerURL: fx.daemon.URL, AllowLoopbackHTTP: true},
		{SigningKey: fx.signingKey, EnforcerToken: fx.signingKey, EnforcerURL: fx.daemon.URL, AllowLoopbackHTTP: true},
		{SigningKey: fx.signingKey, EnforcerToken: fx.daemonToken, EnforcerURL: "http://example.com", AllowLoopbackHTTP: false},
	} {
		fx.h.config.CI.Admission = cfg
		w := fx.post(t, fx.req, "whatever", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("invalid admission config must 404, got %d", w.Code)
		}
	}
}

func TestCIAdmissionHTTPPositiveFlow(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)

	// Receipt echoes the exact request identity plus committed decision.
	if receipt.ciAdmissionRequest != fx.req {
		t.Fatalf("receipt request mismatch %+v vs %+v", receipt.ciAdmissionRequest, fx.req)
	}
	started := time.UnixMilli(fx.s.now().UnixMilli()).Add(-30 * time.Second)
	deadline := started.Add(3600 * time.Second)
	if receipt.ExecutionDeadlineUnixMs != deadline.UnixMilli() {
		t.Fatalf("deadline %d want %d", receipt.ExecutionDeadlineUnixMs, deadline.UnixMilli())
	}

	// DB committed: decision, running phase, deadline, adopted job.
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Phase != "running" || snap.ExecutionStartedAt == nil || snap.DeadlineAt == nil {
		t.Fatalf("lifecycle not committed: %+v", snap)
	}
	if snap.DeadlineAt.UnixMilli() != receipt.ExecutionDeadlineUnixMs {
		t.Fatalf("persisted deadline %d != receipt %d", snap.DeadlineAt.UnixMilli(), receipt.ExecutionDeadlineUnixMs)
	}
	var decision ciAdmissionDecision
	if err := json.Unmarshal(snap.Decision, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Receipt != receipt {
		t.Fatalf("persisted receipt %+v != delivered %+v", decision.Receipt, receipt)
	}
	// Daemon inventory was persisted before the clock started: the server
	// resource and create-settled flag are durable in the lifecycle row.
	found := false
	for _, res := range snap.Resources {
		if res.Kind == "server" && res.ID == fx.serverID {
			found = true
		}
	}
	if !found {
		t.Fatalf("daemon server inventory not retained: %+v", snap.Resources)
	}

	// The enforcer approved the EXACT committed deadline.
	enfClient, err := newCIEnforcerClient(fx.daemon.URL, fx.daemonToken, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := enfClient.Status(context.Background(), fx.plan)
	if err != nil {
		t.Fatal(err)
	}
	if st.DeadlineUnixMs != receipt.ExecutionDeadlineUnixMs {
		t.Fatalf("enforcer deadline %d != receipt %d", st.DeadlineUnixMs, receipt.ExecutionDeadlineUnixMs)
	}
	// One source proof only; a byte-identical retry must not re-prove.
	calls := atomic.LoadInt32(fx.sourceCalls)
	again := fx.allow(t, 3600)
	if again != receipt {
		t.Fatalf("idempotent retry receipt %+v != %+v", again, receipt)
	}
	if atomic.LoadInt32(fx.sourceCalls) != calls {
		t.Fatal("retry triggered a second source proof")
	}
}

func TestCIAdmissionHTTPAuthDenials(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	good := fx.capability(t, 3600, nil)

	// A second Authorization header is never accepted.
	raw, _ := json.Marshal(fx.req)
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", bytes.NewReader(raw))
	r.Header.Add("Authorization", "Bearer "+good)
	r.Header.Add("Authorization", "Bearer "+good)
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate Authorization must 401, got %d", w.Code)
	}

	expired := fx.capabilityAt(t, fx.s.now().Add(-2000*time.Second), 3600, nil)
	foreign := mintWithKey(t, bytes.Repeat([]byte("z"), 32), fx, 3600)
	cases := map[string]string{
		"missing":         "",
		"garbage":         "not-a-token",
		"tampered":        tamperToken(good),
		"oversize":        "v1." + strings.Repeat("a", ciAdmissionCapabilityMaxBytes) + "." + strings.Repeat("b", 43),
		"foreign-key":     foreign,
		"expired":         expired,
		"wrong-admission": fx.capabilityWithNonce(t, ciAdmissionNonce("b"), 3600),
		"bad-max":         fx.capabilityMutatedInvalid(t, 3600, func(c *ciAdmissionCapability) { c.MaximumSeconds = 0 }),
	}
	for name, tok := range cases {
		beforeN, beforeE := atomic.LoadInt32(fx.enforcerN), atomic.LoadInt32(fx.sourceCalls)
		w := fx.post(t, fx.req, tok, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d body %s", name, w.Code, w.Body.String())
		}
		if atomic.LoadInt32(fx.enforcerN) != beforeN || atomic.LoadInt32(fx.sourceCalls) != beforeE {
			t.Fatalf("%s: denied request performed enforcer/source work", name)
		}
	}
	// No decision may be committed by any denial.
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Decision) != 0 {
		t.Fatal("denied request committed a decision")
	}
}

func (fx *ciAdmissionHTTPFixture) capabilityAt(t *testing.T, now time.Time, maxSeconds int64, mutate func(*ciAdmissionCapability)) string {
	t.Helper()
	claims := ciAdmissionCapability{
		Version: 1, Workspace: fx.workspace, Runner: fx.runner,
		AdmissionID: fx.nonce, GitHubRepositoryID: 789, GitHubRunnerID: fx.runnerID,
		RunnerName: fx.plan.ServerName, MaximumSeconds: maxSeconds,
		ExpiresAt: fx.plan.SetupDeadlineAt.Unix(),
	}
	if mutate != nil {
		mutate(&claims)
	}
	tok, err := mintCIAdmissionCapability(fx.signingKey, claims, now, fx.plan.SetupDeadlineAt)
	if err != nil {
		t.Fatalf("mintCIAdmissionCapability: %v", err)
	}
	return tok
}

func (fx *ciAdmissionHTTPFixture) capabilityWithNonce(t *testing.T, nonce string, maxSeconds int64) string {
	t.Helper()
	return fx.capabilityMutated(t, maxSeconds, func(c *ciAdmissionCapability) { c.AdmissionID = nonce })
}

func (fx *ciAdmissionHTTPFixture) capabilityMutated(t *testing.T, maxSeconds int64, mutate func(*ciAdmissionCapability)) string {
	t.Helper()
	return fx.capabilityAt(t, fx.s.now(), maxSeconds, mutate)
}

// capabilityMutatedInvalid hand-signs claims mint would reject, producing a
// token that passes MAC but must fail strict claim validation.
func (fx *ciAdmissionHTTPFixture) capabilityMutatedInvalid(t *testing.T, maxSeconds int64, mutate func(*ciAdmissionCapability)) string {
	t.Helper()
	claims := ciAdmissionCapability{
		Version: 1, Workspace: fx.workspace, Runner: fx.runner,
		AdmissionID: fx.nonce, GitHubRepositoryID: 789, GitHubRunnerID: fx.runnerID,
		RunnerName: fx.plan.ServerName, MaximumSeconds: maxSeconds,
		ExpiresAt: fx.plan.SetupDeadlineAt.Unix(),
	}
	mutate(&claims)
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return "v1." + payload + "." + ciAdmissionMACForTest(fx.signingKey, payload)
}

func mintWithKey(t *testing.T, key []byte, fx *ciAdmissionHTTPFixture, maxSeconds int64) string {
	t.Helper()
	claims := ciAdmissionCapability{
		Version: 1, Workspace: fx.workspace, Runner: fx.runner,
		AdmissionID: fx.nonce, GitHubRepositoryID: 789, GitHubRunnerID: fx.runnerID,
		RunnerName: fx.plan.ServerName, MaximumSeconds: maxSeconds,
		ExpiresAt: fx.plan.SetupDeadlineAt.Unix(),
	}
	tok, err := mintCIAdmissionCapability(key, claims, fx.s.now(), fx.plan.SetupDeadlineAt)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func tamperToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "garbage"
	}
	p := []byte(parts[1])
	if p[0] == 'A' {
		p[0] = 'B'
	} else {
		p[0] = 'A'
	}
	parts[1] = string(p)
	return strings.Join(parts, ".")
}

func TestCIAdmissionHTTPStrictBody(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	tok := fx.capability(t, 3600, nil)
	valid, _ := json.Marshal(fx.req)

	oversize := append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), ciAdmissionRequestMaxBytes)...)
	cases := map[string][]byte{
		"empty":        {},
		"garbage":      []byte("{not json"),
		"oversize":     oversize,
		"duplicate":    []byte(strings.Replace(string(valid), `"jobKey":"build"`, `"jobKey":"build","jobKey":"build"`, 1)),
		"null-field":   []byte(strings.Replace(string(valid), `"jobKey":"build"`, `"jobKey":null`, 1)),
		"unknown-key":  []byte(strings.Replace(string(valid), `"jobKey":"build"`, `"jobKey":"build","extra":1`, 1)),
		"wrong-schema": []byte(strings.Replace(string(valid), `"schemaVersion":1`, `"schemaVersion":2`, 1)),
		"trailing":     append(append([]byte{}, valid...), []byte(" {}")...),
	}
	for name, body := range cases {
		w := fx.post(t, fx.req, tok, body)
		want := http.StatusBadRequest
		if name == "oversize" {
			want = http.StatusRequestEntityTooLarge
		}
		if w.Code != want {
			t.Fatalf("%s: expected %d, got %d body %s", name, want, w.Code, w.Body.String())
		}
	}
	// A non-POST never reaches the handler.
	r := httptest.NewRequest(http.MethodGet, "/v1/ci/runner-admissions", nil)
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET must 404, got %d", w.Code)
	}
}

func TestCIAdmissionHTTPScopeDenials(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")

	// Authenticated and well-formed, but the durable scope disagrees: the
	// request must receive the exact ten-key deny receipt.
	mutations := map[string]func(*ciAdmissionRequest){
		"wrong-runner-id":   func(r *ciAdmissionRequest) { r.GitHubRunnerID = fx.runnerID + 1 },
		"wrong-repo-id":     func(r *ciAdmissionRequest) { r.GitHubRepositoryID = 790 },
		"wrong-runner-name": func(r *ciAdmissionRequest) { r.RunnerName = "canter-other" },
		"wrong-admission":   func(r *ciAdmissionRequest) { r.AdmissionID = ciAdmissionNonce("b") },
	}
	for name, mutate := range mutations {
		req := fx.req
		mutate(&req)
		// Claims must still bind the mutated request or auth fails first;
		// mint claims matching the mutated request so scope is what denies.
		tok := fx.capabilityFor(t, req, 3600)
		w := fx.post(t, req, tok, nil)
		deny := fx.denyReceipt(t, w)
		if deny.ciAdmissionRequest != req {
			t.Fatalf("%s: deny receipt does not echo request %+v", name, deny.ciAdmissionRequest)
		}
	}
	// A request whose claims cannot even be minted for the mutated identity
	// (runner id positive but not the registered one) hits the same path via
	// claims==request but scope mismatch.
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Decision) != 0 {
		t.Fatal("scope denial committed a decision")
	}
}

func (fx *ciAdmissionHTTPFixture) capabilityFor(t *testing.T, req ciAdmissionRequest, maxSeconds int64) string {
	t.Helper()
	claims := ciAdmissionCapability{
		Version: 1, Workspace: fx.workspace, Runner: fx.runner,
		AdmissionID: req.AdmissionID, GitHubRepositoryID: req.GitHubRepositoryID,
		GitHubRunnerID: req.GitHubRunnerID, RunnerName: req.RunnerName,
		MaximumSeconds: maxSeconds, ExpiresAt: fx.plan.SetupDeadlineAt.Unix(),
	}
	tok, err := mintCIAdmissionCapability(fx.signingKey, claims, fx.s.now(), fx.plan.SetupDeadlineAt)
	if err != nil {
		t.Fatalf("mintCIAdmissionCapability: %v", err)
	}
	return tok
}

func TestCIAdmissionHTTPLifecycleDenials(t *testing.T) {
	// Unregistered runner: no admission scope row exists yet.
	fx := newCIAdmissionHTTPFixture(t, "")
	// Move to a second runner that was never registered.
	runner2 := ciFinancialRunner(t, fx.s, fx.workspace, "linux-x64-2", 2, true, fx.s.now().Add(-7*time.Hour))
	lease2, _ := ciEnforcerNewLeaseID()
	plan2 := fx.plan
	plan2.OperationID = "op-admission-http-2"
	plan2.ServerName = ciEnforcerRunnerName(plan2.OperationID)
	plan2.EnforcerLease = lease2
	ctx := context.Background()
	if err := fx.s.trackCIRunnerLifecycle(ctx, fx.workspace, runner2, plan2); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.fenceCIRunnerCreate(ctx, fx.workspace, runner2); err != nil {
		t.Fatal(err)
	}
	ciAdmissionPhase(t, fx.s, fx.workspace, runner2, "registering", false)
	// No daemon lease, no admission registration: claims for runner2 deny.
	tok := fx.capabilityForRunner(t, fx.workspace, runner2, plan2, ciAdmissionNonce("c"), 789, 888, 3600)
	req := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: ciAdmissionNonce("c"), GitHubRunnerID: 888,
		GitHubRepositoryID: 789, Repository: "financial-tests/repo",
		GitHubRunID: 4242, RunAttempt: 2, JobKey: "build", RunnerName: plan2.ServerName,
	}
	w := fx.post(t, req, tok, nil)
	fx.denyReceipt(t, w)

	// Stopped runner: cancel via the existing stop helper, then deny.
	fx2 := newCIAdmissionHTTPFixture(t, "")
	if _, err := fx2.s.requestCIRunnerStop(ctx, fx2.workspace, fx2.runner, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	tok2 := fx2.capability(t, 3600, nil)
	w = fx2.post(t, fx2.req, tok2, nil)
	fx2.denyReceipt(t, w)
}

func (fx *ciAdmissionHTTPFixture) capabilityForRunner(t *testing.T, workspace, runner string, plan ciRunnerLifecyclePlan, nonce string, repoID, runnerID int64, maxSeconds int64) string {
	t.Helper()
	claims := ciAdmissionCapability{
		Version: 1, Workspace: workspace, Runner: runner,
		AdmissionID: nonce, GitHubRepositoryID: repoID, GitHubRunnerID: runnerID,
		RunnerName: plan.ServerName, MaximumSeconds: maxSeconds,
		ExpiresAt: plan.SetupDeadlineAt.Unix(),
	}
	tok, err := mintCIAdmissionCapability(fx.signingKey, claims, fx.s.now(), plan.SetupDeadlineAt)
	if err != nil {
		t.Fatalf("mintCIAdmissionCapability: %v", err)
	}
	return tok
}

func TestCIAdmissionHTTPApproveFailureRetry(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	fail := int32(1)
	fx.h.config.CI.Admission.HTTPClient = &http.Client{
		Transport: &ciFailApproveTransport{base: http.DefaultTransport, fail: &fail},
	}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("approve failure must 503, got %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"decision":"allow"`) {
		t.Fatal("failed attempt leaked an allow receipt")
	}
	// Snapshot the first committed decision/quote/clock/budget BEFORE retry.
	snap1, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	decisionRaw1 := append([]byte(nil), snap1.Decision...)
	started1 := snap1.ExecutionStartedAt
	deadline1 := snap1.DeadlineAt
	units1, cents1, reserved1 := ciFinancialTotals(t, fx.s, fx.workspace)
	// The decision/quote committed before Approve; retry must recover the
	// exact same receipt without a second source proof.
	calls := atomic.LoadInt32(fx.sourceCalls)
	w = fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("retry must succeed, got %d body %s", w.Code, w.Body.String())
	}
	var receipt ciAdmissionReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(fx.sourceCalls) != calls {
		t.Fatal("retry performed a second source proof")
	}
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap.Decision, decisionRaw1) {
		t.Fatal("retry rewrote the committed decision bytes")
	}
	if snap.ExecutionStartedAt == nil || !snap.ExecutionStartedAt.Equal(*started1) ||
		snap.DeadlineAt == nil || !snap.DeadlineAt.Equal(*deadline1) {
		t.Fatal("retry mutated the committed clock")
	}
	units2, cents2, reserved2 := ciFinancialTotals(t, fx.s, fx.workspace)
	if units1 != units2 || cents1 != cents2 || reserved1 != reserved2 {
		t.Fatalf("retry mutated budget totals %d/%d/%d -> %d/%d/%d",
			units1, cents1, reserved1, units2, cents2, reserved2)
	}
	var decision ciAdmissionDecision
	if err := json.Unmarshal(snap.Decision, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Receipt != receipt {
		t.Fatalf("recovered receipt %+v != committed %+v", receipt, decision.Receipt)
	}
	// Third identical retry stays byte-identical.
	w = fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("second retry must succeed, got %d", w.Code)
	}
	var receipt2 ciAdmissionReceipt
	json.Unmarshal(w.Body.Bytes(), &receipt2)
	if receipt2 != receipt {
		t.Fatal("retry receipts differ")
	}
}

func TestCIAdmissionHTTPCancelDuringApprove(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	fx.h.config.CI.Admission.HTTPClient = &http.Client{
		Transport: &ciFailApproveTransport{
			base: http.DefaultTransport,
			onApprove: func() {
				// Cancel committed while the enforcer approval is in flight.
				_, _ = fx.s.requestCIRunnerStop(context.Background(), fx.workspace, fx.runner, "cancelled", nil)
			},
		},
	}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
	// The stop is durable; every later request denies without allow.
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.StopRequestedAt == nil {
		t.Fatal("stop was not committed during approve")
	}
	w = fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
}

func TestCIAdmissionHTTPDeadlineCaps(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	// First allow with a 7200s cap commits quote+decision.
	first := fx.allow(t, 7200)
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	decisionRaw := append([]byte(nil), snap.Decision...)
	started := snap.ExecutionStartedAt

	// A retry claiming a different (valid, smaller) cap must deny and must
	// not rewrite the committed quote or decision.
	tok3600 := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok3600, nil)
	fx.denyReceipt(t, w)
	snap2, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap2.Decision, decisionRaw) {
		t.Fatal("different-cap retry rewrote the committed decision")
	}
	if snap2.ExecutionStartedAt == nil || !snap2.ExecutionStartedAt.Equal(*started) {
		t.Fatal("execution start mutated")
	}
	// Exact retry of the original cap still returns the original receipt.
	again := fx.allow(t, 7200)
	if again != first {
		t.Fatalf("cap retry receipt %+v != %+v", again, first)
	}
}

func TestCIAdmissionHTTPAdvisoryBusyPending(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	ctx := context.Background()
	conn, err := fx.s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,948))`, "ci/"+fx.workspace+"/"+fx.runner); err != nil {
		t.Fatal(err)
	}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("busy advisory lock must 202, got %d body %s", w.Code, w.Body.String())
	}
	var pending map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil || !pending["pending"] || len(pending) != 1 {
		t.Fatalf("bad pending body %s", w.Body.String())
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,948))`, "ci/"+fx.workspace+"/"+fx.runner); err != nil {
		t.Fatal(err)
	}
	// No decision was committed while pending.
	snap, err := fx.s.loadCIRunnerLifecycle(ctx, fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Decision) != 0 {
		t.Fatal("pending request committed a decision")
	}
	fx.allow(t, 3600)
}

func TestCIAdmissionHTTPEnforcerSpecMismatch(t *testing.T) {
	// The daemon lease was registered under a different size identity than
	// the durable admission scope, so the full spec digest cannot match.
	fx := newCIAdmissionHTTPFixture(t, "linux-x64-4")
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Decision) != 0 {
		t.Fatal("spec mismatch committed a decision")
	}
}

// ciBlockingApproveTransport blocks inside the enforcer Approve call until
// released, proving response ordering and letting tests inspect committed
// state mid-approval.
type ciBlockingApproveTransport struct {
	base      http.RoundTripper
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	onApprove func()
}

func (t *ciBlockingApproveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/approve") {
		t.once.Do(func() { close(t.entered) })
		if t.onApprove != nil {
			t.onApprove()
		}
		select {
		case <-t.release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return t.base.RoundTrip(r)
}

// ciMutateApproveTransport rewrites the enforcer Approve response body.
type ciMutateApproveTransport struct {
	base   http.RoundTripper
	mutate func(map[string]any)
}

func (t *ciMutateApproveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil || resp == nil || r.Method != http.MethodPost ||
		!strings.Contains(r.URL.Path, "/approve") {
		return resp, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("approve response is not an object")
	}
	t.mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header,
		Body: io.NopCloser(bytes.NewReader(out)), ContentLength: int64(len(out)),
		Request: r,
	}, nil
}

// ciCountingReader counts bytes actually pulled from the request body.
type ciCountingReader struct {
	r io.Reader
	n *int32
}

func (c *ciCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt32(c.n, int32(n))
	return n, err
}

func TestCIAdmissionHTTPApproveCommitsBeforeACK(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()
	tr := &ciBlockingApproveTransport{
		base: http.DefaultTransport, entered: entered, release: release,
		onApprove: func() {
			// Inside the enforcer Approve call the durable admission must
			// already be committed: decision, source proof, quote, clock,
			// and the retained server inventory.
			snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
			if err != nil {
				t.Errorf("load lifecycle inside approve: %v", err)
				return
			}
			var decision ciAdmissionDecision
			if err := json.Unmarshal(snap.Decision, &decision); err != nil {
				t.Errorf("decision inside approve: %v", err)
				return
			}
			if decision.Proof.JobID != 5001 || decision.Receipt.Decision != "allow" {
				t.Errorf("decision not committed before approve: %+v", decision)
			}
			if snap.Phase != "running" || snap.ExecutionStartedAt == nil || snap.DeadlineAt == nil {
				t.Errorf("execution clock not committed: %+v", snap)
			}
			found := false
			for _, res := range snap.Resources {
				if res.Kind == "server" && res.ID == fx.serverID {
					found = true
				}
			}
			if !found {
				t.Errorf("server inventory missing inside approve: %+v", snap.Resources)
			}
		},
	}
	fx.h.config.CI.Admission.HTTPClient = &http.Client{Transport: tr}
	tok := fx.capability(t, 3600, nil)
	raw, _ := json.Marshal(fx.req)
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("enforcer approve was never reached")
	}
	select {
	case <-done:
		t.Fatal("response completed while approve ACK was blocked")
	case <-time.After(300 * time.Millisecond):
	}
	releaseFn()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish after approve release")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestCIAdmissionHTTPApproveRunnerIDMismatch(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	fx.h.config.CI.Admission.HTTPClient = &http.Client{Transport: &ciMutateApproveTransport{
		base: http.DefaultTransport,
		mutate: func(m map[string]any) {
			m["githubRunnerId"] = fx.runnerID + 1
		},
	}}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
}

func TestCIAdmissionHTTPCapabilityExpiresDuringApprove(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	base := fx.s.now()
	tok := fx.capability(t, 3600, func(c *ciAdmissionCapability) {
		c.ExpiresAt = base.Add(60 * time.Second).Unix()
	})
	fx.h.config.CI.Admission.HTTPClient = &http.Client{Transport: &ciFailApproveTransport{
		base: http.DefaultTransport,
		onApprove: func() {
			// The capability expires while the enforcer approval is in
			// flight; the daemon clock is unaffected.
			fx.s.now = func() time.Time { return base.Add(61 * time.Second) }
		},
	}}
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
}

func TestCIAdmissionHTTPBadAuthReadsNoBody(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	var n int32
	raw, _ := json.Marshal(fx.req)
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", nil)
	r.Body = io.NopCloser(&ciCountingReader{r: bytes.NewReader(raw), n: &n})
	r.Header.Set("Authorization", "Bearer not-a-capability")
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad auth must 401, got %d", w.Code)
	}
	if atomic.LoadInt32(&n) != 0 {
		t.Fatal("request body was read before authentication")
	}
}

func TestCIAdmissionHTTPInvalidRetainedResource(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	ctx := context.Background()
	snap, err := fx.s.loadCIRunnerLifecycle(ctx, fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	// A resource bound to a foreign operation is invalid for this lease; it
	// must fail the admission rather than be silently discarded.
	bad := ciOwnedResource{Kind: "server", ID: "srv-foreign", OperationID: "op-foreign"}
	combined := append(append([]ciOwnedResource{}, snap.Resources...), bad)
	if err := fx.s.ciLifecycleSaveEvidence(ctx, fx.workspace, fx.runner, snap, combined, snap.CreateSettled, false); err != nil {
		t.Fatal(err)
	}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
	snap2, err := fx.s.loadCIRunnerLifecycle(ctx, fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap2.Decision) != 0 {
		t.Fatal("invalid retained inventory admitted a quote")
	}
	found := false
	for _, res := range snap2.Resources {
		if res.ID == "srv-foreign" {
			found = true
		}
	}
	if !found {
		t.Fatal("invalid retained resource ID was discarded")
	}
}

func TestCIAdmissionHTTPMaxConnsOne(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	old := fx.s.pool
	cfg := *old.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.s.pool = pool
	defer func() {
		fx.s.pool = old
		pool.Close()
	}()
	// The full positive path — including the advisory-locked inventory
	// persist — must complete on a single connection under a 2s deadline.
	tok := fx.capability(t, 3600, nil)
	raw, _ := json.Marshal(fx.req)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
	if w.Code != http.StatusOK {
		t.Fatalf("single-connection admission failed: %d body %s", w.Code, w.Body.String())
	}
}

func TestCIAdmissionHTTPCrossWorkspace(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	_, wsB, _, err := fx.s.Signup(context.Background(), "other@example.com", "correct-password-123", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// A capability naming workspace B but runner A's durable identity finds
	// no lifecycle row: deny with zero source and zero enforcer work.
	tok := fx.capabilityForRunner(t, wsB.ID, fx.runner, fx.plan, fx.nonce, 789, fx.runnerID, 3600)
	beforeN, beforeE := atomic.LoadInt32(fx.enforcerN), atomic.LoadInt32(fx.sourceCalls)
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
	if atomic.LoadInt32(fx.enforcerN) != beforeN || atomic.LoadInt32(fx.sourceCalls) != beforeE {
		t.Fatal("cross-workspace denial performed enforcer/source work")
	}
	// The original workspace identity still admits normally afterwards.
	fx.allow(t, 3600)
}

func TestCIAdmissionHTTPTransientFailureIsUnavailable(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	// A dead pool makes the lifecycle load fail transiently: 503, never a
	// pending 202 or a durable 403.
	fx.s.pool.Close()
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("transient failure must 503, got %d body %s", w.Code, w.Body.String())
	}
}

func TestCIAdmissionHTTPApproveMissingAttemptFlag(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	fx.h.config.CI.Admission.HTTPClient = &http.Client{Transport: &ciMutateApproveTransport{
		base: http.DefaultTransport,
		mutate: func(m map[string]any) {
			m["registrationAttempted"] = false
		},
	}}
	tok := fx.capability(t, 3600, nil)
	w := fx.post(t, fx.req, tok, nil)
	fx.denyReceipt(t, w)
}
