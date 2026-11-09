package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func knowledgeFixture(t *testing.T) (*Store, Conversation, Wren, Conversation, string) {
	t.Helper()
	s, private, w := responsibilityFixture(t)
	ctx := context.Background()
	member, _, _, err := s.Signup(ctx, "knowledge-member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator')`, member.ID, private.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, private.WorkspaceID, private.AccountID, WrenRoom{ID: "room_knowledge_test", Kind: "group", Name: "Shared research", WrenIDs: []string{w.ID}, MemberIDs: []string{member.ID}}, true)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := s.Conversation(ctx, private.WorkspaceID, private.AccountID, room.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	return s, private, w, shared, member.ID
}
func memoryInput(c Conversation, w Wren) WrenMemoryInput {
	return WrenMemoryInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "private", WrenID: w.ID}, Title: "Writing preference", Content: "Use concise plain English."}
}
func TestWrenMemoryScopesCorrectionsPromotionForget(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	m, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, memoryInput(private, w))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenMemory(ctx, private.WorkspaceID, member, m.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private memory leaked %v", err)
	}
	if _, err = s.WrenMemory(ctx, "other-workspace", private.AccountID, m.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross workspace %v", err)
	}
	roomScope := WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}
	if items, err := s.WrenMemories(ctx, private.WorkspaceID, member, roomScope, ""); err != nil || len(items) != 0 {
		t.Fatalf("private imported to room %+v %v", items, err)
	}
	promoted, err := s.PromoteWrenMemory(ctx, private.WorkspaceID, private.AccountID, m.ID, shared.ID, m.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.PromotedFromID != m.ID || promoted.Scope != "room" || promoted.OwnerID != "" || promoted.Content != m.Content {
		t.Fatalf("bad explicit copy %+v", promoted)
	}
	if got, err := s.WrenMemory(ctx, private.WorkspaceID, member, promoted.ID, 0); err != nil || got.Content != m.Content {
		t.Fatalf("shared copy inaccessible %+v %v", got, err)
	}
	edit := memoryInput(private, w)
	edit.ID = m.ID
	edit.ExpectedRevision = m.Revision
	edit.Content = "Corrected: detailed answers."
	m, err = s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, edit); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale correction accepted %v", err)
	}
	got, err := s.WrenMemory(ctx, private.WorkspaceID, member, promoted.ID, 0)
	if err != nil || got.Content == m.Content {
		t.Fatalf("private edit silently changed room %+v %v", got, err)
	}
	if _, err = s.PromoteWrenMemory(ctx, private.WorkspaceID, private.AccountID, m.ID, shared.ID, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale promotion %v", err)
	}
	old, err := s.WrenMemory(ctx, private.WorkspaceID, private.AccountID, m.ID, 1)
	if err != nil || !strings.Contains(old.Content, "concise") {
		t.Fatalf("immutable old revision %+v %v", old, err)
	}
	forgotten, err := s.ForgetWrenMemory(ctx, private.WorkspaceID, private.AccountID, m.ID, m.Revision)
	if err != nil || !forgotten.Deleted || forgotten.Content != "" {
		t.Fatalf("forget %+v %v", forgotten, err)
	}
	history, err := s.WrenMemoryRevisions(ctx, private.WorkspaceID, private.AccountID, m.ID)
	if err != nil || len(history) != 3 {
		t.Fatalf("history %+v %v", history, err)
	}
	for _, v := range history {
		if v.Content != "" || v.Title != "Forgotten memory" {
			t.Fatalf("forgotten history returned plaintext %+v", v)
		}
	}
	if items, err := s.WrenMemories(ctx, private.WorkspaceID, private.AccountID, edit.WrenKnowledgeScope, ""); err != nil || len(items) != 0 {
		t.Fatalf("forgotten retrieval %+v %v", items, err)
	}
	if _, err = s.WrenMemory(ctx, private.WorkspaceID, member, promoted.ID, 0); err != nil {
		t.Fatalf("forget silently erased explicit shared copy %v", err)
	}
}
func TestWrenMemorySourcesConcurrencyAndContext(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	root, err := s.EnqueueOperator(ctx, private, "memory-human-root", "Remember my writing preference", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.PutWrenArtifact(ctx, private.WorkspaceID, private.AccountID, WrenArtifactInput{Name: "notes.txt", ConversationID: private.ID, MediaType: "text/plain", Content: []byte("source")})
	if err != nil {
		t.Fatal(err)
	}
	input := memoryInput(private, w)
	input.Sources = WrenMemorySources{RunID: root.ID, ArtifactID: artifact.ID}
	m, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || m.Sources.ArtifactRevision != 1 {
		t.Fatalf("source version %+v %v", m, err)
	}
	text, err := s.MemoryContext(ctx, private, root)
	if err != nil || !strings.Contains(text, m.Content) {
		t.Fatalf("private context %s %v", text, err)
	}
	roomInput := WrenMemoryInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}, Title: "Forged source", Content: "No", Sources: input.Sources}
	if _, err = s.SaveWrenMemory(ctx, private.WorkspaceID, member, roomInput); !errors.Is(err, ErrNotFound) {
		t.Fatalf("forged private provenance allowed %v", err)
	}
	roomInput.Sources = WrenMemorySources{ArtifactID: "artifact_missing"}
	if _, err = s.SaveWrenMemory(ctx, private.WorkspaceID, member, roomInput); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invented artifact source allowed %v", err)
	}
	promoted, err := s.PromoteWrenMemory(ctx, private.WorkspaceID, private.AccountID, m.ID, shared.ID, m.Revision)
	if err != nil || promoted.Sources.RunID != "" || promoted.Sources.ArtifactID != "" {
		t.Fatalf("promotion leaked private refs %+v %v", promoted, err)
	}
	input.ID = m.ID
	input.ExpectedRevision = m.Revision
	result := make(chan error, 2)
	var group sync.WaitGroup
	for _, content := range []string{"Revision A", "Revision B"} {
		group.Add(1)
		go func(content string) {
			defer group.Done()
			edit := input
			edit.Content = content
			_, e := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, edit)
			result <- e
		}(content)
	}
	group.Wait()
	close(result)
	success, conflict := 0, 0
	for err := range result {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent overwrite %d %d", success, conflict)
	}
	roomRun, err := s.EnqueueOperator(ctx, shared, "memory-room-run", "Use shared facts", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The room worker selection is server-owned, not a client memory scope input.
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET speaker=$2 WHERE id=$1`, roomRun.ID, w); err != nil {
		t.Fatal(err)
	}
	text, err = s.MemoryContext(ctx, shared, roomRun)
	if err != nil || !strings.Contains(text, promoted.ID) || strings.Contains(text, "Revision A") || strings.Contains(text, "Revision B") {
		t.Fatalf("room read private context %s %v", text, err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, private.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MemoryContext(ctx, private, root); err == nil {
		t.Fatal("membership loss did not fence context")
	}
}
func TestWrenMemoryChildReceivesNoImplicitPrivateContext(t *testing.T) {
	s, private, w, _, _ := knowledgeFixture(t)
	ctx := context.Background()
	m, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, memoryInput(private, w))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueOperator(ctx, private, "memory-child-root", "Do a bounded task", "test", nil); err != nil {
		t.Fatal(err)
	}
	parent, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	job, err := s.CreateWrenJob(ctx, parent, private, "knowledge-child", "Bounded task", "Only this accepted brief", "test")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Conversation(ctx, private.WorkspaceID, private.AccountID, job.ChildConversationID)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.MemoryContext(ctx, child, OperatorRun{ID: job.ChildRunID})
	if err != nil || text != "" || strings.Contains(text, m.Content) {
		t.Fatalf("child inherited private facts %s %v", text, err)
	}
}

func TestWrenMemoryAndSkillsHTTPContract(t *testing.T) {
	s, private, w, shared, _ := knowledgeFixture(t)
	ctx := context.Background()
	_, _, token, err := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	request := func(method, path, session string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+private.WorkspaceID+path, bytes.NewReader(data))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
		}
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	got := request("POST", "/wren-memory", token, memoryInput(private, w))
	if got.Code != 200 {
		t.Fatalf("memory create %d %s", got.Code, got.Body)
	}
	var memory WrenMemory
	if err = json.Unmarshal(got.Body.Bytes(), &memory); err != nil {
		t.Fatal(err)
	}
	got = request("GET", "/wren-memory?conversationId="+private.ID+"&scope=private&wrenId="+w.ID, token, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), memory.ID) {
		t.Fatalf("memory list %d %s", got.Code, got.Body)
	}
	got = request("POST", "/wren-memory/"+memory.ID+"/promote", token, map[string]any{"expectedRevision": memory.Revision, "conversationId": shared.ID})
	if got.Code != 200 {
		t.Fatalf("promote %d %s", got.Code, got.Body)
	}
	got = request("POST", "/wren-memory/"+memory.ID+"/forget", token, map[string]any{"expectedRevision": memory.Revision})
	if got.Code != 200 {
		t.Fatalf("forget %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-memory/"+memory.ID+"/revisions", token, nil)
	if got.Code != 200 || strings.Contains(got.Body.String(), "concise plain") {
		t.Fatalf("forgotten history leak %d %s", got.Code, got.Body)
	}
	got = request("POST", "/wren-skills", token, skillInput())
	if got.Code != 200 {
		t.Fatalf("skill publish %d %s", got.Code, got.Body)
	}
	var skill WrenSkill
	if err = json.Unmarshal(got.Body.Bytes(), &skill); err != nil {
		t.Fatal(err)
	}
	install := WrenSkillInstallationInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}, SkillID: skill.ID, Version: 1, Enabled: true}
	got = request("POST", "/wren-skills/installations", token, install)
	if got.Code != 200 {
		t.Fatalf("install %d %s", got.Code, got.Body)
	}
	var installed WrenSkillInstallation
	if err = json.Unmarshal(got.Body.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	got = request("GET", "/wren-skills/installations?conversationId="+shared.ID+"&scope=room", token, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), skill.ID) {
		t.Fatalf("list installations %d %s", got.Code, got.Body)
	}
	install.ExpectedRevision = installed.Revision
	got = request("DELETE", "/wren-skills/installations", token, install)
	if got.Code != 200 {
		t.Fatalf("uninstall %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-skills/"+skill.ID+"/availability?version=1&conversationId="+private.ID, token, nil)
	if got.Code != 200 {
		t.Fatalf("actual capability diagnostics %d %s", got.Code, got.Body)
	}
	var diagnostics struct {
		Capabilities []WrenSkillCapability `json:"capabilities"`
	}
	if err = json.Unmarshal(got.Body.Bytes(), &diagnostics); err != nil {
		t.Fatal(err)
	}
	for _, capability := range diagnostics.Capabilities {
		if capability.Name == "canter_computer_read" && capability.Available {
			t.Fatal("unconfigured computer falsely available")
		}
	}
	got = request("GET", "/wren-skills/"+skill.ID+"/availability?version=1&conversationId=conv_missing", token, nil)
	if got.Code != 404 {
		t.Fatalf("foreign dependency diagnostics %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-skills/"+skill.ID+"/versions", token, nil)
	if got.Code != 200 {
		t.Fatalf("versions %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-memory/"+memory.ID, "", nil)
	if got.Code != 401 {
		t.Fatalf("unauthenticated knowledge %d", got.Code)
	}
}

func TestWrenKnowledgeStableCreateAndPromotionRetry(t *testing.T) {
	s, private, w, shared, _ := knowledgeFixture(t)
	ctx := context.Background()
	input := memoryInput(private, w)
	input.ID = "memory_stable_request"
	m, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || same.ID != m.ID || same.Revision != 1 {
		t.Fatalf("memory create replay %+v %v", same, err)
	}
	changed := input
	changed.Content = "Changed retry must not overwrite"
	if _, err = s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("different create replay %v", err)
	}
	type outcome struct {
		v   WrenMemory
		err error
	}
	results := make(chan outcome, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			v, e := s.PromoteWrenMemory(ctx, private.WorkspaceID, private.AccountID, m.ID, shared.ID, m.Revision)
			results <- outcome{v, e}
		}()
	}
	group.Wait()
	close(results)
	id := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if id == "" {
			id = result.v.ID
		} else if id != result.v.ID {
			t.Fatalf("promotion retry duplicated %s %s", id, result.v.ID)
		}
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_memory WHERE conversation_id=$1`, shared.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate copy %d %v", count, err)
	}
	skillInput := skillInput()
	skillInput.ID = "skill_stable_request"
	skill, err := s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, skillInput)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, skillInput)
	if err != nil || again.ID != skill.ID || again.Version != 1 {
		t.Fatalf("skill create replay %+v %v", again, err)
	}
	skillInput.Instructions = "Changed retry must not overwrite"
	if _, err = s.SaveWrenSkill(ctx, private.WorkspaceID, private.AccountID, skillInput); !errors.Is(err, ErrConflict) {
		t.Fatalf("different skill replay %v", err)
	}
}

func TestWrenMemoryCopyKeepsOriginAcrossIndependentEdits(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	original, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, memoryInput(private, w))
	if err != nil {
		t.Fatal(err)
	}
	copy, err := s.PromoteWrenMemory(ctx, private.WorkspaceID, private.AccountID, original.ID, shared.ID, original.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// The copy must remain editable by a room member even after its private
	// origin is forgotten, without granting that member source access.
	if _, err = s.ForgetWrenMemory(ctx, private.WorkspaceID, private.AccountID, original.ID, original.Revision); err != nil {
		t.Fatal(err)
	}
	edit := WrenMemoryInput{ID: copy.ID, WrenKnowledgeScope: copy.WrenKnowledgeScope, ExpectedRevision: copy.Revision, Title: copy.Title, Content: "The room independently prefers a research table."}
	updated, err := s.SaveWrenMemory(ctx, private.WorkspaceID, member, edit)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PromotedFromID != original.ID || updated.PromotedFromRevision != 1 || updated.AuthorID != member || updated.Content != edit.Content {
		t.Fatalf("copy lost origin or editor: %+v", updated)
	}
	if _, err = s.WrenMemory(ctx, private.WorkspaceID, member, original.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("copy granted source access: %v", err)
	}
	// Simulate a revision saved by the older implementation, which dropped
	// origin metadata. The next edit recovers it from immutable revision one.
	if _, err = s.pool.Exec(ctx, `UPDATE wren_memory_revisions SET promoted_from_id='',promoted_from_revision=0 WHERE memory_id=$1 AND revision=2`, copy.ID); err != nil {
		t.Fatal(err)
	}
	edit.ExpectedRevision = 2
	edit.Content = "The room now adds a source-verification column."
	updated, err = s.SaveWrenMemory(ctx, private.WorkspaceID, member, edit)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 3 || updated.PromotedFromID != original.ID || updated.PromotedFromRevision != 1 {
		t.Fatalf("legacy copy origin not restored: %+v", updated)
	}
	originalNow, err := s.WrenMemory(ctx, private.WorkspaceID, private.AccountID, original.ID, 0)
	if err != nil || !originalNow.Deleted || originalNow.Content != "" {
		t.Fatalf("room edit changed forgotten original: %+v %v", originalNow, err)
	}
}
