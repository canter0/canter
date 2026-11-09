package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// No account-mail credentials or default sending address are inherited here.
type WrenMessagingConfig struct{ Provider WrenMessageProvider }
type WrenMessageConnection struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	OwnerID     string `json:"ownerId"`
	WrenKnowledgeScope
	Name              string     `json:"name"`
	Provider          string     `json:"provider"`
	From              string     `json:"from"`
	ReceivingAddress  string     `json:"receivingAddress"`
	ResponsibilityID  string     `json:"responsibilityId,omitempty"`
	WebhookConfigured bool       `json:"webhookConfigured"`
	Enabled           bool       `json:"enabled"`
	Revision          int        `json:"revision"`
	RevokedAt         *time.Time `json:"revokedAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
}
type WrenMessageConnectionInput struct {
	ID string `json:"id"`
	WrenKnowledgeScope
	Name               string `json:"name"`
	From               string `json:"from"`
	APIKey             string `json:"apiKey"`
	WebhookSecret      string `json:"webhookSecret"`
	ClearWebhookSecret bool   `json:"clearWebhookSecret"`
	ReceivingAddress   string `json:"receivingAddress"`
	ResponsibilityID   string `json:"responsibilityId"`
	ExpectedRevision   int    `json:"expectedRevision"`
	Enabled            bool   `json:"enabled"`
	Revoke             bool   `json:"revoke"`
}
type WrenMessageArtifactRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
	SHA256   string `json:"sha256"`
}
type WrenMessageContent struct {
	To          []string                 `json:"to"`
	CC          []string                 `json:"cc"`
	BCC         []string                 `json:"bcc"`
	Subject     string                   `json:"subject"`
	Text        string                   `json:"text"`
	ReplyTo     string                   `json:"replyTo,omitempty"`
	InReplyToID string                   `json:"inReplyToId,omitempty"`
	Attachments []WrenMessageArtifactRef `json:"attachments"`
}
type WrenMessageDraftInput struct {
	ID               string             `json:"id"`
	ConnectionID     string             `json:"connectionId"`
	InstructionKey   string             `json:"instructionKey"`
	ExpectedRevision int                `json:"expectedRevision"`
	Content          WrenMessageContent `json:"content"`
}
type WrenMessageDraft struct {
	ID             string             `json:"id"`
	ConnectionID   string             `json:"connectionId"`
	WorkspaceID    string             `json:"workspaceId"`
	ConversationID string             `json:"conversationId"`
	CreatorID      string             `json:"creatorId"`
	RunID          string             `json:"runId,omitempty"`
	InstructionKey string             `json:"instructionKey"`
	Revision       int                `json:"revision"`
	Content        WrenMessageContent `json:"content"`
	State          string             `json:"state"`
	ProviderID     string             `json:"providerId,omitempty"`
	Failure        string             `json:"failure,omitempty"`
	DispatchAt     *time.Time         `json:"dispatchAt,omitempty"`
	CreatedAt      time.Time          `json:"createdAt"`
	UpdatedAt      time.Time          `json:"updatedAt"`
}

const wrenMessageConnectionColumns = `id,workspace_id,COALESCE(owner_id,''),scope,name,provider,from_address,enabled,revision,revoked_at,created_at,receiving_address,COALESCE(responsibility_id,''),webhook_ciphertext IS NOT NULL`

func scanWrenMessageConnection(row pgx.Row) (WrenMessageConnection, error) {
	var v WrenMessageConnection
	var scope []byte
	err := row.Scan(&v.ID, &v.WorkspaceID, &v.OwnerID, &scope, &v.Name, &v.Provider, &v.From, &v.Enabled, &v.Revision, &v.RevokedAt, &v.CreatedAt, &v.ReceivingAddress, &v.ResponsibilityID, &v.WebhookConfigured)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(scope, &v.WrenKnowledgeScope)
	return v, err
}

const wrenMessageDraftColumns = `id,connection_id,workspace_id,conversation_id,COALESCE(creator_id,''),COALESCE(run_id,''),instruction_key,revision,content,state,provider_id,failure,dispatch_at,created_at,updated_at`

func scanWrenMessageDraft(row pgx.Row) (WrenMessageDraft, error) {
	var v WrenMessageDraft
	var raw []byte
	err := row.Scan(&v.ID, &v.ConnectionID, &v.WorkspaceID, &v.ConversationID, &v.CreatorID, &v.RunID, &v.InstructionKey, &v.Revision, &raw, &v.State, &v.ProviderID, &v.Failure, &v.DispatchAt, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(raw, &v.Content)
	return v, err
}
func wrenMailAddress(value string) bool {
	if value == "" || len(value) > 254 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	a, err := mail.ParseAddress(value)
	return err == nil && a.Address == value && strings.Contains(value, "@")
}
func validateWrenMessageContent(v WrenMessageContent) error {
	if len(v.To) == 0 || len(v.To)+len(v.CC)+len(v.BCC) > 20 || !artifactSafeText(v.Subject, 200) || strings.TrimSpace(v.Subject) == "" || strings.TrimSpace(v.Text) == "" || !utf8.ValidString(v.Text) || len(v.Text) > 64000 || strings.ContainsRune(v.Text, 0) || len(v.Attachments) > 8 {
		return fmt.Errorf("provide recipients, a subject and a message within the correspondence limits")
	}
	seen := map[string]bool{}
	for _, list := range [][]string{v.To, v.CC, v.BCC} {
		for _, a := range list {
			key := strings.ToLower(a)
			if !wrenMailAddress(a) || seen[key] {
				return fmt.Errorf("use distinct email addresses without display names")
			}
			seen[key] = true
		}
	}
	if v.ReplyTo != "" && !wrenMailAddress(v.ReplyTo) {
		return fmt.Errorf("invalid reply address")
	}
	if v.InReplyToID != "" && !knowledgeID(v.InReplyToID, "mailin_") {
		return fmt.Errorf("choose an actual received message to reply to")
	}
	for _, a := range v.Attachments {
		if !validArtifactID(a.ID) || a.Revision < 1 || len(a.SHA256) != 64 {
			return fmt.Errorf("choose an exact saved file revision")
		}
	}
	return nil
}
func wrenMessageAAD(workspace, id string) []byte {
	return []byte("canter:wren-correspondence:v1\x00" + workspace + "\x00" + id)
}
func wrenMessageWebhookAAD(workspace, id string) []byte {
	return append(wrenMessageAAD(workspace, id), []byte(":webhook")...)
}
func (h *HTTPServer) SaveWrenMessageConnection(ctx context.Context, workspace, account string, in WrenMessageConnectionInput) (WrenMessageConnection, error) {
	var zero WrenMessageConnection
	if in.ID == "" {
		var e error
		in.ID, e = newID("mailconn_")
		if e != nil {
			return zero, e
		}
	}
	if !knowledgeID(in.ID, "mailconn_") || in.ExpectedRevision < 0 || !artifactSafeText(in.Name, 100) || strings.TrimSpace(in.Name) == "" {
		return zero, fmt.Errorf("name this email connection")
	}
	tx, e := h.service.Store.messageWriteTx(ctx, workspace, account)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback(ctx)
	if e = extensionScopeWrite(ctx, tx, workspace, account, in.WrenKnowledgeScope, !in.Revoke); e != nil {
		return zero, e
	}
	old, e := scanWrenMessageConnection(tx.QueryRow(ctx, `SELECT `+wrenMessageConnectionColumns+` FROM wren_message_connections WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, in.ID, workspace))
	exists := e == nil
	if e != nil && !errors.Is(e, ErrNotFound) {
		return zero, e
	}
	if exists && (old.OwnerID != account || old.RevokedAt != nil) {
		return zero, ErrForbidden
	}
	if exists && (old.Revision != in.ExpectedRevision || old.ConversationID != in.ConversationID || old.Scope != in.Scope || old.WrenID != in.WrenID) {
		return zero, ErrConflict
	}
	if !exists && in.ExpectedRevision != 0 {
		return zero, ErrConflict
	}
	if exists && in.From != old.From {
		return zero, fmt.Errorf("create a separate connection to change sending identity")
	}
	if !wrenMailAddress(in.From) {
		return zero, fmt.Errorf("use the exact verified sending email address")
	}
	if in.ReceivingAddress != "" && !wrenMailAddress(in.ReceivingAddress) {
		return zero, fmt.Errorf("use the exact configured receiving email address")
	}
	if in.ResponsibilityID != "" && !in.Revoke && (!exists || old.ResponsibilityID != in.ResponsibilityID || old.ReceivingAddress != in.ReceivingAddress) {
		if in.ReceivingAddress == "" {
			return zero, fmt.Errorf("configure the receiving address before connecting an email responsibility")
		}
		var id string
		e = tx.QueryRow(ctx, `SELECT id FROM wren_responsibilities WHERE id=$1 AND workspace_id=$2 AND owner_id=$3 AND conversation_id=$4 AND state='active' AND schedule->>'kind'='event' AND schedule->>'eventName'='email_received' FOR SHARE`, in.ResponsibilityID, workspace, account, in.ConversationID).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			return zero, fmt.Errorf("choose your active email_received responsibility in this conversation")
		}
		if e != nil {
			return zero, e
		}
	}
	webhookKey := ""
	var webhookSealed []byte
	if in.WebhookSecret != "" {
		decoded, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(in.WebhookSecret, "whsec_"))
		if e != nil || len(decoded) < 16 || len(decoded) > 128 || in.ClearWebhookSecret {
			clear(decoded)
			return zero, fmt.Errorf("provide a valid webhook signing secret")
		}
		clear(decoded)
		webhookKey, webhookSealed, e = h.config.Secrets.seal([]byte(in.WebhookSecret), wrenMessageWebhookAAD(workspace, in.ID))
		if e != nil {
			return zero, e
		}
	}
	if in.ReceivingAddress != "" && (in.ClearWebhookSecret || in.WebhookSecret == "" && !old.WebhookConfigured) {
		return zero, fmt.Errorf("incoming email needs its own webhook signing secret")
	}
	key := ""
	var sealed []byte
	if !exists || in.APIKey != "" {
		if len(in.APIKey) < 8 || len(in.APIKey) > 4096 || strings.ContainsAny(in.APIKey, "\r\n") {
			return zero, fmt.Errorf("provide the correspondence API credential; account mail credentials are not reused")
		}
		raw, _ := json.Marshal(map[string]string{"apiKey": in.APIKey})
		key, sealed, e = h.config.Secrets.seal(raw, wrenMessageAAD(workspace, in.ID))
		clear(raw)
		if e != nil {
			return zero, e
		}
	}
	scope, _ := json.Marshal(in.WrenKnowledgeScope)
	if !exists {
		_, e = tx.Exec(ctx, `INSERT INTO wren_message_connections(id,workspace_id,conversation_id,owner_id,scope,name,provider,from_address,enabled,revision,key_id,ciphertext) VALUES($1,$2,$3,$4,$5,$6,'resend',$7,$8,1,$9,$10)`, in.ID, workspace, in.ConversationID, account, scope, in.Name, in.From, in.Enabled, key, sealed)
	} else {
		_, e = tx.Exec(ctx, `UPDATE wren_message_connections SET name=$2,enabled=$3,revision=revision+1,key_id=CASE WHEN $4='' THEN key_id ELSE $4 END,ciphertext=CASE WHEN $6 THEN NULL WHEN $4='' THEN ciphertext ELSE $5 END,revoked_at=CASE WHEN $6 THEN now() ELSE revoked_at END,updated_at=now() WHERE id=$1`, in.ID, in.Name, in.Enabled && !in.Revoke, key, sealed, in.Revoke)
	}
	if e != nil {
		return zero, e
	}
	_, e = tx.Exec(ctx, `UPDATE wren_message_connections SET receiving_address=$2,responsibility_id=NULLIF($3,''),webhook_key_id=CASE WHEN $4='' THEN webhook_key_id ELSE $4 END,webhook_ciphertext=CASE WHEN $6 THEN NULL WHEN $4='' THEN webhook_ciphertext ELSE $5 END WHERE id=$1`, in.ID, in.ReceivingAddress, in.ResponsibilityID, webhookKey, webhookSealed, in.ClearWebhookSecret)
	if e != nil {
		return zero, e
	}
	if e = tx.Commit(ctx); e != nil {
		return zero, e
	}
	return h.service.Store.WrenMessageConnection(ctx, workspace, account, in.ID)
}
func (s *Store) WrenMessageConnection(ctx context.Context, workspace, account, id string) (WrenMessageConnection, error) {
	v, e := scanWrenMessageConnection(s.pool.QueryRow(ctx, `SELECT `+wrenMessageConnectionColumns+` FROM wren_message_connections WHERE id=$1 AND workspace_id=$2`, id, workspace))
	if e != nil {
		return v, e
	}
	if e = knowledgeScopeAccess(ctx, s.pool, workspace, account, v.WrenKnowledgeScope, false); e != nil {
		return WrenMessageConnection{}, e
	}
	return v, nil
}
func (s *Store) WrenMessageConnections(ctx context.Context, workspace, account string, scope WrenKnowledgeScope) ([]WrenMessageConnection, error) {
	if e := knowledgeScopeAccess(ctx, s.pool, workspace, account, scope, false); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+wrenMessageConnectionColumns+` FROM wren_message_connections WHERE workspace_id=$1 AND conversation_id=$2 ORDER BY created_at,id`, workspace, scope.ConversationID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []WrenMessageConnection{}
	for rows.Next() {
		v, e := scanWrenMessageConnection(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) WrenMessageDraft(ctx context.Context, workspace, account, id string) (WrenMessageDraft, error) {
	v, e := scanWrenMessageDraft(s.pool.QueryRow(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE id=$1 AND workspace_id=$2`, id, workspace))
	if e != nil {
		return v, e
	}
	if e = artifactAccess(ctx, s.pool, workspace, account, v.ConversationID, false); e != nil {
		return WrenMessageDraft{}, e
	}
	return v, nil
}
func (s *Store) WrenMessageDrafts(ctx context.Context, workspace, account, conversation string) ([]WrenMessageDraft, error) {
	if e := artifactAccess(ctx, s.pool, workspace, account, conversation, false); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE workspace_id=$1 AND conversation_id=$2 ORDER BY created_at DESC,id LIMIT 100`, workspace, conversation)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []WrenMessageDraft{}
	for rows.Next() {
		v, e := scanWrenMessageDraft(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Model proposals use the actual active run as a permanent proposal fence. A
// rejected proposal cannot be recreated under a different operation key in it.
func (s *Store) SaveWrenMessageDraft(ctx context.Context, workspace, account string, in WrenMessageDraftInput, run *OperatorRun) (WrenMessageDraft, error) {
	var zero WrenMessageDraft
	if e := validateWrenMessageContent(in.Content); e != nil {
		return zero, e
	}
	if in.ID == "" {
		var e error
		in.ID, e = newID("maildraft_")
		if e != nil {
			return zero, e
		}
	}
	if !knowledgeID(in.ID, "maildraft_") || !knowledgeID(in.ConnectionID, "mailconn_") || !artifactSafeText(in.InstructionKey, 120) || in.InstructionKey == "" || in.ExpectedRevision < 0 {
		return zero, fmt.Errorf("provide a stable draft and instruction identity")
	}
	c, e := s.WrenMessageConnection(ctx, workspace, account, in.ConnectionID)
	if e != nil {
		return zero, e
	}
	tx, e := s.messageWriteTx(ctx, workspace, account)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback(ctx)
	if e = extensionScopeWrite(ctx, tx, workspace, account, c.WrenKnowledgeScope, true); e != nil {
		return zero, e
	}
	c, e = scanWrenMessageConnection(tx.QueryRow(ctx, `SELECT `+wrenMessageConnectionColumns+` FROM wren_message_connections WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, c.ID, workspace))
	if e != nil {
		return zero, e
	}
	if !c.Enabled || c.RevokedAt != nil {
		return zero, ErrForbidden
	}
	var runID any
	authorityKey := ""
	if run != nil {
		var instruction string
		authorityKey, instruction, e = messageProposalAuthorityTx(ctx, tx, workspace, account, c.ConversationID, *run)
		if e != nil {
			return zero, e
		}
		runID = run.ID
		in.InstructionKey = instruction
		var rejectedID string
		e = tx.QueryRow(ctx, `SELECT draft_id FROM wren_message_rejections WHERE connection_id=$1 AND creator_id=$2 AND authority_key=$3`, c.ID, account, authorityKey).Scan(&rejectedID)
		if e == nil {
			return scanWrenMessageDraft(tx.QueryRow(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE id=$1`, rejectedID))
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return zero, e
		}
	}
	old, e := scanWrenMessageDraft(tx.QueryRow(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, in.ID, workspace))
	exists := e == nil
	if e != nil && !errors.Is(e, ErrNotFound) {
		return zero, e
	}
	if exists && (old.State != "draft" || old.ConnectionID != c.ID || old.Revision != in.ExpectedRevision) {
		return zero, ErrConflict
	}
	if exists && old.CreatorID != account && c.OwnerID != account {
		return zero, ErrForbidden
	}
	if !exists && in.ExpectedRevision != 0 {
		return zero, ErrConflict
	}
	if _, e = s.messageEnvelopeTx(ctx, tx, workspace, account, c, in.Content); e != nil {
		return zero, e
	}
	raw, _ := json.Marshal(in.Content)
	revision := 1
	if exists {
		revision = old.Revision + 1
		_, e = tx.Exec(ctx, `UPDATE wren_message_drafts SET revision=$2,content=$3,updated_at=now() WHERE id=$1`, in.ID, revision, raw)
	} else {
		// Retry of a creation returns its exact prior proposal, including rejection.
		var previous WrenMessageDraft
		previous, e = scanWrenMessageDraft(tx.QueryRow(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE connection_id=$1 AND creator_id=$2 AND (instruction_key=$3 OR ($4::text IS NOT NULL AND run_id=$4))`, c.ID, account, in.InstructionKey, runID))
		if e == nil {
			return previous, nil
		}
		if !errors.Is(e, ErrNotFound) {
			return zero, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO wren_message_drafts(id,connection_id,workspace_id,conversation_id,creator_id,run_id,instruction_key,revision,content,state,authority_key) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,'draft',$9)`, in.ID, c.ID, workspace, c.ConversationID, account, runID, in.InstructionKey, raw, authorityKey)
	}
	if e != nil {
		return zero, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO wren_message_draft_revisions(draft_id,revision,content,author_id) VALUES($1,$2,$3,$4)`, in.ID, revision, raw, account)
	if e != nil {
		return zero, e
	}
	if e = messageDraftEventTx(ctx, tx, WrenMessageDraft{ID: in.ID, ConversationID: c.ConversationID, State: "draft", Revision: revision}); e != nil {
		return zero, e
	}
	if e = tx.Commit(ctx); e != nil {
		return zero, e
	}
	return s.WrenMessageDraft(ctx, workspace, account, in.ID)
}
func (s *Store) messageEnvelopeTx(ctx context.Context, q artifactQueryer, workspace, account string, c WrenMessageConnection, v WrenMessageContent) (WrenMessageEnvelope, error) {
	out := WrenMessageEnvelope{From: c.From, To: v.To, CC: v.CC, BCC: v.BCC, ReplyTo: v.ReplyTo, Subject: v.Subject, Text: v.Text}
	if v.InReplyToID != "" {
		var messageID, refs, state string
		e := q.QueryRow(ctx, `SELECT message_id,references_header,state FROM wren_message_inbox WHERE id=$1 AND connection_id=$2 AND workspace_id=$3 AND conversation_id=$4`, v.InReplyToID, c.ID, workspace, c.ConversationID).Scan(&messageID, &refs, &state)
		if errors.Is(e, pgx.ErrNoRows) {
			return out, ErrNotFound
		}
		if e != nil {
			return out, e
		}
		if state != "ready" || messageID == "" || !messagingSafeLine(messageID, 998) {
			return out, fmt.Errorf("the actual source message is not ready for a threaded reply")
		}
		refs = strings.TrimSpace(refs + " " + messageID)
		if !messagingSafeLine(refs, 4096) {
			return out, fmt.Errorf("the source thread references exceed the safe email header limit")
		}
		out.Headers = map[string]string{"In-Reply-To": messageID, "References": refs}
	}
	total := 0
	for _, ref := range v.Attachments {
		// Only files already in this exact audience may be sent. A private file may
		// not be attached to a shared draft by guessing its ID.
		a, e := scanArtifact(q.QueryRow(ctx, `SELECT `+artifactMetadataColumns+`,v.content FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id AND v.revision=$2 WHERE a.id=$1 AND a.workspace_id=$3 AND a.conversation_id=$4`, ref.ID, ref.Revision, workspace, c.ConversationID), true)
		if e != nil {
			return out, e
		}
		if a.SHA256 != ref.SHA256 {
			return out, ErrConflict
		}
		total += len(a.Content)
		if total > 8<<20 {
			return out, fmt.Errorf("email attachments exceed 8 MiB")
		}
		out.Attachments = append(out.Attachments, WrenMessageAttachment{Name: a.Name, MediaType: a.MediaType, ContentBase64: base64.StdEncoding.EncodeToString(a.Content)})
	}
	return out, nil
}

func messageDraftEventTx(ctx context.Context, tx pgx.Tx, d WrenMessageDraft) error {
	body, _ := json.Marshal(map[string]any{"draftId": d.ID, "state": d.State, "revision": d.Revision})
	_, e := tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,kind,data) VALUES($1,'message_draft',$2)`, d.ConversationID, body)
	return e
}

// Human-only actions: there is intentionally no model-facing send operation.
func (h *HTTPServer) ActWrenMessageDraft(ctx context.Context, workspace, account, id string, expected int, action string) (WrenMessageDraft, error) {
	var zero WrenMessageDraft
	if action != "send" && action != "reject" {
		return zero, ErrNotFound
	}
	s := h.service.Store
	initial, e := s.WrenMessageDraft(ctx, workspace, account, id)
	if e != nil {
		return zero, e
	}
	c, e := s.WrenMessageConnection(ctx, workspace, account, initial.ConnectionID)
	if e != nil {
		return zero, e
	}
	tx, e := s.messageWriteTx(ctx, workspace, account)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback(ctx)
	if e = extensionScopeWrite(ctx, tx, workspace, account, c.WrenKnowledgeScope, true); e != nil {
		return zero, e
	}
	c, e = scanWrenMessageConnection(tx.QueryRow(ctx, `SELECT `+wrenMessageConnectionColumns+` FROM wren_message_connections WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, c.ID, workspace))
	if e != nil {
		return zero, e
	}
	d, e := scanWrenMessageDraft(tx.QueryRow(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, id, workspace))
	if e != nil {
		return zero, e
	}
	if c.OwnerID != account && (action == "send" || d.CreatorID != account) {
		return zero, ErrForbidden
	}
	if d.State != "draft" {
		return d, nil
	} // Permanent receipt; never silently replay.
	if d.Revision != expected {
		return zero, ErrConflict
	}
	if action == "send" && (!c.Enabled || c.RevokedAt != nil) {
		return zero, ErrForbidden
	}
	if action == "send" {
		if _, e = s.messageEnvelopeTx(ctx, tx, workspace, account, c, d.Content); e != nil {
			return zero, e
		}
	}
	if action == "reject" {
		if _, e = tx.Exec(ctx, `INSERT INTO wren_message_rejections(connection_id,creator_id,authority_key,draft_id) SELECT connection_id,creator_id,authority_key,id FROM wren_message_drafts WHERE id=$1 AND authority_key<>'' AND creator_id IS NOT NULL ON CONFLICT DO NOTHING`, id); e != nil {
			return zero, e
		}
	}
	state := "rejected"
	if action == "send" {
		state = "dispatching"
	}
	_, e = tx.Exec(ctx, `UPDATE wren_message_drafts SET state=$2,revision=revision+1,send_actor_id=$3,dispatch_at=CASE WHEN $2='dispatching' THEN now() ELSE NULL END,updated_at=now() WHERE id=$1`, id, state, account)
	if e != nil {
		return zero, e
	}
	d.State = state
	d.Revision++
	if e = messageDraftEventTx(ctx, tx, d); e != nil {
		return zero, e
	}
	if e = tx.Commit(ctx); e != nil {
		return zero, e
	}
	if action == "reject" {
		return s.WrenMessageDraft(ctx, workspace, account, id)
	}
	// The durable dispatch marker commits BEFORE any HTTP. Recovery never
	// automatically replays an uncertain send, even beyond provider key expiry.
	return h.dispatchWrenMessageDraft(ctx, workspace, account, id)
}
func (h *HTTPServer) dispatchWrenMessageDraft(ctx context.Context, workspace, account, id string) (WrenMessageDraft, error) {
	var zero WrenMessageDraft
	s := h.service.Store
	initial, e := s.WrenMessageDraft(ctx, workspace, account, id)
	if e != nil {
		return zero, e
	}
	c, e := s.WrenMessageConnection(ctx, workspace, account, initial.ConnectionID)
	if e != nil {
		return zero, e
	}
	tx, e := s.messageWriteTx(ctx, workspace, account)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback(ctx)
	if e = extensionScopeWrite(ctx, tx, workspace, account, c.WrenKnowledgeScope, true); e != nil {
		return zero, e
	}
	c, e = scanWrenMessageConnection(tx.QueryRow(ctx, `SELECT `+wrenMessageConnectionColumns+` FROM wren_message_connections WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, c.ID, workspace))
	if e != nil {
		return zero, e
	}
	d, e := scanWrenMessageDraft(tx.QueryRow(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, id, workspace))
	if e != nil {
		return zero, e
	}
	if d.State != "dispatching" || c.OwnerID != account || !c.Enabled || c.RevokedAt != nil {
		return zero, ErrConflict
	}
	// No exported worker or maintenance loop can redispatch this marker. Only
	// the original, just-committed human action calls this private function.
	envelope, e := s.messageEnvelopeTx(ctx, tx, workspace, account, c, d.Content)
	if e != nil {
		return zero, e
	}
	var key string
	var encrypted []byte
	if e = tx.QueryRow(ctx, `SELECT key_id,ciphertext FROM wren_message_connections WHERE id=$1`, c.ID).Scan(&key, &encrypted); e != nil {
		return zero, e
	}
	raw, e := h.config.Secrets.open(key, encrypted, wrenMessageAAD(workspace, c.ID))
	if e != nil {
		return zero, e
	}
	defer clear(raw)
	var credentials struct {
		APIKey string `json:"apiKey"`
	}
	if json.Unmarshal(raw, &credentials) != nil || credentials.APIKey == "" {
		return zero, errSecretUnavailable
	}
	provider := h.config.Messaging.Provider
	if provider == nil {
		provider = &ResendWrenMessageProvider{}
	}
	receipt, sendErr := provider.Send(ctx, credentials.APIKey, "canter-wren-"+id, envelope)
	state, failure := "accepted", ""
	if sendErr != nil || receipt.ID == "" || receipt.Status != "accepted" {
		state = "unknown"
		failure = "The provider outcome is unknown. Check the provider before creating another send; this draft will not be retried automatically."
		if errors.Is(sendErr, ErrWrenMessageRejected) {
			state = "failed"
			failure = "The provider rejected this message. It was not accepted for delivery."
		}
	}
	_, e = tx.Exec(ctx, `UPDATE wren_message_drafts SET state=$2,provider_id=$3,failure=$4,updated_at=now() WHERE id=$1`, id, state, receipt.ID, failure)
	if e != nil {
		return zero, e
	}
	if state == "unknown" {
		if _, e = tx.Exec(ctx, `INSERT INTO wren_message_rejections(connection_id,creator_id,authority_key,draft_id) SELECT connection_id,creator_id,authority_key,id FROM wren_message_drafts WHERE id=$1 AND authority_key<>'' AND creator_id IS NOT NULL ON CONFLICT DO NOTHING`, id); e != nil {
			return zero, e
		}
	}
	receiptID, e := newID("msgreceipt_")
	if e != nil {
		return zero, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO wren_message_receipts(id,draft_id,kind,provider_id,detail) VALUES($1,$2,$3,$4,$5)`, receiptID, id, state, receipt.ID, failure)
	if e != nil {
		return zero, e
	}
	d.State = state
	if e = messageDraftEventTx(ctx, tx, d); e != nil {
		return zero, e
	}
	if e = tx.Commit(ctx); e != nil {
		return zero, e
	}
	return s.WrenMessageDraft(ctx, workspace, account, id)
}

// A stale durable marker is uncertainty, never permission to replay a POST.
// SKIP LOCKED leaves a live dispatch alone; the adapter's timeout is 30 seconds.
func (s *Store) RecoverWrenMessageDispatches(ctx context.Context) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT `+wrenMessageDraftColumns+` FROM wren_message_drafts WHERE state='dispatching' AND dispatch_at<now()-interval '2 minutes' ORDER BY dispatch_at FOR UPDATE SKIP LOCKED LIMIT 32`)
	if e != nil {
		return e
	}
	drafts := []WrenMessageDraft{}
	for rows.Next() {
		d, e := scanWrenMessageDraft(rows)
		if e != nil {
			rows.Close()
			return e
		}
		drafts = append(drafts, d)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, d := range drafts {
		failure := "The send was interrupted before its provider receipt was saved. Check the provider before creating another message; this draft will not be retried automatically."
		if _, e = tx.Exec(ctx, `UPDATE wren_message_drafts SET state='unknown',failure=$2,updated_at=now() WHERE id=$1`, d.ID, failure); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO wren_message_rejections(connection_id,creator_id,authority_key,draft_id) SELECT connection_id,creator_id,authority_key,id FROM wren_message_drafts WHERE id=$1 AND authority_key<>'' AND creator_id IS NOT NULL ON CONFLICT DO NOTHING`, d.ID); e != nil {
			return e
		}
		id, e := newID("msgreceipt_")
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO wren_message_receipts(id,draft_id,kind,detail) VALUES($1,$2,'unknown',$3)`, id, d.ID, failure); e != nil {
			return e
		}
		d.State = "unknown"
		if e = messageDraftEventTx(ctx, tx, d); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

// Match account deletion and member/ownership changes before locking a conversation.
func (s *Store) messageWriteTx(ctx context.Context, workspace, account string) (pgx.Tx, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	var id string
	if e = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&id); e != nil {
		tx.Rollback(ctx)
		return nil, ErrForbidden
	}
	if _, e = s.lockWorkspace(ctx, tx, workspace, account); e == nil {
		e = requireWorkspaceWriter(ctx, tx, workspace, account, true)
	}
	if e != nil {
		tx.Rollback(ctx)
		return nil, e
	}
	return tx, nil
}
