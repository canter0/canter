package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

type scriptedInitialDeploymentWaitEngine struct {
	*initialDeploymentFakeEngine
	wait func(context.Context, sdk.System) (sdk.ReleaseView, error)
}

func (f *scriptedInitialDeploymentWaitEngine) WaitPublicEndpoint(ctx context.Context, system sdk.System) (sdk.ReleaseView, error) {
	return f.wait(ctx, system)
}

type codedObservationError struct {
	code string
}

func (e codedObservationError) Error() string     { return "object storage: " + e.code }
func (e codedObservationError) ErrorCode() string { return e.code }

func TestInitialDeploymentWaitRetriesMissingObservedReleaseUntilHealthy(t *testing.T) {
	calls := 0
	want := sdk.ReleaseView{PublicEndpoint: sdk.PublicEndpointObservation{Phase: "ready", URL: "http://203.0.113.8:8080/health"}}
	engine := &scriptedInitialDeploymentWaitEngine{initialDeploymentFakeEngine: &initialDeploymentFakeEngine{}}
	engine.wait = func(context.Context, sdk.System) (sdk.ReleaseView, error) {
		calls++
		if calls == 1 {
			return sdk.ReleaseView{}, fmt.Errorf("GetObject observed.json: %w", codedObservationError{code: "NoSuchKey"})
		}
		return want, nil
	}
	dispatcher := &InitialDeploymentDispatcher{Engine: engine, ObservationPollInterval: time.Millisecond}
	got, err := dispatcher.waitPublicEndpoint(context.Background(), sdk.System{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || got.PublicEndpoint.URL != want.PublicEndpoint.URL {
		t.Fatalf("calls=%d view=%#v", calls, got)
	}
}

func TestInitialDeploymentWaitBoundsMissingObservedRelease(t *testing.T) {
	calls := 0
	engine := &scriptedInitialDeploymentWaitEngine{initialDeploymentFakeEngine: &initialDeploymentFakeEngine{}}
	engine.wait = func(context.Context, sdk.System) (sdk.ReleaseView, error) {
		calls++
		return sdk.ReleaseView{}, codedObservationError{code: "NotFound"}
	}
	dispatcher := &InitialDeploymentDispatcher{Engine: engine, ObservationPollInterval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := dispatcher.waitPublicEndpoint(ctx, sdk.System{})
	if !errors.Is(err, context.DeadlineExceeded) || calls < 2 {
		t.Fatalf("missing observation was not polled to its bound: calls=%d err=%v", calls, err)
	}
}

func TestInitialDeploymentWaitPreservesNonMissingStorageError(t *testing.T) {
	want := errors.New("access denied")
	calls := 0
	engine := &scriptedInitialDeploymentWaitEngine{initialDeploymentFakeEngine: &initialDeploymentFakeEngine{}}
	engine.wait = func(context.Context, sdk.System) (sdk.ReleaseView, error) {
		calls++
		return sdk.ReleaseView{}, fmt.Errorf("GetObject observed.json: %w", want)
	}
	dispatcher := &InitialDeploymentDispatcher{Engine: engine, ObservationPollInterval: time.Millisecond}
	_, err := dispatcher.waitPublicEndpoint(context.Background(), sdk.System{})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("non-missing storage error was retried or replaced: calls=%d err=%v", calls, err)
	}
}

func TestInitialDeploymentRetryKeepsOriginalWorkspaceAuthorization(t *testing.T) {
	deployment := InitialDeployment{
		Plan:       InitialDeploymentPlan{WorkspaceRevision: 4},
		Operations: []InitialDeploymentOperation{{ID: "01-register-system", Phase: "succeeded"}},
	}
	if !initialDeploymentWorkspaceRevisionValid(deployment, 17, true) {
		t.Fatal("an exact previously registered System could not replay later authorized operations")
	}
	if initialDeploymentWorkspaceRevisionValid(deployment, 17, false) {
		t.Fatal("workspace revision drift was accepted without the exact registered System")
	}
	deployment.Operations[0].Phase = "failed"
	if initialDeploymentWorkspaceRevisionValid(deployment, 17, true) {
		t.Fatal("workspace revision drift was accepted before registration succeeded")
	}
}
