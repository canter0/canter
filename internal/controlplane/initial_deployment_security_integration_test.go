package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

func initialExecutionFixture(t *testing.T, store *Store, email string) (Account, Workspace, InitialDeployment, InitialDeploymentExecution) {
	t.Helper()
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, email, "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("lease-app", "Exercise fenced first deployment recovery").
		OnHost("compute", 1, 1024, 256).
		WithM1("systems/lease-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	plan := InitialDeploymentPlan{System: system, ArtifactSHA256: strings.Repeat("a", 64), Release: InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080}, Verification: sdk.ChangeVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}, WorkspaceRevision: workspace.Revision}
	digest, err := digestInitialDeployment(plan)
	if err != nil {
		t.Fatal(err)
	}
	now := store.now()
	deployment := InitialDeployment{SchemaVersion: "v1", ID: "dep-lease", WorkspaceID: workspace.ID, System: system.Metadata.Name, Summary: "test fencing", Phase: "drafted", Digest: digest, DraftedBy: sdk.ActorRef{Kind: "agent", ID: "agt_test"}, Plan: plan, Operations: []InitialDeploymentOperation{{ID: "01-register-system", Kind: "system.register", Description: "register", Phase: "pending"}}, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateInitialDeployment(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	deployment, err = store.AuthorizeInitialDeployment(ctx, workspace.ID, deployment.ID, digest, sdk.ActorRef{Kind: "human", ID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	return account, workspace, deployment, execution
}

func TestInitialDeploymentLeaseReclaimFencesStaleWorkerAndSkipsSucceededOperation(t *testing.T) {
	store := integrationStore(t)
	_, _, deployment, execution := initialExecutionFixture(t, store, "lease-reclaim@example.com")
	ctx := context.Background()
	clock := time.Now().UTC()
	store.now = func() time.Time { return clock }

	first, ok, err := store.ClaimInitialDeploymentExecution(ctx, "same-worker", time.Minute)
	if err != nil || !ok || first.ID != execution.ID || first.ClaimToken == "" {
		t.Fatalf("first claim: %#v %t %v", first, ok, err)
	}
	clock = clock.Add(2 * time.Minute)
	second, ok, err := store.ClaimInitialDeploymentExecution(ctx, "same-worker", time.Minute)
	if err != nil || !ok || second.ID != execution.ID || second.ClaimToken == first.ClaimToken {
		t.Fatalf("reclaim did not rotate fencing token: %#v %t %v", second, ok, err)
	}
	if err := store.RenewInitialDeploymentExecution(ctx, first.ID, "same-worker", first.ClaimToken, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale renew was accepted: %v", err)
	}
	if _, err := store.BeginInitialDeploymentOperation(ctx, first.ID, "same-worker", first.ClaimToken, deployment.ID, "01-register-system"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale operation transition was accepted: %v", err)
	}
	if err := store.CompleteInitialDeploymentExecution(ctx, first.ID, "same-worker", first.ClaimToken, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion was accepted: %v", err)
	}
	shouldRun, err := store.BeginInitialDeploymentOperation(ctx, second.ID, "same-worker", second.ClaimToken, deployment.ID, "01-register-system")
	if err != nil || !shouldRun {
		t.Fatalf("active claim could not begin operation: %t %v", shouldRun, err)
	}
	evidence := sdk.ChangeEvidence{OperationID: "01-register-system", Kind: "test", Statement: "completed once", ObservedAt: clock}
	if err := store.FinishInitialDeploymentOperation(ctx, second.ID, "same-worker", second.ClaimToken, deployment.ID, "01-register-system", "succeeded", "", &evidence); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	third, ok, err := store.ClaimInitialDeploymentExecution(ctx, "same-worker", time.Minute)
	if err != nil || !ok || third.ClaimToken == second.ClaimToken {
		t.Fatalf("second reclaim: %#v %t %v", third, ok, err)
	}
	shouldRun, err = store.BeginInitialDeploymentOperation(ctx, third.ID, "same-worker", third.ClaimToken, deployment.ID, "01-register-system")
	if err != nil || shouldRun {
		t.Fatalf("succeeded operation was not skipped on recovery: %t %v", shouldRun, err)
	}
	if err := store.CompleteInitialDeploymentExecution(ctx, third.ID, "same-worker", third.ClaimToken, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInitialDeploymentRejectsPlanMutationAtAuthorizationAndExecution(t *testing.T) {
	t.Run("authorization", func(t *testing.T) {
		store := integrationStore(t)
		account, workspace, _, err := store.Signup(context.Background(), "digest-auth@example.com", "correct horse battery staple", "", false)
		if err != nil {
			t.Fatal(err)
		}
		system, _ := sdk.NewSystem("digest-auth", "bind plan").OnHost("compute", 1, 1024, 256).WithM1("systems/digest-auth").Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).Build()
		plan := InitialDeploymentPlan{System: system, ArtifactSHA256: strings.Repeat("b", 64), Release: InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080}, Verification: sdk.ChangeVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}, WorkspaceRevision: workspace.Revision}
		digest, _ := digestInitialDeployment(plan)
		now := store.now()
		deployment := InitialDeployment{SchemaVersion: "v1", ID: "dep-mutated-auth", WorkspaceID: workspace.ID, System: system.Metadata.Name, Summary: "mutate", Phase: "drafted", Digest: digest, DraftedBy: sdk.ActorRef{Kind: "agent", ID: "agt"}, Plan: plan, Operations: []InitialDeploymentOperation{}, CreatedAt: now, UpdatedAt: now}
		if err := store.CreateInitialDeployment(context.Background(), deployment); err != nil {
			t.Fatal(err)
		}
		deployment.Plan.Release.PublicPort = 9090
		raw, _ := json.Marshal(deployment)
		if _, err := store.pool.Exec(context.Background(), `UPDATE initial_deployments SET document=$1 WHERE id=$2`, raw, deployment.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AuthorizeInitialDeployment(context.Background(), workspace.ID, deployment.ID, digest, sdk.ActorRef{Kind: "human", ID: account.ID}); !errors.Is(err, ErrConflict) {
			t.Fatalf("mutated plan was authorized: %v", err)
		}
	})

	t.Run("execution", func(t *testing.T) {
		store := integrationStore(t)
		_, workspace, deployment, execution := initialExecutionFixture(t, store, "digest-execution@example.com")
		deployment.Plan.Release.PublicPort = 9090
		raw, _ := json.Marshal(deployment)
		if _, err := store.pool.Exec(context.Background(), `UPDATE initial_deployments SET document=$1 WHERE id=$2`, raw, deployment.ID); err != nil {
			t.Fatal(err)
		}
		claimed, ok, err := store.ClaimInitialDeploymentExecution(context.Background(), "digest-worker", time.Minute)
		if err != nil || !ok || claimed.ID != execution.ID {
			t.Fatalf("claim: %#v %t %v", claimed, ok, err)
		}
		service := &Service{Store: store, NodeGatewayURL: "https://control.canter.test"}
		dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: &initialDeploymentFakeEngine{}, NodeBinary: []byte("node"), WorkerID: "digest-worker", LeaseDuration: time.Minute}
		if err := dispatcher.runOne(context.Background(), claimed); err != nil {
			t.Fatal(err)
		}
		finished, err := store.InitialDeploymentExecution(context.Background(), execution.ID)
		if err != nil || finished.Phase != "failed" || !strings.Contains(finished.Failure, "immutable digest") {
			t.Fatalf("execution did not reject mutated plan: %#v %v", finished, err)
		}
		_ = workspace
	})
}

func TestInitialDeploymentRehashesArtifactBeforePublication(t *testing.T) {
	store := integrationStore(t)
	engine := &initialDeploymentFakeEngine{}
	service := &Service{Store: store, Engine: engine, NodeGatewayURL: "https://control.canter.test"}
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "artifact-mutation@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := service.UploadDeploymentArtifact(ctx, workspace.ID, testApplicationArtifact(t), "app.tar.gz", "application/gzip", sdk.ActorRef{Kind: "agent", ID: "agt"})
	if err != nil {
		t.Fatal(err)
	}
	system, _ := sdk.NewSystem("mutated-artifact", "reject mutated artifact").OnHost("compute", 1, 1024, 256).WithM1("systems/mutated-artifact").Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).Build()
	system, err = canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutSystem(ctx, workspace.ID, system); err != nil {
		t.Fatal(err)
	}
	deployment, err := service.DraftInitialDeployment(ctx, workspace.ID, DraftInitialDeploymentInput{Summary: "deploy", System: system, ArtifactSHA256: artifact.SHA256, Release: InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080}, Verification: sdk.ChangeVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}}, sdk.ActorRef{Kind: "agent", ID: "agt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthorizeInitialDeployment(ctx, workspace.ID, deployment.ID, deployment.Digest, sdk.ActorRef{Kind: "human", ID: account.ID}); err != nil {
		t.Fatal(err)
	}
	execution, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := sdk.ControlPlaneArtifactKey(artifact.SHA256)
	mutated := append([]byte(nil), engine.artifacts[key]...)
	mutated[len(mutated)/2] ^= 0xff
	engine.artifacts[key] = mutated
	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, "artifact-worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: engine, NodeBinary: []byte("node"), WorkerID: "artifact-worker", LeaseDuration: time.Minute, WaitTimeout: time.Second}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	finished, err := store.InitialDeploymentExecution(ctx, execution.ID)
	if err != nil || finished.Phase != "failed" || !strings.Contains(finished.Failure, "digest mismatch") || engine.published {
		t.Fatalf("mutated artifact reached publication: %#v published=%t err=%v", finished, engine.published, err)
	}
}

func TestInitialDeploymentRunOneSurfacesLostLeaseCompletion(t *testing.T) {
	store := integrationStore(t)
	_, _, _, _ = initialExecutionFixture(t, store, "completion-fence@example.com")
	clock := time.Now().UTC()
	store.now = func() time.Time { return clock }
	claimed, ok, err := store.ClaimInitialDeploymentExecution(context.Background(), "completion-worker", time.Second)
	if err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	clock = clock.Add(2 * time.Second)
	service := &Service{Store: store, NodeGatewayURL: "https://control.canter.test"}
	dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: &initialDeploymentFakeEngine{}, NodeBinary: []byte("node"), WorkerID: "completion-worker", LeaseDuration: time.Second}
	if err := dispatcher.runOne(context.Background(), claimed); !errors.Is(err, ErrConflict) {
		t.Fatalf("lost-lease completion failure was hidden: %v", err)
	}
}
