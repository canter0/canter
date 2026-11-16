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

func publishWren(t *testing.T, s *Store, c Conversation, w Wren, managers ...string) WrenPublication {
	t.Helper()
	p, e := s.SaveWrenPublication(context.Background(), c.WorkspaceID, c.AccountID, w.ID, WrenPublicationInput{Published: true, ManagerIDs: managers, ReviewedProfile: func() *WrenProfile { p := profileFromWren(w); return &p }()})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestWrenSharingCanonicalPersonaHasSeparatePrivateChats(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.EnqueueOperator(ctx, private, "publisher-private-message", "PUBLISHER PRIVATE HISTORY", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveWrenMemory(ctx, private.WorkspaceID, private.AccountID, memoryInput(private, w)); err != nil {
		t.Fatal(err)
	}
	libraryArtifact(t, s, private, "publisher-secret.csv", "text/csv", "private\nsecret\n")
	publication := publishWren(t, s, private, w, member)
	var before, after int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_shared_chats`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	catalog, err := s.WrenPublications(ctx, private.WorkspaceID, member)
	if err != nil || len(catalog) != 1 || catalog[0].ConversationID != "" || !catalog[0].CanManage {
		t.Fatalf("catalog %+v %v", catalog, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_shared_chats`).Scan(&after); err != nil || before != after {
		t.Fatalf("GET created private chat %d %d %v", before, after, err)
	}
	chat, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil || chat.ID != w.ID || chat.ConversationID == private.ID || chat.Name != w.Name {
		t.Fatalf("canonical identity mapping %+v %v", chat, err)
	}
	again, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil || again.ConversationID != chat.ConversationID {
		t.Fatalf("open replay %+v %v", again, err)
	}
	for _, table := range []string{"operator_messages", "wren_memory", "wren_artifacts", "wren_skill_installations"} {
		var n int
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE conversation_id=$1`, chat.ConversationID).Scan(&n)
		if err != nil || n != 0 {
			t.Fatalf("private state copied %s %d %v", table, n, err)
		}
	}
	if _, err = s.Conversation(ctx, private.WorkspaceID, private.AccountID, chat.ConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publisher read subscriber's private chat %v", err)
	}
	if _, err = s.Conversation(ctx, private.WorkspaceID, member, private.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manager read publisher private chat %v", err)
	}
	if publication.Profile != profileFromWren(w) {
		t.Fatalf("profile changed %+v", publication.Profile)
	}
}
func TestWrenSharingRevocationRetainsOwnedHistoryAndDeniesNewWork(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	publication := publishWren(t, s, private, w)
	chat, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Conversation(ctx, private.WorkspaceID, member, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueOperator(ctx, c, "subscriber-work", "My private subscriber request", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact := libraryArtifact(t, s, c, "subscriber.csv", "text/csv", "name\nSubscriber\n")
	revoked, err := s.SaveWrenPublication(ctx, private.WorkspaceID, private.AccountID, w.ID, WrenPublicationInput{Published: false, ExpectedRevision: publication.Revision})
	if err != nil || revoked.Published {
		t.Fatalf("revoke %+v %v", revoked, err)
	}
	if err = s.ValidateWrenSharingConversation(ctx, c.ID, w.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked inference allowed %v", err)
	}
	var status string
	if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, run.ID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("revoked active run %s %v", status, err)
	}
	if _, err = s.Conversation(ctx, c.WorkspaceID, c.AccountID, c.ID); err != nil {
		t.Fatalf("subscriber lost own retained transcript %v", err)
	}
	if _, err = s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, artifact.ID, 0); err != nil {
		t.Fatalf("subscriber lost own artifact %v", err)
	}
	w.Purpose = "NEW PUBLISHER PRIVATE CONFIGURATION"
	if _, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, w, false); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveWrenForConversation(ctx, c)
	if err != nil || resolved.Purpose == w.Purpose {
		t.Fatalf("revocation leaked new private configuration %+v %v", resolved, err)
	}
	if _, err = s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked open allowed %v", err)
	}
}
func TestWrenSharingTemplatesAndCopiesNeverImportStateOrAuthority(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	profile := profileFromWren(w)
	if _, err := s.CopyWrenConfiguration(ctx, private.WorkspaceID, member, WrenCopyInput{ID: "wren_forbidden_copy", SourceWrenID: w.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private profile copied without grant %v", err)
	}
	copy, err := s.CopyWrenConfiguration(ctx, private.WorkspaceID, private.AccountID, WrenCopyInput{ID: "wren_clean_copy", SourceWrenID: w.ID, ReviewedProfile: &profile, Name: "My own Scout"})
	if err != nil || copy.ID == w.ID || copy.ConversationID == private.ID || copy.Name != "My own Scout" {
		t.Fatalf("clean copy %+v %v", copy, err)
	}
	template, err := s.SaveWrenTemplate(ctx, private.WorkspaceID, private.AccountID, WrenTemplateInput{ID: "template_clean_research", Profile: profile, Published: true})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := s.CopyWrenConfiguration(ctx, private.WorkspaceID, member, WrenCopyInput{ID: "wren_from_template", TemplateID: template.ID, ExpectedRevision: template.Revision})
	if err != nil || installed.ID == w.ID || installed.Name != w.Name {
		t.Fatalf("template install %+v %v", installed, err)
	}
	again, err := s.CopyWrenConfiguration(ctx, private.WorkspaceID, member, WrenCopyInput{ID: installed.ID, TemplateID: template.ID, ExpectedRevision: template.Revision})
	if err != nil || again.ID != installed.ID {
		t.Fatalf("copy replay %+v %v", again, err)
	}
	for _, table := range []string{"operator_messages", "wren_memory", "wren_artifacts", "wren_skill_installations"} {
		var n int
		err = s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE conversation_id=$1`, installed.ConversationID).Scan(&n)
		if err != nil || n != 0 {
			t.Fatalf("template imported state %s %d %v", table, n, err)
		}
	}
	template.Profile.Purpose = "Updated template"
	if _, err = s.SaveWrenTemplate(ctx, private.WorkspaceID, private.AccountID, WrenTemplateInput{ID: template.ID, Profile: template.Profile, Published: false, ExpectedRevision: template.Revision}); err != nil {
		t.Fatal(err)
	}
	current, err := s.Wren(ctx, private.WorkspaceID, member, installed.ID)
	if err != nil || current.Purpose == template.Profile.Purpose {
		t.Fatalf("independent copy changed remotely %+v %v", current, err)
	}
	if _, err = s.CopyWrenConfiguration(ctx, private.WorkspaceID, member, WrenCopyInput{ID: "wren_unpublished_template", TemplateID: template.ID, ExpectedRevision: 2}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpublished template copied %v", err)
	}
}
func TestWrenSharingConcurrentOpenCreatesOnePrivateChat(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	publishWren(t, s, private, w)
	ctx := context.Background()
	var wg sync.WaitGroup
	out := make(chan Wren, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
			out <- v
			errs <- e
		}()
	}
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for v := range out {
		if id != "" && id != v.ConversationID {
			t.Fatalf("duplicate private chats %s %s", id, v.ConversationID)
		}
		id = v.ConversationID
	}
}
func TestWrenSharingSubscriberRuntimeScopesAndLocalVersusGlobalArchive(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	publication := publishWren(t, s, private, w, member)
	chat, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Conversation(ctx, private.WorkspaceID, member, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := s.SaveWrenMemory(ctx, c.WorkspaceID, c.AccountID, WrenMemoryInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "private", WrenID: w.ID}, Title: "Subscriber preference", Content: "SUBSCRIBER PRIVATE MEMORY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.WrenMemory(ctx, c.WorkspaceID, private.AccountID, memory.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile manager read subscriber memory %v", err)
	}
	kind, name, owner, err := s.computerScope(ctx, c.WorkspaceID, c.AccountID, c.ID, true)
	if err != nil || kind != "private" || name != w.Name || owner != member {
		t.Fatalf("computer scope %s %s %s %v", kind, name, owner, err)
	}
	if _, _, _, err = s.computerScope(ctx, c.WorkspaceID, private.AccountID, c.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publisher got subscriber computer %v", err)
	}
	routine, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatalf("subscriber responsibility %v", err)
	}
	if _, err = s.EnqueueOperator(ctx, c, "sharing-runtime-request", "Delegate a scoped task", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || run.ConversationID != c.ID {
		t.Fatalf("subscriber run %+v %v %v", run, ok, err)
	}
	scope, exists, err := s.knowledgeRunScope(ctx, c, run)
	if err != nil || !exists || scope.ConversationID != c.ID || scope.WrenID != w.ID || scope.Scope != "private" {
		t.Fatalf("runtime memory scope %+v %v %v", scope, exists, err)
	}
	child := requireWrenJob(t, s, c, run, "shared-child-job")
	if _, err = s.WrenJob(ctx, c.WorkspaceID, private.AccountID, child.ID); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("publisher read subscriber delegated job %v", err)
	}
	if _, err = s.WrenResponsibility(ctx, c.WorkspaceID, private.AccountID, routine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publisher read subscriber responsibility %v", err)
	}
	if child.WrenID != w.ID || child.OriginConversationID != c.ID || child.RequesterAccountID != member {
		t.Fatalf("shared job identity %+v", child)
	}
	local := archiveLifecycle(t, s, private, 0, true)
	if local.Kind != "chat" {
		t.Fatalf("publisher private chat marked global %+v", local)
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, c.ID, w.ID); err != nil {
		t.Fatalf("publisher private archive disabled subscriber %v", err)
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, child.ChildConversationID, ""); err != nil {
		t.Fatalf("publisher private archive disabled subscriber child %v", err)
	}
	global, err := s.SaveWrenPublication(ctx, private.WorkspaceID, member, w.ID, WrenPublicationInput{Published: true, Archived: true, ManagerIDs: []string{member}, ExpectedRevision: publication.Revision})
	if err != nil || !global.Archived {
		t.Fatalf("explicit shared identity archive %+v %v", global, err)
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, c.ID, w.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("global archive didn't fence subscriber %v", err)
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, child.ChildConversationID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("global archive didn't fence subscriber child %v", err)
	}
	paused, err := s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, routine.ID)
	if err != nil || paused.State != "paused" || paused.NextRunAt != nil {
		t.Fatalf("shared archive schedule %+v %v", paused, err)
	}
	export, err := s.ExportWrenTranscript(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil || export.Lifecycle.Kind != "chat" || export.Metadata["wrenId"] != w.ID {
		t.Fatalf("subscriber private export %+v %v", export, err)
	}
}
func TestWrenSharingManagedProfileCASAndRevokedManagerCannotRepublishPrivateEdits(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	publication := publishWren(t, s, private, w, member)
	managed, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	managed.Purpose = "Shared managed instructions"
	saved, err := s.SaveWren(ctx, private.WorkspaceID, member, managed, false)
	if err != nil || saved.PublicationRevision != publication.Revision+1 {
		t.Fatalf("manager profile save %+v %v", saved, err)
	}
	if _, err = s.SaveWren(ctx, private.WorkspaceID, member, managed, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale managed profile overwrote %v", err)
	}
	publication, err = s.WrenPublication(ctx, private.WorkspaceID, private.AccountID, w.ID)
	if err != nil || publication.Profile.Purpose != saved.Purpose {
		t.Fatalf("canonical managed profile %+v %v", publication, err)
	}
	revoked, err := s.SaveWrenPublication(ctx, private.WorkspaceID, private.AccountID, w.ID, WrenPublicationInput{Published: false, ManagerIDs: []string{member}, ExpectedRevision: publication.Revision})
	if err != nil {
		t.Fatal(err)
	}
	original, err := s.Wren(ctx, private.WorkspaceID, private.AccountID, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	original.Purpose = "New private instructions after withdrawal"
	if _, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, original, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveWrenPublication(ctx, private.WorkspaceID, member, w.ID, WrenPublicationInput{Published: true, ExpectedRevision: revoked.Revision}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("withdrawn manager republished private edits %v", err)
	}
	subscriber, err := s.Wren(ctx, private.WorkspaceID, member, w.ID)
	if err != nil || subscriber.Purpose != saved.Purpose || subscriber.SharedAvailable || subscriber.CanManageProfile {
		t.Fatalf("withdrawn safe subscriber snapshot %+v %v", subscriber, err)
	}
}

func TestWrenSharingHTTPExplicitOpenAndReviewedPublication(t *testing.T) {
	s, private, w, _, member := knowledgeFixture(t)
	ctx := context.Background()
	_, _, ownerToken, err := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	_, _, memberToken, err := s.Signin(ctx, "knowledge-member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	request := func(method, path, token string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+private.WorkspaceID+path, bytes.NewReader(data))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	got := request("PATCH", "/wren-sharing/"+w.ID, ownerToken, map[string]any{"published": true, "expectedRevision": 0})
	if got.Code != 409 {
		t.Fatalf("unreviewed profile published %d %s", got.Code, got.Body)
	}
	got = request("PATCH", "/wren-sharing/"+w.ID, ownerToken, map[string]any{"published": true, "expectedRevision": 0, "reviewedProfile": profileFromWren(w)})
	if got.Code != 200 {
		t.Fatalf("publishHTTP %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-sharing", memberToken, nil)
	if got.Code != 200 || strings.Contains(got.Body.String(), private.ID) {
		t.Fatalf("catalog leaked publisher private DM %d %s", got.Code, got.Body)
	}
	var n int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_shared_chats WHERE account_id=$1`, member).Scan(&n); err != nil || n != 0 {
		t.Fatalf("GET opened chat %d %v", n, err)
	}
	got = request("POST", "/wren-sharing/"+w.ID+"/open", memberToken, nil)
	if got.Code != 200 {
		t.Fatalf("openHTTP %d %s", got.Code, got.Body)
	}
	var opened Wren
	if err = json.Unmarshal(got.Body.Bytes(), &opened); err != nil || opened.ConversationID == private.ID || !opened.Shared {
		t.Fatalf("openHTTP actualchat %+v %v", opened, err)
	}
	got = request("GET", "/wrens/"+w.ID, memberToken, nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), opened.ConversationID) || strings.Contains(got.Body.String(), private.ID) {
		t.Fatalf("shared canonical route %d %s", got.Code, got.Body)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	got = request("POST", "/wren-sharing/"+w.ID+"/open", memberToken, nil)
	if got.Code != 403 {
		t.Fatalf("viewer inference grant %d %s", got.Code, got.Body)
	}
}
func TestWrenSharingRoomAndReplyUsePublishedScopeWithoutPrivateConfigLeak(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	publication := publishWren(t, s, private, w)
	room, err := s.SaveRoom(ctx, private.WorkspaceID, member, WrenRoom{ID: "room_subscriber_shared_persona", Kind: "group", Name: "Subscriber team", WrenIDs: []string{w.ID}}, true)
	if err != nil || room.Wrens[0].ID != w.ID {
		t.Fatalf("workspace identity room %+v %v", room, err)
	}
	chat, err := s.OpenSharedWrenChat(ctx, private.WorkspaceID, member, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Conversation(ctx, private.WorkspaceID, member, chat.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueOperator(ctx, c, "reply-canonical-subscriber", "Answer in my own chat", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err = s.finishOperator(ctx, run, "completed", "A real subscriber answer"); err != nil {
		t.Fatal(err)
	}
	var message string
	if err = s.pool.QueryRow(ctx, `SELECT id FROM operator_messages WHERE run_id=$1 AND role='assistant'`, run.ID).Scan(&message); err != nil {
		t.Fatal(err)
	}
	reply, err := s.WrenReplyTarget(ctx, c.WorkspaceID, c.AccountID, c.ID, message)
	if err != nil || reply.WrenID != w.ID || !reply.CanWake {
		t.Fatalf("canonical reply %+v %v", reply, err)
	}
	if _, err = s.SaveWrenPublication(ctx, private.WorkspaceID, private.AccountID, w.ID, WrenPublicationInput{Published: false, ExpectedRevision: publication.Revision}); err != nil {
		t.Fatal(err)
	}
	owner, err := s.Wren(ctx, private.WorkspaceID, private.AccountID, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner.Purpose = "NEW PRIVATE INSTRUCTIONS AFTER UNPUBLISH"
	if _, err = s.SaveWren(ctx, private.WorkspaceID, private.AccountID, owner, false); err != nil {
		t.Fatal(err)
	}
	existing, err := s.Room(ctx, private.WorkspaceID, member, "room_knowledge_test")
	if err != nil || existing.Wrens[0].Purpose == owner.Purpose {
		t.Fatalf("existing room exposed new private config %+v %v", existing, err)
	}
	reply, err = s.WrenReplyTarget(ctx, c.WorkspaceID, c.AccountID, c.ID, message)
	if err != nil || reply.CanWake {
		t.Fatalf("revoked private reply wake %+v %v", reply, err)
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, shared.ID, w.ID); err != nil {
		t.Fatalf("publication withdrawal revoked separate room grant %v", err)
	}
}
