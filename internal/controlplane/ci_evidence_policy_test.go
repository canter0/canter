package controlplane

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func ciEvidencePolicyFixture(t *testing.T) (ciEvidenceScope, ciEvidenceCapability, time.Time) {
	t.Helper()
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	s := ciEvidenceScope{
		Workspace: "ws_evidence", Job: "job_evidence", Runner: "runner_evidence",
		SizeID: "linux-x64-2", GitHubInstallationID: 123, GitHubRepositoryID: 789,
		GitHubJobID: 555, GitHubRunID: 777, AdmissionID: strings.Repeat("a", 64),
		RunnerName: "canter_evidence", AdmissionRepositoryID: 789,
		CreatedAt: now.Add(-2 * time.Minute), StartedAt: now.Add(-time.Minute),
		DeadlineAt: now.Add(time.Hour), HardDeadlineAt: now.Add(2 * time.Hour),
	}
	c := ciEvidenceCapability{
		verified: true, Version: 1, Workspace: s.Workspace, Runner: s.Runner,
		AdmissionID: s.AdmissionID, GitHubInstallationID: 123, GitHubRepositoryID: 789,
		GitHubRunnerID: 42, GitHubJobID: 555, DeadlineUnixMs: s.DeadlineAt.UnixMilli(),
	}
	request := ciAdmissionRequest{
		SchemaVersion: 1, AdmissionID: s.AdmissionID, GitHubRepositoryID: 789,
		GitHubRunnerID: 42, RunnerName: s.RunnerName, Repository: "owner/repo",
		GitHubRunID: 777, RunAttempt: 1, JobKey: "test",
	}
	decision := ciAdmissionDecision{
		Request: request,
		Proof: ciRunnerJobProof{
			JobID: 555, RepositoryID: 789, RunnerID: 42, RunnerName: s.RunnerName,
			Repository: "owner/repo", RunID: 777, Attempt: 1, StartedAt: s.StartedAt,
		},
		Receipt: ciAdmissionReceipt{
			ciAdmissionRequest: request, Decision: "allow",
			ExecutionDeadlineUnixMs: s.DeadlineAt.UnixMilli(),
		},
	}
	var err error
	s.Decision, err = json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, now
}

func TestCIEvidencePolicyWindow(t *testing.T) {
	s, c, now := ciEvidencePolicyFixture(t)
	cutoff, err := s.window(c, now)
	if err != nil || !cutoff.Equal(s.DeadlineAt) {
		t.Fatalf("valid admission: cutoff=%v err=%v", cutoff, err)
	}
	unverified := c
	unverified.verified = false
	if _, err := s.window(unverified, now); err == nil {
		t.Fatal("unverified claims must never authorize writes")
	}
	wrongJob := c
	wrongJob.GitHubJobID++
	if _, err := s.window(wrongJob, now); err == nil {
		t.Fatal("claims must match the exact admitted GitHub job")
	}
	wrongInstallation := c
	wrongInstallation.GitHubInstallationID++
	if _, err := s.window(wrongInstallation, now); err == nil {
		t.Fatal("installation rebinding must reject the old capability")
	}
	retired := s
	retired.GitHubRunnerID = nil
	billStop := now.Add(-time.Second)
	stopRequest := now
	retired.BillStopAt, retired.StopRequestedAt = &billStop, &stopRequest
	cutoff, err = retired.window(c, now)
	if err != nil || !cutoff.Equal(billStop) {
		t.Fatalf("earliest billing/stop boundary wins: %v %v", cutoff, err)
	}
	if _, err := retired.window(c, billStop.Add(ciEvidenceUploadGrace)); err == nil {
		t.Fatal("the exact upload grace boundary must reject")
	}
	if _, err := retired.window(c, billStop.Add(ciEvidenceUploadGrace-time.Nanosecond)); err != nil {
		t.Fatalf("final report before grace must remain possible: %v", err)
	}
}

func ciEvidencePolicyBatch(now time.Time) ciEvidenceBatch {
	cpu := 0.0
	memory, limit := int64(10), int64(20)
	return ciEvidenceBatch{
		SchemaVersion: 1, BatchID: strings.Repeat("b", 64),
		Metrics: []CIMetric{{At: now, CPUPercent: &cpu, MemoryBytes: &memory, MemoryLimitBytes: &limit}},
		Tests:   []CITestResult{{ID: strings.Repeat("c", 64), Name: "reported", Status: "passed"}},
	}
}

func TestCIEvidencePolicyUnknownAndProvenance(t *testing.T) {
	s, _, now := ciEvidencePolicyFixture(t)
	batch := ciEvidencePolicyBatch(now)
	got, err := normalizeCIEvidence(batch, s, s.DeadlineAt, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metrics[0].Source != "guest" || got.Tests[0].Source != "workflow" {
		t.Fatalf("reported evidence must not be promoted to verified: %+v", got)
	}
	if got.Tests[0].DurationMs != nil || got.Metrics[0].OOMKills != nil {
		t.Fatal("missing duration or OOM observations must stay unknown")
	}
	if batch.Metrics[0].Source != "" || batch.Tests[0].Source != "" {
		t.Fatal("normalization must not mutate the caller's input slices")
	}
	zero := int64(0)
	batch.Tests[0].DurationMs = &zero
	got, err = normalizeCIEvidence(batch, s, s.DeadlineAt, now)
	if err != nil || got.Tests[0].DurationMs == nil || *got.Tests[0].DurationMs != 0 {
		t.Fatal("an explicitly observed zero duration is not unknown")
	}
}

func TestCIEvidencePolicyRejectsInvalidObservations(t *testing.T) {
	s, _, now := ciEvidencePolicyFixture(t)
	cases := map[string]func(*ciEvidenceBatch){
		"no observations":        func(b *ciEvidenceBatch) { b.Metrics, b.Tests = []CIMetric{}, []CITestResult{} },
		"timestamp only":         func(b *ciEvidenceBatch) { b.Metrics = []CIMetric{{At: now}} },
		"zero timestamp":         func(b *ciEvidenceBatch) { b.Metrics[0].At = time.Time{} },
		"before observed window": func(b *ciEvidenceBatch) { b.Metrics[0].At = s.StartedAt.Add(-31 * time.Second) },
		"after stop cutoff":      func(b *ciEvidenceBatch) { b.Metrics[0].At = s.DeadlineAt.Add(time.Second) },
		"future timestamp":       func(b *ciEvidenceBatch) { b.Metrics[0].At = now.Add(31 * time.Second) },
		"negative cpu":           func(b *ciEvidenceBatch) { v := -1.0; b.Metrics[0].CPUPercent = &v },
		"overallocated cpu":      func(b *ciEvidenceBatch) { v := 101.0; b.Metrics[0].CPUPercent = &v },
		"nan cpu":                func(b *ciEvidenceBatch) { v := math.NaN(); b.Metrics[0].CPUPercent = &v },
		"infinite cpu":           func(b *ciEvidenceBatch) { v := math.Inf(1); b.Metrics[0].CPUPercent = &v },
		"negative network":       func(b *ciEvidenceBatch) { v := int64(-1); b.Metrics[0].NetworkRxBytes = &v },
		"memory above limit":     func(b *ciEvidenceBatch) { v := int64(21); b.Metrics[0].MemoryBytes = &v },
		"memory above catalog":   func(b *ciEvidenceBatch) { v := ciEvidenceMemoryMaxBytes + 1; b.Metrics[0].MemoryLimitBytes = &v },
		"source spoof":           func(b *ciEvidenceBatch) { b.Metrics[0].Source = "verified" },
		"test source spoof":      func(b *ciEvidenceBatch) { b.Tests[0].Source = "verified" },
		"unsupported outcome":    func(b *ciEvidenceBatch) { b.Tests[0].Status = "probably passed" },
		"negative duration":      func(b *ciEvidenceBatch) { v := int64(-1); b.Tests[0].DurationMs = &v },
		"invalid test identity":  func(b *ciEvidenceBatch) { b.Tests[0].Name = "\x00" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			batch := ciEvidencePolicyBatch(now)
			mutate(&batch)
			if _, err := normalizeCIEvidence(batch, s, s.DeadlineAt, now); err == nil {
				t.Fatal("invalid observation must not be stored")
			}
		})
	}
}
