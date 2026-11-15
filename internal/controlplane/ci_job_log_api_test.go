package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCIJobLogDetailExposure proves the shared REST/MCP detail view carries
// the collection state only after the existing repository-visibility gate,
// and that legacy jobs without a collection row omit the field entirely.
func TestCIJobLogDetailExposure(t *testing.T) {
	s, h, _, c, token, privID, pubID := ciMCPSession(t)
	ctx := context.Background()

	ownerReq := func(path string) *httptest.ResponseRecorder {
		req := ciAuthedRequest(t, http.MethodGet, path, token, "")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	detailPath := "/v1/workspaces/" + c.WorkspaceID + "/ci/jobs/"

	// Legacy jobs have no collection row: the field is omitted, not fabricated.
	rec := ownerReq(detailPath + pubID)
	if rec.Code != http.StatusOK {
		t.Fatalf("public detail: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "logCollection") {
		t.Fatalf("legacy job must omit logCollection: %s", rec.Body.String())
	}

	// Seed complete collection rows for both the public and private jobs.
	for _, jobID := range []string{pubID, privID} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO ci_job_log_collections(workspace_id,job_id,state,owner_nonce,lease_until,next_attempt_at,truncated,collected_at) VALUES($1,$2,'complete','',now(),now(),false,now())`, c.WorkspaceID, jobID); err != nil {
			t.Fatal(err)
		}
	}

	rec = ownerReq(detailPath + pubID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"logCollection"`) ||
		!strings.Contains(rec.Body.String(), `"state":"complete"`) {
		t.Fatalf("public detail must carry collection state: %d %s", rec.Code, rec.Body.String())
	}
	rec = ownerReq(detailPath + privID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"complete"`) {
		t.Fatalf("owner private detail must carry collection state: %d %s", rec.Code, rec.Body.String())
	}

	agent := ciMCPAgent(c, true)
	value, err := ciMCPCall(t, h, agent, "canter_inspect_ci_job", `{"workspaceId":"`+c.WorkspaceID+`","jobId":"`+pubID+`"}`)
	if err != nil {
		t.Fatalf("agent public detail: %v", err)
	}
	detail, ok := value.(CIJobDetail)
	if !ok || detail.LogCollection == nil || detail.LogCollection.State != "complete" || detail.LogCollection.CollectedAt == nil {
		t.Fatalf("agent public detail must carry the same collection field: %#v", value)
	}
	// The private job's collection row exists but visibility still wins: the
	// agent gets the same privacy-preserving not-found as before.
	if _, err = ciMCPCall(t, h, agent, "canter_inspect_ci_job", `{"workspaceId":"`+c.WorkspaceID+`","jobId":"`+privID+`"}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent private detail must stay not-found with collection row present: %v", err)
	}
	human := ciMCPHuman(c)
	value, err = ciMCPCall(t, h, human, "canter_inspect_ci_job", `{"workspaceId":"`+c.WorkspaceID+`","jobId":"`+privID+`"}`)
	if err != nil {
		t.Fatalf("human private detail: %v", err)
	}
	if detail, ok := value.(CIJobDetail); !ok || detail.LogCollection == nil || detail.LogCollection.State != "complete" {
		t.Fatalf("human private detail must carry its collection state: %#v", value)
	}
}

// TestCIJobLogRuntimeWiring proves collector availability follows the
// runtime mode and GitHub configuration, and that Run joins a stalled
// download together with the primary workers under shared cancellation.
func TestCIJobLogRuntimeWiring(t *testing.T) {
	fx := newCIRuntimeFixture(t, 0, 0, false, nil)

	// Off mode constructs nothing.
	if rt, err := newCIManagedRuntime(fx.ctx, fx.s, fx.compute, fx.ciConfig, CIManagedRuntimeConfig{}, true); err != nil || rt != nil {
		t.Fatalf("off mode: rt=%v err=%v", rt, err)
	}

	// Recover without GitHub stays lifecycle-only with zero collector I/O.
	rt := fx.newRuntime(t, fx.runtimeConfig(CIManagedRecover), CIConfig{})
	if rt.logs != nil {
		t.Fatal("recover without GitHub must not install a collector")
	}
	_, cancel, done := ciRuntimeStartRun(t, rt)
	if rt.logsAvailable() {
		t.Fatal("logsAvailable must be false without a collector")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("recover Run: %v", err)
	}
	if rt.logsAvailable() {
		t.Fatal("logsAvailable must be false after Run exits")
	}

	// Recover with GitHub installs the collector; it reports available only
	// while the runtime is actually running.
	rtGH := fx.newRuntime(t, fx.runtimeConfig(CIManagedRecover), fx.ciConfig)
	if rtGH.logs == nil {
		t.Fatal("recover with GitHub must install a collector")
	}
	if rtGH.logsAvailable() {
		t.Fatal("logsAvailable must be false before Run")
	}
	_, cancel2, done2 := ciRuntimeStartRun(t, rtGH)
	deadline := time.Now().Add(10 * time.Second)
	for !rtGH.logsAvailable() {
		if time.Now().After(deadline) {
			t.Fatal("logsAvailable did not become true while running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	if err := <-done2; !errors.Is(err, context.Canceled) {
		t.Fatalf("recover+github Run: %v", err)
	}

	// Swap in a collector whose download stalls forever; a second Run must
	// cancel and join it together with the lifecycle worker.
	entered := make(chan struct{})
	var once sync.Once
	rtGH.logs = &ciJobLogCollector{
		store: fx.s,
		download: func(ctx context.Context, scope ciJobLogScope) (string, bool, error) {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return "", false, ctx.Err()
		},
	}
	ciJobLogSeed(t, fx.s, fx.workspace, 1, "completed", true, true, fx.s.now())
	_, cancel3, done3 := ciRuntimeStartRun(t, rtGH)
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("collector download was not entered")
	}
	start := time.Now()
	cancel3()
	select {
	case <-done3:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not join the blocked download and primary workers")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("join took too long: %v", elapsed)
	}

	// Run mode always installs a collector from its validated GitHub client.
	rtRun := fx.newRuntime(t, fx.runtimeConfig(CIManagedRun), fx.ciConfig)
	if rtRun.logs == nil {
		t.Fatal("run mode must install a collector")
	}
	_, cancel4, done4 := ciRuntimeStartRun(t, rtRun)
	deadline = time.Now().Add(10 * time.Second)
	for !rtRun.logsAvailable() {
		if time.Now().After(deadline) {
			t.Fatal("run-mode logsAvailable did not become true")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel4()
	if err := <-done4; !errors.Is(err, context.Canceled) {
		t.Fatalf("run Run: %v", err)
	}
	if n := fx.compute.createCount(); n != 0 {
		t.Fatalf("log collection must never create provider resources, got %d", n)
	}
}
