package controlplane

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

var ciEnforcerLeaseIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ciEnforcerStatus is the operator-visible lease status DTO (lower-camel).
// Resources is the authenticated retained inventory while unsettled; a
// tombstone reports an empty non-nil array and retains only identity,
// digests, and stop/settled flags.
type ciEnforcerStatus struct {
	LeaseID               string            `json:"leaseId"`
	OperationID           string            `json:"operationId"`
	PlanDigest            string            `json:"planDigest"`
	SpecDigest            string            `json:"specDigest"`
	DeadlineUnixMs        int64             `json:"deadlineUnixMs"`
	CreateSettled         bool              `json:"createSettled"`
	RegistrationAttempted bool              `json:"registrationAttempted"`
	RegistrationSettled   bool              `json:"registrationSettled"`
	GitHubRunnerID        int64             `json:"githubRunnerId"`
	GitHubRepositoryID    int64             `json:"githubRepositoryId"`
	RunnerName            string            `json:"runnerName"`
	StopRequested         bool              `json:"stopRequested"`
	Settled               bool              `json:"settled"`
	Resources             []ciOwnedResource `json:"resources"`
}

type ciEnforcerLeaseSpecDTO struct {
	Plan                 ciRunnerLifecyclePlan `json:"plan"`
	GitHubInstallationID int64                 `json:"githubInstallationId"`
	GitHubRepositoryID   int64                 `json:"githubRepositoryId"`
	SizeID               string                `json:"sizeId"`
}

type ciEnforcerApproveDTO struct {
	ExecutionDeadlineUnixMs int64 `json:"executionDeadlineUnixMs"`
}

type ciEnforcerCloseDTO struct {
	Result   string `json:"result"`
	ServerID string `json:"serverId,omitempty"`
}

type ciEnforcerJITResponse struct {
	Status           ciEnforcerStatus `json:"status"`
	GitHubRunnerID   int64            `json:"githubRunnerId"`
	EncodedJitConfig string           `json:"encodedJitConfig"`
}

// CIEnforcerServerConfig configures the independent enforcer daemon.
type CIEnforcerServerConfig struct {
	StateDir string
	Token    []byte
	Compute  *compute.Client
	GitHub   CIConfig
}

// CIEnforcerServer is the operator-authenticated enforcer HTTP service. It
// implements ServeHTTP, Run (watchdog reconciliation) and Close.
type CIEnforcerServer struct {
	engine  *ciEnforcerEngine
	token   []byte
	handler http.Handler
}

// newCIEnforcerEngine opens or creates a private state directory, takes the
// process flock, loads every durable lease record, and wires the dual clocks.
// Corrupt, oversized, wrong-mode, wrong-owner or non-regular state refuses
// startup; nothing is silently skipped.
func newCIEnforcerEngine(dir string, computeClient ciEnforcerCompute, github *ciGitHubClient, wallNow, monoNow func() time.Time) (*ciEnforcerEngine, error) {
	if computeClient == nil {
		return nil, errors.New("enforcer requires a compute client")
	}
	if wallNow == nil {
		wallNow = func() time.Time { return time.Now().UTC() }
	}
	if monoNow == nil {
		monoNow = time.Now
	}
	var info os.FileInfo
	var err error
	info, err = os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("enforcer state path is not a directory")
	}
	// Canonicalize: all state I/O goes through the resolved real path. The
	// Lstat above already refuses a symlinked final component.
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	dir = canonical
	if info.Mode().Perm() != 0o700 {
		return nil, errors.New("enforcer state directory must be mode 0700")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return nil, errors.New("enforcer state directory has wrong owner")
	}
	// Exclusive process lock: regular file, no symlink, private mode.
	lockPath := filepath.Join(dir, ".lock")
	if li, lerr := os.Lstat(lockPath); lerr == nil {
		if !li.Mode().IsRegular() || li.Mode().Perm() != 0o600 {
			return nil, errors.New("enforcer lock file has wrong type or permissions")
		}
	} else if !errors.Is(lerr, os.ErrNotExist) {
		return nil, lerr
	}
	flock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*ciEnforcerEngine, error) {
		flock.Close()
		return nil, err
	}
	// The lock file itself must be a private regular single-link file.
	if li, ferr := flock.Stat(); ferr != nil {
		return fail(ferr)
	} else if !li.Mode().IsRegular() || li.Mode().Perm() != 0o600 {
		return fail(errors.New("enforcer lock file has wrong type or permissions"))
	} else if st, ok := li.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return fail(errors.New("enforcer lock file has wrong owner or link count"))
	}
	if err = syscall.Flock(int(flock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(errors.New("enforcer state directory is locked by another process"))
	}
	engineContext, cancel := context.WithCancel(context.Background())
	e := &ciEnforcerEngine{
		dir: dir, compute: computeClient, github: github,
		wallNow: wallNow, monoNow: monoNow,
		leases:  map[string]*ciEnforcerLeaseState{},
		opIndex: map[string]string{},
		flock:   flock,
		ctx:     engineContext,
		cancel:  cancel,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fail(err)
	}
	count := 0
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasPrefix(name, "lease-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		count++
		if count > ciEnforcerMaxLeases {
			return fail(errors.New("enforcer lease record limit exceeded"))
		}
		leaseID := strings.TrimSuffix(strings.TrimPrefix(name, "lease-"), ".json")
		l, err := e.loadLeaseFile(leaseID)
		if err != nil {
			return fail(err)
		}
		if l == nil {
			return fail(errors.New("enforcer lease record missing"))
		}
		if !l.Settled && computeClient.ValidateCIRunnerScope(ciEnforcerScope(l.Spec)) != nil {
			return fail(errors.New("enforcer lease provider scope mismatch"))
		}
		opID := l.Spec.Plan.OperationID
		if prev, exists := e.opIndex[opID]; exists && prev != leaseID {
			return fail(errors.New("enforcer lease operation index conflict"))
		}
		e.leases[leaseID] = &ciEnforcerLeaseState{lease: l}
		e.opIndex[opID] = leaseID
	}
	return e, nil
}

func ciEnforcerGitHubClient(cfg CIConfig) (*ciGitHubClient, error) {
	if cfg.AppID <= 0 && len(cfg.PrivateKeyPEM) == 0 {
		return nil, nil
	}
	if cfg.AppID <= 0 {
		return nil, errors.New("CI enforcer GitHub App ID is required with a key")
	}
	key, err := parseCIRSAKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, errors.New("CI enforcer GitHub App key is invalid")
	}
	base := strings.TrimRight(cfg.APIBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	} else {
		clone := *hc
		hc = &clone
	}
	if hc.Timeout <= 0 || hc.Timeout > 20*time.Second {
		hc.Timeout = 20 * time.Second
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ciGitHubClient{appID: cfg.AppID, key: key, base: base, hc: hc}, nil
}

// NewCIEnforcerServer builds the daemon. The operator token must be at least
// 32 bytes; it is compared constant-time and is never a guest capability.
// A missing compute client is rejected: cleanup must never run blind.
func NewCIEnforcerServer(config CIEnforcerServerConfig) (*CIEnforcerServer, error) {
	if len(config.Token) < 32 {
		return nil, errors.New("CI enforcer operator token must be at least 32 bytes")
	}
	if config.Compute == nil {
		return nil, errors.New("CI enforcer requires a compute client")
	}
	github, err := ciEnforcerGitHubClient(config.GitHub)
	if err != nil {
		return nil, err
	}
	e, err := newCIEnforcerEngine(config.StateDir, config.Compute, github, nil, nil)
	if err != nil {
		return nil, err
	}
	s := &CIEnforcerServer{engine: e, token: append([]byte(nil), config.Token...)}
	s.handler = s.routes()
	return s, nil
}

func (s *CIEnforcerServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Run drives watchdog reconciliation until ctx ends; Close stops it safely.
func (s *CIEnforcerServer) Run(ctx context.Context) {
	s.engine.Run(ctx)
}

func (s *CIEnforcerServer) Close() error {
	s.engine.close()
	return nil
}

func (s *CIEnforcerServer) authorized(r *http.Request) bool {
	auth := r.Header.Values("Authorization")
	if len(auth) != 1 {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth[0], prefix) {
		return false
	}
	token := []byte(strings.TrimPrefix(auth[0], prefix))
	return subtle.ConstantTimeCompare(token, s.token) == 1
}

func (s *CIEnforcerServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/leases/", s.handleLease)
	return mux
}

// handleHealth is the authenticated daemon compatibility probe. It runs
// after the same exact Bearer check as every other route, before any method
// or lease handling, and mutates no journal or lease state. The flat DTO
// exposes only protocol version, liveness, and the configured GitHub App ID.
func (s *CIEnforcerServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		ciEnforcerError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		ciEnforcerError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	e := s.engine
	e.mu.Lock()
	running := e.running && !e.closed
	var appID int64
	if e.github != nil {
		appID = e.github.appID
	}
	e.mu.Unlock()
	ciEnforcerJSON(w, http.StatusOK, map[string]any{
		"version":     1,
		"running":     running,
		"githubAppId": appID,
	})
}

func (s *CIEnforcerServer) handleLease(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		ciEnforcerError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		ciEnforcerError(w, http.StatusNotFound, "not found")
		return
	}
	leaseID := parts[0]
	if !ciEnforcerLeaseIDRe.MatchString(leaseID) {
		ciEnforcerError(w, http.StatusNotFound, "not found")
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	switch {
	case sub == "" && r.Method == http.MethodPut:
		s.handlePut(w, r, leaseID)
	case sub == "reject-unstarted" && r.Method == http.MethodPut:
		s.handleRejectUnstarted(w, r, leaseID)
	case sub == "" && r.Method == http.MethodGet:
		s.handleStatus(w, r, leaseID)
	case sub == "approve" && r.Method == http.MethodPost:
		s.handleApprove(w, r, leaseID)
	case sub == "stop" && r.Method == http.MethodPost:
		s.handleStop(w, r, leaseID)
	case sub == "close-creation" && r.Method == http.MethodPost:
		s.handleCloseCreation(w, r, leaseID)
	case sub == "register-runner" && r.Method == http.MethodPost:
		s.handleRegisterRunner(w, r, leaseID)
	default:
		ciEnforcerError(w, http.StatusNotFound, "not found")
	}
}

func ciEnforcerBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		ciEnforcerError(w, http.StatusRequestEntityTooLarge, "request too large")
		return nil, false
	}
	return raw, true
}

// ciEnforcerObjectKeys strictly tokenizes a JSON object: it must start with
// '{', contain only unique string keys with non-null values, end with '}',
// and have no trailing data. dec.More() alone cannot prove this.
func ciEnforcerObjectKeys(raw []byte) (map[string]bool, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || !strings.HasPrefix(trimmed, "{") {
		return nil, errors.New("expected JSON object")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("expected JSON object")
	}
	keys := map[string]bool{}
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := ktok.(string)
		if !ok || keys[key] {
			return nil, errors.New("duplicate or invalid object key")
		}
		keys[key] = true
		var probe any
		if err = dec.Decode(&probe); err != nil {
			return nil, err
		}
		if probe == nil {
			return nil, errors.New("null field value")
		}
	}
	if _, err = dec.Token(); err != nil { // closing '}'
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return keys, nil
}

// ciEnforcerDecode strictly parses an object body: recursive duplicate-key
// and null rejection at every depth (including nested plan fields), exact
// top-level allowed keys only, DisallowUnknownFields, no trailing data.
func ciEnforcerDecode(w http.ResponseWriter, raw []byte, out any, allowed ...string) bool {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := ciStrictJSONValue(dec); err != nil {
		ciEnforcerError(w, http.StatusBadRequest, "malformed request")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		ciEnforcerError(w, http.StatusBadRequest, "malformed request")
		return false
	}
	keys, err := ciEnforcerObjectKeys(raw)
	if err != nil {
		ciEnforcerError(w, http.StatusBadRequest, "malformed request")
		return false
	}
	allowedSet := map[string]bool{}
	for _, k := range allowed {
		allowedSet[k] = true
	}
	for k := range keys {
		if !allowedSet[k] {
			ciEnforcerError(w, http.StatusBadRequest, "malformed request")
			return false
		}
	}
	dec = json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(out); err != nil {
		ciEnforcerError(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

// ciEnforcerEmptyBody accepts only an empty body or the exact object {}.
func ciEnforcerEmptyBody(w http.ResponseWriter, raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" {
		return true
	}
	ciEnforcerError(w, http.StatusBadRequest, "malformed request")
	return false
}

func ciEnforcerError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func ciEnforcerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *CIEnforcerServer) handlePut(w http.ResponseWriter, r *http.Request, leaseID string) {
	raw, ok := ciEnforcerBody(w, r, 16<<10)
	if !ok {
		return
	}
	var dto ciEnforcerLeaseSpecDTO
	if !ciEnforcerDecode(w, raw, &dto, "plan", "githubInstallationId", "githubRepositoryId", "sizeId") {
		return
	}
	l, err := s.engine.putLease(leaseID, ciEnforcerSpec(dto))
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	ciEnforcerJSON(w, http.StatusOK, l)
}

// handleRejectUnstarted accepts the same pinned spec body as PUT: the
// durable stopped, creation-rejected record is valid even after the setup
// or hard deadline because it attests that no provider stage ever fenced.
func (s *CIEnforcerServer) handleRejectUnstarted(w http.ResponseWriter, r *http.Request, leaseID string) {
	raw, ok := ciEnforcerBody(w, r, 16<<10)
	if !ok {
		return
	}
	var dto ciEnforcerLeaseSpecDTO
	if !ciEnforcerDecode(w, raw, &dto, "plan", "githubInstallationId", "githubRepositoryId", "sizeId") {
		return
	}
	l, err := s.engine.rejectUnstarted(leaseID, ciEnforcerSpec(dto))
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	ciEnforcerJSON(w, http.StatusOK, l)
}

func (s *CIEnforcerServer) handleStatus(w http.ResponseWriter, r *http.Request, leaseID string) {
	st, err := s.engine.status(leaseID)
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	if st.LeaseID == "" {
		ciEnforcerError(w, http.StatusNotFound, "not found")
		return
	}
	ciEnforcerJSON(w, http.StatusOK, st)
}

func (s *CIEnforcerServer) handleApprove(w http.ResponseWriter, r *http.Request, leaseID string) {
	raw, ok := ciEnforcerBody(w, r, 16<<10)
	if !ok {
		return
	}
	var dto ciEnforcerApproveDTO
	if !ciEnforcerDecode(w, raw, &dto, "executionDeadlineUnixMs") {
		return
	}
	st, err := s.engine.approve(leaseID, dto.ExecutionDeadlineUnixMs)
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	ciEnforcerJSON(w, http.StatusOK, st)
}

func (s *CIEnforcerServer) handleStop(w http.ResponseWriter, r *http.Request, leaseID string) {
	raw, ok := ciEnforcerBody(w, r, 16<<10)
	if !ok {
		return
	}
	if !ciEnforcerEmptyBody(w, raw) {
		return
	}
	st, err := s.engine.stop(leaseID)
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	ciEnforcerJSON(w, http.StatusOK, st)
}

func (s *CIEnforcerServer) handleCloseCreation(w http.ResponseWriter, r *http.Request, leaseID string) {
	raw, ok := ciEnforcerBody(w, r, 16<<10)
	if !ok {
		return
	}
	var dto ciEnforcerCloseDTO
	if !ciEnforcerDecode(w, raw, &dto, "result", "serverId") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := s.engine.closeCreation(ctx, leaseID, dto.Result, dto.ServerID)
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	ciEnforcerJSON(w, http.StatusOK, st)
}

func (s *CIEnforcerServer) handleRegisterRunner(w http.ResponseWriter, r *http.Request, leaseID string) {
	raw, ok := ciEnforcerBody(w, r, 16<<10)
	if !ok {
		return
	}
	if !ciEnforcerEmptyBody(w, raw) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	st, runnerID, jit, err := s.engine.registerRunner(ctx, leaseID)
	if err != nil {
		ciEnforcerRPCError(w, err)
		return
	}
	ciEnforcerJSON(w, http.StatusOK, ciEnforcerJITResponse{
		Status: st, GitHubRunnerID: runnerID, EncodedJitConfig: jit,
	})
}

func ciEnforcerRPCError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCIEnforcerRunnerConflict), errors.Is(err, errCIEnforcerConflict):
		ciEnforcerError(w, http.StatusConflict, "conflict")
	case errors.Is(err, errCIEnforcerUnavailable):
		ciEnforcerError(w, http.StatusServiceUnavailable, "unavailable")
	case errors.Is(err, errCIRunnerJobPending):
		ciEnforcerError(w, http.StatusAccepted, "pending")
	case errors.Is(err, compute.ErrCIRunnerOwnership):
		ciEnforcerError(w, http.StatusConflict, "conflict")
	default:
		ciEnforcerError(w, http.StatusInternalServerError, "internal error")
	}
}

// errCIEnforcerScope marks an authenticated scope, digest, or identity
// mismatch; the backend maps it to ErrConflict, never to an outage.
var errCIEnforcerScope = errors.New("CI enforcer scope mismatch")

var errCIEnforcerPending = errors.New("CI enforcer request pending")

var errCIEnforcerTransport = errors.New("CI enforcer transport unavailable")

type ciEnforcerHTTPError struct{ status int }

func (e *ciEnforcerHTTPError) Error() string {
	return fmt.Sprintf("CI enforcer request failed (%d)", e.status)
}

// ciEnforcerPlanDigest computes the pinned plan digest for client-side
// validation; the caller's plan is authoritative on every call.
func ciEnforcerPlanDigest(plan ciRunnerLifecyclePlan) string {
	raw, _ := json.Marshal(ciEnforcerSpec{Plan: plan}.normalize().Plan)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ciEnforcerClient is the typed operator client used by the control-plane
// backend. There is no cached expectation: every call binds the caller's
// pinned plan and validates lease, operation, and plan digest on every ack.
type ciEnforcerClient struct {
	base  string
	token []byte
	hc    *http.Client
}

func newCIEnforcerClient(baseURL string, token []byte, hc *http.Client, allowLoopbackHTTP bool) (*ciEnforcerClient, error) {
	if len(token) < 32 {
		return nil, errors.New("CI enforcer client token must be at least 32 bytes")
	}
	u := strings.TrimRight(baseURL, "/")
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("invalid CI enforcer URL")
	}
	if parsed.Scheme != "https" {
		loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
		if !(allowLoopbackHTTP && parsed.Scheme == "http" && loopback) {
			return nil, errors.New("CI enforcer URL must be HTTPS")
		}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("CI enforcer URL must not contain credentials, query, or fragment")
	}
	if hc == nil {
		hc = &http.Client{}
	} else {
		clone := *hc
		hc = &clone
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ciEnforcerClient{base: u, token: append([]byte(nil), token...), hc: hc}, nil
}

func (c *ciEnforcerClient) call(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+string(c.token))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// Never leak raw transport or URL errors to callers.
		return errCIEnforcerTransport
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (128<<10)+1))
	if err != nil {
		return errors.New("CI enforcer response unreadable")
	}
	if len(raw) > 128<<10 {
		return errors.New("CI enforcer response too large")
	}
	if resp.StatusCode == http.StatusAccepted {
		return errCIEnforcerPending
	}
	if resp.StatusCode != http.StatusOK {
		// A missing lease is availability, not an authenticated scope
		// mismatch: proofs are withheld while owned physical cleanup proceeds.
		return &ciEnforcerHTTPError{status: resp.StatusCode}
	}
	if out != nil {
		if err = ciEnforcerStrictJSON(raw, out); err != nil {
			return errors.New("malformed CI enforcer response")
		}
	}
	return nil
}

// ciEnforcerHealth is the flat authenticated daemon health DTO. It carries
// no journal, state, token, key, lease, or inventory detail.
type ciEnforcerHealth struct {
	Version     int   `json:"version"`
	Running     bool  `json:"running"`
	GitHubAppID int64 `json:"githubAppId"`
}

// health performs the bounded read-only daemon probe. The response must be
// a strict flat object with exactly the three documented keys; version must
// be 1. Redirects are already refused by the client's CheckRedirect policy.
func (c *ciEnforcerClient) health(ctx context.Context) (ciEnforcerHealth, error) {
	var raw json.RawMessage
	if err := c.call(ctx, ciManagedDBBound, http.MethodGet, "/v1/health", nil, &raw); err != nil {
		return ciEnforcerHealth{}, err
	}
	keys, err := ciEnforcerObjectKeys(raw)
	if err != nil {
		return ciEnforcerHealth{}, errors.New("malformed CI enforcer health response")
	}
	if len(keys) != 3 || !keys["version"] || !keys["running"] || !keys["githubAppId"] {
		return ciEnforcerHealth{}, errors.New("malformed CI enforcer health response")
	}
	var health ciEnforcerHealth
	if err := json.Unmarshal(raw, &health); err != nil {
		return ciEnforcerHealth{}, errors.New("malformed CI enforcer health response")
	}
	if health.Version != 1 {
		return ciEnforcerHealth{}, errors.New("unsupported CI enforcer version")
	}
	return health, nil
}

// validateStatus binds every status/ack to the caller's pinned plan: lease,
// operation, plan digest, spec digest shape, and full active/tombstone state
// invariants — including tombstones.
func (c *ciEnforcerClient) validateStatus(st ciEnforcerStatus, plan ciRunnerLifecyclePlan) error {
	if !ciEnforcerLeaseIDRe.MatchString(st.LeaseID) || st.LeaseID != plan.EnforcerLease {
		return fmt.Errorf("%w: lease", errCIEnforcerScope)
	}
	if st.OperationID != plan.OperationID {
		return fmt.Errorf("%w: operation", errCIEnforcerScope)
	}
	if st.PlanDigest != ciEnforcerPlanDigest(plan) {
		return fmt.Errorf("%w: plan digest", errCIEnforcerScope)
	}
	if !ciEnforcerLeaseIDRe.MatchString(st.SpecDigest) {
		return fmt.Errorf("%w: spec digest", errCIEnforcerScope)
	}
	if st.Resources == nil {
		return fmt.Errorf("%w: inventory", errCIEnforcerScope)
	}
	seen := map[string]bool{}
	for _, r := range st.Resources {
		key := r.Kind + "/" + r.ID
		if (r.Kind != "server" && r.Kind != "port" && r.Kind != "security_group") ||
			!ciLifecycleID.MatchString(r.ID) || r.OperationID != plan.OperationID || seen[key] {
			return fmt.Errorf("%w: resource", errCIEnforcerScope)
		}
		seen[key] = true
	}
	if st.Settled {
		// Tombstone: both proofs true, inventory empty, all private metadata
		// and deadlines zeroed; operation identity and digests retained.
		if !st.StopRequested || !st.CreateSettled || !st.RegistrationSettled ||
			len(st.Resources) != 0 || st.GitHubRunnerID != 0 || st.GitHubRepositoryID != 0 ||
			st.RunnerName != "" || st.DeadlineUnixMs != 0 {
			return fmt.Errorf("%w: tombstone", errCIEnforcerScope)
		}
		return nil
	}
	// Active lease: canonical runner name, positive repository, sane deadline.
	if st.RunnerName != ciEnforcerRunnerName(plan.OperationID) ||
		st.GitHubRepositoryID <= 0 || st.DeadlineUnixMs <= 0 ||
		st.DeadlineUnixMs > plan.HardDeadlineAt.UnixMilli() {
		return fmt.Errorf("%w: active state", errCIEnforcerScope)
	}
	return nil
}

func (c *ciEnforcerClient) leasePath(plan ciRunnerLifecyclePlan) string {
	return "/v1/leases/" + plan.EnforcerLease
}

func (c *ciEnforcerClient) PutLease(ctx context.Context, spec ciEnforcerSpec) (ciEnforcerStatus, error) {
	spec = spec.normalize()
	_, specDigest := spec.digests()
	var st ciEnforcerStatus
	err := c.call(ctx, 5*time.Second, http.MethodPut, c.leasePath(spec.Plan), ciEnforcerLeaseSpecDTO(spec), &st)
	if err != nil {
		return st, err
	}
	if err = c.validateStatus(st, spec.Plan); err != nil {
		return st, err
	}
	if st.SpecDigest != specDigest {
		return st, fmt.Errorf("%w: spec digest", errCIEnforcerScope)
	}
	if st.GitHubRepositoryID != spec.GitHubRepositoryID {
		return st, fmt.Errorf("%w: repository", errCIEnforcerScope)
	}
	return st, nil
}

// RejectUnstarted records the durable stopped, creation-rejected state for
// a lease whose control-plane funding committed but whose provider creation
// never fenced a stage. It is STOP-ONLY and valid after setup/hard expiry:
// the ack must prove the rejected-stop binding and must never carry
// registration, approval, or server identity.
func (c *ciEnforcerClient) RejectUnstarted(ctx context.Context, spec ciEnforcerSpec) (ciEnforcerStatus, error) {
	spec = spec.normalize()
	_, specDigest := spec.digests()
	var st ciEnforcerStatus
	err := c.call(ctx, 5*time.Second, http.MethodPut, c.leasePath(spec.Plan)+"/reject-unstarted", ciEnforcerLeaseSpecDTO(spec), &st)
	if err != nil {
		return st, err
	}
	if err = c.validateStatus(st, spec.Plan); err != nil {
		return st, err
	}
	if st.SpecDigest != specDigest {
		return st, fmt.Errorf("%w: spec digest", errCIEnforcerScope)
	}
	if !st.StopRequested || !st.CreateSettled {
		return st, fmt.Errorf("%w: reject state", errCIEnforcerScope)
	}
	if st.Settled {
		// Tombstone: validateStatus already enforced zeroed private
		// metadata, empty inventory, and both absence proofs.
		return st, nil
	}
	if st.GitHubRepositoryID != spec.GitHubRepositoryID || st.GitHubRunnerID != 0 ||
		st.RegistrationAttempted || st.RegistrationSettled {
		return st, fmt.Errorf("%w: reject state", errCIEnforcerScope)
	}
	for _, r := range st.Resources {
		if r.Kind == "server" {
			return st, fmt.Errorf("%w: reject inventory", errCIEnforcerScope)
		}
	}
	return st, nil
}

func (c *ciEnforcerClient) Status(ctx context.Context, plan ciRunnerLifecyclePlan) (ciEnforcerStatus, error) {
	var st ciEnforcerStatus
	if err := c.call(ctx, 5*time.Second, http.MethodGet, c.leasePath(plan), nil, &st); err != nil {
		return st, err
	}
	return st, c.validateStatus(st, plan)
}

func (c *ciEnforcerClient) Approve(ctx context.Context, plan ciRunnerLifecyclePlan, executionDeadlineUnixMs int64) (ciEnforcerStatus, error) {
	var st ciEnforcerStatus
	err := c.call(ctx, 5*time.Second, http.MethodPost, c.leasePath(plan)+"/approve",
		ciEnforcerApproveDTO{ExecutionDeadlineUnixMs: executionDeadlineUnixMs}, &st)
	if err != nil {
		return st, err
	}
	if err = c.validateStatus(st, plan); err != nil {
		return st, err
	}
	// Approval ack must be active, un-stopped, unsettled, carry the exact
	// requested cutoff, and prove created VM plus registered runner identity.
	if st.Settled || st.StopRequested || st.DeadlineUnixMs != executionDeadlineUnixMs ||
		!st.CreateSettled || !st.RegistrationSettled || st.GitHubRunnerID <= 0 ||
		st.GitHubRepositoryID <= 0 || st.RunnerName != ciEnforcerRunnerName(plan.OperationID) {
		return st, fmt.Errorf("%w: approval", errCIEnforcerScope)
	}
	knownServer := false
	for _, r := range st.Resources {
		if r.Kind == "server" {
			knownServer = true
		}
	}
	if !knownServer {
		return st, fmt.Errorf("%w: approval inventory", errCIEnforcerScope)
	}
	return st, nil
}

func (c *ciEnforcerClient) Stop(ctx context.Context, plan ciRunnerLifecyclePlan) (ciEnforcerStatus, error) {
	var st ciEnforcerStatus
	if err := c.call(ctx, 5*time.Second, http.MethodPost, c.leasePath(plan)+"/stop", nil, &st); err != nil {
		return st, err
	}
	return st, c.validateStatus(st, plan)
}

func (c *ciEnforcerClient) CloseCreation(ctx context.Context, plan ciRunnerLifecyclePlan, result, serverID string) (ciEnforcerStatus, error) {
	var st ciEnforcerStatus
	err := c.call(ctx, 20*time.Second, http.MethodPost, c.leasePath(plan)+"/close-creation",
		ciEnforcerCloseDTO{Result: result, ServerID: serverID}, &st)
	if err != nil {
		return st, err
	}
	return st, c.validateStatus(st, plan)
}

// RegisterRunner returns the verified status plus the ephemeral JIT
// material. The encoded config is bounded before decode and the status is
// bound to the caller's plan.
func (c *ciEnforcerClient) RegisterRunner(ctx context.Context, plan ciRunnerLifecyclePlan) (ciEnforcerJITResponse, error) {
	var out ciEnforcerJITResponse
	if err := c.call(ctx, 60*time.Second, http.MethodPost, c.leasePath(plan)+"/register-runner", nil, &out); err != nil {
		return out, err
	}
	if err := c.validateStatus(out.Status, plan); err != nil {
		return out, err
	}
	st := out.Status
	if st.Settled || st.StopRequested || !st.CreateSettled || !st.RegistrationSettled ||
		st.GitHubRepositoryID <= 0 || st.RunnerName != ciEnforcerRunnerName(plan.OperationID) {
		return out, fmt.Errorf("%w: registration state", errCIEnforcerScope)
	}
	knownServer := false
	for _, r := range st.Resources {
		if r.Kind == "server" {
			knownServer = true
		}
	}
	if !knownServer {
		return out, fmt.Errorf("%w: registration inventory", errCIEnforcerScope)
	}
	if out.GitHubRunnerID <= 0 || out.GitHubRunnerID != st.GitHubRunnerID ||
		out.EncodedJitConfig == "" || len(out.EncodedJitConfig) > ciJITConfigMaxBytes {
		return out, fmt.Errorf("%w: registration", errCIEnforcerScope)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.EncodedJitConfig)
	if err != nil || len(decoded) == 0 || len(decoded) > ciJITConfigMaxBytes {
		return out, fmt.Errorf("%w: registration config", errCIEnforcerScope)
	}
	return out, nil
}
