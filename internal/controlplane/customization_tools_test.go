package controlplane

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func customizationTestRun(t *testing.T, s *Store, c Conversation) OperatorRun {
	t.Helper()
	ctx := context.Background()
	if _, err := s.EnqueueOperator(ctx, c, "customization-test-request", "Set up my customizations", "test", &OperatorSurface{Kind: "customization", ID: "rule"}); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return run
}

func TestAgentCustomizationSaveReplayAndAuthority(t *testing.T) {
	h, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19994")
	s.SetWrensEnabled(false)
	ctx := context.Background()
	run := customizationTestRun(t, s, c)
	runtime := &OperatorRuntime{Server: h}
	principal, err := s.operatorPrincipal(ctx, c, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	call := operatorTestCall("canter_manage_customizations", `{"operation":"save","kind":"rule","name":"Concise","body":"Use concise paragraphs.","enabled":true}`)
	call.ID = "save_rule"
	result, err := runtime.executeTool(ctx, run, c, principal, call)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Saved bool                  `json:"saved"`
		Item  personalCustomization `json:"item"`
	}
	if json.Unmarshal(result, &saved) != nil || !saved.Saved || saved.Item.ID == "" {
		t.Fatalf("not saved: %s", result)
	}
	again, err := runtime.executeTool(ctx, run, c, principal, call)
	var replay struct {
		Saved bool                  `json:"saved"`
		Item  personalCustomization `json:"item"`
	}
	if err != nil || json.Unmarshal(again, &replay) != nil || !replay.Saved || replay.Item != saved.Item {
		t.Fatalf("tool replay: %s %v", again, err)
	}
	items, err := s.personalCustomizations(ctx, "ws", "owner")
	if err != nil || len(items) != 1 {
		t.Fatalf("duplicate config: %d %v", len(items), err)
	}
	text, err := s.customizationContext(ctx, c, run)
	if err != nil || !strings.Contains(text, "Use concise paragraphs.") {
		t.Fatalf("saved rule not loaded: %s %v", text, err)
	}
	forged := c
	forged.AccountID = "member"
	if _, _, err = runtime.customizationTool(ctx, forged, run, "canter_manage_customizations", json.RawMessage(`{"operation":"list"}`)); err == nil {
		t.Fatal("another person used the run's authority")
	}
	stale := run
	stale.Lease = "stale"
	item := personalCustomization{Kind: "rule", Name: "Stale", Body: "Must not save.", Enabled: true}
	if _, err = s.writeAgentCustomization(ctx, c, stale, item, "save", "stale-op", nil); err == nil {
		t.Fatal("expired executor saved config")
	}
	changed := saved.Item
	changed.Body = "Use bullet points."
	if _, err = s.writeAgentCustomization(ctx, c, run, changed, "save", "edit-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeAgentCustomization(ctx, c, run, changed, "save", "edit-2", nil); err == nil {
		t.Fatal("stale revision overwrote new instructions")
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO conversation_shares(conversation_id,workspace_id,shared_by) VALUES($1,'ws','owner')`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = runtime.customizationTool(ctx, c, run, "canter_manage_customizations", json.RawMessage(`{"operation":"list"}`)); err == nil {
		t.Fatal("shared chat inherited personal customizations")
	}
}

func TestPersonalHookReceiptsAndReplay(t *testing.T) {
	_, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19995")
	ctx := context.Background()
	run := customizationTestRun(t, s, c)
	after := personalCustomization{Kind: "hook", Name: "Database follow-up", Body: "Summarize the query result.", Event: "PostToolUse", Matcher: "database_query$", Action: "agent", Enabled: true}
	after, err := s.writeAgentCustomization(ctx, c, run, after, "save", "post-hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	fail := personalCustomization{Kind: "hook", Name: "Database failure", Body: "Explain the failure.", Event: "ToolFailure", Matcher: "database_query$", Action: "agent", Enabled: true}
	if _, err = s.writeAgentCustomization(ctx, c, run, fail, "save", "failure-hook", nil); err != nil {
		t.Fatal(err)
	}
	call := operatorTestCall("wext_fixturev1_database_query", `{}`)
	call.ID = "database_call"
	receipt := json.RawMessage(`{"status":"completed","toolName":"database_query","result":"` + strings.Repeat("x", 15000) + `"}`)
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result) VALUES($1,$2,$3,'{}',$4)`, run.ID, call.ID, call.Function.Name, receipt); err != nil {
		t.Fatal(err)
	}
	wrapped, err := s.customizationToolResult(ctx, c, run, call, operatorResultForModel(run.ID, call.ID, receipt))
	if err != nil || !bytes.Contains(wrapped, []byte("Summarize the query result.")) {
		t.Fatalf("hook did not fire from long receipt: %s %v", wrapped, err)
	}
	again, err := s.customizationToolResult(ctx, c, run, call, operatorResultForModel(run.ID, call.ID, receipt))
	if err != nil || !bytes.Equal(again, wrapped) {
		t.Fatalf("unstable replay: %v", err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM personal_hook_firings WHERE run_id=$1`, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate firing: %d %v", count, err)
	}
	missing := call
	missing.ID = "no_receipt"
	if value, err := s.customizationToolResult(ctx, c, run, missing, receipt); err != nil || !bytes.Equal(value, receipt) {
		t.Fatal("a fabricated receipt triggered a hook")
	}
	call.ID = "failed_query"
	receipt = json.RawMessage(`{"status":"failed","toolName":"database_query"}`)
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result) VALUES($1,$2,$3,'{}',$4)`, run.ID, call.ID, call.Function.Name, receipt); err != nil {
		t.Fatal(err)
	}
	if wrapped, err = s.customizationToolResult(ctx, c, run, call, receipt); err != nil || !bytes.Contains(wrapped, []byte("Explain the failure.")) || bytes.Contains(wrapped, []byte("Summarize the query result.")) {
		t.Fatalf("wrong failure hook: %s %v", wrapped, err)
	}
	after.Enabled = false
	if _, err = s.writeAgentCustomization(ctx, c, run, after, "save", "disable-hook", nil); err != nil {
		t.Fatal(err)
	}
	call.ID = "database_call"
	if wrapped, err = s.customizationToolResult(ctx, c, run, call, receipt); err != nil || bytes.Contains(wrapped, []byte("Summarize the query result.")) {
		t.Fatal("disabled hook survived replay")
	}
}

func skillArchiveFixture(t *testing.T, files map[string][]byte, link bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: "repo-sha/skills/test-skill/" + name, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if link {
		if err := writer.WriteHeader(&tar.Header{Name: "repo-sha/skills/test-skill/escape", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestSkillBundleFilesAndPersonalLoading(t *testing.T) {
	files := map[string][]byte{"SKILL.md": []byte("---\nname: test-skill\ndescription: Test skill with resources.\n---\nRead references/guide.md before using scripts/check.py."), "references/guide.md": []byte("Instructions with a resource."), "scripts/check.py": []byte("print('fixture')"), "assets/sample.bin": {0, 1, 2, 255}}
	raw := skillArchiveFixture(t, files, false)
	bundle, err := readSkillArchive(bytes.NewReader(raw), "test-skill")
	if err != nil || len(bundle.Files) != 4 {
		t.Fatalf("bundle lost resources: %d %v", len(bundle.Files), err)
	}
	if _, err = readSkillArchive(bytes.NewReader(skillArchiveFixture(t, files, true)), "test-skill"); err == nil {
		t.Fatal("unsafe bundle was partially imported")
	}
	_, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19996")
	ctx := context.Background()
	run := customizationTestRun(t, s, c)
	item, err := s.writeAgentCustomization(ctx, c, run, personalCustomization{Kind: "skill", Name: bundle.Name, Description: bundle.Description, Body: string(bundle.Files["SKILL.md"]), Source: "fixture/skills", SourceRevision: strings.Repeat("a", 40), Enabled: true}, "save", "install-skill", bundle.Files)
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.customizationContext(ctx, c, run)
	if err != nil || !strings.Contains(text, item.ID) || strings.Contains(text, "Read references/guide.md") {
		t.Fatalf("skill should be discoverable without loading its full contents: %s %v", text, err)
	}
	file, err := s.readPersonalSkillFile(ctx, c, item.ID, "references/guide.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(file)
	if !bytes.Contains(encoded, files["references/guide.md"]) {
		t.Fatal("reference file missing")
	}
	file, err = s.readPersonalSkillFile(ctx, c, item.ID, "assets/sample.bin", 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(file)
	if !bytes.Contains(encoded, []byte("AAEC/w==")) {
		t.Fatalf("binary resource lost: %s", encoded)
	}
	other := c
	other.AccountID = "member"
	if _, err = s.readPersonalSkillFile(ctx, other, item.ID, "SKILL.md", 0); err == nil {
		t.Fatal("other person read private skill")
	}
	item.Enabled = false
	if _, err = s.writeAgentCustomization(ctx, c, run, item, "save", "disable-skill", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readPersonalSkillFile(ctx, c, item.ID, "SKILL.md", 0); err == nil {
		t.Fatal("disabled skill remained readable")
	}
}

func TestLiveSkillsDirectoryAndBundle(t *testing.T) {
	if os.Getenv("CANTER_TEST_LIVE_SKILLS") != "1" {
		t.Skip("set CANTER_TEST_LIVE_SKILLS=1 for public directory/source reads")
	}
	result, err := searchSkillsDirectory(context.Background(), "postgres")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte("supabase/agent-skills")) {
		t.Fatalf("expected public search result: %s", encoded)
	}
	bundle, err := fetchSkillBundle(context.Background(), "supabase/agent-skills", "supabase-postgres-best-practices", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Files) < 2 || !pluginRevisionPattern.MatchString(bundle.Commit) {
		t.Fatal("bundle lacked pinned supporting files")
	}
	t.Log(fmt.Sprintf("read %d files at %s", len(bundle.Files), bundle.Commit))
}

func TestApprovedActionHookContinuationIsPersonalAndDeduplicated(t *testing.T) {
	h, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19997")
	ctx := context.Background()
	run := customizationTestRun(t, s, c)
	extension := operatorConnectionExtension(t, s, "http://127.0.0.1:19997", "none", operatorConnectionTools("http://127.0.0.1:19997"))
	install, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_hook_review", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	hook := personalCustomization{Kind: "hook", Name: "After lookup", Body: "Summarize what changed.", Event: "PostToolUse", Matcher: "lookup$", Enabled: true, Action: "agent"}
	if _, err = s.writeAgentCustomization(ctx, c, run, hook, "save", "review-hook", nil); err != nil {
		t.Fatal(err)
	}
	review := WrenExtensionReview{ID: "extreview_hook_fixture", InstallationID: install.ID, ConversationID: c.ID, App: "Fixture", Tool: "lookup", Status: "accepted", Receipt: &WrenExtensionCall{ID: "call_confirmed", Status: "unknown"}}
	if err = h.continueReviewHooks(ctx, "ws", "owner", review); err != nil {
		t.Fatal(err)
	}
	var count int
	check := func(want int) {
		t.Helper()
		if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_runs WHERE request_id=$1`, "hook_review_"+review.ID).Scan(&count); e != nil || count != want {
			t.Fatalf("hook continuations=%d want=%d err=%v", count, want, e)
		}
	}
	check(0)
	review.Receipt.Status = "completed"
	if err = h.continueReviewHooks(ctx, "ws", "owner", review); err != nil {
		t.Fatal(err)
	}
	if err = h.continueReviewHooks(ctx, "ws", "owner", review); err != nil {
		t.Fatal(err)
	}
	check(1)
	if _, err = s.pool.Exec(ctx, `INSERT INTO conversation_shares(conversation_id,workspace_id,shared_by) VALUES($1,'ws','owner')`, c.ID); err != nil {
		t.Fatal(err)
	}
	review.ID = "extreview_hook_shared"
	if err = h.continueReviewHooks(ctx, "ws", "owner", review); err == nil {
		t.Fatal("shared conversation accepted a personal hook continuation")
	}
	check(0)
}

func TestPluginHookFollowsConnectionAcrossConversations(t *testing.T) {
	_, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19998")
	ctx := context.Background()
	run := customizationTestRun(t, s, c)
	extension := operatorConnectionExtension(t, s, "http://127.0.0.1:19998", "none", operatorConnectionTools("http://127.0.0.1:19998"))
	if _, err := s.pool.Exec(ctx, `INSERT INTO plugin_connections(id,workspace_id,account_id,slug,catalog) VALUES('plugin_hook','ws','owner','fixture','{}')`); err != nil {
		t.Fatal(err)
	}
	install := func(conversation, id, connection string) WrenExtensionInstallation {
		t.Helper()
		v, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: id, WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: conversation, Scope: "operator"}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
		if err != nil {
			t.Fatal(err)
		}
		if connection != "" {
			if _, err = s.pool.Exec(ctx, `UPDATE wren_extension_installations SET plugin_connection_id=$2 WHERE id=$1`, id, connection); err != nil {
				t.Fatal(err)
			}
			v.PluginConnectionID = connection
		}
		return v
	}
	first := install(c.ID, "extinstall_first_hook", "plugin_hook")
	original := extensionDynamicName(first.ID, first.Version, "lookup")
	hook, err := s.writeAgentCustomization(ctx, c, run, personalCustomization{Kind: "hook", Name: "Follow the same plugin", Body: "Summarize the result.", Event: "PostToolUse", Matcher: original, Enabled: true}, "save", "stable-hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hook.Matcher == original || !strings.Contains(hook.Matcher, "plugin_hook") {
		t.Fatalf("not normalized: %s", hook.Matcher)
	}
	secondConversation, err := s.CreateConversation(ctx, "ws", "owner", "conv_hook_second", "Use the installed plugin")
	if err != nil {
		t.Fatal(err)
	}
	second := install(secondConversation.ID, "extinstall_second_hook", "plugin_hook")
	if _, err = s.EnqueueOperator(ctx, secondConversation, "hook-second-request", "Use the plugin", "test", nil); err != nil {
		t.Fatal(err)
	}
	secondRun, ok, err := s.claimOperator(ctx)
	if err != nil || !ok || secondRun.ConversationID != secondConversation.ID {
		t.Fatalf("second run: %v %v", ok, err)
	}
	invoke := func(v WrenExtensionInstallation, id string) []byte {
		t.Helper()
		call := operatorTestCall(extensionDynamicName(v.ID, v.Version, "lookup"), `{}`)
		call.ID = id
		receipt, _ := json.Marshal(WrenExtensionCall{InstallationID: v.ID, ToolName: "lookup", Status: "completed"})
		if _, e := s.pool.Exec(ctx, `INSERT INTO operator_tool_calls(run_id,call_id,name,arguments,result) VALUES($1,$2,$3,'{}',$4)`, secondRun.ID, id, call.Function.Name, receipt); e != nil {
			t.Fatal(e)
		}
		value, e := s.customizationToolResult(ctx, secondConversation, secondRun, call, receipt)
		if e != nil {
			t.Fatal(e)
		}
		return value
	}
	if value := invoke(second, "same_plugin"); !bytes.Contains(value, []byte("hookInstructions")) {
		t.Fatalf("another conversation did not trigger the hook: %s", value)
	}
	extension, err = s.saveWrenExtension(ctx, "ws", "owner", WrenExtensionInput{ID: "extension_other_provider", Manifest: extension.Manifest}, WrenExtensionConfig{PreviewOrigins: []string{"http://127.0.0.1:19998"}})
	if err != nil {
		t.Fatal(err)
	}
	other := install(secondConversation.ID, "extinstall_other_provider", "")
	if value := invoke(other, "other_provider"); bytes.Contains(value, []byte("hookInstructions")) {
		t.Fatalf("hook crossed provider boundaries: %s", value)
	}
	// A pre-fix saved alias still resolves to its stable provider when loaded.
	if _, err = s.pool.Exec(ctx, `UPDATE personal_customizations SET matcher=$2 WHERE id=$1`, hook.ID, original); err != nil {
		t.Fatal(err)
	}
	if value := invoke(second, "legacy_alias"); !bytes.Contains(value, []byte("hookInstructions")) {
		t.Fatalf("legacy alias not repaired on load: %s", value)
	}
}

func TestPersonalSkillAPIKeepsRuntimeFileCurrent(t *testing.T) {
	h, s, c := operatorConnectionFixture(t, "http://127.0.0.1:19999")
	item := personalCustomization{Kind: "skill", Name: "Authored skill", Body: "Original instructions.", Enabled: true}
	write := func(method string, parts []string, want int) {
		t.Helper()
		body, _ := json.Marshal(item)
		request := httptest.NewRequest(method, "/", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		h.workspaceCustomizations(response, request, Principal{Account: &Account{ID: "owner"}}, "ws", parts)
		if response.Code != want {
			t.Fatalf("%s returned %d: %s", method, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
	}
	write("POST", nil, 201)
	check := func(want string) {
		t.Helper()
		file, err := s.readPersonalSkillFile(context.Background(), c, item.ID, "SKILL.md", 0)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(file)
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("runtime file is stale: %s", data)
		}
	}
	check("Original instructions.")
	item.Body = "Revised instructions."
	write("PATCH", []string{item.ID}, 200)
	check("Revised instructions.")
	item.Enabled = false
	write("PATCH", []string{item.ID}, 200)
	if _, err := s.readPersonalSkillFile(context.Background(), c, item.ID, "SKILL.md", 0); err == nil {
		t.Fatal("disabled skill is readable")
	}
}
