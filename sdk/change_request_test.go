package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestChangeRequestSchemaIsValidJSON(t *testing.T) {
	if !json.Valid(ChangeRequestSchemaJSON()) {
		t.Fatal("embedded Change request schema is invalid")
	}
}

func TestChangeRequestCompilesToDraftInput(t *testing.T) {
	system := mysqlPairSystem(t)
	request := ChangeRequest{
		APIVersion: APIVersion, Kind: "Change", Metadata: Metadata{Name: "add-confirmations"},
		Spec: ChangeRequestSpec{
			System: system.Metadata.Name, Summary: "Add confirmations",
			Release:      &ChangeRequestRelease{Artifact: "release.tar.gz", Command: []string{"./app", "serve"}, Environment: map[string]string{"FEATURE": "true"}, HealthPath: "/health", PublicPort: 8080},
			Migration:    &ChangeRequestMigration{Path: "migration.sql", ID: "confirmations-v1", Service: "mysql"},
			Verification: ChangeRequestVerification{Method: "GET", Path: "/proof", ExpectedStatus: 200, BodyContains: "ready"},
		},
	}
	input, err := request.DraftInput(system)
	if err != nil {
		t.Fatal(err)
	}
	if input.Release.Command[1] != "serve" || input.MigrationID != "confirmations-v1" || input.Verification.Path != "/proof" {
		t.Fatalf("unexpected draft input: %+v", input)
	}
	input.Release.Environment["FEATURE"] = "mutated"
	if request.Spec.Release.Environment["FEATURE"] != "true" {
		t.Fatal("draft input aliases the reviewed request")
	}
}

func TestLoadChangeRequestRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "change.yaml")
	contents := StarterChangeYAML + "  providerCredentials: forbidden\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadChangeRequest(path); err == nil {
		t.Fatal("unknown request field was accepted")
	}
}

func TestChangeRequestCannotTargetAnotherSystem(t *testing.T) {
	system := mysqlPairSystem(t)
	request := ChangeRequest{APIVersion: APIVersion, Kind: "Change", Metadata: Metadata{Name: "safe-release"}, Spec: ChangeRequestSpec{System: "another-system", Summary: "Ship safely", Release: &ChangeRequestRelease{Artifact: "release.tar.gz", Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080}, Verification: ChangeRequestVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}}}
	if _, err := request.DraftInput(system); err == nil {
		t.Fatal("cross-system request was accepted")
	}
}

func TestScaleChangeRequestIsTypedAndExclusive(t *testing.T) {
	request := ChangeRequest{APIVersion: APIVersion, Kind: "Change", Metadata: Metadata{Name: "scale-web"}, Spec: ChangeRequestSpec{System: "web", Summary: "Add capacity", Scale: &ChangeRequestScale{Service: "web", Replicas: 3}, Verification: ChangeRequestVerification{Method: "GET", Path: "/health", ExpectedStatus: 200}}}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Spec.Release = &ChangeRequestRelease{Artifact: "release.tar.gz", Command: []string{"./app"}, HealthPath: "/health", PublicPort: 8080}
	if err := request.Validate(); err == nil {
		t.Fatal("request containing both scale and release intent was accepted")
	}
}
