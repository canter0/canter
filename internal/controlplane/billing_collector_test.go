package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

func TestSandboxHistoryBudgetUsesUniqueStoredBytes(t *testing.T) {
	f, source, _, _ := snapshotStoreFixture(t)
	ctx := context.Background()
	saved, err := f.h.captureSandboxSnapshot(ctx, source, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic verified-byte observations exercise monetary boundaries without
	// allocating gigabytes in the test. The archive reader is not used here.
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET stored_bytes=5000000000,retained=true WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE workspace_usage_caps SET limit_cents=20,reserved_cents=2,spent_cents=3 WHERE workspace_id=$1`, source.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	change := func(fn func(pgx.Tx) error) error {
		tx, err := f.s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, source.WorkspaceID); err != nil {
			return err
		}
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		if err := f.s.reserveSandboxHistoryTx(ctx, tx, source.WorkspaceID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	assertReserved := func(want int) {
		t.Helper()
		var got int
		if err := f.s.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, source.WorkspaceID).Scan(&got); err != nil || got != want {
			t.Fatalf("reserved %d, want %d (%v)", got, want, err)
		}
	}
	for range 2 {
		if err := change(nil); err != nil {
			t.Fatal(err)
		}
		assertReserved(9) // other reservation 2 + 5GB * 1.4 cents = 7
	}
	if err := change(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name) VALUES('tpl_budget',$1,'tools')`, source.WorkspaceID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id) VALUES($1,'tpl_budget',1,$2),($1,'tpl_budget',2,$2)`, source.WorkspaceID, saved.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertReserved(9) // a pin + two template versions still share one copy
	err = change(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sandbox_snapshots SET stored_bytes=12000000000 WHERE id=$1`, saved.ID)
		return err
	})
	if !errors.Is(err, ErrCapacity) {
		t.Fatal("12GB needs 17 cents; 2 reserved + 3 spent leaves only 15", err)
	}
	assertReserved(9)
	current, err := f.s.SandboxSnapshot(ctx, source.WorkspaceID, saved.ID)
	if err != nil || current.StoredBytes != 5_000_000_000 {
		t.Fatal("capacity rejection did not roll back the reference transaction", err)
	}
	if err := change(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE sandbox_snapshots SET retained=false WHERE id=$1`, saved.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sandbox_templates SET deleted_at=now() WHERE id='tpl_budget'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertReserved(2) // dropping the last extra reference releases only its hold
}

func TestSandboxStorageMeterCountsCopiesAndFreeCurrentSave(t *testing.T) {
	f, source, _, engine := snapshotStoreFixture(t)
	ctx := context.Background()
	saved, err := f.h.captureSandboxSnapshot(ctx, source, &snapshotStoreSource{data: snapshotStoreArchive(t, 1024)}, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := sandboxStorageMeterPrefix + "snapshot/" + saved.ID
	assertResources := func(want map[string]int64) {
		t.Helper()
		resources, err := f.s.sandboxStorageResources(ctx, source.WorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		if len(resources) != len(want) {
			t.Fatalf("observed %v, want %v", resources, want)
		}
		for _, resource := range resources {
			units, ok := want[resource.ID]
			if !ok || resource.Kind != "storage" || resource.Units != units {
				t.Fatalf("unexpected storage observation: %+v", resource)
			}
		}
	}
	assertResources(map[string]int64{snapshotID: 0})
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,status) VALUES($1,'active') ON CONFLICT(workspace_id) DO UPDATE SET status='active'`, source.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	paid := f.create(t, "meter-fork", "s-1x2", 202)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET current_snapshot_id=$2 WHERE id=$1`, paid.ID, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_templates(id,workspace_id,name) VALUES('tpl_meter',$1,'tools')`, source.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `INSERT INTO sandbox_template_versions(workspace_id,template_id,version,snapshot_id) VALUES($1,'tpl_meter',1,$2),($1,'tpl_meter',2,$2)`, source.WorkspaceID, saved.ID); err != nil {
		t.Fatal(err)
	}
	// Several references still occupy one stored copy, included by the free
	// machine's current save.
	assertResources(map[string]int64{snapshotID: 0})
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleting' WHERE id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	assertResources(map[string]int64{snapshotID: saved.StoredBytes})
	foreign, err := f.s.sandboxStorageResources(ctx, "another-workspace")
	if err != nil || len(foreign) != 0 {
		t.Fatal("storage observation crossed organization boundary", foreign, err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='deleted' WHERE id=$1`, paid.ID); err != nil {
		t.Fatal(err)
	}
	assertResources(map[string]int64{snapshotID: saved.StoredBytes}) // templates
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=true WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_templates SET deleted_at=now() WHERE id='tpl_meter'`); err != nil {
		t.Fatal(err)
	}
	assertResources(map[string]int64{snapshotID: saved.StoredBytes}) // kept history
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=false WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	assertResources(map[string]int64{}) // unreferenced cleanup is not charged
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandbox_snapshots SET retained=true,state='failed' WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	assertResources(map[string]int64{})
	legacy, err := engine.PutComputerCheckpoint(ctx, paid.checkpointScope(), []byte("opaque legacy ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(legacy)
	if _, err := f.s.pool.Exec(ctx, `UPDATE sandboxes SET phase='asleep',current_snapshot_id=NULL,checkpoint_object_ref=$2 WHERE id=$1`, paid.ID, raw); err != nil {
		t.Fatal(err)
	}
	assertResources(map[string]int64{sandboxStorageMeterPrefix + "legacy/" + paid.ID: legacy.Size})
}

func TestSandboxStorageFractionsSurviveSnapshotChurn(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, workspace, _, err := s.Signup(ctx, "snapshot-meter@example.test", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_billing(workspace_id,customer_id,subscription_id,status,period_start,period_end) VALUES($1,'cus_snapshot','sub_snapshot','active',$2,$3)`, workspace.ID, start, start.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Each copy is observed for five minutes and replaced after a five-minute
	// unobserved gap. None individually earns one cent. Across 24 groups:
	// 300 decimal GB * 2 observed hours * 1.4 cents/GB/720h = 7/6 cents.
	// The invoice receives 1 cent; 1/6 cent (4.32e15 numerator) remains.
	for group := 0; group < 24; group++ {
		resources := []sdk.BillingResource{}
		for copy := 0; copy < 3; copy++ {
			resources = append(resources, sdk.BillingResource{
				ID:   fmt.Sprintf("%ssnapshot/%d-%d", sandboxStorageMeterPrefix, group, copy),
				Kind: "storage", Units: 100_000_000_000,
			})
		}
		at := start.Add(time.Duration(group) * 10 * time.Minute)
		for _, observation := range []time.Time{at, at.Add(5 * time.Minute), at.Add(5 * time.Minute)} {
			if err := s.recordResourceSnapshot(ctx, workspace.ID, resources, observation); err != nil {
				t.Fatal(err)
			}
		}
	}
	var cents, remainder int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_cents),0) FROM billing_usage_events WHERE workspace_id=$1`, workspace.ID).Scan(&cents); err != nil || cents != 1 {
		t.Fatalf("snapshot churn charged %d cents, want 1 (%v)", cents, err)
	}
	// Deleting every saved copy must not throw away already-earned fractions.
	if err := s.recordResourceSnapshot(ctx, workspace.ID, nil, start.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT remainder::bigint FROM billing_resource_samples WHERE workspace_id=$1 AND resource_id=$2`, workspace.ID, sandboxStorageRoundingID).Scan(&remainder); err != nil || remainder != 4_320_000_000_000_000 {
		t.Fatalf("pooled fraction: %d (%v)", remainder, err)
	}
	var samples int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM billing_resource_samples WHERE workspace_id=$1`, workspace.ID).Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("only rounding state should survive: %d (%v)", samples, err)
	}
	boundary := start.Add(30 * 24 * time.Hour)
	if _, err := s.pool.Exec(ctx, `UPDATE workspace_billing SET period_start=$2,period_end=$3 WHERE workspace_id=$1`, workspace.ID, boundary, boundary.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.recordResourceSnapshot(ctx, workspace.ID, nil, boundary); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT remainder::bigint FROM billing_resource_samples WHERE workspace_id=$1 AND resource_id=$2`, workspace.ID, sandboxStorageRoundingID).Scan(&remainder); err != nil || remainder != 0 {
		t.Fatalf("fraction crossed billing period: %d (%v)", remainder, err)
	}
}

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
