package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ciBootstrapSignPayloadForTest mints a wire token around an arbitrary
// payload under an explicit MAC domain, used to prove domain separation and
// strict-schema rejection of adversarial but correctly signed bodies.
func ciBootstrapSignPayloadForTest(key []byte, domain string, payload []byte) string {
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain + p))
	return "v1." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func ciBootstrapClaimsForTest(now time.Time) ciBootstrapClaims {
	return ciBootstrapClaims{
		Version:            1,
		Workspace:          "w-cap",
		Runner:             "r-cap",
		OperationID:        "op-bootstrap-cap",
		BootstrapID:        strings.Repeat("a", 64),
		GitHubRepositoryID: 789,
		PlanDigest:         strings.Repeat("b", 64),
		MaximumSeconds:     3600,
		ExpiresAt:          now.Unix() + 600,
	}
}

func TestCIBootstrapCapability(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	now := time.Now().UTC().Truncate(time.Second)
	setup := now.Add(700 * time.Second)
	claims := ciBootstrapClaimsForTest(now)

	t.Run("roundtrip and deterministic remint", func(t *testing.T) {
		tok, err := mintCIBootstrapCapability(key, claims, now, setup)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !strings.HasPrefix(tok, "v1.") || len(tok) > ciBootstrapCapabilityMaxBytes {
			t.Fatalf("wire shape: %q", tok)
		}
		got, err := parseCIBootstrapCapability(key, tok, now)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got != claims {
			t.Fatalf("claims drift: %+v", got)
		}
		again, err := mintCIBootstrapCapability(key, claims, now, setup)
		if err != nil || again != tok {
			t.Fatalf("remint not deterministic: %v", err)
		}
		// The input claims value is never mutated by mint or parse.
		if claims.ExpiresAt != now.Unix()+600 {
			t.Fatalf("claims mutated: %+v", claims)
		}
	})

	t.Run("key length and token bounds", func(t *testing.T) {
		if _, err := mintCIBootstrapCapability(bytes.Repeat([]byte("k"), 31), claims, now, setup); err == nil {
			t.Fatal("short key minted")
		}
		if _, err := parseCIBootstrapCapability(bytes.Repeat([]byte("k"), 31), "v1.x.y", now); err == nil {
			t.Fatal("short key parsed")
		}
		tok, _ := mintCIBootstrapCapability(key, claims, now, setup)
		for _, bad := range []string{"", "v2." + tok[3:], strings.Repeat("a", ciBootstrapCapabilityMaxBytes+1), tok + ".extra"} {
			if _, err := parseCIBootstrapCapability(key, bad, now); err == nil {
				t.Fatalf("accepted %q", bad[:min(20, len(bad))])
			}
		}
	})

	t.Run("mac tamper and canonical wire", func(t *testing.T) {
		tok, _ := mintCIBootstrapCapability(key, claims, now, setup)
		parts := strings.Split(tok, ".")
		origTag := parts[2]
		// Flip the FIRST tag character: every bit is significant, unlike a
		// trailing character whose low bits may be unused padding.
		flip := "A"
		if origTag[0] == 'A' {
			flip = "B"
		}
		if _, err := parseCIBootstrapCapability(key, "v1."+parts[1]+"."+flip+origTag[1:], now); err == nil {
			t.Fatal("tampered MAC accepted")
		}
		// A different VALID claims payload (same future expiry, different
		// workspace) under the ORIGINAL valid tag must fail: the MAC binds
		// the exact payload bytes.
		otherClaims := claims
		otherClaims.Workspace = "other-workspace"
		other, _ := json.Marshal(otherClaims)
		forged := "v1." + base64.RawURLEncoding.EncodeToString(other) + "." + origTag
		if _, err := parseCIBootstrapCapability(key, forged, now); err == nil {
			t.Fatal("payload swap accepted")
		}
		// Canonical base64 is enforced on both segments: flipping the last
		// tag character to a neighboring alphabet index decodes to the same
		// bytes under lenient decoding but must be rejected.
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		rawTag, err := base64.RawURLEncoding.DecodeString(origTag)
		if err != nil || len(rawTag) != sha256.Size {
			t.Fatal("tag decode")
		}
		tagIdx := strings.IndexByte(alphabet, origTag[len(origTag)-1])
		tagAliasIdx := tagIdx + 1
		if tagAliasIdx >= len(alphabet) {
			tagAliasIdx = tagIdx - 1
		}
		tagAlias := origTag[:len(origTag)-1] + string(alphabet[tagAliasIdx])
		if _, err := parseCIBootstrapCapability(key, "v1."+parts[1]+"."+tagAlias, now); err == nil {
			t.Fatal("non-canonical tag trailing bits accepted")
		}
		// CRLF and padding aliases are rejected outright.
		for _, bad := range []string{
			"v1." + parts[1] + "." + origTag + "=",
			"v1." + parts[1] + "\r\n." + origTag,
			"v1." + parts[1] + "." + origTag + "\r\n",
		} {
			if _, err := parseCIBootstrapCapability(key, bad, now); err == nil {
				t.Fatalf("wire alias accepted: %q", bad)
			}
		}
	})

	t.Run("mint expiry bounds never clamp", func(t *testing.T) {
		for name, mutate := range map[string]func(*ciBootstrapClaims){
			"expired":    func(c *ciBootstrapClaims) { c.ExpiresAt = now.Unix() },
			"beyond_900": func(c *ciBootstrapClaims) { c.ExpiresAt = now.Unix() + 901 },
			"past_setup": func(c *ciBootstrapClaims) { c.ExpiresAt = now.Unix() + 750 },
		} {
			c := claims
			mutate(&c)
			if _, err := mintCIBootstrapCapability(key, c, now, setup); err == nil {
				t.Fatalf("%s minted", name)
			}
		}
		// claimsValid rejects out-of-range fields outright.
		for name, mutate := range map[string]func(*ciBootstrapClaims){
			"version":   func(c *ciBootstrapClaims) { c.Version = 2 },
			"max_zero":  func(c *ciBootstrapClaims) { c.MaximumSeconds = 0 },
			"max_huge":  func(c *ciBootstrapClaims) { c.MaximumSeconds = ciMaxRuntimeSeconds + 1 },
			"bad_nonce": func(c *ciBootstrapClaims) { c.BootstrapID = "zz" },
			"bad_pd":    func(c *ciBootstrapClaims) { c.PlanDigest = "short" },
			"neg_repo":  func(c *ciBootstrapClaims) { c.GitHubRepositoryID = 0 },
		} {
			c := claims
			mutate(&c)
			if _, err := mintCIBootstrapCapability(key, c, now, setup); err == nil {
				t.Fatalf("%s minted", name)
			}
		}
	})

	t.Run("signed invalid payloads rejected", func(t *testing.T) {
		valid, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		sign := func(payload string) string {
			return ciBootstrapSignPayloadForTest(key, ciBootstrapMACDomain, []byte(payload))
		}
		// Map-driven mutations keep every other field valid.
		base := map[string]any{}
		if err := json.Unmarshal(valid, &base); err != nil {
			t.Fatal(err)
		}
		mutated := func(name string, fn func(map[string]any)) string {
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			fn(m)
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("%s marshal: %v", name, err)
			}
			return sign(string(raw))
		}
		cases := map[string]string{
			"duplicate_key":    sign(strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1)),
			"null_field":       mutated("null", func(m map[string]any) { m["workspace"] = nil }),
			"missing_key":      mutated("missing", func(m map[string]any) { delete(m, "runner") }),
			"unknown_key":      mutated("unknown", func(m map[string]any) { m["extra"] = 1 }),
			"case_alias":       mutated("alias", func(m map[string]any) { delete(m, "workspace"); m["Workspace"] = "w-cap" }),
			"case_variant_dup": mutated("cdup", func(m map[string]any) { m["Workspace"] = "w-cap" }),
			"string_version":   mutated("sver", func(m map[string]any) { m["version"] = "1" }),
			"float_seconds":    mutated("fsec", func(m map[string]any) { m["maximumSeconds"] = 1.5 }),
			"float_exponent":   mutated("fexp", func(m map[string]any) { m["maximumSeconds"] = json.Number("3.6e3") }),
			"overflow":         mutated("ovf", func(m map[string]any) { m["expiresAt"] = json.Number("99999999999999999999") }),
			"deep_nest":        sign(`{"version":1,"workspace":"w","runner":"r","operationId":"o","bootstrapId":"` + strings.Repeat("a", 64) + `","githubRepositoryId":789,"planDigest":"` + strings.Repeat("b", 64) + `","maximumSeconds":3600,"expiresAt":1,"pad":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":1}}}}}}}}}}}`),
			"trailing_value":   sign(string(valid) + " null"),
			"trailing_object":  sign(string(valid) + " {}"),
			"not_json":         sign("this is not json"),
			"expired_signed":   mutated("exp", func(m map[string]any) { m["expiresAt"] = now.Unix() - 10 }),
			"beyond_900_signed": mutated("b900", func(m map[string]any) {
				m["expiresAt"] = now.Unix() + 950
			}),
		}
		for name, tok := range cases {
			if _, err := parseCIBootstrapCapability(key, tok, now); err == nil {
				t.Fatalf("%s accepted", name)
			}
		}
	})

	t.Run("domain separation both directions", func(t *testing.T) {
		setup := now.Add(700 * time.Second)
		// A schema-valid admission token under the shared key must not parse
		// as a bootstrap capability, and vice versa.
		admClaims := ciAdmissionCapability{
			Version: 1, Workspace: "w-cap", Runner: "r-cap",
			AdmissionID: strings.Repeat("c", 64), GitHubRepositoryID: 789,
			GitHubRunnerID: 42, RunnerName: "canter-ci-abc",
			MaximumSeconds: 3600, ExpiresAt: now.Unix() + 600,
		}
		admTok, err := mintCIAdmissionCapability(key, admClaims, now, setup)
		if err != nil {
			t.Fatalf("admission mint: %v", err)
		}
		if _, err := parseCIBootstrapCapability(key, admTok, now); err == nil {
			t.Fatal("admission token parsed as bootstrap")
		}
		bootTok, err := mintCIBootstrapCapability(key, claims, now, setup)
		if err != nil {
			t.Fatalf("bootstrap mint: %v", err)
		}
		if _, err := parseCIAdmissionCapability(key, bootTok, now); err == nil {
			t.Fatal("bootstrap token parsed as admission")
		}
		// Same bootstrap payload signed under the admission domain fails.
		payload, _ := json.Marshal(claims)
		cross := ciBootstrapSignPayloadForTest(key, ciAdmissionMACDomain, payload)
		if _, err := parseCIBootstrapCapability(key, cross, now); err == nil {
			t.Fatal("admission-domain signature accepted as bootstrap")
		}
		// And the mirror image: a schema-valid ADMISSION payload signed under
		// the BOOTSTRAP domain must not verify as an admission capability.
		admPayload, _ := json.Marshal(admClaims)
		crossAdm := ciBootstrapSignPayloadForTest(key, ciBootstrapMACDomain, admPayload)
		if _, err := parseCIAdmissionCapability(key, crossAdm, now); err == nil {
			t.Fatal("bootstrap-domain signature accepted as admission")
		}
	})
}

// ciBootstrapHTTPFixture composes the real enforcer engine (fake compute,
// parameterized GitHub fixture), the durable provisioning journal, and the
// optional HTTP surface exactly as the settled contract requires.
type ciBootstrapHTTPFixture struct {
	s           *Store
	workspace   string
	runner      string
	plan        ciRunnerLifecyclePlan
	policy      ciProvisioningBudgetPolicy
	scope       ciBootstrapScope
	resources   []ciOwnedResource
	serverID    string
	nonce       string
	fc          *fakeCICompute
	gh          *gitHubFixture
	engine      *ciEnforcerEngine
	daemon      *httptest.Server
	daemonToken []byte
	signingKey  []byte
	source      *httptest.Server
	sourceCalls *int32
	sourceStart time.Time
	actualRID   *int64
	h           *HTTPServer
	token       string
	registered  bool
}

func newCIBootstrapHTTPFixture(t *testing.T, op string, registered bool) *ciBootstrapHTTPFixture {
	t.Helper()
	fx := &ciBootstrapHTTPFixture{actualRID: new(int64), sourceCalls: new(int32)}
	s, workspace, _ := ciFinancialFixture(t)
	fx.s = s
	fx.workspace = workspace
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()

	// Funded and reserved; the grant is ensured BEFORE the global fence.
	runner, plan, policy := ciCreationFunded(t, s, workspace, op, 1, now, 1000)
	fx.runner, fx.plan, fx.policy = runner, plan, policy
	fx.nonce = ciAdmissionNonce("a")
	fx.scope = ciBootstrapEnsure(t, s, workspace, runner, policy, plan, fx.nonce)
	spec := fx.scope.Spec

	// Real enforcer engine over fake compute and the GitHub fixture pinned
	// to the same installation/repository identities the DB scope carries.
	fx.fc = newFakeCICompute()
	clk := &fakeClock{wall: now, mono: now}
	gh, _, ghClient := newGitHubFixtureRepo(t, spec.GitHubInstallationID, spec.GitHubRepositoryID, "financial-tests", "repo")
	fx.gh = gh
	fx.engine, _ = newTestEngineAt(t, fx.fc, ghClient, clk)
	if _, err := fx.engine.putLease(spec.Plan.EnforcerLease, spec); err != nil {
		t.Fatalf("putLease: %v", err)
	}

	if registered {
		fx.completeRegistration(t)
	}

	fx.daemonToken = bytes.Repeat([]byte("d"), 40)
	dsrv := &CIEnforcerServer{engine: fx.engine, token: fx.daemonToken}
	dsrv.handler = dsrv.routes()
	fx.daemon = httptest.NewServer(dsrv)
	t.Cleanup(fx.daemon.Close)

	fx.signingKey = bytes.Repeat([]byte("s"), 32)

	// Source-proof side: installation token, repository, and one assigned
	// in-progress job carrying the runner identity populated after bootstrap.
	startedAt := now.Add(-30 * time.Second)
	fx.sourceStart = startedAt
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/123/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(fx.sourceCalls, 1)
		json.NewEncoder(w).Encode(map[string]any{"token": "source-token-0123456789abcdef", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/repositories/789", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(fx.sourceCalls, 1)
		json.NewEncoder(w).Encode(map[string]any{"id": 789, "full_name": "financial-tests/repo", "private": true})
	})
	mux.HandleFunc("/repos/financial-tests/repo/actions/runs/4242/attempts/2/jobs", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(fx.sourceCalls, 1)
		rid := atomic.LoadInt64(fx.actualRID)
		if rid <= 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		job := map[string]any{
			"id": 5001, "run_id": 4242, "run_attempt": 2,
			"runner_id": rid, "runner_name": plan.ServerName,
			"status": "in_progress", "started_at": startedAt.Format(time.RFC3339Nano),
			"completed_at": nil, "conclusion": nil,
			"labels": []string{"canter-2vcpu-ubuntu-2404"},
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []any{job}})
	})
	fx.source = httptest.NewServer(mux)
	t.Cleanup(fx.source.Close)

	fx.h = fx.httpServer(t, nil, nil)
	fx.token = fx.mintToken(t)
	return fx
}

func (fx *ciBootstrapHTTPFixture) httpServer(t *testing.T, enforcerN *int32, mutate http.RoundTripper) *HTTPServer {
	t.Helper()
	tr := http.RoundTripper(http.DefaultTransport)
	if mutate != nil {
		tr = mutate
	} else if enforcerN != nil {
		tr = ciCountingTransport{base: http.DefaultTransport, n: enforcerN}
	}
	return &HTTPServer{
		service: &Service{Store: fx.s},
		config: HTTPConfig{CI: CIConfig{
			AppID: 4321, AppSlug: "canter-ci", PrivateKeyPEM: ciTestKey(t),
			WebhookSecret: bytes.Repeat([]byte("w"), 32), APIBaseURL: fx.sourceURL(),
			Admission: &CIAdmissionConfig{
				SigningKey: fx.signingKey, EnforcerURL: fx.daemon.URL,
				EnforcerToken: fx.daemonToken, AllowLoopbackHTTP: true,
				HTTPClient: &http.Client{Transport: tr},
			},
			Bootstrap: &CIBootstrapConfig{ProfileDigest: fx.policy.ProfileDigest},
		}},
	}
}

func (fx *ciBootstrapHTTPFixture) sourceURL() string {
	if fx.source != nil {
		return fx.source.URL
	}
	return "https://api.github.com"
}

func (fx *ciBootstrapHTTPFixture) mintToken(t *testing.T) string {
	t.Helper()
	claims := fx.scope.claims(fx.workspace, fx.runner)
	tok, err := mintCIBootstrapCapability(fx.signingKey, claims, fx.s.now(), fx.plan.SetupDeadlineAt)
	if err != nil {
		t.Fatalf("mint bootstrap capability: %v", err)
	}
	return tok
}

// completeRegistration drives the remaining trusted steps: global fence,
// full SDK journal, provider server, real enforcer close-creation, and the
// control-plane registering acknowledgement carrying the server-only ack.
func (fx *ciBootstrapHTTPFixture) completeRegistration(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	fenced, err := fx.s.fenceCIRunnerCreate(ctx, fx.workspace, fx.runner)
	if err != nil || !fenced {
		t.Fatalf("global fence: %v %v", fenced, err)
	}
	fx.resources = ciCreationComplete(t, fx.s, fx.workspace, fx.runner, fx.policy, fx.plan)
	fx.serverID = fx.resources[2].ID
	fx.fc.addServer(ciEnforcerScope(fx.scope.Spec), fx.serverID, "ACTIVE")
	st, err := fx.engine.closeCreation(ctx, fx.plan.EnforcerLease, "created", fx.serverID)
	if err != nil {
		t.Fatalf("closeCreation: %v", err)
	}
	if err := fx.s.markCIProvisioningRegistering(ctx, fx.workspace, fx.runner, fx.policy.ProfileDigest, fx.plan, st); err != nil {
		t.Fatalf("markCIProvisioningRegistering real ack: %v", err)
	}
	fx.registered = true
}

func (fx *ciBootstrapHTTPFixture) grant(t *testing.T) ciBootstrapGrant {
	t.Helper()
	return ciBootstrapScopeGet(t, fx.s, fx.workspace, fx.runner, fx.policy, fx.plan, false).Grant
}

// Observe the committed grant without taking the lifecycle advisory lock:
// winning that lock could reject the one-shot request this probe waits for.
func (fx *ciBootstrapHTTPFixture) waitAttempted(t *testing.T, timeout time.Duration) ciBootstrapGrant {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var grant ciBootstrapGrant
		tx, err := fx.s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err == nil {
			grant, err = loadCIBootstrapGrantTx(ctx, tx, fx.plan.OperationID)
			_ = tx.Rollback(ctx)
		}
		cancel()
		if err == nil && grant.AttemptedAt != nil {
			return grant
		}
		if time.Now().After(deadline) {
			t.Fatalf("grant never attempted: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// grantBounded reads the durable grant while a handler may still hold the
// advisory lock: each probe is ctx-bounded and retries on contention so the
// read can never hang the test.
func (fx *ciBootstrapHTTPFixture) grantBounded(t *testing.T, timeout time.Duration) ciBootstrapGrant {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		scope, err := fx.s.ciBootstrapScope(ctx, fx.workspace, fx.runner,
			fx.policy.ProfileDigest, fx.plan, false)
		cancel()
		if err == nil {
			return scope.Grant
		}
		if time.Now().After(deadline) {
			t.Fatalf("bounded grant read: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (fx *ciBootstrapHTTPFixture) post(t *testing.T, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return fx.postCtx(t, context.Background(), token, body, "")
}

func (fx *ciBootstrapHTTPFixture) postCtx(t *testing.T, ctx context.Context, token string, body []byte, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader = bytes.NewReader(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap"+rawQuery, rdr)
	r = r.WithContext(ctx)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-bootstrap"})
	return w
}

func (fx *ciBootstrapHTTPFixture) postAdmission(t *testing.T, capToken string, req ciAdmissionRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+capToken)
	w := httptest.NewRecorder()
	fx.h.ciGitHubPublic(w, r, []string{"runner-admissions"})
	return w
}

func TestCIBootstrapHTTP(t *testing.T) {
	t.Run("route_absent_without_config", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_route", false)
		body := []byte(`{"version":1}`)
		for name, mutate := range map[string]func(*CIConfig){
			"nil_bootstrap": func(c *CIConfig) { c.Bootstrap = nil },
			"bad_profile":   func(c *CIConfig) { c.Bootstrap = &CIBootstrapConfig{ProfileDigest: "not-a-digest"} },
			"nil_admission": func(c *CIConfig) { c.Admission = nil },
		} {
			h := fx.httpServer(t, nil, nil)
			mutate(&h.config.CI)
			r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap", bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+fx.token)
			w := httptest.NewRecorder()
			h.ciGitHubPublic(w, r, []string{"runner-bootstrap"})
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s: status %d", name, w.Code)
			}
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 0 {
			t.Fatalf("jit calls on absent route: %d", got)
		}
	})

	t.Run("auth_before_body_and_strict_request", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_auth", false)
		// Panic-on-read body proves auth runs before any body access.
		pr := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap", panicReader{})
		pr.Header.Set("Authorization", "Bearer bad-token")
		w := httptest.NewRecorder()
		fx.h.ciGitHubPublic(w, pr, []string{"runner-bootstrap"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad token status %d", w.Code)
		}
		// Valid token, malformed bodies: every denial leaves the grant unused.
		valid := []byte(`{"version":1}`)
		denied := map[string][]byte{
			"duplicate":     []byte(`{"version":1,"version":1}`),
			"case_key":      []byte(`{"Version":1}`),
			"null_version":  []byte(`{"version":null}`),
			"wrong_version": []byte(`{"version":2}`),
			"extra_key":     []byte(`{"version":1,"x":0}`),
			"trailing":      append(append([]byte{}, valid...), []byte(" {}")...),
			"empty":         {},
			"not_object":    []byte(`[1]`),
			"oversize":      append(append([]byte(" "), bytes.Repeat([]byte(" "), ciBootstrapRequestMaxBytes+8)...), valid...),
		}
		for name, body := range denied {
			w := fx.post(t, fx.token, body)
			want := http.StatusBadRequest
			if name == "oversize" {
				want = http.StatusRequestEntityTooLarge
			}
			if w.Code != want {
				t.Fatalf("%s: status %d body %s", name, w.Code, w.Body.String())
			}
		}
		// Query string is never allowed, including a bare empty "?".
		for _, q := range []string{"?x=1", "?"} {
			if w := fx.postCtx(t, context.Background(), fx.token, valid, q); w.Code != http.StatusBadRequest {
				t.Fatalf("query %q: status %d", q, w.Code)
			}
		}
		if g := fx.grant(t); g.AttemptedAt != nil || g.DeliveredAt != nil {
			t.Fatalf("denied requests consumed grant: %+v", g)
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 0 {
			t.Fatalf("jit calls after denials: %d", got)
		}
	})

	t.Run("pending_then_registered_retry", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_pending", false)
		w := fx.post(t, fx.token, []byte(`{"version":1}`))
		if w.Code != http.StatusAccepted {
			t.Fatalf("pending status %d body %s", w.Code, w.Body.String())
		}
		var pending map[string]bool
		if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil || !pending["pending"] {
			t.Fatalf("pending body %s", w.Body.String())
		}
		if g := fx.grant(t); g.AttemptedAt != nil {
			t.Fatalf("pending consumed grant: %+v", g)
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 0 {
			t.Fatalf("jit on pending: %d", got)
		}
		// Complete registration, then the SAME capability succeeds once.
		fx.completeRegistration(t)
		w = fx.post(t, fx.token, []byte(`{"version":1}`))
		if w.Code != http.StatusOK {
			t.Fatalf("retry status %d body %s", w.Code, w.Body.String())
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 1 {
			t.Fatalf("jit calls %d", got)
		}
	})

	t.Run("happy_path_exact_dto_and_replay", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_happy", true)
		w := fx.post(t, fx.token, []byte(`{"version":1}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content-type %q", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("cache-control %q", cc)
		}
		var resp ciBootstrapResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("dto decode: %v", err)
		}
		// Exactly eleven fields, no scope/snapshot material.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil || len(raw) != 11 {
			t.Fatalf("field count: %d err=%v", len(raw), err)
		}
		if resp.Version != 1 || resp.OperationID != fx.plan.OperationID ||
			resp.AdmissionID != fx.nonce || resp.RunnerName != fx.plan.ServerName ||
			resp.GitHubRunnerID <= 0 || resp.GitHubRepositoryID != 789 ||
			resp.MaximumSeconds != 3600 ||
			resp.SetupDeadlineUnixMs != fx.plan.SetupDeadlineAt.UnixMilli() ||
			resp.HardDeadlineUnixMs != fx.plan.HardDeadlineAt.UnixMilli() ||
			resp.EncodedJitConfig == "" || resp.AdmissionCapability == "" {
			t.Fatalf("dto: %+v", resp)
		}
		adm, err := parseCIAdmissionCapability(fx.signingKey, resp.AdmissionCapability, fx.s.now())
		if err != nil {
			t.Fatalf("admission capability parse: %v", err)
		}
		if adm.AdmissionID != fx.nonce || adm.GitHubRepositoryID != 789 ||
			adm.GitHubRunnerID != resp.GitHubRunnerID || adm.RunnerName != fx.plan.ServerName ||
			adm.MaximumSeconds != 3600 || adm.Workspace != fx.workspace || adm.Runner != fx.runner {
			t.Fatalf("admission claims: %+v", adm)
		}
		// Durable state: grant used AND delivered, lifecycle idle, no
		// execution clock, customer financials untouched.
		g := fx.grant(t)
		if g.AttemptedAt == nil || g.DeliveredAt == nil {
			t.Fatalf("grant not delivered: %+v", g)
		}
		snap := ciCreationLifecycle(t, fx.s, fx.workspace, fx.runner)
		if snap.Phase != "idle" || !snap.CreateSettled || snap.ExecutionStartedAt != nil {
			t.Fatalf("lifecycle: %+v", snap)
		}
		ciBudgetTotalsZero(t, fx.s, fx.workspace)
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 1 {
			t.Fatalf("jit calls %d", got)
		}
		// No credential material leaks into either body.
		for _, secret := range []string{
			string(fx.signingKey), string(fx.daemonToken), "source-token",
			string(fx.h.config.CI.PrivateKeyPEM)[:32],
		} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("secret material in response")
			}
		}
		// Exact replay conflicts and never issues a second JIT.
		w2 := fx.post(t, fx.token, []byte(`{"version":1}`))
		if w2.Code != http.StatusConflict {
			t.Fatalf("replay status %d", w2.Code)
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 1 {
			t.Fatalf("jit calls after replay %d", got)
		}
	})

	t.Run("concurrent_single_claim", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_race", true)
		// The single-connection pool is installed BEFORE any request so every
		// handler DB call, the mid-flight grant read, and the deferred stop
		// share it: a connection held across the JIT RPC would deadlock the
		// getter probe below.
		old := fx.s.pool
		cfg := *old.Config()
		cfg.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
		if err != nil {
			t.Fatal(err)
		}
		fx.s.pool = pool
		fx.gh.jitBlock = make(chan struct{}) // hold the JIT call open
		var gateOnce sync.Once
		release := func() { gateOnce.Do(func() { close(fx.gh.jitBlock) }) }
		var wg sync.WaitGroup
		defer func() {
			release()
			wg.Wait()
			fx.s.pool = old
			pool.Close()
		}()
		codes := make(chan int, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := fx.post(t, fx.token, []byte(`{"version":1}`))
				codes <- w.Code
			}()
		}
		// The winning claim commits while JIT is still blocked; the grant
		// getter on the SAME MaxConns=1 pool proves no DB connection is held
		// across the enforcer RPC.
		g := fx.waitAttempted(t, 5*time.Second)
		// The JIT RPC must be observably in-flight (blocked inside the fake
		// GitHub handler) before the mid-flight getter proof.
		jitDeadline := time.Now().Add(5 * time.Second)
		for atomic.LoadInt32(&fx.gh.jitCalls) != 1 {
			if time.Now().After(jitDeadline) {
				t.Fatal("jit never entered")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if g.DeliveredAt != nil {
			t.Fatalf("delivered before JIT returned: %+v", g)
		}
		if got := fx.grantBounded(t, 5*time.Second); got.AttemptedAt == nil || got.DeliveredAt != nil {
			t.Fatalf("mid-flight grant: %+v", got)
		}
		release()
		wg.Wait()
		close(codes)
		var oks, conflicts, pendings int
		for c := range codes {
			switch c {
			case http.StatusOK:
				oks++
			case http.StatusConflict:
				conflicts++
			case http.StatusAccepted:
				pendings++
			default:
				t.Fatalf("unexpected status %d", c)
			}
		}
		if oks != 1 || conflicts+pendings != 1 {
			t.Fatalf("codes ok=%d conflict=%d pending=%d", oks, conflicts, pendings)
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 1 {
			t.Fatalf("jit calls %d", got)
		}
		if g := fx.grant(t); g.DeliveredAt == nil {
			t.Fatal("winner never delivered")
		}
	})

	t.Run("cancel_during_jit", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_canceljit", true)
		// Block the JIT POST itself inside the fake GitHub handler.
		fx.gh.jitBlock = make(chan struct{})
		var gateOnce sync.Once
		release := func() { gateOnce.Do(func() { close(fx.gh.jitBlock) }) }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan *httptest.ResponseRecorder, 1)
		joined := false
		defer func() {
			release()
			cancel()
			if !joined {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
			}
		}()
		go func() {
			done <- fx.postCtx(t, ctx, fx.token, []byte(`{"version":1}`), "")
		}()
		fx.waitAttempted(t, 5*time.Second)
		// jitCalls==1 while the block is still closed proves the RPC entered
		// and is parked inside the fake GitHub handler.
		jitDeadline := time.Now().Add(5 * time.Second)
		for atomic.LoadInt32(&fx.gh.jitCalls) != 1 {
			if time.Now().After(jitDeadline) {
				t.Fatal("jit never entered")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		w := <-done
		joined = true
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "encodedJitConfig") ||
			strings.Contains(w.Body.String(), "admissionCapability") {
			t.Fatalf("cancel leaked capability: %d %s", w.Code, w.Body.String())
		}
		if g := fx.grant(t); g.AttemptedAt == nil || g.DeliveredAt != nil {
			t.Fatalf("grant after cancel: %+v", g)
		}
		fx.expectStopped(t)
		// Release the block only after the handler returned: if the fake
		// GitHub server honors the cancelled request context it will not
		// create a runner at all; poll briefly either way.
		release()
		runnerDeadline := time.Now().Add(2 * time.Second)
		for {
			fx.gh.mu.Lock()
			n := len(fx.gh.runners)
			fx.gh.mu.Unlock()
			if n > 0 || time.Now().After(runnerDeadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		for i := 0; i < 2; i++ {
			fx.engine.reconcilePhysical(context.Background(), fx.plan.EnforcerLease)
			fx.engine.reconcileGitHub(context.Background(), fx.plan.EnforcerLease)
		}
		fx.gh.mu.Lock()
		remaining := len(fx.gh.runners)
		fx.gh.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("github runners remain after cleanup: %d", remaining)
		}
		if _, err := fx.fc.Server(context.Background(), fx.serverID); err == nil {
			t.Fatal("physical server still present after cleanup")
		}
	})

	t.Run("cancel_at_final_status_stops_and_cleans_runner", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_cancel", true)
		// Gate ONLY the final enforcer Status GET: the JIT registration and
		// the durable admission row are already committed when cancel lands.
		gate := &ciBootstrapFinalGETGate{
			base: http.DefaultTransport, entered: make(chan struct{}), release: make(chan struct{}),
			leasePath: "/v1/leases/" + fx.plan.EnforcerLease,
		}
		fx.h = fx.httpServer(t, nil, gate)
		var gateOnce sync.Once
		defer func() { gateOnce.Do(func() { close(gate.release) }) }()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- fx.postCtx(t, ctx, fx.token, []byte(`{"version":1}`), "")
		}()
		fx.waitAttempted(t, 5*time.Second)
		// The JIT POST must have actually entered GitHub (runner created on
		// the fake) before the cancel is issued.
		jitDeadline := time.Now().Add(5 * time.Second)
		for atomic.LoadInt32(&fx.gh.jitCalls) != 1 {
			if time.Now().After(jitDeadline) {
				t.Fatal("jit never called")
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("final status GET never reached")
		}
		cancel()
		w := <-done
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "encodedJitConfig") ||
			strings.Contains(w.Body.String(), "admissionCapability") {
			t.Fatalf("cancel leaked capability: %d %s", w.Code, w.Body.String())
		}
		if g := fx.grant(t); g.AttemptedAt == nil || g.DeliveredAt != nil {
			t.Fatalf("grant after cancel: %+v", g)
		}
		fx.expectStopped(t)
		// The JIT-created runner is real on the fake GitHub: engine cleanup
		// must physically delete the VM and remove the runner registration.
		for i := 0; i < 2; i++ {
			fx.engine.reconcilePhysical(context.Background(), fx.plan.EnforcerLease)
			fx.engine.reconcileGitHub(context.Background(), fx.plan.EnforcerLease)
		}
		fx.gh.mu.Lock()
		remaining := len(fx.gh.runners)
		fx.gh.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("github runners remain after cleanup: %d", remaining)
		}
		if _, err := fx.fc.Server(context.Background(), fx.serverID); err == nil {
			t.Fatal("physical server still present after cleanup")
		}
	})

	t.Run("final_status_then_durable_guard_denies", func(t *testing.T) {
		for _, mode := range []string{"stop", "product_off"} {
			t.Run(mode, func(t *testing.T) {
				fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_finalguard_"+mode, true)
				gate := &ciBootstrapFinalGETGate{
					base: http.DefaultTransport, entered: make(chan struct{}), release: make(chan struct{}),
					leasePath: "/v1/leases/" + fx.plan.EnforcerLease,
				}
				fx.h = fx.httpServer(t, nil, gate)
				var gateOnce sync.Once
				defer func() { gateOnce.Do(func() { close(gate.release) }) }()
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					done <- fx.postCtx(t, context.Background(), fx.token, []byte(`{"version":1}`), "")
				}()
				select {
				case <-gate.entered:
				case <-time.After(8 * time.Second):
					t.Fatal("final status GET never reached")
				}
				// The stale-but-valid active status is released only AFTER a
				// durable guard trips: delivery must still be denied.
				if mode == "stop" {
					if _, err := fx.s.requestCIRunnerStop(context.Background(), fx.workspace, fx.runner, "cancelled", nil); err != nil {
						t.Fatalf("request stop: %v", err)
					}
				} else {
					ciAdmissionProductOff(t, fx.s, fx.workspace)
				}
				gateOnce.Do(func() { close(gate.release) })
				w := <-done
				if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "encodedJitConfig") ||
					strings.Contains(w.Body.String(), "admissionCapability") {
					t.Fatalf("%s: capability leaked: %d %s", mode, w.Code, w.Body.String())
				}
				if w.Code != http.StatusConflict && w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: status %d body %s", mode, w.Code, w.Body.String())
				}
				if g := fx.grant(t); g.AttemptedAt == nil || g.DeliveredAt != nil {
					t.Fatalf("%s: grant: %+v", mode, g)
				}
				fx.expectStopped(t)
			})
		}
	})

	t.Run("final_status_empty_inventory_denied", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_finalempty", true)
		gate := &ciBootstrapFinalGETGate{
			base: http.DefaultTransport, entered: make(chan struct{}), release: make(chan struct{}),
			leasePath: "/v1/leases/" + fx.plan.EnforcerLease,
			corrupt: func(st *ciEnforcerStatus) {
				st.Resources = []ciOwnedResource{}
			},
		}
		fx.h = fx.httpServer(t, nil, gate)
		var gateOnce sync.Once
		defer func() { gateOnce.Do(func() { close(gate.release) }) }()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- fx.postCtx(t, context.Background(), fx.token, []byte(`{"version":1}`), "")
		}()
		select {
		case <-gate.entered:
		case <-time.After(8 * time.Second):
			t.Fatal("final status GET never reached")
		}
		gateOnce.Do(func() { close(gate.release) })
		w := <-done
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "encodedJitConfig") {
			t.Fatalf("empty inventory accepted: %d %s", w.Code, w.Body.String())
		}
		if g := fx.grant(t); g.DeliveredAt != nil {
			t.Fatalf("delivered on empty inventory: %+v", g)
		}
		fx.expectStopped(t)
	})

	t.Run("post_delivery_commit_guard", func(t *testing.T) {
		for _, mode := range []string{"setup_expired", "cancelled"} {
			t.Run(mode, func(t *testing.T) {
				fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_postcommit_"+mode, true)
				// Separate probe pool: the now() closure reads the committed
				// delivery marker on a plain MVCC select while the main pool
				// stays untouched.
				old := fx.s.pool
				cfg := *old.Config()
				clockPool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
				if err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var clockErr error
				origNow := fx.s.now
				requestCtx, reqCancel := context.WithCancel(context.Background())
				fx.s.now = func() time.Time {
					n := origNow()
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					var delivered bool
					err := clockPool.QueryRow(ctx,
						`SELECT delivered_at IS NOT NULL FROM ci_runner_bootstrap_grants WHERE operation_id=$1`,
						fx.plan.OperationID).Scan(&delivered)
					mu.Lock()
					if err != nil {
						clockErr = err
					}
					mu.Unlock()
					if err != nil || !delivered {
						return n
					}
					if mode == "setup_expired" {
						return time.Unix(fx.scope.Grant.ExpiresAt.Unix(), 0).Add(time.Second)
					}
					reqCancel()
					return n
				}
				w := fx.postCtx(t, requestCtx, fx.token, []byte(`{"version":1}`), "")
				fx.s.now = origNow
				clockPool.Close()
				mu.Lock()
				cerr := clockErr
				mu.Unlock()
				if cerr != nil {
					t.Fatalf("clock probe error: %v", cerr)
				}
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: status %d body %s", mode, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "admissionCapability") ||
					strings.Contains(w.Body.String(), "encodedJitConfig") {
					t.Fatalf("%s: capability leaked", mode)
				}
				// The delivery marker STAYS committed; cleanup still ran.
				if g := fx.grant(t); g.AttemptedAt == nil || g.DeliveredAt == nil {
					t.Fatalf("%s: grant: %+v", mode, g)
				}
				fx.expectStopped(t)
			})
		}
	})

	t.Run("status_binding_faults_fail_closed", func(t *testing.T) {
		cases := map[string]func(fx *ciBootstrapHTTPFixture, jit *ciEnforcerJITResponse){
			"repository_drift": func(fx *ciBootstrapHTTPFixture, jit *ciEnforcerJITResponse) {
				jit.Status.GitHubRepositoryID = 999
			},
			"spec_digest_drift": func(fx *ciBootstrapHTTPFixture, jit *ciEnforcerJITResponse) {
				jit.Status.SpecDigest = strings.Repeat("0", 64)
			},
			"deadline_drift": func(fx *ciBootstrapHTTPFixture, jit *ciEnforcerJITResponse) {
				jit.Status.DeadlineUnixMs = jit.Status.DeadlineUnixMs - 1
			},
			"runner_id_drift": func(fx *ciBootstrapHTTPFixture, jit *ciEnforcerJITResponse) {
				jit.Status.GitHubRunnerID = 7777
				jit.GitHubRunnerID = 7777
			},
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_fault_"+name, true)
				mutN := new(int32)
				fx.h = fx.httpServer(t, nil, &ciBootstrapJITMutator{
					base: http.DefaultTransport, n: mutN,
					mutate: func(jit *ciEnforcerJITResponse) { mutate(fx, jit) },
				})
				w := fx.post(t, fx.token, []byte(`{"version":1}`))
				if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusConflict {
					t.Fatalf("%s: status %d body %s", name, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "encodedJitConfig") || strings.Contains(w.Body.String(), "admissionCapability") {
					t.Fatalf("%s: capability material in denial", name)
				}
				if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 1 {
					t.Fatalf("%s: jit calls %d", name, got)
				}
				if got := atomic.LoadInt32(mutN); got != 1 {
					t.Fatalf("%s: mutator hits %d", name, got)
				}
				fx.expectStopped(t)
			})
		}
	})

	t.Run("write_failure_still_stops", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_writefail", true)
		fw := &ciBootstrapFailWriter{}
		r := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap", bytes.NewReader([]byte(`{"version":1}`)))
		r.Header.Set("Authorization", "Bearer "+fx.token)
		fx.h.ciGitHubPublic(fw, r, []string{"runner-bootstrap"})
		if fw.status != http.StatusOK {
			t.Fatalf("write status %d", fw.status)
		}
		// Delivery was committed before the failed write; cleanup still ran.
		if g := fx.grant(t); g.DeliveredAt == nil || g.AttemptedAt == nil {
			t.Fatalf("grant after write failure: %+v", g)
		}
		fx.expectStopped(t)
		w2 := fx.post(t, fx.token, []byte(`{"version":1}`))
		if w2.Code != http.StatusConflict {
			t.Fatalf("replay after write failure: %d", w2.Code)
		}
		if got := atomic.LoadInt32(&fx.gh.jitCalls); got != 1 {
			t.Fatalf("jit calls %d", got)
		}
	})

	t.Run("enforcer_stop_survives_db_stop_failure", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_stopfail", true)
		gate := make(chan struct{})
		entered := make(chan struct{})
		var once sync.Once
		mutN := new(int32)
		fx.h = fx.httpServer(t, nil, &ciBootstrapJITMutator{
			base: http.DefaultTransport, n: mutN, gate: gate,
			mutate: func(jit *ciEnforcerJITResponse) {
				once.Do(func() { close(entered) })
				jit.Status.GitHubRepositoryID = 999
			},
		})
		ctx := context.Background()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- fx.postCtx(t, ctx, fx.token, []byte(`{"version":1}`), "") }()
		// Claim committed and the mutator is inside the register-runner RPC.
		deadline := time.Now().Add(5 * time.Second)
		enteredSeen := false
		for !enteredSeen {
			select {
			case <-entered:
				enteredSeen = true
			default:
			}
			if !enteredSeen {
				if time.Now().After(deadline) {
					t.Fatal("mutator never entered")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		// Hold the lifecycle row so the deferred DB stop times out while the
		// independent enforcer stop (no DB) still lands.
		tx, err := fx.s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var lockedID string
		if err := tx.QueryRow(ctx,
			`SELECT r.id FROM ci_runners r JOIN ci_runner_lifecycles l ON l.workspace_id=r.workspace_id AND l.runner_id=r.id WHERE r.workspace_id=$1 AND r.id=$2 FOR UPDATE`,
			fx.workspace, fx.runner).Scan(&lockedID); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		close(gate)
		<-done
		tx.Rollback(ctx)
		// The enforcer stop must land even though requestCIRunnerStop failed.
		deadline = time.Now().Add(10 * time.Second)
		for {
			st, serr := fx.engine.status(fx.plan.EnforcerLease)
			if serr == nil && st.StopRequested {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("enforcer stop missing after DB stop failure")
			}
			time.Sleep(20 * time.Millisecond)
		}
		snap := ciCreationLifecycle(t, fx.s, fx.workspace, fx.runner)
		if snap.StopRequestedAt != nil {
			t.Fatalf("DB stop unexpectedly committed: %+v", snap.StopRequestedAt)
		}
		if g := fx.grant(t); g.DeliveredAt != nil {
			t.Fatalf("delivered despite failed flow: %+v", g)
		}
	})

	t.Run("maxconns1_happy_path", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_max1", true)
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
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		w := fx.postCtx(t, ctx, fx.token, []byte(`{"version":1}`), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		if g := fx.grant(t); g.DeliveredAt == nil {
			t.Fatal("not delivered under MaxConns=1")
		}
	})

	t.Run("admission_flow_after_bootstrap", func(t *testing.T) {
		fx := newCIBootstrapHTTPFixture(t, "bootstrap_op_admit", true)
		w := fx.post(t, fx.token, []byte(`{"version":1}`))
		if w.Code != http.StatusOK {
			t.Fatalf("bootstrap status %d body %s", w.Code, w.Body.String())
		}
		var resp ciBootstrapResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		atomic.StoreInt64(fx.actualRID, resp.GitHubRunnerID)
		ciAdmissionInstallationStatus(t, fx.s, fx.workspace, "active")
		ciAdmissionRepoEnabled(t, fx.s, fx.workspace, true)
		req := ciAdmissionRequest{
			SchemaVersion: 1, AdmissionID: resp.AdmissionID,
			GitHubRunnerID: resp.GitHubRunnerID, GitHubRepositoryID: 789,
			Repository: "financial-tests/repo", GitHubRunID: 4242, RunAttempt: 2,
			JobKey: "build", RunnerName: fx.plan.ServerName,
		}
		w2 := fx.postAdmission(t, resp.AdmissionCapability, req)
		if w2.Code != http.StatusOK {
			t.Fatalf("admission status %d body %s", w2.Code, w2.Body.String())
		}
		var receipt ciAdmissionReceipt
		if err := json.Unmarshal(w2.Body.Bytes(), &receipt); err != nil {
			t.Fatalf("receipt decode: %v", err)
		}
		if receipt.Decision != "allow" || receipt.ExecutionDeadlineUnixMs <= 0 {
			t.Fatalf("receipt %+v", receipt)
		}
		// Free grant: execution deadline is source start + 3600s.
		wantDeadline := fx.sourceStartedAt().Add(3600 * time.Second).UnixMilli()
		if receipt.ExecutionDeadlineUnixMs != wantDeadline {
			t.Fatalf("deadline %d want %d", receipt.ExecutionDeadlineUnixMs, wantDeadline)
		}
		snap := ciCreationLifecycle(t, fx.s, fx.workspace, fx.runner)
		if snap.ExecutionStartedAt == nil || snap.Phase != "running" {
			t.Fatalf("execution not committed: %+v", snap)
		}
		st, err := fx.engine.status(fx.plan.EnforcerLease)
		if err != nil || st.DeadlineUnixMs != receipt.ExecutionDeadlineUnixMs {
			t.Fatalf("engine approval deadline %d err=%v want %d", st.DeadlineUnixMs, err, receipt.ExecutionDeadlineUnixMs)
		}
		// Admission replay cannot move the committed clock.
		started := *snap.ExecutionStartedAt
		w3 := fx.postAdmission(t, resp.AdmissionCapability, req)
		snap2 := ciCreationLifecycle(t, fx.s, fx.workspace, fx.runner)
		if snap2.ExecutionStartedAt == nil || !snap2.ExecutionStartedAt.Equal(started) {
			t.Fatalf("execution clock moved on replay: %v -> %v (status %d)", started, snap2.ExecutionStartedAt, w3.Code)
		}
	})
}

// expectStopped polls until both the durable stop request and the
// independent enforcer stop are visible.
func (fx *ciBootstrapHTTPFixture) expectStopped(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		snap := ciCreationLifecycle(t, fx.s, fx.workspace, fx.runner)
		st, serr := fx.engine.status(fx.plan.EnforcerLease)
		if snap.StopRequestedAt != nil && serr == nil && st.StopRequested {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stop not propagated: snap=%+v st=%+v", snap, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (fx *ciBootstrapHTTPFixture) sourceStartedAt() time.Time {
	return fx.sourceStart
}

// ciBootstrapJITMutator rewrites the register-runner response before the
// typed client sees it, simulating a compromised or buggy enforcer peer.
type ciBootstrapJITMutator struct {
	base   http.RoundTripper
	n      *int32
	mutate func(*ciEnforcerJITResponse)
	gate   chan struct{}
}

func (m *ciBootstrapJITMutator) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := m.base.RoundTrip(r)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK ||
		!strings.HasSuffix(r.URL.Path, "/register-runner") {
		return resp, err
	}
	raw, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	resp2 := *resp
	resp2.Body = io.NopCloser(bytes.NewReader(raw))
	resp2.ContentLength = int64(len(raw))
	var jit ciEnforcerJITResponse
	if json.Unmarshal(raw, &jit) == nil {
		atomic.AddInt32(m.n, 1)
		m.mutate(&jit)
		if m.gate != nil {
			<-m.gate
		}
		if out, merr := json.Marshal(jit); merr == nil {
			raw = out
			resp2.Body = io.NopCloser(bytes.NewReader(raw))
			resp2.ContentLength = int64(len(raw))
		}
	}
	return &resp2, nil
}

// ciBootstrapFinalGETGate intercepts only the final enforcer Status GET,
// obtains the real active response, signals entry, optionally corrupts it,
// then holds delivery until released or the request is cancelled.
type ciBootstrapFinalGETGate struct {
	base      http.RoundTripper
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	leasePath string
	corrupt   func(*ciEnforcerStatus)
}

func (g *ciBootstrapFinalGETGate) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := g.base.RoundTrip(r)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK ||
		r.Method != http.MethodGet || r.URL.Path != g.leasePath {
		return resp, err
	}
	raw, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	var st ciEnforcerStatus
	if json.Unmarshal(raw, &st) != nil {
		resp2 := *resp
		resp2.Body = io.NopCloser(bytes.NewReader(raw))
		resp2.ContentLength = int64(len(raw))
		return &resp2, nil
	}
	g.once.Do(func() { close(g.entered) })
	if g.corrupt != nil {
		g.corrupt(&st)
		raw, _ = json.Marshal(st)
	}
	select {
	case <-g.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	resp2 := *resp
	resp2.Body = io.NopCloser(bytes.NewReader(raw))
	resp2.ContentLength = int64(len(raw))
	return &resp2, nil
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("body must not be read") }

// ciBootstrapFailWriter fails every Write after recording the status,
// simulating a client that disappears mid-response.
type ciBootstrapFailWriter struct {
	header http.Header
	status int
}

func (w *ciBootstrapFailWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *ciBootstrapFailWriter) WriteHeader(status int) { w.status = status }
func (w *ciBootstrapFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("client gone")
}
