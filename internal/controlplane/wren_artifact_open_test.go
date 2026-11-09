package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func artifactToolRuntime(s *Store) *OperatorRuntime {
	return &OperatorRuntime{Server: NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)}
}
func openArtifactTool(t *testing.T, o *OperatorRuntime, c Conversation, run OperatorRun, id string) WrenArtifact {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"id": id})
	result, handled, err := o.wrenComputerTool(context.Background(), run, c, "canter_open_artifact", raw)
	if err != nil || !handled {
		t.Fatalf("open existing: %v %v", handled, err)
	}
	a, ok := result.(WrenArtifact)
	if !ok {
		t.Fatalf("unexpected open result %T", result)
	}
	return a
}
func artifactCounts(t *testing.T, s *Store, conversation string) (int, int) {
	t.Helper()
	var artifacts, revisions int
	err := s.pool.QueryRow(context.Background(), `SELECT count(DISTINCT a.id),count(v.artifact_id) FROM wren_artifacts a JOIN wren_artifact_revisions v ON v.artifact_id=a.id WHERE a.conversation_id=$1`, conversation).Scan(&artifacts, &revisions)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts, revisions
}

func TestWrenOpenArtifactReusesLatestSavedRevisionWithoutBytesOrCopies(t *testing.T) {
	s, c, run := wrenJobFixture(t)
	ctx := context.Background()
	o := artifactToolRuntime(s)
	a := libraryArtifact(t, s, c, "launch.md", "text/markdown", "# Monday\nOriginal draft.")
	content := []byte("# Tuesday\nHuman correction to preserve.")
	latest, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ID: a.ID, ConversationID: c.ID, ExpectedRevision: a.Revision, Name: a.Name, MediaType: a.MediaType, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		opened := openArtifactTool(t, o, c, run, a.ID)
		digest := sha256.Sum256(content)
		if opened.ID != a.ID || opened.Revision != latest.Revision || opened.LatestRevision != latest.Revision || opened.SHA256 != hex.EncodeToString(digest[:]) || len(opened.Content) != 0 {
			t.Fatalf("not authoritative current metadata %+v", opened)
		}
	}
	count, revisions := artifactCounts(t, s, c.ID)
	if count != 1 || revisions != 2 {
		t.Fatalf("open copied or edited artifact: %d/%d", count, revisions)
	}
	var event []byte
	if err = s.pool.QueryRow(ctx, `SELECT data FROM operator_events WHERE conversation_id=$1 AND kind='surface' ORDER BY sequence DESC LIMIT 1`, c.ID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	var surface OperatorSurface
	if err = json.Unmarshal(event, &surface); err != nil || surface.Kind != "wren-artifact" || surface.ID != a.ID {
		t.Fatalf("surface %s %v", event, err)
	}
	policy, ok := operatorPolicy("canter_open_artifact")
	if !ok || policy.Effect != "view" || !policy.RetrySafe || policy.RequireDraft {
		t.Fatalf("open is not retry-safe read/view %+v %v", policy, ok)
	}
	found := false
	for _, tool := range o.tools() {
		if tool.Name == "canter_open_artifact" {
			found = true
		}
	}
	if !found {
		t.Fatal("open tool missing from model catalog")
	}
}

func TestWrenPublishArtifactRejectsEmptyInlineButHumanBlankDraftRemainsValid(t *testing.T) {
	s, c, run := wrenJobFixture(t)
	ctx := context.Background()
	o := artifactToolRuntime(s)
	for _, content := range []string{"", " \n\t", "\u2003"} {
		raw, _ := json.Marshal(map[string]string{"name": "placeholder.md", "mediaType": "text/markdown", "content": content})
		_, handled, err := o.wrenComputerTool(ctx, run, c, "canter_publish_artifact", raw)
		if !handled || err == nil || !strings.Contains(err.Error(), "canter_open_artifact") {
			t.Fatalf("empty publication accepted %q %v", content, err)
		}
	}
	if count, _ := artifactCounts(t, s, c.ID); count != 0 {
		t.Fatal("empty tool call created an artifact")
	}
	blank, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ID: "artifact_human_blank", ConversationID: c.ID, Name: "blank.txt", MediaType: "text/plain", Content: []byte{}})
	if err != nil || blank.Size != 0 {
		t.Fatalf("human blank draft rejected %+v %v", blank, err)
	}
}

func TestWrenOpenArtifactRejectsOtherOriginAndRevokedRoomAudience(t *testing.T) {
	s, private, _, c, member := knowledgeFixture(t)
	ctx := context.Background()
	o := artifactToolRuntime(s)
	if _, err := s.EnqueueOperator(ctx, c, "open-artifact-room", "Review this room's file.", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	a := libraryArtifact(t, s, c, "room.txt", "text/plain", "Authorized room result.")
	other, err := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_other_artifact_origin", Kind: "group", Name: "Other room"}, true)
	if err != nil {
		t.Fatal(err)
	}
	foreignC, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, other.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := libraryArtifact(t, s, foreignC, "other.txt", "text/plain", "Other room file.")
	for _, id := range []string{foreign.ID, "artifact_missing_origin"} {
		raw, _ := json.Marshal(map[string]string{"id": id})
		if _, handled, err := o.wrenComputerTool(ctx, run, c, "canter_open_artifact", raw); !handled || !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-origin open %s %v %v", id, handled, err)
		}
	}
	wrongWorkspace := c
	wrongWorkspace.WorkspaceID = "foreign_workspace"
	raw, _ := json.Marshal(map[string]string{"id": a.ID})
	if _, _, err = o.wrenComputerTool(ctx, run, wrongWorkspace, "canter_open_artifact", raw); err == nil {
		t.Fatal("cross-workspace artifact opened")
	}
	memberC := c
	memberC.AccountID = member
	if _, err = s.pool.Exec(ctx, `DELETE FROM wren_room_members WHERE room_id=(SELECT id FROM wren_rooms WHERE conversation_id=$1) AND account_id=$2`, c.ID, member); err != nil {
		t.Fatal(err)
	}
	if _, _, err = o.wrenComputerTool(ctx, run, memberC, "canter_open_artifact", raw); err == nil {
		t.Fatal("revoked room member opened artifact")
	}
	privateArtifact := libraryArtifact(t, s, private, "private.txt", "text/plain", "Private file.")
	raw, _ = json.Marshal(map[string]string{"id": privateArtifact.ID})
	if _, _, err = o.wrenComputerTool(ctx, run, c, "canter_open_artifact", raw); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private artifact crossed into room %v", err)
	}
}

func TestWrenWorkerReturnListsExistingArtifactAndLeadOpensCurrentHumanRevision(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	o := artifactToolRuntime(s)
	job, err := s.CreateWrenJob(ctx, parent, c, "publish-once", "Tuesday report", "Write and publish Tuesday report.", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finishOperator(ctx, parent, "completed", "The worker is preparing the report."); err != nil {
		t.Fatal(err)
	}
	child, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || child.ID != job.ChildRunID {
		t.Fatalf("worker %v %v %+v", ok, err, child)
	}
	childC, err := s.Conversation(ctx, c.WorkspaceID, c.AccountID, job.ChildConversationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"name": "tuesday.md", "mediaType": "text/markdown", "content": "# Tuesday\nWorker report bytes.", "operationKey": "worker-file"})
	result, handled, err := o.wrenComputerTool(ctx, child, childC, "canter_publish_artifact", raw)
	if err != nil || !handled {
		t.Fatalf("worker publish %v %v", handled, err)
	}
	a := result.(WrenArtifact)
	if a.ConversationID != c.ID || a.Provenance.JobID != job.ID {
		t.Fatalf("worker did not publish directly to origin %+v", a)
	}
	// A real later human revision must win over stale worker bytes and provenance.
	latest, err := s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ID: a.ID, ConversationID: c.ID, ExpectedRevision: a.Revision, Name: a.Name, MediaType: a.MediaType, Content: []byte("# Tuesday\nLater human correction bytes.")})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finishOperator(ctx, child, "completed", "Tuesday report was saved."); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	wake, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || wake.ConversationID != c.ID {
		t.Fatalf("return wake %v %v %+v", ok, err, wake)
	}
	text, err := s.WrenJobReturnContext(ctx, wake)
	if err != nil || !strings.Contains(text, a.ID) || !strings.Contains(text, latest.SHA256) || !strings.Contains(text, "canter_open_artifact") || strings.Contains(text, "Later human correction bytes.") {
		t.Fatalf("return metadata %q %v", text, err)
	}
	opened := openArtifactTool(t, o, c, wake, a.ID)
	if opened.Revision != latest.Revision || opened.SHA256 != latest.SHA256 {
		t.Fatalf("stale opened result %+v", opened)
	}
	count, revisions := artifactCounts(t, s, c.ID)
	if count != 1 || revisions != 2 {
		t.Fatalf("return duplicated saved artifact %d/%d", count, revisions)
	}
}
