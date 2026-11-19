package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These cases share the retained private-schema fixture pattern; nothing here
// truncates or drops any shared database. search_path has ONLY that schema.
func operatorConnectionFixture(t *testing.T, server string) (*HTTPServer, *Store, Conversation) {
	t.Helper()
	url := os.Getenv("CANTER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CANTER_TEST_DATABASE_URL is required for isolated-schema tests")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := newID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_operator_connection_" + strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(suffix))
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	t.Logf("isolated schema (retained): %s", schema)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err = pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&current); err != nil || current != schema {
		t.Fatalf("unexpected schema %q %v", current, err)
	}
	s := NewStore(pool)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `
	INSERT INTO accounts(id,email,password_hash,email_verified_at) VALUES ('owner','owner@example.test','',now()),('member','member@example.test','',now()),('viewer','viewer@example.test','',now()),('outsider','outsider@example.test','',now());
	INSERT INTO workspaces(id,name) VALUES ('ws','Operator connection tests'),('other','Other organization');
	INSERT INTO memberships(account_id,workspace_id,role) VALUES ('owner','ws','owner'),('member','ws','operator'),('viewer','ws','viewer'),('outsider','other','owner');
	INSERT INTO operator_conversations(id,workspace_id,account_id,title) VALUES ('conv_op_main','ws','owner','Ordinary chat'),('conv_op_member','ws','member','Member chat');
	`); err != nil {
		t.Fatal(err)
	}
	config := WrenExtensionConfig{PreviewOrigins: []string{server}}
	config.appProviders = map[string]wrenMCPProvider{"linear": {Name: "Linear fixture", Endpoint: server + "/mcp/readonly", ResourceMetadata: server + "/.well-known/oauth-protected-resource/mcp/readonly", Issuer: server}}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Extensions: config}).(*HTTPServer)
	c, err := s.Conversation(ctx, "ws", "owner", "conv_op_main")
	if err != nil {
		t.Fatal(err)
	}
	return h, s, c
}

func operatorConnectionExtension(t *testing.T, s *Store, server, auth string, tools []WrenExtensionTool) WrenExtension {
	t.Helper()
	extension, err := s.saveWrenExtension(context.Background(), "ws", "owner", WrenExtensionInput{ID: "extension_operator_test", Manifest: WrenExtensionManifest{Name: "Operator QA connector", Description: "A test-owned service.", Origin: server, Auth: auth, Tools: tools, Events: []string{}}}, WrenExtensionConfig{PreviewOrigins: []string{server}})
	if err != nil {
		t.Fatal(err)
	}
	return extension
}

func operatorConnectionTools(server string) []WrenExtensionTool {
	return []WrenExtensionTool{{Name: "lookup", Description: "Read the requested record.", Endpoint: server, Impact: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"additionalProperties":false}`)}}
}

func TestOperatorConnectionGrantAndDynamicTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	s.SetWrensEnabled(false)
	ctx := context.Background()
	extension := operatorConnectionExtension(t, s, server.URL, "none", operatorConnectionTools(server.URL))
	scope := WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}
	for _, kind := range []string{"private", "room"} {
		if _, err := s.WrenExtensionInstallations(ctx, "ws", "owner", WrenKnowledgeScope{ConversationID: c.ID, Scope: kind}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("disabled Wren scope %s remains available: %v", kind, err)
		}
	}
	install, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_operator_main", WrenKnowledgeScope: scope, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if install.Scope != "operator" || install.WrenID != "" {
		t.Fatalf("bad operator installation %+v", install.WrenKnowledgeScope)
	}
	run, err := s.EnqueueOperator(ctx, c, "operator-extension-run", "Check the service", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := h.WrenExtensionTools(ctx, c, run)
	if err != nil {
		t.Fatal(err)
	}
	want := extensionDynamicName(install.ID, install.Version, "lookup")
	found := false
	for _, tool := range tools {
		if tool.Name == want {
			found = true
		}
	}
	if !found || len(tools) != 1 {
		t.Fatalf("dynamic operator tool missing %+v", tools)
	}
	items, err := h.wrenAppConnectionStatuses(ctx, c, run)
	if err != nil || len(items) != 1 || items[0].State != "available" {
		t.Fatalf("operator connection status %+v %v", items, err)
	}
}

func TestOperatorConnectionRejectsNonOrdinaryScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	ctx := context.Background()
	extension := operatorConnectionExtension(t, s, server.URL, "none", operatorConnectionTools(server.URL))
	install := func(account, id, conversation, scope, wren string) error {
		_, err := s.InstallWrenExtension(ctx, "ws", account, WrenExtensionInstallationInput{ID: id, WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: conversation, Scope: scope, WrenID: wren}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
		return err
	}
	if err := install("owner", "extinstall_op_wren", c.ID, "operator", "wren_fake"); err == nil {
		t.Fatal("operator scope accepted a Wren identity")
	}
	if err := install("member", "extinstall_op_member", c.ID, "operator", ""); err == nil {
		t.Fatal("non-owner member installed into another person's conversation")
	}
	if err := install("viewer", "extinstall_op_viewer", c.ID, "operator", ""); err == nil {
		t.Fatal("viewer installed an operator connection")
	}
	if err := install("outsider", "extinstall_op_outside", c.ID, "operator", ""); err == nil {
		t.Fatal("outsider installed an operator connection")
	}
	if err := install("member", "extinstall_op_own", "conv_op_member", "operator", ""); err != nil {
		t.Fatalf("owner of an ordinary conversation could not install: %v", err)
	}
	if _, err := s.WrenExtensionInstallations(ctx, "ws", "member", WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}); !errors.Is(err, ErrNotFound) && err == nil {
		t.Fatal("member listed another person's operator installations")
	}
	if _, err := s.WrenExtensionInstallations(ctx, "ws", "outsider", WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}); err == nil {
		t.Fatal("outsider listed operator installations")
	}
	if _, err := h.WrenExtensionTools(ctx, c, OperatorRun{ID: "run_missing"}); err == nil {
		t.Fatal("missing run received extension tools")
	}
	// Memory and skill scopes must not accept the extension-only operator scope.
	if _, err := s.WrenMemories(ctx, "ws", "owner", WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}, ""); err == nil {
		t.Fatal("operator scope broadened memory access")
	}
	if _, err := s.SaveWrenMemory(ctx, "ws", "owner", WrenMemoryInput{WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}, Title: "Should fail", Content: "Never saved"}); err == nil {
		t.Fatal("operator scope broadened memory writes")
	}
	if _, err := s.WrenSkillInstallations(ctx, "ws", "owner", WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}); err == nil {
		t.Fatal("operator scope broadened skill access")
	}
}

func TestOperatorConnectionAutomationDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	ctx := context.Background()
	extension := operatorConnectionExtension(t, s, server.URL, "none", operatorConnectionTools(server.URL))
	if _, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_operator_auto", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"}); err != nil {
		t.Fatal(err)
	}
	insertRun := func(id, origin, executor, requester string, withMessage bool) OperatorRun {
		t.Helper()
		req := requester
		var requesterArg any
		if req != "" {
			requesterArg = req
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,model,origin_kind,executor_kind,requester_account_id,status) VALUES($1,$2,$3,'test',$4,$5,$6,'queued')`, id, c.ID, "req_"+id, origin, executor, requesterArg); err != nil {
			t.Fatal(err)
		}
		if withMessage {
			if _, err := s.pool.Exec(ctx, `INSERT INTO operator_messages(id,conversation_id,run_id,role,content) VALUES($1,$2,$3,'user','automated probe')`, "msg_"+id, c.ID, id); err != nil {
				t.Fatal(err)
			}
		}
		return OperatorRun{ID: id, ConversationID: c.ID, Status: "running"}
	}
	// A scheduled responsibility run never uses the owner's saved connections.
	run := insertRun("run_op_auto", "responsibility", "operator", "owner", true)
	tools, err := h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 0 {
		t.Fatalf("automated run received tools %+v %v", tools, err)
	}
	// A harness executor is not a verified human requester.
	run = insertRun("run_op_harness", "human", "acp-opencode", "owner", true)
	tools, err = h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 0 {
		t.Fatalf("harness run received tools %+v %v", tools, err)
	}
	// Another person's request never falls back to the owner's credentials.
	run = insertRun("run_op_peer", "human", "operator", "member", true)
	if _, err = h.WrenExtensionTools(ctx, c, run); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other requester inherited owner scope %+v %v", tools, err)
	}
	// A run without a persisted user message is not a verified human trigger.
	run = insertRun("run_op_silent", "human", "operator", "owner", false)
	tools, err = h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 0 {
		t.Fatalf("messageless run received tools %+v %v", tools, err)
	}
	// A positively verified legacy human run still resolves the operator scope.
	run = insertRun("run_op_human", "human", "operator", "", true)
	tools, err = h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 1 {
		t.Fatalf("verified human run missing tools %+v %v", tools, err)
	}
}

func TestOperatorConnectionSharedConversationDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	ctx := context.Background()
	extension := operatorConnectionExtension(t, s, server.URL, "none", operatorConnectionTools(server.URL))
	scope := WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}
	install, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_op_shared", WrenKnowledgeScope: scope, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueOperator(ctx, c, "operator-shared-run", "Check", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools, err := h.WrenExtensionTools(ctx, c, run); err != nil || len(tools) != 1 {
		t.Fatalf("missing tools before share %+v %v", tools, err)
	}
	// Sharing the conversation must immediately close the operator grant for
	// the owner, for peers, and for any new install or invocation.
	if _, err = s.pool.Exec(ctx, `INSERT INTO conversation_shares(conversation_id,workspace_id,shared_by) VALUES($1,'ws','owner')`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_op_after_share", WrenKnowledgeScope: scope, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"}); err == nil {
		t.Fatal("new grant installed into a shared conversation")
	}
	if _, err = s.WrenExtensionInstallations(ctx, "ws", "owner", scope); err == nil {
		t.Fatal("owner listed installations in a shared conversation")
	}
	if tools, err := h.WrenExtensionTools(ctx, c, run); len(tools) != 0 {
		t.Fatalf("shared conversation kept tools %+v %v", tools, err)
	}
	if _, err = h.InvokeWrenExtensionTool(ctx, c, run, extensionDynamicName(install.ID, install.Version, "lookup"), json.RawMessage(`{"query":"x"}`), "shared-op"); err == nil {
		t.Fatal("shared conversation still invoked")
	}
	peer, err := s.Conversation(ctx, "ws", "member", c.ID)
	if err != nil {
		t.Fatalf("share recipient could not open the conversation %v", err)
	}
	if _, err = s.InstallWrenExtension(ctx, "ws", "member", WrenExtensionInstallationInput{ID: "extinstall_op_peer_share", WrenKnowledgeScope: scope, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"}); err == nil {
		t.Fatal("share recipient installed into the shared conversation")
	}
	if tools, err := h.WrenExtensionTools(ctx, peer, run); len(tools) != 0 {
		t.Fatalf("share recipient received tools %+v %v", tools, err)
	}
}

func TestOperatorConnectionSharedLinearCredential(t *testing.T) {
	var calls int32
	mcpHandler := mcp.NewServer(&mcp.Implementation{Name: "Fixture account", Version: "1"}, nil)
	mcpHandler.AddTool(&mcp.Tool{Name: "list_issues", Description: "List issues", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls++
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Authenticated fixture result"}}}, nil
	})
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpHandler }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/readonly" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer qa-token" {
			w.WriteHeader(401)
			return
		}
		transport.ServeHTTP(w, r)
	}))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	ctx := context.Background()
	endpoint := server.URL + "/mcp/readonly"
	extension, err := s.saveWrenExtension(ctx, "ws", "owner", WrenExtensionInput{ID: "extension_linear_like", Manifest: WrenExtensionManifest{Name: "Linear-like QA app", Description: "Read-only MCP app.", Origin: server.URL, Auth: "bearer", Tools: []WrenExtensionTool{{Name: "list_issues", Method: "MCP", RemoteName: "list_issues", Description: "List issues.", Endpoint: endpoint, Impact: "read", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}}, Events: []string{}}}, WrenExtensionConfig{PreviewOrigins: []string{server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	install, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_operator_linear", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if install.CredentialConfigured {
		t.Fatal("credential appeared before any account was linked")
	}
	key, sealed, err := testVault(t).seal([]byte(`{"clientId":"qa-client","accessToken":"qa-token","metadata":{}}`), appAccountBinding("ws", "owner", "appaccount_linear_qa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO wren_app_accounts(id,workspace_id,account_id,provider,endpoint,label,key_id,ciphertext,expires_at) VALUES('appaccount_linear_qa','ws','owner','linear',$1,'QA Linear',$2,$3,$4)`, endpoint, key, sealed, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Another principal cannot attach the owner's saved app account.
	if _, err = h.ConfigureWrenExtensionCredential(ctx, "ws", "member", install.ID, WrenExtensionCredentialInput{AppAccountID: "appaccount_linear_qa"}); err == nil {
		t.Fatal("member linked the owner's app account")
	}
	linked, err := h.ConfigureWrenExtensionCredential(ctx, "ws", "owner", install.ID, WrenExtensionCredentialInput{AppAccountID: "appaccount_linear_qa"})
	if err != nil {
		t.Fatal(err)
	}
	if !linked.CredentialConfigured || linked.AppAccountID != "appaccount_linear_qa" {
		t.Fatalf("app account link not reflected %+v", linked)
	}
	run, err := s.EnqueueOperator(ctx, c, "operator-linear-run", "List issues", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := h.WrenExtensionTools(ctx, c, run)
	if err != nil || len(tools) != 1 {
		t.Fatalf("linked account missing dynamic tool %+v %v", tools, err)
	}
	// The same saved account also powers a private Wren install: one personal
	// credential, reused through the existing app-account path, no parallel one.
	wren, err := s.SaveWren(ctx, "ws", "owner", Wren{ID: "wren_linear_qa", Name: "Scout", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	wrenConv, err := s.Conversation(ctx, "ws", "owner", wren.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	wrenInstall, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_wren_linear", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: wren.ConversationID, Scope: "private", WrenID: wren.ID}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.ConfigureWrenExtensionCredential(ctx, "ws", "owner", wrenInstall.ID, WrenExtensionCredentialInput{AppAccountID: "appaccount_linear_qa"}); err != nil {
		t.Fatal(err)
	}
	wrenRun, err := s.EnqueueOperator(ctx, wrenConv, "wren-linear-run", "List issues", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools, err := h.WrenExtensionTools(ctx, wrenConv, wrenRun); err != nil || len(tools) != 1 {
		t.Fatalf("wren install missing shared credential tool %+v %v", tools, err)
	}
	// A real local MCP round trip proves the personal token is used on the wire.
	call, err := h.TestWrenExtensionTool(ctx, "ws", "owner", install.ID, "list_issues", "operator-linear-live", json.RawMessage(`{}`))
	if err != nil || call.Status != "completed" || calls != 1 {
		t.Fatalf("operator MCP call %+v calls=%d err=%v", call, calls, err)
	}
	// Automation on the Wren conversation gets no personal credentials even
	// though the scope itself is valid and authorized.
	if _, err = s.pool.Exec(ctx, `INSERT INTO operator_runs(id,conversation_id,request_id,model,origin_kind,executor_kind,requester_account_id,status,speaker) VALUES('run_wren_auto',$1,'req_wren_auto','test','responsibility','operator','owner','queued','{"id":"wren_linear_qa"}'::jsonb)`, wren.ConversationID); err != nil {
		t.Fatal(err)
	}
	autoRun := OperatorRun{ID: "run_wren_auto", ConversationID: wren.ConversationID, Status: "running"}
	if tools, err := h.WrenExtensionTools(ctx, wrenConv, autoRun); err != nil || len(tools) != 0 {
		t.Fatalf("automated wren run inherited personal tools %+v %v", tools, err)
	}
	if _, err = h.InvokeWrenExtensionTool(ctx, wrenConv, autoRun, extensionDynamicName(wrenInstall.ID, wrenInstall.Version, "list_issues"), json.RawMessage(`{}`), "auto-op"); err == nil {
		t.Fatal("automated wren run invoked a personal credential")
	}
	// Disconnect through the real API revokes the shared account for both
	// installations, without touching another person's connections.
	token, err := newSecret("chs_", 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO human_sessions(id,account_id,token_hash,created_at,last_seen_at,expires_at,auth_version,authenticated_at) VALUES('hss_owner_qa','owner',$1,now(),now(),now()+interval '1 day',0,now())`, secretHash(token)); err != nil {
		t.Fatal(err)
	}
	var revision int
	if err = s.pool.QueryRow(ctx, `SELECT revision FROM wren_app_accounts WHERE id='appaccount_linear_qa'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "http://canter.test/v1/workspaces/ws/app-accounts/appaccount_linear_qa", strings.NewReader(fmt.Sprintf(`{"expectedRevision":%d}`, revision)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://canter.test")
	req.AddCookie(&http.Cookie{Name: h.humanCookieName(), Value: token})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("disconnect API %d %s", recorder.Code, recorder.Body.String())
	}
	for _, id := range []string{install.ID, wrenInstall.ID} {
		current, err := s.WrenExtensionInstallation(ctx, "ws", "owner", id)
		if err != nil || current.CredentialConfigured {
			t.Fatalf("disconnected account still authorized %s %+v %v", id, current, err)
		}
	}
	if tools, err := h.WrenExtensionTools(ctx, c, run); err != nil || len(tools) != 0 {
		t.Fatalf("disconnected account kept operator tools %+v %v", tools, err)
	}
	if tools, err := h.WrenExtensionTools(ctx, wrenConv, wrenRun); err != nil || len(tools) != 0 {
		t.Fatalf("disconnected account kept wren tools %+v %v", tools, err)
	}
}

func TestOperatorConnectionRevocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	h, s, c := operatorConnectionFixture(t, server.URL)
	ctx := context.Background()
	extension := operatorConnectionExtension(t, s, server.URL, "none", operatorConnectionTools(server.URL))
	install, err := s.InstallWrenExtension(ctx, "ws", "owner", WrenExtensionInstallationInput{ID: "extinstall_operator_revoke", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: c.ID, Scope: "operator"}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueOperator(ctx, c, "operator-revoke-run", "Check", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tools, err := h.WrenExtensionTools(ctx, c, run); err != nil || len(tools) != 1 {
		t.Fatalf("missing tools before revoke %+v %v", tools, err)
	}
	edit := extensionInstallEdit(install)
	edit.Revoke = true
	if _, err = s.InstallWrenExtension(ctx, "ws", "owner", edit); err != nil {
		t.Fatal(err)
	}
	if tools, err := h.WrenExtensionTools(ctx, c, run); err != nil || len(tools) != 0 {
		t.Fatalf("revoked installation kept tools %+v %v", tools, err)
	}
	if _, err := h.InvokeWrenExtensionTool(ctx, c, run, extensionDynamicName(install.ID, install.Version, "lookup"), json.RawMessage(`{"query":"x"}`), "revoked-op"); err == nil {
		t.Fatal("revoked installation still executed")
	}
}
