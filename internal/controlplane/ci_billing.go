package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/canter0/canter/pricing"
	"github.com/jackc/pgx/v5"
)

const ciMaxRuntimeSeconds int64 = 6 * 3600

type ciBudgetQuote struct {
	StartedAt          time.Time
	DeadlineAt         time.Time
	MaxElapsedSeconds  int64
	PaidOverrunEnabled bool
	MaxChargeUnits     int64
}

type ciBillingRunner struct {
	SizeID, RateVersion, CustomerID, SubscriptionID string
	RateMicros, MeteredSeconds                      int64
	Paid                                            bool
	CreatedAt                                       time.Time
	StartedAt, StoppedAt, DeadlineAt                *time.Time
}

type ciBillingAccount struct {
	CustomerID, SubscriptionID, Status string
	Start, End                         *time.Time
}

// SetCIRateV2Enabled controls new admission only. Stored contracts can
// always be quoted and settled after a compatible deployment.
func (s *Store) SetCIRateV2Enabled(enabled bool) {
	s.ciRateV2Enabled.Store(enabled)
}

func (s *Store) ciAdmissionRateVersion(workspace string) string {
	if s.ciRateV2Enabled.Load() && s.inFeatureCanary(workspace) {
		return pricing.CIRateVersion
	}
	return pricing.CIRateVersionV1
}

func (s *Store) lockCIBilling(ctx context.Context, tx pgx.Tx, workspace, runner string) (ciBillingRunner, ciBillingAccount, error) {
	var locked string
	err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspace).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ciBillingRunner{}, ciBillingAccount{}, ErrNotFound
	}
	if err != nil {
		return ciBillingRunner{}, ciBillingAccount{}, err
	}
	var r ciBillingRunner
	err = tx.QueryRow(ctx, `SELECT size_id,rate_version,rate_micros_per_minute,paid_overrun_enabled,billing_customer_id,billing_subscription_id,execution_started_at,execution_stopped_at,deadline_at,metered_billable_seconds,created_at FROM ci_runners WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, workspace, runner).Scan(&r.SizeID, &r.RateVersion, &r.RateMicros, &r.Paid, &r.CustomerID, &r.SubscriptionID, &r.StartedAt, &r.StoppedAt, &r.DeadlineAt, &r.MeteredSeconds, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ciBillingAccount{}, ErrNotFound
	}
	if err != nil {
		return r, ciBillingAccount{}, err
	}
	size, ok := pricing.FindCIContract(r.RateVersion, r.SizeID)
	if !ok || r.RateMicros != size.MicrosPerMinute {
		return r, ciBillingAccount{}, fmt.Errorf("%w: unsupported CI billing contract", ErrConflict)
	}
	var account ciBillingAccount
	err = tx.QueryRow(ctx, `SELECT COALESCE(customer_id,''),COALESCE(subscription_id,''),status,period_start,period_end FROM workspace_billing WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&account.CustomerID, &account.SubscriptionID, &account.Status, &account.Start, &account.End)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, account, nil
	}
	return r, account, err
}

func (a ciBillingAccount) activeAt(at time.Time) bool {
	return a.Status == "active" && a.CustomerID != "" && a.SubscriptionID != "" && a.Start != nil && a.End != nil && !at.Before(*a.Start) && at.Before(*a.End)
}

func ciQuote(r ciBillingRunner) (ciBudgetQuote, error) {
	if r.StartedAt == nil || r.DeadlineAt == nil || r.DeadlineAt.Before(*r.StartedAt) {
		return ciBudgetQuote{}, ErrConflict
	}
	seconds := int64(r.DeadlineAt.Sub(*r.StartedAt) / time.Second)
	if seconds < 1 || seconds > ciMaxRuntimeSeconds || (!r.Paid && seconds > pricing.CIFreeSeconds) {
		return ciBudgetQuote{}, ErrConflict
	}
	usage, err := pricing.CalculateCIForVersion(r.RateVersion, r.SizeID, seconds)
	if err != nil {
		return ciBudgetQuote{}, err
	}
	return ciBudgetQuote{StartedAt: r.StartedAt.UTC(), DeadlineAt: r.DeadlineAt.UTC(), MaxElapsedSeconds: seconds, PaidOverrunEnabled: r.Paid, MaxChargeUnits: usage.ChargeUnits}, nil
}

func (s *Store) reserveCIBudget(ctx context.Context, workspace, runner string, startedAt time.Time, maximumSeconds int64) (ciBudgetQuote, error) {
	startedAt = startedAt.UTC().Truncate(time.Microsecond)
	if startedAt.IsZero() || startedAt.After(s.now().Add(time.Minute)) || maximumSeconds < 1 || maximumSeconds > ciMaxRuntimeSeconds {
		return ciBudgetQuote{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ciBudgetQuote{}, err
	}
	defer tx.Rollback(ctx)
	quote, err := s.reserveCIBudgetTx(ctx, tx, workspace, runner, startedAt, maximumSeconds)
	if err != nil {
		return ciBudgetQuote{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ciBudgetQuote{}, err
	}
	return quote, nil
}

// reserveCIBudgetTx leaves commit ownership with the caller so actual job
// adoption and its immutable execution clock can be persisted atomically.
func (s *Store) reserveCIBudgetTx(ctx context.Context, tx pgx.Tx, workspace, runner string, startedAt time.Time, maximumSeconds int64) (ciBudgetQuote, error) {
	startedAt = startedAt.UTC().Truncate(time.Microsecond)
	if startedAt.IsZero() || startedAt.After(s.now().Add(time.Minute)) || maximumSeconds < 1 || maximumSeconds > ciMaxRuntimeSeconds {
		return ciBudgetQuote{}, ErrConflict
	}
	r, account, err := s.lockCIBilling(ctx, tx, workspace, runner)
	if err != nil {
		return ciBudgetQuote{}, err
	}
	if startedAt.Before(r.CreatedAt.Add(-time.Minute)) {
		return ciBudgetQuote{}, ErrConflict
	}
	if r.StartedAt != nil {
		if !r.StartedAt.Equal(startedAt) {
			return ciBudgetQuote{}, fmt.Errorf("%w: a runner's execution clock cannot be reset", ErrConflict)
		}
		return ciQuote(r)
	}
	var enabled bool
	var limit int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(c.paid_overrun_enabled,false),COALESCE(c.monthly_spend_limit_cents,1000) FROM workspaces w LEFT JOIN ci_settings c ON c.workspace_id=w.id WHERE w.id=$1`, workspace).Scan(&enabled, &limit)
	if err != nil {
		return ciBudgetQuote{}, err
	}
	paid := r.Paid && enabled && account.activeAt(s.now())
	if !paid {
		maximumSeconds = min(maximumSeconds, pricing.CIFreeSeconds)
	} else {
		segments, err := pricing.CIChargeSegmentsForVersion(r.RateVersion, r.SizeID, startedAt, 0, maximumSeconds)
		if err != nil {
			return ciBudgetQuote{}, err
		}
		for _, segment := range segments {
			var spent, reserved int64
			err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT SUM(delta_units) FROM ci_usage_ledger WHERE workspace_id=$1 AND period_start=$2),0),COALESCE((SELECT SUM(reserved_units) FROM ci_budget_reservations WHERE workspace_id=$1 AND period_start=$2),0)`, workspace, segment.PeriodStart).Scan(&spent, &reserved)
			if err != nil {
				return ciBudgetQuote{}, err
			}
			remaining := max(int64(0), limit*pricing.CIChargeUnitsPerCent-spent-reserved)
			seconds := min(segment.ThroughBillableSeconds-segment.FromBillableSeconds, remaining/r.RateMicros)
			if seconds < segment.ThroughBillableSeconds-segment.FromBillableSeconds {
				maximumSeconds = pricing.CIFreeSeconds + segment.FromBillableSeconds + seconds
				break
			}
		}
	}
	paid = paid && maximumSeconds > pricing.CIFreeSeconds
	segments, err := pricing.CIChargeSegmentsForVersion(r.RateVersion, r.SizeID, startedAt, 0, maximumSeconds)
	if err != nil {
		return ciBudgetQuote{}, err
	}
	for _, segment := range segments {
		if _, err = tx.Exec(ctx, `INSERT INTO ci_budget_reservations(workspace_id,runner_id,period_start,reserved_units) VALUES($1,$2,$3,$4)`, workspace, runner, segment.PeriodStart, segment.ChargeUnits); err != nil {
			return ciBudgetQuote{}, err
		}
	}
	customer, subscription := "", ""
	if paid {
		customer, subscription = account.CustomerID, account.SubscriptionID
	}
	deadline := startedAt.Add(time.Duration(maximumSeconds) * time.Second)
	_, err = tx.Exec(ctx, `UPDATE ci_runners SET execution_started_at=$3,deadline_at=$4,paid_overrun_enabled=$5,billing_customer_id=$6,billing_subscription_id=$7,updated_at=$8 WHERE workspace_id=$1 AND id=$2`, workspace, runner, startedAt, deadline, paid, customer, subscription, s.now())
	if err != nil {
		return ciBudgetQuote{}, err
	}
	r.StartedAt, r.DeadlineAt, r.Paid = &startedAt, &deadline, paid
	return ciQuote(r)
}

func (s *Store) checkCISpendLimit(ctx context.Context, tx pgx.Tx, workspace string, limitCents int64) error {
	if limitCents < 1 || limitCents > 100000 {
		return ErrConflict
	}
	now := s.now().UTC()
	period := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var exceeded bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT period_start,delta_units AS units FROM ci_usage_ledger WHERE workspace_id=$1 UNION ALL SELECT period_start,reserved_units AS units FROM ci_budget_reservations WHERE workspace_id=$1) amounts WHERE period_start>=$2 GROUP BY period_start HAVING SUM(units)>$3)`, workspace, period, limitCents*pricing.CIChargeUnitsPerCent).Scan(&exceeded)
	if err != nil {
		return err
	}
	if exceeded {
		return fmt.Errorf("%w: the spend limit is below usage or existing runner commitments; cancel those runs before lowering it", ErrConflict)
	}
	return nil
}

func (s *Store) settleCIBudget(ctx context.Context, workspace, runner string, stoppedAt time.Time) error {
	stoppedAt = stoppedAt.UTC().Truncate(time.Microsecond)
	if stoppedAt.IsZero() || stoppedAt.After(s.now().Add(time.Minute)) {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, account, err := s.lockCIBilling(ctx, tx, workspace, runner)
	if err != nil {
		return err
	}
	if r.StoppedAt != nil {
		if r.StoppedAt.Equal(stoppedAt) {
			return nil
		}
		return fmt.Errorf("%w: settled CI runtime cannot be changed", ErrConflict)
	}
	if r.StartedAt == nil || r.DeadlineAt == nil || stoppedAt.Before(*r.StartedAt) || r.MeteredSeconds != 0 {
		return ErrConflict
	}
	if _, err = ciQuote(r); err != nil {
		return err
	}
	billStop := stoppedAt
	if billStop.After(*r.DeadlineAt) {
		billStop = *r.DeadlineAt
	}
	elapsed := int64(billStop.Sub(*r.StartedAt) / time.Second)
	billable := int64(0)
	if r.Paid {
		billable = max(int64(0), elapsed-pricing.CIFreeSeconds)
	}
	if billable > 0 {
		if !account.activeAt(stoppedAt) || account.CustomerID != r.CustomerID || account.SubscriptionID != r.SubscriptionID {
			return fmt.Errorf("%w: CI billing needs reconciliation before this run can be settled", ErrConflict)
		}
		segments, err := pricing.CIChargeSegmentsForVersion(r.RateVersion, r.SizeID, *r.StartedAt, 0, elapsed)
		if err != nil {
			return err
		}
		var total int64
		for _, segment := range segments {
			var reserved int64
			err = tx.QueryRow(ctx, `SELECT reserved_units FROM ci_budget_reservations WHERE workspace_id=$1 AND runner_id=$2 AND period_start=$3 FOR UPDATE`, workspace, runner, segment.PeriodStart).Scan(&reserved)
			if err != nil {
				return err
			}
			if reserved < segment.ChargeUnits {
				return fmt.Errorf("%w: CI runtime exceeded its authorized budget", ErrCapacity)
			}
			_, err = tx.Exec(ctx, `INSERT INTO ci_usage_ledger(workspace_id,runner_id,through_seconds,period_start,delta_units,rate_version,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, workspace, runner, segment.ThroughBillableSeconds, segment.PeriodStart, segment.ChargeUnits, r.RateVersion, stoppedAt)
			if err != nil {
				return err
			}
			total += segment.ChargeUnits
		}
		var previous int64
		var previousSubscription string
		err = tx.QueryRow(ctx, `SELECT subscription_id,remainder_units FROM ci_usage_balances WHERE workspace_id=$1 FOR UPDATE`, workspace).Scan(&previousSubscription, &previous)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if previousSubscription != "" && previousSubscription != account.SubscriptionID && previous != 0 {
			return fmt.Errorf("%w: an earlier CI subscription balance needs reconciliation", ErrConflict)
		}
		cents, remainder, err := pricing.CIChargeCents(previous + total)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO ci_usage_balances(workspace_id,subscription_id,remainder_units,updated_at) VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id) DO UPDATE SET subscription_id=EXCLUDED.subscription_id,remainder_units=EXCLUDED.remainder_units,updated_at=EXCLUDED.updated_at`, workspace, account.SubscriptionID, remainder, s.now())
		if err != nil {
			return err
		}
		if cents > 0 {
			digest := sha256.Sum256([]byte(workspace + "/" + runner))
			eventID := fmt.Sprintf("ci_%x", digest)
			_, err = tx.Exec(ctx, `INSERT INTO billing_usage_events(id,workspace_id,customer_id,subscription_id,amount_cents,resource,rate_version,occurred_at) VALUES($1,$2,$3,$4,$5,'ci',$6,$7)`, eventID, workspace, account.CustomerID, account.SubscriptionID, cents, r.RateVersion, stoppedAt)
			if err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM ci_budget_reservations WHERE workspace_id=$1 AND runner_id=$2`, workspace, runner); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE ci_runners SET execution_stopped_at=$3,metered_billable_seconds=$4,updated_at=$5 WHERE workspace_id=$1 AND id=$2`, workspace, runner, stoppedAt, billable, s.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
