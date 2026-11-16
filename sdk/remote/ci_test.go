package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	ciOverviewFixture = `{"enabled":true,"pool":"offline","settings":{"defaultSize":"linux-x64-2"},"installations":[],"jobs":[{"id":"job_1","status":"succeeded"}],"features":["overview","jobs"],"futureField":{"keep":["me",2]}}`
	ciJobFixture      = `{"job":{"id":"job_1","status":"succeeded","steps":2},"steps":[{"name":"build","status":"ok"}],"logs":"line one\nline two","metrics":{"durationMs":1234},"tests":[{"name":"unit","status":"pass"}],"futureField":"preserved"}`
)

type ciRequest struct {
	method string
	path   string
	auth   string
}

func newCIInspectionServer(t *testing.T, requests *[]ciRequest, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*requests = append(*requests, ciRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/workspaces/wrk_1/ci":
			_, _ = w.Write([]byte(ciOverviewFixture))
		case "/v1/workspaces/wrk_1/ci/jobs/job_1":
			_, _ = w.Write([]byte(ciJobFixture))
		case "/v1/workspaces/wrk_denied/ci":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"repository access denied"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		}
	}))
}

func TestClientInspectCIReadsRawOverviewAndJobDetail(t *testing.T) {
	var requests []ciRequest
	var mu sync.Mutex
	server := newCIInspectionServer(t, &requests, &mu)
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	overview, err := client.InspectCI(context.Background(), "access-token", "wrk_1")
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if string(overview) != ciOverviewFixture {
		t.Fatalf("overview payload was not preserved verbatim: %s", overview)
	}
	var decoded map[string]any
	if err := json.Unmarshal(overview, &decoded); err != nil || decoded["enabled"] != true {
		t.Fatalf("overview does not decode: %v", err)
	}
	job, err := client.InspectCIJob(context.Background(), "access-token", "wrk_1", "job_1")
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	if string(job) != ciJobFixture {
		t.Fatalf("job payload was not preserved verbatim: %s", job)
	}
	var detail map[string]any
	if err := json.Unmarshal(job, &detail); err != nil {
		t.Fatalf("job does not decode: %v", err)
	}
	for _, field := range []string{"job", "steps", "logs", "metrics", "tests", "futureField"} {
		if _, ok := detail[field]; !ok {
			t.Fatalf("job detail dropped field %q: %s", field, job)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []ciRequest{
		{method: http.MethodGet, path: "/v1/workspaces/wrk_1/ci", auth: "Bearer access-token"},
		{method: http.MethodGet, path: "/v1/workspaces/wrk_1/ci/jobs/job_1", auth: "Bearer access-token"},
	}
	if len(requests) != len(want) {
		t.Fatalf("requests=%+v want %+v", requests, want)
	}
	for i, req := range requests {
		if req != want[i] {
			t.Fatalf("request %d=%+v want %+v", i, req, want[i])
		}
	}
}

func TestClientInspectCIEscapesJobIdentifier(t *testing.T) {
	var requests []ciRequest
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, ciRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/workspaces/wrk_1/ci/jobs/job id" {
			_, _ = w.Write([]byte(`{"job":{"id":"job id"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectCIJob(context.Background(), "access", "wrk_1", "job id"); err != nil {
		t.Fatalf("escaped job id rejected: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0].path != "/v1/workspaces/wrk_1/ci/jobs/job%20id" {
		t.Fatalf("requests=%+v", requests)
	}
}

func TestClientInspectCIRejectsInvalidIdentifiersBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "a\nb", "a\x00b"}
	for _, id := range bad {
		if _, err := client.InspectCI(context.Background(), "access", id); err == nil {
			t.Fatalf("workspace %q accepted", id)
		}
		if _, err := client.InspectCIJob(context.Background(), "access", "wrk_1", id); err == nil {
			t.Fatalf("job %q accepted", id)
		}
	}
	for _, id := range bad {
		if _, err := client.InspectCIJob(context.Background(), "access", id, "job_1"); err == nil {
			t.Fatalf("workspace %q accepted for job inspect", id)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("sent %d requests with invalid identifiers", got)
	}
}

func TestClientInspectCIForwardsAPIErrorAndRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "wrk_malformed"):
			_, _ = w.Write([]byte(`{"enabled":`))
		case strings.Contains(r.URL.Path, "wrk_denied"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"denied"}}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"try later"}}`))
		}
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.InspectCI(context.Background(), "access", "wrk_denied")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("expected 403 APIError, got %v", err)
	}
	_, err = client.InspectCIJob(context.Background(), "access", "wrk_1", "job_1")
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 APIError, got %v", err)
	}
	_, err = client.InspectCI(context.Background(), "access", "wrk_malformed")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("malformed JSON accepted or wrong error: %v", err)
	}
}

func TestClientInspectCIJobDoesNotReplayBearerAcrossRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer credential reached redirect target")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := New(source.URL, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectCIJob(context.Background(), "sensitive-token", "wrk_1", "job_1"); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if redirected.Load() != 0 {
		t.Fatal("client followed a credential-bearing redirect")
	}
}
