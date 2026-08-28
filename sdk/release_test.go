package sdk

import "testing"

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
