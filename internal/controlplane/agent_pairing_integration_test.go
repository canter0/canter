package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentPairingOwnershipAndWorkerLifecycle(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	owner, workspace, human, err := store.Signup(ctx, "pair-owner@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, otherWorkspace, otherHuman, err := store.Signup(ctx, "pair-other@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})
	cookie := &http.Cookie{Name: "canter_session", Value: human}
	otherCookie := &http.Cookie{Name: "canter_session", Value: otherHuman}
	create := func(remember bool) (AgentPairing, DeviceAuthorization, TokenPair) {
		response := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings", map[string]any{"workspaceId": workspace.ID}, cookie)
		if response.Code != 201 {
			t.Fatalf("create: %d %s", response.Code, response.Body)
		}
		var invitation AgentPairing
		if err := json.Unmarshal(response.Body.Bytes(), &invitation); err != nil {
			t.Fatal(err)
		}
		claim := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings/claim", map[string]any{"token": invitation.Token, "name": "Test orchestrator", "harness": "test"}, nil)
		if claim.Code != 200 {
			t.Fatalf("claim: %d %s", claim.Code, claim.Body)
		}
		var device DeviceAuthorization
		json.Unmarshal(claim.Body.Bytes(), &device)
		if _, err := store.ExchangeDevice(ctx, device.DeviceCode, "before-approval"); !errors.Is(err, ErrDevicePending) {
			t.Fatalf("unapproved exchange: %v", err)
		}
		if _, err := store.ApproveDevice(ctx, device.UserCode, owner.ID, workspace.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("legacy approval bypass: %v", err)
		}
		if err := store.DenyDevice(ctx, device.UserCode, owner.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("legacy cancellation bypass: %v", err)
		}
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			if got := requestJSON(t, handler, method, "/v1/agent-pairings/"+invitation.ID, nil, otherCookie); got.Code != 404 {
				t.Fatalf("other account %s: %d", method, got.Code)
			}
		}
		if got := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings/"+invitation.ID+"/approve", map[string]any{"remember": true}, otherCookie); got.Code != 404 {
			t.Fatalf("other account approval: %d", got.Code)
		}
		if got := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings/claim", map[string]any{"token": invitation.Token, "name": "Replay", "harness": "test"}, nil); got.Code != 409 {
			t.Fatalf("invitation reuse: %d", got.Code)
		}
		read := requestJSON(t, handler, http.MethodGet, "/v1/agent-pairings/"+invitation.ID, nil, cookie)
		if strings.Contains(read.Body.String(), invitation.Token) || strings.Contains(read.Body.String(), device.DeviceCode) {
			t.Fatal("browser status exposes secret")
		}
		attack := httptest.NewRequest(http.MethodPost, "http://canter.test/v1/agent-pairings/"+invitation.ID+"/approve", strings.NewReader(`{"remember":true}`))
		attack.AddCookie(cookie)
		attack.Header.Set("Origin", "https://attacker.invalid")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, attack)
		if rr.Code != 403 {
			t.Fatalf("cross-origin approval: %d", rr.Code)
		}
		approve := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings/"+invitation.ID+"/approve", map[string]any{"remember": remember}, cookie)
		if approve.Code != 200 {
			t.Fatalf("approve: %d %s", approve.Code, approve.Body)
		}
		pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "orchestrator-session")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ExchangeDevice(ctx, device.DeviceCode, "replay"); !errors.Is(err, ErrConflict) {
			t.Fatalf("device replay: %v", err)
		}
		state, err := store.AgentPairing(ctx, invitation.ID, owner.ID)
		if err != nil || state.Status != "connected" {
			t.Fatalf("connected: %+v %v", state, err)
		}
		return invitation, device, pair
	}
	if r := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings", map[string]any{"workspaceId": workspace.ID}, nil); r.Code != 401 {
		t.Fatalf("anonymous invitation: %d", r.Code)
	}
	if r := requestJSON(t, handler, http.MethodPost, "/v1/agent-pairings", map[string]any{"workspaceId": otherWorkspace.ID}, cookie); r.Code != 403 {
		t.Fatalf("wrong workspace invitation: %d", r.Code)
	}
	invitation, _, pair := create(false)
	if pair.Installation.ExpiresAt == nil || pair.Installation.ExpiresAt.After(store.now().Add(8*time.Hour)) {
		t.Fatal("temporary connection has no hard expiry")
	}
	principal, err := store.ResolveAgent(ctx, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Installation.LastSeenAt == nil {
		t.Fatal("first agent request was not marked seen")
	}
	worker, err := store.CreateAgentWorker(ctx, principal, WorkerInput{Name: "Reader", ClientInstance: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if worker.ExpiresAt.After(pair.Session.ExpiresAt) {
		t.Fatal("worker outlives parent")
	}
	installations, err := store.ListInstallations(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(installations) != 1 || installations[0].ActiveSessions != 2 {
		t.Fatal("active parent and worker sessions were not counted")
	}
	wp, err := store.ResolveAgent(ctx, worker.AccessToken)
	if err != nil || wp.Installation.Authority.Draft {
		t.Fatalf("worker privileges: %v", err)
	}
	if _, err := store.CreateAgentWorker(ctx, wp, WorkerInput{Name: "Grandchild", ClientInstance: "worker-2"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker delegation: %v", err)
	}
	task, err := store.CreateWorkspaceTask(ctx, workspace.ID, owner.ID, TaskInput{Prompt: "Inspect only."})
	if err != nil {
		t.Fatal(err)
	}
	server := handler.(*HTTPServer)
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err := server.changeTask(request, wp, workspace.ID, task.ID, "working", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker task ownership: %v", err)
	}
	if _, err = store.ClaimWorkspaceTask(ctx, workspace.ID, task.ID, pair.Installation.ID); err != nil {
		t.Fatal(err)
	}
	readWorker := requestJSONWithBearer(t, handler, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "canter_bootstrap", "arguments": map[string]any{}}}, worker.AccessToken)
	if readWorker.Code != 200 || strings.Contains(readWorker.Body.String(), `"isError":true`) {
		t.Fatalf("worker read: %s", readWorker.Body)
	}
	actions, err := store.ListWorkspaceActions(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range actions {
		if a.Actor.SessionID == worker.Session.ID {
			found = true
			if a.Actor.DisplayName != "Reader · Test orchestrator" {
				t.Fatalf("worker attribution: %q", a.Actor.DisplayName)
			}
		}
	}
	if !found {
		t.Fatal("worker action not recorded")
	}
	raw, _ := json.Marshal(actions)
	for _, secret := range []string{invitation.Token, pair.AccessToken, pair.RefreshToken, worker.AccessToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential in activity")
		}
	}
	draftingWorker, err := store.CreateAgentWorker(ctx, principal, WorkerInput{Name: "Drafter", ClientInstance: "worker-3", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	dp, err := store.ResolveAgent(ctx, draftingWorker.AccessToken)
	if err != nil || !dp.Installation.Authority.Draft {
		t.Fatalf("bounded draft worker: %v", err)
	}
	if _, err := server.changeTask(request, dp, workspace.ID, task.ID, "completed", "Done"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("worker terminated parent task: %v", err)
	}
	if _, err = store.FinishWorkspaceTask(ctx, workspace.ID, task.ID, pair.Installation.ID, "completed", "Inspected without changes."); err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{pair.AccessToken, worker.AccessToken, draftingWorker.AccessToken} {
		if _, err := store.ResolveAgent(ctx, access); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("temporary credential survived completion: %v", err)
		}
	}
	if _, err := store.RefreshAgent(ctx, pair.RefreshToken, "after-completion"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("temporary refresh survived: %v", err)
	}
	_, _, remembered := create(true)
	if remembered.Installation.ExpiresAt != nil {
		t.Fatal("remembered connection unexpectedly temporary")
	}
	rememberedTask, err := store.CreateWorkspaceTask(ctx, workspace.ID, owner.ID, TaskInput{Prompt: "Remembered task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimWorkspaceTask(ctx, workspace.ID, rememberedTask.ID, remembered.Installation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.FinishWorkspaceTask(ctx, workspace.ID, rememberedTask.ID, remembered.Installation.ID, "completed", "Done"); err != nil {
		t.Fatal(err)
	}
	rp, err := store.ResolveAgent(ctx, remembered.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateAgentWorker(ctx, rp, WorkerInput{Name: "Remembered worker", ClientInstance: "worker-4"})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := store.RefreshAgent(ctx, remembered.RefreshToken, "next-conversation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveAgent(ctx, child.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("worker survived parent session end: %v", err)
	}
	rotatedPrincipal, err := store.ResolveAgent(ctx, rotated.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DisconnectAgent(ctx, rotatedPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveAgent(ctx, rotated.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("disconnected session still authorized")
	}
	if _, err := store.RefreshAgent(ctx, rotated.RefreshToken, "remembered-return"); err != nil {
		t.Fatalf("remembered agent cannot return: %v", err)
	}
}

func TestAgentPairingExpiryCancellationAndClaimRace(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	owner, workspace, _, err := store.Signup(ctx, "pair-expiry@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			defer wg.Done()
			_, err := store.ClaimAgentPairing(ctx, invite.Token, "Racing agent", "test", "http://canter.test")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("claim race: %d wins, %d conflicts", successes, conflicts)
	}
	if err := store.CancelAgentPairing(ctx, invite.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveAgentPairing(ctx, invite.ID, owner.ID, true); !errors.Is(err, ErrDeviceDenied) {
		t.Fatalf("cancelled approval: %v", err)
	}
	fresh, err := store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	if _, err := store.ClaimAgentPairing(ctx, fresh.Token, "Late agent", "test", "http://canter.test"); !errors.Is(err, ErrDeviceExpired) {
		t.Fatalf("expired claim: %v", err)
	}
	temp, err := store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.ClaimAgentPairing(ctx, temp.Token, "Temporary agent", "test", "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApproveAgentPairing(ctx, temp.ID, owner.ID, false); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "temporary")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(8*time.Hour + time.Second)
	if _, err := store.RefreshAgent(ctx, pair.RefreshToken, "too-late"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("hard expiry bypass: %v", err)
	}
	if _, err := store.ResolveAgent(ctx, pair.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("hard expiry access: %v", err)
	}
}

func TestCancellingApprovedPairingRevokesIssuedCredentials(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	owner, workspace, _, err := store.Signup(ctx, "cancel-approved@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := store.CreateAgentPairing(ctx, owner.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.ClaimAgentPairing(ctx, invitation.Token, "Cancelled agent", "test", "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApproveAgentPairing(ctx, invitation.ID, owner.ID, true); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "cancel-session")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CancelAgentPairing(ctx, invitation.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ResolveAgent(ctx, pair.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("access survived cancellation: %v", err)
	}
	if _, err = store.RefreshAgent(ctx, pair.RefreshToken, "cancel-refresh"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh survived cancellation: %v", err)
	}
}
