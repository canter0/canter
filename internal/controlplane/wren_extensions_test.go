package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func extensionFixture(t *testing.T, server string, auth string) (*HTTPServer, Conversation, Conversation, string, WrenExtensionInstallation) {
	t.Helper()
	s, private, _, shared, member := knowledgeFixture(t)
	var err error
	config := WrenExtensionConfig{PreviewOrigins: []string{server}}
	h := NewHTTPServer(&Service{Store: s}, HTTPConfig{PublicURL: "http://canter.test", Secrets: testVault(t), Extensions: config}).(*HTTPServer)
	input := WrenExtensionInput{ID: "extension_owned_test", Manifest: WrenExtensionManifest{Name: "Owned QA connector", Description: "A test-owned service.", Origin: server, Auth: auth, Tools: []WrenExtensionTool{{Name: "lookup", Description: "Read the requested record.", Endpoint: server + "/read", Impact: "read", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object","required":["answer"]}`)}, {Name: "update", Description: "Apply an explicitly granted change.", Endpoint: server + "/write", Impact: "write", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}}, Events: []string{"lead_received"}}}
	extension, err := s.saveWrenExtension(context.Background(), private.WorkspaceID, private.AccountID, input, config)
	if err != nil {
		t.Fatal(err)
	}
	install, err := s.InstallWrenExtension(context.Background(), private.WorkspaceID, private.AccountID, WrenExtensionInstallationInput{ID: "extinstall_owned_test", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}, ExtensionID: extension.ID, Version: extension.Version, Enabled: true, CredentialMode: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	return h, private, shared, member, install
}
func extensionInstallEdit(v WrenExtensionInstallation) WrenExtensionInstallationInput {
	return WrenExtensionInstallationInput{ID: v.ID, WrenKnowledgeScope: v.WrenKnowledgeScope, ExtensionID: v.ExtensionID, Version: v.Version, ExpectedRevision: v.Revision, Enabled: v.Enabled, AllowWrite: v.AllowWrite, CredentialMode: v.CredentialMode}
}
func TestWrenExtensionPersonalCredentialsRealCallReplayAndRedaction(t *testing.T) {
	var hits atomic.Int32
	bearers := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		bearers <- r.Header.Get("Authorization")
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing stable provider request key")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["protocol"] != "canter-json-tools-v1" {
			t.Error("invalid real request")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"answer": "Actual service response", "echo": r.Header.Get("Authorization"), "access_token": "never-expose-service-token"})
	}))
	defer server.Close()
	h, private, shared, member, install := extensionFixture(t, server.URL, "bearer")
	s := h.service.Store
	ctx := context.Background()
	ownerSecret := "owner_test_bearer_123456"
	memberSecret := "member_test_bearer_987654"
	owner, err := h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{Value: ownerSecret})
	if err != nil || !owner.CredentialConfigured {
		t.Fatalf("owner setup %+v %v", owner, err)
	}
	memberView, err := s.WrenExtensionInstallation(ctx, private.WorkspaceID, member, install.ID)
	if err != nil || memberView.CredentialConfigured {
		t.Fatalf("owner credential shared implicitly %+v %v", memberView, err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "no-personal-fallback", json.RawMessage(`{"query":"first"}`)); err == nil || hits.Load() != 0 {
		t.Fatalf("member used installer credential, hits=%d err=%v", hits.Load(), err)
	}
	memberView, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, member, install.ID, WrenExtensionCredentialInput{Value: memberSecret})
	if err != nil || !memberView.CredentialConfigured {
		t.Fatal(err)
	}
	call, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "actual-read-operation", json.RawMessage(`{"query":"first"}`))
	if err != nil || call.Status != "completed" || !call.Private || call.HTTPStatus != 200 || !strings.Contains(string(call.Result), "Actual service response") {
		t.Fatalf("actual call %+v %v", call, err)
	}
	if got := <-bearers; got != "Bearer "+memberSecret {
		t.Fatal("call did not use current requester's credential")
	}
	if strings.Contains(string(call.Result), memberSecret) || strings.Contains(string(call.Result), "never-expose-service-token") || !strings.Contains(string(call.Result), "[redacted]") {
		t.Fatalf("credential leaked through external result %s", call.Result)
	}
	replay, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "actual-read-operation", json.RawMessage(`{"query":"first"}`))
	if err != nil || replay.ID != call.ID || hits.Load() != 1 {
		t.Fatalf("replay sent duplicate %+v %v hits=%d", replay, err, hits.Load())
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "actual-read-operation", json.RawMessage(`{"query":"changed"}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key changed arguments %v", err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "invalid-schema-input", json.RawMessage(`{"wrong":true}`)); err == nil || hits.Load() != 1 {
		t.Fatalf("invalid schema reached service %v", err)
	}
	ownerCalls, err := s.WrenExtensionCalls(ctx, private.WorkspaceID, private.AccountID, install.ID)
	if err != nil || len(ownerCalls) != 0 {
		t.Fatalf("private setup test disclosed to room %+v %v", ownerCalls, err)
	}
	ownCalls, err := s.WrenExtensionCalls(ctx, private.WorkspaceID, member, install.ID)
	if err != nil || len(ownCalls) != 1 {
		t.Fatalf("test receipt missing %+v %v", ownCalls, err)
	}
	var ciphertext []byte
	if err = s.pool.QueryRow(ctx, `SELECT ciphertext FROM wren_extension_credentials WHERE installation_id=$1 AND credential_key=$2`, install.ID, "user:"+member).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte(memberSecret)) {
		t.Fatalf("setup secret not vaulted %v", err)
	}
	raw, _ := json.Marshal(memberView)
	if bytes.Contains(raw, []byte(memberSecret)) || bytes.Contains(raw, []byte(ownerSecret)) {
		t.Fatal("credential in installation metadata")
	}
	// Removing the installation creator's grant prevents even another member's
	// previously configured credential from sustaining the departed issuer.
	if _, err = s.pool.Exec(ctx, `UPDATE memberships SET role='viewer' WHERE workspace_id=$1 AND account_id=$2`, private.WorkspaceID, private.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "issuer-role-revoked", json.RawMessage(`{"query":"x"}`)); !errors.Is(err, ErrForbidden) || hits.Load() != 1 {
		t.Fatalf("departed issuer authority sustained %v", err)
	}
	_ = shared
}
func TestWrenExtensionUncertainAndConcurrentAttemptsNeverBlindResend(t *testing.T) {
	var hits atomic.Int32
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			arrived <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer server.Close()
	h, private, _, _, install := extensionFixture(t, server.URL, "none")
	ctx := context.Background()
	out := make(chan WrenExtensionCall, 1)
	fail := make(chan error, 1)
	go func() {
		v, e := h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "lookup", "uncertain-operation-key", json.RawMessage(`{"query":"x"}`))
		out <- v
		fail <- e
	}()
	<-arrived
	replay, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "lookup", "uncertain-operation-key", json.RawMessage(`{"query":"x"}`))
	if err != nil || replay.Status != "unknown" || hits.Load() != 1 {
		close(release)
		t.Fatalf("inflight retry guessed safe %+v %v", replay, err)
	}
	close(release)
	first := <-out
	if err = <-fail; err != nil || first.Status != "unknown" || first.HTTPStatus != 503 {
		t.Fatalf("uncertain response %+v %v", first, err)
	}
	again, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "lookup", "uncertain-operation-key", json.RawMessage(`{"query":"x"}`))
	if err != nil || again.ID != first.ID || hits.Load() != 1 {
		t.Fatalf("unknown outcome resent %+v %v", again, err)
	}
}
func TestWrenExtensionVersionPinRollbackWriteGrantAndRevocation(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"answer": r.URL.Path})
	}))
	defer server.Close()
	h, private, _, _, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	if _, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "update", "ungranted-write-test", json.RawMessage(`{"query":"x"}`)); !errors.Is(err, ErrForbidden) || hits.Load() != 0 {
		t.Fatalf("ungranted write executed %v", err)
	}
	manifest := install.Extension.Manifest
	manifest.Tools[0].Endpoint = server.URL + "/v2"
	version2, err := s.saveWrenExtension(ctx, private.WorkspaceID, private.AccountID, WrenExtensionInput{ID: install.ExtensionID, ExpectedVersion: 1, Manifest: manifest}, h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := s.WrenExtensionInstallation(ctx, private.WorkspaceID, private.AccountID, install.ID)
	if err != nil || pinned.Version != 1 {
		t.Fatalf("catalog silently upgraded pin %+v %v", pinned, err)
	}
	input := extensionInstallEdit(pinned)
	input.Version = version2.Version
	input.AllowWrite = true
	upgraded, err := s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	call, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "lookup", "v2-read-operation", json.RawMessage(`{"query":"x"}`))
	if err != nil || call.Version != 2 || !strings.Contains(string(call.Result), "/v2") {
		t.Fatalf("actual v2 pin %+v %v", call, err)
	}
	input = extensionInstallEdit(upgraded)
	input.Version = 1
	rolled, err := s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	call, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "update", "explicit-write-operation", json.RawMessage(`{"query":"x"}`))
	if err != nil || call.Status != "completed" || call.Version != 1 {
		t.Fatalf("explicit grant/rollback %+v %v", call, err)
	}
	input = extensionInstallEdit(rolled)
	input.Enabled = false
	disabled, err := s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, private.AccountID, install.ID, "lookup", "disabled-no-request", json.RawMessage(`{"query":"x"}`)); !errors.Is(err, ErrForbidden) || hits.Load() != 2 {
		t.Fatalf("disabled invocation %v", err)
	}
	input = extensionInstallEdit(disabled)
	input.Revoke = true
	revoked, err := s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke %+v %v", revoked, err)
	}
	input = extensionInstallEdit(revoked)
	input.Enabled = true
	if _, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked authority resumed %v", err)
	}
	input.ID = "extinstall_fresh_grant"
	input.ExpectedRevision = 0
	if _, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input); err != nil {
		t.Fatalf("fresh intentional installation failed %v", err)
	}
}
func TestWrenExtensionSSRFAndSchemaResourcesFailClosed(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/private", "https://169.254.169.254/latest", "https://example.com:8443/tool", "https://user:password@example.com/tool", "https://example.com/tool?key=secret", "https://localhost/tool"} {
		if _, _, err := extensionURL(raw, WrenExtensionConfig{}); err == nil {
			t.Fatalf("restricted URL accepted %s", raw)
		}
	}
	var dials atomic.Int32
	config := WrenExtensionConfig{lookup: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}, dialContext: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}}
	if _, _, err := extensionHTTPClient(context.Background(), config, "https://api.example.com/tool"); err == nil || dials.Load() != 0 {
		t.Fatalf("mixed DNS private IP reached dial %v", err)
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "100.100.100.200", "169.254.169.254", "192.0.2.1", "198.18.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2001:db8::1", "3fff::1"} {
		if extensionPublicIP(netip.MustParseAddr(raw)) {
			t.Fatalf("restricted IP accepted %s", raw)
		}
	}
	if !extensionPublicIP(netip.MustParseAddr("8.8.8.8")) || !extensionPublicIP(netip.MustParseAddr("2606:4700:4700::1111")) {
		t.Fatal("ordinary public IP rejected")
	}
	if _, err := extensionSchema(json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"http://127.0.0.1/private"}}}`), true); err == nil {
		t.Fatal("external schema resource accepted")
	}
	schema, err := extensionSchema(json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"#/$defs/name"}},"required":["x"],"$defs":{"name":{"type":"string","minLength":3}}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if schema.Validate(map[string]any{"x": "a"}) == nil {
		t.Fatal("actual JSON schema constraint ignored")
	}
}
func TestWrenExtensionPinnedDNSAndNoRedirect(t *testing.T) {
	var lookups atomic.Int32
	var address string
	config := WrenExtensionConfig{lookup: func(context.Context, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, dialContext: func(_ context.Context, _ string, target string) (net.Conn, error) {
		address = target
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}}
	client, closeClient, err := extensionHTTPClient(context.Background(), config, "https://api.example.com/tool")
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	transport := client.Transport.(*http.Transport)
	connection, err := transport.DialContext(context.Background(), "tcp", "api.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if address != "8.8.8.8:443" || lookups.Load() != 1 {
		t.Fatalf("DNS rebinding window %s lookups=%d", address, lookups.Load())
	}
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	h, private, _, _, install := extensionFixture(t, redirect.URL, "none")
	call, err := h.TestWrenExtensionTool(context.Background(), private.WorkspaceID, private.AccountID, install.ID, "lookup", "redirect-rejected-test", json.RawMessage(`{"query":"x"}`))
	if err != nil || call.Status != "unknown" || call.HTTPStatus != 307 || targetHits.Load() != 0 {
		t.Fatalf("redirect followed %+v %v hits=%d", call, err, targetHits.Load())
	}
}

func TestWrenExtensionLeasedRuntimeCatalogInvocationAndPrivateWorkerIsolation(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var data struct {
			Arguments map[string]any `json:"arguments"`
		}
		if json.NewDecoder(r.Body).Decode(&data) != nil {
			t.Error("invalid arguments")
		}
		if _, exists := data.Arguments["workspaceId"]; exists {
			t.Error("platform injected workspace authority into an external schema")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answer":"Actual runtime connector response"}`))
	}))
	defer server.Close()
	h, private, shared, _, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	if _, err := s.EnqueueOperator(ctx, shared, "extension-runtime-root", "Look up the requested record", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	tools, err := h.WrenExtensionTools(ctx, shared, run)
	if err != nil || len(tools) != 1 {
		t.Fatalf("actual dynamic catalog %+v %v", tools, err)
	}
	expected := extensionDynamicName(install.ID, 1, "lookup")
	if tools[0].Name != expected {
		t.Fatalf("unstable tool identity %s", tools[0].Name)
	}
	principal, err := s.operatorPrincipal(ctx, shared, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := OperatorRuntime{Server: h}
	call := operatorTestCall(expected, `{"query":"requested"}`)
	result, err := runtime.executeTool(ctx, run, shared, principal, call)
	if err != nil || !strings.Contains(string(result), "Actual runtime connector response") || hits.Load() != 1 {
		t.Fatalf("actual executeTool %s %v hits=%d", result, err, hits.Load())
	}
	again, err := runtime.executeTool(ctx, run, shared, principal, call)
	var initialReceipt, replayedReceipt WrenExtensionCall
	firstDecode := json.Unmarshal(result, &initialReceipt)
	replayDecode := json.Unmarshal(again, &replayedReceipt)
	if err != nil || firstDecode != nil || replayDecode != nil || initialReceipt.ID != replayedReceipt.ID || hits.Load() != 1 {
		t.Fatalf("runtime replay changed receipt or duplicated request %s %v hits=%d", again, err, hits.Load())
	}
	invalid := operatorTestCall(expected, `{"wrong":true}`)
	invalid.ID = "call_invalid_external_schema"
	result, err = runtime.executeTool(ctx, run, shared, principal, invalid)
	if err == nil && !strings.Contains(string(result), "schema") {
		t.Fatalf("invalid schema not rejected %s", result)
	}
	if hits.Load() != 1 {
		t.Fatal("invalid schema reached external endpoint")
	}
	job, err := s.CreateWrenJob(ctx, run, shared, "extension-worker-brief", "Bounded worker", "Only this task brief is granted", "test")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Conversation(ctx, private.WorkspaceID, private.AccountID, job.ChildConversationID)
	if err != nil {
		t.Fatal(err)
	}
	childTools, err := h.WrenExtensionTools(ctx, child, OperatorRun{ID: job.ChildRunID})
	if err != nil || len(childTools) != 0 {
		t.Fatalf("worker inherited private/service credentials %+v %v", childTools, err)
	}
	input := extensionInstallEdit(install)
	input.Enabled = false
	if _, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input); err != nil {
		t.Fatal(err)
	}
	disabled := operatorTestCall(expected, `{"query":"requested"}`)
	disabled.ID = "call_disabled_external"
	result, err = runtime.executeTool(ctx, run, shared, principal, disabled)
	if err != nil && !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if hits.Load() != 1 || err == nil && !strings.Contains(string(result), "not available") {
		t.Fatalf("disabled runtime call %s %v", result, err)
	}
}
func TestWrenExtensionExplicitSharedServiceSetupAndSecretRevocation(t *testing.T) {
	received := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answer":"service account result"}`))
	}))
	defer server.Close()
	h, private, _, member, install := extensionFixture(t, server.URL, "bearer")
	s := h.service.Store
	ctx := context.Background()
	if _, err := h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{Value: "private_owner_key_123456"}); err != nil {
		t.Fatal(err)
	}
	input := extensionInstallEdit(install)
	input.CredentialMode = "service"
	service, err := s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil || service.CredentialConfigured {
		t.Fatalf("personal key silently shared %+v %v", service, err)
	}
	if _, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{Value: "shared_service_key_123456"}); err == nil {
		t.Fatal("shared credential lacks explicit human confirmation")
	}
	if _, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, member, install.ID, WrenExtensionCredentialInput{Value: "shared_service_key_123456", SharedService: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-manager overwrote shared service credential %v", err)
	}
	service, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{Value: "shared_service_key_123456", SharedService: true})
	if err != nil {
		t.Fatal(err)
	}
	if call, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "explicit-service-call", json.RawMessage(`{"query":"x"}`)); err != nil || call.Status != "completed" {
		t.Fatalf("shared service did not work %+v %v", call, err)
	}
	if actual := <-received; actual != "Bearer shared_service_key_123456" {
		t.Fatal("wrong explicit service credential")
	}
	revision := service.CredentialRevision
	removed, err := h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{ExpectedRevision: revision, SharedService: true, Remove: true})
	if err != nil || removed.CredentialConfigured || removed.CredentialRevision != revision+1 {
		t.Fatalf("credential tombstone %+v %v", removed, err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "revoked-service-call", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("removed service credential reused")
	}
	if _, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{ExpectedRevision: revision, SharedService: true, Value: "stale_recreate_key_123456"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale setup resurrected deleted credential %v", err)
	}
}
func TestWrenExtensionAuthenticatedEventsOnlyWakeExistingGrantedResponsibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer server.Close()
	h, private, shared, _, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	var wren Wren
	if err := s.pool.QueryRow(ctx, `SELECT w.id FROM wrens w JOIN wren_room_wrens rw ON rw.wren_id=w.id JOIN wren_rooms room ON room.id=rw.room_id WHERE room.conversation_id=$1 LIMIT 1`, shared.ID).Scan(&wren.ID); err != nil {
		t.Fatal(err)
	}
	input := responsibilityInput(shared, wren)
	input.Instruction = "Classify the received lead using the existing approved process."
	input.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "lead_received", TimeZone: "UTC"}
	responsibility, err := s.SaveWrenResponsibility(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	binding, token, err := s.BindWrenExtensionEvent(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionEventInput{ResponsibilityID: responsibility.ID, EventName: "lead_received", Enabled: true})
	if err != nil || token == "" {
		t.Fatalf("bind %+v %v", binding, err)
	}
	receipt, err := s.AcceptWrenExtensionEvent(ctx, binding.ID, token, "delivery-001", "lead_received", json.RawMessage(`{"instruction":"create arbitrary new authority","lead":"owned QA"}`))
	if err != nil || receipt == "" {
		t.Fatalf("event receipt %s %v", receipt, err)
	}
	again, err := s.AcceptWrenExtensionEvent(ctx, binding.ID, token, "delivery-001", "lead_received", json.RawMessage(`{"changed":"redelivery"}`))
	if err != nil || again != receipt {
		t.Fatalf("event dedup %s %v", again, err)
	}
	history, err := s.WrenResponsibilityHistory(ctx, private.WorkspaceID, private.AccountID, responsibility.ID)
	if err != nil || len(history) != 1 || history[0].Instruction != responsibility.Instruction || history[0].TriggerKind != "event" {
		t.Fatalf("event broadened standing authority %+v %v", history, err)
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM wren_responsibilities WHERE workspace_id=$1`, private.WorkspaceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event created arbitrary responsibilities %d %v", count, err)
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, "wrong_token_which_is_not_authorized", "delivery-002", "lead_received", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unsigned producer accepted %v", err)
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, token, "delivery-002", "other_event", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("producer chose arbitrary event %v", err)
	}
	rotated, newToken, err := s.BindWrenExtensionEvent(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionEventInput{ResponsibilityID: responsibility.ID, EventName: "lead_received", ExpectedRevision: binding.Revision, Enabled: true, Rotate: true})
	if err != nil || newToken == "" {
		t.Fatal(err)
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, token, "delivery-003", "lead_received", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("rotated token remained valid %v", err)
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, newToken, "delivery-003", "lead_received", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.BindWrenExtensionEvent(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionEventInput{ResponsibilityID: responsibility.ID, EventName: "lead_received", ExpectedRevision: rotated.Revision, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, newToken, "delivery-004", "lead_received", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("disabled event token woke work %v", err)
	}
}

func TestWrenExtensionHTTPPublishSetupInvokeAndOpaqueEventReceipt(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answer":"Actual HTTP setup test response"}`))
	}))
	defer server.Close()
	h, private, shared, _, existing := extensionFixture(t, server.URL, "bearer")
	s := h.service.Store
	ctx := context.Background()
	_, _, token, err := s.Signin(ctx, "operator@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, session, bearer string, input any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(input)
		r := httptest.NewRequest(method, "http://canter.test/v1"+path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://canter.test")
		if session != "" {
			r.AddCookie(&http.Cookie{Name: "canter_session", Value: session})
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}
	prefix := "/workspaces/" + private.WorkspaceID + "/wren-extensions"
	got := request("POST", prefix, token, "", WrenExtensionInput{ID: "extension_http_published", Manifest: existing.Extension.Manifest})
	if got.Code != 200 {
		t.Fatalf("HTTP publish %d %s", got.Code, got.Body)
	}
	var extension WrenExtension
	if err = json.Unmarshal(got.Body.Bytes(), &extension); err != nil {
		t.Fatal(err)
	}
	input := WrenExtensionInstallationInput{ID: "extinstall_http_setup", WrenKnowledgeScope: WrenKnowledgeScope{ConversationID: shared.ID, Scope: "room"}, ExtensionID: extension.ID, Version: 1, Enabled: true, CredentialMode: "personal"}
	got = request("POST", prefix+"/installations", token, "", input)
	if got.Code != 200 {
		t.Fatalf("HTTP installation %d %s", got.Code, got.Body)
	}
	var install WrenExtensionInstallation
	if err = json.Unmarshal(got.Body.Bytes(), &install); err != nil {
		t.Fatal(err)
	}
	secret := "write_only_http_setup_credential_123456"
	got = request("POST", prefix+"/installations/"+install.ID+"/credentials", token, "", WrenExtensionCredentialInput{Value: secret})
	if got.Code != 200 || strings.Contains(got.Body.String(), secret) {
		t.Fatalf("write-only setup leaked %d %s", got.Code, got.Body)
	}
	got = request("GET", prefix+"/installations/"+install.ID+"/tools", token, "", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"canInvoke":true`) || !strings.Contains(got.Body.String(), "human has not granted write access") {
		t.Fatalf("actual tool diagnostics %d %s", got.Code, got.Body)
	}
	got = request("POST", prefix+"/installations/"+install.ID+"/test", token, "", map[string]any{"tool": "lookup", "operationKey": "http-owned-setup-test", "arguments": map[string]string{"query": "requested"}})
	if got.Code != 200 || !strings.Contains(got.Body.String(), "Actual HTTP setup test response") || hits.Load() != 1 {
		t.Fatalf("real HTTP test %d %s", got.Code, got.Body)
	}
	var wrenID string
	if err = s.pool.QueryRow(ctx, `SELECT w.id FROM wrens w JOIN wren_room_wrens rw ON rw.wren_id=w.id JOIN wren_rooms room ON room.id=rw.room_id WHERE room.conversation_id=$1 LIMIT 1`, shared.ID).Scan(&wrenID); err != nil {
		t.Fatal(err)
	}
	routineInput := responsibilityInput(shared, Wren{ID: wrenID})
	routineInput.Instruction = "A private saved instruction not disclosed to a producer."
	routineInput.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "lead_received", TimeZone: "UTC"}
	responsibility, err := s.SaveWrenResponsibility(ctx, private.WorkspaceID, private.AccountID, routineInput)
	if err != nil {
		t.Fatal(err)
	}
	got = request("POST", prefix+"/installations/"+install.ID+"/events", token, "", WrenExtensionEventInput{ResponsibilityID: responsibility.ID, EventName: "lead_received", Enabled: true})
	if got.Code != 200 {
		t.Fatalf("HTTP event bind %d %s", got.Code, got.Body)
	}
	var binding struct {
		Binding WrenExtensionEventBinding `json:"binding"`
		Token   string                    `json:"token"`
	}
	if err = json.Unmarshal(got.Body.Bytes(), &binding); err != nil || binding.Token == "" {
		t.Fatalf("event setup %v", err)
	}
	eventPath := "/wren-extension-events/" + binding.Binding.ID
	got = request("POST", eventPath, "", binding.Token, map[string]any{"eventId": "http-producer-delivery-001", "eventName": "lead_received", "data": map[string]string{"lead": "owned QA"}})
	if got.Code != 202 || strings.Contains(got.Body.String(), routineInput.Instruction) || strings.Contains(got.Body.String(), "instruction") || !strings.Contains(got.Body.String(), "receiptId") {
		t.Fatalf("producer got private instruction or no receipt %d %s", got.Code, got.Body)
	}
	got = request("POST", eventPath, "", "wrong_producer_credential_123456", map[string]string{"eventId": "delivery-002", "eventName": "lead_received"})
	if got.Code != 403 {
		t.Fatalf("unauthenticated ingress %d %s", got.Code, got.Body)
	}
}
func TestWrenExtensionBuilderInstallInSecondWorkspaceWithoutPrivateInheritance(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answer":"Actual second workspace result"}`))
	}))
	defer server.Close()
	h, private, _, recipient, source := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	var receiverWorkspace string
	if err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM memberships WHERE account_id=$1 AND role='owner' AND workspace_id<>$2 LIMIT 1`, recipient, private.WorkspaceID).Scan(&receiverWorkspace); err != nil {
		t.Fatal(err)
	}
	_, invite, err := s.CreateWorkspaceInvite(ctx, receiverWorkspace, recipient, WorkspaceInviteInput{Email: "operator@example.com", Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeWorkspaceInvite(ctx, private.AccountID, invite, true); err != nil {
		t.Fatal(err)
	}
	// Explicit manifest import is configuration only; the other workspace owns
	// its own installation, identity, credentials and outcomes.
	imported, err := s.saveWrenExtension(ctx, receiverWorkspace, private.AccountID, WrenExtensionInput{ID: "extension_receiver_import", Manifest: source.Extension.Manifest}, h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	wren, err := s.SaveWren(ctx, receiverWorkspace, recipient, Wren{ID: "wren_receiver_private", Name: "Clinic", Shape: "circle"}, true)
	if err != nil {
		t.Fatal(err)
	}
	scope := WrenKnowledgeScope{ConversationID: wren.ConversationID, Scope: "private", WrenID: wren.ID}
	input := WrenExtensionInstallationInput{ID: "extinstall_receiver_owned", WrenKnowledgeScope: scope, ExtensionID: imported.ID, Version: 1, Enabled: true, CredentialMode: "personal"}
	if _, err = s.InstallWrenExtension(ctx, receiverWorkspace, private.AccountID, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("builder imported access to receiver private bot %v", err)
	}
	install, err := s.InstallWrenExtension(ctx, receiverWorkspace, recipient, input)
	if err != nil {
		t.Fatal(err)
	}
	call, err := h.TestWrenExtensionTool(ctx, receiverWorkspace, recipient, install.ID, "lookup", "second-workspace-first-use", json.RawMessage(`{"query":"authorized clinic record"}`))
	if err != nil || call.Status != "completed" || hits.Load() != 1 || !strings.Contains(string(call.Result), "Actual second workspace result") {
		t.Fatalf("second developer first use %+v %v", call, err)
	}
	if _, err = s.WrenExtensionInstallation(ctx, receiverWorkspace, private.AccountID, install.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("catalog publisher can read private receiver install %v", err)
	}
}
func TestWrenExtensionUnknownCallCannotBeRetriedUnderNewModelCallID(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"uncertain"}`))
	}))
	defer server.Close()
	h, _, shared, _, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	if _, err := s.EnqueueOperator(ctx, shared, "unknown-model-operation", "Read the authorized service record", "test", nil); err != nil {
		t.Fatal(err)
	}
	run, ok, err := s.claimOperator(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	name := extensionDynamicName(install.ID, 1, "lookup")
	first, err := h.InvokeWrenExtensionTool(ctx, shared, run, name, json.RawMessage(`{"query":"same"}`), "model_call_001")
	if err != nil {
		t.Fatal(err)
	}
	next, err := h.InvokeWrenExtensionTool(ctx, shared, run, name, json.RawMessage(`{"query":"same"}`), "model_call_002")
	if err != nil {
		t.Fatal(err)
	}
	if first.(WrenExtensionCall).ID != next.(WrenExtensionCall).ID || hits.Load() != 1 {
		t.Fatalf("model bypassed uncertain receipt with fresh ID hits=%d", hits.Load())
	}
}
func TestWrenExtensionEventBackpressureDoesNotCreateUnboundedWakeQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	h, private, shared, _, install := extensionFixture(t, server.URL, "none")
	s := h.service.Store
	ctx := context.Background()
	var wrenID string
	if err := s.pool.QueryRow(ctx, `SELECT wren_id FROM wren_room_wrens WHERE room_id=(SELECT id FROM wren_rooms WHERE conversation_id=$1) LIMIT 1`, shared.ID).Scan(&wrenID); err != nil {
		t.Fatal(err)
	}
	input := responsibilityInput(shared, Wren{ID: wrenID})
	input.Schedule = WrenResponsibilitySchedule{Kind: "event", EventName: "lead_received", TimeZone: "UTC"}
	r, err := s.SaveWrenResponsibility(ctx, private.WorkspaceID, private.AccountID, input)
	if err != nil {
		t.Fatal(err)
	}
	binding, token, err := s.BindWrenExtensionEvent(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionEventInput{ResponsibilityID: r.ID, EventName: "lead_received", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, token, id, "lead_received", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, token, "five", "lead_received", nil); !errors.Is(err, ErrCapacity) {
		t.Fatalf("event source bypassed backpressure %v", err)
	}
	if _, err = s.AcceptWrenExtensionEvent(ctx, binding.ID, token, "one", "lead_received", nil); err != nil {
		t.Fatalf("duplicate receipt stopped being idempotent under backpressure %v", err)
	}
}
