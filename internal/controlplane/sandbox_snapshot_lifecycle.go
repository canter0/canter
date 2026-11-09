package controlplane

import (
	"context"
	"fmt"
	"io"
	"time"

	wrencomputer "github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

// sandboxSnapshotTarget is the restore side of the guest snapshot transport —
// separate from the export side so a restore-only client needn't implement it.
type sandboxSnapshotTarget interface {
	RestoreSandboxSnapshot(context.Context, wrencomputer.SandboxSnapshotOptions, wrencomputer.SandboxSnapshotStats, io.Reader) error
}

// settleStoppedSandbox runs only after provider removal is confirmed. Fence
// accounting too: an old worker must not clear a newer generation's awake
// watermark or release the reservation for its replacement VM.
func (d *SandboxDispatcher) settleStoppedSandbox(ctx context.Context, v *Sandbox) error {
	s := d.Server.service.Store
	stoppedAt := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var workspace, phase string
	var generation int
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, v.WorkspaceID).Scan(&workspace); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT generation,phase FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, v.WorkspaceID, v.ID).Scan(&generation, &phase); err != nil {
		return err
	}
	if generation != v.Generation || phase != v.observedPhase {
		return ErrConflict
	}
	var released int64
	if err = tx.QueryRow(ctx, `WITH released AS (
 UPDATE usage_reservations SET phase='released',updated_at=now()
 WHERE workspace_id=$1 AND subject_kind='sandbox' AND subject_id=$2 AND phase='reserved'
 RETURNING amount_cents
) SELECT COALESCE(sum(amount_cents),0)::bigint FROM released`, v.WorkspaceID, v.ID).Scan(&released); err != nil {
		return err
	}
	if released > 0 {
		result, err := tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=greatest(0,reserved_cents-$2),updated_at=now() WHERE workspace_id=$1`, v.WorkspaceID, released)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	if err := flushSandboxAwake(ctx, tx, v.WorkspaceID, v.ID, stoppedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandboxes SET awake_since=NULL WHERE workspace_id=$1 AND id=$2`, v.WorkspaceID, v.ID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	v.AwakeSince = nil
	return nil
}

// saveSandboxFiles quiesces tracked jobs, captures a verified v2 snapshot, and
// publishes it atomically. On any failure it keeps the VM and parks the
// sandbox in save_failed — never a silent destructive fallback.
func (d *SandboxDispatcher) saveSandboxFiles(ctx context.Context, v *Sandbox) error {
	h := d.Server
	// If a previous attempt already committed a snapshot for THIS generation,
	// publication is done — never export again after a lost acknowledgement.
	if v.CurrentSnapshotID != "" {
		snapshot, err := h.service.Store.SandboxSnapshot(ctx, v.WorkspaceID, v.CurrentSnapshotID)
		if err == nil && snapshot.State == "ready" && snapshot.SourceSandboxID == v.ID && snapshot.SourceGeneration == v.Generation {
			return nil
		}
	}
	fail := func(cause error) error {
		v.Phase = "save_failed"
		v.SleepRequested = true
		v.Failure = "Saving or verifying files failed. The VM and its files are still available. Retry sleep, download your files, or resume to repair the project."
		saveCtx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			saveCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
		}
		if saveErr := d.save(saveCtx, v); saveErr != nil {
			return saveErr
		}
		_ = h.service.Store.Audit(ctx, v.WorkspaceID, sdk.ActorRef{Kind: "system", ID: "sandbox-dispatcher"}, "sandbox.save_failed", v.ID, map[string]any{"generation": v.Generation})
		return cause
	}
	client, err := d.sandboxDirectClient(*v)
	if err != nil {
		return fail(err)
	}
	defer client.Close()
	source, ok := client.(sandboxSnapshotSource)
	if !ok {
		return fail(fmt.Errorf("sandbox client cannot export snapshots"))
	}
	if err := d.stopSandboxJobsForSave(ctx, *v, client); err != nil {
		return fail(err)
	}
	snapshot, err := h.captureSandboxSnapshot(ctx, *v, source, false)
	if err != nil {
		return fail(err)
	}
	v.CurrentSnapshotID = snapshot.ID
	v.PendingSnapshotID = ""
	v.HasCheckpoint = true
	v.CheckpointObject = sdk.ComputerCheckpointObject{}
	v.CheckpointKey, v.CheckpointNonce, v.CheckpointManifest = "", nil, nil
	return nil
}

// stopSandboxJobsForSave quiesces every tracked job before exporting: each
// StopJob creates a late-launch tombstone and must report a terminal status —
// a job still running means files may still be written.
func (d *SandboxDispatcher) stopSandboxJobsForSave(ctx context.Context, v Sandbox, client sandboxAgentClient) error {
	operation, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	jobs, err := d.Server.service.Store.openSandboxJobs(operation, v.ID)
	if err != nil {
		return err
	}
	for _, record := range jobs {
		for {
			stopped, err := client.StopJob(operation, record.ID)
			if err != nil {
				return err
			}
			if stopped.ID != record.ID {
				return fmt.Errorf("stop job %s returned receipt %s", record.ID, stopped.ID)
			}
			if stopped.Status == "completed" || stopped.Status == "failed" || stopped.Status == "cancelled" {
				break
			}
			if stopped.Status != "starting" && stopped.Status != "running" {
				return fmt.Errorf("job %s in unknown status %s", record.ID, stopped.Status)
			}
			select {
			case <-operation.Done():
				return operation.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		d.Server.service.Store.finishSandboxJob(ctx, v.ID, record.ID)
	}
	return nil
}

// restoreSandboxFiles loads the current v2 snapshot via the live transport,
// or the legacy sealed checkpoint when one exists. The source snapshot stays
// referenced as current during wake, protecting it from GC.
func (d *SandboxDispatcher) restoreSandboxFiles(ctx context.Context, v Sandbox, client sandboxAgentClient) error {
	h := d.Server
	if v.CurrentSnapshotID != "" {
		target, ok := client.(sandboxSnapshotTarget)
		if !ok {
			return fmt.Errorf("sandbox client cannot restore snapshots")
		}
		reader, snapshot, err := h.openSandboxSnapshot(ctx, v.WorkspaceID, v.CurrentSnapshotID)
		if err != nil {
			return err
		}
		defer reader.Close()
		if snapshot.SourceDiskGiB > v.DiskGiB {
			return fmt.Errorf("snapshot needs %d GiB but the sandbox has %d", snapshot.SourceDiskGiB, v.DiskGiB)
		}
		return target.RestoreSandboxSnapshot(ctx, snapshot.Options, snapshot.Stats, reader)
	}
	// Legacy sealed-checkpoint path — retained for pre-v2 saves.
	if v.CheckpointObject.Key == "" {
		return nil
	}
	engine2, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return fmt.Errorf("checkpoint storage unavailable")
	}
	ciphertext, err := engine2.GetComputerCheckpoint(ctx, v.checkpointScope(), v.CheckpointObject)
	if err != nil {
		return err
	}
	defer clear(ciphertext)
	archive, err := h.config.Secrets.open(v.CheckpointKey, ciphertext, secretBinding(v.WorkspaceID, v.ID, "sandbox-checkpoint"))
	if err != nil {
		return err
	}
	defer clear(archive)
	return client.RestoreCheckpoint(ctx, v.ID, archive, false)
}

// stopSandboxWake parks an unfinished wake: the VM is removed under durable
// sleep intent, accounting settles, and the saved snapshot/legacy pointers
// stay intact — a later reconcile never re-applies or re-exports this VM.
func (d *SandboxDispatcher) stopSandboxWake(ctx context.Context, v *Sandbox, engine VPSEngine) error {
	persist := func() error {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return d.save(saveCtx, v)
	}
	v.SleepRequested = true
	if err := persist(); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	state, err := engine.DestroyVPS(operation, v.Plan)
	cancel()
	if err != nil || state.Phase != "destroyed" {
		v.Notice = "Waiting for the unfinished VM to be removed. Your saved files are retained."
		if saveErr := persist(); saveErr != nil {
			return saveErr
		}
		return firstErr(err, ErrConflict)
	}
	accounting, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err = d.settleStoppedSandbox(accounting, v)
	done()
	if err != nil {
		return err
	}
	v.Phase = "asleep"
	v.SleepRequested = false
	v.SleepForcedAt = nil
	v.Address, v.HostFingerprint = "", ""
	v.AwakeSince = nil
	v.Notice = "Wake stopped before completion. Your saved files are retained."
	return persist()
}
