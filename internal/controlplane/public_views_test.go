package controlplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestPublicChangeRedactsSecretsWithoutMutatingExecutionDocument(t *testing.T) {
	change := sdk.Change{
		Failure:   "provider request leaked-secret failed",
		Residuals: []string{"provider-id leaked-secret"},
		Plan: sdk.ChangePlan{
			Release:   sdk.ReleaseManifest{ArtifactKey: "private/object/key", Command: []string{"./app", "leaked-secret"}, Environment: map[string]string{"API_TOKEN": "leaked-secret", "MODE": "production"}},
			Migration: &sdk.Migration{SQL: "SELECT 'leaked-secret'", Digest: strings.Repeat("a", 64)},
		},
		Operations: []sdk.ChangeOperation{{ID: "01", Failure: "leaked-secret in runtime error"}},
	}
	public := publicChange(change)
	raw, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "leaked-secret") || strings.Contains(string(raw), "private/object/key") || strings.Contains(string(raw), "artifactKey") {
		t.Fatalf("public Change leaked secret-bearing execution state: %s", raw)
	}
	if public.Plan.Release.Environment["API_TOKEN"] != redactedValue || public.Plan.Release.Command[1] != "[redacted-argument]" || public.Plan.Migration.SQL != "" {
		t.Fatalf("public Change did not redact every secret-bearing field: %#v", public)
	}
	if change.Plan.Release.Environment["API_TOKEN"] != "leaked-secret" || change.Plan.Release.Command[1] != "leaked-secret" || change.Plan.Migration.SQL == "" {
		t.Fatal("public redaction mutated the internal execution document")
	}
}

func TestPublicInitialDeploymentRedactsEnvironmentAndFailures(t *testing.T) {
	deployment := InitialDeployment{Plan: InitialDeploymentPlan{Release: InitialDeploymentRelease{Command: []string{"./app", "leaked-secret"}, Environment: map[string]string{"API_TOKEN": "leaked-secret"}}}, Operations: []InitialDeploymentOperation{{Failure: "leaked-secret"}}, Failure: "leaked-secret"}
	public := publicInitialDeployment(deployment)
	raw, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "leaked-secret") || public.Plan.Release.Environment["API_TOKEN"] != redactedValue {
		t.Fatalf("public initial deployment leaked secret-bearing state: %s", raw)
	}
	if deployment.Plan.Release.Environment["API_TOKEN"] != "leaked-secret" {
		t.Fatal("public initial deployment redaction mutated the execution document")
	}
}

func TestPublicInitialDeploymentPreservesOnlySafeActionableFailures(t *testing.T) {
	deployment := InitialDeployment{Failure: `unsupported compute class "shared"`, Operations: []InitialDeploymentOperation{
		{Failure: `unsupported compute class "shared"`},
		{Failure: "provider request leaked-secret failed"},
	}}
	public := publicInitialDeployment(deployment)
	if !strings.HasPrefix(public.Operations[0].Failure, "unsupported_compute_class:") {
		t.Fatalf("safe legacy domain failure was not upgraded: %q", public.Operations[0].Failure)
	}
	if !strings.HasPrefix(public.Failure, "unsupported_compute_class:") {
		t.Fatalf("safe top-level failure was redacted: %q", public.Failure)
	}
	if public.Operations[1].Failure != "operation failed; operator inspection required" {
		t.Fatalf("provider failure was exposed: %q", public.Operations[1].Failure)
	}
}
