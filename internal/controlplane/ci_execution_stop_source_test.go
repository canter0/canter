package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func ciStopSourceProof() ciRunnerJobProof {
	return ciRunnerJobProof{
		RepositoryID: 789,
		Repository:   "financial-tests/repo",
		JobID:        1001,
		RunID:        1001,
		Attempt:      1,
		RunnerID:     31,
		RunnerName:   ciAdmissionName,
		StartedAt:    time.Date(2026, 11, 30, 23, 0, 3, 0, time.UTC),
	}
}

func ciStopSourceCompletedAt(proof ciRunnerJobProof) time.Time {
	return proof.StartedAt.Add(30 * time.Minute)
}

func ciStopSourceJobBody(proof ciRunnerJobProof, completedAt time.Time, mutate func(map[string]any)) string {
	job := map[string]any{
		"id":           proof.JobID,
		"run_id":       proof.RunID,
		"run_attempt":  proof.Attempt,
		"runner_id":    proof.RunnerID,
		"runner_name":  proof.RunnerName,
		"status":       "completed",
		"started_at":   proof.StartedAt.Format(time.RFC3339),
		"completed_at": completedAt.Format(time.RFC3339),
		"conclusion":   "success",
		"labels":       []string{},
	}
	if mutate != nil {
		mutate(job)
	}
	raw, _ := json.Marshal(job)
	return string(raw)
}

func ciStopSourceFixture(t *testing.T, fullName string, private *bool, jobBody string) *ciSourceFixture {
	t.Helper()
	f := newCISourceFixture(t)
	f.setRespond(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/789":
			priv := "null"
			if private != nil {
				priv = fmt.Sprint(*private)
			}
			fmt.Fprintf(w, `{"id":789,"full_name":%q,"private":%s}`, fullName, priv)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/actions/jobs/1001"):
			fmt.Fprint(w, jobBody)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"unstubbed"}`)
		}
	})
	return f
}

func TestCIExecutionStopSourceCompletedRunnerJob(t *testing.T) {
	ctx := context.Background()
	proof := ciStopSourceProof()
	now := proof.StartedAt.Add(time.Hour)
	completedAt := ciStopSourceCompletedAt(proof)
	tru := true

	t.Run("completed", func(t *testing.T) {
		f := ciStopSourceFixture(t, "financial-tests/repo", &tru, ciStopSourceJobBody(proof, completedAt, nil))
		at, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", proof, now)
		if err != nil || !at.Equal(completedAt) {
			t.Fatalf("completedRunnerJob = %v %v", at, err)
		}
		if f.count() != 2 {
			t.Fatalf("requests = %d", f.count())
		}
		for i := 0; i < f.count(); i++ {
			if got := f.request(i).Authorization; got != "Bearer ghs_stop_source" {
				t.Fatalf("request %d authorization = %q", i, got)
			}
		}
	})

	t.Run("repo_renamed_numeric_id", func(t *testing.T) {
		f := ciStopSourceFixture(t, "renamed-org/widgets", &tru, ciStopSourceJobBody(proof, completedAt, nil))
		at, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", proof, now)
		if err != nil || !at.Equal(completedAt) {
			t.Fatalf("renamed repo: %v %v", at, err)
		}
		if got := f.request(1).Path; got != "/repos/renamed-org/widgets/actions/jobs/1001" {
			t.Fatalf("job path = %q", got)
		}
	})

	t.Run("identity_mismatches", func(t *testing.T) {
		cases := map[string]func(map[string]any){
			"job_id":       func(j map[string]any) { j["id"] = 999 },
			"run_id":       func(j map[string]any) { j["run_id"] = 1002 },
			"runner_id":    func(j map[string]any) { j["runner_id"] = 32 },
			"runner_name":  func(j map[string]any) { j["runner_name"] = "other-runner" },
			"run_attempt":  func(j map[string]any) { j["run_attempt"] = 2 },
			"started_at":   func(j map[string]any) { j["started_at"] = proof.StartedAt.Add(time.Second).Format(time.RFC3339) },
			"completed_at": func(j map[string]any) { j["completed_at"] = proof.StartedAt.Add(-time.Second).Format(time.RFC3339) },
			"future":       func(j map[string]any) { j["completed_at"] = now.Add(2 * time.Hour).Format(time.RFC3339) },
			"no_conclusion": func(j map[string]any) {
				j["conclusion"] = nil
			},
			"no_completed_at": func(j map[string]any) {
				j["completed_at"] = nil
			},
			"bad_conclusion": func(j map[string]any) { j["conclusion"] = "bogus" },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				f := ciStopSourceFixture(t, "financial-tests/repo", &tru, ciStopSourceJobBody(proof, completedAt, mutate))
				if _, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", proof, now); !errors.Is(err, ErrConflict) {
					t.Fatalf("mismatch accepted: %v", err)
				}
			})
		}
	})

	t.Run("in_progress_pending", func(t *testing.T) {
		body := ciStopSourceJobBody(proof, completedAt, func(j map[string]any) {
			j["status"] = "in_progress"
			j["completed_at"] = nil
			j["conclusion"] = nil
		})
		f := ciStopSourceFixture(t, "financial-tests/repo", &tru, body)
		if _, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", proof, now); !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("in_progress: %v", err)
		}
	})

	t.Run("in_progress_with_completion_conflicts", func(t *testing.T) {
		body := ciStopSourceJobBody(proof, completedAt, func(j map[string]any) {
			j["status"] = "in_progress"
		})
		f := ciStopSourceFixture(t, "financial-tests/repo", &tru, body)
		if _, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", proof, now); !errors.Is(err, ErrConflict) {
			t.Fatalf("inconsistent job: %v", err)
		}
	})

	t.Run("repo_metadata_conflicts", func(t *testing.T) {
		f := ciStopSourceFixture(t, "financial-tests/repo", nil, ciStopSourceJobBody(proof, completedAt, nil))
		if _, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", proof, now); !errors.Is(err, ErrConflict) {
			t.Fatalf("missing private flag: %v", err)
		}
	})

	t.Run("bad_token_rejected_before_requests", func(t *testing.T) {
		f := ciStopSourceFixture(t, "financial-tests/repo", &tru, ciStopSourceJobBody(proof, completedAt, nil))
		for _, token := range []string{"", "app", "has space"} {
			if _, err := f.client.completedRunnerJob(ctx, token, proof, now); !errors.Is(err, ErrConflict) {
				t.Fatalf("token %q accepted: %v", token, err)
			}
		}
		if f.count() != 0 {
			t.Fatalf("invalid token made %d requests", f.count())
		}
	})

	t.Run("bad_expected_conflicts", func(t *testing.T) {
		f := ciStopSourceFixture(t, "financial-tests/repo", &tru, ciStopSourceJobBody(proof, completedAt, nil))
		bad := proof
		bad.JobID = 0
		if _, err := f.client.completedRunnerJob(ctx, "ghs_stop_source", bad, now); !errors.Is(err, ErrConflict) {
			t.Fatalf("bad proof: %v", err)
		}
		if f.count() != 0 {
			t.Fatalf("invalid proof made %d requests", f.count())
		}
	})
}
