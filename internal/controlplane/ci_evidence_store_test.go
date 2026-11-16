package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ciEvidenceAdmittedCapability runs a real admission allow, then mints and
// parses an evidence capability bound to the committed proof — the same
// claims the admission response header carries. It returns the verified
// capability and the raw token.
func ciEvidenceAdmittedCapability(t *testing.T, fx *ciAdmissionHTTPFixture, receipt ciAdmissionReceipt) (ciEvidenceCapability, string) {
	t.Helper()
	snap, err := fx.s.loadCIRunnerLifecycle(context.Background(), fx.workspace, fx.runner)
	if err != nil {
		t.Fatal(err)
	}
	var decision ciAdmissionDecision
	if err := ciEnforcerStrictJSON(snap.Decision, &decision); err != nil {
		t.Fatal(err)
	}
	if snap.GitHubInstallationID == nil {
		t.Fatal("admission snapshot missing installation id")
	}
	claims := ciEvidenceCapability{
		Version:              1,
		Workspace:            fx.workspace,
		Runner:               fx.runner,
		AdmissionID:          fx.nonce,
		GitHubInstallationID: *snap.GitHubInstallationID,
		GitHubRepositoryID:   fx.req.GitHubRepositoryID,
		GitHubRunnerID:       fx.req.GitHubRunnerID,
		GitHubJobID:          decision.Proof.JobID,
		DeadlineUnixMs:       receipt.ExecutionDeadlineUnixMs,
	}
	tok, err := mintCIEvidenceCapability(fx.signingKey, claims, fx.s.now())
	if err != nil {
		t.Fatal(err)
	}
	capability, err := parseCIEvidenceCapability(fx.signingKey, tok, fx.s.now())
	if err != nil {
		t.Fatal(err)
	}
	return capability, tok
}

func ciEvidenceBatchID(n int) string {
	return fmt.Sprintf("%064x", n)
}

func ciEvidenceBody(t *testing.T, batchID string, metrics, tests []any) []byte {
	t.Helper()
	if metrics == nil {
		metrics = []any{}
	}
	if tests == nil {
		tests = []any{}
	}
	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "batchId": batchID, "metrics": metrics, "tests": tests,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ciEvidenceMetric(at time.Time, fields map[string]any) map[string]any {
	out := map[string]any{"at": at.UTC().Format(time.RFC3339Nano)}
	if fields == nil {
		// Root policy requires at least one observed numeric per sample.
		fields = map[string]any{"cpuPercent": 1.0}
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func ciEvidenceTest(id string, fields map[string]any) map[string]any {
	out := map[string]any{
		"id": id, "name": "Test " + id[:8], "suite": "unit",
		"status": "passed", "message": "", "durationMs": nil,
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func ciEvidenceUsageRow(t *testing.T, s *Store, workspace, jobID string) (metricCount, testCount, batchCount int, acceptedBytes int64) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT metric_count,test_count,batch_count,accepted_bytes FROM ci_job_evidence_usage WHERE workspace_id=$1 AND job_id=$2`,
		workspace, jobID).Scan(&metricCount, &testCount, &batchCount, &acceptedBytes); err != nil {
		t.Fatal(err)
	}
	return
}

func ciEvidenceRowCounts(t *testing.T, s *Store, workspace, jobID string) (metrics, tests, batches int) {
	t.Helper()
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_job_metrics WHERE workspace_id=$1 AND job_id=$2`, workspace, jobID).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_job_tests WHERE workspace_id=$1 AND job_id=$2`, workspace, jobID).Scan(&tests); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ci_job_evidence_batches WHERE workspace_id=$1 AND job_id=$2`, workspace, jobID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	return
}

// provisioningSnapshot digests every funding row so evidence writes can be
// proven to leave billing untouched.
func provisioningSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	var out string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT coalesce(string_agg(md5(ci_provisioning_budget::text), '' ORDER BY md5(ci_provisioning_budget::text)), '') FROM ci_provisioning_budget`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func runnerClockSnapshot(t *testing.T, s *Store, workspace, runner string) string {
	t.Helper()
	var out string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT coalesce(string_agg(md5(r::text), '' ORDER BY md5(r::text)), '') FROM ci_runners r WHERE workspace_id=$1 AND id=$2`,
		workspace, runner).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCIEvidenceStoreAcceptAndReadback(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)
	body := ciEvidenceBody(t, ciEvidenceBatchID(1),
		[]any{
			ciEvidenceMetric(at, map[string]any{
				"cpuPercent": 12.5, "memoryBytes": int64(4096), "memoryLimitBytes": int64(8192),
				"networkRxBytes": int64(100), "networkTxBytes": int64(200), "oomKills": int64(0),
			}),
			ciEvidenceMetric(at.Add(-time.Second), map[string]any{"oomKills": int64(0)}),
		},
		[]any{
			ciEvidenceTest(strings.Repeat("a", 64), map[string]any{"durationMs": int64(1500), "status": "passed"}),
			ciEvidenceTest(strings.Repeat("b", 64), map[string]any{"durationMs": nil, "status": "failed", "message": "boom"}),
			ciEvidenceTest(strings.Repeat("c", 64), map[string]any{"durationMs": int64(0)}),
		})
	before := provisioningSnapshot(t, fx.s)
	clock := runnerClockSnapshot(t, fx.s, fx.workspace, fx.runner)
	if err := fx.s.acceptCIEvidence(ctx, capability, body); err != nil {
		t.Fatal(err)
	}
	if got := provisioningSnapshot(t, fx.s); got != before {
		t.Fatal("evidence upload changed provisioning budget")
	}
	if got := runnerClockSnapshot(t, fx.s, fx.workspace, fx.runner); got != clock {
		t.Fatal("evidence upload changed runner lifecycle clock")
	}

	metrics, err := fx.s.CIJobMetrics(ctx, fx.workspace, scope.Job)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 2 {
		t.Fatalf("metrics %d want 2", len(metrics))
	}
	// CIJobMetrics returns oldest first.
	sparse, full := metrics[0], metrics[1]
	if full.Source != "guest" || sparse.Source != "guest" {
		t.Fatalf("server must label source guest, got %q %q", full.Source, sparse.Source)
	}
	if full.CPUPercent == nil || *full.CPUPercent != 12.5 {
		t.Fatalf("cpu not preserved: %+v", full.CPUPercent)
	}
	if full.MemoryBytes == nil || *full.MemoryBytes != 4096 || full.OOMKills == nil || *full.OOMKills != 0 {
		t.Fatalf("numeric fields not preserved: %+v", full)
	}
	if sparse.CPUPercent != nil || sparse.MemoryBytes != nil || sparse.MemoryLimitBytes != nil ||
		sparse.NetworkRxBytes != nil || sparse.NetworkTxBytes != nil {
		t.Fatalf("missing fields must stay unknown: %+v", sparse)
	}
	if sparse.OOMKills == nil || *sparse.OOMKills != 0 {
		t.Fatalf("observed zero must be preserved: %+v", sparse.OOMKills)
	}
	tests, err := fx.s.CIJobTests(ctx, fx.workspace, scope.Job)
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) != 3 {
		t.Fatalf("tests %d want 3", len(tests))
	}
	for _, tst := range tests {
		if tst.Source != "workflow" {
			t.Fatalf("server must label source workflow, got %q", tst.Source)
		}
	}
	byID := map[string]CITestResult{}
	for _, tst := range tests {
		byID[tst.ID] = tst
	}
	if d := byID[strings.Repeat("a", 64)].DurationMs; d == nil || *d != 1500 {
		t.Fatalf("duration not preserved: %+v", d)
	}
	if d := byID[strings.Repeat("b", 64)].DurationMs; d != nil {
		t.Fatalf("null duration must stay NULL not 0: %+v", *d)
	}
	if d := byID[strings.Repeat("c", 64)].DurationMs; d == nil || *d != 0 {
		t.Fatalf("observed zero duration must be preserved: %+v", d)
	}
	mc, tc, bc, ab := ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
	if mc != 2 || tc != 3 || bc != 1 || ab != int64(len(body)) {
		t.Fatalf("usage counters %d/%d/%d/%d want 2/3/1/%d", mc, tc, bc, ab, len(body))
	}
	if m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 2 || tt != 3 || b != 1 {
		t.Fatalf("rows %d/%d/%d want 2/3/1", m, tt, b)
	}
}

func TestCIEvidenceStoreIdempotentAndNewBatch(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)
	metrics := []any{ciEvidenceMetric(at, map[string]any{"cpuPercent": 5.0})}
	tests := []any{ciEvidenceTest(strings.Repeat("d", 64), nil)}
	body1 := ciEvidenceBody(t, ciEvidenceBatchID(10), metrics, tests)
	if err := fx.s.acceptCIEvidence(ctx, capability, body1); err != nil {
		t.Fatal(err)
	}
	// Exact retry: nil, counters and rows untouched.
	if err := fx.s.acceptCIEvidence(ctx, capability, body1); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	mc, tc, bc, ab := ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
	if mc != 1 || tc != 1 || bc != 1 || ab != int64(len(body1)) {
		t.Fatalf("retry changed counters: %d/%d/%d/%d", mc, tc, bc, ab)
	}
	// A new batch carrying identical documents dedupes the observations but
	// still counts the batch and its bytes.
	body2 := ciEvidenceBody(t, ciEvidenceBatchID(11), metrics, tests)
	if err := fx.s.acceptCIEvidence(ctx, capability, body2); err != nil {
		t.Fatal(err)
	}
	mc, tc, bc, ab = ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
	if mc != 1 || tc != 1 || bc != 2 || ab != int64(len(body1)+len(body2)) {
		t.Fatalf("new-batch counters: %d/%d/%d/%d", mc, tc, bc, ab)
	}
	if m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 1 || tt != 1 || b != 2 {
		t.Fatalf("rows after dedupe batch: %d/%d/%d", m, tt, b)
	}
	// Same batch id with different bytes is a conflict, not an overwrite.
	body3 := ciEvidenceBody(t, ciEvidenceBatchID(10),
		[]any{ciEvidenceMetric(at.Add(time.Second), map[string]any{"cpuPercent": 9.0})}, tests)
	if err := fx.s.acceptCIEvidence(ctx, capability, body3); !errors.Is(err, ErrConflict) {
		t.Fatalf("batch-id hash mismatch must conflict, got %v", err)
	}
}

func TestCIEvidenceStoreQuotaRollback(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)

	// An accepted batch keeps its idempotent retry working even after the
	// quota is exhausted.
	seedBody := ciEvidenceBody(t, ciEvidenceBatchID(20),
		[]any{ciEvidenceMetric(at, nil)}, []any{ciEvidenceTest(strings.Repeat("e", 64), nil)})
	if err := fx.s.acceptCIEvidence(ctx, capability, seedBody); err != nil {
		t.Fatal(err)
	}
	setUsage := func(metric, test, batch int, bytes int64) {
		t.Helper()
		if _, err := fx.s.pool.Exec(ctx,
			`UPDATE ci_job_evidence_usage SET metric_count=$3,test_count=$4,batch_count=$5,accepted_bytes=$6 WHERE workspace_id=$1 AND job_id=$2`,
			fx.workspace, scope.Job, metric, test, batch, bytes); err != nil {
			t.Fatal(err)
		}
	}
	quotaCases := []struct {
		name   string
		metric int
		test   int
		batch  int
		bytes  int64
		body   []byte
	}{
		{"metric cap", 4096, 1, 2, int64(len(seedBody) * 2), ciEvidenceBody(t, ciEvidenceBatchID(21),
			[]any{ciEvidenceMetric(at.Add(time.Second), map[string]any{"cpuPercent": 1.0})}, nil)},
		{"test cap", 1, 1000, 2, int64(len(seedBody) * 2), ciEvidenceBody(t, ciEvidenceBatchID(22), nil,
			[]any{ciEvidenceTest(strings.Repeat("f", 64), nil)})},
		{"batch cap", 1, 1, 4096, int64(len(seedBody) * 2), ciEvidenceBody(t, ciEvidenceBatchID(23),
			[]any{ciEvidenceMetric(at.Add(2*time.Second), nil)}, nil)},
		{"byte cap", 1, 1, 2, 8388608 - 1, ciEvidenceBody(t, ciEvidenceBatchID(24),
			[]any{ciEvidenceMetric(at.Add(3*time.Second), nil)}, nil)},
	}
	for _, tc := range quotaCases {
		t.Run(tc.name, func(t *testing.T) {
			// Baseline: seed batch rows present.
			beforeM, beforeT, beforeB := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job)
			setUsage(tc.metric, tc.test, tc.batch, tc.bytes)
			defer setUsage(1, 1, 1, int64(len(seedBody)))
			err := fx.s.acceptCIEvidence(ctx, capability, tc.body)
			if !errors.Is(err, errCIEvidenceQuota) {
				t.Fatalf("%s: want quota error, got %v", tc.name, err)
			}
			m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job)
			if m != beforeM || tt != beforeT || b != beforeB {
				t.Fatalf("%s: quota failure wrote rows %d/%d/%d want %d/%d/%d", tc.name, m, tt, b, beforeM, beforeT, beforeB)
			}
			// Usage counters roll back with the batch.
			mc, tc2, bc, ab := ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
			if mc != tc.metric || tc2 != tc.test || bc != tc.batch || ab != tc.bytes {
				t.Fatalf("%s: usage mutated on quota failure: %d/%d/%d/%d", tc.name, mc, tc2, bc, ab)
			}
		})
	}
	// Restore sane counters, then prove the accepted batch still retries
	// cleanly at the batch cap.
	setUsage(1, 1, 4096, int64(len(seedBody)*2))
	if err := fx.s.acceptCIEvidence(ctx, capability, seedBody); err != nil {
		t.Fatalf("idempotent retry must survive full quota: %v", err)
	}
	_, _, bc, _ := ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
	if bc != 4096 {
		t.Fatalf("idempotent retry changed batch count to %d", bc)
	}
}

func TestCIEvidenceStoreConflicts(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)

	// Same sampled_at with a different document conflicts and rolls back the
	// rest of the batch atomically.
	first := ciEvidenceBody(t, ciEvidenceBatchID(30),
		[]any{ciEvidenceMetric(at, map[string]any{"cpuPercent": 1.0})}, nil)
	if err := fx.s.acceptCIEvidence(ctx, capability, first); err != nil {
		t.Fatal(err)
	}
	conflict := ciEvidenceBody(t, ciEvidenceBatchID(31),
		[]any{
			ciEvidenceMetric(at, map[string]any{"cpuPercent": 2.0}),
			ciEvidenceMetric(at.Add(time.Second), map[string]any{"cpuPercent": 3.0}),
		}, nil)
	if err := fx.s.acceptCIEvidence(ctx, capability, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("document mismatch must conflict, got %v", err)
	}
	if m, _, _ := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 1 {
		t.Fatalf("conflict batch partially wrote %d metrics", m)
	}
	// Same test id with a different document conflicts identically.
	testOK := ciEvidenceBody(t, ciEvidenceBatchID(32), nil,
		[]any{ciEvidenceTest(strings.Repeat("1", 64), map[string]any{"status": "passed"})})
	if err := fx.s.acceptCIEvidence(ctx, capability, testOK); err != nil {
		t.Fatal(err)
	}
	testBad := ciEvidenceBody(t, ciEvidenceBatchID(33), nil,
		[]any{ciEvidenceTest(strings.Repeat("1", 64), map[string]any{"status": "failed", "message": "changed"})})
	if err := fx.s.acceptCIEvidence(ctx, capability, testBad); !errors.Is(err, ErrConflict) {
		t.Fatalf("test document mismatch must conflict, got %v", err)
	}
	// An in-batch duplicate timestamp with different documents conflicts and
	// leaves nothing behind.
	dup := ciEvidenceBody(t, ciEvidenceBatchID(34),
		[]any{
			ciEvidenceMetric(at.Add(2*time.Second), map[string]any{"cpuPercent": 4.0}),
			ciEvidenceMetric(at.Add(2*time.Second), map[string]any{"cpuPercent": 5.0}),
		}, nil)
	if err := fx.s.acceptCIEvidence(ctx, capability, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("in-batch document conflict must conflict, got %v", err)
	}
	// An in-batch exact duplicate dedupes to a single record.
	dupSame := ciEvidenceBody(t, ciEvidenceBatchID(35),
		[]any{
			ciEvidenceMetric(at.Add(3*time.Second), map[string]any{"cpuPercent": 6.0}),
			ciEvidenceMetric(at.Add(3*time.Second), map[string]any{"cpuPercent": 6.0}),
		}, nil)
	if err := fx.s.acceptCIEvidence(ctx, capability, dupSame); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 2 {
		t.Fatalf("metrics after dedupe cases: %d want 2", m)
	}
	mc, tc, _, _ := ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
	// Batches 30,32,35 accepted (31,33,34 rolled back): 2 metrics + 1 test.
	if mc != 2 || tc != 1 {
		t.Fatalf("usage counts metric=%d test=%d want 2/1", mc, tc)
	}
}

func TestCIEvidenceStoreRejectsInvalidBodies(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	at := fx.s.now().UTC().Format(time.RFC3339Nano)
	goodMetric := `{"at":"` + at + `","cpuPercent":1.0}`
	goodTest := `{"id":"` + strings.Repeat("a", 64) + `","name":"t","suite":"s","status":"passed","message":""}`
	batch := func(metrics, tests string) string {
		return `{"schemaVersion":1,"batchId":"` + ciEvidenceBatchID(40) + `","metrics":` + metrics + `,"tests":` + tests + `}`
	}
	cases := map[string]string{
		"not json":          `x`,
		"empty":             ``,
		"null root":         `null`,
		"array root":        `[]`,
		"missing fields":    `{"schemaVersion":1}`,
		"wrong schema":      strings.Replace(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`), `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"schema string":     strings.Replace(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`), `"schemaVersion":1`, `"schemaVersion":"1"`, 1),
		"bad batch id":      strings.Replace(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`), ciEvidenceBatchID(40), "abc", 1),
		"upper batch id":    strings.Replace(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`), ciEvidenceBatchID(40), strings.ToUpper(strings.Repeat("a", 64)), 1),
		"null metrics":      batch(`null`, `[`+goodTest+`]`),
		"null tests":        batch(`[`+goodMetric+`]`, `null`),
		"metrics object":    batch(`{}`, `[`+goodTest+`]`),
		"empty batch":       batch(`[]`, `[]`),
		"metric no at":      batch(`[{"cpuPercent":1.0}]`, `[`+goodTest+`]`),
		"metric null at":    batch(`[{"at":null}]`, `[`+goodTest+`]`),
		"metric bad at":     batch(`[{"at":"soon"}]`, `[`+goodTest+`]`),
		"metric unknown":    batch(`[{"at":"`+at+`","bogus":1}]`, `[`+goodTest+`]`),
		"metric source":     batch(`[{"at":"`+at+`","source":"guest"}]`, `[`+goodTest+`]`),
		"metric dup key":    batch(`[{"at":"`+at+`","at":"`+at+`"}]`, `[`+goodTest+`]`),
		"metric case alias": batch(`[{"At":"`+at+`"}]`, `[`+goodTest+`]`),
		"metric bad num":    batch(`[{"at":"`+at+`","cpuPercent":"high"}]`, `[`+goodTest+`]`),
		"metric float int":  batch(`[{"at":"`+at+`","memoryBytes":1.5}]`, `[`+goodTest+`]`),
		"test missing id":   batch(`[`+goodMetric+`]`, `[{"name":"t","suite":"s","status":"passed","message":""}]`),
		"test missing msg":  batch(`[`+goodMetric+`]`, `[{"id":"`+strings.Repeat("b", 64)+`","name":"t","suite":"s","status":"passed"}]`),
		"test bad dur":      batch(`[`+goodMetric+`]`, `[{"id":"`+strings.Repeat("b", 64)+`","name":"t","suite":"s","status":"passed","message":"","durationMs":"fast"}]`),
		"test float dur":    batch(`[`+goodMetric+`]`, `[{"id":"`+strings.Repeat("b", 64)+`","name":"t","suite":"s","status":"passed","message":"","durationMs":1.5}]`),
		"test source":       batch(`[`+goodMetric+`]`, `[{"id":"`+strings.Repeat("b", 64)+`","name":"t","suite":"s","status":"passed","message":"","source":"workflow"}]`),
		"top extra key":     batch(`[`+goodMetric+`]`, `[`+goodTest+`]`)[:len(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`))-1] + `,"extra":1}`,
		"top case alias":    strings.Replace(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`), `"schemaVersion"`, `"SchemaVersion"`, 1),
		"trailing":          batch(`[`+goodMetric+`]`, `[`+goodTest+`]`) + ` {}`,
		"trailing garbage":  batch(`[`+goodMetric+`]`, `[`+goodTest+`]`) + `xyz`,
		"dup top key":       strings.Replace(batch(`[`+goodMetric+`]`, `[`+goodTest+`]`), `}`, `,"batchId":"`+ciEvidenceBatchID(41)+`"}`, 1),
		"invalid utf8":      "{\"schemaVersion\":1,\"batchId\":\"" + ciEvidenceBatchID(40) + "\",\"metrics\":[],\"tests\":[],\"x\":\"\xff\"}",
		"oversize":          strings.Repeat(" ", ciEvidenceRequestMaxBytes+1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			err := fx.s.acceptCIEvidence(ctx, capability, []byte(raw))
			if !errors.Is(err, errCIEvidenceInvalid) {
				t.Fatalf("want invalid-evidence error, got %v", err)
			}
		})
	}
	// NUL bytes inside text fields are rejected, never normalized.
	nulBody := ciEvidenceBody(t, ciEvidenceBatchID(42), nil,
		[]any{ciEvidenceTest(strings.Repeat("9", 64), map[string]any{"message": "a\x00b"})})
	if err := fx.s.acceptCIEvidence(ctx, capability, nulBody); !errors.Is(err, errCIEvidenceInvalid) {
		t.Fatalf("NUL in message must be invalid, got %v", err)
	}
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	oldAt := scope.StartedAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	stale := `{"schemaVersion":1,"batchId":"` + ciEvidenceBatchID(43) + `","metrics":[{"at":"` + oldAt + `","cpuPercent":1.0}],"tests":[]}`
	if err := fx.s.acceptCIEvidence(ctx, capability, []byte(stale)); !errors.Is(err, errCIEvidenceInvalid) {
		t.Fatalf("pre-window timestamp must be invalid, got %v", err)
	}
}

func TestCIEvidenceStoreScopeAndWindow(t *testing.T) {
	// An unverified capability is unauthorized before any database work: a
	// canceled context would surface a pool error if a query were attempted.
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	capability.verified = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fx.s.loadCIEvidenceScope(ctx, capability); !errors.Is(err, errCIEvidenceUnauthorized) {
		t.Fatalf("unverified scope load: %v", err)
	}
	if err := fx.s.acceptCIEvidence(ctx, capability, []byte(`{}`)); !errors.Is(err, errCIEvidenceUnauthorized) {
		t.Fatalf("unverified accept: %v", err)
	}

	driftCases := []struct {
		name   string
		mutate func(t *testing.T, fx *ciAdmissionHTTPFixture)
	}{
		{"github run id changed", func(t *testing.T, fx *ciAdmissionHTTPFixture) {
			if _, err := fx.s.pool.Exec(context.Background(), `UPDATE ci_jobs SET github_run_id=999999 WHERE workspace_id=$1`, fx.workspace); err != nil {
				t.Fatal(err)
			}
		}},
		{"repository rebound", func(t *testing.T, fx *ciAdmissionHTTPFixture) {
			ctx := context.Background()
			if _, err := fx.s.pool.Exec(ctx, `INSERT INTO ci_repositories(workspace_id,installation_id,github_repository_id,full_name,enabled) VALUES($1,'ci_financial_installation',790,'financial-tests/repo-two',true)`, fx.workspace); err != nil {
				t.Fatal(err)
			}
			if _, err := fx.s.pool.Exec(ctx, `UPDATE ci_jobs SET github_repository_id=790 WHERE workspace_id=$1`, fx.workspace); err != nil {
				t.Fatal(err)
			}
		}},
		{"installation id changed", func(t *testing.T, fx *ciAdmissionHTTPFixture) {
			if _, err := fx.s.pool.Exec(context.Background(), `UPDATE ci_github_installations SET github_installation_id=124 WHERE workspace_id=$1`, fx.workspace); err != nil {
				t.Fatal(err)
			}
		}},
		{"ci product removed", func(t *testing.T, fx *ciAdmissionHTTPFixture) {
			if _, err := fx.s.pool.Exec(context.Background(), `DELETE FROM workspace_products WHERE workspace_id=$1 AND product='ci'`, fx.workspace); err != nil {
				t.Fatal(err)
			}
		}},
		{"runner reassigned", func(t *testing.T, fx *ciAdmissionHTTPFixture) {
			if _, err := fx.s.pool.Exec(context.Background(), `UPDATE ci_runners SET assigned_job_id=NULL WHERE workspace_id=$1 AND id=$2`, fx.workspace, fx.runner); err != nil {
				t.Fatal(err)
			}
		}},
		{"admission decision changed", func(t *testing.T, fx *ciAdmissionHTTPFixture) {
			if _, err := fx.s.pool.Exec(context.Background(), `UPDATE ci_runner_admissions SET decision='{}'::jsonb WHERE workspace_id=$1 AND runner_id=$2`, fx.workspace, fx.runner); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range driftCases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCIAdmissionHTTPFixture(t, "")
			receipt := fx.allow(t, 3600)
			capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
			ctx := context.Background()
			scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
			if err != nil {
				t.Fatal(err)
			}
			at := fx.s.now().UTC().Truncate(time.Microsecond)
			body := ciEvidenceBody(t, ciEvidenceBatchID(50),
				[]any{ciEvidenceMetric(at, nil)}, []any{ciEvidenceTest(strings.Repeat("7", 64), nil)})
			tc.mutate(t, fx)
			if err := fx.s.acceptCIEvidence(ctx, capability, body); err == nil {
				t.Fatalf("%s: drifted scope must deny", tc.name)
			}
			if m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 0 || tt != 0 || b != 0 {
				t.Fatalf("%s: drift wrote rows %d/%d/%d", tc.name, m, tt, b)
			}
			if _, err := fx.s.loadCIEvidenceScope(ctx, capability); err == nil {
				t.Fatalf("%s: drifted scope must fail auth", tc.name)
			}
		})
	}
}

func TestCIEvidenceStoreWindowBoundaries(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	if _, err := fx.s.loadCIEvidenceScope(ctx, capability); err != nil {
		t.Fatal(err)
	}

	// After execution stops, the final test report still lands inside the
	// upload grace, but metrics sampled after the stop cutoff are rejected.
	stoppedAt := fx.s.now().UTC().Truncate(time.Microsecond)
	if _, err := fx.s.pool.Exec(ctx,
		`UPDATE ci_runners SET execution_stopped_at=$3 WHERE workspace_id=$1 AND id=$2`,
		fx.workspace, fx.runner, stoppedAt); err != nil {
		t.Fatal(err)
	}
	lateMetric := ciEvidenceBody(t, ciEvidenceBatchID(60),
		[]any{ciEvidenceMetric(stoppedAt.Add(time.Second), nil)}, nil)
	if err := fx.s.acceptCIEvidence(ctx, capability, lateMetric); !errors.Is(err, errCIEvidenceInvalid) {
		t.Fatalf("post-stop metric must be invalid, got %v", err)
	}
	finalTests := ciEvidenceBody(t, ciEvidenceBatchID(61), nil,
		[]any{ciEvidenceTest(strings.Repeat("8", 64), map[string]any{"status": "passed"})})
	if err := fx.s.acceptCIEvidence(ctx, capability, finalTests); err != nil {
		t.Fatalf("final report inside grace must land: %v", err)
	}

	// Grace expiry is exact: deadline+5m rejects, one ms before accepts on a
	// fresh fixture state is covered by the token tests; here the store clock
	// is moved past the boundary.
	oldNow := fx.s.now
	fx.s.now = func() time.Time {
		return time.UnixMilli(receipt.ExecutionDeadlineUnixMs).Add(ciEvidenceUploadGrace)
	}
	defer func() { fx.s.now = oldNow }()
	expired := ciEvidenceBody(t, ciEvidenceBatchID(62),
		[]any{ciEvidenceMetric(stoppedAt, nil)}, nil)
	if err := fx.s.acceptCIEvidence(ctx, capability, expired); !errors.Is(err, errCIEvidenceUnauthorized) {
		t.Fatalf("expired window must be unauthorized, got %v", err)
	}
}

func TestCIEvidenceStoreFinalWindowRecheck(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)
	body := ciEvidenceBody(t, ciEvidenceBatchID(70),
		[]any{ciEvidenceMetric(at, nil)}, []any{ciEvidenceTest(strings.Repeat("6", 64), nil)})
	// The clock crosses the upload grace between the first window check and
	// the pre-commit recheck: every write must roll back.
	oldNow := fx.s.now
	calls := 0
	fx.s.now = func() time.Time {
		calls++
		if calls == 1 {
			return oldNow()
		}
		return time.UnixMilli(receipt.ExecutionDeadlineUnixMs).Add(ciEvidenceUploadGrace)
	}
	defer func() { fx.s.now = oldNow }()
	if err := fx.s.acceptCIEvidence(ctx, capability, body); !errors.Is(err, errCIEvidenceUnauthorized) {
		t.Fatalf("mid-write grace crossing must deny, got %v", err)
	}
	if m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 0 || tt != 0 || b != 0 {
		t.Fatalf("crossed grace left rows %d/%d/%d", m, tt, b)
	}
}

func TestCIEvidenceStoreConcurrentDuplicate(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)
	body := ciEvidenceBody(t, ciEvidenceBatchID(80),
		[]any{ciEvidenceMetric(at, map[string]any{"cpuPercent": 7.5})},
		[]any{ciEvidenceTest(strings.Repeat("5", 64), nil)})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fx.s.acceptCIEvidence(ctx, capability, body)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent duplicate %d: %v", i, err)
		}
	}
	if m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 1 || tt != 1 || b != 1 {
		t.Fatalf("concurrent duplicate doubled state: %d/%d/%d", m, tt, b)
	}
	mc, tc, bc, _ := ciEvidenceUsageRow(t, fx.s, fx.workspace, scope.Job)
	if mc != 1 || tc != 1 || bc != 1 {
		t.Fatalf("concurrent duplicate doubled counters: %d/%d/%d", mc, tc, bc)
	}
}

func TestCIEvidenceStoreCascadeAndNoResurrection(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	at := fx.s.now().UTC().Truncate(time.Microsecond)
	body := ciEvidenceBody(t, ciEvidenceBatchID(90),
		[]any{ciEvidenceMetric(at, nil)}, []any{ciEvidenceTest(strings.Repeat("4", 64), nil)})
	if err := fx.s.acceptCIEvidence(ctx, capability, body); err != nil {
		t.Fatal(err)
	}
	// Break the job↔runner FK cycle, delete the job: usage, batches, metrics,
	// and tests must all cascade away.
	if _, err := fx.s.pool.Exec(ctx, `UPDATE ci_jobs SET runner_id=NULL WHERE workspace_id=$1 AND id=$2`, fx.workspace, scope.Job); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.pool.Exec(ctx, `DELETE FROM ci_runners WHERE workspace_id=$1 AND id=$2`, fx.workspace, fx.runner); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.pool.Exec(ctx, `DELETE FROM ci_jobs WHERE workspace_id=$1 AND id=$2`, fx.workspace, scope.Job); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ci_job_metrics", "ci_job_tests", "ci_job_evidence_usage", "ci_job_evidence_batches"} {
		var n int
		if err := fx.s.pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE workspace_id=$1 AND job_id=$2`, table),
			fx.workspace, scope.Job).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s kept %d rows after job delete", table, n)
		}
	}
	// The stale token cannot resurrect evidence for a deleted job.
	if err := fx.s.acceptCIEvidence(ctx, capability, body); !errors.Is(err, errCIEvidenceUnauthorized) {
		t.Fatalf("stale token after delete: %v", err)
	}
	if _, err := fx.s.loadCIEvidenceScope(ctx, capability); !errors.Is(err, errCIEvidenceUnauthorized) {
		t.Fatalf("stale scope after delete: %v", err)
	}
}

func TestCIEvidenceStoreMaxConnsOne(t *testing.T) {
	fx := newCIAdmissionHTTPFixture(t, "")
	receipt := fx.allow(t, 3600)
	capability, _ := ciEvidenceAdmittedCapability(t, fx, receipt)
	ctx := context.Background()
	scope, err := fx.s.loadCIEvidenceScope(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	// A single-connection pool must not deadlock: the whole accept runs in
	// one transaction.
	cfg, err := pgxpool.ParseConfig(os.Getenv("CANTER_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := fx.s.pool
	fx.s.pool = pool
	t.Cleanup(func() {
		fx.s.pool = old
		pool.Close()
	})
	at := fx.s.now().UTC().Truncate(time.Microsecond)
	body := ciEvidenceBody(t, ciEvidenceBatchID(95),
		[]any{ciEvidenceMetric(at, nil)}, []any{ciEvidenceTest(strings.Repeat("3", 64), nil)})
	if err := fx.s.acceptCIEvidence(ctx, capability, body); err != nil {
		t.Fatal(err)
	}
	if m, tt, b := ciEvidenceRowCounts(t, fx.s, fx.workspace, scope.Job); m != 1 || tt != 1 || b != 1 {
		t.Fatalf("MaxConns=1 rows %d/%d/%d", m, tt, b)
	}
}
