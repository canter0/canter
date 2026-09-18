package sdk

import (
	"context"
	"fmt"
	"strings"

	"github.com/canter0/canter/internal/provider/compute"
)

// BillingResource is a provider-observed allocation, not a requested contract.
// Compute units are 1-vCPU/1-GiB bundles; storage units are bytes.
type BillingResource struct {
	ID    string
	Kind  string
	Units int64
}

func (c *Client) BillingResources(ctx context.Context, system System) ([]BillingResource, error) {
	shapes, ok := c.compute.(interface {
		ServerShape(context.Context, string) (compute.Shape, error)
	})
	if !ok {
		return nil, fmt.Errorf("compute metering unavailable")
	}
	objects, ok := c.m1.(interface {
		StoredBytes(context.Context, string) (int64, error)
	})
	if !ok {
		return nil, fmt.Errorf("storage metering unavailable")
	}
	var state State
	found, err := c.m1.GetOptional(ctx, stateKey(SystemHostSpec(system, "true")), &state)
	if err != nil {
		return nil, err
	}
	out := []BillingResource{}
	if found && state.Phase != "destroyed" {
		for _, resource := range state.Resources {
			if resource.Status == "DELETED" {
				continue
			}
			shape, err := shapes.ServerShape(ctx, resource.ID)
			if compute.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if shape.VCPU < 1 || shape.Memory < 1 {
				return nil, fmt.Errorf("invalid observed compute shape")
			}
			out = append(out, BillingResource{ID: "compute/" + resource.ID, Kind: "compute", Units: int64(max(shape.VCPU, (shape.Memory+1023)/1024))})
		}
	}
	// The trailing slash prevents sibling-prefix leakage across systems.
	prefix := strings.TrimRight(system.Spec.M1.Prefix, "/") + "/"
	size, err := objects.StoredBytes(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out = append(out, BillingResource{ID: "storage/" + prefix, Kind: "storage", Units: size})
	return out, nil
}
