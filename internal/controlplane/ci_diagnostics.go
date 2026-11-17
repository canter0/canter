package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CIRunnerDiagnostics exposes recorded lifecycle evidence, never provider
// inventory, admission capabilities, or a claim that a stop request succeeded.
type CIRunnerDiagnostics struct {
	SchemaVersion      int    `json:"schemaVersion"`
	RunnerID           string `json:"runnerId"`
	Association        string `json:"association"`
	pinnedRate         CIJobRate
	Phase              string     `json:"phase"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	DeleteRequested    bool       `json:"deleteRequested"`
	CreateAttemptedAt  *time.Time `json:"createAttemptedAt"`
	CreateSettled      *bool      `json:"createSettled"`
	SetupDeadlineAt    *time.Time `json:"setupDeadlineAt"`
	HardDeadlineAt     *time.Time `json:"hardDeadlineAt"`
	ExecutionStartedAt *time.Time `json:"executionStartedAt"`
	ExecutionStoppedAt *time.Time `json:"executionStoppedAt"`
	DeadlineAt         *time.Time `json:"deadlineAt"`
	StopRequestedAt    *time.Time `json:"stopRequestedAt"`
	StopReason         *string    `json:"stopReason"`
	BillingStopAt      *time.Time `json:"billingStopAt"`
	StoppedVerifiedAt  *time.Time `json:"stoppedVerifiedAt"`
	CleanupVerifiedAt  *time.Time `json:"cleanupVerifiedAt"`
}

// The assigned runner takes precedence over unassigned provisioning attempts.
// An attempt that accepted a different job is not evidence about this job.
const ciJobRunnerDiagnosticsSQL = `
SELECT r.id,
       CASE WHEN r.id=j.runner_id THEN 'assigned' ELSE 'provisioning_attempt' END,
       r.rate_version,r.rate_micros_per_minute,r.phase,r.created_at,r.updated_at,r.delete_requested,
       l.create_attempted_at,l.create_settled,l.setup_deadline_at,l.hard_deadline_at,
       r.execution_started_at,r.execution_stopped_at,r.deadline_at,
       l.stop_requested_at,l.stop_reason,l.bill_stop_at,
       l.stopped_verified_at,l.cleanup_verified_at
FROM ci_jobs j
JOIN ci_runners r ON r.workspace_id=j.workspace_id
 AND (r.id=j.runner_id OR
      (r.trigger_job_id=j.id AND r.assigned_job_id IS NULL
       AND r.installation_id=j.installation_id))
LEFT JOIN ci_runner_lifecycles l
 ON l.workspace_id=r.workspace_id AND l.runner_id=r.id
WHERE j.workspace_id=$1 AND j.id=$2
ORDER BY (r.id=COALESCE(j.runner_id,'')) DESC,r.created_at DESC,r.id DESC
LIMIT 1`

// ciJobRunnerDiagnostics must only be called after the caller's workspace and
// repository access has been verified by ciJobDetailView.
func (s *Store) ciJobRunnerDiagnostics(ctx context.Context, workspaceID, jobID string) (*CIRunnerDiagnostics, error) {
	out := &CIRunnerDiagnostics{SchemaVersion: 1}
	err := s.pool.QueryRow(ctx, ciJobRunnerDiagnosticsSQL, workspaceID, jobID).Scan(
		&out.RunnerID, &out.Association, &out.pinnedRate.Version, &out.pinnedRate.MicrosPerMinute, &out.Phase, &out.CreatedAt, &out.UpdatedAt,
		&out.DeleteRequested, &out.CreateAttemptedAt, &out.CreateSettled,
		&out.SetupDeadlineAt, &out.HardDeadlineAt, &out.ExecutionStartedAt,
		&out.ExecutionStoppedAt, &out.DeadlineAt, &out.StopRequestedAt,
		&out.StopReason, &out.BillingStopAt, &out.StoppedVerifiedAt,
		&out.CleanupVerifiedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
