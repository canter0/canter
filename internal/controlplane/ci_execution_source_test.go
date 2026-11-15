package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/pricing"
)

const ciSourceFixtureToken = "ghs_source_token_fixture"

type ciSourceRequest struct {
	Method, Path, Query, Authorization, Body string
}

type ciSourceFixture struct {
	mu       sync.Mutex
	server   *httptest.Server
	client   *ciGitHubClient
	requests []ciSourceRequest
	respond  func(http.ResponseWriter, *http.Request)
}

func newCISourceFixture(t *testing.T) *ciSourceFixture {
	t.Helper()
	key, err := parseCIRSAKey(ciTestKey(t))
	if err != nil {
		t.Fatal(err)
	}
	f := &ciSourceFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, ciSourceRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Authorization: r.Header.Get("Authorization"), Body: string(raw)})
		respond := f.respond
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if respond == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
			return
		}
		respond(w, r)
	}))
	t.Cleanup(f.server.Close)
	f.client = &ciGitHubClient{appID: 4321, key: key, base: f.server.URL, hc: f.server.Client()}
	return f
}

func (f *ciSourceFixture) setRespond(fn func(http.ResponseWriter, *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = fn
}

func (f *ciSourceFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *ciSourceFixture) request(i int) ciSourceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func ciSourceScope() ciRunnerJobScope {
	return ciRunnerJobScope{
		RepositoryID: 555, RunnerID: 9001, RunnerName: "ci-jit-1",
		SizeID:    "linux-x64-2",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func ciSourceHint() ciRunnerJobHint {
	return ciRunnerJobHint{RunID: 777, Attempt: 2}
}

func ciSourceNow() time.Time {
	return time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
}

func ciSourceJobJSON(mut func(map[string]any)) string {
	job := map[string]any{
		"id": 424242, "run_id": 777, "run_attempt": 2,
		"runner_id": 9001, "runner_name": "ci-jit-1",
		"status": "in_progress", "started_at": "2026-01-01T00:30:00Z",
		"completed_at": nil, "conclusion": nil,
		"labels": []any{"canter-2vcpu-ubuntu-2404"},
		"name":   "build", "head_sha": "0123456789abcdef",
	}
	if mut != nil {
		mut(job)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func ciSourcePageJSON(total int64, jobs ...string) string {
	return fmt.Sprintf(`{"total_count":%d,"jobs":[%s]}`, total, strings.Join(jobs, ","))
}

func ciSourceFillers(start, count int) []string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, ciSourceJobJSON(func(job map[string]any) {
			job["id"] = start + i
			job["runner_id"] = nil
			job["runner_name"] = nil
			job["status"] = "queued"
			job["started_at"] = nil
			job["labels"] = []any{}
		}))
	}
	return out
}

func (f *ciSourceFixture) stubSource(repoBody string, pages ...string) {
	f.setRespond(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repositories/555" {
			fmt.Fprint(w, repoBody)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") && strings.Contains(r.URL.Path, "/actions/runs/") {
			page := 1
			fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
			if page >= 1 && page <= len(pages) {
				fmt.Fprint(w, pages[page-1])
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"not found"}`)
	})
}

func ciSourceDefaultRepo() string {
	return `{"id":555,"full_name":"octo/widgets","private":true}`
}

func TestCIExecutionSourceTokenScopedRequest(t *testing.T) {
	f := newCISourceFixture(t)
	f.setRespond(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"source-token-1","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	token, err := f.client.runnerSourceToken(context.Background(), 42, 555)
	if err != nil {
		t.Fatal(err)
	}
	if token != "source-token-1" {
		t.Fatalf("token = %q", token)
	}
	if f.count() != 1 {
		t.Fatalf("requests = %d", f.count())
	}
	req := f.request(0)
	if req.Method != http.MethodPost || req.Path != "/app/installations/42/access_tokens" {
		t.Fatalf("request = %s %s", req.Method, req.Path)
	}
	if !strings.HasPrefix(req.Authorization, "Bearer eyJ") || strings.Count(req.Authorization, ".") != 2 {
		t.Fatalf("authorization is not an App JWT: %q", req.Authorization)
	}
	wantBody := `{"repository_ids":[555],"permissions":{"actions":"read","metadata":"read"}}`
	if req.Body != wantBody {
		t.Fatalf("body = %s", req.Body)
	}
}

func TestCIExecutionSourceTokenRejectsInvalidIDsAndTokens(t *testing.T) {
	f := newCISourceFixture(t)
	f.setRespond(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"source-token-1"}`)
	})
	for _, ids := range [][2]int64{{0, 555}, {-1, 555}, {42, 0}, {42, -3}} {
		token, err := f.client.runnerSourceToken(context.Background(), ids[0], ids[1])
		if err == nil || token != "" {
			t.Fatalf("ids %v: token=%q err=%v", ids, token, err)
		}
	}
	if f.count() != 0 {
		t.Fatalf("invalid ids caused %d HTTP calls", f.count())
	}
	for _, body := range []string{
		`{}`, `{"token":""}`, `{"token":"app"}`,
		`{"token":"has space"}`, `{"token":"tab\there"}`,
		`{"token":"ctl\u0001char"}`,
		`{"token":123}`, `{"token":{}}`, `{"token":[]}`, `{"token":null}`,
		`null`, `5`, `[]`, `"str"`, `{bad`, `{"token":"x"`,
	} {
		f.setRespond(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		})
		token, err := f.client.runnerSourceToken(context.Background(), 42, 555)
		if err == nil || token != "" {
			t.Fatalf("body %s: token=%q err=%v", body, token, err)
		}
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("body %s: err = %v, want ErrConflict", body, err)
		}
		if strings.Contains(err.Error(), "source-token") || strings.Contains(err.Error(), "123") {
			t.Fatalf("body %s: error leaks response data: %v", body, err)
		}
	}
}

func TestCIExecutionSourceProofSuccess(t *testing.T) {
	f := newCISourceFixture(t)
	target := ciSourceJobJSON(func(job map[string]any) {
		job["id"] = 987654
		job["name"] = "totally-different-display-name"
	})
	decoy := ciSourceJobJSON(func(job map[string]any) {
		job["id"] = 111
		job["runner_id"] = 4242
		job["runner_name"] = "ci-jit-1"
		job["name"] = "ci-jit-1"
	})
	page1 := ciSourcePageJSON(102, append(ciSourceFillers(1, 99), decoy)...)
	page2 := ciSourcePageJSON(102, append(ciSourceFillers(1000, 1), target)...)
	f.stubSource(`{"id":555,"full_name":"octo-renamed/widgets","private":true}`, page1, page2)

	proof, err := f.client.proveRunnerJob(context.Background(), ciSourceFixtureToken,
		ciSourceScope(), ciSourceHint(), ciSourceNow())
	if err != nil {
		t.Fatal(err)
	}
	if proof.JobID != 987654 || proof.RepositoryID != 555 ||
		proof.Repository != "octo-renamed/widgets" || proof.RunID != 777 ||
		proof.Attempt != 2 || proof.RunnerID != 9001 || proof.RunnerName != "ci-jit-1" {
		t.Fatalf("proof = %+v", proof)
	}
	wantStart := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	if !proof.StartedAt.Equal(wantStart) || proof.StartedAt.Location() != time.UTC {
		t.Fatalf("startedAt = %v", proof.StartedAt)
	}
	if f.count() != 3 {
		t.Fatalf("requests = %d", f.count())
	}
	repoReq := f.request(0)
	if repoReq.Path != "/repositories/555" || repoReq.Authorization != "Bearer "+ciSourceFixtureToken {
		t.Fatalf("repo request = %+v", repoReq)
	}
	for i, wantPage := range []string{"per_page=100&page=1", "per_page=100&page=2"} {
		req := f.request(i + 1)
		wantPath := "/repos/octo-renamed/widgets/actions/runs/777/attempts/2/jobs"
		if req.Path != wantPath || req.Query != wantPage ||
			req.Authorization != "Bearer "+ciSourceFixtureToken {
			t.Fatalf("jobs request %d = %+v", i+1, req)
		}
	}
}

func TestCIExecutionSourceProofRejectsInvalidInput(t *testing.T) {
	f := newCISourceFixture(t)
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	withCreated := func(at time.Time) ciRunnerJobScope {
		s := ciSourceScope()
		s.CreatedAt = at
		return s
	}
	cases := []struct {
		name  string
		scope ciRunnerJobScope
		hint  ciRunnerJobHint
		now   time.Time
		token string
	}{
		{"repo id zero", ciRunnerJobScope{RepositoryID: 0, RunnerID: 1, RunnerName: "a", SizeID: "linux-x64-2", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"repo id negative", ciRunnerJobScope{RepositoryID: -5, RunnerID: 1, RunnerName: "a", SizeID: "linux-x64-2", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"runner id zero", ciRunnerJobScope{RepositoryID: 1, RunnerID: 0, RunnerName: "a", SizeID: "linux-x64-2", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"runner name empty", ciRunnerJobScope{RepositoryID: 1, RunnerID: 1, RunnerName: "", SizeID: "linux-x64-2", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"runner name space", ciRunnerJobScope{RepositoryID: 1, RunnerID: 1, RunnerName: "bad name", SizeID: "linux-x64-2", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"runner name long", ciRunnerJobScope{RepositoryID: 1, RunnerID: 1, RunnerName: strings.Repeat("a", 65), SizeID: "linux-x64-2", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"size unknown", ciRunnerJobScope{RepositoryID: 1, RunnerID: 1, RunnerName: "a", SizeID: "linux-x64-9", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"size empty", ciRunnerJobScope{RepositoryID: 1, RunnerID: 1, RunnerName: "a", SizeID: "", CreatedAt: now}, hint, now, ciSourceFixtureToken},
		{"run id zero", scope, ciRunnerJobHint{RunID: 0, Attempt: 2}, now, ciSourceFixtureToken},
		{"run id negative", scope, ciRunnerJobHint{RunID: -9, Attempt: 2}, now, ciSourceFixtureToken},
		{"attempt zero", scope, ciRunnerJobHint{RunID: 7, Attempt: 0}, now, ciSourceFixtureToken},
		{"attempt negative", scope, ciRunnerJobHint{RunID: 7, Attempt: -2}, now, ciSourceFixtureToken},
		{"now zero", scope, hint, time.Time{}, ciSourceFixtureToken},
		{"created zero", withCreated(time.Time{}), hint, now, ciSourceFixtureToken},
		{"created future", withCreated(now.Add(2 * time.Minute)), hint, now, ciSourceFixtureToken},
	}
	for _, tc := range cases {
		proof, err := f.client.proveRunnerJob(ctx, tc.token, tc.scope, tc.hint, tc.now)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: err = %v, want ErrConflict", tc.name, err)
		}
		if proof != (ciRunnerJobProof{}) {
			t.Fatalf("%s: partial proof %+v", tc.name, proof)
		}
	}
	for _, token := range []string{"", "app", "has space", "tab\there", "ctl\u0001x"} {
		proof, err := f.client.proveRunnerJob(ctx, token, scope, hint, now)
		if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
			t.Fatalf("token %q: err=%v proof=%+v", token, err, proof)
		}
	}
	if f.count() != 0 {
		t.Fatalf("invalid input caused %d HTTP calls", f.count())
	}
}

func TestCIExecutionSourceRepositoryIdentity(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	cases := []struct {
		name string
		body string
	}{
		{"id mismatch", `{"id":999,"full_name":"octo/widgets","private":true}`},
		{"id zero", `{"id":0,"full_name":"octo/widgets","private":true}`},
		{"private missing", `{"id":555,"full_name":"octo/widgets"}`},
		{"private null", `{"id":555,"full_name":"octo/widgets","private":null}`},
		{"name no slash", `{"id":555,"full_name":"widgets","private":true}`},
		{"dotdot repo", `{"id":555,"full_name":"octo/..","private":true}`},
		{"dot repo", `{"id":555,"full_name":"octo/.","private":true}`},
		{"owner underscore", `{"id":555,"full_name":"oct_o/widgets","private":true}`},
		{"extra segment", `{"id":555,"full_name":"octo/widgets/extra","private":true}`},
		{"space in name", `{"id":555,"full_name":"octo/wi dgets","private":true}`},
		{"empty name", `{"id":555,"full_name":"","private":true}`},
	}
	for _, tc := range cases {
		f := newCISourceFixture(t)
		f.stubSource(tc.body, ciSourcePageJSON(0))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
			t.Fatalf("%s: err=%v proof=%+v", tc.name, err, proof)
		}
		if f.count() != 1 {
			t.Fatalf("%s: jobs endpoint was queried (%d requests)", tc.name, f.count())
		}
	}
	f := newCISourceFixture(t)
	f.stubSource(`{"id":555,"full_name":"octo/widgets","private":false}`, ciSourcePageJSON(0))
	_, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
	if !errors.Is(err, errCIRunnerJobPending) {
		t.Fatalf("private=false: err=%v, want pending", err)
	}
}

func ciSourceMatchedJobJSON(mut func(map[string]any)) string {
	return ciSourceJobJSON(func(job map[string]any) {
		job["id"] = 5150
		if mut != nil {
			mut(job)
		}
	})
}

func TestCIExecutionSourceJobMatching(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	unassigned := ciSourceJobJSON(func(job map[string]any) {
		job["id"] = 1
		job["runner_id"] = nil
		job["runner_name"] = nil
		job["status"] = "queued"
		job["started_at"] = nil
	})
	otherRunner := ciSourceJobJSON(func(job map[string]any) {
		job["id"] = 2
		job["runner_id"] = 7777
		job["status"] = "in_progress"
	})
	cases := []struct {
		name    string
		page    string
		pending bool
	}{
		{"empty page", ciSourcePageJSON(0), true},
		{"unassigned only", ciSourcePageJSON(1, unassigned), true},
		{"other runner only", ciSourcePageJSON(1, otherRunner), true},
		{"two matches", ciSourcePageJSON(2,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 10 }),
			ciSourceMatchedJobJSON(func(j map[string]any) {
				j["id"] = 11
				j["status"] = "completed"
				j["conclusion"] = "success"
				j["completed_at"] = "2026-01-01T00:40:00Z"
			})), false},
		{"wrong runner name", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["runner_name"] = "other-runner" })), false},
		{"wrong run id", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["run_id"] = 999 })), false},
		{"zero job id", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 0 })), false},
		{"negative job id", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = -3 })), false},
		{"attempt wrong", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["run_attempt"] = 3 })), false},
		{"attempt zero", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["run_attempt"] = 0 })), false},
		{"attempt negative", ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["run_attempt"] = -1 })), false},
	}
	for _, tc := range cases {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), tc.page)
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if tc.pending {
			if !errors.Is(err, errCIRunnerJobPending) || proof != (ciRunnerJobProof{}) {
				t.Fatalf("%s: err=%v proof=%+v, want pending", tc.name, err, proof)
			}
		} else {
			if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
				t.Fatalf("%s: err=%v proof=%+v, want conflict", tc.name, err, proof)
			}
		}
	}
}

func TestCIExecutionSourceAttemptScoping(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	for name, mut := range map[string]func(map[string]any){
		"attempt absent": func(j map[string]any) { delete(j, "run_attempt") },
		"attempt match":  func(j map[string]any) { j["run_attempt"] = 2 },
	} {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), ciSourcePageJSON(1, ciSourceMatchedJobJSON(mut)))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if proof.JobID != 5150 {
			t.Fatalf("%s: proof = %+v", name, proof)
		}
	}
	for name, value := range map[string]any{
		"attempt null":   nil,
		"attempt bool":   true,
		"attempt float":  2.5,
		"attempt string": "2",
	} {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["run_attempt"] = value })))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
			t.Fatalf("%s: err=%v proof=%+v, want conflict", name, err, proof)
		}
	}
}

func TestCIExecutionSourceStatusAndTimestamps(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	pendingStatuses := []string{"queued", "waiting", "requested", "pending"}
	for _, status := range pendingStatuses {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["status"] = status })))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if !errors.Is(err, errCIRunnerJobPending) || proof != (ciRunnerJobProof{}) {
			t.Fatalf("status %s: err=%v proof=%+v, want pending", status, err, proof)
		}
	}
	conflictCases := []struct {
		name string
		mut  func(map[string]any)
	}{
		{"completed", func(j map[string]any) {
			j["status"] = "completed"
			j["conclusion"] = "success"
			j["completed_at"] = "2026-01-01T00:40:00Z"
		}},
		{"unknown status", func(j map[string]any) { j["status"] = "mystery" }},
		{"empty status", func(j map[string]any) { j["status"] = "" }},
		{"started missing", func(j map[string]any) { delete(j, "started_at") }},
		{"started null", func(j map[string]any) { j["started_at"] = nil }},
		{"started zero", func(j map[string]any) { j["started_at"] = "0001-01-01T00:00:00Z" }},
		{"completed_at set", func(j map[string]any) { j["completed_at"] = "2026-01-01T00:40:00Z" }},
		{"conclusion set", func(j map[string]any) { j["conclusion"] = "success" }},
		{"conclusion empty", func(j map[string]any) { j["conclusion"] = "" }},
		{"started future", func(j map[string]any) { j["started_at"] = "2026-01-01T01:02:00Z" }},
		{"started before window", func(j map[string]any) { j["started_at"] = "2025-12-31T23:58:00Z" }},
	}
	for _, tc := range conflictCases {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), ciSourcePageJSON(1, ciSourceMatchedJobJSON(tc.mut)))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
			t.Fatalf("%s: err=%v proof=%+v, want conflict", tc.name, err, proof)
		}
	}
	// boundary-acceptable timestamps still prove
	for name, started := range map[string]string{
		"created minus one minute": "2025-12-31T23:59:00Z",
		"now plus one minute":      "2026-01-01T01:01:00Z",
		"offset timezone":          "2026-01-01T01:30:00+01:00",
	} {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["started_at"] = started })))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if proof.StartedAt.Location() != time.UTC {
			t.Fatalf("%s: startedAt not UTC: %v", name, proof.StartedAt)
		}
	}
}

func TestCIExecutionSourceLabels(t *testing.T) {
	ctx := context.Background()
	now := ciSourceNow()
	type labelCase struct {
		name    string
		size    string
		labels  any
		wantErr bool
	}
	cases := []labelCase{
		{"size2 canonical", "linux-x64-2", []any{"canter-2vcpu-ubuntu-2404"}, false},
		{"size2 uppercase", "linux-x64-2", []any{"CANTER-2VCPU-UBUNTU-2404"}, false},
		{"size2 alias rejected", "linux-x64-2", []any{"canter"}, true},
		{"size2 wrong size", "linux-x64-2", []any{"canter-4vcpu-ubuntu-2404"}, true},
		{"size4 canonical", "linux-x64-4", []any{"canter-4vcpu-ubuntu-2404"}, false},
		{"size4 alias", "linux-x64-4", []any{"canter"}, false},
		{"size4 alias mixed case", "linux-x64-4", []any{"CaNtEr"}, false},
		{"size4 wrong size", "linux-x64-4", []any{"canter-2vcpu-ubuntu-2404"}, true},
		{"mixed sizes", "linux-x64-2", []any{"canter-2vcpu-ubuntu-2404", "canter-4vcpu-ubuntu-2404"}, true},
		{"generic labels", "linux-x64-2", []any{"self-hosted", "linux", "x64"}, true},
		{"extra label", "linux-x64-2", []any{"canter-2vcpu-ubuntu-2404", "extra"}, true},
		{"empty labels", "linux-x64-2", []any{}, true},
		{"null labels", "linux-x64-2", nil, true},
		{"leading space", "linux-x64-2", []any{" canter-2vcpu-ubuntu-2404"}, true},
		{"trailing space", "linux-x64-2", []any{"canter-2vcpu-ubuntu-2404 "}, true},
		{"labels not array", "linux-x64-2", "canter-2vcpu-ubuntu-2404", true},
	}
	for _, tc := range cases {
		scope := ciSourceScope()
		scope.SizeID = tc.size
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), ciSourcePageJSON(1,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["labels"] = tc.labels })))
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, ciSourceHint(), now)
		if tc.wantErr {
			if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
				t.Fatalf("%s: err=%v proof=%+v, want conflict", tc.name, err, proof)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

func TestCIExecutionSourcePagination(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	raw := func(total, jobs string) string {
		return `{"total_count":` + total + `,"jobs":` + jobs + `}`
	}
	fullPage := func() string {
		return ciSourcePageJSON(101, ciSourceFillers(1, 100)...)
	}
	match := ciSourceMatchedJobJSON(nil)
	cases := []struct {
		name    string
		pages   []string
		pending bool
	}{
		{"total missing", []string{`{"jobs":[]}`}, false},
		{"total null", []string{`{"total_count":null,"jobs":[]}`}, false},
		{"total string", []string{raw(`"x"`, `[]`)}, false},
		{"total negative", []string{raw(`-1`, `[]`)}, false},
		{"total over cap", []string{raw(`10001`, `[]`)}, false},
		{"jobs missing", []string{`{"total_count":0}`}, false},
		{"jobs null", []string{`{"total_count":0,"jobs":null}`}, false},
		{"jobs not array", []string{raw(`0`, `{}`)}, false},
		{"page1 short", []string{ciSourcePageJSON(2, match)}, true},
		{"page1 long", []string{ciSourcePageJSON(0, match)}, true},
		{"page2 total changed", []string{
			fullPage(),
			ciSourcePageJSON(100, match),
		}, true},
		{"page2 short", []string{
			fullPage(),
			ciSourcePageJSON(101),
		}, true},
		{"page2 long", []string{
			fullPage(),
			ciSourcePageJSON(101, match, ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 99 })),
		}, true},
		{"duplicate id same page", []string{ciSourcePageJSON(2,
			match,
			ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 5150; j["runner_id"] = 1 })),
		}, true},
		{"later page duplicates match", []string{
			ciSourcePageJSON(101, append(ciSourceFillers(1, 99), match)...),
			ciSourcePageJSON(101, match),
		}, true},
	}
	for _, tc := range cases {
		f := newCISourceFixture(t)
		f.stubSource(ciSourceDefaultRepo(), tc.pages...)
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if proof != (ciRunnerJobProof{}) {
			t.Fatalf("%s: partial proof %+v", tc.name, proof)
		}
		if tc.pending {
			if !errors.Is(err, errCIRunnerJobPending) {
				t.Fatalf("%s: err=%v, want pending", tc.name, err)
			}
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: err=%v, want conflict", tc.name, err)
		}
	}
}

func TestCIExecutionSourceHTTPFailures(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusInternalServerError} {
		f := newCISourceFixture(t)
		f.setRespond(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"denied"}`)
		})
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		var ghErr *ciGitHubError
		if !errors.As(err, &ghErr) || ghErr.status != status || proof != (ciRunnerJobProof{}) {
			t.Fatalf("status %d: err=%v proof=%+v", status, err, proof)
		}
	}
	for name, body := range map[string]string{
		"malformed json":   `{not json`,
		"truncated json":   `{"total_count":0,"jobs":[`,
		"wrong shape page": `{"total_count":"x","jobs":[]}`,
		"wrong shape repo": `{"id":"555","full_name":"octo/widgets","private":true}`,
		"bad timestamp":    `{"total_count":0,"jobs":[{"id":1,"run_id":777,"started_at":"not-a-time"}]}`,
	} {
		f := newCISourceFixture(t)
		f.setRespond(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repositories/555" && name == "wrong shape repo" {
				fmt.Fprint(w, body)
				return
			}
			if r.URL.Path == "/repositories/555" {
				fmt.Fprint(w, ciSourceDefaultRepo())
				return
			}
			fmt.Fprint(w, body)
		})
		proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
		if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
			t.Fatalf("%s: err=%v proof=%+v, want conflict", name, err, proof)
		}
		if strings.Contains(err.Error(), ciSourceFixtureToken) {
			t.Fatalf("%s: error leaks token: %v", name, err)
		}
	}
	f := newCISourceFixture(t)
	f.setRespond(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, ciSourceDefaultRepo())
	})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	proof, err := f.client.proveRunnerJob(cancelled, ciSourceFixtureToken, scope, hint, now)
	if err == nil || proof != (ciRunnerJobProof{}) {
		t.Fatalf("cancelled ctx: err=%v proof=%+v", err, proof)
	}
}

func TestCIExecutionSourcePaginationDistinctSecondMatch(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	target := ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 987654 })
	page1 := ciSourcePageJSON(101, append(ciSourceFillers(1, 99), target)...)
	second := ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 888001 })
	page2 := ciSourcePageJSON(101, second)
	f := newCISourceFixture(t)
	f.stubSource(ciSourceDefaultRepo(), page1, page2)
	proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
	if !errors.Is(err, ErrConflict) || proof != (ciRunnerJobProof{}) {
		t.Fatalf("distinct second assignment: err=%v proof=%+v, want conflict", err, proof)
	}
	if f.count() != 3 {
		t.Fatalf("requests = %d, want 3", f.count())
	}

	f2 := newCISourceFixture(t)
	f2.setRespond(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repositories/555" {
			fmt.Fprint(w, ciSourceDefaultRepo())
			return
		}
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, page1)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
	})
	proof, err = f2.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
	var ghErr *ciGitHubError
	if !errors.As(err, &ghErr) || ghErr.status != http.StatusInternalServerError ||
		proof != (ciRunnerJobProof{}) {
		t.Fatalf("page2 500: err=%v proof=%+v, want GitHub 500", err, proof)
	}
	if f2.count() != 3 {
		t.Fatalf("requests = %d, want 3", f2.count())
	}
}

func TestCIExecutionSourcePaginationFullScanSuccess(t *testing.T) {
	ctx := context.Background()
	scope := ciSourceScope()
	hint := ciSourceHint()
	now := ciSourceNow()
	target := ciSourceMatchedJobJSON(func(j map[string]any) { j["id"] = 987654 })
	pages := make([]string, 0, 100)
	for p := 1; p <= 99; p++ {
		pages = append(pages, ciSourcePageJSON(10000, ciSourceFillers((p-1)*100+1, 100)...))
	}
	pages = append(pages, ciSourcePageJSON(10000, append(ciSourceFillers(9901, 99), target)...))
	f := newCISourceFixture(t)
	f.stubSource(ciSourceDefaultRepo(), pages...)
	proof, err := f.client.proveRunnerJob(ctx, ciSourceFixtureToken, scope, hint, now)
	if err != nil {
		t.Fatal(err)
	}
	if proof.JobID != 987654 {
		t.Fatalf("proof = %+v", proof)
	}
	if f.count() != 101 {
		t.Fatalf("requests = %d, want 101", f.count())
	}
}

func TestCIExecutionSourceSizeLabelMapping(t *testing.T) {
	for _, tc := range []struct {
		id    string
		vcpus int
		want  bool
	}{
		{"linux-x64-2", 2, true},
		{"linux-x64-4", 4, true},
		{"linux-arm64-2", 2, false},
		{"windows-x64-4", 4, false},
		{"linux-x64-9", 2, false},
	} {
		labels, ok := ciSourceSizeLabels(pricing.CISize{ID: tc.id, VCPUs: tc.vcpus})
		if ok != tc.want || (ok && len(labels) == 0) {
			t.Fatalf("size %s vcpus %d: ok=%v labels=%v", tc.id, tc.vcpus, ok, labels)
		}
	}
}
