package sdk

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

// recoveryCompute wraps the shared fake to return controlled partial and
// ambiguous exposure lookups without editing the shared fixture.
type recoveryCompute struct {
	*fakeCompute
	findErr          error
	findPolicy       *compute.SecurityPolicy
	findErrs         []error
	deletePolicyErrs []error
}

func (f *recoveryCompute) FindManagedTCPExposure(ctx context.Context, input compute.ManagedTCPExposureRequest) (compute.SecurityPolicy, bool, error) {
	if len(f.findErrs) > 0 {
		err := f.findErrs[0]
		f.findErrs = f.findErrs[1:]
		return compute.SecurityPolicy{}, false, err
	}
	if f.findErr != nil {
		return compute.SecurityPolicy{}, false, f.findErr
	}
	if f.findPolicy != nil {
		return *f.findPolicy, true, nil
	}
	return f.fakeCompute.FindManagedTCPExposure(ctx, input)
}

func (f *recoveryCompute) DeleteSecurityPolicy(ctx context.Context, id string) error {
	if len(f.deletePolicyErrs) > 0 {
		err := f.deletePolicyErrs[0]
		f.deletePolicyErrs = f.deletePolicyErrs[1:]
		if err != nil {
			return err
		}
	}
	return f.fakeCompute.DeleteSecurityPolicy(ctx, id)
}

func recoveryTestPlan(spec Spec) VPSPlan {
	return VPSPlan{Spec: spec, Shape: compute.Shape{ID: "shape-1", Name: "shape"}, ImageID: "image-1", Networks: []string{"network-a"}}
}

func ownedPartialPolicyErr() error {
	return &compute.AmbiguousManagedResourceError{
		Kind:        "network policy attachment",
		Identity:    "server-1:demo-ssh",
		Cause:       fmt.Errorf("owned policy exists but is not attached"),
		OwnedPolicy: &compute.SecurityPolicy{ID: "policy-1", PortID: "port-1", RuleID: "rule-1", Port: 22},
	}
}

func sshIntent(spec Spec, operationID, serverID, phase string, mutate func(*ExposureIntent)) *ExposureIntent {
	sum := sha256.Sum256([]byte(spec.Spec.M1.Prefix))
	intent := &ExposureIntent{
		OperationID: operationID, ServerID: serverID, Name: spec.Metadata.Name + "-ssh",
		Ownership: fmt.Sprintf("sha256:%x", sum), Protocol: "tcp", Port: 22,
		Phase: phase, MutationUnresolved: true, CreatedAt: time.Now().UTC(),
	}
	if mutate != nil {
		mutate(intent)
	}
	return intent
}

func sshPolicy(spec Spec) NetworkPolicy {
	sum := sha256.Sum256([]byte(spec.Spec.M1.Prefix))
	return NetworkPolicy{ID: "policy-1", PortID: "port-1", RuleID: "rule-1", Name: spec.Metadata.Name + "-ssh", Ownership: fmt.Sprintf("sha256:%x", sum), Protocol: "tcp", Port: 22}
}

func seedEscalatedSSHState(t *testing.T, store *fakeStore, spec Spec, mutate func(*State)) State {
	t.Helper()
	state := readyState(spec, "op-create", "server-1")
	state.Phase = "escalated"
	state.ExposureIntent = sshIntent(spec, "op-expose", "server-1", "escalated", nil)
	if mutate != nil {
		mutate(&state)
	}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}
	return state
}

// A restart after the provider created the SSH policy and its rule but was
// interrupted before the port attachment: the ready VM and durable intent
// persist with the mutation unresolved and lookup reports the owned partial.
func TestVPSApplyRecoversInterruptedSSHAttachment(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{fakeCompute: provider, findErr: ownedPartialPolicyErr()}
	client.compute = wrapped
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("op-create", "server-1")}
	state := readyState(spec, "op-create", "server-1")
	state.ExposureIntent = sshIntent(spec, "op-expose", "server-1", "creating", nil)
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}

	got, err := client.ApplyVPS(context.Background(), recoveryTestPlan(spec))
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "ready" || provider.createCalls != 0 || provider.resolveCalls != 0 || provider.exposeCalls != 1 || got.Resources[0].ID != "server-1" {
		t.Fatalf("state=%+v create=%d resolve=%d expose=%d", got, provider.createCalls, provider.resolveCalls, provider.exposeCalls)
	}
	if got.ExposureIntent.OperationID != "op-expose" || got.ExposureIntent.Phase != "ready" || got.ExposureIntent.MutationUnresolved {
		t.Fatalf("intent=%+v", got.ExposureIntent)
	}
	if len(got.NetworkPolicies) != 1 || got.NetworkPolicies[0].ID != "policy-1" || got.NetworkPolicies[0].Port != 22 {
		t.Fatalf("policies=%+v", got.NetworkPolicies)
	}
}

func TestVPSApplyRecoversPersistedEscalatedPartial(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{fakeCompute: provider, findErr: ownedPartialPolicyErr()}
	client.compute = wrapped
	seedEscalatedSSHState(t, store, spec, nil)

	state, err := client.ApplyVPS(context.Background(), recoveryTestPlan(spec))
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "ready" || provider.createCalls != 0 || provider.exposeCalls != 1 {
		t.Fatalf("state=%+v create=%d expose=%d", state, provider.createCalls, provider.exposeCalls)
	}
	if state.ExposureIntent.OperationID != "op-expose" || state.ExposureIntent.Phase != "ready" || len(state.NetworkPolicies) != 1 || state.NetworkPolicies[0].ID != "policy-1" {
		t.Fatalf("intent=%+v policies=%+v", state.ExposureIntent, state.NetworkPolicies)
	}
}

func TestVPSDestroyDeletesOwnedPartialPolicy(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{fakeCompute: provider, findErr: ownedPartialPolicyErr()}
	client.compute = wrapped
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("op-create", "server-1")}
	state := readyState(spec, "op-create", "server-1")
	state.ExposureIntent = sshIntent(spec, "op-expose", "server-1", "creating", nil)
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}

	got, err := client.DestroyVPS(context.Background(), recoveryTestPlan(spec))
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "destroyed" || provider.exposeCalls != 0 || provider.createCalls != 0 {
		t.Fatalf("state=%+v expose=%d create=%d", got, provider.exposeCalls, provider.createCalls)
	}
	if len(provider.deletedServers) != 1 || provider.deletedServers[0] != "server-1" || len(provider.deletedPolicies) != 1 || provider.deletedPolicies[0] != "policy-1" {
		t.Fatalf("servers=%v policies=%v", provider.deletedServers, provider.deletedPolicies)
	}
}

// An escalated state whose exact policy was already tracked adopts the lookup
// result and finishes ready without any new ensure or provider mutation.
func TestVPSApplyRecoversEscalatedWithTrackedPolicy(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	policy := sshPolicy(spec)
	found := compute.SecurityPolicy{ID: policy.ID, PortID: policy.PortID, RuleID: policy.RuleID, Port: policy.Port}
	wrapped := &recoveryCompute{fakeCompute: provider, findPolicy: &found}
	client.compute = wrapped
	seedEscalatedSSHState(t, store, spec, func(s *State) { s.NetworkPolicies = []NetworkPolicy{policy} })

	state, err := client.ApplyVPS(context.Background(), recoveryTestPlan(spec))
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "ready" || state.ExposureIntent.Phase != "ready" || state.ExposureIntent.MutationUnresolved {
		t.Fatalf("state=%+v intent=%+v", state, state.ExposureIntent)
	}
	if provider.exposeCalls != 0 || provider.createCalls != 0 {
		t.Fatalf("tracked policy must be adopted without ensure, expose=%d create=%d", provider.exposeCalls, provider.createCalls)
	}
}

func TestVPSExposureRecoveryFailClosed(t *testing.T) {
	cases := []struct {
		name       string
		findErr    error
		findPolicy *compute.SecurityPolicy
		mutate     func(*State)
	}{
		{name: "duplicate", findErr: &compute.DuplicateManagedResourceError{Kind: "network policy", Identity: "demo-ssh", Count: 2}},
		{name: "unknown", findErr: errors.New("provider transport reset")},
		{name: "absent"},
		{name: "unowned ambiguous", findErr: &compute.AmbiguousManagedResourceError{Kind: "network policy", Identity: "demo-ssh", Cause: errors.New("lookup ambiguous")}},
		{name: "wrong ownership", mutate: func(s *State) { s.ExposureIntent.Ownership = "sha256:deadbeef" }},
		{name: "wrong server", mutate: func(s *State) { s.ExposureIntent.ServerID = "server-9" }},
		{name: "wrong port", mutate: func(s *State) { s.ExposureIntent.Port = 443 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			client, store, provider, _ := testClient(spec)
			wrapped := &recoveryCompute{fakeCompute: provider, findErr: tc.findErr, findPolicy: tc.findPolicy}
			client.compute = wrapped
			seedEscalatedSSHState(t, store, spec, tc.mutate)

			state, err := client.ApplyVPS(context.Background(), recoveryTestPlan(spec))
			if err == nil {
				t.Fatalf("expected fail-closed, got %+v", state)
			}
			if provider.exposeCalls != 0 {
				t.Fatalf("recovery must not ensure a new policy, expose=%d", provider.exposeCalls)
			}
		})
	}
}

func TestVPSExposureRecoveryLifecycleSafety(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*State)
	}{
		{name: "failed destroy", mutate: func(s *State) { s.CreationIntent.Phase = "escalated" }},
		{name: "escalated creation", mutate: func(s *State) { s.CreationIntent.Phase = "escalated"; s.CreationIntent.Resources[0].Phase = "creating" }},
		{name: "destroyed tombstone", mutate: func(s *State) {
			s.Phase = "destroyed"
			s.CreationIntent.Phase = "destroyed"
			s.ExposureIntent.Phase = "destroyed"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			client, store, provider, _ := testClient(spec)
			wrapped := &recoveryCompute{fakeCompute: provider, findErr: ownedPartialPolicyErr()}
			client.compute = wrapped
			seedEscalatedSSHState(t, store, spec, tc.mutate)

			state, err := client.ApplyVPS(context.Background(), recoveryTestPlan(spec))
			if err == nil {
				t.Fatalf("expected refusal, got %+v", state)
			}
			if provider.exposeCalls != 0 || provider.createCalls != 0 {
				t.Fatalf("must not reopen lifecycle, expose=%d create=%d", provider.exposeCalls, provider.createCalls)
			}
		})
	}
}

// First destroy adopts the owned partial policy and records it before the VM
// delete; when the policy delete fails, a retry can no longer discover the
// policy by server port but finishes cleanup from the persisted owned record.
func TestVPSDestroyRetriesOwnedPolicyAfterVMGone(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{
		fakeCompute:      provider,
		findErrs:         []error{ownedPartialPolicyErr(), errors.New("policy port lookup failed")},
		deletePolicyErrs: []error{errors.New("provider delete timeout")},
	}
	client.compute = wrapped
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("op-create", "server-1")}
	state := readyState(spec, "op-create", "server-1")
	state.ExposureIntent = sshIntent(spec, "op-expose", "server-1", "creating", nil)
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}

	first, err := client.DestroyVPS(context.Background(), recoveryTestPlan(spec))
	if err == nil {
		t.Fatalf("first destroy must report the injected policy delete failure, got %+v", first)
	}
	if len(provider.deletedServers) != 1 || len(provider.deletedPolicies) != 0 {
		t.Fatalf("VM must be deleted before the failed policy delete, servers=%v policies=%v", provider.deletedServers, provider.deletedPolicies)
	}
	var persisted State
	if err := store.Get(context.Background(), stateKey(spec), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "escalated" || len(persisted.NetworkPolicies) != 1 || persisted.NetworkPolicies[0].ID != "policy-1" {
		t.Fatalf("owned policy must be persisted for retry, state=%+v", persisted)
	}

	second, err := client.DestroyVPS(context.Background(), recoveryTestPlan(spec))
	if err != nil {
		t.Fatal(err)
	}
	if second.Phase != "destroyed" || len(provider.deletedPolicies) != 1 || provider.deletedPolicies[0] != "policy-1" {
		t.Fatalf("state=%+v policies=%v", second, provider.deletedPolicies)
	}
	if provider.exposeCalls != 0 || provider.createCalls != 0 {
		t.Fatalf("cleanup must never ensure or create, expose=%d create=%d", provider.exposeCalls, provider.createCalls)
	}
}

// A duplicate lookup error always fails closed: even with the VM gone and an
// exact owned policy persisted, cleanup must not delete on unresolved
// ambiguity or mark the sandbox destroyed.
func TestVPSDestroyDuplicateLookupAfterVMGoneFailsClosed(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{fakeCompute: provider, findErr: &compute.DuplicateManagedResourceError{Kind: "network policy", Identity: "demo-ssh", Count: 2}}
	client.compute = wrapped
	state := readyState(spec, "op-create", "server-1")
	state.Phase = "destroying"
	state.CreationIntent.Phase = "destroying"
	state.CreationIntent.Resources[0].Phase = "destroyed"
	state.Resources[0].Status = "DELETED"
	state.ExposureIntent = sshIntent(spec, "op-expose", "server-1", "destroying", nil)
	state.NetworkPolicies = []NetworkPolicy{sshPolicy(spec)}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}

	got, err := client.DestroyVPS(context.Background(), recoveryTestPlan(spec))
	if err == nil {
		t.Fatalf("destroy must fail closed on duplicate lookup, got %+v", got)
	}
	if len(provider.deletedPolicies) != 0 || provider.exposeCalls != 0 {
		t.Fatalf("nothing may be deleted or ensured on ambiguity, policies=%v expose=%d", provider.deletedPolicies, provider.exposeCalls)
	}
	var final State
	if err := store.Get(context.Background(), stateKey(spec), &final); err != nil {
		t.Fatal(err)
	}
	if final.Phase == "destroyed" {
		t.Fatal("unresolved ambiguity must not mark the sandbox destroyed")
	}
}

// An unknown lookup error cannot be bypassed while the VM is still present,
// even when a matching policy was already tracked.
func TestVPSDestroyUnknownLookupWithLiveVMFailsClosed(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{fakeCompute: provider, findErr: errors.New("provider transport reset")}
	client.compute = wrapped
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("op-create", "server-1")}
	state := readyState(spec, "op-create", "server-1")
	state.ExposureIntent = sshIntent(spec, "op-expose", "server-1", "creating", nil)
	state.NetworkPolicies = []NetworkPolicy{sshPolicy(spec)}
	if err := store.PutJSON(context.Background(), stateKey(spec), state); err != nil {
		t.Fatal(err)
	}

	if _, err := client.DestroyVPS(context.Background(), recoveryTestPlan(spec)); err == nil {
		t.Fatal("destroy must fail closed on unknown lookup while the VM lives")
	}
	if len(provider.deletedServers) != 0 || len(provider.deletedPolicies) != 0 {
		t.Fatalf("nothing may be deleted on unknown lookup, servers=%v policies=%v", provider.deletedServers, provider.deletedPolicies)
	}
}

func TestVPSConcurrentDestroyDuringRecoveryCannotResurrect(t *testing.T) {
	spec := testSpec()
	client, store, provider, _ := testClient(spec)
	wrapped := &recoveryCompute{fakeCompute: provider, findErr: ownedPartialPolicyErr()}
	client.compute = wrapped
	provider.servers["canter-demo-1"] = []compute.Server{managedTestServer("op-create", "server-1")}
	provider.exposeAccepted = make(chan struct{})
	provider.exposeRelease = make(chan struct{})
	seedEscalatedSSHState(t, store, spec, nil)

	plan := recoveryTestPlan(spec)
	applyDone := make(chan struct{})
	var applyState State
	var applyErr error
	go func() {
		defer close(applyDone)
		applyState, applyErr = client.ApplyVPS(context.Background(), plan)
	}()

	select {
	case <-provider.exposeAccepted:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery ensure did not start")
	}
	if _, err := client.DestroyVPS(context.Background(), plan); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	close(provider.exposeRelease)
	<-applyDone
	if applyErr == nil {
		t.Fatalf("apply must not complete the endpoint after destroy, got %+v", applyState)
	}
	var final State
	if err := store.Get(context.Background(), stateKey(spec), &final); err != nil {
		t.Fatal(err)
	}
	if final.Phase != "destroyed" || len(final.NetworkPolicies) != 0 {
		t.Fatalf("destroy must win the lifecycle fence, final=%+v", final)
	}
	if provider.createCalls != 0 || len(provider.deletedServers) != 1 {
		t.Fatalf("create=%d deleted=%v", provider.createCalls, provider.deletedServers)
	}
}
