package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/jackc/pgx/v5"
)

func testApplicationArtifact(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gz)
	payload := []byte("application")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "app", Mode: 0o750, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type initialDeploymentFakeEngine struct {
	bootstrapped    bool
	exposed         bool
	exposeCalls     int
	recoverCalls    int
	escalated       bool
	bootstrapErrors []error
	bootstrapCalls  int
	destroyed       bool
	waitErrors      []error
	published       bool
	verified        bool
	artifacts       map[string][]byte
	gateway         sdk.NodeBootstrapConfig
}

func (f *initialDeploymentFakeEngine) InspectSystem(_ context.Context, system sdk.System) (sdk.SystemView, error) {
	return sdk.CompileSystemView(system)
}
func (f *initialDeploymentFakeEngine) DraftChangeRequest(context.Context, sdk.System, sdk.ChangeRequest) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}
func (f *initialDeploymentFakeEngine) InspectChange(context.Context, sdk.System, string) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}
func (f *initialDeploymentFakeEngine) AuthorizeChange(context.Context, sdk.System, string, string) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}
func (f *initialDeploymentFakeEngine) ApplyChange(context.Context, sdk.System, string) (sdk.Change, error) {
	return sdk.Change{}, errors.New("unused")
}
func (f *initialDeploymentFakeEngine) StageControlPlaneArtifact(_ context.Context, data []byte, filename, contentType string) (sdk.StagedArtifact, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	key, _ := sdk.ControlPlaneArtifactKey(digest)
	if f.artifacts == nil {
		f.artifacts = make(map[string][]byte)
	}
	f.artifacts[key] = append([]byte(nil), data...)
	return sdk.StagedArtifact{Key: key, SHA256: digest, Size: int64(len(data)), ContentType: contentType, Filename: filename}, nil
}
func (f *initialDeploymentFakeEngine) VerifyStagedArtifact(_ context.Context, artifact sdk.StagedArtifact) error {
	key, err := sdk.ControlPlaneArtifactKey(artifact.SHA256)
	if err != nil || key != artifact.Key {
		return errors.New("artifact key mismatch")
	}
	data := f.artifacts[key]
	if int64(len(data)) != artifact.Size {
		return errors.New("artifact size mismatch")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return errors.New("artifact digest mismatch")
	}
	return nil
}
func (f *initialDeploymentFakeEngine) BootstrapSystemHostViaGateway(_ context.Context, _ sdk.System, _ []byte, gateway sdk.NodeBootstrapConfig) (sdk.ApplyResult, error) {
	if gateway.GatewayURL == "" || gateway.EnrollmentID == "" || gateway.EnrollmentToken == "" {
		return sdk.ApplyResult{}, errors.New("missing node gateway enrollment")
	}
	f.gateway = gateway
	f.bootstrapCalls++
	f.bootstrapped = true
	f.destroyed = false
	if len(f.bootstrapErrors) > 0 {
		err := f.bootstrapErrors[0]
		f.bootstrapErrors = f.bootstrapErrors[1:]
		if err != nil {
			f.escalated = true
			return sdk.ApplyResult{}, err
		}
	}
	f.exposed = true
	return sdk.ApplyResult{OperationID: "provider-operation"}, nil
}
func (f *initialDeploymentFakeEngine) SystemHostStatus(context.Context, sdk.System) (sdk.State, error) {
	if !f.bootstrapped {
		return sdk.State{}, ErrNotFound
	}
	if f.destroyed {
		return sdk.State{Phase: "destroyed", Resources: []sdk.Resource{{ID: "compute-hidden", Status: "DELETED"}}}, nil
	}
	state := sdk.State{Phase: "ready", Resources: []sdk.Resource{{ID: "compute-hidden", Address: "192.0.2.1"}}}
	if f.escalated {
		state.Phase = "escalated"
		state.ExposureIntent = &sdk.ExposureIntent{OperationID: "expose-test", ServerID: "compute-hidden", Name: "test", Ownership: "sha256:test", Protocol: "tcp", Port: 8080, Phase: "escalated", MutationUnresolved: true}
		state.Failure = "provider response was ambiguous"
		return state, nil
	}
	if f.exposed {
		state.ExposureIntent = &sdk.ExposureIntent{OperationID: "expose-test", ServerID: "compute-hidden", Name: "test", Ownership: "sha256:test", Protocol: "tcp", Port: 8080, Phase: "ready"}
		state.NetworkPolicies = []sdk.NetworkPolicy{{ID: "policy-test", PortID: "port-test", RuleID: "rule-test", Protocol: "tcp", Port: 8080}}
	}
	return state, nil
}
func (f *initialDeploymentFakeEngine) ExposeSystemHost(ctx context.Context, system sdk.System) (sdk.State, error) {
	f.exposeCalls++
	f.exposed = true
	return f.SystemHostStatus(ctx, system)
}
func (f *initialDeploymentFakeEngine) RecoverSystemHostExposure(ctx context.Context, system sdk.System) (sdk.State, error) {
	f.recoverCalls++
	f.escalated = false
	f.exposed = true
	return f.SystemHostStatus(ctx, system)
}
func (f *initialDeploymentFakeEngine) PublishStagedRelease(_ context.Context, system sdk.System, input sdk.StagedReleaseInput) (sdk.ReleaseManifest, error) {
	f.published = true
	return sdk.ReleaseManifest{SchemaVersion: "v1", System: system.Metadata.Name, Version: input.Artifact.SHA256[:12], ArtifactSHA: input.Artifact.SHA256, ArtifactKey: input.Artifact.Key, HealthPath: input.HealthPath, PublicPort: input.PublicPort}, nil
}

func TestSystemHostEndpointReadyRequiresDurableMatchingExposure(t *testing.T) {
	system := sdk.System{Spec: sdk.SystemContract{
		Constraints: sdk.Constraints{Host: sdk.HostConstraint{Count: 1}},
		Services:    []sdk.SystemService{{Name: "web", Networking: "public", Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}},
	}}
	state := sdk.State{Phase: "ready", Resources: []sdk.Resource{{ID: "host-1"}}}
	if systemHostEndpointReady(system, state) {
		t.Fatal("compute readiness alone was treated as endpoint readiness")
	}
	state.ExposureIntent = &sdk.ExposureIntent{OperationID: "expose-test", ServerID: "host-1", Name: "test", Ownership: "sha256:test", Protocol: "tcp", Port: 8080, Phase: "creating"}
	if systemHostEndpointReady(system, state) {
		t.Fatal("in-flight exposure was treated as ready")
	}
	state.ExposureIntent.Phase = "ready"
	state.NetworkPolicies = []sdk.NetworkPolicy{{ID: "sg-1", PortID: "port-1", RuleID: "rule-1", Protocol: "tcp", Port: 8080}}
	if !systemHostEndpointReady(system, state) {
		t.Fatal("matching durable exposure was not treated as ready")
	}
	state.ExposureIntent.ServerID = "other-host"
	if systemHostEndpointReady(system, state) {
		t.Fatal("exposure for another host was treated as ready")
	}
}
func (f *initialDeploymentFakeEngine) WaitPublicEndpoint(context.Context, sdk.System) (sdk.ReleaseView, error) {
	if !f.published {
		return sdk.ReleaseView{}, errors.New("release was not published")
	}
	if len(f.waitErrors) > 0 {
		err := f.waitErrors[0]
		f.waitErrors = f.waitErrors[1:]
		if err != nil {
			return sdk.ReleaseView{}, err
		}
	}
	return sdk.ReleaseView{PublicEndpoint: sdk.PublicEndpointObservation{Phase: "ready", URL: "http://example.test/health", StatusCode: 200}}, nil
}
func (f *initialDeploymentFakeEngine) VerifyPublicEndpoint(_ context.Context, system sdk.System, version string, verification sdk.ChangeVerification) (sdk.PublicEndpointObservation, error) {
	f.verified = true
	return sdk.PublicEndpointObservation{SchemaVersion: "v1", System: system.Metadata.Name, Version: version, Phase: "ready", URL: "http://example.test" + verification.Path, StatusCode: verification.ExpectedStatus}, nil
}

func TestInitialDeploymentIsAgentDraftedHumanAuthorizedAndServerExecuted(t *testing.T) {
	store := integrationStore(t)
	engine := &initialDeploymentFakeEngine{}
	service := &Service{Store: store, Engine: engine, NodeGatewayURL: "https://control.canter.test"}
	handler := NewHTTPServer(service, HTTPConfig{PublicURL: "http://canter.test"})
	ctx := context.Background()
	account, workspace, humanToken, err := store.Signup(ctx, "first-deploy@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.BeginDeviceAuthorization(ctx, "No-context Codex", "codex", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApproveDevice(ctx, device.UserCode, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(ctx, device.DeviceCode, "blackout-session")
	if err != nil {
		t.Fatal(err)
	}

	artifactRequest := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+workspace.ID+"/artifacts", bytes.NewReader(testApplicationArtifact(t)))
	artifactRequest.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	artifactRequest.Header.Set("Content-Type", "application/gzip")
	artifactRequest.Header.Set("X-Canter-Filename", "application.tar.gz")
	artifactResponse := httptest.NewRecorder()
	handler.ServeHTTP(artifactResponse, artifactRequest)
	if artifactResponse.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", artifactResponse.Code, artifactResponse.Body.String())
	}
	if bytes.Contains(artifactResponse.Body.Bytes(), []byte("private-m1")) {
		t.Fatal("agent-visible artifact response leaked its server-side m1 key")
	}
	var artifact DeploymentArtifact
	if err := json.Unmarshal(artifactResponse.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}

	system, err := sdk.NewSystem("first-app", "Serve a real first application").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/first-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	proposal := DraftInitialDeploymentInput{Summary: "Create the first app", System: system, ArtifactSHA256: artifact.SHA256, Release: InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080}, Verification: sdk.ChangeVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}}
	draft := requestJSONWithBearer(t, handler, "/v1/workspaces/"+workspace.ID+"/initial-deployments", proposal, pair.AccessToken)
	if draft.Code != http.StatusCreated {
		t.Fatalf("draft status %d: %s", draft.Code, draft.Body.String())
	}
	var deployment InitialDeployment
	if err := json.Unmarshal(draft.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.DraftedBy.ID != pair.Installation.ID || deployment.DraftedBy.SessionID != pair.Session.ID || deployment.Phase != "drafted" {
		t.Fatalf("draft attribution or phase missing: %#v", deployment)
	}
	wantPrefix, err := canonicalSystemPrefix(workspace.ID, system.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Plan.System.Spec.M1.Prefix != wantPrefix {
		t.Fatalf("initial deployment digest did not bind canonical prefix: got %q want %q", deployment.Plan.System.Spec.M1.Prefix, wantPrefix)
	}

	agentAuthorize := requestJSONWithBearer(t, handler, "/v1/workspaces/"+workspace.ID+"/initial-deployments/"+deployment.ID+"/authorize", map[string]string{"digest": deployment.Digest}, pair.AccessToken)
	if agentAuthorize.Code != http.StatusForbidden {
		t.Fatalf("agent authorized its own deployment: %d %s", agentAuthorize.Code, agentAuthorize.Body.String())
	}
	humanCookie := &http.Cookie{Name: "canter_session", Value: humanToken}
	authorized := requestJSON(t, handler, http.MethodPost, "/v1/workspaces/"+workspace.ID+"/initial-deployments/"+deployment.ID+"/authorize", map[string]string{"digest": deployment.Digest}, humanCookie)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorize status %d: %s", authorized.Code, authorized.Body.String())
	}
	queued := requestJSON(t, handler, http.MethodPost, "/v1/workspaces/"+workspace.ID+"/initial-deployments/"+deployment.ID+"/apply", map[string]any{}, humanCookie)
	if queued.Code != http.StatusAccepted {
		t.Fatalf("queue status %d: %s", queued.Code, queued.Body.String())
	}
	var execution InitialDeploymentExecution
	if err := json.Unmarshal(queued.Body.Bytes(), &execution); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, "test-worker", time.Minute)
	if err != nil || !ok || claimed.ID != execution.ID {
		t.Fatalf("claim: %#v %t %v", claimed, ok, err)
	}
	dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: engine, NodeBinary: []byte("server-owned node"), WorkerID: "test-worker", LeaseDuration: time.Minute, WaitTimeout: time.Second}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	finished, err := store.InitialDeploymentExecution(ctx, execution.ID)
	if err != nil || finished.Phase != "succeeded" {
		t.Fatalf("execution was not completed: %#v %v", finished, err)
	}
	committed, err := store.InitialDeployment(ctx, workspace.ID, deployment.ID)
	if err != nil || committed.Phase != "succeeded" || len(committed.Evidence) != 5 || !engine.bootstrapped || !engine.published || !engine.verified {
		t.Fatalf("deployment did not execute all real boundaries: %#v %v", committed, err)
	}
	if engine.gateway.GatewayURL != service.NodeGatewayURL || !strings.HasPrefix(engine.gateway.EnrollmentToken, "ce_") {
		t.Fatalf("provider bootstrap did not receive scoped gateway enrollment: %#v", engine.gateway)
	}
}

func TestCorrectedProposalReplacesLegacyUnsupportedClassBeforeRuntimeAndReusesReservation(t *testing.T) {
	store := integrationStore(t)
	engine := &initialDeploymentFakeEngine{}
	service := &Service{Store: store, Engine: engine, NodeGatewayURL: "https://control.canter.test"}
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "corrected-class@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	agent := sdk.ActorRef{Kind: "agent", ID: "agt_correct", SessionID: "ags_correct"}
	artifact, err := service.UploadDeploymentArtifact(ctx, workspace.ID, testApplicationArtifact(t), "application.tar.gz", "application/gzip", agent)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := sdk.NewSystem("legacy-class-app", "Serve the corrected application").
		OnHost("c1", 1, 512, 128).
		WithM1("systems/legacy-class-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalizeSystemForWorkspace(workspace.ID, corrected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutSystem(ctx, workspace.ID, canonical); err != nil {
		t.Fatal(err)
	}
	legacy := canonical
	legacy.Spec.Constraints.Host.Class = "shared"
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE systems SET contract=$1 WHERE workspace_id=$2 AND name=$3`, legacyRaw, workspace.ID, legacy.Metadata.Name); err != nil {
		t.Fatal(err)
	}
	workspace, err = store.Workspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan := InitialDeploymentPlan{
		System: legacy, ArtifactSHA256: artifact.SHA256,
		Release:           InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080},
		Verification:      sdk.ChangeVerification{Method: http.MethodGet, Path: "/health", ExpectedStatus: http.StatusOK},
		WorkspaceRevision: workspace.Revision - 1,
	}
	legacyDigest, err := digestInitialDeployment(legacyPlan)
	if err != nil {
		t.Fatal(err)
	}
	now := store.now()
	legacyDeployment := InitialDeployment{
		SchemaVersion: "v1", ID: "dep_legacy_shared", WorkspaceID: workspace.ID, System: legacy.Metadata.Name,
		Summary: "legacy unsupported class", Phase: "failed", Digest: legacyDigest, DraftedBy: agent, Plan: legacyPlan,
		Operations: []InitialDeploymentOperation{
			{ID: "01-register-system", Kind: "system.register", Phase: "succeeded"},
			{ID: "02-bootstrap-host", Kind: "system-host.bootstrap", Phase: "failed", Failure: `unsupported compute class "shared"`},
			{ID: "03-publish-release", Kind: "release.publish-staged", Phase: "pending"},
			{ID: "04-wait-healthy", Kind: "release.wait-public", Phase: "pending"},
			{ID: "05-verify-public", Kind: "http.verify", Phase: "pending"},
		},
		Failure: `unsupported compute class "shared"`, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	if err := store.CreateInitialDeployment(ctx, legacyDeployment); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if err := reserveInitialDeploymentUsage(ctx, tx, workspace.ID, legacyDeployment.ID, "", now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	proposal, err := service.DraftInitialDeployment(ctx, workspace.ID, DraftInitialDeploymentInput{
		Summary: "correct unsupported class", System: corrected, ArtifactSHA256: artifact.SHA256,
		Release:      InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080},
		Verification: sdk.ChangeVerification{Method: http.MethodGet, Path: "/health", ExpectedStatus: http.StatusOK},
	}, agent)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Plan.ReplacesDeploymentID != legacyDeployment.ID {
		t.Fatalf("corrected proposal replacement=%q want %q", proposal.Plan.ReplacesDeploymentID, legacyDeployment.ID)
	}
	if _, err := service.AuthorizeInitialDeployment(ctx, workspace.ID, proposal.ID, proposal.Digest, sdk.ActorRef{Kind: "human", ID: account.ID}); err != nil {
		t.Fatal(err)
	}
	execution, err := store.EnqueueInitialDeployment(ctx, workspace.ID, proposal.ID, sdk.ActorRef{Kind: "human", ID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	var reservationSubject string
	var reservationCount int
	if err := store.pool.QueryRow(ctx, `SELECT min(subject_id),count(*) FROM usage_reservations WHERE workspace_id=$1`, workspace.ID).Scan(&reservationSubject, &reservationCount); err != nil {
		t.Fatal(err)
	}
	if reservationCount != 1 || reservationSubject != proposal.ID {
		t.Fatalf("reservation was not transferred: count=%d subject=%q", reservationCount, reservationSubject)
	}
	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, "correction-worker", time.Minute)
	if err != nil || !ok || claimed.ID != execution.ID {
		t.Fatalf("claim=%#v ok=%t err=%v", claimed, ok, err)
	}
	dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: engine, NodeBinary: []byte("node"), WorkerID: "correction-worker", LeaseDuration: time.Minute, WaitTimeout: time.Second}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSystem(ctx, workspace.ID, corrected.Metadata.Name)
	if err != nil || record.Contract.Spec.Constraints.Host.Class != "c1" {
		t.Fatalf("corrected System was not registered: %#v err=%v", record, err)
	}
	finished, err := store.InitialDeployment(ctx, workspace.ID, proposal.ID)
	if err != nil || finished.Phase != "succeeded" {
		t.Fatalf("corrected deployment did not succeed: %#v err=%v", finished, err)
	}
}

func TestFailedInitialDeploymentHumanRetryRecoversEscalatedExposure(t *testing.T) {
	store := integrationStore(t)
	engine := &initialDeploymentFakeEngine{bootstrapErrors: []error{errors.New("provider attachment response was ambiguous")}}
	service := &Service{Store: store, Engine: engine, NodeGatewayURL: "https://control.canter.test"}
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "exposure-retry@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	actor := sdk.ActorRef{Kind: "agent", ID: "agt_retry", SessionID: "ags_retry"}
	artifact, err := service.UploadDeploymentArtifact(ctx, workspace.ID, testApplicationArtifact(t), "application.tar.gz", "application/gzip", actor)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("retry-app", "Recover an exact escalated endpoint").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/retry-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := service.DraftInitialDeployment(ctx, workspace.ID, DraftInitialDeploymentInput{
		Summary: "retry exact endpoint", System: system, ArtifactSHA256: artifact.SHA256,
		Release:      InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080},
		Verification: sdk.ChangeVerification{Method: http.MethodGet, Path: "/health", ExpectedStatus: http.StatusOK},
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthorizeInitialDeployment(ctx, workspace.ID, deployment.ID, deployment.Digest, sdk.ActorRef{Kind: "human", ID: account.ID}); err != nil {
		t.Fatal(err)
	}
	firstExecution, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID, SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, "retry-worker", time.Minute)
	if err != nil || !ok || claimed.ID != firstExecution.ID {
		t.Fatalf("first claim=%#v ok=%t err=%v", claimed, ok, err)
	}
	dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: engine, NodeBinary: []byte("node"), WorkerID: "retry-worker", LeaseDuration: time.Minute, WaitTimeout: time.Second}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	failed, err := store.InitialDeploymentExecution(ctx, firstExecution.ID)
	if err != nil || failed.Phase != "failed" || !engine.escalated || engine.recoverCalls != 0 {
		t.Fatalf("failed=%#v engine=%#v err=%v", failed, engine, err)
	}

	retryExecution, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID, SessionID: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = store.ClaimInitialDeploymentExecution(ctx, "retry-worker", time.Minute)
	if err != nil || !ok || claimed.ID != retryExecution.ID {
		t.Fatalf("retry claim=%#v ok=%t err=%v", claimed, ok, err)
	}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	succeeded, err := store.InitialDeploymentExecution(ctx, retryExecution.ID)
	if err != nil || succeeded.Phase != "succeeded" || engine.recoverCalls != 1 || engine.escalated || !engine.published || !engine.verified {
		t.Fatalf("succeeded=%#v engine=%#v err=%v", succeeded, engine, err)
	}
}

func TestHumanRetryReopensBootstrapReceiptAfterDestroyedHost(t *testing.T) {
	store := integrationStore(t)
	engine := &initialDeploymentFakeEngine{waitErrors: []error{errors.New("first release failed")}}
	service := &Service{Store: store, Engine: engine, NodeGatewayURL: "https://control.canter.test"}
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "destroyed-host-retry@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	actor := sdk.ActorRef{Kind: "agent", ID: "agt_destroyed_retry", SessionID: "ags_destroyed_retry"}
	artifact, err := service.UploadDeploymentArtifact(ctx, workspace.ID, testApplicationArtifact(t), "application.tar.gz", "application/gzip", actor)
	if err != nil {
		t.Fatal(err)
	}
	system, err := sdk.NewSystem("destroyed-retry-app", "Replace a destroyed first host").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/destroyed-retry-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := service.DraftInitialDeployment(ctx, workspace.ID, DraftInitialDeploymentInput{
		Summary: "replace destroyed host", System: system, ArtifactSHA256: artifact.SHA256,
		Release:      InitialDeploymentRelease{Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080},
		Verification: sdk.ChangeVerification{Method: http.MethodGet, Path: "/health", ExpectedStatus: http.StatusOK},
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthorizeInitialDeployment(ctx, workspace.ID, deployment.ID, deployment.Digest, sdk.ActorRef{Kind: "human", ID: account.ID}); err != nil {
		t.Fatal(err)
	}
	first, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID, SessionID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &InitialDeploymentDispatcher{Store: store, Service: service, Engine: engine, NodeBinary: []byte("node"), WorkerID: "destroyed-retry-worker", LeaseDuration: time.Minute, WaitTimeout: time.Second}
	claimed, ok, err := store.ClaimInitialDeploymentExecution(ctx, dispatcher.WorkerID, time.Minute)
	if err != nil || !ok || claimed.ID != first.ID {
		t.Fatalf("first claim=%#v ok=%t err=%v", claimed, ok, err)
	}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	failed, err := store.InitialDeploymentExecution(ctx, first.ID)
	if err != nil || failed.Phase != "failed" || engine.bootstrapCalls != 1 {
		t.Fatalf("failed=%#v bootstrapCalls=%d err=%v", failed, engine.bootstrapCalls, err)
	}

	// This represents an explicit operator replacement after the failed first
	// release. The durable destroyed state invalidates only operation 02.
	engine.destroyed = true
	retry, err := store.EnqueueInitialDeployment(ctx, workspace.ID, deployment.ID, sdk.ActorRef{Kind: "human", ID: account.ID, SessionID: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = store.ClaimInitialDeploymentExecution(ctx, dispatcher.WorkerID, time.Minute)
	if err != nil || !ok || claimed.ID != retry.ID {
		t.Fatalf("retry claim=%#v ok=%t err=%v", claimed, ok, err)
	}
	if err := dispatcher.runOne(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	succeeded, err := store.InitialDeploymentExecution(ctx, retry.ID)
	if err != nil || succeeded.Phase != "succeeded" || engine.bootstrapCalls != 2 || engine.destroyed || !engine.verified {
		t.Fatalf("succeeded=%#v engine=%#v err=%v", succeeded, engine, err)
	}
	final, err := store.InitialDeployment(ctx, workspace.ID, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapEvidence := 0
	for _, evidence := range final.Evidence {
		if evidence.OperationID == "02-bootstrap-host" {
			bootstrapEvidence++
		}
	}
	if bootstrapEvidence != 1 {
		t.Fatalf("stale bootstrap evidence survived replacement: %#v", final.Evidence)
	}
}
