package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

// stagedChangeFakeEngine records staged and legacy release drafting so the test
// can prove remote requests never reach the local-path planner.
type stagedChangeFakeEngine struct {
	initialDeploymentFakeEngine
	stagedCalls       int
	legacyCalls       int
	lastSystem        sdk.System
	lastRequest       sdk.ChangeRequest
	lastArtifact      sdk.StagedArtifact
	lastActor         sdk.ActorRef
	lastActorFound    bool
	lastBase          sdk.ChangeBaseRevision
	lastBaseFound     bool
	stagedDraftResult sdk.Change
}

func (f *stagedChangeFakeEngine) DraftChangeRequest(context.Context, sdk.System, sdk.ChangeRequest) (sdk.Change, error) {
	f.legacyCalls++
	return sdk.Change{}, errors.New("legacy drafting must not see remote releases")
}

func (f *stagedChangeFakeEngine) DraftStagedChangeRequest(ctx context.Context, system sdk.System, request sdk.ChangeRequest, artifact sdk.StagedArtifact) (sdk.Change, error) {
	f.stagedCalls++
	f.lastSystem, f.lastRequest, f.lastArtifact = system, request, artifact
	f.lastActor, f.lastActorFound = sdk.ActorFromContext(ctx)
	f.lastBase, f.lastBaseFound = sdk.ChangeBaseRevisionFromContext(ctx)
	if f.stagedDraftResult.ID == "" {
		f.stagedDraftResult = sdk.Change{SchemaVersion: "v1", ID: "change-staged", System: system.Metadata.Name, Summary: request.Spec.Summary, Phase: "drafted", Digest: "staged-digest", Plan: sdk.ChangePlan{BaseVersion: "base1234abcd", Release: sdk.ReleaseManifest{SchemaVersion: "v1", System: system.Metadata.Name, Version: artifact.SHA256[:12], ArtifactKey: artifact.Key, ArtifactSHA: artifact.SHA256, PublicPort: 8080}, StagedArtifactSize: artifact.Size}}
	}
	return f.stagedDraftResult, nil
}

func stagedChangeRequestBody(systemName, sha string) map[string]any {
	return map[string]any{
		"apiVersion": sdk.APIVersion, "kind": "Change",
		"metadata": map[string]any{"name": "ship-staged"},
		"spec": map[string]any{
			"system":  systemName,
			"summary": "Ship the staged artifact",
			"release": map[string]any{
				"artifactSha256": sha,
				"command":        []string{"./app"},
				"healthPath":     "/health",
				"publicPort":     8080,
			},
			"verification": map[string]any{"method": "GET", "path": "/version", "expectedStatus": 200},
		},
	}
}

func stagedReleaseDevice(t *testing.T, store *Store, account Account, workspace Workspace) TokenPair {
	t.Helper()
	device, err := store.BeginDeviceAuthorization(context.Background(), "Staged release agent", "test", Authority{Inspect: true, Draft: true}, "http://canter.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApproveDevice(context.Background(), device.UserCode, account.ID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	pair, err := store.ExchangeDevice(context.Background(), device.DeviceCode, "staged-session")
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func TestRemoteStagedReleaseDraft(t *testing.T) {
	store := integrationStore(t)
	engine := &stagedChangeFakeEngine{}
	handler := NewHTTPServer(&Service{Store: store, Engine: engine}, HTTPConfig{PublicURL: "http://canter.test"})
	ctx := context.Background()
	account, workspace, _, err := store.Signup(ctx, "staged-owner@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	pair := stagedReleaseDevice(t, store, account, workspace)

	system, err := sdk.NewSystem("staged-app", "Serve a staged application").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/staged-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalizeSystemForWorkspace(workspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.PutSystem(ctx, workspace.ID, canonical)
	if err != nil {
		t.Fatal(err)
	}

	upload := func(token string) DeploymentArtifact {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/"+workspace.ID+"/artifacts", bytes.NewReader(testApplicationArtifact(t)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("X-Canter-Filename", "application.tar.gz")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusCreated {
			t.Fatalf("upload status %d: %s", res.Code, res.Body.String())
		}
		var artifact DeploymentArtifact
		if err := json.Unmarshal(res.Body.Bytes(), &artifact); err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	artifact := upload(pair.AccessToken)

	workspaceRecord, err := store.Workspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	changesPath := "/v1/workspaces/" + workspace.ID + "/systems/" + canonical.Metadata.Name + "/changes"
	draft := requestJSONWithBearer(t, handler, changesPath, stagedChangeRequestBody(canonical.Metadata.Name, artifact.SHA256), pair.AccessToken)
	if draft.Code != http.StatusCreated {
		t.Fatalf("staged draft status %d: %s", draft.Code, draft.Body.String())
	}
	if bytes.Contains(draft.Body.Bytes(), []byte("control-plane/artifacts")) || bytes.Contains(draft.Body.Bytes(), []byte(artifact.SHA256[:12]+".tar.gz")) {
		t.Fatalf("staged draft response leaked a storage key: %s", draft.Body.String())
	}
	var change sdk.Change
	if err := json.Unmarshal(draft.Body.Bytes(), &change); err != nil {
		t.Fatal(err)
	}
	if change.Phase != "drafted" || change.Plan.StagedArtifactSize != artifact.Size || change.Plan.Release.ArtifactSHA != artifact.SHA256 {
		t.Fatalf("unexpected staged change: %#v", change)
	}
	if engine.stagedCalls != 1 || engine.legacyCalls != 0 {
		t.Fatalf("engine calls staged=%d legacy=%d", engine.stagedCalls, engine.legacyCalls)
	}
	if engine.lastArtifact.SHA256 != artifact.SHA256 || engine.lastArtifact.Size != artifact.Size || engine.lastRequest.Spec.Release.ArtifactSHA256 != artifact.SHA256 || engine.lastSystem.Metadata.Name != canonical.Metadata.Name {
		t.Fatalf("staged engine received wrong inputs: %#v %#v", engine.lastArtifact, engine.lastRequest.Spec.Release)
	}
	if !engine.lastActorFound || engine.lastActor.Kind != "agent" || engine.lastActor.ID != pair.Installation.ID {
		t.Fatalf("staged engine lost actor context: %#v", engine.lastActor)
	}
	if !engine.lastBaseFound || engine.lastBase.WorkspaceID != workspace.ID || engine.lastBase.WorkspaceRevision != workspaceRecord.Revision || engine.lastBase.SystemRevision != record.Revision {
		t.Fatalf("staged engine lost base revision context: %#v", engine.lastBase)
	}

	// The drafting agent has no apply authority; only a human or an
	// automatic-write agent may approve the exact digest.
	agentAuthorize := requestJSONWithBearer(t, handler, changesPath+"/"+change.ID+"/authorize", map[string]string{"digest": change.Digest}, pair.AccessToken)
	if agentAuthorize.Code != http.StatusForbidden {
		t.Fatalf("agent authorized its own staged change: %d %s", agentAuthorize.Code, agentAuthorize.Body.String())
	}

	// A second workspace holding a fully authorized agent cannot reference the
	// owner's upload until it stages the bytes itself.
	otherAccount, otherWorkspace, _, err := store.Signup(ctx, "staged-other@example.com", "correct horse battery staple", "", false)
	if err != nil {
		t.Fatal(err)
	}
	otherPair := stagedReleaseDevice(t, store, otherAccount, otherWorkspace)
	otherCanonical, err := canonicalizeSystemForWorkspace(otherWorkspace.ID, system)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutSystem(ctx, otherWorkspace.ID, otherCanonical); err != nil {
		t.Fatal(err)
	}
	foreign := requestJSONWithBearer(t, handler, "/v1/workspaces/"+otherWorkspace.ID+"/systems/"+otherCanonical.Metadata.Name+"/changes", stagedChangeRequestBody(otherCanonical.Metadata.Name, artifact.SHA256), otherPair.AccessToken)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign workspace referenced the owner's artifact: %d %s", foreign.Code, foreign.Body.String())
	}

	missing := stagedChangeRequestBody(canonical.Metadata.Name, strings.Repeat("b", 64))
	if res := requestJSONWithBearer(t, handler, changesPath, missing, pair.AccessToken); res.Code != http.StatusNotFound {
		t.Fatalf("missing digest status %d: %s", res.Code, res.Body.String())
	}

	wrongCommand := stagedChangeRequestBody(canonical.Metadata.Name, artifact.SHA256)
	wrongCommand["spec"].(map[string]any)["release"].(map[string]any)["command"] = []string{"./missing"}
	if res := requestJSONWithBearer(t, handler, changesPath, wrongCommand, pair.AccessToken); res.Code == http.StatusCreated {
		t.Fatal("non-executable release command reached drafting")
	}

	reserved := stagedChangeRequestBody(canonical.Metadata.Name, artifact.SHA256)
	reserved["spec"].(map[string]any)["release"].(map[string]any)["environment"] = map[string]string{"CANTER_SECRET": "x"}
	if res := requestJSONWithBearer(t, handler, changesPath, reserved, pair.AccessToken); res.Code == http.StatusCreated {
		t.Fatal("reserved environment name reached drafting")
	}

	localPath := stagedChangeRequestBody(canonical.Metadata.Name, "")
	localPath["spec"].(map[string]any)["release"] = map[string]any{"artifact": "release.tar.gz", "command": []string{"./app"}, "healthPath": "/health", "publicPort": 8080}
	if res := requestJSONWithBearer(t, handler, changesPath, localPath, pair.AccessToken); res.Code == http.StatusCreated {
		t.Fatal("hosted drafting accepted a local artifact path")
	}

	withMigration := stagedChangeRequestBody(canonical.Metadata.Name, artifact.SHA256)
	withMigration["spec"].(map[string]any)["migration"] = map[string]any{"path": "migration.sql", "id": "expand-v1", "service": "database"}
	if res := requestJSONWithBearer(t, handler, changesPath, withMigration, pair.AccessToken); res.Code == http.StatusCreated {
		t.Fatal("hosted staged drafting accepted a migration")
	}

	if engine.stagedCalls != 1 || engine.legacyCalls != 0 {
		t.Fatalf("rejected requests reached an engine: staged=%d legacy=%d", engine.stagedCalls, engine.legacyCalls)
	}
}
