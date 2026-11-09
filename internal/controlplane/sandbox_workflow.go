package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

// Sandbox workflow contract: async jobs with cwd/env, resumable logs, chunked
// uploads, streamed downloads, resource metrics and paid idle/labels. Public
// REST input only reaches guest JobRequest.Env through the validated copy
// below — the untrusted Sandbox flag is set by the control plane itself.

type SandboxExecInput struct {
	Command        string            `json:"command"`
	Directory      string            `json:"directory,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

var sandboxEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

var errSandboxWorkflowUpgrade = fmt.Errorf("%w: sleep and wake this sandbox to enable these tools", ErrConflict)

func requireSandboxWorkflow(ctx context.Context, client sandboxAgentClient) error {
	capabilities, err := client.Probe(ctx)
	if err != nil {
		return err
	}
	if capabilities.WorkflowProtocol < 1 {
		return errSandboxWorkflowUpgrade
	}
	return nil
}

func (h *HTTPServer) sandboxWorkflowClient(ctx context.Context, v Sandbox) (sandboxAgentClient, error) {
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		return nil, err
	}
	if err = requireSandboxWorkflow(ctx, client); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func sandboxMutationAllowed(v Sandbox) error {
	if v.Phase != "ready" || v.SleepRequested || v.DeleteRequested {
		return fmt.Errorf("%w: sandbox must be ready and not suspending or deleting", ErrConflict)
	}
	return nil
}

// Explicit user I/O is activity; passive metrics/status polling is not.
// Keep long transfers active until their request ends, including completion
// copying a large staged upload. The free monthly cap still overrides activity.
func (h *HTTPServer) sandboxActivity(ctx context.Context, id string) func() {
	h.service.Store.touchSandbox(ctx, id)
	active, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				h.service.Store.touchSandbox(active, id)
			}
		}
	}()
	return cancel
}

func validateSandboxEnv(env map[string]string) error {
	if len(env) > 64 {
		return fmt.Errorf("%w: at most 64 environment entries", ErrConflict)
	}
	total := 0
	for key, value := range env {
		if !sandboxEnvKey.MatchString(key) {
			return fmt.Errorf("%w: invalid environment variable name %q", ErrConflict, key)
		}
		if strings.ContainsRune(value, 0) || len(value) > 8192 {
			return fmt.Errorf("%w: invalid environment value for %q", ErrConflict, key)
		}
		total += len(key) + len(value)
	}
	if total > 32768 {
		return fmt.Errorf("%w: environment exceeds its aggregate limit", ErrConflict)
	}
	return nil
}

// sandboxJobKnown reports whether the guest job belongs to this sandbox's
// durable job table, so internal checkpoint jobs stay unreachable by ID.
func (s *Store) sandboxJobKnown(ctx context.Context, sandboxID, jobID string) bool {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM sandbox_jobs WHERE sandbox_id=$1 AND job_id=$2`, sandboxID, jobID).Scan(&one)
	return err == nil
}

// sandboxJobStart starts a durable sandbox job without waiting for completion.
func (h *HTTPServer) sandboxJobStart(ctx context.Context, v Sandbox, input SandboxExecInput, actor sdk.ActorRef) (wrencomputer.Job, error) {
	return h.startSandboxJob(ctx, v, input, actor, 3600, true)
}

func (h *HTTPServer) startSandboxJob(ctx context.Context, v Sandbox, input SandboxExecInput, actor sdk.ActorRef, defaultTimeout int, requireWorkflow bool) (wrencomputer.Job, error) {
	if err := sandboxMutationAllowed(v); err != nil {
		return wrencomputer.Job{}, err
	}
	if strings.TrimSpace(input.Command) == "" || len(input.Command) > 64000 {
		return wrencomputer.Job{}, fmt.Errorf("%w: command is required", ErrConflict)
	}
	timeout := input.TimeoutSeconds
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 || timeout > 86400 {
		return wrencomputer.Job{}, fmt.Errorf("%w: timeoutSeconds must be 1-86400", ErrConflict)
	}
	if err := validateSandboxEnv(input.Env); err != nil {
		return wrencomputer.Job{}, err
	}
	directory, err := sandboxFilePath(input.Directory)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	defer client.Close()
	capabilities, err := client.Probe(ctx)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	workflow := capabilities.WorkflowProtocol >= 1
	if !workflow && (requireWorkflow || len(input.Env) > 0 || strings.TrimSpace(input.Directory) != "" || timeout > 600) {
		return wrencomputer.Job{}, errSandboxWorkflowUpgrade
	}
	jobID, err := newID("job_")
	if err != nil {
		return wrencomputer.Job{}, err
	}
	if err := h.service.Store.recordSandboxJob(ctx, v.ID, jobID); err != nil {
		return wrencomputer.Job{}, err
	}
	defer h.sandboxActivity(ctx, v.ID)()
	job, err := client.StartJob(ctx, wrencomputer.JobRequest{
		ID: jobID, Command: input.Command, Directory: directory,
		TimeoutSeconds: timeout, Env: input.Env, Sandbox: workflow,
	})
	if err != nil {
		var remote *wrencomputer.Error
		var rejected *sandboxMutationRejected
		if errors.As(err, &remote) && (strings.HasPrefix(remote.Code, "invalid_") || remote.Code == "forbidden" || remote.Code == "conflict") {
			h.service.Store.finishSandboxJob(ctx, v.ID, jobID)
			return wrencomputer.Job{}, err
		}
		if errors.As(err, &rejected) {
			// Preflight rejected the claim — the job definitely never launched.
			h.service.Store.finishSandboxJob(ctx, v.ID, jobID)
			return wrencomputer.Job{}, rejected.Unwrap()
		}
		// Keep the row and expose its handle even when an RPC result is lost.
		return wrencomputer.Job{ID: jobID, Status: "unknown"}, fmt.Errorf("could not confirm whether job %s started; inspect this job before retrying: %w", jobID, err)
	}
	if job.ID != jobID {
		return wrencomputer.Job{ID: jobID, Status: "unknown"}, fmt.Errorf("unexpected job receipt; inspect job %s before retrying", jobID)
	}
	if sandboxJobDone(job.Status) {
		h.service.Store.finishSandboxJob(ctx, v.ID, jobID)
	}
	action := "sandbox.job_started"
	if !requireWorkflow {
		action = "sandbox.command"
	}
	_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, action, v.ID, map[string]any{"jobId": job.ID})
	return job, nil
}

// workflowJobView is the public job receipt: never env, never request fields.
func workflowJobView(job wrencomputer.Job) map[string]any {
	out := map[string]any{"jobId": job.ID, "status": job.Status, "updatedAt": job.UpdatedAt}
	if job.ExitCode != nil {
		out["exitCode"] = *job.ExitCode
	}
	if job.Error != "" {
		out["error"] = job.Error
	}
	if job.OutputTruncated {
		out["logTruncated"] = true
	}
	return out
}

// sandboxWorkflows serves /sandboxes/{id}/{jobs,metrics,uploads,files} routes.
func (h *HTTPServer) sandboxWorkflows(w http.ResponseWriter, r *http.Request, v Sandbox, p Principal, write bool, parts []string) {
	switch {
	// POST /sandboxes/{id}/jobs — async start, 202 + job view.
	case len(parts) == 2 && parts[1] == "jobs" && r.Method == http.MethodPost:
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		if v.SleepRequested || v.DeleteRequested {
			writeStoreError(w, fmt.Errorf("%w: sandbox is suspending; wait for it to finish", ErrConflict))
			return
		}
		var input SandboxExecInput
		if !decodeLimit(w, r, &input, 96<<10) {
			return
		}
		job, err := h.sandboxJobStart(r.Context(), v, input, p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, workflowJobView(job))

	// GET /sandboxes/{id}/jobs/{job} — current status view.
	case len(parts) == 3 && parts[1] == "jobs" && r.Method == http.MethodGet:
		h.sandboxJobStatus(w, r, v, parts[2])

	// GET /sandboxes/{id}/jobs/{job}/logs — resumable byte chunks.
	case len(parts) == 4 && parts[1] == "jobs" && parts[3] == "logs" && r.Method == http.MethodGet:
		h.sandboxJobLogs(w, r, v, parts[2])

	// GET /sandboxes/{id}/jobs/{job}/events — resumable SSE feed.
	case len(parts) == 4 && parts[1] == "jobs" && parts[3] == "events" && r.Method == http.MethodGet:
		h.sandboxJobEvents(w, r, v, p, parts[2])

	// POST /sandboxes/{id}/jobs/{job}/stop — cancel, return current receipt.
	case len(parts) == 4 && parts[1] == "jobs" && parts[3] == "stop" && r.Method == http.MethodPost:
		if !write {
			writeStoreError(w, ErrForbidden)
			return
		}
		job, err := h.stopSandboxJob(r.Context(), v, parts[2], p.Actor)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workflowJobView(job))

	// GET /sandboxes/{id}/metrics — live resource snapshot, read-authorized.
	case len(parts) == 2 && parts[1] == "metrics" && r.Method == http.MethodGet:
		client, err := h.sandboxClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		if err := requireSandboxWorkflow(r.Context(), client); err != nil {
			writeStoreError(w, err)
			return
		}
		metrics, err := client.Metrics(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, metrics)

	// Upload lifecycle.
	case len(parts) >= 2 && parts[1] == "uploads":
		if write && (v.SleepRequested || v.DeleteRequested) {
			writeStoreError(w, fmt.Errorf("%w: sandbox is suspending; wait for it to finish", ErrConflict))
			return
		}
		h.sandboxUploads(w, r, v, p.Actor, write, parts[1:])

	default:
		writeStoreError(w, ErrNotFound)
	}
}

func (h *HTTPServer) sandboxJobLogs(w http.ResponseWriter, r *http.Request, v Sandbox, jobID string) {
	if !h.service.Store.sandboxJobKnown(r.Context(), v.ID, jobID) {
		writeStoreError(w, ErrNotFound)
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "combined"
	}
	if stream != "combined" && stream != "stdout" && stream != "stderr" {
		writeStoreError(w, fmt.Errorf("%w: invalid stream", ErrConflict))
		return
	}
	offset, limit := int64(0), 65536
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err != nil || n < 0 {
			writeStoreError(w, fmt.Errorf("%w: invalid offset", ErrConflict))
			return
		} else {
			offset = n
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 262144 {
			writeStoreError(w, fmt.Errorf("%w: invalid limit", ErrConflict))
			return
		} else {
			limit = n
		}
	}
	client, err := h.sandboxClient(r.Context(), v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer client.Close()
	if err := requireSandboxWorkflow(r.Context(), client); err != nil {
		writeStoreError(w, err)
		return
	}
	chunk, err := client.JobLogs(r.Context(), jobID, stream, offset, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := validateSandboxLogChunk(chunk, jobID, stream, offset, limit); err != nil {
		writeStoreError(w, err)
		return
	}
	if sandboxJobDone(chunk.Status) {
		h.service.Store.finishSandboxJob(r.Context(), v.ID, jobID)
	}
	writeJSON(w, http.StatusOK, chunk)
}

func validateSandboxLogChunk(chunk wrencomputer.JobLogChunk, jobID, stream string, offset int64, limit int) error {
	if chunk.JobID != jobID || chunk.Stream != stream || chunk.Offset != offset ||
		len(chunk.Data) > limit || chunk.NextOffset < offset ||
		chunk.NextOffset-offset != int64(len(chunk.Data)) || chunk.Status == "" || (len(chunk.Data) == 0 && !chunk.EOF) {
		return fmt.Errorf("invalid sandbox log receipt")
	}
	return nil
}

func sandboxJobDone(status string) bool {
	return status != "starting" && status != "running"
}

func (h *HTTPServer) stopSandboxJob(ctx context.Context, v Sandbox, jobID string, actor sdk.ActorRef) (wrencomputer.Job, error) {
	if !h.service.Store.sandboxJobKnown(ctx, v.ID, jobID) {
		return wrencomputer.Job{}, ErrNotFound
	}
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	defer client.Close()
	defer h.sandboxActivity(ctx, v.ID)()
	job, err := client.StopJob(ctx, jobID)
	if err != nil {
		return wrencomputer.Job{}, err
	}
	if sandboxJobDone(job.Status) {
		h.service.Store.finishSandboxJob(ctx, v.ID, jobID)
	}
	_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, "sandbox.job_stopped", v.ID, map[string]any{"jobId": jobID})
	return job, nil
}

// sandboxReadStreamContext validates the initial request, then monitors every
// second: fresh principal + membership + current sandbox phase/generation.
// A revoked session or changed sandbox cancels the guest RPC itself, not just
// the response after it returns.
func (h *HTTPServer) sandboxReadStreamContext(r *http.Request, v Sandbox) (context.Context, context.CancelFunc, error) {
	check := func(ctx context.Context) error {
		bounded, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		request := r.WithContext(bounded)
		fresh, err := h.principal(request)
		if err != nil {
			return err
		}
		if err = h.allowWorkspace(request, fresh, v.WorkspaceID, false); err != nil {
			return err
		}
		current, err := h.service.Store.Sandbox(bounded, v.WorkspaceID, v.ID)
		if err != nil {
			return err
		}
		ready := current.Phase == "ready" && !current.SleepRequested
		failed := current.Phase == "save_failed" && current.Address != ""
		if (!ready && !failed) || current.DeleteRequested ||
			current.Generation != v.Generation || current.Address != v.Address || current.HostFingerprint != v.HostFingerprint {
			return fmt.Errorf("%w: this sandbox is no longer available", ErrConflict)
		}
		return nil
	}
	if err := check(r.Context()); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if check(ctx) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel, nil
}

func sandboxStreamWrite(w http.ResponseWriter, data []byte, flush bool) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if n, err := w.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if flush {
		return controller.Flush()
	}
	return nil
}

// sandboxJobEvents is a bounded, resumable SSE feed over recorded log bytes.
func (h *HTTPServer) sandboxJobEvents(w http.ResponseWriter, r *http.Request, v Sandbox, _ Principal, jobID string) {
	if !h.service.Store.sandboxJobKnown(r.Context(), v.ID, jobID) {
		writeStoreError(w, ErrNotFound)
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "combined"
	}
	if stream != "combined" && stream != "stdout" && stream != "stderr" {
		writeStoreError(w, fmt.Errorf("%w: invalid stream", ErrConflict))
		return
	}
	offset := int64(0)
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err != nil || n < 0 {
			writeStoreError(w, fmt.Errorf("%w: invalid offset", ErrConflict))
			return
		} else {
			offset = n
		}
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			writeStoreError(w, fmt.Errorf("%w: invalid Last-Event-ID", ErrConflict))
			return
		}
		offset = n
	}
	authCtx, stopAuth, err := h.sandboxReadStreamContext(r, v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer stopAuth()
	ctx, cancel := context.WithTimeout(authCtx, 25*time.Second)
	defer cancel()
	client, err := h.sandboxClient(ctx, v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer client.Close()
	if err := requireSandboxWorkflow(ctx, client); err != nil {
		writeStoreError(w, err)
		return
	}
	first, err := client.JobLogs(ctx, jobID, stream, offset, 65536)
	if err == nil {
		err = validateSandboxLogChunk(first, jobID, stream, offset, 65536)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	send := func(event string, payload any, id int64) bool {
		raw, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		var frame strings.Builder
		if id >= 0 {
			fmt.Fprintf(&frame, "id: %d\n", id)
		}
		fmt.Fprintf(&frame, "event: %s\ndata: %s\n\n", event, raw)
		return ctx.Err() == nil && sandboxStreamWrite(w, []byte(frame.String()), true) == nil
	}
	status := ""
	handle := func(chunk wrencomputer.JobLogChunk) bool {
		view := workflowJobView(wrencomputer.Job{ID: jobID, Status: chunk.Status, ExitCode: chunk.ExitCode, OutputTruncated: chunk.LogTruncated})
		if !sandboxJobDone(chunk.Status) && status != chunk.Status {
			if !send("status", view, -1) {
				return false
			}
			status = chunk.Status
		}
		if len(chunk.Data) > 0 {
			if !send("output", chunk, chunk.NextOffset) {
				return false
			}
		}
		if sandboxJobDone(chunk.Status) && chunk.EOF {
			h.service.Store.finishSandboxJob(ctx, v.ID, jobID)
			send("status", view, -1)
			return false
		}
		return true
	}
	if !handle(first) {
		return
	}
	// Poll in a separate cancellable task so a slow guest cannot suppress
	// heartbeats or fresh authorization checks.
	type result struct {
		chunk wrencomputer.JobLogChunk
		err   error
	}
	results := make(chan result)
	done := make(chan struct{})
	go func() {
		defer close(done)
		previous := first
		for {
			if previous.EOF {
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
			chunk, err := client.JobLogs(ctx, jobID, stream, previous.NextOffset, 65536)
			if err == nil {
				err = validateSandboxLogChunk(chunk, jobID, stream, previous.NextOffset, 65536)
			}
			select {
			case <-ctx.Done():
				return
			case results <- result{chunk, err}:
			}
			if err != nil {
				return
			}
			previous = chunk
		}
	}()
	defer func() { cancel(); <-done }()
	heartbeat := time.NewTicker(9 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-results:
			if item.err != nil || !handle(item.chunk) {
				return
			}
		case <-heartbeat.C:
			if err := sandboxStreamWrite(w, []byte(": heartbeat\n\n"), true); err != nil {
				return
			}
		}
	}
}

// sandboxUploads serves POST /uploads, PATCH /uploads/{id}, POST
// /uploads/{id}/complete and DELETE /uploads/{id} over the guest RPC surface.
func (h *HTTPServer) sandboxUploads(w http.ResponseWriter, r *http.Request, v Sandbox, actor sdk.ActorRef, write bool, parts []string) {
	if !write {
		writeStoreError(w, ErrForbidden)
		return
	}
	if err := sandboxMutationAllowed(v); err != nil {
		writeStoreError(w, err)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodPost:
		var input struct {
			Path   string `json:"path"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256,omitempty"`
		}
		if !decodeLimit(w, r, &input, 8192) {
			return
		}
		path, err := sandboxFilePath(input.Path)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		client, err := h.sandboxWorkflowClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		defer h.sandboxActivity(r.Context(), v.ID)()
		uploadID, err := newID("upl_")
		if err != nil {
			writeStoreError(w, err)
			return
		}
		up, err := client.BeginUpload(r.Context(), uploadID, path, input.Size, input.SHA256)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, up)

	case len(parts) == 2 && r.Method == http.MethodPatch:
		offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if err != nil || offset < 0 {
			writeStoreError(w, fmt.Errorf("%w: invalid offset", ErrConflict))
			return
		}
		if r.ContentLength > 4<<20 {
			writeStoreError(w, fmt.Errorf("%w: an upload chunk is limited to 4MiB", ErrConflict))
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if len(data) > 4<<20 {
			writeStoreError(w, fmt.Errorf("%w: an upload chunk is limited to 4MiB", ErrConflict))
			return
		}
		client, err := h.sandboxWorkflowClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		defer h.sandboxActivity(r.Context(), v.ID)()
		up, err := client.UploadChunk(r.Context(), parts[1], offset, data)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, up)

	case len(parts) == 3 && parts[2] == "complete" && r.Method == http.MethodPost:
		client, err := h.sandboxWorkflowClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		defer h.sandboxActivity(r.Context(), v.ID)()
		up, err := client.CompleteUpload(r.Context(), parts[1])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), v.WorkspaceID, actor, "sandbox.file_uploaded", v.ID, map[string]any{"uploadId": parts[1], "path": up.Path, "size": up.Size})
		writeJSON(w, http.StatusOK, up)

	case len(parts) == 2 && r.Method == http.MethodDelete:
		client, err := h.sandboxWorkflowClient(r.Context(), v)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		defer client.Close()
		defer h.sandboxActivity(r.Context(), v.ID)()
		if err := client.AbortUpload(r.Context(), parts[1]); err != nil {
			writeStoreError(w, err)
			return
		}
		_ = h.service.Store.Audit(r.Context(), v.WorkspaceID, actor, "sandbox.upload_aborted", v.ID, map[string]any{"uploadId": parts[1]})
		w.WriteHeader(http.StatusNoContent)

	default:
		writeStoreError(w, ErrNotFound)
	}
}

// sandboxPatch applies the settled PATCH contract: pointer fields only;
// omitted leaves the column unchanged. Audit records field names, not values.
func (h *HTTPServer) sandboxPatch(ctx context.Context, v Sandbox, name *string, labels map[string]string, idle *int, actor sdk.ActorRef) (Sandbox, error) {
	if v.DeleteRequested || v.Phase == "deleting" {
		return Sandbox{}, fmt.Errorf("%w: this sandbox is being deleted", ErrConflict)
	}
	changed := []string{}
	sets := []string{}
	args := []any{v.WorkspaceID, v.ID}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" || len(trimmed) > 80 || strings.ContainsAny(trimmed, "\x00\r\n") {
			return Sandbox{}, fmt.Errorf("%w: sandbox name must be 1-80 characters", ErrConflict)
		}
		sets = append(sets, "name=$"+strconv.Itoa(len(args)+1))
		args = append(args, trimmed)
		changed = append(changed, "name")
	}
	if labels != nil {
		if err := validateSandboxLabels(labels); err != nil {
			return Sandbox{}, err
		}
		raw, _ := json.Marshal(labels)
		sets = append(sets, "labels=$"+strconv.Itoa(len(args)+1))
		args = append(args, raw)
		changed = append(changed, "labels")
	}
	if idle != nil {
		effective, err := effectiveIdle(idle)
		if err != nil {
			return Sandbox{}, err
		}
		if v.Tier == "free" && effective != -1 {
			return Sandbox{}, fmt.Errorf("%w: free sandboxes keep the mandatory idle policy", ErrConflict)
		}
		sets = append(sets, "idle_timeout_seconds=$"+strconv.Itoa(len(args)+1))
		args = append(args, effective)
		changed = append(changed, "idleTimeoutSeconds")
	}
	if len(sets) == 0 {
		return v, nil
	}
	result, err := h.service.Store.pool.Exec(ctx,
		`UPDATE sandboxes SET `+strings.Join(sets, ",")+`,updated_at=now() WHERE workspace_id=$1 AND id=$2 AND phase<>'deleted' AND phase<>'deleting' AND NOT delete_requested`, args...)
	if err != nil {
		return Sandbox{}, err
	}
	if result.RowsAffected() == 0 {
		return Sandbox{}, fmt.Errorf("%w: this sandbox is being deleted", ErrConflict)
	}
	_ = h.service.Store.Audit(ctx, v.WorkspaceID, actor, "sandbox.updated", v.ID, map[string]any{"fields": changed})
	return h.service.Store.Sandbox(ctx, v.WorkspaceID, v.ID)
}

// sandboxFileDownload streams a binary attachment through successive version
// locked <=4MiB guest reads. It never writes JSON errors after bytes flush.
func (h *HTTPServer) sandboxFileDownload(w http.ResponseWriter, r *http.Request, v Sandbox, _ Principal, path string) {
	ctx, cancel, err := h.sandboxReadStreamContext(r, v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer cancel()
	client, err := h.sandboxWorkflowClient(ctx, v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer client.Close()
	defer h.sandboxActivity(ctx, v.ID)()
	first, err := client.ReadFileVersion(ctx, path, 0, 4<<20, "")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := validateSandboxFileChunk(first, 0, first.Size, first.Version); err != nil {
		writeStoreError(w, err)
		return
	}
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.FormatInt(first.Size, 10))
	header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	if ctx.Err() != nil {
		return
	}
	w.WriteHeader(http.StatusOK)
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	if err := sandboxStreamWrite(w, first.Data, false); err != nil {
		return
	}
	offset := first.NextOffset
	for offset < first.Size {
		select {
		case <-ctx.Done():
			return
		default:
		}
		chunk, err := client.ReadFileVersion(ctx, path, offset, 4<<20, first.Version)
		if err != nil {
			return
		}
		if err := validateSandboxFileChunk(chunk, offset, first.Size, first.Version); err != nil || ctx.Err() != nil {
			return
		}
		if err := sandboxStreamWrite(w, chunk.Data, false); err != nil {
			return
		}
		offset = chunk.NextOffset
		if chunk.EOF {
			return
		}
	}
}

func validateSandboxFileChunk(chunk wrencomputer.FileChunk, offset, size int64, version string) error {
	if size < 0 || version == "" || chunk.Version != version || chunk.Size != size ||
		chunk.Offset != offset || chunk.NextOffset < offset || chunk.NextOffset > size ||
		chunk.NextOffset-offset != int64(len(chunk.Data)) || len(chunk.Data) > 4<<20 ||
		chunk.EOF != (chunk.NextOffset == size) || (len(chunk.Data) == 0 && offset < size) ||
		!ComputerUserFileAllowed(chunk.Path) {
		return fmt.Errorf("invalid or changed sandbox file receipt")
	}
	return nil
}

// sandboxJobStatus reports a tracked job's current receipt.
func (h *HTTPServer) sandboxJobStatus(w http.ResponseWriter, r *http.Request, v Sandbox, jobID string) {
	if !h.service.Store.sandboxJobKnown(r.Context(), v.ID, jobID) {
		writeStoreError(w, ErrNotFound)
		return
	}
	client, err := h.sandboxClient(r.Context(), v)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer client.Close()
	job, err := client.Job(r.Context(), jobID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if sandboxJobDone(job.Status) {
		h.service.Store.finishSandboxJob(r.Context(), v.ID, jobID)
	}
	writeJSON(w, http.StatusOK, sandboxJobView(job))
}
