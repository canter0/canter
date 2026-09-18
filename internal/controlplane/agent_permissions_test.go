package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/canter0/canter/sdk"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentPermissionsPersistAndRestrictExistingSessions(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	owner, workspace, human, err := store.Signup(ctx, "permissions@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, otherWorkspace, otherHuman, err := store.Signup(ctx, "permissions-other@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})
	cookie := &http.Cookie{Name: "canter_session", Value: human}
	write := Authority{Inspect: true, Draft: true, ApplyMode: "automatic"}
	read := Authority{Inspect: true, Draft: false, ApplyMode: "never"}
	invitation, err := store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.ClaimAgentPairing(ctx, invitation.Token, "Permissions test", "test", "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	approved := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings/"+invitation.ID+"/approve", map[string]any{"remember": true, "authority": write}, cookie)
	if approved.Code != 200 {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "test")
	if err != nil {
		t.Fatal(err)
	}
	if pair.Installation.Authority != write {
		t.Fatal("pairing lost requested authority")
	}
	principal, err := store.ResolveAgent(ctx, pair.AccessToken)
	if err != nil || !agentCanApply(principal) {
		t.Fatalf("automatic authority unavailable: %v", err)
	}
	system, err := sdk.NewSystem("permission-api", "permission test").OnHost("c1", 1, 1024, 256).WithM1("systems/permission-api").Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).Build()
	if err != nil {
		t.Fatal(err)
	}
	system, err = canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PutSystem(ctx, workspace.ID, system); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change := sdk.Change{SchemaVersion: "v1", ID: "permission-change", System: system.Metadata.Name, Summary: "test", Phase: "drafted", Digest: strings.Repeat("a", 64), CreatedAt: now, UpdatedAt: now}
	if err = store.RecordChange(ctx, workspace.ID, change); err != nil {
		t.Fatal(err)
	}
	engine := &standingPolicyEngine{changes: map[string]sdk.Change{change.ID: change}}
	applyHandler := &HTTPServer{service: &Service{Store: store, Engine: engine}}
	args, _ := json.Marshal(map[string]string{"workspaceId": workspace.ID, "system": system.Metadata.Name, "changeId": change.ID, "digest": "wrong"})
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, err = applyHandler.callMCPTool(r, principal, "canter_apply_change", args); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	args, _ = json.Marshal(map[string]string{"workspaceId": workspace.ID, "system": system.Metadata.Name, "changeId": change.ID, "digest": change.Digest})
	if _, err = applyHandler.callMCPTool(r, principal, "canter_apply_change", args); err != nil {
		t.Fatalf("write agent cannot apply exact change: %v", err)
	}
	execution, err := store.ExecutionForChange(ctx, workspace.ID, system.Metadata.Name, change.ID)
	if err != nil || execution.RequestedBy.ID != principal.Actor.ID {
		t.Fatalf("missing attributed execution: %v", err)
	}

	worker, err := store.CreateAgentWorker(ctx, principal, WorkerInput{Name: "Worker", ClientInstance: "worker", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	workerPrincipal, err := store.ResolveAgent(ctx, worker.AccessToken)
	if err != nil || agentCanApply(workerPrincipal) {
		t.Fatalf("worker inherited automatic deployment: %v", err)
	}
	path := "/v1/installations/" + pair.Installation.ID + "?workspaceId=" + workspace.ID
	if r := requestJSON(t, handler, http.MethodPatch, path, map[string]any{"authority": read}, &http.Cookie{Name: "canter_session", Value: otherHuman}); r.Code != 403 {
		t.Fatalf("other owner changed grant: %d", r.Code)
	}
	if r := requestJSON(t, handler, http.MethodPatch, "/v1/installations/"+pair.Installation.ID+"?workspaceId="+otherWorkspace.ID, map[string]any{"authority": read}, &http.Cookie{Name: "canter_session", Value: otherHuman}); r.Code != 404 {
		t.Fatalf("cross-workspace update: %d", r.Code)
	}
	if r := requestJSON(t, handler, http.MethodPatch, path, map[string]any{"authority": Authority{Inspect: true, ApplyMode: "automatic"}}, cookie); r.Code != 400 {
		t.Fatalf("invalid read grant: %d", r.Code)
	}
	saved := requestJSON(t, handler, http.MethodPatch, path, map[string]any{"authority": read}, cookie)
	if saved.Code != 200 {
		t.Fatalf("save: %d %s", saved.Code, saved.Body)
	}
	for _, token := range []string{pair.AccessToken, worker.AccessToken} {
		p, err := store.ResolveAgent(ctx, token)
		if err != nil || p.Installation.Authority != read || agentCanApply(p) {
			t.Fatalf("existing session retained writes: %+v %v", p.Installation, err)
		}
		h := &HTTPServer{service: &Service{Store: store}}
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if err := h.allowWorkspace(r, p, workspace.ID, true); !errors.Is(err, ErrForbidden) {
			t.Fatal("read agent can write")
		}
		if err := h.allowWorkspace(r, p, workspace.ID, false); err != nil {
			t.Fatal("read agent cannot read")
		}
		for _, name := range []string{"canter_apply_change", "canter_apply_initial_deployment"} {
			args, _ := json.Marshal(map[string]string{"workspaceId": workspace.ID})
			if _, err := h.callMCPTool(r, p, name, args); !errors.Is(err, ErrForbidden) {
				t.Fatalf("read agent can use %s: %v", name, err)
			}
		}
		for _, suffix := range []string{"systems/test/changes/test/authorize", "systems/test/changes/test/apply", "initial-deployments/test/authorize", "initial-deployments/test/apply"} {
			response := requestJSONWithBearer(t, handler, "/v1/workspaces/"+workspace.ID+"/"+suffix, map[string]string{"digest": "test"}, token)
			if response.Code != 403 {
				t.Fatalf("read apply %s: %d", suffix, response.Code)
			}
		}
	}
	// A pairing can start read-only without briefly issuing broader credentials.
	invitation, err = store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	device, err = store.ClaimAgentPairing(ctx, invitation.Token, "Reader", "test", "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	approved = requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings/"+invitation.ID+"/approve", map[string]any{"authority": read}, cookie)
	if approved.Code != 200 {
		t.Fatalf("read approve: %d %s", approved.Code, approved.Body)
	}
	pair, err = store.ExchangeDevice(ctx, device.DeviceCode, "reader")
	if err != nil || pair.Installation.Authority != read {
		t.Fatalf("read pairing: %v", err)
	}
}
