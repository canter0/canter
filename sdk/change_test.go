package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

func TestHTTPVerificationFailureDoesNotPersistTenantResponseOrURL(t *testing.T) {
	const responseMarker = "synthetic-tenant-response-secret"
	const queryMarker = "synthetic-verification-query-secret"
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, responseMarker)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	system, err := NewSystem("verify-api", "verify the public application endpoint").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/verify-api").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	computeClient := newFakeCompute(store, stateKey(SystemHostSpec(system, "true")))
	computeClient.servers["server-1"] = []compute.Server{{
		ID: "server-1", Status: "ACTIVE",
		Addresses: map[string][]compute.Address{"public": {{Addr: "127.0.0.1", Version: 4}}},
	}}
	if err = store.PutJSON(context.Background(), computeClient.stateKey, State{Sandbox: system.Metadata.Name, Phase: "running", Resources: []Resource{{ID: "server-1", Status: "ACTIVE"}}}); err != nil {
		t.Fatal(err)
	}
	client := &Client{m1: store, compute: computeClient}
	change := Change{Plan: ChangePlan{
		Release:      ReleaseManifest{PublicPort: listener.Addr().(*net.TCPAddr).Port},
		Verification: ChangeVerification{Method: http.MethodGet, Path: "/health?token=" + queryMarker, ExpectedStatus: http.StatusOK},
	}}
	_, err = client.executeChangeOperation(context.Background(), system, &change, &ChangeOperation{Kind: "http.verify"}, nil)
	if err == nil {
		t.Fatal("verification unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), responseMarker) || strings.Contains(err.Error(), queryMarker) {
		t.Fatalf("verification error retained tenant response or URL data: %v", err)
	}
	if !strings.Contains(err.Error(), "status=500") {
		t.Fatalf("verification error omitted useful status evidence: %v", err)
	}
}

func TestInspectChangeRejectsUnsafeSystemPrefix(t *testing.T) {
	system, err := NewSystem("inspect-api", "inspect changes safely").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/inspect-api").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	system.Spec.M1.Prefix = "systems/../other"
	client := &Client{m1: newFakeStore()}
	if _, err := client.InspectChange(context.Background(), system, "change-123"); err == nil || !strings.Contains(err.Error(), "m1 prefix") {
		t.Fatal("InspectChange accepted a system with an unsafe object prefix")
	}
}

func TestInspectChangeBindsStoredIdentityAndReleaseObjectKeys(t *testing.T) {
	ctx := context.Background()
	system, err := NewSystem("inspect-api", "inspect changes safely").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/inspect-api").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	client := &Client{m1: store}
	valid := Change{SchemaVersion: "v1", ID: "change-one", System: system.Metadata.Name, Plan: ChangePlan{
		BaseVersion: "release-old", Release: ReleaseManifest{System: system.Metadata.Name, Version: "release-new"},
	}}

	if err := store.PutJSON(ctx, changeKey(system, "change-one"), valid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectChange(ctx, system, "change-one"); err != nil {
		t.Fatalf("valid stored change was rejected: %v", err)
	}

	valid.ID = "change-two"
	if err := store.PutJSON(ctx, changeKey(system, "change-one"), valid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectChange(ctx, system, "change-one"); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("stored change identity mismatch was accepted: %v", err)
	}

	valid.ID = "change-one"
	for _, version := range []string{"../../outside", `..\\outside`, "release?version=1", ".", ".."} {
		valid.Plan.BaseVersion = version
		if err := store.PutJSON(ctx, changeKey(system, "change-one"), valid); err != nil {
			t.Fatal(err)
		}
		if _, err := client.InspectChange(ctx, system, "change-one"); err == nil || !strings.Contains(err.Error(), "release identity") {
			t.Fatalf("unsafe compensation release key %q was accepted: %v", version, err)
		}
	}

	valid.Plan.BaseVersion = "release-old"
	valid.Plan.Release.System = "other-system"
	if err := store.PutJSON(ctx, changeKey(system, "change-one"), valid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectChange(ctx, system, "change-one"); err == nil || !strings.Contains(err.Error(), "release identity") {
		t.Fatalf("cross-system release identity was accepted: %v", err)
	}

	// Stage an artifact whose actual SHA-derived release version begins with a
	// digit, then ensure the resulting plan identity remains inspectable.
	var artifact []byte
	for i := 0; i < 1000; i++ {
		candidate := []byte(fmt.Sprintf("numeric-leading-release-%d", i))
		sum := sha256.Sum256(candidate)
		if sum[0] >= '0' && sum[0] <= '9' {
			artifact = candidate
			break
		}
	}
	if artifact == nil {
		t.Fatal("could not derive a numeric-leading SHA-256 version")
	}
	artifactPath := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(artifactPath, artifact, 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := client.StageRelease(ctx, system, PublishReleaseInput{ArtifactPath: artifactPath, Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	sha := sha256.Sum256(artifact)
	if manifest.Version != hex.EncodeToString(sha[:])[:12] || manifest.Version[0] < '0' || manifest.Version[0] > '9' {
		t.Fatalf("expected numeric-leading SHA version, got %q", manifest.Version)
	}
	valid.Plan.BaseVersion = manifest.Version
	valid.Plan.Release = manifest
	if err := store.PutJSON(ctx, changeKey(system, "change-one"), valid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InspectChange(ctx, system, "change-one"); err != nil {
		t.Fatalf("numeric-leading SHA release was rejected: %v", err)
	}
}

func stagedReleaseFixture(t *testing.T) (*Client, *fakeStore, System, ReleaseManifest, StagedArtifact) {
	t.Helper()
	ctx := context.Background()
	store := newFakeStore()
	client := &Client{m1: store}
	system, err := NewSystem("staged-api", "serve staged releases").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/staged-api").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.Repeat("a", 64)
	base := ReleaseManifest{SchemaVersion: "v1", System: system.Metadata.Name, Version: "base1234abcd", ArtifactKey: "systems/staged-api/artifacts/" + baseSHA + ".tar.gz", ArtifactSHA: baseSHA, Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080, Replicas: 1, RequestedAt: time.Now().UTC()}
	if err := store.PutJSON(ctx, desiredKey(system), base); err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON(ctx, releaseKey(system, base.Version), base); err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON(ctx, observedKey(system), ObservedRelease{SchemaVersion: "v1", System: system.Metadata.Name, Phase: "running", DesiredVersion: base.Version, RunningVersion: base.Version, PID: 10, ReplicaPIDs: []int{10}, DesiredReplicas: 1, ReadyReplicas: 1, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	staged, err := client.StageControlPlaneArtifact(ctx, []byte("staged application bytes"), "app.tar.gz", "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	return client, store, system, base, staged
}

func stagedReleaseRequest(system System, sha string) ChangeRequest {
	return ChangeRequest{APIVersion: APIVersion, Kind: "Change", Metadata: Metadata{Name: "ship-staged"}, Spec: ChangeRequestSpec{
		System: system.Metadata.Name, Summary: "Ship the staged artifact",
		Release:      &ChangeRequestRelease{ArtifactSHA256: sha, Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080},
		Verification: ChangeRequestVerification{Method: "GET", Path: "/version", ExpectedStatus: 200},
	}}
}

func TestDraftStagedRelease(t *testing.T) {
	ctx := context.Background()
	client, store, system, base, staged := stagedReleaseFixture(t)

	request := stagedReleaseRequest(system, staged.SHA256)
	change, err := client.DraftStagedChangeRequest(ctx, system, request, staged)
	if err != nil {
		t.Fatal(err)
	}
	version := staged.SHA256[:12]
	if change.Plan.Release.Version != version || change.Plan.Release.ArtifactSHA != staged.SHA256 || change.Plan.Release.ArtifactKey != staged.Key || change.Plan.BaseVersion != base.Version || change.Plan.StagedArtifactSize != staged.Size {
		t.Fatalf("unexpected staged plan: %#v", change.Plan)
	}
	// The unapproved candidate must not become a retained manifest or desired state.
	var candidate ReleaseManifest
	if found, err := store.GetOptional(ctx, releaseKey(system, version), &candidate); err != nil || found {
		t.Fatalf("unapproved staged release was persisted: found=%t err=%v", found, err)
	}
	var desired ReleaseManifest
	if err := store.Get(ctx, desiredKey(system), &desired); err != nil || desired.Version != base.Version {
		t.Fatalf("draft mutated desired release: %#v err=%v", desired, err)
	}

	mismatched := staged
	mismatched.SHA256 = strings.Repeat("c", 64)
	if _, err := client.DraftStagedChangeRequest(ctx, system, request, mismatched); err == nil {
		t.Fatal("request digest and staged artifact digest mismatch was accepted")
	}
	withPath := stagedReleaseRequest(system, staged.SHA256)
	withPath.Spec.Release.Artifact = "release.tar.gz"
	if _, err := client.DraftStagedChangeRequest(ctx, system, withPath, staged); err == nil {
		t.Fatal("staged request carrying a local artifact path was accepted")
	}
	withMigration := stagedReleaseRequest(system, staged.SHA256)
	withMigration.Spec.Migration = &ChangeRequestMigration{Path: "migration.sql", ID: "expand-v1", Service: "database"}
	if _, err := client.DraftStagedChangeRequest(ctx, system, withMigration, staged); err == nil {
		t.Fatal("staged release combined with a migration was accepted")
	}
	if _, err := request.DraftInput(system); err == nil {
		t.Fatal("local draft input accepted a workspace digest without an upload")
	}

	if _, err := client.AuthorizeChange(ctx, system, change.ID, strings.Repeat("f", 64)); err == nil {
		t.Fatal("wrong authorization digest was accepted")
	}
	authorized, err := client.AuthorizeChange(ctx, system, change.ID, change.Digest)
	if err != nil || authorized.Phase != "authorized" {
		t.Fatalf("exact digest was not authorized: %#v %v", authorized, err)
	}
	statement, err := client.executeChangeOperation(ctx, system, &authorized, &ChangeOperation{Kind: "state.assert"}, nil)
	if err != nil || statement != "base release remained healthy and unchanged" {
		t.Fatalf("pre-apply assert rejected the verified staged artifact: %q %v", statement, err)
	}
}

func TestStagedReleaseIntegrityBeforeDesired(t *testing.T) {
	ctx := context.Background()
	client, store, system, base, staged := stagedReleaseFixture(t)

	change, err := client.DraftStagedChangeRequest(ctx, system, stagedReleaseRequest(system, staged.SHA256), staged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthorizeChange(ctx, system, change.ID, change.Digest); err != nil {
		t.Fatal(err)
	}

	// Replace the approved bytes with different content of the same size. The
	// authorized digest covers the staged size, so only the content check can
	// stop the mutation from reaching desired state.
	corrupted := []byte("staged application bytex")
	if int64(len(corrupted)) != staged.Size {
		t.Fatal("corrupted fixture must keep the approved size")
	}
	if err := store.Put(ctx, staged.Key, corrupted, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	result, err := client.ApplyChange(ctx, system, change.ID)
	if err == nil {
		t.Fatal("apply executed a tampered staged artifact")
	}
	if result.Phase != "rejected" {
		t.Fatalf("tampered apply did not stop before mutation: phase=%q err=%v", result.Phase, err)
	}
	var desired ReleaseManifest
	if err := store.Get(ctx, desiredKey(system), &desired); err != nil || desired.Version != base.Version {
		t.Fatalf("rejected apply advanced desired release: %#v err=%v", desired, err)
	}
	var candidate ReleaseManifest
	if found, err := store.GetOptional(ctx, releaseKey(system, staged.SHA256[:12]), &candidate); err != nil || found {
		t.Fatalf("rejected apply persisted the candidate manifest: found=%t err=%v", found, err)
	}
}

// stagedReleaseObservedStore stands in for the node: whenever the executor
// publishes an approved desired release it writes the matching
// ObservedRelease, so ApplyChange's wait/verify steps run the real operation
// executor end to end. A manifest whose version equals unhealthyVersion is
// reported as a failed candidate that keeps the previous release running.
type stagedReleaseObservedStore struct {
	*fakeStore
	system           System
	unhealthyVersion string
}

func (s *stagedReleaseObservedStore) PutJSON(ctx context.Context, key string, value any) error {
	if err := s.fakeStore.PutJSON(ctx, key, value); err != nil {
		return err
	}
	manifest, ok := value.(ReleaseManifest)
	if !ok || key != desiredKey(s.system) {
		return nil
	}
	if manifest.Version == s.unhealthyVersion {
		var previous ObservedRelease
		if err := s.fakeStore.Get(ctx, observedKey(s.system), &previous); err != nil {
			return err
		}
		return s.fakeStore.PutJSON(ctx, observedKey(s.system), ObservedRelease{SchemaVersion: "v1", System: s.system.Metadata.Name, Phase: "release-failed", DesiredVersion: manifest.Version, RunningVersion: previous.RunningVersion, Healthy: true, Message: "fixture candidate unhealthy"})
	}
	return s.fakeStore.PutJSON(ctx, observedKey(s.system), ObservedRelease{SchemaVersion: "v1", System: s.system.Metadata.Name, Phase: "running", DesiredVersion: manifest.Version, RunningVersion: manifest.Version, DesiredReplicas: manifest.Replicas, ReadyReplicas: manifest.Replicas, Healthy: true})
}

func TestApplyStagedReleaseCommitsAndReverts(t *testing.T) {
	ctx := context.Background()
	inner := newFakeStore()
	store := &stagedReleaseObservedStore{fakeStore: inner}

	var system System
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		var observed ObservedRelease
		if err := inner.Get(r.Context(), observedKey(system), &observed); err != nil {
			http.Error(w, "observed release unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"version":%q}`, observed.RunningVersion)
	}))
	defer endpoint.Close()
	port, err := strconv.Atoi(endpoint.URL[strings.LastIndex(endpoint.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	system, err = NewSystem("apply-staged", "apply staged releases").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/apply-staged").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: port}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	store.system = system

	baseVersion := strings.Repeat("b", 12)
	base := ReleaseManifest{SchemaVersion: "v1", System: system.Metadata.Name, Version: baseVersion, ArtifactSHA: strings.Repeat("b", 64), Command: []string{"./app"}, HealthPath: "/health", PublicPort: port, Replicas: 1, RequestedAt: time.Now().UTC()}
	if err := inner.PutJSON(ctx, desiredKey(system), base); err != nil {
		t.Fatal(err)
	}
	if err := inner.PutJSON(ctx, releaseKey(system, baseVersion), base); err != nil {
		t.Fatal(err)
	}
	if err := inner.PutJSON(ctx, observedKey(system), ObservedRelease{SchemaVersion: "v1", System: system.Metadata.Name, Phase: "running", DesiredVersion: baseVersion, RunningVersion: baseVersion, DesiredReplicas: 1, ReadyReplicas: 1, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	hostSpec := SystemHostSpec(system, "true")
	computeClient := newFakeCompute(inner, stateKey(hostSpec))
	computeClient.servers["server-1"] = []compute.Server{{
		ID: "server-1", Status: "ACTIVE",
		Addresses: map[string][]compute.Address{"public": {{Addr: "127.0.0.1", Version: 4}}},
	}}
	if err := inner.PutJSON(ctx, computeClient.stateKey, State{Sandbox: system.Metadata.Name, Phase: "running", Resources: []Resource{{ID: "server-1", Status: "ACTIVE"}}}); err != nil {
		t.Fatal(err)
	}
	client := &Client{m1: store, compute: computeClient}

	stagedRequest := func(staged StagedArtifact) ChangeRequest {
		request := stagedReleaseRequest(system, staged.SHA256)
		request.Spec.Release.PublicPort = port
		request.Spec.Verification.BodyContains = staged.SHA256[:12]
		return request
	}

	stagedV2, err := client.StageControlPlaneArtifact(ctx, []byte("healthy staged release v2"), "app.tar.gz", "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	change, err := client.DraftStagedChangeRequest(ctx, system, stagedRequest(stagedV2), stagedV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthorizeChange(ctx, system, change.ID, change.Digest); err != nil {
		t.Fatal(err)
	}
	result, err := client.ApplyChange(ctx, system, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != "committed" {
		t.Fatalf("approved staged release did not commit: phase=%q", result.Phase)
	}
	verifyEvidence := false
	for _, evidence := range result.Evidence {
		if evidence.Kind == "http.verify" {
			verifyEvidence = true
		}
	}
	if !verifyEvidence {
		t.Fatalf("committed staged release lacks http.verify evidence: %#v", result.Evidence)
	}
	var desired ReleaseManifest
	if err := inner.Get(ctx, desiredKey(system), &desired); err != nil || desired.ArtifactSHA != stagedV2.SHA256 || desired.Version != stagedV2.SHA256[:12] {
		t.Fatalf("desired release was not the approved staged manifest: %#v err=%v", desired, err)
	}
	var retained ReleaseManifest
	if err := inner.Get(ctx, releaseKey(system, desired.Version), &retained); err != nil || retained.ArtifactSHA != stagedV2.SHA256 {
		t.Fatalf("approved release manifest was not retained: %#v err=%v", retained, err)
	}

	stagedBad, err := client.StageControlPlaneArtifact(ctx, []byte("unhealthy staged release v3"), "app.tar.gz", "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	store.unhealthyVersion = stagedBad.SHA256[:12]
	badChange, err := client.DraftStagedChangeRequest(ctx, system, stagedRequest(stagedBad), stagedBad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AuthorizeChange(ctx, system, badChange.ID, badChange.Digest); err != nil {
		t.Fatal(err)
	}
	reverted, err := client.ApplyChange(ctx, system, badChange.ID)
	if err == nil || !strings.Contains(err.Error(), "reverted") {
		t.Fatalf("unhealthy candidate apply should report reversion: result=%#v err=%v", reverted, err)
	}
	if reverted.Phase != "reverted" {
		t.Fatalf("unhealthy candidate did not revert: phase=%q", reverted.Phase)
	}
	waitFailed, restored := false, false
	for _, operation := range reverted.Operations {
		if operation.Kind == "release.wait-healthy" && operation.Phase == "failed" {
			waitFailed = true
		}
	}
	for _, evidence := range reverted.Evidence {
		if evidence.Kind == "release.restore" {
			restored = true
		}
	}
	if !waitFailed || !restored {
		t.Fatalf("reverted change lacks wait-healthy failure or release.restore evidence: %#v %#v", reverted.Operations, reverted.Evidence)
	}
	if err := inner.Get(ctx, desiredKey(system), &desired); err != nil || desired.Version != stagedV2.SHA256[:12] {
		t.Fatalf("revert did not restore the prior desired release: %#v err=%v", desired, err)
	}
	var observed ObservedRelease
	if err := inner.Get(ctx, observedKey(system), &observed); err != nil || observed.RunningVersion != stagedV2.SHA256[:12] || observed.Phase != "running" || !observed.Healthy {
		t.Fatalf("observed state did not stay on the healthy release: %#v err=%v", observed, err)
	}
	response, err := http.Get(endpoint.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var version struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil || response.StatusCode != http.StatusOK || version.Version != stagedV2.SHA256[:12] {
		t.Fatalf("public endpoint did not serve the healthy release: status=%d version=%q err=%v", response.StatusCode, version.Version, err)
	}
}

func TestExpandMigrationValidatorAcceptsOnlyIdempotentExpansion(t *testing.T) {
	valid := `
ALTER TABLE posts ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS posts_archived_at_idx ON posts(archived_at);
CREATE TABLE IF NOT EXISTS audit_events (id BIGSERIAL PRIMARY KEY);
`
	if err := validateExpandMigration(valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`DROP TABLE users;`,
		`ALTER TABLE users RENAME COLUMN email TO address;`,
		`ALTER TABLE posts ADD COLUMN archived_at TIMESTAMPTZ;`,
		`UPDATE users SET email='x';`,
	} {
		if err := validateExpandMigration(invalid); err == nil {
			t.Fatalf("unsafe migration was accepted: %s", invalid)
		}
	}
}

func TestChangeDigestBindsReleaseEnvironmentAndVerification(t *testing.T) {
	plan := ChangePlan{BaseRevision: ChangeBaseRevision{WorkspaceID: "wrk_1", WorkspaceRevision: 7, SystemRevision: 3}, BaseVersion: "old", Release: ReleaseManifest{Version: "new", ArtifactSHA: "abc", Environment: map[string]string{"FEATURE": "false"}}, Verification: ChangeVerification{Method: "GET", Path: "/proof", ExpectedStatus: 200, BodyContains: "ready"}}
	operations := []ChangeOperation{{ID: "01", Kind: "release.set-desired", Description: "deploy", Reversibility: "compensatable"}}
	first, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	plan.Release.Environment["FEATURE"] = "true"
	second, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("environment mutation did not change authorization digest")
	}
	plan.Verification.BodyContains = "different"
	third, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	if second == third {
		t.Fatal("verification mutation did not change authorization digest")
	}
	operations[0].Kind = "unexpected.mutation"
	fourth, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	if third == fourth {
		t.Fatal("operation program mutation did not change authorization digest")
	}
	plan.Impact.MonthlyCostDeltaCents = 500
	fifth, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	if fourth == fifth {
		t.Fatal("impact mutation did not change authorization digest")
	}
	plan.BaseRevision.SystemRevision++
	sixth, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	if fifth == sixth {
		t.Fatal("semantic base revision mutation did not change authorization digest")
	}
}

func TestChangeDigestBindsReplicaTransition(t *testing.T) {
	plan := ChangePlan{BaseVersion: "release-one", Release: ReleaseManifest{Version: "release-one", Replicas: 3}, Scale: &ReplicaScalePlan{Service: "web", FromReplicas: 1, ToReplicas: 3, CapacityMode: "existing-host"}}
	operations := []ChangeOperation{{ID: "02-scale", Kind: "release.scale", Description: "scale web", Reversibility: "compensatable"}}
	first, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	plan.Scale.ToReplicas = 4
	plan.Release.Replicas = 4
	second, err := digestChange(plan, operations)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("replica target mutation did not change authorization digest")
	}
}

func TestScaleCapacityIsBoundedToExistingHostMemory(t *testing.T) {
	system, err := NewSystem("capacity-api", "serve within allocated capacity").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/capacity-api").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).
		Provide(SystemService{Name: "database", Kind: "database", Engine: "postgres", Isolation: "process", Instances: 1, Resources: ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: Readiness{Protocol: "tcp", Port: 5432}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	service, maximum, err := ScaleCapacity(system, "web")
	if err != nil {
		t.Fatal(err)
	}
	if service.Name != "web" || maximum != 4 {
		t.Fatalf("service=%s maximum=%d", service.Name, maximum)
	}
	if _, _, err = ScaleCapacity(system, "database"); err == nil {
		t.Fatal("database was accepted as an application replica target")
	}
}

func TestDraftScaleChangeDerivesCurrentCapacityAndCompensation(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	client := &Client{m1: store}
	system, err := NewSystem("scale-api", "serve a scalable application").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/scale-api").
		Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	release := ReleaseManifest{SchemaVersion: "v1", System: "scale-api", Version: "release-one", ArtifactSHA: "digest", Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080, Replicas: 1, RequestedAt: time.Now().UTC()}
	if err = store.PutJSON(ctx, desiredKey(system), release); err != nil {
		t.Fatal(err)
	}
	if err = store.PutJSON(ctx, observedKey(system), ObservedRelease{SchemaVersion: "v1", System: "scale-api", Phase: "running", DesiredVersion: "release-one", RunningVersion: "release-one", PID: 10, ReplicaPIDs: []int{10}, DesiredReplicas: 1, ReadyReplicas: 1, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	change, err := client.DraftScaleChange(ctx, system, DraftScaleChangeInput{Summary: "Scale for traffic", Service: "web", Replicas: 4, Verification: ChangeVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}})
	if err != nil {
		t.Fatal(err)
	}
	if change.Plan.Scale == nil || change.Plan.Scale.FromReplicas != 1 || change.Plan.Scale.ToReplicas != 4 || change.Plan.Release.Replicas != 4 {
		t.Fatalf("unexpected scale plan: %#v", change.Plan)
	}
	if change.Operations[1].Kind != "release.scale" || change.Operations[1].Compensation != "restore web to 1 replicas" {
		t.Fatalf("scale operation did not bind compensation: %#v", change.Operations)
	}
	if _, err = client.DraftScaleChange(ctx, system, DraftScaleChangeInput{Summary: "Too large", Service: "web", Replicas: 7, Verification: ChangeVerification{Path: "/health"}}); err == nil {
		t.Fatal("scale above current host capacity was accepted")
	}
	temporary, err := client.DraftScaleChange(ctx, system, DraftScaleChangeInput{Summary: "Temporary traffic burst", Service: "web", Replicas: 3, ForSeconds: 120, Verification: ChangeVerification{Path: "/health"}})
	if err != nil {
		t.Fatal(err)
	}
	if temporary.Plan.Scale == nil || temporary.Plan.Scale.LeaseSeconds != 120 || temporary.Plan.Scale.RestoreToReplicas != 1 || temporary.Plan.Scale.RestoreAt == nil || temporary.Plan.Release.CapacityLease == nil {
		t.Fatalf("temporary scale was not bound into the Change: %#v", temporary.Plan.Scale)
	}
}
