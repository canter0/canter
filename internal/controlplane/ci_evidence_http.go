package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// ciEvidenceRouteConfigured gates POST /v1/ci/runner-evidence: it exists only
// for a managed run runtime with a fully configured admission endpoint.
// Off, recover, and plain admission configurations leave the route absent.
func (h *HTTPServer) ciEvidenceRouteConfigured() bool {
	return h.config.CI.Managed != nil && h.config.CI.Managed.mode == CIManagedRun &&
		h.ciAdmissionConfigured()
}

func (h *HTTPServer) ciEvidenceAvailable() bool {
	return h.ciEvidenceRouteConfigured() && h.config.CI.Managed.running.Load()
}

// ciEvidenceWriteError emits only the fixed public error bodies; internal
// errors, IDs, tokens, and request material are never echoed.
func ciEvidenceWriteError(w http.ResponseWriter, status int) {
	var message string
	switch status {
	case http.StatusBadRequest:
		message = "invalid CI evidence"
	case http.StatusUnauthorized:
		message = "unauthorized"
	case http.StatusMethodNotAllowed:
		message = "method not allowed"
	case http.StatusConflict:
		message = "conflict"
	case http.StatusTooManyRequests:
		message = "quota exceeded"
	case http.StatusServiceUnavailable:
		message = "unavailable"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if message != "" {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	}
}

// ciRunnerEvidenceHTTP accepts one authenticated metrics/tests batch from a
// managed runner. The capability is verified before method or body work; the
// authoritative scope is loaded before the body is read; every response is a
// fixed status with no internal detail.
func (h *HTTPServer) ciRunnerEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	store := h.service.Store
	cfg := h.config.CI.Admission

	token, ok := ciAdmissionBearer(r)
	if !ok {
		ciEvidenceWriteError(w, http.StatusUnauthorized)
		return
	}
	capability, err := parseCIEvidenceCapability(cfg.SigningKey, token, store.now())
	if err != nil {
		ciEvidenceWriteError(w, http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		ciEvidenceWriteError(w, http.StatusMethodNotAllowed)
		return
	}
	select {
	case h.ciEvidenceSlots <- struct{}{}:
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer func() { <-h.ciEvidenceSlots }()

	// One bounded context for the whole exchange, clipped to the remaining
	// token window so a request can never outlive its capability grace.
	budget := ciEvidenceRequestBound
	if remaining := time.UnixMilli(capability.DeadlineUnixMs).
		Add(ciEvidenceUploadGrace).Sub(store.now()); remaining < budget {
		budget = remaining
	}
	if budget <= 0 {
		ciEvidenceWriteError(w, http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	// Real connection deadlines are required before any body or response
	// work; unsupported writers fail closed.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(budget)); err != nil {
		ciEvidenceWriteError(w, http.StatusServiceUnavailable)
		return
	}
	if err := rc.SetWriteDeadline(time.Now().Add(budget)); err != nil {
		ciEvidenceWriteError(w, http.StatusServiceUnavailable)
		return
	}
	stopWatch := ciCacheDeadlineWatch(w, r.Body, ctx)
	defer stopWatch()

	// Scope authorization precedes the body: an unauthorized capability must
	// cost zero body reads and zero database writes.
	if _, err := store.loadCIEvidenceScope(ctx, capability); err != nil {
		if errors.Is(err, errCIEvidenceUnauthorized) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ciEvidenceWriteError(w, http.StatusServiceUnavailable)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, ciEvidenceRequestMaxBytes+1))
	if err != nil {
		ciEvidenceWriteError(w, http.StatusBadRequest)
		return
	}
	if len(raw) > ciEvidenceRequestMaxBytes {
		ciEvidenceWriteError(w, http.StatusRequestEntityTooLarge)
		return
	}
	if err := store.acceptCIEvidence(ctx, capability, raw); err != nil {
		switch {
		case errors.Is(err, errCIEvidenceUnauthorized):
			w.WriteHeader(http.StatusUnauthorized)
		case errors.Is(err, errCIEvidenceInvalid):
			ciEvidenceWriteError(w, http.StatusBadRequest)
		case errors.Is(err, errCIEvidenceQuota):
			ciEvidenceWriteError(w, http.StatusTooManyRequests)
		case errors.Is(err, ErrConflict):
			ciEvidenceWriteError(w, http.StatusConflict)
		default:
			ciEvidenceWriteError(w, http.StatusServiceUnavailable)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ciEvidenceAdmissionToken mints the evidence capability for a committed
// admission allow. It re-verifies the durable decision against the exact
// request and receipt before signing; any drift fails closed.
func (h *HTTPServer) ciEvidenceAdmissionToken(workspace, runner string, req ciAdmissionRequest, receipt ciAdmissionReceipt, snap ciRunnerLifecycleSnapshot) (string, error) {
	if snap.GitHubInstallationID == nil || *snap.GitHubInstallationID <= 0 || len(snap.Decision) == 0 {
		return "", errCIEvidenceCapabilityInvalid
	}
	var decision ciAdmissionDecision
	if err := ciEnforcerStrictJSON(snap.Decision, &decision); err != nil {
		return "", errCIEvidenceCapabilityInvalid
	}
	if decision.Request != req || decision.Receipt != receipt ||
		decision.Proof.JobID <= 0 || decision.Proof.RunnerID != req.GitHubRunnerID ||
		decision.Proof.RepositoryID != req.GitHubRepositoryID {
		return "", errCIEvidenceCapabilityInvalid
	}
	return mintCIEvidenceCapability(h.config.CI.Admission.SigningKey, ciEvidenceCapability{
		Version:              1,
		Workspace:            workspace,
		Runner:               runner,
		AdmissionID:          req.AdmissionID,
		GitHubInstallationID: *snap.GitHubInstallationID,
		GitHubRepositoryID:   req.GitHubRepositoryID,
		GitHubRunnerID:       req.GitHubRunnerID,
		GitHubJobID:          decision.Proof.JobID,
		DeadlineUnixMs:       receipt.ExecutionDeadlineUnixMs,
	}, h.service.Store.now())
}
