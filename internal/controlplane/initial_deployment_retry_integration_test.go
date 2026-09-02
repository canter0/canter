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
)

func failedInitialDeploymentRetryFixture(t *testing.T, store *Store, email string) (Account, Workspace, InitialDeployment, InitialDeploymentExecution) {
	t.Helper()
	account, workspace, deployment, firstExecution := initialExecutionFixture(t, store, email)
	ctx := context.Background()

	deployment, err := store.InitialDeployment(ctx, workspace.ID, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	deployment.Operations = append(deployment.Operations,
		InitialDeploymentOperation{ID: "02-bootstrap-host", Kind: "system-host.bootstrap", Description: "bootstrap", Phase: "pending"},
		InitialDeploymentOperation{ID: "03-publish-release", Kind: "release.publish-staged", Description: "publish", Phase: "pending"},
		InitialDeploymentOperation{ID: "04-wait-healthy", Kind: "release.wait-public", Description: "wait", Phase: "pending"},
	)
	raw, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE initial_deployments SET document=$1 WHERE id=$2`, raw, deployment.ID); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, "first-worker", time.Minute)
	if err != nil || !ok || claimed.ID != firstExecution.ID {
		t.Fatalf("claim first execution: %#v %t %v", claimed, ok, err)
	}
	for _, operationID := range []string{"01-register-system", "02-bootstrap-host", "03-publish-release"} {
		shouldRun, err := store.BeginInitialDeploymentOperation(ctx, claimed.ID, "first-worker", claimed.ClaimToken, deployment.ID, operationID)
		if err != nil || !shouldRun {
			t.Fatalf("begin successful operation %s: %t %v", operationID, shouldRun, err)
		}
		evidence := sdk.ChangeEvidence{OperationID: operationID, Kind: "test", Statement: operationID + " completed exactly once", ObservedAt: store.now()}
		if err := store.FinishInitialDeploymentOperation(ctx, claimed.ID, "first-worker", claimed.ClaimToken, deployment.ID, operationID, "succeeded", "", &evidence); err != nil {
			t.Fatal(err)
		}
	}
	shouldRun, err := store.BeginInitialDeploymentOperation(ctx, claimed.ID, "first-worker", claimed.ClaimToken, deployment.ID, "04-wait-healthy")
	if err != nil || !shouldRun {
		t.Fatalf("begin failed operation: %t %v", shouldRun, err)
	}
	if err := store.FinishInitialDeploymentOperation(ctx, claimed.ID, "first-worker", claimed.ClaimToken, deployment.ID, "04-wait-healthy", "failed", "observed state was pending", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteInitialDeploymentExecution(ctx, claimed.ID, "first-worker", claimed.ClaimToken, errors.New("observed state was pending")); err != nil {
		t.Fatal(err)
	}
	failed, err := store.InitialDeployment(ctx, workspace.ID, deployment.ID)
	if err != nil || failed.Phase != "failed" || failed.Authorization == nil {
		t.Fatalf("failed proposal fixture: %#v %v", failed, err)
	}
	return account, workspace, failed, claimed
}

func TestFailedInitialDeploymentRetryIsSerializableAndResumesOperations(t *testing.T) {
	store := integrationStore(t)
	account, workspace, deployment, firstExecution := failedInitialDeploymentRetryFixture(t, store, "retry-race@example.com")
	ctx := context.Background()
	// A replay of the already-authorized digest does not re-bind authorization
	// to the workspace's later revision.
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=revision+7 WHERE id=$1`, workspace.ID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		execution InitialDeploymentExecution
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			ready.Done()
			<-start
			execution, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID, SessionID: fmt.Sprintf("retry-%d", index)})
			results <- result{execution: execution, err: err}
		}(index)
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results

	successes := 0
	var retryExecution InitialDeploymentExecution
	for _, got := range []result{first, second} {
		if got.err == nil {
			successes++
			retryExecution = got.execution
		} else if !errors.Is(got.err, ErrConflict) {
			t.Fatalf("concurrent retry returned unexpected error: %v", got.err)
		}
	}
	if successes != 1 || retryExecution.ID == firstExecution.ID {
		t.Fatalf("retry successes=%d execution=%#v", successes, retryExecution)
	}
	var executionCount, activeCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE phase IN ('queued','running')) FROM initial_deployment_executions WHERE deployment_id=$1`, deployment.ID).Scan(&executionCount, &activeCount); err != nil {
		t.Fatal(err)
	}
	if executionCount != 2 || activeCount != 1 {
		t.Fatalf("execution ledger count=%d active=%d", executionCount, activeCount)
	}

	queued, err := store.InitialDeployment(ctx, workspace.ID, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Phase != "queued" || queued.Failure != "" || queued.CompletedAt != nil || queued.Authorization == nil || queued.Authorization.Digest != deployment.Digest || len(queued.Evidence) != 3 {
		t.Fatalf("retry did not preserve authorization/evidence and clear terminal state: %#v", queued)
	}
	if queued.Operations[0].Phase != "succeeded" || queued.Operations[1].Phase != "succeeded" || queued.Operations[2].Phase != "succeeded" || queued.Operations[3].Phase != "pending" || queued.Operations[3].Failure != "" {
		t.Fatalf("retry operation state was not resumed safely: %#v", queued.Operations)
	}

	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, "retry-worker", time.Minute)
	if err != nil || !ok || claimed.ID != retryExecution.ID {
		t.Fatalf("claim retry: %#v %t %v", claimed, ok, err)
	}
	for _, operationID := range []string{"01-register-system", "02-bootstrap-host", "03-publish-release"} {
		shouldRun, err := store.BeginInitialDeploymentOperation(ctx, claimed.ID, "retry-worker", claimed.ClaimToken, deployment.ID, operationID)
		if err != nil || shouldRun {
			t.Fatalf("succeeded operation %s was not skipped: %t %v", operationID, shouldRun, err)
		}
	}
	shouldRun, err := store.BeginInitialDeploymentOperation(ctx, claimed.ID, "retry-worker", claimed.ClaimToken, deployment.ID, "04-wait-healthy")
	if err != nil || !shouldRun {
		t.Fatalf("failed operation was not rerun: %t %v", shouldRun, err)
	}
}

func TestFailedInitialDeploymentRetryRejectsInvalidAuthorizationDigestAndActiveExecution(t *testing.T) {
	t.Run("active execution", func(t *testing.T) {
		store := integrationStore(t)
		account, workspace, deployment, _ := initialExecutionFixture(t, store, "retry-active@example.com")
		if _, err := store.EnqueueInitialDeployment(context.Background(), workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("second active execution was accepted: %v", err)
		}
	})

	t.Run("authorization mismatch", func(t *testing.T) {
		store := integrationStore(t)
		account, workspace, deployment, _ := failedInitialDeploymentRetryFixture(t, store, "retry-authorization@example.com")
		deployment.Authorization.Digest = "different"
		raw, _ := json.Marshal(deployment)
		if _, err := store.pool.Exec(context.Background(), `UPDATE initial_deployments SET document=$1 WHERE id=$2`, raw, deployment.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnqueueInitialDeployment(context.Background(), workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("authorization mismatch was accepted: %v", err)
		}
	})

	t.Run("plan digest mismatch", func(t *testing.T) {
		store := integrationStore(t)
		account, workspace, deployment, _ := failedInitialDeploymentRetryFixture(t, store, "retry-digest@example.com")
		deployment.Plan.Release.PublicPort++
		raw, _ := json.Marshal(deployment)
		if _, err := store.pool.Exec(context.Background(), `UPDATE initial_deployments SET document=$1 WHERE id=$2`, raw, deployment.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnqueueInitialDeployment(context.Background(), workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("mutated exact plan was accepted: %v", err)
		}
	})
}
