package controlplane

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ciCacheHTTPFixture wires a real admitted job, a real filesystem backend, a
// real httptest server and a mock GitHub source into one cache server.
type ciCacheHTTPFixture struct {
	t         *testing.T
	s         *Store
	srv       *CICacheServer
	http      *httptest.Server
	source    *ciSourceFixture
	repo      ciCacheRepoFixture
	sign      func(string, string) string
	proof     ciRunnerJobProof
	workspace string
	root      string
	rsaKey    *rsa.PrivateKey
}

func ciCacheHTTPFixtureNew(t *testing.T) *ciCacheHTTPFixture {
	t.Helper()
	s, workspace, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, "9001")
	source := ciCacheMockGitHubFor(t, repo, nil)
	key, sign := ciCacheTestSigner(t)
	root := ciCacheBlobTestRoot(t)
	srv, err := NewCICacheServer(context.Background(), CICacheServerConfig{
		Store:     s,
		RootDir:   root,
		PublicURL: "https://cache.example.com",
		GitHub: CIConfig{
			AppID: 4321, AppSlug: "canter-ci",
			PrivateKeyPEM: ciTestKey(t),
			WebhookSecret: []byte(strings.Repeat("w", 32)),
			APIBaseURL:    source.server.URL,
		},
		AllowLoopbackHTTP: true,
		keySet:            ciCacheTestKeys(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := ciCachePoolConfigFor(srv.BackendDigest(), 64<<20, 64<<20, 8<<20, 1000, 1000, 3600)
	if err := s.configureCICachePool(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { srv.Close() })
	fx := &ciCacheHTTPFixture{
		t: t, s: s, srv: srv, http: ts, source: source,
		repo: repo, sign: sign, proof: proof, workspace: workspace,
		root: root, rsaKey: key,
	}
	// Drain every object this fixture's backend still holds so the next
	// test's pool configuration never sees a backend change with live quota.
	t.Cleanup(func() {
		ciCacheDrainBackend(t, s, srv.blob, root, srv.BackendDigest(), repo)
	})
	return fx
}

// ciCacheDrainBackend invalidates and physically removes every non-deleted
// object owned by this fixture's backend so pool counters return to zero. It
// uses each row's stored namespace digest directly — no fresh job grant is
// required after the source job completes or the clock advances.
func ciCacheDrainBackend(t *testing.T, s *Store, blob *ciCacheBlobStore, root, backend string,
	_ ciCacheRepoFixture) {
	t.Helper()
	// The test may already have closed the server (closing its blob store);
	// reopen the same root — the process flock is free once Close returned.
	if blob == nil {
		var err error
		blob, err = newCICacheBlobStore(root)
		if err != nil {
			t.Fatal(err)
		}
		defer blob.Close()
	}
	ctx := context.Background()
	rows, err := s.pool.Query(ctx,
		`SELECT id,namespace_digest FROM ci_cache_objects WHERE backend_digest=$1 AND state<>'deleted'`, backend)
	if err != nil {
		t.Fatal(err)
	}
	var fresh *ciCacheBlobStore
	grants := map[string]ciCacheGrant{}
	var pairs [][2]string
	for rows.Next() {
		var id, ns string
		if err := rows.Scan(&id, &ns); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, [2]string{id, ns})
		if _, ok := grants[ns]; !ok {
			grants[ns] = ciCacheGrant{namespace: ns}
		}
	}
	rows.Close()
	for _, pair := range pairs {
		if err := s.invalidateCICacheObject(ctx, grants[pair[1]], backend, pair[0]); err != nil &&
			!errors.Is(err, ErrNotFound) {
			t.Fatalf("drain invalidate %s: %v", pair[0], err)
		}
	}
	for i := 0; i < 16; i++ {
		claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) == 0 {
			return
		}
		for _, c := range claims {
			absent, err := blob.Delete(ctx, c.ObjectID)
			if err != nil && errors.Is(err, errCICacheBlobClosed) && fresh == nil {
				var ferr error
				fresh, ferr = newCICacheBlobStore(root)
				if ferr != nil {
					t.Fatal(ferr)
				}
				defer fresh.Close()
				blob = fresh
				absent, err = blob.Delete(ctx, c.ObjectID)
			}
			if err == nil && absent {
				if err := s.ackCICacheAbsent(ctx, c); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Logf("drain delete %s: absent=%v err=%v", c.ObjectID, absent, err)
				if err := s.retryCICacheCleanup(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
		}
		now := s.now()
		s.now = func() time.Time { return now.Add(10 * time.Second) }
	}
	t.Fatal("drain left cleanup claims")
}

func (fx *ciCacheHTTPFixture) token(mutate func(map[string]any)) string {
	claims := ciCacheClaimsForRepo(fx.s.now(), fx.repo)
	claims["aud"] = fx.srv.audience
	if mutate != nil {
		mutate(claims)
	}
	return ciCacheSignClaims(fx.t, fx.sign, claims)
}

func (fx *ciCacheHTTPFixture) do(method, path, token string,
	body io.Reader, headers map[string]string) (*http.Response, []byte) {
	fx.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return fx.doCtx(ctx, method, path, token, body, headers)
}

func (fx *ciCacheHTTPFixture) doCtx(ctx context.Context, method, path, token string,
	body io.Reader, headers map[string]string) (*http.Response, []byte) {
	fx.t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, fx.http.URL+path, body)
	if err != nil {
		fx.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := fx.http.Client().Do(req)
	if err != nil {
		fx.t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

func (fx *ciCacheHTTPFixture) postJSON(path, token string, payload any) (*http.Response, map[string]any) {
	fx.t.Helper()
	raw, _ := json.Marshal(payload)
	resp, body := fx.do(http.MethodPost, path, token, bytes.NewReader(raw),
		map[string]string{"Content-Type": "application/json"})
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return resp, out
}

func ciCacheLookupPayload(key, version, runner string) map[string]any {
	return map[string]any{
		"schemaVersion": 1, "runnerName": runner,
		"key": key, "cacheVersion": version,
	}
}

func ciCacheReservePayload(key, version, runner string, size int64, sha string) map[string]any {
	body := ciCacheLookupPayload(key, version, runner)
	body["sizeBytes"] = size
	body["sha256"] = sha
	return body
}

func ciCacheDigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ciCachePublishViaHTTP drives reserve -> PUT -> lookup through the real
// routes and returns the object id plus entry proof.
func (fx *ciCacheHTTPFixture) publishViaHTTP(key, version string, data []byte) string {
	fx.t.Helper()
	sha := ciCacheDigestOf(data)
	resp, out := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload(key, version, fx.proof.RunnerName, int64(len(data)), sha))
	if resp.StatusCode != http.StatusCreated {
		fx.t.Fatalf("reserve: %d %v", resp.StatusCode, out)
	}
	id, _ := out["uploadId"].(string)
	if id == "" {
		fx.t.Fatalf("reserve missing uploadId: %v", out)
	}
	putPath := "/v1/ci/cache/objects/" + id + "?runnerName=" + fx.proof.RunnerName
	put, raw := fx.do(http.MethodPut, putPath, fx.token(nil), bytes.NewReader(data),
		map[string]string{"Content-Type": "application/octet-stream"})
	if put.StatusCode != http.StatusOK {
		fx.t.Fatalf("put: %d %s", put.StatusCode, raw)
	}
	return id
}

func (fx *ciCacheHTTPFixture) lookupHit(key, version string) map[string]any {
	fx.t.Helper()
	resp, out := fx.postJSON("/v1/ci/cache/lookup", fx.token(nil),
		ciCacheLookupPayload(key, version, fx.proof.RunnerName))
	if resp.StatusCode != http.StatusOK {
		fx.t.Fatalf("lookup: %d %v", resp.StatusCode, out)
	}
	return out
}

func (fx *ciCacheHTTPFixture) getBlob(id string) (*http.Response, []byte) {
	return fx.do(http.MethodGet,
		"/v1/ci/cache/objects/"+id+"?runnerName="+fx.proof.RunnerName,
		fx.token(nil), nil, nil)
}

func TestCICacheHTTPServerConfig(t *testing.T) {
	ctx := context.Background()
	bad := func(cfg CICacheServerConfig) {
		if _, err := NewCICacheServer(ctx, cfg); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	bad(CICacheServerConfig{})
	bad(CICacheServerConfig{Store: integrationStore(t), PublicURL: "https://x.example.com"})
	bad(CICacheServerConfig{Store: integrationStore(t), RootDir: ciCacheBlobTestRoot(t),
		PublicURL: "https://x.example.com/path"})
	bad(CICacheServerConfig{Store: integrationStore(t), RootDir: ciCacheBlobTestRoot(t),
		PublicURL: "https://x.example.com",
		GitHub:    CIConfig{APIBaseURL: "https://evil.example.com"}})
	bad(CICacheServerConfig{Store: integrationStore(t), RootDir: ciCacheBlobTestRoot(t),
		PublicURL: "https://x.example.com"})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := NewCICacheServer(cctx, CICacheServerConfig{}); err == nil {
		t.Fatal("canceled ctx accepted")
	}
	if _, err := NewCICacheServer(nil, CICacheServerConfig{}); err == nil {
		t.Fatal("nil ctx accepted")
	}
}

func TestCICacheHTTPInfoAndDelegation(t *testing.T) {
	// Nil cache: outer CI router returns 404; no filesystem or pool change.
	h := &HTTPServer{config: HTTPConfig{CI: CIConfig{}}}
	rec := httptest.NewRecorder()
	h.ciGitHubPublic(rec, httptest.NewRequest(http.MethodGet, "/v1/ci/cache/info", nil),
		[]string{"cache", "info"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil cache route: %d", rec.Code)
	}
	fx := ciCacheHTTPFixtureNew(t)
	resp, raw := fx.do(http.MethodGet, "/v1/ci/cache/info", "", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: %d %s", resp.StatusCode, raw)
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info["schemaVersion"].(float64) != 1 || info["enabled"] != true ||
		info["audience"] != fx.srv.audience || info["maxEntryBytes"].(float64) <= 0 {
		t.Fatalf("info shape: %v", info)
	}
	// Non-nil outer delegation through a real NewHTTPServer handler.
	outer := NewHTTPServer(nil, HTTPConfig{CI: CIConfig{Cache: fx.srv}})
	outerRec := httptest.NewRecorder()
	outer.ServeHTTP(outerRec, httptest.NewRequest(http.MethodGet, "/v1/ci/cache/info", nil))
	if outerRec.Code != http.StatusOK {
		t.Fatalf("outer delegation: %d", outerRec.Code)
	}
	// Explicitly disable THIS fixture's own matching pool: info reports
	// disabled and cache operations are unavailable without auto-config.
	state, err := fx.s.ciCachePoolState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	disabled := ciCachePoolConfigFor(fx.srv.BackendDigest(), state.MaxTotalBytes,
		state.MaxWorkspaceBytes, state.MaxEntryBytes, state.MaxObjects,
		state.MaxWorkspaceObjects, state.TTLSeconds)
	disabled.Enabled = false
	if err := fx.s.configureCICachePool(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
	resp3, raw3 := fx.do(http.MethodGet, "/v1/ci/cache/info", "", nil, nil)
	var info3 map[string]any
	_ = json.Unmarshal(raw3, &info3)
	if resp3.StatusCode != http.StatusOK || info3["enabled"] != false {
		t.Fatalf("disabled info: %d %v", resp3.StatusCode, info3)
	}
	resp4, _ := fx.postJSON("/v1/ci/cache/lookup", fx.token(nil),
		ciCacheLookupPayload("k", "v1", fx.proof.RunnerName))
	if resp4.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled lookup: %d", resp4.StatusCode)
	}
	// Re-enable for any later use in this test.
	disabled.Enabled = true
	if err := fx.s.configureCICachePool(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}
}

func TestCICacheHTTPFeatureList(t *testing.T) {
	// ciFeatureList is a pure function: unconfigured cache stays
	// not_configured, a ready pool reports available with the trust-scoped
	// description.
	features := ciFeatureList(false, false)
	for _, f := range features {
		if f.ID == "cache" {
			if f.Status != "not_configured" || f.Description != "Colocated caching is not configured." {
				t.Fatalf("unconfigured cache feature: %+v", f)
			}
		}
	}
	features = ciFeatureList(false, true)
	for _, f := range features {
		if f.ID == "cache" {
			if f.Status != "available" || f.Description !=
				"Immutable cache blobs are scoped to admitted GitHub jobs and repository trust." {
				t.Fatalf("available cache feature: %+v", f)
			}
		}
	}
}

func TestCICacheHTTPAuthAndJSON(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	before := fx.source.count()
	lookup := func(token string, raw []byte) *http.Response {
		resp, _ := fx.do(http.MethodPost, "/v1/ci/cache/lookup", token,
			bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
		return resp
	}
	good := func() string { return fx.token(nil) }
	valid, _ := json.Marshal(ciCacheLookupPayload("k", "v1", fx.proof.RunnerName))
	// Missing, malformed and duplicate Authorization headers.
	if r := lookup("", valid); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, fx.http.URL+"/v1/ci/cache/lookup",
		bytes.NewReader(valid))
	req.Header.Add("Authorization", "Bearer "+good())
	req.Header.Add("Authorization", "Bearer "+good())
	resp, err := fx.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("duplicate auth: %d", resp.StatusCode)
	}
	// Foreign audience, expired and wrong-runner claims never reach GitHub.
	for name, mutate := range map[string]func(map[string]any){
		"foreign-aud": func(c map[string]any) { c["aud"] = "urn:other:cache" },
		"expired":     func(c map[string]any) { c["exp"] = fx.s.now().Add(-time.Hour).Unix() },
	} {
		tok := fx.token(mutate)
		if r := lookup(tok, valid); r.StatusCode == http.StatusOK {
			t.Fatalf("%s accepted", name)
		}
	}
	if got := fx.source.count() - before; got != 0 {
		t.Fatalf("privileged GitHub calls on rejected tokens: %d", got)
	}
	// Wrong runner-name hint cannot resolve an admitted job.
	badRunner, _ := json.Marshal(ciCacheLookupPayload("k", "v1", "no-such-runner-xyz"))
	if r := lookup(good(), badRunner); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong runner name: %d", r.StatusCode)
	}
	// JSON shape rejections: unknown/case-alias keys, floats, strings,
	// duplicates, oversize.
	badBodies := [][]byte{
		[]byte(`{"schemaVersion":1,"runnerName":"` + fx.proof.RunnerName + `","key":"k","cacheVersion":"v1","extra":1}`),
		[]byte(`{"schemaVersion":1,"runnerName":"` + fx.proof.RunnerName + `","key":"k","cacheVersion":"v1","SchemaVersion":1}`),
		[]byte(`{"schemaVersion":"1","runnerName":"` + fx.proof.RunnerName + `","key":"k","cacheVersion":"v1"}`),
		[]byte(`{"schemaVersion":1.0,"runnerName":"` + fx.proof.RunnerName + `","key":"k","cacheVersion":"v1"}`),
		[]byte(`{"schemaVersion":1,"runnerName":"` + fx.proof.RunnerName + `","key":"k","cacheVersion":"v1","key":"k2"}`),
		[]byte(`{"schemaVersion":1,"runnerName":"bad name!","key":"k","cacheVersion":"v1"}`),
		[]byte(strings.Repeat(" ", ciCacheJSONLimit+1)),
		[]byte(`not json`),
	}
	for i, raw := range badBodies {
		if r := lookup(good(), raw); r.StatusCode != http.StatusBadRequest &&
			r.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("bad body %d: %d", i, r.StatusCode)
		}
	}
	// Routing rejects: wrong method/path/query/RawPath alias.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/ci/cache/info"},
		{http.MethodGet, "/v1/ci/cache/lookup"},
		{http.MethodDelete, "/v1/ci/cache/reservations"},
		{http.MethodGet, "/v1/ci/cache/objects/" + strings.Repeat("a", 64)},
		{http.MethodPost, "/v1/ci/cache/unknown"},
		{http.MethodGet, "/v1/ci/cache/info?x=1"},
	} {
		resp, _ := fx.do(tc.method, tc.path, "", nil, nil)
		want := http.StatusMethodNotAllowed
		if tc.method == http.MethodPost && strings.HasSuffix(tc.path, "/info") ||
			tc.method == http.MethodGet && strings.HasSuffix(tc.path, "/lookup") ||
			tc.method == http.MethodDelete {
			want = http.StatusMethodNotAllowed
		} else if tc.method == http.MethodGet && strings.Contains(tc.path, "objects/") {
			want = http.StatusUnauthorized // routed, auth required
		} else {
			want = http.StatusNotFound
		}
		if strings.Contains(tc.path, "info?x=1") {
			want = http.StatusBadRequest
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, resp.StatusCode, want)
		}
	}
}

func TestCICacheHTTPRoundTrip(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := []byte("cache-payload-v1")
	id := fx.publishViaHTTP("ns/key", "v1", payload)
	// Duplicate reservation for the same key/version reports created:false.
	sha := ciCacheDigestOf(payload)
	resp, out := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload("ns/key", "v1", fx.proof.RunnerName, int64(len(payload)), sha))
	if resp.StatusCode != http.StatusOK || out["created"] != false {
		t.Fatalf("duplicate reserve: %d %v", resp.StatusCode, out)
	}
	// Lookup hit with exact metadata.
	hit := fx.lookupHit("ns/key", "v1")
	if hit["found"] != true || hit["objectId"] != id ||
		hit["sizeBytes"].(float64) != float64(len(payload)) || hit["sha256"] != sha {
		t.Fatalf("lookup hit: %v", hit)
	}
	// Miss for other version.
	miss := fx.lookupHit("ns/key", "v2")
	if miss["found"] != false {
		t.Fatalf("version miss: %v", miss)
	}
	// GET returns exact bytes with integrity headers.
	gresp, gbody := fx.getBlob(id)
	if gresp.StatusCode != http.StatusOK || !bytes.Equal(gbody, payload) {
		t.Fatalf("get: %d %d bytes", gresp.StatusCode, len(gbody))
	}
	if gresp.Header.Get("Content-Type") != "application/octet-stream" ||
		gresp.Header.Get("ETag") != `"`+sha+`"` ||
		gresp.Header.Get("X-Canter-Cache-SHA256") != sha ||
		gresp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", gresp.Header)
	}
	// Published retry of the same upload is idempotent.
	retry, raw := fx.do(http.MethodPut,
		"/v1/ci/cache/objects/"+id+"?runnerName="+fx.proof.RunnerName,
		fx.token(nil), bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/octet-stream"})
	var rout map[string]any
	_ = json.Unmarshal(raw, &rout)
	if retry.StatusCode != http.StatusOK || rout["saved"] != true {
		t.Fatalf("published retry: %d %v", retry.StatusCode, rout)
	}
	// Complete the original job, then a legitimate sibling in the same trust
	// namespace must still hit the published object.
	if _, err := fx.s.pool.Exec(context.Background(),
		`UPDATE ci_jobs SET status='completed' WHERE github_job_id=$1`, fx.proof.JobID); err != nil {
		t.Fatal(err)
	}
	sibling := ciCacheSiblingJob(t, fx.s, fx.workspace, fx.s.now(), "http1")
	repoB := ciCacheRepoFor(sibling, 456, 123, "9001")
	// The fixture mock serves only the primary job; extend it for the sibling.
	fx.source.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			fmt.Fprint(w, `{"token":"ghs_cache_fixture"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/789":
			fmt.Fprintf(w, `{"id":789,"full_name":%q,"private":false}`, fx.repo.Proof.Repository)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/jobs/"+fmt.Sprint(fx.proof.JobID)):
			owner, name, _ := strings.Cut(fx.repo.Proof.Repository, "/")
			job := map[string]any{
				"id": fx.proof.JobID, "run_id": fx.proof.RunID, "run_attempt": fx.proof.Attempt,
				"runner_id": fx.proof.RunnerID, "runner_name": fx.proof.RunnerName,
				"status": "in_progress", "started_at": fx.proof.StartedAt.UTC().Format(time.RFC3339Nano),
				"completed_at": nil, "conclusion": nil,
				"check_run_url": fx.source.server.URL + "/repos/" + owner + "/" + name + "/check-runs/9001",
			}
			raw, _ := json.Marshal(job)
			w.Write(raw)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/jobs/"+fmt.Sprint(sibling.JobID)):
			owner, name, _ := strings.Cut(fx.repo.Proof.Repository, "/")
			job := map[string]any{
				"id": sibling.JobID, "run_id": sibling.RunID, "run_attempt": sibling.Attempt,
				"runner_id": sibling.RunnerID, "runner_name": sibling.RunnerName,
				"status": "in_progress", "started_at": sibling.StartedAt.UTC().Format(time.RFC3339Nano),
				"completed_at": nil, "conclusion": nil,
				"check_run_url": fx.source.server.URL + "/repos/" + owner + "/" + name + "/check-runs/9001",
			}
			raw, _ := json.Marshal(job)
			w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
	claims := ciCacheClaimsForRepo(fx.s.now(), repoB)
	claims["aud"] = fx.srv.audience
	tokB := ciCacheSignClaims(t, fx.sign, claims)
	respB, outB := fx.postJSON("/v1/ci/cache/lookup", tokB,
		ciCacheLookupPayload("ns/key", "v1", sibling.RunnerName))
	if respB.StatusCode != http.StatusOK || outB["found"] != true {
		t.Fatalf("sibling hit after job completion: %d %v", respB.StatusCode, outB)
	}
	grespB, gbodyB := fx.do(http.MethodGet,
		"/v1/ci/cache/objects/"+id+"?runnerName="+sibling.RunnerName,
		tokB, nil, nil)
	if grespB.StatusCode != http.StatusOK || !bytes.Equal(gbodyB, payload) {
		t.Fatalf("sibling get: %d", grespB.StatusCode)
	}
}

func TestCICacheHTTPForeignScope(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := []byte("scoped")
	id := fx.publishViaHTTP("scope/key", "v1", payload)
	// Second legitimate workspace on the same store.
	_, otherProof := ciCacheOtherFixture(t, fx.s, fx.s.now())
	otherRepo := ciCacheRepoFor(otherProof, 457, 124, "9002")
	claims := ciCacheClaimsForRepo(fx.s.now(), otherRepo)
	claims["aud"] = fx.srv.audience
	claims["sub"] = "repo:other-cache/repo:ref:refs/heads/main"
	claims["workflow_ref"] = "other-cache/repo/.github/workflows/ci.yml@refs/heads/main"
	claims["job_workflow_ref"] = "other-cache/repo/.github/workflows/ci.yml@refs/heads/main"
	otherTok := ciCacheSignClaims(t, fx.sign, claims)
	// Serve both workspaces' repository/job/check-run bindings; the other
	// job document is built from its exact proof fields.
	fx.source.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			fmt.Fprint(w, `{"token":"ghs_cache_fixture"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/"+fmt.Sprint(fx.repo.Proof.RepositoryID):
			fmt.Fprintf(w, `{"id":%d,"full_name":%q,"private":false}`,
				fx.repo.Proof.RepositoryID, fx.repo.Proof.Repository)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/jobs/"+fmt.Sprint(fx.proof.JobID)):
			owner, name, _ := strings.Cut(fx.repo.Proof.Repository, "/")
			job := map[string]any{
				"id": fx.proof.JobID, "run_id": fx.proof.RunID, "run_attempt": fx.proof.Attempt,
				"runner_id": fx.proof.RunnerID, "runner_name": fx.proof.RunnerName,
				"status": "in_progress", "started_at": fx.proof.StartedAt.UTC().Format(time.RFC3339Nano),
				"completed_at": nil, "conclusion": nil,
				"check_run_url": fx.source.server.URL + "/repos/" + owner + "/" + name + "/check-runs/9001",
			}
			raw, _ := json.Marshal(job)
			w.Write(raw)
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/790":
			fmt.Fprint(w, `{"id":790,"full_name":"other-cache/repo","private":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/jobs/"+fmt.Sprint(otherProof.JobID)):
			job := map[string]any{
				"id": otherProof.JobID, "run_id": otherProof.RunID, "run_attempt": otherProof.Attempt,
				"runner_id": otherProof.RunnerID, "runner_name": otherProof.RunnerName,
				"status": "in_progress", "started_at": otherProof.StartedAt.UTC().Format(time.RFC3339Nano),
				"completed_at": nil, "conclusion": nil,
				"check_run_url": fx.source.server.URL + "/repos/other-cache/repo/check-runs/9002",
			}
			raw, _ := json.Marshal(job)
			w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
	callsBefore := fx.source.count()
	// Foreign workspace resolves its own job then misses cleanly — never bytes.
	resp, out := fx.postJSON("/v1/ci/cache/lookup", otherTok,
		ciCacheLookupPayload("scope/key", "v1", otherProof.RunnerName))
	if resp.StatusCode != http.StatusOK || out["found"] != false {
		t.Fatalf("foreign lookup: %d %v", resp.StatusCode, out)
	}
	if got := fx.source.count() - callsBefore; got != 3 {
		t.Fatalf("foreign lookup source calls: %d", got)
	}
	callsBefore = fx.source.count()
	gresp, _ := fx.do(http.MethodGet,
		"/v1/ci/cache/objects/"+id+"?runnerName="+otherProof.RunnerName,
		otherTok, nil, nil)
	if gresp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign get: %d", gresp.StatusCode)
	}
	if got := fx.source.count() - callsBefore; got != 3 {
		t.Fatalf("foreign get source calls: %d", got)
	}
	// Mutated ref and environment land in a different trust namespace: miss.
	for name, mutate := range map[string]func(map[string]any){
		"ref": func(c map[string]any) { c["ref"] = "refs/heads/other" },
		"env": func(c map[string]any) { c["environment"] = "prod" },
		"event": func(c map[string]any) {
			c["event_name"] = "pull_request"
			c["ref"] = "refs/pull/1/merge"
		},
	} {
		tok := fx.token(mutate)
		resp, out := fx.postJSON("/v1/ci/cache/lookup", tok,
			ciCacheLookupPayload("scope/key", "v1", fx.proof.RunnerName))
		if resp.StatusCode != http.StatusOK || out["found"] != false {
			t.Fatalf("%s namespace: %d %v", name, resp.StatusCode, out)
		}
	}
	// Epoch bump changes the namespace for new grants: still a miss.
	if _, err := fx.s.pool.Exec(context.Background(),
		`UPDATE ci_settings SET cache_epoch=cache_epoch+1 WHERE workspace_id=$1`,
		fx.workspace); err != nil {
		t.Fatal(err)
	}
	resp, out = fx.postJSON("/v1/ci/cache/lookup", fx.token(nil),
		ciCacheLookupPayload("scope/key", "v1", fx.proof.RunnerName))
	if resp.StatusCode != http.StatusOK || out["found"] != false {
		t.Fatalf("epoch namespace: %d %v", resp.StatusCode, out)
	}
	// Restore epoch; original entry must still be intact and hittable.
	if _, err := fx.s.pool.Exec(context.Background(),
		`UPDATE ci_settings SET cache_epoch=cache_epoch-1 WHERE workspace_id=$1`,
		fx.workspace); err != nil {
		t.Fatal(err)
	}
	hit := fx.lookupHit("scope/key", "v1")
	if hit["found"] != true || hit["objectId"] != id {
		t.Fatalf("original entry lost: %v", hit)
	}
}

func TestCICacheHTTPUploadValidation(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := []byte("upload-checks")
	sha := ciCacheDigestOf(payload)
	// Reserve then exercise PUT rejections.
	resp, out := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload("uv/key", "v1", fx.proof.RunnerName, int64(len(payload)), sha))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reserve: %d %v", resp.StatusCode, out)
	}
	id := out["uploadId"].(string)
	putPath := "/v1/ci/cache/objects/" + id + "?runnerName=" + fx.proof.RunnerName
	// Wrong content type and wrong declared length.
	if r, _ := fx.do(http.MethodPut, putPath, fx.token(nil), bytes.NewReader(payload),
		map[string]string{"Content-Type": "text/plain"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("content-type: %d", r.StatusCode)
	}
	if r, _ := fx.do(http.MethodPut, putPath, fx.token(nil),
		bytes.NewReader(append(payload, 'x')), // ContentLength auto = len(payload)+1
		map[string]string{"Content-Type": "application/octet-stream"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("length: %d", r.StatusCode)
	}
	// Missing runnerName query and duplicate query params.
	if r, _ := fx.do(http.MethodPut, "/v1/ci/cache/objects/"+id, fx.token(nil),
		bytes.NewReader(payload), map[string]string{"Content-Type": "application/octet-stream"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing runnerName: %d", r.StatusCode)
	}
	if r, _ := fx.do(http.MethodPut, putPath+"&runnerName=x", fx.token(nil),
		bytes.NewReader(payload), map[string]string{"Content-Type": "application/octet-stream"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("dup runnerName: %d", r.StatusCode)
	}
	// Unknown object id.
	if r, _ := fx.do(http.MethodPut, "/v1/ci/cache/objects/"+strings.Repeat("b", 64)+
		"?runnerName="+fx.proof.RunnerName, fx.token(nil), bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/octet-stream"}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown put: %d", r.StatusCode)
	}
	// Valid upload still succeeds after the rejections.
	if r, raw := fx.do(http.MethodPut, putPath, fx.token(nil), bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/octet-stream"}); r.StatusCode != http.StatusOK {
		t.Fatalf("valid put: %d %s", r.StatusCode, raw)
	}
	// Oversize reservation beyond pool entry cap -> 413.
	big := int64(8<<20) + 1
	resp2, _ := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload("uv/big", "v1", fx.proof.RunnerName, big, sha))
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize reserve: %d", resp2.StatusCode)
	}
	// Disabled pool: operations unavailable, pool untouched.
	state, err := fx.s.ciCachePoolState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := ciCachePoolConfigFor(fx.srv.BackendDigest(), state.MaxTotalBytes,
		state.MaxWorkspaceBytes, state.MaxEntryBytes, state.MaxObjects,
		state.MaxWorkspaceObjects, state.TTLSeconds)
	cfg.Enabled = false
	if err := fx.s.configureCICachePool(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	resp3, _ := fx.postJSON("/v1/ci/cache/lookup", fx.token(nil),
		ciCacheLookupPayload("uv/key", "v1", fx.proof.RunnerName))
	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled lookup: %d", resp3.StatusCode)
	}
	info3, raw3 := fx.do(http.MethodGet, "/v1/ci/cache/info", "", nil, nil)
	var infoOut map[string]any
	_ = json.Unmarshal(raw3, &infoOut)
	if info3.StatusCode != http.StatusOK || infoOut["enabled"] != false {
		t.Fatalf("disabled info: %d %v", info3.StatusCode, infoOut)
	}
}

func TestCICacheHTTPWorkerCleanup(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := []byte("worker-object")
	id := fx.publishViaHTTP("w/key", "v1", payload)
	// Invalidate through the real metadata path using the object's own
	// stored namespace (no fresh job grant needed).
	var ns string
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT namespace_digest FROM ci_cache_objects WHERE id=$1`, id).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	grant := ciCacheGrant{namespace: ns}
	if err := fx.s.invalidateCICacheObject(context.Background(), grant,
		fx.srv.BackendDigest(), id); err != nil {
		t.Fatal(err)
	}
	before, _ := ciCachePoolCounters(t, fx.s)
	if before == 0 {
		t.Fatal("expected held quota before cleanup")
	}
	fx.srv.cleanupOnce(context.Background())
	after, _ := ciCachePoolCounters(t, fx.s)
	if after != 0 {
		t.Fatalf("counters after cleanup: %d", after)
	}
	if st := ciCacheObjectState(t, fx.s, id); st != "deleted" {
		t.Fatalf("object state: %s", st)
	}
	// Idempotent second tick.
	fx.srv.cleanupOnce(context.Background())
	// Uncertain delete: owned symlink at the object path -> retry holds quota.
	id2 := fx.publishViaHTTP("w/key2", "v1", payload)
	var ns2 string
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT namespace_digest FROM ci_cache_objects WHERE id=$1`, id2).Scan(&ns2); err != nil {
		t.Fatal(err)
	}
	grant2 := ciCacheGrant{namespace: ns2}
	if err := fx.s.invalidateCICacheObject(context.Background(), grant2,
		fx.srv.BackendDigest(), id2); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(fx.srv.blob.root, "objects", id2+".blob")
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("remove published blob: %v", err)
	}
	if err := os.Symlink("/etc/hostname", blobPath); err != nil {
		t.Fatal(err)
	}
	fx.srv.cleanupOnce(context.Background())
	held, _ := ciCachePoolCounters(t, fx.s)
	if held == 0 {
		t.Fatal("uncertain delete refunded quota")
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("remove symlink fixture: %v", err)
	}
	// Retry backoff is 5s on the fake clock: advance it, then clean.
	fakeNow := fx.s.now()
	fx.s.now = func() time.Time { return fakeNow.Add(10 * time.Second) }
	for i := 0; i < 4; i++ {
		fx.srv.cleanupOnce(context.Background())
	}
	final, _ := ciCachePoolCounters(t, fx.s)
	if final != 0 {
		t.Fatalf("counters after uncertain retry: %d", final)
	}
}

func TestCICacheHTTPLifecycle(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	// Run once; second call rejected; Run after Close rejected.
	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fx.srv.Run(runCtx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !fx.srv.workerStarted() {
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := fx.srv.Run(context.Background()); err == nil {
		t.Fatal("second Run accepted")
	}
	runCancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	// Close is idempotent and concurrent-safe.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = fx.srv.Close() }(i)
	}
	wg.Wait()
	if errs[0] != errs[1] {
		t.Fatalf("concurrent close errors differ: %v %v", errs[0], errs[1])
	}
	if err := fx.srv.Run(context.Background()); err == nil {
		t.Fatal("Run after Close accepted")
	}
	// Requests after Close are unavailable.
	resp, _ := fx.do(http.MethodGet, "/v1/ci/cache/info", "", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("post-close info: %d", resp.StatusCode)
	}
	// The filesystem lock is released: a fresh server reopens the same root.
	srv2, err := NewCICacheServer(context.Background(), CICacheServerConfig{
		Store: fx.s, RootDir: fx.root,
		PublicURL: "https://cache.example.com",
		GitHub: CIConfig{AppID: 1, AppSlug: "x", PrivateKeyPEM: ciTestKey(t),
			WebhookSecret: []byte(strings.Repeat("w", 32))},
		AllowLoopbackHTTP: true, keySet: ciCacheTestKeys(fx.rsaKey)})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	defer srv2.Close()
	if srv2.BackendDigest() != fx.srv.BackendDigest() {
		t.Fatal("backend digest changed across reopen")
	}
}

// ciCacheDoneServer wraps a handler so each completed request signals done.
func ciCacheDoneServer(t *testing.T, h http.Handler) (*httptest.Server, chan struct{}) {
	t.Helper()
	done := make(chan struct{}, 16)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		done <- struct{}{}
	}))
	t.Cleanup(ts.Close)
	return ts, done
}

func TestCICacheHTTPCloseDuringUpload(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := bytes.Repeat([]byte("y"), 2048)
	sha := ciCacheDigestOf(payload)
	resp, out := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload("close/key", "v1", fx.proof.RunnerName, int64(len(payload)), sha))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reserve: %d %v", resp.StatusCode, out)
	}
	id := out["uploadId"].(string)
	// Wrap the SERVER-side request body so entered/Close observe the real
	// stream the handler reads, not the client transport.
	sigCh := make(chan *ciCacheSignalReader, 1)
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig := &ciCacheSignalReader{rc: r.Body, entered: make(chan struct{}, 4)}
		sigCh <- sig
		r.Body = sig
		fx.srv.ServeHTTP(w, r)
		close(done)
	}))
	t.Cleanup(ts.Close)
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPut,
		ts.URL+"/v1/ci/cache/objects/"+id+"?runnerName="+fx.proof.RunnerName, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+fx.token(nil))
	clientDone := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		clientDone <- err
	}()
	// Positive proof the handler is genuinely inside the blocked stream:
	// the server-side body was read, the upload slot is held and the object
	// reached 'writing' — all before Close is invoked.
	if _, err := pw.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	var sig *ciCacheSignalReader
	select {
	case sig = <-sigCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the request")
	}
	select {
	case <-sig.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("server-side request body was never read")
	}
	ciCachePollFor(t, "upload slot", func() bool { return len(fx.srv.uploads) == 1 })
	ciCachePollFor(t, "writing state", func() bool { return ciCacheObjectState(t, fx.s, id) == "writing" })
	closeDone := make(chan error, 1)
	go func() { closeDone <- fx.srv.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish during blocked upload")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler still running after Close")
	}
	pw.CloseWithError(errors.New("release"))
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client request never terminated")
	}
	if got := atomic.LoadInt32(&sig.closes); got != 1 {
		t.Fatalf("server request body Close count: %d", got)
	}
	var entries int
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ci_cache_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Fatalf("entries after close-during-upload: %d", entries)
	}
	held, _ := ciCachePoolCounters(t, fx.s)
	if held == 0 {
		t.Fatal("close during upload refunded held quota")
	}
}

func TestCICacheHTTPSlowUploadBudget(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	ts, done := ciCacheDoneServer(t, fx.srv)
	payload := bytes.Repeat([]byte("x"), 4096)
	sha := ciCacheDigestOf(payload)
	resp, out := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload("slow/key", "v1", fx.proof.RunnerName, int64(len(payload)), sha))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reserve: %d %v", resp.StatusCode, out)
	}
	id := out["uploadId"].(string)
	// Token issued 299s ago: ~1s real budget remains while fake-clock SQL
	// deadlines stay comfortably future.
	tok := fx.token(func(c map[string]any) {
		c["iat"] = fx.s.now().Add(-299 * time.Second).Unix()
		c["nbf"] = fx.s.now().Add(-299 * time.Second).Unix()
	})
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPut,
		ts.URL+"/v1/ci/cache/objects/"+id+"?runnerName="+fx.proof.RunnerName, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	clientDone := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		clientDone <- err
	}()
	if _, err := pw.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	// POSITIVE proof: the server handler must finish within the ~1s budget
	// and release its upload slot BEFORE any client-side abort.
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("server handler outlived the token-age budget")
	}
	if n := len(fx.srv.uploads); n != 0 {
		t.Fatalf("upload slot still held: %d", n)
	}
	// Only now is it safe to release the stalled client producer.
	pw.CloseWithError(errors.New("stall release"))
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client request never terminated after server finish")
	}
	// No entry was published; quota stays held until the worker proves
	// physical absence.
	resp2, out2 := fx.postJSON("/v1/ci/cache/lookup", fx.token(nil),
		ciCacheLookupPayload("slow/key", "v1", fx.proof.RunnerName))
	if out2["found"] == true {
		t.Fatalf("entry published after budget kill: %v", out2)
	}
	_ = resp2
	held, _ := ciCachePoolCounters(t, fx.s)
	if held == 0 {
		t.Fatal("budget kill refunded quota before physical absence")
	}
	fx.srv.cleanupOnce(context.Background())
	final, _ := ciCachePoolCounters(t, fx.s)
	if final != 0 {
		t.Fatalf("counters after worker cleanup: %d", final)
	}
}

func TestCICacheHTTPSlowJSONAuth(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	ts, done := ciCacheDoneServer(t, fx.srv)
	// Valid JWT with only ~1s of token age left: the whole auth stage is
	// clipped, so a stalled JSON body must be interrupted server-side.
	tok := fx.token(func(c map[string]any) {
		c["iat"] = fx.s.now().Add(-299 * time.Second).Unix()
		c["nbf"] = fx.s.now().Add(-299 * time.Second).Unix()
	})
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/ci/cache/lookup", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	clientDone := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		clientDone <- err
	}()
	if _, err := pw.Write([]byte{'{'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("auth stage outlived the clipped token budget")
	}
	if n := len(fx.srv.meta); n != 0 {
		t.Fatalf("metadata slot still held: %d", n)
	}
	pw.CloseWithError(errors.New("stall release"))
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client request never terminated")
	}
	if got := fx.source.count(); got != 0 {
		t.Fatalf("privileged GitHub calls during stalled auth: %d", got)
	}
	held, _ := ciCachePoolCounters(t, fx.s)
	if held != 0 {
		t.Fatalf("stalled auth created quota: %d", held)
	}
}

func TestCICacheHTTPInvalidTokenSkipsBody(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	// An invalid bearer must be rejected before the body is ever read.
	reads := 0
	body := &ciCacheCountingReader{onRead: func() { reads++ }}
	req := httptest.NewRequest(http.MethodPost, "/v1/ci/cache/lookup", body)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec := httptest.NewRecorder()
	fx.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: %d", rec.Code)
	}
	if reads != 0 {
		t.Fatalf("body read %d times before auth", reads)
	}
}

// ciCachePollFor waits until fn reports true or fails after 2 seconds.
func ciCachePollFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ciCacheSignalReader reports when the transport first pulls bytes and
// counts Close calls exactly.
type ciCacheSignalReader struct {
	rc      io.ReadCloser
	entered chan struct{}
	closes  int32
}

func (r *ciCacheSignalReader) Read(p []byte) (int, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	return r.rc.Read(p)
}

func (r *ciCacheSignalReader) Close() error {
	atomic.AddInt32(&r.closes, 1)
	return r.rc.Close()
}

type ciCacheCountingReader struct {
	onRead func()
}

func (r *ciCacheCountingReader) Read(p []byte) (int, error) {
	r.onRead()
	return 0, io.EOF
}

func TestCICacheHTTPMaxConnsOne(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	// A second store on the same database with a single pooled connection
	// proves the runner resolver closes rows before follow-up queries.
	cfg, err := pgxpool.ParseConfig(os.Getenv("CANTER_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(pool)
	s2.now = fx.s.now
	defer pool.Close()
	// The single-conn store runs the same grant-resolution path directly.
	key2, sign2 := ciCacheTestSigner(t)
	raw := ciCacheSignClaims(t, sign2, func() map[string]any {
		c := ciCacheClaimsForRepo(s2.now(), fx.repo)
		c["aud"] = "urn:canter:ci:cache:v1:" + strings.Repeat("b", 64)
		return c
	}())
	token, err := verifyCICacheToken(context.Background(), ciCacheTestKeys(key2),
		s2.now, raw, "urn:canter:ci:cache:v1:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	job, err := s2.ciCacheJobForRunner(context.Background(), s2.pool,
		fx.proof.RunnerName, token)
	if err != nil {
		t.Fatalf("single-conn resolver: %v", err)
	}
	if job.Proof.JobID != fx.proof.JobID {
		t.Fatalf("wrong job: %+v", job.Proof)
	}
}

// ciCacheBlockWriter passes the first response chunk through with a flush,
// then blocks every subsequent write until the request context ends. It is
// wrapped around the server handler so ResponseController deadlines still
// reach the real connection through Unwrap.
type ciCacheBlockWriter struct {
	http.ResponseWriter
	r           *http.Request
	entered     chan struct{}
	blocked     chan struct{}
	writes      int32
	firstOnce   sync.Once
	blockedOnce sync.Once
}

func (w *ciCacheBlockWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *ciCacheBlockWriter) WriteHeader(code int) { w.ResponseWriter.WriteHeader(code) }

func (w *ciCacheBlockWriter) Write(p []byte) (int, error) {
	if atomic.AddInt32(&w.writes, 1) == 1 {
		w.firstOnce.Do(func() { close(w.entered) })
		n, err := w.ResponseWriter.Write(p)
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		return n, err
	}
	w.blockedOnce.Do(func() { close(w.blocked) })
	select {
	case <-w.r.Context().Done():
		return 0, w.r.Context().Err()
	case <-time.After(10 * time.Second):
		return 0, errors.New("ciCacheBlockWriter timeout")
	}
}

func TestCICacheHTTPCancelDownload(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := bytes.Repeat([]byte("d"), 256<<10)
	id := fx.publishViaHTTP("dl/key", "v1", payload)
	bwReady := make(chan *ciCacheBlockWriter, 1)
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bw := &ciCacheBlockWriter{ResponseWriter: w, r: r,
			entered: make(chan struct{}), blocked: make(chan struct{})}
		bwReady <- bw
		fx.srv.ServeHTTP(bw, r)
		close(handlerDone)
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/ci/cache/objects/"+id+"?runnerName="+fx.proof.RunnerName, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.token(nil))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bw := <-bwReady
	// Deterministic proof the server is mid-stream with the reader lock
	// held: the second socket write is blocked on backpressure.
	select {
	case <-bw.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("server never blocked mid-stream")
	}
	cancel()
	resp.Body.Close()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("download handler did not finish after cancel")
	}
	// An exclusive delete completing proves the leaked-read-lock path is
	// closed; a still-held RLock would block or fail the delete.
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
	absent, err := fx.srv.blob.Delete(dctx, id)
	dcancel()
	if err != nil || !absent {
		t.Fatalf("exclusive delete after cancel: absent=%v err=%v", absent, err)
	}
	// User cancellation alone never refunds published quota.
	held, _ := ciCachePoolCounters(t, fx.s)
	if held == 0 {
		t.Fatal("cancel refunded published quota")
	}
}

// TestCICacheHTTPQueuedClose holds the object's exclusive blob lock so an
// authenticated PUT queues inside Put's acquire; Close must still finish
// bounded once the lock is released, with no late publication.
func TestCICacheHTTPQueuedClose(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	payload := bytes.Repeat([]byte("q"), 2048)
	sha := ciCacheDigestOf(payload)
	resp, out := fx.postJSON("/v1/ci/cache/reservations", fx.token(nil),
		ciCacheReservePayload("queued/key", "v1", fx.proof.RunnerName, int64(len(payload)), sha))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reserve: %d %v", resp.StatusCode, out)
	}
	id := out["uploadId"].(string)
	// Manual exclusive lock: the PUT's acquire increments refs then blocks
	// on the lock mutex, exactly the queued-writer scenario.
	release, err := fx.srv.blob.acquire(id, true)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	ts, handlerDone := ciCacheDoneServer(t, fx.srv)
	putDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut,
			ts.URL+"/v1/ci/cache/objects/"+id+"?runnerName="+fx.proof.RunnerName,
			bytes.NewReader(payload))
		if err != nil {
			putDone <- err
			return
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+fx.token(nil))
		resp, err := ts.Client().Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		putDone <- err
	}()
	ciCachePollFor(t, "upload slot", func() bool { return len(fx.srv.uploads) == 1 })
	ciCachePollFor(t, "queued lock refs", func() bool {
		fx.srv.blob.mu.Lock()
		defer fx.srv.blob.mu.Unlock()
		l := fx.srv.blob.locks[id]
		return l != nil && l.refs >= 2
	})
	closeDone := make(chan error, 1)
	go func() { closeDone <- fx.srv.Close() }()
	ciCachePollFor(t, "closed flag", fx.srv.isClosed)
	// While the manual lock is held the blob store cannot release its flock
	// and Close cannot finish: a reopen attempt must fail.
	select {
	case <-closeDone:
		t.Fatal("Close finished while the object lock was still held")
	default:
	}
	if _, err := newCICacheBlobStore(fx.root); err == nil {
		t.Fatal("blob store reopened while Close was still in flight")
	}
	unlock()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after lock release")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("queued handler still running")
	}
	select {
	case <-putDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client PUT never terminated")
	}
	var entries int
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ci_cache_entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Fatalf("late publication after Close: %d entries", entries)
	}
	held, _ := ciCachePoolCounters(t, fx.s)
	if held == 0 {
		t.Fatal("queued-close path refunded held quota")
	}
}

// TestCICacheHTTPGitHubBlocked proves the whole auth stage — including the
// privileged GitHub source lookup — is bounded by the token-clipped budget:
// a repository GET that stalls until its context ends must not hold the
// handler past ~1s for a 299s-old token, with no client abort.
func TestCICacheHTTPGitHubBlocked(t *testing.T) {
	fx := ciCacheHTTPFixtureNew(t)
	fx.source.mu.Lock()
	orig := fx.source.respond
	fx.source.mu.Unlock()
	entered := make(chan struct{}, 1)
	sourceStopped := make(chan struct{}, 1)
	fx.source.setRespond(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repositories/") {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			select {
			case sourceStopped <- struct{}{}:
			default:
			}
			return
		}
		orig(w, r)
	})
	token := fx.token(func(c map[string]any) {
		at := fx.s.now().Add(-299 * time.Second).Unix()
		c["iat"] = at
		c["nbf"] = at
	})
	ts, done := ciCacheDoneServer(t, fx.srv)
	raw, _ := json.Marshal(ciCacheReservePayload("gh/key", "v1",
		fx.proof.RunnerName, 16, ciCacheDigestOf([]byte("0123456789abcdef"))))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.URL+"/v1/ci/cache/reservations", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// The clipped auth budget kills the connection at expiry, so the client
	// may see a transport error instead of an HTTP status; a completed
	// response is equally valid. What must hold is bounded handler exit.
	// Absolute 1.5s window measured from before the client call: the 3s
	// request context is only a fail-safe and must never be what ends it.
	start := time.Now()
	deadline := start.Add(1500 * time.Millisecond)
	resp, err := ts.Client().Do(req)
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("client call alone took %v", time.Since(start))
	}
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} else {
		t.Logf("transport ended at auth budget: %v", err)
	}
	remaining := func() time.Duration { return time.Until(deadline) }
	select {
	case <-entered:
	case <-time.After(remaining()):
		t.Fatal("blocked repository lookup never reached the mock")
	}
	select {
	case <-done:
	case <-time.After(remaining()):
		t.Fatal("handler outlived the clipped auth budget")
	}
	select {
	case <-sourceStopped:
	case <-time.After(remaining()):
		t.Fatal("mock request context was never canceled")
	}
	var objects int
	if err := fx.s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ci_cache_objects WHERE backend_digest=$1`,
		fx.srv.BackendDigest()).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if objects != 0 {
		t.Fatalf("blocked source created metadata: %d", objects)
	}
}

func TestCICacheHTTPJWKSAndCtor(t *testing.T) {
	// Dedicated JWKS transport: bounded body, no proxy, no redirects.
	client := ciCacheJWKSClient()
	if client == nil || client.Timeout != ciCacheJWKSDeadline {
		t.Fatalf("jwks client: %+v", client)
	}
	tr, ok := client.Transport.(ciCacheJWKSTransport)
	if !ok {
		t.Fatalf("jwks transport: %T", client.Transport)
	}
	base, ok := tr.base.(*http.Transport)
	if !ok || base.Proxy != nil {
		t.Fatal("jwks transport must disable proxy")
	}
	if client.CheckRedirect == nil {
		t.Fatal("jwks client must reject redirects")
	}
	// Body cap: over-limit reads fail, under-limit pass through.
	body := &ciCacheJWKSBody{rc: io.NopCloser(bytes.NewReader(
		bytes.Repeat([]byte("k"), ciCacheJWKSBytes+16))), left: ciCacheJWKSBytes}
	buf := make([]byte, ciCacheJWKSBytes+32)
	var read int
	for {
		n, err := body.Read(buf[read:])
		read += n
		if err != nil {
			if !errors.Is(err, errCICacheJWKSBytes) {
				t.Fatalf("jwks body error: %v", err)
			}
			break
		}
		if read >= len(buf) {
			t.Fatal("jwks body never capped")
		}
	}
	if read != ciCacheJWKSBytes {
		t.Fatalf("jwks body read %d", read)
	}
	// Constructor context canceled AFTER return must not kill the server:
	// the runtime context is owned independently.
	fx := ciCacheHTTPFixtureNew(t)
	ctx, cancel := context.WithCancel(context.Background())
	key, sign := ciCacheTestSigner(t)
	_ = sign
	srv, err := NewCICacheServer(ctx, CICacheServerConfig{
		Store: fx.s, RootDir: ciCacheBlobTestRoot(t), PublicURL: "https://cache.example.com",
		GitHub: CIConfig{AppID: 1, AppSlug: "x", PrivateKeyPEM: ciTestKey(t),
			WebhookSecret: []byte(strings.Repeat("w", 32))},
		AllowLoopbackHTTP: true, keySet: ciCacheTestKeys(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cancel()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/ci/cache/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatal("server died with constructor context")
	}
	// Rejected configs fail before any filesystem side effect.
	badRoot := filepath.Join(t.TempDir(), "never-created")
	if _, err := NewCICacheServer(context.Background(), CICacheServerConfig{
		Store: nil, RootDir: badRoot, PublicURL: "https://x.example.com"}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := os.Stat(badRoot); !os.IsNotExist(err) {
		t.Fatal("filesystem root created on rejected config")
	}
}
