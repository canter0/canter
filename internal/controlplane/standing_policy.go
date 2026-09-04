package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

func canonicalPolicyEnvelope(envelope StandingPolicyEnvelope) (StandingPolicyEnvelope, error) {
	envelope.AllowedInstallationIDs = canonicalPolicyValues(envelope.AllowedInstallationIDs)
	envelope.AffectedServices = canonicalPolicyValues(envelope.AffectedServices)
	envelope.OperationKinds = canonicalPolicyValues(envelope.OperationKinds)
	envelope.Availability = canonicalPolicyValues(envelope.Availability)
	envelope.Data = canonicalPolicyValues(envelope.Data)
	envelope.AllowedReversibility = canonicalPolicyValues(envelope.AllowedReversibility)
	for label, values := range map[string][]string{
		"allowedInstallationIds": envelope.AllowedInstallationIDs,
		"affectedServices":       envelope.AffectedServices,
		"operationKinds":         envelope.OperationKinds,
		"availability":           envelope.Availability,
		"data":                   envelope.Data,
		"allowedReversibility":   envelope.AllowedReversibility,
	} {
		if len(values) == 0 {
			return StandingPolicyEnvelope{}, fmt.Errorf("policy envelope requires %s", label)
		}
		if len(values) > 64 {
			return StandingPolicyEnvelope{}, fmt.Errorf("policy envelope %s exceeds 64 values", label)
		}
		for _, value := range values {
			if value == "*" || len(value) > 160 {
				return StandingPolicyEnvelope{}, fmt.Errorf("policy envelope %s must use bounded exact values", label)
			}
		}
	}
	if envelope.MaxAdditionalMonthlyCostCents < 0 {
		return StandingPolicyEnvelope{}, fmt.Errorf("policy cost ceiling cannot be negative")
	}
	if envelope.MaxOperations < 1 || envelope.MaxOperations > 64 {
		return StandingPolicyEnvelope{}, fmt.Errorf("policy maxOperations must be between 1 and 64")
	}
	if len(envelope.ScaleLimits) > 64 {
		return StandingPolicyEnvelope{}, fmt.Errorf("policy scaleLimits exceeds 64 services")
	}
	if len(envelope.ScaleLimits) > 0 && !envelope.AllowPermanentScale && (envelope.MaxScaleDurationSeconds < 60 || envelope.MaxScaleDurationSeconds > 86400) {
		return StandingPolicyEnvelope{}, fmt.Errorf("scale policy must allow permanent scaling or bind a maximum duration between 60 and 86400 seconds")
	}
	if envelope.MaxScaleDurationSeconds < 0 || envelope.MaxScaleDurationSeconds > 86400 {
		return StandingPolicyEnvelope{}, fmt.Errorf("policy maxScaleDurationSeconds must be between 0 and 86400")
	}
	for service, replicas := range envelope.ScaleLimits {
		if strings.TrimSpace(service) != service || service == "" || service == "*" || replicas.Min < 1 || replicas.Max < replicas.Min {
			return StandingPolicyEnvelope{}, fmt.Errorf("policy scaleLimits must use exact services and valid inclusive replica ranges")
		}
	}
	return envelope, nil
}

func canonicalPolicyValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Store) CreateStandingPolicy(ctx context.Context, workspaceID, systemName, accountID string, input CreateStandingPolicyInput) (StandingPolicy, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if input.Name == "" || len(input.Name) > 80 || len(input.Description) > 500 {
		return StandingPolicy{}, fmt.Errorf("policy requires a name of at most 80 characters and description of at most 500 characters")
	}
	envelope, err := canonicalPolicyEnvelope(input.Envelope)
	if err != nil {
		return StandingPolicy{}, err
	}
	now := s.now()
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(366*24*time.Hour)) {
		return StandingPolicy{}, fmt.Errorf("policy expiry must be in the future and no more than 366 days away")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StandingPolicy{}, err
	}
	defer tx.Rollback(ctx)
	var workspaceRevision, systemRevision int64
	var contractRaw []byte
	if err = tx.QueryRow(ctx, `SELECT w.revision,s.revision,s.contract FROM workspaces w JOIN systems s ON s.workspace_id=w.id WHERE w.id=$1 AND s.name=$2 FOR SHARE OF w,s`, workspaceID, systemName).Scan(&workspaceRevision, &systemRevision, &contractRaw); errors.Is(err, pgx.ErrNoRows) {
		return StandingPolicy{}, ErrNotFound
	} else if err != nil {
		return StandingPolicy{}, err
	}
	var contract sdk.System
	if err = json.Unmarshal(contractRaw, &contract); err != nil {
		return StandingPolicy{}, err
	}
	services := make(map[string]struct{}, len(contract.Spec.Services))
	for _, service := range contract.Spec.Services {
		services[service.Name] = struct{}{}
	}
	for _, service := range envelope.AffectedServices {
		if _, ok := services[service]; !ok {
			return StandingPolicy{}, fmt.Errorf("policy affected service %q is not declared by the System", service)
		}
	}
	for service, replicas := range envelope.ScaleLimits {
		if _, ok := services[service]; !ok {
			return StandingPolicy{}, fmt.Errorf("policy scale service %q is not declared by the System", service)
		}
		_, maximum, capacityErr := sdk.ScaleCapacity(contract, service)
		if capacityErr != nil {
			return StandingPolicy{}, capacityErr
		}
		if replicas.Max > maximum {
			return StandingPolicy{}, fmt.Errorf("policy scale service %s maximum %d exceeds current host capacity %d", service, replicas.Max, maximum)
		}
	}
	var activeInstallations int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM agent_installations WHERE workspace_id=$1 AND id=ANY($2) AND revoked_at IS NULL`, workspaceID, envelope.AllowedInstallationIDs).Scan(&activeInstallations); err != nil {
		return StandingPolicy{}, err
	}
	if activeInstallations != len(envelope.AllowedInstallationIDs) {
		return StandingPolicy{}, fmt.Errorf("policy may reference only active agent installations in this workspace")
	}
	payload := struct {
		WorkspaceID       string                 `json:"workspaceId"`
		System            string                 `json:"system"`
		Name              string                 `json:"name"`
		Description       string                 `json:"description"`
		Envelope          StandingPolicyEnvelope `json:"envelope"`
		WorkspaceRevision int64                  `json:"workspaceRevision"`
		SystemRevision    int64                  `json:"systemRevision"`
		ExpiresAt         time.Time              `json:"expiresAt"`
	}{workspaceID, systemName, input.Name, input.Description, envelope, workspaceRevision, systemRevision, input.ExpiresAt.UTC()}
	raw, err := json.Marshal(payload)
	if err != nil {
		return StandingPolicy{}, err
	}
	sum := sha256.Sum256(raw)
	id, _ := newID("pol_")
	policy := StandingPolicy{ID: id, WorkspaceID: workspaceID, System: systemName, Name: input.Name, Description: input.Description, Digest: hex.EncodeToString(sum[:]), Envelope: envelope, WorkspaceRevision: workspaceRevision, SystemRevision: systemRevision, CreatedByAccount: accountID, CreatedAt: now, ExpiresAt: input.ExpiresAt.UTC()}
	envelopeRaw, _ := json.Marshal(envelope)
	err = tx.QueryRow(ctx, `INSERT INTO standing_policies(id,workspace_id,system_name,name,description,digest,envelope,workspace_revision,system_revision,created_by_account,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING created_at`, policy.ID, workspaceID, systemName, policy.Name, policy.Description, policy.Digest, envelopeRaw, workspaceRevision, systemRevision, accountID, now, policy.ExpiresAt).Scan(&policy.CreatedAt)
	if err != nil {
		return StandingPolicy{}, mapPolicyStoreError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return StandingPolicy{}, err
	}
	return policy, nil
}

func scanStandingPolicy(row pgx.Row) (StandingPolicy, error) {
	var policy StandingPolicy
	var envelopeRaw []byte
	err := row.Scan(&policy.ID, &policy.WorkspaceID, &policy.System, &policy.Name, &policy.Description, &policy.Digest, &envelopeRaw, &policy.WorkspaceRevision, &policy.SystemRevision, &policy.CreatedByAccount, &policy.CreatedAt, &policy.ExpiresAt, &policy.RevokedAt, &policy.RevokedByAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, ErrNotFound
	}
	if err != nil {
		return policy, err
	}
	if err = json.Unmarshal(envelopeRaw, &policy.Envelope); err != nil {
		return StandingPolicy{}, err
	}
	return policy, nil
}

const standingPolicyColumns = `id,workspace_id,system_name,name,description,digest,envelope,workspace_revision,system_revision,created_by_account,created_at,expires_at,revoked_at,COALESCE(revoked_by_account,'')`

func (s *Store) StandingPolicy(ctx context.Context, workspaceID, systemName, policyID string) (StandingPolicy, error) {
	return scanStandingPolicy(s.pool.QueryRow(ctx, `SELECT `+standingPolicyColumns+` FROM standing_policies WHERE workspace_id=$1 AND system_name=$2 AND id=$3`, workspaceID, systemName, policyID))
}

func (s *Store) ListStandingPolicies(ctx context.Context, workspaceID, systemName string) ([]StandingPolicy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+standingPolicyColumns+` FROM standing_policies WHERE workspace_id=$1 AND system_name=$2 ORDER BY created_at DESC`, workspaceID, systemName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StandingPolicy
	for rows.Next() {
		policy, err := scanStandingPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, policy)
	}
	return out, rows.Err()
}

func (s *Store) RevokeStandingPolicy(ctx context.Context, workspaceID, systemName, policyID, accountID string) (StandingPolicy, error) {
	now := s.now()
	result, err := s.pool.Exec(ctx, `UPDATE standing_policies SET revoked_at=$1,revoked_by_account=$2 WHERE workspace_id=$3 AND system_name=$4 AND id=$5 AND revoked_at IS NULL`, now, accountID, workspaceID, systemName, policyID)
	if err != nil {
		return StandingPolicy{}, err
	}
	if result.RowsAffected() != 1 {
		return StandingPolicy{}, ErrNotFound
	}
	return s.StandingPolicy(ctx, workspaceID, systemName, policyID)
}

func policyAllows(policy StandingPolicy, installationID string, change sdk.Change) (bool, string) {
	if !containsPolicyValue(policy.Envelope.AllowedInstallationIDs, installationID) {
		return false, "requesting installation is outside the policy"
	}
	if change.Phase != "drafted" || change.Authorization != nil {
		return false, "Change is not an unauthorised draft"
	}
	if len(change.Operations) == 0 || len(change.Operations) > policy.Envelope.MaxOperations {
		return false, "operation count exceeds the policy"
	}
	if change.Plan.Impact.MonthlyCostDeltaCents > policy.Envelope.MaxAdditionalMonthlyCostCents {
		return false, "additional monthly cost exceeds the policy"
	}
	if change.Plan.Scale != nil {
		limit, ok := policy.Envelope.ScaleLimits[change.Plan.Scale.Service]
		if !ok {
			return false, "replica scaling is not authorized for this service"
		}
		if change.Plan.Scale.FromReplicas < limit.Min || change.Plan.Scale.FromReplicas > limit.Max || change.Plan.Scale.ToReplicas < limit.Min || change.Plan.Scale.ToReplicas > limit.Max {
			return false, "replica transition is outside the policy range"
		}
		if change.Plan.Scale.LeaseSeconds == 0 && !policy.Envelope.AllowPermanentScale {
			return false, "permanent replica scaling is outside the policy"
		}
		if change.Plan.Scale.LeaseSeconds > 0 && (policy.Envelope.MaxScaleDurationSeconds == 0 || change.Plan.Scale.LeaseSeconds > policy.Envelope.MaxScaleDurationSeconds) {
			return false, "temporary replica duration is outside the policy"
		}
	}
	if !containsPolicyValue(policy.Envelope.Availability, change.Plan.Impact.Availability) {
		return false, "availability impact is outside the policy"
	}
	if !containsPolicyValue(policy.Envelope.Data, change.Plan.Impact.Data) {
		return false, "data impact is outside the policy"
	}
	for _, service := range change.Plan.Impact.AffectedServices {
		if !containsPolicyValue(policy.Envelope.AffectedServices, service) {
			return false, "an affected service is outside the policy"
		}
	}
	for _, operation := range change.Operations {
		if !containsPolicyValue(policy.Envelope.OperationKinds, operation.Kind) {
			return false, "an operation kind is outside the policy"
		}
		if !containsPolicyValue(policy.Envelope.AllowedReversibility, operation.Reversibility) {
			return false, "an operation reversibility class is outside the policy"
		}
	}
	return true, "exact Change is within the standing policy envelope"
}

func containsPolicyValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (s *Store) EvaluateStandingPolicies(ctx context.Context, workspaceID, systemName, installationID string, workspaceRevision, systemRevision int64, change sdk.Change) (PolicyDecision, *StandingPolicy, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PolicyDecision{}, nil, err
	}
	defer tx.Rollback(ctx)

	var existing PolicyDecision
	existing, err = scanPolicyDecision(tx.QueryRow(ctx, `SELECT `+policyDecisionColumns+` FROM change_policy_decisions WHERE workspace_id=$1 AND system_name=$2 AND change_id=$3 AND change_digest=$4 FOR UPDATE`, workspaceID, systemName, change.ID, change.Digest))
	if err == nil && existing.Outcome == "automatic" {
		policy, policyErr := scanStandingPolicy(tx.QueryRow(ctx, `SELECT `+standingPolicyColumns+` FROM standing_policies WHERE id=$1`, existing.PolicyID))
		if policyErr != nil {
			return PolicyDecision{}, nil, policyErr
		}
		if err = tx.Commit(ctx); err != nil {
			return PolicyDecision{}, nil, err
		}
		return existing, &policy, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return PolicyDecision{}, nil, err
	}

	now := s.now()
	rows, err := tx.Query(ctx, `SELECT `+standingPolicyColumns+` FROM standing_policies WHERE workspace_id=$1 AND system_name=$2 AND revoked_at IS NULL AND expires_at>$3 AND workspace_revision=$4 AND system_revision=$5 ORDER BY created_at FOR SHARE`, workspaceID, systemName, now, workspaceRevision, systemRevision)
	if err != nil {
		return PolicyDecision{}, nil, err
	}
	var matched *StandingPolicy
	reason := "no active standing policy matched the exact Change envelope"
	for rows.Next() {
		policy, scanErr := scanStandingPolicy(rows)
		if scanErr != nil {
			rows.Close()
			return PolicyDecision{}, nil, scanErr
		}
		allowed, mismatch := policyAllows(policy, installationID, change)
		if allowed && matched == nil {
			copy := policy
			matched = &copy
			reason = mismatch
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return PolicyDecision{}, nil, err
	}
	id, _ := newID("pdc_")
	outcome, phase, policyID, policyDigest := "human-approval-required", "decided", "", ""
	if matched != nil {
		outcome, policyID, policyDigest = "automatic", matched.ID, matched.Digest
	}
	decision := PolicyDecision{ID: id, WorkspaceID: workspaceID, System: systemName, ChangeID: change.ID, ChangeDigest: change.Digest, Outcome: outcome, Phase: phase, PolicyID: policyID, PolicyDigest: policyDigest, EvaluatedByInstallation: installationID, Reason: reason, CreatedAt: now, UpdatedAt: now}
	err = tx.QueryRow(ctx, `INSERT INTO change_policy_decisions(id,workspace_id,system_name,change_id,change_digest,outcome,phase,policy_id,policy_digest,evaluated_by_installation,reason,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,$12,$12) ON CONFLICT(workspace_id,system_name,change_id,change_digest) DO UPDATE SET outcome=EXCLUDED.outcome,phase=EXCLUDED.phase,policy_id=EXCLUDED.policy_id,policy_digest=EXCLUDED.policy_digest,evaluated_by_installation=EXCLUDED.evaluated_by_installation,reason=EXCLUDED.reason,updated_at=EXCLUDED.updated_at RETURNING `+policyDecisionColumns, decision.ID, workspaceID, systemName, change.ID, change.Digest, outcome, phase, policyID, policyDigest, installationID, reason, now).Scan(policyDecisionScanTargets(&decision)...)
	if err != nil {
		return PolicyDecision{}, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PolicyDecision{}, nil, err
	}
	return decision, matched, nil
}

const policyDecisionColumns = `id,workspace_id,system_name,change_id,change_digest,outcome,phase,COALESCE(policy_id,''),policy_digest,evaluated_by_installation,reason,COALESCE(execution_id,''),failure,created_at,updated_at`

func policyDecisionScanTargets(decision *PolicyDecision) []any {
	return []any{&decision.ID, &decision.WorkspaceID, &decision.System, &decision.ChangeID, &decision.ChangeDigest, &decision.Outcome, &decision.Phase, &decision.PolicyID, &decision.PolicyDigest, &decision.EvaluatedByInstallation, &decision.Reason, &decision.ExecutionID, &decision.Failure, &decision.CreatedAt, &decision.UpdatedAt}
}

func scanPolicyDecision(row pgx.Row) (PolicyDecision, error) {
	var decision PolicyDecision
	err := row.Scan(policyDecisionScanTargets(&decision)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return decision, ErrNotFound
	}
	return decision, err
}

func (s *Store) updatePolicyDecision(ctx context.Context, decisionID, phase, executionID, failure string) (PolicyDecision, error) {
	var decision PolicyDecision
	err := s.pool.QueryRow(ctx, `UPDATE change_policy_decisions SET phase=$1,execution_id=NULLIF($2,''),failure=$3,updated_at=$4 WHERE id=$5 RETURNING `+policyDecisionColumns, phase, executionID, failure, s.now(), decisionID).Scan(policyDecisionScanTargets(&decision)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return decision, ErrNotFound
	}
	return decision, err
}

func mapPolicyStoreError(err error) error {
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}
