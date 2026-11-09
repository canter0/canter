package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// All additions of references must hold the workspace lock and require READY.
// EXISTS avoids multiplying storage ownership when several machines/templates
// use the same immutable snapshot.
const sandboxSnapshotHasReferences = `(
 EXISTS(SELECT 1 FROM sandboxes b WHERE b.workspace_id=p.workspace_id AND b.phase NOT IN ('deleting','deleted') AND (b.current_snapshot_id=p.id OR b.pending_snapshot_id=p.id))
 OR EXISTS(SELECT 1 FROM sandbox_template_versions v JOIN sandbox_templates t ON t.workspace_id=v.workspace_id AND t.id=v.template_id WHERE v.workspace_id=p.workspace_id AND v.snapshot_id=p.id AND v.deleted_at IS NULL AND t.deleted_at IS NULL)
)`

// workspaceLocked is only for account erasure, whose caller already holds
// the workspace row lock on its identity-deletion transaction/connection.
func (h *HTTPServer) queueSandboxSnapshotErasure(ctx context.Context, workspace, id string, workspaceLocked bool) error {
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	orphan := false
	if !workspaceLocked {
		var locked string
		err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			orphan = true
		} else if err != nil {
			return err
		}
	}
	snapshot, err := scanSandboxSnapshot(tx.QueryRow(ctx, `SELECT `+sandboxSnapshotColumns+` FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, id))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if snapshot.State == "deleted" || snapshot.State == "deleting" {
		return nil
	}
	var referenced bool
	if err = tx.QueryRow(ctx, `SELECT `+sandboxSnapshotHasReferences+` FROM sandbox_snapshots p WHERE p.workspace_id=$1 AND p.id=$2`, workspace, id).Scan(&referenced); err != nil {
		return err
	}
	if !orphan && (snapshot.Retained || referenced) {
		return nil
	}
	// Part planning locks this same snapshot row before inserting its handle.
	// Thus every possibly-issued PUT is included before the key is erased.
	if _, err = tx.Exec(ctx, `INSERT INTO sandbox_storage_erasures(scope,object_key,object_ref,uncertain)
SELECT $2,object_ref->>'key',object_ref,put_confirmed_at IS NULL FROM sandbox_snapshot_parts WHERE snapshot_id=$1
ON CONFLICT(scope,object_key) DO UPDATE SET uncertain=sandbox_storage_erasures.uncertain OR EXCLUDED.uncertain`,
		id, snapshot.scope()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox_snapshots SET state='deleting',retained=false,key_version='',key_ciphertext='',updated_at=now() WHERE workspace_id=$1 AND id=$2`, workspace, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// collectSandboxSnapshotGarbage bounds work per pass. A separate periodic
// caller can continue cleanup without delaying provisioning or user requests.
func (h *HTTPServer) collectSandboxSnapshotGarbage(ctx context.Context) error {
	return h.collectSandboxSnapshotGarbageInWorkspace(ctx, "", false)
}

// workspaceLocked is reserved for account deletion: its outer transaction
// fences the workspace while erasure handles commit on separate connections.
func (h *HTTPServer) collectSandboxSnapshotGarbageInWorkspace(ctx context.Context, workspace string, workspaceLocked bool) error {
	engine, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return fmt.Errorf("snapshot erasure unavailable")
	}
	rows, err := h.service.Store.pool.Query(ctx, `SELECT p.workspace_id,p.id FROM sandbox_snapshots p
WHERE ($1='' OR p.workspace_id=$1) AND p.state IN ('creating','ready','failed') AND
 ((NOT p.retained AND NOT `+sandboxSnapshotHasReferences+`) OR NOT EXISTS(SELECT 1 FROM workspaces w WHERE w.id=p.workspace_id))
ORDER BY p.created_at,p.id LIMIT 32`, workspace)
	if err != nil {
		return err
	}
	var candidates [][2]string
	for rows.Next() {
		var item [2]string
		if err = rows.Scan(&item[0], &item[1]); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range candidates {
		if err = h.queueSandboxSnapshotErasure(ctx, item[0], item[1], workspaceLocked); err != nil {
			return err
		}
	}
	rows, err = h.service.Store.pool.Query(ctx, `SELECT scope,object_ref,uncertain FROM sandbox_storage_erasures WHERE retry_at<=now() AND ($1='' OR starts_with(scope,'workspaces/'||$1||'/')) ORDER BY retry_at,scope,object_key LIMIT 256`, workspace)
	if err != nil {
		return err
	}
	type erasure struct {
		scope     string
		object    sdk.ComputerCheckpointObject
		uncertain bool
	}
	var erasures []erasure
	for rows.Next() {
		var item erasure
		var raw []byte
		if err = rows.Scan(&item.scope, &raw, &item.uncertain); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &item.object); err != nil {
			rows.Close()
			return err
		}
		erasures = append(erasures, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var first error
	for _, item := range erasures {
		if err = ctx.Err(); err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, time.Minute)
		err = sdk.ValidateComputerCheckpointObject(item.scope, item.object)
		if err == nil {
			err = engine.DeleteComputerCheckpoint(operation, item.scope, item.object)
		}
		cancel()
		if err != nil {
			_, _ = h.service.Store.pool.Exec(ctx, `UPDATE sandbox_storage_erasures SET retry_at=now()+interval '1 minute' WHERE scope=$1 AND object_key=$2`, item.scope, item.object.Key)
			if first == nil {
				first = fmt.Errorf("snapshot storage erasure failed")
			}
			continue
		}
		if item.uncertain {
			// DELETE may precede an old unacknowledged PUT. Keep a minimal,
			// key-free tombstone and recheck; do not forget that object's key.
			_, err = h.service.Store.pool.Exec(ctx, `UPDATE sandbox_storage_erasures SET erased_at=now(),retry_at=now()+interval '1 day' WHERE scope=$1 AND object_key=$2`, item.scope, item.object.Key)
		} else {
			_, err = h.service.Store.pool.Exec(ctx, `DELETE FROM sandbox_storage_erasures WHERE scope=$1 AND object_key=$2 AND NOT uncertain`, item.scope, item.object.Key)
		}
		if err != nil {
			return err
		}
	}
	// Uncertain writes retain their tombstones even after this snapshot is
	// cryptographically erased and its first remote DELETE has succeeded.
	_, err = h.service.Store.pool.Exec(ctx, `UPDATE sandbox_snapshots p SET state='deleted',updated_at=now()
WHERE ($1='' OR p.workspace_id=$1) AND p.state='deleting' AND NOT EXISTS(
 SELECT 1 FROM sandbox_storage_erasures e WHERE e.scope='workspaces/'||p.workspace_id||'/sandbox-snapshots/'||p.id AND e.erased_at IS NULL
)`, workspace)
	if err != nil {
		return err
	}
	return first
}

// eraseSandboxSnapshots leaves the current/pending handles on the deleting
// machine until all unshared data has been erased. A template, retained
// snapshot, or another machine can independently keep the data alive.
func (h *HTTPServer) eraseSandboxSnapshots(ctx context.Context, v Sandbox) error {
	pendingErasure := func() (bool, error) {
		var pending bool
		err := h.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM sandbox_snapshots p
 WHERE p.workspace_id=$1 AND (p.source_sandbox_id=$2 OR p.id IN (
  SELECT current_snapshot_id FROM sandboxes WHERE workspace_id=$1 AND id=$2
  UNION SELECT pending_snapshot_id FROM sandboxes WHERE workspace_id=$1 AND id=$2
 )) AND p.state<>'deleted' AND NOT p.retained AND NOT `+sandboxSnapshotHasReferences+`
) OR EXISTS(SELECT 1 FROM sandbox_storage_erasures WHERE scope=$3 AND erased_at IS NULL)`,
			v.WorkspaceID, v.ID, v.checkpointScope()).Scan(&pending)
		return pending, err
	}
	pending, err := pendingErasure()
	if err != nil || !pending {
		return err
	}
	if err := h.collectSandboxSnapshotGarbageInWorkspace(ctx, v.WorkspaceID, false); err != nil {
		return err
	}
	pending, err = pendingErasure()
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: saved files are still being removed", ErrConflict)
	}
	return nil
}

// purgeSandboxSnapshots runs only after account deletion has locked these
// workspaces and verified that they have no non-deleted machines. Remote
// deletion and its minimal retry handles survive a rollback of identity
// deletion; a storage error must not delete the account.
func (h *HTTPServer) purgeSandboxSnapshots(ctx context.Context, workspaces []string) error {
	for _, workspace := range workspaces {
		for _, query := range []string{
			`UPDATE sandbox_templates SET deleted_at=COALESCE(deleted_at,now()),updated_at=now() WHERE workspace_id=$1`,
			`UPDATE sandbox_template_versions SET deleted_at=COALESCE(deleted_at,now()) WHERE workspace_id=$1`,
			`UPDATE sandbox_snapshots SET retained=false WHERE workspace_id=$1 AND retained`,
		} {
			if _, err := h.service.Store.pool.Exec(ctx, query, workspace); err != nil {
				return err
			}
		}
		// Legacy saves may remain on old deleted rows. Journal their scope
		// before removing tenant metadata, just like version-two parts.
		rows, err := h.service.Store.pool.Query(ctx, `SELECT id,checkpoint_object_ref FROM sandboxes WHERE workspace_id=$1 AND checkpoint_object_ref IS NOT NULL`, workspace)
		if err != nil {
			return err
		}
		type legacySave struct {
			id     string
			object sdk.ComputerCheckpointObject
		}
		var legacy []legacySave
		for rows.Next() {
			var item legacySave
			var raw []byte
			if err = rows.Scan(&item.id, &raw); err != nil {
				break
			}
			if err = json.Unmarshal(raw, &item.object); err != nil {
				break
			}
			if item.object.Key != "" {
				legacy = append(legacy, item)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, item := range legacy {
			scope := (Sandbox{WorkspaceID: workspace, ID: item.id}).checkpointScope()
			if err := sdk.ValidateComputerCheckpointObject(scope, item.object); err != nil {
				return err
			}
			raw, _ := json.Marshal(item.object)
			if _, err := h.service.Store.pool.Exec(ctx, `INSERT INTO sandbox_storage_erasures(scope,object_key,object_ref) VALUES($1,$2,$3) ON CONFLICT(scope,object_key) DO NOTHING`, scope, item.object.Key, raw); err != nil {
				return err
			}
		}
		// A deliberate retry need not wait for the periodic collector's
		// backoff. Already-erased uncertain PUTs retain their daily check.
		if _, err := h.service.Store.pool.Exec(ctx, `UPDATE sandbox_storage_erasures SET retry_at=now() WHERE starts_with(scope,'workspaces/'||$1||'/') AND erased_at IS NULL`, workspace); err != nil {
			return err
		}
		for {
			var pending bool
			if err := h.service.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_snapshots WHERE workspace_id=$1 AND state<>'deleted') OR EXISTS(SELECT 1 FROM sandbox_storage_erasures WHERE starts_with(scope,'workspaces/'||$1||'/') AND erased_at IS NULL)`, workspace).Scan(&pending); err != nil {
				return err
			}
			if !pending {
				break
			}
			if err := h.collectSandboxSnapshotGarbageInWorkspace(ctx, workspace, true); err != nil {
				return err
			}
		}
	}
	return nil
}
