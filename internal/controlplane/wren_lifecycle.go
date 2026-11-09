package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type WrenLifecycle struct {
	ConversationID string     `json:"conversationId"`
	Kind           string     `json:"kind"`
	Name           string     `json:"name"`
	Hidden         bool       `json:"hidden"`
	Archived       bool       `json:"archived"`
	Revision       int        `json:"revision"`
	ArchivedAt     *time.Time `json:"archivedAt,omitempty"`
	ArchivedBy     string     `json:"archivedBy,omitempty"`
	CanManage      bool       `json:"canManage"`
}
type WrenLifecycleInput struct {
	Hidden           *bool `json:"hidden,omitempty"`
	Archived         *bool `json:"archived,omitempty"`
	ExpectedRevision *int  `json:"expectedRevision,omitempty"`
}

// Reading archived content retains the original audience checks. Neither a
// personal hide nor archive grants access to another Wren's private history.
func lifecycleMetadata(ctx context.Context, q artifactQueryer, workspace, account, conversation string, lockMembership bool) (WrenLifecycle, string, error) {
	var v WrenLifecycle
	if err := artifactAccess(ctx, q, workspace, account, conversation, false); err != nil {
		return v, "", err
	}
	role, err := workspaceRole(ctx, q, workspace, account, lockMembership)
	if err != nil {
		return v, "", err
	}
	var owner, wrenID string
	err = q.QueryRow(ctx, `SELECT c.id,CASE WHEN room.id IS NOT NULL THEN 'room' WHEN chat.conversation_id IS NOT NULL OR EXISTS(SELECT 1 FROM wren_publications ownpub WHERE ownpub.wren_id=w.id) THEN 'chat' ELSE 'wren' END,COALESCE(room.name,w.name,CASE WHEN pub.published THEN pub.profile->>'name' ELSE chat.profile_snapshot->>'name' END),COALESCE(room.account_id,w.account_id,c.account_id),COALESCE(w.id,chat.wren_id,''),l.archived_at,COALESCE(l.archived_by,''),COALESCE(l.revision,0),EXISTS(SELECT 1 FROM wren_hidden_conversations h WHERE h.conversation_id=c.id AND h.account_id=$3) FROM operator_conversations c LEFT JOIN wrens w ON w.conversation_id=c.id LEFT JOIN wren_shared_chats chat ON chat.conversation_id=c.id LEFT JOIN wren_publications pub ON pub.wren_id=chat.wren_id LEFT JOIN wren_rooms room ON room.conversation_id=c.id LEFT JOIN wren_lifecycle l ON l.conversation_id=c.id WHERE c.id=$1 AND c.workspace_id=$2 AND (w.id IS NOT NULL OR room.id IS NOT NULL OR chat.conversation_id IS NOT NULL)`, conversation, workspace, account).Scan(&v.ConversationID, &v.Kind, &v.Name, &owner, &wrenID, &v.ArchivedAt, &v.ArchivedBy, &v.Revision, &v.Hidden)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return v, "", err
	}
	v.Archived = v.ArchivedAt != nil
	v.CanManage = role != "viewer" && (owner == account || (v.Kind == "room" && role == "owner"))
	return v, wrenID, nil
}
func (s *Store) WrenConversationLifecycle(ctx context.Context, workspace, account, conversation string) (WrenLifecycle, error) {
	v, _, err := lifecycleMetadata(ctx, s.pool, workspace, account, conversation, false)
	return v, err
}
func (s *Store) WrenLifecycles(ctx context.Context, workspace, account string) ([]WrenLifecycle, error) {
	if _, err := workspaceRole(ctx, s.pool, workspace, account, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id FROM operator_conversations c WHERE c.workspace_id=$1 AND `+strings.ReplaceAll(conversationAccess, "$3", "$2")+` AND (EXISTS(SELECT 1 FROM wrens w WHERE w.conversation_id=c.id) OR EXISTS(SELECT 1 FROM wren_rooms r WHERE r.conversation_id=c.id) OR EXISTS(SELECT 1 FROM wren_shared_chats chat WHERE chat.conversation_id=c.id)) ORDER BY c.id`, workspace, account)
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
	out := []WrenLifecycle{}
	for _, id := range ids {
		v, err := s.WrenConversationLifecycle(ctx, workspace, account, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Store) UpdateWrenLifecycle(ctx context.Context, workspace, account, conversation string, input WrenLifecycleInput) (WrenLifecycle, error) {
	if input.Hidden == nil && input.Archived == nil {
		return WrenLifecycle{}, fmt.Errorf("choose a visibility or archive change")
	}
	if input.Archived != nil && (input.ExpectedRevision == nil || *input.ExpectedRevision < 0) {
		return WrenLifecycle{}, fmt.Errorf("the current archive revision is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenLifecycle{}, err
	}
	defer tx.Rollback(ctx)
	// Match membership-management order; never wait for a role lock while holding
	// a responsibility/job row. Conversation locks fence concurrent sends/tools.
	if _, err = s.lockWorkspace(ctx, tx, workspace, account); err != nil {
		return WrenLifecycle{}, err
	}
	_, wrenID, err := lifecycleMetadata(ctx, tx, workspace, account, conversation, true)
	if err != nil {
		return WrenLifecycle{}, err
	}
	// A published persona is independent of every person's DM, including the
	// publisher's original chat. Only the explicit publication archive is global.
	var sharedIdentity bool
	if wrenID != "" {
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_publications WHERE wren_id=$1) OR EXISTS(SELECT 1 FROM wren_shared_chats WHERE conversation_id=$2)`, wrenID, conversation).Scan(&sharedIdentity); err != nil {
			return WrenLifecycle{}, err
		}
		if sharedIdentity {
			wrenID = ""
		}
	}
	affected := []string{conversation}
	if input.Archived != nil && *input.Archived && wrenID != "" {
		rows, queryErr := tx.Query(ctx, `SELECT c.id FROM operator_conversations c WHERE c.workspace_id=$1 AND (c.id=$2 OR EXISTS(SELECT 1 FROM wren_rooms room JOIN wren_room_wrens rw ON rw.room_id=room.id WHERE room.conversation_id=c.id AND rw.wren_id=$3) OR EXISTS(SELECT 1 FROM wren_jobs j WHERE j.origin_conversation_id=c.id AND j.wren_id=$3)) ORDER BY c.id`, workspace, conversation, wrenID)
		if queryErr != nil {
			return WrenLifecycle{}, queryErr
		}
		affected = []string{}
		for rows.Next() {
			var id string
			if queryErr = rows.Scan(&id); queryErr != nil {
				rows.Close()
				return WrenLifecycle{}, queryErr
			}
			affected = append(affected, id)
		}
		queryErr = rows.Err()
		rows.Close()
		if queryErr != nil {
			return WrenLifecycle{}, queryErr
		}
	}
	rows, err := tx.Query(ctx, `SELECT id FROM operator_conversations WHERE id=ANY($1) ORDER BY id FOR UPDATE`, affected)
	if err != nil {
		return WrenLifecycle{}, err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return WrenLifecycle{}, err
	}
	current, _, err := lifecycleMetadata(ctx, tx, workspace, account, conversation, true)
	if err != nil {
		return WrenLifecycle{}, err
	}
	if input.Archived != nil {
		if !current.CanManage {
			return WrenLifecycle{}, ErrForbidden
		}
		if current.Revision != *input.ExpectedRevision {
			return WrenLifecycle{}, fmt.Errorf("%w: archive state changed; reload before trying again", ErrConflict)
		}
		if current.Archived != *input.Archived {
			_, err = tx.Exec(ctx, `INSERT INTO wren_lifecycle(conversation_id,archived_at,archived_by,revision) VALUES($1,CASE WHEN $2 THEN now() ELSE NULL END,CASE WHEN $2 THEN $3 ELSE NULL END,1) ON CONFLICT(conversation_id) DO UPDATE SET archived_at=CASE WHEN $2 THEN now() ELSE NULL END,archived_by=CASE WHEN $2 THEN $3 ELSE NULL END,revision=wren_lifecycle.revision+1,updated_at=now()`, conversation, *input.Archived, account)
			if err != nil {
				return WrenLifecycle{}, err
			}
			if *input.Archived {
				if err = s.stopArchivedWrenWorkTx(ctx, tx, workspace, conversation, wrenID); err != nil {
					return WrenLifecycle{}, err
				}
			}
			data, _ := json.Marshal(map[string]any{"conversationId": conversation, "wrenId": wrenID, "archived": *input.Archived, "actorId": account})
			if _, err = tx.Exec(ctx, `INSERT INTO operator_events(conversation_id,kind,data) SELECT unnest($1::text[]),'lifecycle',$2::jsonb`, affected, data); err != nil {
				return WrenLifecycle{}, err
			}
		}
	}
	if input.Hidden != nil && current.Hidden != *input.Hidden {
		if *input.Hidden {
			_, err = tx.Exec(ctx, `INSERT INTO wren_hidden_conversations(conversation_id,account_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, conversation, account)
		} else {
			_, err = tx.Exec(ctx, `DELETE FROM wren_hidden_conversations WHERE conversation_id=$1 AND account_id=$2`, conversation, account)
		}
		if err != nil {
			return WrenLifecycle{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenLifecycle{}, err
	}
	return s.WrenConversationLifecycle(ctx, workspace, account, conversation)
}
func (s *Store) stopArchivedWrenWorkTx(ctx context.Context, tx pgx.Tx, workspace, conversation, wrenID string) error {
	rows, err := tx.Query(ctx, `SELECT `+wrenJobColumns+` FROM wren_jobs j JOIN operator_runs r ON r.id=j.child_run_id WHERE j.workspace_id=$1 AND (j.origin_conversation_id=$2 OR ($3<>'' AND j.wren_id=$3)) ORDER BY j.id FOR UPDATE OF j`, workspace, conversation, wrenID)
	if err != nil {
		return err
	}
	jobs := []WrenJob{}
	for rows.Next() {
		j, e := scanWrenJob(rows)
		if e != nil {
			rows.Close()
			return e
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err = cancelWrenJobRowTx(ctx, tx, j); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, `SELECT r.id FROM operator_runs r JOIN operator_conversations c ON c.id=r.conversation_id WHERE c.workspace_id=$1 AND (c.id=$2 OR ($3<>'' AND r.speaker->>'id'=$3)) AND r.executor_kind='acp-opencode' AND r.status IN ('queued','running')`, workspace, conversation, wrenID)
	if err != nil {
		return err
	}
	ids := []string{}
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
	if err = requestHarnessStopsTx(ctx, tx, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `WITH stopped AS (UPDATE operator_runs r SET status='cancelled',completed_at=now(),lease_token=NULL,lease_expires_at=NULL FROM operator_conversations c WHERE c.id=r.conversation_id AND c.workspace_id=$1 AND (c.id=$2 OR ($3<>'' AND r.speaker->>'id'=$3)) AND r.executor_kind<>'acp-opencode' AND r.status IN ('queued','running') RETURNING r.id,r.conversation_id) INSERT INTO operator_events(conversation_id,run_id,kind,data) SELECT conversation_id,id,'finished','{"status":"cancelled","content":"Archived. Submitted operations retain their own state."}'::jsonb FROM stopped`, workspace, conversation, wrenID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_responsibilities SET state='paused',next_run_at=NULL,revision=revision+1,updated_at=now() WHERE workspace_id=$1 AND (conversation_id=$2 OR ($3<>'' AND wren_id=$3)) AND state='active'`, workspace, conversation, wrenID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE wren_responsibility_wakes wake SET state='cancelled',failure='Archived; restore does not resume schedules.' FROM wren_responsibilities r WHERE r.id=wake.responsibility_id AND r.workspace_id=$1 AND (r.conversation_id=$2 OR ($3<>'' AND r.wren_id=$3)) AND wake.state='queued'`, workspace, conversation, wrenID)
	return err
}

// Call after the source conversation lock in enqueue/dispatch/create-job
// transactions. Archive locks the same origins before changing state.
func validateWrenLifecycleTx(ctx context.Context, q artifactQueryer, conversation, wrenID string) error {
	if err := validateWrenSharingTx(ctx, q, conversation, wrenID); err != nil {
		return err
	}
	var archived bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_lifecycle l WHERE l.archived_at IS NOT NULL AND (l.conversation_id=$1 OR l.conversation_id IN (SELECT w.conversation_id FROM wrens w WHERE w.id=$2 AND NOT EXISTS(SELECT 1 FROM wren_publications p WHERE p.wren_id=w.id)) OR EXISTS(SELECT 1 FROM wren_jobs j JOIN wrens w ON w.id=j.wren_id WHERE j.child_conversation_id=$1 AND (l.conversation_id=j.origin_conversation_id OR l.conversation_id=w.conversation_id AND NOT EXISTS(SELECT 1 FROM wren_publications p WHERE p.wren_id=w.id)))))`, conversation, wrenID).Scan(&archived)
	if err != nil {
		return err
	}
	if archived {
		return fmt.Errorf("%w: this Wren or room is archived; restore it before starting work", ErrConflict)
	}
	return nil
}
func (s *Store) ValidateWrenLifecycleConversation(ctx context.Context, conversation, wrenID string) error {
	return validateWrenLifecycleTx(ctx, s.pool, conversation, wrenID)
}
func (s *Store) ValidateWrenLifecycleRun(ctx context.Context, run OperatorRun) error {
	var conversation, wrenID string
	err := s.pool.QueryRow(ctx, `SELECT conversation_id,COALESCE(speaker->>'id','') FROM operator_runs WHERE id=$1`, run.ID).Scan(&conversation, &wrenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.ValidateWrenLifecycleConversation(ctx, conversation, wrenID)
}

type WrenTranscriptExport struct {
	FormatVersion int                 `json:"formatVersion"`
	ExportedAt    time.Time           `json:"exportedAt"`
	WorkspaceID   string              `json:"workspaceId"`
	Lifecycle     WrenLifecycle       `json:"lifecycle"`
	Metadata      map[string]any      `json:"metadata"`
	Messages      []WrenExportMessage `json:"messages"`
	Artifacts     []WrenArtifact      `json:"artifacts"`
	Retention     string              `json:"retention"`
}
type WrenExportMessage struct {
	ID               string          `json:"id"`
	RunID            string          `json:"runId"`
	Role             string          `json:"role"`
	SenderName       string          `json:"senderName"`
	Content          string          `json:"content"`
	CreatedAt        time.Time       `json:"createdAt"`
	Status           string          `json:"status"`
	OriginKind       string          `json:"originKind"`
	ReplyToMessageID string          `json:"replyToMessageId,omitempty"`
	Attachments      json.RawMessage `json:"attachments,omitempty"`
}

func (s *Store) ExportWrenTranscript(ctx context.Context, workspace, account, conversation string) (WrenTranscriptExport, error) {
	out := WrenTranscriptExport{FormatVersion: 1, ExportedAt: time.Now().UTC(), WorkspaceID: workspace, Metadata: map[string]any{}, Messages: []WrenExportMessage{}, Artifacts: []WrenArtifact{}, Retention: "Archive preserves conversation history, files, memory, and submitted outcomes. Computers retain their own state, cost, and expiry. Private child-worker transcripts and private Wren memory are not included. This export references saved artifacts; download their bytes separately."}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	state, wrenID, err := lifecycleMetadata(ctx, tx, workspace, account, conversation, false)
	if err != nil {
		return out, err
	}
	out.Lifecycle = state
	var purpose, section string
	if state.Kind != "room" {
		persona, e := resolveWrenForConversation(ctx, tx, Conversation{ID: conversation, WorkspaceID: workspace, AccountID: account})
		if e != nil {
			return out, e
		}
		purpose, section = persona.Purpose, persona.Section
		out.Metadata["wrenId"] = wrenID
		out.Metadata["shape"] = persona.Shape
		out.Metadata["palette"] = persona.Palette

	} else {
		var roomID, kind string
		err = tx.QueryRow(ctx, `SELECT id,kind,purpose,section FROM wren_rooms WHERE conversation_id=$1`, conversation).Scan(&roomID, &kind, &purpose, &section)
		out.Metadata["roomId"] = roomID
		out.Metadata["roomKind"] = kind
	}
	if err != nil {
		return out, err
	}
	out.Metadata["purpose"] = purpose
	out.Metadata["section"] = section
	rows, err := tx.Query(ctx, `SELECT m.id,m.run_id,m.role,COALESCE(NULLIF(r.speaker->>'name',''),NULLIF(r.sender_name,''),CASE WHEN m.role='assistant' THEN $2 ELSE 'Person' END),m.content,m.created_at,r.status,r.origin_kind,COALESCE(reply.target_message_id,''),COALESCE((SELECT jsonb_agg(jsonb_build_object('id',a->>'id','name',a->>'name','mediaType',a->>'mediaType','size',a->'size')) FROM jsonb_array_elements(COALESCE(m.attachments,'[]'::jsonb)) a),'[]'::jsonb) FROM operator_messages m JOIN operator_runs r ON r.id=m.run_id LEFT JOIN wren_message_replies reply ON reply.message_id=m.id WHERE m.conversation_id=$1 ORDER BY m.created_at,m.id`, conversation, state.Name)
	if err != nil {
		return out, err
	}
	size := 0
	for rows.Next() {
		var message WrenExportMessage
		if err = rows.Scan(&message.ID, &message.RunID, &message.Role, &message.SenderName, &message.Content, &message.CreatedAt, &message.Status, &message.OriginKind, &message.ReplyToMessageID, &message.Attachments); err != nil {
			rows.Close()
			return out, err
		}
		size += len(message.Content) + len(message.Attachments) + len(message.SenderName) + 256
		if size > 32<<20 || len(out.Messages) >= 100000 {
			rows.Close()
			return out, fmt.Errorf("this transcript exceeds the 32 MiB export limit; no partial export was created")
		}
		out.Messages = append(out.Messages, message)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.Query(ctx, `SELECT `+artifactMetadataColumns+` FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id AND v.revision=a.current_revision WHERE a.workspace_id=$1 AND a.conversation_id=$2 ORDER BY a.created_at,a.id`, workspace, conversation)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		artifact, e := scanArtifact(rows, false)
		if e != nil {
			rows.Close()
			return out, e
		}
		encoded, _ := json.Marshal(artifact)
		size += len(encoded)
		if size > 32<<20 {
			rows.Close()
			return out, fmt.Errorf("this transcript exceeds the 32 MiB export limit; no partial export was created")
		}
		out.Artifacts = append(out.Artifacts, artifact)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}
func (v WrenTranscriptExport) Markdown() string {
	var b strings.Builder
	heading := func(text string) string {
		return strings.NewReplacer("\n", " ", "\r", " ", "#", "\\#", "[", "\\[", "]", "\\]", "*", "\\*", "`", "\\`").Replace(text)
	}
	fmt.Fprintf(&b, "# %s\n\nExported %s\n\nConversation: %s\n\n", heading(v.Lifecycle.Name), v.ExportedAt.Format(time.RFC3339), v.Lifecycle.ConversationID)
	if purpose, ok := v.Metadata["purpose"].(string); ok && purpose != "" {
		fmt.Fprintf(&b, "%s\n\n", purpose)
	}
	for _, message := range v.Messages {
		fmt.Fprintf(&b, "## %s · %s\n\n%s · %s · %s\n\n", heading(message.SenderName), message.Role, message.CreatedAt.Format(time.RFC3339), message.ID, message.Status)
		if message.ReplyToMessageID != "" {
			fmt.Fprintf(&b, "Reply to: %s\n\n", message.ReplyToMessageID)
		}
		fmt.Fprintf(&b, "%s\n\n", message.Content)
		if len(message.Attachments) > 2 {
			fmt.Fprintf(&b, "Attachments (metadata): %s\n\n", message.Attachments)
		}
	}
	b.WriteString("## Saved artifact references\n\n")
	for _, artifact := range v.Artifacts {
		fmt.Fprintf(&b, "- %s · version %d · %s · %d bytes · ID %s · SHA256 %s\n", heading(artifact.Name), artifact.Revision, artifact.MediaType, artifact.Size, artifact.ID, artifact.SHA256)
	}
	fmt.Fprintf(&b, "\n%s\n", v.Retention)
	return b.String()
}
func (h *HTTPServer) workspaceWrenLifecycle(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if len(parts) == 0 && r.Method == http.MethodGet {
		conversation := r.URL.Query().Get("conversationId")
		if conversation != "" {
			value, err := h.service.Store.WrenConversationLifecycle(r.Context(), workspace, account, conversation)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, value)
			return
		}
		values, err := h.service.Store.WrenLifecycles(r.Context(), workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lifecycles": values})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var input WrenLifecycleInput
		if !decodeLimit(w, r, &input, 4096) {
			return
		}
		value, err := h.service.Store.UpdateWrenLifecycle(r.Context(), workspace, account, parts[0], input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, value)
		return
	}
	if len(parts) == 2 && parts[1] == "export" && r.Method == http.MethodGet {
		format := r.URL.Query().Get("format")
		if format != "json" && format != "markdown" {
			writeStoreError(w, fmt.Errorf("choose json or markdown export"))
			return
		}
		value, err := h.service.Store.ExportWrenTranscript(r.Context(), workspace, account, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		var body []byte
		extension := ".json"
		media := "application/json; charset=utf-8"
		if format == "markdown" {
			body = []byte(value.Markdown())
			extension = ".md"
			media = "text/markdown; charset=utf-8"
		} else {
			body, err = json.MarshalIndent(value, "", "  ")
			if err != nil {
				writeStoreError(w, err)
				return
			}
		}
		w.Header().Set("Content-Type", media)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "canter-conversation" + extension}))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	methodNotAllowed(w)
}
