package controlplane

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("CANTER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CANTER_TEST_DATABASE_URL is not set")
	}
	store, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(context.Background(), `TRUNCATE change_approval_capabilities,audit_events,change_records,executions,systems,agent_sessions,agent_credentials,device_authorizations,agent_installations,human_sessions,memberships,workspaces,accounts,beta_invites CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestConcurrentRefreshReplayRevokesCredentialFamily(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "refresh-race@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.BeginDeviceAuthorization(ctx, "Concurrent agent", "test", Authority{Inspect: true, Draft: true}, "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "initial")
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		pair TokenPair
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			ready.Done()
			<-start
			refreshed, refreshErr := store.RefreshAgent(ctx, pair.RefreshToken, "racing-client")
			results <- result{pair: refreshed, err: refreshErr}
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	close(results)

	var succeeded TokenPair
	successes := 0
	for _, got := range []result{first, second} {
		if got.err == nil {
			successes++
			succeeded = got.pair
		} else if !errors.Is(got.err, ErrUnauthorized) {
			t.Fatalf("concurrent refresh returned unexpected error: %v", got.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent refresh successes=%d, want exactly one", successes)
	}
	if _, err := store.ResolveAgent(ctx, succeeded.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay loser did not revoke the winning child session: %v", err)
	}
	if _, err := store.RefreshAgent(ctx, succeeded.RefreshToken, "after-race"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay loser did not revoke the winning child refresh token: %v", err)
	}
}

func TestWorkspaceUsageCapReservesOneInitialHostAndReusesRetry(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	_, workspace, _, err := store.Signup(ctx, "cap@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(subject string) error {
		tx, beginErr := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback(ctx)
		if reserveErr := reserveInitialDeploymentUsage(ctx, tx, workspace.ID, subject, store.now()); reserveErr != nil {
			return reserveErr
		}
		return tx.Commit(ctx)
	}
	if err := reserve("deployment-one"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("deployment-one"); err != nil {
		t.Fatalf("retry did not reuse its reservation: %v", err)
	}
	if err := reserve("deployment-two"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second host reservation err=%v", err)
	}
	var reserved, reservations int
	if err := store.pool.QueryRow(ctx, `SELECT reserved_cents FROM workspace_usage_caps WHERE workspace_id=$1`, workspace.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM usage_reservations WHERE workspace_id=$1`, workspace.ID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reserved != 500 || reservations != 1 {
		t.Fatalf("reserved=%d reservations=%d", reserved, reservations)
	}
}

func TestDurableAgentContinuityLifecycle(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	if err := store.SeedInvite(ctx, "beta-one", "test"); err != nil {
		t.Fatal(err)
	}
	account, workspace, humanToken, err := store.Signup(ctx, "ACE@example.com", "correct horse battery staple", "beta-one", true)
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != "ace@example.com" || workspace.Revision != 1 || humanToken == "" {
		t.Fatalf("unexpected signup result: %#v %#v", account, workspace)
	}
	human, err := store.ResolveHuman(ctx, humanToken)
	if err != nil || human.Actor.Kind != "human" {
		t.Fatalf("resolve human: %#v %v", human, err)
	}
	device, err := store.BeginDeviceAuthorization(ctx, "Codex on test", "codex", Authority{Inspect: true, Draft: true}, "http://localhost:3001")
	if err != nil {
		t.Fatal(err)
	}
	installation, err := store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "blackout-conversation-a")
	if err != nil {
		t.Fatal(err)
	}
	if pair.Installation.ID != installation.ID || pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatalf("unexpected token pair: %#v", pair)
	}
	first, err := store.ResolveAgent(ctx, pair.AccessToken)
	if err != nil || first.Actor.ID != installation.ID || first.Actor.SessionID == "" {
		t.Fatalf("resolve agent: %#v %v", first, err)
	}
	refreshed, err := store.RefreshAgent(ctx, pair.RefreshToken, "blackout-conversation-b")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Installation.ID != installation.ID || refreshed.Session.ID == pair.Session.ID {
		t.Fatal("new conversation did not retain installation identity with a distinct session")
	}
	if _, err := store.ResolveAgent(ctx, refreshed.AccessToken); err != nil {
		t.Fatalf("refreshed access was not active before replay: %v", err)
	}
	if _, err := store.RefreshAgent(ctx, pair.RefreshToken, "replay"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("rotated refresh token replay was not rejected: %v", err)
	}
	if _, err := store.ResolveAgent(ctx, refreshed.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh replay did not revoke the credential family: %v", err)
	}
	if err := store.RevokeInstallation(ctx, workspace.ID, installation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveAgent(ctx, refreshed.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked installation access remained valid: %v", err)
	}
	if _, err := store.RefreshAgent(ctx, refreshed.RefreshToken, "post-revocation"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked installation refresh remained valid: %v", err)
	}
}

func TestBootstrapAndDurableExecutionLedger(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "owner@example.com", "another correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("api", "Serve an authenticated API").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/api").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	system, err = canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutSystem(ctx, workspace.ID, system); err != nil {
		t.Fatal(err)
	}
	device, err := store.BeginDeviceAuthorization(ctx, "Blackout", "test", Authority{Inspect: true, Draft: true}, "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.ResolveAgent(ctx, pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	change := sdk.Change{SchemaVersion: "v1", ID: "change-integration", System: "api", Summary: "Ship a new release", Phase: "authorized", Digest: "digest", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), DraftedBy: &principal.Actor}
	if err := store.RecordChange(ctx, workspace.ID, change); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	bootstrap, err := service.Bootstrap(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(bootstrap.Systems) != 1 || len(bootstrap.Changes) != 1 || len(bootstrap.PendingChanges) != 1 || bootstrap.Workspace.ID != workspace.ID {
		t.Fatalf("bootstrap omitted durable state: %#v", bootstrap)
	}
	execution, err := store.EnqueueExecution(ctx, workspace.ID, "api", change.ID, sdk.ActorRef{Kind: "human", ID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimExecution(ctx, "worker-one", time.Minute)
	if err != nil || !ok || claimed.ID != execution.ID || claimed.Attempts != 1 {
		t.Fatalf("claim: %#v %t %v", claimed, ok, err)
	}
	if err := store.CompleteExecution(ctx, execution.ID, "worker-one", nil); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Execution(ctx, execution.ID)
	if err != nil || completed.Phase != "succeeded" || completed.CompletedAt == nil {
		t.Fatalf("execution was not durably completed: %#v %v", completed, err)
	}
	byChange, err := store.ExecutionForChange(ctx, workspace.ID, "api", change.ID)
	if err != nil || byChange.ID != execution.ID || byChange.Phase != "succeeded" || byChange.RequestedBy.ID != account.ID {
		t.Fatalf("execution was not addressable from its Change: %#v %v", byChange, err)
	}
	changes, err := store.ListChanges(ctx, workspace.ID)
	if err != nil || len(changes) != 1 || changes[0].ExecutionID != execution.ID || changes[0].ExecutionPhase != "succeeded" {
		t.Fatalf("Change index omitted durable execution identity: %#v %v", changes, err)
	}
	inspection, err := service.InspectChangeWithExecution(ctx, workspace.ID, "api", change.ID)
	if err != nil || inspection.Execution == nil || inspection.Execution.ID != execution.ID {
		t.Fatalf("Change inspection omitted durable execution: %#v %v", inspection, err)
	}
	bootstrap, err = service.Bootstrap(ctx, principal)
	if err != nil || len(bootstrap.Changes) != 1 || bootstrap.Changes[0].ExecutionID != execution.ID || bootstrap.Changes[0].ExecutionPhase != "succeeded" || len(bootstrap.PendingChanges) != 1 || bootstrap.PendingChanges[0].ExecutionID != execution.ID || bootstrap.PendingChanges[0].ExecutionPhase != "succeeded" {
		t.Fatalf("fresh conversation bootstrap omitted execution continuity: %#v %v", bootstrap, err)
	}
}
