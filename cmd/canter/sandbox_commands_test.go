package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// sandboxCLIEnv points the CLI at a test server via env vars.
func sandboxCLIEnv(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Setenv("CANTER_API_URL", server.URL)
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "token_test")
	t.Setenv("CANTER_WORKSPACE_ID", "wrk_1")
}

func TestSandboxCommandRequiresSubcommandAndFlags(t *testing.T) {
	if err := sandboxCommand(nil); err == nil {
		t.Fatal("missing subcommand must fail")
	}
	if err := sandboxCommand([]string{"bogus"}); err == nil {
		t.Fatal("unknown subcommand must fail")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer server.Close()
	sandboxCLIEnv(t, server)
	if err := sandboxCommand([]string{"get"}); err == nil {
		t.Fatal("get without --id must fail")
	}
	if err := sandboxCommand([]string{"exec", "--id", "sbx_1"}); err == nil {
		t.Fatal("exec without --command must fail")
	}
	if err := sandboxCommand([]string{"list", "extra-positional"}); err == nil {
		t.Fatal("positional args must be rejected")
	}
}

func TestSandboxCommandListAndCreate(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer token_test" {
			t.Error("missing bearer")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces/wrk_1/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []map[string]any{{"id": "sbx_1", "phase": "ready"}}, "enabled": true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces/wrk_1/sandboxes":
			body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sbx_2", "phase": "provisioning"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	sandboxCLIEnv(t, server)
	if err := sandboxCommand([]string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
	spec := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(spec, []byte(`{"name":"build","size":"s-2x4","idempotencyKey":"cli-key-1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sandboxCommand([]string{"create", "--file", spec}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal(body, &sent)
	if sent["idempotencyKey"] != "cli-key-1" {
		t.Fatalf("create body: %s", body)
	}
}

func TestSandboxCommandExecFollowsLogsAndNonzeroFails(t *testing.T) {
	exit := 7
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"jobId": "job_1", "status": "running"})
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"b3V0cHV0Cg==\",\"offset\":0,\"nextOffset\":7,\"eof\":false,\"status\":\"running\"}\n\n")
			status, _ := json.Marshal(map[string]any{"jobId": "job_1", "status": "completed", "exitCode": exit})
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	sandboxCLIEnv(t, server)
	// Nonzero job exit must return an error.
	err := sandboxCommand([]string{"exec", "--id", "sbx_1", "--command", "false"})
	if err == nil || !strings.Contains(err.Error(), "exited 7") {
		t.Fatalf("nonzero exit must fail: %v", err)
	}
	// --detach returns the job receipt only.
	if err := sandboxCommand([]string{"exec", "--id", "sbx_1", "--command", "true", "--detach"}); err != nil {
		t.Fatalf("detach: %v", err)
	}
}

func TestSandboxCommandDownloadAtomic(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files/content") {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	sandboxCLIEnv(t, server)
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := sandboxCommand([]string{"download", "--id", "sbx_1", "--path", "/workspace/f", "--file", dest}); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("download bytes: %v", err)
	}
	// Refuse to clobber without --force.
	if err := sandboxCommand([]string{"download", "--id", "sbx_1", "--path", "/workspace/f", "--file", dest}); err == nil {
		t.Fatal("existing file must require --force")
	}
	if err := sandboxCommand([]string{"download", "--id", "sbx_1", "--path", "/workspace/f", "--file", dest, "--force"}); err != nil {
		t.Fatalf("forced download: %v", err)
	}
}

func TestSandboxCommandRejectsUnknownJSONFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer server.Close()
	sandboxCLIEnv(t, server)
	spec := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(spec, []byte(`{"name":"x","size":"s-1x2","idempotencyKey":"key-1234","bogus":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sandboxCommand([]string{"create", "--file", spec}); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown field must fail without a request: %v", err)
	}
}

func TestSandboxCommandExplicitAPIURLBeatsEnvFile(t *testing.T) {
	var hit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}, "enabled": true})
	}))
	defer server.Close()
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("CANTER_API_URL=http://127.0.0.1:1\nCANTER_AGENT_ACCESS_TOKEN=tok\nCANTER_WORKSPACE_ID=wrk_1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANTER_API_URL", "")
	// Explicit --api-url equal to the default must NOT be overridden by the file.
	if err := sandboxCommand([]string{"list", "--api-url", server.URL, "--env-file", envFile}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hit.Load() {
		t.Fatal("explicit --api-url was overridden by env file")
	}
}

func TestSandboxCommandDownloadNoClobberRace(t *testing.T) {
	payload := []byte("data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.Write(payload)
	}))
	defer server.Close()
	sandboxCLIEnv(t, server)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	// Dangling symlink at the destination must not be replaced.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "nonexistent"), link); err != nil {
		t.Fatal(err)
	}
	if err := sandboxCommand([]string{"download", "--id", "sbx_1", "--path", "/workspace/f", "--file", link}); err == nil {
		t.Fatal("dangling symlink must not be replaced")
	}
	if target, err := os.Readlink(link); err != nil || target == "" {
		t.Fatal("symlink was clobbered")
	}
	// A destination created between the pre-check and publish is not replaced.
	// (sandboxDownloadFile's os.Link publish is atomic: simulate by creating the
	// file first — the same code path.)
	if err := os.WriteFile(dest, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sandboxCommand([]string{"download", "--id", "sbx_1", "--path", "/workspace/f", "--file", dest}); err == nil {
		t.Fatal("existing destination must not be clobbered")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "existing" {
		t.Fatal("destination contents were replaced")
	}
}

func TestSandboxCommandExecMissingExitCodeFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"jobId": "job_1", "status": "running"})
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: status\ndata: {\"jobId\":\"job_1\",\"status\":\"completed\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	sandboxCLIEnv(t, server)
	if err := sandboxCommand([]string{"exec", "--id", "sbx_1", "--command", "true"}); err == nil {
		t.Fatal("completed without explicit exit code must not succeed")
	}
}

func TestSandboxDownloadFileRaceCreatesDestDuringWrite(t *testing.T) {
	// The destination is created while the write callback runs — the atomic
	// os.Link publish must refuse to clobber it.
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	err := sandboxDownloadFile(dest, false, func(w io.Writer) error {
		if err := os.WriteFile(dest, []byte("other"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := w.Write([]byte("downloaded"))
		return err
	})
	if err == nil {
		t.Fatal("destination created during download must not be clobbered")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "other" {
		t.Fatalf("destination replaced: %q", got)
	}
}

func TestSandboxCommandExplicitURLSameAsEnvFile(t *testing.T) {
	// Explicit --api-url equal to the env default must NOT be overridden by a
	// bad env-file URL (the original bug compared the flag VALUE to default).
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}, "enabled": true})
	}))
	defer server.Close()
	t.Setenv("CANTER_API_URL", server.URL)
	envFile := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envFile, []byte("CANTER_API_URL=http://127.0.0.1:1\nCANTER_AGENT_ACCESS_TOKEN=tok\nCANTER_WORKSPACE_ID=wrk_1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "")
	// Explicit --api-url = env default value must win over the bad file URL.
	if err := sandboxCommand([]string{"list", "--api-url", server.URL, "--env-file", envFile}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("explicit --api-url was overridden by the env file")
	}
}

func TestSandboxCommandUnknownSpecNoNetwork(t *testing.T) {
	// An invalid spec must fail before any credential/WhoAmI request.
	var whoami atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "whoami") {
			whoami.Store(true)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	t.Setenv("CANTER_API_URL", server.URL)
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "tok")
	t.Setenv("CANTER_WORKSPACE_ID", "") // workspace unset — WhoAmI would resolve it
	spec := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(spec, []byte(`{"name":"x","size":"s-1x2","idempotencyKey":"key-1234","bogus":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sandboxCommand([]string{"create", "--file", spec}); err == nil {
		t.Fatal("unknown spec field must fail")
	}
	if whoami.Load() {
		t.Fatal("WhoAmI was called for an invalid spec")
	}
}

func TestSandboxCommandExecStdoutFailure(t *testing.T) {
	// A failing stdout must propagate through the log callback.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"jobId": "job_1", "status": "running"})
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: output\ndata: {\"jobId\":\"job_1\",\"stream\":\"combined\",\"dataBase64\":\"eA==\",\"offset\":0,\"nextOffset\":1,\"eof\":false,\"status\":\"running\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	sandboxCLIEnv(t, server)
	// Close stdout so the write fails inside the callback.
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	w.Close()
	defer func() { os.Stdout = old; r.Close() }()
	if err := sandboxCommand([]string{"exec", "--id", "sbx_1", "--command", "echo hi"}); err == nil {
		t.Fatal("stdout write failure must propagate")
	}
}
