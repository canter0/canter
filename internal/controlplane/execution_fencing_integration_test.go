package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

func TestExecutionLeaseReclaimFencesStaleClaimWithSameWorkerID(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	_, workspace, _, err := store.Signup(ctx, "execution-fence@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	change := sdk.Change{ID: "change-fence", System: "api", Phase: "authorized", Digest: "digest", CreatedAt: now, UpdatedAt: now}
	if err := store.RecordChange(ctx, workspace.ID, change); err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueExecution(ctx, workspace.ID, change.System, change.ID, sdk.ActorRef{Kind: "human", ID: "acct_test"})
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.ClaimExecution(ctx, "shared-worker", time.Minute)
	if err != nil || !ok || first.ID != queued.ID || first.ClaimToken == "" {
		t.Fatalf("initial claim: %#v %t %v", first, ok, err)
	}

	now = now.Add(time.Minute + time.Second)
	second, ok, err := store.ClaimExecution(ctx, "shared-worker", time.Minute)
	if err != nil || !ok || second.ID != first.ID || second.ClaimToken == "" || second.ClaimToken == first.ClaimToken {
		t.Fatalf("reclaimed claim did not get a fresh fence: %#v %t %v", second, ok, err)
	}
	if err := store.RenewExecution(ctx, first.ID, "shared-worker", first.ClaimToken, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale claim renewed the reclaimed lease: %v", err)
	}
	if err := store.CompleteExecution(ctx, first.ID, "shared-worker", first.ClaimToken, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale claim completed the reclaimed execution: %v", err)
	}
	if err := store.CompleteExecution(ctx, second.ID, "shared-worker", second.ClaimToken, nil); err != nil {
		t.Fatalf("current claim could not complete: %v", err)
	}
}

func TestExecutionClaimMigrationPreservesLegacyRowsAndHidesFence(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	_, workspace, _, err := store.Signup(ctx, "execution-migration@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change := sdk.Change{ID: "change-migration", System: "api", Phase: "authorized", Digest: "digest", CreatedAt: now, UpdatedAt: now}
	if err := store.RecordChange(ctx, workspace.ID, change); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.EnqueueExecution(ctx, workspace.ID, change.System, change.ID, sdk.ActorRef{Kind: "human", ID: "acct_test"})
	if err != nil {
		t.Fatal(err)
	}

	// Model a queued execution persisted by a version of the schema before
	// migration 032 added claim_token. Ensure the test database is repaired even
	// if an assertion fails after the simulated downgrade.
	if _, err := store.pool.Exec(ctx, `ALTER TABLE executions DROP COLUMN claim_token`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Migrate(context.Background())
	})
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("repeat Migrate after legacy-schema upgrade: %v", err)
		}
	}
	var tokenBeforeClaim string
	if err := store.pool.QueryRow(ctx, `SELECT claim_token FROM executions WHERE id=$1`, legacy.ID).Scan(&tokenBeforeClaim); err != nil {
		t.Fatal(err)
	}
	if tokenBeforeClaim != "" {
		t.Fatalf("legacy row got an unexpected claim token before claim: %q", tokenBeforeClaim)
	}

	claimed, ok, err := store.ClaimExecution(ctx, "migration-worker", time.Minute)
	if err != nil || !ok || claimed.ID != legacy.ID || claimed.ClaimToken == "" {
		t.Fatalf("legacy execution could not be claimed with a new fence: %#v %t %v", claimed, ok, err)
	}
	publicJSON, err := json.Marshal(claimed)
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]any
	if err := json.Unmarshal(publicJSON, &public); err != nil {
		t.Fatal(err)
	}
	if _, exists := public["claimToken"]; exists {
		t.Fatalf("claim token leaked through public execution JSON: %s", publicJSON)
	}
}
