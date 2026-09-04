package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/canter0/canter/sdk"
)

func TestStandingPolicyEnvelopeUsesOnlyBoundedExactValues(t *testing.T) {
	base := StandingPolicyEnvelope{
		AllowedInstallationIDs:        []string{"agt_one"},
		AffectedServices:              []string{"web"},
		OperationKinds:                []string{"release.set-desired"},
		Availability:                  []string{"rolling replacement"},
		Data:                          []string{"none"},
		AllowedReversibility:          []string{"compensatable"},
		MaxAdditionalMonthlyCostCents: 500,
		MaxOperations:                 4,
	}
	canonical, err := canonicalPolicyEnvelope(base)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.AllowedInstallationIDs[0] != "agt_one" {
		t.Fatalf("canonical policy changed exact installation: %#v", canonical)
	}
	wildcard := base
	wildcard.OperationKinds = []string{"*"}
	if _, err = canonicalPolicyEnvelope(wildcard); err == nil {
		t.Fatal("wildcard operation policy was accepted")
	}
	missing := base
	missing.AllowedInstallationIDs = nil
	if _, err = canonicalPolicyEnvelope(missing); err == nil {
		t.Fatal("policy without an exact installation was accepted")
	}
}

func TestStandingPolicyMatchesServerOwnedChangeEnvelope(t *testing.T) {
	policy := StandingPolicy{Envelope: StandingPolicyEnvelope{
		AllowedInstallationIDs:        []string{"agt_allowed"},
		AffectedServices:              []string{"web"},
		OperationKinds:                []string{"http.verify", "release.set-desired"},
		Availability:                  []string{"rolling replacement"},
		Data:                          []string{"none"},
		AllowedReversibility:          []string{"compensatable", "not-applicable"},
		MaxAdditionalMonthlyCostCents: 500,
		MaxOperations:                 3,
	}}
	change := sdk.Change{
		ID: "change-policy", Phase: "drafted", Digest: strings.Repeat("a", 64),
		Plan: sdk.ChangePlan{Impact: sdk.ChangeImpact{AffectedServices: []string{"web"}, Availability: "rolling replacement", Data: "none", MonthlyCostDeltaCents: 500}},
		Operations: []sdk.ChangeOperation{
			{Kind: "release.set-desired", Reversibility: "compensatable"},
			{Kind: "http.verify", Reversibility: "not-applicable"},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if allowed, reason := policyAllows(policy, "agt_allowed", change); !allowed {
		t.Fatalf("exact policy envelope did not match: %s", reason)
	}
	tooExpensive := change
	tooExpensive.Plan.Impact.MonthlyCostDeltaCents++
	if allowed, _ := policyAllows(policy, "agt_allowed", tooExpensive); allowed {
		t.Fatal("cost above the standing policy was accepted")
	}
	migration := change
	migration.Operations = append(migration.Operations, sdk.ChangeOperation{Kind: "migration.apply", Reversibility: "irreversible"})
	if allowed, _ := policyAllows(policy, "agt_allowed", migration); allowed {
		t.Fatal("unlisted migration operation was accepted")
	}
	if allowed, _ := policyAllows(policy, "agt_other", change); allowed {
		t.Fatal("unlisted agent installation was accepted")
	}
}

func TestStandingPolicyBindsReplicaRange(t *testing.T) {
	policy := StandingPolicy{Envelope: StandingPolicyEnvelope{
		AllowedInstallationIDs: []string{"agt_allowed"}, AffectedServices: []string{"web"},
		OperationKinds: []string{"release.scale", "release.wait-replicas", "state.assert", "http.verify"},
		Availability:   []string{"capacity adjustment; healthy replicas remain serving"}, Data: []string{"none"},
		AllowedReversibility: []string{"read-only", "compensatable"}, MaxOperations: 4,
		ScaleLimits: map[string]ReplicaRange{"web": {Min: 2, Max: 6}}, AllowPermanentScale: true,
	}}
	change := sdk.Change{Phase: "drafted", Plan: sdk.ChangePlan{Scale: &sdk.ReplicaScalePlan{Service: "web", FromReplicas: 2, ToReplicas: 6}, Impact: sdk.ChangeImpact{AffectedServices: []string{"web"}, Availability: "capacity adjustment; healthy replicas remain serving", Data: "none"}}, Operations: []sdk.ChangeOperation{{Kind: "state.assert", Reversibility: "read-only"}, {Kind: "release.scale", Reversibility: "compensatable"}, {Kind: "release.wait-replicas", Reversibility: "read-only"}, {Kind: "http.verify", Reversibility: "read-only"}}}
	if allowed, reason := policyAllows(policy, "agt_allowed", change); !allowed {
		t.Fatalf("bounded scale was denied: %s", reason)
	}
	change.Plan.Scale.ToReplicas = 7
	if allowed, _ := policyAllows(policy, "agt_allowed", change); allowed {
		t.Fatal("scale above policy maximum was accepted")
	}
	delete(policy.Envelope.ScaleLimits, "web")
	change.Plan.Scale.ToReplicas = 4
	if allowed, _ := policyAllows(policy, "agt_allowed", change); allowed {
		t.Fatal("scale without an exact service range was accepted")
	}
	policy.Envelope.ScaleLimits = map[string]ReplicaRange{"web": {Min: 2, Max: 6}}
	policy.Envelope.AllowPermanentScale = false
	policy.Envelope.MaxScaleDurationSeconds = 180
	change.Plan.Scale.LeaseSeconds = 120
	if allowed, reason := policyAllows(policy, "agt_allowed", change); !allowed {
		t.Fatalf("bounded temporary scale was denied: %s", reason)
	}
	change.Plan.Scale.LeaseSeconds = 181
	if allowed, _ := policyAllows(policy, "agt_allowed", change); allowed {
		t.Fatal("temporary scale longer than policy duration was accepted")
	}
}

func TestMCPPublishesPolicyEvaluationButNotPolicyMutation(t *testing.T) {
	wanted := map[string]bool{"canter_list_standing_policies": false, "canter_apply_change_under_policy": false, "canter_inspect_change_execution": false}
	for _, tool := range mcpTools() {
		if _, ok := wanted[tool.Name]; ok {
			wanted[tool.Name] = true
		}
		if tool.Name == "canter_create_standing_policy" || tool.Name == "canter_revoke_standing_policy" {
			t.Fatalf("agent MCP exposed human policy mutation tool %s", tool.Name)
		}
	}
	for name, found := range wanted {
		if !found {
			t.Fatalf("MCP tool %s is missing", name)
		}
	}
}
