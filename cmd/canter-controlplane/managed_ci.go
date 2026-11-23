package main

import (
	"context"
	"strings"

	"github.com/canter0/canter/internal/controlplane"
	"github.com/canter0/canter/internal/provider/compute"
)

func ciManagedServiceFromEnv(store *controlplane.Store, github controlplane.CIConfig, getenv func(string) string) *controlplane.CIManagedService {
	cfg, loadErr := ciManagedStartupConfigFromEnv(getenv)
	mode := controlplane.CIManagedMode(strings.TrimSpace(getenv("CANTER_CI_MANAGED_MODE")))
	region := strings.TrimSpace(getenv("CANTER_CI_COMPUTE_REGION"))
	return controlplane.NewCIManagedService(mode, func(ctx context.Context) (*controlplane.CIManagedRuntime, error) {
		if loadErr != nil {
			clear(cfg.SigningKey)
			clear(cfg.EnforcerToken)
			return nil, controlplane.ErrConflict
		}
		provider, err := compute.NewFromEnv()
		if err != nil {
			return nil, controlplane.ErrConflict
		}
		if region != "" {
			provider = provider.WithRegion(region)
		}
		runtime, err := controlplane.NewCIManagedRuntime(ctx, store, provider, github, cfg)
		if err == nil {
			// A service never reinitializes after successful construction.
			clear(cfg.SigningKey)
			clear(cfg.EnforcerToken)
		}
		return runtime, err
	})
}
