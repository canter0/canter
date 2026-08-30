package sdk

import "testing"

func TestCompileSystemViewMakesCapabilityConsumptionExplicit(t *testing.T) {
	system, err := NewSystem("stateful", "run a stateful service").
		OnHost("c1", 1, 1024, 384).
		WithM1("systems/stateful").
		Provide(SystemService{Name: "database", Kind: "database", Engine: "postgres", Isolation: "process", Instances: 1, Resources: ServiceResources{VCPU: 1, MemoryMiB: 256}, Readiness: Readiness{Protocol: "tcp", Port: 5432}, Networking: "private"}).
		Provide(SystemService{Name: "web", Kind: "service", Engine: "http", Isolation: "process", Instances: 1, DependsOn: []string{"database"}, Resources: ServiceResources{VCPU: 1, MemoryMiB: 128}, Readiness: Readiness{Protocol: "http", Port: 8080}, Networking: "public"}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	view, err := CompileSystemView(system)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Bindings) != 1 || view.Bindings[0].Environment != "CANTER_SERVICE_DATABASE_URL" || len(view.Bindings[0].Consumers) != 1 || view.Bindings[0].Consumers[0] != "web" {
		t.Fatalf("unexpected bindings: %+v", view.Bindings)
	}
}
