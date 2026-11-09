package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Conversation struct {
	ID             string    `json:"id"`
	WorkspaceID    string    `json:"workspaceId"`
	AccountID      string    `json:"-"`
	Shared         bool      `json:"shared,omitempty"`
	OwnerAccountID string    `json:"ownerAccountId,omitempty"`
	OwnerEmail     string    `json:"ownerEmail,omitempty"`
	Title          string    `json:"title"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Status         string    `json:"status"`
}
type OperatorMessage struct {
	ReplyTo       *WrenReplyTarget     `json:"replyTo,omitempty"`
	OriginKind    string               `json:"originKind,omitempty"`
	HandoffStatus string               `json:"handoffStatus,omitempty"`
	Speaker       *Wren                `json:"speaker,omitempty"`
	SenderName    string               `json:"senderName,omitempty"`
	TurnID        string               `json:"turnId,omitempty"`
	ID            string               `json:"id"`
	RunID         string               `json:"runId"`
	Role          string               `json:"role"`
	Content       string               `json:"content"`
	Attachments   []OperatorAttachment `json:"attachments,omitempty"`
	Surface       *OperatorSurface     `json:"surface,omitempty"`
	CreatedAt     time.Time            `json:"createdAt"`
}
type OperatorEvent struct {
	Sequence  int64           `json:"sequence"`
	RunID     string          `json:"runId,omitempty"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"createdAt"`
}
type OperatorRun struct {
	Speaker            *Wren                `json:"speaker,omitempty"`
	SenderName         string               `json:"senderName,omitempty"`
	RequesterAccountID string               `json:"-"`
	TurnID             string               `json:"turnId,omitempty"`
	ID                 string               `json:"id"`
	ConversationID     string               `json:"conversationId"`
	Status             string               `json:"status"`
	Model              string               `json:"model"`
	ModelOptions       OperatorModelOptions `json:"modelOptions"`
	Failure            string               `json:"failure,omitempty"`
	Checkpoint         []modelMessage       `json:"-"`
	Steps              int                  `json:"-"`
	Lease              string               `json:"-"`
}

func (s *Store) Conversations(ctx context.Context, workspace, account string) ([]Conversation, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.workspace_id,c.account_id,c.title,c.updated_at,COALESCE((SELECT status FROM operator_runs WHERE conversation_id=c.id ORDER BY created_at DESC LIMIT 1),'idle'),cs.conversation_id IS NOT NULL,COALESCE(o.email,'') FROM operator_conversations c JOIN accounts o ON o.id=c.account_id LEFT JOIN conversation_shares cs ON cs.conversation_id=c.id WHERE c.workspace_id=$1 AND (c.account_id=$2 OR cs.conversation_id IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM wren_rooms rr WHERE rr.conversation_id=c.id) AND NOT EXISTS(SELECT 1 FROM wren_jobs j WHERE j.child_conversation_id=c.id) AND NOT EXISTS(SELECT 1 FROM wrens w WHERE w.conversation_id=c.id) AND NOT EXISTS(SELECT 1 FROM wren_shared_chats sc WHERE sc.conversation_id=c.id) ORDER BY c.updated_at DESC,c.id LIMIT 100`, workspace, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err = rows.Scan(&c.ID, &c.WorkspaceID, &c.OwnerAccountID, &c.Title, &c.UpdatedAt, &c.Status, &c.Shared, &c.OwnerEmail); err != nil {
			return nil, err
		}
		c.AccountID = account
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) Conversation(ctx context.Context, workspace, account, id string) (Conversation, error) {
	var c Conversation
	err := s.pool.QueryRow(ctx, `SELECT c.id,c.workspace_id,c.account_id,c.title,c.updated_at,EXISTS(SELECT 1 FROM conversation_shares cs WHERE cs.conversation_id=c.id),COALESCE(o.email,'') FROM operator_conversations c JOIN accounts o ON o.id=c.account_id WHERE c.id=$1 AND c.workspace_id=$2 AND `+conversationAccess, id, workspace, account).Scan(&c.ID, &c.WorkspaceID, &c.OwnerAccountID, &c.Title, &c.UpdatedAt, &c.Shared, &c.OwnerEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil {
		c.AccountID = account
		if err = s.ValidateWrenJobConversation(ctx, c); err != nil {
			return Conversation{}, err
		}
	}
	return c, err
}
func (s *Store) CreateConversation(ctx context.Context, workspace, account, id, prompt string) (Conversation, error) {
	if err := s.RequireWorkspaceWriter(ctx, workspace, account); err != nil {
		return Conversation{}, err
	}
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
	return s.EnqueueOperatorWithOptions(ctx, c, request, prompt, model, OperatorModelOptions{}, surface, attachments...)
}

func (s *Store) EnqueueOperatorWithOptions(ctx context.Context, c Conversation, request, prompt, model string, options OperatorModelOptions, surface *OperatorSurface, attachments ...OperatorAttachment) (OperatorRun, error) {
	return s.enqueueOperatorWithRecipients(ctx, c, request, prompt, model, options, surface, nil, false, attachments...)
}
func (s *Store) enqueueOperatorWithRecipients(ctx context.Context, c Conversation, request, prompt, model string, options OperatorModelOptions, surface *OperatorSurface, recipients []string, postOnly bool, attachments ...OperatorAttachment) (OperatorRun, error) {
	return s.enqueueOperatorWithRecipientsAndReply(ctx, c, request, prompt, model, options, surface, recipients, postOnly, "", attachments...)
}
func (s *Store) enqueueOperatorWithRecipientsAndReply(ctx context.Context, c Conversation, request, prompt, model string, options OperatorModelOptions, surface *OperatorSurface, recipients []string, postOnly bool, replyID string, attachments ...OperatorAttachment) (OperatorRun, error) {
	return s.enqueueOperatorWithSource(ctx, c, request, prompt, model, options, surface, recipients, postOnly, replyID, nil, attachments...)
}
func (s *Store) enqueueOperatorWithSource(ctx context.Context, c Conversation, request, prompt, model string, options OperatorModelOptions, surface *OperatorSurface, recipients []string, postOnly bool, replyID string, handoff *roomHandoffSource, attachments ...OperatorAttachment) (OperatorRun, error) {
	if len(replyID) > 100 {
		return OperatorRun{}, fmt.Errorf("invalid reply message ID")
	}

	if err := s.RequireWorkspaceWriter(ctx, c.WorkspaceID, c.AccountID); err != nil {
		return OperatorRun{}, err
	}
	if !postOnly {
		if err := validateOperatorModelOptions(model, options); err != nil {
			return OperatorRun{}, err
		}
	}
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
	var convOwner string
	if err = tx.QueryRow(ctx, `SELECT account_id FROM operator_conversations WHERE id=$1 FOR UPDATE`, c.ID).Scan(&convOwner); err != nil {
		return OperatorRun{}, err
	}
	if err = artifactAccess(ctx, tx, c.WorkspaceID, c.AccountID, c.ID, false); err != nil {
		return OperatorRun{}, err
	}
	if err = requireWorkspaceWriter(ctx, tx, c.WorkspaceID, c.AccountID, true); err != nil {
		return OperatorRun{}, err
	}
	var run OperatorRun
	err = tx.QueryRow(ctx, `SELECT id,conversation_id,status,model,failure,model_options,speaker,sender_name,COALESCE(requester_account_id,''),turn_id FROM operator_runs WHERE conversation_id=$1 AND request_id=$2`, c.ID, request).Scan(&run.ID, &run.ConversationID, &run.Status, &run.Model, &run.Failure, &run.ModelOptions, &run.Speaker, &run.SenderName, &run.RequesterAccountID, &run.TurnID)
	if err == nil {
		if run.RequesterAccountID != "" && run.RequesterAccountID != c.AccountID {
			return run, ErrForbidden
		}
		if handoff != nil {
			var same bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_room_handoffs h JOIN operator_runs r ON r.id=h.target_run_id JOIN operator_messages m ON m.run_id=r.id AND m.role='user' WHERE h.target_run_id=$1 AND h.parent_run_id=$2 AND r.speaker->>'id'=$3 AND m.content=$4)`, run.ID, handoff.Parent.ID, handoff.TargetID, prompt).Scan(&same); err != nil {
				return run, err
			}
			if !same {
				return run, ErrConflict
			}
		}
		var existingReply string
		if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT p.target_message_id FROM wren_message_replies p JOIN operator_messages m ON m.id=p.message_id WHERE m.run_id=$1 AND m.role='user'),'')`, run.ID).Scan(&existingReply); err != nil {
			return run, err
		}
		if existingReply != replyID {
			return run, fmt.Errorf("%w: this request was already sent with a different reply target", ErrConflict)
		}
		return run, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return run, err
	}
	if err = validateWrenLifecycleTx(ctx, tx, c.ID, ""); err != nil {
		return run, err
	}
	if handoff != nil {
		if err = s.validateRoomHandoffTx(ctx, tx, c, handoff); err != nil {
			return run, err
		}
	}
	var reply *WrenReplyTarget
	if replyID != "" {
		target, e := resolveWrenReplyTarget(ctx, tx, c.WorkspaceID, c.AccountID, c.ID, replyID)
		if e != nil {
			return run, e
		}
		reply = &target
		if !postOnly && target.WrenID != "" && !target.CanWake {
			return run, fmt.Errorf("%w: this Wren is no longer in this room; post a people-only reference or choose a current Wren without this reply", ErrConflict)
		}
	}
	routingPrompt := prompt
	if handoff != nil {
		routingPrompt = ""
		recipients = []string{handoff.TargetID}
	}
	if postOnly {
		routingPrompt = ""
		recipients = nil
	}
	if reply != nil && !postOnly && reply.WrenID != "" {
		var isRoom bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_rooms WHERE conversation_id=$1)`, c.ID).Scan(&isRoom); err != nil {
			return run, err
		}
		if isRoom {
			recipients = []string{reply.WrenID}
			routingPrompt = ""
		}
	}
	actors, room, err := s.roomRecipients(ctx, tx, c, routingPrompt, recipients)
	if err != nil {
		return run, err
	}
	for _, actor := range actors {
		if actor != nil {
			if err = validateWrenLifecycleTx(ctx, tx, c.ID, actor.ID); err != nil {
				return run, err
			}
		}
	}
	if postOnly && !room {
		return run, fmt.Errorf("people-only messages require a room")
	}
	if postOnly {
		actors = nil
		model = ""
		options = OperatorModelOptions{}
	}
	if room && surface != nil && surface.Kind == "conversation" {
		return run, fmt.Errorf("private conversation context cannot be attached to a shared room")
	}
	if len(actors) == 0 {
		actors = []*Wren{nil}
	}
	if room || convOwner != c.AccountID {
		if err = tx.QueryRow(ctx, `SELECT email FROM accounts WHERE id=$1 AND disabled_at IS NULL`, c.AccountID).Scan(&run.SenderName); err != nil {
			return run, err
		}
		run.RequesterAccountID = c.AccountID
	}
	if handoff != nil {
		run.SenderName = handoff.From.Name
	}
	var pending int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE conversation_id=$1 AND status='queued'`, c.ID).Scan(&pending); err != nil {
		return run, err
	}
	if !postOnly && pending+len(actors) > 4 {
		return run, fmt.Errorf("%w: four messages are queued; wait for the agent to catch up before sending another", ErrConflict)
	}

	// Bound abuse and spending independently of the language model.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,37))`, c.AccountID); err != nil {
		return run, err
	}
	var recent int
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE COALESCE(r.requester_account_id,c.account_id)=$1 AND r.created_at>now()-interval '1 hour'`, c.AccountID).Scan(&recent); err != nil {
		return run, err
	}
	if recent+len(actors) > 120 {
		return run, fmt.Errorf("too many agent requests; try again later")
	}
	run.ID, err = newID("run_")
	if err != nil {
		return run, err
	}
	run.ConversationID = c.ID
	run.Status = "queued"
	if postOnly {
		run.Status = "completed"
	}
	run.Model = model
	run.ModelOptions = options
	run.Speaker = actors[0]
	if room {
		run.TurnID = run.ID
	}
	if handoff != nil {
		run.TurnID = handoff.TurnID
	}
	_, err = tx.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,model,model_options,speaker,sender_name,requester_account_id,turn_id,status,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,CASE WHEN $10='completed' THEN now() ELSE NULL END)`, run.ID, c.ID, request, model, options, run.Speaker, run.SenderName, run.RequesterAccountID, run.TurnID, run.Status)
	if err != nil {
		return run, err
	}
	if handoff != nil {
		if _, err = tx.Exec(ctx, `UPDATE operator_runs SET origin_kind='handoff' WHERE id=$1`, run.ID); err != nil {
			return run, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO wren_room_handoffs(target_run_id,parent_run_id,from_wren) VALUES($1,$2,$3)`, run.ID, handoff.Parent.ID, handoff.From); err != nil {
			return run, err
		}
	}
	id, err := newID("msg_")
	if err != nil {
		return run, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content,surface,attachments) VALUES($1,$2,$3,'user',$4,$5,$6)`, id, c.ID, run.ID, prompt, surface, attachments)
	if err != nil {
		return run, err
	}
	if reply != nil {
		if _, err = RecordWrenMessageReply(ctx, tx, c.WorkspaceID, c.AccountID, c.ID, id, replyID); err != nil {
			return run, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE operator_conversations SET updated_at=now() WHERE id=$1`, c.ID)
	if err != nil {
		return run, err
	}
	originKind := "human"
	if handoff != nil {
		originKind = "handoff"
	}
	data, _ := json.Marshal(map[string]any{"status": run.Status, "message": OperatorMessage{ID: id, RunID: run.ID, Role: "user", Content: prompt, Surface: surface, CreatedAt: s.now(), ReplyTo: reply, SenderName: run.SenderName, Speaker: run.Speaker, TurnID: run.TurnID, OriginKind: originKind}, "model": model, "modelOptions": options, "speaker": run.Speaker, "turnId": run.TurnID})
	_, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'queued',$3)`, c.ID, run.ID, data)
	if err != nil {
		return run, err
	}
	if postOnly {
		if _, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'finished','{"status":"completed","content":""}')`, c.ID, run.ID); err != nil {
			return run, err
		}
	}
	// Each explicitly selected teammate gets a durable, ordered run. The next
	// reads the previous teammate's saved answer; no bot-to-bot auto-trigger loop.
	for i, actor := range actors[1:] {
		nextID, e := newID("run_")
		if e != nil {
			return run, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,model,model_options,speaker,sender_name,requester_account_id,turn_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,clock_timestamp())`, nextID, c.ID, request+fmt.Sprintf(":%d", i+1), model, options, actor, run.SenderName, run.RequesterAccountID, run.ID)
		if e != nil {
			return run, e
		}
		data, _ := json.Marshal(map[string]any{"status": "queued", "speaker": actor, "turnId": run.ID, "model": model})
		if _, e = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,run_id,kind,data) VALUES($1,$2,'queued',$3)`, c.ID, nextID, data); e != nil {
			return run, e
		}
	}
	return run, tx.Commit(ctx)
}

const (
	operatorMessagePageCount = 50
	operatorMessagePageBytes = 8 << 20
)

var errInvalidOperatorMessageCursor = errors.New("invalid message cursor")
var errOperatorMessageTooLarge = errors.New("a message exceeds the 8 MiB conversation history page limit")

type OperatorMessagePage struct {
	Messages   []OperatorMessage `json:"messages"`
	HasMore    bool              `json:"hasMore"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

func operatorMessageCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "\n" + id))
}

func parseOperatorMessageCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", errInvalidOperatorMessageCursor
	}
	parts := strings.SplitN(string(raw), "\n", 2)
	if len(parts) != 2 || parts[1] == "" || len(parts[1]) > 100 {
		return time.Time{}, "", errInvalidOperatorMessageCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", errInvalidOperatorMessageCursor
	}
	return createdAt.UTC(), parts[1], nil
}

func (s *Store) OperatorMessages(ctx context.Context, id, cursor string) (OperatorMessagePage, error) {
	createdAt, messageID, err := parseOperatorMessageCursor(cursor)
	if err != nil {
		return OperatorMessagePage{}, err
	}
	// Read only metadata for the bounded candidate set first. Attachment bytes
	// are fetched in the second query only for rows that fit the page budget.
	rows, err := s.pool.Query(ctx, `SELECT id,created_at,
		(octet_length(to_json(content)::text)::bigint + char_length(content)::bigint*5 +
		 COALESCE((SELECT sum(octet_length(attachment->>'dataBase64')) FROM jsonb_array_elements(COALESCE(attachments,'[]'::jsonb)) attachment),0) + 16384)::bigint
		FROM operator_messages WHERE conversation_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)<($2,$3))
		ORDER BY created_at DESC,id DESC LIMIT $4`, id, nullableMessageCursor(createdAt), messageID, operatorMessagePageCount+1)
	if err != nil {
		return OperatorMessagePage{}, err
	}
	type candidate struct {
		id        string
		createdAt time.Time
		bytes     int64
	}
	candidates := make([]candidate, 0, operatorMessagePageCount+1)
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.createdAt, &item.bytes); err != nil {
			rows.Close()
			return OperatorMessagePage{}, err
		}
		candidates = append(candidates, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return OperatorMessagePage{}, err
	}
	rows.Close()
	selected := make([]string, 0, operatorMessagePageCount)
	var pageBytes int64
	for _, item := range candidates {
		if len(selected) == operatorMessagePageCount || pageBytes+item.bytes > operatorMessagePageBytes {
			break
		}
		selected = append(selected, item.id)
		pageBytes += item.bytes
	}
	page := OperatorMessagePage{Messages: []OperatorMessage{}, HasMore: len(candidates) > len(selected)}
	if len(selected) == 0 {
		if len(candidates) > 0 {
			return OperatorMessagePage{}, errOperatorMessageTooLarge
		}
		return page, nil
	}
	rows, err = s.pool.Query(ctx, `SELECT m.id,m.run_id,m.role,m.content,m.created_at,m.surface,m.attachments,r.speaker,r.sender_name,r.turn_id,r.origin_kind,CASE WHEN r.origin_kind='handoff' THEN r.status ELSE '' END FROM operator_messages m JOIN operator_runs r ON r.id=m.run_id WHERE m.conversation_id=$1 AND m.id=ANY($2) ORDER BY m.created_at DESC,m.id DESC`, id, selected)
	if err != nil {
		return OperatorMessagePage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var m OperatorMessage
		if err = rows.Scan(&m.ID, &m.RunID, &m.Role, &m.Content, &m.CreatedAt, &m.Surface, &m.Attachments, &m.Speaker, &m.SenderName, &m.TurnID, &m.OriginKind, &m.HandoffStatus); err != nil {
			return OperatorMessagePage{}, err
		}
		page.Messages = append(page.Messages, m)
	}
	if err = rows.Err(); err != nil {
		return OperatorMessagePage{}, err
	}
	rows.Close()
	replies, err := s.wrenMessageRepliesForMessages(ctx, id, selected)
	if err != nil {
		return OperatorMessagePage{}, err
	}
	for i := range page.Messages {
		if reply, ok := replies[page.Messages[i].ID]; ok {
			page.Messages[i].ReplyTo = &reply
		}
	}
	if page.HasMore && len(page.Messages) > 0 {
		last := page.Messages[len(page.Messages)-1]
		page.NextCursor = operatorMessageCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

func nullableMessageCursor(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
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
	err := s.pool.QueryRow(ctx, `SELECT id,conversation_id,status,model,failure,model_options,speaker,sender_name,COALESCE(requester_account_id,''),turn_id FROM operator_runs WHERE conversation_id=$1 ORDER BY CASE status WHEN 'running' THEN 0 WHEN 'queued' THEN 1 ELSE 2 END, CASE WHEN status IN ('queued','running') THEN created_at END ASC, created_at DESC,id DESC LIMIT 1`, id).Scan(&r.ID, &r.ConversationID, &r.Status, &r.Model, &r.Failure, &r.ModelOptions, &r.Speaker, &r.SenderName, &r.RequesterAccountID, &r.TurnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func (s *Store) ownActiveOperatorRun(ctx context.Context, conversation, account string) (*OperatorRun, error) {
	var r OperatorRun
	err := s.pool.QueryRow(ctx, `SELECT r.id,r.conversation_id,r.status,r.model,r.failure,r.model_options,r.speaker,r.sender_name,COALESCE(r.requester_account_id,c.account_id),r.turn_id FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.conversation_id=$1 AND COALESCE(r.requester_account_id,c.account_id)=$2 AND r.status IN ('queued','running') ORDER BY r.created_at,r.id LIMIT 1`, conversation, account).Scan(&r.ID, &r.ConversationID, &r.Status, &r.Model, &r.Failure, &r.ModelOptions, &r.Speaker, &r.SenderName, &r.RequesterAccountID, &r.TurnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

type OperatorActiveRun struct {
	OperatorRun
	CreatedAt time.Time `json:"createdAt"`
}

// Active runs and their event boundary must describe the same database snapshot.
// Clients can then replay old history without reviving work that already ended.
func (s *Store) activeOperatorRuns(ctx context.Context, conversation string) ([]OperatorActiveRun, int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx)
	var sequence int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(sequence),0) FROM operator_events WHERE conversation_id=$1`, conversation).Scan(&sequence); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id,conversation_id,status,model,failure,model_options,speaker,sender_name,turn_id,created_at FROM operator_runs WHERE conversation_id=$1 AND status IN ('queued','running') ORDER BY created_at,id`, conversation)
	if err != nil {
		return nil, 0, err
	}
	runs := []OperatorActiveRun{}
	for rows.Next() {
		var r OperatorActiveRun
		if err = rows.Scan(&r.ID, &r.ConversationID, &r.Status, &r.Model, &r.Failure, &r.ModelOptions, &r.Speaker, &r.SenderName, &r.TurnID, &r.CreatedAt); err != nil {
			rows.Close()
			return nil, 0, err
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return runs, sequence, nil
}

func (s *Store) claimOperator(ctx context.Context) (OperatorRun, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OperatorRun{}, false, err
	}
	defer tx.Rollback(ctx)
	var r OperatorRun
	var raw []byte
	// Different room Wrens can make progress together. Same-Wren assignments,
	// ordered team contributions, and ordinary conversations retain their inbox
	// order. Earlier queued rows block later claims even across dispatchers.
	err = tx.QueryRow(ctx, `SELECT r.id,r.conversation_id,r.status,r.model,r.failure,r.checkpoint,r.steps,r.model_options,r.speaker,r.sender_name,COALESCE(r.requester_account_id,''),r.turn_id
        FROM operator_runs r WHERE r.executor_kind='operator' AND (
          (r.status='running' AND r.lease_expires_at<now()) OR
          (r.status='queued' AND NOT EXISTS(
            SELECT 1 FROM operator_runs earlier
            WHERE earlier.conversation_id=r.conversation_id AND earlier.id<>r.id
            AND (earlier.status='running' OR (earlier.status='queued' AND (earlier.created_at,earlier.id)<(r.created_at,r.id)))
            AND (COALESCE(r.speaker->>'id','')='' OR COALESCE(earlier.speaker->>'id','')=''
              OR earlier.speaker->>'id'=r.speaker->>'id'
              OR (r.turn_id<>'' AND earlier.turn_id=r.turn_id)
              OR NOT EXISTS(SELECT 1 FROM wren_rooms room WHERE room.conversation_id=r.conversation_id))
          ))
        ) ORDER BY r.created_at,r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&r.ID, &r.ConversationID, &r.Status, &r.Model, &r.Failure, &raw, &r.Steps, &r.ModelOptions, &r.Speaker, &r.SenderName, &r.RequesterAccountID, &r.TurnID)
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
		var conflict *pgconn.PgError
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			return r, false, nil
		}
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
	if status == "completed" && strings.HasPrefix(r.ConversationID, "conv_job_") {
		var locked string
		if err = tx.QueryRow(ctx, `SELECT id FROM operator_runs WHERE id=$1 AND lease_token=$2 AND status='running' AND lease_expires_at>now() FOR UPDATE`, r.ID, r.Lease).Scan(&locked); err != nil {
			return ErrConflict
		}
		var pending bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_job_inputs i JOIN wren_jobs j ON j.id=i.job_id WHERE j.child_run_id=$1 AND i.applied_at IS NULL AND j.cancelled_at IS NULL AND NOT j.return_suppressed)`, r.ID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return errWrenJobInputPending
		}
	}
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
	return s.cancelOperatorRequest(ctx, "", "", conversation, "")
}

// Human stops bind to one observed request. The empty workspace/account form is
// reserved for internal conversation-wide cancellation and existing lifecycle callers.
func (s *Store) cancelOperatorRequest(ctx context.Context, workspace, account, conversation, runID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var turn string
	if account != "" {
		if err = authorizeWrenJobOriginTx(ctx, tx, workspace, account, conversation); err != nil {
			return err
		}
		// A stale/retried click must never move on to a newer request. Callers
		// without a run ID can stop their oldest active request for compatibility.
		err = tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(r.turn_id,''),r.id) FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE r.conversation_id=$1 AND COALESCE(r.requester_account_id,c.account_id)=$2 AND (($3<>'' AND r.id=$3) OR ($3='' AND r.status IN ('queued','running'))) ORDER BY r.created_at,r.id LIMIT 1`, conversation, account, runID).Scan(&turn)
		if errors.Is(err, pgx.ErrNoRows) {
			if runID != "" {
				return ErrNotFound
			}
			return tx.Commit(ctx)
		}
		if err != nil {
			return err
		}
	} else {
		var locked string
		if err = tx.QueryRow(ctx, `SELECT id FROM operator_conversations WHERE id=$1 FOR UPDATE`, conversation).Scan(&locked); err != nil {
			return err
		}
	}
	if err = s.cancelWrenJobsForRequestTx(ctx, tx, conversation, account, turn); err != nil {
		return err
	}
	// The conversation lock serializes enqueue, delegation and cancellation.
	// Recheck requester as well as turn identity; a shared room is not ownership.
	const scope = `conversation_id=$1 AND ($2='' OR (COALESCE(requester_account_id,(SELECT account_id FROM operator_conversations WHERE id=$1))=$2 AND COALESCE(NULLIF(turn_id,''),id)=$3))`
	rows, err := tx.Query(ctx, `SELECT id FROM operator_runs WHERE `+scope+` AND executor_kind='acp-opencode' AND status IN ('queued','running')`, conversation, account, turn)
	if err != nil {
		return err
	}
	var remoteIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		remoteIDs = append(remoteIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(remoteIDs) > 0 {
		if err = requestHarnessStopsTx(ctx, tx, remoteIDs); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, `UPDATE operator_runs SET status='cancelled',completed_at=now(),lease_token=NULL,lease_expires_at=NULL WHERE `+scope+` AND executor_kind<>'acp-opencode' AND status IN ('queued','running') RETURNING id`, conversation, account, turn)
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
