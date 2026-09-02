package sdk

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/canter0/canter/internal/computeclass"
	"gopkg.in/yaml.v3"
)

type System struct {
	APIVersion string         `yaml:"apiVersion" json:"apiVersion"`
	Kind       string         `yaml:"kind" json:"kind"`
	Metadata   Metadata       `yaml:"metadata" json:"metadata"`
	Spec       SystemContract `yaml:"spec" json:"spec"`
}

type SystemContract struct {
	Intent      string          `yaml:"intent" json:"intent"`
	Constraints Constraints     `yaml:"constraints" json:"constraints"`
	Services    []SystemService `yaml:"services" json:"services"`
	M1          M1Spec          `yaml:"m1" json:"m1"`
}

type Constraints struct {
	Host HostConstraint `yaml:"host" json:"host"`
}

type HostConstraint struct {
	Class         string `yaml:"class" json:"class"`
	Count         int    `yaml:"count" json:"count"`
	MemoryMiB     int    `yaml:"memoryMiB" json:"memoryMiB"`
	SystemReserve int    `yaml:"systemReserveMiB" json:"systemReserveMiB"`
}

// SupportedHostClasses returns the complete compute-class vocabulary accepted
// by System contracts and advertised to agents.
func SupportedHostClasses() []string { return computeclass.Supported() }

type SystemService struct {
	Name       string           `yaml:"name" json:"name"`
	Kind       string           `yaml:"kind" json:"kind"`
	Engine     string           `yaml:"engine,omitempty" json:"engine,omitempty"`
	Isolation  string           `yaml:"isolation" json:"isolation"`
	Instances  int              `yaml:"instances" json:"instances"`
	DependsOn  []string         `yaml:"dependsOn,omitempty" json:"dependsOn,omitempty"`
	Resources  ServiceResources `yaml:"resources" json:"resources"`
	Readiness  Readiness        `yaml:"readiness" json:"readiness"`
	Networking string           `yaml:"networking,omitempty" json:"networking,omitempty"`
}

type ServiceResources struct {
	VCPU      int `yaml:"vcpu" json:"vcpu"`
	MemoryMiB int `yaml:"memoryMiB" json:"memoryMiB"`
}

type Readiness struct {
	Protocol string `yaml:"protocol" json:"protocol"`
	Port     int    `yaml:"port" json:"port"`
}

type ExecutionGraph struct {
	SchemaVersion string      `json:"schemaVersion"`
	System        string      `json:"system"`
	Nodes         []GraphNode `json:"nodes"`
	Invariants    []Invariant `json:"invariants"`
	Capacity      Capacity    `json:"capacity"`
}

type GraphNode struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	DependsOn  []string          `json:"dependsOn,omitempty"`
	Placement  string            `json:"placement,omitempty"`
	Resources  ServiceResources  `json:"resources,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

// ServiceBindingName is the stable environment variable through which an
// application consumes a private Canter-managed service capability.
func ServiceBindingName(service string) (string, error) {
	if !safeName.MatchString(service) {
		return "", fmt.Errorf("invalid service name %q", service)
	}
	var binding strings.Builder
	binding.WriteString("CANTER_SERVICE_")
	for _, r := range service {
		switch {
		case r >= 'a' && r <= 'z':
			binding.WriteRune(r - ('a' - 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			binding.WriteRune(r)
		default:
			binding.WriteByte('_')
		}
	}
	binding.WriteString("_URL")
	return binding.String(), nil
}

type Invariant struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Value   string `json:"value"`
}

type Capacity struct {
	HostMemoryMiB     int `json:"hostMemoryMiB"`
	SystemReserveMiB  int `json:"systemReserveMiB"`
	GuestMemoryMiB    int `json:"guestMemoryMiB"`
	UnallocatedMemory int `json:"unallocatedMemoryMiB"`
}

type SystemBuilder struct {
	system System
}

func NewSystem(name, intent string) *SystemBuilder {
	return &SystemBuilder{system: System{
		APIVersion: APIVersion,
		Kind:       "System",
		Metadata:   Metadata{Name: name},
		Spec:       SystemContract{Intent: intent},
	}}
}

func (b *SystemBuilder) OnHost(class string, count, memoryMiB, reserveMiB int) *SystemBuilder {
	b.system.Spec.Constraints.Host = HostConstraint{Class: class, Count: count, MemoryMiB: memoryMiB, SystemReserve: reserveMiB}
	return b
}

func (b *SystemBuilder) WithM1(prefix string) *SystemBuilder {
	b.system.Spec.M1 = M1Spec{Prefix: prefix}
	return b
}

func (b *SystemBuilder) Provide(service SystemService) *SystemBuilder {
	b.system.Spec.Services = append(b.system.Spec.Services, service)
	return b
}

func (b *SystemBuilder) Build() (System, error) {
	if err := b.system.Validate(); err != nil {
		return System{}, err
	}
	return b.system, nil
}

func LoadSystem(path string) (System, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return System{}, err
	}
	var system System
	if err := yaml.Unmarshal(b, &system); err != nil {
		return System{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := system.Validate(); err != nil {
		return System{}, err
	}
	return system, nil
}

func (s System) Validate() error {
	if s.APIVersion != APIVersion || s.Kind != "System" {
		return fmt.Errorf("system requires apiVersion %s and kind System", APIVersion)
	}
	if !safeName.MatchString(s.Metadata.Name) || strings.TrimSpace(s.Spec.Intent) == "" {
		return fmt.Errorf("system requires a valid name and non-empty intent")
	}
	host := s.Spec.Constraints.Host
	if host.Class == "" || host.Count < 1 || host.MemoryMiB < 1 || host.SystemReserve < 0 || host.SystemReserve >= host.MemoryMiB {
		return fmt.Errorf("system requires a valid host constraint")
	}
	if err := computeclass.Validate(host.Class); err != nil {
		return err
	}
	if len(s.Spec.Services) == 0 {
		return fmt.Errorf("system requires at least one service")
	}
	if err := ValidateM1Prefix(s.Spec.M1.Prefix); err != nil {
		return fmt.Errorf("system requires a safe relative m1 prefix")
	}
	seen := map[string]bool{}
	guestMemory := 0
	for _, service := range s.Spec.Services {
		if !safeName.MatchString(service.Name) || seen[service.Name] {
			return fmt.Errorf("service names must be valid and unique")
		}
		seen[service.Name] = true
		if service.Kind == "" || service.Instances < 1 || service.Resources.VCPU < 1 || service.Resources.MemoryMiB < 1 {
			return fmt.Errorf("service %s has invalid kind, instances, or resources", service.Name)
		}
		if service.Isolation != "firecracker" && service.Isolation != "process" {
			return fmt.Errorf("service %s requests unsupported isolation %q", service.Name, service.Isolation)
		}
		if service.Kind == "database" && service.Engine == "" {
			return fmt.Errorf("database service %s requires an engine", service.Name)
		}
		if service.Readiness.Protocol == "" || service.Readiness.Port < 1 || service.Readiness.Port > 65535 {
			return fmt.Errorf("service %s requires a valid readiness check", service.Name)
		}
		guestMemory += service.Instances * service.Resources.MemoryMiB
	}
	for _, service := range s.Spec.Services {
		for _, dependency := range service.DependsOn {
			if dependency == service.Name || !seen[dependency] {
				return fmt.Errorf("service %s has invalid dependency %q", service.Name, dependency)
			}
		}
	}
	available := host.Count * (host.MemoryMiB - host.SystemReserve)
	if guestMemory > available {
		return fmt.Errorf("guest memory %d MiB exceeds host capacity %d MiB after system reserve", guestMemory, available)
	}
	return nil
}

func CompileSystem(system System) (ExecutionGraph, error) {
	if err := system.Validate(); err != nil {
		return ExecutionGraph{}, err
	}
	host := system.Spec.Constraints.Host
	graph := ExecutionGraph{SchemaVersion: "v1", System: system.Metadata.Name}
	needsFirecracker, needsProcess := false, false
	for _, service := range system.Spec.Services {
		needsFirecracker = needsFirecracker || service.Isolation == "firecracker"
		needsProcess = needsProcess || service.Isolation == "process"
	}
	graph.Nodes = append(graph.Nodes, GraphNode{
		ID: "m1/system", Kind: "m1.namespace", Properties: map[string]string{"prefix": system.Spec.M1.Prefix},
	})
	for i := 1; i <= host.Count; i++ {
		hostID := fmt.Sprintf("compute/host-%d", i)
		graph.Nodes = append(graph.Nodes, GraphNode{ID: hostID, Kind: "compute.host", Properties: map[string]string{"class": host.Class, "memoryMiB": fmt.Sprint(host.MemoryMiB)}})
		if needsFirecracker {
			graph.Nodes = append(graph.Nodes, GraphNode{ID: fmt.Sprintf("runtime/firecracker-host-%d", i), Kind: "runtime.firecracker", DependsOn: []string{hostID}, Placement: hostID})
			graph.Invariants = append(graph.Invariants, Invariant{Kind: "host.capability", Subject: hostID, Value: "kvm"})
		}
		if needsProcess {
			graph.Nodes = append(graph.Nodes, GraphNode{ID: fmt.Sprintf("runtime/process-host-%d", i), Kind: "runtime.process", DependsOn: []string{hostID}, Placement: hostID})
		}
	}
	guestMemory := 0
	hostIndex := 1
	for _, service := range system.Spec.Services {
		for instance := 1; instance <= service.Instances; instance++ {
			hostID := fmt.Sprintf("compute/host-%d", hostIndex)
			runtimeID := fmt.Sprintf("runtime/firecracker-host-%d", hostIndex)
			workloadID := fmt.Sprintf("microvm/%s-%d", service.Name, instance)
			workloadKind := "runtime.microvm"
			if service.Isolation == "process" {
				runtimeID = fmt.Sprintf("runtime/process-host-%d", hostIndex)
				workloadID = fmt.Sprintf("process/%s-%d", service.Name, instance)
				workloadKind = "runtime.process-instance"
			}
			serviceID := fmt.Sprintf("service/%s-%d", service.Name, instance)
			properties := map[string]string{"readiness": fmt.Sprintf("%s:%d", service.Readiness.Protocol, service.Readiness.Port), "networking": service.Networking}
			if service.Kind == "database" {
				binding, _ := ServiceBindingName(service.Name)
				properties["binding"] = binding
			}
			dependencies := []string{workloadID, "m1/system"}
			for _, dependency := range service.DependsOn {
				dependencies = append(dependencies, fmt.Sprintf("service/%s-1", dependency))
			}
			graph.Nodes = append(graph.Nodes,
				GraphNode{ID: workloadID, Kind: workloadKind, DependsOn: []string{runtimeID}, Placement: hostID, Resources: service.Resources, Properties: map[string]string{"isolation": service.Isolation}},
				GraphNode{ID: serviceID, Kind: service.Kind + "." + service.Engine, DependsOn: dependencies, Placement: workloadID, Properties: properties},
			)
			graph.Invariants = append(graph.Invariants,
				Invariant{Kind: "resource.memory", Subject: workloadID, Value: fmt.Sprintf("%dMiB", service.Resources.MemoryMiB)},
				Invariant{Kind: "service.ready", Subject: serviceID, Value: fmt.Sprintf("%s:%d", service.Readiness.Protocol, service.Readiness.Port)},
			)
			guestMemory += service.Resources.MemoryMiB
			hostIndex = hostIndex%host.Count + 1
		}
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].ID < graph.Nodes[j].ID })
	graph.Capacity = Capacity{
		HostMemoryMiB: host.Count * host.MemoryMiB, SystemReserveMiB: host.Count * host.SystemReserve,
		GuestMemoryMiB: guestMemory, UnallocatedMemory: host.Count*(host.MemoryMiB-host.SystemReserve) - guestMemory,
	}
	return graph, nil
}

func (g ExecutionGraph) JSON() ([]byte, error) { return json.MarshalIndent(g, "", "  ") }
