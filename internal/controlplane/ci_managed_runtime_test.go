package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ciRuntimeFixture wires a real store fixture, a real enforcer engine over
// HTTP, and a fake compute provider — the same proof flow the private
// scheduler fixtures use, but exposing the enforcer URL/token so the public
// runtime factory can build its own client.
type ciRuntimeFixture struct {
	t             *testing.T
	s             *Store
	workspace     string
	compute       *fakeCISchedulerCompute
	engine        *ciEnforcerEngine
	client        *ciEnforcerClient
	enforcerURL   string
	enforcerToken []byte
	github        *ciGitHubClient
	profile       ciManagedRunnerProfile
	profileJSON   []byte
	digest        string
	ciConfig      CIConfig
	ctx           context.Context
	cancel        context.CancelFunc
	engineCtx     context.Context
	engineCancel  context.CancelFunc
}

func newCIRuntimeFixture(t *testing.T, credit int64, jobs int, grant bool, adjust func(*ciProvisioningBudgetPolicy)) *ciRuntimeFixture {
	t.Helper()
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	profile := ciManagedClaimProfile()
	digest, err := profile.digest()
	if err != nil {
		t.Fatal(err)
	}
	policy := ciBudgetPolicy(s.now())
	policy.ProfileDigest = digest
	if adjust != nil {
		adjust(&policy)
	}
	if grant {
		ciBudgetGrant(t, s, "credit_runtime", credit, policy)
	}
	for n := 1; n <= jobs; n++ {
		ciManagedQueuedJob(t, s, workspace, "linux-x64-2", n)
	}
	fx := &ciRuntimeFixture{t: t, s: s, workspace: workspace, profile: profile, digest: digest}
	// Store and enforcer clocks advance in real time from the fixture base,
	// matching attachCISchedulerRuntime.
	base := s.now()
	started := time.Now()
	now := func() time.Time { return base.Add(time.Since(started)) }
	s.now = now
	fx.compute = newFakeCISchedulerCompute()
	fx.github = nil
	var ghFixture *gitHubFixture
	ghFixture, _, fx.github = newGitHubFixtureRepo(t, 123, 789, "financial-tests", "repo")
	_ = ghFixture
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fx.engine, err = newCIEnforcerEngine(dir, fx.compute, fx.github,
		func() time.Time { return now().UTC() }, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fx.engine.close)
	fx.enforcerToken = []byte(strings.Repeat("t", 40))
	srv := &CIEnforcerServer{engine: fx.engine, token: fx.enforcerToken}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	fx.enforcerURL = httpSrv.URL
	fx.client, err = newCIEnforcerClient(fx.enforcerURL, fx.enforcerToken, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(profile.normalized())
	if err != nil {
		t.Fatal(err)
	}
	fx.profileJSON = raw
	fx.ciConfig = CIConfig{
		AppID:         42,
		AppSlug:       "canter-ci",
		PrivateKeyPEM: ciTestKey(t),
		WebhookSecret: []byte(ciTestWebhookSecret),
		APIBaseURL:    fx.github.base,
	}
	fx.ctx, fx.cancel = context.WithCancel(context.Background())
	t.Cleanup(fx.cancel)
	fx.engineCtx, fx.engineCancel = context.WithCancel(context.Background())
	t.Cleanup(fx.engineCancel)
	go fx.engine.Run(fx.engineCtx)
	fx.waitHealth(t, true)
	return fx
}

func (fx *ciRuntimeFixture) waitHealth(t *testing.T, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h, err := fx.client.health(fx.ctx)
		if err == nil && h.Running == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("enforcer health did not reach running=%v", want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (fx *ciRuntimeFixture) runtimeConfig(mode CIManagedMode) CIManagedRuntimeConfig {
	return CIManagedRuntimeConfig{
		Mode:          mode,
		ProfileJSON:   fx.profileJSON,
		PublicURL:     "https://ci.example.test",
		SigningKey:    []byte(strings.Repeat("k", 32)),
		EnforcerURL:   fx.enforcerURL,
		EnforcerToken: append([]byte(nil), fx.enforcerToken...),
	}
}

func (fx *ciRuntimeFixture) newRuntime(t *testing.T, cfg CIManagedRuntimeConfig, github CIConfig) *CIManagedRuntime {
	t.Helper()
	rt, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, github, cfg, true)
	if err != nil {
		t.Fatalf("newCIManagedRuntime: %v", err)
	}
	if rt == nil {
		t.Fatal("expected runtime")
	}
	return rt
}

// ciRuntimeStartRun launches rt.Run under a cancellable context and registers
// a cleanup that always cancels and joins, so a failed assertion can never
// leak the worker goroutine into the next fixture. The producer closes done
// after sending, so a test that already consumed the result still lets the
// cleanup read return immediately.
func ciRuntimeStartRun(t *testing.T, rt *CIManagedRuntime) (context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- rt.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runtime did not join after cancel")
		}
	})
	return ctx, cancel, done
}

func ciRuntimeProfileVariant(t *testing.T, fx *ciRuntimeFixture, mutate func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(fx.profileJSON, &doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCIManagedRuntimeOffAndValidation(t *testing.T) {
	t.Run("off and empty return nil before validation", func(t *testing.T) {
		for _, cfg := range []CIManagedRuntimeConfig{
			{},
			{Mode: CIManagedOff, EnforcerURL: ":::", SigningKey: []byte("x"), ProfileJSON: []byte("{")},
		} {
			rt, err := newCIManagedRuntime(nil, nil, nil, CIConfig{}, cfg, true)
			if err != nil || rt != nil {
				t.Fatalf("mode %q: rt=%v err=%v", cfg.Mode, rt, err)
			}
		}
	})

	t.Run("unknown mode sanitized", func(t *testing.T) {
		_, err := newCIManagedRuntime(context.Background(), nil, nil, CIConfig{},
			CIManagedRuntimeConfig{Mode: "bogus-secret-mode"}, true)
		if err == nil || strings.Contains(err.Error(), "bogus-secret-mode") {
			t.Fatalf("expected sanitized error, got %v", err)
		}
	})

	fx := newCIRuntimeFixture(t, 0, 0, false, nil)
	good := fx.runtimeConfig(CIManagedRun)

	t.Run("baseline run constructs", func(t *testing.T) {
		rt := fx.newRuntime(t, good, fx.ciConfig)
		if rt.ProfileDigest() != fx.digest {
			t.Fatalf("digest %q != %q", rt.ProfileDigest(), fx.digest)
		}
	})

	cases := []struct {
		name   string
		mutate func(*CIManagedRuntimeConfig)
	}{
		{"nil store", func(c *CIManagedRuntimeConfig) {}},
		{"short key", func(c *CIManagedRuntimeConfig) { c.SigningKey = []byte(strings.Repeat("k", 31)) }},
		{"long key", func(c *CIManagedRuntimeConfig) { c.SigningKey = []byte(strings.Repeat("k", 33)) }},
		{"key equals enforcer token", func(c *CIManagedRuntimeConfig) {
			c.EnforcerToken = append([]byte(nil), c.SigningKey...)
		}},
		{"empty profile", func(c *CIManagedRuntimeConfig) { c.ProfileJSON = nil }},
		{"oversize profile", func(c *CIManagedRuntimeConfig) { c.ProfileJSON = make([]byte, 32769) }},
		{"invalid json", func(c *CIManagedRuntimeConfig) { c.ProfileJSON = []byte("{") }},
		{"duplicate keys", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = []byte(`{"version":1,"version":1}`)
		}},
		{"unknown top field", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = ciRuntimeProfileVariant(t, fx, func(d map[string]any) { d["extra"] = 1 })
		}},
		{"case alias field", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = ciRuntimeProfileVariant(t, fx, func(d map[string]any) {
				delete(d, "version")
				d["Version"] = 1
			})
		}},
		{"case alias machine field", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = ciRuntimeProfileVariant(t, fx, func(d map[string]any) {
				machines := d["machines"].([]any)
				m0 := machines[0].(map[string]any)
				delete(m0, "vcpus")
				m0["Vcpus"] = 2
			})
		}},
		{"null field", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = ciRuntimeProfileVariant(t, fx, func(d map[string]any) { d["region"] = nil })
		}},
		{"trailing data", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = append(append([]byte(nil), fx.profileJSON...), []byte(" x")...)
		}},
		{"invalid utf8", func(c *CIManagedRuntimeConfig) { c.ProfileJSON = []byte{'{', 0xff, '}'} }},
		{"stale bundle sha", func(c *CIManagedRuntimeConfig) {
			c.ProfileJSON = ciRuntimeProfileVariant(t, fx, func(d map[string]any) {
				d["guestBundleSha256"] = strings.Repeat("b", 64)
			})
		}},
		{"http public url", func(c *CIManagedRuntimeConfig) { c.PublicURL = "http://ci.example.test" }},
		{"origin mismatch", func(c *CIManagedRuntimeConfig) { c.PublicURL = "https://other.example.test" }},
		{"empty public url", func(c *CIManagedRuntimeConfig) { c.PublicURL = "" }},
		{"bad enforcer url", func(c *CIManagedRuntimeConfig) { c.EnforcerURL = "not a url" }},
		{"empty enforcer url", func(c *CIManagedRuntimeConfig) { c.EnforcerURL = "" }},
		{"short token", func(c *CIManagedRuntimeConfig) { c.EnforcerToken = []byte(strings.Repeat("t", 31)) }},
		{"missing github config", func(c *CIManagedRuntimeConfig) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			cfg.ProfileJSON = append([]byte(nil), good.ProfileJSON...)
			cfg.SigningKey = append([]byte(nil), good.SigningKey...)
			cfg.EnforcerToken = append([]byte(nil), good.EnforcerToken...)
			tc.mutate(&cfg)
			github := fx.ciConfig
			if tc.name == "nil store" {
				if _, err := newCIManagedRuntime(fx.ctx, nil, fx.compute, github, cfg, true); err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if tc.name == "missing github config" {
				github = CIConfig{}
			}
			before := fx.compute.createCount()
			if _, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, github, cfg, true); err == nil {
				t.Fatal("expected error")
			}
			if got := fx.compute.createCount(); got != before {
				t.Fatalf("provider writes before validation: %d creates", got-before)
			}
		})
	}

	t.Run("stopped context rejected", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := newCIManagedRuntime(ctx, fx.s, fx.compute, fx.ciConfig, good, true); err == nil {
			t.Fatal("expected stopped-context rejection")
		}
	})

	t.Run("public factory rejects typed-nil provider", func(t *testing.T) {
		for _, mode := range []CIManagedMode{CIManagedRun, CIManagedRecover} {
			cfg := fx.runtimeConfig(mode)
			before := fx.compute.createCount()
			if _, err := NewCIManagedRuntime(fx.ctx, fx.s, nil, fx.ciConfig, cfg); !errors.Is(err, ErrConflict) {
				t.Fatalf("mode %s: expected ErrConflict, got %v", mode, err)
			}
			if got := fx.compute.createCount(); got != before {
				t.Fatalf("provider writes before nil guard: %d", got-before)
			}
		}
		// Off still returns nil before any dependency is examined.
		rt, err := NewCIManagedRuntime(nil, nil, nil, CIConfig{}, CIManagedRuntimeConfig{})
		if err != nil || rt != nil {
			t.Fatalf("off with nil provider: rt=%v err=%v", rt, err)
		}
	})

	t.Run("wrong app id fails before provider writes", func(t *testing.T) {
		wrong := fx.ciConfig
		wrong.AppID = 43
		before := fx.compute.createCount()
		if _, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, wrong, good, true); err == nil {
			t.Fatal("expected app id mismatch rejection")
		}
		if got := fx.compute.createCount(); got != before {
			t.Fatalf("provider writes before health probe: %d creates", got-before)
		}
	})
}

func TestCIManagedRuntimeUnfunded(t *testing.T) {
	fx := newCIRuntimeFixture(t, 0, 1, false, nil)
	rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
	ctx, cancel, done := ciRuntimeStartRun(t, rt)
	deadline := time.Now().Add(10 * time.Second)
	for rt.Pool(ctx).Status != "unfunded" {
		if time.Now().After(deadline) {
			t.Fatalf("pool never reported unfunded: %+v", rt.Pool(ctx))
		}
		time.Sleep(50 * time.Millisecond)
	}
	pool := rt.Pool(ctx)
	if pool.Ready || pool.Status != "unfunded" {
		t.Fatalf("unexpected pool: %+v", pool)
	}
	if got := fx.compute.createCount(); got != 0 {
		t.Fatalf("unfunded run created %d VMs", got)
	}
	var reservations, budgets int
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL),
		        (SELECT count(*) FROM ci_provisioning_budget)`).Scan(&reservations, &budgets); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 || budgets != 0 {
		t.Fatalf("unfunded run wrote reservations=%d budgets=%d", reservations, budgets)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runtime did not join workers after cancel")
	}
	if pool := rt.Pool(context.Background()); pool.Status != "not_running" {
		t.Fatalf("pool after stop: %+v", pool)
	}
}

func TestCIManagedRuntimeRecover(t *testing.T) {
	fx := newCIRuntimeFixture(t, 100000, 2, true, nil)
	// Durable claim + stop intent before recovery starts: runner, plan,
	// funding reservation, and lifecycle row exist with zero provider calls.
	claim, err := fx.s.claimCIManagedRunner(context.Background(), fx.profile)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := fx.s.requestCIRunnerStop(context.Background(), claim.Workspace, claim.Runner, "cancelled", nil); err != nil {
		t.Fatalf("stop intent: %v", err)
	}
	// Recovery requires only mode, enforcer URL, and token.
	cfg := CIManagedRuntimeConfig{
		Mode:          CIManagedRecover,
		EnforcerURL:   fx.enforcerURL,
		EnforcerToken: append([]byte(nil), fx.enforcerToken...),
	}
	rt, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, CIConfig{}, cfg, true)
	if err != nil {
		t.Fatalf("recover runtime: %v", err)
	}
	if rt == nil || rt.ProfileDigest() != "" {
		t.Fatalf("unexpected recover runtime: %+v", rt)
	}
	base := CIConfig{
		AppID: 1, AppSlug: "base", PrivateKeyPEM: []byte("pem"),
		WebhookSecret: []byte("secret"), APIBaseURL: "https://api.base.test",
		Admission: &CIAdmissionConfig{SigningKey: []byte("stale")},
		Bootstrap: &CIBootstrapConfig{ProfileDigest: "stale"},
	}
	out := rt.CIConfig(base)
	if out.Managed != rt || out.Admission != nil || out.Bootstrap != nil {
		t.Fatalf("recover CIConfig: %+v", out)
	}
	if out.AppID != 1 || out.AppSlug != "base" || out.APIBaseURL != "https://api.base.test" {
		t.Fatal("recover CIConfig did not preserve base fields")
	}
	// Before Run the pool must report not running, never recovering.
	if pool := rt.Pool(context.Background()); pool.Status != "not_running" || pool.Ready ||
		pool.Message != "Managed CI workers are not running." {
		t.Fatalf("recover pool before run: %+v", pool)
	}
	_, cancel, done := ciRuntimeStartRun(t, rt)
	// Positive proof reconciliation actually runs: the cancelled runner's
	// lifecycle reaches verified cleanup even though no VM or lease ever
	// existed, while funded queued demand remains unclaimed.
	var snap ciRunnerLifecycleSnapshot
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		snap, err = fx.s.loadCIRunnerLifecycle(context.Background(), claim.Workspace, claim.Runner)
		if err == nil && snap.CleanupVerifiedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup never verified: snap=%+v err=%v", snap, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snap.StoppedVerifiedAt == nil || snap.Phase != "deleted" || !snap.CreateSettled {
		t.Fatalf("cleanup proofs incomplete: %+v", snap)
	}
	if pool := rt.Pool(context.Background()); pool.Status != "recovering" || pool.Ready {
		t.Fatalf("recover pool while running: %+v", pool)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recover run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("recover runtime did not join")
	}
	if got := fx.compute.createCount(); got != 0 {
		t.Fatalf("recover created %d VMs despite funded queued demand", got)
	}
	if pool := rt.Pool(context.Background()); pool.Status != "not_running" || pool.Ready {
		t.Fatalf("recover pool after stop: %+v", pool)
	}
}

// TestCIManagedRuntimeRecoverSingleConn repeats the claim-before-lease
// recovery on a MaxConns=1 store: the pre-session unstarted setter must
// finish without a second pool slot or the worker's session advisory lock.
func TestCIManagedRuntimeRecoverSingleConn(t *testing.T) {
	fx := newCIRuntimeFixture(t, 100000, 2, true, nil)
	old := fx.s.pool
	cfg := *old.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.s.pool = pool
	t.Cleanup(func() {
		fx.s.pool = old
		pool.Close()
	})
	claim, err := fx.s.claimCIManagedRunner(context.Background(), fx.profile)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := fx.s.requestCIRunnerStop(context.Background(), claim.Workspace, claim.Runner, "cancelled", nil); err != nil {
		t.Fatalf("stop intent: %v", err)
	}
	cfg2 := CIManagedRuntimeConfig{
		Mode:          CIManagedRecover,
		EnforcerURL:   fx.enforcerURL,
		EnforcerToken: append([]byte(nil), fx.enforcerToken...),
	}
	rt, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, CIConfig{}, cfg2, true)
	if err != nil {
		t.Fatalf("recover runtime: %v", err)
	}
	ctx, cancel, done := ciRuntimeStartRun(t, rt)
	var snap ciRunnerLifecycleSnapshot
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		snap, err = fx.s.loadCIRunnerLifecycle(context.Background(), claim.Workspace, claim.Runner)
		if err == nil && snap.CleanupVerifiedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup never verified under MaxConns=1: snap=%+v err=%v", snap, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snap.StoppedVerifiedAt == nil || snap.Phase != "deleted" || !snap.CreateSettled {
		t.Fatalf("cleanup proofs incomplete: %+v", snap)
	}
	// Funding capacity is released only after verified cleanup; the committed
	// ceiling is retained, never refunded.
	var authorizedBefore, committedBefore int64
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT authorized_micros,committed_micros FROM ci_provisioning_budget WHERE singleton`).
		Scan(&authorizedBefore, &committedBefore); err != nil {
		t.Fatal(err)
	}
	if got := ciSchedulerCount(t, fx.s,
		`SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`); got != 1 {
		t.Fatalf("expected one active reservation before release, got %d", got)
	}
	if err := fx.s.releaseCIProvisioningCapacity(ctx, claim.Workspace, claim.Runner); err != nil {
		t.Fatalf("release capacity: %v", err)
	}
	var authorizedAfter, committedAfter int64
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT authorized_micros,committed_micros FROM ci_provisioning_budget WHERE singleton`).
		Scan(&authorizedAfter, &committedAfter); err != nil {
		t.Fatal(err)
	}
	if authorizedAfter != authorizedBefore || committedAfter != committedBefore {
		t.Fatalf("capacity release refunded the committed ceiling: before=(%d,%d) after=(%d,%d)",
			authorizedBefore, committedBefore, authorizedAfter, committedAfter)
	}
	if got := ciSchedulerCount(t, fx.s,
		`SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`); got != 0 {
		t.Fatalf("reservations still active after release: %d", got)
	}
	var released *time.Time
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT released_at FROM ci_provisioning_reservations WHERE workspace_id=$1 AND runner_id=$2`,
		claim.Workspace, claim.Runner).Scan(&released); err != nil || released == nil {
		t.Fatalf("reservation not released: released=%v err=%v", released, err)
	}
	if got := fx.compute.createCount(); got != 0 {
		t.Fatalf("recover created %d VMs despite funded queued demand", got)
	}
	// Repeated reconciliation after proof is idempotent: no VM work, no new
	// authorization, no double capacity release.
	if err := rt.lifecycle.Reconcile(context.Background(), claim.Workspace, claim.Runner); err != nil {
		t.Fatalf("post-proof reconcile: %v", err)
	}
	if err := fx.s.releaseCIProvisioningCapacity(context.Background(), claim.Workspace, claim.Runner); err != nil {
		t.Fatalf("repeat capacity release: %v", err)
	}
	var authorizedFinal, committedFinal int64
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT authorized_micros,committed_micros FROM ci_provisioning_budget WHERE singleton`).
		Scan(&authorizedFinal, &committedFinal); err != nil {
		t.Fatal(err)
	}
	if authorizedFinal != authorizedAfter || committedFinal != committedAfter ||
		ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`) != 0 {
		t.Fatal("repeated reconcile mutated funding state")
	}
	if got := fx.compute.createCount(); got != 0 {
		t.Fatalf("post-proof reconcile created %d VMs", got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recover run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("recover runtime did not join")
	}
	if pool := rt.Pool(context.Background()); pool.Status != "not_running" || pool.Ready {
		t.Fatalf("recover pool after stop: %+v", pool)
	}
}

// TestCIManagedRuntimeRecoverEnforcerRetry proves a failing unstarted-stop
// RPC withholds every proof and the same worker completes once the trusted
// enforcer client recovers — with zero provider writes throughout.
func TestCIManagedRuntimeRecoverEnforcerRetry(t *testing.T) {
	fx := newCIRuntimeFixture(t, 100000, 2, true, nil)
	claim, err := fx.s.claimCIManagedRunner(context.Background(), fx.profile)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := fx.s.requestCIRunnerStop(context.Background(), claim.Workspace, claim.Runner, "cancelled", nil); err != nil {
		t.Fatalf("stop intent: %v", err)
	}
	bad, err := newCIEnforcerClient(fx.enforcerURL, []byte(strings.Repeat("x", 40)), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	worker := &ciRunnerLifecycleWorker{
		Store:             fx.s,
		Backend:           &ciLifecycleProviderBackend{Compute: fx.compute, Enforcer: fx.client},
		UnstartedEnforcer: bad,
	}
	if err := worker.Reconcile(fx.ctx, claim.Workspace, claim.Runner); err == nil {
		t.Fatal("reconcile with a rejected unstarted RPC must surface the error")
	}
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), claim.Workspace, claim.Runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap.CreateSettled || snap.StoppedVerifiedAt != nil || snap.CleanupVerifiedAt != nil {
		t.Fatalf("failed unstarted RPC produced proofs: %+v", snap)
	}
	if got := fx.compute.createCount(); got != 0 {
		t.Fatalf("provider writes during failed recovery: %d", got)
	}
	worker.UnstartedEnforcer = fx.client
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := worker.Reconcile(fx.ctx, claim.Workspace, claim.Runner); err != nil && time.Now().After(deadline) {
			t.Fatalf("reconcile never completed: %v", err)
		}
		snap, err = fx.s.loadCIRunnerLifecycle(context.Background(), claim.Workspace, claim.Runner)
		if err == nil && snap.CleanupVerifiedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup never verified after client recovery: snap=%+v err=%v", snap, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if snap.StoppedVerifiedAt == nil || snap.Phase != "deleted" || !snap.CreateSettled {
		t.Fatalf("cleanup proofs incomplete: %+v", snap)
	}
	if got := fx.compute.createCount(); got != 0 {
		t.Fatalf("recover created %d VMs despite funded queued demand", got)
	}
}

func TestCIManagedRuntimeFundedRun(t *testing.T) {
	fx := newCIRuntimeFixture(t, 1000000, 1, true, nil)
	rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
	ctx, cancel, done := ciRuntimeStartRun(t, rt)
	deadline := time.Now().Add(30 * time.Second)
	for fx.compute.createCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("scheduler never created a runner")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rt.ProfileDigest() != fx.digest {
		t.Fatalf("ProfileDigest %q != %q", rt.ProfileDigest(), fx.digest)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		pool := rt.Pool(ctx)
		if pool.Ready && pool.Status == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool never ready: %+v", pool)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runtime did not join workers")
	}
}

func TestCIManagedRuntimeCIConfig(t *testing.T) {
	fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
	key := []byte(strings.Repeat("z", 32))
	token := append([]byte(nil), fx.enforcerToken...)
	cfg := CIManagedRuntimeConfig{
		Mode:          CIManagedRun,
		ProfileJSON:   fx.profileJSON,
		PublicURL:     "https://ci.example.test",
		SigningKey:    key,
		EnforcerURL:   fx.enforcerURL,
		EnforcerToken: token,
	}
	rt, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, fx.ciConfig, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	// Caller clears its own buffers; owned copies must be unaffected.
	clear(key)
	clear(token)
	cacheStub := &CICacheServer{}
	base := CIConfig{
		AppID: 7, AppSlug: "base-app", PrivateKeyPEM: []byte("pem"),
		WebhookSecret: []byte("secret"), APIBaseURL: "https://api.base.test",
		HTTPClient: &http.Client{Timeout: 3 * time.Second},
		Cache:      cacheStub,
	}
	out := rt.CIConfig(base)
	if out.Managed != rt {
		t.Fatal("CIConfig did not set Managed")
	}
	if out.AppID != 7 || out.AppSlug != "base-app" || out.APIBaseURL != "https://api.base.test" ||
		out.Cache != cacheStub || out.HTTPClient != base.HTTPClient {
		t.Fatal("CIConfig did not preserve base fields")
	}
	if out.Admission == nil || out.Bootstrap == nil {
		t.Fatal("run mode must set Admission and Bootstrap")
	}
	if out.Bootstrap.ProfileDigest != fx.digest {
		t.Fatalf("bootstrap digest %q", out.Bootstrap.ProfileDigest)
	}
	if string(out.Admission.SigningKey) != strings.Repeat("z", 32) {
		t.Fatal("signing key not preserved after caller clear")
	}
	if string(out.Admission.EnforcerToken) != strings.Repeat("t", 40) {
		t.Fatal("enforcer token not preserved after caller clear")
	}
	// Mutating a returned copy must not affect the runtime's owned copies.
	out.Admission.SigningKey[0] = 'X'
	out.Admission.EnforcerToken[0] = 'X'
	out2 := rt.CIConfig(base)
	if out2.Admission.SigningKey[0] != 'z' || out2.Admission.EnforcerToken[0] != 't' {
		t.Fatal("caller mutation reached runtime-owned key material")
	}
}

func TestCIManagedRuntimePool(t *testing.T) {
	poolStatus := func(rt *CIManagedRuntime, ctx context.Context) CIPool { return rt.Pool(ctx) }

	t.Run("not running", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		pool := poolStatus(rt, fx.ctx)
		if pool.Ready || pool.Status != "not_running" || pool.Message != "Managed CI workers are not running." {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("unfunded", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 0, 0, false, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		pool := poolStatus(rt, fx.ctx)
		if pool.Ready || pool.Status != "unfunded" {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("profile mismatch", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, func(p *ciProvisioningBudgetPolicy) {
			p.ProfileDigest = strings.Repeat("c", 64)
		})
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		if pool := poolStatus(rt, fx.ctx); pool.Status != "profile_mismatch" || pool.Ready {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("expired", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		// Authorization requires a still-valid deadline, so expire the stored
		// policy column directly on this fixture's own database.
		if _, err := fx.s.pool.Exec(context.Background(),
			`UPDATE ci_provisioning_budget SET admit_before=$1 WHERE singleton`,
			fx.s.now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if pool := poolStatus(rt, fx.ctx); pool.Status != "funding_expired" || pool.Ready {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 1, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		if pool := poolStatus(rt, fx.ctx); pool.Status != "funding_exhausted" || pool.Ready {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("ready", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		pool := poolStatus(rt, fx.ctx)
		if !pool.Ready || pool.Status != "ready" {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("enforcer unavailable after stop", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		fx.engineCancel()
		fx.waitHealth(t, false)
		pool := poolStatus(rt, fx.ctx)
		if pool.Ready || pool.Status != "enforcer_unavailable" {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("enforcer unavailable on app id change", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		rt.appID.Store(99)
		if pool := poolStatus(rt, fx.ctx); pool.Status != "enforcer_unavailable" || pool.Ready {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("funding db error unavailable", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.running.Store(true)
		defer rt.running.Store(false)
		fx.s.pool.Close()
		pool := poolStatus(rt, fx.ctx)
		if pool.Ready || pool.Status != "unavailable" || pool.Message != "CI infrastructure readiness could not be verified." {
			t.Fatalf("pool: %+v", pool)
		}
	})

	t.Run("at capacity", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 2, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		ctx, cancel, done := ciRuntimeStartRun(t, rt)
		deadline := time.Now().Add(30 * time.Second)
		for ciSchedulerCount(t, fx.s,
			`SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`) < 2 {
			if time.Now().After(deadline) {
				t.Fatal("reservations were never claimed")
			}
			time.Sleep(100 * time.Millisecond)
		}
		pool := rt.Pool(ctx)
		if pool.Ready || pool.Status != "at_capacity" {
			t.Fatalf("pool: %+v", pool)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("runtime did not join")
		}
	})
}

func TestCIManagedRuntimeAdmissionGate(t *testing.T) {
	// Single synchronous tick with fresh cursors, empty inflight map, and a
	// buffered completion channel: no Run loop, no sleeps, no racing
	// dispatch. Funded demand and free slots exist, so a missing or ignored
	// gate would claim and fail these assertions.
	tickOnce := func(t *testing.T, rt *CIManagedRuntime, fx *ciRuntimeFixture) {
		t.Helper()
		var wg sync.WaitGroup
		active := map[string]bool{}
		completions := make(chan ciManagedWorkItem, ciManagedConcurrency)
		cursorWorkspace, cursorRunner := "", ""
		rt.scheduler.tick(fx.ctx, &wg, active, completions, &cursorWorkspace, &cursorRunner, rt.ProfileDigest())
		wg.Wait()
	}
	assertNoClaims := func(t *testing.T, rt *CIManagedRuntime, fx *ciRuntimeFixture) {
		t.Helper()
		if got := ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners`); got != 0 {
			t.Fatalf("runner rows created while gated: %d", got)
		}
		if got := ciSchedulerCount(t, fx.s,
			`SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`); got != 0 {
			t.Fatalf("reservations created while gated: %d", got)
		}
		if got := fx.compute.createCount(); got != 0 {
			t.Fatalf("provider creates while gated: %d", got)
		}
	}

	t.Run("healthy gate allows the claim", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 1000000, 1, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		tickOnce(t, rt, fx)
		if got := ciSchedulerCount(t, fx.s, `SELECT count(*) FROM ci_runners`); got != 1 {
			t.Fatalf("expected exactly one claimed runner, got %d", got)
		}
		if got := ciSchedulerCount(t, fx.s,
			`SELECT count(*) FROM ci_provisioning_reservations WHERE released_at IS NULL`); got != 1 {
			t.Fatalf("expected one reservation, got %d", got)
		}
	})

	t.Run("daemon stop blocks claims", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 1000000, 1, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		fx.engineCancel()
		fx.waitHealth(t, false)
		tickOnce(t, rt, fx)
		assertNoClaims(t, rt, fx)
	})

	t.Run("app id mismatch blocks claims", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 1000000, 1, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		rt.appID.Store(77)
		tickOnce(t, rt, fx)
		assertNoClaims(t, rt, fx)
	})
}

func TestCIManagedRuntimeOverview(t *testing.T) {
	t.Run("default overview stays offline", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		h := ciTestServer(s, CIConfig{})
		view, err := h.ciOverviewView(context.Background(), Principal{}, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if view.Pool.Ready || view.Pool.Status != "not_configured" {
			t.Fatalf("pool: %+v", view.Pool)
		}
		for _, f := range view.Features {
			if f.ID == "runner-pool" {
				if f.Status != "not_configured" || f.Description != "The CI runner backend has not been activated." {
					t.Fatalf("runner-pool feature: %+v", f)
				}
				return
			}
		}
		t.Fatal("runner-pool feature missing")
	})

	t.Run("managed overview reports live pool", func(t *testing.T) {
		fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
		rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
		h := ciTestServer(fx.s, fx.ciConfig)
		h.config.CI.Managed = rt
		ctx, _, _ := ciRuntimeStartRun(t, rt)
		deadline := time.Now().Add(15 * time.Second)
		for {
			view, err := h.ciOverviewView(ctx, Principal{}, fx.workspace)
			if err != nil {
				t.Fatal(err)
			}
			if view.Pool.Ready && view.Pool.Status == "ready" {
				for _, f := range view.Features {
					if f.ID == "runner-pool" {
						if f.Status != "available" {
							t.Fatalf("runner-pool feature status: %+v", f)
						}
						return
					}
				}
				t.Fatal("runner-pool feature missing")
			}
			if time.Now().After(deadline) {
				t.Fatalf("overview pool never ready: %+v", view.Pool)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}

func TestCIManagedRuntimeEnforcerLoopbackHTTP(t *testing.T) {
	fx := newCIRuntimeFixture(t, 100000, 0, true, nil)
	if !strings.HasPrefix(fx.enforcerURL, "http://127.0.0.1:") {
		t.Fatalf("fixture enforcer must be loopback HTTP, got %q", fx.enforcerURL)
	}
	cfg := CIManagedRuntimeConfig{
		Mode:          CIManagedRun,
		ProfileJSON:   fx.profileJSON,
		PublicURL:     "https://ci.example.test",
		SigningKey:    []byte(strings.Repeat("z", 32)),
		EnforcerURL:   fx.enforcerURL,
		EnforcerToken: append([]byte(nil), fx.enforcerToken...),
	}
	if _, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, fx.ciConfig, cfg, false); err == nil {
		t.Fatal("loopback HTTP enforcer must require the explicit opt-in")
	}
	cfg.EnforcerLoopbackHTTP = true
	rt, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, fx.ciConfig, cfg, false)
	if err != nil {
		t.Fatalf("loopback enforcer opt-in: %v", err)
	}
	if rt.allowLoopbackHTTP {
		t.Fatal("enforcer opt-in must not allow loopback GitHub log downloads")
	}
	if out := rt.CIConfig(CIConfig{}); out.Admission == nil || !out.Admission.AllowLoopbackHTTP {
		t.Fatalf("admission must reach the same loopback enforcer: %+v", out.Admission)
	}
	cfg.EnforcerURL = "http://203.0.113.10:8097"
	if _, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, fx.ciConfig, cfg, false); err == nil {
		t.Fatal("non-loopback HTTP enforcer must stay rejected")
	}
}
