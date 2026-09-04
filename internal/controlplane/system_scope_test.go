package controlplane

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/canter0/canter/sdk"
)

func scopedTestSystem(t *testing.T, prefix string) sdk.System {
	t.Helper()
	system, err := sdk.NewSystem("api", "Serve an API").
		OnHost("c1", 1, 1024, 256).
		WithM1("systems/api").
		Provide(sdk.SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Resources: sdk.ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: sdk.Readiness{Protocol: "http", Port: 8080}}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	system.Spec.M1.Prefix = prefix
	return system
}

func TestCanonicalizeSystemForWorkspaceDerivesPrefixBeforePersistence(t *testing.T) {
	system, err := canonicalizeSystemForWorkspace("wrk_123", scopedTestSystem(t, "systems/api"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := system.Spec.M1.Prefix, "workspaces/wrk_123/systems/api"; got != want {
		t.Fatalf("canonical prefix=%q want %q", got, want)
	}
	if _, err := canonicalizeSystemForWorkspace("wrk_123", scopedTestSystem(t, "systems/api\nEnvironment=oops")); err == nil {
		t.Fatal("unsafe caller prefix was hidden by canonicalization")
	}
}

func TestValidateCanonicalSystemRejectsLegacyPrefixAtStoreBoundary(t *testing.T) {
	err := validateCanonicalSystemForWorkspace("wrk_123", scopedTestSystem(t, "systems/api"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("noncanonical store write returned %v, want conflict", err)
	}
}

func TestStoredLegacyUnsupportedClassRemainsReadableButCannotBeWritten(t *testing.T) {
	legacy := scopedTestSystem(t, "workspaces/wrk_123/systems/api")
	legacy.Spec.Constraints.Host.Class = "shared"
	if err := validateStoredSystemPrefix(legacy.Spec.M1.Prefix, legacy); err != nil {
		t.Fatalf("otherwise valid legacy contract became unreadable: %v", err)
	}
	if err := validateCanonicalSystemForWorkspace("wrk_123", legacy); err == nil {
		t.Fatal("legacy unsupported class was accepted at the write boundary")
	}
	broken := legacy
	broken.Spec.Services = nil
	if err := validateStoredSystemPrefix(broken.Spec.M1.Prefix, broken); err == nil {
		t.Fatal("malformed legacy contract was accepted merely because its class was unsupported")
	}
}

func TestAllowWorkspaceRejectsInspectOnlyAgentWrites(t *testing.T) {
	h := &HTTPServer{}
	request := httptest.NewRequest("GET", "/", nil)
	installation := &Installation{WorkspaceID: "wrk_123", Authority: Authority{Inspect: true, Draft: false}}
	principal := Principal{Installation: installation, WorkspaceID: installation.WorkspaceID}
	if err := h.allowWorkspace(request, principal, installation.WorkspaceID, false); err != nil {
		t.Fatalf("inspect-only read rejected: %v", err)
	}
	if err := h.allowWorkspace(request, principal, installation.WorkspaceID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("inspect-only write returned %v, want forbidden", err)
	}
	installation.Authority.Draft = true
	if err := h.allowWorkspace(request, principal, installation.WorkspaceID, true); err != nil {
		t.Fatalf("draft-authorized write rejected: %v", err)
	}
}
