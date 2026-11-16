package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func runtimeToolSet() []mcpTool {
	v := wrenComputerTools()
	v = append(v, wrenJobTools()...)
	return append(v, wrenMediaTools()...)
}
func runtimeToolNamed(tools []mcpTool, name string) (mcpTool, bool) {
	for _, v := range tools {
		if v.Name == name {
			return v, true
		}
	}
	return mcpTool{}, false
}
func requireRuntimeTool(t *testing.T, tools []mcpTool, name string, present bool) mcpTool {
	t.Helper()
	v, ok := runtimeToolNamed(tools, name)
	if ok != present {
		t.Fatalf("tool %s present%v want%v", name, ok, present)
	}
	return v
}
func runtimeToolJSON(v mcpTool) string { b, _ := json.Marshal(v.InputSchema); return string(b) }
func TestWrenRuntimeFreshConversationHasInlineArtifactsNotUnavailableComputer(t *testing.T) {
	h, c, _, _ := computerFixture(t)
	o := OperatorRuntime{Server: h}
	if note, err := o.wrenComputerRuntimeNotice(context.Background(), c, ""); err != nil || !strings.Contains(note, "no ready computer") {
		t.Fatalf("missing computer must not fail ordinary chat: %q %v", note, err)
	}
	base := runtimeToolSet()
	tools, e := o.wrenConversationTools(context.Background(), c, base)
	if e != nil {
		t.Fatal(e)
	}
	requireRuntimeTool(t, tools, "canter_computer_status", true)
	for _, name := range []string{"canter_computer_write", "canter_computer_run", "canter_computer_read", "canter_computer_screenshot", "canter_computer_import_attachment"} {
		requireRuntimeTool(t, tools, name, false)
	}
	publish := requireRuntimeTool(t, tools, "canter_publish_artifact", true)
	if strings.Contains(runtimeToolJSON(publish), `"path"`) || !strings.Contains(runtimeToolJSON(publish), `"content"`) || !strings.Contains(publish.Description, "UTF-8") {
		t.Fatalf("bad inline artifact affordance %s", runtimeToolJSON(publish))
	}
	delegate := requireRuntimeTool(t, tools, "canter_delegate_job", true)
	if strings.Contains(runtimeToolJSON(delegate), "acp-opencode") || !strings.Contains(runtimeToolJSON(delegate), "operator") {
		t.Fatal("unavailable ACP advertised")
	}
	original, _ := runtimeToolNamed(base, "canter_publish_artifact")
	if !strings.Contains(runtimeToolJSON(original), `"path"`) {
		t.Fatal("filter mutated shared schema")
	}
	original, _ = runtimeToolNamed(base, "canter_delegate_job")
	if !strings.Contains(runtimeToolJSON(original), "acp-opencode") {
		t.Fatal("filter mutated shared executor schema")
	}
}
func TestWrenRuntimeReadyComputerAndHumanTakeoverCatalog(t *testing.T) {
	h, c, _, _ := computerFixture(t)
	ctx := context.Background()
	wren, createErr := h.service.Store.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_catalog_ready", Name: "Catalog", Shape: "circle"}, true)
	if createErr != nil {
		t.Fatal(createErr)
	}
	c, createErr = h.service.Store.Conversation(ctx, c.WorkspaceID, c.AccountID, wren.ConversationID)
	if createErr != nil {
		t.Fatal(createErr)
	}
	h.config.Operator = OperatorConfig{APIKey: "test", Model: "test", BaseURL: "https://provider.example"}
	h.config.Harnesses.BrokerBaseURL = "https://broker.example/v1/wren-model"
	computer, e := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	computer = readyComputer(t, h, computer)
	o := OperatorRuntime{Server: h}
	tools, e := o.wrenConversationTools(ctx, c, runtimeToolSet())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"canter_computer_write", "canter_computer_run", "canter_computer_read", "canter_computer_screenshot"} {
		requireRuntimeTool(t, tools, name, true)
	}
	delegate := requireRuntimeTool(t, tools, "canter_delegate_job", true)
	if !strings.Contains(runtimeToolJSON(delegate), "acp-opencode") {
		t.Fatal("ready authorized ACP omitted")
	}
	publish := requireRuntimeTool(t, tools, "canter_publish_artifact", true)
	if !strings.Contains(runtimeToolJSON(publish), `"path"`) {
		t.Fatal("actual guest file publishing omitted")
	}
	if _, e = h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, computer.ID, "human"); e != nil {
		t.Fatal(e)
	}
	tools, e = o.wrenConversationTools(ctx, c, runtimeToolSet())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"canter_computer_write", "canter_computer_run", "canter_computer_input", "canter_computer_stop", "canter_computer_import_artifact"} {
		requireRuntimeTool(t, tools, name, false)
	}
	for _, name := range []string{"canter_computer_status", "canter_computer_read", "canter_computer_files", "canter_computer_job", "canter_computer_screenshot", "canter_publish_artifact"} {
		requireRuntimeTool(t, tools, name, true)
	}
	delegate, _ = runtimeToolNamed(tools, "canter_delegate_job")
	if strings.Contains(runtimeToolJSON(delegate), "acp-opencode") {
		t.Fatal("takeover advertised ACP mutation")
	}
	h.config.Harnesses.BrokerBaseURL = ""
	h.SetComputerControl(ctx, c.WorkspaceID, c.AccountID, computer.ID, "agent")
	tools, e = o.wrenConversationTools(ctx, c, runtimeToolSet())
	if e != nil {
		t.Fatal(e)
	}
	delegate, _ = runtimeToolNamed(tools, "canter_delegate_job")
	if strings.Contains(runtimeToolJSON(delegate), "acp-opencode") {
		t.Fatal("unconfigured harness advertised")
	}
}
func TestWrenRuntimeChildComputerUsesOnlyActualOriginGrant(t *testing.T) {
	s, c, parent := wrenJobFixture(t)
	ctx := context.Background()
	h := NewHTTPServer(&Service{Store: s, Engine: &computerFakeEngine{}}, HTTPConfig{Secrets: testVault(t), Computers: WrenComputerConfig{SSHSourceCIDR: "203.0.113.8/32", AllowedWorkspaces: []string{c.WorkspaceID}}}).(*HTTPServer)
	sibling, e := s.SaveWren(ctx, c.WorkspaceID, c.AccountID, Wren{ID: "wren_sibling_computer", Name: "Sibling", Shape: "circle"}, true)
	if e != nil {
		t.Fatal(e)
	}
	other, e := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, sibling.ConversationID)
	if e != nil {
		t.Fatal(e)
	}
	readyComputer(t, h, other)
	job := requireWrenJob(t, s, c, parent, "catalog-child")
	child, e := s.Conversation(ctx, c.WorkspaceID, c.AccountID, job.ChildConversationID)
	if e != nil {
		t.Fatal(e)
	}
	o := OperatorRuntime{Server: h}
	tools, e := o.wrenConversationTools(ctx, child, runtimeToolSet())
	if e != nil {
		t.Fatal(e)
	}
	requireRuntimeTool(t, tools, "canter_computer_write", false)
	requireRuntimeTool(t, tools, "canter_generate_image", false)
	requireRuntimeTool(t, tools, "canter_publish_artifact", true)
	if note, err := o.wrenComputerRuntimeNotice(ctx, child, parent.ID); err != nil || strings.Contains(note, other.ID) || !strings.Contains(note, "no ready computer") {
		t.Fatalf("child borrowed sibling computer context: %q %v", note, err)
	}
	own, e := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	readyComputer(t, h, own)
	tools, e = o.wrenConversationTools(ctx, child, runtimeToolSet())
	if e != nil {
		t.Fatal(e)
	}
	requireRuntimeTool(t, tools, "canter_computer_write", true)
	requireRuntimeTool(t, tools, "canter_computer_read", true)
	requireRuntimeTool(t, tools, "canter_generate_image", false)
	if note, err := o.wrenComputerRuntimeNotice(ctx, child, parent.ID); err != nil || !strings.Contains(note, own.ID) || strings.Contains(note, other.ID) {
		t.Fatalf("child did not use its actual origin: %q %v", note, err)
	}
	if _, e = s.CancelWrenJob(ctx, c.WorkspaceID, c.AccountID, job.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = o.wrenConversationTools(ctx, child, runtimeToolSet()); e == nil {
		t.Fatal("cancelled child reused origin grant")
	}
	if _, err := o.wrenComputerRuntimeNotice(ctx, child, parent.ID); err == nil {
		t.Fatal("cancelled child retained computer context")
	}
}

func TestWrenRuntimeComputerContextRefreshesAfterReplacement(t *testing.T) {
	h, c, old := operationComputer(t)
	ctx := context.Background()
	o := OperatorRuntime{Server: h}
	note, err := o.wrenComputerRuntimeNotice(ctx, c, "")
	if err != nil || !strings.Contains(note, old.ID) || !strings.Contains(note, old.CreatedAt.UTC().Format(time.RFC3339Nano)) || !strings.Contains(note, `"restoredSavedFiles":false`) {
		t.Fatalf("missing live identity: %q %v", note, err)
	}
	if _, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET expires_at=now()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	note, err = o.wrenComputerRuntimeNotice(ctx, c, "")
	if err != nil || strings.Contains(note, old.ID) || !strings.Contains(note, "no ready computer") {
		t.Fatalf("expired computer advertised as current: %q %v", note, err)
	}
	if _, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET phase='deleted' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	next, err := h.EnableWrenComputer(ctx, c.WorkspaceID, c.AccountID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	note, err = o.wrenComputerRuntimeNotice(ctx, c, "")
	if err != nil || strings.Contains(note, next.ID) || !strings.Contains(note, "no ready computer") {
		t.Fatalf("provisioning computer advertised as ready: %q %v", note, err)
	}
	readyComputer(t, h, next)
	if _, err = h.service.Store.pool.Exec(ctx, `UPDATE wren_computers SET restore_checkpoint_id='checkpoint_fixture' WHERE id=$1`, next.ID); err != nil {
		t.Fatal(err)
	}
	note, err = o.wrenComputerRuntimeNotice(ctx, c, "")
	if err != nil || !strings.Contains(note, next.ID) || strings.Contains(note, old.ID) || !strings.Contains(note, `"restoredSavedFiles":true`) || !strings.Contains(note, "do not resume running processes") {
		t.Fatalf("stale replacement context: %q %v", note, err)
	}
}

func TestWrenRuntimeDoesNotOfferUnclaimableExternalTaskQueue(t *testing.T) {
	h, c, _, _ := computerFixture(t)
	o := OperatorRuntime{Server: h}
	ctx := context.Background()
	base := append(runtimeToolSet(), mcpTool{Name: "canter_create_task"})
	tools, err := o.wrenConversationTools(ctx, c, base)
	if err != nil {
		t.Fatal(err)
	}
	requireRuntimeTool(t, tools, "canter_create_task", false)
	if _, err = h.service.Store.pool.Exec(ctx, `INSERT INTO agent_installations(id,workspace_id,name,harness,created_by) VALUES('external_queue_fixture',$1,'External fixture','codex',$2)`, c.WorkspaceID, c.AccountID); err != nil {
		t.Fatal(err)
	}
	tools, err = o.wrenConversationTools(ctx, c, base)
	if err != nil {
		t.Fatal(err)
	}
	requireRuntimeTool(t, tools, "canter_create_task", true)
	if _, err = h.service.Store.pool.Exec(ctx, `UPDATE agent_installations SET revoked_at=now() WHERE id='external_queue_fixture'`); err != nil {
		t.Fatal(err)
	}
	tools, err = o.wrenConversationTools(ctx, c, base)
	if err != nil {
		t.Fatal(err)
	}
	requireRuntimeTool(t, tools, "canter_create_task", false)
}

func TestWrenComputerJobLookupExplainsDelegatedWorkerIdentity(t *testing.T) {
	h, c, _, _ := computerFixture(t)
	o := OperatorRuntime{Server: h}
	for name, want := range map[string]string{"canter_computer_job": "canter_inspect_job", "canter_computer_stop": "canter_cancel_job"} {
		_, handled, err := o.wrenComputerTool(context.Background(), OperatorRun{}, c, name, json.RawMessage(`{"id":"job_existing_worker"}`))
		if !handled || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("misrouted identity has no correction: %v %v", handled, err)
		}
	}
}
