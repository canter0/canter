package controlplane

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

// providerBackendFixture wires a fake compute client plus a real enforcer
// HTTP server so backend tests exercise the typed client end to end.
type providerBackendFixture struct {
	backend *ciLifecycleProviderBackend
	engine  *ciEnforcerEngine
	fc      *fakeCICompute
	srv     *httptest.Server
	token   []byte
	client  *ciEnforcerClient
}

func newProviderBackendFixture(t *testing.T) *providerBackendFixture {
	t.Helper()
	fx := &providerBackendFixture{fc: newFakeCICompute(), token: []byte(strings.Repeat("e", 40))}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := newCIEnforcerEngine(dir, fx.fc, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fx.engine = e
	t.Cleanup(e.close)
	srv := &CIEnforcerServer{engine: e, token: fx.token}
	srv.handler = srv.routes()
	fx.srv = httptest.NewServer(srv)
	t.Cleanup(fx.srv.Close)
	fx.client, err = newCIEnforcerClient(fx.srv.URL, fx.token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	fx.backend = &ciLifecycleProviderBackend{Compute: fx.fc, Enforcer: fx.client}
	return fx
}

func providerTestPlan(opID string) ciRunnerLifecyclePlan {
	clk := newFakeClock()
	spec := enforcerTestSpecAt(opID, clk.wall, 800*time.Second, 20000*time.Second)
	return spec.Plan
}

func providerTestSpec(plan ciRunnerLifecyclePlan) ciEnforcerSpec {
	return ciEnforcerSpec{
		Plan: plan, GitHubInstallationID: 1, GitHubRepositoryID: 2, SizeID: "linux-x64-2",
	}
}

func TestCILifecycleProviderObserveMergesEnforcerInventory(t *testing.T) {
	fx := newProviderBackendFixture(t)
	plan := providerTestPlan("op-backend-1")
	if _, err := fx.client.PutLease(context.Background(), providerTestSpec(plan)); err != nil {
		t.Fatalf("PutLease: %v", err)
	}
	// Daemon retains a VM ID via close-creation; CP retained slice stays empty.
	scope := compute.CIRunnerScope{OperationID: plan.OperationID, ServerName: plan.ServerName, Region: plan.Region, NetworkID: plan.NetworkID}
	fx.fc.addServer(scope, "srv-daemon-1", "ACTIVE")
	if _, err := fx.engine.closeCreation(context.Background(), plan.EnforcerLease, "created", "srv-daemon-1"); err != nil {
		t.Fatalf("closeCreation: %v", err)
	}
	// Provider now reports the server gone (direct 404) while CP retained is
	// empty: the daemon inventory must still surface the direct read AND be
	// carried as authenticated Retained, chained through root evidence.
	fx.fc.removeServer("srv-daemon-1")
	obs, err := fx.backend.Observe(context.Background(), plan, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	retainedFound := false
	for _, r := range obs.Retained {
		if r.Kind == "server" && r.ID == "srv-daemon-1" {
			retainedFound = true
		}
	}
	if !retainedFound {
		t.Fatalf("daemon inventory must be emitted as Retained, got %+v", obs.Retained)
	}
	readFound := false
	for _, r := range obs.Reads {
		if r.Resource.Kind == "server" && r.Resource.ID == "srv-daemon-1" && r.HTTPStatus == 404 {
			readFound = true
		}
	}
	if !readFound {
		t.Fatalf("daemon-retained ID must produce a direct 404 read, got %+v", obs.Reads)
	}
	// Chained through the ROOT evidence function: the Retained 404 read is
	// accepted and the resource is retained in the combined inventory.
	combined, stopped, absent, err := ciResourceEvidence(plan.OperationID, nil, obs)
	if err != nil {
		t.Fatalf("root evidence must accept retained 404: %v", err)
	}
	kept := false
	for _, r := range combined {
		if r.Kind == "server" && r.ID == "srv-daemon-1" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("retained 404 resource must persist in combined inventory")
	}
	_ = stopped
	_ = absent
	if obs.CreateSettled != true {
		t.Fatalf("createSettled must come from exact enforcer status")
	}
	if obs.EnforcerSettled {
		t.Fatalf("unsettled enforcer must not report settled")
	}
}

func TestCILifecycleProviderRetainedRequiresRead(t *testing.T) {
	fx := newProviderBackendFixture(t)
	plan := providerTestPlan("op-backend-retained")
	if _, err := fx.client.PutLease(context.Background(), providerTestSpec(plan)); err != nil {
		t.Fatal(err)
	}
	scope := compute.CIRunnerScope{OperationID: plan.OperationID, ServerName: plan.ServerName, Region: plan.Region, NetworkID: plan.NetworkID}
	fx.fc.addServer(scope, "srv-needread-1", "ACTIVE")
	if _, err := fx.engine.closeCreation(context.Background(), plan.EnforcerLease, "created", "srv-needread-1"); err != nil {
		t.Fatal(err)
	}
	obs, err := fx.backend.Observe(context.Background(), plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every Retained item must have a direct read or root evidence conflicts.
	reads := map[string]int{}
	for _, r := range obs.Reads {
		reads[r.Resource.Kind+"/"+r.Resource.ID] = r.HTTPStatus
	}
	for _, r := range obs.Retained {
		if _, ok := reads[r.Kind+"/"+r.ID]; !ok {
			t.Fatalf("retained %s/%s lacks a direct read", r.Kind, r.ID)
		}
	}
	// A missing read is rejected by root evidence.
	broken := obs
	broken.Reads = nil
	if _, _, _, err := ciResourceEvidence(plan.OperationID, nil, broken); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing retained read must conflict, got %v", err)
	}
}

func TestCILifecycleProviderAliveEnforcerResourceIsRetained(t *testing.T) {
	fx := newProviderBackendFixture(t)
	plan := providerTestPlan("op-backend-2")
	if _, err := fx.client.PutLease(context.Background(), providerTestSpec(plan)); err != nil {
		t.Fatal(err)
	}
	scope := compute.CIRunnerScope{OperationID: plan.OperationID, ServerName: plan.ServerName, Region: plan.Region, NetworkID: plan.NetworkID}
	fx.fc.addServer(scope, "srv-live-1", "ACTIVE")
	if _, err := fx.engine.closeCreation(context.Background(), plan.EnforcerLease, "created", "srv-live-1"); err != nil {
		t.Fatal(err)
	}
	obs, err := fx.backend.Observe(context.Background(), plan, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Daemon inventory is Retained (authenticated creator inventory), and a
	// live 200 read keeps it present — never fabricated as discovery.
	retained := false
	for _, r := range obs.Retained {
		if r.Kind == "server" && r.ID == "srv-live-1" {
			retained = true
		}
	}
	if !retained {
		t.Fatalf("alive enforcer resource must be Retained, got %+v", obs.Retained)
	}
	for _, r := range obs.Discovered {
		if r.Kind == "server" && r.ID == "srv-live-1" {
			t.Fatalf("daemon inventory must not be fabricated as Discovered")
		}
	}
}

func TestCILifecycleProviderEnforcerUnavailableWithholdsProofs(t *testing.T) {
	fx := newProviderBackendFixture(t)
	plan := providerTestPlan("op-backend-3")
	scope := compute.CIRunnerScope{OperationID: plan.OperationID, ServerName: plan.ServerName, Region: plan.Region, NetworkID: plan.NetworkID}
	fx.fc.addServer(scope, "srv-down-1", "ACTIVE")
	// No lease exists and the enforcer HTTP endpoint is gone: status fails.
	fx.srv.Close()
	obs, err := fx.backend.Observe(context.Background(), plan, []ciOwnedResource{{Kind: "server", ID: "srv-down-1", OperationID: plan.OperationID}})
	if err != nil {
		t.Fatalf("enforcer outage must not fail the provider observation: %v", err)
	}
	if obs.CreateSettled || obs.EnforcerSettled {
		t.Fatalf("unavailable enforcer must withhold both proofs")
	}
	// Physical stop/delete must still work with the enforcer down.
	if err := fx.backend.Stop(context.Background(), plan, []ciOwnedResource{{Kind: "server", ID: "srv-down-1", OperationID: plan.OperationID}}); err != nil {
		t.Fatalf("Stop must proceed during enforcer outage: %v", err)
	}
	if err := fx.backend.Delete(context.Background(), plan, []ciOwnedResource{{Kind: "server", ID: "srv-down-1", OperationID: plan.OperationID}}); err != nil {
		t.Fatalf("Delete must proceed during enforcer outage: %v", err)
	}
	if atomic.LoadInt32(&fx.fc.deleteCalls) == 0 {
		t.Fatalf("physical delete must not depend on enforcer connectivity")
	}
	if err := fx.backend.SettleEnforcer(context.Background(), plan); !errors.Is(err, errCILifecycleSettlementPending) {
		t.Fatalf("settlement must stay pending without enforcer proof, got %v", err)
	}
}

func TestCILifecycleProviderScopeMismatchConflicts(t *testing.T) {
	fx := newProviderBackendFixture(t)
	plan := providerTestPlan("op-backend-4")
	if _, err := fx.client.PutLease(context.Background(), providerTestSpec(plan)); err != nil {
		t.Fatal(err)
	}
	// First-call wrong operation: the status returned for this plan's lease
	// cannot match a different expected operation -> authenticated conflict.
	wrongOp := plan
	wrongOp.OperationID = "op-intruder"
	wrongOp.ServerName = ciEnforcerRunnerName("op-intruder")
	_, err := fx.backend.Observe(context.Background(), wrongOp, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-operation status must map to ErrConflict, got %v", err)
	}
	// A plan pointing at a lease the enforcer does not know is an
	// availability case: 404 withholds proofs but owned physical cleanup
	// still proceeds — never an authenticated conflict.
	wrongLease := plan
	otherLease, _ := ciEnforcerNewLeaseID()
	wrongLease.EnforcerLease = otherLease
	obs, err := fx.backend.Observe(context.Background(), wrongLease, nil)
	if err != nil {
		t.Fatalf("missing lease must withhold proofs, not fail: %v", err)
	}
	if obs.CreateSettled || obs.EnforcerSettled {
		t.Fatalf("missing lease must withhold both proofs")
	}
	// Wrong plan digest: tamper a deadline so the pinned digest differs ->
	// authenticated scope conflict on the first call.
	wrongDigest := plan
	wrongDigest.HardDeadlineAt = wrongDigest.HardDeadlineAt.Add(time.Second)
	_, err = fx.backend.Observe(context.Background(), wrongDigest, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong-digest status must map to ErrConflict, got %v", err)
	}
}

func TestCILifecycleProviderDeleteErrorMapping(t *testing.T) {
	fx := newProviderBackendFixture(t)
	plan := providerTestPlan("op-backend-6")
	scope := compute.CIRunnerScope{OperationID: plan.OperationID, ServerName: plan.ServerName, Region: plan.Region, NetworkID: plan.NetworkID}
	fx.fc.addServer(scope, "srv-map-1", "ACTIVE")
	res := []ciOwnedResource{{Kind: "server", ID: "srv-map-1", OperationID: plan.OperationID}}
	// Pending delete maps to nil (worker observes absence itself).
	fx.fc.mu.Lock()
	fx.fc.deleteErr = compute.ErrCIRunnerPending
	fx.fc.mu.Unlock()
	if err := fx.backend.Delete(context.Background(), plan, res); err != nil {
		t.Fatalf("pending delete must map to nil, got %v", err)
	}
	// Ownership conflict maps to ErrConflict.
	fx.fc.mu.Lock()
	fx.fc.deleteErr = compute.ErrCIRunnerOwnership
	fx.fc.mu.Unlock()
	if err := fx.backend.Delete(context.Background(), plan, res); !errors.Is(err, ErrConflict) {
		t.Fatalf("ownership delete must map to ErrConflict, got %v", err)
	}
}
