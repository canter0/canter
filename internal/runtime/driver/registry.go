package driver

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/canter0/canter/sdk"
)

type Result struct {
	URL      string
	Endpoint string
}

type Driver interface {
	Ensure(context.Context, sdk.RuntimeService) (Result, error)
}

type Registry struct {
	drivers map[string]Driver
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
		bindings[bindingName(service.Name)] = result.URL
		observed = append(observed, sdk.ObservedService{Name: service.Name, Kind: service.Kind, Engine: service.Engine, Phase: "ready", Endpoint: result.Endpoint})
	}
	return bindings, observed, nil
}

func bindingName(name string) string {
	var b strings.Builder
	b.WriteString("CANTER_SERVICE_")
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteByte('_')
		}
	}
	b.WriteString("_URL")
	return b.String()
}
