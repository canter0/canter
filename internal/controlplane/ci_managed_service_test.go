package controlplane

import (
	"context"
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
)

func ciServiceTestRuntime() *CIManagedRuntime {
	return &CIManagedRuntime{
		mode: CIManagedRun, signingKey: []byte(strings.Repeat("k", 32)),
		profileDigest: strings.Repeat("a", 64),
		enforcer:      &ciEnforcerClient{base: "https://enforcer.example.test", token: []byte(strings.Repeat("t", 40))},
	}
}

func ciServiceAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("managed service did not reach expected synchronization point")
		var zero T
		return zero
	}
}

func TestCIManagedServiceRetryAndWorkerFailure(t *testing.T) {
	core, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	runtime := ciServiceTestRuntime()
	s := NewCIManagedService(CIManagedRun, func(ctx context.Context) (*CIManagedRuntime, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			return nil, errors.New("unbounded startup probe")
		}
		if calls.Add(1) == 1 {
			return nil, &ciManagedStartupError{reason: "enforcer_unavailable", retryable: true}
		}
		return runtime, nil
	})
	var mu sync.Mutex
	var logs []string
	s.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	waiting, retry := make(chan time.Duration, 1), make(chan struct{}, 1)
	s.jitter = func(d time.Duration) time.Duration { return d }
	s.wait = func(ctx context.Context, d time.Duration) bool {
		waiting <- d
		select {
		case <-ctx.Done():
			return false
		case <-retry:
			return true
		}
	}
	started, stopWorker := make(chan bool, 1), make(chan struct{}, 1)
	s.run = func(ctx context.Context, r *CIManagedRuntime, publish func()) error {
		_, hasDeadline := ctx.Deadline()
		r.running.Store(true)
		defer r.running.Store(false)
		publish()
		started <- !hasDeadline
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopWorker:
			return errors.New("SECRET-provider-error")
		}
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- s.Run(core)
	}()
	t.Cleanup(func() {
		cancel()
		ciServiceAwait(t, done)
	})
	if delay := ciServiceAwait(t, waiting); delay != time.Second || calls.Load() != 1 {
		t.Fatalf("startup did not back off exactly once: %v, calls=%d", delay, calls.Load())
	}
	if pool := s.pool(core); pool.Ready || pool.Status != "waiting_dependency" {
		t.Fatalf("incorrect waiting state: %+v", pool)
	}
	retry <- struct{}{}
	if !ciServiceAwait(t, started) {
		t.Fatal("startup probe deadline leaked into the worker lifetime")
	}
	requestCtx, release, ok := s.acquire(core)
	if !ok {
		t.Fatal("running service did not admit a request handle")
	}
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	defer releaseOnce()
	if err := s.Run(core); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping service start accepted: %v", err)
	}
	handler := NewHTTPServer(&Service{Store: &Store{now: time.Now}}, HTTPConfig{CI: CIConfig{ManagedService: s}})
	body := &ciManagedUnreadBody{}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap", body))
	if w.Code != http.StatusUnauthorized || body.reads.Load() != 0 {
		t.Fatalf("running route bypassed authentication: status=%d reads=%d", w.Code, body.reads.Load())
	}
	stopWorker <- struct{}{}
	ciServiceAwait(t, requestCtx.Done())
	if core.Err() != nil {
		t.Fatal("managed failure canceled the core API context")
	}
	select {
	case <-done:
		t.Fatal("service returned before its active request joined")
	default:
	}
	if _, _, ok := s.acquire(core); ok {
		t.Fatal("failed service admitted another request")
	}
	releaseOnce()
	if err := ciServiceAwait(t, done); err == nil {
		t.Fatal("unexpected worker stop was not reported")
	}
	if pool := s.pool(core); pool.Ready || pool.Status != "failed" || calls.Load() != 2 {
		t.Fatalf("worker was replayed or failure was hidden: %+v, calls=%d", pool, calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(strings.Join(logs, "\n"), "SECRET") {
		t.Fatal("raw worker error leaked into lifecycle logs")
	}
}

func TestCIManagedServicePermanentFailureAndOff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  CIManagedMode
		err   error
		calls int32
		state string
	}{
		{"off", CIManagedOff, nil, 0, "not_configured"},
		{"empty", "", nil, 0, "not_configured"},
		{"invalid mode", "SECRET-invalid-mode", nil, 0, "failed"},
		{"configuration", CIManagedRun, errors.New("SECRET-configuration"), 1, "failed"},
		{"identity", CIManagedRun, &ciManagedStartupError{reason: "enforcer_app_mismatch"}, 1, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := NewCIManagedService(tc.mode, func(context.Context) (*CIManagedRuntime, error) {
				calls.Add(1)
				return nil, tc.err
			})
			s.wait = func(context.Context, time.Duration) bool {
				t.Fatal("permanent configuration failure was retried")
				return false
			}
			_ = s.Run(context.Background())
			pool := s.pool(context.Background())
			if calls.Load() != tc.calls || pool.Status != tc.state || pool.Ready {
				t.Fatalf("calls=%d pool=%+v", calls.Load(), pool)
			}
		})
	}
}

func TestCIManagedServiceShutdownDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{}, 1)
	s := NewCIManagedService(CIManagedRun, func(context.Context) (*CIManagedRuntime, error) {
		return nil, &ciManagedStartupError{reason: "enforcer_unavailable", retryable: true}
	})
	s.wait = func(ctx context.Context, _ time.Duration) bool {
		waiting <- struct{}{}
		<-ctx.Done()
		return false
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	ciServiceAwait(t, waiting)
	cancel()
	if err := ciServiceAwait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown did not cancel startup: %v", err)
	}
}

type ciManagedUnreadBody struct{ reads atomic.Int32 }

func (b *ciManagedUnreadBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	return 0, io.EOF
}
func (*ciManagedUnreadBody) Close() error { return nil }

type ciManagedBlockedBody struct {
	reading chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *ciManagedBlockedBody) Read([]byte) (int, error) {
	close(b.reading)
	<-b.closed
	return 0, context.Canceled
}

func (b *ciManagedBlockedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestCIManagedServiceHTTPFailureCancelsRequest(t *testing.T) {
	core, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := ciServiceTestRuntime()
	s := NewCIManagedService(CIManagedRun, func(context.Context) (*CIManagedRuntime, error) { return runtime, nil })
	started, fail := make(chan struct{}, 1), make(chan struct{}, 1)
	s.run = func(ctx context.Context, r *CIManagedRuntime, publish func()) error {
		r.running.Store(true)
		defer r.running.Store(false)
		publish()
		started <- struct{}{}
		select {
		case <-ctx.Done():
		case <-fail:
		}
		return errors.New("worker stopped")
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- s.Run(core)
	}()
	body := &ciManagedBlockedBody{reading: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		_ = body.Close()
		ciServiceAwait(t, done)
	})
	ciServiceAwait(t, started)
	now := time.Now().UTC()
	token, err := mintCIBootstrapCapability(runtime.signingKey, ciBootstrapClaims{
		Version: 1, Workspace: "ws", Runner: "runner", OperationID: "operation",
		BootstrapID: strings.Repeat("a", 64), GitHubRepositoryID: 789,
		PlanDigest: strings.Repeat("b", 64), MaximumSeconds: 60, ExpiresAt: now.Add(time.Minute).Unix(),
	}, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: &Store{now: func() time.Time { return now }}},
		HTTPConfig{CI: CIConfig{ManagedService: s}})
	request := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap", body)
	request.Header.Set("Authorization", "Bearer "+token)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		response <- w
	}()
	ciServiceAwait(t, body.reading)
	fail <- struct{}{}
	w := ciServiceAwait(t, response)
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "JitConfig") ||
		strings.Contains(w.Body.String(), "Capability") {
		t.Fatalf("failed service released bootstrap authority: %d %s", w.Code, w.Body.String())
	}
	ciServiceAwait(t, done)
	if core.Err() != nil {
		t.Fatal("request cancellation propagated to the core API")
	}
}

func TestCIManagedServiceHTTPUnavailable(t *testing.T) {
	for _, mode := range []CIManagedMode{CIManagedRun, CIManagedRecover, CIManagedOff} {
		t.Run(string(mode), func(t *testing.T) {
			s := NewCIManagedService(mode, nil)
			stale := ciServiceTestRuntime().CIConfig(CIConfig{ManagedService: s, WebhookSecret: []byte(strings.Repeat("w", 40))})
			h := NewHTTPServer(&Service{}, HTTPConfig{CI: stale})
			for _, path := range []string{"runner-admissions", "runner-bootstrap", "runner-evidence"} {
				body := &ciManagedUnreadBody{}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/ci/"+path, body))
				want := http.StatusServiceUnavailable
				if mode != CIManagedRun {
					want = http.StatusNotFound
				}
				if w.Code != want || body.reads.Load() != 0 {
					t.Fatalf("%s: status=%d reads=%d", path, w.Code, body.reads.Load())
				}
			}
			for _, tc := range []struct {
				path string
				want int
			}{{"/healthz", http.StatusOK}, {"/v1/ci/github/webhook", http.StatusUnauthorized}} {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}")))
				if tc.path == "/healthz" {
					w = httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
				}
				if w.Code != tc.want {
					t.Fatalf("unrelated route %s blocked: %d", tc.path, w.Code)
				}
			}
		})
	}
}

func TestCIManagedServiceEnforcerFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		reason string
		retry  bool
	}{
		{"starting", 200, `{"version":1,"running":false,"githubAppId":42}`, "enforcer_unavailable", true},
		{"wrong app", 200, `{"version":1,"running":true,"githubAppId":99}`, "enforcer_app_mismatch", false},
		{"auth", 401, `{}`, "invalid_configuration", false},
		{"malformed", 200, `{"SECRET":"payload"}`, "invalid_configuration", false},
		{"unavailable", 503, `{}`, "enforcer_unavailable", true},
		{"ready", 200, `{"version":1,"running":true,"githubAppId":42}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client, err := newCIEnforcerClient(server.URL, []byte(strings.Repeat("t", 40)), nil, true)
			if err != nil {
				t.Fatal(err)
			}
			r := &CIManagedRuntime{enforcer: client}
			r.appID.Store(42)
			err = r.checkEnforcer(context.Background())
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var startup *ciManagedStartupError
			if !errors.As(err, &startup) || !errors.Is(err, ErrConflict) ||
				startup.reason != tc.reason || startup.retryable != tc.retry || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("incorrect classification: %v", err)
			}
		})
	}
}

func TestCIManagedServiceDBIsolation(t *testing.T) {
	// This fixture creates and retains a unique schema with an exclusive
	// search_path; it never truncates the shared integration database.
	fixture, _ := superSandboxFixture(t)
	store := fixture.service.Store
	const session = "managed-service-test-session"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := store.pool.Exec(ctx, `UPDATE accounts SET email_verified_at=now() WHERE id='owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,expires_at,auth_version)
		SELECT 'managed-service-session',id,$1,now()+interval '1 hour',auth_version FROM accounts WHERE id='owner'`, secretHash(session)); err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	enforcer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ciEnforcerHealth{Version: 1, Running: ready.Load(), GitHubAppID: 42})
	}))
	defer enforcer.Close()
	profile, err := json.Marshal(ciManagedClaimProfile())
	if err != nil {
		t.Fatal(err)
	}
	compute := newFakeCISchedulerCompute()
	github := CIConfig{AppID: 42, AppSlug: "canter-ci", PrivateKeyPEM: ciTestKey(t), WebhookSecret: []byte(ciTestWebhookSecret)}
	s := NewCIManagedService(CIManagedRun, func(ctx context.Context) (*CIManagedRuntime, error) {
		return newCIManagedRuntime(ctx, store, compute, github, CIManagedRuntimeConfig{
			Mode: CIManagedRun, ProfileJSON: profile, PublicURL: "https://ci.example.test",
			SigningKey: []byte(strings.Repeat("k", 32)), EnforcerToken: []byte(strings.Repeat("t", 40)), EnforcerURL: enforcer.URL,
		}, true)
	})
	waiting, retry := make(chan struct{}, 1), make(chan struct{}, 1)
	s.wait = func(ctx context.Context, _ time.Duration) bool {
		waiting <- struct{}{}
		select {
		case <-retry:
			return true
		case <-ctx.Done():
			return false
		}
	}
	activated, fail := make(chan struct{}, 1), make(chan struct{}, 1)
	s.run = func(ctx context.Context, r *CIManagedRuntime, publish func()) error {
		child, stop := context.WithCancel(ctx)
		defer stop()
		watcher := make(chan struct{})
		go func() {
			defer close(watcher)
			select {
			case <-child.Done():
			case <-fail:
				stop()
			}
		}()
		err := r.run(child, func() {
			publish()
			activated <- struct{}{}
		})
		stop()
		<-watcher
		return err
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		ciServiceAwait(t, done)
	})
	httpConfig := github
	httpConfig.ManagedService = s
	h := NewHTTPServer(&Service{Store: store}, HTTPConfig{CI: httpConfig})
	checkCore := func() {
		t.Helper()
		for _, path := range []string{"/readyz", "/v1/me", "/v1/workspaces/ws/ci"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("core route %s failed during managed outage: %d %s", path, w.Code, w.Body.String())
			}
			if path == "/v1/workspaces/ws/ci" {
				var overview CIOverview
				if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil || overview.Pool.Ready {
					t.Fatalf("unfunded/degraded CI advertised as ready: %s", w.Body.String())
				}
			}
		}
	}
	ciServiceAwait(t, waiting)
	checkCore()
	ready.Store(true)
	retry <- struct{}{}
	ciServiceAwait(t, activated)
	checkCore()
	if pool := s.pool(ctx); pool.Ready || pool.Status != "unfunded" {
		t.Fatalf("activation bypassed the funding gate: %+v", pool)
	}
	fail <- struct{}{}
	ciServiceAwait(t, done)
	checkCore()
	if ctx.Err() != nil || compute.createCount() != 0 {
		t.Fatalf("core canceled or compute mutated: ctx=%v creates=%d", ctx.Err(), compute.createCount())
	}
	store.Close()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("database failure was hidden by fault isolation: %d", w.Code)
	}
}
