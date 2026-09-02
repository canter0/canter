package controlplane

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func archiveWithHeaders(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for index := range headers {
		header := headers[index]
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte{'x'}, int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func archiveWithDeclaredSize(t *testing.T, size int64) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "huge", Mode: 0o750, Size: size, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestInitialDeploymentDigestBindsSystemArtifactReleaseVerificationAndRevision(t *testing.T) {
	system, err := sdk.NewSystem("digest-app", "Test immutable first deployment").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/digest-app").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	plan := InitialDeploymentPlan{System: system, ArtifactSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Release: InitialDeploymentRelease{Command: []string{"./app"}, Environment: map[string]string{"MODE": "production"}, HealthPath: "/health", PublicPort: 8080}, Verification: sdk.ChangeVerification{Method: "GET", Path: "/ready", ExpectedStatus: 200, BodyContains: "ready"}, WorkspaceRevision: 4}
	original, err := digestInitialDeployment(plan)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*InitialDeploymentPlan){
		func(p *InitialDeploymentPlan) {
			p.ArtifactSHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		func(p *InitialDeploymentPlan) { p.Release.Command = []string{"./other"} },
		func(p *InitialDeploymentPlan) { p.Release.Environment["MODE"] = "other" },
		func(p *InitialDeploymentPlan) { p.Verification.BodyContains = "different" },
		func(p *InitialDeploymentPlan) { p.WorkspaceRevision++ },
		func(p *InitialDeploymentPlan) { p.ReplacesDeploymentID = "dep_failed_legacy" },
		func(p *InitialDeploymentPlan) { p.System.Spec.Constraints.Host.Class = "larger" },
	}
	for index, mutate := range mutations {
		copyPlan := plan
		copyPlan.Release.Command = append([]string(nil), plan.Release.Command...)
		copyPlan.Release.Environment = map[string]string{"MODE": plan.Release.Environment["MODE"]}
		mutate(&copyPlan)
		changed, err := digestInitialDeployment(copyPlan)
		if err != nil {
			t.Fatal(err)
		}
		if changed == original {
			t.Fatalf("mutation %d was not bound into digest", index)
		}
	}
}

func TestMCPPublishesInitialDeploymentTools(t *testing.T) {
	wanted := map[string]bool{
		"canter_upload_artifact":                      false,
		"canter_draft_initial_deployment":             false,
		"canter_inspect_initial_deployment":           false,
		"canter_inspect_initial_deployment_execution": false,
	}
	for _, tool := range mcpTools() {
		if _, ok := wanted[tool.Name]; ok {
			wanted[tool.Name] = true
		}
	}
	for name, found := range wanted {
		if !found {
			t.Fatalf("MCP tool %s is missing", name)
		}
	}
	for _, tool := range mcpTools() {
		if tool.Name == "canter_authorize_initial_deployment" || tool.Name == "canter_apply_initial_deployment" || tool.Name == "canter_authorize_change" || tool.Name == "canter_apply_change" {
			t.Fatalf("MCP must not expose human approval capability %s", tool.Name)
		}
	}
}

func TestMCPConstrainsInitialDeploymentHostClass(t *testing.T) {
	raw, err := json.Marshal(mcpTools())
	if err != nil {
		t.Fatal(err)
	}
	contract := string(raw)
	if !strings.Contains(contract, `"class":{"description":"Provider-neutral Canter compute class.`) || !strings.Contains(contract, `"enum":["c1","c2","c3"]`) {
		t.Fatalf("MCP did not publish the supported host-class enum: %s", contract)
	}
}

func TestMCPUnsupportedHostClassErrorIsStructuredAndNotRetryable(t *testing.T) {
	broken := sdk.System{
		APIVersion: sdk.APIVersion,
		Kind:       "System",
		Metadata:   sdk.Metadata{Name: "broken"},
		Spec: sdk.SystemContract{
			Intent:      "prove invalid classes fail before deployment",
			Constraints: sdk.Constraints{Host: sdk.HostConstraint{Class: "shared", Count: 1, MemoryMiB: 512, SystemReserve: 128}},
			M1:          sdk.M1Spec{Prefix: "systems/broken"},
			Services:    []sdk.SystemService{{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}},
		},
	}
	result := publicMCPToolError(broken.Validate())
	if result["errorCode"] != "unsupported_compute_class" || result["retryable"] != false {
		t.Fatalf("unsupported class error is not machine-actionable: %#v", result)
	}
	if got, ok := result["supportedHostClasses"].([]string); !ok || len(got) != 3 || got[0] != "c1" {
		t.Fatalf("unsupported class error omitted alternatives: %#v", result)
	}
}

func TestLegacyUnsupportedClassCorrectionRequiresProofOfNoRuntimeMutation(t *testing.T) {
	legacy := InitialDeployment{
		Phase: "failed",
		Plan:  InitialDeploymentPlan{System: sdk.System{Spec: sdk.SystemContract{Constraints: sdk.Constraints{Host: sdk.HostConstraint{Class: "shared"}}}}},
		Operations: []InitialDeploymentOperation{
			{ID: "01-register-system", Phase: "succeeded"},
			{ID: "02-bootstrap-host", Phase: "failed", Failure: `unsupported compute class "shared"`},
			{ID: "03-publish-release", Phase: "pending"},
			{ID: "04-wait-healthy", Phase: "pending"},
			{ID: "05-verify-public", Phase: "pending"},
		},
	}
	if !failedBeforeRuntimeMutationForUnsupportedClass(legacy) {
		t.Fatal("exact legacy pre-runtime class failure was not recognized")
	}
	mutated := legacy
	mutated.Operations = append([]InitialDeploymentOperation(nil), legacy.Operations...)
	mutated.Operations[2].Phase = "succeeded"
	if failedBeforeRuntimeMutationForUnsupportedClass(mutated) {
		t.Fatal("proposal with a later side effect was considered safe to replace")
	}
	providerFailure := legacy
	providerFailure.Operations = append([]InitialDeploymentOperation(nil), legacy.Operations...)
	providerFailure.Operations[1].Failure = "provider request failed"
	if failedBeforeRuntimeMutationForUnsupportedClass(providerFailure) {
		t.Fatal("provider failure was considered an unsupported-class correction")
	}
}

func TestBlackoutBootstrapCapabilityIsSelfDescribing(t *testing.T) {
	capabilities := initialDeploymentCapabilities("wrk_blackout")
	raw, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	contract := string(raw)
	for _, required := range []string{
		`"format":"tar.gz"`, `"maxBytes":67108864`, `"form":"argv"`, `./-prefixed executable`,
		`"PORT"`, `CANTER_SERVICE_`, `/v1/workspaces/wrk_blackout/artifacts`,
		`/initial-deployments/{deploymentId}/authorize`, `"principal":"human"`,
		`"url":"/mcp"`, `canter_draft_initial_deployment`, `"hostClasses":["c1","c2","c3"]`, `approve + start deployment`, `transfers the original beta usage reservation`,
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("blackout capability contract omitted %q: %s", required, contract)
		}
	}
	for _, forbidden := range []string{`canter_authorize_initial_deployment`, `canter_apply_initial_deployment`} {
		if strings.Contains(contract, forbidden) {
			t.Fatalf("blackout capability contract advertised unavailable MCP tool %q: %s", forbidden, contract)
		}
	}
}

func TestArtifactManifestPreservesExecutableMode(t *testing.T) {
	entries, err := validateApplicationArtifact(testApplicationArtifact(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "app" || entries[0].Mode&0o100 == 0 {
		t.Fatalf("uploaded artifact did not expose a runnable command capability: %#v", entries)
	}
}

func TestArtifactValidationRejectsAmbiguousOrDangerousArchives(t *testing.T) {
	cases := map[string][]tar.Header{
		"duplicate": {
			{Name: "app", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg},
			{Name: "app", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg},
		},
		"noncanonical": {{Name: "./app", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg}},
		"traversal":    {{Name: "../app", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg}},
		"backslash":    {{Name: `bin\app`, Mode: 0o750, Size: 1, Typeflag: tar.TypeReg}},
		"path-limit":   {{Name: strings.Repeat("a", maxArtifactPathBytes+1), Mode: 0o750, Size: 1, Typeflag: tar.TypeReg}},
		"header-limit": {{Name: "app", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg, PAXRecords: map[string]string{"comment": strings.Repeat("x", maxArtifactMetadataBytes+1)}}},
		"symlink":      {{Name: "app", Mode: 0o750, Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}},
		"file-parent": {
			{Name: "bin", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg},
			{Name: "bin/app", Mode: 0o750, Size: 1, Typeflag: tar.TypeReg},
		},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := validateApplicationArtifact(archiveWithHeaders(t, headers...)); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
		})
	}
}

func TestArtifactValidationCapsDeclaredExpansion(t *testing.T) {
	if _, err := validateApplicationArtifact(archiveWithDeclaredSize(t, maxExpandedArtifactBytes+1)); err == nil || !strings.Contains(err.Error(), "expands beyond") {
		t.Fatalf("declared expansion limit was not enforced: %v", err)
	}
}

func TestArtifactValidationCapsEntryCount(t *testing.T) {
	headers := make([]tar.Header, maxArtifactEntries+1)
	for index := range headers {
		headers[index] = tar.Header{Name: fmt.Sprintf("dir-%04d/", index), Mode: 0o750, Typeflag: tar.TypeDir}
	}
	if _, err := validateApplicationArtifact(archiveWithHeaders(t, headers...)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("entry limit was not enforced: %v", err)
	}
}
