package controlplane

import (
	"context"
	"errors"

	"github.com/canter0/canter/internal/provider/compute"
)

// ciLifecycleCompute is the narrow shared-compute seam; *compute.Client
// satisfies it and tests may substitute fixtures.
type ciLifecycleCompute interface {
	ObserveCIRunner(ctx context.Context, scope compute.CIRunnerScope, retained []compute.CIRunnerResource) (compute.CIRunnerObservation, error)
	StopCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error
	DeleteCIRunner(ctx context.Context, scope compute.CIRunnerScope, resources []compute.CIRunnerResource) error
}

// ciLifecycleProviderBackend is the concrete ciRunnerLifecycleBackend that
// combines fresh shared-compute observations with authenticated enforcer
// status. Transport outages withhold proofs but never block physical
// Stop/Delete of already-retained owned resources.
type ciLifecycleProviderBackend struct {
	Compute  ciLifecycleCompute
	Enforcer *ciEnforcerClient
}

func ciProviderScope(plan ciRunnerLifecyclePlan) compute.CIRunnerScope {
	return compute.CIRunnerScope{
		OperationID: plan.OperationID,
		ServerName:  plan.ServerName,
		Region:      plan.Region,
		NetworkID:   plan.NetworkID,
	}
}

func ciProviderResources(resources []ciOwnedResource) []compute.CIRunnerResource {
	out := make([]compute.CIRunnerResource, 0, len(resources))
	for _, r := range resources {
		out = append(out, compute.CIRunnerResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
	}
	return out
}

func ciOwnedFromProvider(r compute.CIRunnerResource) ciOwnedResource {
	return ciOwnedResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID}
}

// enforcerStatus fetches authenticated enforcer status plus its retained
// resource inventory, bound to the caller's pinned plan. A transport or
// availability failure withholds both proofs (fail closed) while preserving
// the control-plane inventory; an authenticated scope/digest/ID mismatch is
// a hard conflict.
func (b *ciLifecycleProviderBackend) enforcerStatus(ctx context.Context, plan ciRunnerLifecyclePlan) (ciEnforcerStatus, bool, error) {
	if b.Enforcer == nil {
		return ciEnforcerStatus{}, false, nil
	}
	st, err := b.Enforcer.Status(ctx, plan)
	if err != nil {
		if errors.Is(err, errCIEnforcerScope) {
			return ciEnforcerStatus{}, false, ErrConflict
		}
		return ciEnforcerStatus{}, false, nil
	}
	return st, true, nil
}

func (b *ciLifecycleProviderBackend) Observe(ctx context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) (ciResourceObservation, error) {
	if b == nil || b.Compute == nil {
		return ciResourceObservation{}, errCILifecycleSettlementPending
	}
	st, enforcerOK, err := b.enforcerStatus(ctx, plan)
	if err != nil {
		return ciResourceObservation{}, err
	}
	// Union the daemon's authenticated retained inventory with the
	// control-plane retained set BEFORE the shared compute observation, so a
	// direct provider 404 can prove disappearance even when discovery is
	// empty. Neither set is ever dropped.
	retained := append([]compute.CIRunnerResource(nil), ciProviderResources(resources)...)
	seen := make(map[string]bool, len(retained))
	for _, r := range retained {
		seen[r.Kind+"/"+r.ID] = true
	}
	for _, r := range st.Resources {
		key := r.Kind + "/" + r.ID
		if !seen[key] {
			seen[key] = true
			retained = append(retained, compute.CIRunnerResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
		}
	}
	scope := ciProviderScope(plan)
	obs, err := b.Compute.ObserveCIRunner(ctx, scope, retained)
	if err != nil {
		if errors.Is(err, compute.ErrCIRunnerOwnership) {
			return ciResourceObservation{}, ErrConflict
		}
		return ciResourceObservation{}, err
	}
	out := ciResourceObservation{
		OperationID:       plan.OperationID,
		Retained:          make([]ciOwnedResource, 0, len(st.Resources)),
		Discovered:        make([]ciOwnedResource, 0, len(obs.Discovered)),
		Reads:             make([]ciResourceRead, 0, len(obs.Reads)),
		ServerPorts:       make([]ciServerPorts, 0, len(obs.ServerPorts)),
		DiscoveryComplete: obs.DiscoveryComplete,
	}
	// The daemon's authenticated inventory is reported as Retained — the
	// durable creator inventory channel — never fabricated as discovery.
	for _, r := range st.Resources {
		out.Retained = append(out.Retained, r)
	}
	for _, r := range obs.Discovered {
		out.Discovered = append(out.Discovered, ciOwnedFromProvider(r))
	}
	for _, r := range obs.Reads {
		out.Reads = append(out.Reads, ciResourceRead{
			Resource:    ciOwnedFromProvider(r.Resource),
			HTTPStatus:  r.HTTPStatus,
			ServerState: r.ServerState,
		})
	}
	for _, p := range obs.ServerPorts {
		out.ServerPorts = append(out.ServerPorts, ciServerPorts{
			ServerID: p.ServerID, Complete: p.Complete, PortIDs: p.PortIDs,
		})
	}
	if enforcerOK {
		out.CreateSettled = st.CreateSettled
		out.EnforcerSettled = st.Settled
	}
	return out, nil
}

func (b *ciLifecycleProviderBackend) Stop(ctx context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) error {
	if b == nil || b.Compute == nil {
		return errCILifecycleSettlementPending
	}
	// Durable enforcer stop is best-effort: an enforcer outage must not
	// prevent the owned physical stop below, and a generic stop failure must
	// not prevent the worker's Delete path.
	if b.Enforcer != nil {
		_, _ = b.Enforcer.Stop(ctx, plan)
	}
	err := b.Compute.StopCIRunner(ctx, ciProviderScope(plan), ciProviderResources(resources))
	if errors.Is(err, compute.ErrCIRunnerOwnership) {
		return ErrConflict
	}
	return err
}

func (b *ciLifecycleProviderBackend) Delete(ctx context.Context, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) error {
	if b == nil || b.Compute == nil {
		return errCILifecycleSettlementPending
	}
	err := b.Compute.DeleteCIRunner(ctx, ciProviderScope(plan), ciProviderResources(resources))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, compute.ErrCIRunnerPending):
		// A pending provider delete is not verified absence; the worker's
		// fresh observation decides.
		return nil
	case errors.Is(err, compute.ErrCIRunnerOwnership):
		return ErrConflict
	default:
		return err
	}
}

// SettleEnforcer requests a durable stop then probes independent enforcer
// completion. There is no caller-forced settled flag; it returns pending
// until the service proves its own tombstone.
func (b *ciLifecycleProviderBackend) SettleEnforcer(ctx context.Context, plan ciRunnerLifecyclePlan) error {
	if b == nil || b.Enforcer == nil {
		return errCILifecycleSettlementPending
	}
	if _, err := b.Enforcer.Stop(ctx, plan); err != nil {
		if errors.Is(err, errCIEnforcerScope) {
			return ErrConflict
		}
		// Stop transport failure still allows the status probe below.
	}
	st, err := b.Enforcer.Status(ctx, plan)
	if err != nil {
		if errors.Is(err, errCIEnforcerScope) {
			return ErrConflict
		}
		return errCILifecycleSettlementPending
	}
	if !st.Settled {
		return errCILifecycleSettlementPending
	}
	return nil
}
