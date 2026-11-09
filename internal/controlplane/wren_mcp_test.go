package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWrenMCPDiscoveryAndRealTransport(t *testing.T) {
	for _, jsonResponse := range []bool{false, true} {
		t.Run(fmt.Sprint("json=", jsonResponse), func(t *testing.T) {
			var calls atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "Test MCP", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "Lookup.Record", Description: "Read a record", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Actual result for " + string(r.Params.Arguments)}}}, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: jsonResponse})
			remote := httptest.NewServer(handler)
			defer remote.Close()
			config := WrenExtensionConfig{PreviewOrigins: []string{remote.URL}}
			discovery, err := discoverExtensionMCP(context.Background(), config, remote.URL+"/mcp", "")
			if err != nil || len(discovery.Tools) != 1 {
				t.Fatalf("discovery %+v %v", discovery, err)
			}
			tool := discovery.Tools[0]
			if discovery.Auth != "none" || tool.Impact != "write" || tool.RemoteName != "Lookup.Record" || !extensionName(tool.Name) || calls.Load() != 0 {
				t.Fatal("discovery executed or trusted read annotation")
			}
			client, closeClient, err := extensionHTTPClient(context.Background(), config, tool.Endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer closeClient()
			raw, status, outcome, failure := performWrenMCP(context.Background(), client, tool, json.RawMessage(`{"query":"hello"}`), "")
			if outcome != "completed" || failure != "" || status != 200 || !strings.Contains(string(raw), "hello") || calls.Load() != 1 {
				t.Fatalf("real transport: %s %d %s %s calls=%d", raw, status, outcome, failure, calls.Load())
			}
		})
	}
}

func TestWrenMCPRoomCredentialsReceiptsAndRevocation(t *testing.T) {
	var hits atomic.Int32
	bearers := make(chan string, 32)
	server := mcp.NewServer(&mcp.Implementation{Name: "Credential test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "lookup", Description: "Lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hits.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "result contains member_test_bearer_987654"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearers <- r.Header.Get("Authorization")
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	h, private, _, member, install := extensionFixture(t, remote.URL, "bearer")
	ctx := context.Background()
	s := h.service.Store
	manifest := install.Extension.Manifest
	manifest.Tools = []WrenExtensionTool{{Name: "lookup", Method: "MCP", Description: "Read record", Endpoint: remote.URL + "/mcp", Impact: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	extension, err := s.saveWrenExtension(ctx, private.WorkspaceID, private.AccountID, WrenExtensionInput{ID: install.ExtensionID, ExpectedVersion: 1, Manifest: manifest}, h.config.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	edit := extensionInstallEdit(install)
	edit.Version = extension.Version
	install, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, edit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, private.AccountID, install.ID, WrenExtensionCredentialInput{Value: "owner_test_bearer_123456"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "no-own-token", json.RawMessage(`{}`)); err == nil || hits.Load() != 0 {
		t.Fatal("peer inherited owner's token")
	}
	_, err = h.ConfigureWrenExtensionCredential(ctx, private.WorkspaceID, member, install.ID, WrenExtensionCredentialInput{Value: "member_test_bearer_987654"})
	if err != nil {
		t.Fatal(err)
	}
	call, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "mcp-real-call", json.RawMessage(`{}`))
	if err != nil || call.Status != "completed" || hits.Load() != 1 || strings.Contains(string(call.Result), "member_test_bearer") || !strings.Contains(string(call.Result), "[redacted]") {
		t.Fatalf("call %+v %v", call, err)
	}
	for len(bearers) > 0 {
		if <-bearers != "Bearer member_test_bearer_987654" {
			t.Fatal("wrong requester credential")
		}
	}
	replay, err := h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "mcp-real-call", json.RawMessage(`{}`))
	if err != nil || replay.ID != call.ID || hits.Load() != 1 {
		t.Fatal("duplicate provider invocation")
	}
	edit = extensionInstallEdit(install)
	edit.Revoke = true
	if _, err = s.InstallWrenExtension(ctx, private.WorkspaceID, private.AccountID, edit); err != nil {
		t.Fatal(err)
	}
	if _, err = h.TestWrenExtensionTool(ctx, private.WorkspaceID, member, install.ID, "lookup", "after-revoke", json.RawMessage(`{}`)); err == nil || hits.Load() != 1 {
		t.Fatal("revoked grant invoked MCP")
	}
}

func TestWrenMCPLimitsRedirectsAndWriteUncertainty(t *testing.T) {
	if _, err := discoverExtensionMCP(context.Background(), WrenExtensionConfig{}, "https://127.0.0.1/mcp", ""); err == nil {
		t.Fatal("private address allowed")
	}
	body := &extensionMCPBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 20))), remaining: 10}
	raw, err := io.ReadAll(body)
	if err == nil || len(raw) != 10 {
		t.Fatal("response limit not enforced")
	}
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "broken", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var req struct {
			Method string `json:"method"`
		}
		json.Unmarshal(raw, &req)
		if req.Method == "tools/call" {
			calls.Add(1)
			w.WriteHeader(502)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	config := WrenExtensionConfig{PreviewOrigins: []string{remote.URL}}
	client, closeClient, err := extensionHTTPClient(context.Background(), config, remote.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	for _, impact := range []string{"read", "write"} {
		_, _, status, _ := performWrenMCP(context.Background(), client, WrenExtensionTool{Name: "fail", Endpoint: remote.URL + "/mcp", Impact: impact}, json.RawMessage(`{}`), "")
		want := "failed"
		if impact == "write" {
			want = "unknown"
		}
		if status != want {
			t.Fatalf("%s outcome %s", impact, status)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected retry: %d", calls.Load())
	}
	transport := &extensionMCPTransport{base: http.DefaultTransport, endpoint: remote.URL + "/mcp", credential: "secret"}
	request, _ := http.NewRequest("POST", remote.URL+"/other", nil)
	if _, err = transport.RoundTrip(request); err == nil {
		t.Fatal("changed destination accepted")
	}
}

func TestWrenMCPLiveDeepWiki(t *testing.T) {
	if os.Getenv("CANTER_TEST_LIVE_MCP") != "1" {
		t.Skip("opt-in live provider check")
	}
	ctx := context.Background()
	discovery, err := discoverExtensionMCP(ctx, WrenExtensionConfig{}, "https://mcp.deepwiki.com/mcp", "")
	if err != nil || len(discovery.Tools) < 1 {
		t.Fatalf("live discovery: %+v %v", discovery, err)
	}
	var tool WrenExtensionTool
	for _, candidate := range discovery.Tools {
		if candidate.RemoteName == "read_wiki_structure" {
			tool = candidate
		}
	}
	if tool.Name == "" {
		t.Fatal("live server no longer offers the expected tool")
	}
	tool.Impact = "read"
	client, closeClient, err := extensionHTTPClient(ctx, WrenExtensionConfig{}, tool.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	result, httpStatus, status, failure := performWrenMCP(ctx, client, tool, json.RawMessage(`{"repoName":"modelcontextprotocol/go-sdk"}`), "")
	if status != "completed" || httpStatus != 200 || len(result) < 50 {
		t.Fatalf("live call: %s HTTP %d %s", status, httpStatus, failure)
	}
	t.Logf("Live DeepWiki: %d tools, HTTP %d, %d result bytes", len(discovery.Tools), httpStatus, len(result))
	for _, entry := range wrenConnectorCatalog() {
		if entry.Slug == "deepwiki" {
			questionTool := entry.Manifest.Tools[1]
			questionResult, code, outcome, detail := performWrenMCP(ctx, client, questionTool, json.RawMessage(`{"repoName":"modelcontextprotocol/go-sdk","question":"In two sentences, what is the purpose of the mcp package?"}`), "")
			if outcome != "completed" || code != 200 || len(questionResult) < 50 {
				t.Fatalf("live catalog question: %s HTTP %d %s", outcome, code, detail)
			}
			t.Logf("Live catalog question: HTTP %d, %d result bytes", code, len(questionResult))
		}
	}
}

func TestWrenMCPDiscoveryRedactsEchoedCredential(t *testing.T) {
	token := "write_only_bearer_123456"
	value := wrenMCPDiscovery{Name: token, Tools: []WrenExtensionTool{{Description: "echo " + token, InputSchema: json.RawMessage(`{"type":"object","description":"write_only_bearer_123456","properties":{"password":{"type":"string"}}}`)}}, Warnings: []string{token}}
	raw, _ := json.Marshal(redactMCPDiscovery(value, token))
	if strings.Contains(string(raw), token) || !strings.Contains(string(raw), "[redacted]") {
		t.Fatal("discovery echoed credential")
	}
	if !strings.Contains(string(raw), `"password":{"type":"string"}`) {
		t.Fatal("schema property definition was destroyed")
	}
}
