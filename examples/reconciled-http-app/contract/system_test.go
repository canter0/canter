package contract

import (
	"testing"

	"github.com/canter0/canter/sdk"
)

func TestContractCompilesToProcessRuntime(t *testing.T) {
	system, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := sdk.CompileSystem(system)
	if err != nil {
		t.Fatal(err)
	}
	var hosts, runtimes, processes, services int
	for _, node := range graph.Nodes {
		switch node.Kind {
		case "compute.host":
			hosts++
		case "runtime.process":
			runtimes++
		case "runtime.process-instance":
			processes++
		case "service.http":
			services++
		}
	}
	if hosts != 1 || runtimes != 1 || processes != 1 || services != 1 {
		t.Fatalf("unexpected graph: hosts=%d runtimes=%d processes=%d services=%d", hosts, runtimes, processes, services)
	}
	for _, invariant := range graph.Invariants {
		if invariant.Value == "kvm" {
			t.Fatal("process system unexpectedly requires KVM")
		}
	}
}
