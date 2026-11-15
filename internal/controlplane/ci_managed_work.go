package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	ciManagedConcurrency  = 2
	ciManagedPollInterval = 2 * time.Second
)

type ciManagedWorkItem struct {
	Workspace string
	Runner    string
	Plan      ciRunnerLifecyclePlan
}

// The cursor rotates through a bounded batch instead of letting ambiguous
// old operations consume every dispatch slot. Restarting revisits identities,
// never replays their durable provider-write fences.
func (s *Store) listCIManagedWork(ctx context.Context, profile, afterWorkspace, afterRunner string) ([]ciManagedWorkItem, error) {
	if !ciProvisioningDigest.MatchString(profile) ||
		(afterWorkspace != "" && !ciAdmissionScopeIDValid(afterWorkspace)) ||
		(afterRunner != "" && !ciAdmissionScopeIDValid(afterRunner)) {
		return nil, ErrConflict
	}
	rows, err := s.pool.Query(ctx, `SELECT r.workspace_id,r.id,l.plan
 FROM ci_provisioning_reservations f
 JOIN ci_runners r ON r.id=f.runner_id AND r.workspace_id=f.workspace_id
 JOIN ci_runner_lifecycles l ON l.runner_id=r.id AND l.workspace_id=r.workspace_id AND l.operation_id=f.operation_id
 WHERE f.profile_digest=$1 AND f.released_at IS NULL
 AND (l.cleanup_verified_at IS NOT NULL OR
      (r.execution_started_at IS NULL AND
       (r.phase IN ('queued','provisioning') OR NOT l.create_settled OR
        l.stop_requested_at IS NOT NULL OR l.setup_deadline_at<=$2)))
 ORDER BY CASE WHEN (r.workspace_id,r.id)>($3,$4) THEN 0 ELSE 1 END,r.workspace_id,r.id
 LIMIT 2`, profile, s.now(), afterWorkspace, afterRunner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ciManagedWorkItem
	for rows.Next() {
		var item ciManagedWorkItem
		var raw []byte
		if err := rows.Scan(&item.Workspace, &item.Runner, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &item.Plan) != nil || !item.Plan.shapeValid() {
			return nil, ErrConflict
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Stop only before actual adoption, under the same workspace lock as
// admission. A late eligibility failure must not cancel an approved job.
func (s *Store) stopUnstartedCIManagedRunner(ctx context.Context, workspace, runner string, plan ciRunnerLifecyclePlan) (bool, error) {
	if !ciAdmissionScopeIDValid(workspace) || !ciAdmissionScopeIDValid(runner) || !plan.shapeValid() {
		return false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	snap, err := s.loadCIRunnerLifecycleWith(ctx, tx, workspace, runner)
	if err != nil {
		return false, err
	}
	if !ciRunnerLifecyclePlanEqual(snap.Plan, plan) {
		return false, ErrConflict
	}
	if snap.ExecutionStartedAt != nil || snap.CleanupVerifiedAt != nil || snap.Phase == "deleted" {
		return false, nil
	}
	if snap.StopRequestedAt != nil {
		return true, nil
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	_, err = tx.Exec(ctx, `UPDATE ci_runner_lifecycles SET stop_requested_at=$4,stop_reason='setup_failed',next_attempt_at=$4 WHERE workspace_id=$1 AND runner_id=$2 AND operation_id=$3 AND stop_requested_at IS NULL`,
		workspace, runner, plan.OperationID, now)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE ci_runners SET delete_requested=true,phase='deleting',updated_at=$3 WHERE workspace_id=$1 AND id=$2 AND execution_started_at IS NULL`,
		workspace, runner, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
