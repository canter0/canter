package sdk

import "testing"

func mysqlPairSystem(t *testing.T) System {
	t.Helper()
	system, err := NewSystem("mysql-pair", "provide two isolated MySQL instances").
		OnHost("c1", 1, 1024, 384).
		WithM1("systems/mysql-pair").
		Provide(SystemService{
			Name: "mysql", Kind: "database", Engine: "mysql", Isolation: "firecracker", Instances: 2,
			Resources: ServiceResources{VCPU: 1, MemoryMiB: 250}, Readiness: Readiness{Protocol: "mysql", Port: 3306}, Networking: "private",
		}).Build()
	if err != nil {
		t.Fatal(err)
	}
	return system
}

func TestCompileSystemExpandsCapabilityIntoExecutionGraph(t *testing.T) {
	graph, err := CompileSystem(mysqlPairSystem(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 7 {
		t.Fatalf("got %d nodes, want 7", len(graph.Nodes))
	}
	if graph.Capacity.GuestMemoryMiB != 500 || graph.Capacity.UnallocatedMemory != 140 {
		t.Fatalf("unexpected capacity: %+v", graph.Capacity)
	}
	var guests, databases int
	for _, node := range graph.Nodes {
		if node.Kind == "runtime.microvm" {
			guests++
		}
		if node.Kind == "database.mysql" {
			databases++
			if node.Properties["binding"] != "CANTER_SERVICE_MYSQL_URL" {
				t.Fatalf("database binding was not compiled: %+v", node.Properties)
			}
		}
	}
	if guests != 2 || databases != 2 {
		t.Fatalf("guests=%d databases=%d", guests, databases)
	}
}

func TestServiceBindingNameIsStableAndValidated(t *testing.T) {
	binding, err := ServiceBindingName("primary-data")
	if err != nil || binding != "CANTER_SERVICE_PRIMARY_DATA_URL" {
		t.Fatalf("binding=%q err=%v", binding, err)
	}
	if _, err := ServiceBindingName("../../secret"); err == nil {
		t.Fatal("unsafe service name was accepted")
	}
}

func TestSystemRejectsOversubscribedMemory(t *testing.T) {
	s := mysqlPairSystem(t)
	s.Spec.Services[0].Resources.MemoryMiB = 400
	if err := s.Validate(); err == nil {
		t.Fatal("oversubscribed system was accepted")
	}
}
