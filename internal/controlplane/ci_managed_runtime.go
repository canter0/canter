package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"unicode/utf8"

	"github.com/canter0/canter/internal/provider/compute"
	"golang.org/x/sync/errgroup"
)

// CIManagedRuntime is the public opt-in managed CI runtime. It owns the
// managed provisioner, scheduler, and lifecycle worker for exactly one
// control-plane process. Mode "off" (or empty) constructs nothing; "recover"
// runs only physical lifecycle reconciliation; "run" adds admission,
// bootstrap, scheduling, and enforcer-gated provisioning within an explicit
// funding envelope. It never authorizes funds, creates leases outside the
// claim path, or spawns the independent enforcer process.
type CIManagedRuntime struct {
	store             *Store
	mode              CIManagedMode
	profileDigest     string
	signingKey        []byte
	enforcer          *ciEnforcerClient
	compute           ciManagedProvisionCompute
	github            *ciGitHubClient
	logs              *ciJobLogCollector
	lifecycle         *ciRunnerLifecycleWorker
	scheduler         *ciManagedScheduler
	appID             atomic.Int64
	httpClient        *http.Client
	allowLoopbackHTTP bool
	// enforcerLoopbackHTTP applies only to enforcer calls, never to GitHub
	// log downloads, which allowLoopbackHTTP covers in tests.
	enforcerLoopbackHTTP bool
	running              atomic.Bool
}

// NewCIManagedRuntime is the production factory: real compute provider,
// production URL policy (no loopback HTTP). A typed-nil provider is rejected
// for recover/run before it can be wrapped into a non-nil interface; off and
// empty modes still return (nil, nil) before any dependency is examined.
func NewCIManagedRuntime(ctx context.Context, store *Store, provider *compute.Client, github CIConfig, cfg CIManagedRuntimeConfig) (*CIManagedRuntime, error) {
	if (cfg.Mode == CIManagedRecover || cfg.Mode == CIManagedRun) && provider == nil {
		return nil, ErrConflict
	}
	return newCIManagedRuntime(ctx, store, provider, github, cfg, false)
}

// newCIManagedRuntime is the private test-seamed factory: compute may be a
// fixture and allowLoopbackHTTP permits loopback enforcer URLs in tests.
func newCIManagedRuntime(ctx context.Context, store *Store, compute ciManagedProvisionCompute, github CIConfig, cfg CIManagedRuntimeConfig, allowLoopbackHTTP bool) (*CIManagedRuntime, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = CIManagedOff
	}
	if mode == CIManagedOff {
		// Off returns before any context, store, compute, URL, key, or
		// profile validation: zero database, network, file, or provider
		// side effects, and stale inactive managed options are ignored.
		return nil, nil
	}
	if mode != CIManagedRecover && mode != CIManagedRun {
		return nil, errors.New("invalid CI managed runtime mode")
	}
	if ctx == nil || ctx.Err() != nil || store == nil || store.pool == nil || compute == nil {
		return nil, ErrConflict
	}
	if cfg.EnforcerURL == "" || len(cfg.EnforcerToken) < 32 {
		return nil, ErrConflict
	}
	enforcerLoopbackHTTP := allowLoopbackHTTP || cfg.EnforcerLoopbackHTTP
	enforcer, err := newCIEnforcerClient(cfg.EnforcerURL, cfg.EnforcerToken, cfg.HTTPClient, enforcerLoopbackHTTP)
	if err != nil {
		return nil, ErrConflict
	}
	r := &CIManagedRuntime{
		store:                store,
		mode:                 mode,
		enforcer:             enforcer,
		compute:              compute,
		httpClient:           cfg.HTTPClient,
		allowLoopbackHTTP:    allowLoopbackHTTP,
		enforcerLoopbackHTTP: enforcerLoopbackHTTP,
	}
	if mode == CIManagedRecover {
		// Physical recovery only: lifecycle reconciliation against the
		// trusted provider backend. GitHub is optional (nil is allowed for
		// physical recovery); no profile, signing key, creator, scheduler,
		// admission, bootstrap, lease, JIT, or funding work happens here.
		gh, err := ciEnforcerGitHubClient(github)
		if err != nil {
			return nil, ErrConflict
		}
		r.github = gh
		r.lifecycle = &ciRunnerLifecycleWorker{
			Store:             store,
			Backend:           &ciLifecycleProviderBackend{Compute: compute, Enforcer: enforcer},
			GitHub:            gh,
			UnstartedEnforcer: enforcer,
		}
		// Log collection is optional in recovery: it exists only when a
		// GitHub client was configured, and it never creates runners.
		if gh != nil {
			r.logs = newCIJobLogCollector(store, gh, r.allowLoopbackHTTP)
		}
		return r, nil
	}
	// Run mode.
	if len(cfg.SigningKey) != 32 || string(cfg.SigningKey) == string(cfg.EnforcerToken) {
		return nil, ErrConflict
	}
	if len(cfg.ProfileJSON) < 1 || len(cfg.ProfileJSON) > 32768 {
		return nil, ErrConflict
	}
	if err := ValidateCIConfig(github); err != nil || github.AppID <= 0 {
		return nil, ErrConflict
	}
	profile, err := ciManagedRuntimeProfile(cfg.ProfileJSON)
	if err != nil {
		return nil, &ciManagedStartupError{reason: "profile_invalid"}
	}
	pu, err := url.Parse(profile.BootstrapURL)
	if err != nil {
		return nil, ErrConflict
	}
	bootstrapOrigin, _, err := ciCacheAudienceForURL(pu.Scheme+"://"+pu.Host, false)
	if err != nil {
		return nil, ErrConflict
	}
	publicOrigin, _, err := ciCacheAudienceForURL(cfg.PublicURL, false)
	if err != nil || bootstrapOrigin != publicOrigin {
		return nil, ErrConflict
	}
	digest, err := profile.digest()
	if err != nil {
		return nil, ErrConflict
	}
	r.profileDigest = digest
	r.appID.Store(github.AppID)
	r.signingKey = append([]byte(nil), cfg.SigningKey...)
	gh, err := (&HTTPServer{config: HTTPConfig{CI: github}}).ciGitHub()
	if err != nil {
		return nil, ErrConflict
	}
	r.github = gh
	r.logs = newCIJobLogCollector(store, gh, r.allowLoopbackHTTP)
	prov, err := newCIManagedProvisioner(store, profile, cfg.SigningKey, enforcer, compute)
	if err != nil {
		return nil, ErrConflict
	}
	sched, err := newCIManagedScheduler(prov, gh)
	if err != nil {
		return nil, ErrConflict
	}
	// Only this runtime's own scheduler is enabled; there is no global
	// setter and no public route.
	sched.enabled = true
	sched.admissionReady = func(ctx context.Context) bool {
		return r.enforcerReady(ctx)
	}
	r.scheduler = sched
	r.lifecycle = sched.lifecycle
	// Read-only authenticated health probe before the factory succeeds:
	// the independent enforcer must be running and bound to the same GitHub
	// App, or no creator/VM path may come up.
	if err := r.checkEnforcer(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

var ciManagedProfileTopKeys = map[string]bool{
	"version": true, "region": true, "imageId": true, "networkId": true,
	"bootstrapUrl": true, "runnerVersion": true, "runnerSha256": true,
	"guestBundleSha256": true, "machines": true,
}

var ciManagedProfileMachineKeys = map[string]bool{
	"sizeId": true, "flavorId": true, "vcpus": true, "memoryMiB": true, "diskGiB": true,
}

// ciManagedRuntimeProfile parses the provisioning profile with the existing
// strict JSON rules (recursive duplicate/null/structure checks, unknown-field
// rejection, EOF) plus exact case-sensitive field names at every level and an
// explicit UTF-8 check, since encoding/json would silently replace invalid
// UTF-8 inside strings.
func ciManagedRuntimeProfile(raw []byte) (ciManagedRunnerProfile, error) {
	if len(raw) < 1 || len(raw) > 32768 || !utf8.Valid(raw) {
		return ciManagedRunnerProfile{}, ErrConflict
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := ciStrictJSONValue(dec); err != nil {
		return ciManagedRunnerProfile{}, ErrConflict
	}
	if _, err := dec.Token(); err != io.EOF {
		return ciManagedRunnerProfile{}, ErrConflict
	}
	if err := ciManagedProfileKeys(raw); err != nil {
		return ciManagedRunnerProfile{}, err
	}
	var p ciManagedRunnerProfile
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return ciManagedRunnerProfile{}, ErrConflict
	}
	if _, err := dec.Token(); err != io.EOF {
		return ciManagedRunnerProfile{}, ErrConflict
	}
	if !p.valid() {
		return ciManagedRunnerProfile{}, ErrConflict
	}
	return p, nil
}

// ciManagedProfileKeys enforces the exact root-authored field names: Go's
// folded-name matching would otherwise let case variants like "Version"
// populate tagged fields, which the duplicate-key pass cannot see.
func ciManagedProfileKeys(raw []byte) error {
	keys, err := ciEnforcerObjectKeys(raw)
	if err != nil {
		return ErrConflict
	}
	for k := range keys {
		if !ciManagedProfileTopKeys[k] {
			return ErrConflict
		}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ErrConflict
	}
	machines, ok := doc["machines"]
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(machines, &items); err != nil {
		return ErrConflict
	}
	for _, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return ErrConflict
		}
		for k := range fields {
			if !ciManagedProfileMachineKeys[k] {
				return ErrConflict
			}
		}
	}
	return nil
}

func (r *CIManagedRuntime) checkEnforcer(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, ciManagedDBBound)
	defer cancel()
	health, err := r.enforcer.health(hctx)
	if err != nil {
		var httpErr *ciEnforcerHTTPError
		transient := errors.Is(err, errCIEnforcerTransport) ||
			(errors.As(err, &httpErr) && (httpErr.status == http.StatusTooManyRequests ||
				httpErr.status == http.StatusBadGateway || httpErr.status == http.StatusServiceUnavailable ||
				httpErr.status == http.StatusGatewayTimeout))
		if transient {
			return &ciManagedStartupError{reason: "enforcer_unavailable", retryable: true}
		}
		return &ciManagedStartupError{reason: "invalid_configuration"}
	}
	if !health.Running {
		return &ciManagedStartupError{reason: "enforcer_unavailable", retryable: true}
	}
	if health.GitHubAppID != r.appID.Load() {
		return &ciManagedStartupError{reason: "enforcer_app_mismatch"}
	}
	return nil
}

// enforcerReady is the scheduler admission gate: a bounded read-only health
// probe that must show the daemon running and bound to the same App. It goes
// false after an enforcer stop or App ID change, blocking new claims while
// lifecycle reconciliation continues independently.
func (r *CIManagedRuntime) enforcerReady(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, ciManagedDBBound)
	defer cancel()
	health, err := r.enforcer.health(hctx)
	return err == nil && health.Running && health.GitHubAppID == r.appID.Load()
}

// Run drives the owned workers until ctx ends. Off (or a nil runtime)
// returns nil; a stopped context is rejected before any worker starts;
// concurrent calls are rejected via the atomic running flag.
func (r *CIManagedRuntime) Run(ctx context.Context) error {
	return r.run(ctx, nil)
}

func (r *CIManagedRuntime) run(ctx context.Context, started func()) error {
	if r == nil || (r.mode != CIManagedRecover && r.mode != CIManagedRun) {
		return nil
	}
	if ctx == nil {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.running.CompareAndSwap(false, true) {
		return ErrConflict
	}
	defer r.running.Store(false)
	if r.mode == CIManagedRecover && r.lifecycle == nil {
		return ErrConflict
	}
	if r.mode != CIManagedRecover && r.scheduler == nil {
		return ErrConflict
	}
	if started != nil {
		started()
	}
	// Primary workers and the log collector share one child context: a
	// stalled download is cancelled and joined with everything else before
	// Run returns, and the running flag clears only after all joins.
	group, gctx := errgroup.WithContext(ctx)
	if r.mode == CIManagedRecover {
		group.Go(func() error { return r.lifecycle.Run(gctx) })
	} else {
		group.Go(func() error { return r.scheduler.Run(gctx) })
	}
	if r.logs != nil {
		group.Go(func() error { return r.logs.Run(gctx) })
	}
	return group.Wait()
}

// logsAvailable reports whether completed-job log collection is actually
// running. It is a liveness fact only — never proof of funding, readiness,
// or job results.
func (r *CIManagedRuntime) logsAvailable() bool {
	return r != nil && r.running.Load() && r.logs != nil
}

// CIConfig overlays runtime-owned admission/bootstrap configuration onto a
// copy of the base CI config. Run mode sets Admission and Bootstrap;
// recovery clears both. Cache, GitHub App, webhook, and every other base
// field is preserved. Key and token material is copied again here so caller
// mutation or clearing of either buffer cannot affect the runtime.
func (r *CIManagedRuntime) CIConfig(base CIConfig) CIConfig {
	if r == nil {
		return base
	}
	out := base
	out.Managed = r
	if r.mode == CIManagedRun {
		out.Admission = &CIAdmissionConfig{
			SigningKey:        append([]byte(nil), r.signingKey...),
			EnforcerURL:       r.enforcer.base,
			EnforcerToken:     append([]byte(nil), r.enforcer.token...),
			HTTPClient:        r.httpClient,
			AllowLoopbackHTTP: r.enforcerLoopbackHTTP,
		}
		out.Bootstrap = &CIBootstrapConfig{ProfileDigest: r.profileDigest}
	} else {
		out.Admission = nil
		out.Bootstrap = nil
	}
	return out
}

// Pool reports the live managed-runner readiness snapshot for the CI
// overview. It is read-only: PoolReady from the funding query is a snapshot,
// never a reservation, and claimCIManagedRunner remains the admission
// authority. No amounts, tokens, hosts, or provider inventory are exposed.
func (r *CIManagedRuntime) Pool(ctx context.Context) CIPool {
	if r == nil {
		return ciPoolOffline
	}
	if r.mode != CIManagedRecover && r.mode != CIManagedRun {
		return ciPoolOffline
	}
	if !r.running.Load() {
		return CIPool{Ready: false, Status: "not_running", Message: "Managed CI workers are not running."}
	}
	if r.mode == CIManagedRecover {
		return CIPool{
			Ready:   false,
			Status:  "recovering",
			Message: "Existing CI runners are being reconciled. New provisioning and guest admission are paused.",
		}
	}
	pctx, cancel := context.WithTimeout(ctx, ciManagedDBBound)
	defer cancel()
	health, err := r.enforcer.health(pctx)
	if err != nil || !health.Running || health.GitHubAppID != r.appID.Load() {
		return CIPool{
			Ready:   false,
			Status:  "enforcer_unavailable",
			Message: "The independent CI enforcer is unavailable or does not match this GitHub App.",
		}
	}
	pool, err := r.store.ciManagedFundingPool(pctx, r.profileDigest)
	if err != nil {
		return CIPool{Ready: false, Status: "unavailable", Message: "CI infrastructure readiness could not be verified."}
	}
	return pool
}

// ProfileDigest returns the normalized provisioning profile digest. It is
// non-secret and empty for recovery mode.
func (r *CIManagedRuntime) ProfileDigest() string {
	if r == nil {
		return ""
	}
	return r.profileDigest
}
