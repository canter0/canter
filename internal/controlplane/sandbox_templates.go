package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

type SandboxTemplateVersion struct {
	Version       int       `json:"version"`
	SnapshotID    string    `json:"snapshotId"`
	SourceSize    string    `json:"sourceSize"`
	SourceDiskGiB int       `json:"sourceDiskGiB"`
	StoredBytes   int64     `json:"storedBytes"`
	CreatedAt     time.Time `json:"createdAt"`
}

type SandboxTemplate struct {
	ID          string                   `json:"id"`
	WorkspaceID string                   `json:"workspaceId"`
	Name        string                   `json:"name"`
	CreatedAt   time.Time                `json:"createdAt"`
	UpdatedAt   time.Time                `json:"updatedAt"`
	Latest      *SandboxTemplateVersion  `json:"latest,omitempty"`
	Versions    []SandboxTemplateVersion `json:"versions,omitempty"`
}

type SandboxTemplateCreate struct {
	Name       string `json:"name"`
	SnapshotID string `json:"snapshotId"`
}

// SandboxSnapshotSelection selects which ready save an asleep sandbox restores.
type SandboxSnapshotSelection struct {
	SnapshotID         string `json:"snapshotId"`
	ExpectedGeneration int    `json:"expectedGeneration"`
}

func sandboxVersionRow(rows pgx.Rows) (SandboxTemplateVersion, error) {
	var v SandboxTemplateVersion
	err := rows.Scan(&v.Version, &v.SnapshotID, &v.SourceSize, &v.SourceDiskGiB, &v.StoredBytes, &v.CreatedAt)
	return v, err
}

func (s *Store) ListSandboxTemplates(ctx context.Context, workspace string) ([]SandboxTemplate, error) {
	out := []SandboxTemplate{}
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.workspace_id,t.name,t.created_at,t.updated_at,
		(SELECT json_build_object('version',v.version,'snapshotId',v.snapshot_id,'sourceSize',p.source_size,'sourceDiskGiB',p.source_disk_gib,'storedBytes',p.stored_bytes,'createdAt',v.created_at)
		 FROM sandbox_template_versions v JOIN sandbox_snapshots p ON p.workspace_id=v.workspace_id AND p.id=v.snapshot_id
		 WHERE v.template_id=t.id AND v.workspace_id=t.workspace_id AND v.deleted_at IS NULL AND p.state='ready' ORDER BY v.version DESC LIMIT 1)
		FROM sandbox_templates t WHERE t.workspace_id=$1 AND t.deleted_at IS NULL ORDER BY lower(t.name),t.id LIMIT 100`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t SandboxTemplate
		var latest []byte
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.UpdatedAt, &latest); err != nil {
			return nil, err
		}
		if len(latest) > 0 {
			var v SandboxTemplateVersion
			if err := json.Unmarshal(latest, &v); err != nil {
				return nil, err
			}
			t.Latest = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SandboxTemplate(ctx context.Context, workspace, id string) (SandboxTemplate, error) {
	var t SandboxTemplate
	err := s.pool.QueryRow(ctx, `SELECT id,workspace_id,name,created_at,updated_at FROM sandbox_templates WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL`, workspace, id).
		Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return t, ErrNotFound
		}
		return t, err
	}
	t.Versions = []SandboxTemplateVersion{}
	rows, err := s.pool.Query(ctx, `SELECT v.version,v.snapshot_id,p.source_size,p.source_disk_gib,p.stored_bytes,v.created_at
		FROM sandbox_template_versions v JOIN sandbox_snapshots p ON p.workspace_id=v.workspace_id AND p.id=v.snapshot_id
		WHERE v.workspace_id=$1 AND v.template_id=$2 AND v.deleted_at IS NULL AND p.state='ready'
		ORDER BY v.version DESC LIMIT 100`, workspace, id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := sandboxVersionRow(rows)
		if err != nil {
			return t, err
		}
		t.Versions = append(t.Versions, v)
	}
	if len(t.Versions) > 0 {
		latest := t.Versions[0]
		t.Latest = &latest
	}
	return t, rows.Err()
}

func (h *HTTPServer) sandboxLibraryTx(ctx context.Context, workspace, account string) (pgx.Tx, error) {
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if account != "" {
		if err := requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
			tx.Rollback(ctx)
			return nil, err
		}
	}
	return tx, nil
}

func (h *HTTPServer) requireSandboxLibraryCreation(ctx context.Context, tx pgx.Tx, workspace string) error {
	if err := requireSandboxHistoryBilling(ctx, tx, workspace); err != nil {
		return err
	}
	enabled, err := h.service.Store.productEnabled(ctx, tx, workspace, "sandboxes")
	if err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("%w: Enable Sandboxes first", ErrConflict)
	}
	return nil
}

func sandboxReusableSnapshot(ctx context.Context, tx pgx.Tx, workspace, id string) (SandboxSnapshot, error) {
	snapshot, err := scanSandboxSnapshot(tx.QueryRow(ctx, `SELECT `+sandboxSnapshotColumns+` FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, workspace, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshot, ErrNotFound
		}
		return snapshot, err
	}
	if snapshot.State != "ready" || snapshot.ReadyAt == nil {
		return snapshot, ErrNotFound
	}
	if snapshot.KeyVersion == "" || len(snapshot.KeyCiphertext) == 0 || len(snapshot.Manifest.Key) == 0 {
		return snapshot, fmt.Errorf("%w: snapshot has no verified archive", ErrConflict)
	}
	if snapshot.Options.IncludeBrowser {
		return snapshot, fmt.Errorf("%w: browser state cannot be reused", ErrConflict)
	}
	return snapshot, nil
}

func validateSandboxRefID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if len(id) < 1 || len(id) > 128 {
		return "", fmt.Errorf("%w: invalid source id", ErrConflict)
	}
	for _, c := range id {
		if unicode.IsControl(c) {
			return "", fmt.Errorf("%w: invalid source id", ErrConflict)
		}
	}
	return id, nil
}

func validateTemplateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 80 || strings.ContainsAny(name, "\x00\r\n") {
		return "", fmt.Errorf("%w: template names are 1-80 characters of single-line text", ErrConflict)
	}
	return name, nil
}

func (h *HTTPServer) CreateSandboxTemplate(ctx context.Context, workspace, account string, actor sdk.ActorRef, input SandboxTemplateCreate) (SandboxTemplate, error) {
	s := h.service.Store
	var out SandboxTemplate
	name, err := validateTemplateName(input.Name)
	if err != nil {
		return out, err
	}
	snapshotID, err := validateSandboxRefID(input.SnapshotID)
	if err != nil {
		return out, err
	}
	tx, err := h.sandboxLibraryTx(ctx, workspace, account)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Idempotent replay: same name + same original v1 snapshot.
	var existingID, firstSnapshot string
	err = tx.QueryRow(ctx, `SELECT t.id,COALESCE((SELECT v.snapshot_id FROM sandbox_template_versions v WHERE v.template_id=t.id AND v.workspace_id=t.workspace_id ORDER BY v.version LIMIT 1),'')
		FROM sandbox_templates t WHERE t.workspace_id=$1 AND lower(t.name)=lower($2) AND t.deleted_at IS NULL`, workspace, name).Scan(&existingID, &firstSnapshot)
	if err == nil {
		if firstSnapshot != snapshotID {
			tx.Rollback(ctx)
			return out, fmt.Errorf("%w: a template with that name uses a different save", ErrConflict)
		}
		tx.Rollback(ctx)
		return s.SandboxTemplate(ctx, workspace, existingID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err := h.requireSandboxLibraryCreation(ctx, tx, workspace); err != nil {
		return out, err
	}
	if _, err := sandboxReusableSnapshot(ctx, tx, workspace, snapshotID); err != nil {
		return out, err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sandbox_templates WHERE workspace_id=$1 AND deleted_at IS NULL`, workspace).Scan(&active); err != nil {
		return out, err
	}
	if active >= 100 {
		return out, fmt.Errorf("%w: template limit reached", ErrCapacity)
	}
	id, err := newID("stp_")
	if err != nil {
		return out, err
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, id, workspace, name, now); err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id,created_at) VALUES($1,$2,1,$3,$4)`, workspace, id, snapshotID, now); err != nil {
		return out, err
	}
	if err := s.reserveSandboxHistoryTx(ctx, tx, workspace); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	_ = s.Audit(ctx, workspace, actor, "sandbox-template.created", id, map[string]any{"name": name, "snapshotId": snapshotID})
	return s.SandboxTemplate(ctx, workspace, id)
}

func (h *HTTPServer) AddSandboxTemplateVersion(ctx context.Context, workspace, account, id, snapshotID string, actor sdk.ActorRef) (SandboxTemplateVersion, error) {
	s := h.service.Store
	var out SandboxTemplateVersion
	validated, err := validateSandboxRefID(snapshotID)
	if err != nil {
		return out, err
	}
	snapshotID = validated
	tx, err := h.sandboxLibraryTx(ctx, workspace, account)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var active bool
	if err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM sandbox_templates WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&active); err != nil || !active {
		if err == nil {
			err = pgx.ErrNoRows
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrNotFound
		}
		return out, err
	}
	// Idempotent retry: same snapshot already an active version.
	if err := tx.QueryRow(ctx, `SELECT v.version,v.snapshot_id,p.source_size,p.source_disk_gib,p.stored_bytes,v.created_at
		FROM sandbox_template_versions v JOIN sandbox_snapshots p ON p.workspace_id=v.workspace_id AND p.id=v.snapshot_id
		WHERE v.workspace_id=$1 AND v.template_id=$2 AND v.snapshot_id=$3 AND v.deleted_at IS NULL AND p.state='ready'`, workspace, id, snapshotID).
		Scan(&out.Version, &out.SnapshotID, &out.SourceSize, &out.SourceDiskGiB, &out.StoredBytes, &out.CreatedAt); err == nil {
		tx.Rollback(ctx)
		return out, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err := h.requireSandboxLibraryCreation(ctx, tx, workspace); err != nil {
		return out, err
	}
	snapshot, err := sandboxReusableSnapshot(ctx, tx, workspace, snapshotID)
	if err != nil {
		return out, err
	}
	var activeCount int
	var next int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE deleted_at IS NULL),COALESCE(max(version)::bigint,0)+1 FROM sandbox_template_versions WHERE workspace_id=$1 AND template_id=$2`, workspace, id).Scan(&activeCount, &next); err != nil {
		return out, err
	}
	if activeCount >= 100 {
		return out, fmt.Errorf("%w: version limit reached", ErrCapacity)
	}
	if next < 1 || next > math.MaxInt32 {
		return out, fmt.Errorf("%w: version number exhausted", ErrCapacity)
	}
	now := s.now()
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id,created_at) VALUES($1,$2,$3,$4,$5) RETURNING created_at`, workspace, id, int(next), snapshotID, now).Scan(&createdAt); err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox_templates SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, id, now); err != nil {
		return out, err
	}
	if err := s.reserveSandboxHistoryTx(ctx, tx, workspace); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	out = SandboxTemplateVersion{Version: int(next), SnapshotID: snapshotID, SourceSize: snapshot.SourceSize, SourceDiskGiB: snapshot.SourceDiskGiB, StoredBytes: snapshot.StoredBytes, CreatedAt: createdAt}
	_ = s.Audit(ctx, workspace, actor, "sandbox-template.version_created", id, map[string]any{"version": next, "snapshotId": snapshotID})
	return out, nil
}

func (h *HTTPServer) DeleteSandboxTemplate(ctx context.Context, workspace, account, id string, actor sdk.ActorRef) error {
	s := h.service.Store
	tx, err := h.sandboxLibraryTx(ctx, workspace, account)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM sandbox_templates WHERE workspace_id=$1 AND id=$2`, workspace, id).Scan(&deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if deleted {
		tx.Rollback(ctx)
		return nil
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE sandbox_templates SET deleted_at=$3,updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, id, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox_template_versions SET deleted_at=$3 WHERE workspace_id=$1 AND template_id=$2 AND deleted_at IS NULL`, workspace, id, now); err != nil {
		return err
	}
	if err := s.reserveSandboxHistoryTx(ctx, tx, workspace); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	_ = s.Audit(ctx, workspace, actor, "sandbox-template.deleted", id, map[string]any{})
	return nil
}

func (h *HTTPServer) DeleteSandboxTemplateVersion(ctx context.Context, workspace, account, id string, version int, actor sdk.ActorRef) error {
	s := h.service.Store
	if version < 1 || version > math.MaxInt32 {
		return fmt.Errorf("%w: invalid version", ErrConflict)
	}
	tx, err := h.sandboxLibraryTx(ctx, workspace, account)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM sandbox_template_versions WHERE workspace_id=$1 AND template_id=$2 AND version=$3`, workspace, id, version).Scan(&deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if deleted {
		tx.Rollback(ctx)
		return nil
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE sandbox_template_versions SET deleted_at=$4 WHERE workspace_id=$1 AND template_id=$2 AND version=$3`, workspace, id, version, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox_templates SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`, workspace, id, now); err != nil {
		return err
	}
	if err := s.reserveSandboxHistoryTx(ctx, tx, workspace); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	_ = s.Audit(ctx, workspace, actor, "sandbox-template.version_deleted", id, map[string]any{"version": version})
	return nil
}

func (h *HTTPServer) workspaceSandboxTemplates(w http.ResponseWriter, r *http.Request, p Principal, workspace string, parts []string) {
	s := h.service.Store
	account := ""
	if p.Account != nil {
		account = p.Account.ID
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			templates, err := s.ListSandboxTemplates(r.Context(), workspace)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			var history bool
			if err := s.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM workspace_billing WHERE workspace_id=$1 AND status='active')`, workspace).Scan(&history); err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"templates": templates, "historyEnabled": history})
		case http.MethodPost:
			if err := h.allowWorkspace(r, p, workspace, true); err != nil {
				writeStoreError(w, err)
				return
			}
			var input SandboxTemplateCreate
			if !decodeLimit(w, r, &input, 8192) {
				return
			}
			t, err := h.CreateSandboxTemplate(r.Context(), workspace, account, p.Actor, input)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, t)
		default:
			methodNotAllowed(w)
		}
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			t, err := s.SandboxTemplate(r.Context(), workspace, id)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, t)
		case http.MethodDelete:
			if err := h.allowWorkspace(r, p, workspace, true); err != nil {
				writeStoreError(w, err)
				return
			}
			if err := h.DeleteSandboxTemplate(r.Context(), workspace, account, id, p.Actor); err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		default:
			methodNotAllowed(w)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "versions" {
		if r.Method == http.MethodPost {
			if err := h.allowWorkspace(r, p, workspace, true); err != nil {
				writeStoreError(w, err)
				return
			}
			var input struct {
				SnapshotID string `json:"snapshotId"`
			}
			if !decodeLimit(w, r, &input, 8192) {
				return
			}
			v, err := h.AddSandboxTemplateVersion(r.Context(), workspace, account, id, input.SnapshotID, p.Actor)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, v)
			return
		}
		methodNotAllowed(w)
		return
	}
	if len(parts) == 3 && parts[1] == "versions" {
		if r.Method == http.MethodDelete {
			if err := h.allowWorkspace(r, p, workspace, true); err != nil {
				writeStoreError(w, err)
				return
			}
			parsed, err := strconv.ParseInt(parts[2], 10, 32)
			if err != nil || parsed < 1 {
				writeStoreError(w, ErrNotFound)
				return
			}
			if err := h.DeleteSandboxTemplateVersion(r.Context(), workspace, account, id, int(parsed), p.Actor); err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
			return
		}
		methodNotAllowed(w)
		return
	}
	http.NotFound(w, r)
}

// sandboxCreationSourceTx resolves a snapshot/template reference to a ready v2
// snapshot the new sandbox restores from. Free-current forking needs no
// billing; every other source allocation does.
func (h *HTTPServer) sandboxCreationSourceTx(ctx context.Context, tx pgx.Tx, workspace string, input SandboxCreateInput, diskGiB int) (string, error) {
	var snapshot SandboxSnapshot
	var err error
	if input.TemplateID != "" {
		if err := h.requireSandboxLibraryCreation(ctx, tx, workspace); err != nil {
			return "", err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT deleted_at IS NULL FROM sandbox_templates WHERE workspace_id=$1 AND id=$2`, workspace, input.TemplateID).Scan(&active); err != nil || !active {
			if err == nil {
				err = pgx.ErrNoRows
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrNotFound
			}
			return "", err
		}
		var snapshotID string
		if input.TemplateVersion > 0 {
			err := tx.QueryRow(ctx, `SELECT v.snapshot_id FROM sandbox_template_versions v WHERE v.workspace_id=$1 AND v.template_id=$2 AND v.version=$3 AND v.deleted_at IS NULL`, workspace, input.TemplateID, input.TemplateVersion).Scan(&snapshotID)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrNotFound
			}
			if err != nil {
				return "", err
			}
		} else {
			err := tx.QueryRow(ctx, `SELECT v.snapshot_id FROM sandbox_template_versions v WHERE v.workspace_id=$1 AND v.template_id=$2 AND v.deleted_at IS NULL ORDER BY v.version DESC LIMIT 1`, workspace, input.TemplateID).Scan(&snapshotID)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrNotFound
			}
			if err != nil {
				return "", err
			}
		}
		snapshot, err = sandboxReusableSnapshot(ctx, tx, workspace, snapshotID)
		if err != nil {
			return "", err
		}
	} else if input.SnapshotID != "" {
		var err error
		snapshot, err = sandboxReusableSnapshot(ctx, tx, workspace, input.SnapshotID)
		if err != nil {
			return "", err
		}
		var freeOrigin bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandboxes WHERE workspace_id=$1 AND current_snapshot_id=$2 AND tier='free' AND phase NOT IN ('deleting','deleted'))`, workspace, input.SnapshotID).Scan(&freeOrigin); err != nil {
			return "", err
		}
		if !freeOrigin {
			if err := h.requireSandboxLibraryCreation(ctx, tx, workspace); err != nil {
				return "", err
			}
		}
	} else {
		return "", nil
	}
	if snapshot.SourceDiskGiB > diskGiB {
		return "", fmt.Errorf("%w: destination disk too small for save", ErrConflict)
	}
	return snapshot.ID, nil
}

// SelectSandboxSnapshot points an asleep sandbox at a different ready v2 save
// it restores on its next wake. Same-target is a no-op; a new pointer pins the
// previous save so GC can't reclaim it mid-selection.
func (h *HTTPServer) SelectSandboxSnapshot(ctx context.Context, workspace, account, id string, actor sdk.ActorRef, input SandboxSnapshotSelection) (Sandbox, error) {
	s := h.service.Store
	var out Sandbox
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrNotFound
		}
		return out, err
	}
	if account != "" {
		if err := requireWorkspaceWriter(ctx, tx, workspace, account, true); err != nil {
			return out, err
		}
	}
	snapshotID, err := validateSandboxRefID(input.SnapshotID)
	if err != nil {
		return out, err
	}
	v, err := sandboxForUpdate(ctx, tx, workspace, id)
	if err != nil {
		return out, err
	}
	if v.Phase != "asleep" || v.DeleteRequested || v.SleepRequested || v.PendingSnapshotID != "" {
		return out, fmt.Errorf("%w: sandbox must be asleep with no pending save", ErrConflict)
	}
	if input.ExpectedGeneration <= 0 || input.ExpectedGeneration != v.Generation {
		return out, fmt.Errorf("%w: stale generation", ErrConflict)
	}
	if snapshotID == v.CurrentSnapshotID {
		tx.Rollback(ctx)
		return s.sandbox(ctx, s.pool, workspace, id)
	}
	if v.CheckpointObject.Key != "" {
		return out, fmt.Errorf("%w: wake and sleep this sandbox once before selecting another save", ErrConflict)
	}
	if err := h.requireSandboxLibraryCreation(ctx, tx, workspace); err != nil {
		return out, err
	}
	snapshot, err := sandboxReusableSnapshot(ctx, tx, workspace, snapshotID)
	if err != nil {
		return out, err
	}
	if snapshot.SourceDiskGiB > v.DiskGiB {
		return out, fmt.Errorf("%w: destination disk too small for save", ErrConflict)
	}
	if v.CurrentSnapshotID != "" {
		pinned, err := tx.Exec(ctx, `UPDATE sandbox_snapshots SET retained=true,updated_at=now() WHERE workspace_id=$1 AND id=$2 AND state='ready'`, workspace, v.CurrentSnapshotID)
		if err != nil {
			return out, err
		}
		if pinned.RowsAffected() != 1 {
			return out, fmt.Errorf("%w: current save cannot be preserved", ErrConflict)
		}
	}
	result, err := tx.Exec(ctx, `UPDATE sandboxes SET current_snapshot_id=$3,generation=generation+1,failure='',
		notice='This save will be restored on wake.',updated_at=now()
		WHERE workspace_id=$1 AND id=$2 AND phase='asleep' AND generation=$4
		 AND NOT delete_requested AND NOT sleep_requested AND pending_snapshot_id IS NULL`,
		workspace, id, snapshot.ID, v.Generation)
	if err != nil {
		return out, err
	}
	if result.RowsAffected() != 1 {
		return out, ErrConflict
	}
	if err := s.reserveSandboxHistoryTx(ctx, tx, workspace); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	_ = s.Audit(ctx, workspace, actor, "sandbox.snapshot_selected", id, map[string]any{"previousSnapshotId": v.CurrentSnapshotID, "snapshotId": snapshot.ID})
	return s.sandbox(ctx, s.pool, workspace, id)
}
