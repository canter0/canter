package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

const (
	defaultWorkspaceIndexPageSize = 50
	maxWorkspaceIndexPageSize     = 100
)

type workspaceIndexCursor struct {
	WorkspaceID string    `json:"workspaceId"`
	Kind        string    `json:"kind"`
	CreatedAt   time.Time `json:"createdAt"`
	ID          string    `json:"id"`
}

type ChangeIndexPage struct {
	Items       []ChangeIndex `json:"changes"`
	NextCursor  string        `json:"nextCursor,omitempty"`
	HasMore     bool          `json:"hasMore"`
	PendingOnly bool          `json:"pendingOnly,omitempty"`
}

type InitialDeploymentIndexPage struct {
	Items      []InitialDeploymentIndex `json:"initialDeployments"`
	NextCursor string                   `json:"nextCursor,omitempty"`
	HasMore    bool                     `json:"hasMore"`
}

func workspaceIndexPageSize(limit int) int {
	if limit <= 0 {
		return defaultWorkspaceIndexPageSize
	}
	if limit > maxWorkspaceIndexPageSize {
		return maxWorkspaceIndexPageSize
	}
	return limit
}

func decodeWorkspaceIndexCursor(raw, workspaceID, kind string) (workspaceIndexCursor, error) {
	if raw == "" {
		return workspaceIndexCursor{}, nil
	}
	if len(raw) > 512 {
		return workspaceIndexCursor{}, fmt.Errorf("invalid cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return workspaceIndexCursor{}, fmt.Errorf("invalid cursor")
	}
	var cursor workspaceIndexCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ID == "" || cursor.CreatedAt.IsZero() || cursor.WorkspaceID != workspaceID || (cursor.Kind != kind && !(kind != "pendingChanges" && cursor.Kind == "")) {
		return workspaceIndexCursor{}, fmt.Errorf("invalid cursor")
	}
	return cursor, nil
}

func encodeWorkspaceIndexCursor(workspaceID, kind string, createdAt time.Time, id string) string {
	data, _ := json.Marshal(workspaceIndexCursor{WorkspaceID: workspaceID, Kind: kind, CreatedAt: createdAt, ID: id})
	return base64.RawURLEncoding.EncodeToString(data)
}

func (s *Store) RecordChange(ctx context.Context, workspaceID string, change sdk.Change) error {
	raw, err := json.Marshal(change)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO change_records(workspace_id,system_name,change_id,phase,summary,digest,document,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(workspace_id,system_name,change_id) DO UPDATE SET phase=EXCLUDED.phase,summary=EXCLUDED.summary,digest=EXCLUDED.digest,document=EXCLUDED.document,updated_at=EXCLUDED.updated_at`, workspaceID, change.System, change.ID, change.Phase, change.Summary, change.Digest, raw, change.CreatedAt, change.UpdatedAt)
	return err
}

func (s *Store) ListChanges(ctx context.Context, workspaceID string) ([]ChangeIndex, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.change_id,c.system_name,c.phase,c.summary,c.digest,COALESCE(e.id,''),COALESCE(e.phase,'') FROM change_records c LEFT JOIN executions e ON e.workspace_id=c.workspace_id AND e.system_name=c.system_name AND e.change_id=c.change_id WHERE c.workspace_id=$1 ORDER BY c.created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChangeIndex{}
	for rows.Next() {
		var c ChangeIndex
		if err := rows.Scan(&c.ID, &c.System, &c.Phase, &c.Summary, &c.Digest, &c.ExecutionID, &c.ExecutionPhase); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListChangeIndexPage bounds workspace list reads while preserving a stable
// newest-first traversal. The unpaged method remains for internal full-state
// reconstruction paths that require all rows.
func (s *Store) ListChangeIndexPage(ctx context.Context, workspaceID string, limit int, rawCursor string) (ChangeIndexPage, error) {
	return s.listChangeIndexPage(ctx, workspaceID, limit, rawCursor, false)
}

func (s *Store) ListPendingChangeIndexPage(ctx context.Context, workspaceID string, limit int, rawCursor string) (ChangeIndexPage, error) {
	page, err := s.listChangeIndexPage(ctx, workspaceID, limit, rawCursor, true)
	page.PendingOnly = true
	return page, err
}

func (s *Store) listChangeIndexPage(ctx context.Context, workspaceID string, limit int, rawCursor string, pendingOnly bool) (ChangeIndexPage, error) {
	pageSize := workspaceIndexPageSize(limit)
	kind := "changes"
	if pendingOnly {
		kind = "pendingChanges"
	}
	cursor, err := decodeWorkspaceIndexCursor(rawCursor, workspaceID, kind)
	if err != nil {
		return ChangeIndexPage{}, err
	}
	where := `c.workspace_id=$1`
	if pendingOnly {
		where += ` AND c.phase NOT IN ('committed','rejected','reverted')`
	}
	query := `SELECT c.change_id,c.system_name,c.phase,c.summary,c.digest,COALESCE(e.id,''),COALESCE(e.phase,''),c.created_at FROM change_records c LEFT JOIN executions e ON e.workspace_id=c.workspace_id AND e.system_name=c.system_name AND e.change_id=c.change_id WHERE ` + where + ` ORDER BY c.created_at DESC,c.change_id DESC LIMIT $2`
	args := []any{workspaceID, pageSize + 1}
	if !cursor.CreatedAt.IsZero() {
		query = `SELECT c.change_id,c.system_name,c.phase,c.summary,c.digest,COALESCE(e.id,''),COALESCE(e.phase,''),c.created_at FROM change_records c LEFT JOIN executions e ON e.workspace_id=c.workspace_id AND e.system_name=c.system_name AND e.change_id=c.change_id WHERE ` + where + ` AND (c.created_at,c.change_id)<($2,$3) ORDER BY c.created_at DESC,c.change_id DESC LIMIT $4`
		args = []any{workspaceID, cursor.CreatedAt, cursor.ID, pageSize + 1}
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return ChangeIndexPage{}, err
	}
	defer rows.Close()
	page := ChangeIndexPage{Items: make([]ChangeIndex, 0, pageSize)}
	page.PendingOnly = pendingOnly
	var lastTime time.Time
	var lastID string
	for rows.Next() {
		var item ChangeIndex
		var createdAt time.Time
		if err := rows.Scan(&item.ID, &item.System, &item.Phase, &item.Summary, &item.Digest, &item.ExecutionID, &item.ExecutionPhase, &createdAt); err != nil {
			return ChangeIndexPage{}, err
		}
		if len(page.Items) == pageSize {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, item)
		lastTime, lastID = createdAt, item.ID
	}
	if err := rows.Err(); err != nil {
		return ChangeIndexPage{}, err
	}
	if page.HasMore && lastID != "" {
		page.NextCursor = encodeWorkspaceIndexCursor(workspaceID, kind, lastTime, lastID)
	}
	return page, nil
}

func (s *Store) ExecutionForChange(ctx context.Context, workspaceID, systemName, changeID string) (Execution, error) {
	var e Execution
	err := s.pool.QueryRow(ctx, `SELECT id,workspace_id,system_name,change_id,phase,requested_by_kind,requested_by_id,requested_session_id,attempts,available_at,COALESCE(claimed_by,''),lease_expires_at,failure,created_at,started_at,completed_at FROM executions WHERE workspace_id=$1 AND system_name=$2 AND change_id=$3`, workspaceID, systemName, changeID).Scan(&e.ID, &e.WorkspaceID, &e.SystemName, &e.ChangeID, &e.Phase, &e.RequestedBy.Kind, &e.RequestedBy.ID, &e.RequestedBy.SessionID, &e.Attempts, &e.AvailableAt, &e.ClaimedBy, &e.LeaseExpiresAt, &e.Failure, &e.CreatedAt, &e.StartedAt, &e.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) GetRecordedChange(ctx context.Context, workspaceID, systemName, changeID string) (sdk.Change, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT document FROM change_records WHERE workspace_id=$1 AND system_name=$2 AND change_id=$3`, workspaceID, systemName, changeID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return sdk.Change{}, ErrNotFound
	}
	if err != nil {
		return sdk.Change{}, err
	}
	var change sdk.Change
	if err = json.Unmarshal(raw, &change); err != nil {
		return sdk.Change{}, err
	}
	return change, nil
}

func (s *Store) EnqueueExecution(ctx context.Context, workspaceID, systemName, changeID string, actor sdk.ActorRef) (Execution, error) {
	id, _ := newID("exe_")
	now := s.now()
	var e Execution
	err := s.pool.QueryRow(ctx, `INSERT INTO executions(id,workspace_id,system_name,change_id,phase,requested_by_kind,requested_by_id,requested_session_id,available_at,created_at) VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$8,$8) ON CONFLICT(workspace_id,system_name,change_id) DO UPDATE SET change_id=EXCLUDED.change_id RETURNING id,workspace_id,system_name,change_id,phase,requested_by_kind,requested_by_id,requested_session_id,attempts,available_at,COALESCE(claimed_by,''),lease_expires_at,failure,created_at,started_at,completed_at`, id, workspaceID, systemName, changeID, actor.Kind, actor.ID, actor.SessionID, now).Scan(&e.ID, &e.WorkspaceID, &e.SystemName, &e.ChangeID, &e.Phase, &e.RequestedBy.Kind, &e.RequestedBy.ID, &e.RequestedBy.SessionID, &e.Attempts, &e.AvailableAt, &e.ClaimedBy, &e.LeaseExpiresAt, &e.Failure, &e.CreatedAt, &e.StartedAt, &e.CompletedAt)
	return e, err
}

func (s *Store) Execution(ctx context.Context, id string) (Execution, error) {
	var e Execution
	err := s.pool.QueryRow(ctx, `SELECT id,workspace_id,system_name,change_id,phase,requested_by_kind,requested_by_id,requested_session_id,attempts,available_at,COALESCE(claimed_by,''),lease_expires_at,failure,created_at,started_at,completed_at FROM executions WHERE id=$1`, id).Scan(&e.ID, &e.WorkspaceID, &e.SystemName, &e.ChangeID, &e.Phase, &e.RequestedBy.Kind, &e.RequestedBy.ID, &e.RequestedBy.SessionID, &e.Attempts, &e.AvailableAt, &e.ClaimedBy, &e.LeaseExpiresAt, &e.Failure, &e.CreatedAt, &e.StartedAt, &e.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) ClaimExecution(ctx context.Context, worker string, lease time.Duration) (Execution, bool, error) {
	if worker == "" || lease <= 0 {
		return Execution{}, false, errors.New("execution worker and positive lease are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Execution{}, false, err
	}
	defer tx.Rollback(ctx)
	var id string
	now := s.now()
	err = tx.QueryRow(ctx, `SELECT id FROM executions WHERE (phase='queued' AND available_at<=$1) OR (phase='running' AND lease_expires_at<=$1) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Execution{}, false, nil
	}
	if err != nil {
		return Execution{}, false, err
	}
	claimToken, err := newSecret("fence_", 24)
	if err != nil {
		return Execution{}, false, err
	}
	var e Execution
	err = tx.QueryRow(ctx, `UPDATE executions SET phase='running',claimed_by=$1,claim_token=$2,lease_expires_at=$3,attempts=attempts+1,started_at=COALESCE(started_at,$4),failure='' WHERE id=$5 RETURNING id,workspace_id,system_name,change_id,phase,requested_by_kind,requested_by_id,requested_session_id,attempts,available_at,claimed_by,claim_token,lease_expires_at,failure,created_at,started_at,completed_at`, worker, claimToken, now.Add(lease), now, id).Scan(&e.ID, &e.WorkspaceID, &e.SystemName, &e.ChangeID, &e.Phase, &e.RequestedBy.Kind, &e.RequestedBy.ID, &e.RequestedBy.SessionID, &e.Attempts, &e.AvailableAt, &e.ClaimedBy, &e.ClaimToken, &e.LeaseExpiresAt, &e.Failure, &e.CreatedAt, &e.StartedAt, &e.CompletedAt)
	if err != nil {
		return Execution{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Execution{}, false, err
	}
	return e, true, nil
}

func (s *Store) RenewExecution(ctx context.Context, id, worker, claimToken string, lease time.Duration) error {
	if lease <= 0 || claimToken == "" {
		return ErrConflict
	}
	now := s.now()
	result, err := s.pool.Exec(ctx, `UPDATE executions SET lease_expires_at=$1 WHERE id=$2 AND phase='running' AND claimed_by=$3 AND claim_token=$4 AND lease_expires_at>$5`, now.Add(lease), id, worker, claimToken, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) CompleteExecution(ctx context.Context, id, worker, claimToken string, applyErr error) error {
	if claimToken == "" {
		return ErrConflict
	}
	phase, failure := "succeeded", ""
	if applyErr != nil {
		phase = "failed"
		failure = applyErr.Error()
	}
	now := s.now()
	result, err := s.pool.Exec(ctx, `UPDATE executions SET phase=$1,failure=$2,completed_at=$3,lease_expires_at=NULL WHERE id=$4 AND phase='running' AND claimed_by=$5 AND claim_token=$6 AND lease_expires_at>$3`, phase, failure, now, id, worker, claimToken)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
