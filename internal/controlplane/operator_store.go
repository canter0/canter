package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

type Conversation struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	AccountID   string    `json:"-"`
	Title       string    `json:"title"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Status      string    `json:"status"`
}
type OperatorMessage struct {
	ID          string               `json:"id"`
	RunID       string               `json:"runId"`
	Role        string               `json:"role"`
	Content     string               `json:"content"`
	Attachments []OperatorAttachment `json:"attachments,omitempty"`
	Surface     *OperatorSurface     `json:"surface,omitempty"`
	CreatedAt   time.Time            `json:"createdAt"`
}
type OperatorEvent struct {
	Sequence  int64           `json:"sequence"`
	RunID     string          `json:"runId,omitempty"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"createdAt"`
}
type OperatorRun struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversationId"`
	Status         string         `json:"status"`
	Model          string         `json:"model"`
	Failure        string         `json:"failure,omitempty"`
	Checkpoint     []modelMessage `json:"-"`
	Steps          int            `json:"-"`
	Lease          string         `json:"-"`
}

func (s *Store) Conversations(ctx context.Context, workspace, account string) ([]Conversation, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.workspace_id,c.account_id,c.title,c.updated_at,COALESCE((SELECT status FROM operator_runs WHERE conversation_id=c.id ORDER BY created_at DESC LIMIT 1),'idle') FROM operator_conversations c WHERE workspace_id=$1 AND account_id=$2 ORDER BY updated_at DESC,id LIMIT 100`, workspace, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err = rows.Scan(&c.ID, &c.WorkspaceID, &c.AccountID, &c.Title, &c.UpdatedAt, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) Conversation(ctx context.Context, workspace, account, id string) (Conversation, error) {
	var c Conversation
	err := s.pool.QueryRow(ctx, `SELECT id,workspace_id,account_id,title,updated_at FROM operator_conversations WHERE id=$1 AND workspace_id=$2 AND account_id=$3`, id, workspace, account).Scan(&c.ID, &c.WorkspaceID, &c.AccountID, &c.Title, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return c, err
}
func (s *Store) CreateConversation(ctx context.Context, workspace, account, id, prompt string) (Conversation, error) {
	if !strings.HasPrefix(id, "conv_") || len(id) > 100 {
		return Conversation{}, fmt.Errorf("invalid conversation ID")
	}
	title := []rune(strings.Join(strings.Fields(prompt), " "))
	if len(title) > 80 {
		title = append(title[:77], '.', '.', '.')
	}
	if len(title) == 0 {
		return Conversation{}, fmt.Errorf("message is required")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO operator_conversations(id,workspace_id,account_id,title) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, id, workspace, account, string(title))
	if err != nil {
		return Conversation{}, err
	}
	return s.Conversation(ctx, workspace, account, id)
}
func (s *Store) EnqueueOperator(ctx context.Context, c Conversation, request, prompt, model string, surface *OperatorSurface, attachments ...OperatorAttachment) (OperatorRun, error) {
	if err := validateOperatorAttachments(attachments); err != nil {
		return OperatorRun{}, err
	}
	if attachments == nil {
		attachments = []OperatorAttachment{}
	}
	if len(request) < 8 || len(request) > 100 || len(strings.TrimSpace(prompt)) == 0 || len(prompt) > 24000 {
		return OperatorRun{}, fmt.Errorf("a request ID and a message of up to 24000 bytes are required")
	}
	if err := validateOperatorSurface(surface); err != nil {
		return OperatorRun{}, err
	}
	if surface != nil && surface.Kind == "conversation" {
		if surface.ID == "" {
			return OperatorRun{}, fmt.Errorf("select a conversation")
		}
		if _, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, surface.ID); err != nil {
			return OperatorRun{}, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperatorRun{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM operator_conversations WHERE id=$1 FOR UPDATE`, c.ID).Scan(&locked); err != nil {
		return OperatorRun{}, err
	}
	var run OperatorRun
	err = tx.QueryRow(ctx, `SELECT id,conversation_id,status,model,failure FROM operator_runs WHERE conversation_id=$1 AND request_id=$2`, c.ID, request).Scan(&run.ID, &run.ConversationID, &run.Status, &run.Model, &run.Failure)
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return run, err
	}
	var pending int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE conversation_id=$1 AND status='queued'`, c.ID).Scan(&pending); err != nil {
		return run, err
	}
	if pending >= 4 {
		return run, fmt.Errorf("%w: four messages are queued; wait for the agent to catch up before sending another", ErrConflict)
	}

	// Bound abuse and spending independently of the language model.
	var recent int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE c.account_id=$1 AND r.created_at>now()-interval '1 hour'`, c.AccountID).Scan(&recent); err != nil {
		return run, err
	}
	if recent >= 120 {
		return run, fmt.Errorf("too many agent requests; try again later")
	}
	run.ID, err = newID("run_")
	if err != nil {
		return run, err
	}
	run.ConversationID = c.ID
	run.Status = "queued"
	run.Model = model
	_, err = tx.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,model) VALUES($1,$2,$3,$4)`, run.ID, c.ID, request, model)
	if err != nil {
		return run, err
	}
	id, err := newID("msg_")
	if err != nil {
		return run, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content,surface,attachments) VALUES($1,$2,$3,'user',$4,$5,$6)`, id, c.ID, run.ID, prompt, surface, attachments)
	if err != nil {
		return run, err
	}
	_, err = tx.Exec(ctx, `UPDATE operator_conversations SET updated_at=now() WHERE id=$1`, c.ID)
	if err != nil {
		return run, err
	}
	data, _ := json.Marshal(map[string]any{"status": "queued", "message": OperatorMessage{ID: id, RunID: run.ID, Role: "user", Content: prompt, Surface: surface, CreatedAt: s.now()}, "model": model})
	_, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'queued',$3)`, c.ID, run.ID, data)
	if err != nil {
		return run, err
	}
	return run, tx.Commit(ctx)
}
func (s *Store) OperatorMessages(ctx context.Context, id string) ([]OperatorMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,run_id,role,content,created_at,surface,attachments FROM operator_messages WHERE conversation_id=$1 ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OperatorMessage{}
	for rows.Next() {
		var m OperatorMessage
		if err = rows.Scan(&m.ID, &m.RunID, &m.Role, &m.Content, &m.CreatedAt, &m.Surface, &m.Attachments); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) OperatorEvents(ctx context.Context, id string, after int64) ([]OperatorEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT sequence,COALESCE(run_id,''),kind,data,created_at FROM operator_events WHERE conversation_id=$1 AND sequence>$2 ORDER BY sequence LIMIT 300`, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OperatorEvent{}
	for rows.Next() {
		var e OperatorEvent
		if err = rows.Scan(&e.Sequence, &e.RunID, &e.Kind, &e.Data, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) LatestOperatorRun(ctx context.Context, id string) (*OperatorRun, error) {
	var r OperatorRun
	err := s.pool.QueryRow(ctx, `SELECT id,conversation_id,status,model,failure FROM operator_runs WHERE conversation_id=$1 ORDER BY CASE status WHEN 'running' THEN 0 WHEN 'queued' THEN 1 ELSE 2 END, CASE WHEN status IN ('queued','running') THEN created_at END ASC, created_at DESC,id DESC LIMIT 1`, id).Scan(&r.ID, &r.ConversationID, &r.Status, &r.Model, &r.Failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}
func (s *Store) claimOperator(ctx context.Context) (OperatorRun, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperatorRun{}, false, err
	}
	defer tx.Rollback(ctx)
	var r OperatorRun
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT r.id,r.conversation_id,r.status,r.model,r.failure,r.checkpoint,r.steps FROM operator_runs r WHERE (r.status='running' AND r.lease_expires_at<now()) OR (r.status='queued' AND NOT EXISTS(SELECT 1 FROM operator_runs earlier WHERE earlier.conversation_id=r.conversation_id AND earlier.id<>r.id AND (earlier.status='running' OR (earlier.status='queued' AND (earlier.created_at,earlier.id)<(r.created_at,r.id))))) ORDER BY r.created_at,r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&r.ID, &r.ConversationID, &r.Status, &r.Model, &r.Failure, &raw, &r.Steps)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err = json.Unmarshal(raw, &r.Checkpoint); err != nil {
		return r, false, err
	}
	r.Lease, err = newID("lease_")
	if err != nil {
		return r, false, err
	}
	r.Status = "running"
	_, err = tx.Exec(ctx, `UPDATE operator_runs SET status='running',lease_token=$2,lease_expires_at=now()+interval '45 seconds' WHERE id=$1`, r.ID, r.Lease)
	if err != nil {
		return r, false, err
	}
	return r, true, tx.Commit(ctx)
}
func (s *Store) operatorCheckpoint(ctx context.Context, r OperatorRun, messages []modelMessage) error {
	raw, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx, `UPDATE operator_runs SET checkpoint=$3,steps=$4 WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now()`, r.ID, r.Lease, raw, r.Steps)
	if err == nil && result.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
func (s *Store) operatorEvent(ctx context.Context, r OperatorRun, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) SELECT conversation_id,id,$3,$4 FROM operator_runs WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now()`, r.ID, r.Lease, kind, raw)
	if err == nil && result.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
func (s *Store) finishOperator(ctx context.Context, r OperatorRun, status, content string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE operator_runs SET status=$3,completed_at=now(),failure=$4,lease_token=NULL,lease_expires_at=NULL WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now()`, r.ID, r.Lease, status, func() string {
		if status == "failed" {
			return content
		}
		return ""
	}())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	if status == "completed" {
		id, err := newID("msg_")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES($1,$2,$3,'assistant',$4) ON CONFLICT(run_id,role) DO NOTHING`, id, r.ConversationID, r.ID, content)
		if err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(map[string]string{"status": status, "content": content})
	_, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'finished',$3)`, r.ConversationID, r.ID, raw)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE operator_conversations SET updated_at=now() WHERE id=$1`, r.ConversationID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) cancelOperator(ctx context.Context, conversation string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `UPDATE operator_runs SET status='cancelled',completed_at=now(),lease_token=NULL,lease_expires_at=NULL WHERE conversation_id=$1 AND status IN ('queued','running') RETURNING id`, conversation)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'finished','{"status":"cancelled","content":"Response stopped. Already submitted operations retain their own state."}')`, conversation, id)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Each person's hosted operator is a durable agent identity, not their human
// browser session. Recheck membership and revocation before every tool call.
func (s *Store) operatorPrincipal(ctx context.Context, c Conversation, runID string) (Principal, error) {
	role, err := s.Membership(ctx, c.AccountID, c.WorkspaceID)
	if err != nil {
		return Principal{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Principal{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize first-use creation without relying on a client-held credential.
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, c.WorkspaceID).Scan(&locked); err != nil {
		return Principal{}, err
	}
	var installationID string
	err = tx.QueryRow(ctx, `SELECT installation_id FROM operator_grants WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID).Scan(&installationID)
	if errors.Is(err, pgx.ErrNoRows) {
		installationID, err = newID("agt_")
		if err != nil {
			return Principal{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_installations(id,workspace_id,name,harness,inspect_allowed,draft_allowed,apply_mode,created_by) VALUES($1,$2,'Canter','canter-hosted',true,$3,'human-approval-required',$4)`, installationID, c.WorkspaceID, role != "viewer", c.AccountID)
		if err != nil {
			return Principal{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO operator_grants(workspace_id,account_id,installation_id) VALUES($1,$2,$3)`, c.WorkspaceID, c.AccountID, installationID)
	}
	if err != nil {
		return Principal{}, err
	}
	installation, err := installationByID(ctx, tx, installationID)
	if err != nil {
		return Principal{}, err
	}
	if installation.RevokedAt != nil {
		return Principal{}, fmt.Errorf("%w: the Canter agent's access has been revoked", ErrForbidden)
	}
	if !installation.Authority.Inspect {
		return Principal{}, ErrForbidden
	}
	installation.Authority.Draft = installation.Authority.Draft && role != "viewer"
	sessionID, err := newID("ass_")
	if err != nil {
		return Principal{}, err
	}
	secret, err := newID("unused_")
	if err != nil {
		return Principal{}, err
	}
	session := AgentSession{ID: sessionID, InstallationID: installationID, ClientInstance: runID, CreatedAt: s.now(), ExpiresAt: s.now().Add(15 * time.Minute)}
	_, err = tx.Exec(ctx, `INSERT INTO agent_sessions(id,installation_id,access_hash,client_instance,created_at,last_seen_at,expires_at) VALUES($1,$2,$3,$4,$5,$5,$6)`, sessionID, installationID, secretHash(secret), runID, session.CreatedAt, session.ExpiresAt)
	if err != nil {
		return Principal{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Principal{}, err
	}
	return Principal{WorkspaceID: c.WorkspaceID, Installation: &installation, Session: &session, Actor: sdk.ActorRef{Kind: "agent", ID: installationID, SessionID: sessionID}}, nil
}
