package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/pricing"
)

const (
	ciCacheTestBackendStatic = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ciCacheTestWriter        = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	ciCacheTestCheckID       = "9001"
)

// ciCacheRepoFixture carries the server-sourced GitHub identity a signed
// token must match, plus the trusted check-run ID the source lookup returns.
type ciCacheRepoFixture struct {
	Proof                ciRunnerJobProof
	OwnerID              int64
	GitHubInstallationID int64
	CheckRunID           string
}

func ciCacheRepoFor(proof ciRunnerJobProof, ownerID, ghInstall int64, checkRun string) ciCacheRepoFixture {
	return ciCacheRepoFixture{Proof: proof, OwnerID: ownerID,
		GitHubInstallationID: ghInstall, CheckRunID: checkRun}
}

// ciCacheAdmittedFixture builds a real admitted running job through the
// actual admission path, with a tracked lifecycle, on the deterministic
// store clock. No authorization JSON is ever hand-written.
func ciCacheAdmittedFixture(t *testing.T) (*Store, string, string, time.Time, ciAdmissionRequest, ciRunnerJobProof) {
	t.Helper()
	s, workspace, runner, start, req, proof := ciAdmissionJobFixture(t)
	lcTrack(t, s, workspace, runner, lcPlan(s.now()))
	if _, err := s.admitCIRunnerJob(context.Background(), workspace, runner, req, &proof, 7200); err != nil {
		t.Fatal(err)
	}
	return s, workspace, runner, start, req, proof
}

// ciCacheOtherFixture admits a second, fully independent workspace/job on an
// existing store WITHOUT resetting shared tables, so foreign-scope denials
// prove isolation rather than truncation.
func ciCacheOtherFixture(t *testing.T, s *Store, start time.Time) (string, ciRunnerJobProof) {
	t.Helper()
	ctx := context.Background()
	acct, ws, _, err := s.Signup(ctx, "cache-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	workspace := ws.ID
	stmts := []struct {
		q string
		a []any
	}{
		{`INSERT INTO ci_github_installations(id,workspace_id,github_installation_id,github_account_id,owner,account_type,created_by) VALUES('ci_cache_other_install',$1,124,457,'cache-other','Organization',$2)`, []any{workspace, acct.ID}},
		{`INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name) VALUES($1,'ci_cache_other_install',790,'other-cache/repo')`, []any{workspace}},
		{`INSERT INTO ci_settings(workspace_id,paid_overrun_enabled) VALUES($1,true)`, []any{workspace}},
		{`INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end) VALUES($1,'cus_ci2','sub_ci2','active',$2,$3)`, []any{workspace, start.Add(-24 * time.Hour), start.Add(72 * time.Hour)}},
		{`INSERT INTO workspace_products(workspace_id,product,enabled_by_kind,enabled_by_id) VALUES($1,'ci','human',$2)`, []any{workspace, acct.ID}},
		{`INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document) VALUES('ci_cache_other_job',$1,'ci_cache_other_install',790,1002,2001,'queued','{}'::jsonb)`, []any{workspace}},
	}
	for i, st := range stmts {
		if _, err := s.pool.Exec(ctx, st.q, st.a...); err != nil {
			t.Fatalf("stmt %d: %v", i, err)
		}
	}
	price, ok := pricing.FindCI("linux-x64-2")
	if !ok {
		t.Fatal("invalid test size")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,phase,document,rate_version,rate_micros_per_minute,paid_overrun_enabled,created_at) VALUES('ci_cache_other_runner',$1,'ci_cache_other_install','linux-x64-2','ci_cache_other_job','queued','{}'::jsonb,$2,$3,true,$4)`, workspace, pricing.CIRateVersion, price.MicrosPerMinute, start); err != nil {
		t.Fatal(err)
	}
	ciAdmissionPhase(t, s, workspace, "ci_cache_other_runner", "registering", false)
	scope := ciRunnerJobScope{RepositoryID: 790, RunnerID: 32,
		RunnerName: "canter-cache-other", SizeID: "linux-x64-2", CreatedAt: start}
	if err := s.registerCIRunnerAdmission(ctx, workspace, "ci_cache_other_runner", ciAdmissionNonce("e"), scope); err != nil {
		t.Fatalf("register: %v", err)
	}
	plan := lcPlan(s.now())
	plan.OperationID = "op_ci_cache_other"
	plan.ServerName = "canter-cache-other"
	plan.EnforcerLease = "lease_cache_other"
	if err := s.trackCIRunnerLifecycle(ctx, workspace, "ci_cache_other_runner", plan); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	req := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: ciAdmissionNonce("e"),
		GitHubRunnerID: 32, GitHubRepositoryID: 790, Repository: "other-cache/repo",
		GitHubRunID: 2001, RunAttempt: 1, JobKey: "build", RunnerName: "canter-cache-other",
	}
	proof := ciAdmissionJobProof(req, 1002, start.Add(5*time.Second))
	if _, err := s.admitCIRunnerJob(ctx, workspace, "ci_cache_other_runner", req, &proof, 7200); err != nil {
		t.Fatalf("admit: %v", err)
	}
	return workspace, proof
}

// ciCacheSiblingJob admits a second running job inside an EXISTING workspace
// so a later job can hit cache published by a completed earlier job.
func ciCacheSiblingJob(t *testing.T, s *Store, workspace string, start time.Time, suffix string) ciRunnerJobProof {
	t.Helper()
	ctx := context.Background()
	jobID, runnerID, ghJob, ghRun := "ci_cache_sibling_job_"+suffix, "ci_cache_sibling_runner_"+suffix, int64(1004), int64(2001)
	ghRunner, ghRunnerName := int64(33), "canter-test-"+suffix
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_jobs(id,workspace_id,installation_id,github_repository_id,github_job_id,github_run_id,status,document) VALUES($1,$2,'ci_financial_installation',789,$3,$4,'queued','{}'::jsonb)`, jobID, workspace, ghJob, ghRun); err != nil {
		t.Fatal(err)
	}
	price, ok := pricing.FindCI("linux-x64-2")
	if !ok {
		t.Fatal("invalid test size")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,phase,document,rate_version,rate_micros_per_minute,paid_overrun_enabled,created_at) VALUES($1,$2,'ci_financial_installation','linux-x64-2',$3,'queued','{}'::jsonb,$4,$5,true,$6)`, runnerID, workspace, jobID, pricing.CIRateVersion, price.MicrosPerMinute, start); err != nil {
		t.Fatal(err)
	}
	ciAdmissionPhase(t, s, workspace, runnerID, "registering", false)
	scope := ciRunnerJobScope{RepositoryID: 789, RunnerID: ghRunner,
		RunnerName: ghRunnerName, SizeID: "linux-x64-2", CreatedAt: start}
	if err := s.registerCIRunnerAdmission(ctx, workspace, runnerID, ciAdmissionNonce("3"), scope); err != nil {
		t.Fatal(err)
	}
	plan := lcPlan(s.now())
	plan.OperationID = "op_ci_cache_" + suffix
	plan.EnforcerLease = "lease_cache_" + suffix
	lcTrack(t, s, workspace, runnerID, plan)
	req := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: ciAdmissionNonce("3"),
		GitHubRunnerID: ghRunner, GitHubRepositoryID: 789, Repository: ciAdmissionRepoName,
		GitHubRunID: ghRun, RunAttempt: 1, JobKey: "build", RunnerName: ghRunnerName,
	}
	proof := ciAdmissionJobProof(req, ghJob, start.Add(5*time.Second))
	if _, err := s.admitCIRunnerJob(ctx, workspace, runnerID, req, &proof, 7200); err != nil {
		t.Fatal(err)
	}
	return proof
}

func ciCacheClaimsForRepo(now time.Time, repo ciCacheRepoFixture) map[string]any {
	claims := ciCacheValidClaims(now)
	claims["repository_id"] = strconv.FormatInt(repo.Proof.RepositoryID, 10)
	claims["repository_owner_id"] = strconv.FormatInt(repo.OwnerID, 10)
	claims["repository"] = repo.Proof.Repository
	claims["run_id"] = strconv.FormatInt(repo.Proof.RunID, 10)
	claims["run_attempt"] = strconv.FormatInt(repo.Proof.Attempt, 10)
	claims["check_run_id"] = repo.CheckRunID
	return claims
}

// ciCacheMockGitHub serves the exact repository lookup, job lookup and
// check-run URL binding ciCacheCheckRun performs. mutate may rewrite the job
// document; base is this mock's own origin so URL variants stay same-origin.
func ciCacheMockGitHubFor(t *testing.T, repo ciCacheRepoFixture, mutate func(map[string]any, string)) *ciSourceFixture {
	t.Helper()
	fx := newCISourceFixture(t)
	owner, name, _ := strings.Cut(repo.Proof.Repository, "/")
	fx.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			fmt.Fprint(w, `{"token":"ghs_cache_fixture"}`)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repositories/%d", repo.Proof.RepositoryID):
			fmt.Fprintf(w, `{"id":%d,"full_name":%q,"private":false}`, repo.Proof.RepositoryID, repo.Proof.Repository)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/%s/%s/actions/jobs/%d", owner, name, repo.Proof.JobID):
			job := map[string]any{
				"id": repo.Proof.JobID, "run_id": repo.Proof.RunID, "run_attempt": repo.Proof.Attempt,
				"runner_id": repo.Proof.RunnerID, "runner_name": repo.Proof.RunnerName,
				"status": "in_progress", "started_at": repo.Proof.StartedAt.UTC().Format(time.RFC3339Nano),
				"completed_at": nil, "conclusion": nil,
				"check_run_url": fx.server.URL + "/repos/" + owner + "/" + name + "/check-runs/" + repo.CheckRunID,
			}
			if mutate != nil {
				mutate(job, fx.server.URL)
			}
			raw, _ := json.Marshal(job)
			w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
	return fx
}

// ciCachePipelineFor runs the real crypto -> DB active-job -> GitHub
// check-run -> grant chain and returns the private grant plus its fixture.
func ciCachePipelineFor(t *testing.T, s *Store, repo ciCacheRepoFixture) (ciCacheGrant, *ciSourceFixture) {
	t.Helper()
	ctx := context.Background()
	key, sign := ciCacheTestSigner(t)
	raw := ciCacheSignClaims(t, sign, ciCacheClaimsForRepo(s.now(), repo))
	nowFn := func() time.Time { return s.now() }
	token, err := verifyCICacheToken(ctx, ciCacheTestKeys(key), nowFn, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ciCacheActiveJob(ctx, s.pool, repo.Proof.JobID, token, false)
	if err != nil {
		t.Fatal(err)
	}
	fx := ciCacheMockGitHubFor(t, repo, nil)
	checkRun, err := fx.client.ciCacheCheckRun(ctx, job, token, nowFn)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := makeCICacheGrant(job, token, checkRun, s.now())
	if err != nil {
		t.Fatal(err)
	}
	return grant, fx
}

func ciCacheBlobFor(t *testing.T) (*ciCacheBlobStore, string) {
	t.Helper()
	root := ciCacheBlobTestRoot(t)
	store, err := newCICacheBlobStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, root
}

func ciCachePoolConfigFor(backend string, total, workspace, entry int64, objects, workspaceObjects int64, ttl int64) ciCachePoolConfig {
	return ciCachePoolConfig{
		BackendDigest: backend, Enabled: true,
		MaxTotalBytes: total, MaxWorkspaceBytes: workspace, MaxEntryBytes: entry,
		MaxObjects: objects, MaxWorkspaceObjects: workspaceObjects, TTLSeconds: ttl,
	}
}

func ciCachePoolCounters(t *testing.T, s *Store) (int64, int64) {
	t.Helper()
	state, err := s.ciCachePoolState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state.ReservedBytes, int64(state.ReservedObjects)
}

func ciCacheWait(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func ciCacheObjectState(t *testing.T, s *Store, id string) string {
	t.Helper()
	var state string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT state FROM ci_cache_objects WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// ciCacheRemoveOwned drives owned objects through invalidate -> cleanup
// claim -> real blob delete -> absence ack so no quota leaks between tests.
// The backend digest is unique per blob store, so every claim returned for
// it belongs to this test; all are deleted and acked, never just the first.
func ciCacheRemoveOwned(t *testing.T, s *Store, blob *ciCacheBlobStore, backend string, grant ciCacheGrant, id string) {
	t.Helper()
	ctx := context.Background()
	var state string
	if err := s.pool.QueryRow(ctx,
		`SELECT state FROM ci_cache_objects WHERE id=$1`, id).Scan(&state); err == nil && state == "deleted" {
		return
	}
	if err := s.invalidateCICacheObject(ctx, grant, backend, id); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalidate owned object: %v", err)
	}
	found := false
	for attempt := 0; attempt < 8 && !found; attempt++ {
		claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
		if err != nil {
			t.Fatal(err)
		}
		if len(claims) == 0 {
			break
		}
		for _, claim := range claims {
			absent, err := blob.Delete(ctx, claim.ObjectID)
			if err != nil || !absent {
				t.Fatalf("owned delete failed: absent=%v err=%v", absent, err)
			}
			if err := s.ackCICacheAbsent(ctx, claim); err != nil {
				t.Fatal(err)
			}
			found = found || claim.ObjectID == id
		}
	}
	if !found {
		t.Fatalf("no cleanup claim for owned object %s", id)
	}
}

func TestCICacheMigrationPoolDefaultsAndSecondMigrate(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	before, err := s.ciCachePoolState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Observe the migration's actual defaults through an owned temp probe;
	// never reconfigure or reset the shared singleton.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE ci_cache_pool_defaults_probe (LIKE ci_cache_pool INCLUDING DEFAULTS INCLUDING CONSTRAINTS) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ci_cache_pool_defaults_probe(singleton) VALUES(true)`); err != nil {
		t.Fatal(err)
	}
	probe, err := ciCacheScanPool(tx.QueryRow(ctx, `
SELECT backend_digest,enabled,max_total_bytes,max_workspace_bytes,max_entry_bytes,
       max_objects,max_workspace_objects,ttl_seconds,reserved_bytes,reserved_objects
FROM ci_cache_pool_defaults_probe`))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Enabled || probe.BackendDigest != "" || probe.MaxTotalBytes != 0 ||
		probe.MaxWorkspaceBytes != 0 || probe.MaxEntryBytes != 0 || probe.MaxObjects != 0 ||
		probe.MaxWorkspaceObjects != 0 || probe.TTLSeconds != 0 ||
		probe.ReservedBytes != 0 || probe.ReservedObjects != 0 {
		t.Fatalf("pool defaults not secure: %+v", probe)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate not idempotent: %v", err)
	}
	after, err := s.ciCachePoolState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("second migrate changed pool: %+v -> %+v", before, after)
	}
}

func TestCICacheMetadataPoolConfigAndEpoch(t *testing.T) {
	ctx := context.Background()
	s := integrationStore(t)
	poolBefore, err := s.ciCachePoolState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Epoch semantics first: a user cacheEnabled save never changes the
	// operator pool, whatever state it is already in.
	s2, conversation, _ := operatorFixture(t)
	settings, err := s2.CISettings(ctx, conversation.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	settings.CacheEnabled = false
	if err := s2.SaveCISettings(ctx, conversation.WorkspaceID, conversation.AccountID, settings); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var enabled bool
	if err := s2.pool.QueryRow(ctx,
		`SELECT cache_epoch,cache_enabled FROM ci_settings WHERE workspace_id=$1`,
		conversation.WorkspaceID).Scan(&epoch, &enabled); err != nil {
		t.Fatal(err)
	}
	if enabled || epoch != 2 {
		t.Fatalf("first disabled save: enabled=%v epoch=%d", enabled, epoch)
	}
	settings.CacheEnabled = true
	if err := s2.SaveCISettings(ctx, conversation.WorkspaceID, conversation.AccountID, settings); err != nil {
		t.Fatal(err)
	}
	var epoch2 int64
	if err := s2.pool.QueryRow(ctx,
		`SELECT cache_epoch FROM ci_settings WHERE workspace_id=$1`,
		conversation.WorkspaceID).Scan(&epoch2); err != nil {
		t.Fatal(err)
	}
	if epoch2 != 3 {
		t.Fatalf("reenable epoch = %d, want 3", epoch2)
	}
	again := settings
	again.PaidOverrunEnabled = !settings.PaidOverrunEnabled
	if err := s2.SaveCISettings(ctx, conversation.WorkspaceID, conversation.AccountID, again); err != nil {
		t.Fatal(err)
	}
	var epoch3 int64
	if err := s2.pool.QueryRow(ctx,
		`SELECT cache_epoch FROM ci_settings WHERE workspace_id=$1`,
		conversation.WorkspaceID).Scan(&epoch3); err != nil {
		t.Fatal(err)
	}
	if epoch3 != 3 {
		t.Fatalf("unrelated save changed epoch: %d", epoch3)
	}
	poolAfter, err := s2.ciCachePoolState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if poolAfter != poolBefore {
		t.Fatalf("user cache settings changed operator pool: %+v -> %+v", poolBefore, poolAfter)
	}

	// Pool configuration validation on a known-disabled singleton.
	if err := s.configureCICachePool(ctx, ciCachePoolConfig{BackendDigest: ciCacheTestBackendStatic}); err != nil {
		t.Fatal(err)
	}
	good := ciCachePoolConfigFor(ciCacheTestBackendStatic, 1<<20, 1<<19, 1<<18, 100, 50, 3600)
	for name, mutate := range map[string]func(*ciCachePoolConfig){
		"bad backend":      func(c *ciCachePoolConfig) { c.BackendDigest = "zz" },
		"entry too big":    func(c *ciCachePoolConfig) { c.MaxEntryBytes = ciCacheBlobMaxSize + 1 },
		"entry>workspace":  func(c *ciCachePoolConfig) { c.MaxEntryBytes = c.MaxWorkspaceBytes + 1 },
		"workspace>total":  func(c *ciCachePoolConfig) { c.MaxWorkspaceBytes = c.MaxTotalBytes + 1 },
		"objects inverted": func(c *ciCachePoolConfig) { c.MaxWorkspaceObjects = c.MaxObjects + 1 },
		"objects too many": func(c *ciCachePoolConfig) { c.MaxObjects = ciCacheMaxObjects + 1 },
		"ttl too long":     func(c *ciCachePoolConfig) { c.TTLSeconds = int64(ciCacheMaxRetention/time.Second) + 1 },
		"enabled no ttl":   func(c *ciCachePoolConfig) { c.TTLSeconds = 0 },
		"enabled no entry": func(c *ciCachePoolConfig) { c.MaxEntryBytes = 0 },
		"enabled no objs":  func(c *ciCachePoolConfig) { c.MaxWorkspaceObjects = 0 },
		"negative bytes":   func(c *ciCachePoolConfig) { c.MaxTotalBytes = -1 },
	} {
		cfg := good
		mutate(&cfg)
		if err := s.configureCICachePool(ctx, cfg); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: expected ErrConflict, got %v", name, err)
		}
	}
	if err := s.configureCICachePool(ctx, good); err != nil {
		t.Fatal(err)
	}
	state, err := s.ciCachePoolState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || state.BackendDigest != ciCacheTestBackendStatic ||
		state.MaxTotalBytes != good.MaxTotalBytes || state.TTLSeconds != good.TTLSeconds {
		t.Fatalf("pool state mismatch: %+v", state)
	}
}

func TestCICacheMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, workspace, _, start, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	cfg := ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)
	if err := s.configureCICachePool(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	payload := "canter-cache-payload-8"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	obj, created, err := s.reserveCICacheObject(ctx, grant, backend, "deps/node", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve: created=%v err=%v", created, err)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, obj.ID) })
	if obj.State != "reserved" || obj.SizeBytes != size || obj.SHA256 != sha ||
		obj.KeyDigest == "" || obj.VersionDigest == "" {
		t.Fatalf("unexpected reserved object: %+v", obj)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != size || objects != 1 {
		t.Fatalf("pool counters %d/%d", bytes, objects)
	}
	dup, created, err := s.reserveCICacheObject(ctx, grant, backend, "deps/node", "v1", size, sha)
	if err != nil || created || dup.ID != "" {
		t.Fatalf("duplicate reserve: %+v created=%v err=%v", dup, created, err)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != size || objects != 1 {
		t.Fatalf("duplicate changed counters %d/%d", bytes, objects)
	}
	var claimed ciCacheObject
	createdBlob, err := blob.Put(ctx, obj.ID,
		io.NopCloser(strings.NewReader(payload)), size, sha,
		func(opCtx context.Context) error {
			var err error
			claimed, err = s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
			return err
		})
	if err != nil || !createdBlob {
		t.Fatalf("blob put: created=%v err=%v", createdBlob, err)
	}
	if claimed.State != "writing" || claimed.WriterToken != ciCacheTestWriter ||
		claimed.SizeBytes != size || claimed.SHA256 != sha {
		t.Fatalf("claim mismatch: %+v", claimed)
	}
	published, err := s.publishCICacheObject(ctx, grant, backend, obj.ID, ciCacheTestWriter)
	if err != nil || !published {
		t.Fatalf("publish: published=%v err=%v", published, err)
	}
	entry, found, err := s.lookupCICacheEntry(ctx, grant, backend, "deps/node", "v1")
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if entry.ObjectID != obj.ID || entry.SizeBytes != size || entry.SHA256 != sha {
		t.Fatalf("entry mismatch: %+v", entry)
	}
	if _, found, err = s.lookupCICacheEntry(ctx, grant, backend, "deps/node", "v2"); err != nil || found {
		t.Fatalf("version miss expected: found=%v err=%v", found, err)
	}
	if _, found, err = s.lookupCICacheEntry(ctx, grant, ciCacheTestBackendStatic, "deps/node", "v1"); err != nil || found {
		t.Fatalf("backend miss expected: found=%v err=%v", found, err)
	}
	download, err := s.ciCacheDownloadEntry(ctx, grant, backend, obj.ID)
	if err != nil || download.ObjectID != obj.ID {
		t.Fatalf("download entry: %+v err=%v", download, err)
	}
	reader, err := blob.Open(ctx, obj.ID, size, sha)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(got) != payload {
		t.Fatalf("blob roundtrip: %q err=%v", got, err)
	}

	// A real second workspace on the SAME store cannot see this object, and
	// the original grant still hits afterwards (no truncation involved).
	_, proof2 := ciCacheOtherFixture(t, s, start)
	repo2 := ciCacheRepoFor(proof2, 457, 124, "9002")
	grant2, _ := ciCachePipelineFor(t, s, repo2)
	if _, found, err = s.lookupCICacheEntry(ctx, grant2, backend, "deps/node", "v1"); err != nil || found {
		t.Fatalf("cross-workspace lookup leaked: found=%v err=%v", found, err)
	}
	if _, err = s.ciCacheDownloadEntry(ctx, grant2, backend, obj.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace download: %v", err)
	}
	if _, found, err = s.lookupCICacheEntry(ctx, grant, backend, "deps/node", "v1"); err != nil || !found {
		t.Fatalf("original grant lost entry: found=%v err=%v", found, err)
	}
	_ = workspace

	if err := s.invalidateCICacheObject(ctx, grant, backend, obj.ID); err != nil {
		t.Fatal(err)
	}
	claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	var mine *ciCacheCleanupClaim
	for i := range claims {
		if claims[i].ObjectID == obj.ID {
			mine = &claims[i]
		}
	}
	if mine == nil {
		t.Fatal("cleanup did not claim invalidated object")
	}
	absent, err := blob.Delete(ctx, obj.ID)
	if err != nil || !absent {
		t.Fatalf("delete: absent=%v err=%v", absent, err)
	}
	if err := s.ackCICacheAbsent(ctx, *mine); err != nil {
		t.Fatal(err)
	}
	if err := s.ackCICacheAbsent(ctx, *mine); err != nil {
		t.Fatalf("second ack not idempotent: %v", err)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 0 || objects != 0 {
		t.Fatalf("counters after ack %d/%d", bytes, objects)
	}
	var state, ns, kd, vd, shaCol, runner, job string
	err = s.pool.QueryRow(ctx,
		`SELECT state,namespace_digest,key_digest,version_digest,sha256,runner_id,source_job_id FROM ci_cache_objects WHERE id=$1`,
		obj.ID).Scan(&state, &ns, &kd, &vd, &shaCol, &runner, &job)
	if err != nil {
		t.Fatal(err)
	}
	if state != "deleted" || ns != "" || kd != "" || vd != "" || shaCol != "" || runner != "" || job != "" {
		t.Fatalf("deleted row retained private metadata: %s %q %q", state, ns, runner)
	}
}

func TestCICacheMetadataLateWriterAndCleanupRace(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	if err := s.configureCICachePool(ctx, ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)); err != nil {
		t.Fatal(err)
	}
	payload := "late-writer-payload"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	obj, created, err := s.reserveCICacheObject(ctx, grant, backend, "race/key", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve: %v %v", created, err)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, obj.ID) })

	// Cleanup wins first: claim commits state=cleanup, delete proves absence,
	// ack releases quota. A late Put authorize must then be denied.
	if err := s.invalidateCICacheObject(ctx, grant, backend, obj.ID); err != nil {
		t.Fatal(err)
	}
	claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	var mine *ciCacheCleanupClaim
	for i := range claims {
		if claims[i].ObjectID == obj.ID {
			mine = &claims[i]
		}
	}
	if mine == nil {
		t.Fatal("cleanup claim missing")
	}
	absent, err := blob.Delete(ctx, obj.ID)
	if err != nil || !absent {
		t.Fatalf("delete: %v %v", absent, err)
	}
	if err := s.ackCICacheAbsent(ctx, *mine); err != nil {
		t.Fatal(err)
	}
	putErr := make(chan error, 1)
	go func() {
		_, err := blob.Put(ctx, obj.ID,
			io.NopCloser(strings.NewReader(payload)), size, sha,
			func(opCtx context.Context) error {
				_, err := s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
				return err
			})
		putErr <- err
	}()
	if err := ciCacheWait(t, putErr, "late put"); err == nil {
		t.Fatal("late writer resurrected a deleted object")
	}
	if _, err := blob.Open(ctx, obj.ID, size, sha); err == nil {
		t.Fatal("late writer left a readable object")
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 0 || objects != 0 {
		t.Fatalf("late writer changed budget %d/%d", bytes, objects)
	}

	// Concurrent seam: cleanup commits while the writer's authorize callback
	// holds the blob lock; the DB claim must then fail and Delete proceeds.
	obj2, created, err := s.reserveCICacheObject(ctx, grant, backend, "race/key2", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve2: %v %v", created, err)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, obj2.ID) })
	entered := make(chan struct{})
	release := make(chan struct{})
	putDone := make(chan error, 1)
	go func() {
		_, err := blob.Put(ctx, obj2.ID,
			io.NopCloser(strings.NewReader(payload)), size, sha,
			func(opCtx context.Context) error {
				close(entered)
				<-release
				_, err := s.claimCICacheWrite(opCtx, grant, backend, obj2.ID, ciCacheTestWriter)
				return err
			})
		putDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("authorize callback never entered")
	}
	if err := s.invalidateCICacheObject(ctx, grant, backend, obj2.ID); err != nil {
		t.Fatal(err)
	}
	claims, err = s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	var mine2 *ciCacheCleanupClaim
	for i := range claims {
		if claims[i].ObjectID == obj2.ID {
			mine2 = &claims[i]
		}
	}
	if mine2 == nil {
		t.Fatal("cleanup claim missing for racing object")
	}
	deleteDone := make(chan error, 1)
	go func() {
		absent, err := blob.Delete(ctx, obj2.ID)
		if err == nil && !absent {
			err = errors.New("delete reported present")
		}
		deleteDone <- err
	}()
	close(release)
	if err := ciCacheWait(t, putDone, "racing put"); err == nil {
		t.Fatal("writer claimed after cleanup committed")
	}
	if err := ciCacheWait(t, deleteDone, "racing delete"); err != nil {
		t.Fatal(err)
	}
	if err := s.ackCICacheAbsent(ctx, *mine2); err != nil {
		t.Fatal(err)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 0 || objects != 0 {
		t.Fatalf("race left budget %d/%d", bytes, objects)
	}
}

func TestCICacheMetadataCapsAndHeldReservations(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	cfg := ciCachePoolConfigFor(backend, 8, 8, 8, 10, 10, 3600)
	if err := s.configureCICachePool(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	payload := "12345678"
	size := int64(8)
	sha := ciCacheDigestHex([]byte(payload))
	reserve := func(key string) (ciCacheObject, bool, error) {
		return s.reserveCICacheObject(ctx, grant, backend, key, "v1", size, sha)
	}
	// Two concurrent different-key 8-byte writes against an 8-byte pool:
	// exactly one may be created.
	type result struct {
		obj     ciCacheObject
		created bool
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"cap/a", "cap/b"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			obj, created, err := reserve(k)
			results <- result{obj, created, err}
		}(key)
	}
	wg.Wait()
	close(results)
	var winner ciCacheObject
	createdCount := 0
	for r := range results {
		if r.err == nil && r.created {
			createdCount++
			winner = r.obj
		} else if !errors.Is(r.err, ErrCapacity) {
			t.Fatalf("concurrent reserve: unexpected %v", r.err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("concurrent reserves created %d objects", createdCount)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, winner.ID) })
	if bytes, objects := ciCachePoolCounters(t, s); bytes != size || objects != 1 {
		t.Fatalf("counters after race %d/%d", bytes, objects)
	}
	// Bytes, entry and workspace caps are all exhausted by the held object.
	if _, _, err := reserve("cap/c"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second reserve expected ErrCapacity, got %v", err)
	}
	// Workspace object cap exercised independently of byte caps.
	wsObjCfg := cfg
	wsObjCfg.MaxTotalBytes = 1 << 20
	wsObjCfg.MaxWorkspaceBytes = 1 << 20
	wsObjCfg.MaxEntryBytes = 1 << 19
	wsObjCfg.MaxObjects = 10
	wsObjCfg.MaxWorkspaceObjects = 1
	if err := s.configureCICachePool(ctx, wsObjCfg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reserve("cap/d"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("workspace object cap expected ErrCapacity, got %v", err)
	}
	// Global object cap exercised independently.
	poolObjCfg := wsObjCfg
	poolObjCfg.MaxWorkspaceObjects = 1
	poolObjCfg.MaxObjects = 1
	if err := s.configureCICachePool(ctx, poolObjCfg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reserve("cap/e"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("pool object cap expected ErrCapacity, got %v", err)
	}
	// Lowering caps below held reservations and backend changes are rejected.
	lower := poolObjCfg
	lower.MaxTotalBytes = 7
	lower.MaxWorkspaceBytes = 7
	lower.MaxEntryBytes = 7
	if err := s.configureCICachePool(ctx, lower); !errors.Is(err, ErrConflict) {
		t.Fatalf("lower caps expected ErrConflict, got %v", err)
	}
	changed := poolObjCfg
	changed.BackendDigest = strings.Repeat("aa", 32)
	if err := s.configureCICachePool(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("backend change expected ErrConflict, got %v", err)
	}
	// Disabling keeps caps and held reservations intact.
	disabled := poolObjCfg
	disabled.Enabled = false
	if err := s.configureCICachePool(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	state, err := s.ciCachePoolState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled || state.ReservedBytes != size || state.ReservedObjects != 1 {
		t.Fatalf("disable lost accounting: %+v", state)
	}
	if _, _, err = reserve("cap/f"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("disabled pool reserve expected ErrCapacity, got %v", err)
	}
}

func TestCICacheMetadataPublishDeniedAfterInvalidation(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	if err := s.configureCICachePool(ctx, ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)); err != nil {
		t.Fatal(err)
	}
	payload := "sealed-before-denial"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	put := func(key string) ciCacheObject {
		obj, created, err := s.reserveCICacheObject(ctx, grant, backend, key, "v1", size, sha)
		if err != nil || !created {
			t.Fatalf("reserve %s: %v %v", key, created, err)
		}
		t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, obj.ID) })
		createdBlob, err := blob.Put(ctx, obj.ID,
			io.NopCloser(strings.NewReader(payload)), size, sha,
			func(opCtx context.Context) error {
				_, err := s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
				return err
			})
		if err != nil || !createdBlob {
			t.Fatalf("put %s: %v %v", key, createdBlob, err)
		}
		return obj
	}
	objEpoch := put("deny/epoch")
	objStop := put("deny/stop")

	entriesFor := func(id string) int {
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM ci_cache_entries WHERE object_id=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Epoch toggle between sealed put and publish denies publication; quota
	// stays held and no entry is created.
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_settings SET cache_epoch=cache_epoch+1 WHERE workspace_id=$1`, grant.job.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.publishCICacheObject(ctx, grant, backend, objEpoch.ID, ciCacheTestWriter); err == nil {
		t.Fatal("publish succeeded after epoch change")
	}
	if entriesFor(objEpoch.ID) != 0 {
		t.Fatal("denied publish created an entry")
	}
	if state := ciCacheObjectState(t, s, objEpoch.ID); state != "writing" {
		t.Fatalf("denied publish changed state: %s", state)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 2*size || objects != 2 {
		t.Fatalf("denied publish released quota: %d/%d", bytes, objects)
	}
	// Restore the epoch, then stop the job: publish must still be denied.
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_settings SET cache_epoch=cache_epoch-1 WHERE workspace_id=$1`, grant.job.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	var internalJob string
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM ci_jobs WHERE workspace_id=$1 AND github_job_id=$2`,
		grant.job.WorkspaceID, proof.JobID).Scan(&internalJob); err != nil {
		t.Fatal(err)
	}
	ciAdmissionSetJobStatus(t, s, grant.job.WorkspaceID, internalJob, "completed")
	if _, err := s.publishCICacheObject(ctx, grant, backend, objStop.ID, ciCacheTestWriter); err == nil {
		t.Fatal("publish succeeded after job stop")
	}
	if entriesFor(objStop.ID) != 0 {
		t.Fatal("post-stop publish created an entry")
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 2*size || objects != 2 {
		t.Fatalf("post-stop publish released quota: %d/%d", bytes, objects)
	}
}

func TestCICacheMetadataUncertainDeleteRetry(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, root := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	if err := s.configureCICachePool(ctx, ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)); err != nil {
		t.Fatal(err)
	}
	payload := "uncertain-delete"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	obj, created, err := s.reserveCICacheObject(ctx, grant, backend, "retry/key", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve: %v %v", created, err)
	}
	createdBlob, err := blob.Put(ctx, obj.ID,
		io.NopCloser(strings.NewReader(payload)), size, sha,
		func(opCtx context.Context) error {
			_, err := s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
			return err
		})
	if err != nil || !createdBlob {
		t.Fatalf("put: %v %v", createdBlob, err)
	}
	if _, err := s.publishCICacheObject(ctx, grant, backend, obj.ID, ciCacheTestWriter); err != nil {
		t.Fatal(err)
	}
	if err := s.invalidateCICacheObject(ctx, grant, backend, obj.ID); err != nil {
		t.Fatal(err)
	}
	claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	var mine *ciCacheCleanupClaim
	for i := range claims {
		if claims[i].ObjectID == obj.ID {
			mine = &claims[i]
		}
	}
	if mine == nil {
		t.Fatal("cleanup claim missing")
	}
	// Make physical deletion uncertain: replace our own final blob with a
	// symlink the backend must refuse to treat as an owned object.
	blobPath := filepath.Join(root, "objects", obj.ID+".blob")
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", blobPath); err != nil {
		t.Fatal(err)
	}
	if absent, err := blob.Delete(ctx, obj.ID); err == nil && absent {
		t.Fatal("delete claimed absence over foreign symlink")
	}
	if err := s.retryCICacheCleanup(ctx, *mine); err != nil {
		t.Fatal(err)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != size || objects != 1 {
		t.Fatalf("uncertain delete refunded quota: %d/%d", bytes, objects)
	}
	// Remove ONLY our own symlink. Retry cleared the claim token, so the
	// object must be re-claimed after its next_attempt delay before a fresh
	// delete + ack can release quota exactly once.
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	prevNow := s.now
	s.now = func() time.Time { return prevNow().Add(6 * time.Second) }
	claims, err = s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	mine = nil
	for i := range claims {
		if claims[i].ObjectID == obj.ID {
			mine = &claims[i]
		}
	}
	if mine == nil {
		t.Fatal("re-claim after retry missing")
	}
	absent, err := blob.Delete(ctx, obj.ID)
	if err != nil || !absent {
		t.Fatalf("fresh delete: absent=%v err=%v", absent, err)
	}
	if err := s.ackCICacheAbsent(ctx, *mine); err != nil {
		t.Fatal(err)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 0 || objects != 0 {
		t.Fatalf("counters after ack %d/%d", bytes, objects)
	}
}

func TestCICacheMetadataNamespaceCascade(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	if err := s.configureCICachePool(ctx, ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)); err != nil {
		t.Fatal(err)
	}
	payload := "namespace-cascade"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	obj, created, err := s.reserveCICacheObject(ctx, grant, backend, "ns/key", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve: %v %v", created, err)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, obj.ID) })
	createdBlob, err := blob.Put(ctx, obj.ID,
		io.NopCloser(strings.NewReader(payload)), size, sha,
		func(opCtx context.Context) error {
			_, err := s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
			return err
		})
	if err != nil || !createdBlob {
		t.Fatalf("put: %v %v", createdBlob, err)
	}
	if _, err := s.publishCICacheObject(ctx, grant, backend, obj.ID, ciCacheTestWriter); err != nil {
		t.Fatal(err)
	}
	// Deleting our own namespace row cascades the private entry away, but the
	// object ledger must keep holding quota until physical absence.
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM ci_cache_namespaces WHERE digest=$1`, grant.namespace); err != nil {
		t.Fatal(err)
	}
	var entries int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM ci_cache_entries WHERE object_id=$1`, obj.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Fatal("namespace delete did not cascade entry")
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != size || objects != 1 {
		t.Fatalf("namespace delete released quota early: %d/%d", bytes, objects)
	}
	if err := s.invalidateCICacheObject(ctx, grant, backend, obj.ID); err != nil {
		t.Fatal(err)
	}
	claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	var mine *ciCacheCleanupClaim
	for i := range claims {
		if claims[i].ObjectID == obj.ID {
			mine = &claims[i]
		}
	}
	if mine == nil {
		t.Fatal("cleanup claim missing after namespace cascade")
	}
	absent, err := blob.Delete(ctx, obj.ID)
	if err != nil || !absent {
		t.Fatalf("delete: absent=%v err=%v", absent, err)
	}
	if err := s.ackCICacheAbsent(ctx, *mine); err != nil {
		t.Fatal(err)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != 0 || objects != 0 {
		t.Fatalf("counters after ack %d/%d", bytes, objects)
	}
}

func TestCICacheMetadataPublishedSurvivesJobCompletion(t *testing.T) {
	ctx := context.Background()
	s, workspace, _, start, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	grant, _ := ciCachePipelineFor(t, s, repo)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	if err := s.configureCICachePool(ctx, ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)); err != nil {
		t.Fatal(err)
	}
	payload := "shared-trust-scope"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	obj, created, err := s.reserveCICacheObject(ctx, grant, backend, "shared/deps", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve: %v %v", created, err)
	}
	createdBlob, err := blob.Put(ctx, obj.ID,
		io.NopCloser(strings.NewReader(payload)), size, sha,
		func(opCtx context.Context) error {
			_, err := s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
			return err
		})
	if err != nil || !createdBlob {
		t.Fatalf("put: %v %v", createdBlob, err)
	}
	if _, err := s.publishCICacheObject(ctx, grant, backend, obj.ID, ciCacheTestWriter); err != nil {
		t.Fatal(err)
	}
	// Complete the source job; the published entry must NOT be garbage
	// collected merely because its writer stopped.
	var internalJob string
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM ci_jobs WHERE workspace_id=$1 AND github_job_id=$2`,
		workspace, proof.JobID).Scan(&internalJob); err != nil {
		t.Fatal(err)
	}
	ciAdmissionSetJobStatus(t, s, workspace, internalJob, "completed")
	claims, err := s.claimCICacheCleanup(ctx, backend, ciCacheCleanupWorkers)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims {
		if claim.ObjectID == obj.ID {
			t.Fatal("cleanup claimed live published object after job completion")
		}
	}
	// A subsequent admitted job in the same workspace/repo/ref/event shares
	// the trust namespace and hits the published entry.
	proofB := ciCacheSiblingJob(t, s, workspace, start, "b")
	repoB := ciCacheRepoFor(proofB, 456, 123, "9004")
	grantB, _ := ciCachePipelineFor(t, s, repoB)
	if grantB.namespace != grant.namespace {
		t.Fatal("same-trust-scope job derived a different namespace")
	}
	entry, found, err := s.lookupCICacheEntry(ctx, grantB, backend, "shared/deps", "v1")
	if err != nil || !found || entry.ObjectID != obj.ID {
		t.Fatalf("subsequent job missed published cache: found=%v entry=%+v err=%v", found, entry, err)
	}
	download, err := s.ciCacheDownloadEntry(ctx, grantB, backend, obj.ID)
	if err != nil || download.ObjectID != obj.ID {
		t.Fatalf("download: %+v err=%v", download, err)
	}
	reader, err := blob.Open(ctx, obj.ID, size, sha)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(got) != payload {
		t.Fatalf("roundtrip via later job: %q err=%v", got, err)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grantB, obj.ID) })
}

func TestCICacheMetadataPublishClockFenced(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	blob, _ := ciCacheBlobFor(t)
	backend := blob.BackendDigest()
	base := s.now()
	var clockMu sync.Mutex
	current := base
	s.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	}
	// Token signed at base-299s, expiring base+301s: lifetime 600s valid, age
	// 299s under the 300s cap, expiry still future after a +2s advance.
	claims := ciCacheClaimsForRepo(base, repo)
	claims["iat"] = base.Add(-299 * time.Second).Unix()
	claims["nbf"] = base.Add(-299 * time.Second).Unix()
	claims["exp"] = base.Add(301 * time.Second).Unix()
	key, sign := ciCacheTestSigner(t)
	raw := ciCacheSignClaims(t, sign, claims)
	nowFn := func() time.Time { return s.now() }
	token, err := verifyCICacheToken(ctx, ciCacheTestKeys(key), nowFn, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ciCacheActiveJob(ctx, s.pool, proof.JobID, token, false)
	if err != nil {
		t.Fatal(err)
	}
	fx := ciCacheMockGitHubFor(t, repo, nil)
	checkRun, err := fx.client.ciCacheCheckRun(ctx, job, token, nowFn)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := makeCICacheGrant(job, token, checkRun, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.configureCICachePool(ctx, ciCachePoolConfigFor(backend, 1<<20, 1<<19, 1<<18, 100, 50, 3600)); err != nil {
		t.Fatal(err)
	}
	payload := "clock-fenced"
	size := int64(len(payload))
	sha := ciCacheDigestHex([]byte(payload))
	obj, created, err := s.reserveCICacheObject(ctx, grant, backend, "clock/key", "v1", size, sha)
	if err != nil || !created {
		t.Fatalf("reserve: %v %v", created, err)
	}
	t.Cleanup(func() { ciCacheRemoveOwned(t, s, blob, backend, grant, obj.ID) })
	createdBlob, err := blob.Put(ctx, obj.ID,
		io.NopCloser(strings.NewReader(payload)), size, sha,
		func(opCtx context.Context) error {
			_, err := s.claimCICacheWrite(opCtx, grant, backend, obj.ID, ciCacheTestWriter)
			return err
		})
	if err != nil || !createdBlob {
		t.Fatalf("put at base: %v %v", createdBlob, err)
	}

	// Hold the pool lock so publish blocks AFTER passing revalidation, then
	// advance the fake clock past the token's max age while SQL deadlines
	// (upload deadline base+120s, expiry) remain comfortably in the future.
	lockTx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx)
	if _, err := ciCacheScanPool(lockTx.QueryRow(ctx, ciCachePoolLockQuery)); err != nil {
		t.Fatal(err)
	}
	publishDone := make(chan error, 1)
	go func() {
		_, err := s.publishCICacheObject(ctx, grant, backend, obj.ID, ciCacheTestWriter)
		publishDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	waiting := false
	for !waiting {
		var exists bool
		if err := s.pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM pg_stat_activity
  WHERE datname=current_database() AND pid<>pg_backend_pid()
    AND wait_event_type='Lock' AND query=$1)`, ciCachePoolLockQuery).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		waiting = exists
		if !waiting {
			if time.Now().After(deadline) {
				t.Fatal("publish never waited on the pool lock")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	clockMu.Lock()
	current = base.Add(2 * time.Second)
	clockMu.Unlock()
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ciCacheWait(t, publishDone, "blocked publish"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("publish after token age: %v", err)
	}
	var entries int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM ci_cache_entries WHERE object_id=$1`, obj.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Fatal("clock-fenced publish created an entry")
	}
	if state := ciCacheObjectState(t, s, obj.ID); state != "writing" {
		t.Fatalf("clock-fenced publish changed state: %s", state)
	}
	if bytes, objects := ciCachePoolCounters(t, s); bytes != size || objects != 1 {
		t.Fatalf("clock-fenced publish released quota: %d/%d", bytes, objects)
	}
}

func TestCICacheJobScope(t *testing.T) {
	ctx := context.Background()
	s, workspace, runner, _, _, proof := ciCacheAdmittedFixture(t)
	key, sign := ciCacheTestSigner(t)
	keys := ciCacheTestKeys(key)
	nowFn := func() time.Time { return s.now() }

	tokenFor := func(claims map[string]any) ciCacheToken {
		raw := ciCacheSignClaims(t, sign, claims)
		token, err := verifyCICacheToken(ctx, keys, nowFn, raw, ciCacheTestAudience)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	jobFor := func(token ciCacheToken, githubJobID int64) (ciCacheActiveJob, error) {
		return s.ciCacheActiveJob(ctx, s.pool, githubJobID, token, false)
	}

	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	good := tokenFor(ciCacheClaimsForRepo(s.now(), repo))
	job, err := jobFor(good, proof.JobID)
	if err != nil {
		t.Fatalf("admitted job denied: %v", err)
	}
	if job.WorkspaceID != workspace || job.RunnerID != runner ||
		job.Proof.JobID != proof.JobID || job.Proof.RunID != proof.RunID ||
		job.Epoch < 1 {
		t.Fatalf("job scope mismatch: %+v", job)
	}
	// RUNNER_NAME resolves the same admitted job without a numeric job ID.
	byName, err := s.ciCacheJobForRunner(ctx, s.pool, proof.RunnerName, good)
	if err != nil || byName.Proof.JobID != proof.JobID {
		t.Fatalf("runner-name resolver: %+v %v", byName, err)
	}
	if _, err = s.ciCacheJobForRunner(ctx, s.pool, "no-such-runner", good); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("wrong runner name: %v", err)
	}
	// Wrong GitHub job ID hint resolves nothing.
	if _, err = jobFor(good, proof.JobID+1); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("wrong job id: %v", err)
	}
	// Signed run/attempt/repository mismatches deny on both resolvers.
	for name, mutate := range map[string]func(map[string]any){
		"run":     func(c map[string]any) { c["run_id"] = "9999" },
		"attempt": func(c map[string]any) { c["run_attempt"] = "9" },
		"repo":    func(c map[string]any) { c["repository_id"] = "788" },
		"owner":   func(c map[string]any) { c["repository_owner_id"] = "455" },
	} {
		claims := ciCacheClaimsForRepo(s.now(), repo)
		mutate(claims)
		token := tokenFor(claims)
		if _, err = jobFor(token, proof.JobID); !errors.Is(err, errCICacheUnauthorized) {
			t.Fatalf("%s: expected unauthorized, got %v", name, err)
		}
		if _, err = s.ciCacheJobForRunner(ctx, s.pool, proof.RunnerName, token); !errors.Is(err, errCICacheUnauthorized) {
			t.Fatalf("%s runner-name: expected unauthorized, got %v", name, err)
		}
	}
	// runner_environment is enforced at token verification, before job lookup.
	badEnv := ciCacheClaimsForRepo(s.now(), repo)
	badEnv["runner_environment"] = "github-hosted"
	rawBad := ciCacheSignClaims(t, sign, badEnv)
	if _, err = verifyCICacheToken(ctx, keys, nowFn, rawBad, ciCacheTestAudience); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("runner env: expected unauthorized, got %v", err)
	}
	// Terminal job denies.
	var internalJob string
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM ci_jobs WHERE workspace_id=$1 AND github_job_id=$2`,
		workspace, proof.JobID).Scan(&internalJob); err != nil {
		t.Fatal(err)
	}
	ciAdmissionSetJobStatus(t, s, workspace, internalJob, "completed")
	if _, err = jobFor(good, proof.JobID); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("terminal job: %v", err)
	}
	ciAdmissionSetJobStatus(t, s, workspace, internalJob, "running")
	// Cache disabled denies through the settings epoch path.
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_settings SET cache_enabled=false,cache_epoch=cache_epoch+1 WHERE workspace_id=$1`,
		workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = jobFor(good, proof.JobID); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("cache disabled: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_settings SET cache_enabled=true WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	// Repository disabled denies.
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_repositories SET enabled=false WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = jobFor(good, proof.JobID); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("repo disabled: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_repositories SET enabled=true WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	// Installation removed denies.
	if _, err := s.pool.Exec(ctx,
		`UPDATE ci_github_installations SET status='removed' WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = jobFor(good, proof.JobID); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("install removed: %v", err)
	}
}

func TestCICacheSource(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, proof := ciCacheAdmittedFixture(t)
	repo := ciCacheRepoFor(proof, 456, 123, ciCacheTestCheckID)
	key, sign := ciCacheTestSigner(t)
	nowFn := func() time.Time { return s.now() }
	raw := ciCacheSignClaims(t, sign, ciCacheClaimsForRepo(s.now(), repo))
	token, err := verifyCICacheToken(ctx, ciCacheTestKeys(key), nowFn, raw, ciCacheTestAudience)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.ciCacheActiveJob(ctx, s.pool, proof.JobID, token, false)
	if err != nil {
		t.Fatal(err)
	}

	// Valid binding returns only the numeric check-run ID after exactly the
	// token, repository and job requests.
	fx := ciCacheMockGitHubFor(t, repo, nil)
	checkRun, err := fx.client.ciCacheCheckRun(ctx, job, token, nowFn)
	if err != nil || checkRun != 9001 {
		t.Fatalf("check run: %d %v", checkRun, err)
	}
	if fx.count() != 3 {
		t.Fatalf("expected 3 source requests, got %d", fx.count())
	}
	// Absent run_attempt is accepted: the admitted job ID binds the attempt.
	fx2 := ciCacheMockGitHubFor(t, repo, func(j map[string]any, _ string) { delete(j, "run_attempt") })
	if _, err = fx2.client.ciCacheCheckRun(ctx, job, token, nowFn); err != nil {
		t.Fatalf("absent run_attempt rejected: %v", err)
	}
	base := "/repos/financial-tests/repo/check-runs/"
	for name, mutate := range map[string]func(map[string]any, string){
		"wrong job":      func(j map[string]any, _ string) { j["id"] = float64(proof.JobID + 1) },
		"wrong run":      func(j map[string]any, _ string) { j["run_id"] = float64(proof.RunID + 1) },
		"wrong runner":   func(j map[string]any, _ string) { j["runner_id"] = float64(proof.RunnerID + 1) },
		"wrong rname":    func(j map[string]any, _ string) { j["runner_name"] = "other-runner" },
		"wrong status":   func(j map[string]any, _ string) { j["status"] = "completed" },
		"wrong start":    func(j map[string]any, _ string) { j["started_at"] = "2026-01-01T00:00:00Z" },
		"completed set":  func(j map[string]any, _ string) { j["completed_at"] = "2026-01-01T01:00:00Z" },
		"conclusion set": func(j map[string]any, _ string) { j["conclusion"] = "success" },
		"wrong attempt":  func(j map[string]any, _ string) { j["run_attempt"] = float64(proof.Attempt + 1) },
		"foreign origin": func(j map[string]any, _ string) {
			j["check_run_url"] = "https://evil.example" + base + "9001"
		},
		"url query": func(j map[string]any, b string) {
			j["check_run_url"] = b + base + "9001?x=1"
		},
		"url fragment": func(j map[string]any, b string) {
			j["check_run_url"] = b + base + "9001#frag"
		},
		"url extra path": func(j map[string]any, b string) {
			j["check_run_url"] = b + base + "9001/extra"
		},
		"leading zero": func(j map[string]any, b string) {
			j["check_run_url"] = b + base + "09001"
		},
		"wrong checkrun": func(j map[string]any, b string) {
			j["check_run_url"] = b + base + "9002"
		},
	} {
		fx := ciCacheMockGitHubFor(t, repo, mutate)
		before := fx.count()
		if _, err = fx.client.ciCacheCheckRun(ctx, job, token, nowFn); !errors.Is(err, errCICacheUnauthorized) {
			t.Fatalf("%s: expected unauthorized, got %v", name, err)
		}
		if fx.count() != before+3 {
			t.Fatalf("%s: followed unexpected requests (%d -> %d)", name, before, fx.count())
		}
	}
	// A token that ages past the clock policy mid-pipeline fails before any
	// privileged request is made.
	baseTime := s.now()
	s.now = func() time.Time { return baseTime.Add(400 * time.Second) }
	fx3 := ciCacheMockGitHubFor(t, repo, nil)
	if _, err = fx3.client.ciCacheCheckRun(ctx, job, token, nowFn); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("aged token reached source: %v", err)
	}
	if fx3.count() != 0 {
		t.Fatalf("aged token triggered %d privileged calls", fx3.count())
	}
	// An unverified token is rejected before any request as well.
	fx4 := ciCacheMockGitHubFor(t, repo, nil)
	if _, err = fx4.client.ciCacheCheckRun(ctx, job, ciCacheToken{}, nowFn); !errors.Is(err, errCICacheUnauthorized) {
		t.Fatalf("unverified token reached source: %v", err)
	}
	if fx4.count() != 0 {
		t.Fatal("unverified token triggered privileged GitHub calls")
	}
}
