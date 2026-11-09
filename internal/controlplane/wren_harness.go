package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/internal/wrenharness"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type WrenHarnessConfig struct {
	transport                 http.RoundTripper
	BrokerBaseURL             string
	MaxRequests               int
	MaxOutputTokens           int
	MaxAccountRequestsPerHour int
	GrantSeconds              int
}

func (c WrenHarnessConfig) requests() int {
	if c.MaxRequests < 1 {
		return 32
	}
	return min(c.MaxRequests, 200)
}
func (c WrenHarnessConfig) output() int {
	if c.MaxOutputTokens < 1 {
		return 16384
	}
	return min(c.MaxOutputTokens, 16384)
}
func (c WrenHarnessConfig) accountRequests() int {
	if c.MaxAccountRequestsPerHour < 1 {
		return 120
	}
	return min(c.MaxAccountRequestsPerHour, 1000)
}
func (c WrenHarnessConfig) lifetime() time.Duration {
	if c.GrantSeconds < 30 {
		return 30 * time.Minute
	}
	return time.Duration(min(c.GrantSeconds, 3600)) * time.Second
}

// HarnessConfigured advertises only the real API-funded ACP adapter. Consumer
// subscription/partner login integrations are not implied by ACP availability.
func (h *HTTPServer) HarnessConfigured() bool {
	if h.config.Secrets == nil || !h.config.Operator.Ready() {
		return false
	}
	u, err := url.Parse(h.config.Harnesses.BrokerBaseURL)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (h *HTTPServer) CreateHarnessWrenJob(ctx context.Context, parent OperatorRun, origin Conversation, operationKey, title, prompt string, projectIDs ...[]string) (WrenJob, error) {
	if !h.HarnessConfigured() {
		return WrenJob{}, fmt.Errorf("the API-funded coding harness is not configured")
	}
	if err := h.ComputerAgentWriteAllowed(ctx, origin); err != nil {
		return WrenJob{}, err
	}
	_, computer, err := h.ClientForConversation(ctx, origin)
	if err != nil {
		return WrenJob{}, err
	}
	operation, err := h.service.Store.beginComputerOperation(ctx, computer.ID)
	if err != nil {
		return WrenJob{}, err
	}
	defer operation.close()
	ctx = operation.ctx
	if err = h.ComputerAgentWriteAllowed(ctx, origin); err != nil {
		return WrenJob{}, err
	}
	return h.service.Store.CreateWrenJobWithExecutor(ctx, parent, origin, operationKey, title, prompt, h.config.Operator.wrenModel(), "acp-opencode", projectIDs...)
}

type wrenModelGrant struct {
	ID, JobID, ComputerID, WorkspaceID, RequesterID, Model, KeyVersion string
	Ciphertext                                                         []byte
	MaxRequests, UsedRequests, MaxOutputTokens                         int
	ExpiresAt                                                          time.Time
	RevokedAt                                                          *time.Time
}

const wrenModelGrantColumns = `id,job_id,computer_id,workspace_id,COALESCE(requester_account_id,''),model,token_key_version,token_ciphertext,max_requests,used_requests,max_output_tokens,expires_at,revoked_at`

func scanWrenModelGrant(row pgx.Row) (wrenModelGrant, error) {
	var g wrenModelGrant
	err := row.Scan(&g.ID, &g.JobID, &g.ComputerID, &g.WorkspaceID, &g.RequesterID, &g.Model, &g.KeyVersion, &g.Ciphertext, &g.MaxRequests, &g.UsedRequests, &g.MaxOutputTokens, &g.ExpiresAt, &g.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrUnauthorized
	}
	return g, err
}
func (h *HTTPServer) validateHarnessJob(ctx context.Context, jobID string) (WrenJob, WrenComputer, error) {
	s := h.service.Store
	var workspace, requester string
	err := s.pool.QueryRow(ctx, `SELECT workspace_id,COALESCE(requester_account_id,'') FROM wren_jobs WHERE id=$1`, jobID).Scan(&workspace, &requester)
	if errors.Is(err, pgx.ErrNoRows) {
		return WrenJob{}, WrenComputer{}, ErrForbidden
	}
	if err != nil {
		return WrenJob{}, WrenComputer{}, err
	}
	var stopping bool
	if err = s.pool.QueryRow(ctx, `SELECT stop_requested OR status IN ('completed','failed','stopped') FROM wren_harness_executions WHERE job_id=$1`, jobID).Scan(&stopping); err != nil || stopping {
		return WrenJob{}, WrenComputer{}, ErrForbidden
	}
	if requester == "" || !h.config.Computers.allows(workspace) {
		return WrenJob{}, WrenComputer{}, ErrForbidden
	}
	job, err := s.WrenJob(ctx, workspace, requester, jobID)
	if err != nil {
		return job, WrenComputer{}, err
	}
	if job.CancelledAt != nil || job.ReturnSuppressed || job.Status != "running" {
		return job, WrenComputer{}, ErrForbidden
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, job.OriginConversationID, job.Lead.ID); err != nil {
		return job, WrenComputer{}, err
	}
	child := Conversation{ID: job.ChildConversationID, WorkspaceID: workspace, AccountID: requester}
	if err = s.ValidateWrenJobConversation(ctx, child); err != nil {
		return job, WrenComputer{}, err
	}
	var parentStatus string
	if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, job.ParentRunID).Scan(&parentStatus); err != nil || parentStatus == "cancelled" {
		return job, WrenComputer{}, ErrForbidden
	}
	role, err := s.Membership(ctx, requester, workspace)
	if err != nil || role == "viewer" {
		return job, WrenComputer{}, ErrForbidden
	}
	v, err := scanWrenComputer(s.pool.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE workspace_id=$1 AND conversation_id=$2 AND phase='ready' AND delete_requested=false AND expires_at>now()`, workspace, job.OriginConversationID))
	if err != nil {
		return job, v, err
	}
	if _, _, _, err = s.computerScope(ctx, workspace, v.OwnerID, v.ConversationID, true); err != nil {
		return job, v, err
	}
	return job, v, nil
}
func (h *HTTPServer) mintWrenModelGrant(ctx context.Context, job WrenJob, computer WrenComputer) (wrenModelGrant, string, error) {
	if _, _, err := h.validateHarnessJob(ctx, job.ID); err != nil {
		return wrenModelGrant{}, "", err
	}
	id, err := newID("model_grant_")
	if err != nil {
		return wrenModelGrant{}, "", err
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return wrenModelGrant{}, "", err
	}
	token := "wren_broker_" + base64.RawURLEncoding.EncodeToString(random)
	clear(random)
	key, sealed, err := h.config.Secrets.seal([]byte(token), secretBinding(job.WorkspaceID, id, "wren-model-broker"))
	if err != nil {
		return wrenModelGrant{}, "", err
	}
	expires := minTime(h.service.Store.now().Add(h.config.Harnesses.lifetime()), computer.ExpiresAt)
	if !expires.After(h.service.Store.now().Add(30 * time.Second)) {
		return wrenModelGrant{}, "", ErrConflict
	}
	result, err := h.service.Store.pool.Exec(ctx, `INSERT INTO wren_model_grants(id,job_id,computer_id,workspace_id,requester_account_id,token_hash,token_key_version,token_ciphertext,model,max_requests,max_output_tokens,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(job_id) WHERE revoked_at IS NULL DO NOTHING`, id, job.ID, computer.ID, job.WorkspaceID, job.RequesterAccountID, secretHash(token), key, sealed, h.config.Operator.wrenModel(), h.config.Harnesses.requests(), h.config.Harnesses.output(), expires)
	if err != nil {
		return wrenModelGrant{}, "", err
	}
	if result.RowsAffected() == 0 {
		existing, e := scanWrenModelGrant(h.service.Store.pool.QueryRow(ctx, `SELECT `+wrenModelGrantColumns+` FROM wren_model_grants WHERE job_id=$1 AND revoked_at IS NULL`, job.ID))
		if e != nil {
			return existing, "", e
		}
		if !existing.ExpiresAt.After(h.service.Store.now()) || existing.UsedRequests >= existing.MaxRequests {
			return existing, "", ErrForbidden
		}
		plain, e := h.config.Secrets.open(existing.KeyVersion, existing.Ciphertext, secretBinding(existing.WorkspaceID, existing.ID, "wren-model-broker"))
		if e != nil {
			return existing, "", e
		}
		restored := string(plain)
		clear(plain)
		return existing, restored, nil
	}
	grant, err := scanWrenModelGrant(h.service.Store.pool.QueryRow(ctx, `SELECT `+wrenModelGrantColumns+` FROM wren_model_grants WHERE id=$1`, id))
	return grant, token, err
}
func (h *HTTPServer) consumeWrenModelRequest(ctx context.Context, token, model string) (wrenModelGrant, int64, error) {
	s := h.service.Store
	g, err := scanWrenModelGrant(s.pool.QueryRow(ctx, `SELECT `+wrenModelGrantColumns+` FROM wren_model_grants WHERE token_hash=$1`, secretHash(token)))
	if err != nil {
		return g, 0, err
	}
	if g.RequesterID == "" || g.RevokedAt != nil || !g.ExpiresAt.After(s.now()) || g.UsedRequests >= g.MaxRequests {
		return g, 0, ErrForbidden
	}
	if model != "" && model != g.Model {
		return g, 0, fmt.Errorf("%w: this job cannot select another model", ErrForbidden)
	}
	_, computer, err := h.validateHarnessJob(ctx, g.JobID)
	if err != nil || computer.ID != g.ComputerID {
		return g, 0, ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return g, 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,942))`, g.RequesterID); err != nil {
		return g, 0, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM wren_model_requests WHERE requester_account_id=$1 AND created_at>now()-interval '1 hour'`, g.RequesterID).Scan(&count); err != nil {
		return g, 0, err
	}
	if count >= h.config.Harnesses.accountRequests() {
		return g, 0, fmt.Errorf("%w: this account has reached its shared coding-request limit", ErrCapacity)
	}
	result, err := tx.Exec(ctx, `UPDATE wren_model_grants SET used_requests=used_requests+1 WHERE id=$1 AND revoked_at IS NULL AND expires_at>now() AND used_requests<max_requests`, g.ID)
	if err != nil {
		return g, 0, err
	}
	if result.RowsAffected() != 1 {
		return g, 0, ErrForbidden
	}
	var requestID int64
	if err = tx.QueryRow(ctx, `INSERT INTO wren_model_requests(grant_id,workspace_id,requester_account_id,model) VALUES($1,$2,$3,$4) RETURNING id`, g.ID, g.WorkspaceID, g.RequesterID, g.Model).Scan(&requestID); err != nil {
		return g, 0, err
	}
	return g, requestID, tx.Commit(ctx)
}

// wrenModelBroker is the ONLY unauthenticated-human route intended for a scoped
// guest broker capability. It never accepts destination URLs or provider keys.
func (h *HTTPServer) wrenModelBroker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer wren_broker_") || len(auth) > 256 {
		writeStoreError(w, ErrUnauthorized)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	var input map[string]json.RawMessage
	if !decodeLimit(w, r, &input, 4<<20) {
		return
	}
	var requested string
	if raw, ok := input["model"]; ok && json.Unmarshal(raw, &requested) != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid coding model"))
		return
	}
	var messages []json.RawMessage
	if json.Unmarshal(input["messages"], &messages) != nil || len(messages) < 1 || len(messages) > 500 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a bounded message history is required"))
		return
	}
	g, requestID, err := h.consumeWrenModelRequest(r.Context(), token, requested)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := "uncertain"
	usage := json.RawMessage(`{}`)
	upstreamID := ""
	httpStatus := 0
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = h.service.Store.pool.Exec(ctx, `UPDATE wren_model_requests SET status=$2,http_status=$3,usage=$4,upstream_id=$5,completed_at=now() WHERE id=$1`, requestID, status, httpStatus, usage, upstreamID)
	}()
	config, err := h.workspaceModelConfig(r.Context(), g.WorkspaceID, sdk.ActorRef{Kind: "human", ID: g.RequesterID}, h.config.Operator)
	if err != nil {
		status = "failed"
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the coding model connection is unavailable"))
		return
	}
	maxOutput := g.MaxOutputTokens
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := input[key]; ok {
			var n int
			if json.Unmarshal(raw, &n) != nil || n < 1 {
				status = "failed"
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid output token limit"))
				return
			}
			maxOutput = min(maxOutput, n)
		}
	}
	payload := map[string]json.RawMessage{}
	for _, key := range []string{"messages", "tools", "tool_choice", "parallel_tool_calls", "temperature", "top_p", "stop", "response_format", "stream", "stream_options"} {
		if raw, ok := input[key]; ok {
			payload[key] = raw
		}
	}
	config.Model = g.Model
	effort := config.modelReasoningEffort()
	reasoning := map[string]any{"effort": effort}
	if effort == "on" {
		reasoning = map[string]any{"enabled": true}
	}
	payload["reasoning"], _ = json.Marshal(reasoning)
	payload["provider"], _ = json.Marshal(map[string]bool{"require_parameters": true})
	payload["model"], _ = json.Marshal(g.Model)
	payload["max_tokens"], _ = json.Marshal(maxOutput)
	body, err := json.Marshal(payload)
	if err != nil {
		status = "failed"
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid coding request"))
		return
	}
	upstreamCtx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	upstream, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, strings.TrimRight(config.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		status = "failed"
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the coding model connection is unavailable"))
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+config.APIKey)
	client := &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if h.config.Harnesses.transport != nil {
		client.Transport = h.config.Harnesses.transport
	}
	var paid *WrenAIPaidRequest
	if h.config.AIUsage.Enforce {
		var origin, childRun, childLease string
		if err = h.service.Store.pool.QueryRow(r.Context(), `SELECT j.origin_conversation_id,j.child_run_id,COALESCE(r.lease_token,'') FROM wren_jobs j JOIN operator_runs r ON r.id=j.child_run_id WHERE j.id=$1 AND j.workspace_id=$2`, g.JobID, g.WorkspaceID).Scan(&origin, &childRun, &childLease); err != nil {
			status = "failed"
			writeStoreError(w, err)
			return
		}
		imageCount, countErr := wrenAIModelImageCount(body)
		if countErr != nil {
			status = "failed"
			writeStoreError(w, countErr)
			return
		}
		paid, err = h.BeginWrenAIPaidRequest(r.Context(), WrenAIReservationInput{WorkspaceID: g.WorkspaceID, AccountID: g.RequesterID, ConversationID: origin, RunID: childRun, RunLease: childLease, RequestKey: fmt.Sprintf("acp_request_%d", requestID), Kind: "acp", Model: g.Model}, WrenAIRequestEstimate{Kind: "acp", Model: g.Model, InputBytes: len(body), MaxOutputTokens: maxOutput, ImageCount: imageCount})
		if err != nil {
			status = "failed"
			writeStoreError(w, err)
			return
		}
		defer func() { _ = paid.Finish(r.Context(), usage, upstreamID) }()
		if err = paid.MarkDispatched(r.Context()); err != nil {
			status = "failed"
			writeStoreError(w, err)
			return
		}
	}
	response, err := client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the coding model request did not finish"))
		return
	}
	defer response.Body.Close()
	httpStatus = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status = "failed"
		writeError(w, http.StatusBadGateway, fmt.Errorf("the coding model provider rejected the request"))
		return
	}
	recordUsage := func(data []byte) {
		var event struct {
			ID    string          `json:"id"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(data, &event) == nil {
			if len(event.Usage) > 0 && string(event.Usage) != "null" {
				usage = event.Usage
			}
			if event.ID != "" {
				upstreamID = event.ID
			}
		}
	}
	if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		scanner := bufio.NewScanner(io.LimitReader(response.Body, 32<<20))
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		done := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					done = true
				} else {
					recordUsage([]byte(data))
				}
			}
			if _, err = io.WriteString(w, line+"\n"); err != nil {
				return
			}
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
		}
		if scanner.Err() == nil && done {
			status = "completed"
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		writeError(w, http.StatusBadGateway, fmt.Errorf("the coding model response exceeded its limit"))
		return
	}
	recordUsage(raw)
	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(raw); err == nil {
		status = "completed"
	}
}

type wrenHarnessExecution struct {
	JobID, RunID, ComputerID, Status, ProcessID, GrantID, ResponseText, TerminalStatus string
	Snapshot                                                                           wrenharness.Snapshot
	PromptRequestID                                                                    int64
	StopRequested, ProcessStopped                                                      bool
}

func (h *HTTPServer) harnessExecution(ctx context.Context, job string) (wrenHarnessExecution, error) {
	var e wrenHarnessExecution
	var snapshot []byte
	err := h.service.Store.pool.QueryRow(ctx, `SELECT job_id,run_id,computer_id,status,process_id,COALESCE(grant_id,''),snapshot,response_text,prompt_request_id,stop_requested,process_stopped,terminal_status FROM wren_harness_executions WHERE job_id=$1`, job).Scan(&e.JobID, &e.RunID, &e.ComputerID, &e.Status, &e.ProcessID, &e.GrantID, &snapshot, &e.ResponseText, &e.PromptRequestID, &e.StopRequested, &e.ProcessStopped, &e.TerminalStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(snapshot, &e.Snapshot)
	}
	return e, err
}
func (s *Store) claimWrenHarness(ctx context.Context) (OperatorRun, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperatorRun{}, false, err
	}
	defer tx.Rollback(ctx)
	var run OperatorRun
	err = tx.QueryRow(ctx, `SELECT r.id,r.conversation_id,r.status,r.model,COALESCE(r.requester_account_id,'') FROM operator_runs r JOIN wren_harness_executions e ON e.run_id=r.id WHERE r.executor_kind='acp-opencode' AND e.status IN ('pending','running','stop_requested') AND (r.status='queued' OR (r.status='running' AND r.lease_expires_at<now())) ORDER BY r.created_at,r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&run.ID, &run.ConversationID, &run.Status, &run.Model, &run.RequesterAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, false, nil
	}
	if err != nil {
		return run, false, err
	}
	run.Lease, err = newID("lease_")
	if err != nil {
		return run, false, err
	}
	run.Status = "running"
	if _, err = tx.Exec(ctx, `UPDATE operator_runs SET status='running',lease_token=$2,lease_expires_at=now()+interval '45 seconds' WHERE id=$1`, run.ID, run.Lease); err != nil {
		return run, false, err
	}
	return run, true, tx.Commit(ctx)
}

// WrenHarnessDispatcher executes ACP runs independently of the operator loop,
// so a lead can continue responding while the actual coding process works.
type WrenHarnessDispatcher struct{ Server *HTTPServer }

func (d *WrenHarnessDispatcher) Run(ctx context.Context) error {
	if d.Server == nil {
		return fmt.Errorf("harness dispatcher has no server")
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	slots := make(chan struct{}, 2)
	var workers sync.WaitGroup
	workCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); workers.Wait() }()
	for {
		if err := d.reconcileStops(workCtx); err != nil && workCtx.Err() == nil {
			return err
		}
		if d.Server.HarnessConfigured() {
			select {
			case slots <- struct{}{}:
				run, ok, err := d.Server.service.Store.claimWrenHarness(workCtx)
				if err != nil {
					<-slots
					return err
				}
				if ok {
					workers.Add(1)
					go func() { defer workers.Done(); defer func() { <-slots }(); d.execute(workCtx, run) }()
				} else {
					<-slots
				}
			default:
			}
		}
		select {
		case <-workCtx.Done():
			return workCtx.Err()
		case <-tick.C:
		}
	}
}
func (d *WrenHarnessDispatcher) execute(parent context.Context, run OperatorRun) {
	h := d.Server
	s := h.service.Store
	ctx, cancel := context.WithTimeout(parent, h.config.Harnesses.lifetime()+5*time.Minute)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	var lost atomic.Bool
	go func() {
		checks := 0
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-finished:
				return
			case <-ticker.C:
				result, err := s.pool.Exec(ctx, `UPDATE operator_runs SET lease_expires_at=now()+interval '45 seconds' WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now()`, run.ID, run.Lease)
				if err != nil || result.RowsAffected() != 1 {
					lost.Store(true)
					cancel()
					return
				}
				var stop bool
				if err = s.pool.QueryRow(ctx, `SELECT stop_requested FROM wren_harness_executions WHERE run_id=$1`, run.ID).Scan(&stop); err != nil || stop {
					cancel()
					return
				}
				checks++
				if checks%5 == 0 {
					var jobID string
					if e := s.pool.QueryRow(ctx, `SELECT job_id FROM wren_harness_executions WHERE run_id=$1`, run.ID).Scan(&jobID); e != nil {
						cancel()
						return
					}
					if currentJob, currentComputer, e := h.validateHarnessJob(ctx, jobID); e != nil {
						_, _ = s.pool.Exec(ctx, `UPDATE wren_harness_executions SET stop_requested=true,status='stop_requested',terminal_status='cancelled',updated_at=now() WHERE run_id=$1`, run.ID)
						_, _ = s.pool.Exec(ctx, `UPDATE wren_model_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE job_id=$1`, jobID)
						cancel()
						return
					} else if checks%20 == 0 && currentComputer.MutationRunID == run.ID {
						origin := Conversation{ID: currentJob.OriginConversationID, WorkspaceID: currentJob.WorkspaceID, AccountID: currentJob.RequesterAccountID}
						if _, e = h.AcquireComputerMutationLease(ctx, origin, run); e != nil {
							cancel()
							return
						}
					}
				}

			}
		}
	}()
	err := d.work(ctx, run)
	if parent.Err() != nil || lost.Load() {
		return
	} // Recovery attaches the same process.
	endCtx, endCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer endCancel()
	var jobID string
	if e := s.pool.QueryRow(endCtx, `SELECT job_id FROM wren_harness_executions WHERE run_id=$1`, run.ID).Scan(&jobID); e != nil {
		return
	}
	execution, e := h.harnessExecution(endCtx, jobID)
	if e != nil {
		return
	}
	_, _ = s.pool.Exec(endCtx, `UPDATE wren_model_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE job_id=$1`, jobID)
	if err != nil {
		stopped, stopErr := d.stopExecution(endCtx, execution)
		cancelled := execution.StopRequested || errors.Is(err, ErrForbidden)
		if cancelled && stopped {
			_ = s.finishOperator(endCtx, run, "cancelled", "")
			_, _ = s.pool.Exec(endCtx, `UPDATE wren_harness_executions SET status='stopped',process_stopped=true,updated_at=now() WHERE job_id=$1`, jobID)
			return
		}
		if !stopped {
			terminal := "failed"
			if cancelled {
				terminal = "cancelled"
			}
			_, _ = s.pool.Exec(endCtx, `UPDATE wren_harness_executions SET status='stop_requested',stop_requested=true,terminal_status=$2,failure='The worker stop is pending; its remote process has not been verified stopped.',next_attempt_at=now(),updated_at=now() WHERE job_id=$1`, jobID, terminal)
			_ = stopErr
			return
		}
		message := "The external coding worker did not finish. Its saved files and session evidence remain available."
		if errors.Is(err, ErrCapacity) {
			message = "The coding worker reached its request allowance. Its saved files and session evidence remain available."
		}
		_ = s.finishOperator(endCtx, run, "failed", message)
		_, _ = s.pool.Exec(endCtx, `UPDATE wren_harness_executions SET status='failed',failure=$2,process_stopped=true,updated_at=now() WHERE job_id=$1`, jobID, message)
		return
	}
}
func (d *WrenHarnessDispatcher) work(ctx context.Context, run OperatorRun) error {
	h := d.Server
	s := h.service.Store
	var jobID string
	if err := s.pool.QueryRow(ctx, `SELECT job_id FROM wren_harness_executions WHERE run_id=$1`, run.ID).Scan(&jobID); err != nil {
		return err
	}
	job, computer, err := h.validateHarnessJob(ctx, jobID)
	if err != nil {
		return err
	}
	origin := Conversation{ID: job.OriginConversationID, WorkspaceID: job.WorkspaceID, AccountID: job.RequesterAccountID}
	for {
		if _, err = h.AcquireComputerMutationLease(ctx, origin, run); err == nil {
			break
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = h.ReleaseComputerMutationLease(releaseCtx, computer.ID, run.ID, run.Lease)
	}()
	client, _, err := h.ClientForConversation(ctx, origin)
	if err != nil {
		return err
	}
	execution, err := h.harnessExecution(ctx, jobID)
	if err != nil {
		return err
	}
	if execution.StopRequested {
		return ErrForbidden
	}
	// Installation is a real bounded job inside this already-authorized computer.
	// It is independently inspectable and reconnects through the same job ID.
	installID := "opencode_install_" + strings.TrimPrefix(jobID, "job_")
	install, err := client.StartJob(ctx, wrencomputer.JobRequest{ID: installID, Command: `set -euo pipefail; export PATH="/home/wren/.opencode/bin:/usr/local/bin:$PATH"; if ! command -v opencode >/dev/null || [ "$(opencode --version | tail -n 1)" != 'opencode v2.1.4' ]; then curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 https://opencode.ai/v2/install | bash -s -- --version 2.1.4 --no-modify-path; fi; test "$(opencode --version | tail -n 1)" = 'opencode v2.1.4'; opencode --version`, TimeoutSeconds: 300})
	if err != nil {
		return err
	}
	if err = s.operatorEvent(ctx, run, "working", map[string]any{"label": "Preparing the external coding harness", "status": "running", "executor": "acp-opencode"}); err != nil {
		return err
	}
	for install.Status == "starting" || install.Status == "running" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		install, err = client.Job(ctx, installID)
		if err != nil {
			return err
		}
	}
	if install.Status != "completed" {
		return fmt.Errorf("OpenCode installation did not finish")
	}
	grant, token, err := h.mintWrenModelGrant(ctx, job, computer)
	if err != nil {
		return err
	}
	process := execution.ProcessID
	if process == "" {
		process = "harness_" + strings.TrimPrefix(jobID, "job_")
	}
	result, err := s.pool.Exec(ctx, `UPDATE wren_harness_executions SET grant_id=$2,process_id=$3,status='running',updated_at=now() WHERE job_id=$1 AND NOT stop_requested AND NOT process_stopped AND EXISTS(SELECT 1 FROM operator_runs WHERE id=$4 AND lease_token=$5 AND status='running' AND lease_expires_at>now())`, jobID, grant.ID, process, run.ID, run.Lease)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	directory := "/workspace/jobs/" + jobID
	if _, err = client.WriteFile(ctx, directory+"/.canter-job", []byte("Scoped coding work for "+jobID+". This directory is a working scope inside the explicitly granted origin computer, not another OS trust boundary.\n")); err != nil {
		return err
	}
	remote, err := wrenharness.AttachRemote(client, process)
	if err != nil {
		return err
	}
	remoteJob, lookupErr := client.Job(ctx, process)
	if lookupErr != nil {
		var guestError *wrencomputer.Error
		if !errors.As(lookupErr, &guestError) || guestError.Code != "not_found" {
			return lookupErr
		}
		remote, err = wrenharness.StartOpenCode(ctx, client, wrenharness.OpenCodeLaunch{ProcessID: process, Directory: directory, TimeoutSeconds: 3600, Broker: wrenharness.BrokerCredential{BaseURL: h.config.Harnesses.BrokerBaseURL, Token: token, Model: grant.Model, ExpiresAt: grant.ExpiresAt, MaxRequests: grant.MaxRequests, MaxOutputTokens: grant.MaxOutputTokens}})
		if err != nil {
			return err
		}
	} else if remoteJob.Status != "starting" && remoteJob.Status != "running" && execution.PromptRequestID == 0 {
		return wrenharness.ErrProcessExited
	}
	execution, err = h.harnessExecution(ctx, jobID)
	if err != nil {
		return err
	}
	snapshot := execution.Snapshot
	snapshot.ProcessID = process
	persist := func(value wrenharness.Snapshot) error {
		raw, e := json.Marshal(value)
		if e != nil {
			return e
		}
		result, e := s.pool.Exec(ctx, `UPDATE wren_harness_executions SET snapshot=$2,prompt_request_id=CASE WHEN $3>0 THEN $3 ELSE prompt_request_id END,updated_at=now() WHERE job_id=$1 AND NOT stop_requested AND NOT process_stopped AND EXISTS(SELECT 1 FROM operator_runs WHERE id=$4 AND lease_token=$5 AND status='running' AND lease_expires_at>now())`, jobID, raw, value.PromptRequestID, run.ID, run.Lease)
		if e == nil && result.RowsAffected() != 1 {
			return ErrConflict
		}
		return e
	}
	onUpdate := func(eventCtx context.Context, cursor int64, data json.RawMessage) error {
		return h.saveHarnessUpdate(eventCtx, run, jobID, cursor, data)
	}
	permission := func(permissionCtx context.Context, request wrenharness.PermissionRequest) (string, error) {
		current, e := h.harnessExecution(permissionCtx, jobID)
		if e != nil {
			return "", e
		}
		if current.Snapshot.SessionID == "" || request.SessionID != current.Snapshot.SessionID {
			return "", ErrForbidden
		}
		for {
			if _, _, e := h.validateHarnessJob(permissionCtx, jobID); e != nil {
				return "", e
			}
			e := h.ComputerAgentWriteAllowed(permissionCtx, origin)
			if e == nil {
				break
			}
			if !errors.Is(e, ErrConflict) {
				return "", e
			}
			select {
			case <-permissionCtx.Done():
				return "", permissionCtx.Err()
			case <-time.After(time.Second):
			}
		}
		// Protocol path checks are defense in depth. The explicit grant is to this
		// origin VM; cwd alone is never claimed as an OS isolation boundary.
		var call struct {
			Kind      string `json:"kind"`
			Locations []struct {
				Path string `json:"path"`
			} `json:"locations"`
		}
		if json.Unmarshal(request.ToolCall, &call) != nil {
			return "", ErrForbidden
		}
		for _, location := range call.Locations {
			if !harnessLocationAllowed(call.Kind, location.Path, directory) {
				return "", nil
			}
		}
		for _, option := range request.Options {
			if option.Kind == "allow_once" {
				return option.ID, nil
			}
		}
		return "", nil
	}
	acp, err := wrenharness.NewClient(remote, snapshot, persist, onUpdate, permission)
	if err != nil {
		return err
	}
	if snapshot.ProtocolVersion == 0 {
		if err = acp.Initialize(ctx); err != nil {
			return err
		}
	}
	if acp.Snapshot().SessionID == "" {
		if _, err = acp.NewSession(ctx, directory); err != nil {
			return err
		}
	}
	promptID := execution.PromptRequestID
	if promptID == 0 {
		promptID, err = acp.StartPrompt(ctx, "Work only from this scoped assignment and the explicitly authorized computer. Do not borrow other account credentials or pretend external actions happened. Work directory: "+directory+"\nTitle: "+job.Title+"\nAssignment:\n"+job.Prompt)
		if err != nil {
			return err
		}
	}
	outcome, err := acp.AwaitPrompt(ctx, promptID)
	if err != nil {
		return err
	}
	if outcome.StopReason == "cancelled" {
		return ErrForbidden
	}
	if outcome.StopReason != "end_turn" {
		return fmt.Errorf("external worker ended before a normal completion")
	}
	if _, _, err = h.validateHarnessJob(ctx, jobID); err != nil {
		return err
	}
	execution, err = h.harnessExecution(ctx, jobID)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(execution.ResponseText)
	if text == "" {
		text = "The coding worker completed its turn. Inspect its files in " + directory + " for the result."
	}
	// The actual ACP end-turn is the completion evidence. Closing an idle agent
	// process is tracked separately and is retried if its stop is uncertain.
	stopped := acp.Close(ctx) == nil
	if err = s.finishOperator(ctx, run, "completed", text); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE wren_harness_executions SET status='completed',process_stopped=$2,updated_at=now(),next_attempt_at=now() WHERE job_id=$1`, jobID, stopped)
	return err
}

// Follow-up workers may read earlier deliverables on this same explicitly
// granted origin VM. Their structured edit permissions remain scoped to their
// own working directory. This is policy guidance, not OS process isolation.
func harnessLocationAllowed(kind, location, directory string) bool {
	clean := path.Clean(location)
	if clean == directory || strings.HasPrefix(clean, directory+"/") {
		return true
	}
	return (kind == "read" || kind == "search") && (clean == "/workspace/jobs" || strings.HasPrefix(clean, "/workspace/jobs/"))
}

func (h *HTTPServer) saveHarnessUpdate(ctx context.Context, run OperatorRun, job string, cursor int64, data json.RawMessage) error {
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM operator_runs WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now() FOR SHARE`, run.ID, run.Lease).Scan(&id); err != nil {
		return ErrConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO wren_harness_events(job_id,cursor,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, job, cursor, data)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	var update struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind       string          `json:"sessionUpdate"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"toolCallId"`
			Status     string          `json:"status"`
			Title      string          `json:"title"`
		} `json:"update"`
	}
	if json.Unmarshal(data, &update) != nil {
		return fmt.Errorf("invalid harness update")
	}
	kind := "working"
	event := map[string]any{"label": "Working in OpenCode", "executor": "acp-opencode"}
	var content struct{ Type, Text string }
	if update.Update.Kind == "agent_message_chunk" {
		if json.Unmarshal(update.Update.Content, &content) != nil {
			return fmt.Errorf("invalid harness message content")
		}
	}
	if update.Update.Kind == "agent_message_chunk" && content.Type == "text" {
		kind = "text"
		event = map[string]any{"content": content.Text, "delta": true, "step": 1}
		if _, err = tx.Exec(ctx, `UPDATE wren_harness_executions SET response_text=left(response_text||$2,100000),updated_at=now() WHERE job_id=$1`, job, content.Text); err != nil {
			return err
		}
	}
	if update.Update.Kind == "tool_call" || update.Update.Kind == "tool_call_update" {
		kind = "tool"
		status := update.Update.Status
		if status == "pending" || status == "in_progress" || status == "" {
			status = "running"
		}
		event = map[string]any{"name": "OpenCode", "callId": update.Update.ToolCallID, "status": status, "label": update.Update.Title}
	}
	encoded, _ := json.Marshal(event)
	if _, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,$3,$4)`, run.ConversationID, run.ID, kind, encoded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (d *WrenHarnessDispatcher) privateComputerClient(ctx context.Context, computerID string) (*wrencomputer.Client, WrenComputer, error) {
	h := d.Server
	v, err := scanWrenComputer(h.service.Store.pool.QueryRow(ctx, `SELECT `+computerColumns+` FROM wren_computers WHERE id=$1`, computerID))
	if err != nil {
		return nil, v, err
	}
	if v.Phase == "deleted" {
		return nil, v, nil
	}
	key, err := h.config.Secrets.open(v.KeyVersion, v.KeyCiphertext, secretBinding(v.WorkspaceID, v.ID, "wren-computer-ssh"))
	if err != nil {
		return nil, v, err
	}
	defer clear(key)
	client, err := wrencomputer.NewSSH(wrencomputer.Config{Address: v.Address, PrivateKeyPEM: key, HostKeySHA256: v.HostFingerprint})
	return client, v, err
}
func (d *WrenHarnessDispatcher) stopExecution(ctx context.Context, e wrenHarnessExecution) (bool, error) {
	if e.ProcessStopped {
		return true, nil
	}
	client, v, err := d.privateComputerClient(ctx, e.ComputerID)
	if err != nil {
		return false, err
	}
	if v.Phase == "deleted" {
		return true, nil
	}
	// Installation is a detached guest job too. A cancellation before ACP
	// launch must confirm that installer stopped, rather than leaving paid work
	// active behind a terminal UI state.
	installID := "opencode_install_" + strings.TrimPrefix(e.JobID, "job_")
	install, installErr := client.StopJob(ctx, installID)
	if installErr != nil {
		return false, installErr
	}
	if !computerCommandFinished(install.Status) {
		return false, fmt.Errorf("coding harness installation stop is pending")
	}
	if e.ProcessID == "" {
		return true, nil
	}
	remote, err := wrenharness.AttachRemote(client, e.ProcessID)
	if err != nil {
		return false, err
	}
	if err = remote.Stop(ctx); err != nil {
		var guestError *wrencomputer.Error
		if errors.As(err, &guestError) && guestError.Code == "not_found" {
			job, stopErr := client.StopJob(ctx, e.ProcessID)
			return stopErr == nil && computerCommandFinished(job.Status), stopErr
		}
		return false, err
	}
	return true, nil
}
func (d *WrenHarnessDispatcher) reconcileStops(ctx context.Context) error {
	s := d.Server.service.Store
	rows, err := s.pool.Query(ctx, `SELECT e.job_id FROM wren_harness_executions e LEFT JOIN wren_jobs j ON j.id=e.job_id LEFT JOIN operator_runs r ON r.id=e.run_id WHERE e.next_attempt_at<=now() AND e.process_stopped=false AND (r.id IS NULL OR r.status NOT IN ('queued','running') OR r.lease_expires_at IS NULL OR r.lease_expires_at<=now()) AND (e.stop_requested OR j.id IS NULL OR r.id IS NULL OR r.status IN ('completed','failed','cancelled')) LIMIT 20`)
	if err != nil {
		return err
	}
	jobs := []string{}
	for rows.Next() {
		var job string
		if err = rows.Scan(&job); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		execution, e := d.Server.harnessExecution(ctx, job)
		if e != nil {
			continue
		}
		_, _ = s.pool.Exec(ctx, `UPDATE wren_model_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE job_id=$1`, job)
		operation, cancel := context.WithTimeout(ctx, 10*time.Second)
		stopped, _ := d.stopExecution(operation, execution)
		cancel()
		if stopped {
			terminal := execution.TerminalStatus
			if terminal == "" {
				terminal = "cancelled"
			}
			_, _ = s.pool.Exec(ctx, `WITH ended AS (UPDATE operator_runs SET status=$2,completed_at=now(),lease_token=NULL,lease_expires_at=NULL,failure=CASE WHEN $2='failed' THEN 'The external coding worker stopped after a failure. Its saved files remain available.' ELSE failure END WHERE id=$1 AND status IN ('queued','running') RETURNING id,conversation_id) INSERT INTO operator_events(conversation_id,run_id,kind,data) SELECT conversation_id,id,'finished',jsonb_build_object('status',$2::text,'content','The external coding worker has stopped.') FROM ended`, execution.RunID, terminal)
			_, _ = s.pool.Exec(ctx, `UPDATE wren_harness_executions SET process_stopped=true,status=CASE WHEN status IN ('pending','running','stop_requested') THEN 'stopped' ELSE status END,updated_at=now() WHERE job_id=$1`, job)
		} else {
			_, _ = s.pool.Exec(ctx, `UPDATE wren_harness_executions SET next_attempt_at=now()+interval '10 seconds' WHERE job_id=$1`, job)
		}
	}
	return nil
}
