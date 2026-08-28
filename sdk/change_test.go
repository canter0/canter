package sdk

import "testing"

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
	plan := ChangePlan{BaseVersion: "old", Release: ReleaseManifest{Version: "new", ArtifactSHA: "abc", Environment: map[string]string{"FEATURE": "false"}}, Verification: ChangeVerification{Method: "GET", Path: "/proof", ExpectedStatus: 200, BodyContains: "ready"}}
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
}
