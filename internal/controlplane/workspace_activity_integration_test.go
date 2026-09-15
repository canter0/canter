package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestWorkspaceTasksContextAndAgentActivity(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	owner, workspace, session, err := store.Signup(ctx, "task-owner@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	_, otherWorkspace, otherSession, err := store.Signup(ctx, "task-other@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPServer(&Service{Store: store}, HTTPConfig{PublicURL: "http://canter.test"})
	cookie := &http.Cookie{Name: "canter_session", Value: session}
	otherCookie := &http.Cookie{Name: "canter_session", Value: otherSession}
	connect := func(name string, draft bool) TokenPair {
		device, err := store.BeginDeviceAuthorization(ctx, name, "test", Authority{Inspect: true, Draft: draft}, "http://canter.test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.ApproveDevice(ctx, device.UserCode, owner.ID, workspace.ID); err != nil {
			t.Fatal(err)
		}
		pair, err := store.ExchangeDevice(ctx, device.DeviceCode, name)
		if err != nil {
			t.Fatal(err)
		}
		return pair
	}
	agent := connect("Task agent", true)
	observer := connect("Observer", false)
	rival := connect("Other agent", true)
	path := "/v1/workspaces/" + workspace.ID
	input := TaskInput{Prompt: "Review the context without deploying anything.", Model: "glm-5.3-flash", Reasoning: "high", Context: []TaskContext{{Kind: "attachment", Name: "notes.txt", MediaType: "text/plain", DataBase64: base64.StdEncoding.EncodeToString([]byte("private-test-context"))}, {Kind: "repository", Name: "example/repository", URL: "https://example.com/repository"}}}
	created := requestJSON(t, handler, http.MethodPost, path+"/tasks", input, cookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var task WorkspaceTask
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.Model != input.Model || task.Reasoning != "high" || task.Status != "queued" {
		t.Fatalf("task preferences/state lost: %#v", task)
	}
	detail := requestJSON(t, handler, http.MethodGet, path+"/tasks/"+task.ID, nil, cookie)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", detail.Code, detail.Body.String())
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if len(task.Context) != 2 || task.Context[0].DataBase64 != "" || task.Context[0].Size != len("private-test-context") {
		t.Fatalf("context metadata: %#v", task.Context)
	}
	attachmentPath := path + "/tasks/" + task.ID + "/context/" + task.Context[0].ID
	download := requestJSON(t, handler, http.MethodGet, attachmentPath, nil, cookie)
	if download.Code != http.StatusOK || download.Body.String() != "private-test-context" || download.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("attachment download: %d %s", download.Code, download.Body.String())
	}
	for _, url := range []string{path + "/tasks", path + "/tasks/" + task.ID, path + "/activity", attachmentPath} {
		if response := requestJSON(t, handler, http.MethodGet, url, nil, otherCookie); response.Code == http.StatusOK {
			t.Fatalf("cross-workspace read allowed: %s", url)
		}
		if response := requestJSON(t, handler, http.MethodGet, url, nil, nil); response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous read allowed: %s: %d", url, response.Code)
		}
	}
	mcp := func(pair TokenPair, name string, args map[string]any, wantError bool) json.RawMessage {
		t.Helper()
		response := requestJSONWithBearer(t, handler, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}, pair.AccessToken)
		var result struct {
			Result struct {
				IsError bool            `json:"isError"`
				Data    json.RawMessage `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || result.Result.IsError != wantError {
			t.Fatalf("%s error=%v: %d %s", name, wantError, response.Code, response.Body.String())
		}
		return result.Result.Data
	}
	mcp(observer, "canter_claim_task", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID}, true)
	mcp(agent, "canter_claim_task", map[string]any{"workspaceId": otherWorkspace.ID, "taskId": task.ID}, true)
	mcp(agent, "canter_claim_task", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID}, false)
	mcp(rival, "canter_claim_task", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID}, true)
	mcp(agent, "canter_inspect_task", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID}, false)
	content := mcp(agent, "canter_read_task_context", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID, "contextId": task.Context[0].ID}, false)
	if !bytes.Contains(content, []byte(input.Context[0].DataBase64)) {
		t.Fatal("agent cannot retrieve attachment")
	}
	mcp(agent, "canter_whoami", map[string]any{"dataBase64": "secret-argument-must-not-be-logged"}, false)
	mcp(rival, "canter_finish_task", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID, "status": "completed", "result": "not mine"}, true)
	mcp(agent, "canter_finish_task", map[string]any{"workspaceId": workspace.ID, "taskId": task.ID, "status": "completed", "result": "Read context. No deployment performed."}, false)
	done, err := store.WorkspaceTask(ctx, workspace.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" || done.ClaimedBy != agent.Installation.ID {
		t.Fatalf("bad final state: %#v", done)
	}
	actions := requestJSON(t, handler, http.MethodGet, path+"/activity", nil, cookie)
	if actions.Code != http.StatusOK || !bytes.Contains(actions.Body.Bytes(), []byte("canter_whoami")) || !bytes.Contains(actions.Body.Bytes(), []byte(agent.Installation.Name)) {
		t.Fatalf("missing live actions: %s", actions.Body.String())
	}
	for _, secret := range []string{"private-test-context", input.Context[0].DataBase64, "secret-argument-must-not-be-logged"} {
		if bytes.Contains(actions.Body.Bytes(), []byte(secret)) {
			t.Fatal("action feed contains file or request contents")
		}
	}
	var decoded struct {
		Actions []AuditEvent `json:"actions"`
	}
	_ = json.Unmarshal(actions.Body.Bytes(), &decoded)
	found := false
	for _, event := range decoded.Actions {
		if event.Subject == "canter_whoami" && bytes.Contains(event.Metadata, []byte(task.ID)) {
			found = true
		}
	}
	if !found {
		t.Fatal("agent actions were not tied to active task")
	}
	next, err := store.CreateWorkspaceTask(ctx, workspace.ID, owner.ID, TaskInput{Prompt: "Check defaults"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Model != "gpt-5.6-luna" || next.Reasoning != "medium" {
		t.Fatal("default model/reasoning changed")
	}
	badInput := TaskInput{Prompt: "Cross-workspace context", Context: []TaskContext{{Kind: "task", Name: "private task", ReferenceID: next.ID}}}
	if _, err := store.CreateWorkspaceTask(ctx, otherWorkspace.ID, owner.ID, badInput); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace context: %v", err)
	}
	for _, input := range []TaskInput{{Prompt: "Bad model", Model: "fusion"}, {Prompt: "Bad reasoning", Reasoning: "ultra"}, {Prompt: "Bad URL", Context: []TaskContext{{Kind: "repository", Name: "bad", URL: "javascript:alert(1)"}}}} {
		if result := requestJSON(t, handler, http.MethodPost, path+"/tasks", input, cookie); result.Code == http.StatusCreated {
			t.Fatal("invalid context or preference accepted")
		}
	}
	// A second agent request can discover waiting tasks without any conversation history.
	principal, err := store.ResolveAgent(ctx, agent.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := (&Service{Store: store}).Bootstrap(ctx, principal)
	if err != nil || len(bootstrap.Tasks) != 2 {
		t.Fatalf("tasks absent from bootstrap: %v", err)
	}
	// No deployment or engine execution occurred during task lifecycle operations.
	var executions int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM executions`).Scan(&executions); err != nil || executions != 0 {
		t.Fatalf("task mutated execution boundary: %d %v", executions, err)
	}
}

func TestWorkspaceTaskConcurrentClaim(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "claim@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, name := range []string{"first", "second"} {
		device, err := store.BeginDeviceAuthorization(ctx, name, "test", Authority{Inspect: true, Draft: true}, "http://canter.test")
		if err != nil {
			t.Fatal(err)
		}
		installation, err := store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, installation.ID)
	}
	task, err := store.CreateWorkspaceTask(ctx, workspace.ID, account.ID, TaskInput{Prompt: "Claim once"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, id := range ids {
		go func(id string) {
			ready.Done()
			<-start
			_, err := store.ClaimWorkspaceTask(ctx, workspace.ID, task.ID, id)
			results <- err
		}(id)
	}
	ready.Wait()
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful claims=%d", success)
	}
	// Read-only activity scopes must not expose arbitrary future audit metadata.
	if err := store.Audit(ctx, workspace.ID, sdk.ActorRef{Kind: "human", ID: account.ID}, "test", task.ID, map[string]any{"token": "not-for-the-UI", "system": "example"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListWorkspaceActions(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	if bytes.Contains(raw, []byte("not-for-the-UI")) {
		t.Fatal("unapproved metadata exposed")
	}
}
