package sdk

import (
	"strings"
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

func TestValidateM1PrefixRejectsUnsafeSegments(t *testing.T) {
	invalid := []string{
		"", "/systems/app", "systems/app/", "systems//app", "systems/./app", "systems/../app",
		"systems/app name", "systems/app\nEnvironment=oops", `systems/app\name`, "systems/app%2fescape",
	}
	for _, prefix := range invalid {
		if err := ValidateM1Prefix(prefix); err == nil {
			t.Errorf("unsafe m1 prefix %q was accepted", prefix)
		}
	}
	for _, prefix := range []string{"systems/app", "workspaces/wrk_123/systems/api-v2", "a/b.c_d-e"} {
		if err := ValidateM1Prefix(prefix); err != nil {
			t.Errorf("safe m1 prefix %q rejected: %v", prefix, err)
		}
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
	if !strings.HasPrefix(script, "#!/bin/sh\nset +e\ncanter_log=") || !strings.Contains(script, "set -eu\nfalse") {
		t.Fatalf("boot script does not isolate bootstrap failure: %q", script[:28])
	}
	if !strings.Contains(script, "canter_status=failed") {
		t.Fatal("boot script does not report failure")
	}
}
