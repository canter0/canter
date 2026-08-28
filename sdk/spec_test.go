package sdk

import (
	"testing"
)

func validSpec() Spec {
	return Spec{
		APIVersion: APIVersion,
		Kind:       "Sandbox",
		Metadata:   Metadata{Name: "test-sandbox"},
		Spec: Desired{
			Intent:  "prove a real sandbox booted",
			Compute: ComputeSpec{Class: "c1", Image: "ubuntu-24.04", Replicas: 1},
			M1:      M1Spec{Prefix: "sandboxes/test-sandbox"},
			Policy:  Policy{MaxReplicas: 1},
		},
	}
}

func TestSpecValidate(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestSpecPolicyRejectsReplicaEscalation(t *testing.T) {
	s := validSpec()
	s.Spec.Compute.Replicas = 2
	if err := s.Validate(); err == nil {
		t.Fatal("replica escalation was accepted")
	}
}

func TestValidatePlanRejectsModelDrift(t *testing.T) {
	s := validSpec()
	p := Plan{SchemaVersion: "v1", Operations: []Operation{
		{Kind: "m1.ensure", Name: s.Metadata.Name, Prefix: s.Spec.M1.Prefix},
		{Kind: "compute.ensure", Name: s.Metadata.Name, Class: "c3", Image: s.Spec.Compute.Image, Replicas: 1},
	}}
	if err := validatePlan(s, p); err == nil {
		t.Fatal("plan that changed compute class was accepted")
	}
}

func TestBootScriptRequiresBootstrapSuccessBeforeProof(t *testing.T) {
	s := validSpec()
	s.Spec.Compute.Bootstrap = "false"
	script := bootScript(s, "https://m1.invalid/proof")
	if script[:18] != "#!/bin/sh\nset -eu\n" {
		t.Fatalf("boot script does not fail closed: %q", script[:18])
	}
}
