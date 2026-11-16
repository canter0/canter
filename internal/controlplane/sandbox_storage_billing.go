package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

// Existing M1 tariff: 1.4 cents per decimal GB per 720 hours. Keep byte-second
// arithmetic in PostgreSQL numeric, including fractions smaller than one cent.
const storageCentsNumerator int64 = 14
const storageByteSecondsDenominator int64 = 25920000000000000
const sandboxStorageMeterPrefix = "sandbox-storage/"
const sandboxStorageRoundingID = sandboxStorageMeterPrefix + "rounding"

// sandboxStorageResources observes each immutable stored copy once. A free
// machine's current save is included even when templates/forks also refer to
// it. Failed uploads, abandoned saves and cleanup leftovers are not billable.
// The UNION observes legacy/v2 publication in one database snapshot.
func (s *Store) sandboxStorageResources(ctx context.Context, workspace string) ([]sdk.BillingResource, error) {
	rows, err := s.pool.Query(ctx, `SELECT 'snapshot/'||p.id,
 CASE WHEN EXISTS(
  SELECT 1 FROM sandboxes b WHERE b.workspace_id=p.workspace_id AND b.current_snapshot_id=p.id
   AND b.tier='free' AND b.phase NOT IN ('deleting','deleted')
 ) THEN 0 ELSE p.stored_bytes END, NULL::jsonb
FROM sandbox_snapshots p
WHERE p.workspace_id=$1 AND p.state='ready' AND (p.retained OR `+sandboxSnapshotHasReferences+`)
UNION ALL
SELECT 'legacy/'||b.id,0::bigint,NULLIF(b.checkpoint_object_ref,'')::jsonb FROM sandboxes b
WHERE b.workspace_id=$1 AND b.tier='paid' AND b.phase NOT IN ('deleting','deleted')
 AND COALESCE(NULLIF(b.checkpoint_object_ref,'')::jsonb->>'key','')<>''`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resources := []sdk.BillingResource{}
	for rows.Next() {
		var id string
		var units int64
		var legacy []byte
		if err := rows.Scan(&id, &units, &legacy); err != nil {
			return nil, err
		}
		if len(legacy) > 0 {
			var object sdk.ComputerCheckpointObject
			if err := json.Unmarshal(legacy, &object); err != nil {
				return nil, err
			}
			scope := (Sandbox{WorkspaceID: workspace, ID: strings.TrimPrefix(id, "legacy/")}).checkpointScope()
			if err := sdk.ValidateComputerCheckpointObject(scope, object); err != nil {
				return nil, err
			}
			units = object.Size
		}
		if units < 0 || units > 1<<50 {
			return nil, fmt.Errorf("invalid sandbox storage observation")
		}
		resources = append(resources, sdk.BillingResource{ID: sandboxStorageMeterPrefix + id, Kind: "storage", Units: units})
	}
	return resources, rows.Err()
}

// poolSandboxStorageRemainders preserves fractional cents when a small save is
// replaced or deleted. Samples remain per copy: pooling byte counts instead
// would incorrectly treat different short-lived copies as continuous storage.
// Only the monetary remainder is shared, within one org/subscription/period.
// The caller holds the workspace_billing row lock and commits the outbox with
// these updates. Existing compute and other M1 storage rounding is unchanged.
func (s *Store) poolSandboxStorageRemainders(ctx context.Context, tx pgx.Tx, workspace, customer, subscription string, start, at time.Time) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_resource_samples WHERE workspace_id=$1 AND starts_with(resource_id,$2))`, workspace, sandboxStorageMeterPrefix).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_resource_samples(workspace_id,resource_id,subscription_id,period_start,observed_at,units)
VALUES($1,$2,$3,$4,$5,0)
ON CONFLICT(workspace_id,resource_id) DO UPDATE SET
 subscription_id=EXCLUDED.subscription_id,period_start=EXCLUDED.period_start,
 observed_at=GREATEST(billing_resource_samples.observed_at,EXCLUDED.observed_at),
 remainder=CASE WHEN billing_resource_samples.subscription_id=EXCLUDED.subscription_id
  AND billing_resource_samples.period_start=EXCLUDED.period_start
 THEN billing_resource_samples.remainder ELSE 0 END`, workspace, sandboxStorageRoundingID, subscription, start, at); err != nil {
		return false, err
	}
	var cents int64
	if err := tx.QueryRow(ctx, `UPDATE billing_resource_samples p SET remainder=(
 SELECT COALESCE(sum(b.remainder),0) FROM billing_resource_samples b
 WHERE b.workspace_id=$1 AND starts_with(b.resource_id,$2)
  AND b.subscription_id=$3 AND b.period_start=$4
) WHERE p.workspace_id=$1 AND p.resource_id=$5
RETURNING floor(p.remainder/$6::numeric)::bigint`,
		workspace, sandboxStorageMeterPrefix, subscription, start, sandboxStorageRoundingID, storageByteSecondsDenominator).Scan(&cents); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_resource_samples SET remainder=0
WHERE workspace_id=$1 AND starts_with(resource_id,$2) AND resource_id<>$3
 AND subscription_id=$4 AND period_start=$5`, workspace, sandboxStorageMeterPrefix, sandboxStorageRoundingID, subscription, start); err != nil {
		return false, err
	}
	if cents > 0 {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%s", workspace, sandboxStorageRoundingID, subscription, at.UTC().Format(time.RFC3339Nano))))
		if _, err := tx.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, fmt.Sprintf("resource_%x", digest), workspace, customer, subscription, cents, sandboxStorageRoundingID, resourceRateVersion, at.Add(-time.Second)); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_resource_samples SET remainder=remainder-($3::numeric*$4::numeric) WHERE workspace_id=$1 AND resource_id=$2`,
			workspace, sandboxStorageRoundingID, cents, storageByteSecondsDenominator); err != nil {
			return false, err
		}
	}
	return true, nil
}
