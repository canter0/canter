package sdk

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

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

func TestSystemRejectsOverflowingCapacityAndUnboundedGraphs(t *testing.T) {
	base := mysqlPairSystem(t)
	for name, mutate := range map[string]func(*System){
		"host multiplication": func(s *System) {
			s.Spec.Constraints.Host.Count = 2
			s.Spec.Constraints.Host.MemoryMiB = math.MaxInt
			s.Spec.Constraints.Host.SystemReserve = 0
		},
		"service multiplication": func(s *System) {
			s.Spec.Services[0].Resources.MemoryMiB = math.MaxInt
		},
		"host graph expansion": func(s *System) {
			s.Spec.Constraints.Host.Count = maxCompiledSystemNodes
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := base
			s.Spec.Services = append([]SystemService(nil), base.Spec.Services...)
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("unsafe capacity or graph size was accepted")
			}
		})
	}
}

func TestSystemGraphLimitCountsBothRuntimeTypes(t *testing.T) {
	system := mysqlPairSystem(t)
	system.Spec.Constraints.Host.Count = 3333
	system.Spec.Services = append(system.Spec.Services, SystemService{
		Name: "worker", Kind: "web", Isolation: "process", Instances: 1,
		Resources: ServiceResources{VCPU: 1, MemoryMiB: 1}, Readiness: Readiness{Protocol: "http", Port: 8080},
	})
	if err := system.Validate(); err == nil || !strings.Contains(err.Error(), "compilation limit") {
		t.Fatalf("mixed-runtime graph at node limit returned %v, want compilation limit error", err)
	}
}

func TestSystemRejectsDependencyEdgeAmplification(t *testing.T) {
	base := mysqlPairSystem(t)
	base.Spec.Services[0].DependsOn = []string{"web", "web"}
	base.Spec.Services = append(base.Spec.Services, SystemService{
		Name: "web", Kind: "web", Isolation: "process", Instances: 1,
		Resources: ServiceResources{VCPU: 1, MemoryMiB: 1}, Readiness: Readiness{Protocol: "http", Port: 8080},
	})
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate dependency") {
		t.Fatalf("duplicate dependency returned %v, want duplicate dependency error", err)
	}

	cycle := mysqlPairSystem(t)
	cycle.Spec.Services[0].DependsOn = []string{"web"}
	cycle.Spec.Services = append(cycle.Spec.Services, SystemService{
		Name: "web", Kind: "web", Isolation: "process", Instances: 1,
		Resources: ServiceResources{VCPU: 1, MemoryMiB: 1}, Readiness: Readiness{Protocol: "http", Port: 8080},
		DependsOn: []string{"mysql"},
	})
	if err := cycle.Validate(); err == nil || !strings.Contains(err.Error(), "contain a cycle") {
		t.Fatalf("cyclic dependencies returned %v, want cycle error", err)
	}

	nearLimit := dependencyFanoutSystem(t, 1000, 100)
	graph, err := CompileSystem(nearLimit)
	if err != nil {
		t.Fatalf("system with exactly %d expanded dependency edges was rejected: %v", maxCompiledSystemEdges, err)
	}
	if len(graph.Nodes) != 2203 {
		t.Fatalf("near-limit graph has %d nodes, want 2203", len(graph.Nodes))
	}

	system := dependencyFanoutSystem(t, 1001, 100)
	if err := system.Validate(); err == nil || !strings.Contains(err.Error(), "edge compilation limit") {
		t.Fatalf("expanded dependencies returned %v, want edge compilation limit error", err)
	}
}

func dependencyFanoutSystem(t *testing.T, instances, dependencyCount int) System {
	t.Helper()
	system := mysqlPairSystem(t)
	system.Spec.Constraints.Host.MemoryMiB = 8192
	system.Spec.Services = make([]SystemService, dependencyCount+1)
	dependencies := make([]string, dependencyCount)
	for i := range dependencies {
		name := fmt.Sprintf("service-%d", i)
		system.Spec.Services[i] = SystemService{
			Name: name, Kind: "web", Isolation: "process", Instances: 1,
			Resources: ServiceResources{VCPU: 1, MemoryMiB: 1}, Readiness: Readiness{Protocol: "http", Port: 8080},
		}
		dependencies[i] = name
	}
	system.Spec.Services[dependencyCount] = SystemService{
		Name: "target", Kind: "web", Isolation: "process", Instances: instances,
		Resources: ServiceResources{VCPU: 1, MemoryMiB: 1}, Readiness: Readiness{Protocol: "http", Port: 8080},
		DependsOn: dependencies,
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

func TestSystemComputeClassesAreDiscoverableAndValidated(t *testing.T) {
	if got, want := SupportedHostClasses(), []string{"c1", "c2", "c3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("supported host classes = %v, want %v", got, want)
	}
	s := mysqlPairSystem(t)
	s.Spec.Constraints.Host.Class = "shared"
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "unsupported_compute_class") || !strings.Contains(err.Error(), "c1, c2, c3") {
		t.Fatalf("unsupported host class returned non-actionable error: %v", err)
	}
}
