package driver

import (
	"context"
	"fmt"
	"strings"

	"github.com/canter0/canter/sdk"
)

type Result struct {
	URL      string
	Endpoint string
}

type Driver interface {
	Ensure(context.Context, sdk.RuntimeService) (Result, error)
}

type ActionDriver interface {
	Execute(context.Context, sdk.RuntimeService, sdk.RuntimeAction) (sdk.RuntimeActionResult, error)
}

type Registry struct {
	drivers map[string]Driver
}

func (r *Registry) Execute(ctx context.Context, plan sdk.RuntimePlan, action sdk.RuntimeAction) (sdk.RuntimeActionResult, error) {
	for _, service := range plan.Services {
		if service.Name != action.Service {
			continue
		}
		key := strings.ToLower(service.Kind + "." + service.Engine)
		implementation, ok := r.drivers[key]
		if !ok {
			return sdk.RuntimeActionResult{}, fmt.Errorf("no runtime driver registered for %s", key)
		}
		actionDriver, ok := implementation.(ActionDriver)
		if !ok {
			return sdk.RuntimeActionResult{}, fmt.Errorf("runtime driver %s does not accept actions", key)
		}
		return actionDriver.Execute(ctx, service, action)
	}
	return sdk.RuntimeActionResult{}, fmt.Errorf("runtime action references unknown service %q", action.Service)
}

func NewRegistry() *Registry {
	return &Registry{drivers: make(map[string]Driver)}
}

func (r *Registry) Register(kind, engine string, implementation Driver) {
	r.drivers[strings.ToLower(kind+"."+engine)] = implementation
}

func (r *Registry) Ensure(ctx context.Context, plan sdk.RuntimePlan) (map[string]string, []sdk.ObservedService, error) {
	bindings := make(map[string]string)
	observed := make([]sdk.ObservedService, 0, len(plan.Services))
	for _, service := range plan.Services {
		key := strings.ToLower(service.Kind + "." + service.Engine)
		implementation, ok := r.drivers[key]
		if !ok {
			return nil, observed, fmt.Errorf("no runtime driver registered for %s", key)
		}
		result, err := implementation.Ensure(ctx, service)
		if err != nil {
			observed = append(observed, sdk.ObservedService{Name: service.Name, Kind: service.Kind, Engine: service.Engine, Phase: "failed"})
			return nil, observed, fmt.Errorf("reconcile service %s: %w", service.Name, err)
		}
		binding, err := sdk.ServiceBindingName(service.Name)
		if err != nil {
			return nil, observed, err
		}
		bindings[binding] = result.URL
		observed = append(observed, sdk.ObservedService{Name: service.Name, Binding: binding, Kind: service.Kind, Engine: service.Engine, Phase: "ready", Endpoint: result.Endpoint})
	}
	return bindings, observed, nil
}
