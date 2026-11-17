package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/pricing"
)

var ciDiagRunnerKeys = []string{
	"schemaVersion", "runnerId", "association", "phase", "createdAt", "updatedAt",
	"deleteRequested", "createAttemptedAt", "createSettled", "setupDeadlineAt",
	"hardDeadlineAt", "executionStartedAt", "executionStoppedAt", "deadlineAt",
	"stopRequestedAt", "stopReason", "billingStopAt", "stoppedVerifiedAt",
	"cleanupVerifiedAt",
}

// ciDiagAttempt inserts an unassigned provisioning attempt for a job using the
// root-authored fixture recipe; no provider resources are created.
func ciDiagAttempt(t *testing.T, s *Store, ctx context.Context, workspace, jobID string, createdAt time.Time) string {
	t.Helper()
	id, err := newID("ci_diag_")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO ci_runners(id,workspace_id,installation_id,size_id,trigger_job_id,phase,document,rate_micros_per_minute,created_at,updated_at) SELECT $3,workspace_id,installation_id,'linux-x64-2',id,'provisioning','{}'::jsonb,2000,$4,$4 FROM ci_jobs WHERE workspace_id=$1 AND id=$2`, workspace, jobID, id, createdAt); err != nil {
		t.Fatal(err)
	}
	return id
}

func ciDiagTriggerJob(t *testing.T, s *Store, ctx context.Context, workspace, runner string) string {
	t.Helper()
	var jobID string
	if err := s.pool.QueryRow(ctx, `SELECT trigger_job_id FROM ci_runners WHERE workspace_id=$1 AND id=$2`, workspace, runner).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func ciDiagAssertNullLifecycle(t *testing.T, d *CIRunnerDiagnostics) {
	t.Helper()
	if d == nil {
		t.Fatal("expected diagnostics, got nil")
	}
	if d.CreateAttemptedAt != nil || d.CreateSettled != nil || d.SetupDeadlineAt != nil ||
		d.HardDeadlineAt != nil || d.ExecutionStartedAt != nil || d.ExecutionStoppedAt != nil ||
		d.DeadlineAt != nil || d.StopRequestedAt != nil || d.StopReason != nil ||
		d.BillingStopAt != nil || d.StoppedVerifiedAt != nil || d.CleanupVerifiedAt != nil {
		t.Fatalf("unrecorded lifecycle evidence must stay null: %#v", d)
	}
	if d.DeleteRequested {
		t.Fatal("deleteRequested must not be fabricated")
	}
}

func TestCIDiagnosticsAbsentRunnerAndScope(t *testing.T) {
	s, h, _, c, _, _, pubID := ciMCPSession(t)
	ctx := context.Background()

	for _, jobID := range []string{pubID, "ci_job_missing"} {
		d, err := s.ciJobRunnerDiagnostics(ctx, c.WorkspaceID, jobID)
		if err != nil || d != nil {
			t.Fatalf("job %s without runner must yield nil diagnostics: %v %#v", jobID, err, d)
		}
	}
	d, err := s.ciJobRunnerDiagnostics(ctx, "ws_missing", pubID)
	if err != nil || d != nil {
		t.Fatalf("foreign workspace must yield nil diagnostics: %v %#v", err, d)
	}

	agent := ciMCPAgent(c, true)
	if _, err := h.ciJobDetailView(ctx, agent, c.WorkspaceID, "ci_job_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job must be not-found through shared view: %v", err)
	}
	if _, err := h.ciJobDetailView(ctx, agent, "ws_other", pubID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign workspace job must be not-found through shared view: %v", err)
	}
}

func TestCIDiagnosticsProvisioningAttempt(t *testing.T) {
	s, workspace, runner, _, _, _ := ciAdmissionJobFixture(t)
	ctx := context.Background()
	plan := lcPlan(s.now())
	lcTrack(t, s, workspace, runner, plan)
	jobID := ciDiagTriggerJob(t, s, ctx, workspace, runner)

	d, err := s.ciJobRunnerDiagnostics(ctx, workspace, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("expected provisioning-attempt diagnostics")
	}
	if d.SchemaVersion != 1 || d.RunnerID != runner || d.Association != "provisioning_attempt" {
		t.Fatalf("unexpected identity: %#v", d)
	}
	if d.Phase != "idle" {
		t.Fatalf("phase: %q", d.Phase)
	}
	if d.SetupDeadlineAt == nil || !d.SetupDeadlineAt.Equal(plan.SetupDeadlineAt) {
		t.Fatalf("setup deadline must match tracked plan: %v want %v", d.SetupDeadlineAt, plan.SetupDeadlineAt)
	}
	if d.HardDeadlineAt == nil || !d.HardDeadlineAt.Equal(plan.HardDeadlineAt) {
		t.Fatalf("hard deadline must match tracked plan: %v want %v", d.HardDeadlineAt, plan.HardDeadlineAt)
	}
	if d.CreateSettled == nil || *d.CreateSettled {
		t.Fatalf("createSettled must be recorded false (known unsettled), not unknown or true: %v", d.CreateSettled)
	}
	if d.CreateAttemptedAt != nil || d.ExecutionStartedAt != nil || d.ExecutionStoppedAt != nil ||
		d.DeadlineAt != nil || d.StopRequestedAt != nil || d.StopReason != nil ||
		d.BillingStopAt != nil || d.StoppedVerifiedAt != nil || d.CleanupVerifiedAt != nil {
		t.Fatalf("unrecorded lifecycle evidence must stay null: %#v", d)
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		t.Fatal("runner timestamps must be recorded")
	}
}

func TestCIDiagnosticsAssignedRunnerAndStopIntent(t *testing.T) {
	s, workspace, runner, _, req, proof := ciAdmissionJobFixture(t)
	ctx := context.Background()
	plan := lcPlan(s.now())
	lcTrack(t, s, workspace, runner, plan)
	jobID := ciDiagTriggerJob(t, s, ctx, workspace, runner)
	receipt, err := s.admitCIRunnerJob(ctx, workspace, runner, req, &proof, 7200)
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.ciJobRunnerDiagnostics(ctx, workspace, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Association != "assigned" || d.RunnerID != runner {
		t.Fatalf("assigned runner must win association: %#v", d)
	}
	if d.Phase != "running" {
		t.Fatalf("phase after admit: %q", d.Phase)
	}
	if d.ExecutionStartedAt == nil || !d.ExecutionStartedAt.Equal(proof.StartedAt.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("execution start must equal admission receipt source start: %v want %v", d.ExecutionStartedAt, proof.StartedAt)
	}
	if d.DeadlineAt == nil || d.DeadlineAt.UnixMilli() != receipt.ExecutionDeadlineUnixMs {
		t.Fatalf("deadline must equal admission receipt deadline: %v want %d", d.DeadlineAt, receipt.ExecutionDeadlineUnixMs)
	}

	ciDiagAttempt(t, s, ctx, workspace, jobID, s.now().Add(time.Hour))
	d, err = s.ciJobRunnerDiagnostics(ctx, workspace, jobID)
	if err != nil || d == nil || d.RunnerID != runner || d.Association != "assigned" {
		t.Fatalf("assigned runner must outrank a newer unassigned attempt: %v %#v", err, d)
	}

	if _, err = s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	d, err = s.ciJobRunnerDiagnostics(ctx, workspace, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Association != "assigned" || !d.DeleteRequested || d.Phase != "deleting" {
		t.Fatalf("stop intent must be recorded without proof: %#v", d)
	}
	if d.StopRequestedAt == nil || d.StopReason == nil || *d.StopReason != "cancelled" || d.BillingStopAt == nil {
		t.Fatalf("stop intent fields: %#v", d)
	}
	if d.StoppedVerifiedAt != nil || d.CleanupVerifiedAt != nil {
		t.Fatalf("stop intent must not fabricate verified timestamps: %#v", d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stoppedVerifiedAt", "cleanupVerifiedAt"} {
		if string(fields[key]) != "null" {
			t.Fatalf("%s must serialize explicit null, got %s", key, fields[key])
		}
	}
	if string(fields["stopReason"]) != `"cancelled"` {
		t.Fatalf("stopReason must serialize recorded reason: %s", fields["stopReason"])
	}

	verified := time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)
	if _, err = s.pool.Exec(ctx, `UPDATE ci_runner_lifecycles SET stopped_verified_at=$3,cleanup_verified_at=$3 WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner, verified); err != nil {
		t.Fatal(err)
	}
	d, err = s.ciJobRunnerDiagnostics(ctx, workspace, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if d.StoppedVerifiedAt == nil || !d.StoppedVerifiedAt.Equal(verified) || d.CleanupVerifiedAt == nil || !d.CleanupVerifiedAt.Equal(verified) {
		t.Fatalf("stored verification timestamps must project: %#v", d)
	}
}

func TestCIDiagnosticsAssociationPrecedence(t *testing.T) {
	s, _, _, c, _, privID, pubID := ciMCPSession(t)
	ctx := context.Background()
	base := time.Date(2026, time.December, 2, 0, 0, 0, 0, time.UTC)

	first := ciDiagAttempt(t, s, ctx, c.WorkspaceID, pubID, base)
	latest := ciDiagAttempt(t, s, ctx, c.WorkspaceID, pubID, base.Add(time.Hour))
	d, err := s.ciJobRunnerDiagnostics(ctx, c.WorkspaceID, pubID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.RunnerID != latest || d.Association != "provisioning_attempt" {
		t.Fatalf("latest unassigned attempt must win: %#v (first=%s)", d, first)
	}

	// A trigger attempt that accepted a different job is not evidence here.
	rerouted := ciDiagAttempt(t, s, ctx, c.WorkspaceID, privID, base.Add(2*time.Hour))
	if _, err = s.pool.Exec(ctx, `UPDATE ci_runners SET assigned_job_id=$3,phase='running' WHERE workspace_id=$1 AND id=$2`, c.WorkspaceID, rerouted, pubID); err != nil {
		t.Fatal(err)
	}
	d, err = s.ciJobRunnerDiagnostics(ctx, c.WorkspaceID, privID)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("attempt assigned to another job must not surface for its trigger job: %#v", d)
	}
	d, err = s.ciJobRunnerDiagnostics(ctx, c.WorkspaceID, pubID)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.RunnerID != latest {
		t.Fatalf("rerouted attempt must not replace or leak into this job: %#v", d)
	}
}

func TestCIDiagnosticsLegacyRunnerWithoutLifecycle(t *testing.T) {
	s, _, _, c, _, _, pubID := ciMCPSession(t)
	ctx := context.Background()
	base := time.Date(2026, time.December, 3, 0, 0, 0, 0, time.UTC)

	runner := ciDiagAttempt(t, s, ctx, c.WorkspaceID, pubID, base)
	d, err := s.ciJobRunnerDiagnostics(ctx, c.WorkspaceID, pubID)
	if err != nil {
		t.Fatal(err)
	}
	ciDiagAssertNullLifecycle(t, d)
	if d.RunnerID != runner || d.Association != "provisioning_attempt" || d.Phase != "provisioning" {
		t.Fatalf("legacy runner record must still surface: %#v", d)
	}
}

func TestCIDiagnosticsRESTAndMCPExposure(t *testing.T) {
	s, h, _, c, token, privID, pubID := ciMCPSession(t)
	ctx := context.Background()
	base := time.Date(2026, time.December, 4, 0, 0, 0, 0, time.UTC)

	ownerReq := func(path string) *httptest.ResponseRecorder {
		req := ciAuthedRequest(t, http.MethodGet, path, token, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := ownerReq("/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/" + pubID)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST public detail before runner: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"runnerDiagnostics":null`) {
		t.Fatalf("missing runner must serialize explicit null: %s", rec.Body.String())
	}

	pubRunner := ciDiagAttempt(t, s, ctx, c.WorkspaceID, pubID, base)
	privRunner := ciDiagAttempt(t, s, ctx, c.WorkspaceID, privID, base.Add(time.Minute))

	agent := ciMCPAgent(c, true)
	value, err := ciMCPCall(t, h, agent, "canter_inspect_ci_job", `{"workspaceId":"`+c.WorkspaceID+`","jobId":"`+pubID+`"}`)
	if err != nil {
		t.Fatalf("agent public detail: %v", err)
	}
	detail, ok := value.(CIJobDetail)
	if !ok || detail.RunnerDiagnostics == nil {
		t.Fatalf("agent public detail must carry runner diagnostics: %#v", value)
	}
	if detail.RunnerDiagnostics.RunnerID != pubRunner || detail.RunnerDiagnostics.Association != "provisioning_attempt" {
		t.Fatalf("unexpected diagnostics: %#v", detail.RunnerDiagnostics)
	}
	raw, err := json.Marshal(detail.RunnerDiagnostics)
	if err != nil {
		t.Fatal(err)
	}
	var diagFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &diagFields); err != nil {
		t.Fatal(err)
	}
	if len(diagFields) != len(ciDiagRunnerKeys) {
		t.Fatalf("diagnostics must expose exactly the recorded DTO fields: %d keys %s", len(diagFields), raw)
	}
	for _, key := range ciDiagRunnerKeys {
		if _, ok := diagFields[key]; !ok {
			t.Fatalf("diagnostics missing recorded field %q: %s", key, raw)
		}
	}
	for _, forbidden := range []string{"plan", "resources", "enforcerLease", "decision", "providerId", "jit", "credentials", "region", "flavorId", "networkId", "imageId", "operationId", "githubRunnerId", "admissionId"} {
		if _, ok := diagFields[forbidden]; ok {
			t.Fatalf("diagnostics must not expose %q: %s", forbidden, raw)
		}
	}

	if _, err = ciMCPCall(t, h, agent, "canter_inspect_ci_job", `{"workspaceId":"`+c.WorkspaceID+`","jobId":"`+privID+`"}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent private detail must stay not-found with diagnostics row present: %v", err)
	}
	human := ciMCPHuman(c)
	value, err = ciMCPCall(t, h, human, "canter_inspect_ci_job", `{"workspaceId":"`+c.WorkspaceID+`","jobId":"`+privID+`"}`)
	if err != nil {
		t.Fatalf("human private detail: %v", err)
	}
	if detail, ok := value.(CIJobDetail); !ok || detail.RunnerDiagnostics == nil || detail.RunnerDiagnostics.RunnerID != privRunner {
		t.Fatalf("human private detail must carry its own diagnostics: %#v", value)
	}

	rec = ownerReq("/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/" + pubID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runnerDiagnostics"`) || !strings.Contains(rec.Body.String(), pubRunner) {
		t.Fatalf("REST public detail must carry runner diagnostics: %d %s", rec.Code, rec.Body.String())
	}
	member, _, memberToken, err := s.Signup(ctx, "diag-member@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO memberships(account_id,workspace_id,role) VALUES($1,$2,'viewer')`, member.ID, c.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	req := ciAuthedRequest(t, http.MethodGet, "/v1/workspaces/"+c.WorkspaceID+"/ci/jobs/"+privID, memberToken, "")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("REST hidden private detail must stay 404 with diagnostics row present: %d %s", rec.Code, rec.Body.String())
	}
}

// The billing contract is additive at job detail level; the established
// runner diagnostics DTO stays the same for REST and agent consumers.
func TestCIDiagnosticsPublishesOnlyAssignedPinnedTariff(t *testing.T) {
	s, h, _, c, token, privID, pubID := ciMCPSession(t)
	ctx := context.Background()
	h.service.Store.SetCIRateV2Enabled(true) // Current catalog is v2 even for the v1 runner.
	for _, test := range []struct {
		jobID, version string
		rate           int64
	}{
		{pubID, pricing.CIRateVersionV1, 2000},
		{privID, pricing.CIRateVersion, 1000},
	} {
		path := "/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/" + test.jobID
		runner := ciDiagAttempt(t, s, ctx, c.WorkspaceID, test.jobID, time.Now())
		read := func() CIJobDetail {
			t.Helper()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, ciAuthedRequest(t, http.MethodGet, path, token, ""))
			requireStatus(t, rec, http.StatusOK)
			var detail CIJobDetail
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			return detail
		}
		if before := read(); before.PinnedRate != nil {
			t.Fatalf("unassigned attempt published a job rate: %+v", before.PinnedRate)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET assigned_job_id=$3,rate_version=$4,rate_micros_per_minute=$5
			WHERE workspace_id=$1 AND id=$2`, c.WorkspaceID, runner, test.jobID, test.version, test.rate); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE ci_jobs SET runner_id=$3 WHERE workspace_id=$1 AND id=$2`, c.WorkspaceID, test.jobID, runner); err != nil {
			t.Fatal(err)
		}
		detail := read()
		if detail.PinnedRate == nil || detail.PinnedRate.Version != test.version || detail.PinnedRate.MicrosPerMinute != test.rate {
			t.Fatalf("job rate: %+v, want %s at %d", detail.PinnedRate, test.version, test.rate)
		}
	}
}
