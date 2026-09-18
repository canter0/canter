package sdk

import (
	"context"
	"fmt"
	"github.com/canter0/canter/internal/provider/compute"
	"testing"
)

type meteredCompute struct {
	*fakeCompute
	missing bool
}

func (f meteredCompute) ServerShape(context.Context, string) (compute.Shape, error) {
	if f.missing {
		return compute.Shape{}, fmt.Errorf("provider unavailable")
	}
	return compute.Shape{VCPU: 2, Memory: 3072}, nil
}

type meteredObjects struct {
	*fakeStore
	prefix string
}

func (f *meteredObjects) StoredBytes(_ context.Context, prefix string) (int64, error) {
	f.prefix = prefix
	return 12345, nil
}
func TestBillingResourcesUseActualAllocationAndScopedStorage(t *testing.T) {
	ctx := context.Background()
	objects := &meteredObjects{fakeStore: newFakeStore()}
	provider := meteredCompute{fakeCompute: &fakeCompute{}}
	c := &Client{compute: provider, m1: objects}
	system := System{Metadata: Metadata{Name: "app"}}
	system.Spec.M1.Prefix = "workspaces/owner/systems/app"
	if err := objects.PutJSON(ctx, stateKey(SystemHostSpec(system, "true")), State{Phase: "ready", Resources: []Resource{{ID: "real", Status: "ACTIVE"}, {ID: "gone", Status: "DELETED"}}}); err != nil {
		t.Fatal(err)
	}
	resources, err := c.BillingResources(ctx, system)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 || resources[0].ID != "compute/real" || resources[0].Units != 3 || resources[1].Units != 12345 || objects.prefix != "workspaces/owner/systems/app/" {
		t.Fatalf("unexpected allocation: %+v prefix=%s", resources, objects.prefix)
	}
	provider.missing = true
	c.compute = provider
	if _, err = c.BillingResources(ctx, system); err == nil {
		t.Fatal("partial inventory accepted")
	}
}
