package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/wrencomputer"
	"github.com/canter0/canter/sdk"
)

type snapshotStoreSource struct {
	data       []byte
	calls      int
	afterWrite func()
}

func (s *snapshotStoreSource) ExportSandboxSnapshot(_ context.Context, options wrencomputer.SandboxSnapshotOptions, destination io.Writer) (wrencomputer.SandboxSnapshotStats, error) {
	s.calls++
	stats, err := wrencomputer.ValidateSandboxArchive(bytes.NewReader(s.data), options)
	if err != nil {
		return stats, err
	}
	if _, err = io.Copy(destination, bytes.NewReader(s.data)); err != nil {
		return stats, err
	}
	if s.afterWrite != nil {
		s.afterWrite()
	}
	return stats, nil
}

type snapshotStoreEngine struct {
	*sandboxFakeEngine
	failPut     bool
	corruptRead bool
	beforePut   func(string, sdk.ComputerCheckpointObject)
	latePut     []byte
	lateScope   string
	lateObject  sdk.ComputerCheckpointObject
}

func (e *snapshotStoreEngine) PutComputerCheckpoint(ctx context.Context, scope string, data []byte) (sdk.ComputerCheckpointObject, error) {
	object, err := sdk.PlanComputerCheckpointObject(scope, data)
	if err != nil {
		return object, err
	}
	if e.beforePut != nil {
		e.beforePut(scope, object)
	}
	if e.failPut {
		e.latePut, e.lateScope, e.lateObject = bytes.Clone(data), scope, object
		return object, fmt.Errorf("simulated storage failure")
	}
	return e.sandboxFakeEngine.PutComputerCheckpoint(ctx, scope, data)
}

func (e *snapshotStoreEngine) DeleteComputerCheckpoint(ctx context.Context, scope string, object sdk.ComputerCheckpointObject) error {
	if err := sdk.ValidateComputerCheckpointObject(scope, object); err != nil {
		return err
	}
	if e.failDelete {
		return fmt.Errorf("simulated erasure failure")
	}
	if _, exists := e.objects[object.Key]; !exists {
		return nil // R2 DELETE is idempotent even if PUT never reached storage.
	}
	return e.sandboxFakeEngine.DeleteComputerCheckpoint(ctx, scope, object)
}

func (e *snapshotStoreEngine) GetComputerCheckpoint(ctx context.Context, scope string, object sdk.ComputerCheckpointObject) ([]byte, error) {
	data, err := e.sandboxFakeEngine.GetComputerCheckpoint(ctx, scope, object)
	if err == nil && e.corruptRead && len(data) > 0 && strings.Contains(scope, "/sandbox-snapshots/") {
		data[0] ^= 1
	}
	return data, err
}

func snapshotStoreArchive(t *testing.T, size int64) []byte {
	t.Helper()
	var data bytes.Buffer
	zipped := gzip.NewWriter(&data)
	archive := tar.NewWriter(zipped)
	for _, header := range []tar.Header{
		{Name: "home/", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0700},
		{Name: "workspace/data.bin", Typeflag: tar.TypeReg, Mode: 0600, Size: size},
	} {
		if err := archive.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := io.CopyN(archive, rand.Reader, header.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func snapshotStoreFixture(t *testing.T) (*sandboxFixtureT, Sandbox, sdk.ComputerCheckpointObject, *snapshotStoreEngine) {
	t.Helper()
	f := sandboxFixture(t)
	f.enable(t)
	v := f.create(t, "saved project", "free-1x1", http.StatusAccepted)
	ctx := context.Background()
	version, sealed, err := f.h.config.Secrets.seal([]byte("previous good checkpoint"), secretBinding(v.WorkspaceID, v.ID, "sandbox-checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := f.engine.PutComputerCheckpoint(ctx, v.checkpointScope(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(previous)
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true,checkpoint_object_ref=$2,checkpoint_key_version=$3 WHERE id=$1`, v.ID, string(raw), version); err != nil {
		t.Fatal(err)
	}
	v, err = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	engine := &snapshotStoreEngine{sandboxFakeEngine: f.engine}
	f.h.service.Engine = engine
	return f, v, previous, engine
}

func TestSandboxSnapshotStorePublishesVerifiedParts(t *testing.T) {
	f, v, previous, engine := snapshotStoreFixture(t)
	ctx := context.Background()
	source := &snapshotStoreSource{data: snapshotStoreArchive(t, 17<<20)}
	engine.beforePut = func(scope string, object sdk.ComputerCheckpointObject) {
		var planned bool
		if err := f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_snapshot_parts WHERE object_ref->>'key'=$1 AND state='planned')`, object.Key).Scan(&planned); err != nil || !planned {
			t.Fatalf("object has no durable intent before PUT: %v", err)
		}
		current, err := f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
		if err != nil || current.CheckpointObject != previous {
			t.Fatal("previous checkpoint replaced during upload", err)
		}
	}
	saved, err := f.h.captureSandboxSnapshot(ctx, v, source, false)
	if err != nil {
		t.Fatal(err)
	}
	var current, pending string
	if err = f.s.pool.QueryRow(ctx, `SELECT COALESCE(current_snapshot_id,''),COALESCE(pending_snapshot_id,'') FROM sandboxes WHERE id=$1`, v.ID).Scan(&current, &pending); err != nil || current != saved.ID || pending != "" {
		t.Fatalf("publication: %s %s %v", current, pending, err)
	}
	var parts int
	var bytesStored int64
	if err = f.s.pool.QueryRow(ctx, `SELECT count(*),sum((object_ref->>'size')::bigint) FROM sandbox_snapshot_parts WHERE snapshot_id=$1 AND state='stored'`, saved.ID).Scan(&parts, &bytesStored); err != nil || parts != 3 || bytesStored != saved.StoredBytes {
		t.Fatalf("expected two data parts plus manifest, counted once: %d %d %v", parts, bytesStored, err)
	}
	var legacyTracked bool
	if err = f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_storage_erasures WHERE scope=$1 AND object_key=$2)`, v.checkpointScope(), previous.Key).Scan(&legacyTracked); err != nil || !legacyTracked {
		t.Fatal("legacy erasure handle was lost", err)
	}
	if _, err = engine.GetComputerCheckpoint(ctx, v.checkpointScope(), previous); err != nil {
		t.Fatal("legacy data was removed before asynchronous cleanup", err)
	}
	reader, stored, err := f.h.openSandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, source.data) || stored.Stats.CompressedBytes != int64(len(source.data)) {
		t.Fatal("snapshot round trip changed archive bytes", err)
	}
	if _, _, err = f.h.openSandboxSnapshot(ctx, "another-workspace", saved.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("snapshot crossed organization boundary", err)
	}
	// A publication acknowledgement can be lost. Abandon must not erase a
	// committed key, and the next reconciliation must reuse the committed save.
	f.h.abandonSandboxSnapshot(saved)
	again, err := f.h.captureSandboxSnapshot(ctx, v, source, false)
	if err != nil || again.ID != saved.ID || source.calls != 1 {
		t.Fatalf("committed save was re-exported or erased: calls=%d err=%v", source.calls, err)
	}
	manifest, _, err := f.h.snapshotManifest(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	first := manifest.Parts[0].Object
	engine.objects[first.Key][len(engine.objects[first.Key])-1] ^= 1
	reader, _, err = f.h.openSandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var firstByte [1]byte
	if n, err := reader.Read(firstByte[:]); n != 0 || err == nil {
		t.Fatal("corrupt encrypted part released plaintext")
	}
}

func TestSandboxSnapshotStoreFailuresKeepPreviousSave(t *testing.T) {
	for _, failure := range []string{"put", "readback", "publication", "deleted during export"} {
		t.Run(failure, func(t *testing.T) {
			f, v, previous, engine := snapshotStoreFixture(t)
			ctx := context.Background()
			source := &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}
			switch failure {
			case "put":
				engine.failPut = true
			case "readback":
				engine.corruptRead = true
			case "publication":
				// This fires after parts/manifest verify and the snapshot READY
				// update runs, proving the pointer and READY update roll back together.
				t.Cleanup(func() {
					if _, err := f.s.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_snapshot_publication ON sandboxes; DROP FUNCTION IF EXISTS reject_snapshot_publication()`); err != nil {
						t.Error(err)
					}
				})
				if _, err := f.s.pool.Exec(ctx, `CREATE FUNCTION reject_snapshot_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'publication unavailable'; END $$;
CREATE TRIGGER reject_snapshot_publication BEFORE UPDATE OF current_snapshot_id ON sandboxes FOR EACH ROW WHEN (NEW.current_snapshot_id IS NOT NULL) EXECUTE FUNCTION reject_snapshot_publication()`); err != nil {
					t.Fatal(err)
				}
			case "deleted during export":
				source.afterWrite = func() {
					if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET delete_requested=true WHERE id=$1`, v.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.h.captureSandboxSnapshot(ctx, v, source, false); err == nil {
				t.Fatal("failed save reported success")
			}
			current, err := f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
			if err != nil || current.CheckpointObject != previous {
				t.Fatal("failure replaced previous good checkpoint", err)
			}
			var ready int
			if err = f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_snapshots WHERE workspace_id=$1 AND state='ready'`, v.WorkspaceID).Scan(&ready); err != nil || ready != 0 {
				t.Fatal("failed snapshot became ready", err)
			}
			var pending string
			if err = f.s.pool.QueryRow(ctx, `SELECT COALESCE(pending_snapshot_id,'') FROM sandboxes WHERE id=$1`, v.ID).Scan(&pending); err != nil || pending != "" {
				t.Fatal("failed intent not detached", err)
			}
			var keys int
			if err = f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_snapshots WHERE workspace_id=$1 AND octet_length(key_ciphertext)>0`, v.WorkspaceID).Scan(&keys); err != nil || keys != 0 {
				t.Fatal("abandoned data key survived", err)
			}
		})
	}
}

func TestSandboxSnapshotGarbageHonorsReferences(t *testing.T) {
	f, v, _, engine := snapshotStoreFixture(t)
	ctx := context.Background()
	saved, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false)
	if err != nil {
		t.Fatal(err)
	}
	assertReady := func() {
		t.Helper()
		if err := f.h.collectSandboxSnapshotGarbage(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
		if err != nil || current.State != "ready" || len(current.KeyCiphertext) == 0 {
			t.Fatal("referenced snapshot was erased", err)
		}
	}
	assertReady() // current machine reference
	if _, err = f.s.pool.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name) VALUES('template-test',$1,'toolchain')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id) VALUES($1,'template-test',1,$2)`, v.WorkspaceID, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandboxes SET current_snapshot_id=NULL,phase='deleted' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	assertReady() // template outlives source sandbox
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=true WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandbox_template_versions SET deleted_at=now() WHERE template_id='template-test'`); err != nil {
		t.Fatal(err)
	}
	assertReady() // separately retained snapshot outlives the template version
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=false WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	engine.failDelete = true
	if err = f.h.collectSandboxSnapshotGarbage(ctx); err == nil {
		t.Fatal("storage erasure error was lost")
	}
	pending, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil || pending.State != "deleting" || len(pending.KeyCiphertext) != 0 {
		t.Fatal("erasure failure discarded handles or retained an unused data key", err)
	}
	var handles int
	if err = f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_storage_erasures WHERE scope=$1`, saved.scope()).Scan(&handles); err != nil || handles != 2 {
		t.Fatal("missing retry handles", handles, err)
	}
	engine.failDelete = false
	if _, err = f.s.pool.Exec(ctx, `UPDATE sandbox_storage_erasures SET retry_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err = f.h.collectSandboxSnapshotGarbage(ctx); err != nil {
		t.Fatal(err)
	}
	deleted, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil || deleted.State != "deleted" || len(engine.objects) != 0 {
		t.Fatal("last-reference cleanup did not finish", err)
	}
}

func TestSandboxSnapshotGarbageRemembersUncertainPut(t *testing.T) {
	f, v, previous, engine := snapshotStoreFixture(t)
	ctx := context.Background()
	engine.failPut = true
	if _, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false); err == nil {
		t.Fatal("uncertain PUT was published")
	}
	if err := f.h.collectSandboxSnapshotGarbage(ctx); err != nil {
		t.Fatal(err)
	}
	var remembered bool
	if err := f.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_storage_erasures WHERE scope=$1 AND object_key=$2 AND uncertain AND erased_at IS NOT NULL)`, engine.lateScope, engine.lateObject.Key).Scan(&remembered); err != nil || !remembered {
		t.Fatal("uncertain write's erasure handle was forgotten", err)
	}
	// The remote PUT completes after the first DELETE acknowledgement.
	engine.objects[engine.lateObject.Key] = bytes.Clone(engine.latePut)
	engine.scopes[engine.lateObject.Key] = engine.lateScope
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_storage_erasures SET retry_at=now() WHERE scope=$1`, engine.lateScope); err != nil {
		t.Fatal(err)
	}
	if err := f.h.collectSandboxSnapshotGarbage(ctx); err != nil {
		t.Fatal(err)
	}
	if _, remains := engine.objects[engine.lateObject.Key]; remains {
		t.Fatal("late object survived tombstone recheck")
	}
	if _, err := engine.GetComputerCheckpoint(ctx, v.checkpointScope(), previous); err != nil {
		t.Fatal("cleanup removed the previous good checkpoint", err)
	}
}

func TestSandboxSnapshotLibraryRetention(t *testing.T) {
	f, v, _, _ := snapshotStoreFixture(t)
	ctx := context.Background()
	source := &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}
	saved, err := f.h.captureSandboxSnapshot(ctx, v, source, false)
	if err != nil {
		t.Fatal(err)
	}
	// GET list contains the save; secret/key/manifest fields absent.
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots", nil)
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list status %d: %s", rec.Code, rec.Body)
	}
	// Raw JSON — decoder would ignore json:"-" fields, so inspect the wire bytes.
	var rawPage struct {
		Snapshots      []map[string]json.RawMessage `json:"snapshots"`
		HistoryEnabled bool                         `json:"historyEnabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawPage); err != nil {
		t.Fatal(err)
	}
	if len(rawPage.Snapshots) != 1 {
		t.Fatalf("list has %d snapshots, want the one captured save", len(rawPage.Snapshots))
	}
	var listedSaved bool
	for _, s := range rawPage.Snapshots {
		var id string
		_ = json.Unmarshal(s["id"], &id)
		if id == saved.ID {
			listedSaved = true
		}
		for _, field := range []string{"keyVersion", "keyCiphertext", "manifest", "options", "imageId", "KeyVersion", "KeyCiphertext", "Manifest", "Options", "ImageID"} {
			if _, leaked := s[field]; leaked {
				t.Fatalf("secret/key/manifest field %s leaked", field)
			}
		}
	}
	if !listedSaved {
		t.Fatalf("captured save %s absent from list", saved.ID)
	}
	// No billing → retain rejected, retained/budget unchanged.
	patch := func(input SandboxSnapshotUpdate) *httptest.ResponseRecorder {
		body, _ := json.Marshal(input)
		r := httptest.NewRequest(http.MethodPatch, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots/"+saved.ID, bytes.NewReader(body))
		r.AddCookie(f.cookie)
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		return rec
	}
	tru2, fls := true, false
	if rec := patch(SandboxSnapshotUpdate{Retained: &tru2}); rec.Code != 409 {
		t.Fatalf("retain without billing status %d want 409 body=%s", rec.Code, rec.Body.String())
	}
	current, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Retained {
		t.Fatal("retained flipped without billing")
	}
	// Active billing → name+retain succeeds; bytes/key/manifest unchanged.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	name := "my-save"
	if rec := patch(SandboxSnapshotUpdate{Name: &name, Retained: &tru2}); rec.Code != 200 {
		t.Fatalf("retain status %d: %s", rec.Code, rec.Body)
	}
	kept, kerr := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if kerr != nil {
		t.Fatal(kerr)
	}
	if kept.StoredBytes != saved.StoredBytes || !bytes.Equal(kept.KeyCiphertext, saved.KeyCiphertext) || !reflect.DeepEqual(kept.Manifest, saved.Manifest) {
		t.Fatal("metadata update changed bytes/key/manifest")
	}
	// Free-current meter still 0 — one snapshot resource, zero units.
	resources, rerr := f.s.sandboxStorageResources(ctx, v.WorkspaceID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(resources) != 1 || resources[0].Units != 0 {
		t.Fatalf("storage resources %#v", resources)
	}
	// History reservation exists; free-current meter still 0.
	var reservations int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox_history' AND phase='reserved'`, v.WorkspaceID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations == 0 {
		t.Fatal("no history reservation after retain")
	}
	// Cancel billing → re-pin no new hold; unpin succeeds and releases it.
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if rec := patch(SandboxSnapshotUpdate{Retained: &tru2}); rec.Code != 200 {
		t.Fatalf("re-pin after cancel status %d", rec.Code)
	}
	if rec := patch(SandboxSnapshotUpdate{Retained: &fls}); rec.Code != 200 {
		t.Fatalf("unpin status %d", rec.Code)
	}
	var remaining int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='sandbox_history' AND phase='reserved'`, v.WorkspaceID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("history reservation not released on unpin: %d", remaining)
	}
	// Viewer → GET 200, PATCH 403.
	if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, v.WorkspaceID, f.account.ID); err != nil {
		t.Fatal(err)
	}
	if rec := patch(SandboxSnapshotUpdate{Name: &name}); rec.Code != 403 {
		t.Fatalf("viewer patch status %d want 403", rec.Code)
	}
	vlist := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots", nil)
	vlist.AddCookie(f.cookie)
	vlistRec := httptest.NewRecorder()
	f.h.ServeHTTP(vlistRec, vlist)
	if vlistRec.Code != 200 {
		t.Fatalf("viewer list status %d", vlistRec.Code)
	}
	vget := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots/"+saved.ID, nil)
	vget.AddCookie(f.cookie)
	vgetRec := httptest.NewRecorder()
	f.h.ServeHTTP(vgetRec, vget)
	if vgetRec.Code != 200 || !strings.Contains(vgetRec.Body.String(), saved.ID) {
		t.Fatalf("viewer detail status %d", vgetRec.Code)
	}
	// Foreign workspace → not found.
	foreign := httptest.NewRequest(http.MethodGet, "/v1/workspaces/wsp_other/sandbox-snapshots", nil)
	foreign.AddCookie(f.cookie)
	recF := httptest.NewRecorder()
	f.h.ServeHTTP(recF, foreign)
	if recF.Code != 403 && recF.Code != 404 {
		t.Fatalf("foreign list status %d", recF.Code)
	}
}

func TestSandboxSnapshotLibraryBudgetRollback(t *testing.T) {
	f, v, _, _ := snapshotStoreFixture(t)
	ctx := context.Background()
	saved, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Cap so tight the estimated hold cannot fit → retain rejected, no state change.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_usage_caps(workspace_id,limit_cents,reserved_cents,spent_cents) VALUES($1,1,1,0) ON CONFLICT (workspace_id) DO UPDATE SET limit_cents=1,reserved_cents=1,spent_cents=0`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	tru := true
	body, _ := json.Marshal(SandboxSnapshotUpdate{Retained: &tru})
	req := httptest.NewRequest(http.MethodPatch, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots/"+saved.ID, bytes.NewReader(body))
	req.AddCookie(f.cookie)
	req.Header.Set("Origin", "http://canter.test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 429 && rec.Code != 409 {
		t.Fatalf("over-budget retain status %d want capacity/conflict", rec.Code)
	}
	kept, _ := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if kept.Retained {
		t.Fatal("retained flipped despite rejected hold")
	}
	var reserved int
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, v.WorkspaceID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 1 {
		t.Fatalf("budget diverged: reserved=%d", reserved)
	}
}

func TestSandboxSnapshotLibraryPagination(t *testing.T) {
	f, v, _, _ := snapshotStoreFixture(t)
	ctx := context.Background()
	// 120 compact metadata rows — ties on ready_at, distinct IDs.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 120; i++ {
		if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_snapshots(id,workspace_id,source_sandbox_id,source_generation,source_size,source_disk_gib,image_id,name,state,retained,options,stats,stored_bytes,created_at,ready_at)
			VALUES($1,$2,$3,1,'s-2x4',20,'img','p'||$1,'ready',true,'{}','{}',10,now(),$4)`, fmt.Sprintf("snap_p%03d", i), v.WorkspaceID, v.ID, base.Add(-time.Duration(i/3)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// A row from another sandbox so the source filter has something to exclude.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_snapshots(id,workspace_id,source_sandbox_id,source_generation,source_size,source_disk_gib,image_id,name,state,retained,options,stats,stored_bytes,created_at,ready_at)
		VALUES('snap_foreign_src',$1,'sbx_other',1,'s-2x4',20,'img','foreign','ready',true,'{}','{}',10,now(),$2)`, v.WorkspaceID, base); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	// Traverse all pages — no duplicates/misses.
	seen := map[string]bool{}
	cursor := ""
	total := 0
	for i := 0; i < 5; i++ {
		page, err := f.s.ListSandboxSnapshots(ctx, v.WorkspaceID, "", cursor)
		if err != nil {
			t.Fatal(err)
		}
		if (i == 0 && len(page.Snapshots) != 100) || (i == 1 && len(page.Snapshots) != 21) || i > 1 {
			t.Fatalf("unexpected page %d size %d", i, len(page.Snapshots))
		}
		for _, s := range page.Snapshots {
			if seen[s.ID] {
				t.Fatalf("duplicate %s", s.ID)
			}
			seen[s.ID] = true
			total++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if total != 121 {
		t.Fatalf("paginated %d want 121", total)
	}
	if !seen["snap_foreign_src"] {
		t.Fatal("second-source row missing from unfiltered traversal")
	}
	// Source filter excludes the other sandbox's row and returns matches.
	page, err := f.s.ListSandboxSnapshots(ctx, v.WorkspaceID, v.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Snapshots) == 0 {
		t.Fatal("source filter returned nothing")
	}
	for _, s := range page.Snapshots {
		if s.SourceSandboxID != v.ID {
			t.Fatalf("source filter leaked %s", s.SourceSandboxID)
		}
	}
	// Invalid cursor, missing cursor row, NULL ready_at cursor → 404.
	for _, bad := range []string{"snap_missing"} {
		if _, err := f.s.ListSandboxSnapshots(ctx, v.WorkspaceID, "", bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid cursor %s: %v want ErrNotFound", bad, err)
		}
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_snapshots(id,workspace_id,source_sandbox_id,source_generation,source_size,source_disk_gib,image_id,name,state,retained,options,stats,stored_bytes,created_at)
		VALUES('snap_creating',$1,$2,1,'s-2x4',20,'img','c','creating',false,'{}','{}',10,now())`, v.WorkspaceID, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListSandboxSnapshots(ctx, v.WorkspaceID, "", "snap_creating"); !errors.Is(err, ErrNotFound) {
		t.Fatal("NULL ready_at cursor accepted")
	}
	// Missing and not-yet-ready cursors are both ordinary HTTP 404 responses.
	for _, cursor := range []string{"snap_missing", "snap_creating"} {
		badReq := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots?before="+cursor, nil)
		badReq.AddCookie(f.cookie)
		badRec := httptest.NewRecorder()
		f.h.ServeHTTP(badRec, badReq)
		if badRec.Code != 404 {
			t.Fatalf("invalid HTTP cursor %s status %d", cursor, badRec.Code)
		}
	}
	// Deleted cursor with valid timestamp still paginates.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET state='deleted' WHERE id='snap_p000'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListSandboxSnapshots(ctx, v.WorkspaceID, "", "snap_p000"); err != nil {
		t.Fatal("deleted cursor rejected")
	}
	// Pin one → delete machine → disable product 409; unpin all → allowed.
	pin := "snap_p001"
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=true WHERE id=$1`, pin); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleted' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/workspaces/"+v.WorkspaceID+"/products/sandboxes", nil)
	req.AddCookie(f.cookie)
	req.Header.Set("Origin", "http://canter.test")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("disable with retained snapshot status %d", rec.Code)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=false WHERE workspace_id=$1`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest(http.MethodDelete, "/v1/workspaces/"+v.WorkspaceID+"/products/sandboxes", nil)
	req2.AddCookie(f.cookie)
	req2.Header.Set("Origin", "http://canter.test")
	rec2 := httptest.NewRecorder()
	f.h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("disable after unpin status %d", rec2.Code)
	}
}

func TestSandboxSnapshotRetainVsGC(t *testing.T) {
	f, v, _, _ := snapshotStoreFixture(t)
	ctx := context.Background()
	saved, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Retain after clearing the source pointer while ready.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET current_snapshot_id=NULL WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	retain := true
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	if _, err := f.h.UpdateSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, saved.ID, actor, SandboxSnapshotUpdate{Retained: &retain}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.queueSandboxSnapshotErasure(ctx, v.WorkspaceID, saved.ID, false); err != nil {
		t.Fatal(err)
	}
	kept, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State != "ready" || kept.KeyVersion != saved.KeyVersion || !bytes.Equal(kept.KeyCiphertext, saved.KeyCiphertext) {
		t.Fatalf("retained snapshot erased: state=%s", kept.State)
	}
	// Unpin → erasure proceeds.
	retain = false
	if _, err := f.h.UpdateSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, saved.ID, actor, SandboxSnapshotUpdate{Retained: &retain}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.queueSandboxSnapshotErasure(ctx, v.WorkspaceID, saved.ID, false); err != nil {
		t.Fatal(err)
	}
	gone, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.State != "deleting" || gone.KeyVersion != "" || len(gone.KeyCiphertext) != 0 {
		t.Fatal("unpinned snapshot not cryptographically erased and queued")
	}
	// GET must return 404, rather than passing a nil error to writeStoreError.
	read := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots/"+saved.ID, nil)
	read.AddCookie(f.cookie)
	readResult := httptest.NewRecorder()
	f.h.ServeHTTP(readResult, read)
	if readResult.Code != http.StatusNotFound {
		t.Fatalf("read deleting snapshot: %d", readResult.Code)
	}
	// PATCH cannot resurrect an erased snapshot.
	tru := true
	body, _ := json.Marshal(SandboxSnapshotUpdate{Retained: &tru})
	req := httptest.NewRequest(http.MethodPatch, "/v1/workspaces/"+v.WorkspaceID+"/sandbox-snapshots/"+saved.ID, bytes.NewReader(body))
	req.AddCookie(f.cookie)
	req.Header.Set("Origin", "http://canter.test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch deleting snapshot: %d", rec.Code)
	}
}

func TestSandboxTemplatesVersioning(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "tmpl-src", "free-1x1", http.StatusAccepted))
	archive := snapshotStoreArchive(t, 1024)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snap1, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: archive}, false)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		r.AddCookie(f.cookie)
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		return rec
	}
	base := "/v1/workspaces/" + v.WorkspaceID + "/sandbox-templates"
	// No billing → 409.
	if rec := post(base, SandboxTemplateCreate{Name: "t1", SnapshotID: snap1.ID}); rec.Code != 409 {
		t.Fatalf("create without billing %d", rec.Code)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	rec := post(base, SandboxTemplateCreate{Name: "t1", SnapshotID: snap1.ID})
	if rec.Code != 201 {
		t.Fatalf("create %d: %s", rec.Code, rec.Body)
	}
	var tmpl SandboxTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &tmpl); err != nil {
		t.Fatal(err)
	}
	// Replay same request → same template ID.
	rec2 := post(base, SandboxTemplateCreate{Name: "t1", SnapshotID: snap1.ID})
	var tmpl2 SandboxTemplate
	_ = json.Unmarshal(rec2.Body.Bytes(), &tmpl2)
	if tmpl2.ID != tmpl.ID {
		t.Fatalf("idempotent replay got different template %s", tmpl2.ID)
	}
	// Add version 2 + replay returns same version.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET generation=generation+1,phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snap2, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: snapshotStoreArchive(t, 2048)}, false)
	if err != nil {
		t.Fatal(err)
	}
	vr := post(base+"/"+tmpl.ID+"/versions", map[string]string{"snapshotId": snap2.ID})
	if vr.Code != 201 {
		t.Fatalf("add version %d: %s", vr.Code, vr.Body)
	}
	var ver SandboxTemplateVersion
	_ = json.Unmarshal(vr.Body.Bytes(), &ver)
	if ver.Version != 2 {
		t.Fatalf("version=%d want 2", ver.Version)
	}
	// Detail shows latest=2, versions desc.
	got, _ := f.s.SandboxTemplate(ctx, v.WorkspaceID, tmpl.ID)
	if got.Latest == nil || got.Latest.Version != 2 || len(got.Versions) != 2 {
		t.Fatalf("latest/versions wrong: %#v", got.Latest)
	}
	// Delete version 2 → latest back to 1; next version uses 3.
	if err := f.h.DeleteSandboxTemplateVersion(ctx, v.WorkspaceID, f.account.ID, tmpl.ID, 2, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	got, _ = f.s.SandboxTemplate(ctx, v.WorkspaceID, tmpl.ID)
	if got.Latest == nil || got.Latest.Version != 1 {
		t.Fatalf("latest after delete: %#v", got.Latest)
	}
	vr2 := post(base+"/"+tmpl.ID+"/versions", map[string]string{"snapshotId": snap2.ID})
	var ver3 SandboxTemplateVersion
	_ = json.Unmarshal(vr2.Body.Bytes(), &ver3)
	if ver3.Version != 3 {
		t.Fatalf("next version=%d want 3", ver3.Version)
	}
	// Inactive billing → delete allowed.
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := f.h.DeleteSandboxTemplate(ctx, v.WorkspaceID, f.account.ID, tmpl.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatalf("inactive delete: %v", err)
	}
}

func TestSandboxCreateSavedIdempotency(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "idmp-src", "free-1x1", http.StatusAccepted))
	archive := snapshotStoreArchive(t, 1024)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snap1, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: archive}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Free-current fork without billing succeeds (sandbox's own save).
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',current_snapshot_id=$2 WHERE id=$1`, v.ID, snap1.ID); err != nil {
		t.Fatal(err)
	}
	clone, err := f.h.CreateSandbox(ctx, v.WorkspaceID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxCreateInput{Name: "clone", Size: "free-1x1", SnapshotID: snap1.ID})
	if err != nil {
		t.Fatalf("free fork: %v", err)
	}
	if clone.CurrentSnapshotID != snap1.ID || clone.Phase != "waking" {
		t.Fatalf("clone snap=%s phase=%s", clone.CurrentSnapshotID, clone.Phase)
	}
	// Same stable Idempotency-Key replay returns the same sandbox.
	clone2, err := f.h.CreateSandbox(ctx, v.WorkspaceID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxCreateInput{Name: "clone", Size: "free-1x1", SnapshotID: snap1.ID, IdempotencyKey: "stable-key-1"})
	if err != nil {
		t.Fatal(err)
	}
	clone3, err := f.h.CreateSandbox(ctx, v.WorkspaceID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxCreateInput{Name: "clone", Size: "free-1x1", SnapshotID: snap1.ID, IdempotencyKey: "stable-key-1"})
	if err != nil {
		t.Fatal(err)
	}
	if clone2.ID != clone3.ID {
		t.Fatalf("idempotent replay got different ID %s vs %s", clone2.ID, clone3.ID)
	}
	// Old no-source hash equals the literal fingerprint.
	expected := sha256.Sum256([]byte(`{"n":"plain","s":"free-1x1","l":[],"i":-1}`))
	if got := sandboxRequestHash(SandboxCreateInput{Name: "plain", Size: "free-1x1"}, -1); got != hex.EncodeToString(expected[:]) {
		t.Fatalf("old fingerprint changed: %s", got)
	}
}

func TestSandboxSnapshotSelection(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "sel", "free-1x1", http.StatusAccepted))
	archiveA := snapshotStoreArchive(t, 1024)
	archiveB := snapshotStoreArchive(t, 2048)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snapB, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: archiveB}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Park asleep with snapshotB as current.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',sleep_requested=false,current_snapshot_id=$2 WHERE id=$1`, v.ID, snapB.ID); err != nil {
		t.Fatal(err)
	}
	// Save A needs its own generation + sleeping intent so it's a real new save.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET generation=generation+1,phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snapA, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: archiveA}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Back to asleep with B still current so selection swaps to A.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',sleep_requested=false,current_snapshot_id=$2 WHERE id=$1`, v.ID, snapB.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	// Select A — previous B retained, A becomes current, generation+1.
	out, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: snapA.ID, ExpectedGeneration: v.Generation})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if out.CurrentSnapshotID != snapA.ID || out.Generation != v.Generation+1 || out.Phase != "asleep" {
		t.Fatalf("select result snap=%s gen=%d phase=%s", out.CurrentSnapshotID, out.Generation, out.Phase)
	}
	var retainedB bool
	if err := f.s.pool.QueryRow(ctx, `SELECT retained FROM sandbox_snapshots WHERE id=$1`, snapB.ID).Scan(&retainedB); err != nil || !retainedB {
		t.Fatal("previous save not retained")
	}
	// Same-target same-generation → no-op.
	out2, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: snapA.ID, ExpectedGeneration: v.Generation + 1})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Generation != v.Generation+1 {
		t.Fatal("same-target changed generation")
	}
	// Stale generation rejected.
	if _, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: snapB.ID, ExpectedGeneration: 99}); err == nil {
		t.Fatal("stale generation accepted")
	}
	// Wake restores the selected archive.
	if err := f.h.RequestSandboxWake(ctx, v.WorkspaceID, v.ID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	d := &SandboxDispatcher{Server: f.h}
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		client := &sandboxFakeClient{capabilities: wrencomputer.Capabilities{WorkflowProtocol: 1, PersistentFiles: true, DurableJobs: true}, archive: archiveA, jobs: f.jobs}
		f.lastClient = client
		return client, nil
	}
	if err := d.Reconcile(ctx, v.WorkspaceID, v.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if f.lastClient == nil || len(f.lastClient.restored) == 0 || !bytes.Equal(f.lastClient.restored[0], archiveA) {
		t.Fatal("wake did not restore selected snapshot bytes")
	}
}

func TestSandboxTemplateVersionParseRegression(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "parse", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snap, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := f.h.CreateSandboxTemplate(ctx, v.WorkspaceID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxTemplateCreate{Name: "t", SnapshotID: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + v.WorkspaceID + "/sandbox-templates/" + tmpl.ID + "/versions/"
	for _, bad := range []string{"1junk", "1.5", "2147483648", "-1", "abc"} {
		req := httptest.NewRequest(http.MethodDelete, base+bad, nil)
		req.AddCookie(f.cookie)
		req.Header.Set("Origin", "http://canter.test")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("DELETE versions/%s status %d want 404", bad, rec.Code)
		}
	}
	got, _ := f.s.SandboxTemplate(ctx, v.WorkspaceID, tmpl.ID)
	if got.Latest == nil || got.Latest.Version != 1 {
		t.Fatal("version 1 deleted by bad parse")
	}
}

func TestSandboxTemplateReferenceLifetimes(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "life-src", "free-1x1", http.StatusAccepted))
	archive := snapshotStoreArchive(t, 1024)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='sleeping',sleep_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	snap, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: archive}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := f.h.CreateSandboxTemplate(ctx, v.WorkspaceID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxTemplateCreate{Name: "life", SnapshotID: snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Delete source machine; template still protects the snapshot.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleted',current_snapshot_id=NULL WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.h.queueSandboxSnapshotErasure(ctx, v.WorkspaceID, snap.ID, false); err != nil {
		t.Fatal(err)
	}
	kept, _ := f.s.SandboxSnapshot(ctx, v.WorkspaceID, snap.ID)
	if kept.State != "ready" {
		t.Fatalf("snapshot erased despite template reference: state=%s", kept.State)
	}
	// Clone from template — wakes restoring the source bytes.
	clone, err := f.h.CreateSandbox(ctx, v.WorkspaceID, f.account.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxCreateInput{Name: "clone", Size: "free-1x1", TemplateID: tmpl.ID})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if clone.CurrentSnapshotID != snap.ID || clone.Phase != "waking" {
		t.Fatalf("clone snap=%s phase=%s", clone.CurrentSnapshotID, clone.Phase)
	}
	// Delete template — clone's own current_snapshot_id now protects the save.
	if err := f.h.DeleteSandboxTemplate(ctx, v.WorkspaceID, f.account.ID, tmpl.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.queueSandboxSnapshotErasure(ctx, v.WorkspaceID, snap.ID, false); err != nil {
		t.Fatal(err)
	}
	kept, _ = f.s.SandboxSnapshot(ctx, v.WorkspaceID, snap.ID)
	if kept.State != "ready" {
		t.Fatalf("snapshot erased despite clone reference: state=%s", kept.State)
	}
	var before int
	if err := f.s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_snapshots WHERE workspace_id=$1`, v.WorkspaceID).Scan(&before); err != nil || before != 1 {
		t.Fatalf("fork copied snapshot data: count=%d err=%v", before, err)
	}
	changed := snapshotStoreArchive(t, 2048)
	f.h.sandboxDial = func(wrencomputer.Config) (sandboxAgentClient, error) {
		f.lastClient = &sandboxFakeClient{capabilities: wrencomputer.Capabilities{PersistentFiles: true, DurableJobs: true}, archive: changed, jobs: f.jobs}
		return f.lastClient, nil
	}
	d := &SandboxDispatcher{Server: f.h}
	if err := d.Reconcile(ctx, v.WorkspaceID, clone.ID); err != nil {
		t.Fatal(err)
	}
	if f.lastClient == nil || len(f.lastClient.restored) != 1 || !bytes.Equal(f.lastClient.restored[0], archive) {
		t.Fatal("fork did not restore source bytes")
	}
	if err := f.h.RequestSandboxSleep(ctx, v.WorkspaceID, clone.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(ctx, v.WorkspaceID, clone.ID); err != nil {
		t.Fatal(err)
	}
	clone, err = f.s.Sandbox(ctx, v.WorkspaceID, clone.ID)
	if err != nil || clone.Phase != "asleep" || clone.CurrentSnapshotID == snap.ID {
		t.Fatalf("fork did not save independently: %v", err)
	}
	if err := f.h.queueSandboxSnapshotErasure(ctx, v.WorkspaceID, snap.ID, false); err != nil {
		t.Fatal(err)
	}
	original, err := f.s.SandboxSnapshot(ctx, v.WorkspaceID, snap.ID)
	if err != nil || original.State != "deleting" || len(original.KeyCiphertext) != 0 {
		t.Fatalf("unreferenced original not erased: %v", err)
	}
	reader, _, err := f.h.openSandboxSnapshot(ctx, v.WorkspaceID, clone.CurrentSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("fork's independent save damaged: %v", err)
	}
}

// libraryFixtureSave creates a verified save and parks this test's simulated VM.
func libraryFixtureSave(t *testing.T, f *sandboxFixtureT, id string, data []byte) SandboxSnapshot {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET generation=generation+1,phase='sleeping',sleep_requested=true WHERE workspace_id=$1 AND id=$2`, f.space.ID, id); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Sandbox(ctx, f.space.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: data}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',sleep_requested=false,address=NULL,host_fingerprint=NULL,awake_since=NULL WHERE workspace_id=$1 AND id=$2`, f.space.ID, id); err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestSandboxTemplatePermissionsAndLimits(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	source := f.ready(t, f.create(t, "library", "free-1x1", http.StatusAccepted))
	a := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 1024))
	b := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 2048))
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := f.h.CreateSandboxTemplate(ctx, f.space.ID, f.account.ID, actor, SandboxTemplateCreate{Name: "library", SnapshotID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + f.space.ID + "/sandbox-templates"
	if _, err := f.h.CreateSandboxTemplate(ctx, f.space.ID, f.account.ID, actor, SandboxTemplateCreate{Name: "LIBRARY", SnapshotID: b.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name/different save: %v", err)
	}
	second, err := f.h.AddSandboxTemplateVersion(ctx, f.space.ID, f.account.ID, tmpl.ID, b.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.h.AddSandboxTemplateVersion(ctx, f.space.ID, f.account.ID, tmpl.ID, b.ID, actor)
	if err != nil || replayed != second {
		t.Fatalf("inactive version replay changed binding: %v", err)
	}
	replayedTemplate, err := f.h.CreateSandboxTemplate(ctx, f.space.ID, f.account.ID, actor, SandboxTemplateCreate{Name: "library", SnapshotID: a.ID})
	if err != nil || replayedTemplate.ID != tmpl.ID {
		t.Fatalf("inactive create replay changed template: %v", err)
	}
	if replayedTemplate.Latest == nil || replayedTemplate.Latest.Version != 2 || len(replayedTemplate.Versions) != 2 || replayedTemplate.Versions[1].Version != 1 {
		t.Fatal("version order/latest incorrect")
	}
	list, err := f.s.ListSandboxTemplates(ctx, f.space.ID)
	if err != nil || len(list) != 1 || list[0].Latest == nil || list[0].Latest.SnapshotID != b.ID || len(list[0].Versions) != 0 {
		t.Fatalf("template summary incorrect: %v", err)
	}
	preserved, err := f.s.SandboxSnapshot(ctx, f.space.ID, a.ID)
	if err != nil || !bytes.Equal(preserved.KeyCiphertext, a.KeyCiphertext) || !reflect.DeepEqual(preserved.Manifest, a.Manifest) || preserved.StoredBytes != a.StoredBytes {
		t.Fatalf("template references mutated snapshot data: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base, base + "/" + tmpl.ID} {
		requireStatus(t, requestJSON(t, f.h, http.MethodGet, path, nil, f.cookie), http.StatusOK)
	}
	for _, mutation := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, base, SandboxTemplateCreate{Name: "forbidden", SnapshotID: a.ID}},
		{http.MethodPost, base + "/" + tmpl.ID + "/versions", map[string]string{"snapshotId": a.ID}},
		{http.MethodDelete, base + "/" + tmpl.ID, nil},
		{http.MethodDelete, base + "/" + tmpl.ID + "/versions/1", nil},
		{http.MethodPost, "/v1/workspaces/" + f.space.ID + "/sandboxes/" + source.ID + "/restore", SandboxSnapshotSelection{SnapshotID: a.ID, ExpectedGeneration: 1}},
	} {
		requireStatus(t, requestJSON(t, f.h, mutation.method, mutation.path, mutation.body, f.cookie), http.StatusForbidden)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='owner' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_billing SET status='active' WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	foreignAccount, foreign, foreignCookie := workspaceSession(t, f.s, "foreign-library@example.com")
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name) VALUES('stp_foreign',$1,'foreign')`, foreign.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodGet, base+"/stp_foreign", nil, f.cookie), http.StatusNotFound)
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, base+"/stp_foreign", nil, f.cookie), http.StatusNotFound)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET workspace_id=$2 WHERE id=$1`, b.ID, foreign.ID); err == nil {
		t.Fatal("composite foreign key allowed moving a referenced save across organizations")
	}
	// Reuse the same Store; constructing a second sandboxFixture would reset it.
	foreignFixture := *f
	foreignFixture.account, foreignFixture.space, foreignFixture.cookie = foreignAccount, foreign, foreignCookie
	foreignFixture.enable(t)
	foreignVM := foreignFixture.ready(t, foreignFixture.create(t, "foreign-source", "free-1x1", http.StatusAccepted))
	foreignSave := libraryFixtureSave(t, &foreignFixture, foreignVM.ID, snapshotStoreArchive(t, 512))
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base, SandboxTemplateCreate{Name: "foreign", SnapshotID: foreignSave.ID}, f.cookie), http.StatusNotFound)
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base+"/"+tmpl.ID+"/versions", map[string]string{"snapshotId": foreignSave.ID}, f.cookie), http.StatusNotFound)
	for _, invalid := range []string{"", strings.Repeat("x", 129), "bad\x01id"} {
		requireStatus(t, requestJSON(t, f.h, http.MethodPost, base, SandboxTemplateCreate{Name: "invalid", SnapshotID: invalid}, f.cookie), http.StatusConflict)
	}
	c := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 768))
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET options=jsonb_set(options,'{includeBrowser}','true') WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base, SandboxTemplateCreate{Name: "browser", SnapshotID: c.ID}, f.cookie), http.StatusConflict)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET options=jsonb_set(options,'{includeBrowser}','false'),state='creating' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodPost, base, SandboxTemplateCreate{Name: "unverified", SnapshotID: c.ID}, f.cookie), http.StatusNotFound)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET state='ready' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	// Synthetic metadata fills the bounded library; these empty templates are
	// equivalent to templates whose last version was removed.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name) SELECT 'stp_limit_'||i,$1,'limit-'||i FROM generate_series(1,99) i`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.CreateSandboxTemplate(ctx, f.space.ID, f.account.ID, actor, SandboxTemplateCreate{Name: "over-limit", SnapshotID: c.ID}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("template cap: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id) SELECT $1,$2,i,$3 FROM generate_series(3,100) i`, f.space.ID, tmpl.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.AddSandboxTemplateVersion(ctx, f.space.ID, f.account.ID, tmpl.ID, c.ID, actor); !errors.Is(err, ErrCapacity) {
		t.Fatalf("active version cap: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_template_versions SET deleted_at=now() WHERE workspace_id=$1 AND template_id=$2 AND version>2`, f.space.ID, tmpl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id,deleted_at) VALUES($1,$2,2147483647,$3,now())`, f.space.ID, tmpl.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.AddSandboxTemplateVersion(ctx, f.space.ID, f.account.ID, tmpl.ID, c.ID, actor); !errors.Is(err, ErrCapacity) {
		t.Fatalf("version integer exhaustion: %v", err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_templates SET deleted_at=now() WHERE workspace_id=$1 AND id<>$2`, f.space.ID, tmpl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleted',current_snapshot_id=NULL WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	product := "/v1/workspaces/" + f.space.ID + "/products/sandboxes"
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, product, nil, f.cookie), http.StatusConflict)
	if err := f.h.DeleteSandboxTemplate(ctx, f.space.ID, f.account.ID, tmpl.ID, actor); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, requestJSON(t, f.h, http.MethodDelete, product, nil, f.cookie), http.StatusOK)
}

func TestSandboxCreateSavedTemplateReplayAndPolicy(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	source := f.ready(t, f.create(t, "replay-source", "free-1x1", http.StatusAccepted))
	a := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 1024))
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	tmpl, err := f.h.CreateSandboxTemplate(ctx, f.space.ID, f.account.ID, actor, SandboxTemplateCreate{Name: "replay", SnapshotID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	input := SandboxCreateInput{Name: "latest-copy", Size: "free-1x1", TemplateID: tmpl.ID, IdempotencyKey: "template-latest-stable"}
	first, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor, input)
	if err != nil || first.CurrentSnapshotID != a.ID {
		t.Fatalf("initial latest selection: %v", err)
	}
	b := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 2048))
	if _, err := f.h.AddSandboxTemplateVersion(ctx, f.space.ID, f.account.ID, tmpl.ID, b.ID, actor); err != nil {
		t.Fatal(err)
	}
	assertReplay := func() {
		t.Helper()
		replay, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor, input)
		if err != nil || replay.ID != first.ID || replay.CurrentSnapshotID != a.ID {
			t.Fatalf("retry resolved a different latest version: %v", err)
		}
	}
	assertReplay()
	conflicting := input
	conflicting.TemplateVersion = 2
	if _, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor, conflicting); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key different source accepted: %v", err)
	}
	for version, snapshotID := range map[int]string{1: a.ID, 2: b.ID} {
		copy, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor, SandboxCreateInput{Name: fmt.Sprintf("version-%d", version), Size: "free-1x1", TemplateID: tmpl.ID, TemplateVersion: version})
		if err != nil || copy.CurrentSnapshotID != snapshotID {
			t.Fatalf("explicit version %d: %v", version, err)
		}
	}
	countState := func() [2]int {
		t.Helper()
		var counts [2]int
		if err := f.s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sandboxes WHERE workspace_id=$1),(SELECT count(*) FROM usage_reservations WHERE workspace_id=$1)`, f.space.ID).Scan(&counts[0], &counts[1]); err != nil {
			t.Fatal(err)
		}
		return counts
	}
	before := countState()
	// Model a save from a larger disk without allocating a second provider VM.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET source_disk_gib=20 WHERE workspace_id=$1 AND id=$2`, f.space.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor, SandboxCreateInput{Name: "too-small", Size: "free-1x1", SnapshotID: b.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("undersized fork: %v", err)
	}
	if countState() != before {
		t.Fatal("failed source resolution left an allocation/reservation")
	}
	if err := f.h.DeleteSandboxTemplate(ctx, f.space.ID, f.account.ID, tmpl.ID, actor); err != nil {
		t.Fatal(err)
	}
	assertReplay()
	c := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 512))
	retain := true
	if _, err := f.h.UpdateSandboxSnapshot(ctx, f.space.ID, f.account.ID, c.ID, actor, SandboxSnapshotUpdate{Retained: &retain}); err != nil {
		t.Fatal(err)
	}
	// C is now extra history; other free copies refer only to A/B.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET current_snapshot_id=$2 WHERE id=$1`, source.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	before = countState()
	if _, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor, SandboxCreateInput{Name: "paid-history", Size: "free-1x1", SnapshotID: c.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("extra-history fork without billing: %v", err)
	}
	if countState() != before {
		t.Fatal("billing rejection left an allocation/reservation")
	}
}

func TestSandboxSnapshotSelectionAtomicFailures(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	source := f.ready(t, f.create(t, "atomic-selection", "free-1x1", http.StatusAccepted))
	a := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 1024))
	b := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 2048))
	v, err := f.s.Sandbox(ctx, f.space.ID, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	input := SandboxSnapshotSelection{SnapshotID: a.ID, ExpectedGeneration: v.Generation}
	assertUnchanged := func() {
		t.Helper()
		current, err := f.s.Sandbox(ctx, f.space.ID, source.ID)
		if err != nil || current.CurrentSnapshotID != b.ID || current.Generation != v.Generation {
			t.Fatalf("rejected selection changed machine: %v", err)
		}
		prior, err := f.s.SandboxSnapshot(ctx, f.space.ID, b.ID)
		if err != nil || prior.Retained {
			t.Fatalf("rejected selection pinned previous save: %v", err)
		}
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_usage_caps SET limit_cents=1,reserved_cents=1,spent_cents=0 WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SelectSandboxSnapshot(ctx, f.space.ID, f.account.ID, v.ID, actor, input); !errors.Is(err, ErrCapacity) {
		t.Fatalf("selection must fail capacity atomically: %v", err)
	}
	assertUnchanged()
	var reserved int
	if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, f.space.ID).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("failed selection changed budget: %d %v", reserved, err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_usage_caps SET limit_cents=10000,reserved_cents=0 WHERE workspace_id=$1`, f.space.ID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, setup, reset string
	}{
		{"ready", `UPDATE sandboxes SET phase='ready' WHERE id=$1`, `UPDATE sandboxes SET phase='asleep' WHERE id=$1`},
		{"deleting", `UPDATE sandboxes SET delete_requested=true WHERE id=$1`, `UPDATE sandboxes SET delete_requested=false WHERE id=$1`},
		{"legacy", `UPDATE sandboxes SET checkpoint_object_ref='{"key":"legacy","size":1}' WHERE id=$1`, `UPDATE sandboxes SET checkpoint_object_ref=NULL WHERE id=$1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.s.pool.Exec(ctx, test.setup, v.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.h.SelectSandboxSnapshot(ctx, f.space.ID, f.account.ID, v.ID, actor, input); !errors.Is(err, ErrConflict) {
				t.Fatalf("unsafe selection allowed: %v", err)
			}
			assertUnchanged()
			if _, err := f.s.pool.Exec(ctx, test.reset, v.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, test := range []struct {
		name, snapshot, setup, reset string
	}{
		{"undersized", a.ID, `UPDATE sandbox_snapshots SET source_disk_gib=20 WHERE id=$1`, `UPDATE sandbox_snapshots SET source_disk_gib=10 WHERE id=$1`},
		{"damaged prior", b.ID, `UPDATE sandbox_snapshots SET state='failed' WHERE id=$1`, `UPDATE sandbox_snapshots SET state='ready' WHERE id=$1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.s.pool.Exec(ctx, test.setup, test.snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := f.h.SelectSandboxSnapshot(ctx, f.space.ID, f.account.ID, v.ID, actor, input); !errors.Is(err, ErrConflict) {
				t.Fatalf("unsafe selection allowed: %v", err)
			}
			assertUnchanged()
			if _, err := f.s.pool.Exec(ctx, test.reset, test.snapshot); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, id := range []string{"", "bad\x01id", strings.Repeat("x", 129)} {
		bad := input
		bad.SnapshotID = id
		if _, err := f.h.SelectSandboxSnapshot(ctx, f.space.ID, f.account.ID, v.ID, actor, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid snapshot id: %v", err)
		}
		assertUnchanged()
	}
	_, foreign, _ := workspaceSession(t, f.s, "selection-foreign@example.com")
	// No reference currently points to A; its parts retain A's original scope,
	// so use a separate foreign metadata row solely for authorization testing.
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_snapshots(id,workspace_id,source_sandbox_id,source_generation,source_size,source_disk_gib,image_id,name,state,retained,options,stats,stored_bytes,ready_at)
		VALUES('snap_foreign_selection',$1,'foreign-source',1,'free-1x1',10,'img','foreign','ready',false,'{}','{}',10,now())`, foreign.ID); err != nil {
		t.Fatal(err)
	}
	foreignInput := input
	foreignInput.SnapshotID = "snap_foreign_selection"
	if _, err := f.h.SelectSandboxSnapshot(ctx, f.space.ID, f.account.ID, v.ID, actor, foreignInput); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign selection: %v", err)
	}
	assertUnchanged()
	path := "/v1/workspaces/" + f.space.ID + "/sandboxes/" + v.ID + "/restore"
	requireStatus(t, requestJSON(t, f.h, http.MethodGet, path, nil, f.cookie), http.StatusMethodNotAllowed)
	if f.engine.applies.Load() != 0 {
		t.Fatal("selection unexpectedly applied a provider VM")
	}
}

func TestSandboxSnapshotSelectionRejects(t *testing.T) {
	f := sandboxFixture(t)
	ctx := context.Background()
	f.enable(t)
	v := f.ready(t, f.create(t, "selrej", "free-1x1", http.StatusAccepted))
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, v.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	gen := v.Generation
	// Ready (not asleep) → rejected, pointer/gen unchanged.
	if _, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: "snap_x", ExpectedGeneration: gen}); err == nil {
		t.Fatal("ready sandbox accepted selection")
	}
	// Asleep + deleteRequested → rejected.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',delete_requested=true WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: "snap_x", ExpectedGeneration: gen}); err == nil {
		t.Fatal("delete-requested accepted")
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET delete_requested=false WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	// Stale generation → rejected.
	if _, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: "snap_x", ExpectedGeneration: gen + 99}); err == nil {
		t.Fatal("stale generation accepted")
	}
	// Malformed/empty ID → rejected.
	if _, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: "", ExpectedGeneration: gen}); err == nil {
		t.Fatal("empty snapshot id accepted")
	}
	// Legacy checkpoint pointer → rejected.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET checkpoint_object_ref='{"key":"k","sha256":"s","size":1}',checkpoint_key_version='v1' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.SelectSandboxSnapshot(ctx, v.WorkspaceID, f.account.ID, v.ID, sdk.ActorRef{Kind: "human", ID: f.account.ID}, SandboxSnapshotSelection{SnapshotID: "snap_x", ExpectedGeneration: gen}); err == nil {
		t.Fatal("legacy checkpoint pointer accepted")
	}
	fresh, _ := f.s.Sandbox(ctx, v.WorkspaceID, v.ID)
	if fresh.Generation != gen || fresh.CurrentSnapshotID != "" {
		t.Fatal("reject changed pointer/generation")
	}
}

func TestSandboxLibraryAgentTools(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	source := f.ready(t, f.create(t, "agent-source", "free-1x1", http.StatusAccepted))
	a := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 1024))
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	call := func(write bool, name string, args map[string]any) (any, error) {
		raw, _ := json.Marshal(args)
		return f.h.callSandboxTool(ctx, f.space.ID, f.account.ID, actor, write, name, raw)
	}
	mcpCall := func(name string, args map[string]any) (any, error) {
		raw, _ := json.Marshal(args)
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
		return f.h.callMCPTool(req, Principal{Account: &f.account, Actor: actor}, name, raw)
	}

	t.Run("registered", func(t *testing.T) {
		create, _ := sandboxToolByName("canter_sandbox_create")
		props, _ := create.InputSchema["properties"].(map[string]any)
		for _, field := range []string{"snapshotId", "templateId", "templateVersion"} {
			if props[field] == nil {
				t.Fatalf("canter_sandbox_create schema missing %s", field)
			}
		}
		want := []string{"canter_sandbox_snapshots", "canter_sandbox_snapshot_get", "canter_sandbox_snapshot_update", "canter_sandbox_templates", "canter_sandbox_template_create", "canter_sandbox_template_version_add", "canter_sandbox_template_delete", "canter_sandbox_template_version_delete", "canter_sandbox_restore"}
		registered := map[string]bool{}
		for _, tool := range mcpTools() {
			registered[tool.Name] = true
		}
		for _, name := range want {
			if _, ok := sandboxToolByName(name); !ok {
				t.Fatalf("%s missing from sandboxToolByName", name)
			}
			if !registered[name] {
				t.Fatalf("%s missing from mcpTools", name)
			}
		}
	})

	t.Run("reads_without_write_and_no_secrets", func(t *testing.T) {
		page, err := call(false, "canter_sandbox_snapshots", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page)
		for _, field := range []string{"keyCiphertext", "keyVersion", "manifest", "options", "imageId"} {
			if strings.Contains(string(raw), `"`+field+`"`) {
				t.Fatalf("secret field %s leaked", field)
			}
		}
		if _, err := call(false, "canter_sandbox_snapshot_get", map[string]any{"id": a.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := call(false, "canter_sandbox_templates", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("mutations_require_write", func(t *testing.T) {
		for _, name := range []string{"canter_sandbox_snapshot_update", "canter_sandbox_template_create", "canter_sandbox_template_version_add", "canter_sandbox_template_delete", "canter_sandbox_template_version_delete", "canter_sandbox_restore"} {
			if !sandboxMutatingTools[name] {
				t.Fatalf("%s not in sandboxMutatingTools", name)
			}
			if _, err := call(false, name, map[string]any{"id": "bad", "version": 1, "snapshotId": "bad", "expectedGeneration": 1, "name": "x"}); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s write=false: %v want ErrForbidden", name, err)
			}
		}
	})

	t.Run("snapshot_update", func(t *testing.T) {
		if _, err := call(true, "canter_sandbox_snapshot_update", map[string]any{"id": a.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("empty update: %v want ErrConflict", err)
		}
		name := "renamed"
		out, err := call(true, "canter_sandbox_snapshot_update", map[string]any{"id": a.ID, "name": name})
		if err != nil {
			t.Fatal(err)
		}
		if out.(SandboxSnapshot).Name != name || out.(SandboxSnapshot).Retained != a.Retained {
			t.Fatal("rename dropped retained flag")
		}
		tru := false
		out, err = call(true, "canter_sandbox_snapshot_update", map[string]any{"id": a.ID, "retained": tru})
		if err != nil {
			t.Fatal(err)
		}
		if out.(SandboxSnapshot).Retained {
			t.Fatal("retained=false did not unpin")
		}
	})

	t.Run("billing_and_source_create", func(t *testing.T) {
		// No billing: template create + retained:true rejected.
		if _, err := call(true, "canter_sandbox_template_create", map[string]any{"name": "t", "snapshotId": a.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("template create without billing: %v", err)
		}
		if _, err := call(true, "canter_sandbox_snapshot_update", map[string]any{"id": a.ID, "retained": true}); !errors.Is(err, ErrConflict) {
			t.Fatalf("retain without billing: %v", err)
		}
		// Free-current snapshot copy succeeds without billing.
		clone, err := call(true, "canter_sandbox_create", map[string]any{"name": "clone", "size": "free-1x1", "snapshotId": a.ID, "idempotencyKey": "agent-key-1"})
		if err != nil {
			t.Fatalf("free-current copy: %v", err)
		}
		if clone.(Sandbox).CurrentSnapshotID != a.ID || clone.(Sandbox).Phase != "waking" {
			t.Fatalf("clone snap=%s phase=%s", clone.(Sandbox).CurrentSnapshotID, clone.(Sandbox).Phase)
		}
		// Same key + same source → same ID; changed source → conflict.
		again, err := call(true, "canter_sandbox_create", map[string]any{"name": "clone", "size": "free-1x1", "snapshotId": a.ID, "idempotencyKey": "agent-key-1"})
		if err != nil || again.(Sandbox).ID != clone.(Sandbox).ID {
			t.Fatal("idempotent replay got different ID")
		}
		b := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 2048))
		if _, err := call(true, "canter_sandbox_create", map[string]any{"name": "clone", "size": "free-1x1", "snapshotId": b.ID, "idempotencyKey": "agent-key-1"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("same key different source: %v", err)
		}
	})

	t.Run("template_lifecycle", func(t *testing.T) {
		if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
			t.Fatal(err)
		}
		tmpl, err := call(true, "canter_sandbox_template_create", map[string]any{"name": "agent-tpl", "snapshotId": a.ID})
		if err != nil {
			t.Fatal(err)
		}
		tplID := tmpl.(SandboxTemplate).ID
		b := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 2048))
		ver, err := call(true, "canter_sandbox_template_version_add", map[string]any{"id": tplID, "snapshotId": b.ID})
		if err != nil || ver.(SandboxTemplateVersion).Version != 2 {
			t.Fatalf("add version: %v", err)
		}
		// Replay add same snapshot → same version.
		replay, err := call(true, "canter_sandbox_template_version_add", map[string]any{"id": tplID, "snapshotId": b.ID})
		if err != nil || replay.(SandboxTemplateVersion).Version != 2 {
			t.Fatalf("idempotent version add: %v", err)
		}
		// Explicit templateVersion=1 copy points at a.
		clone, err := call(true, "canter_sandbox_create", map[string]any{"name": "v1clone", "size": "free-1x1", "templateId": tplID, "templateVersion": 1, "idempotencyKey": "agent-key-2"})
		if err != nil || clone.(Sandbox).CurrentSnapshotID != a.ID {
			t.Fatalf("explicit version copy: %v", err)
		}
		// Delete version 2 → latest back to 1.
		if _, err := call(true, "canter_sandbox_template_version_delete", map[string]any{"id": tplID, "version": 2}); err != nil {
			t.Fatal(err)
		}
		detail, err := call(true, "canter_sandbox_templates", map[string]any{"id": tplID})
		if err != nil || detail.(SandboxTemplate).Latest == nil || detail.(SandboxTemplate).Latest.Version != 1 {
			t.Fatal("latest not 1 after version delete")
		}
		// Delete template — clone keeps its save.
		if _, err := call(true, "canter_sandbox_template_delete", map[string]any{"id": tplID}); err != nil {
			t.Fatal(err)
		}
		kept, _ := f.s.Sandbox(ctx, f.space.ID, clone.(Sandbox).ID)
		if kept.CurrentSnapshotID != a.ID {
			t.Fatal("clone pointer lost after template delete")
		}
	})

	t.Run("restore_selection", func(t *testing.T) {
		b := libraryFixtureSave(t, f, source.ID, snapshotStoreArchive(t, 2048))
		if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',sleep_requested=false,checkpoint_object_ref=NULL,checkpoint_key_version='',checkpoint_nonce=NULL,checkpoint_manifest=NULL WHERE id=$1`, source.ID); err != nil {
			t.Fatal(err)
		}
		v, err := f.s.Sandbox(ctx, f.space.ID, source.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Select the earlier save a — b is already the source's current pointer.
		out, err := call(true, "canter_sandbox_restore", map[string]any{"id": source.ID, "snapshotId": a.ID, "expectedGeneration": v.Generation})
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if out.(Sandbox).CurrentSnapshotID != a.ID || out.(Sandbox).Generation != v.Generation+1 || out.(Sandbox).Phase != "asleep" {
			t.Fatalf("restore result: %v", out)
		}
		// Prior current save b must be retained by the swap.
		var retainedB bool
		if err := f.s.pool.QueryRow(ctx, `SELECT retained FROM sandbox_snapshots WHERE id=$1`, b.ID).Scan(&retainedB); err != nil || !retainedB {
			t.Fatal("previous save not retained after selection")
		}
		// Stale generation rejected.
		if _, err := call(true, "canter_sandbox_restore", map[string]any{"id": source.ID, "snapshotId": a.ID, "expectedGeneration": v.Generation}); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale generation: %v", err)
		}
	})

	t.Run("viewer_readonly_and_foreign", func(t *testing.T) {
		if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := mcpCall("canter_sandbox_snapshots", map[string]any{"workspaceId": f.space.ID}); err != nil {
			t.Fatalf("viewer read: %v", err)
		}
		for _, name := range []string{"canter_sandbox_snapshot_update", "canter_sandbox_template_create", "canter_sandbox_template_version_add", "canter_sandbox_template_delete", "canter_sandbox_template_version_delete", "canter_sandbox_restore"} {
			if _, err := mcpCall(name, map[string]any{"workspaceId": f.space.ID, "id": a.ID, "snapshotId": a.ID, "name": "x", "version": 1, "expectedGeneration": 1}); !errors.Is(err, ErrForbidden) {
				t.Fatalf("viewer %s: %v want ErrForbidden", name, err)
			}
		}
		// Foreign scope: another membership sees nothing of this org's saves.
		other, otherSpace, _ := workspaceSession(t, f.s, "sandbox-library-foreign@example.com")
		raw, err := json.Marshal(map[string]any{"workspaceId": otherSpace.ID, "id": a.ID})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
		if _, err := f.h.callMCPTool(req, Principal{Account: &f.account, Actor: actor}, "canter_sandbox_snapshot_get", raw); !errors.Is(err, ErrForbidden) {
			t.Fatalf("foreign membership: %v", err)
		}
		if _, err := f.h.callMCPTool(req, Principal{Account: &other, Actor: sdk.ActorRef{Kind: "human", ID: other.ID}}, "canter_sandbox_snapshot_get", raw); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign save in own scope: %v", err)
		}
		// Restore role for cleanup.
		if _, err := f.s.pool.Exec(ctx, `UPDATE memberships SET role='owner' WHERE workspace_id=$1 AND account_id=$2`, f.space.ID, f.account.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSandboxDesktopAgentTools(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	ctx := context.Background()
	actor := sdk.ActorRef{Kind: "human", ID: f.account.ID}
	call := func(write bool, name string, args map[string]any) (any, error) {
		raw, _ := json.Marshal(args)
		return f.h.callSandboxTool(ctx, f.space.ID, f.account.ID, actor, write, name, raw)
	}

	t.Run("registered", func(t *testing.T) {
		create, _ := sandboxToolByName("canter_sandbox_create")
		props, _ := create.InputSchema["properties"].(map[string]any)
		if props["desktop"] == nil {
			t.Fatal("create schema missing desktop")
		}
		for _, name := range []string{"canter_sandbox_screenshot", "canter_sandbox_input"} {
			if _, ok := sandboxToolByName(name); !ok {
				t.Fatalf("%s missing", name)
			}
		}
		if !sandboxMutatingTools["canter_sandbox_input"] {
			t.Fatal("input not in sandboxMutatingTools")
		}
	})

	t.Run("screenshot_and_input", func(t *testing.T) {
		// Paid desktop fixture — s-2x4 with active billing.
		if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active')`, f.space.ID); err != nil {
			t.Fatal(err)
		}
		v := f.ready(t, func() Sandbox {
			created, err := f.h.CreateSandbox(ctx, f.space.ID, f.account.ID, actor,
				SandboxCreateInput{Name: "desktop", Size: "s-2x4", Desktop: true})
			if err != nil {
				t.Fatal(err)
			}
			return created
		}())
		desktopFixtureClient(t, f)

		// Screenshot without writeGrant — read tool.
		out, err := call(false, "canter_sandbox_screenshot", map[string]any{"id": v.ID})
		if err != nil {
			t.Fatalf("screenshot: %v", err)
		}
		shot, ok := out.(sandboxDesktopScreenshotResult)
		if !ok || shot.SandboxID != v.ID || shot.Capture.MediaType != "image/png" || shot.Width != 4 || shot.Height != 4 {
			t.Fatalf("screenshot result: %+v", out)
		}
		raw, _ := json.Marshal(shot)
		for _, field := range []string{"keyCiphertext", "plan", "address"} {
			if strings.Contains(string(raw), `"`+field+`"`) {
				t.Fatalf("screenshot leaked %s", field)
			}
		}
		// mcpToolResult produces text + image with metadata.
		result := mcpToolResult(shot, false)
		content, _ := result["content"].([]map[string]string)
		if len(content) != 2 || content[1]["type"] != "image" || content[1]["data"] == "" {
			t.Fatalf("mcp result content: %v", result["content"])
		}
		meta, _ := result["structuredContent"].(map[string]any)
		if meta["sandboxId"] != v.ID || meta["generation"] != v.Generation {
			t.Fatalf("mcp metadata: %v", meta)
		}

		// Input requires writeGrant.
		if _, err := call(false, "canter_sandbox_input", map[string]any{"id": v.ID, "kind": "click", "expectedGeneration": v.Generation}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("input write=false: %v", err)
		}
		// Stale generation rejected.
		if _, err := call(true, "canter_sandbox_input", map[string]any{"id": v.ID, "kind": "click", "expectedGeneration": v.Generation + 1}); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale generation: %v", err)
		}
		// Current generation accepted.
		out, err = call(true, "canter_sandbox_input", map[string]any{"id": v.ID, "kind": "click", "x": 10, "y": 20, "expectedGeneration": v.Generation})
		if err != nil {
			t.Fatalf("input: %v", err)
		}
		result2, _ := out.(map[string]any)
		if result2["submitted"] != true || result2["action"] != "click" {
			t.Fatalf("input result: %v", result2)
		}
		// Active human lease blocks agent input.
		if _, err := f.h.SetSandboxDesktopControl(ctx, f.space.ID, v.ID, f.account.ID, actor,
			SandboxDesktopControlInput{Mode: "human", ExpectedGeneration: v.Generation}); err != nil {
			t.Fatal(err)
		}
		if _, err := call(true, "canter_sandbox_input", map[string]any{"id": v.ID, "kind": "click", "x": 10, "y": 20, "expectedGeneration": v.Generation}); !errors.Is(err, ErrConflict) {
			t.Fatalf("agent input under human control: %v", err)
		}
	})
}
