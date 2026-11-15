package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	ciCLIFixtureOverview = `{"enabled":true,"pool":"offline","settings":{"defaultSize":"linux-x64-2"},"installations":[],"jobs":[{"id":"job_1","status":"succeeded"}],"features":["overview","jobs"],"futureField":{"keep":"me"}}`
	ciCLIFixtureJob      = `{"job":{"id":"job_1","status":"succeeded"},"steps":[{"name":"build","status":"ok"}],"logs":"line one\nline two","metrics":{"durationMs":1234},"tests":[{"name":"unit","status":"pass"}],"futureField":"preserved"}`
)

type ciCLIRequest struct {
	method string
	path   string
	auth   string
}

func newCICommandServer(t *testing.T, requests *[]ciCLIRequest, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*requests = append(*requests, ciCLIRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.URL.Path == "/v1/workspaces/wrk_1/ci":
			_, _ = w.Write([]byte(ciCLIFixtureOverview))
		case r.URL.Path == "/v1/workspaces/wrk_1/ci/jobs/job_1":
			_, _ = w.Write([]byte(ciCLIFixtureJob))
		case strings.HasPrefix(r.URL.Path, "/v1/workspaces/wrk_denied/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"denied"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	}))
}

func captureCIOutput(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()
	previousOut, previousErr := os.Stdout, os.Stderr
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outWriter, errWriter
	done := make(chan [2]string, 1)
	go func() {
		stdout, _ := io.ReadAll(outReader)
		stderr, _ := io.ReadAll(errReader)
		done <- [2]string{string(stdout), string(stderr)}
	}()
	runErr := fn()
	_ = outWriter.Close()
	_ = errWriter.Close()
	os.Stdout, os.Stderr = previousOut, previousErr
	captured := <-done
	_ = outReader.Close()
	_ = errReader.Close()
	return captured[0], captured[1], runErr
}

func captureCIStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	previous := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	done := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(reader)
		done <- string(raw)
	}()
	runErr := fn()
	_ = writer.Close()
	os.Stdout = previous
	out := <-done
	_ = reader.Close()
	return out, runErr
}

func isolateCIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "")
	t.Setenv("CANTER_API_URL", "")
}

func TestCIStatusPrintsOverviewObject(t *testing.T) {
	isolateCIEnv(t)
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	out, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--api-url", server.URL, "--access-token", "tok-status-secret"})
	})
	if err != nil {
		t.Fatalf("ci status: %v", err)
	}
	if strings.Contains(out, "tok-status-secret") {
		t.Fatal("access token leaked to stdout")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout is not a JSON object: %q", out)
	}
	for _, field := range []string{"enabled", "pool", "settings", "jobs", "features", "futureField"} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("overview output dropped field %q: %s", field, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].method != http.MethodGet ||
		requests[0].path != "/v1/workspaces/wrk_1/ci" || requests[0].auth != "Bearer tok-status-secret" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestCIInspectPrintsJobDetail(t *testing.T) {
	isolateCIEnv(t)
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	out, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"inspect", "--workspace", "wrk_1", "--job", "job_1", "--api-url", server.URL, "--access-token", "tok-inspect"})
	})
	if err != nil {
		t.Fatalf("ci inspect: %v", err)
	}
	if strings.Contains(out, "tok-inspect") {
		t.Fatal("access token leaked to stdout")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout is not a JSON object: %q", out)
	}
	for _, field := range []string{"job", "steps", "logs", "metrics", "tests", "futureField"} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("job output dropped field %q: %s", field, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].method != http.MethodGet ||
		requests[0].path != "/v1/workspaces/wrk_1/ci/jobs/job_1" || requests[0].auth != "Bearer tok-inspect" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestCICommandRejectsUsageErrorsBeforeAnyRequest(t *testing.T) {
	isolateCIEnv(t)
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	cases := [][]string{
		{"status"},
		{"status", "--api-url", server.URL, "--access-token", "x"},
		{"status", "--workspace", "wrk_1", "--job", "job_1", "--api-url", server.URL, "--access-token", "x"},
		{"status", "--workspace", "wrk_1", "extra", "--api-url", server.URL, "--access-token", "x"},
		{"status", "--workspace", "wrk_1", "--bogus", "--api-url", server.URL},
		{"inspect", "--workspace", "wrk_1", "--api-url", server.URL, "--access-token", "x"},
		{"inspect", "--job", "job_1", "--api-url", server.URL, "--access-token", "x"},
		{"inspect", "--workspace", "wrk_1", "--job", "job_1", "extra", "--api-url", server.URL, "--access-token", "x"},
		{"bogus", "--api-url", server.URL},
		{"status", "--workspace", "..", "--api-url", server.URL, "--access-token", "x"},
	}
	for _, args := range cases {
		if _, err := captureCIStdout(t, func() error { return ciCommand(args) }); err == nil {
			t.Fatalf("ciCommand(%v) succeeded", args)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("usage errors reached the network: %+v", requests)
	}
}

func TestCIHelpRequiresNoCredentialsOrEnvFile(t *testing.T) {
	isolateCIEnv(t)
	t.Chdir(t.TempDir())
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	for _, args := range [][]string{
		{}, {"help"}, {"--help"}, {"-h"},
		{"status", "--help"}, {"status", "help"}, {"inspect", "-h"},
		{"status", "--workspace", "wrk_1", "--help"},
	} {
		out, err := captureCIStdout(t, func() error { return ciCommand(args) })
		if err != nil {
			t.Fatalf("ciCommand(%v): %v", args, err)
		}
		if !strings.Contains(out, "usage: canter ci") {
			t.Fatalf("ciCommand(%v) printed no usage: %q", args, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("help issued requests: %+v", requests)
	}
}

func TestCIEnvFileSuppliesCredentialsReadOnly(t *testing.T) {
	isolateCIEnv(t)
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	envPath := filepath.Join(t.TempDir(), "agent.env")
	envBody := "CANTER_API_URL=" + server.URL + "\nCANTER_AGENT_ACCESS_TOKEN=env-file-token\n"
	if err := os.WriteFile(envPath, []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--env-file", envPath})
	}); err != nil {
		t.Fatalf("ci status with env-file: %v", err)
	}
	mu.Lock()
	if len(requests) != 1 || requests[0].auth != "Bearer env-file-token" || requests[0].path != "/v1/workspaces/wrk_1/ci" {
		t.Fatalf("env-file credentials not honored: %+v", requests)
	}
	mu.Unlock()
	after, err := os.ReadFile(envPath)
	if err != nil || string(after) != string(before) {
		t.Fatal("env-file was modified")
	}
	info, err := os.Stat(envPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env-file mode changed: %v", err)
	}
}

func TestCIExplicitFlagsBeatEnvFileValues(t *testing.T) {
	isolateCIEnv(t)
	var primary []ciCLIRequest
	var secondary []ciCLIRequest
	var mu sync.Mutex
	primaryServer := newCICommandServer(t, &primary, &mu)
	defer primaryServer.Close()
	secondaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		secondary = append(secondary, ciCLIRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(ciCLIFixtureOverview))
	}))
	defer secondaryServer.Close()
	envPath := filepath.Join(t.TempDir(), "agent.env")
	envBody := "CANTER_API_URL=" + primaryServer.URL + "\nCANTER_AGENT_ACCESS_TOKEN=env-file-token\n"
	if err := os.WriteFile(envPath, []byte(envBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--env-file", envPath,
			"--api-url", secondaryServer.URL, "--access-token", "flag-token"})
	}); err != nil {
		t.Fatalf("ci status: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(primary) != 0 {
		t.Fatalf("env-file API URL used despite explicit --api-url: %+v", primary)
	}
	if len(secondary) != 1 || secondary[0].auth != "Bearer flag-token" {
		t.Fatalf("explicit flags did not win: %+v", secondary)
	}
}

func TestCIHelpNeverPrintsAmbientToken(t *testing.T) {
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "canter-secret-sentinel")
	t.Setenv("CANTER_API_URL", "")
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	for _, args := range [][]string{
		{"status", "--workspace", "wrk_1", "--help"},
		{"inspect", "--workspace", "wrk_1", "--job", "job_1", "--help"},
		{"status", "--workspace", "wrk_1", "--api-url", server.URL, "--access-token", "explicit", "--help"},
	} {
		stdout, stderr, err := captureCIOutput(t, func() error { return ciCommand(args) })
		if err != nil {
			t.Fatalf("ciCommand(%v): %v", args, err)
		}
		if strings.Contains(stdout, "canter-secret-sentinel") || strings.Contains(stderr, "canter-secret-sentinel") {
			t.Fatalf("ambient token leaked in help output for %v", args)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("help issued requests: %+v", requests)
	}
}

func TestCIAmbientTokenResolvesOnlyWhenFlagUnset(t *testing.T) {
	t.Setenv("CANTER_AGENT_ACCESS_TOKEN", "ambient-token")
	t.Setenv("CANTER_API_URL", "")
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--api-url", server.URL})
	}); err != nil {
		t.Fatalf("ambient token status: %v", err)
	}
	mu.Lock()
	if len(requests) != 1 || requests[0].auth != "Bearer ambient-token" {
		t.Fatalf("ambient token not used: %+v", requests)
	}
	mu.Unlock()
	requests = nil
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--api-url", server.URL, "--access-token", "flag-token"})
	}); err != nil {
		t.Fatalf("explicit token status: %v", err)
	}
	mu.Lock()
	if len(requests) != 1 || requests[0].auth != "Bearer flag-token" {
		t.Fatalf("explicit token did not beat ambient: %+v", requests)
	}
	mu.Unlock()
	envPath := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(envPath, []byte("CANTER_AGENT_ACCESS_TOKEN=file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests = nil
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--api-url", server.URL,
			"--access-token", "", "--env-file", envPath})
	}); err != nil {
		t.Fatalf("explicit empty token with env-file: %v", err)
	}
	mu.Lock()
	if len(requests) != 1 || requests[0].auth != "Bearer file-token" {
		t.Fatalf("explicit empty token should fall back to env-file, not ambient: %+v", requests)
	}
	mu.Unlock()
	requests = nil
	if _, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_1", "--api-url", server.URL, "--access-token", ""})
	}); err == nil {
		t.Fatal("explicit empty token used ambient credential")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("rejected credential still issued a request: %+v", requests)
	}
}

func TestCIPropagatesAPIErrorWithoutLeakingCredentials(t *testing.T) {
	isolateCIEnv(t)
	var requests []ciCLIRequest
	var mu sync.Mutex
	server := newCICommandServer(t, &requests, &mu)
	defer server.Close()
	_, err := captureCIStdout(t, func() error {
		return ciCommand([]string{"status", "--workspace", "wrk_denied", "--api-url", server.URL, "--access-token", "secret-token-xyz"})
	})
	if err == nil {
		t.Fatal("403 response reported success")
	}
	if strings.Contains(err.Error(), "secret-token-xyz") {
		t.Fatalf("access token leaked in error: %v", err)
	}
	_, err = captureCIStdout(t, func() error {
		return ciCommand([]string{"inspect", "--workspace", "wrk_1", "--job", "missing", "--api-url", server.URL, "--access-token", "secret-token-xyz"})
	})
	if err == nil {
		t.Fatal("404 response reported success")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, req := range requests {
		if req.method != http.MethodGet {
			t.Fatalf("non-GET request issued: %+v", req)
		}
	}
}
