package controlplane

import (
	"context"
	"encoding/json"

	"fmt"
	"github.com/jackc/pgx/v5"

	"strings"
	"time"
	"unicode/utf8"
)

type WrenProject struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	SystemName string    `json:"systemName,omitempty"`
	Revision   int       `json:"revision"`
	Archived   bool      `json:"archived"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}
type WrenProjectSources struct {
	URLs []string `json:"urls,omitempty"`
}
type WrenProjectNote struct {
	ID        string             `json:"id"`
	ProjectID string             `json:"projectId"`
	Title     string             `json:"title"`
	Content   string             `json:"content"`
	Revision  int                `json:"revision"`
	AuthorID  string             `json:"authorId,omitempty"`
	Sources   WrenProjectSources `json:"sources"`
	Deleted   bool               `json:"deleted"`
	CreatedAt time.Time          `json:"createdAt"`
	UpdatedAt time.Time          `json:"updatedAt"`
}
type WrenProjectGrant struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"projectId"`
	ConversationID string    `json:"conversationId"`
	TargetKind     string    `json:"targetKind"`
	WrenID         string    `json:"wrenId,omitempty"`
	OwnerID        string    `json:"ownerId,omitempty"`
	Revision       int       `json:"revision"`
	Revoked        bool      `json:"revoked"`
	CanManage      bool      `json:"canManage"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}
type WrenProjectPatch struct {
	ExpectedRevision int     `json:"expectedRevision"`
	Name             *string `json:"name"`
	SystemName       *string `json:"systemName"`
	Archived         *bool   `json:"archived"`
}
type WrenProjectNoteInput struct {
	ID               string             `json:"id"`
	Title            string             `json:"title"`
	Content          string             `json:"content"`
	ExpectedRevision int                `json:"expectedRevision"`
	Sources          WrenProjectSources `json:"sources"`
}

const projectColumns = `id,name,COALESCE(system_name,''),revision,archived,created_at,updated_at`

func scanWrenProject(row pgx.Row) (WrenProject, error) {
	var v WrenProject
	e := row.Scan(&v.ID, &v.Name, &v.SystemName, &v.Revision, &v.Archived, &v.CreatedAt, &v.UpdatedAt)
	return v, wrenAIStoreError(e)
}
func projectMember(ctx context.Context, q artifactQueryer, ws, account string) error {
	_, e := workspaceRole(ctx, q, ws, account, false)
	return e
}
func (s *Store) projectWriteTx(ctx context.Context, ws, account string) (pgx.Tx, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	var id string
	if e = tx.QueryRow(ctx, `SELECT id FROM accounts WHERE id=$1 AND disabled_at IS NULL FOR KEY SHARE`, account).Scan(&id); e != nil {
		tx.Rollback(ctx)
		return nil, ErrForbidden
	}
	if e = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, ws).Scan(&id); e == nil {
		e = requireWorkspaceWriter(ctx, tx, ws, account, true)
	}
	if e != nil {
		tx.Rollback(ctx)
		return nil, e
	}
	return tx, nil
}
func projectSystem(ctx context.Context, q artifactQueryer, ws, name string) error {
	if name == "" {
		return nil
	}
	var yes bool
	e := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM systems WHERE workspace_id=$1 AND name=$2)`, ws, name).Scan(&yes)
	if e != nil {
		return e
	}
	if !yes {
		return ErrNotFound
	}
	return nil
}
func (s *Store) WrenProjects(ctx context.Context, ws, account string) ([]WrenProject, error) {
	if e := projectMember(ctx, s.pool, ws, account); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+projectColumns+` FROM wren_projects WHERE workspace_id=$1 ORDER BY name,id LIMIT 100`, ws)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []WrenProject{}
	for rows.Next() {
		v, e := scanWrenProject(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) WrenProject(ctx context.Context, ws, account, id string) (WrenProject, error) {
	if e := projectMember(ctx, s.pool, ws, account); e != nil {
		return WrenProject{}, e
	}
	return scanWrenProject(s.pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM wren_projects WHERE id=$1 AND workspace_id=$2`, id, ws))
}
func (s *Store) CreateWrenProject(ctx context.Context, ws, account, name, system string) (WrenProject, error) {
	name = strings.TrimSpace(name)
	if !artifactSafeText(name, 80) || name == "" {
		return WrenProject{}, fmt.Errorf("choose a project name of 1–80 characters")
	}
	tx, e := s.projectWriteTx(ctx, ws, account)
	if e != nil {
		return WrenProject{}, e
	}
	defer tx.Rollback(ctx)
	if e = projectSystem(ctx, tx, ws, system); e != nil {
		return WrenProject{}, e
	}
	var count int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM wren_projects WHERE workspace_id=$1`, ws).Scan(&count); e != nil {
		return WrenProject{}, e
	}
	if count >= 100 {
		return WrenProject{}, ErrCapacity
	}
	id, e := newID("project_")
	if e != nil {
		return WrenProject{}, e
	}
	v, e := scanWrenProject(tx.QueryRow(ctx, `INSERT INTO wren_projects(id,workspace_id,name,system_name,created_by) VALUES($1,$2,$3,NULLIF($4,''),$5) RETURNING `+projectColumns, id, ws, name, system, account))
	if e != nil {
		return v, e
	}
	return v, tx.Commit(ctx)
}
func (s *Store) UpdateWrenProject(ctx context.Context, ws, account, id string, in WrenProjectPatch) (WrenProject, error) {
	if in.ExpectedRevision < 1 {
		return WrenProject{}, ErrConflict
	}
	tx, e := s.projectWriteTx(ctx, ws, account)
	if e != nil {
		return WrenProject{}, e
	}
	defer tx.Rollback(ctx)
	if in.Archived != nil && *in.Archived {
		if e = lockProjectTargetsTx(ctx, tx, ws, id); e != nil {
			return WrenProject{}, e
		}
	}
	v, e := scanWrenProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM wren_projects WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, id, ws))
	if e != nil {
		return v, e
	}
	if v.Revision != in.ExpectedRevision {
		return v, ErrConflict
	}
	if in.Name != nil {
		v.Name = strings.TrimSpace(*in.Name)
		if !artifactSafeText(v.Name, 80) || v.Name == "" {
			return v, fmt.Errorf("choose a project name")
		}
	}
	if in.SystemName != nil {
		if e = projectSystem(ctx, tx, ws, *in.SystemName); e != nil {
			return v, e
		}
		v.SystemName = *in.SystemName
	}
	if in.Archived != nil {
		v.Archived = *in.Archived
	}
	v, e = scanWrenProject(tx.QueryRow(ctx, `UPDATE wren_projects SET name=$3,system_name=NULLIF($4,''),archived=$5,revision=revision+1,updated_at=now() WHERE id=$1 AND workspace_id=$2 RETURNING `+projectColumns, id, ws, v.Name, v.SystemName, v.Archived))
	if e != nil {
		return v, e
	}
	if in.Archived != nil && *in.Archived {
		if e = s.stopWrenProjectReadersTx(ctx, tx, ws, id, ""); e != nil {
			return v, e
		}
	}
	return v, tx.Commit(ctx)
}

const projectNoteColumns = `n.id,n.project_id,v.title,v.content,v.revision,COALESCE(v.author_id,''),v.sources,n.deleted,n.created_at,n.updated_at`

func scanProjectNote(row pgx.Row) (WrenProjectNote, error) {
	var v WrenProjectNote
	e := row.Scan(&v.ID, &v.ProjectID, &v.Title, &v.Content, &v.Revision, &v.AuthorID, &v.Sources, &v.Deleted, &v.CreatedAt, &v.UpdatedAt)
	if v.Deleted {
		v.Title = "Forgotten note"
		v.Content = ""
		v.Sources = WrenProjectSources{}
	}
	return v, wrenAIStoreError(e)
}
func (s *Store) WrenProjectNote(ctx context.Context, ws, account, project, id string, revision int) (WrenProjectNote, error) {
	if revision < 0 {
		return WrenProjectNote{}, ErrConflict
	}
	if _, e := s.WrenProject(ctx, ws, account, project); e != nil {
		return WrenProjectNote{}, e
	}
	return scanProjectNote(s.pool.QueryRow(ctx, `SELECT `+projectNoteColumns+` FROM wren_project_notes n JOIN wren_project_note_revisions v ON v.note_id=n.id AND v.revision=CASE WHEN $4=0 THEN n.current_revision ELSE $4 END WHERE n.workspace_id=$1 AND n.project_id=$2 AND n.id=$3`, ws, project, id, revision))
}
func (s *Store) WrenProjectNotes(ctx context.Context, ws, account, project, query string) ([]WrenProjectNote, error) {
	if len(query) > 160 {
		return nil, ErrCapacity
	}
	if _, e := s.WrenProject(ctx, ws, account, project); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+projectNoteColumns+` FROM wren_project_notes n JOIN wren_project_note_revisions v ON v.note_id=n.id AND v.revision=n.current_revision WHERE n.workspace_id=$1 AND n.project_id=$2 AND NOT n.deleted AND ($3='' OR position(lower($3) in lower(v.title||' '||v.content))>0) ORDER BY n.updated_at DESC,n.id LIMIT 100`, ws, project, query)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []WrenProjectNote{}
	for rows.Next() {
		v, e := scanProjectNote(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) SaveWrenProjectNote(ctx context.Context, ws, account, project string, in WrenProjectNoteInput, forget bool) (WrenProjectNote, error) {
	if !forget && (in.Title == "" || !artifactSafeText(in.Title, 120) || len(in.Content) == 0 || len(in.Content) > 64<<10 || !utf8.ValidString(in.Content) || strings.ContainsRune(in.Content, 0)) {
		return WrenProjectNote{}, fmt.Errorf("provide a title and up to 64 KiB of note text")
	}
	if e := validateMemorySources(WrenMemorySources{URLs: in.Sources.URLs}); e != nil {
		return WrenProjectNote{}, e
	}
	tx, e := s.projectWriteTx(ctx, ws, account)
	if e != nil {
		return WrenProjectNote{}, e
	}
	defer tx.Rollback(ctx)
	p, e := scanWrenProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM wren_projects WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, project, ws))
	if e != nil {
		return WrenProjectNote{}, e
	}
	if p.Archived {
		return WrenProjectNote{}, ErrForbidden
	}
	rev := 1
	if in.ID == "" {
		if forget {
			return WrenProjectNote{}, ErrNotFound
		}
		var count int
		if e = tx.QueryRow(ctx, `SELECT count(*)FROM wren_project_notes WHERE project_id=$1`, project).Scan(&count); e != nil {
			return WrenProjectNote{}, e
		}
		if count >= 100 {
			return WrenProjectNote{}, ErrCapacity
		}
		in.ID, e = newID("pnote_")
		if e != nil {
			return WrenProjectNote{}, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO wren_project_notes(id,workspace_id,project_id,current_revision)VALUES($1,$2,$3,1)`, in.ID, ws, project); e != nil {
			return WrenProjectNote{}, e
		}
	} else {
		var old int
		var deleted bool
		if e = tx.QueryRow(ctx, `SELECT current_revision,deleted FROM wren_project_notes WHERE id=$1 AND workspace_id=$2 AND project_id=$3 FOR UPDATE`, in.ID, ws, project).Scan(&old, &deleted); e != nil {
			return WrenProjectNote{}, wrenAIStoreError(e)
		}
		if old != in.ExpectedRevision || deleted {
			return WrenProjectNote{}, ErrConflict
		}
		rev = old + 1
		if _, e = tx.Exec(ctx, `UPDATE wren_project_notes SET current_revision=$2,deleted=$3,updated_at=now()WHERE id=$1`, in.ID, rev, forget); e != nil {
			return WrenProjectNote{}, e
		}
	}
	if forget {
		in.Title = "Forgotten note"
		in.Content = ""
		in.Sources = WrenProjectSources{}
	}
	sources, _ := json.Marshal(in.Sources)
	if _, e = tx.Exec(ctx, `INSERT INTO wren_project_note_revisions(note_id,revision,author_id,title,content,sources)VALUES($1,$2,$3,$4,$5,$6)`, in.ID, rev, account, in.Title, in.Content, sources); e != nil {
		return WrenProjectNote{}, e
	}
	if _, e = tx.Exec(ctx, `UPDATE wren_projects SET revision=revision+1,updated_at=now() WHERE id=$1`, project); e != nil {
		return WrenProjectNote{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return WrenProjectNote{}, e
	}
	return s.WrenProjectNote(ctx, ws, account, project, in.ID, 0)
}
func (s *Store) WrenProjectNoteHistory(ctx context.Context, ws, account, project, id string) ([]WrenProjectNote, error) {
	if _, e := s.WrenProjectNote(ctx, ws, account, project, id, 0); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+projectNoteColumns+` FROM wren_project_notes n JOIN wren_project_note_revisions v ON v.note_id=n.id WHERE n.workspace_id=$1 AND n.project_id=$2 AND n.id=$3 ORDER BY v.revision DESC LIMIT 100`, ws, project, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []WrenProjectNote{}
	for rows.Next() {
		v, e := scanProjectNote(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
