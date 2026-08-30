package sdk

import "testing"

func TestRuntimePlanProjectsManagedCapabilities(t *testing.T) {
	system, err := NewSystem("stateful", "run a stateful application").
		OnHost("c1", 1, 1024, 384).
		WithM1("systems/stateful").
		Provide(SystemService{Name: "database", Kind: "database", Engine: "postgres", Isolation: "process", Instances: 1, Resources: ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: Readiness{Protocol: "tcp", Port: 5432}}).
		Provide(SystemService{Name: "web", Kind: "service", Engine: "http", Isolation: "process", Instances: 1, DependsOn: []string{"database"}, Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}, Networking: "public"}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := CompileRuntimePlan(system)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Services) != 1 || plan.Services[0].Name != "database" || plan.Services[0].Binding != "CANTER_SERVICE_DATABASE_URL" || plan.Services[0].Engine != "postgres" {
		t.Fatalf("unexpected runtime plan: %+v", plan)
	}
	graph, err := CompileSystem(system)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range graph.Nodes {
		if node.ID == "service/web-1" {
			for _, dependency := range node.DependsOn {
				if dependency == "service/database-1" {
					return
				}
			}
		}
	}
	t.Fatal("compiled web service did not depend on database service")
}

func TestSystemRejectsUnknownDependency(t *testing.T) {
	_, err := NewSystem("broken", "invalid dependency").OnHost("c1", 1, 1024, 512).WithM1("systems/broken").Provide(SystemService{Name: "web", Kind: "service", Engine: "http", Isolation: "process", Instances: 1, DependsOn: []string{"missing"}, Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}}).Build()
	if err == nil {
		t.Fatal("unknown dependency was accepted")
	}
}
