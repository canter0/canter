package sdk

import (
	"context"
	"strings"
	"testing"
)

func TestSystemHostSpecSeparatesNodeBootstrapFromRelease(t *testing.T) {
	system, err := NewSystem("web-system", "keep a versioned web service healthy").OnHost("c1", 1, 1024, 512).WithM1("systems/web-system").Provide(SystemService{Name: "web", Kind: "service", Engine: "http", Isolation: "process", Instances: 1, Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}, Networking: "public"}).Build()
	if err != nil {
		t.Fatal(err)
	}
	spec := SystemHostSpec(system, "install-canter-node-only")
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if spec.Spec.Compute.Bootstrap != "install-canter-node-only" {
		t.Fatal("host bootstrap was replaced by application content")
	}
	if spec.Spec.M1.Prefix != system.Spec.M1.Prefix {
		t.Fatal("host and release state do not share the system namespace")
	}
}

func TestLegacySystemHostBootstrapFailsClosed(t *testing.T) {
	if _, err := (&Client{}).BootstrapSystemHost(context.Background(), System{}, []byte("node")); err == nil || !strings.Contains(err.Error(), "node gateway enrollment is required") {
		t.Fatalf("legacy provider-credential bootstrap did not fail closed: %v", err)
	}
}

func TestSystemdQuoteArgRejectsControlCharactersAndEscapesSpecifiers(t *testing.T) {
	for _, value := range []string{"line\nbreak", "carriage\rreturn", "nul\x00byte"} {
		if _, err := systemdQuoteArg(value); err == nil {
			t.Fatalf("unsafe systemd argument %q was accepted", value)
		}
	}
	quoted, err := systemdQuoteArg(`systems/app%name`)
	if err != nil {
		t.Fatal(err)
	}
	if quoted != `"systems/app%%name"` || strings.Contains(quoted, "\n") {
		t.Fatalf("unexpected quoted systemd argument: %s", quoted)
	}
}

func TestControlPlaneArtifactKeyIsCanonicallyBoundToDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	key, err := ControlPlaneArtifactKey(digest)
	if err != nil {
		t.Fatal(err)
	}
	if key != "control-plane/artifacts/sha256/"+digest+".tar.gz" {
		t.Fatalf("unexpected key %q", key)
	}
	for _, invalid := range []string{strings.Repeat("A", 64), strings.Repeat("g", 64), "short"} {
		if _, err := ControlPlaneArtifactKey(invalid); err == nil {
			t.Fatalf("invalid digest %q was accepted", invalid)
		}
	}
}

func TestNodeBootstrapContainsOneTimeGatewayCapabilityNotProviderCredentials(t *testing.T) {
	system, err := NewSystem("gateway-app", "serve through a governed node").OnHost("compute", 1, 1024, 256).WithM1("systems/gateway-app").Provide(SystemService{Name: "web", Kind: "application", Isolation: "process", Instances: 1, Networking: "public", Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).Build()
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := renderNodeBootstrap(system, "https://objects.example/node", strings.Repeat("a", 64), 8080, NodeBootstrapConfig{GatewayURL: "https://control.example", EnrollmentID: "nen_one", EnrollmentToken: "ce_one-time"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"CANTER_M1_ACCESS_KEY", "CANTER_M1_SECRET_KEY", "CANTER_M1_ENDPOINT", "--prefix"} {
		if strings.Contains(bootstrap, forbidden) {
			t.Fatalf("bootstrap contains provider material %q", forbidden)
		}
	}
	if !strings.Contains(bootstrap, "/v1/node/enrollments/nen_one/exchange") || !strings.Contains(bootstrap, "--gateway") || !strings.Contains(bootstrap, "/etc/canter/node.token") {
		t.Fatal("bootstrap does not enroll and start the gateway node")
	}
}
