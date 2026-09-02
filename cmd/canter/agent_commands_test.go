package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk/remote"
)

func TestAgentExchangePersistsOnlyWithExplicitEnvFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/device/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(remote.TokenPair{AccessToken: "access-secret", RefreshToken: "refresh-secret", Installation: remote.Installation{ID: "agt_test"}, Session: remote.Session{ID: "ass_test"}})
	}))
	defer server.Close()
	directory := t.TempDir()
	prior, _ := os.Getwd()
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prior) })
	if err := agentCommand([]string{"exchange", "--api-url", server.URL, "--device-code", "device"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("agent exchange wrote files without --env-file: %#v", entries)
	}
	envPath := filepath.Join(directory, "credentials", "agent.env")
	if err := agentCommand([]string{"exchange", "--api-url", server.URL, "--device-code", "device", "--env-file", envPath}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, expected := range []string{"CANTER_API_URL=", "CANTER_AGENT_ACCESS_TOKEN='access-secret'", "CANTER_AGENT_REFRESH_TOKEN='refresh-secret'", "CANTER_AGENT_INSTALLATION_ID='agt_test'", "CANTER_MCP_URL="} {
		if !strings.Contains(content, expected) {
			t.Fatalf("credential file omitted %q: %s", expected, content)
		}
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode is %o", info.Mode().Perm())
	}
}

func TestPersistAgentEnvPreservesUnrelatedValuesAndRotatesManagedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(path, []byte("KEEP='yes'\nCANTER_AGENT_ACCESS_TOKEN='old'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistAgentEnv(path, map[string]string{"CANTER_AGENT_ACCESS_TOKEN": "new", "CANTER_AGENT_REFRESH_TOKEN": "refresh"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "KEEP='yes'") || !strings.Contains(content, "CANTER_AGENT_ACCESS_TOKEN='new'") || strings.Contains(content, "'old'") {
		t.Fatalf("unexpected updated env: %s", content)
	}
	if strings.Count(content, "CANTER_AGENT_ACCESS_TOKEN=") != 1 {
		t.Fatalf("managed value duplicated: %s", content)
	}
}

func TestLegacyAgentEnvIsReadAndMigratedWithoutDuplicatingSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(path, []byte("KEEP='yes'\nCANTER_ACCESS_TOKEN='legacy-access'\nCANTER_REFRESH_TOKEN='legacy-refresh'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := explicitEnvValues(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["CANTER_AGENT_ACCESS_TOKEN"] != "legacy-access" || values["CANTER_AGENT_REFRESH_TOKEN"] != "legacy-refresh" {
		t.Fatalf("legacy credentials were not normalized: %#v", values)
	}
	if err := persistAgentEnv(path, map[string]string{
		"CANTER_AGENT_ACCESS_TOKEN":  "current-access",
		"CANTER_AGENT_REFRESH_TOKEN": "current-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, stale := range []string{"CANTER_ACCESS_TOKEN=", "CANTER_REFRESH_TOKEN=", "legacy-access", "legacy-refresh"} {
		if strings.Contains(content, stale) {
			t.Fatalf("legacy credential remained after migration: %s", content)
		}
	}
	if !strings.Contains(content, "KEEP='yes'") || !strings.Contains(content, "CANTER_AGENT_ACCESS_TOKEN='current-access'") || !strings.Contains(content, "CANTER_AGENT_REFRESH_TOKEN='current-refresh'") {
		t.Fatalf("unexpected migrated env: %s", content)
	}
}

func TestPersistAgentEnvRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "agent.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := persistAgentEnv(link, map[string]string{"CANTER_AGENT_ACCESS_TOKEN": "secret"}); err == nil {
		t.Fatal("credential writer followed a symlink")
	}
}
