package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

const resourceRateVersion = "resources-2026-09-18-v1"

type BillingResourceSource interface {
	BillingResources(context.Context, sdk.System) ([]sdk.BillingResource, error)
}

// CollectUsage samples actual allocations. Only adjacent successful observations
// within five minutes are charged. Missing evidence is absorbed by Canter, never
// backfilled. The entire workspace snapshot and monetary outbox commit together.
func (s *Store) CollectUsage(ctx context.Context, source BillingResourceSource) error {
	rows, err := s.pool.Query(ctx, `SELECT workspace_id FROM workspace_billing WHERE subscription_id IS NOT NULL ORDER BY workspace_id`)
	if err != nil {
		return err
	}
	var workspaces []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		workspaces = append(workspaces, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var first error
	for _, workspace := range workspaces {
		err = s.collectWorkspaceUsage(ctx, source, workspace)
		if err != nil {
			// Provider errors can contain account details; persist a safe action only.
			_, _ = s.pool.Exec(ctx, `INSERT INTO billing_collection_status(workspace_id,attempted_at,issue) VALUES($1,now(),'Resource metering needs attention; unverified time is not charged.') ON CONFLICT(workspace_id) DO UPDATE SET attempted_at=now(),issue=EXCLUDED.issue`, workspace)
			if first == nil {
				first = fmt.Errorf("resource metering failed for workspace %s", workspace)
			}
		}
	}
	return first
}

func (s *Store) collectWorkspaceUsage(ctx context.Context, source BillingResourceSource, workspace string) error {
	// A session advisory lock spans provider reads, preventing stale concurrent
	// observations from overwriting a newer snapshot without holding a DB tx open.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,918))`, workspace).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,918))`, workspace)
	var status string
	if err = conn.QueryRow(ctx, `SELECT status FROM workspace_billing WHERE workspace_id=$1`, workspace).Scan(&status); err != nil {
		return err
	}
	if status != "active" {
		_, err = conn.Exec(ctx, `DELETE FROM billing_resource_samples WHERE workspace_id=$1`, workspace)
		if err != nil {
			return err
		}
		_, err = conn.Exec(ctx, `INSERT INTO billing_collection_status(workspace_id,attempted_at,issue) VALUES($1,now(),'Billing inactive: new paid provisioning is blocked. Existing resources and data are retained for owner review; inactive time is not charged.') ON CONFLICT(workspace_id) DO UPDATE SET attempted_at=now(),issue=EXCLUDED.issue`, workspace)
		return err
	}
	systems, err := s.ListSystems(ctx, workspace)
	if err != nil {
		return err
	}
	resources := []sdk.BillingResource{}
	seen := map[string]bool{}
	for _, system := range systems {
		if err = validateCanonicalSystemForWorkspace(workspace, system.Contract); err != nil {
			return err
		}
		items, e := source.BillingResources(ctx, system.Contract)
		if e != nil {
			return e
		}
		for _, r := range items {
			if r.ID == "" || seen[r.ID] || r.Units < 0 || r.Units > 1<<50 || (r.Kind != "compute" && r.Kind != "storage") {
				return fmt.Errorf("invalid metering snapshot")
			}
			seen[r.ID] = true
			resources = append(resources, r)
		}
	}
	return s.recordResourceSnapshot(ctx, workspace, resources, s.now().UTC().Truncate(time.Second))
}

func (s *Store) recordResourceSnapshot(ctx context.Context, workspace string, resources []sdk.BillingResource, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var customer, subscription, status string
	var start, end *time.Time
	err = tx.QueryRow(ctx, `SELECT COALESCE(customer_id,''),COALESCE(subscription_id,''),status,period_start,period_end FROM workspace_billing WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&customer, &subscription, &status, &start, &end)
	if err != nil {
		return err
	}
	if status != "active" || customer == "" || subscription == "" || start == nil || end == nil || at.Before(*start) || !at.Before(*end) {
		return fmt.Errorf("billing period is not active")
	}
	ids := []string{}
	for _, r := range resources {
		ids = append(ids, r.ID)
		var oldSub string
		var oldPeriod, previous time.Time
		var units int64
		err = tx.QueryRow(ctx, `SELECT subscription_id,period_start,observed_at,units FROM billing_resource_samples WHERE workspace_id=$1 AND resource_id=$2`, workspace, r.ID).Scan(&oldSub, &oldPeriod, &previous, &units)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == nil && !at.After(previous) {
			continue
		}
		samePeriod := err == nil && oldSub == subscription && oldPeriod.Equal(*start)
		if samePeriod && at.Sub(previous) <= 5*time.Minute && !previous.Before(*start) {
			// SQL numeric arithmetic retains exact fractional cents, including bytes.
			numerator, denominator := pricing.ComputeCentsPerUnitPer720Hours, int64(pricing.HoursPerResourceMonth*3600)
			if r.Kind == "storage" {
				numerator = 14
				denominator = 25920000000000000
			} // 1.4 cents per decimal GB / 720h
			seconds := int64(at.Sub(previous) / time.Second)
			var cents int64
			err = tx.QueryRow(ctx, `UPDATE billing_resource_samples SET remainder=remainder+($3::numeric*$4::numeric*$5::numeric) WHERE workspace_id=$1 AND resource_id=$2 RETURNING floor(remainder/$6::numeric)::bigint`, workspace, r.ID, min(units, r.Units), seconds, numerator, denominator).Scan(&cents)
			if err != nil {
				return err
			}
			if cents > 0 {
				digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%d/%d", workspace, r.ID, subscription, previous.Unix(), at.Unix())))
				id := fmt.Sprintf("resource_%x", digest)
				_, err = tx.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, workspace, customer, subscription, cents, r.ID, resourceRateVersion, at.Add(-time.Second))
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE billing_resource_samples SET remainder=remainder-($3::numeric*$4::numeric) WHERE workspace_id=$1 AND resource_id=$2`, workspace, r.ID, cents, denominator)
				if err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO billing_resource_samples(workspace_id,resource_id,subscription_id,period_start,observed_at,units) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,resource_id) DO UPDATE SET subscription_id=EXCLUDED.subscription_id,period_start=EXCLUDED.period_start,observed_at=EXCLUDED.observed_at,units=EXCLUDED.units,remainder=CASE WHEN $7 THEN billing_resource_samples.remainder ELSE 0 END`, workspace, r.ID, subscription, *start, at, r.Units, samePeriod)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM billing_resource_samples WHERE workspace_id=$1 AND NOT(resource_id=ANY($2::text[]))`, workspace, ids)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_collection_status(workspace_id,attempted_at,succeeded_at) VALUES($1,$2,$2) ON CONFLICT(workspace_id) DO UPDATE SET attempted_at=$2,succeeded_at=$2,issue=''`, workspace, at)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
