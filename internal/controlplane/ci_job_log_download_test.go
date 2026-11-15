package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type ciJobLogHTTPReq struct {
	Method, Path, Authorization, Cookie, Referer, Body string
}

type ciJobLogDownloadFixture struct {
	mu       sync.Mutex
	api      *httptest.Server
	blob     *httptest.Server
	client   *ciGitHubClient
	requests []ciJobLogHTTPReq
	repoJSON string
	jobJSON  string
	logsFunc func(w http.ResponseWriter, r *http.Request)
	blobFunc func(w http.ResponseWriter, r *http.Request)
}

func newCIJobLogDownloadFixture(t *testing.T) *ciJobLogDownloadFixture {
	t.Helper()
	key, err := parseCIRSAKey(ciTestKey(t))
	if err != nil {
		t.Fatal(err)
	}
	f := &ciJobLogDownloadFixture{
		repoJSON: `{"id":789,"full_name":"financial-tests/repo","private":true}`,
		jobJSON:  `{"id":555,"run_id":777,"status":"completed","runner_id":null,"runner_name":"","started_at":"2026-01-01T00:00:00Z","completed_at":"2026-01-01T00:10:00Z","conclusion":"success","labels":[]}`,
		blobFunc: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "blob log body")
		},
	}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, ciJobLogHTTPReq{
			Method: r.Method, Path: r.URL.Path, Body: string(raw),
			Authorization: r.Header.Get("Authorization"),
			Cookie:        r.Header.Get("Cookie"), Referer: r.Header.Get("Referer"),
		})
		logsFunc := f.logsFunc
		repoJSON, jobJSON := f.repoJSON, f.jobJSON
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/123/access_tokens":
			fmt.Fprint(w, `{"token":"ghs_log_scoped_token"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/789":
			fmt.Fprint(w, repoJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/financial-tests/repo/actions/jobs/555":
			fmt.Fprint(w, jobJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/financial-tests/repo/actions/jobs/555/logs" && logsFunc != nil:
			logsFunc(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	}))
	t.Cleanup(f.api.Close)
	f.blob = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, ciJobLogHTTPReq{
			Method: r.Method, Path: r.URL.Path + "?" + r.URL.RawQuery,
			Authorization: r.Header.Get("Authorization"),
			Cookie:        r.Header.Get("Cookie"), Referer: r.Header.Get("Referer"),
		})
		blobFunc := f.blobFunc
		f.mu.Unlock()
		blobFunc(w, r)
	}))
	t.Cleanup(f.blob.Close)
	hc := f.api.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	f.client = &ciGitHubClient{appID: 4321, key: key, base: f.api.URL, hc: hc}
	return f
}

func (f *ciJobLogDownloadFixture) set(fn func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsFunc = fn
}

func (f *ciJobLogDownloadFixture) setBlob(fn func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobFunc = fn
}

func (f *ciJobLogDownloadFixture) setRepoJSON(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoJSON = raw
}

func (f *ciJobLogDownloadFixture) setJob(mut func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var job map[string]any
	if err := json.Unmarshal([]byte(f.jobJSON), &job); err != nil {
		panic(err)
	}
	mut(job)
	raw, err := json.Marshal(job)
	if err != nil {
		panic(err)
	}
	f.jobJSON = string(raw)
}

func (f *ciJobLogDownloadFixture) apiRequests() []ciJobLogHTTPReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ciJobLogHTTPReq
	for _, r := range f.requests {
		if strings.HasPrefix(r.Path, "/app/") || strings.HasPrefix(r.Path, "/repositories/") || strings.HasPrefix(r.Path, "/repos/") {
			out = append(out, r)
		}
	}
	return out
}

func (f *ciJobLogDownloadFixture) blobRequests() []ciJobLogHTTPReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ciJobLogHTTPReq
	for _, r := range f.requests {
		if strings.HasPrefix(r.Path, "/blob") {
			out = append(out, r)
		}
	}
	return out
}

func (f *ciJobLogDownloadFixture) logFetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasSuffix(r.Path, "/logs") {
			n++
		}
	}
	return n
}

func ciJobLogTestScope() ciJobLogScope {
	return ciJobLogScope{
		Workspace: "ws_log", Job: "job_log", Runner: "runner_log",
		GitHubInstallationID: 123, GitHubRepositoryID: 789,
		GitHubJobID: 555, GitHubRunID: 777,
	}
}

// ciJobLogOutboundReq snapshots one non-API request a client attempted, so a
// test can prove zero outbound traffic to arbitrary hosts.
type ciJobLogOutboundReq struct {
	Method, URL, Authorization, Cookie, Referer string
}

// ciJobLogTransportGuard passes API fixture traffic through to the real
// transport and intercepts every other host, so tests never dial disallowed
// destinations for real.
type ciJobLogTransportGuard struct {
	apiHost string
	base    http.RoundTripper
	respond func(*http.Request) (*http.Response, error)
	mu      sync.Mutex
	others  []ciJobLogOutboundReq
}

func (g *ciJobLogTransportGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == g.apiHost {
		return g.base.RoundTrip(req)
	}
	g.mu.Lock()
	g.others = append(g.others, ciJobLogOutboundReq{
		Method:        req.Method,
		URL:           req.URL.String(),
		Authorization: req.Header.Get("Authorization"),
		Cookie:        req.Header.Get("Cookie"),
		Referer:       req.Header.Get("Referer"),
	})
	respond := g.respond
	g.mu.Unlock()
	if respond != nil {
		return respond(req)
	}
	return nil, fmt.Errorf("blocked non-API request to %s", req.URL.Host)
}

func (g *ciJobLogTransportGuard) outbound() []ciJobLogOutboundReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ciJobLogOutboundReq(nil), g.others...)
}

func (f *ciJobLogDownloadFixture) guardOutbound(respond func(*http.Request) (*http.Response, error)) *ciJobLogTransportGuard {
	apiURL, err := url.Parse(f.api.URL)
	if err != nil {
		panic(err)
	}
	base := f.client.hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	g := &ciJobLogTransportGuard{apiHost: apiURL.Host, base: base, respond: respond}
	f.client.hc.Transport = g
	return g
}

func TestCIJobLogDownloadRedirectFlow(t *testing.T) {
	f := newCIJobLogDownloadFixture(t)
	f.set(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_log_scoped_token" {
			t.Errorf("logs request missing scoped bearer: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Location", f.blob.URL+"/blob?sig=abc123")
		w.WriteHeader(http.StatusFound)
	})
	text, truncated, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if text != "blob log body" || truncated {
		t.Fatalf("unexpected result: %q truncated=%v", text, truncated)
	}
	reqs := f.apiRequests()
	if len(reqs) != 4 {
		t.Fatalf("expected token+repo+job+logs requests, got %d: %#v", len(reqs), reqs)
	}
	tokenReq := reqs[0]
	if tokenReq.Method != http.MethodPost || tokenReq.Path != "/app/installations/123/access_tokens" {
		t.Fatalf("token request: %s %s", tokenReq.Method, tokenReq.Path)
	}
	var tokenBody struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(tokenReq.Body), &tokenBody); err != nil {
		t.Fatal(err)
	}
	if len(tokenBody.RepositoryIDs) != 1 || tokenBody.RepositoryIDs[0] != 789 ||
		tokenBody.Permissions["actions"] != "read" || tokenBody.Permissions["metadata"] != "read" ||
		len(tokenBody.Permissions) != 2 {
		t.Fatalf("token must be scoped to repo 789 read-only: %#v", tokenBody)
	}
	for _, r := range reqs[1:] {
		if r.Authorization != "Bearer ghs_log_scoped_token" {
			t.Fatalf("API request missing bearer: %s %s auth=%q", r.Method, r.Path, r.Authorization)
		}
	}
	blobs := f.blobRequests()
	if len(blobs) != 1 {
		t.Fatalf("expected exactly one blob request, got %d", len(blobs))
	}
	b := blobs[0]
	if b.Authorization != "" || b.Cookie != "" || b.Referer != "" {
		t.Fatalf("blob request must not carry credentials: %#v", b)
	}
	if !strings.Contains(b.Path, "sig=abc123") {
		t.Fatalf("signed query must reach the blob request: %s", b.Path)
	}
}

func TestCIJobLogDownloadDirect200(t *testing.T) {
	f := newCIJobLogDownloadFixture(t)
	f.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "inline log body")
	})
	text, truncated, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
	if err != nil || text != "inline log body" || truncated {
		t.Fatalf("direct 200: %q %v truncated=%v", text, err, truncated)
	}
	if len(f.blobRequests()) != 0 {
		t.Fatal("direct 200 must not touch a blob endpoint")
	}
}

func TestCIJobLogDownloadRejectsMismatchedSource(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *ciJobLogDownloadFixture)
	}{
		{"wrong repository id", func(f *ciJobLogDownloadFixture) {
			f.setRepoJSON(`{"id":999,"full_name":"financial-tests/repo","private":true}`)
		}},
		{"wrong repository name", func(f *ciJobLogDownloadFixture) {
			f.setRepoJSON(`{"id":789,"full_name":"other/repo","private":true}`)
		}},
		{"wrong job id", func(f *ciJobLogDownloadFixture) {
			f.setJob(func(job map[string]any) { job["id"] = 556 })
		}},
		{"wrong run id", func(f *ciJobLogDownloadFixture) {
			f.setJob(func(job map[string]any) { job["run_id"] = 778 })
		}},
		{"running job", func(f *ciJobLogDownloadFixture) {
			f.setJob(func(job map[string]any) { job["status"] = "in_progress" })
		}},
		{"queued job", func(f *ciJobLogDownloadFixture) {
			f.setJob(func(job map[string]any) { job["status"] = "queued" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIJobLogDownloadFixture(t)
			tc.mutate(f)
			f.set(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			})
			_, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
			if err == nil || err.Error() != "CI job logs are unavailable" {
				t.Fatalf("expected fixed unavailable error, got %v", err)
			}
			if f.logFetches() != 0 {
				t.Fatalf("logs endpoint must never be fetched: %d requests", f.logFetches())
			}
			if len(f.blobRequests()) != 0 {
				t.Fatal("blob endpoint must never be fetched")
			}
		})
	}
}

func TestCIJobLogDownloadRejectsBadRedirects(t *testing.T) {
	cases := []struct {
		name      string
		locations []string
	}{
		{"missing location", nil},
		{"multiple locations", []string{"http://127.0.0.1:1/a", "http://127.0.0.1:1/b"}},
		{"relative location", []string{"/blob/path"}},
		{"protocol relative", []string{"//127.0.0.1:1/blob"}},
		{"plain http public host", []string{"http://evil.example.com/blob"}},
		{"https non-blob host", []string{"https://evil.example.com/blob"}},
		{"blob root not subdomain", []string{"https://blob.core.windows.net/blob"}},
		{"actions root not subdomain", []string{"https://actions.githubusercontent.com/blob"}},
		{"lookalike host", []string{"https://blob.core.windows.net.evil.com/blob"}},
		{"ip host", []string{"https://1.2.3.4/blob"}},
		{"userinfo", []string{"https://user:pass@acct.blob.core.windows.net/blob"}},
		{"fragment", []string{"https://acct.blob.core.windows.net/blob#frag"}},
		{"bad port", []string{"https://acct.blob.core.windows.net:8443/blob"}},
		{"nondefault https port", []string{"https://acct.actions.githubusercontent.com:444/blob"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIJobLogDownloadFixture(t)
			guard := f.guardOutbound(nil)
			f.set(func(w http.ResponseWriter, r *http.Request) {
				for _, loc := range tc.locations {
					w.Header().Add("Location", loc)
				}
				w.WriteHeader(http.StatusFound)
			})
			_, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
			if err == nil || err.Error() != "CI job logs are unavailable" {
				t.Fatalf("expected fixed unavailable error, got %v", err)
			}
			if len(f.blobRequests()) != 0 {
				t.Fatalf("disallowed destination must never be contacted: %#v", f.blobRequests())
			}
			if out := guard.outbound(); len(out) != 0 {
				t.Fatalf("no request may leave for a disallowed host: %#v", out)
			}
		})
	}
}

func TestCIJobLogDownloadUnexpectedStatuses(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newCIJobLogDownloadFixture(t)
			f.set(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, "secret-body-should-not-leak")
			})
			_, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
			if err == nil || err.Error() != "CI job logs are unavailable" {
				t.Fatalf("status %d: expected fixed unavailable error, got %v", code, err)
			}
			if strings.Contains(err.Error(), "secret-body") || strings.Contains(err.Error(), f.blob.URL) {
				t.Fatalf("error must not leak body or URL: %v", err)
			}
		})
	}
	// A blob-side non-200 is also unavailable.
	f := newCIJobLogDownloadFixture(t)
	f.setBlob(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	f.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", f.blob.URL+"/blob?sig=x")
		w.WriteHeader(http.StatusFound)
	})
	_, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
	if err == nil || err.Error() != "CI job logs are unavailable" {
		t.Fatalf("blob 403: %v", err)
	}
}

func TestCIJobLogDownloadLoopbackSeam(t *testing.T) {
	f := newCIJobLogDownloadFixture(t)
	f.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", f.blob.URL+"/blob?sig=loop")
		w.WriteHeader(http.StatusFound)
	})
	// allowLoopbackHTTP=false must refuse the http loopback blob URL.
	if _, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), false); err == nil {
		t.Fatal("loopback http blob must be refused without the private seam")
	}
	if len(f.blobRequests()) != 0 {
		t.Fatal("refused destination must not be contacted")
	}
	text, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
	if err != nil || text != "blob log body" {
		t.Fatalf("loopback seam: %q %v", text, err)
	}
}

func TestCIJobLogDownloadSecondRedirectRefused(t *testing.T) {
	f := newCIJobLogDownloadFixture(t)
	secondHits := atomic.Int32{}
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "second hop")
	}))
	t.Cleanup(second.Close)
	f.setBlob(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", second.URL+"/final")
		w.WriteHeader(http.StatusFound)
	})
	f.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", f.blob.URL+"/blob?sig=x")
		w.WriteHeader(http.StatusFound)
	})
	_, _, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
	if err == nil || err.Error() != "CI job logs are unavailable" {
		t.Fatalf("second redirect must be refused: %v", err)
	}
	if secondHits.Load() != 0 {
		t.Fatal("second redirect target must never be contacted")
	}
}

// Production mode (no loopback seam) must still reach allowlisted blob hosts.
// The guard intercepts the HTTPS blob request so no real network is touched,
// while API calls keep flowing to the local fixture.
func TestCIJobLogDownloadProductionBlobAllowlist(t *testing.T) {
	for _, target := range []string{
		"https://acct.blob.core.windows.net/blob?sig=synthetic-blob",
		"https://pipelines.actions.githubusercontent.com/blob?sig=synthetic-actions",
	} {
		t.Run(target, func(t *testing.T) {
			f := newCIJobLogDownloadFixture(t)
			// A seeded jar makes the clone's Jar=nil meaningful: without it
			// the blob request would carry this cookie.
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			blobURL, err := url.Parse(target)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(blobURL, []*http.Cookie{{Name: "session", Value: "secret"}})
			f.client.hc.Jar = jar
			body := "production blob body"
			guard := f.guardOutbound(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					Status:        "200 OK",
					StatusCode:    http.StatusOK,
					Proto:         "HTTP/1.1",
					ProtoMajor:    1,
					ProtoMinor:    1,
					Header:        make(http.Header),
					Body:          io.NopCloser(strings.NewReader(body)),
					ContentLength: int64(len(body)),
					Request:       req,
				}, nil
			})
			f.set(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target)
				w.WriteHeader(http.StatusFound)
			})
			text, truncated, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), false)
			if err != nil || text != body || truncated {
				t.Fatalf("allowlisted blob: %q %v truncated=%v", text, err, truncated)
			}
			out := guard.outbound()
			if len(out) != 1 {
				t.Fatalf("expected exactly one intercepted blob request: %#v", out)
			}
			b := out[0]
			if b.Authorization != "" || b.Cookie != "" || b.Referer != "" {
				t.Fatalf("blob request must not carry credentials: %#v", b)
			}
			if !strings.Contains(b.URL, "sig=synthetic") {
				t.Fatalf("signed query must be preserved on the blob request: %s", b.URL)
			}
			reqs := f.apiRequests()
			if len(reqs) != 4 || reqs[3].Authorization != "Bearer ghs_log_scoped_token" {
				t.Fatalf("API requests must keep the scoped bearer: %#v", reqs)
			}
		})
	}
}

func TestCIJobLogDownloadNormalizesAndTruncates(t *testing.T) {
	f := newCIJobLogDownloadFixture(t)
	big := make([]byte, ciJobLogMaxBytes+100)
	for i := range big {
		big[i] = 'a'
	}
	copy(big[10:14], []byte{0xff, 0xfe, 0x00, 'x'})
	f.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(big)
	})
	text, truncated, err := f.client.downloadCIJobLogs(context.Background(), ciJobLogTestScope(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(text) > ciJobLogMaxBytes {
		t.Fatalf("expected truncated <=1MiB text, got len=%d truncated=%v", len(text), truncated)
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		t.Fatal("normalized text must be valid UTF-8 without NUL")
	}
}

func TestCIJobLogDownloadContextBound(t *testing.T) {
	f := newCIJobLogDownloadFixture(t)
	f.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		// Dribble bytes until the client context is cancelled.
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := f.client.downloadCIJobLogs(ctx, ciJobLogTestScope(), true)
	if err == nil || err.Error() != "CI job logs are unavailable" {
		t.Fatalf("expected fixed unavailable error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("download must honor the bounded context, took %v", elapsed)
	}
}

// Chunk boundaries must never split a multi-byte rune: every stored chunk is
// valid UTF-8 within the byte bound and the chunks join back to the exact
// normalized text.
func TestCIJobLogSplitUTF8Boundaries(t *testing.T) {
	text, truncated := normalizeCIJobLog([]byte(strings.Repeat("\u96ea", 6000)))
	if truncated {
		t.Fatal("18KiB of valid UTF-8 must not be marked truncated")
	}
	chunks := splitCIJobLog(text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) > ciJobLogChunkBytes {
			t.Fatalf("chunk %d exceeds the byte bound: %d", i, len(chunk))
		}
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
	}
	if joined := strings.Join(chunks, ""); joined != text {
		t.Fatal("chunks must join back to the exact normalized text")
	}
}
