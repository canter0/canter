package sdk

import (
	"context"
	"fmt"
)

type ServiceBinding struct {
	Service     string   `json:"service"`
	Kind        string   `json:"kind"`
	Engine      string   `json:"engine"`
	Environment string   `json:"environment"`
	Consumers   []string `json:"consumers"`
}

// SystemView is the semantic read model agents and humans share. It includes
// declared intent, its deterministic projection, capability bindings, and the
// observed production boundary without exposing provider credentials.
type SystemView struct {
	SchemaVersion string           `json:"schemaVersion"`
	Contract      System           `json:"contract"`
	Graph         ExecutionGraph   `json:"graph"`
	Bindings      []ServiceBinding `json:"bindings"`
	Host          *State           `json:"host,omitempty"`
	Release       *ReleaseView     `json:"release,omitempty"`
	Issues        []string         `json:"issues,omitempty"`
}

func CompileSystemView(system System) (SystemView, error) {
	graph, err := CompileSystem(system)
	if err != nil {
		return SystemView{}, err
	}
	view := SystemView{SchemaVersion: "v1", Contract: system, Graph: graph}
	for _, service := range system.Spec.Services {
		if service.Kind != "database" {
			continue
		}
		binding, err := ServiceBindingName(service.Name)
		if err != nil {
			return SystemView{}, err
		}
		capability := ServiceBinding{Service: service.Name, Kind: service.Kind, Engine: service.Engine, Environment: binding}
		for _, consumer := range system.Spec.Services {
			for _, dependency := range consumer.DependsOn {
				if dependency == service.Name {
					capability.Consumers = append(capability.Consumers, consumer.Name)
				}
			}
		}
		view.Bindings = append(view.Bindings, capability)
	}
	return view, nil
}

func (c *Client) InspectSystem(ctx context.Context, system System) (SystemView, error) {
	view, err := CompileSystemView(system)
	if err != nil {
		return SystemView{}, err
	}
	spec := SystemHostSpec(system, "true")
	var recorded State
	found, err := c.m1.GetOptional(ctx, stateKey(spec), &recorded)
	if err != nil {
		return SystemView{}, fmt.Errorf("read host state: %w", err)
	}
	if found {
		host, statusErr := c.Status(ctx, spec)
		if statusErr != nil {
			view.Host = &recorded
			view.Issues = append(view.Issues, "host observation: "+statusErr.Error())
		} else {
			view.Host = &host
		}
	}
	var observed ObservedRelease
	releaseFound, err := c.m1.GetOptional(ctx, observedKey(system), &observed)
	if err != nil {
		return SystemView{}, fmt.Errorf("read release state: %w", err)
	}
	if releaseFound {
		release, inspectErr := c.InspectRelease(ctx, system)
		if inspectErr != nil {
			view.Release = &ReleaseView{Release: observed}
			view.Issues = append(view.Issues, "public endpoint observation: "+inspectErr.Error())
		} else {
			view.Release = &release
		}
	}
	return view, nil
}
