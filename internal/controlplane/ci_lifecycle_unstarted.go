package controlplane

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const ciUnstartedLifecycleProfileSQL = `
SELECT profile_digest
FROM ci_provisioning_reservations
WHERE workspace_id=$1 AND runner_id=$2 AND operation_id=$3 AND released_at IS NULL`

// recoverUnstartedCILifecycle must run before the lifecycle worker acquires its
// session advisory lock: the existing creation APIs acquire the same lock in
// their own transactions. It replays only a stop-only creation rejection.
// Independent enforcer and provider observations still own all cleanup proof.
func (s *Store) recoverUnstartedCILifecycle(ctx context.Context, workspace, runner string, enforcer *ciEnforcerClient) error {
	if enforcer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ciManagedDBBound)
	defer cancel()
	snap, err := s.loadCIRunnerLifecycle(ctx, workspace, runner)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if snap.CleanupVerifiedAt != nil || snap.StopRequestedAt == nil ||
		snap.ExecutionStartedAt != nil || snap.CreateAttemptedAt != nil {
		return nil
	}
	var profile string
	err = s.pool.QueryRow(ctx, ciUnstartedLifecycleProfileSQL,
		workspace, runner, snap.Plan.OperationID).Scan(&profile)
	if errors.Is(err, pgx.ErrNoRows) {
		// Non-managed lifecycles have no managed funding reservation. They
		// retain the existing observation-only cleanup path.
		return nil
	}
	if err != nil {
		return err
	}
	scope, err := s.ciProvisioningIdentity(ctx, workspace, runner, profile, snap.Plan)
	if err != nil {
		return err
	}
	if scope.Snapshot.StopRequestedAt == nil || scope.Snapshot.ExecutionStartedAt != nil ||
		scope.Snapshot.CreateAttemptedAt != nil {
		return ErrConflict
	}
	// Under the existing creation/workspace fences, a committed stop plus
	// a not-started journal permanently forbids every future provider stage.
	// This persists no stopped/deleted proof and grants no new spending.
	if err := s.sealUnstartedCIProvisioningCreation(ctx, workspace, runner, profile, snap.Plan); err != nil {
		return err
	}
	_, err = enforcer.RejectUnstarted(ctx, scope.Spec)
	return ciManagedEnforcerErr(err)
}
