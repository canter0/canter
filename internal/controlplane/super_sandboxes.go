package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

type SuperSandbox struct {
	ID                    string             `json:"id"`
	WorkspaceID           string             `json:"workspaceId"`
	Name                  string             `json:"name"`
	State                 string             `json:"state"`
	WorkerIDs             []string           `json:"workerIds"`
	MaxConcurrency        int                `json:"maxConcurrency"`
	MaxWorkerSeconds      int                `json:"maxWorkerSeconds"`
	ReservedWorkerSeconds int                `json:"reservedWorkerSeconds"`
	CreatedAt             time.Time          `json:"createdAt"`
	StartedAt             *time.Time         `json:"startedAt,omitempty"`
	FinishedAt            *time.Time         `json:"finishedAt,omitempty"`
	Failure               string             `json:"failure,omitempty"`
	BlockedReason         string             `json:"blockedReason,omitempty"`
	Tasks                 []SuperSandboxTask `json:"tasks"`
}

type SuperSandboxTask struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Index          int        `json:"index"`
	Command        string     `json:"command"`
	Directory      string     `json:"directory,omitempty"`
	TimeoutSeconds int        `json:"timeoutSeconds"`
	State          string     `json:"state"`
	WorkerID       string     `json:"workerId,omitempty"`
	JobID          string     `json:"jobId,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	ExitCode       *int       `json:"exitCode,omitempty"`
	Error          string     `json:"error,omitempty"`
	Reason         string     `json:"reason,omitempty"`
}

type SuperSandboxTaskInput struct {
	Name           string `json:"name"`
	Command        string `json:"command"`
	Directory      string `json:"directory,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type SuperSandboxCreateInput struct {
	Name             string                  `json:"name"`
	WorkerIDs        []string                `json:"workerIds"`
	MaxConcurrency   int                     `json:"maxConcurrency"`
	MaxWorkerSeconds int                     `json:"maxWorkerSeconds"`
	IdempotencyKey   string                  `json:"idempotencyKey"`
	Tasks            []SuperSandboxTaskInput `json:"tasks"`
}

var superSandboxIDPattern = regexp.MustCompile(`^ssb_[A-Za-z0-9_-]+$`)

func normalizeSuperSandbox(in SuperSandboxCreateInput) (SuperSandboxCreateInput, int, string, error) {
	bad := func(message string) (SuperSandboxCreateInput, int, string, error) {
		return in, 0, "", fmt.Errorf("%w: %s", ErrConflict, message)
	}
	validName := func(s string) bool {
		return utf8.ValidString(s) && utf8.RuneCountInString(s) >= 1 && utf8.RuneCountInString(s) <= 100 && !strings.ContainsRune(s, 0)
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validName(in.Name) {
		return bad("name must contain 1-100 characters and no NUL")
	}
	if len(in.WorkerIDs) < 1 || len(in.WorkerIDs) > 16 || in.MaxConcurrency < 1 || in.MaxConcurrency > len(in.WorkerIDs) {
		return bad("supply 1-16 unique ready workers and maxConcurrency between 1 and worker count")
	}
	seen := map[string]bool{}
	in.WorkerIDs = append([]string(nil), in.WorkerIDs...)
	for _, id := range in.WorkerIDs {
		if !validSandboxSurfaceID(id) || seen[id] {
			return bad("workerIds must be unique sandbox IDs")
		}
		seen[id] = true
	}
	if in.MaxWorkerSeconds < 1 || in.MaxWorkerSeconds > 86400 || len(in.Tasks) < 1 || len(in.Tasks) > 256 {
		return bad("maxWorkerSeconds must be 1-86400 and tasks must contain 1-256 entries")
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > 256 || strings.ContainsRune(in.IdempotencyKey, 0) {
		return bad("idempotencyKey is required (at most 256 bytes, no NUL)")
	}
	in.Tasks = append([]SuperSandboxTaskInput(nil), in.Tasks...)
	total, commandBytes := 0, 0
	for i := range in.Tasks {
		t := &in.Tasks[i]
		t.Name = strings.TrimSpace(t.Name)
		if !validName(t.Name) {
			return bad("each task name must contain 1-100 characters and no NUL")
		}
		if strings.TrimSpace(t.Command) == "" || len(t.Command) > 64000 || !utf8.ValidString(t.Command) || strings.ContainsRune(t.Command, 0) {
			return bad("each command must contain 1-64000 bytes and no NUL")
		}
		commandBytes += len(t.Command)
		if commandBytes > 192<<10 {
			return bad("total commands exceed 192 KiB")
		}
		var err error
		t.Directory, err = sandboxFilePath(t.Directory)
		if err != nil {
			return in, 0, "", err
		}
		if t.TimeoutSeconds == 0 {
			t.TimeoutSeconds = 300
		}
		if t.TimeoutSeconds < 1 || t.TimeoutSeconds > 3600 {
			return bad("task timeoutSeconds must be 1-3600")
		}
		total += t.TimeoutSeconds
		if total > in.MaxWorkerSeconds {
			return bad("sum of task timeouts exceeds maxWorkerSeconds execution allocation (not a VM billing cap)")
		}
	}
	// The key identifies the request, but is not part of its immutable payload.
	canonical := in
	canonical.IdempotencyKey = ""
	raw, _ := json.Marshal(canonical)
	return in, total, fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func (s *Store) superSandbox(ctx context.Context, q pgx.Tx, workspace, id string) (SuperSandbox, error) {
	v := SuperSandbox{WorkerIDs: []string{}, Tasks: []SuperSandboxTask{}}
	err := q.QueryRow(ctx, `SELECT id,workspace_id,name,state,max_concurrency,max_worker_seconds,reserved_worker_seconds,created_at,started_at,finished_at,failure FROM super_sandboxes WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(
		&v.ID, &v.WorkspaceID, &v.Name, &v.State, &v.MaxConcurrency, &v.MaxWorkerSeconds, &v.ReservedWorkerSeconds, &v.CreatedAt, &v.StartedAt, &v.FinishedAt, &v.Failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	rows, err := q.Query(ctx, `SELECT worker_id FROM super_sandbox_workers WHERE run_id=$1 ORDER BY worker_index`, id)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var worker string
		if err = rows.Scan(&worker); err != nil {
			rows.Close()
			return v, err
		}
		v.WorkerIDs = append(v.WorkerIDs, worker)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return v, err
	}
	rows, err = q.Query(ctx, `SELECT id,name,task_index,command,directory,timeout_seconds,state,COALESCE(worker_id,''),COALESCE(job_id,''),started_at,finished_at,exit_code,error,COALESCE(reason,'') FROM super_sandbox_tasks WHERE run_id=$1 ORDER BY task_index`, id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var t SuperSandboxTask
		if err = rows.Scan(&t.ID, &t.Name, &t.Index, &t.Command, &t.Directory, &t.TimeoutSeconds, &t.State, &t.WorkerID, &t.JobID, &t.StartedAt, &t.FinishedAt, &t.ExitCode, &t.Error, &t.Reason); err != nil {
			return v, err
		}
		v.Tasks = append(v.Tasks, t)
	}
	if err = rows.Err(); err != nil {
		return v, err
	}
	if v.FinishedAt == nil {
		if err = q.QueryRow(ctx, `SELECT `+superSandboxBlockerSQL+` FROM super_sandboxes r WHERE r.workspace_id=$1 AND r.id=$2`, workspace, id).Scan(&v.BlockedReason); err != nil {
			return v, err
		}
	}
	return v, nil
}

// superSandboxBlockerSQL derives the current blocker for an unfinished batch
// (run alias r) from worker and task facts, in priority order. It reports
// workers_busy only when an eligible ready worker exists but none is free,
// worker_unavailable only when no assigned worker is currently eligible, and
// never a worker blocker merely because tasks sit at the run's concurrency.
const superSandboxBlockerSQL = `CASE
 WHEN r.finished_at IS NOT NULL THEN ''
 WHEN EXISTS(SELECT 1 FROM super_sandbox_workers bw LEFT JOIN sandboxes bs ON bs.id=bw.sandbox_id
   WHERE bw.run_id=r.id AND (bs.id IS NULL OR bs.generation<>bw.generation OR bs.phase IN ('deleted','failed')))
   THEN 'worker_replaced'
 WHEN EXISTS(SELECT 1 FROM super_sandbox_tasks bt WHERE bt.run_id=r.id AND bt.state='unknown')
   THEN COALESCE((SELECT bt.reason FROM super_sandbox_tasks bt WHERE bt.run_id=r.id AND bt.state='unknown' ORDER BY bt.task_index LIMIT 1),'outcome_unknown')
 WHEN EXISTS(SELECT 1 FROM super_sandbox_tasks bt WHERE bt.run_id=r.id AND bt.state='queued')
   AND (SELECT count(*) FROM super_sandbox_tasks at WHERE at.run_id=r.id AND at.state IN ('starting','running','unknown')) < r.max_concurrency
   THEN CASE
     WHEN NOT EXISTS(SELECT 1 FROM super_sandbox_workers bw JOIN sandboxes bs ON bs.id=bw.sandbox_id AND bs.id=bw.worker_id
       WHERE bw.run_id=r.id AND bs.workspace_id=r.workspace_id AND bs.generation=bw.generation
       AND bs.phase='ready' AND NOT bs.sleep_requested AND NOT bs.delete_requested)
       THEN 'worker_unavailable'
     WHEN NOT EXISTS(SELECT 1 FROM super_sandbox_workers bw JOIN sandboxes bs ON bs.id=bw.sandbox_id AND bs.id=bw.worker_id
       WHERE bw.run_id=r.id AND bs.workspace_id=r.workspace_id AND bs.generation=bw.generation
       AND bs.phase='ready' AND NOT bs.sleep_requested AND NOT bs.delete_requested
       AND NOT EXISTS(SELECT 1 FROM super_sandbox_tasks bt WHERE bt.worker_id=bw.worker_id AND bt.state IN ('starting','running','unknown')))
       THEN 'workers_busy'
     ELSE '' END
 ELSE '' END`

// Reads are passive. Dispatch is exclusively performed by the background reconciler.
func (s *Store) SuperSandbox(ctx context.Context, workspace, id string) (SuperSandbox, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SuperSandbox{}, err
	}
	defer tx.Rollback(ctx)
	return s.superSandbox(ctx, tx, workspace, id)
}

func (s *Store) SuperSandboxes(ctx context.Context, workspace string) ([]SuperSandbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM super_sandboxes WHERE workspace_id=$1 ORDER BY created_at DESC,id LIMIT 100`, workspace)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []SuperSandbox{}
	for _, id := range ids {
		v, err := s.SuperSandbox(ctx, workspace, id)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (h *HTTPServer) CreateSuperSandbox(ctx context.Context, workspace, account string, actor sdk.ActorRef, input SuperSandboxCreateInput) (SuperSandbox, error) {
	if !h.config.Sandboxes.Enabled {
		return SuperSandbox{}, fmt.Errorf("%w: Sandboxes is disabled", ErrForbidden)
	}
	if h.config.Sandboxes.SuperSandboxAdmissionDisabled {
		return SuperSandbox{}, fmt.Errorf("%w: Super Sandbox admission is currently disabled", ErrForbidden)
	}
	if h.config.Sandboxes.SuperSandboxReconcilePaused {
		return SuperSandbox{}, fmt.Errorf("%w: Super Sandbox reconciliation is currently paused", ErrForbidden)
	}
	if account == "" {
		return SuperSandbox{}, fmt.Errorf("%w: a linked human owner/operator is required for a durable Super Sandbox", ErrForbidden)
	}
	input, reserved, hash, err := normalizeSuperSandbox(input)
	if err != nil {
		return SuperSandbox{}, err
	}
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SuperSandbox{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.lockWorkspace(ctx, tx, workspace, account); err != nil {
		return SuperSandbox{}, err
	}
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return SuperSandbox{}, err
	}
	if err = s.requireWorkspaceProductForUseTx(ctx, tx, workspace, "sandboxes", account, actor); err != nil {
		return SuperSandbox{}, err
	}
	keyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(input.IdempotencyKey)))
	var priorID, priorHash string
	err = tx.QueryRow(ctx, `SELECT id,request_hash FROM super_sandboxes WHERE workspace_id=$1 AND key_hash=$2`, workspace, keyHash).Scan(&priorID, &priorHash)
	if err == nil {
		if priorHash != hash {
			return SuperSandbox{}, fmt.Errorf("%w: idempotency key already used with a different payload", ErrConflict)
		}
		return s.superSandbox(ctx, tx, workspace, priorID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SuperSandbox{}, err
	}
	id, err := newID("ssb_")
	if err != nil {
		return SuperSandbox{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO super_sandboxes(id,workspace_id,creator_account_id,name,max_concurrency,max_worker_seconds,reserved_worker_seconds,key_hash,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, workspace, account, input.Name, input.MaxConcurrency, input.MaxWorkerSeconds, reserved, keyHash, hash)
	if err != nil {
		return SuperSandbox{}, err
	}
	for i, worker := range input.WorkerIDs {
		var generation int
		var environmentJSON, setupJSON []byte
		err = tx.QueryRow(ctx, `SELECT generation,environment,setup FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND phase='ready' AND NOT sleep_requested AND NOT delete_requested FOR SHARE`, workspace, worker).Scan(&generation, &environmentJSON, &setupJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return SuperSandbox{}, fmt.Errorf("%w: all workers must be existing ready sandboxes in this organization", ErrConflict)
		}
		if err != nil {
			return SuperSandbox{}, err
		}
		var environment SandboxEnvironment
		if len(environmentJSON) > 0 {
			if err = json.Unmarshal(environmentJSON, &environment); err != nil {
				return SuperSandbox{}, err
			}
		}
		var setup SandboxSetup
		if len(setupJSON) > 0 {
			if err = json.Unmarshal(setupJSON, &setup); err != nil {
				return SuperSandbox{}, err
			}
		}
		if sandboxSetupPending(setup) {
			return SuperSandbox{}, fmt.Errorf("%w: workers must finish setup before joining a Super Sandbox", ErrConflict)
		}
		if len(environment.Env) > 0 || len(environment.SecretRefs) > 0 {
			return SuperSandbox{}, fmt.Errorf("%w: Super Sandbox tasks do not inherit recipe environment variables or secret references; select workers without env or secretRefs", ErrConflict)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO super_sandbox_workers(run_id,worker_id,sandbox_id,generation,worker_index) VALUES($1,$2,$2,$3,$4)`, id, worker, generation, i); err != nil {
			return SuperSandbox{}, err
		}
	}
	for i, t := range input.Tasks {
		taskID, err := newID("sst_")
		if err != nil {
			return SuperSandbox{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO super_sandbox_tasks(id,run_id,task_index,name,command,directory,timeout_seconds) VALUES($1,$2,$3,$4,$5,$6,$7)`, taskID, id, i, t.Name, t.Command, t.Directory, t.TimeoutSeconds); err != nil {
			return SuperSandbox{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return SuperSandbox{}, err
	}
	_ = s.Audit(ctx, workspace, actor, "super_sandbox.created", id, map[string]any{"tasks": len(input.Tasks), "workers": len(input.WorkerIDs), "reservedWorkerSeconds": reserved})
	return s.SuperSandbox(ctx, workspace, id)
}

// superSandboxesEnabled reports the admission gate new consumers see; it
// never affects reconciliation or cancellation of existing batches.
func (h *HTTPServer) superSandboxesEnabled() bool {
	return h.config.Sandboxes.Enabled && !h.config.Sandboxes.SuperSandboxAdmissionDisabled && !h.config.Sandboxes.SuperSandboxReconcilePaused
}

type SuperSandboxProgress struct {
	Total              int `json:"total"`
	Settled            int `json:"settled"`
	Succeeded          int `json:"succeeded"`
	Unknown            int `json:"unknown"`
	OutcomeUnavailable int `json:"outcomeUnavailable"`
}

// SuperSandboxSummary is a read-only page row: no command text, task arrays,
// or per-row detail reads. Detail remains the source for exact tasks/IDs.
type SuperSandboxSummary struct {
	ID                    string               `json:"id"`
	Name                  string               `json:"name"`
	State                 string               `json:"state"`
	BlockedReason         string               `json:"blockedReason,omitempty"`
	WorkerCount           int                  `json:"workerCount"`
	MaxConcurrency        int                  `json:"maxConcurrency"`
	MaxWorkerSeconds      int                  `json:"maxWorkerSeconds"`
	ReservedWorkerSeconds int                  `json:"reservedWorkerSeconds"`
	Progress              SuperSandboxProgress `json:"progress"`
	CreatedAt             time.Time            `json:"createdAt"`
	StartedAt             *time.Time           `json:"startedAt,omitempty"`
	FinishedAt            *time.Time           `json:"finishedAt,omitempty"`
}

// SuperSandboxSummaries returns one keyset page ordered by (created_at,id)
// descending, always scoped to the workspace. A nil `after` reads page one.
// superSandboxCursor is the decoded keyset position; its wire form is
// base64url "<unixnano>|<id>" and malformed input is rejected, never ignored.
type superSandboxCursor struct {
	CreatedAt time.Time
	ID        string
}

func (s *Store) SuperSandboxSummaries(ctx context.Context, workspace string, limit int, after *superSandboxCursor) ([]SuperSandboxSummary, error) {
	rows, err := s.pool.Query(ctx, `WITH page AS MATERIALIZED (
 SELECT id FROM super_sandboxes
 WHERE workspace_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id) < ($2::timestamptz,$3::text))
 ORDER BY created_at DESC,id DESC LIMIT $4)
 SELECT r.id,r.name,r.state,r.max_concurrency,r.max_worker_seconds,r.reserved_worker_seconds,r.created_at,r.started_at,r.finished_at,
 w.count,t.total,t.settled,t.succeeded,t.unknown,t.outcome_unavailable,`+superSandboxBlockerSQL+`
 FROM page
 JOIN super_sandboxes r ON r.id=page.id
 JOIN LATERAL (SELECT count(*) AS count FROM super_sandbox_workers sw WHERE sw.run_id=r.id) w ON true
 JOIN LATERAL (SELECT count(*) AS total,
   count(*) FILTER (WHERE bt.state NOT IN ('queued','starting','running','unknown')) AS settled,
   count(*) FILTER (WHERE bt.state='succeeded') AS succeeded,
   count(*) FILTER (WHERE bt.state='unknown') AS unknown,
   count(*) FILTER (WHERE bt.state='outcome_unavailable') AS outcome_unavailable
   FROM super_sandbox_tasks bt WHERE bt.run_id=r.id) t ON true
 ORDER BY r.created_at DESC,r.id DESC`, workspace, nullableTime(after), nullableID(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SuperSandboxSummary{}
	for rows.Next() {
		var v SuperSandboxSummary
		if err = rows.Scan(&v.ID, &v.Name, &v.State, &v.MaxConcurrency, &v.MaxWorkerSeconds, &v.ReservedWorkerSeconds, &v.CreatedAt, &v.StartedAt, &v.FinishedAt, &v.WorkerCount, &v.Progress.Total, &v.Progress.Settled, &v.Progress.Succeeded, &v.Progress.Unknown, &v.Progress.OutcomeUnavailable, &v.BlockedReason); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func nullableTime(after *superSandboxCursor) any {
	if after == nil {
		return nil
	}
	return after.CreatedAt
}

func nullableID(after *superSandboxCursor) any {
	if after == nil {
		return nil
	}
	return after.ID
}

func encodeSuperSandboxCursor(c superSandboxCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + "|" + c.ID))
}

func decodeSuperSandboxCursor(raw string) (superSandboxCursor, error) {
	var c superSandboxCursor
	if len(raw) == 0 || len(raw) > 512 {
		return c, errors.New("malformed cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, err
	}
	nanos, id, ok := strings.Cut(string(data), "|")
	if !ok || !superSandboxIDPattern.MatchString(id) {
		return c, errors.New("malformed cursor")
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return c, err
	}
	c = superSandboxCursor{CreatedAt: time.Unix(0, n).UTC(), ID: id}
	return c, nil
}

func (h *HTTPServer) CancelSuperSandbox(ctx context.Context, workspace, account, id string, actor sdk.ActorRef) (SuperSandbox, error) {
	if account == "" {
		return SuperSandbox{}, ErrForbidden
	}
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SuperSandbox{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.lockWorkspace(ctx, tx, workspace, account); err != nil {
		return SuperSandbox{}, err
	}
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return SuperSandbox{}, err
	}
	var state string
	if err = tx.QueryRow(ctx, `SELECT state FROM super_sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return SuperSandbox{}, ErrNotFound
	} else if err != nil {
		return SuperSandbox{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE super_sandboxes SET state='cancelling' WHERE id=$1 AND finished_at IS NULL`, id); err != nil {
		return SuperSandbox{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return SuperSandbox{}, err
	}
	_ = s.Audit(ctx, workspace, actor, "super_sandbox.cancel_requested", id, map[string]any{})
	return s.SuperSandbox(ctx, workspace, id)
}

func (h *HTTPServer) workspaceSuperSandboxes(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	account := ""
	if p.Account != nil {
		account = p.Account.ID
	}
	if r.Method == http.MethodPost {
		if err := h.allowWorkspace(r, p, workspace, true); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	var value any
	var err error
	status := http.StatusOK
	switch {
	case len(parts) == 0 && r.Method == http.MethodGet && r.URL.Query().Get("view") == "summary":
		limit := 25
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed < 1 || parsed > 100 {
				writeError(w, http.StatusBadRequest, errors.New("limit must be an integer between 1 and 100"))
				return
			}
			limit = parsed
		}
		var after *superSandboxCursor
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			decoded, decodeErr := decodeSuperSandboxCursor(raw)
			if decodeErr != nil {
				writeError(w, http.StatusBadRequest, errors.New("malformed cursor"))
				return
			}
			after = &decoded
		}
		var items []SuperSandboxSummary
		items, err = h.service.Store.SuperSandboxSummaries(r.Context(), workspace, limit+1, after)
		if err == nil {
			next := ""
			if len(items) > limit {
				items = items[:limit]
				last := items[len(items)-1]
				next = encodeSuperSandboxCursor(superSandboxCursor{CreatedAt: last.CreatedAt, ID: last.ID})
			}
			value = map[string]any{"superSandboxes": items, "enabled": h.superSandboxesEnabled(), "nextCursor": next}
		}
	case len(parts) == 0 && r.Method == http.MethodGet:
		var items []SuperSandbox
		items, err = h.service.Store.SuperSandboxes(r.Context(), workspace)
		value = map[string]any{"superSandboxes": items, "enabled": h.superSandboxesEnabled()}
	case len(parts) == 0 && r.Method == http.MethodPost:
		var in SuperSandboxCreateInput
		if !decodeLimit(w, r, &in, 256<<10) {
			return
		}
		value, err = h.CreateSuperSandbox(r.Context(), workspace, account, p.Actor, in)
		status = http.StatusAccepted
	case len(parts) == 1 && r.Method == http.MethodGet:
		value, err = h.service.Store.SuperSandbox(r.Context(), workspace, parts[0])
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost:
		value, err = h.CancelSuperSandbox(r.Context(), workspace, account, parts[0], p.Actor)
	default:
		writeError(w, http.StatusNotFound, ErrNotFound)
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, status, value)
}
