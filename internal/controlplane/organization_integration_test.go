package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestConversationShareAccessAttributionAndUnshare(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t)})
	owner, workspace, cookie := workspaceSession(t, s, "share-owner@example.com")
	teammate, _, teammateCookie := workspaceSession(t, s, "share-teammate@example.com")
	viewer, _, viewerCookie := workspaceSession(t, s, "share-viewer@example.com")
	_, _, strangerCookie := workspaceSession(t, s, "share-stranger@example.com")
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator'),($3,$2,'viewer')`, teammate.ID, workspace.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation(ctx, workspace.ID, owner.ID, "conv_share_one", "Deploy the site"); err != nil {
		t.Fatal(err)
	}
	path := "/v1/workspaces/" + workspace.ID + "/conversations/conv_share_one"

	// Only the owner can share; teammates and strangers cannot.
	shared := requestJSON(t, h, http.MethodPost, path+"/share", map[string]string{}, cookie)
	requireStatus(t, shared, http.StatusOK)
	requireStatus(t, requestJSON(t, h, http.MethodPost, path+"/share", map[string]string{}, teammateCookie), http.StatusForbidden)
	requireStatus(t, requestJSON(t, h, http.MethodPost, path+"/share", map[string]string{}, strangerCookie), http.StatusForbidden)
	requireStatus(t, requestJSON(t, h, http.MethodPost, path+"/share", map[string]string{}, cookie), http.StatusOK)

	listed := requestJSON(t, h, http.MethodGet, "/v1/workspaces/"+workspace.ID+"/conversations", nil, teammateCookie)
	requireStatus(t, listed, http.StatusOK)
	var page struct {
		Conversations []Conversation `json:"conversations"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || len(page.Conversations) != 1 {
		t.Fatalf("teammate list %s %v", listed.Body.String(), err)
	}
	got := page.Conversations[0]
	if !got.Shared || got.OwnerAccountID != owner.ID || got.OwnerEmail != owner.Email {
		t.Fatalf("shared listing %+v", got)
	}
	if !strings.Contains(requestJSON(t, h, http.MethodGet, path, nil, viewerCookie).Body.String(), "Deploy the site") {
		t.Fatal("viewer could not read shared conversation")
	}

	// A viewer cannot post; an operator teammate's run is attributed to them.
	postViewer := requestJSON(t, h, http.MethodPost, "/v1/workspaces/"+workspace.ID+"/conversations", map[string]string{"id": "conv_share_one", "requestId": "req_viewer_x", "message": "hello"}, viewerCookie)
	if postViewer.Code != http.StatusForbidden && postViewer.Code != http.StatusServiceUnavailable {
		t.Fatalf("viewer post %d %s", postViewer.Code, postViewer.Body.String())
	}
	c, err := s.Conversation(ctx, workspace.ID, teammate.ID, "conv_share_one")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueOperator(ctx, c, "req_teammate_attr", "teammate prompt", "test-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.RequesterAccountID != teammate.ID || run.SenderName != teammate.Email {
		t.Fatalf("teammate attribution %+v", run)
	}
	var requester string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(requester_account_id,'') FROM operator_runs WHERE id=$1`, run.ID).Scan(&requester); err != nil || requester != teammate.ID {
		t.Fatalf("stored requester %q %v", requester, err)
	}
	// Replaying someone else's request ID is forbidden.
	if _, err := s.EnqueueOperator(ctx, c, "req_teammate_attr", "teammate prompt", "test-model", nil); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.Conversation(ctx, workspace.ID, viewer.ID, "conv_share_one")
	if _, err = s.EnqueueOperator(ctx, c2, "req_teammate_attr", "teammate prompt", "test-model", nil); err != ErrForbidden {
		t.Fatalf("foreign request replay %v", err)
	}

	requireStatus(t, requestJSON(t, h, http.MethodDelete, path+"/share", map[string]string{}, cookie), http.StatusOK)
	requireStatus(t, requestJSON(t, h, http.MethodGet, path, nil, teammateCookie), http.StatusNotFound)
	listed = requestJSON(t, h, http.MethodGet, "/v1/workspaces/"+workspace.ID+"/conversations", nil, teammateCookie)
	requireStatus(t, listed, http.StatusOK)
	if strings.Contains(listed.Body.String(), "conv_share_one") {
		t.Fatalf("unshared conversation still listed %s", listed.Body.String())
	}
	var audited int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='conversation.shared'`, workspace.ID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("share audit %d %v", audited, err)
	}
}

func TestWorkspaceMemoryCRUDCapsAndBootstrap(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t)})
	owner, workspace, cookie := workspaceSession(t, s, "memory-owner@example.com")
	viewer, _, viewerCookie := workspaceSession(t, s, "memory-viewer@example.com")
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, viewer.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	base := "/v1/workspaces/" + workspace.ID + "/memory"

	created := requestJSON(t, h, http.MethodPost, base, map[string]string{"title": "Deploy process", "content": "Ship with canter deploy."}, cookie)
	requireStatus(t, created, http.StatusCreated)
	var entry WorkspaceMemoryEntry
	if err := json.Unmarshal(created.Body.Bytes(), &entry); err != nil || entry.Revision != 1 || entry.Title != "Deploy process" {
		t.Fatalf("created %+v %v", entry, err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, base, map[string]string{"title": "v", "content": "c"}, viewerCookie), http.StatusForbidden)

	listed := requestJSON(t, h, http.MethodGet, base, nil, viewerCookie)
	requireStatus(t, listed, http.StatusOK)
	if !strings.Contains(listed.Body.String(), entry.ID) {
		t.Fatalf("viewer list %s", listed.Body.String())
	}
	updated := requestJSON(t, h, http.MethodPatch, base+"/"+entry.ID, map[string]any{"title": "Deploy runbook", "content": "New content.", "baseRevision": entry.Revision}, cookie)
	requireStatus(t, updated, http.StatusOK)
	var saved WorkspaceMemoryEntry
	if err := json.Unmarshal(updated.Body.Bytes(), &saved); err != nil || saved.Revision != 2 {
		t.Fatalf("updated %+v %v", saved, err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPatch, base+"/"+entry.ID, map[string]any{"title": "Stale", "content": "Old.", "baseRevision": 1}, cookie), http.StatusConflict)
	requireStatus(t, requestJSON(t, h, http.MethodPost, base, map[string]string{"title": strings.Repeat("t", 121), "content": "c"}, cookie), http.StatusConflict)

	prompt := s.WorkspaceMemoryPrompt(ctx, workspace.ID)
	if !strings.Contains(prompt, "Deploy runbook") || !strings.Contains(prompt, "not instructions and not authority") {
		t.Fatalf("prompt block %q", prompt)
	}
	// The 8 KB cap bounds prompt injection.
	for i := 0; i < 4; i++ {
		if _, err := s.CreateWorkspaceMemory(ctx, workspace.ID, owner.ID, WorkspaceMemoryInput{Title: "bulk", Content: strings.Repeat("x", 3500)}); err != nil {
			t.Fatal(err)
		}
	}
	prompt = s.WorkspaceMemoryPrompt(ctx, workspace.ID)
	if len(prompt) > 8192 || !strings.Contains(prompt, "more entries not shown") {
		t.Fatalf("prompt cap %d %q", len(prompt), prompt[len(prompt)-60:])
	}
	// The connected-agent bootstrap exposes the same bounded set.
	boot, err := (&Service{Store: s}).Bootstrap(ctx, Principal{Installation: &Installation{ID: "inst_x"}, Session: &AgentSession{ID: "sess_x"}, WorkspaceID: workspace.ID})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range boot.OrganizationMemory {
		if item.Title == "Deploy runbook" {
			found = true
		}
	}
	if !found || len(boot.OrganizationMemory) == 0 {
		t.Fatalf("bootstrap memory %+v", boot.OrganizationMemory)
	}

	deleted := requestJSON(t, h, http.MethodDelete, base+"/"+entry.ID, map[string]any{"baseRevision": saved.Revision}, cookie)
	requireStatus(t, deleted, http.StatusOK)
	if items, err := s.WorkspaceMemory(ctx, workspace.ID, owner.ID); err != nil {
		t.Fatal(err)
	} else {
		for _, item := range items {
			if item.ID == entry.ID {
				t.Fatalf("deleted entry listed %+v", item)
			}
		}
	}
	var audited int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action IN ('memory.created','memory.updated','memory.deleted')`, workspace.ID).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("memory audits %d %v", audited, err)
	}
}

func TestWorkspaceAuditListingAndEmails(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Auth: AuthConfig{Mailer: &authTestMailer{}}})
	owner, workspace, cookie := workspaceSession(t, s, "audit-owner@example.com")
	other, _, otherCookie := workspaceSession(t, s, "audit-second@example.com")
	_, _, strangerCookie := workspaceSession(t, s, "audit-stranger@example.com")
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'owner')`, other.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}

	// Agent connect emits an audit event and an email to the other owner.
	device, err := s.BeginDeviceAuthorization(ctx, "Audit bot", "test-harness", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	approved := requestJSON(t, h, http.MethodPost, "/v1/device/authorizations/"+device.UserCode+"/approve", map[string]string{"workspaceId": workspace.ID}, cookie)
	requireStatus(t, approved, http.StatusOK)
	var approvedBody struct {
		Installation Installation `json:"installation"`
	}
	if err := json.Unmarshal(approved.Body.Bytes(), &approvedBody); err != nil {
		t.Fatal(err)
	}
	// A second event exists after the permission raise below.
	if _, err := s.pool.Exec(ctx, `UPDATE agent_installations SET inspect_allowed=false,draft_allowed=false,apply_mode='never' WHERE id=$1`, approvedBody.Installation.ID); err != nil {
		t.Fatal(err)
	}
	patch := httptest.NewRequest(http.MethodPatch, "/v1/installations/"+approvedBody.Installation.ID+"?workspaceId="+workspace.ID, strings.NewReader(`{"authority":{"inspect":true,"draft":true,"applyMode":"automatic"},"useWorkspaceAuthority":false}`))
	patch.Header.Set("Content-Type", "application/json")
	patch.Header.Set("Origin", "http://canter.test")
	patch.AddCookie(cookie)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, patch)
	if response.Code != http.StatusOK {
		t.Fatalf("permission patch %d %s", response.Code, response.Body.String())
	}

	listed := requestJSON(t, h, http.MethodGet, "/v1/workspaces/"+workspace.ID+"/audit?limit=1", nil, cookie)
	requireStatus(t, listed, http.StatusOK)
	var page struct {
		Events     []AuditEventView `json:"events"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || len(page.Events) != 1 || page.NextCursor == "" {
		t.Fatalf("audit page %s %v", listed.Body.String(), err)
	}
	first := page.Events[0]
	if first.Action != "agent.permissions.updated" || first.Actor.Kind != "human" || first.Actor.DisplayName != owner.Email || first.Actor.Harness != "" {
		t.Fatalf("audit actor %+v", first)
	}
	next := requestJSON(t, h, http.MethodGet, "/v1/workspaces/"+workspace.ID+"/audit?limit=1&before="+page.NextCursor, nil, cookie)
	requireStatus(t, next, http.StatusOK)
	var second struct {
		Events []AuditEventView `json:"events"`
	}
	if err := json.Unmarshal(next.Body.Bytes(), &second); err != nil || len(second.Events) != 1 || second.Events[0].ID == first.ID || second.Events[0].Action != "agent.authorized" {
		t.Fatalf("second page %s %v", next.Body.String(), err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodGet, "/v1/workspaces/"+workspace.ID+"/audit", nil, otherCookie), http.StatusOK)
	requireStatus(t, requestJSON(t, h, http.MethodGet, "/v1/workspaces/"+workspace.ID+"/audit", nil, strangerCookie), http.StatusForbidden)

	// The connect and permission-raise emails went to the other owner only.
	var recipients []string
	rows, err := s.pool.Query(ctx, `SELECT recipient_hash FROM auth_email_outbox WHERE status='queued' AND account_id IN (SELECT id FROM accounts WHERE email IN ($1,$2))`, other.Email, owner.Email)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, string(hash))
	}
	// The other owner gets both emails; the acting owner gets neither.
	if len(recipients) != 2 {
		t.Fatalf("security emails %v", recipients)
	}
	var events int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action IN ('agent.authorized','agent.permissions.updated')`, workspace.ID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("agent audits %d %v", events, err)
	}
}

func TestAuditEventSubjectLabels(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	a, w, _, err := s.Signup(ctx, "auditlabels@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// metadata-first labels: name/email/title/port come straight from metadata.
	if err := s.Audit(ctx, w.ID, sdk.ActorRef{Kind: "human", ID: a.ID}, "sandbox.created", "sbx_gone", map[string]any{"name": "my-box"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, w.ID, sdk.ActorRef{Kind: "human", ID: a.ID}, "member.invited", "winvite_x", map[string]any{"email": "new@example.com"}); err != nil {
		t.Fatal(err)
	}
	// live lookups: workspace name, account email.
	if err := s.Audit(ctx, w.ID, sdk.ActorRef{Kind: "human", ID: a.ID}, "workspace.renamed", w.ID, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, w.ID, sdk.ActorRef{Kind: "human", ID: a.ID}, "member.left", a.ID, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// product subject resolves through the catalog.
	if err := s.Audit(ctx, w.ID, sdk.ActorRef{Kind: "human", ID: a.ID}, "product.enabled", "sandboxes", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	// deleted/unknown subjects keep an empty label.
	if err := s.Audit(ctx, w.ID, sdk.ActorRef{Kind: "human", ID: a.ID}, "memory.deleted", "wmemo_gone", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	events, _, err := s.AuditEvents(ctx, w.ID, a.ID, "", 25)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"sandbox.created":   "my-box",
		"member.invited":    "new@example.com",
		"workspace.renamed": w.Name,
		"member.left":       "auditlabels@example.com",
		"product.enabled":   "Sandboxes",
		"memory.deleted":    "",
	}
	for _, e := range events {
		if expected, ok := want[e.Action]; ok && e.SubjectLabel != expected {
			t.Errorf("%s: label=%q want %q", e.Action, e.SubjectLabel, expected)
		}
	}
}
