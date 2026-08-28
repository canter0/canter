package sdk

import (
	"fmt"
	"strings"
)

// RuntimePlan is the deterministic host-side projection of a System. It keeps
// logical capabilities independent from the drivers that realize them.
type RuntimePlan struct {
	SchemaVersion string           `json:"schemaVersion"`
	System        string           `json:"system"`
	Services      []RuntimeService `json:"services"`
}

type RuntimeService struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Engine    string           `json:"engine"`
	Instances int              `json:"instances"`
	Resources ServiceResources `json:"resources"`
	Readiness Readiness        `json:"readiness"`
}

func CompileRuntimePlan(system System) (RuntimePlan, error) {
	if err := system.Validate(); err != nil {
		return RuntimePlan{}, err
	}
	plan := RuntimePlan{SchemaVersion: "v1", System: system.Metadata.Name}
	for _, service := range system.Spec.Services {
		if service.Kind != "database" {
			continue
		}
		plan.Services = append(plan.Services, RuntimeService{
			Name: service.Name, Kind: service.Kind, Engine: strings.ToLower(service.Engine),
			Instances: service.Instances, Resources: service.Resources, Readiness: service.Readiness,
		})
	}
	return plan, nil
}

func (p RuntimePlan) Validate(system string) error {
	if p.SchemaVersion != "v1" || p.System != system {
		return fmt.Errorf("runtime plan does not belong to system %s", system)
	}
	for _, service := range p.Services {
		if !safeName.MatchString(service.Name) || service.Kind == "" || service.Engine == "" || service.Instances < 1 {
			return fmt.Errorf("runtime plan contains an invalid service")
		}
	}
	return nil
}
