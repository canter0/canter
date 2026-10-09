package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
