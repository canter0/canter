package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func projectTest(t *testing.T) (*Store, Conversation, OperatorRun, WrenProject) {
	t.Helper()
	s, c, run := wrenJobFixture(t)
	p, e := s.CreateWrenProject(context.Background(), c.WorkspaceID, c.AccountID, "Cedar clinic", "")
	if e != nil {
		t.Fatal(e)
	}
	return s, c, run, p
}
func projectNoteTest(t *testing.T, s *Store, c Conversation, p WrenProject, text string) WrenProjectNote {
	t.Helper()
	n, e := s.SaveWrenProjectNote(context.Background(), c.WorkspaceID, c.AccountID, p.ID, WrenProjectNoteInput{Title: "Public clinic fact", Content: text}, false)
	if e != nil {
		t.Fatal(e)
	}
	return n
}
func TestWrenProjectKnowledgeExplicitScopeTenantCASAndForget(t *testing.T) {
	s, c, run, p := projectTest(t)
	ctx := context.Background()
	n := projectNoteTest(t, s, c, p, "Shared project hours: Tuesday 9am.")
	if text, e := s.ProjectContext(ctx, c, run); e != nil || text != "" {
		t.Fatalf("unattachedprojectleaked %q %v", text, e)
	}
	g, e := s.AttachWrenProject(ctx, c.WorkspaceID, c.AccountID, p.ID, c.ID, nil)
	if e != nil || g.TargetKind != "private" || !g.CanManage {
		t.Fatal(g, e)
	}
	text, e := s.ProjectContext(ctx, c, run)
	if e != nil || !strings.Contains(text, "Tuesday 9am") {
		t.Fatalf("attachedprojectmissing %q %v", text, e)
	}
	if _, e = s.SaveWrenProjectNote(ctx, c.WorkspaceID, c.AccountID, p.ID, WrenProjectNoteInput{ID: n.ID, Title: "stale", Content: "wrong", ExpectedRevision: 0}, false); !errors.Is(e, ErrConflict) {
		t.Fatal("staleeditoverwroteknowledge")
	}
	n, e = s.SaveWrenProjectNote(ctx, c.WorkspaceID, c.AccountID, p.ID, WrenProjectNoteInput{ID: n.ID, Title: n.Title, Content: "Wednesday 10am", ExpectedRevision: n.Revision}, false)
	if e != nil {
		t.Fatal(e)
	}
	notes, e := s.WrenProjectNotes(ctx, c.WorkspaceID, c.AccountID, p.ID, "Wednesday")
	if e != nil || len(notes) != 1 {
		t.Fatal(notes, e)
	}
	_, other, _, e := s.Signup(ctx, "project-foreign@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.WrenProject(ctx, other.ID, c.AccountID, p.ID); e == nil {
		t.Fatal("foreignworkspaceprojectleaked")
	}
	if _, e = s.SaveWrenProjectNote(ctx, c.WorkspaceID, c.AccountID, p.ID, WrenProjectNoteInput{ID: n.ID, ExpectedRevision: n.Revision}, true); e != nil {
		t.Fatal(e)
	}
	history, e := s.WrenProjectNoteHistory(ctx, c.WorkspaceID, c.AccountID, p.ID, n.ID)
	if e != nil || len(history) != 3 {
		t.Fatal(history, e)
	}
	for _, v := range history {
		if v.Content != "" || v.Title != "Forgotten note" || len(v.Sources.URLs) != 0 {
			t.Fatal("forgottenoldrevisionleaked")
		}
	}
}
func TestWrenProjectRoomGrantDoesNotMovePrivateKnowledgeOrPersonaGrant(t *testing.T) {
	s, c, _, p := projectTest(t)
	ctx := context.Background()
	private, e := s.SaveWrenMemory(ctx, c.WorkspaceID, c.AccountID, WrenMemoryInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "private", WrenID: "wren_job_lead"}, Title: "Secret", Content: "PRIVATE PIN 7421"})
	_ = private
	if e != nil {
		t.Fatal(e)
	}
	projectNoteTest(t, s, c, p, "WORKSPACE FACT blue chairs")
	g, e := s.AttachWrenProject(ctx, c.WorkspaceID, c.AccountID, p.ID, c.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	room, e := s.SaveRoom(ctx, c.WorkspaceID, c.AccountID, WrenRoom{ID: "room_project_shared", Kind: "channel", Name: "Clinic", WrenIDs: []string{"wren_job_lead"}}, true)
	if e != nil {
		t.Fatal(e)
	}
	rc, e := s.Conversation(ctx, c.WorkspaceID, c.AccountID, room.ConversationID)
	if e != nil {
		t.Fatal(e)
	}
	s.EnqueueOperator(ctx, rc, "project-room-run", "Review clinic", "test", nil)
	rr, ok, e := s.claimOperator(ctx)
	if e != nil || !ok {
		t.Fatal(e)
	}
	text, e := s.ProjectContext(ctx, rc, rr)
	if e != nil || text != "" {
		t.Fatalf("globalpersona grant leakedroom %q %v", text, e)
	}
	if _, e = s.AttachWrenProject(ctx, c.WorkspaceID, c.AccountID, p.ID, rc.ID, nil); e != nil {
		t.Fatal(e)
	}
	text, e = s.ProjectContext(ctx, rc, rr)
	if e != nil || !strings.Contains(text, "blue chairs") || strings.Contains(text, "PRIVATE PIN") {
		t.Fatalf("wrongsharedcontext %q %v", text, e)
	}
	actor, _, _, e := s.Signup(ctx, "project-colleague@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role)VALUES($1,$2,'operator')`, actor.ID, c.WorkspaceID)
	grants, e := s.WrenProjectGrants(ctx, c.WorkspaceID, actor.ID, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range grants {
		if v.ID == g.ID {
			t.Fatal("otherhumanprivategrantmetadataleaked")
		}
	}
}
func TestWrenProjectChildSelectionAndRevokeFencePublication(t *testing.T) {
	s, c, parent, p := projectTest(t)
	ctx := context.Background()
	projectNoteTest(t, s, c, p, "PROJECT ONLY FIELD 923")
	g, e := s.AttachWrenProject(ctx, c.WorkspaceID, c.AccountID, p.ID, c.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	plain, e := s.CreateWrenJob(ctx, parent, c, "without-project", "No access", "Use the brief only", "test")
	if e != nil {
		t.Fatal(e)
	}
	selected, e := s.CreateWrenJob(ctx, parent, c, "with-project", "Selected access", "Use the selected clinic project", "test", []string{p.ID})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateWrenJob(ctx, parent, c, "with-project", "Selected access", "Use project", "test", []string{}); !errors.Is(e, ErrConflict) {
		t.Fatal("replaychangedacceptedgrants")
	}
	for _, j := range []WrenJob{plain, selected} {
		child, e := s.Conversation(ctx, c.WorkspaceID, c.AccountID, j.ChildConversationID)
		if e != nil {
			t.Fatal(e)
		}
		var run OperatorRun
		run.ID = j.ChildRunID
		run.ConversationID = j.ChildConversationID
		var text string
		text, e = s.ProjectContext(ctx, child, run)
		if e != nil {
			t.Fatal(e)
		}
		if (j.ID == selected.ID) != strings.Contains(text, "PROJECT ONLY FIELD") {
			t.Fatalf("implicit ormissingchildinherit %s %q", j.ID, text)
		}
	}
	if _, e = s.RevokeWrenProjectGrant(ctx, c.WorkspaceID, c.AccountID, p.ID, g.ID, g.Revision); e != nil {
		t.Fatal(e)
	}
	var cancelled bool
	s.pool.QueryRow(ctx, `SELECT cancelled_at IS NOT NULL FROM wren_jobs WHERE id=$1`, selected.ID).Scan(&cancelled)
	if !cancelled {
		t.Fatal("revokedchildstillactive")
	}
	child := Conversation{ID: selected.ChildConversationID, WorkspaceID: c.WorkspaceID, AccountID: c.AccountID}
	if e = s.ValidateWrenProjectRun(ctx, child, OperatorRun{ID: selected.ChildRunID}); e == nil {
		t.Fatal("revokedchildstillreads")
	}
	_, e = s.PutWrenArtifact(ctx, c.WorkspaceID, c.AccountID, WrenArtifactInput{ConversationID: c.ID, Name: "leak.txt", MediaType: "text/plain", Content: []byte("PROJECT ONLY FIELD 923"), Provenance: WrenArtifactProvenance{RunID: selected.ChildRunID, JobID: selected.ID}})
	if e == nil {
		t.Fatal("revokedworkerpublishedprojectfacts")
	}
}

func TestWrenProjectHTTPWriterViewerAndRealSystemLink(t *testing.T) {
	s, c, _, p := projectTest(t)
	ctx := context.Background()
	_, _, ownerToken, e := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if e != nil {
		t.Fatal(e)
	}
	viewer, _, viewerToken, e := s.Signup(ctx, "project-viewer@example.test", "correct horse battery staple", "", false)
	if e != nil {
		t.Fatal(e)
	}
	s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role)VALUES($1,$2,'viewer')`, viewer.ID, c.WorkspaceID)
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "https://canter.test"})
	call := func(method, tail, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/workspaces/"+c.WorkspaceID+"/wren-projects"+tail, strings.NewReader(body))
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		}
		r.Header.Set("Origin", "https://canter.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call("GET", "/"+p.ID, viewerToken, ""); w.Code != 200 {
		t.Fatalf("viewerread %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/"+p.ID+"/notes", viewerToken, `{"title":"bad","content":"viewer mutation"}`); w.Code != 403 {
		t.Fatal("viewerwrote", w.Code)
	}
	if w := call("POST", "", ownerToken, `{"name":"Fake system","systemName":"nonexistent"}`); w.Code != 404 {
		t.Fatalf("inventedSystemlink %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/"+p.ID+"/notes", ownerToken, `{"title":"Hours","content":"Public hours Monday","sources":{"urls":["https://example.com/hours"]}}`); w.Code != 201 {
		t.Fatalf("HTTPnotecreate %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/"+p.ID+"/notes?q=PRIVATE", ownerToken, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"notes":[]`) {
		t.Fatalf("projectsearchleakedprivatecontent %d %s", w.Code, w.Body.String())
	}
}
