package controlplane

import (
	"fmt"

	"github.com/canter0/canter/sdk"
)

// canonicalSystemPrefix is the only namespace assigned to new control-plane
// Systems. Workspace IDs are part of the path so two tenants cannot address
// the same runtime state by choosing the same System name.
func canonicalSystemPrefix(workspaceID, systemName string) (string, error) {
	prefix := "workspaces/" + workspaceID + "/systems/" + systemName
	if err := sdk.ValidateM1Prefix(prefix); err != nil {
		return "", fmt.Errorf("cannot derive workspace-scoped m1 prefix: %w", err)
	}
	return prefix, nil
}

// canonicalizeSystemForWorkspace accepts an omitted prefix or a safe legacy
// client suggestion, but always returns the server-derived namespace. Unsafe
// input is rejected instead of being hidden by normalization.
func canonicalizeSystemForWorkspace(workspaceID string, system sdk.System) (sdk.System, error) {
	if system.Spec.M1.Prefix != "" {
		if err := sdk.ValidateM1Prefix(system.Spec.M1.Prefix); err != nil {
			return sdk.System{}, err
		}
	}
	prefix, err := canonicalSystemPrefix(workspaceID, system.Metadata.Name)
	if err != nil {
		return sdk.System{}, err
	}
	system.Spec.M1.Prefix = prefix
	if err := system.Validate(); err != nil {
		return sdk.System{}, err
	}
	return system, nil
}

func validateCanonicalSystemForWorkspace(workspaceID string, system sdk.System) error {
	if err := system.Validate(); err != nil {
		return err
	}
	prefix, err := canonicalSystemPrefix(workspaceID, system.Metadata.Name)
	if err != nil {
		return err
	}
	if system.Spec.M1.Prefix != prefix {
		return fmt.Errorf("%w: System m1 prefix must be %q", ErrConflict, prefix)
	}
	return nil
}
