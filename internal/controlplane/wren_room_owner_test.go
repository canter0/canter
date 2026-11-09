package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrenRoomOwnerTransferPreservesConversationAndEncryptedScope(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	rooms, err := s.Rooms(ctx, private.WorkspaceID, private.AccountID)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms %v %v", rooms, err)
	}
	room := rooms[0]
	if _, err = s.enqueueOperatorWithRecipients(ctx, shared, "owner-transfer-message", "Shared conversation survives.", "test", OperatorModelOptions{}, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	artifact := libraryArtifact(t, s, shared, "report.csv", "text/csv", "name\nTeam\n")
	h := NewHTTPServer(&Service{Store: s, Engine: &computerFakeEngine{}}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Computers: WrenComputerConfig{SSHSourceCIDR: "203.0.113.8/32", AllowedWorkspaces: []string{shared.WorkspaceID}, MaxLifetimeSeconds: 3600}}).(*HTTPServer)
	computer, err := h.EnableWrenComputer(ctx, shared.WorkspaceID, shared.AccountID, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	computer = readyComputer(t, h, computer)
	cp := WrenComputerCheckpoint{ID: func() string { id, _ := newID("checkpoint_"); return id }(), WorkspaceID: shared.WorkspaceID, ConversationID: shared.ID, OwnerID: shared.AccountID, ScopeKind: "room", ComputerID: computer.ID, State: "ready"}
	version, cipher, err := h.config.Secrets.seal([]byte("encrypted saved files"), cp.binding())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_computer_checkpoints(id,workspace_id,conversation_id,owner_id,scope_kind,computer_id,state,key_version,encryption_nonce) VALUES($1,$2,$3,$4,'room',$5,'ready',$6,$7)`, cp.ID, shared.WorkspaceID, shared.ID, shared.AccountID, computer.ID, version, cipher); err != nil {
		t.Fatal(err)
	}
	beforeScope, beforeBinding := cp.storageScope(), cp.binding()
	result, err := s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, shared.AccountID, room.ID, member)
	if err != nil || result.OwnerID != member {
		t.Fatalf("transfer %+v %v", result, err)
	}
	var conversationOwner, computerOwner, checkpointOwner string
	if err = s.pool.QueryRow(ctx, `SELECT c.account_id,computer.owner_id,checkpoint.owner_id FROM operator_conversations c JOIN wren_computers computer ON computer.conversation_id=c.id JOIN wren_computer_checkpoints checkpoint ON checkpoint.conversation_id=c.id WHERE c.id=$1 AND checkpoint.id=$2 AND computer.id=$3`, shared.ID, cp.ID, computer.ID).Scan(&conversationOwner, &computerOwner, &checkpointOwner); err != nil {
		t.Fatal(err)
	}
	if conversationOwner != member || computerOwner != member || checkpointOwner != member {
		t.Fatalf("partial ownership %s %s %s", conversationOwner, computerOwner, checkpointOwner)
	}
	cp.OwnerID = member
	if beforeScope != cp.storageScope() || !bytes.Equal(beforeBinding, cp.binding()) {
		t.Fatal("encrypted scope changed")
	}
	plaintext, err := h.config.Secrets.open(version, cipher, cp.binding())
	if err != nil || string(plaintext) != "encrypted saved files" {
		t.Fatalf("saved file encryption lost %v", err)
	}
	var message string
	if err = s.pool.QueryRow(ctx, `SELECT content FROM operator_messages WHERE conversation_id=$1`, shared.ID).Scan(&message); err != nil || message != "Shared conversation survives." {
		t.Fatalf("message lost %q %v", message, err)
	}
	if _, err = s.GetWrenArtifact(ctx, shared.WorkspaceID, member, artifact.ID, 0); err != nil {
		t.Fatalf("new owner lost artifact %v", err)
	}
	if _, _, _, err = s.computerScope(ctx, shared.WorkspaceID, member, shared.ID, true); err != nil {
		t.Fatalf("new owner cannot manage computer %v", err)
	}
	if _, _, _, err = s.computerScope(ctx, shared.WorkspaceID, private.AccountID, shared.ID, true); err == nil {
		t.Fatal("former owner still manages computer")
	}
	result.Name = "Managed by new owner"
	if _, err = s.SaveRoom(ctx, shared.WorkspaceID, member, result, false); err != nil {
		t.Fatalf("new owner cannot save %v", err)
	}
	if _, err = s.SaveRoom(ctx, shared.WorkspaceID, private.AccountID, result, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("former owner settings %v", err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, private.AccountID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("workspace owner overrode current room owner %v", err)
	}
}

func TestWrenRoomOwnerTransferRejectsIneligibleTargetsAndCallers(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	rooms, _ := s.Rooms(ctx, shared.WorkspaceID, shared.AccountID)
	room := rooms[0]
	outside := multiplayerPerson(t, s, "room-transfer-outside@example.com")
	cases := []struct {
		name, actor, target, workspace string
		want                           error
	}{
		{"nonowner", member, private.AccountID, shared.WorkspaceID, ErrForbidden},
		{"crossworkspace", private.AccountID, member, "foreign_workspace", ErrNotFound},
		{"nonmember", private.AccountID, outside.ID, shared.WorkspaceID, ErrNotFound},
	}
	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if _, err := s.TransferWrenRoomOwnership(ctx, v.workspace, v.actor, room.ID, v.target); !errors.Is(err, v.want) {
				t.Fatalf("%v want %v", err, v.want)
			}
		})
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(workspace_id,account_id,role) VALUES($1,$2,'operator')`, shared.WorkspaceID, outside.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, outside.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-room-member gained audience %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, shared.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer became owner %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memberships SET role='operator' WHERE workspace_id=$1 AND account_id=$2;`, shared.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, member); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled owner %v", err)
	}
}

func TestWrenRoomOwnerTransferRejectsPendingCheckpoints(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	rooms, _ := s.Rooms(ctx, shared.WorkspaceID, shared.AccountID)
	room := rooms[0]
	checkpointID, _ := newID("checkpoint_")
	for _, state := range []string{"creating", "restoring", "deleting"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO wren_computer_checkpoints(id,workspace_id,conversation_id,owner_id,scope_kind,computer_id,state) VALUES($5,$1,$2,$3,'room','computer_original',$4) ON CONFLICT(id) DO UPDATE SET state=excluded.state`, shared.WorkspaceID, shared.ID, private.AccountID, state, checkpointID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s checkpoint allowed transfer %v", state, err)
		}
	}
}

func TestWrenRoomOwnerTransferFencesActiveComputerWork(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	rooms, _ := s.Rooms(ctx, shared.WorkspaceID, shared.AccountID)
	room := rooms[0]
	h := NewHTTPServer(&Service{Store: s, Engine: &computerFakeEngine{}}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Computers: WrenComputerConfig{SSHSourceCIDR: "203.0.113.8/32", AllowedWorkspaces: []string{shared.WorkspaceID}, MaxLifetimeSeconds: 3600}}).(*HTTPServer)
	computer, err := h.EnableWrenComputer(ctx, shared.WorkspaceID, shared.AccountID, shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); !errors.Is(err, ErrConflict) {
		t.Fatalf("allocation allowed transfer %v", err)
	}
	computer = readyComputer(t, h, computer)
	if _, err = s.EnqueueOperator(ctx, shared, "room-active-mutation", "Work on the room project.", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("run claim %v %v", ok, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_computers SET mutation_run_id=$2,mutation_lease_token=$3,mutation_expires_at=now()+interval '60 seconds' WHERE id=$1`, computer.ID, run.ID, run.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); !errors.Is(err, ErrConflict) {
		t.Fatalf("active mutation allowed transfer %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_computers SET mutation_expires_at=now()-interval '1 second' WHERE id=$1`, computer.ID); err != nil {
		t.Fatal(err)
	}
	executionID, _ := newID("job_")
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_harness_executions(job_id,run_id,computer_id,status,process_id) VALUES($1,$2,$3,'stop_requested','guest_process_still_running')`, executionID, run.ID, computer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); !errors.Is(err, ErrConflict) {
		t.Fatalf("unconfirmed process stop allowed transfer %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE wren_harness_executions SET status='stopped',process_stopped=true WHERE job_id=$1`, executionID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); err != nil {
		t.Fatalf("confirmed stopped work blocked transfer %v", err)
	}
}

func TestWrenRoomOwnerRecoveryEligibilityIsReturnedOnlyToExistingAudience(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	rooms, _ := s.Rooms(ctx, shared.WorkspaceID, shared.AccountID)
	room := rooms[0]
	if _, err := s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, member); err != nil {
		t.Fatal(err)
	}
	current, err := s.Room(ctx, shared.WorkspaceID, private.AccountID, room.ID)
	if err != nil || current.CanRecoverOwnership {
		t.Fatalf("valid owner recoverable %+v %v", current, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, shared.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	current, err = s.Room(ctx, shared.WorkspaceID, private.AccountID, room.ID)
	if err != nil || !current.CanRecoverOwnership {
		t.Fatalf("orphan recovery not surfaced %+v %v", current, err)
	}
	if _, err = s.TransferWrenRoomOwnership(ctx, shared.WorkspaceID, private.AccountID, room.ID, private.AccountID); err != nil {
		t.Fatalf("authorized recovery %v", err)
	}
}

func TestWrenRoomOwnerHTTPRequiresReviewingOwnersAuthority(t *testing.T) {
	s, private, _, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	rooms, _ := s.Rooms(ctx, shared.WorkspaceID, shared.AccountID)
	room := rooms[0]
	_, _, ownerToken, err := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	_, _, memberToken, err := s.Signin(ctx, "knowledge-member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"})
	request := func(token, target, path string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"accountId": target})
		r := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/workspaces/"+private.WorkspaceID+path, bytes.NewReader(body))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	route := "/rooms/" + room.ID + "/owner"
	if result := request(memberToken, private.AccountID, route); result.Code != http.StatusForbidden {
		t.Fatalf("member transfer status %d: %s", result.Code, result.Body.String())
	}
	result := request(ownerToken, member, route)
	if result.Code != http.StatusOK {
		t.Fatalf("owner transfer status %d: %s", result.Code, result.Body.String())
	}
	var transferred WrenRoom
	if err = json.Unmarshal(result.Body.Bytes(), &transferred); err != nil || transferred.OwnerID != member {
		t.Fatalf("owner response %+v %v", transferred, err)
	}
	if result = request(ownerToken, private.AccountID, "/wren-members/rooms/"+room.ID+"/transfer"); result.Code != http.StatusForbidden {
		t.Fatalf("legacy bypass status %d", result.Code)
	}
}
