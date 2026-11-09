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

func lifecycleBool(v bool) *bool { return &v }
func lifecycleInt(v int) *int    { return &v }
func archiveLifecycle(t *testing.T, s *Store, c Conversation, revision int, archived bool) WrenLifecycle {
	t.Helper()
	v, err := s.UpdateWrenLifecycle(context.Background(), c.WorkspaceID, c.AccountID, c.ID, WrenLifecycleInput{Archived: lifecycleBool(archived), ExpectedRevision: lifecycleInt(revision)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestWrenLifecycleHideIsPersonalAndNeverStops(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	run, err := s.EnqueueOperator(ctx, private, "lifecycle-hide-request", "Keep working", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := s.UpdateWrenLifecycle(ctx, private.WorkspaceID, private.AccountID, private.ID, WrenLifecycleInput{Hidden: lifecycleBool(true)})
	if err != nil || !hidden.Hidden || hidden.Archived || hidden.Revision != 0 {
		t.Fatalf("personal hide %+v %v", hidden, err)
	}
	var status string
	if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, run.ID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("hide stopped work %s %v", status, err)
	}
	if _, err = s.UpdateWrenLifecycle(ctx, shared.WorkspaceID, member, shared.ID, WrenLifecycleInput{Hidden: lifecycleBool(true)}); err != nil {
		t.Fatal(err)
	}
	other, err := s.WrenConversationLifecycle(ctx, shared.WorkspaceID, private.AccountID, shared.ID)
	if err != nil || other.Hidden {
		t.Fatalf("personal preference leaked %+v %v", other, err)
	}
	ownerList, ownerErr := s.WrenLifecycles(ctx, private.WorkspaceID, private.AccountID)
	if ownerErr != nil || len(ownerList) != 2 {
		t.Fatalf("owner recovery list %+v %v", ownerList, ownerErr)
	}
	list, err := s.WrenLifecycles(ctx, private.WorkspaceID, member)
	if err != nil || len(list) != 1 || !list[0].Hidden {
		t.Fatalf("recovery list %+v %v", list, err)
	}
	if _, err = s.UpdateWrenLifecycle(ctx, private.WorkspaceID, member, w.ConversationID, WrenLifecycleInput{Hidden: lifecycleBool(true)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private hide exposed %v", err)
	}
}
func TestWrenLifecycleArchiveStopsModelsJobsAndPausesSchedules(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	job := requireWrenJob(t, s, c, parent, "lifecycle-child")
	w, err := s.Wren(ctx, c.WorkspaceID, c.AccountID, "wren_job_lead")
	if err != nil {
		t.Fatal(err)
	}
	responsibility, err := s.SaveWrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibilityInput(c, w))
	if err != nil {
		t.Fatal(err)
	}
	artifact := libraryArtifact(t, s, c, "retained.csv", "text/csv", "name\nKept\n")
	// A retained computer record must not be changed merely by archive. A fake
	// provider phase is never executed by this store-only integration test.
	_, err = s.pool.Exec(ctx, `INSERT INTO wren_computers(id,workspace_id,conversation_id,owner_id,scope_kind,name,phase,plan,ssh_key_version,ssh_key_ciphertext,monthly_cents,expires_at) VALUES('computer_lifecycle',$1,$2,$3,'private','Retained computer','queued','{}','test',''::bytea,500,now()+interval '1 hour')`, c.WorkspaceID, c.ID, c.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	state := archiveLifecycle(t, s, c, 0, true)
	if !state.Archived || state.Revision != 1 || state.ArchivedBy != c.AccountID {
		t.Fatalf("archive %+v", state)
	}
	for _, id := range []string{parent.ID, job.ChildRunID} {
		var status string
		if err = s.pool.QueryRow(ctx, `SELECT status FROM operator_runs WHERE id=$1`, id).Scan(&status); err != nil || status != "cancelled" {
			t.Fatalf("run not stopped %s %s %v", id, status, err)
		}
	}
	child, err := s.WrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID)
	if err != nil || !child.ReturnSuppressed || child.Status != "cancelled" {
		t.Fatalf("child return remains live %+v %v", child, err)
	}
	routine, err := s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibility.ID)
	if err != nil || routine.State != "paused" || routine.NextRunAt != nil || routine.Revision != responsibility.Revision+1 {
		t.Fatalf("schedule %+v %v", routine, err)
	}
	if _, enqueueErr := s.EnqueueOperator(ctx, c, "lifecycle-after-archive", "Must not start", "test", nil); !errors.Is(enqueueErr, ErrConflict) {
		t.Fatalf("archived enqueue allowed %v", enqueueErr)
	}
	if err = s.ReconcileWrenJobs(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	var returns int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE request_id=$1`, "job_return_"+job.ID).Scan(&returns); err != nil || returns != 0 {
		t.Fatalf("archived job returned %d %v", returns, err)
	}
	if err = s.ValidateWrenLifecycleRun(ctx, parent); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived run allowed %v", err)
	}
	if err = s.ValidateWrenLifecycleConversation(ctx, job.ChildConversationID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived child allowed %v", err)
	}
	if _, err = s.GetWrenArtifact(ctx, c.WorkspaceID, c.AccountID, artifact.ID, 0); err != nil {
		t.Fatalf("archive deleted artifact %v", err)
	}
	var phase string
	var cents int64
	if err = s.pool.QueryRow(ctx, `SELECT phase,monthly_cents FROM wren_computers WHERE id='computer_lifecycle'`).Scan(&phase, &cents); err != nil || phase != "queued" || cents != 500 {
		t.Fatalf("archive changed computer %s %d %v", phase, cents, err)
	}
	archiveLifecycle(t, s, c, 1, false)
	routine, err = s.WrenResponsibility(ctx, c.WorkspaceID, c.AccountID, responsibility.ID)
	if err != nil || routine.State != "paused" || routine.NextRunAt != nil {
		t.Fatalf("restoration silently resumed %+v %v", routine, err)
	}
	if err = s.ValidateWrenLifecycleRun(ctx, parent); err != nil {
		t.Fatalf("restored guard %v", err)
	}
	if _, err = s.UpdateWrenLifecycle(ctx, c.WorkspaceID, c.AccountID, c.ID, WrenLifecycleInput{Archived: lifecycleBool(true), ExpectedRevision: lifecycleInt(0)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale archive overwrote restore %v", err)
	}
}
func TestWrenLifecycleRoomAuthorityAndWrenSpeakerIsolation(t *testing.T) {
	s, private, w, shared, member := knowledgeFixture(t)
	ctx := context.Background()
	if _, err := s.UpdateWrenLifecycle(ctx, shared.WorkspaceID, member, shared.ID, WrenLifecycleInput{Archived: lifecycleBool(true), ExpectedRevision: lifecycleInt(0)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ordinary participant archived group %v", err)
	}
	// Persist two actual queued room runs: only the archived Wren's speaker is
	// stopped. A colleague's independently authorized turn stays queued.
	first, err := s.EnqueueOperator(ctx, shared, "lifecycle-room-one", "First", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueOperator(ctx, shared, "lifecycle-room-two", "Second", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET speaker=$2 WHERE id=$1`, first.ID, []byte(`{"id":"`+w.ID+`","name":"Scout"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operator_runs SET speaker='{"id":"wren_other","name":"Other"}' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	archiveLifecycle(t, s, private, 0, true)
	var firstStatus, secondStatus string
	if err = s.pool.QueryRow(ctx, `SELECT (SELECT status FROM operator_runs WHERE id=$1),(SELECT status FROM operator_runs WHERE id=$2)`, first.ID, second.ID).Scan(&firstStatus, &secondStatus); err != nil || firstStatus != "cancelled" || secondStatus != "queued" {
		t.Fatalf("speaker isolation %s %s %v", firstStatus, secondStatus, err)
	}
	if err = s.ValidateWrenLifecycleRun(ctx, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived speaker run allowed %v", err)
	}
	if err = s.ValidateWrenLifecycleRun(ctx, second); err != nil {
		t.Fatalf("other speaker blocked %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, shared.WorkspaceID, member); err != nil {
		t.Fatal(err)
	}
	v, err := s.UpdateWrenLifecycle(ctx, shared.WorkspaceID, member, shared.ID, WrenLifecycleInput{Hidden: lifecycleBool(true)})
	if err != nil || !v.Hidden || v.CanManage {
		t.Fatalf("viewer preference %+v %v", v, err)
	}
}
func TestWrenLifecycleExportActualScopeAndRetainedContent(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	job := requireWrenJob(t, s, c, parent, "export-child")
	if err := s.finishOperator(ctx, parent, "completed", "Visible completion retained."); err != nil {
		t.Fatal(err)
	}
	artifact := libraryArtifact(t, s, c, "published.csv", "text/csv", "name\nReal\n")
	if _, err := s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES('message_private_worker',$1,$2,'assistant','CHILD PRIVATE MUST NOT EXPORT')`, job.ChildConversationID, job.ChildRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE operator_messages SET attachments='[{"id":"attachment_export","name":"notes.txt","mediaType":"text/plain","size":12,"dataBase64":"SECRETBASE64"}]' WHERE run_id=$1 AND role='user'`, parent.ID); err != nil {
		t.Fatal(err)
	}
	export, err := s.ExportWrenTranscript(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	if len(export.Messages) != 2 || len(export.Artifacts) != 1 || export.Artifacts[0].ID != artifact.ID || !strings.Contains(string(body), "Visible completion retained.") || strings.Contains(string(body), "CHILD PRIVATE MUST NOT EXPORT") || strings.Contains(string(body), "dataBase64") || strings.Contains(string(body), "SECRETBASE64") || !strings.Contains(string(body), "notes.txt") {
		t.Fatalf("invalid scoped export %s", body)
	}
	markdown := export.Markdown()
	if !strings.Contains(markdown, "Visible completion retained.") || !strings.Contains(markdown, artifact.SHA256) || strings.Contains(markdown, "CHILD PRIVATE") {
		t.Fatalf("invalid Markdown %s", markdown)
	}
	archiveLifecycle(t, s, c, 0, true)
	retained, err := s.ExportWrenTranscript(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil || len(retained.Messages) != 2 || !retained.Lifecycle.Archived {
		t.Fatalf("archive lost export %+v %v", retained, err)
	}
	if _, err = s.ExportWrenTranscript(ctx, c.WorkspaceID, c.AccountID, job.ChildConversationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child export allowed %v", err)
	}
	if _, err = s.pool.Exec(ctx, `DELETE FROM memberships WHERE workspace_id=$1 AND account_id=$2`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExportWrenTranscript(ctx, c.WorkspaceID, c.AccountID, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked export %v", err)
	}
}
func TestWrenLifecycleConcurrentArchiveCAS(t *testing.T) {
	s, c, _ := responsibilityFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdateWrenLifecycle(ctx, c.WorkspaceID, c.AccountID, c.ID, WrenLifecycleInput{Archived: lifecycleBool(true), ExpectedRevision: lifecycleInt(0)})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("archive race %d success %d conflict", success, conflict)
	}
}

func TestWrenLifecycleHTTPRecoveryAndDownloadScope(t *testing.T) {
	s, private, _, shared, _ := knowledgeFixture(t)
	ctx := context.Background()
	_, _, token, err := s.Signin(ctx, "knowledge-member@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test"}).(*HTTPServer)
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+private.WorkspaceID+path, bytes.NewReader(data))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	got := request("PATCH", "/wren-lifecycle/"+shared.ID, map[string]bool{"hidden": true})
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"hidden":true`) {
		t.Fatalf("HTTP hide %d %s", got.Code, got.Body)
	}
	got = request("GET", "/wren-lifecycle", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), shared.ID) || strings.Contains(got.Body.String(), private.ID) {
		t.Fatalf("HTTP recovery list %d %s", got.Code, got.Body)
	}
	for _, format := range []string{"json", "markdown"} {
		got = request("GET", "/wren-lifecycle/"+shared.ID+"/export?format="+format, nil)
		if got.Code != 200 || !strings.HasPrefix(got.Header().Get("Content-Disposition"), "attachment;") || got.Header().Get("Content-Security-Policy") != "sandbox" || got.Header().Get("Cache-Control") != "no-store" || !strings.Contains(got.Body.String(), shared.ID) {
			t.Fatalf("HTTP export %d %s", got.Code, got.Body)
		}
	}
	got = request("GET", "/wren-lifecycle/"+private.ID+"/export?format=json", nil)
	if got.Code != 404 {
		t.Fatalf("HTTP private export %d", got.Code)
	}
	got = request("PATCH", "/wren-lifecycle/"+shared.ID, map[string]any{"archived": true, "expectedRevision": 0})
	if got.Code != 403 {
		t.Fatalf("HTTP unauthorized archive %d", got.Code)
	}
}

func TestWrenLifecycleArchiveRevokesHarnessGrantWithoutClaimingGuestStopped(t *testing.T) {
	h, c, run, job, grant, token := harnessFixture(t)
	s := h.service.Store
	ctx := context.Background()
	archiveLifecycle(t, s, c, 0, true)
	var status string
	var requested, revoked bool
	if err := s.pool.QueryRow(ctx, `SELECT r.status,e.stop_requested,g.revoked_at IS NOT NULL FROM operator_runs r JOIN wren_harness_executions e ON e.run_id=r.id JOIN wren_model_grants g ON g.id=$2 WHERE r.id=$1`, run.ID, grant.ID).Scan(&status, &requested, &revoked); err != nil || status != "running" || !requested || !revoked {
		t.Fatalf("archive falsely completed remote stop %s %v %v %v", status, requested, revoked, err)
	}
	current, err := s.WrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID)
	if err != nil || current.Status != "stop_requested" || !current.ReturnSuppressed {
		t.Fatalf("remote state %+v %v", current, err)
	}
	response := brokerRequest(h, token, `{"model":"test-worker","messages":[{"role":"user","content":"must not execute"}]}`)
	if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
		t.Fatalf("archived grant retained access %d", response.Code)
	}
}
