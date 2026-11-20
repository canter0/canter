package controlplane

import (
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
)

// The bootstrap capability is a one-shot, domain-separated HMAC token that
// proves a caller may attempt the durable bootstrap grant claim for one
// pinned lifecycle. It carries metadata only: no JIT material, bearer token,
// provider credential, or operator key is ever placed inside it.
const (
	ciBootstrapCapabilityMaxBytes = 4096
	ciBootstrapCapabilityTTL      = 900 * time.Second
	ciBootstrapMACDomain          = "canter-ci-bootstrap-v1."
	ciBootstrapRequestMaxBytes    = 1024
	ciBootstrapResponseMaxBytes   = 128 << 10
	ciBootstrapHandlerBudget      = 120 * time.Second
)

var ciBootstrapClaimsKeys = []string{
	"version", "workspace", "runner", "operationId", "bootstrapId",
	"githubRepositoryId", "planDigest", "maximumSeconds", "expiresAt",
}

func (c ciBootstrapClaims) claimsValid() bool {
	return c.Version == 1 &&
		ciAdmissionScopeIDValid(c.Workspace) && ciAdmissionScopeIDValid(c.Runner) &&
		ciAdmissionScopeIDValid(c.OperationID) &&
		ciAdmissionID.MatchString(c.BootstrapID) &&
		c.GitHubRepositoryID > 0 &&
		ciProvisioningDigest.MatchString(c.PlanDigest) &&
		c.MaximumSeconds >= 1 && c.MaximumSeconds <= ciMaxRuntimeSeconds &&
		c.ExpiresAt > 0
}

// mintCIBootstrapCapability signs claims into the v1.<payload>.<mac> wire
// token. Unlike the admission capability the expiry is NEVER clamped or
// rewritten: the durable grant fixes it, so a parent that crashes after the
// grant commit but before the create fence can deterministically remint the
// identical token for the same nonce.
func mintCIBootstrapCapability(key []byte, claims ciBootstrapClaims, now time.Time, setupDeadline time.Time) (string, error) {
	if len(key) < 32 {
		return "", errors.New("ci bootstrap signing key too short")
	}
	if !claims.claimsValid() {
		return "", errors.New("ci bootstrap capability claims invalid")
	}
	if claims.ExpiresAt <= now.Unix() ||
		claims.ExpiresAt > now.Unix()+int64(ciBootstrapCapabilityTTL/time.Second) ||
		claims.ExpiresAt > setupDeadline.Unix() {
		return "", errors.New("ci bootstrap capability expiry out of bounds")
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciBootstrapMACDomain + payload))
	tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	token := "v1." + payload + "." + tag
	if len(token) > ciBootstrapCapabilityMaxBytes {
		return "", errors.New("ci bootstrap capability too large")
	}
	return token, nil
}

// parseCIBootstrapCapability verifies the MAC before any claim parsing and
// enforces the strict nine-key lower-camel schema. DisallowUnknownFields
// alone would accept case aliases and case-variant duplicates, so the exact
// key set is checked against the raw object after the strict duplicate walk.
func parseCIBootstrapCapability(key []byte, token string, now time.Time) (ciBootstrapClaims, error) {
	var claims ciBootstrapClaims
	if len(key) < 32 || token == "" || len(token) > ciBootstrapCapabilityMaxBytes {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] == "" || parts[2] == "" {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	macRaw, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(macRaw) != sha256.Size ||
		base64.RawURLEncoding.EncodeToString(macRaw) != parts[2] {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ciBootstrapMACDomain + parts[1]))
	if !hmac.Equal(macRaw, mac.Sum(nil)) {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > ciBootstrapCapabilityMaxBytes ||
		base64.RawURLEncoding.EncodeToString(payload) != parts[1] {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	dec := newJSONStrictDecoder(payload)
	if err := ciStrictJSONValue(dec); err != nil {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	if _, err := dec.Token(); err != io.EOF {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || len(object) != len(ciBootstrapClaimsKeys) {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	for _, k := range ciBootstrapClaimsKeys {
		if _, ok := object[k]; !ok {
			return claims, errors.New("invalid ci bootstrap capability")
		}
	}
	dec2 := newJSONStrictDecoder(payload)
	dec2.DisallowUnknownFields()
	if err := dec2.Decode(&claims); err != nil {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	if !claims.claimsValid() {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	if claims.ExpiresAt <= now.Unix() || claims.ExpiresAt > now.Unix()+int64(ciBootstrapCapabilityTTL/time.Second) {
		return claims, errors.New("invalid ci bootstrap capability")
	}
	return claims, nil
}

// ciBootstrapConfigured reports whether the optional bootstrap endpoint is
// wired: an explicit profile digest plus the fully configured admission
// signing key and enforcer client. Anything less leaves the route absent.
func (h *HTTPServer) ciBootstrapConfigured() bool {
	cfg := h.config.CI.Bootstrap
	return cfg != nil && ciProvisioningDigest.MatchString(cfg.ProfileDigest) && h.ciAdmissionConfigured()
}

func ciBootstrapWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ciBootstrapPending(w http.ResponseWriter) {
	ciBootstrapWriteJSON(w, http.StatusAccepted, map[string]bool{"pending": true})
}

func ciBootstrapBadRequest(w http.ResponseWriter) {
	ciBootstrapWriteJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
}

func ciBootstrapConflict(w http.ResponseWriter) {
	ciBootstrapWriteJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
}

func ciBootstrapUnauthorized(w http.ResponseWriter) {
	ciBootstrapWriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func ciBootstrapUnavailable(w http.ResponseWriter) {
	ciBootstrapWriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
}

// ciBootstrapRequestValid accepts exactly {"version":1}: one lowercase key,
// strict duplicate/null/depth walk, no unknown keys, no trailing data.
func ciBootstrapRequestValid(raw []byte) bool {
	dec := newJSONStrictDecoder(raw)
	if err := ciStrictJSONValue(dec); err != nil {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	// Exactly one lowercase "version" key: a typed struct decode would accept
	// case aliases such as "Version", so the raw key set is checked first.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) != 1 {
		return false
	}
	versionRaw, ok := object["version"]
	if !ok {
		return false
	}
	var version int
	if err := json.Unmarshal(versionRaw, &version); err != nil || version != 1 {
		return false
	}
	return true
}

// ciBootstrapStatusBound applies the additional handler-side binding the
// typed client cannot know: the expected plan/spec digests come from the
// durable provisioning scope, the deadline must be the ORIGINAL setup
// deadline, and every reported resource must be one of the exact resources
// the control-plane creation journal recorded, including the SDK server.
func ciBootstrapStatusBound(st ciEnforcerStatus, scope ciBootstrapScope, plan ciRunnerLifecyclePlan, githubRunnerID int64) bool {
	pd, sd := scope.Spec.digests()
	if st.PlanDigest != pd || st.SpecDigest != sd ||
		st.GitHubRepositoryID != scope.Spec.GitHubRepositoryID ||
		st.RunnerName != plan.ServerName ||
		st.GitHubRunnerID != githubRunnerID || githubRunnerID <= 0 ||
		!st.CreateSettled || !st.RegistrationAttempted || !st.RegistrationSettled ||
		st.StopRequested || st.Settled ||
		st.DeadlineUnixMs != plan.SetupDeadlineAt.UnixMilli() {
		return false
	}
	known := make(map[string]bool, len(scope.Snapshot.Resources))
	for _, res := range scope.Snapshot.Resources {
		known[res.Kind+"/"+res.ID+"/"+res.OperationID] = true
	}
	// The status inventory must itself witness the SDK server resource: an
	// active lease may legally report an empty non-nil inventory, so the
	// snapshot alone cannot prove current enforcer-side existence.
	serverSeen := false
	for _, res := range st.Resources {
		key := res.Kind + "/" + res.ID + "/" + res.OperationID
		if !known[key] {
			return false
		}
		if res.Kind == "server" {
			serverSeen = true
		}
	}
	return serverSeen
}

// ciBootstrapResponse is the exact eleven-field success DTO. It carries the
// ephemeral JIT config and the scoped admission capability by design; it
// never serializes the scope, snapshot, journal, or any provider, operator,
// or GitHub App credential material.
type ciBootstrapResponse struct {
	Version             int    `json:"version"`
	OperationID         string `json:"operationId"`
	AdmissionID         string `json:"admissionId"`
	RunnerName          string `json:"runnerName"`
	GitHubRunnerID      int64  `json:"githubRunnerId"`
	GitHubRepositoryID  int64  `json:"githubRepositoryId"`
	MaximumSeconds      int64  `json:"maximumSeconds"`
	SetupDeadlineUnixMs int64  `json:"setupDeadlineUnixMs"`
	HardDeadlineUnixMs  int64  `json:"hardDeadlineUnixMs"`
	EncodedJitConfig    string `json:"encodedJitConfig"`
	AdmissionCapability string `json:"admissionCapability"`
}

// ciRunnerBootstrapHTTP serves POST /v1/ci/runner-bootstrap. It is reachable
// only when CI.Bootstrap names a valid profile and CI.Admission is fully
// configured; otherwise the route is absent. The grant claim is one-shot: a
// request that loses the claim consumes nothing, while any failure after a
// committed claim leaves the grant used and triggers the fail-closed stop.
func (h *HTTPServer) ciRunnerBootstrapHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ciBootstrapHandlerBudget)
	defer cancel()
	store := h.service.Store
	execution := h.ciExecutionConfig(ctx)
	admission := execution.Admission
	profile := execution.Bootstrap.ProfileDigest

	// Authenticate the capability before touching the request body: an
	// invalid credential must cost zero body, source, or enforcer work.
	token, ok := ciAdmissionBearer(r)
	if !ok {
		ciBootstrapUnauthorized(w)
		return
	}
	claims, err := parseCIBootstrapCapability(admission.SigningKey, token, store.now())
	if err != nil {
		ciBootstrapUnauthorized(w)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		ciBootstrapBadRequest(w)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, ciBootstrapRequestMaxBytes+1))
	if err != nil {
		ciBootstrapBadRequest(w)
		return
	}
	if len(raw) > ciBootstrapRequestMaxBytes {
		ciBootstrapWriteJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
		return
	}
	if !ciBootstrapRequestValid(raw) {
		ciBootstrapBadRequest(w)
		return
	}

	// The enforcer client exists before the claim so the independent stop
	// is available even if the durable commit lands and the next RPC fails.
	enforcer, err := h.ciAdmissionEnforcer(ctx)
	if err != nil {
		ciBootstrapUnavailable(w)
		return
	}
	snap, err := store.loadCIRunnerLifecycle(ctx, claims.Workspace, claims.Runner)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			ciBootstrapConflict(w)
		} else {
			ciBootstrapUnavailable(w)
		}
		return
	}
	plan := snap.Plan
	if claims.OperationID != plan.OperationID || claims.PlanDigest != ciEnforcerPlanDigest(plan) {
		ciBootstrapConflict(w)
		return
	}

	scope, err := store.claimCIBootstrap(ctx, profile, plan, claims)
	if err != nil {
		switch {
		case errors.Is(err, errCIRunnerJobPending):
			ciBootstrapPending(w)
		case errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound):
			ciBootstrapConflict(w)
		default:
			ciBootstrapUnavailable(w)
		}
		return
	}

	// The claim is committed: every failure below must fail closed with a
	// synchronous bounded stop on BOTH the durable lifecycle and the
	// independent enforcer, using fresh contexts that survive request
	// cancellation. The grant is never un-used and no JIT is reissued.
	sent := false
	defer func() {
		if sent {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, _ = store.requestCIRunnerStop(stopCtx, claims.Workspace, claims.Runner, "setup_failed", nil)
		stopCancel()
		enfCtx, enfCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer enfCancel()
		_, _ = enforcer.Stop(enfCtx, plan)
	}()

	jit, err := enforcer.RegisterRunner(ctx, plan)
	if err != nil {
		ciBootstrapUnavailable(w)
		return
	}
	if !ciBootstrapStatusBound(jit.Status, scope, plan, jit.GitHubRunnerID) {
		ciBootstrapUnavailable(w)
		return
	}
	if err := store.registerCIRunnerAdmission(ctx, claims.Workspace, claims.Runner, claims.BootstrapID, ciRunnerJobScope{
		RepositoryID: scope.Spec.GitHubRepositoryID,
		RunnerID:     jit.GitHubRunnerID,
		RunnerName:   plan.ServerName,
		SizeID:       scope.Spec.SizeID,
		CreatedAt:    scope.CreatedAt,
	}); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			ciBootstrapConflict(w)
		} else {
			ciBootstrapUnavailable(w)
		}
		return
	}
	now := store.now()
	admissionClaims := ciAdmissionCapability{
		Version:            1,
		Workspace:          claims.Workspace,
		Runner:             claims.Runner,
		AdmissionID:        claims.BootstrapID,
		GitHubRepositoryID: scope.Spec.GitHubRepositoryID,
		GitHubRunnerID:     jit.GitHubRunnerID,
		RunnerName:         plan.ServerName,
		MaximumSeconds:     claims.MaximumSeconds,
		ExpiresAt:          claims.ExpiresAt,
	}
	admissionToken, err := mintCIAdmissionCapability(admission.SigningKey, admissionClaims, now, plan.SetupDeadlineAt)
	if err != nil {
		ciBootstrapUnavailable(w)
		return
	}
	// Final liveness re-proof with the EXACT same binding before delivery.
	st, err := enforcer.Status(ctx, plan)
	if err != nil {
		ciBootstrapUnavailable(w)
		return
	}
	if !ciBootstrapStatusBound(st, scope, plan, jit.GitHubRunnerID) {
		ciBootstrapUnavailable(w)
		return
	}
	if ctx.Err() != nil {
		ciBootstrapUnavailable(w)
		return
	}
	if err := store.markCIBootstrapDelivered(ctx, profile, plan, claims, jit.GitHubRunnerID); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			ciBootstrapConflict(w)
		} else {
			ciBootstrapUnavailable(w)
		}
		return
	}
	// Post-commit guard: the delivery marker is durable, but a cancellation
	// or deadline crossing committed during the write still fails closed.
	// The fresh clock sample precedes the ctx check so a cancellation that
	// coincides with the read is caught either way. The marker is never
	// rolled back and no JIT is reissued.
	now = store.now()
	if claims.ExpiresAt <= now.Unix() || !plan.SetupDeadlineAt.After(now) || !h.ciExecutionActive(ctx) {
		ciBootstrapUnavailable(w)
		return
	}
	out, err := json.Marshal(ciBootstrapResponse{
		Version:             1,
		OperationID:         plan.OperationID,
		AdmissionID:         claims.BootstrapID,
		RunnerName:          plan.ServerName,
		GitHubRunnerID:      jit.GitHubRunnerID,
		GitHubRepositoryID:  scope.Spec.GitHubRepositoryID,
		MaximumSeconds:      claims.MaximumSeconds,
		SetupDeadlineUnixMs: plan.SetupDeadlineAt.UnixMilli(),
		HardDeadlineUnixMs:  plan.HardDeadlineAt.UnixMilli(),
		EncodedJitConfig:    jit.EncodedJitConfig,
		AdmissionCapability: admissionToken,
	})
	if err != nil || len(out) > ciBootstrapResponseMaxBytes || !h.ciExecutionActive(ctx) {
		ciBootstrapUnavailable(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	n, werr := w.Write(out)
	if werr != nil || n != len(out) {
		return
	}
	sent = true
}
