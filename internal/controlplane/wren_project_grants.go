package controlplane

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"strings"
)

const projectGrantColumns = `id,project_id,conversation_id,target_kind,COALESCE(wren_id,''),COALESCE(owner_id,''),revision,revoked,created_at,updated_at`

func scanProjectGrant(row pgx.Row) (WrenProjectGrant, error) {
	var g WrenProjectGrant
	e := row.Scan(&g.ID, &g.ProjectID, &g.ConversationID, &g.TargetKind, &g.WrenID, &g.OwnerID, &g.Revision, &g.Revoked, &g.CreatedAt, &g.UpdatedAt)
	return g, wrenAIStoreError(e)
}
func projectTarget(ctx context.Context, q artifactQueryer, ws, account, conversation string, manage bool) (kind, wren, owner string, err error) {
	if strings.HasPrefix(conversation, "conv_job_") {
		err = ErrForbidden
		return
	}
	if err = artifactAccess(ctx, q, ws, account, conversation, manage); err != nil {
		return
	}
	err = q.QueryRow(ctx, `SELECT CASE WHEN r.id IS NULL THEN 'private' ELSE 'room' END,COALESCE(w.id,chat.wren_id,''),COALESCE(r.account_id,c.account_id) FROM operator_conversations c LEFT JOIN wren_rooms r ON r.conversation_id=c.id LEFT JOIN wrens w ON w.conversation_id=c.id LEFT JOIN wren_shared_chats chat ON chat.conversation_id=c.id WHERE c.id=$1 AND c.workspace_id=$2 AND (r.id IS NOT NULL OR w.id IS NOT NULL OR chat.conversation_id IS NOT NULL)`, conversation, ws).Scan(&kind, &wren, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil && manage && owner != account {
		err = ErrForbidden
	}
	return
}
func (s *Store) WrenProjectGrants(ctx context.Context, ws, account, project string) ([]WrenProjectGrant, error) {
	if _, e := s.WrenProject(ctx, ws, account, project); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+projectGrantColumns+` FROM wren_project_grants WHERE workspace_id=$1 AND project_id=$2 ORDER BY created_at,id`, ws, project)
	if e != nil {
		return nil, e
	}
	all := []WrenProjectGrant{}
	for rows.Next() {
		v, e := scanProjectGrant(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		all = append(all, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []WrenProjectGrant{}
	role, _ := s.Membership(ctx, account, ws)
	for _, g := range all {
		_, _, owner, e := projectTarget(ctx, s.pool, ws, account, g.ConversationID, false)
		if e == nil {
			g.CanManage = role != "viewer" && owner == account
			out = append(out, g)
		}
	}
	return out, nil
}
func (s *Store) AttachWrenProject(ctx context.Context, ws, account, project, conversation string, expected *int) (WrenProjectGrant, error) {
	tx, e := s.projectWriteTx(ctx, ws, account)
	if e != nil {
		return WrenProjectGrant{}, e
	}
	defer tx.Rollback(ctx)
	kind, wren, owner, e := projectTarget(ctx, tx, ws, account, conversation, true)
	if e != nil {
		return WrenProjectGrant{}, e
	}
	p, e := scanWrenProject(tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM wren_projects WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, project, ws))
	if e != nil {
		return WrenProjectGrant{}, e
	}
	if p.Archived {
		return WrenProjectGrant{}, ErrForbidden
	}
	g, e := scanProjectGrant(tx.QueryRow(ctx, `SELECT `+projectGrantColumns+` FROM wren_project_grants WHERE project_id=$1 AND conversation_id=$2 FOR UPDATE`, project, conversation))
	if e == nil {
		if g.Revoked {
			if expected == nil || *expected != g.Revision {
				return g, ErrConflict
			}
			g, e = scanProjectGrant(tx.QueryRow(ctx, `UPDATE wren_project_grants SET revoked=false,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING `+projectGrantColumns, g.ID))
		}
		if e != nil {
			return g, e
		}
		g.CanManage = true
		return g, tx.Commit(ctx)
	}
	if !errors.Is(e, ErrNotFound) {
		return g, e
	}
	id, e := newID("pgrant_")
	if e != nil {
		return g, e
	}
	g, e = scanProjectGrant(tx.QueryRow(ctx, `INSERT INTO wren_project_grants(id,workspace_id,project_id,conversation_id,target_kind,wren_id,owner_id,created_by)VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8)RETURNING `+projectGrantColumns, id, ws, project, conversation, kind, wren, owner, account))
	if e != nil {
		return g, e
	}
	g.CanManage = true
	return g, tx.Commit(ctx)
}
func (s *Store) RevokeWrenProjectGrant(ctx context.Context, ws, account, project, id string, expected int) (WrenProjectGrant, error) {
	tx, e := s.projectWriteTx(ctx, ws, account)
	if e != nil {
		return WrenProjectGrant{}, e
	}
	defer tx.Rollback(ctx)
	g, e := scanProjectGrant(tx.QueryRow(ctx, `SELECT `+projectGrantColumns+` FROM wren_project_grants WHERE id=$1 AND project_id=$2 AND workspace_id=$3`, id, project, ws))
	if e != nil {
		return g, e
	}
	if _, _, _, e = projectTarget(ctx, tx, ws, account, g.ConversationID, true); e != nil {
		return g, e
	}
	if e = tx.QueryRow(ctx, `SELECT id FROM wren_projects WHERE id=$1 AND workspace_id=$2 FOR UPDATE`, project, ws).Scan(&project); e != nil {
		return g, e
	}
	g, e = scanProjectGrant(tx.QueryRow(ctx, `SELECT `+projectGrantColumns+` FROM wren_project_grants WHERE id=$1 FOR UPDATE`, id))
	if e != nil {
		return g, e
	}
	if g.Revision != expected {
		return g, ErrConflict
	}
	g, e = scanProjectGrant(tx.QueryRow(ctx, `UPDATE wren_project_grants SET revoked=true,revision=revision+1,updated_at=now()WHERE id=$1 RETURNING `+projectGrantColumns, id))
	if e != nil {
		return g, e
	}
	if e = s.stopWrenProjectReadersTx(ctx, tx, ws, project, id); e != nil {
		return g, e
	}
	g.CanManage = true
	return g, tx.Commit(ctx)
}
func (s *Store) stopWrenProjectReadersTx(ctx context.Context, tx pgx.Tx, ws, project, grant string) error {
	// Preserve actual ACP stopping state. Mark jobs cancelled/suppressed through
	// the same durable cancellation primitive used by explicit human Stop.
	rows, e := tx.Query(ctx, `SELECT `+wrenJobColumns+` FROM wren_jobs j JOIN operator_runs r ON r.id=j.child_run_id WHERE j.workspace_id=$1 AND (EXISTS(SELECT 1 FROM wren_project_job_grants x WHERE x.job_id=j.id AND x.project_id=$2 AND ($3='' OR x.source_grant_id=$3)) OR EXISTS(SELECT 1 FROM wren_project_run_reads x WHERE x.run_id=j.child_run_id AND x.project_id=$2 AND ($3='' OR x.grant_id=$3))) ORDER BY j.id FOR UPDATE OF j`, ws, project, grant)
	if e != nil {
		return e
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
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, j := range jobs {
		if e = cancelWrenJobRowTx(ctx, tx, j); e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `WITH ended AS (UPDATE operator_runs r SET status='cancelled',lease_token=NULL,lease_expires_at=NULL,completed_at=now() WHERE r.executor_kind<>'acp-opencode' AND r.status IN ('queued','running') AND EXISTS(SELECT 1 FROM wren_project_run_reads x WHERE x.run_id=r.id AND x.project_id=$1 AND ($2='' OR x.grant_id=$2)) RETURNING id,conversation_id)INSERT INTO operator_events(conversation_id,run_id,kind,data)SELECT conversation_id,id,'finished','{"status":"cancelled","content":"Project knowledge access was revoked. Completed external operations retain their state."}'::jsonb FROM ended`, project, grant)
	return e
}
func projectGrantActive(ctx context.Context, q artifactQueryer, ws, account, conversation, project string) (WrenProjectGrant, error) {
	g, e := scanProjectGrant(q.QueryRow(ctx, `SELECT `+strings.ReplaceAll(projectGrantColumns, "id,project_id", "g.id,g.project_id")+` FROM wren_project_grants g WHERE g.workspace_id=$1 AND g.project_id=$2 AND g.conversation_id=$3 AND NOT g.revoked AND EXISTS(SELECT 1 FROM wren_projects p WHERE p.id=g.project_id AND NOT p.archived)`, ws, project, conversation))
	if e != nil {
		return g, e
	}
	kind, wren, owner, e := projectTarget(ctx, q, ws, account, conversation, false)
	if e != nil {
		return g, e
	}
	if g.TargetKind != kind || (kind == "private" && (g.OwnerID != account || g.OwnerID != owner || g.WrenID != wren)) {
		return g, ErrForbidden
	}
	return g, nil
}

func lockProjectTargetsTx(ctx context.Context, tx pgx.Tx, ws, project string) error {
	rows, e := tx.Query(ctx, `SELECT c.id FROM operator_conversations c WHERE c.workspace_id=$1 AND EXISTS(SELECT 1 FROM wren_project_grants g WHERE g.project_id=$2 AND g.conversation_id=c.id) ORDER BY c.id FOR UPDATE OF c`, ws, project)
	if e != nil {
		return e
	}
	for rows.Next() {
	}
	e = rows.Err()
	rows.Close()
	return e
}
