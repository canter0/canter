package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

func TestResourceMeteringFractionalIdempotencyAndLifecycle(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, w, _, err := s.Signup(ctx, "meter@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)
	_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end) VALUES($1,'cus_meter','sub_meter','active',$2,$3)`, w.ID, start, start.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	resources := []sdk.BillingResource{{ID: "compute/real-id", Kind: "compute", Units: 1}, {ID: "storage/owned/", Kind: "storage", Units: 1000000000}}
	record := func(at time.Time) {
		t.Helper()
		if err := s.recordResourceSnapshot(ctx, w.ID, resources, at); err != nil {
			t.Fatal(err)
		}
	}
	sum := func() int64 {
		t.Helper()
		var n int64
		if err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents),0) FROM billing_usage_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	record(start)
	// 144 five-minute intervals = 12h = exactly 5 compute cents, 0.0233 storage cents.
	for i := 1; i <= 144; i++ {
		record(start.Add(time.Duration(i) * 5 * time.Minute))
	}
	if got := sum(); got != 5 {
		t.Fatalf("fractional rounding: got %d want 5", got)
	}
	at := start.Add(12 * time.Hour)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.recordResourceSnapshot(ctx, w.ID, resources, at); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := sum(); got != 5 {
		t.Fatalf("duplicate collection charged: %d", got)
	}
	record(at.Add(6 * time.Hour))
	if got := sum(); got != 5 {
		t.Fatalf("observation outage backfilled: %d", got)
	}
	resources = nil
	record(at.Add(6*time.Hour + time.Minute))
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM billing_resource_samples`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("destroyed resources retained: %d %v", count, err)
	}
	resources = []sdk.BillingResource{{ID: "compute/new-id", Kind: "compute", Units: 100}}
	record(at.Add(6*time.Hour + 2*time.Minute))
	if sum() != 5 {
		t.Fatal("new resource retroactively billed")
	}
	// A renewal resets fractional credit and never crosses into the old period.
	boundary := at.Add(6*time.Hour + 3*time.Minute)
	_, err = s.pool.Exec(ctx, `UPDATE workspace_billing SET period_start=$2,period_end=$3 WHERE workspace_id=$1`, w.ID, boundary, boundary.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	record(boundary)
	if sum() != 5 {
		t.Fatal("period crossed")
	}
	_, err = s.pool.Exec(ctx, `UPDATE workspace_billing SET status='canceled' WHERE workspace_id=$1`, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.recordResourceSnapshot(ctx, w.ID, resources, boundary.Add(time.Minute)); err == nil {
		t.Fatal("canceled subscription charged")
	}
	_, err = s.pool.Exec(ctx, `UPDATE workspace_billing SET status='past_due' WHERE workspace_id=$1`, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.recordResourceSnapshot(ctx, w.ID, resources, boundary.Add(time.Minute)); err == nil {
		t.Fatal("failed-payment subscription charged")
	}
}

func TestResourceStorageRateAndOutbox(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, w, _, err := s.Signup(ctx, "storage@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	_, err = s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end) VALUES($1,'cus_test','sub_test','active',$2,$3)`, w.ID, start, start.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	// 43200 GB for 5 minutes = exactly 7 cents at $0.014/GB/720h.
	resources := []sdk.BillingResource{{ID: "storage/owned/", Kind: "storage", Units: 43200 * 1000000000}}
	for _, at := range []time.Time{start, start.Add(5 * time.Minute)} {
		if err = s.recordResourceSnapshot(ctx, w.ID, resources, at); err != nil {
			t.Fatal(err)
		}
	}
	var amount int64
	if err = s.pool.QueryRow(ctx, `SELECT SUM(amount_cents) FROM billing_usage_events`).Scan(&amount); err != nil || amount != 7 {
		t.Fatalf("rate: %d %v", amount, err)
	}
	gateway, fixture := billingTestGateway(t)
	fixture.expectedMeterValue = "7"
	if err = gateway.DispatchUsage(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = gateway.DispatchUsage(ctx, s); err != nil {
		t.Fatal(err)
	}
	if fixture.meterCalls != 1 {
		t.Fatalf("meter submissions %d", fixture.meterCalls)
	}
}
