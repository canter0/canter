package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func workspaceSession(t *testing.T, s *Store, email string) (Account, Workspace, *http.Cookie) {
	t.Helper()
	a, w, token, err := s.Signup(context.Background(), email, "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return a, w, &http.Cookie{Name: "canter_session", Value: token}
}

func workspaceTestAgent(t *testing.T, s *Store, owner Account, workspace string, name string) TokenPair {
	t.Helper()
	device, err := s.BeginDeviceAuthorization(context.Background(), name, "test", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDevice(context.Background(), device.UserCode, owner.ID, workspace); err != nil {
		t.Fatal(err)
	}
	pair, err := s.ExchangeDevice(context.Background(), device.DeviceCode, name)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func TestWorkspaceCreateIdempotentCapacityAndAgents(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t)})
	owner, personal, cookie := workspaceSession(t, s, "org-owner@example.com")

	created := requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"id": "wrk_create_test_alpha", "name": "  Acme Team  "}, cookie)
	requireStatus(t, created, http.StatusCreated)
	var workspace Workspace
	if err := json.Unmarshal(created.Body.Bytes(), &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.ID != "wrk_create_test_alpha" || workspace.Name != "Acme Team" || workspace.Role != "owner" || !workspace.AgentAuthority.Inspect {
		t.Fatalf("created workspace %+v", workspace)
	}
	if role, err := s.Membership(ctx, owner.ID, workspace.ID); err != nil || role != "owner" {
		t.Fatalf("creator membership %q %v", role, err)
	}
	var caps int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_usage_caps WHERE workspace_id=$1`, workspace.ID).Scan(&caps); err != nil || caps != 1 {
		t.Fatalf("workspace not initialized like signup %d %v", caps, err)
	}
	var audited int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='workspace.created'`, workspace.ID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("create audit %d %v", audited, err)
	}

	replay := requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"id": "wrk_create_test_alpha", "name": "Acme Team"}, cookie)
	requireStatus(t, replay, http.StatusOK)
	var replayed Workspace
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil || replayed.ID != workspace.ID {
		t.Fatalf("replay %+v %v", replayed, err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"id": "wrk_create_test_alpha", "name": "Different Name"}, cookie), http.StatusConflict)

	for _, name := range []string{"", "   ", strings.Repeat("x", 65), "two\nlines"} {
		requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"name": name}, cookie), http.StatusBadRequest)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"id": "not-a-workspace-id", "name": "Team"}, cookie), http.StatusBadRequest)

	for i := 0; i < 8; i++ {
		response := requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"name": fmt.Sprintf("Team %d", i)}, cookie)
		requireStatus(t, response, http.StatusCreated)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"name": "One too many"}, cookie), http.StatusTooManyRequests)
	// A replayed request still resolves its original record at the cap.
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/workspaces", map[string]string{"id": "wrk_create_test_alpha", "name": "Acme Team"}, cookie), http.StatusOK)

	pair := workspaceTestAgent(t, s, owner, personal.ID, "Organization builder")
	requireStatus(t, requestJSONWithBearer(t, h, "/v1/workspaces", map[string]string{"name": "Agent org"}, pair.AccessToken), http.StatusForbidden)
}

func TestWorkspaceRenameRequiresOwner(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t)})
	owner, workspace, cookie := workspaceSession(t, s, "rename-owner@example.com")
	operator, _, operatorCookie := workspaceSession(t, s, "rename-operator@example.com")
	viewer, _, viewerCookie := workspaceSession(t, s, "rename-viewer@example.com")
	_, _, strangerCookie := workspaceSession(t, s, "rename-stranger@example.com")
	if _, err := s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'operator'),($3,$2,'viewer')`, operator.ID, workspace.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	path := "/v1/workspaces/" + workspace.ID
	requireStatus(t, requestJSON(t, h, http.MethodPatch, path, map[string]string{"name": "Operator rename"}, operatorCookie), http.StatusForbidden)
	requireStatus(t, requestJSON(t, h, http.MethodPatch, path, map[string]string{"name": "Viewer rename"}, viewerCookie), http.StatusForbidden)
	requireStatus(t, requestJSON(t, h, http.MethodPatch, path, map[string]string{"name": "Stranger rename"}, strangerCookie), http.StatusNotFound)
	requireStatus(t, requestJSON(t, h, http.MethodPatch, path, map[string]string{"name": ""}, cookie), http.StatusBadRequest)
	pair := workspaceTestAgent(t, s, owner, workspace.ID, "Renamer")
	agentRename := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(`{"name":"Agent rename"}`))
	agentRename.Header.Set("Content-Type", "application/json")
	agentRename.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, agentRename)
	requireStatus(t, response, http.StatusForbidden)

	renamed := requestJSON(t, h, http.MethodPatch, path, map[string]string{"name": "  Ship Crew  "}, cookie)
	requireStatus(t, renamed, http.StatusOK)
	var out Workspace
	if err := json.Unmarshal(renamed.Body.Bytes(), &out); err != nil || out.Name != "Ship Crew" || out.Role != "owner" {
		t.Fatalf("renamed %+v %v", out, err)
	}
	current, err := s.Workspace(ctx, workspace.ID)
	if err != nil || current.Name != "Ship Crew" {
		t.Fatalf("stored name %+v %v", current, err)
	}
	var audited int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='workspace.renamed'`, workspace.ID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("rename audit %d %v", audited, err)
	}
}

func TestWorkspaceMembersAliasMatchesWrenMembers(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t)})
	owner, workspace, cookie := workspaceSession(t, s, "alias-owner@example.com")
	member, _, _ := workspaceSession(t, s, "alias-member@example.com")
	_, _, otherCookie := workspaceSession(t, s, "alias-other@example.com")
	path := "/v1/workspaces/" + workspace.ID

	invited := requestJSON(t, h, http.MethodPost, path+"/members/invites", map[string]string{"id": "winvite_alias_member", "email": member.Email, "role": "viewer"}, cookie)
	requireStatus(t, invited, http.StatusOK)
	var created struct {
		Invite WorkspaceInvite `json:"invite"`
		Token  string          `json:"token"`
	}
	if err := json.Unmarshal(invited.Body.Bytes(), &created); err != nil || created.Token == "" {
		t.Fatalf("alias invite %s %v", invited.Body.String(), err)
	}
	listInvites := requestJSON(t, h, http.MethodGet, path+"/members/invites", nil, cookie)
	requireStatus(t, listInvites, http.StatusOK)
	if !strings.Contains(listInvites.Body.String(), created.Invite.ID) {
		t.Fatalf("invites list %s", listInvites.Body.String())
	}
	if _, err := s.ConsumeWorkspaceInvite(ctx, member.ID, created.Token, true); err != nil {
		t.Fatal(err)
	}

	for _, route := range []string{"members", "wren-members"} {
		list := requestJSON(t, h, http.MethodGet, path+"/"+route, nil, cookie)
		requireStatus(t, list, http.StatusOK)
		if !strings.Contains(list.Body.String(), member.Email) {
			t.Fatalf("%s list %s", route, list.Body.String())
		}
	}
	requireStatus(t, requestJSON(t, h, http.MethodPatch, path+"/members/"+member.ID, map[string]string{"role": "operator"}, cookie), http.StatusOK)
	if role, err := s.Membership(ctx, member.ID, workspace.ID); err != nil || role != "operator" {
		t.Fatalf("aliased role patch %q %v", role, err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodDelete, path+"/members/"+member.ID, nil, cookie), http.StatusOK)
	if members, err := s.WorkspaceMembers(ctx, workspace.ID, owner.ID); err != nil || len(members) != 1 {
		t.Fatalf("aliased delete %+v %v", members, err)
	}
	// Room transfer stays under wren-members; the alias does not expose it.
	requireStatus(t, requestJSON(t, h, http.MethodPost, path+"/members/rooms/room_x/transfer", map[string]string{"accountId": member.ID}, cookie), http.StatusNotFound)
	requireStatus(t, requestJSON(t, h, http.MethodGet, path+"/members", nil, otherCookie), http.StatusForbidden)
}

func TestMeInvitesListAcceptDecline(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t)})
	owner, workspace, _ := workspaceSession(t, s, "inviter@example.com")
	member, _, memberCookie := workspaceSession(t, s, "invitee@example.com")
	expiree, _, expireeCookie := workspaceSession(t, s, "expiree@example.com")
	revokee, _, revokeeCookie := workspaceSession(t, s, "revokee@example.com")
	declinee, _, declineeCookie := workspaceSession(t, s, "declinee@example.com")
	_, _, strangerCookie := workspaceSession(t, s, "stranger@example.com")

	invite, _, err := s.CreateWorkspaceInvite(ctx, workspace.ID, owner.ID, WorkspaceInviteInput{ID: "winvite_inapp_one", Email: "Invitee@Example.com", Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	expired, _, err := s.CreateWorkspaceInvite(ctx, workspace.ID, owner.ID, WorkspaceInviteInput{ID: "winvite_inapp_expired", Email: expiree.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE workspace_invites SET expires_at=now()-interval '1 minute' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	revoked, _, err := s.CreateWorkspaceInvite(ctx, workspace.ID, owner.ID, WorkspaceInviteInput{ID: "winvite_inapp_revoked", Email: revokee.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeWorkspaceInvite(ctx, workspace.ID, owner.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	declined, _, err := s.CreateWorkspaceInvite(ctx, workspace.ID, owner.ID, WorkspaceInviteInput{ID: "winvite_inapp_declined", Email: declinee.Email, Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}

	listed := requestJSON(t, h, http.MethodGet, "/v1/me/invites", nil, memberCookie)
	requireStatus(t, listed, http.StatusOK)
	var body struct {
		Invites []WorkspaceInvitation `json:"invites"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &body); err != nil || len(body.Invites) != 1 {
		t.Fatalf("invitee list %s %v", listed.Body.String(), err)
	}
	got := body.Invites[0]
	if got.ID != invite.ID || got.WorkspaceID != workspace.ID || got.WorkspaceName != "default" || got.InviterEmail != owner.Email || got.Role != "operator" || got.ExpiresAt.IsZero() {
		t.Fatalf("listed invite %+v", got)
	}
	for _, cookie := range []*http.Cookie{expireeCookie, revokeeCookie, strangerCookie} {
		empty := requestJSON(t, h, http.MethodGet, "/v1/me/invites", nil, cookie)
		requireStatus(t, empty, http.StatusOK)
		if strings.Contains(empty.Body.String(), "winvite_") {
			t.Fatalf("filtered invites listed %s", empty.Body.String())
		}
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+invite.ID+"/accept", map[string]string{}, strangerCookie), http.StatusNotFound)
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/winvite_missing/accept", map[string]string{}, memberCookie), http.StatusNotFound)

	accepted := requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+invite.ID+"/accept", map[string]string{}, memberCookie)
	requireStatus(t, accepted, http.StatusOK)
	var state WorkspaceInvite
	if err := json.Unmarshal(accepted.Body.Bytes(), &state); err != nil || state.State != "accepted" {
		t.Fatalf("accept %+v %v", state, err)
	}
	if role, err := s.Membership(ctx, member.ID, workspace.ID); err != nil || role != "operator" {
		t.Fatalf("accepted membership %q %v", role, err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+invite.ID+"/accept", map[string]string{}, memberCookie), http.StatusOK)
	if members, err := s.WorkspaceMembers(ctx, workspace.ID, owner.ID); err != nil || len(members) != 2 {
		t.Fatalf("accept not idempotent %+v %v", members, err)
	}

	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+expired.ID+"/accept", map[string]string{}, expireeCookie), http.StatusConflict)
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+revoked.ID+"/accept", map[string]string{}, revokeeCookie), http.StatusConflict)

	responded := requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+declined.ID+"/decline", map[string]string{}, declineeCookie)
	requireStatus(t, responded, http.StatusOK)
	if err := json.Unmarshal(responded.Body.Bytes(), &state); err != nil || state.State != "declined" {
		t.Fatalf("decline %+v %v", state, err)
	}
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+declined.ID+"/accept", map[string]string{}, declineeCookie), http.StatusConflict)
	requireStatus(t, requestJSON(t, h, http.MethodPost, "/v1/me/invites/"+declined.ID+"/decline", map[string]string{}, memberCookie), http.StatusNotFound)

	for _, cookie := range []*http.Cookie{memberCookie, declineeCookie} {
		listed = requestJSON(t, h, http.MethodGet, "/v1/me/invites", nil, cookie)
		requireStatus(t, listed, http.StatusOK)
		if strings.Contains(listed.Body.String(), "winvite_inapp") {
			t.Fatalf("consumed invites still listed %s", listed.Body.String())
		}
	}

	pair := workspaceTestAgent(t, s, owner, workspace.ID, "Invitation reader")
	requireStatus(t, requestJSONWithBearer(t, h, "/v1/me/invites", map[string]string{}, pair.AccessToken), http.StatusForbidden)
}
