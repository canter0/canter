package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/canter0/canter/sdk"
	"testing"
	"time"
)

func TestCheckpointCreationWaitsForMutationAndFencesNextWrite(t *testing.T) {
	h, c, v := operationComputer(t)
	h.service.Engine = &checkpointTestEngine{}
	ctx := context.Background()
	op, err := h.service.Store.beginComputerOperation(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer op.close()
	result := make(chan error, 1)
	go func() {
		_, e := h.CreateWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, v.ID, false, false)
		result <- e
	}()
	select {
	case err = <-result:
		t.Fatalf("snapshot admitted before remote mutation receipt: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	var count int
	if err = h.service.Store.pool.QueryRow(ctx, `SELECT count(*) FROM wren_computer_checkpoints WHERE computer_id=$1`, v.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("premature checkpoint count %d: %v", count, err)
	}
	op.close()
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot did not start after mutation finished")
	}
	if err = h.ComputerAgentWriteAllowed(ctx, c); !errors.Is(err, ErrConflict) {
		t.Fatalf("agent write admitted during snapshot: %v", err)
	}
	if err = h.computerHumanMutationAllowed(ctx, v, c.AccountID); !errors.Is(err, ErrConflict) {
		t.Fatalf("human write admitted during snapshot: %v", err)
	}
}

func TestCheckpointRejectsExpiredAllocationBeforeMetadata(t *testing.T) {
	h, c, v := operationComputer(t)
	h.service.Engine = &checkpointTestEngine{}
	ctx := context.Background()
	if _, err := h.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET expires_at=now()-interval '1 second' WHERE id=$1`, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, v.ID, false, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired computer accepted a save: %v", err)
	}
	var count int
	if err := h.service.Store.pool.QueryRow(ctx, `SELECT count(*) FROM wren_computer_checkpoints WHERE computer_id=$1`, v.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired checkpoint count %d: %v", count, err)
	}
}

type checkpointTestEngine struct {
	computerFakeEngine
	objects    map[string][]byte
	scopes     map[string]string
	failDelete bool
	puts       int
}

func (e *checkpointTestEngine) PutComputerCheckpoint(_ context.Context, scope string, data []byte) (sdk.ComputerCheckpointObject, error) {
	o, err := sdk.PlanComputerCheckpointObject(scope, data)
	if err != nil {
		return o, err
	}
	if e.objects == nil {
		e.objects = map[string][]byte{}
		e.scopes = map[string]string{}
	}
	e.objects[o.Key] = bytes.Clone(data)
	e.scopes[o.Key] = scope
	e.puts++
	return o, nil
}
func (e *checkpointTestEngine) GetComputerCheckpoint(_ context.Context, scope string, o sdk.ComputerCheckpointObject) ([]byte, error) {
	data := e.objects[o.Key]
	expected, err := sdk.PlanComputerCheckpointObject(scope, data)
	if err != nil || expected != o || e.scopes[o.Key] != scope {
		return nil, fmt.Errorf("object denied")
	}
	return bytes.Clone(data), nil
}
func (e *checkpointTestEngine) DeleteComputerCheckpoint(_ context.Context, scope string, o sdk.ComputerCheckpointObject) error {
	if e.failDelete {
		return fmt.Errorf("storage unavailable")
	}
	if e.scopes[o.Key] != scope {
		return ErrForbidden
	}
	delete(e.objects, o.Key)
	delete(e.scopes, o.Key)
	return nil
}
func checkpointControlFixture(t *testing.T) (*HTTPServer, Conversation, WrenComputerCheckpoint, *checkpointTestEngine) {
	t.Helper()
	h, c, _, job, _, _ := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	engine := &checkpointTestEngine{}
	h.service.Engine = engine
	var computer string
	if err := s.pool.QueryRow(ctx, `SELECT computer_id FROM wren_harness_executions WHERE job_id=$1`, job.ID).Scan(&computer); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, computer, false, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("checkpoint captured active coding work: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wren_harness_executions SET status='stopped',process_stopped=true WHERE job_id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	cp, err := h.CreateWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, computer, false, true)
	if err != nil {
		t.Fatal(err)
	}
	var deleting bool
	if err = s.pool.QueryRow(ctx, `SELECT delete_requested FROM wren_computers WHERE id=$1`, computer).Scan(&deleting); err != nil || deleting {
		t.Fatal("sleep deleted before verified storage")
	}
	return h, c, cp, engine
}
func TestWrenComputerCheckpointScopeEncryptionAndWakeGrant(t *testing.T) {
	h, c, cp, engine := checkpointControlFixture(t)
	ctx := context.Background()
	s := h.service.Store
	if _, err := h.computerCheckpoint(ctx, c.WorkspaceID, "foreign-account", cp.ID, false); err == nil {
		t.Fatal("foreign checkpoint metadata access")
	}
	plain := []byte("private workspace checkpoint fixture")
	version, ciphertext, err := h.config.Secrets.seal(plain, cp.binding())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plain) {
		t.Fatal("checkpoint not encrypted")
	}
	object, err := engine.PutComputerCheckpoint(ctx, cp.storageScope(), ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(object)
	if _, err = s.pool.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='ready',key_version=$2,object=$3 WHERE id=$1`, cp.ID, version, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = h.config.Secrets.open(version, ciphertext, secretBinding(c.WorkspaceID, cp.ID, "wren-computer-checkpoint:another-room:room")); err == nil {
		t.Fatal("cross-room checkpoint decryption")
	}
	visible, err := h.computerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(visible)
	if bytes.Contains(metadata, []byte(object.Key)) || bytes.Contains(metadata, ciphertext) {
		t.Fatal("storage capability exposed in metadata")
	}
	if _, err = h.WakeWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("wake clobbered active computer: %v", err)
	}
	if engine.plans != 0 {
		t.Fatal("failed wake allocated compute")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_computers SET phase='deleted',ssh_key_ciphertext=''::bytea WHERE id=$1`, cp.ComputerID); err != nil {
		t.Fatal(err)
	}
	h.config.Computers.AllowedWorkspaces = nil
	if _, err = h.WakeWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wake bypassed paid workspace grant: %v", err)
	}
	h.config.Computers.AllowedWorkspaces = []string{c.WorkspaceID}
	restored, err := h.WakeWrenComputerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID == cp.ComputerID || restored.ConversationID != cp.ConversationID || restored.RestoreCheckpointID != cp.ID {
		t.Fatal("restore allocation was not atomically bound to same scope")
	}
	current, err := h.computerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID, false)
	if err != nil || current.State != "restoring" {
		t.Fatalf("restore state %s %v", current.State, err)
	}
}
func TestWrenComputerCheckpointErasureRetainsHandleOnStorageFailure(t *testing.T) {
	h, c, cp, engine := checkpointControlFixture(t)
	ctx := context.Background()
	data := []byte("encrypted fixture")
	object, err := engine.PutComputerCheckpoint(ctx, cp.storageScope(), data)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(object)
	if _, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='ready',object=$2 WHERE id=$1`, cp.ID, raw); err != nil {
		t.Fatal(err)
	}
	engine.failDelete = true
	if err = h.PurgeWrenComputerCheckpoints(ctx, []string{c.WorkspaceID}, ""); err == nil {
		t.Fatal("storage deletion failure hidden")
	}
	if _, err = h.computerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID, false); err != nil {
		t.Fatal("failed erasure lost its ownership handle")
	}
	engine.failDelete = false
	if err = h.PurgeWrenComputerCheckpoints(ctx, []string{c.WorkspaceID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = h.computerCheckpoint(ctx, c.WorkspaceID, c.AccountID, cp.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("checkpoint metadata survived verified erase: %v", err)
	}
	if len(engine.objects) != 0 {
		t.Fatal("encrypted storage object orphaned")
	}
}

func TestWrenComputerStatusReportsSavedWorkspaceWithoutProvisioning(t *testing.T) {
	h, c, cp, engine := checkpointControlFixture(t)
	ctx := context.Background()
	s := h.service.Store
	if _, err := s.pool.Exec(ctx, `UPDATE wren_computer_checkpoints SET state='ready' WHERE id=$1`, cp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wren_computers SET phase='deleted',ssh_key_ciphertext=''::bytea WHERE id=$1`, cp.ComputerID); err != nil {
		t.Fatal(err)
	}
	var run OperatorRun
	if err := s.pool.QueryRow(ctx, `SELECT id,conversation_id,lease_token FROM operator_runs WHERE conversation_id=$1 AND status='running' LIMIT 1`, c.ID).Scan(&run.ID, &run.ConversationID, &run.Lease); err != nil {
		t.Fatal(err)
	}
	operator := &OperatorRuntime{Server: h}
	result, handled, err := operator.wrenComputerTool(ctx, run, c, "canter_computer_status", json.RawMessage(`{}`))
	if err != nil || !handled {
		t.Fatalf("saved status: %v", err)
	}
	data := result.(map[string]any)
	if data["phase"] != "saved_workspace" || data["computeAllocated"] != false {
		t.Fatalf("misleading saved status: %#v", result)
	}
	if engine.plans != 0 || engine.applied != 0 {
		t.Fatal("read-only status provisioned compute")
	}
	foreign := c
	foreign.AccountID = "foreign-account"
	if _, err = h.LatestSavedWrenComputerCheckpoint(ctx, foreign); err == nil {
		t.Fatal("private checkpoint exposed to foreign user")
	}
}
