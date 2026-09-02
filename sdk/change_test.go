package sdk

import (
	"context"
	"testing"
	"time"
)

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
