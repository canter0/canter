package controlplane

import (
	"context"
	"net/http"
	"testing"
)

func TestWorkspaceAgentDefaultsAndIndividualOverrides(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	owner, workspace, human, err := store.Signup(ctx, "defaults@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, otherHuman, err := store.Signup(ctx, "defaults-other@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})
	cookie := &http.Cookie{Name: "canter_session", Value: human}
	read := Authority{Inspect: true, ApplyMode: "never"}
	write := Authority{Inspect: true, Draft: true, ApplyMode: "automatic"}
	path := "/v1/workspaces/" + workspace.ID + "/agent-settings"
	if r := requestJSON(t, handler, http.MethodPatch, path, map[string]any{"authority": read}, &http.Cookie{Name: "canter_session", Value: otherHuman}); r.Code != 403 {
		t.Fatalf("other owner changed defaults: %d", r.Code)
	}
	if r := requestJSON(t, handler, http.MethodPatch, path, map[string]any{"authority": read}, cookie); r.Code != 200 {
		t.Fatalf("save defaults: %d %s", r.Code, r.Body)
	}
	create := func(name string, authority ...Authority) TokenPair {
		invitation, err := store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		device, err := store.ClaimAgentPairing(ctx, invitation.Token, name, "test", "http://canter.test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.ApproveAgentPairing(ctx, invitation.ID, owner.ID, true, authority...); err != nil {
			t.Fatal(err)
		}
		pair, err := store.ExchangeDevice(ctx, device.DeviceCode, name)
		if err != nil {
			t.Fatal(err)
		}
		return pair
	}
	inherited := create("Inherited")
	override := create("Override", read)
	if !inherited.Installation.UseWorkspaceAuthority || inherited.Installation.Authority != read || override.Installation.UseWorkspaceAuthority {
		t.Fatal("pairing did not preserve inheritance choice")
	}
	if r := requestJSON(t, handler, http.MethodPatch, path, map[string]any{"authority": write}, cookie); r.Code != 200 {
		t.Fatalf("change defaults: %d %s", r.Code, r.Body)
	}
	p, err := store.ResolveAgent(ctx, inherited.AccessToken)
	if err != nil || p.Installation.Authority != write {
		t.Fatalf("existing inherited session not updated: %v", err)
	}
	p, err = store.ResolveAgent(ctx, override.AccessToken)
	if err != nil || p.Installation.Authority != read {
		t.Fatalf("override was widened: %v", err)
	}
	patchPath := "/v1/installations/" + override.Installation.ID + "?workspaceId=" + workspace.ID
	if r := requestJSON(t, handler, http.MethodPatch, patchPath, map[string]any{"useWorkspaceAuthority": true}, cookie); r.Code != 200 {
		t.Fatalf("reset to workspace defaults: %d %s", r.Code, r.Body)
	}
	p, err = store.ResolveAgent(ctx, override.AccessToken)
	if err != nil || !p.Installation.UseWorkspaceAuthority || p.Installation.Authority != write {
		t.Fatalf("reset failed: %v", err)
	}
	if r := requestJSON(t, handler, http.MethodPatch, patchPath, map[string]any{"authority": read}, cookie); r.Code != 200 {
		t.Fatalf("set override: %d", r.Code)
	}
	p, err = store.ResolveAgent(ctx, override.AccessToken)
	if err != nil || p.Installation.UseWorkspaceAuthority || p.Installation.Authority != read {
		t.Fatalf("override did not detach: %v", err)
	}
	if r := requestJSONWithBearer(t, handler, path, map[string]any{"authority": write}, inherited.AccessToken); r.Code != 403 {
		t.Fatalf("agent can change workspace defaults: %d", r.Code)
	}
	workspaces, err := store.WorkspacesForAccount(ctx, owner.ID)
	if err != nil || len(workspaces) != 1 || workspaces[0].AgentAuthority != write {
		t.Fatalf("defaults not exposed to workspace UI: %v", err)
	}
}
