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
	"strings"
	"time"
	"unicode/utf8"
)

// ciAdmissionCapability is the dedicated guest admission credential. Every
// field is bound into the HMAC and must match the durable admission identity;
// no body or query value may override them.
type ciAdmissionCapability struct {
	Version            int    `json:"version"`
	Workspace          string `json:"workspace"`
	Runner             string `json:"runner"`
	AdmissionID        string `json:"admissionId"`
	GitHubRepositoryID int64  `json:"githubRepositoryId"`
	GitHubRunnerID     int64  `json:"githubRunnerId"`
	RunnerName         string `json:"runnerName"`
	MaximumSeconds     int64  `json:"maximumSeconds"`
	ExpiresAt          int64  `json:"expiresAt"`
}

const (
	ciAdmissionCapabilityMaxBytes = 4096
	ciAdmissionCapabilityTTL      = 900 * time.Second
	ciAdmissionMACDomain          = "canter-ci-admission-v1."
)

func ciAdmissionScopeIDValid(s string) bool {
	return s != "" && len(s) <= 128 && utf8.ValidString(s) &&
		!strings.ContainsAny(s, "\x00\r\n\t ")
}

func (c ciAdmissionCapability) claimsValid() bool {
	return c.Version == 1 &&
		ciAdmissionScopeIDValid(c.Workspace) && ciAdmissionScopeIDValid(c.Runner) &&
		ciAdmissionID.MatchString(c.AdmissionID) &&
		c.GitHubRepositoryID > 0 && c.GitHubRunnerID > 0 &&
		ciSourceRunnerName.MatchString(c.RunnerName) &&
		c.MaximumSeconds >= 1 && c.MaximumSeconds <= ciMaxRuntimeSeconds &&
		c.ExpiresAt > 0
}

// mintCIAdmissionCapability signs claims into the v1.<payload>.<mac> wire
// token. The expiry is bounded by now+900s and the pinned setup deadline.
func mintCIAdmissionCapability(key []byte, claims ciAdmissionCapability, now time.Time, setupDeadline time.Time) (string, error) {
	if len(key) < 32 {
		return "", errors.New("ci admission signing key too short")
	}
	if !claims.claimsValid() {
		return "", errors.New("ci admission capability claims invalid")
	}
	expiry := time.Unix(claims.ExpiresAt, 0)
	cap := now.Add(ciAdmissionCapabilityTTL)
	if setupDeadline.Before(cap) {
		cap = setupDeadline
	}
	if expiry.After(cap) {
		claims.ExpiresAt = cap.Unix()
	}
	if claims.ExpiresAt <= now.Unix() || int64(claims.ExpiresAt) > now.Unix()+int64(ciAdmissionCapabilityTTL/time.Second) ||
		(time.Unix(claims.ExpiresAt, 0).After(setupDeadline)) {
		return "", errors.New("ci admission capability expiry out of bounds")
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciAdmissionMACDomain + payload))
	tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	token := "v1." + payload + "." + tag
	if len(token) > ciAdmissionCapabilityMaxBytes {
		return "", errors.New("ci admission capability too large")
	}
	return token, nil
}

// parseCIAdmissionCapability verifies the MAC before any claim parsing and
// enforces the strict nine-key schema, types, and ranges.
func parseCIAdmissionCapability(key []byte, token string, now time.Time) (ciAdmissionCapability, error) {
	var claims ciAdmissionCapability
	if len(key) < 32 || token == "" || len(token) > ciAdmissionCapabilityMaxBytes {
		return claims, errors.New("invalid ci admission capability")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] == "" || parts[2] == "" {
		return claims, errors.New("invalid ci admission capability")
	}
	macRaw, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(macRaw) != sha256.Size {
		return claims, errors.New("invalid ci admission capability")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciAdmissionMACDomain + parts[1]))
	if !hmac.Equal(macRaw, mac.Sum(nil)) {
		return claims, errors.New("invalid ci admission capability")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > ciAdmissionCapabilityMaxBytes {
		return claims, errors.New("invalid ci admission capability")
	}
	dec := newJSONStrictDecoder(payload)
	if err := ciStrictJSONValue(dec); err != nil {
		return claims, errors.New("invalid ci admission capability")
	}
	if _, err := dec.Token(); err != io.EOF {
		return claims, errors.New("invalid ci admission capability")
	}
	dec2 := newJSONStrictDecoder(payload)
	dec2.DisallowUnknownFields()
	if err := dec2.Decode(&claims); err != nil {
		return claims, errors.New("invalid ci admission capability")
	}
	if !claims.claimsValid() {
		return claims, errors.New("invalid ci admission capability")
	}
	if claims.ExpiresAt <= now.Unix() || claims.ExpiresAt > now.Unix()+int64(ciAdmissionCapabilityTTL/time.Second) {
		return claims, errors.New("invalid ci admission capability")
	}
	return claims, nil
}

func newJSONStrictDecoder(raw []byte) *json.Decoder {
	return json.NewDecoder(bytes.NewReader(raw))
}

// ciAdmissionDenyReceipt is the exact ten-key denial body: the nine request
// echoes plus decision=deny and NO deadline, which the legacy receipt
// validator rejects if serialized as zero.
type ciAdmissionDenyReceipt struct {
	ciAdmissionRequest
	Decision string `json:"decision"`
}

func (h *HTTPServer) ciAdmissionConfigured() bool {
	cfg := h.config.CI.Admission
	if cfg == nil || len(cfg.SigningKey) < 32 || len(cfg.EnforcerToken) < 32 ||
		string(cfg.SigningKey) == string(cfg.EnforcerToken) {
		return false
	}
	if _, err := newCIEnforcerClient(cfg.EnforcerURL, cfg.EnforcerToken, cfg.HTTPClient, cfg.AllowLoopbackHTTP); err != nil {
		return false
	}
	return true
}

func (h *HTTPServer) ciAdmissionEnforcer() (*ciEnforcerClient, error) {
	cfg := h.config.CI.Admission
	return newCIEnforcerClient(cfg.EnforcerURL, cfg.EnforcerToken, cfg.HTTPClient, cfg.AllowLoopbackHTTP)
}

func ciAdmissionBearer(r *http.Request) (string, bool) {
	values, ok := r.Header["Authorization"]
	if !ok || len(values) != 1 {
		return "", false
	}
	v := values[0]
	if !strings.HasPrefix(v, "Bearer ") || len(v) <= len("Bearer ") || len(v) > ciAdmissionCapabilityMaxBytes+8 {
		return "", false
	}
	return v[len("Bearer "):], true
}

func ciAdmissionWriteReceipt(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ciAdmissionPending(w http.ResponseWriter) {
	ciAdmissionWriteReceipt(w, http.StatusAccepted, map[string]bool{"pending": true})
}

func ciAdmissionUnavailable(w http.ResponseWriter) {
	ciAdmissionWriteReceipt(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
}

func ciAdmissionUnauthorized(w http.ResponseWriter) {
	ciAdmissionWriteReceipt(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func ciAdmissionDeny(w http.ResponseWriter, req ciAdmissionRequest) {
	ciAdmissionWriteReceipt(w, http.StatusForbidden, ciAdmissionDenyReceipt{ciAdmissionRequest: req, Decision: "deny"})
}

// ciRunnerAdmissionHTTP serves POST /v1/ci/runner-admissions. It is reachable
// only when CI.Admission is fully configured; otherwise the route is absent.
func (h *HTTPServer) ciRunnerAdmissionHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := h.service.Store
	cfg := h.config.CI.Admission

	// Authenticate the capability before touching the request body: an
	// invalid credential must cost zero body, source, or enforcer work.
	token, ok := ciAdmissionBearer(r)
	if !ok {
		ciAdmissionUnauthorized(w)
		return
	}
	now := store.now()
	claims, err := parseCIAdmissionCapability(cfg.SigningKey, token, now)
	if err != nil {
		ciAdmissionUnauthorized(w)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, ciAdmissionRequestMaxBytes+1))
	if err != nil {
		ciAdmissionWriteReceipt(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}
	if len(raw) > ciAdmissionRequestMaxBytes {
		ciAdmissionWriteReceipt(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
		return
	}
	req, err := decodeCIAdmissionRequest(raw)
	if err != nil {
		ciAdmissionWriteReceipt(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}
	// Claims must bind this exact request and select the durable identity.
	if claims.AdmissionID != req.AdmissionID || claims.GitHubRepositoryID != req.GitHubRepositoryID ||
		claims.GitHubRunnerID != req.GitHubRunnerID || claims.RunnerName != req.RunnerName {
		ciAdmissionUnauthorized(w)
		return
	}
	workspace, runner := claims.Workspace, claims.Runner

	snap, err := store.loadCIRunnerLifecycle(ctx, workspace, runner)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			ciAdmissionDeny(w, req)
		} else {
			ciAdmissionUnavailable(w)
		}
		return
	}
	// Trusted one-shot fence plus lifecycle guards before any source work.
	if snap.CreateAttemptedAt == nil || snap.StopRequestedAt != nil || snap.CleanupVerifiedAt != nil ||
		snap.ExecutionStoppedAt != nil || (snap.Phase != "idle" && snap.Phase != "running" && snap.Phase != "registering") ||
		snap.Plan.ServerName != claims.RunnerName || !snap.Plan.HardDeadlineAt.After(now) ||
		snap.GitHubInstallationID == nil || *snap.GitHubInstallationID <= 0 {
		ciAdmissionDeny(w, req)
		return
	}
	// Token expiry must fit the pinned setup deadline; an uncommitted first
	// request must arrive before setup closes.
	if claims.ExpiresAt > snap.Plan.SetupDeadlineAt.Unix() {
		ciAdmissionDeny(w, req)
		return
	}
	committed := len(snap.Decision) != 0
	if !committed && !snap.Plan.SetupDeadlineAt.After(now) {
		ciAdmissionDeny(w, req)
		return
	}
	scope, err := store.ciRunnerAdmissionScope(ctx, workspace, runner, req.AdmissionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			ciAdmissionDeny(w, req)
		} else {
			ciAdmissionUnavailable(w)
		}
		return
	}
	if scope.RepositoryID != claims.GitHubRepositoryID || scope.RunnerID != claims.GitHubRunnerID ||
		scope.RunnerName != claims.RunnerName {
		ciAdmissionDeny(w, req)
		return
	}
	spec := ciEnforcerSpec{
		Plan:                 snap.Plan,
		GitHubInstallationID: *snap.GitHubInstallationID,
		GitHubRepositoryID:   scope.RepositoryID,
		SizeID:               scope.SizeID,
	}
	planDigest, specDigest := spec.digests()

	enforcer, err := h.ciAdmissionEnforcer()
	if err != nil {
		ciAdmissionUnavailable(w)
		return
	}
	st, err := enforcer.Status(ctx, snap.Plan)
	if err != nil {
		if errors.Is(err, errCIEnforcerScope) {
			ciAdmissionDeny(w, req)
		} else {
			ciAdmissionUnavailable(w)
		}
		return
	}
	// Authenticated daemon status must prove an active, fully settled
	// registration whose full spec digest matches the DB-pinned identity.
	knownServer := false
	for _, res := range st.Resources {
		if res.Kind == "server" {
			knownServer = true
		}
	}
	if st.Settled || st.StopRequested || !st.CreateSettled || !st.RegistrationSettled ||
		!st.RegistrationAttempted ||
		st.GitHubRunnerID != claims.GitHubRunnerID || st.GitHubRepositoryID != claims.GitHubRepositoryID ||
		st.RunnerName != claims.RunnerName || st.PlanDigest != planDigest || st.SpecDigest != specDigest ||
		st.DeadlineUnixMs <= now.UnixMilli() || !knownServer {
		ciAdmissionDeny(w, req)
		return
	}

	// Persist the authenticated daemon inventory in Postgres BEFORE any
	// source proof or clock mutation, under the same advisory-948 lock the
	// lifecycle worker uses. No network I/O while locked.
	if err := h.ciAdmissionRetainInventory(ctx, workspace, runner, snap, st); err != nil {
		switch {
		case errors.Is(err, errCIRunnerJobPending):
			ciAdmissionPending(w)
		case errors.Is(err, ErrConflict):
			ciAdmissionDeny(w, req)
		default:
			ciAdmissionUnavailable(w)
		}
		return
	}

	gh, err := h.ciGitHub()
	if err != nil {
		ciAdmissionUnavailable(w)
		return
	}
	receipt, err := gh.admitRunnerJobBefore(ctx, store, workspace, runner, req, claims.MaximumSeconds, snap.Plan.HardDeadlineAt)
	if err != nil {
		if errors.Is(err, errCIRunnerJobPending) {
			ciAdmissionPending(w)
			return
		}
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			ciAdmissionDeny(w, req)
			return
		}
		ciAdmissionUnavailable(w)
		return
	}
	// The committed quote deadline must fit both the plan hard deadline and
	// the persisted execution start plus the capability's immutable cap.
	snapAfter, lerr := store.loadCIRunnerLifecycle(ctx, workspace, runner)
	if lerr != nil {
		ciAdmissionUnavailable(w)
		return
	}
	if snapAfter.ExecutionStartedAt == nil ||
		receipt.ExecutionDeadlineUnixMs > snap.Plan.HardDeadlineAt.UnixMilli() ||
		receipt.ExecutionDeadlineUnixMs > snapAfter.ExecutionStartedAt.Add(time.Duration(claims.MaximumSeconds)*time.Second).UnixMilli() {
		ciAdmissionDeny(w, req)
		return
	}
	// External enforcer approval of the EXACT committed deadline precedes
	// any allow; the typed client revalidates the acknowledgement identity.
	approved, err := enforcer.Approve(ctx, snap.Plan, receipt.ExecutionDeadlineUnixMs)
	if err != nil {
		ciAdmissionUnavailable(w)
		return
	}
	if !approved.RegistrationAttempted || approved.GitHubRunnerID != claims.GitHubRunnerID ||
		approved.GitHubRepositoryID != claims.GitHubRepositoryID || approved.RunnerName != claims.RunnerName ||
		approved.SpecDigest != specDigest || approved.PlanDigest != planDigest {
		ciAdmissionDeny(w, req)
		return
	}
	// Revalidate the durable admission scope after approval and before
	// delivery: a cancellation committed during Approve must deny.
	receipt2, err := store.admitCIRunnerJob(ctx, workspace, runner, req, nil, claims.MaximumSeconds)
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			ciAdmissionDeny(w, req)
		} else {
			ciAdmissionUnavailable(w)
		}
		return
	}
	if receipt2 != receipt {
		ciAdmissionDeny(w, req)
		return
	}
	snapFinal, ferr := store.loadCIRunnerLifecycle(ctx, workspace, runner)
	if ferr != nil {
		ciAdmissionUnavailable(w)
		return
	}
	if snapFinal.ExecutionStoppedAt != nil || snapFinal.StopRequestedAt != nil ||
		snapFinal.CleanupVerifiedAt != nil || snapFinal.Phase != "running" ||
		snapFinal.ExecutionStartedAt == nil ||
		!ciRunnerLifecyclePlanEqual(snapFinal.Plan, snap.Plan) ||
		claims.ExpiresAt <= store.now().Unix() ||
		!time.UnixMilli(receipt.ExecutionDeadlineUnixMs).After(store.now()) {
		ciAdmissionDeny(w, req)
		return
	}
	// A managed run also returns the evidence capability as a response
	// header only; the receipt JSON is unchanged. Mint failure denies the
	// allow with a fixed 503 — the committed receipt is retried identically.
	if h.config.CI.Managed != nil && h.config.CI.Managed.mode == CIManagedRun {
		token, err := h.ciEvidenceAdmissionToken(workspace, runner, req, receipt, snapFinal)
		if err != nil {
			ciAdmissionUnavailable(w)
			return
		}
		w.Header().Set(ciEvidenceHeader, token)
	}
	ciAdmissionWriteReceipt(w, http.StatusOK, receipt)
}

// ciAdmissionRetainInventory persists the union of the CP-pinned resources
// and the authenticated daemon inventory under advisory lock 948, reloading
// the lifecycle inside the lock and refusing to proceed on drift. A busy
// lock reports errCIRunnerJobPending (202); durable conflicts report
// ErrConflict (403); anything else is transient (503). Known resource IDs
// are never discarded: an invalid entry fails the whole admission.
func (h *HTTPServer) ciAdmissionRetainInventory(ctx context.Context, workspace, runner string, snap ciRunnerLifecycleSnapshot, st ciEnforcerStatus) error {
	store := h.service.Store
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := "ci/" + workspace + "/" + runner
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,948))`, key).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return errCIRunnerJobPending
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1,948))`, key).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Hijack().Close(unlockCtx)
		}
	}()
	// Use the already-held connection for every query while the advisory
	// lock is held: a second pool acquisition could deadlock at MaxConns=1.
	fresh, err := store.loadCIRunnerLifecycleWith(ctx, conn, workspace, runner)
	if err != nil {
		return err
	}
	if !ciRunnerLifecyclePlanEqual(fresh.Plan, snap.Plan) {
		return ErrConflict
	}
	if fresh.CreateAttemptedAt == nil ||
		(fresh.Phase != "idle" && fresh.Phase != "running" && fresh.Phase != "registering") ||
		fresh.StopRequestedAt != nil || fresh.CleanupVerifiedAt != nil || fresh.ExecutionStoppedAt != nil {
		return ErrConflict
	}
	seen := map[string]bool{}
	combined := make([]ciOwnedResource, 0, len(fresh.Resources)+len(st.Resources))
	for _, list := range [][]ciOwnedResource{fresh.Resources, st.Resources} {
		for _, res := range list {
			if !ciResourceValid(res, fresh.Plan.OperationID) {
				return ErrConflict
			}
			k := ciResourceKey(res)
			if seen[k] {
				continue
			}
			seen[k] = true
			combined = append(combined, res)
		}
	}
	if len(combined) == 0 {
		return ErrConflict
	}
	return store.ciLifecycleSaveEvidenceWith(ctx, conn, workspace, runner, fresh, combined, st.CreateSettled, false)
}
