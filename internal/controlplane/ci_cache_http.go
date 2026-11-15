package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	ciCacheJSONLimit    = 8 << 10
	ciCacheAuthBudget   = 15 * time.Second
	ciCacheJWKSBytes    = 256 << 10
	ciCacheJWKSDeadline = 5 * time.Second
	ciCacheMetaWorkers  = 8
	ciCacheGCInterval   = 2 * time.Second
	ciCacheGCBudget     = 2 * time.Second

	ciCacheInfoPath      = "/v1/ci/cache/info"
	ciCacheLookupPath    = "/v1/ci/cache/lookup"
	ciCacheReservePath   = "/v1/ci/cache/reservations"
	ciCacheObjectsPrefix = "/v1/ci/cache/objects/"
)

var (
	errCICacheConfig      = errors.New("invalid CI cache server configuration")
	errCICacheUnavailable = errors.New("CI cache is unavailable")
	errCICacheBusy        = errors.New("CI cache is busy")
	errCICacheBadRequest  = errors.New("invalid CI cache request")
	errCICacheTooLarge    = errors.New("CI cache request too large")
)

// CICacheServerConfig wires the authenticated cache API to the shared store,
// a private filesystem backend root and the existing GitHub App credentials.
// keySet is a private test seam; production always uses the fixed remote JWKS.
type CICacheServerConfig struct {
	Store             *Store
	RootDir           string
	PublicURL         string
	GitHub            CIConfig
	AllowLoopbackHTTP bool
	keySet            oidc.KeySet
}

// CICacheServer is the standalone cache HTTP handler plus its bounded cleanup
// worker. It never configures, funds or migrates anything.
type CICacheServer struct {
	store    *Store
	blob     *ciCacheBlobStore
	github   *ciGitHubClient
	keys     oidc.KeySet
	audience string

	root   context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	done     chan struct{}
	closeErr error
	started  bool
	wg       sync.WaitGroup

	meta    chan struct{}
	reads   chan struct{}
	uploads chan struct{}
}

func ciCacheAPIBase(raw string, allowLoopback bool) (string, error) {
	trimmed := strings.TrimRight(raw, "/")
	if trimmed == "" {
		trimmed = "https://api.github.com"
	}
	origin, _, err := ciCacheAudienceForURL(trimmed, allowLoopback)
	if err != nil {
		return "", err
	}
	if origin == "https://api.github.com" {
		return origin, nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", errCICacheConfig
	}
	ip := net.ParseIP(u.Hostname())
	if allowLoopback && u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return origin, nil
	}
	return "", errCICacheConfig
}

// NewCICacheServer validates configuration, opens the private blob store once
// and derives the service-realm audience from the canonical public origin.
// The constructor context covers startup checks only; the server owns a
// separate runtime context for HTTP, worker and JWKS refresh.
func NewCICacheServer(ctx context.Context, cfg CICacheServerConfig) (*CICacheServer, error) {
	if ctx == nil || ctx.Err() != nil || cfg.Store == nil || cfg.RootDir == "" {
		return nil, errCICacheConfig
	}
	_, audience, err := ciCacheAudienceForURL(cfg.PublicURL, cfg.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	base, err := ciCacheAPIBase(cfg.GitHub.APIBaseURL, cfg.AllowLoopbackHTTP)
	if err != nil {
		return nil, err
	}
	gh := cfg.GitHub
	gh.APIBaseURL = base
	github, err := (&HTTPServer{config: HTTPConfig{CI: gh}}).ciGitHub()
	if err != nil {
		return nil, errCICacheConfig
	}
	// The pool singleton must already exist (migrations are the caller's
	// job); a bounded read proves the database/schema is reachable before any
	// filesystem side effect. Disabled or empty pools are valid.
	poolCtx := ctx
	poolCancel := context.CancelFunc(func() {})
	if _, bounded := ctx.Deadline(); !bounded {
		poolCtx, poolCancel = context.WithTimeout(ctx, ciCacheAuthBudget)
	}
	_, err = cfg.Store.ciCachePoolState(poolCtx)
	poolCancel()
	if err != nil || ctx.Err() != nil {
		return nil, errCICacheConfig
	}
	blob, err := newCICacheBlobStore(cfg.RootDir)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		blob.Close()
		return nil, errCICacheConfig
	}
	root, cancel := context.WithCancel(context.Background())
	keys := cfg.keySet
	if keys == nil {
		keys = oidc.NewRemoteKeySet(
			oidc.ClientContext(root, ciCacheJWKSClient()),
			ciCacheOIDCIssuer+"/.well-known/jwks")
	}
	return &CICacheServer{
		store: cfg.Store, blob: blob, github: github, keys: keys,
		audience: audience, root: root, cancel: cancel,
		done:    make(chan struct{}),
		meta:    make(chan struct{}, ciCacheMetaWorkers),
		reads:   make(chan struct{}, ciCacheReadWorkers),
		uploads: make(chan struct{}, ciCacheUploadWorkers),
	}, nil
}

// available reports whether the cache feature may be advertised: the server
// is open and the pool is enabled against this exact backend fingerprint.
func (s *CICacheServer) available(ctx context.Context) bool {
	if s == nil || s.isClosed() {
		return false
	}
	pool, err := s.store.ciCachePoolState(ctx)
	return err == nil && pool.Enabled && pool.BackendDigest == s.blob.BackendDigest()
}

// BackendDigest exposes the filesystem backend fingerprint for operators and
// tests configuring the matching pool; it is not served over HTTP.
func (s *CICacheServer) BackendDigest() string {
	if s == nil || s.blob == nil {
		return ""
	}
	return s.blob.BackendDigest()
}

func (s *CICacheServer) workerStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *CICacheServer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *CICacheServer) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

func (s *CICacheServer) end() { s.wg.Done() }

// Close cancels the runtime context, closes the filesystem backend and waits
// for every in-flight HTTP operation and worker tick before returning. The
// shared store is owned by the caller and is never closed here.
func (s *CICacheServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	err := s.blob.Close()
	s.wg.Wait()
	s.mu.Lock()
	s.closeErr = err
	close(s.done)
	s.mu.Unlock()
	return err
}

// Run drives the bounded cleanup worker until ctx or server shutdown. It may
// be called once and never after Close.
func (s *CICacheServer) Run(ctx context.Context) error {
	if ctx == nil {
		return errCICacheConfig
	}
	s.mu.Lock()
	if s.closed || s.started {
		s.mu.Unlock()
		return errCICacheConfig
	}
	s.started = true
	s.mu.Unlock()
	workerCtx, stop := ciCacheMergeCtx(ctx, s.root)
	defer stop()
	if !s.begin() {
		return errCICacheUnavailable
	}
	defer s.end()
	ticker := time.NewTicker(ciCacheGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-workerCtx.Done():
			return nil
		case <-ticker.C:
			s.cleanupOnce(workerCtx)
		}
	}
}

func ciCacheMergeCtx(parent, stop context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	done := context.AfterFunc(stop, cancel)
	if stop.Err() != nil {
		cancel()
	}
	return ctx, func() { done(); cancel() }
}

// ciCacheDeadlineWatch interrupts in-flight HTTP body reads and response
// writes when ctx ends: connection deadlines are set to now BEFORE the body
// is closed, because a blocked Read may not observe Close alone. The returned
// stop function joins the watcher; callers must invoke it before canceling so
// a successful request never gets an early deadline, and before returning so
// the goroutine cannot outlive the handler.
func ciCacheDeadlineWatch(w http.ResponseWriter, closer io.Closer, ctx context.Context) func() {
	var once sync.Once
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now())
			_ = rc.SetWriteDeadline(time.Now())
			if closer != nil {
				closer.Close()
			}
		case <-stop:
			if ctx.Err() != nil {
				rc := http.NewResponseController(w)
				_ = rc.SetReadDeadline(time.Now())
				_ = rc.SetWriteDeadline(time.Now())
				if closer != nil {
					closer.Close()
				}
			}
		}
	}()
	return func() {
		once.Do(func() { close(stop) })
		<-finished
	}
}

// cleanupOnce claims one bounded batch for THIS backend only (the pool's
// configured digest must match our filesystem fingerprint), commits the
// claims, then deletes and acks each with bounded concurrency. Uncertain
// deletions are retried later and never refund quota.
func (s *CICacheServer) cleanupOnce(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, ciCacheMaxOperation)
	defer cancel()
	pool, err := s.store.ciCachePoolState(opCtx)
	if err != nil || !ciCacheBlobValidDigest(pool.BackendDigest) ||
		pool.BackendDigest != s.blob.BackendDigest() {
		return
	}
	claims, err := s.store.claimCICacheCleanup(opCtx, pool.BackendDigest, ciCacheCleanupWorkers)
	if err != nil || len(claims) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, claim := range claims {
		wg.Add(1)
		go func(c ciCacheCleanupClaim) {
			defer wg.Done()
			dctx, dcancel := context.WithTimeout(opCtx, ciCacheMaxOperation)
			absent, err := s.blob.Delete(dctx, c.ObjectID)
			dcancel()
			fresh, fcancel := context.WithTimeout(s.root, ciCacheGCBudget)
			defer fcancel()
			if err == nil && absent {
				_ = s.store.ackCICacheAbsent(fresh, c)
				return
			}
			_ = s.store.retryCICacheCleanup(fresh, c)
		}(claim)
	}
	wg.Wait()
}

func (s *CICacheServer) acquire(sem chan struct{}, w http.ResponseWriter) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		writeError(w, http.StatusServiceUnavailable, errCICacheBusy)
		return false
	}
}

func (s *CICacheServer) release(sem chan struct{}) { <-sem }

func (s *CICacheServer) writeCacheError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCICacheTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, errCICacheBadRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, errCICacheBusy), errors.Is(err, errCICacheUnavailable):
		writeError(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, err)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, ErrCapacity), errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusServiceUnavailable, errCICacheUnavailable)
	}
}

// ServeHTTP routes the exact five cache endpoints. It is normally reached
// through the outer HTTP server's CI delegation, which already applied the
// shared request limiter.
func (s *CICacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.begin() {
		writeError(w, http.StatusServiceUnavailable, errCICacheUnavailable)
		return
	}
	defer s.end()
	// A future connection deadline keeps transport-level body draining after
	// any early return bounded; per-stage budgets tighten it below.
	ciCacheDeadline(w, ciCacheAuthBudget)
	if r == nil || r.URL == nil || r.URL.RawPath != "" {
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	path := r.URL.Path
	switch path {
	case ciCacheInfoPath:
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.ciCacheInfo(w, r)
		return
	case ciCacheLookupPath:
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.ciCacheLookup(w, r)
		return
	case ciCacheReservePath:
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.ciCacheReserve(w, r)
		return
	}
	if rest, ok := strings.CutPrefix(path, ciCacheObjectsPrefix); ok {
		if strings.Contains(rest, "/") || !ciCacheBlobValidID(rest) {
			writeError(w, http.StatusNotFound, ErrNotFound)
			return
		}
		switch r.Method {
		case http.MethodPut:
			s.ciCacheUpload(w, r, rest)
		case http.MethodGet:
			s.ciCacheDownload(w, r, rest)
		default:
			methodNotAllowed(w)
		}
		return
	}
	writeError(w, http.StatusNotFound, ErrNotFound)
}

func (s *CICacheServer) ciCacheInfo(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		s.writeCacheError(w, errCICacheBadRequest)
		return
	}
	if !s.acquire(s.meta, w) {
		return
	}
	defer s.release(s.meta)
	ctx, stop := ciCacheMergeCtx(r.Context(), s.root)
	defer stop()
	infoCtx, cancel := context.WithTimeout(ctx, ciCacheAuthBudget)
	defer cancel()
	enabled := false
	var maxEntry int64
	if !s.isClosed() {
		if pool, err := s.store.ciCachePoolState(infoCtx); err == nil &&
			pool.Enabled && pool.BackendDigest == s.blob.BackendDigest() {
			enabled = true
			maxEntry = pool.MaxEntryBytes
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schemaVersion": 1, "enabled": enabled,
		"audience": s.audience, "maxEntryBytes": maxEntry,
	})
}

// ciCacheRunnerQuery accepts exactly one well-formed runnerName hint.
func ciCacheRunnerQuery(r *http.Request) (string, error) {
	if r.URL.RawQuery == "" {
		return "", errCICacheBadRequest
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 {
		return "", errCICacheBadRequest
	}
	values, ok := query["runnerName"]
	if !ok || len(values) != 1 || !ciSourceRunnerName.MatchString(values[0]) {
		return "", errCICacheBadRequest
	}
	return values[0], nil
}

func (s *CICacheServer) ciCacheToken(ctx context.Context, r *http.Request) (ciCacheToken, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	raw := strings.TrimPrefix(values[0], "Bearer ")
	if raw == "" || len(raw) > ciCacheMaxTokenBytes ||
		strings.ContainsAny(raw, " \t\r\n") {
		return ciCacheToken{}, errCICacheUnauthorized
	}
	return verifyCICacheToken(ctx, s.keys, s.store.now, raw, s.audience)
}

// ciCacheGrant resolves the runner-name hint to the actual admitted job,
// binds GitHub's check run and re-checks the clock after the source call.
func (s *CICacheServer) ciCacheGrant(ctx context.Context, runnerName string,
	token ciCacheToken) (ciCacheGrant, ciCacheActiveJob, error) {
	job, err := s.store.ciCacheJobForRunner(ctx, s.store.pool, runnerName, token)
	if err != nil {
		return ciCacheGrant{}, ciCacheActiveJob{}, err
	}
	checkRun, err := s.github.ciCacheCheckRun(ctx, job, token, s.store.now)
	if err != nil {
		return ciCacheGrant{}, ciCacheActiveJob{}, err
	}
	grant, err := makeCICacheGrant(job, token, checkRun, s.store.now())
	if err != nil {
		return ciCacheGrant{}, ciCacheActiveJob{}, err
	}
	if err := ciCacheCheckGrantClock(ctx, grant,
		job.Scope.ExecutionDeadline, s.store.now()); err != nil {
		return ciCacheGrant{}, ciCacheActiveJob{}, err
	}
	return grant, job, nil
}

// ciCacheReadyPool requires an enabled pool bound to this exact backend.
func (s *CICacheServer) ciCacheReadyPool(ctx context.Context) (ciCachePoolState, error) {
	pool, err := s.store.ciCachePoolState(ctx)
	if err != nil || !pool.Enabled || pool.BackendDigest != s.blob.BackendDigest() {
		return pool, errCICacheUnavailable
	}
	return pool, nil
}

// ciCacheBudget derives the remaining real duration from the tightest of the
// operation cap, identity expiry, original token age, job deadline and any
// caller-supplied object deadlines.
func ciCacheBudget(g ciCacheGrant, job ciCacheActiveJob, now time.Time,
	extra ...time.Time) (time.Duration, error) {
	deadline := now.Add(ciCacheMaxOperation)
	candidates := append([]time.Time{
		g.identity.ExpiresAt,
		g.token.identity.IssuedAt.Add(ciCacheMaxTokenAge),
		job.Scope.ExecutionDeadline,
	}, extra...)
	for _, candidate := range candidates {
		if candidate.Before(deadline) {
			deadline = candidate
		}
	}
	d := deadline.Sub(now)
	if d <= 0 {
		return 0, errCICacheUnauthorized
	}
	return d, nil
}

func ciCacheDeadline(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(d))
	_ = rc.SetWriteDeadline(time.Now().Add(d))
}

// ciCacheInvalidate queues owned-object cleanup on a fresh bounded context;
// it never refunds quota and never runs inline file deletion.
func (s *CICacheServer) ciCacheInvalidate(grant ciCacheGrant, id string) {
	ctx, cancel := context.WithTimeout(s.root, ciCacheGCBudget)
	defer cancel()
	_ = s.store.invalidateCICacheObject(ctx, grant, s.blob.BackendDigest(), id)
}

func ciCacheRequestJSON(r *http.Request) ([]byte, error) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return nil, errCICacheBadRequest
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, ciCacheJSONLimit+1))
	if err != nil || !utf8.Valid(body) {
		return nil, errCICacheBadRequest
	}
	if len(body) > ciCacheJSONLimit {
		return nil, errCICacheTooLarge
	}
	return body, nil
}

// ciCacheJSONFields enforces the exact required key set after the recursive
// strict-JSON pass (duplicate keys, nulls, trailing data, non-UTF8 already
// rejected); case-variant aliases land as unknown keys and fail the count.
func ciCacheJSONFields(raw []byte, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := ciEnforcerStrictJSON(raw, &fields); err != nil || fields == nil {
		return nil, errCICacheBadRequest
	}
	if len(fields) != len(required) {
		return nil, errCICacheBadRequest
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, errCICacheBadRequest
		}
	}
	return fields, nil
}

func ciCacheJSONString(fields map[string]json.RawMessage, key string) (string, error) {
	var value string
	if err := json.Unmarshal(fields[key], &value); err != nil {
		return "", errCICacheBadRequest
	}
	return value, nil
}

func ciCacheJSONInt(fields map[string]json.RawMessage, key string) (int64, error) {
	raw := string(fields[key])
	if raw == "" || len(raw) > 20 {
		return 0, errCICacheBadRequest
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !(c >= '0' && c <= '9' || (i == 0 && c == '-')) {
			return 0, errCICacheBadRequest
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errCICacheBadRequest
	}
	return value, nil
}

type ciCacheLookupBody struct {
	runnerName   string
	key          string
	cacheVersion string
}

func ciCacheParseLookup(raw []byte) (ciCacheLookupBody, error) {
	var out ciCacheLookupBody
	fields, err := ciCacheJSONFields(raw, "schemaVersion", "runnerName", "key", "cacheVersion")
	if err != nil {
		return out, err
	}
	version, err := ciCacheJSONInt(fields, "schemaVersion")
	if err != nil || version != 1 {
		return out, errCICacheBadRequest
	}
	if out.runnerName, err = ciCacheJSONString(fields, "runnerName"); err != nil ||
		!ciSourceRunnerName.MatchString(out.runnerName) {
		return out, errCICacheBadRequest
	}
	if out.key, err = ciCacheJSONString(fields, "key"); err != nil ||
		!ciCacheIdentityText(out.key, ciCacheMaxKeyBytes, false) {
		return out, errCICacheBadRequest
	}
	if out.cacheVersion, err = ciCacheJSONString(fields, "cacheVersion"); err != nil ||
		!ciCacheIdentityText(out.cacheVersion, ciCacheMaxVersionBytes, false) {
		return out, errCICacheBadRequest
	}
	return out, nil
}

type ciCacheReserveBody struct {
	ciCacheLookupBody
	sizeBytes int64
	sha256    string
}

func ciCacheParseReserve(raw []byte) (ciCacheReserveBody, error) {
	var out ciCacheReserveBody
	fields, err := ciCacheJSONFields(raw, "schemaVersion", "runnerName", "key",
		"cacheVersion", "sizeBytes", "sha256")
	if err != nil {
		return out, err
	}
	version, err := ciCacheJSONInt(fields, "schemaVersion")
	if err != nil || version != 1 {
		return out, errCICacheBadRequest
	}
	if out.runnerName, err = ciCacheJSONString(fields, "runnerName"); err != nil ||
		!ciSourceRunnerName.MatchString(out.runnerName) {
		return out, errCICacheBadRequest
	}
	if out.key, err = ciCacheJSONString(fields, "key"); err != nil ||
		!ciCacheIdentityText(out.key, ciCacheMaxKeyBytes, false) {
		return out, errCICacheBadRequest
	}
	if out.cacheVersion, err = ciCacheJSONString(fields, "cacheVersion"); err != nil ||
		!ciCacheIdentityText(out.cacheVersion, ciCacheMaxVersionBytes, false) {
		return out, errCICacheBadRequest
	}
	if out.sizeBytes, err = ciCacheJSONInt(fields, "sizeBytes"); err != nil ||
		!ciCacheBlobValidSize(out.sizeBytes) {
		return out, errCICacheBadRequest
	}
	if out.sha256, err = ciCacheJSONString(fields, "sha256"); err != nil ||
		!ciCacheBlobValidDigest(out.sha256) {
		return out, errCICacheBadRequest
	}
	return out, nil
}

// ciCacheAuth carries the two-stage request context: auth covers token
// verification, body/header parsing, job resolution, GitHub source binding,
// pool and metadata preload under one 15s budget clipped to the verified
// token's remaining age and expiry. Data operations run under the merged
// request/server context with their own budget.
type ciCacheAuth struct {
	base   context.Context
	auth   context.Context
	token  ciCacheToken
	stream *ciCacheBlobSource
	finish func() error
	done   func()
}

// data derives the operation context from the base (never the auth context)
// and installs a deadline watcher that is joined before cancel on return.
func (a *ciCacheAuth) data(w http.ResponseWriter, d time.Duration, closer io.Closer) (context.Context, func()) {
	ciCacheDeadline(w, d)
	ctx, cancel := context.WithTimeout(a.base, d)
	stop := ciCacheDeadlineWatch(w, closer, ctx)
	return ctx, func() { stop(); cancel() }
}

// ciCacheBeginAuth acquires the route semaphore, verifies the bearer under a
// bounded stage context, then clips the remaining auth budget to the token's
// own age and expiry windows.
func (s *CICacheServer) ciCacheBeginAuth(w http.ResponseWriter, r *http.Request,
	sem chan struct{}) (*ciCacheAuth, bool) {
	if !s.acquire(sem, w) {
		return nil, false
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { s.release(sem) }) }
	base, baseStop := ciCacheMergeCtx(r.Context(), s.root)
	fail := func(err error) {
		baseStop()
		release()
		s.writeCacheError(w, err)
	}
	stageStart := time.Now()
	stageCtx, stageCancel := context.WithTimeout(base, ciCacheAuthBudget)
	token, err := s.ciCacheToken(stageCtx, r)
	if err != nil {
		stageCancel()
		fail(err)
		return nil, false
	}
	now := s.store.now()
	budget := ciCacheAuthBudget - time.Since(stageStart)
	for _, candidate := range []time.Time{
		token.identity.IssuedAt.Add(ciCacheMaxTokenAge),
		token.identity.ExpiresAt,
	} {
		if remaining := candidate.Sub(now); remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		stageCancel()
		fail(errCICacheUnauthorized)
		return nil, false
	}
	authCtx, cancelAuth := context.WithTimeout(base, budget)
	stageCancel()
	// Every authenticated route reads a body or streams a response; without
	// real connection deadlines a stalled peer could hold a slot forever.
	// Fail closed before touching the stream when unsupported.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(budget)); err != nil {
		cancelAuth()
		baseStop()
		release()
		s.writeCacheError(w, errCICacheUnavailable)
		return nil, false
	}
	if err := rc.SetWriteDeadline(time.Now().Add(budget)); err != nil {
		cancelAuth()
		baseStop()
		release()
		s.writeCacheError(w, errCICacheUnavailable)
		return nil, false
	}
	var stream *ciCacheBlobSource
	if r.Body != nil && r.Body != http.NoBody {
		stream = &ciCacheBlobSource{rc: r.Body}
	}
	stopAuthWatch := ciCacheDeadlineWatch(w, stream, authCtx)
	finish := func() error {
		stopAuthWatch()
		expired := authCtx.Err() != nil
		cancelAuth()
		if expired {
			return errCICacheUnauthorized
		}
		return nil
	}
	done := func() {
		stopAuthWatch()
		cancelAuth()
		baseStop()
		release()
	}
	return &ciCacheAuth{
		base: base, auth: authCtx, token: token, stream: stream,
		finish: finish, done: done,
	}, true
}

func (s *CICacheServer) ciCacheLookup(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ciCacheBeginAuth(w, r, s.meta)
	if !ok {
		return
	}
	defer a.done()
	raw, err := ciCacheRequestJSON(r)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	body, err := ciCacheParseLookup(raw)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	grant, job, err := s.ciCacheGrant(a.auth, body.runnerName, a.token)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if _, err := s.ciCacheReadyPool(a.auth); err != nil {
		s.writeCacheError(w, err)
		return
	}
	if err := a.finish(); err != nil {
		s.writeCacheError(w, err)
		return
	}
	d, err := ciCacheBudget(grant, job, s.store.now())
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	opCtx, finishData := a.data(w, d, nil)
	defer finishData()
	entry, found, err := s.store.lookupCICacheEntry(opCtx, grant,
		s.blob.BackendDigest(), body.key, body.cacheVersion)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"schemaVersion": 1, "found": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schemaVersion": 1, "found": true, "objectId": entry.ObjectID,
		"sizeBytes": entry.SizeBytes, "sha256": entry.SHA256,
	})
}

func (s *CICacheServer) ciCacheReserve(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ciCacheBeginAuth(w, r, s.meta)
	if !ok {
		return
	}
	defer a.done()
	raw, err := ciCacheRequestJSON(r)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	body, err := ciCacheParseReserve(raw)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	pool, err := s.ciCacheReadyPool(a.auth)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if body.sizeBytes > pool.MaxEntryBytes {
		s.writeCacheError(w, errCICacheTooLarge)
		return
	}
	grant, job, err := s.ciCacheGrant(a.auth, body.runnerName, a.token)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if err := a.finish(); err != nil {
		s.writeCacheError(w, err)
		return
	}
	d, err := ciCacheBudget(grant, job, s.store.now())
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	opCtx, finishData := a.data(w, d, nil)
	defer finishData()
	object, created, err := s.store.reserveCICacheObject(opCtx, grant,
		s.blob.BackendDigest(), body.key, body.cacheVersion,
		body.sizeBytes, body.sha256)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, map[string]any{"schemaVersion": 1, "created": false})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"schemaVersion": 1, "created": true, "uploadId": object.ID,
		"deadline": object.UploadDeadlineAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *CICacheServer) ciCacheUpload(w http.ResponseWriter, r *http.Request, id string) {
	a, ok := s.ciCacheBeginAuth(w, r, s.uploads)
	if !ok {
		return
	}
	defer a.done()
	runnerName, err := ciCacheRunnerQuery(r)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	grant, job, err := s.ciCacheGrant(a.auth, runnerName, a.token)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if _, err := s.ciCacheReadyPool(a.auth); err != nil {
		s.writeCacheError(w, err)
		return
	}
	object, err := s.store.ciCacheUploadObject(a.auth, grant, s.blob.BackendDigest(), id)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if object.State == "published" {
		// Retry of an already-published upload: valid only while its entry is
		// still visible inside this exact trust scope.
		if _, err := s.store.ciCacheDownloadEntry(a.auth, grant,
			s.blob.BackendDigest(), id); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"schemaVersion": 1, "saved": true})
			return
		}
		s.ciCacheInvalidate(grant, id)
		s.writeCacheError(w, ErrConflict)
		return
	}
	if r.ContentLength != object.SizeBytes ||
		r.Header.Get("Content-Type") != "application/octet-stream" ||
		r.Header.Get("Content-Encoding") != "" || len(r.Trailer) != 0 {
		s.writeCacheError(w, errCICacheBadRequest)
		return
	}
	d, err := ciCacheBudget(grant, job, s.store.now(),
		object.UploadDeadlineAt, object.ExpiresAt)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if err := a.finish(); err != nil {
		s.writeCacheError(w, err)
		return
	}
	opCtx, finishData := a.data(w, d, a.stream)
	defer finishData()
	writerToken, err := ciCacheHexID()
	if err != nil {
		s.writeCacheError(w, errCICacheUnavailable)
		return
	}
	backend := s.blob.BackendDigest()
	_, err = s.blob.Put(opCtx, id, a.stream, object.SizeBytes, object.SHA256,
		func(claimCtx context.Context) error {
			claimed, err := s.store.claimCICacheWrite(claimCtx, grant, backend, id, writerToken)
			if err != nil {
				return err
			}
			if claimed.SizeBytes != object.SizeBytes || claimed.SHA256 != object.SHA256 ||
				claimed.KeyDigest != object.KeyDigest ||
				claimed.VersionDigest != object.VersionDigest {
				return ErrConflict
			}
			return nil
		})
	if err != nil {
		s.ciCacheInvalidate(grant, id)
		s.writeCacheError(w, err)
		return
	}
	published, err := s.store.publishCICacheObject(opCtx, grant, backend, id, writerToken)
	if err != nil {
		s.ciCacheInvalidate(grant, id)
		s.writeCacheError(w, err)
		return
	}
	// saved is true only when the metadata publish actually committed; a
	// sealed-but-unpublished or immutably-conflicted object is never saved.
	writeJSON(w, http.StatusOK, map[string]any{"schemaVersion": 1, "saved": published})
}

func (s *CICacheServer) ciCacheDownload(w http.ResponseWriter, r *http.Request, id string) {
	a, ok := s.ciCacheBeginAuth(w, r, s.reads)
	if !ok {
		return
	}
	defer a.done()
	runnerName, err := ciCacheRunnerQuery(r)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	grant, job, err := s.ciCacheGrant(a.auth, runnerName, a.token)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if _, err := s.ciCacheReadyPool(a.auth); err != nil {
		s.writeCacheError(w, err)
		return
	}
	entry, err := s.store.ciCacheDownloadEntry(a.auth, grant, s.blob.BackendDigest(), id)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	d, err := ciCacheBudget(grant, job, s.store.now(), entry.ExpiresAt)
	if err != nil {
		s.writeCacheError(w, err)
		return
	}
	if r.Header.Get("Range") != "" {
		s.writeCacheError(w, errCICacheBadRequest)
		return
	}
	if err := a.finish(); err != nil {
		s.writeCacheError(w, err)
		return
	}
	opCtx, finishData := a.data(w, d, nil)
	defer finishData()
	reader, err := s.blob.Open(opCtx, id, entry.SizeBytes, entry.SHA256)
	if err != nil {
		s.ciCacheInvalidate(grant, id)
		s.writeCacheError(w, err)
		return
	}
	defer reader.Close()
	// The grant and entry are revalidated after the integrity hash and before
	// any byte or header is exposed.
	fresh, err := s.store.ciCacheDownloadEntry(opCtx, grant, s.blob.BackendDigest(), id)
	if err != nil || fresh.ObjectID != entry.ObjectID || fresh.SizeBytes != entry.SizeBytes ||
		fresh.SHA256 != entry.SHA256 {
		s.writeCacheError(w, errCICacheUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(entry.SizeBytes, 10))
	w.Header().Set("ETag", `"`+entry.SHA256+`"`)
	w.Header().Set("X-Canter-Cache-SHA256", entry.SHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = io.CopyN(w, reader, entry.SizeBytes)
}

// ciCacheJWKSClient is the dedicated remote-key-set transport: no proxy, no
// redirects, a hard timeout and a bounded response body.
func ciCacheJWKSClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: ciCacheJWKSTransport{base: transport},
		Timeout:   ciCacheJWKSDeadline,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type ciCacheJWKSTransport struct {
	base http.RoundTripper
}

var errCICacheJWKSBytes = errors.New("JWKS response too large")

func (t ciCacheJWKSTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &ciCacheJWKSBody{rc: resp.Body, left: ciCacheJWKSBytes}
	return resp, nil
}

type ciCacheJWKSBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *ciCacheJWKSBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var probe [1]byte
		n, err := b.rc.Read(probe[:])
		if n > 0 {
			return 0, errCICacheJWKSBytes
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *ciCacheJWKSBody) Close() error { return b.rc.Close() }
