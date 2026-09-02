package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

func (s *Store) RecordDeploymentArtifact(ctx context.Context, workspaceID string, staged sdk.StagedArtifact, entries []DeploymentArtifactEntry, actor sdk.ActorRef) (DeploymentArtifact, error) {
	now := s.now()
	var record DeploymentArtifact
	expectedKey, err := sdk.ControlPlaneArtifactKey(staged.SHA256)
	if err != nil || staged.Key != expectedKey || staged.Size < 1 {
		return record, fmt.Errorf("staged artifact does not match its canonical digest key")
	}
	rawEntries, err := json.Marshal(entries)
	if err != nil {
		return record, err
	}
	var storedEntries []byte
	err = s.pool.QueryRow(ctx, `INSERT INTO deployment_artifacts(workspace_id,sha256,storage_key,size_bytes,content_type,filename,entries,uploaded_by_kind,uploaded_by_id,uploaded_session_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(workspace_id,sha256) DO UPDATE SET sha256=EXCLUDED.sha256 RETURNING workspace_id,sha256,size_bytes,content_type,filename,entries,uploaded_by_kind,uploaded_by_id,uploaded_session_id,created_at`, workspaceID, staged.SHA256, staged.Key, staged.Size, staged.ContentType, staged.Filename, rawEntries, actor.Kind, actor.ID, actor.SessionID, now).Scan(&record.WorkspaceID, &record.SHA256, &record.Size, &record.ContentType, &record.Filename, &storedEntries, &record.UploadedBy.Kind, &record.UploadedBy.ID, &record.UploadedBy.SessionID, &record.CreatedAt)
	if err == nil {
		err = json.Unmarshal(storedEntries, &record.Entries)
	}
	return record, err
}

func (s *Store) DeploymentArtifact(ctx context.Context, workspaceID, sha string) (DeploymentArtifact, sdk.StagedArtifact, error) {
	var record DeploymentArtifact
	var staged sdk.StagedArtifact
	var rawEntries []byte
	err := s.pool.QueryRow(ctx, `SELECT workspace_id,sha256,storage_key,size_bytes,content_type,filename,entries,uploaded_by_kind,uploaded_by_id,uploaded_session_id,created_at FROM deployment_artifacts WHERE workspace_id=$1 AND sha256=$2`, workspaceID, sha).Scan(&record.WorkspaceID, &record.SHA256, &staged.Key, &record.Size, &record.ContentType, &record.Filename, &rawEntries, &record.UploadedBy.Kind, &record.UploadedBy.ID, &record.UploadedBy.SessionID, &record.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, staged, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(rawEntries, &record.Entries)
	}
	staged.SHA256, staged.Size, staged.ContentType, staged.Filename = record.SHA256, record.Size, record.ContentType, record.Filename
	if err == nil {
		expectedKey, keyErr := sdk.ControlPlaneArtifactKey(staged.SHA256)
		if keyErr != nil || staged.Key != expectedKey {
			return record, staged, fmt.Errorf("stored artifact key is not canonical for its digest")
		}
	}
	return record, staged, err
}

func (s *Store) CreateInitialDeployment(ctx context.Context, deployment InitialDeployment) error {
	raw, err := json.Marshal(deployment)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO initial_deployments(id,workspace_id,system_name,phase,summary,digest,document,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, deployment.ID, deployment.WorkspaceID, deployment.System, deployment.Phase, deployment.Summary, deployment.Digest, raw, deployment.CreatedAt, deployment.UpdatedAt)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && stringsContains(err.Error(), "SQLSTATE 23505")
}

func stringsContains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}

func (s *Store) InitialDeployment(ctx context.Context, workspaceID, id string) (InitialDeployment, error) {
	deployment, err := scanInitialDeploymentWithDigest(s.pool.QueryRow(ctx, `SELECT document,digest FROM initial_deployments WHERE workspace_id=$1 AND id=$2`, workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return deployment, ErrNotFound
	}
	return deployment, err
}

func (s *Store) ListInitialDeployments(ctx context.Context, workspaceID string) ([]InitialDeploymentIndex, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,system_name,phase,summary,digest FROM initial_deployments WHERE workspace_id=$1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]InitialDeploymentIndex, 0)
	for rows.Next() {
		var item InitialDeploymentIndex
		if err := rows.Scan(&item.ID, &item.System, &item.Phase, &item.Summary, &item.Digest); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) AuthorizeInitialDeployment(ctx context.Context, workspaceID, id, digest string, actor sdk.ActorRef) (InitialDeployment, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return InitialDeployment{}, err
	}
	defer tx.Rollback(ctx)
	deployment, err := scanInitialDeploymentWithDigest(tx.QueryRow(ctx, `SELECT document,digest FROM initial_deployments WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return deployment, ErrNotFound
	}
	if err != nil {
		return deployment, err
	}
	recomputed, digestErr := digestInitialDeployment(deployment.Plan)
	if digestErr != nil {
		return deployment, digestErr
	}
	if deployment.Phase != "drafted" || digest == "" || digest != deployment.Digest || recomputed != deployment.Digest {
		return deployment, ErrConflict
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM workspaces WHERE id=$1`, workspaceID).Scan(&revision); err != nil {
		return deployment, err
	}
	if revision != deployment.Plan.WorkspaceRevision {
		return deployment, ErrConflict
	}
	now := s.now()
	deployment.Phase = "authorized"
	deployment.Authorization = &sdk.Authorization{Digest: digest, AuthorizedAt: now, AuthorizedBy: &actor}
	deployment.UpdatedAt = now
	if err := updateInitialDeploymentTx(ctx, tx, deployment); err != nil {
		return deployment, err
	}
	return deployment, tx.Commit(ctx)
}

func updateInitialDeploymentTx(ctx context.Context, tx pgx.Tx, deployment InitialDeployment) error {
	raw, err := json.Marshal(deployment)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE initial_deployments SET phase=$1,document=$2,updated_at=$3 WHERE id=$4`, deployment.Phase, raw, deployment.UpdatedAt, deployment.ID)
	return err
}

func (s *Store) EnqueueInitialDeployment(ctx context.Context, workspaceID, deploymentID string, actor sdk.ActorRef) (InitialDeploymentExecution, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return InitialDeploymentExecution{}, err
	}
	defer tx.Rollback(ctx)
	deployment, err := scanInitialDeploymentWithDigest(tx.QueryRow(ctx, `SELECT document,digest FROM initial_deployments WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspaceID, deploymentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return InitialDeploymentExecution{}, ErrNotFound
	}
	if err != nil {
		return InitialDeploymentExecution{}, err
	}
	recomputed, digestErr := digestInitialDeployment(deployment.Plan)
	if digestErr != nil {
		return InitialDeploymentExecution{}, digestErr
	}
	if (deployment.Phase != "authorized" && deployment.Phase != "failed") || deployment.Authorization == nil || deployment.Authorization.Digest != deployment.Digest || recomputed != deployment.Digest {
		return InitialDeploymentExecution{}, ErrConflict
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM initial_deployment_executions WHERE deployment_id=$1 AND phase IN ('queued','running'))`, deploymentID).Scan(&active); err != nil {
		return InitialDeploymentExecution{}, enqueueInitialDeploymentError(err)
	}
	if active {
		return InitialDeploymentExecution{}, ErrConflict
	}
	if err := reserveInitialDeploymentUsage(ctx, tx, workspaceID, deploymentID, s.now()); err != nil {
		return InitialDeploymentExecution{}, enqueueInitialDeploymentError(err)
	}
	id, _ := newID("ide_")
	now := s.now()
	var execution InitialDeploymentExecution
	err = tx.QueryRow(ctx, `INSERT INTO initial_deployment_executions(id,workspace_id,deployment_id,system_name,phase,requested_by_kind,requested_by_id,requested_session_id,available_at,created_at) VALUES($1,$2,$3,$4,'queued',$5,$6,$7,$8,$8) RETURNING `+initialExecutionColumns, id, workspaceID, deploymentID, deployment.System, actor.Kind, actor.ID, actor.SessionID, now).Scan(&execution.ID, &execution.WorkspaceID, &execution.DeploymentID, &execution.SystemName, &execution.Phase, &execution.RequestedBy.Kind, &execution.RequestedBy.ID, &execution.RequestedBy.SessionID, &execution.Attempts, &execution.AvailableAt, &execution.ClaimedBy, &execution.ClaimToken, &execution.LeaseExpiresAt, &execution.Failure, &execution.CreatedAt, &execution.StartedAt, &execution.CompletedAt)
	if err != nil {
		return execution, enqueueInitialDeploymentError(err)
	}
	for index := range deployment.Operations {
		operation := &deployment.Operations[index]
		if operation.Phase == "succeeded" {
			continue
		}
		operation.Phase, operation.Failure = "pending", ""
		operation.StartedAt, operation.CompletedAt = nil, nil
	}
	deployment.Phase, deployment.Failure, deployment.CompletedAt, deployment.UpdatedAt = "queued", "", nil, now
	if err := updateInitialDeploymentTx(ctx, tx, deployment); err != nil {
		return execution, enqueueInitialDeploymentError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return execution, enqueueInitialDeploymentError(err)
	}
	return execution, nil
}

const initialDeploymentReservationCents = 500

// reserveInitialDeploymentUsage is deliberately internal to the beta. It is a
// hard provider-spend guard, not a customer-facing balance or pricing model.
// Retries of the same authorized proposal reuse its reservation; distinct
// proposals serialize on the workspace cap row and cannot race past the cap.
func reserveInitialDeploymentUsage(ctx context.Context, tx pgx.Tx, workspaceID, deploymentID string, now time.Time) error {
	var alreadyReserved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_reservations WHERE workspace_id=$1 AND subject_kind='initial-deployment' AND subject_id=$2 AND phase IN ('reserved','committed'))`, workspaceID, deploymentID).Scan(&alreadyReserved); err != nil {
		return err
	}
	if alreadyReserved {
		return nil
	}
	var limit, reserved, spent int
	if err := tx.QueryRow(ctx, `SELECT limit_cents,reserved_cents,spent_cents FROM workspace_usage_caps WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(&limit, &reserved, &spent); err != nil {
		return err
	}
	if reserved+spent+initialDeploymentReservationCents > limit {
		return fmt.Errorf("%w: this workspace cannot allocate more beta compute", ErrCapacity)
	}
	id, _ := newID("usrv_")
	if _, err := tx.Exec(ctx, `INSERT INTO usage_reservations(id,workspace_id,subject_kind,subject_id,amount_cents,phase,created_at,updated_at) VALUES($1,$2,'initial-deployment',$3,$4,'reserved',$5,$5)`, id, workspaceID, deploymentID, initialDeploymentReservationCents, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE workspace_usage_caps SET reserved_cents=reserved_cents+$1,updated_at=$2 WHERE workspace_id=$3`, initialDeploymentReservationCents, now, workspaceID)
	return err
}

func enqueueInitialDeploymentError(err error) error {
	if err == nil {
		return nil
	}
	var databaseError interface{ SQLState() string }
	if errors.As(err, &databaseError) && (databaseError.SQLState() == "40001" || databaseError.SQLState() == "23505") {
		return ErrConflict
	}
	return err
}

func scanInitialExecution(row pgx.Row) (InitialDeploymentExecution, error) {
	var e InitialDeploymentExecution
	err := row.Scan(&e.ID, &e.WorkspaceID, &e.DeploymentID, &e.SystemName, &e.Phase, &e.RequestedBy.Kind, &e.RequestedBy.ID, &e.RequestedBy.SessionID, &e.Attempts, &e.AvailableAt, &e.ClaimedBy, &e.ClaimToken, &e.LeaseExpiresAt, &e.Failure, &e.CreatedAt, &e.StartedAt, &e.CompletedAt)
	return e, err
}

const initialExecutionColumns = `id,workspace_id,deployment_id,system_name,phase,requested_by_kind,requested_by_id,requested_session_id,attempts,available_at,COALESCE(claimed_by,''),claim_token,lease_expires_at,failure,created_at,started_at,completed_at`

func (s *Store) InitialDeploymentExecution(ctx context.Context, id string) (InitialDeploymentExecution, error) {
	e, err := scanInitialExecution(s.pool.QueryRow(ctx, `SELECT `+initialExecutionColumns+` FROM initial_deployment_executions WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Store) HasPriorFailedInitialDeploymentExecution(ctx context.Context, deploymentID, currentExecutionID string) (bool, error) {
	var prior bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM initial_deployment_executions WHERE deployment_id=$1 AND id<>$2 AND phase='failed')`, deploymentID, currentExecutionID).Scan(&prior)
	return prior, err
}

// ReopenDestroyedInitialDeploymentHost invalidates only a previously
// succeeded host-bootstrap receipt after durable state proves that host was
// explicitly destroyed. The active execution must be a later human retry and
// a prior failed execution must exist; ordinary lease reclaim cannot reopen a
// provider side effect.
func (s *Store) ReopenDestroyedInitialDeploymentHost(ctx context.Context, executionID, worker, claimToken, deploymentID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	if err := lockActiveInitialExecution(ctx, tx, executionID, worker, claimToken, deploymentID, now); err != nil {
		return err
	}
	var requestedByKind string
	if err := tx.QueryRow(ctx, `SELECT requested_by_kind FROM initial_deployment_executions WHERE id=$1`, executionID).Scan(&requestedByKind); err != nil {
		return err
	}
	if requestedByKind != "human" {
		return ErrForbidden
	}
	var priorFailed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM initial_deployment_executions WHERE deployment_id=$1 AND id<>$2 AND phase='failed')`, deploymentID, executionID).Scan(&priorFailed); err != nil {
		return err
	}
	if !priorFailed {
		return ErrConflict
	}
	deployment, err := scanInitialDeployment(tx.QueryRow(ctx, `SELECT document FROM initial_deployments WHERE id=$1 FOR UPDATE`, deploymentID))
	if err != nil {
		return err
	}
	reopened := false
	for index := range deployment.Operations {
		operation := &deployment.Operations[index]
		if operation.ID != "02-bootstrap-host" || operation.Phase != "succeeded" {
			continue
		}
		operation.Phase, operation.Failure = "pending", ""
		operation.StartedAt, operation.CompletedAt = nil, nil
		reopened = true
		break
	}
	if !reopened {
		return tx.Commit(ctx)
	}
	evidence := deployment.Evidence[:0]
	for _, item := range deployment.Evidence {
		if item.OperationID != "02-bootstrap-host" {
			evidence = append(evidence, item)
		}
	}
	deployment.Evidence = evidence
	deployment.UpdatedAt = now
	if err := updateInitialDeploymentTx(ctx, tx, deployment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ClaimInitialDeploymentExecution(ctx context.Context, worker string, lease time.Duration) (InitialDeploymentExecution, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InitialDeploymentExecution{}, false, err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM initial_deployment_executions WHERE (phase='queued' AND available_at<=$1) OR (phase='running' AND lease_expires_at<=$1) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return InitialDeploymentExecution{}, false, nil
	}
	if err != nil {
		return InitialDeploymentExecution{}, false, err
	}
	claimToken, _ := newSecret("fence_", 24)
	e, err := scanInitialExecution(tx.QueryRow(ctx, `UPDATE initial_deployment_executions SET phase='running',claimed_by=$1,claim_token=$2,lease_expires_at=$3,attempts=attempts+1,started_at=COALESCE(started_at,$4),failure='' WHERE id=$5 RETURNING `+initialExecutionColumns, worker, claimToken, now.Add(lease), now, id))
	if err != nil {
		return e, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE initial_deployments SET phase='running',document=jsonb_set(document,'{phase}','"running"'),updated_at=$1 WHERE id=$2`, now, e.DeploymentID); err != nil {
		return e, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return e, false, err
	}
	return e, true, nil
}

func (s *Store) RenewInitialDeploymentExecution(ctx context.Context, id, worker, claimToken string, lease time.Duration) error {
	now := s.now()
	result, err := s.pool.Exec(ctx, `UPDATE initial_deployment_executions SET lease_expires_at=$1 WHERE id=$2 AND phase='running' AND claimed_by=$3 AND claim_token=$4 AND lease_expires_at>$5`, now.Add(lease), id, worker, claimToken, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) CompleteInitialDeploymentExecution(ctx context.Context, id, worker, claimToken string, applyErr error) error {
	phase, failure := "succeeded", ""
	if applyErr != nil {
		phase, failure = "failed", applyErr.Error()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	var deploymentID string
	if err := tx.QueryRow(ctx, `UPDATE initial_deployment_executions SET phase=$1,failure=$2,completed_at=$3,lease_expires_at=NULL WHERE id=$4 AND phase='running' AND claimed_by=$5 AND claim_token=$6 AND lease_expires_at>$3 RETURNING deployment_id`, phase, failure, now, id, worker, claimToken).Scan(&deploymentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	deployment, err := scanInitialDeployment(tx.QueryRow(ctx, `SELECT document FROM initial_deployments WHERE id=$1 FOR UPDATE`, deploymentID))
	if err != nil {
		return err
	}
	deployment.Phase, deployment.Failure, deployment.UpdatedAt, deployment.CompletedAt = phase, failure, now, &now
	if err := updateInitialDeploymentTx(ctx, tx, deployment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockActiveInitialExecution(ctx context.Context, tx pgx.Tx, executionID, worker, claimToken, deploymentID string, now time.Time) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM initial_deployment_executions WHERE id=$1 AND phase='running' AND claimed_by=$2 AND claim_token=$3 AND deployment_id=$4 AND lease_expires_at>$5 FOR UPDATE`, executionID, worker, claimToken, deploymentID, now).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	return err
}

// BeginInitialDeploymentOperation atomically verifies the active claim and
// marks an unfinished operation running. A succeeded operation returns false so
// a reclaimed worker never repeats its provider side effect.
func (s *Store) BeginInitialDeploymentOperation(ctx context.Context, executionID, worker, claimToken, deploymentID, operationID string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	if err := lockActiveInitialExecution(ctx, tx, executionID, worker, claimToken, deploymentID, now); err != nil {
		return false, err
	}
	deployment, err := scanInitialDeployment(tx.QueryRow(ctx, `SELECT document FROM initial_deployments WHERE id=$1 FOR UPDATE`, deploymentID))
	if err != nil {
		return false, err
	}
	found := false
	for index := range deployment.Operations {
		operation := &deployment.Operations[index]
		if operation.ID != operationID {
			continue
		}
		found = true
		if operation.Phase == "succeeded" {
			return false, tx.Commit(ctx)
		}
		operation.Phase, operation.Failure = "running", ""
		operation.StartedAt, operation.CompletedAt = &now, nil
		break
	}
	if !found {
		return false, ErrNotFound
	}
	deployment.UpdatedAt = now
	if err := updateInitialDeploymentTx(ctx, tx, deployment); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) FinishInitialDeploymentOperation(ctx context.Context, executionID, worker, claimToken, deploymentID, operationID, phase, failure string, evidence *sdk.ChangeEvidence) error {
	if phase != "succeeded" && phase != "failed" {
		return fmt.Errorf("operation completion phase must be succeeded or failed")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	if err := lockActiveInitialExecution(ctx, tx, executionID, worker, claimToken, deploymentID, now); err != nil {
		return err
	}
	deployment, err := scanInitialDeployment(tx.QueryRow(ctx, `SELECT document FROM initial_deployments WHERE id=$1 FOR UPDATE`, deploymentID))
	if err != nil {
		return err
	}
	found := false
	for index := range deployment.Operations {
		operation := &deployment.Operations[index]
		if operation.ID != operationID {
			continue
		}
		found = true
		operation.Phase, operation.Failure, operation.CompletedAt = phase, failure, &now
		break
	}
	if !found {
		return ErrNotFound
	}
	if evidence != nil {
		duplicate := false
		for _, existing := range deployment.Evidence {
			if existing.OperationID == evidence.OperationID && existing.Statement == evidence.Statement {
				duplicate = true
				break
			}
		}
		if !duplicate {
			deployment.Evidence = append(deployment.Evidence, *evidence)
		}
	}
	deployment.UpdatedAt = now
	if err := updateInitialDeploymentTx(ctx, tx, deployment); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
