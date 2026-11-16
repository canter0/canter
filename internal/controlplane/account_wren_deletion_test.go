package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
	"github.com/canter0/canter/sdk"
)

func TestAccountDeletionErasesSavedSandboxesBeforeIdentity(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, owner := deletionAccount(t, h, "sandbox-erase@example.test")
	otherCookie, other := deletionAccount(t, h, "sandbox-keep@example.test")
	engine := &snapshotStoreEngine{sandboxFakeEngine: &sandboxFakeEngine{}}
	h.service.Engine = engine
	createSave := func(workspace, id string) (SandboxSnapshot, []byte) {
		t.Helper()
		plan, err := json.Marshal(sdk.VPSPlan{Shape: compute.Shape{VCPU: 1, Memory: 1024, GB: 10}, ImageID: "private-image"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase,plan,sleep_requested) VALUES($1,$2,'saved project','free-1x1','free','sleeping',$3,true)`, id, workspace, plan); err != nil {
			t.Fatal(err)
		}
		v, err := s.Sandbox(ctx, workspace, id)
		if err != nil {
			t.Fatal(err)
		}
		data := snapshotStoreArchive(t, 1024)
		snapshot, err := h.captureSandboxSnapshot(ctx, v, &snapshotStoreSource{data: data}, false)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot, data
	}
	saved, _ := createSave(owner.WorkspaceID, "sbx_account_erase")
	kept, keptData := createSave(other.WorkspaceID, "sbx_account_keep")
	legacyScope := (Sandbox{WorkspaceID: owner.WorkspaceID, ID: "sbx_account_legacy"}).checkpointScope()
	legacyObject, err := engine.PutComputerCheckpoint(ctx, legacyScope, []byte("opaque legacy encrypted save"))
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal(legacyObject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sandboxes(id,workspace_id,name,size,tier,phase,checkpoint_object_ref) VALUES('sbx_account_legacy',$1,'legacy save','free-1x1','free','deleted',$2)`, owner.WorkspaceID, legacyJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleted',awake_since=NULL WHERE id='sbx_account_erase'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=true WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name) VALUES('tpl_account_erase',$1,'saved tools')`, owner.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id) VALUES($1,'tpl_account_erase',1,$2)`, owner.WorkspaceID, saved.ID); err != nil {
		t.Fatal(err)
	}
	proof := startDeletion(t, h, cookie)
	code := lastEmailCode(t, h)
	finish := func() *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"code": code})
		if err != nil {
			t.Fatal(err)
		}
		operation, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/account/delete/finish", bytes.NewReader(body)).WithContext(operation)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", h.config.PublicURL)
		request.AddCookie(cookie)
		request.AddCookie(proof)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		return response
	}
	engine.failDelete = true
	if response := finish(); response.Code == http.StatusOK {
		t.Fatal("account deletion ignored sandbox storage failure")
	}
	if _, err := s.ResolveHuman(ctx, cookie.Value); err != nil {
		t.Fatal("storage failure removed the account", err)
	}
	pending, err := s.SandboxSnapshot(ctx, owner.WorkspaceID, saved.ID)
	if err != nil || pending.State != "deleting" {
		t.Fatalf("snapshot erasure state: %s (%v)", pending.State, err)
	}
	var handles int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_storage_erasures WHERE scope=$1`, saved.scope()).Scan(&handles); err != nil || handles != 2 {
		t.Fatalf("data and manifest erasure handles: %d (%v)", handles, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_storage_erasures WHERE scope=$1`, legacyScope).Scan(&handles); err != nil || handles != 1 {
		t.Fatalf("legacy save erasure handle: %d (%v)", handles, err)
	}
	engine.failDelete = false
	requireStatus(t, finish(), http.StatusOK)
	if _, err := s.ResolveHuman(ctx, cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted account is still usable", err)
	}
	if _, err := s.SandboxSnapshot(ctx, owner.WorkspaceID, saved.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted account retained snapshot metadata", err)
	}
	var templates bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox_templates WHERE workspace_id=$1) OR EXISTS(SELECT 1 FROM sandbox_template_versions WHERE workspace_id=$1)`, owner.WorkspaceID).Scan(&templates); err != nil || templates {
		t.Fatal("deleted account retained template metadata", err)
	}
	if _, err := s.ResolveHuman(ctx, otherCookie.Value); err != nil {
		t.Fatal("erasure affected another account", err)
	}
	reader, _, err := h.openSandboxSnapshot(ctx, other.WorkspaceID, kept.ID)
	if err != nil {
		t.Fatal("erasure affected another organization's save", err)
	}
	defer reader.Close()
	restored, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(restored, keptData) {
		t.Fatal("another organization's saved data changed", err)
	}
	for _, scope := range engine.scopes {
		if scope != kept.scope() {
			t.Fatal("deleted account's saved objects remain")
		}
	}
}

func TestAccountDeletionErasesSavedComputersBeforeIdentity(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := deletionAccount(t, h, "saved-computer-delete@example.test")
	engine := &checkpointTestEngine{}
	h.service.Engine = engine
	cp := WrenComputerCheckpoint{ID: "checkpoint_account_erase", WorkspaceID: p.WorkspaceID, ConversationID: "private_deleted_chat", OwnerID: p.Account.ID, ScopeKind: "private"}
	object, err := engine.PutComputerCheckpoint(ctx, cp.storageScope(), []byte("opaque encrypted checkpoint test bytes"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(object)
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_computer_checkpoints(id,workspace_id,conversation_id,owner_id,scope_kind,computer_id,state,object) VALUES($1,$2,$3,$4,'private','deleted_computer','ready',$5)`, cp.ID, cp.WorkspaceID, cp.ConversationID, cp.OwnerID, raw); err != nil {
		t.Fatal(err)
	}
	proof := startDeletion(t, h, cookie)
	code := lastEmailCode(t, h)
	engine.failDelete = true
	failed := authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof)
	if failed.Code == http.StatusOK {
		t.Fatal("account deletion ignored storage failure")
	}
	if _, err = s.ResolveHuman(ctx, cookie.Value); err != nil {
		t.Fatal("storage failure erased identity", err)
	}
	var handles int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_computer_checkpoints WHERE id=$1`, cp.ID).Scan(&handles); err != nil || handles != 1 {
		t.Fatal("storage failure lost erasure handle", err)
	}
	engine.failDelete = false
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof), http.StatusOK)
	if _, err = s.ResolveHuman(ctx, cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted identity remains usable", err)
	}
	if len(engine.objects) != 0 {
		t.Fatal("saved files survived account deletion")
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_computer_checkpoints WHERE id=$1`, cp.ID).Scan(&handles); err != nil || handles != 0 {
		t.Fatal("erased checkpoint metadata remains", err)
	}
}

func TestAccountDeletionClosesAIAllowanceAndPreservesUncertainCharge(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	cookie, p := deletionAccount(t, h, "ai-ledger-delete@example.test")
	c, err := s.CreateConversation(ctx, p.WorkspaceID, p.Account.ID, "conv_deletion_ai", "Private draft")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureWrenAIAllowance(ctx, p.WorkspaceID, 100); err != nil {
		t.Fatal(err)
	}
	r, err := s.ReserveWrenAIUsage(ctx, aiTestInput(c, "unresolved-paid-request", "operator", 50))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DispatchWrenAIUsage(ctx, p.WorkspaceID, r.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkWrenAIUsageUncertain(ctx, p.WorkspaceID, r.ID); err != nil {
		t.Fatal(err)
	}
	proof := startDeletion(t, h, cookie)
	code := lastEmailCode(t, h)
	requireStatus(t, authRequest(t, h, "account/delete/finish", map[string]string{"code": code}, cookie, proof), http.StatusOK)
	if _, err = s.ResolveHuman(ctx, cookie.Value); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("deleted session survived", err)
	}
	var closed bool
	var held int64
	var name, state string
	if err = s.pool.QueryRow(ctx, `SELECT a.closed_at IS NOT NULL,a.held_micro_usd,w.name FROM wren_ai_allowances a JOIN workspaces w ON w.id=a.workspace_id WHERE a.workspace_id=$1`, p.WorkspaceID).Scan(&closed, &held, &name); err != nil || !closed || held != 50 || name != "Closed workspace" {
		t.Fatalf("financial shell %v %d %q %v", closed, held, name, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT state FROM wren_ai_reservations WHERE id=$1`, r.ID).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("lost uncertain charge %q %v", state, err)
	}
	if err = s.SettleWrenAIUsage(ctx, p.WorkspaceID, r.ID, 20, "late-provider-receipt", json.RawMessage(`{"cost":0.00002}`)); err != nil {
		t.Fatal("closed ledger cannot reconcile", err)
	}
	if err = s.ConfigureWrenAIAllowance(ctx, p.WorkspaceID, 1000); err == nil {
		t.Fatal("closed workspace allowance reopened")
	}
}

func TestAccountDeletionBlocksLivePrivateComputerInSharedWorkspace(t *testing.T) {
	h, c, _, _ := computerFixture(t)
	s, ctx := h.service.Store, context.Background()
	w, err := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_delete_guard", Name: "Notes", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	computer, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, w.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _, err := s.Signup(ctx, "other-owner-delete@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, other.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = s.accountDeletionWorkspacesTx(ctx, tx, c.AccountID); err == nil {
		t.Fatal("shared membership hid live private computer")
	}
	if _, err = tx.Exec(ctx, `UPDATE wren_computers SET phase='deleted' WHERE id=$1`, computer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.accountDeletionWorkspacesTx(ctx, tx, c.AccountID); err != nil {
		t.Fatal("removed private computer still blocks deletion", err)
	}
}

func TestAccountDeletionPreservesSharedRoomOwnership(t *testing.T) {
	s, h, _ := newAuthTestServer(t)
	ctx := context.Background()
	_, p := deletionAccount(t, h, "room-owner-delete@example.test")
	_, other := deletionAccount(t, h, "room-successor@example.test")
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, other.Account.ID, p.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRoom(ctx, p.WorkspaceID, p.Account.ID, WrenRoom{ID: "room_deletion_guard", Kind: "channel", Name: "Team decisions"}, true); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = s.accountDeletionWorkspacesTx(ctx, tx, p.Account.ID); err == nil {
		t.Fatal("account deletion can cascade into the team's room")
	}
	if _, err = tx.Exec(ctx, `DELETE FROM memberships WHERE account_id=$1 AND workspace_id=$2`, p.Account.ID, p.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.accountDeletionWorkspacesTx(ctx, tx, p.Account.ID); err == nil {
		t.Fatal("leaving a workspace bypassed shared room protection")
	}
}

func TestAccountDeletionRequiresSandboxRemoval(t *testing.T) {
	f := sandboxFixture(t)
	f.enable(t)
	v := f.create(t, "Deletion guard", "free-1x1", http.StatusAccepted)
	ctx := context.Background()
	tx, err := f.s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, phase := range []string{"queued", "ready", "asleep", "failed"} {
		if _, err = tx.Exec(ctx, `UPDATE sandboxes SET phase=$2 WHERE id=$1`, v.ID, phase); err != nil {
			t.Fatal(err)
		}
		_, err = f.s.accountDeletionWorkspacesTx(ctx, tx, f.account.ID)
		var blocked accountDeletionBlocked
		if !errors.As(err, &blocked) {
			t.Fatalf("phase %s must prevent identity/resource erasure: %v", phase, err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE sandboxes SET phase='deleted' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.accountDeletionWorkspacesTx(ctx, tx, f.account.ID); err != nil {
		t.Fatalf("removed sandbox still blocks deletion: %v", err)
	}
}
