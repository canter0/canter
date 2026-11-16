package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// This allowlist is deliberately configuration only. A persona is not a grant
// to copy memory, transcripts, computers, credentials, files or installations.
type WrenProfile struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Role    string `json:"role"`
	Section string `json:"section"`
	Shape   string `json:"shape"`
	Palette int    `json:"palette"`
}

func profileFromWren(w Wren) WrenProfile {
	return WrenProfile{w.Name, w.Purpose, w.Role, w.Section, w.Shape, w.Palette}
}
func (p WrenProfile) wren(id string) Wren {
	return Wren{ID: id, Name: p.Name, Purpose: p.Purpose, Role: p.Role, Section: p.Section, Shape: p.Shape, Palette: p.Palette}
}

type WrenPublication struct {
	WrenID         string      `json:"wrenId"`
	WorkspaceID    string      `json:"workspaceId"`
	PublisherID    string      `json:"publisherId"`
	PublisherName  string      `json:"publisherName"`
	Profile        WrenProfile `json:"profile"`
	Published      bool        `json:"published"`
	Archived       bool        `json:"archived"`
	Revision       int         `json:"revision"`
	ManagerIDs     []string    `json:"managerIds"`
	CanManage      bool        `json:"canManage"`
	ConversationID string      `json:"conversationId,omitempty"`
	UpdatedAt      time.Time   `json:"updatedAt"`
}
type WrenPublicationInput struct {
	ReviewedProfile  *WrenProfile `json:"reviewedProfile,omitempty"`
	Published        bool         `json:"published"`
	Archived         bool         `json:"archived"`
	ManagerIDs       []string     `json:"managerIds"`
	ExpectedRevision int          `json:"expectedRevision"`
}
type WrenTemplate struct {
	ID          string      `json:"id"`
	WorkspaceID string      `json:"workspaceId"`
	AuthorID    string      `json:"authorId"`
	Profile     WrenProfile `json:"profile"`
	Published   bool        `json:"published"`
	Revision    int         `json:"revision"`
	UpdatedAt   time.Time   `json:"updatedAt"`
}
type WrenCopyInput struct {
	ID               string       `json:"id"`
	SourceWrenID     string       `json:"sourceWrenId,omitempty"`
	TemplateID       string       `json:"templateId,omitempty"`
	ExpectedRevision int          `json:"expectedRevision"`
	Name             string       `json:"name,omitempty"`
	ReviewedProfile  *WrenProfile `json:"reviewedProfile,omitempty"`
}

func publicationManager(ctx context.Context, q artifactQueryer, workspace, account, wrenID string) (bool, error) {
	role, err := workspaceRole(ctx, q, workspace, account, false)
	if err != nil {
		return false, err
	}
	if role == "viewer" {
		return false, nil
	}
	var manager bool
	err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wrens w LEFT JOIN wren_publications p ON p.wren_id=w.id WHERE w.id=$1 AND w.workspace_id=$2 AND (w.account_id=$3 OR (p.published AND ($4='owner' OR EXISTS(SELECT 1 FROM wren_publication_managers m WHERE m.wren_id=w.id AND m.account_id=$3)))))`, wrenID, workspace, account, role).Scan(&manager)
	return manager, err
}
func (s *Store) WrenPublication(ctx context.Context, workspace, account, wrenID string) (WrenPublication, error) {
	var out WrenPublication
	if _, err := workspaceRole(ctx, s.pool, workspace, account, false); err != nil {
		return out, err
	}
	out.ManagerIDs = []string{}
	err := s.pool.QueryRow(ctx, `SELECT p.wren_id,p.workspace_id,COALESCE(p.publisher_id,''),COALESCE(a.email,''),p.profile,p.published,p.archived,p.revision,p.updated_at,COALESCE(chat.conversation_id,'') FROM wren_publications p LEFT JOIN accounts a ON a.id=p.publisher_id LEFT JOIN wren_shared_chats chat ON chat.wren_id=p.wren_id AND chat.account_id=$3 WHERE p.wren_id=$1 AND p.workspace_id=$2 AND (p.published OR p.publisher_id=$3 OR EXISTS(SELECT 1 FROM wren_publication_managers m WHERE m.wren_id=p.wren_id AND m.account_id=$3) OR chat.conversation_id IS NOT NULL)`, wrenID, workspace, account).Scan(&out.WrenID, &out.WorkspaceID, &out.PublisherID, &out.PublisherName, &out.Profile, &out.Published, &out.Archived, &out.Revision, &out.UpdatedAt, &out.ConversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rows, err := s.pool.Query(ctx, `SELECT m.account_id FROM wren_publication_managers m JOIN memberships member ON member.account_id=m.account_id AND member.workspace_id=$2 AND member.role IN ('owner','operator') WHERE m.wren_id=$1 ORDER BY m.account_id`, wrenID, workspace)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		out.ManagerIDs = append(out.ManagerIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.CanManage, err = publicationManager(ctx, s.pool, workspace, account, wrenID)
	return out, err
}
func (s *Store) WrenPublications(ctx context.Context, workspace, account string) ([]WrenPublication, error) {
	if _, err := workspaceRole(ctx, s.pool, workspace, account, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT p.wren_id FROM wren_publications p WHERE p.workspace_id=$1 AND (p.published OR p.publisher_id=$2 OR EXISTS(SELECT 1 FROM wren_publication_managers m WHERE m.wren_id=p.wren_id AND m.account_id=$2) OR EXISTS(SELECT 1 FROM wren_shared_chats chat WHERE chat.wren_id=p.wren_id AND chat.account_id=$2)) ORDER BY p.created_at,p.wren_id`, workspace, account)
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
	out := []WrenPublication{}
	for _, id := range ids {
		p, e := s.WrenPublication(ctx, workspace, account, id)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}
func (s *Store) SaveWrenPublication(ctx context.Context, workspace, account, wrenID string, input WrenPublicationInput) (WrenPublication, error) {
	if input.ExpectedRevision < 0 || len(input.ManagerIDs) > 50 {
		return WrenPublication{}, fmt.Errorf("use the current publication revision and up to 50 managers")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenPublication{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.lockWorkspace(ctx, tx, workspace, account); err != nil {
		return WrenPublication{}, err
	}
	allowed, err := publicationManager(ctx, tx, workspace, account, wrenID)
	if err != nil {
		return WrenPublication{}, err
	}
	if !allowed {
		return WrenPublication{}, ErrForbidden
	}
	var source Wren
	var publisher string
	err = tx.QueryRow(ctx, `SELECT w.id,w.name,w.purpose,w.role,w.section,w.shape,w.palette,w.account_id FROM wrens w WHERE w.id=$1 AND w.workspace_id=$2 FOR UPDATE`, wrenID, workspace).Scan(&source.ID, &source.Name, &source.Purpose, &source.Role, &source.Section, &source.Shape, &source.Palette, &publisher)
	if errors.Is(err, pgx.ErrNoRows) {
		return WrenPublication{}, ErrNotFound
	}
	if err != nil {
		return WrenPublication{}, err
	}
	var revision int
	var previousPublished, previousArchived bool
	err = tx.QueryRow(ctx, `SELECT revision,published,archived FROM wren_publications WHERE wren_id=$1 FOR UPDATE`, wrenID).Scan(&revision, &previousPublished, &previousArchived)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WrenPublication{}, err
	}
	if revision != input.ExpectedRevision {
		return WrenPublication{}, fmt.Errorf("%w: this shared identity changed; reload before saving", ErrConflict)
	}
	seen := map[string]bool{}
	managers := []string{}
	for _, id := range input.ManagerIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		role, e := workspaceRole(ctx, tx, workspace, id, true)
		if e != nil || role == "viewer" {
			return WrenPublication{}, fmt.Errorf("choose current workspace operators or owners as managers")
		}
		managers = append(managers, id)
	}
	profile := profileFromWren(source)
	if !previousPublished && input.Published && (input.ReviewedProfile == nil || *input.ReviewedProfile != profile) {
		return WrenPublication{}, fmt.Errorf("%w: review the current saved profile before publishing it", ErrConflict)
	}
	if previousPublished && !input.Published {
		if _, err = tx.Exec(ctx, `UPDATE wren_shared_chats SET profile_snapshot=(SELECT profile FROM wren_publications WHERE wren_id=$1) WHERE wren_id=$1`, wrenID); err != nil {
			return WrenPublication{}, err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO wren_publications(wren_id,workspace_id,publisher_id,profile,published,archived,revision) VALUES($1,$2,$3,$4,$5,$6,1) ON CONFLICT(wren_id) DO UPDATE SET profile=CASE WHEN $5 THEN EXCLUDED.profile ELSE wren_publications.profile END,published=$5,archived=$6,revision=wren_publications.revision+1,updated_at=now()`, wrenID, workspace, publisher, profile, input.Published, input.Archived)
	if err != nil {
		return WrenPublication{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM wren_publication_managers WHERE wren_id=$1`, wrenID); err != nil {
		return WrenPublication{}, err
	}
	for _, id := range managers {
		if _, err = tx.Exec(ctx, `INSERT INTO wren_publication_managers(wren_id,account_id) VALUES($1,$2)`, wrenID, id); err != nil {
			return WrenPublication{}, err
		}
	}
	// Do not let private lifecycle changes masquerade as a global identity stop.
	// This explicit publication archive is the global shared-persona action.
	if (previousPublished && !input.Published) || (!previousArchived && input.Archived) {
		if err = s.stopWrenPublicationChatsTx(ctx, tx, workspace, wrenID, input.Archived); err != nil {
			return WrenPublication{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenPublication{}, err
	}
	return s.WrenPublication(ctx, workspace, account, wrenID)
}
func (s *Store) OpenSharedWrenChat(ctx context.Context, workspace, account, wrenID string) (Wren, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Wren{}, err
	}
	defer tx.Rollback(ctx)
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return Wren{}, err
	}
	var profile WrenProfile
	var published, archived bool
	err = tx.QueryRow(ctx, `SELECT profile,published,archived FROM wren_publications WHERE workspace_id=$1 AND wren_id=$2 FOR SHARE`, workspace, wrenID).Scan(&profile, &published, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wren{}, ErrNotFound
	}
	if err != nil {
		return Wren{}, err
	}
	if !published || archived {
		return Wren{}, fmt.Errorf("%w: this shared identity is unavailable", ErrConflict)
	}
	var ownConversation string
	err = tx.QueryRow(ctx, `SELECT conversation_id FROM wrens WHERE id=$1 AND account_id=$2 AND workspace_id=$3`, wrenID, account, workspace).Scan(&ownConversation)
	if err == nil {
		if err = tx.Commit(ctx); err != nil {
			return Wren{}, err
		}
		return s.Wren(ctx, workspace, account, wrenID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Wren{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,48))`, wrenID+":"+account); err != nil {
		return Wren{}, err
	}
	var conversation string
	err = tx.QueryRow(ctx, `SELECT conversation_id FROM wren_shared_chats WHERE wren_id=$1 AND account_id=$2`, wrenID, account).Scan(&conversation)
	if errors.Is(err, pgx.ErrNoRows) {
		conversation, err = newID("conv_shared_")
		if err != nil {
			return Wren{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO operator_conversations(id,workspace_id,account_id,title) VALUES($1,$2,$3,$4)`, conversation, workspace, account, profile.Name); err != nil {
			return Wren{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO wren_shared_chats(wren_id,workspace_id,account_id,conversation_id,profile_snapshot) VALUES($1,$2,$3,$4,$5)`, wrenID, workspace, account, conversation, profile)
		if err != nil {
			return Wren{}, err
		}
	} else if err != nil {
		return Wren{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Wren{}, err
	}
	return s.ResolveWrenForConversation(ctx, Conversation{ID: conversation, WorkspaceID: workspace, AccountID: account})
}

// Resolve persona separately from the current person's private conversation.
// Shared managers have no implicit access to a subscriber's conversation.
func (s *Store) ResolveWrenForConversation(ctx context.Context, c Conversation) (Wren, error) {
	return resolveWrenForConversation(ctx, s.pool, c)
}
func resolveWrenForConversation(ctx context.Context, q artifactQueryer, c Conversation) (Wren, error) {
	if err := artifactAccess(ctx, q, c.WorkspaceID, c.AccountID, c.ID, false); err != nil {
		return Wren{}, err
	}
	var id string
	var profile WrenProfile
	var updated time.Time
	var status, lastMessage string
	var lastMessageAt *time.Time
	var available bool
	var revision int
	err := q.QueryRow(ctx, `SELECT chat.wren_id,CASE WHEN p.published THEN p.profile ELSE chat.profile_snapshot END,COALESCE(p.updated_at,chat.created_at),COALESCE((SELECT status FROM operator_runs WHERE conversation_id=chat.conversation_id ORDER BY created_at DESC,id DESC LIMIT 1),'idle'),COALESCE(p.published AND NOT p.archived,false),COALESCE(p.revision,0),COALESCE((SELECT substring(content from 1 for 220) FROM operator_messages WHERE conversation_id=chat.conversation_id ORDER BY created_at DESC,id DESC LIMIT 1),''),(SELECT created_at FROM operator_messages WHERE conversation_id=chat.conversation_id ORDER BY created_at DESC,id DESC LIMIT 1) FROM wren_shared_chats chat LEFT JOIN wren_publications p ON p.wren_id=chat.wren_id WHERE chat.workspace_id=$1 AND chat.account_id=$2 AND chat.conversation_id=$3`, c.WorkspaceID, c.AccountID, c.ID).Scan(&id, &profile, &updated, &status, &available, &revision, &lastMessage, &lastMessageAt)
	if err == nil {
		w := profile.wren(id)
		w.ConversationID = c.ID
		w.UpdatedAt = updated
		w.Status = status
		w.Shared = true
		w.SharedAvailable = available
		w.PublicationRevision = revision
		w.LastMessage = lastMessage
		w.LastMessageAt = lastMessageAt
		w.CanManageProfile, err = publicationManager(ctx, q, c.WorkspaceID, c.AccountID, id)
		return w, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Wren{}, err
	}
	var w Wren
	err = q.QueryRow(ctx, `SELECT `+wrenColumns+` FROM wrens w JOIN operator_conversations c ON c.id=w.conversation_id WHERE w.workspace_id=$1 AND w.account_id=$2 AND c.id=$3`, c.WorkspaceID, c.AccountID, c.ID).Scan(&w.ID, &w.ConversationID, &w.Name, &w.Purpose, &w.Shape, &w.Palette, &w.UpdatedAt, &w.Status, &w.Role, &w.Section, &w.LastMessage, &w.LastMessageAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Wren{}, ErrNotFound
	}
	if err != nil {
		return Wren{}, err
	}
	return decorateWrenPublication(ctx, q, c.WorkspaceID, c.AccountID, w)
}
func decorateWrenPublication(ctx context.Context, q artifactQueryer, workspace, account string, w Wren) (Wren, error) {
	w.CanManageProfile = true
	w.SharedAvailable = true
	var published, archived bool
	err := q.QueryRow(ctx, `SELECT revision,published,archived FROM wren_publications WHERE wren_id=$1`, w.ID).Scan(&w.PublicationRevision, &published, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, nil
	}
	if err != nil {
		return w, err
	}
	w.SharedAvailable = !archived
	return w, nil
}

func validateWrenSharingTx(ctx context.Context, q artifactQueryer, conversation, wrenID string) error {
	var denied bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wren_shared_chats chat LEFT JOIN wren_publications p ON p.wren_id=chat.wren_id WHERE (chat.conversation_id=$1 OR EXISTS(SELECT 1 FROM wren_jobs j WHERE j.child_conversation_id=$1 AND j.origin_conversation_id=chat.conversation_id)) AND (p.wren_id IS NULL OR NOT p.published OR p.archived)) OR EXISTS(SELECT 1 FROM wren_publications p WHERE p.archived AND (p.wren_id=$2 OR EXISTS(SELECT 1 FROM wren_jobs j WHERE j.child_conversation_id=$1 AND j.wren_id=p.wren_id) OR EXISTS(SELECT 1 FROM wrens w WHERE w.id=p.wren_id AND w.conversation_id=$1)))`, conversation, wrenID).Scan(&denied)
	if err != nil {
		return err
	}
	if denied {
		return fmt.Errorf("%w: this shared Wren is unavailable; existing private history and files are retained", ErrConflict)
	}
	return nil
}
func (s *Store) ValidateWrenSharingConversation(ctx context.Context, conversation, wrenID string) error {
	return validateWrenSharingTx(ctx, s.pool, conversation, wrenID)
}
func (s *Store) stopWrenPublicationChatsTx(ctx context.Context, tx pgx.Tx, workspace, wrenID string, global bool) error {
	rows, err := tx.Query(ctx, `SELECT c.id,EXISTS(SELECT 1 FROM wren_shared_chats chat WHERE chat.conversation_id=c.id AND chat.wren_id=$2) OR EXISTS(SELECT 1 FROM wrens w WHERE w.conversation_id=c.id AND w.id=$2) FROM operator_conversations c WHERE c.workspace_id=$1 AND (EXISTS(SELECT 1 FROM wren_shared_chats chat WHERE chat.conversation_id=c.id AND chat.wren_id=$2) OR ($3 AND (EXISTS(SELECT 1 FROM wrens w WHERE w.conversation_id=c.id AND w.id=$2) OR EXISTS(SELECT 1 FROM wren_rooms room JOIN wren_room_wrens rw ON rw.room_id=room.id WHERE room.conversation_id=c.id AND rw.wren_id=$2)))) ORDER BY c.id FOR UPDATE OF c`, workspace, wrenID, global)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		var personal bool
		if err = rows.Scan(&id, &personal); err != nil {
			rows.Close()
			return err
		}
		if personal {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.stopArchivedWrenWorkTx(ctx, tx, workspace, id, ""); err != nil {
			return err
		}
	}
	if global {
		var origin string
		if err = tx.QueryRow(ctx, `SELECT conversation_id FROM wrens WHERE id=$1`, wrenID).Scan(&origin); err != nil {
			return err
		}
		if err = s.stopArchivedWrenWorkTx(ctx, tx, workspace, origin, wrenID); err != nil {
			return err
		}
	}
	return nil
}

type WrenTemplateInput struct {
	ID               string      `json:"id"`
	Profile          WrenProfile `json:"profile"`
	Published        bool        `json:"published"`
	ExpectedRevision int         `json:"expectedRevision"`
}

func (s *Store) SaveWrenTemplate(ctx context.Context, workspace, account string, input WrenTemplateInput) (WrenTemplate, error) {
	if !validArtifactID(strings.Replace(input.ID, "template_", "artifact_", 1)) || !strings.HasPrefix(input.ID, "template_") || input.ExpectedRevision < 0 {
		return WrenTemplate{}, fmt.Errorf("use a template ID and its current revision")
	}
	check := input.Profile.wren("wren_template_validation")
	if err := validateWren(&check); err != nil {
		return WrenTemplate{}, err
	}
	input.Profile = profileFromWren(check)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WrenTemplate{}, err
	}
	defer tx.Rollback(ctx)
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return WrenTemplate{}, err
	}
	var current WrenTemplate
	err = tx.QueryRow(ctx, `SELECT id,workspace_id,COALESCE(author_id,''),profile,published,revision,updated_at FROM wren_templates WHERE id=$1 FOR UPDATE`, input.ID).Scan(&current.ID, &current.WorkspaceID, &current.AuthorID, &current.Profile, &current.Published, &current.Revision, &current.UpdatedAt)
	if err == nil {
		if current.WorkspaceID != workspace || current.AuthorID != account {
			return WrenTemplate{}, ErrNotFound
		}
		if current.Revision != input.ExpectedRevision {
			if input.ExpectedRevision == 0 && current.Revision == 1 && current.Profile == input.Profile && current.Published == input.Published {
				return current, nil
			}
			return WrenTemplate{}, ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE wren_templates SET profile=$2,published=$3,revision=revision+1,updated_at=now() WHERE id=$1`, input.ID, input.Profile, input.Published)
	} else if errors.Is(err, pgx.ErrNoRows) {
		if input.ExpectedRevision != 0 {
			return WrenTemplate{}, ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO wren_templates(id,workspace_id,author_id,profile,published) VALUES($1,$2,$3,$4,$5)`, input.ID, workspace, account, input.Profile, input.Published)
	}
	if err != nil {
		return WrenTemplate{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return WrenTemplate{}, err
	}
	return s.WrenTemplate(ctx, workspace, account, input.ID)
}
func (s *Store) WrenTemplate(ctx context.Context, workspace, account, id string) (WrenTemplate, error) {
	var v WrenTemplate
	if _, err := workspaceRole(ctx, s.pool, workspace, account, false); err != nil {
		return v, err
	}
	err := s.pool.QueryRow(ctx, `SELECT id,workspace_id,COALESCE(author_id,''),profile,published,revision,updated_at FROM wren_templates WHERE id=$1 AND workspace_id=$2 AND (published OR author_id=$3)`, id, workspace, account).Scan(&v.ID, &v.WorkspaceID, &v.AuthorID, &v.Profile, &v.Published, &v.Revision, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}
func (s *Store) WrenTemplates(ctx context.Context, workspace, account string) ([]WrenTemplate, error) {
	if _, err := workspaceRole(ctx, s.pool, workspace, account, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,COALESCE(author_id,''),profile,published,revision,updated_at FROM wren_templates WHERE workspace_id=$1 AND (published OR author_id=$2) ORDER BY updated_at DESC,id`, workspace, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WrenTemplate{}
	for rows.Next() {
		var v WrenTemplate
		if err = rows.Scan(&v.ID, &v.WorkspaceID, &v.AuthorID, &v.Profile, &v.Published, &v.Revision, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) CopyWrenConfiguration(ctx context.Context, workspace, account string, input WrenCopyInput) (Wren, error) {
	if (input.SourceWrenID == "") == (input.TemplateID == "") || input.ExpectedRevision < 0 {
		return Wren{}, fmt.Errorf("choose one Wren or template source")
	}
	check := Wren{ID: input.ID, Name: "Copy", Shape: "circle"}
	if err := validateWren(&check); err != nil {
		return Wren{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Wren{}, err
	}
	defer tx.Rollback(ctx)
	if err = requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
		return Wren{}, err
	}
	kind, sourceID := "wren", input.SourceWrenID
	if input.TemplateID != "" {
		kind, sourceID = "template", input.TemplateID
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,48))`, input.ID); err != nil {
		return Wren{}, err
	}
	var priorSource, priorKind, priorOwner string
	var priorRevision int
	var priorProfile WrenProfile
	err = tx.QueryRow(ctx, `SELECT source_kind,source_id,source_revision,account_id,profile FROM wren_configuration_copies WHERE wren_id=$1 AND workspace_id=$2`, input.ID, workspace).Scan(&priorKind, &priorSource, &priorRevision, &priorOwner, &priorProfile)
	if err == nil {
		if priorOwner != account || priorSource != sourceID || priorKind != kind || priorRevision != input.ExpectedRevision || (input.Name != "" && strings.TrimSpace(input.Name) != priorProfile.Name) {
			return Wren{}, ErrConflict
		}
		if err = tx.Commit(ctx); err != nil {
			return Wren{}, err
		}
		return s.Wren(ctx, workspace, account, input.ID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Wren{}, err
	}
	var profile WrenProfile
	if kind == "template" {
		var revision int
		err = tx.QueryRow(ctx, `SELECT profile,revision FROM wren_templates WHERE id=$1 AND workspace_id=$2 AND (published OR author_id=$3) FOR SHARE`, sourceID, workspace, account).Scan(&profile, &revision)
		if err == nil && revision != input.ExpectedRevision {
			return Wren{}, ErrConflict
		}
	} else {
		var owner string
		var current Wren
		err = tx.QueryRow(ctx, `SELECT id,name,purpose,role,section,shape,palette,account_id FROM wrens WHERE id=$1 AND workspace_id=$2 FOR SHARE`, sourceID, workspace).Scan(&current.ID, &current.Name, &current.Purpose, &current.Role, &current.Section, &current.Shape, &current.Palette, &owner)
		if err == nil {
			if owner == account {
				profile = profileFromWren(current)
				if input.ReviewedProfile == nil || *input.ReviewedProfile != profile {
					return Wren{}, fmt.Errorf("%w: review the current configuration before copying", ErrConflict)
				}
			} else {
				var revision int
				err = tx.QueryRow(ctx, `SELECT profile,revision FROM wren_publications WHERE wren_id=$1 AND workspace_id=$2 AND published AND NOT archived FOR SHARE`, sourceID, workspace).Scan(&profile, &revision)
				if err == nil && revision != input.ExpectedRevision {
					return Wren{}, ErrConflict
				}
			}
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Wren{}, ErrNotFound
	}
	if err != nil {
		return Wren{}, err
	}
	if input.Name != "" {
		profile.Name = input.Name
	}
	copy := profile.wren(input.ID)
	if err = validateWren(&copy); err != nil {
		return Wren{}, err
	}
	profile = profileFromWren(copy)
	conversation := "conv_" + input.ID
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wrens WHERE id=$1) OR EXISTS(SELECT 1 FROM operator_conversations WHERE id=$2)`, input.ID, conversation).Scan(&exists); err != nil {
		return Wren{}, err
	}
	if exists {
		return Wren{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operator_conversations(id,workspace_id,account_id,title) VALUES($1,$2,$3,$4)`, conversation, workspace, account, profile.Name); err != nil {
		return Wren{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO wrens(id,workspace_id,account_id,conversation_id,name,purpose,role,section,shape,palette) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, input.ID, workspace, account, conversation, profile.Name, profile.Purpose, profile.Role, profile.Section, profile.Shape, profile.Palette); err != nil {
		return Wren{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO wren_configuration_copies(wren_id,workspace_id,account_id,source_kind,source_id,source_revision,profile) VALUES($1,$2,$3,$4,$5,$6,$7)`, input.ID, workspace, account, kind, sourceID, input.ExpectedRevision, profile); err != nil {
		return Wren{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Wren{}, err
	}
	return s.Wren(ctx, workspace, account, input.ID)
}
func (h *HTTPServer) workspaceWrenSharing(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.WrenPublications(r.Context(), workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"publications": items})
		return
	}
	if len(parts) == 1 && parts[0] == "copy" && r.Method == http.MethodPost {
		var input WrenCopyInput
		if !decodeLimit(w, r, &input, 16<<10) {
			return
		}
		item, err := h.service.Store.CopyWrenConfiguration(r.Context(), workspace, account, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		item, err := h.service.Store.WrenPublication(r.Context(), workspace, account, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPatch {
		var input WrenPublicationInput
		if !decodeLimit(w, r, &input, 16<<10) {
			return
		}
		item, err := h.service.Store.SaveWrenPublication(r.Context(), workspace, account, parts[0], input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	if len(parts) == 2 && parts[1] == "open" && r.Method == http.MethodPost {
		item, err := h.service.Store.OpenSharedWrenChat(r.Context(), workspace, account, parts[0])
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	methodNotAllowed(w)
}
func (h *HTTPServer) workspaceWrenTemplates(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	if p.Account == nil || p.Installation != nil {
		writeStoreError(w, ErrForbidden)
		return
	}
	account := p.Account.ID
	if len(parts) == 0 && r.Method == http.MethodGet {
		items, err := h.service.Store.WrenTemplates(r.Context(), workspace, account)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"templates": items})
		return
	}
	if len(parts) <= 1 && (r.Method == http.MethodPost || r.Method == http.MethodPatch) {
		var input WrenTemplateInput
		if !decodeLimit(w, r, &input, 16<<10) {
			return
		}
		if len(parts) == 1 {
			input.ID = parts[0]
		}
		item, err := h.service.Store.SaveWrenTemplate(r.Context(), workspace, account, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	methodNotAllowed(w)
}
