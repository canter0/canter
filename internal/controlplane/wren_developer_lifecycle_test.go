package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// This proves the supported distribution contract: export/import immutable
// configuration into an independently owned workspace catalog. It intentionally
// does not invent a global cross-workspace package/update/revocation grant.
func TestWrenDeveloperSecondWorkspaceHTTPVersionLifecycleAndPrivacy(t *testing.T) {
	s, author, _ := operatorFixture(t)
	ctx := context.Background()
	_, _, authorToken, err := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	receiver, receiverWorkspace, receiverToken, err := s.Signup(ctx, "lifecycle-receiver@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	const authorSecret = "owned-lifecycle-author-credential-12345"
	const receiverSecret = "owned-lifecycle-receiver-credential-67890"
	var hits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var body struct {
			Protocol, Tool, OperationID string
			Arguments                   map[string]string
		}
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Protocol != "canter-json-tools-v1" || body.Tool != "lookup" || body.OperationID == "" || body.Arguments["query"] == "" {
			http.Error(w, "invalid owned connector protocol", 400)
			return
		}
		identity := ""
		switch r.Header.Get("Authorization") {
		case "Bearer " + authorSecret:
			identity = "author"
		case "Bearer " + receiverSecret:
			identity = "receiver"
		default:
			http.Error(w, "missing owned credential", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"answer": "Actual owned connector response", "endpoint": r.URL.Path, "identity": identity})
	}))
	defer remote.Close()
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Extensions: WrenExtensionConfig{PreviewOrigins: []string{remote.URL}}}).(*HTTPServer)
	request := func(token, workspace, method, path string, input any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(input)
		r := httptest.NewRequest(method, "http://canter.test/v1/workspaces/"+workspace+path, bytes.NewReader(raw))
		r.Header.Set("Origin", "http://canter.test")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "canter_session", Value: token})
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	decode := func(out *httptest.ResponseRecorder, target any) {
		t.Helper()
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), target) != nil {
			t.Fatalf("HTTP lifecycle status=%d body=%s", out.Code, out.Body)
		}
		if strings.Contains(out.Body.String(), authorSecret) || strings.Contains(out.Body.String(), receiverSecret) {
			t.Fatal("credential returned by lifecycle API")
		}
	}
	manifest := WrenExtensionManifest{Name: "Independent developer connector", Description: "An owned versioned read connector", Origin: remote.URL, Auth: "bearer", Tools: []WrenExtensionTool{{Name: "lookup", Description: "Look up the requested owned record", Endpoint: remote.URL + "/v1", Impact: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object","required":["answer","endpoint","identity"]}`)}}, Events: []string{}}
	var sourceV1 WrenExtension
	decode(request(authorToken, author.WorkspaceID, "POST", "/wren-extensions", WrenExtensionInput{ID: "extension_lifecycle_author", Manifest: manifest}), &sourceV1)
	// The receiver has no source-workspace grant; configuration export is explicit.
	var exportedV1 WrenExtension
	decode(request(authorToken, author.WorkspaceID, "GET", "/wren-extensions/"+sourceV1.ID+"?version=1", nil), &exportedV1)
	if out := request(receiverToken, author.WorkspaceID, "GET", "/wren-extensions/"+sourceV1.ID, nil); out.Code == 200 {
		t.Fatal("receiver read private author catalog")
	}
	authorWren, err := s.SaveWren(ctx, author.WorkspaceID, author.AccountID, Wren{ID: "wren_lifecycle_author", Name: "Author", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	receiverWren, err := s.SaveWren(ctx, receiverWorkspace.ID, receiver.ID, Wren{ID: "wren_lifecycle_receiver", Name: "Receiver", Shape: "square"}, true)
	if err != nil {
		t.Fatal(err)
	}
	authorScope := WrenKnowledgeScope{Scope: "private", ConversationID: authorWren.ConversationID, WrenID: authorWren.ID}
	receiverScope := WrenKnowledgeScope{Scope: "private", ConversationID: receiverWren.ConversationID, WrenID: receiverWren.ID}
	var authorInstall WrenExtensionInstallation
	decode(request(authorToken, author.WorkspaceID, "POST", "/wren-extensions/installations", WrenExtensionInstallationInput{ID: "extinstall_lifecycle_author", WrenKnowledgeScope: authorScope, ExtensionID: sourceV1.ID, Version: 1, Enabled: true, CredentialMode: "personal"}), &authorInstall)
	configure := func(token, workspace string, install WrenExtensionInstallation, secret string) {
		t.Helper()
		var current WrenExtensionInstallation
		decode(request(token, workspace, "POST", "/wren-extensions/installations/"+install.ID+"/credentials", WrenExtensionCredentialInput{ExpectedRevision: install.CredentialRevision, Value: secret}), &current)
		if !current.CredentialConfigured {
			t.Fatal("personal credential not configured")
		}
	}
	invoke := func(token, workspace string, install WrenExtensionInstallation, key string) WrenExtensionCall {
		t.Helper()
		var call WrenExtensionCall
		decode(request(token, workspace, "POST", "/wren-extensions/installations/"+install.ID+"/test", map[string]any{"tool": "lookup", "operationKey": key, "arguments": map[string]string{"query": "authorized record"}}), &call)
		if call.Status != "completed" {
			t.Fatalf("owned service did not complete %+v", call)
		}
		return call
	}
	configure(authorToken, author.WorkspaceID, authorInstall, authorSecret)
	authorCall := invoke(authorToken, author.WorkspaceID, authorInstall, "author-private-first-use")
	if !strings.Contains(string(authorCall.Result), `"identity":"author"`) {
		t.Fatal("author fixture credential not used")
	}
	// A catalog ID from A cannot be directly installed in B.
	forbiddenInput := WrenExtensionInstallationInput{ID: "extinstall_lifecycle_receiver", WrenKnowledgeScope: receiverScope, ExtensionID: sourceV1.ID, Version: 1, Enabled: true, CredentialMode: "personal"}
	if out := request(receiverToken, receiverWorkspace.ID, "POST", "/wren-extensions/installations", forbiddenInput); out.Code != 404 {
		t.Fatalf("cross-workspace catalog ID admitted %d", out.Code)
	}
	var importedV1 WrenExtension
	decode(request(receiverToken, receiverWorkspace.ID, "POST", "/wren-extensions", WrenExtensionInput{ID: "extension_lifecycle_receiver", Manifest: exportedV1.Manifest}), &importedV1)
	forbiddenInput.ExtensionID = importedV1.ID
	var install WrenExtensionInstallation
	decode(request(receiverToken, receiverWorkspace.ID, "POST", "/wren-extensions/installations", forbiddenInput), &install)
	if install.Version != 1 || install.CredentialConfigured {
		t.Fatal("import inherited credentials or wrong version")
	}
	before := hits.Load()
	if out := request(receiverToken, receiverWorkspace.ID, "POST", "/wren-extensions/installations/"+install.ID+"/test", map[string]any{"tool": "lookup", "operationKey": "before-personal-setup", "arguments": map[string]string{"query": "record"}}); out.Code != 400 || hits.Load() != before {
		t.Fatal("import used author credential fallback", out.Code, hits.Load())
	}
	configure(receiverToken, receiverWorkspace.ID, install, receiverSecret)
	first := invoke(receiverToken, receiverWorkspace.ID, install, "receiver-first-use-v1")
	if first.Version != 1 || !strings.Contains(string(first.Result), `"endpoint":"/v1"`) || !strings.Contains(string(first.Result), `"identity":"receiver"`) {
		t.Fatalf("wrong first use %+v", first)
	}
	replay := invoke(receiverToken, receiverWorkspace.ID, install, "receiver-first-use-v1")
	if replay.ID != first.ID || hits.Load() != before+1 {
		t.Fatal("same operation replayed remote call")
	}
	// Author publishes v2. Receiver export/import is explicit and cannot change a pin.
	manifestV2 := exportedV1.Manifest
	manifestV2.Tools = append([]WrenExtensionTool(nil), manifestV2.Tools...)
	manifestV2.Tools[0].Endpoint = remote.URL + "/v2"
	var sourceV2 WrenExtension
	decode(request(authorToken, author.WorkspaceID, "PATCH", "/wren-extensions/"+sourceV1.ID, WrenExtensionInput{ID: sourceV1.ID, ExpectedVersion: 1, Manifest: manifestV2}), &sourceV2)
	var unchanged WrenExtensionInstallation
	decode(request(receiverToken, receiverWorkspace.ID, "GET", "/wren-extensions/installations/"+install.ID, nil), &unchanged)
	if unchanged.Version != 1 || unchanged.Extension.LatestVersion != 1 {
		t.Fatal("source workspace update leaked into imported copy")
	}
	var exportedV2 WrenExtension
	decode(request(authorToken, author.WorkspaceID, "GET", "/wren-extensions/"+sourceV2.ID+"?version=2", nil), &exportedV2)
	var importedV2 WrenExtension
	decode(request(receiverToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/"+importedV1.ID, WrenExtensionInput{ID: importedV1.ID, ExpectedVersion: 1, Manifest: exportedV2.Manifest}), &importedV2)
	decode(request(receiverToken, receiverWorkspace.ID, "GET", "/wren-extensions/installations/"+install.ID, nil), &unchanged)
	if unchanged.Version != 1 || unchanged.Extension.LatestVersion != 2 {
		t.Fatal("catalog publication silently upgraded explicit pin")
	}
	stillV1 := invoke(receiverToken, receiverWorkspace.ID, unchanged, "receiver-after-v2-publish")
	if stillV1.Version != 1 || !strings.Contains(string(stillV1.Result), `"endpoint":"/v1"`) {
		t.Fatal("pin did not preserve v1 endpoint")
	}
	update := extensionInstallEdit(unchanged)
	update.Version = 2
	var upgraded WrenExtensionInstallation
	decode(request(receiverToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/installations/"+install.ID, update), &upgraded)
	second := invoke(receiverToken, receiverWorkspace.ID, upgraded, "receiver-explicit-update-v2")
	if second.Version != 2 || !strings.Contains(string(second.Result), `"endpoint":"/v2"`) {
		t.Fatal("explicit update did not use v2")
	}
	if out := request(receiverToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/installations/"+install.ID, update); out.Code != 409 {
		t.Fatal("stale installation mutation did not conflict", out.Code)
	}
	rollback := extensionInstallEdit(upgraded)
	rollback.Version = 1
	var rolled WrenExtensionInstallation
	decode(request(receiverToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/installations/"+install.ID, rollback), &rolled)
	reverted := invoke(receiverToken, receiverWorkspace.ID, rolled, "receiver-explicit-rollback-v1")
	if reverted.Version != 1 || !strings.Contains(string(reverted.Result), `"endpoint":"/v1"`) {
		t.Fatal("rollback did not restore v1")
	}
	// Independent imports grant neither private history nor remote edit authority.
	for _, path := range []string{"/wren-extensions/installations/" + install.ID, "/wren-extensions/installations/" + install.ID + "/calls"} {
		if out := request(authorToken, author.WorkspaceID, "GET", path, nil); out.Code == 200 || strings.Contains(out.Body.String(), first.ID) {
			t.Fatal("author read receiver private installation/outcomes")
		}
	}
	if out := request(authorToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/"+importedV1.ID, WrenExtensionInput{ExpectedVersion: 2, Manifest: manifestV2, Disabled: true}); out.Code == 200 {
		t.Fatal("source author remotely revised independent receiver copy")
	}
	// Revoke receiver installation; immutable outcomes remain in its own scope.
	revoke := extensionInstallEdit(rolled)
	revoke.Enabled = false
	revoke.Revoke = true
	var revoked WrenExtensionInstallation
	decode(request(receiverToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/installations/"+install.ID, revoke), &revoked)
	if revoked.RevokedAt == nil {
		t.Fatal("revocation did not persist")
	}
	before = hits.Load()
	if out := request(receiverToken, receiverWorkspace.ID, "POST", "/wren-extensions/installations/"+install.ID+"/test", map[string]any{"tool": "lookup", "operationKey": "after-revocation", "arguments": map[string]string{"query": "record"}}); out.Code != 403 || hits.Load() != before {
		t.Fatal("revoked installation called service", out.Code)
	}
	resume := extensionInstallEdit(revoked)
	resume.Enabled = true
	if out := request(receiverToken, receiverWorkspace.ID, "PATCH", "/wren-extensions/installations/"+install.ID, resume); out.Code != 409 {
		t.Fatal("revoked installation resumed", out.Code)
	}
	var outcomes struct{ Calls []WrenExtensionCall }
	decode(request(receiverToken, receiverWorkspace.ID, "GET", "/wren-extensions/installations/"+install.ID+"/calls", nil), &outcomes)
	if len(outcomes.Calls) != 4 {
		t.Fatalf("durable private outcomes lost on revoke %+v", outcomes.Calls)
	}
	for _, call := range outcomes.Calls {
		if call.AccountID != receiver.ID || call.ID == authorCall.ID {
			t.Fatal("cross-workspace outcome copied")
		}
	}
	t.Logf("source catalog v%d/v%d explicitly imported as independent receiver catalog v%d/v%d; actual endpoint path /v1 -> /v1 -> /v2 -> /v1; %d owned HTTP requests; no credentials/history inheritance", sourceV1.Version, sourceV2.Version, importedV1.Version, importedV2.Version, hits.Load())
}
