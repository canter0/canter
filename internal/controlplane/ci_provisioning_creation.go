package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type ciProvisioningCreateOutcome struct {
	MutationAttempted bool
	CreationSettled   bool
	Resources         []ciOwnedResource
}

type ciProvisioningCreationJournal struct {
	Stage          string
	KnownResources []ciOwnedResource
	FinishedAt     *time.Time
	ResultDigest   string
	Outcome        *ciProvisioningCreateOutcome
}

func ciProvisioningStageIndex(stage string) int {
	switch stage {
	case "not_started":
		return 0
	case "security_group":
		return 1
	case "port":
		return 2
	case "server":
		return 3
	default:
		return -1
	}
}

// A creator acknowledges at most one resource per stage, in SDK order.
// These are retained IDs, not proof that a DELETE is safe or effective.
func ciProvisioningResourcePrefix(operation string, resources []ciOwnedResource) ([]ciOwnedResource, error) {
	kinds := []string{"security_group", "port", "server"}
	if len(resources) > len(kinds) {
		return nil, ErrConflict
	}
	out := make([]ciOwnedResource, len(resources))
	for i, r := range resources {
		if !ciResourceValid(r, operation) || r.Kind != kinds[i] {
			return nil, ErrConflict
		}
		out[i] = r
	}
	return out, nil
}

func ciProvisioningPrefixExtends(old, next []ciOwnedResource) bool {
	if len(old) > len(next) {
		return false
	}
	for i, r := range old {
		if r != next[i] {
			return false
		}
	}
	return true
}

func ciProvisioningMergeResources(operation string, retained, extra []ciOwnedResource) ([]ciOwnedResource, error) {
	out := make([]ciOwnedResource, 0, len(retained)+len(extra))
	all := make(map[string]bool)
	for _, list := range [][]ciOwnedResource{retained, extra} {
		seen := make(map[string]bool)
		for _, r := range list {
			key := ciResourceKey(r)
			if !ciResourceValid(r, operation) || seen[key] {
				return nil, ErrConflict
			}
			seen[key] = true
			if !all[key] {
				all[key] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func loadCIProvisioningCreationTx(ctx context.Context, tx pgx.Tx, operation string) (ciProvisioningCreationJournal, error) {
	j := ciProvisioningCreationJournal{Stage: "not_started", KnownResources: []ciOwnedResource{}}
	var knownRaw, resultRaw []byte
	var digest *string
	var attempted, settled *bool
	err := tx.QueryRow(ctx, `SELECT stage,known_resources,creator_finished_at,result_digest,mutation_attempted,result_settled,result_resources FROM ci_runner_creation_journal WHERE operation_id=$1`, operation).Scan(
		&j.Stage, &knownRaw, &j.FinishedAt, &digest, &attempted, &settled, &resultRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	index := ciProvisioningStageIndex(j.Stage)
	if index < 0 || json.Unmarshal(knownRaw, &j.KnownResources) != nil {
		return j, ErrConflict
	}
	prefix, err := ciProvisioningResourcePrefix(operation, j.KnownResources)
	if err != nil || (index == 0 && len(prefix) != 0) || (index > 0 && len(prefix) != index-1) {
		return j, ErrConflict
	}
	j.KnownResources = prefix
	if j.FinishedAt != nil {
		if digest == nil || attempted == nil || settled == nil || !ciProvisioningDigest.MatchString(*digest) {
			return j, ErrConflict
		}
		j.ResultDigest = *digest
		j.Outcome = &ciProvisioningCreateOutcome{MutationAttempted: *attempted, CreationSettled: *settled}
		if json.Unmarshal(resultRaw, &j.Outcome.Resources) != nil {
			return j, ErrConflict
		}
		normalized, err := normalizeCIProvisioningOutcome(operation, j, *j.Outcome)
		if err != nil {
			return j, err
		}
		computed, err := ciProvisioningJSONDigest(normalized)
		if err != nil || computed != j.ResultDigest {
			return j, ErrConflict
		}
		j.Outcome = &normalized
	}
	return j, nil
}

// Every journal transition shares the lifecycle lock and one transaction.
// An xact advisory lock conflicts with the worker's session lock, releases
// automatically, and never needs a second pool connection (MaxConns=1).
// Actions must perform database work only, never provider or enforcer I/O.
func (s *Store) withCIProvisioningCreationTx(ctx context.Context, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan, ready bool, action func(pgx.Tx, ciRunnerLifecycleSnapshot) error) error {
	plan = plan.normalize()
	if !ciAdmissionScopeIDValid(workspace) || !ciAdmissionScopeIDValid(runner) ||
		!ciProvisioningDigest.MatchString(profileDigest) || !plan.shapeValid() ||
		plan.ServerName != ciEnforcerRunnerName(plan.OperationID) || !ciLifecycleLeaseID.MatchString(plan.EnforcerLease) {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,948))`, "ci/"+workspace+"/"+runner).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return errCIRunnerJobPending
	}
	state, err := loadCIProvisioningBudgetTx(ctx, tx)
	if err != nil {
		return err
	}
	var snap ciRunnerLifecycleSnapshot
	if ready {
		snap, err = s.ciProvisioningReadyScopeTx(ctx, tx, workspace, runner, profileDigest, plan, state)
	} else {
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		if err == nil {
			snap, err = s.loadCIRunnerLifecycleWith(ctx, tx, workspace, runner)
		}
	}
	if err != nil {
		return err
	}
	if !ciRunnerLifecyclePlanEqual(snap.Plan, plan) || state.Policy.ProfileDigest != profileDigest {
		return ErrConflict
	}
	if _, err = loadCIProvisioningReservationTx(ctx, tx, workspace, runner, profileDigest, plan, state); err != nil {
		return err
	}
	if err = action(tx, snap); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// fenceCIProvisioningStage is the trusted producer's BeforeMutation hook.
// The caller must also verify the independent lease's exact live spec before
// each hook. A stage is never replayable, even with an identical payload.
func (s *Store) fenceCIProvisioningStage(ctx context.Context, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan, stage string, known []ciOwnedResource) error {
	index := ciProvisioningStageIndex(stage)
	prefix, err := ciProvisioningResourcePrefix(plan.OperationID, known)
	if err != nil || index < 1 || len(prefix) != index-1 {
		return ErrConflict
	}
	return s.withCIProvisioningCreationTx(ctx, workspace, runner, profileDigest, plan, true, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		if snap.CreateAttemptedAt == nil || snap.CreateSettled || snap.Phase != "provisioning" {
			return ErrConflict
		}
		j, err := loadCIProvisioningCreationTx(ctx, tx, plan.OperationID)
		if err != nil {
			return err
		}
		if j.FinishedAt != nil || ciProvisioningStageIndex(j.Stage)+1 != index ||
			!ciProvisioningPrefixExtends(j.KnownResources, prefix) {
			return ErrConflict
		}
		now := s.now()
		raw, err := json.Marshal(prefix)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO ci_runner_creation_journal(operation_id,stage,known_resources,created_at,updated_at) VALUES($1,$2,$3,$4,$4) ON CONFLICT(operation_id) DO UPDATE SET stage=EXCLUDED.stage,known_resources=EXCLUDED.known_resources,updated_at=EXCLUDED.updated_at`, plan.OperationID, stage, raw, now)
		if err != nil {
			return err
		}
		combined, err := ciProvisioningMergeResources(plan.OperationID, snap.Resources, prefix)
		if err != nil {
			return err
		}
		return s.ciLifecycleSaveEvidenceWith(ctx, tx, workspace, runner, snap, combined, false, false)
	})
}

func normalizeCIProvisioningOutcome(operation string, j ciProvisioningCreationJournal, result ciProvisioningCreateOutcome) (ciProvisioningCreateOutcome, error) {
	prefix, err := ciProvisioningResourcePrefix(operation, result.Resources)
	index := ciProvisioningStageIndex(j.Stage)
	if err != nil || index < 0 || len(prefix) > index || !ciProvisioningPrefixExtends(j.KnownResources, prefix) {
		return result, ErrConflict
	}
	if !result.MutationAttempted {
		if !result.CreationSettled || len(prefix) != 0 || index > 1 {
			return result, ErrConflict
		}
	} else if index < 1 || (result.CreationSettled && (index != 3 || len(prefix) != 3)) {
		return result, ErrConflict
	}
	result.Resources = prefix
	return result, nil
}

func (s *Store) persistCIProvisioningOutcomeTx(ctx context.Context, tx pgx.Tx, workspace, runner string, snap ciRunnerLifecycleSnapshot, result ciProvisioningCreateOutcome) error {
	j, err := loadCIProvisioningCreationTx(ctx, tx, snap.Plan.OperationID)
	if err != nil {
		return err
	}
	result, err = normalizeCIProvisioningOutcome(snap.Plan.OperationID, j, result)
	if err != nil {
		return err
	}
	digest, err := ciProvisioningJSONDigest(result)
	if err != nil {
		return err
	}
	if j.FinishedAt != nil {
		if j.ResultDigest != digest {
			return ErrConflict
		}
		return nil
	}
	resultRaw, err := json.Marshal(result.Resources)
	if err != nil {
		return err
	}
	now := s.now()
	_, err = tx.Exec(ctx, `INSERT INTO ci_runner_creation_journal(operation_id,stage,known_resources,creator_finished_at,result_digest,mutation_attempted,result_settled,result_resources,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$4,$4) ON CONFLICT(operation_id) DO UPDATE SET creator_finished_at=EXCLUDED.creator_finished_at,result_digest=EXCLUDED.result_digest,mutation_attempted=EXCLUDED.mutation_attempted,result_settled=EXCLUDED.result_settled,result_resources=EXCLUDED.result_resources,updated_at=EXCLUDED.updated_at`,
		snap.Plan.OperationID, j.Stage, mustCIProvisioningResourceJSON(j.KnownResources), now, digest, result.MutationAttempted, result.CreationSettled, resultRaw)
	if err != nil {
		return err
	}
	combined, err := ciProvisioningMergeResources(snap.Plan.OperationID, snap.Resources, result.Resources)
	if err != nil {
		return err
	}
	// Creator completion is not an independent enforcer acknowledgement.
	// Keep lifecycle create_settled false until that acknowledgement is
	// observed through the existing lifecycle backend.
	return s.ciLifecycleSaveEvidenceWith(ctx, tx, workspace, runner, snap, combined, false, false)
}

func mustCIProvisioningResourceJSON(resources []ciOwnedResource) []byte {
	if resources == nil {
		resources = []ciOwnedResource{}
	}
	raw, _ := json.Marshal(resources)
	return raw
}

// recordCIProvisioningCreationResult is called only after the sole SDK call
// returns. It persists even after stop/expiry, so cleanup is never stranded
// by a creation-only guard. It never closes the independent lease itself.
func (s *Store) recordCIProvisioningCreationResult(ctx context.Context, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan, result ciProvisioningCreateOutcome) error {
	return s.withCIProvisioningCreationTx(ctx, workspace, runner, profileDigest, plan, false, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		if snap.CreateAttemptedAt == nil {
			return ErrConflict
		}
		return s.persistCIProvisioningOutcomeTx(ctx, tx, workspace, runner, snap, result)
	})
}

// sealUnstartedCIProvisioningCreation recovers a crash before any stage fence.
// Stop must already be committed under the workspace lock. Missing inventory
// is NOT the proof: no stage was authorized, and stop forbids every later one.
func (s *Store) sealUnstartedCIProvisioningCreation(ctx context.Context, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan) error {
	return s.withCIProvisioningCreationTx(ctx, workspace, runner, profileDigest, plan, false, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		if snap.StopRequestedAt == nil {
			return ErrConflict
		}
		j, err := loadCIProvisioningCreationTx(ctx, tx, plan.OperationID)
		if err != nil {
			return err
		}
		if j.Stage != "not_started" {
			return ErrConflict
		}
		return s.persistCIProvisioningOutcomeTx(ctx, tx, workspace, runner, snap, ciProvisioningCreateOutcome{CreationSettled: true, Resources: []ciOwnedResource{}})
	})
}

func (s *Store) ciProvisioningCreationState(ctx context.Context, workspace, runner, profileDigest string, plan ciRunnerLifecyclePlan) (ciProvisioningCreationJournal, error) {
	var j ciProvisioningCreationJournal
	err := s.withCIProvisioningCreationTx(ctx, workspace, runner, profileDigest, plan, false, func(tx pgx.Tx, _ ciRunnerLifecycleSnapshot) error {
		var err error
		j, err = loadCIProvisioningCreationTx(ctx, tx, plan.OperationID)
		return err
	})
	return j, err
}
