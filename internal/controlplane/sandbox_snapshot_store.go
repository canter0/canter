package controlplane

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

const sandboxSnapshotChunkBytes = 16 << 20
const sandboxSnapshotMaxParts = 70000
const sandboxSnapshotKeyPurpose = "sandbox-snapshot-data-key-v2"

// Snapshot data is immutable after publication. Current machines, retained
// snapshots and template versions own independent references to that data.
type SandboxSnapshot struct {
	ID               string                              `json:"id"`
	WorkspaceID      string                              `json:"workspaceId"`
	SourceSandboxID  string                              `json:"sourceSandboxId"`
	SourceGeneration int                                 `json:"sourceGeneration"`
	SourceSize       string                              `json:"sourceSize"`
	SourceDiskGiB    int                                 `json:"sourceDiskGiB"`
	Name             string                              `json:"name"`
	State            string                              `json:"state"`
	Retained         bool                                `json:"retained"`
	Stats            wrencomputer.SandboxSnapshotStats   `json:"stats"`
	StoredBytes      int64                               `json:"storedBytes"`
	Failure          string                              `json:"failure,omitempty"`
	CreatedAt        time.Time                           `json:"createdAt"`
	ReadyAt          *time.Time                          `json:"readyAt,omitempty"`
	Options          wrencomputer.SandboxSnapshotOptions `json:"-"`
	ImageID          string                              `json:"-"`
	KeyVersion       string                              `json:"-"`
	KeyCiphertext    []byte                              `json:"-"`
	Manifest         sdk.ComputerCheckpointObject        `json:"-"`
}

func (s SandboxSnapshot) scope() string {
	return "workspaces/" + s.WorkspaceID + "/sandbox-snapshots/" + s.ID
}

func (s SandboxSnapshot) binding(ordinal int) []byte {
	return secretBinding(s.WorkspaceID, s.ID, fmt.Sprintf("sandbox-snapshot-v2-part:%d", ordinal))
}

const sandboxSnapshotColumns = `id,workspace_id,source_sandbox_id,source_generation,source_size,source_disk_gib,image_id,name,state,retained,options,stats,key_version,key_ciphertext,manifest_ref,stored_bytes,failure,created_at,ready_at`

func scanSandboxSnapshot(row pgx.Row) (SandboxSnapshot, error) {
	var s SandboxSnapshot
	var options, stats, manifest []byte
	err := row.Scan(&s.ID, &s.WorkspaceID, &s.SourceSandboxID, &s.SourceGeneration, &s.SourceSize, &s.SourceDiskGiB, &s.ImageID,
		&s.Name, &s.State, &s.Retained, &options, &stats, &s.KeyVersion, &s.KeyCiphertext, &manifest, &s.StoredBytes, &s.Failure, &s.CreatedAt, &s.ReadyAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	if err != nil {
		return s, err
	}
	for _, value := range []struct {
		data []byte
		out  any
	}{{options, &s.Options}, {stats, &s.Stats}, {manifest, &s.Manifest}} {
		if err = json.Unmarshal(value.data, value.out); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (s *Store) SandboxSnapshot(ctx context.Context, workspace, id string) (SandboxSnapshot, error) {
	return scanSandboxSnapshot(s.pool.QueryRow(ctx, `SELECT `+sandboxSnapshotColumns+` FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, workspace, id))
}

type sandboxSnapshotPart struct {
	Ordinal int                          `json:"ordinal"`
	Object  sdk.ComputerCheckpointObject `json:"object"`
	Bytes   int64                        `json:"bytes"`
	SHA256  string                       `json:"sha256"`
}

type sandboxSnapshotManifest struct {
	Version     int                                 `json:"version"`
	WorkspaceID string                              `json:"workspaceId"`
	SnapshotID  string                              `json:"snapshotId"`
	ImageID     string                              `json:"imageId"`
	Options     wrencomputer.SandboxSnapshotOptions `json:"options"`
	Stats       wrencomputer.SandboxSnapshotStats   `json:"stats"`
	Parts       []sandboxSnapshotPart               `json:"parts"`
}

type sandboxSnapshotSource interface {
	ExportSandboxSnapshot(context.Context, wrencomputer.SandboxSnapshotOptions, io.Writer) (wrencomputer.SandboxSnapshotStats, error)
}

func snapshotCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid snapshot data key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// beginSandboxSnapshot installs an intent, never a current checkpoint pointer.
// A previous interrupted attempt is abandoned; its object ledger stays intact.
func (h *HTTPServer) beginSandboxSnapshot(ctx context.Context, v Sandbox, browser bool) (SandboxSnapshot, bool, error) {
	var candidate SandboxSnapshot
	if v.DiskGiB < 1 || v.DiskGiB > 1024 {
		return candidate, false, fmt.Errorf("invalid snapshot disk bound")
	}
	s := h.service.Store
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return candidate, false, err
	}
	defer tx.Rollback(ctx)
	var workspace string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, v.WorkspaceID).Scan(&workspace); err != nil {
		return candidate, false, err
	}
	var generation int
	var phase, current, pending string
	var sleeping, deleting bool
	err = tx.QueryRow(ctx, `SELECT generation,phase,COALESCE(current_snapshot_id,''),COALESCE(pending_snapshot_id,''),sleep_requested,delete_requested FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, v.WorkspaceID, v.ID).
		Scan(&generation, &phase, &current, &pending, &sleeping, &deleting)
	if err != nil {
		return candidate, false, err
	}
	if generation != v.Generation || phase != "sleeping" || !sleeping || deleting {
		return candidate, false, ErrConflict
	}
	if current != "" {
		existing, e := scanSandboxSnapshot(tx.QueryRow(ctx, `SELECT `+sandboxSnapshotColumns+` FROM sandbox_snapshots WHERE workspace_id=$1 AND id=$2`, v.WorkspaceID, current))
		if e != nil {
			return candidate, false, e
		}
		if existing.State == "ready" && existing.SourceSandboxID == v.ID && existing.SourceGeneration == v.Generation {
			// Publication committed, but VM removal or its acknowledgement did
			// not. The saved receipt is the proof; don't export from that VM again.
			return existing, true, tx.Commit(ctx)
		}
	}
	if pending != "" {
		result, e := tx.Exec(ctx, `UPDATE sandbox_snapshots SET state='failed',key_version='',key_ciphertext='',failure='An interrupted save was replaced.',updated_at=now() WHERE workspace_id=$1 AND id=$2 AND state='creating'`, v.WorkspaceID, pending)
		if e != nil {
			return candidate, false, e
		}
		if result.RowsAffected() != 1 {
			return candidate, false, ErrConflict
		}
	}
	id, err := newID("ssn_")
	if err != nil {
		return candidate, false, err
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err = rand.Read(key); err != nil {
		return candidate, false, err
	}
	keyVersion, sealedKey, err := h.config.Secrets.seal(key, secretBinding(v.WorkspaceID, id, sandboxSnapshotKeyPurpose))
	if err != nil {
		return candidate, false, err
	}
	options := wrencomputer.SandboxSnapshotOptions{MaxUnpackedBytes: int64(v.DiskGiB) << 30, MaxEntries: wrencomputer.SandboxSnapshotMaxEntries, IncludeBrowser: browser}
	rawOptions, _ := json.Marshal(options)
	candidate, err = scanSandboxSnapshot(tx.QueryRow(ctx, `INSERT INTO sandbox_snapshots(id,workspace_id,source_sandbox_id,source_generation,source_size,source_disk_gib,image_id,name,state,options,key_version,key_ciphertext,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'creating',$9,$10,$11,$12) RETURNING `+sandboxSnapshotColumns,
		id, v.WorkspaceID, v.ID, v.Generation, v.Size, v.DiskGiB, v.Plan.ImageID, v.Name, rawOptions, keyVersion, sealedKey, s.now()))
	if err != nil {
		return candidate, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE sandboxes SET pending_snapshot_id=$3 WHERE workspace_id=$1 AND id=$2`, v.WorkspaceID, v.ID, id); err != nil {
		return candidate, false, err
	}
	return candidate, false, tx.Commit(ctx)
}

func (h *HTTPServer) abandonSandboxSnapshot(candidate SandboxSnapshot) {
	// A commit error can be ambiguous. Never downgrade a READY snapshot or
	// erase its data key: the publication transaction may have committed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, candidate.WorkspaceID).Scan(&locked); err != nil {
		return
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, candidate.WorkspaceID, candidate.SourceSandboxID).Scan(&locked); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE sandbox_snapshots SET state='failed',key_version='',key_ciphertext='',failure='Saving files or storage verification failed. The previous save is unchanged.',updated_at=now() WHERE workspace_id=$1 AND id=$2 AND state='creating'`, candidate.WorkspaceID, candidate.ID); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE sandboxes SET pending_snapshot_id=NULL WHERE workspace_id=$1 AND id=$2 AND pending_snapshot_id=$3 AND EXISTS(SELECT 1 FROM sandbox_snapshots WHERE id=$3 AND state='failed')`, candidate.WorkspaceID, candidate.SourceSandboxID, candidate.ID); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

type sandboxSnapshotWriter struct {
	ctx      context.Context
	store    *Store
	engine   ComputerCheckpointEngine
	snapshot SandboxSnapshot
	aead     cipher.AEAD
	buffer   []byte
	parts    []sandboxSnapshotPart
	count    int64
	hash     hash.Hash
	err      error
}

func (w *sandboxSnapshotWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	written := 0
	for len(data) > 0 {
		take := min(sandboxSnapshotChunkBytes-len(w.buffer), len(data))
		w.buffer = append(w.buffer, data[:take]...)
		_, _ = w.hash.Write(data[:take])
		w.count += int64(take)
		data = data[take:]
		written += take
		if len(w.buffer) == sandboxSnapshotChunkBytes {
			if w.err = w.flush(); w.err != nil {
				return written, w.err
			}
		}
	}
	return written, nil
}

func (w *sandboxSnapshotWriter) flush() error {
	if len(w.buffer) == 0 {
		return nil
	}
	if len(w.parts) >= sandboxSnapshotMaxParts {
		return fmt.Errorf("snapshot has too many storage parts")
	}
	part, err := w.putPart(len(w.parts), w.buffer)
	clear(w.buffer)
	w.buffer = w.buffer[:0]
	if err != nil {
		return err
	}
	w.parts = append(w.parts, part)
	return nil
}

func (w *sandboxSnapshotWriter) putPart(ordinal int, plain []byte) (sandboxSnapshotPart, error) {
	part := sandboxSnapshotPart{Ordinal: ordinal, Bytes: int64(len(plain))}
	sum := sha256.Sum256(plain)
	part.SHA256 = hex.EncodeToString(sum[:])
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return part, err
	}
	sealed := w.aead.Seal(nonce, nonce, plain, w.snapshot.binding(ordinal))
	defer clear(sealed)
	object, err := sdk.PlanComputerCheckpointObject(w.snapshot.scope(), sealed)
	if err != nil {
		return part, err
	}
	part.Object = object
	rawObject, _ := json.Marshal(object)
	// Persist the exact erasure handle BEFORE the first possible remote write.
	// A stale worker cannot add parts after its intent has been replaced.
	result, err := w.store.pool.Exec(w.ctx, `INSERT INTO sandbox_snapshot_parts(snapshot_id,ordinal,object_ref,plain_bytes,plain_sha256)
SELECT p.id,$2,$3,$4,$5 FROM sandbox_snapshots p JOIN sandboxes b ON b.id=p.source_sandbox_id AND b.workspace_id=p.workspace_id
WHERE p.id=$1 AND p.state='creating' AND b.pending_snapshot_id=p.id AND b.generation=p.source_generation AND b.phase='sleeping' AND b.sleep_requested AND NOT b.delete_requested
FOR UPDATE OF p`,
		w.snapshot.ID, ordinal, rawObject, part.Bytes, part.SHA256)
	if err != nil {
		return part, err
	}
	if result.RowsAffected() != 1 {
		return part, ErrConflict
	}
	operation, cancel := context.WithTimeout(w.ctx, time.Minute)
	defer cancel()
	stored, err := w.engine.PutComputerCheckpoint(operation, w.snapshot.scope(), sealed)
	if err != nil {
		return part, err
	}
	if stored != object {
		return part, fmt.Errorf("storage returned an unexpected snapshot object")
	}
	if _, err = w.store.pool.Exec(operation, `UPDATE sandbox_snapshot_parts SET put_confirmed_at=now() WHERE snapshot_id=$1 AND ordinal=$2`, w.snapshot.ID, ordinal); err != nil {
		return part, err
	}
	verified, err := w.engine.GetComputerCheckpoint(operation, w.snapshot.scope(), object)
	defer clear(verified)
	if err != nil {
		return part, err
	}
	if !bytes.Equal(verified, sealed) {
		return part, fmt.Errorf("snapshot storage readback did not match")
	}
	_, err = w.store.pool.Exec(operation, `UPDATE sandbox_snapshot_parts SET state='stored' WHERE snapshot_id=$1 AND ordinal=$2 AND state='planned'`, w.snapshot.ID, ordinal)
	return part, err
}

// captureSandboxSnapshot requires the caller to have fenced mutations and
// quiesced tracked jobs/terminals before persisting phase=sleeping.
func (h *HTTPServer) captureSandboxSnapshot(ctx context.Context, v Sandbox, source sandboxSnapshotSource, browser bool) (result SandboxSnapshot, err error) {
	engine, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return result, fmt.Errorf("snapshot storage unavailable")
	}
	candidate, published, err := h.beginSandboxSnapshot(ctx, v, browser)
	if err != nil || published {
		return candidate, err
	}
	defer func() {
		if err != nil {
			h.abandonSandboxSnapshot(candidate)
		}
	}()
	key, err := h.config.Secrets.open(candidate.KeyVersion, candidate.KeyCiphertext, secretBinding(candidate.WorkspaceID, candidate.ID, sandboxSnapshotKeyPurpose))
	if err != nil {
		return result, err
	}
	defer clear(key)
	aead, err := snapshotCipher(key)
	if err != nil {
		return result, err
	}
	operation, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	writer := &sandboxSnapshotWriter{ctx: operation, store: h.service.Store, engine: engine, snapshot: candidate, aead: aead, hash: sha256.New(), buffer: make([]byte, 0, sandboxSnapshotChunkBytes)}
	defer func() { clear(writer.buffer) }()
	// Lifecycle changes cancel long exports even if no new part has reached
	// its SQL fence yet (for example, a source filesystem has stalled).
	go func() {
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-operation.Done():
				return
			case <-tick.C:
				check, stop := context.WithTimeout(operation, 5*time.Second)
				var valid bool
				e := h.service.Store.pool.QueryRow(check, `SELECT EXISTS(SELECT 1 FROM sandboxes WHERE workspace_id=$1 AND id=$2 AND generation=$3 AND NOT delete_requested AND ((phase='sleeping' AND sleep_requested AND pending_snapshot_id=$4) OR current_snapshot_id=$4))`, v.WorkspaceID, v.ID, v.Generation, candidate.ID).Scan(&valid)
				stop()
				if e != nil || !valid {
					cancel()
					return
				}
			}
		}
	}()
	stats, err := source.ExportSandboxSnapshot(operation, candidate.Options, writer)
	if err != nil {
		return result, err
	}
	if writer.err != nil {
		return result, writer.err
	}
	if err = wrencomputer.ValidateSandboxSnapshotStats(stats, candidate.Options); err != nil {
		return result, err
	}
	if stats.CompressedBytes != writer.count || stats.ArchiveSHA256 != hex.EncodeToString(writer.hash.Sum(nil)) {
		return result, fmt.Errorf("snapshot source receipt did not match stored bytes")
	}
	if err = writer.flush(); err != nil {
		return result, err
	}
	manifest := sandboxSnapshotManifest{Version: 2, WorkspaceID: candidate.WorkspaceID, SnapshotID: candidate.ID, ImageID: candidate.ImageID, Options: candidate.Options, Stats: stats, Parts: writer.parts}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		return result, err
	}
	defer clear(rawManifest)
	manifestPart, err := writer.putPart(-1, rawManifest)
	if err != nil {
		return result, err
	}
	candidate.Stats, candidate.Manifest = stats, manifestPart.Object
	candidate.StoredBytes = manifestPart.Object.Size
	for _, part := range writer.parts {
		candidate.StoredBytes += part.Object.Size
	}
	if err = h.publishSandboxSnapshot(operation, candidate, len(writer.parts)); err != nil {
		return result, err
	}
	candidate.State = "ready"
	return candidate, nil
}

func (h *HTTPServer) publishSandboxSnapshot(ctx context.Context, candidate SandboxSnapshot, parts int) error {
	tx, err := h.service.Store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var workspace string
	if err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, candidate.WorkspaceID).Scan(&workspace); err != nil {
		return err
	}
	var legacy []byte
	var generation int
	var phase, pending string
	var sleeping, deleting bool
	err = tx.QueryRow(ctx, `SELECT generation,phase,COALESCE(pending_snapshot_id,''),sleep_requested,delete_requested,checkpoint_object_ref FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, candidate.WorkspaceID, candidate.SourceSandboxID).
		Scan(&generation, &phase, &pending, &sleeping, &deleting, &legacy)
	if err != nil {
		return err
	}
	if generation != candidate.SourceGeneration || phase != "sleeping" || pending != candidate.ID || !sleeping || deleting {
		return ErrConflict
	}
	var total, dataParts, last int
	var verified bool
	var storedBytes int64
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE ordinal>=0),COALESCE(max(ordinal),-1),COALESCE(bool_and(state='stored'),false),COALESCE(sum((object_ref->>'size')::bigint),0) FROM sandbox_snapshot_parts WHERE snapshot_id=$1`, candidate.ID).Scan(&total, &dataParts, &last, &verified, &storedBytes)
	if err != nil {
		return err
	}
	if parts < 1 || total != parts+1 || dataParts != parts || last != parts-1 || !verified || storedBytes != candidate.StoredBytes {
		return fmt.Errorf("snapshot storage ledger is incomplete")
	}
	stats, _ := json.Marshal(candidate.Stats)
	manifest, _ := json.Marshal(candidate.Manifest)
	updated, err := tx.Exec(ctx, `UPDATE sandbox_snapshots SET state='ready',stats=$3,manifest_ref=$4,stored_bytes=$5,ready_at=$6,updated_at=$6 WHERE workspace_id=$1 AND id=$2 AND state='creating'`, candidate.WorkspaceID, candidate.ID, stats, manifest, storedBytes, h.service.Store.now())
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return ErrConflict
	}
	if len(legacy) > 0 {
		var previous sdk.ComputerCheckpointObject
		if err = json.Unmarshal(legacy, &previous); err != nil {
			return err
		}
		if previous.Key != "" {
			scope := "workspaces/" + candidate.WorkspaceID + "/sandboxes/" + candidate.SourceSandboxID
			if err = sdk.ValidateComputerCheckpointObject(scope, previous); err != nil {
				return err
			}
			raw, _ := json.Marshal(previous)
			if _, err = tx.Exec(ctx, `INSERT INTO sandbox_storage_erasures(scope,object_key,object_ref) VALUES($1,$2,$3) ON CONFLICT(scope,object_key) DO NOTHING`, scope, previous.Key, raw); err != nil {
				return err
			}
		}
	}
	// READY and the current pointer become visible together. The previous
	// object remains tracked, including legacy erasure handles, on every path.
	_, err = tx.Exec(ctx, `UPDATE sandboxes SET current_snapshot_id=$3,pending_snapshot_id=NULL,checkpoint_object_ref=NULL,checkpoint_key_version='',checkpoint_nonce=NULL,checkpoint_manifest=NULL WHERE workspace_id=$1 AND id=$2`, candidate.WorkspaceID, candidate.SourceSandboxID, candidate.ID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (h *HTTPServer) snapshotManifest(ctx context.Context, snapshot SandboxSnapshot) (sandboxSnapshotManifest, cipher.AEAD, error) {
	var manifest sandboxSnapshotManifest
	if snapshot.State != "ready" {
		return manifest, nil, ErrConflict
	}
	engine, ok := h.service.Engine.(ComputerCheckpointEngine)
	if !ok {
		return manifest, nil, fmt.Errorf("snapshot storage unavailable")
	}
	key, err := h.config.Secrets.open(snapshot.KeyVersion, snapshot.KeyCiphertext, secretBinding(snapshot.WorkspaceID, snapshot.ID, sandboxSnapshotKeyPurpose))
	if err != nil {
		return manifest, nil, err
	}
	defer clear(key)
	aead, err := snapshotCipher(key)
	if err != nil {
		return manifest, nil, err
	}
	data, err := engine.GetComputerCheckpoint(ctx, snapshot.scope(), snapshot.Manifest)
	if err != nil {
		return manifest, nil, err
	}
	defer clear(data)
	planned, err := sdk.PlanComputerCheckpointObject(snapshot.scope(), data)
	if err != nil || planned != snapshot.Manifest || len(data) < aead.NonceSize()+aead.Overhead() {
		return manifest, nil, fmt.Errorf("invalid encrypted snapshot manifest")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], snapshot.binding(-1))
	if err != nil {
		return manifest, nil, fmt.Errorf("snapshot manifest authentication failed")
	}
	defer clear(plain)
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return manifest, nil, err
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return manifest, nil, fmt.Errorf("snapshot manifest has trailing data")
	}
	if manifest.Version != 2 || manifest.WorkspaceID != snapshot.WorkspaceID || manifest.SnapshotID != snapshot.ID || manifest.ImageID != snapshot.ImageID ||
		manifest.Stats != snapshot.Stats || manifest.Options != snapshot.Options || len(manifest.Parts) < 1 || len(manifest.Parts) > sandboxSnapshotMaxParts {
		return manifest, nil, fmt.Errorf("snapshot manifest does not match its owner")
	}
	if err = wrencomputer.ValidateSandboxSnapshotStats(manifest.Stats, manifest.Options); err != nil {
		return manifest, nil, err
	}
	var compressed int64
	stored := snapshot.Manifest.Size
	for i, part := range manifest.Parts {
		digest, e := hex.DecodeString(part.SHA256)
		if part.Ordinal != i || part.Bytes < 1 || part.Bytes > sandboxSnapshotChunkBytes ||
			(i != len(manifest.Parts)-1 && part.Bytes != sandboxSnapshotChunkBytes) ||
			e != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != part.SHA256 ||
			part.Object.Size != part.Bytes+int64(aead.NonceSize()+aead.Overhead()) {
			return manifest, nil, fmt.Errorf("snapshot part metadata is invalid")
		}
		if e = sdk.ValidateComputerCheckpointObject(snapshot.scope(), part.Object); e != nil {
			return manifest, nil, e
		}
		compressed += part.Bytes
		stored += part.Object.Size
	}
	if compressed != snapshot.Stats.CompressedBytes || stored != snapshot.StoredBytes {
		return manifest, nil, fmt.Errorf("snapshot byte totals do not match")
	}
	return manifest, aead, nil
}

type sandboxSnapshotReader struct {
	ctx      context.Context
	cancel   context.CancelFunc
	engine   ComputerCheckpointEngine
	snapshot SandboxSnapshot
	manifest sandboxSnapshotManifest
	aead     cipher.AEAD
	hash     hash.Hash
	index    int
	buffer   []byte
	position int
	closed   bool
	err      error
}

func (h *HTTPServer) openSandboxSnapshot(ctx context.Context, workspace, id string) (*sandboxSnapshotReader, SandboxSnapshot, error) {
	snapshot, err := h.service.Store.SandboxSnapshot(ctx, workspace, id)
	if err != nil {
		return nil, snapshot, err
	}
	operation, cancel := context.WithCancel(ctx)
	manifest, aead, err := h.snapshotManifest(operation, snapshot)
	if err != nil {
		cancel()
		return nil, snapshot, err
	}
	engine := h.service.Engine.(ComputerCheckpointEngine)
	return &sandboxSnapshotReader{ctx: operation, cancel: cancel, engine: engine, snapshot: snapshot, manifest: manifest, aead: aead, hash: sha256.New()}, snapshot, nil
}

func (r *sandboxSnapshotReader) Read(destination []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if r.err != nil {
		return 0, r.err
	}
	if len(destination) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.position == len(r.buffer) {
		clear(r.buffer)
		r.buffer = nil
		r.position = 0
		if r.index == len(r.manifest.Parts) {
			return 0, io.EOF
		}
		r.err = r.loadPart()
		if r.err != nil {
			return 0, r.err
		}
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n := copy(destination, r.buffer[r.position:])
	r.position += n
	return n, nil
}

func (r *sandboxSnapshotReader) loadPart() error {
	part := r.manifest.Parts[r.index]
	operation, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	sealed, err := r.engine.GetComputerCheckpoint(operation, r.snapshot.scope(), part.Object)
	if err != nil {
		return err
	}
	defer clear(sealed)
	planned, err := sdk.PlanComputerCheckpointObject(r.snapshot.scope(), sealed)
	if err != nil || planned != part.Object || len(sealed) < r.aead.NonceSize()+r.aead.Overhead() {
		return fmt.Errorf("snapshot part integrity check failed")
	}
	plain, err := r.aead.Open(nil, sealed[:r.aead.NonceSize()], sealed[r.aead.NonceSize():], r.snapshot.binding(part.Ordinal))
	if err != nil {
		return fmt.Errorf("snapshot part authentication failed")
	}
	digest := sha256.Sum256(plain)
	if int64(len(plain)) != part.Bytes || hex.EncodeToString(digest[:]) != part.SHA256 {
		clear(plain)
		return fmt.Errorf("snapshot plaintext did not match its manifest")
	}
	_, _ = r.hash.Write(plain)
	r.index++
	// Verify the complete digest BEFORE exposing the final part, so an HTTP
	// download cannot appear complete if the assembled stream is wrong.
	if r.index == len(r.manifest.Parts) && hex.EncodeToString(r.hash.Sum(nil)) != r.snapshot.Stats.ArchiveSHA256 {
		clear(plain)
		return fmt.Errorf("snapshot stream digest did not match")
	}
	r.buffer = plain
	return nil
}

func (r *sandboxSnapshotReader) Close() error {
	if !r.closed {
		r.closed = true
		r.cancel()
		clear(r.buffer)
		r.buffer = nil
		r.aead = nil
	}
	return nil
}
