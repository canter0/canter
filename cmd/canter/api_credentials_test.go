package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func isolateAPICredentials(t *testing.T) {
	t.Helper()
	t.Setenv("CANTER_API_KEY", "")
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "")
	t.Setenv("CANTER_API_URL", "")
	t.Setenv("CANTER_WORKSPACE_ID", "")
}

func TestResolveAPICredentialPrecedence(t *testing.T) {
	isolateAPICredentials(t)
	values := map[string]string{
		"CANTER_API_KEY":            "file-key",
		"CANTER_AGENT_ACCESS_TOKEN": "file-agent",
	}
	// File-only resolution prefers the personal key over the agent token.
	token, personal, err := resolveAPICredential(values)
	if err != nil || token != "file-key" || !personal {
		t.Fatalf("file precedence %q %v %v", token, personal, err)
	}
	// Ambient entries beat every file value, in personal-then-agent order.
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "ambient-agent")
	token, personal, err = resolveAPICredential(values)
	if err != nil || token != "ambient-agent" || personal {
		t.Fatalf("ambient agent %q %v %v", token, personal, err)
	}
	t.Setenv("CANTER_API_KEY", "ambient-key")
	token, personal, err = resolveAPICredential(values)
	if err != nil || token != "ambient-key" || !personal {
		t.Fatalf("ambient key %q %v %v", token, personal, err)
	}
	// Blank trimmed values are skipped entirely.
	t.Setenv("CANTER_API_KEY", "   ")
	token, personal, err = resolveAPICredential(values)
	if err != nil || token != "ambient-agent" || personal {
		t.Fatalf("blank ambient key %q %v %v", token, personal, err)
	}
}

func TestResolveAPICredentialRejectsMalformedWithoutEcho(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_API_KEY", "token with\nnewline")
	_, _, err := resolveAPICredential(nil)
	if !errors.Is(err, errMalformedAPICredential) {
		t.Fatalf("malformed error %v", err)
	}
	if strings.Contains(err.Error(), "newline") {
		t.Fatalf("error echoed token: %v", err)
	}
	t.Setenv("CANTER_API_KEY", "")
	if _, _, err := resolveAPICredential(nil); !errors.Is(err, errNoAPICredential) {
		t.Fatalf("missing credential %v", err)
	}
}

func TestResolvePersonalAPICredentialRejectsAgentToken(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "agent-only")
	if _, err := resolvePersonalAPICredential(nil); err == nil || strings.Contains(err.Error(), "agent-only") {
		t.Fatalf("agent token accepted or echoed: %v", err)
	}
}

func TestResolvePersonalAPICredentialIgnoresAgentToken(t *testing.T) {
	isolateAPICredentials(t)
	// An ambient agent token must not mask an explicit file personal key.
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "ambient-agent")
	token, err := resolvePersonalAPICredential(map[string]string{"CANTER_API_KEY": "file-key"})
	if err != nil || token != "file-key" {
		t.Fatalf("file key under ambient agent %q %v", token, err)
	}
	// A malformed ambient key is a hard error, never a silent fallback.
	t.Setenv("CANTER_API_KEY", "bad key")
	if _, err := resolvePersonalAPICredential(map[string]string{"CANTER_API_KEY": "file-key"}); !errors.Is(err, errMalformedAPICredential) {
		t.Fatalf("malformed ambient key %v", err)
	}
	t.Setenv("CANTER_API_KEY", "")
	// A malformed file key is also a hard error with no agent fallback.
	if _, err := resolvePersonalAPICredential(map[string]string{"CANTER_API_KEY": "bad\tkey"}); !errors.Is(err, errMalformedAPICredential) {
		t.Fatalf("malformed file key %v", err)
	}
	// Non-ASCII values are not usable HTTP bearer tokens either.
	if _, err := resolvePersonalAPICredential(map[string]string{"CANTER_API_KEY": "key-f\u00fcne"}); !errors.Is(err, errMalformedAPICredential) {
		t.Fatalf("non-ascii file key %v", err)
	}
}

func TestAuthWhoAmIUsesPersonalKey(t *testing.T) {
	isolateAPICredentials(t)
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, ciCLIRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/me" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account":            map[string]any{"id": "usr_1", "email": "a@example.com", "createdAt": "2026-01-01T00:00:00Z"},
				"workspaces":         []map[string]any{{"id": "wrk_1", "name": "default", "role": "owner"}},
				"onboardingComplete": true,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	t.Setenv("CANTER_API_KEY", "canter_pk_testkey")
	out, err := captureCIStdout(t, func() error {
		return authCommand([]string{"whoami", "--api-url", server.URL})
	})
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if strings.Contains(out, "canter_pk_testkey") {
		t.Fatal("key leaked to stdout")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output not JSON: %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].path != "/v1/me" || requests[0].auth != "Bearer canter_pk_testkey" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestAuthWhoAmIRejectsAgentToken(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "agent-token")
	if err := authCommand([]string{"whoami", "--api-url", "http://127.0.0.1:1"}); err == nil {
		t.Fatal("agent token must not satisfy auth whoami")
	}
}

func TestSandboxClientPersonalKeyRequiresExplicitWorkspace(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_API_KEY", "canter_pk_testkey")
	t.Setenv("CANTER_API_URL", "http://127.0.0.1:1")
	f := newSandboxFlags("list")
	if err := f.parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client(t.Context()); err == nil || !strings.Contains(err.Error(), "CANTER_WORKSPACE_ID") {
		t.Fatalf("personal key without workspace: %v", err)
	}
}

func TestSandboxClientPersonalKeyUsesExplicitWorkspace(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_API_KEY", "canter_pk_testkey")
	f := newSandboxFlags("list")
	if err := f.parse([]string{"--api-url", "http://127.0.0.1:1", "--workspace", "wrk_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client(t.Context()); err != nil {
		t.Fatalf("personal key with workspace: %v", err)
	}
}

func TestSandboxClientAgentTokenStillInfersWorkspace(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "agent-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/agent/whoami" && r.Header.Get("Authorization") == "Bearer agent-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"installation": map[string]any{"id": "ins_1", "workspaceId": "wrk_inferred"},
				"session":      map[string]any{"id": "ses_1"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	f := newSandboxFlags("list")
	if err := f.parse([]string{"--api-url", server.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client(t.Context()); err != nil {
		t.Fatalf("agent token inference: %v", err)
	}
}

func TestCIStatusAcceptsPersonalAPIKey(t *testing.T) {
	isolateAPICredentials(t)
	t.Setenv("CANTER_API_KEY", "canter_pk_ci")
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--api-url", server.URL})
	}); err != nil {
		t.Fatalf("ci status with personal key: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].auth != "Bearer canter_pk_ci" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestAgentHelpNeverPrintsAmbientToken(t *testing.T) {
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "agent-secret-sentinel")
	t.Setenv("CANTER_AGENT_REFRESH_TOKEN", "refresh-secret-sentinel")
	for _, args := range [][]string{
		{"whoami", "--help"},
		{"refresh", "--help"},
	} {
		stdout, stderr, err := captureCIOutput(t, func() error { return agentCommand(args) })
		_ = stderr
		if err == nil {
			// --help under flag.ContinueOnError returns flag.ErrHelp.
		}
		if strings.Contains(stdout, "sentinel") || strings.Contains(stderr, "sentinel") {
			t.Fatalf("ambient credential leaked in help for %v", args)
		}
	}
}
