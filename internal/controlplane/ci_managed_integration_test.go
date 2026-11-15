package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real control-plane handoffs together. Only provider and
// GitHub HTTP are fixtures; no VM, external credential or paid capacity exists.
func TestCIManagedProvisioningAdmissionCleanup(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx, `UPDATE ci_settings SET paid_overrun_enabled=false WHERE workspace_id=$1`, workspace); err != nil {
		t.Fatal(err)
	}
	profile := ciManagedClaimProfile()
	policy := ciBudgetPolicy(s.now())
	policy.ProfileDigest, _ = profile.digest()
	ciBudgetGrant(t, s, "credit_integrated", 1000, policy)
	trigger := ciManagedQueuedJob(t, s, workspace, "linux-x64-2", 1)
	claim, err := s.claimCIManagedRunner(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	gh, _, ghClient := newGitHubFixtureRepo(t, 123, 789, "financial-tests", "repo")
	compute := newFakeCIManagedCompute(claim.Spec.Plan.OperationID)
	clock := &fakeClock{wall: s.now(), mono: s.now()}
	engine, _ := newTestEngineAt(t, compute, ghClient, clock)
	operatorToken := bytes.Repeat([]byte("o"), 40)
	daemon := &CIEnforcerServer{engine: engine, token: operatorToken}
	daemon.handler = daemon.routes()
	daemonHTTP := httptest.NewServer(daemon)
	defer daemonHTTP.Close()
	enforcer, err := newCIEnforcerClient(daemonHTTP.URL, operatorToken, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte("s"), 32)
	provisioner, err := newCIManagedProvisioner(s, profile, key, enforcer, compute)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Provision(ctx, workspace, claim.Runner, claim.Spec); err != nil {
		t.Fatal("integrated creation did not reach registration")
	}
	userdata, _, _, _, _, _, _ := compute.captured()
	capability := ciManagedUserdataToken(t, userdata)
	startedAt := s.now().Add(-30 * time.Second)
	var actualRunner atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/123/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "source-token-0123456789abcdef", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/repositories/789", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 789, "full_name": "financial-tests/repo", "private": true})
	})
	mux.HandleFunc("/repos/financial-tests/repo/actions/runs/4242/attempts/2/jobs", func(w http.ResponseWriter, r *http.Request) {
		id := actualRunner.Load()
		if id <= 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		job := map[string]any{
			"id": 5001, "run_id": 4242, "run_attempt": 2,
			"runner_id": id, "runner_name": claim.Spec.Plan.ServerName,
			"status": "in_progress", "started_at": startedAt.Format(time.RFC3339Nano),
			"completed_at": nil, "conclusion": nil,
			"labels": []string{"canter-2vcpu-ubuntu-2404"},
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []any{job}})
	})
	source := httptest.NewServer(mux)
	defer source.Close()
	h := &HTTPServer{
		service: &Service{Store: s},
		config: HTTPConfig{CI: CIConfig{
			AppID: 4321, AppSlug: "canter-ci", PrivateKeyPEM: ciTestKey(t),
			WebhookSecret: bytes.Repeat([]byte("w"), 32), APIBaseURL: source.URL,
			Admission: &CIAdmissionConfig{
				SigningKey: key, EnforcerURL: daemonHTTP.URL,
				EnforcerToken: operatorToken, AllowLoopbackHTTP: true,
			},
			Bootstrap: &CIBootstrapConfig{ProfileDigest: policy.ProfileDigest},
		}},
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/ci/runner-bootstrap", strings.NewReader(`{"version":1}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+capability)
	bootstrap := httptest.NewRecorder()
	h.ciGitHubPublic(bootstrap, request, []string{"runner-bootstrap"})
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("integrated bootstrap status=%d", bootstrap.Code)
	}
	var response ciBootstrapResponse
	if json.Unmarshal(bootstrap.Body.Bytes(), &response) != nil {
		t.Fatal("integrated bootstrap response does not decode")
	}
	actualRunner.Store(response.GitHubRunnerID)
	ciBudgetTotalsZero(t, s, workspace)
	admission := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: response.AdmissionID,
		GitHubRunnerID: response.GitHubRunnerID, GitHubRepositoryID: 789,
		Repository: "financial-tests/repo", GitHubRunID: 4242, RunAttempt: 2,
		JobKey: "build", RunnerName: claim.Spec.Plan.ServerName,
	}
	raw, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/ci/runner-admissions", bytes.NewReader(raw)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+response.AdmissionCapability)
	approved := httptest.NewRecorder()
	h.ciGitHubPublic(approved, request, []string{"runner-admissions"})
	if approved.Code != http.StatusOK {
		t.Fatalf("integrated admission status=%d", approved.Code)
	}
	var receipt ciAdmissionReceipt
	if json.Unmarshal(approved.Body.Bytes(), &receipt) != nil ||
		receipt.Decision != "allow" || receipt.ExecutionDeadlineUnixMs != startedAt.Add(time.Hour).UnixMilli() {
		t.Fatal("actual adoption did not receive its original free-hour deadline")
	}
	var original string
	var assigned *string
	if err := s.pool.QueryRow(ctx, `SELECT trigger_job_id,assigned_job_id FROM ci_runners WHERE workspace_id=$1 AND id=$2`,
		workspace, claim.Runner).Scan(&original, &assigned); err != nil ||
		original != trigger || assigned == nil || *assigned == original {
		t.Fatal("trigger job was mistaken for the job GitHub actually assigned")
	}
	if compute.createCount() != 1 || atomic.LoadInt32(&gh.jitCalls) != 1 {
		t.Fatal("creation or GitHub registration replayed")
	}
	if _, err := s.requestCIRunnerStop(ctx, workspace, claim.Runner, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Stop(ctx, claim.Spec.Plan); err != nil {
		t.Fatal(err)
	}
	driveCleanup(t, engine, claim.Spec.Plan.EnforcerLease, 2)
	worker := &ciRunnerLifecycleWorker{Store: s, Backend: &ciLifecycleProviderBackend{Compute: compute, Enforcer: enforcer}}
	if err := worker.Reconcile(ctx, workspace, claim.Runner); err != nil {
		t.Fatal("integrated cleanup did not verify absence")
	}
	snap, err := s.loadCIRunnerLifecycle(ctx, workspace, claim.Runner)
	if err != nil || snap.Phase != "deleted" || snap.CleanupVerifiedAt == nil || snap.ExecutionStoppedAt == nil {
		t.Fatal("cleanup claimed success without durable stop and absence proofs")
	}
	if err := s.releaseCIProvisioningCapacity(ctx, workspace, claim.Runner); err != nil {
		t.Fatal(err)
	}
	state := ciBudgetPeek(t, s, "credit_integrated", 1000, policy)
	if state.Active != 0 || state.CommittedMicros != 100 || state.AuthorizedMicros != 1000 {
		t.Fatal("verified cleanup lost funding history or failed to release capacity")
	}
	ciBudgetTotalsZero(t, s, workspace)
}
