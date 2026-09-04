package controlplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestPublicSystemViewHidesProviderTopology(t *testing.T) {
	contract := sdk.System{Spec: sdk.SystemContract{Constraints: sdk.Constraints{Host: sdk.HostConstraint{Class: "c1", Count: 1}}}}
	internal := sdk.SystemView{
		SchemaVersion: "v1",
		Contract:      contract,
		Host: &sdk.State{
			Phase: "escalated", Class: "c1", Failure: "https://provider.invalid resource provider-server-id failed",
			Resources:       []sdk.Resource{{ID: "provider-server-id", Name: "provider-name", Status: "ACTIVE", Address: "192.0.2.44"}},
			ProofKeys:       []string{"private/proof/key"},
			NetworkPolicies: []sdk.NetworkPolicy{{ID: "provider-policy-id", PortID: "provider-port-id", RuleID: "provider-rule-id", Name: "provider-policy", Ownership: "sha256:private", Protocol: "tcp", Port: 8080}},
			ExposureIntent:  &sdk.ExposureIntent{OperationID: "private-operation", ServerID: "provider-server-id", Name: "provider-policy", Ownership: "sha256:private", Protocol: "tcp", Port: 8080, Phase: "escalated", MutationUnresolved: true},
		},
		Issues: []string{"provider endpoint https://provider.invalid failed"},
	}
	view := publicSystemView(internal)
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, forbidden := range []string{"provider.invalid", "provider-server-id", "provider-policy-id", "provider-port-id", "provider-rule-id", "private/proof/key", "sha256:private", "private-operation", "192.0.2.44"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("public System view leaked %q: %s", forbidden, text)
		}
	}
	if view.Host == nil || view.Host.Class != "c1" || view.Host.Count != 1 || len(view.Host.Resources) != 1 || view.Host.Resources[0].Name != "compute-1" || view.Host.Resources[0].Status != "ACTIVE" || view.Host.Exposure == nil || view.Host.Exposure.Port != 8080 || !view.Host.Exposure.MutationUnresolved || !view.Host.RequiresOperator {
		t.Fatalf("semantic host state was lost: %#v", view.Host)
	}
	if len(view.Issues) != 1 || view.Issues[0] != "System observation requires operator inspection" {
		t.Fatalf("provider issue was not normalized: %#v", view.Issues)
	}
}
