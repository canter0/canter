package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
)

// rejectedLeaseFields asserts the durable stopped, creation-rejected shape a
// rejectUnstarted transition must leave behind.
func rejectedLeaseFields(t *testing.T, l *ciEnforcerLease, spec ciEnforcerSpec) {
	t.Helper()
	if !l.StopRequested || !l.CreateSettled || l.CreateResult != "rejected" ||
		l.CreateServerID != "" || l.Settled || l.RegistrationAttempted ||
		l.RegistrationSettled || l.RegistrationConflict || l.GitHubRunnerID != 0 ||
		l.ApprovedDeadlineUnixMs != 0 || l.PhysicalAbsent || l.RegAbsent {
		t.Fatalf("bad rejected lease: %+v", l)
	}
	if l.DeadlineUnixMs != spec.Plan.SetupDeadlineAt.UnixMilli() {
		t.Fatalf("deadline must remain the original setup cutoff %d, got %d",
			spec.Plan.SetupDeadlineAt.UnixMilli(), l.DeadlineUnixMs)
	}
}

// TestCIEnforcerRejectUnstartedMissingPastDeadlines covers the upstream gap:
// funding committed but no stage ever fenced, and the setup or even hard
// deadline already passed. Normal Put stays refused while the STOP-ONLY
// reject allocates the durable rejected record without any provider or
// GitHub I/O.
func TestCIEnforcerRejectUnstartedMissingPastDeadlines(t *testing.T) {
	for _, advance := range []time.Duration{1000 * time.Second, 21000 * time.Second} {
		t.Run(fmt.Sprintf("advance_%ds", int64(advance.Seconds())), func(t *testing.T) {
			gh, _, ghClient := newGitHubFixture(t)
			fc := newFakeCICompute()
			clk := newFakeClock()
			e, _ := newTestEngineAt(t, fc, ghClient, clk)
			spec := enforcerTestSpecAt(fmt.Sprintf("op-rej-%d", int64(advance.Seconds())), clk.wall, 800*time.Second, 20000*time.Second)
			leaseID := spec.Plan.EnforcerLease
			clk.advance(advance) // past setup (800s) or past hard (20000s) before ANY put

			if _, err := e.putLease(leaseID, spec); !errors.Is(err, errCIEnforcerConflict) {
				t.Fatalf("expired Put must conflict, got %v", err)
			}
			st, err := e.rejectUnstarted(leaseID, spec)
			if err != nil {
				t.Fatalf("rejectUnstarted past deadline: %v", err)
			}
			if !st.StopRequested || !st.CreateSettled || st.Settled {
				t.Fatalf("bad reject status: %+v", st)
			}
			if st.DeadlineUnixMs != spec.Plan.SetupDeadlineAt.UnixMilli() {
				t.Fatalf("status deadline must stay original setup %d, got %d",
					spec.Plan.SetupDeadlineAt.UnixMilli(), st.DeadlineUnixMs)
			}
			rejectedLeaseFields(t, leaseState(t, e, leaseID), spec)

			// The reject itself performs no provider or GitHub I/O.
			if atomic.LoadInt32(&fc.observeCalls) != 0 || atomic.LoadInt32(&fc.stopCalls) != 0 ||
				atomic.LoadInt32(&fc.deleteCalls) != 0 || atomic.LoadInt32(&gh.jitCalls) != 0 ||
				atomic.LoadInt32(&gh.listCalls) != 0 {
				t.Fatalf("rejectUnstarted must not invoke provider or GitHub I/O")
			}
			// Approve and RegisterRunner stay denied with zero GitHub calls.
			if _, err := e.approve(leaseID, clk.wall.Add(time.Hour).UnixMilli()); !errors.Is(err, errCIEnforcerConflict) {
				t.Fatalf("approve rejected lease must conflict, got %v", err)
			}
			if _, _, _, err := e.registerRunner(context.Background(), leaseID); !errors.Is(err, errCIEnforcerConflict) {
				t.Fatalf("register rejected lease must conflict, got %v", err)
			}
			if atomic.LoadInt32(&gh.jitCalls) != 0 || atomic.LoadInt32(&gh.listCalls) != 0 {
				t.Fatalf("rejected lease must never reach GitHub")
			}
		})
	}
}

// TestCIEnforcerRejectUnstartedRetryRestartTombstone proves exact retries are
// durable no-ops, the strict loader accepts the rejected record across a
// restart without renewing the window, cleanup tombstones only after fresh
// absence proofs, and a tombstone retry returns settled status while a
// normal Put still conflicts.
func TestCIEnforcerRejectUnstartedRetryRestartTombstone(t *testing.T) {
	_, _, ghClient := newGitHubFixture(t)
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, dir := newTestEngineAt(t, fc, ghClient, clk)
	spec := enforcerTestSpecAt("op-rej-tomb", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	clk.advance(1000 * time.Second) // past setup before any put

	if _, err := e.rejectUnstarted(leaseID, spec); err != nil {
		t.Fatalf("rejectUnstarted: %v", err)
	}
	journalPath := dir + "/lease-" + leaseID + ".json"
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	// Exact retry before cleanup: identical state, deadline not reset, no rewrite.
	st2, err := e.rejectUnstarted(leaseID, spec)
	if err != nil {
		t.Fatalf("exact reject retry: %v", err)
	}
	if !st2.StopRequested || !st2.CreateSettled || st2.Settled {
		t.Fatalf("retry status: %+v", st2)
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("exact retry must not rewrite the journal")
	}

	// Restart on the same dir and clock: strict loader accepts the rejected
	// record, state preserved, window never renewed.
	e.close()
	wall, mono := clk.pair()
	e2, err := newCIEnforcerEngine(dir, fc, ghClient, wall, mono)
	if err != nil {
		t.Fatalf("rejected lease must pass strict loader on restart: %v", err)
	}
	defer e2.close()
	l := leaseState(t, e2, leaseID)
	rejectedLeaseFields(t, l, spec)
	if l.PhysicalAbsent || l.RegAbsent {
		t.Fatalf("absence proofs must require fresh independent observation: %+v", l)
	}

	// Physical absence first, then GitHub absence, then tombstone.
	e2.reconcilePhysical(context.Background(), leaseID)
	l = leaseState(t, e2, leaseID)
	if !l.PhysicalAbsent || l.Settled || l.RegAbsent {
		t.Fatalf("physical proof must land before settlement: %+v", l)
	}
	e2.reconcileGitHub(context.Background(), leaseID)
	l = leaseState(t, e2, leaseID)
	if !l.RegAbsent || l.Settled {
		t.Fatalf("registration proof must land before settlement: %+v", l)
	}
	e2.reconcilePhysical(context.Background(), leaseID)
	l = leaseState(t, e2, leaseID)
	if !l.Settled {
		t.Fatalf("fully proven rejected lease must tombstone: %+v", l)
	}

	// Exact tombstone retry returns settled status with no private fields and
	// never revives the lease.
	st3, err := e2.rejectUnstarted(leaseID, spec)
	if err != nil {
		t.Fatalf("tombstone reject retry: %v", err)
	}
	if !st3.Settled || !st3.StopRequested || !st3.CreateSettled ||
		st3.DeadlineUnixMs != 0 || st3.GitHubRepositoryID != 0 || st3.GitHubRunnerID != 0 ||
		st3.RunnerName != "" || len(st3.Resources) != 0 {
		t.Fatalf("tombstone retry must return clean settled status: %+v", st3)
	}
	l = leaseState(t, e2, leaseID)
	if !l.Settled || l.DeadlineUnixMs != 0 {
		t.Fatalf("tombstone retry must not revive or rewrite: %+v", l)
	}
	// Normal Put on the same tombstone still conflicts.
	if _, err = e2.putLease(leaseID, spec); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("Put on tombstone must still conflict, got %v", err)
	}
}

// TestCIEnforcerRejectUnstartedPendingPutAndConcurrent covers reject against
// an existing pending lease, Put-after-reject staying stopped, reject-first
// ordering, and a 16-way mixed Put/Reject race converging to one
// stopped+rejected lease.
func TestCIEnforcerRejectUnstartedPendingPutAndConcurrent(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)

	// Existing pending Put first: reject marks stop+rejected, original
	// deadline preserved; plain Put afterwards returns stopped, never revives.
	spec := enforcerTestSpecAt("op-rej-pending", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	if _, err := e.putLease(leaseID, spec); err != nil {
		t.Fatalf("putLease: %v", err)
	}
	if _, err := e.rejectUnstarted(leaseID, spec); err != nil {
		t.Fatalf("rejectUnstarted pending: %v", err)
	}
	rejectedLeaseFields(t, leaseState(t, e, leaseID), spec)
	stPut, err := e.putLease(leaseID, spec)
	if err != nil {
		t.Fatalf("exact Put after reject must stay a no-op status: %v", err)
	}
	if !stPut.StopRequested || !stPut.CreateSettled || stPut.Settled {
		t.Fatalf("Put after reject must report stopped state: %+v", stPut)
	}
	clk.advance(900 * time.Second) // past setup, still before hard
	stPut2, err := e.putLease(leaseID, spec)
	if err != nil {
		t.Fatalf("expired exact Put after reject must stay a no-op status: %v", err)
	}
	if !stPut2.StopRequested || !stPut2.CreateSettled || stPut2.Settled {
		t.Fatalf("expired Put after reject must report stopped state: %+v", stPut2)
	}

	// Reject first on a missing lease, then exact Put: stopped status too.
	specB := enforcerTestSpecAt("op-rej-first", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err := e.rejectUnstarted(specB.Plan.EnforcerLease, specB); err != nil {
		t.Fatalf("rejectUnstarted missing: %v", err)
	}
	stB, err := e.putLease(specB.Plan.EnforcerLease, specB)
	if err != nil {
		t.Fatalf("Put after reject-first must stay a no-op status: %v", err)
	}
	if !stB.StopRequested || !stB.CreateSettled || stB.Settled {
		t.Fatalf("Put after reject-first must report stopped state: %+v", stB)
	}

	// Concurrent 16-way mixed Put/Reject on one future spec: every call
	// succeeds, the final committed state is stop+rejected, one lease only.
	fc3 := newFakeCICompute()
	clk3 := newFakeClock()
	e3, dir3 := newTestEngineAt(t, fc3, nil, clk3)
	specC := enforcerTestSpecAt("op-rej-race", clk3.wall, 800*time.Second, 20000*time.Second)
	leaseC := specC.Plan.EnforcerLease
	var wg sync.WaitGroup
	putErrs := make([]error, 8)
	rejErrs := make([]error, 8)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, putErrs[i/2] = e3.putLease(leaseC, specC)
			} else {
				_, rejErrs[i/2] = e3.rejectUnstarted(leaseC, specC)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range putErrs {
		if err != nil {
			t.Fatalf("concurrent Put %d must not error unsafely: %v", i, err)
		}
	}
	for i, err := range rejErrs {
		if err != nil {
			t.Fatalf("concurrent reject %d must succeed: %v", i, err)
		}
	}
	l := leaseState(t, e3, leaseC)
	rejectedLeaseFields(t, l, specC)
	if len(e3.leases) != 1 || len(e3.opIndex) != 1 || e3.opIndex[specC.Plan.OperationID] != leaseC {
		t.Fatalf("race must converge to exactly one operation/lease: leases=%d opIndex=%d", len(e3.leases), len(e3.opIndex))
	}
	// Restart proves the durable converged state through the strict loader.
	e3.close()
	wall3, mono3 := clk3.pair()
	e4, err := newCIEnforcerEngine(dir3, fc3, nil, wall3, mono3)
	if err != nil {
		t.Fatalf("raced rejected lease must pass strict loader: %v", err)
	}
	defer e4.close()
	rejectedLeaseFields(t, leaseState(t, e4, leaseC), specC)
}

// TestCIEnforcerRejectUnstartedCreatedRegisteredConflict proves the guard
// never turns a created, registered, approved, or in-flight registration
// lease into 'rejected', and that an in-flight registering claim blocks the
// transition without mutation.
func TestCIEnforcerRejectUnstartedCreatedRegisteredConflict(t *testing.T) {
	_, _, ghClient := newGitHubFixture(t)
	fc := newFakeCICompute()
	e, _ := newTestEngine(t, fc, ghClient)

	// Created owned server: conflict, state untouched.
	spec1 := enforcerTestSpec("op-rej-created", 800*time.Second, 20000*time.Second)
	putAndCreate(t, e, fc, spec1, "srv-rej-1")
	if _, err := e.rejectUnstarted(spec1.Plan.EnforcerLease, spec1); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("created lease must conflict, got %v", err)
	}
	after := leaseState(t, e, spec1.Plan.EnforcerLease)
	if after.CreateResult != "created" || after.CreateServerID != "srv-rej-1" ||
		after.StopRequested || !after.CreateSettled {
		t.Fatalf("created lease must be unchanged: %+v", after)
	}

	// Registered lease: conflict, runner identity untouched.
	spec2 := enforcerTestSpec("op-rej-reg", 800*time.Second, 20000*time.Second)
	runnerID := registeredLeaseForTest(t, e, fc, ghClient, spec2, "srv-rej-2")
	if _, err := e.rejectUnstarted(spec2.Plan.EnforcerLease, spec2); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("registered lease must conflict, got %v", err)
	}
	l2 := leaseState(t, e, spec2.Plan.EnforcerLease)
	if !l2.RegistrationSettled || l2.GitHubRunnerID != runnerID || l2.StopRequested {
		t.Fatalf("registered lease must be unchanged: %+v", l2)
	}

	// In-flight registering claim on a pending lease: conflict, no mutation.
	spec3 := enforcerTestSpec("op-rej-regflag", 800*time.Second, 20000*time.Second)
	if _, err := e.putLease(spec3.Plan.EnforcerLease, spec3); err != nil {
		t.Fatalf("putLease: %v", err)
	}
	st3, err := e.stateFor(spec3.Plan.EnforcerLease)
	if err != nil || st3 == nil {
		t.Fatalf("stateFor: %v", err)
	}
	st3.stateMu.Lock()
	st3.registering = true
	st3.stateMu.Unlock()
	if _, err := e.rejectUnstarted(spec3.Plan.EnforcerLease, spec3); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("registering lease must conflict, got %v", err)
	}
	l3 := leaseState(t, e, spec3.Plan.EnforcerLease)
	if l3.CreateSettled || l3.StopRequested || l3.CreateResult != "" {
		t.Fatalf("registering conflict must not mutate: %+v", l3)
	}
	st3.stateMu.Lock()
	st3.registering = false
	st3.stateMu.Unlock()
	// With the claim cleared the same pending lease rejects cleanly.
	if _, err := e.rejectUnstarted(spec3.Plan.EnforcerLease, spec3); err != nil {
		t.Fatalf("reject after claim cleared: %v", err)
	}
	rejectedLeaseFields(t, leaseState(t, e, spec3.Plan.EnforcerLease), spec3)
}

// TestCIEnforcerRejectUnstartedRetainedResources proves retained
// security-group/port inventory is preserved through the transition while
// absence proofs reset for fresh independent observation, and that any
// retained server resource blocks the reject even with unsettled creation.
func TestCIEnforcerRejectUnstartedRetainedResources(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	op := "op-rej-res"
	spec := enforcerTestSpecAt(op, clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	if _, err := e.putLease(leaseID, spec); err != nil {
		t.Fatalf("putLease: %v", err)
	}
	// Retained SG+port via the same durable commit mechanism discovery uses.
	st, err := e.stateFor(leaseID)
	if err != nil || st == nil {
		t.Fatalf("stateFor: %v", err)
	}
	if err := e.commit(st, func(next *ciEnforcerLease) {
		next.Resources = []compute.CIRunnerResource{
			{Kind: "security_group", ID: "sg-retained-1", OperationID: op},
			{Kind: "port", ID: "port-retained-1", OperationID: op},
		}
	}); err != nil {
		t.Fatalf("commit retained resources: %v", err)
	}
	// A retained server blocks reject even while creation is unsettled.
	specSrv := enforcerTestSpecAt("op-rej-srvres", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err := e.putLease(specSrv.Plan.EnforcerLease, specSrv); err != nil {
		t.Fatalf("putLease srv: %v", err)
	}
	stSrv, err := e.stateFor(specSrv.Plan.EnforcerLease)
	if err != nil || stSrv == nil {
		t.Fatalf("stateFor srv: %v", err)
	}
	if err := e.commit(stSrv, func(next *ciEnforcerLease) {
		next.Resources = []compute.CIRunnerResource{
			{Kind: "server", ID: "srv-retained-1", OperationID: specSrv.Plan.OperationID},
		}
	}); err != nil {
		t.Fatalf("commit server resource: %v", err)
	}
	if _, err := e.rejectUnstarted(specSrv.Plan.EnforcerLease, specSrv); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("retained server must block reject, got %v", err)
	}
	lSrv := leaseState(t, e, specSrv.Plan.EnforcerLease)
	if lSrv.CreateSettled || lSrv.StopRequested {
		t.Fatalf("server-resource conflict must not mutate: %+v", lSrv)
	}

	// SG/port survive the reject; absence proofs reset for fresh observation.
	if _, err := e.rejectUnstarted(leaseID, spec); err != nil {
		t.Fatalf("rejectUnstarted retained: %v", err)
	}
	l := leaseState(t, e, leaseID)
	rejectedLeaseFields(t, l, spec)
	if len(l.Resources) != 2 {
		t.Fatalf("retained SG/port must be preserved: %+v", l.Resources)
	}
	seen := map[string]bool{}
	for _, r := range l.Resources {
		seen[r.Kind+"/"+r.ID] = true
		if r.OperationID != op {
			t.Fatalf("retained resource operation mismatch: %+v", r)
		}
	}
	if !seen["security_group/sg-retained-1"] || !seen["port/port-retained-1"] {
		t.Fatalf("retained SG/port must be preserved: %+v", l.Resources)
	}
	stt, err := e.status(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stt.Resources) != 2 {
		t.Fatalf("status must expose retained resources: %+v", stt.Resources)
	}
}

// TestCIEnforcerRejectUnstartedSpecScopeDenials proves spec drift, identity
// mismatches, invalid scopes, and index collisions all conflict before any
// persistence, and that zero/negative setup deadlines can never allocate a
// record the strict loader would reject.
func TestCIEnforcerRejectUnstartedSpecScopeDenials(t *testing.T) {
	fc := newFakeCICompute()
	clk := newFakeClock()
	e, _ := newTestEngineAt(t, fc, nil, clk)
	spec := enforcerTestSpecAt("op-rej-deny", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	if _, err := e.rejectUnstarted(leaseID, spec); err != nil {
		t.Fatalf("seed rejectUnstarted: %v", err)
	}
	committed := leaseState(t, e, leaseID)

	// Spec drift under the existing lease: every mutation conflicts and the
	// committed record is untouched.
	drifts := map[string]func(*ciEnforcerSpec){
		"repository":   func(s *ciEnforcerSpec) { s.GitHubRepositoryID = 778 },
		"installation": func(s *ciEnforcerSpec) { s.GitHubInstallationID = 43 },
		"size":         func(s *ciEnforcerSpec) { s.SizeID = "linux-x64-4" },
		"image":        func(s *ciEnforcerSpec) { s.Plan.ImageID = "i2" },
		"network":      func(s *ciEnforcerSpec) { s.Plan.NetworkID = "n2" },
		"setup_time":   func(s *ciEnforcerSpec) { s.Plan.SetupDeadlineAt = s.Plan.SetupDeadlineAt.Add(time.Second) },
		"flavor":       func(s *ciEnforcerSpec) { s.Plan.FlavorID = "f2" },
	}
	for name, mutate := range drifts {
		t.Run("drift_"+name, func(t *testing.T) {
			bad := spec
			mutate(&bad)
			if _, err := e.rejectUnstarted(leaseID, bad); !errors.Is(err, errCIEnforcerConflict) {
				t.Fatalf("drifted spec must conflict, got %v", err)
			}
			l := leaseState(t, e, leaseID)
			if l.PlanDigest != committed.PlanDigest || l.SpecDigest != committed.SpecDigest ||
				!l.StopRequested || l.CreateResult != "rejected" {
				t.Fatalf("drift must not mutate committed lease: %+v", l)
			}
		})
	}

	// Lease argument different from plan.EnforcerLease denies before persist.
	otherLease, _ := ciEnforcerNewLeaseID()
	if _, err := e.rejectUnstarted(otherLease, spec); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("lease/plan mismatch must conflict, got %v", err)
	}

	// Invalid specs on fresh leases deny before any persistence.
	invalid := map[string]func(*ciEnforcerSpec){
		"zero_repo":      func(s *ciEnforcerSpec) { s.GitHubRepositoryID = 0 },
		"negative_repo":  func(s *ciEnforcerSpec) { s.GitHubRepositoryID = -5 },
		"bad_size":       func(s *ciEnforcerSpec) { s.SizeID = "not-a-size" },
		"bad_servername": func(s *ciEnforcerSpec) { s.Plan.ServerName = "not-canonical" },
		"bad_lease": func(s *ciEnforcerSpec) {
			s.Plan.EnforcerLease = "not-64-hex"
		},
		"zero_setup": func(s *ciEnforcerSpec) { s.Plan.SetupDeadlineAt = time.Time{} },
		"negative_setup": func(s *ciEnforcerSpec) {
			s.Plan.SetupDeadlineAt = time.UnixMilli(-1000)
		},
		"provider_region": func(s *ciEnforcerSpec) { s.Plan.Region = "r2" },
	}
	for name, mutate := range invalid {
		t.Run("invalid_"+name, func(t *testing.T) {
			bad := enforcerTestSpecAt("op-rej-inv-"+name, clk.wall, 800*time.Second, 20000*time.Second)
			mutate(&bad)
			if _, err := e.rejectUnstarted(bad.Plan.EnforcerLease, bad); !errors.Is(err, errCIEnforcerConflict) {
				t.Fatalf("invalid spec must conflict, got %v", err)
			}
			st, serr := e.stateFor(bad.Plan.EnforcerLease)
			if serr != nil {
				t.Fatal(serr)
			}
			if st != nil {
				t.Fatalf("invalid spec must not persist a lease: %+v", st.snapshot())
			}
		})
	}

	// Operation index collision: same operation, different lease conflicts.
	collide := enforcerTestSpecAt("op-rej-deny", clk.wall, 800*time.Second, 20000*time.Second)
	if _, err := e.rejectUnstarted(collide.Plan.EnforcerLease, collide); !errors.Is(err, errCIEnforcerConflict) {
		t.Fatalf("opIndex collision must conflict, got %v", err)
	}
	stc, err := e.stateFor(collide.Plan.EnforcerLease)
	if err != nil {
		t.Fatal(err)
	}
	if stc != nil {
		t.Fatalf("colliding lease must not persist: %+v", stc.snapshot())
	}
}

// TestCIEnforcerRejectUnstartedHTTPClientContract covers the operator RPC:
// auth before body, strict body rejection identical to PUT, a valid expired
// spec returning 200/no-store, the typed client roundtrip through rejected
// and tombstone states, and client-side scope mismatches.
func TestCIEnforcerRejectUnstartedHTTPClientContract(t *testing.T) {
	token := []byte(strings.Repeat("t", 40))
	fc := newFakeCICompute()
	clk := newFakeClock()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	wall, mono := clk.pair()
	e, err := newCIEnforcerEngine(dir, fc, nil, wall, mono)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	srv := &CIEnforcerServer{engine: e, token: token}
	srv.handler = srv.routes()
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()
	client, err := newCIEnforcerClient(httpSrv.URL, token, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	spec := enforcerTestSpecAt("op-rej-http", clk.wall, 800*time.Second, 20000*time.Second)
	leaseID := spec.Plan.EnforcerLease
	clk.advance(1000 * time.Second) // past setup before any call

	raw, err := json.Marshal(ciEnforcerLeaseSpecDTO(spec))
	if err != nil {
		t.Fatal(err)
	}
	putRaw := func(body string, auth bool) int {
		req, err := http.NewRequest(http.MethodPut, httpSrv.URL+"/v1/leases/"+leaseID+"/reject-unstarted", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if auth {
			req.Header.Set("Authorization", "Bearer "+string(token))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Auth runs before the body is read: garbage body without a token is 401.
	if c := putRaw("{not-json", false); c != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reject must be 401 before body read, got %d", c)
	}
	// Strict body rules identical to PUT.
	dup := strings.Replace(string(raw), `"githubRepositoryId":777`, `"githubRepositoryId":777,"githubRepositoryId":777`, 1)
	for name, body := range map[string]string{
		"duplicate": dup,
		"null":      strings.Replace(string(raw), `"sizeId":"linux-x64-2"`, `"sizeId":null`, 1),
		"unknown":   strings.Replace(string(raw), `"sizeId"`, `"bogus":1,"sizeId"`, 1),
		"truncated": string(raw)[:len(raw)-2],
		"trailing":  string(raw) + ` {"x":1}`,
		"empty":     "",
	} {
		if c := putRaw(body, true); c != http.StatusBadRequest {
			t.Fatalf("%s body must be 400, got %d", name, c)
		}
	}
	// Oversize body hits the shared 16KiB bound (413, same as PUT).
	if c := putRaw(strings.Repeat(" ", (16<<10)+1), true); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body must be 413, got %d", c)
	}
	// A missing required field decodes but fails engine validation (409),
	// identical to PUT semantics.
	missing := strings.Replace(string(raw), `,"sizeId":"linux-x64-2"`, ``, 1)
	if c := putRaw(missing, true); c != http.StatusConflict {
		t.Fatalf("missing-field body must conflict like PUT, got %d", c)
	}

	// Valid full spec past setup: 200 with no-store and the canonical DTO.
	st, err := client.RejectUnstarted(context.Background(), spec)
	if err != nil {
		t.Fatalf("client RejectUnstarted expired: %v", err)
	}
	if !st.StopRequested || !st.CreateSettled || st.Settled ||
		st.DeadlineUnixMs != spec.Plan.SetupDeadlineAt.UnixMilli() ||
		st.GitHubRepositoryID != spec.GitHubRepositoryID {
		t.Fatalf("bad client reject status: %+v", st)
	}

	// Client roundtrip through cleanup to tombstone.
	driveCleanup(t, e, leaseID, 3)
	l := leaseState(t, e, leaseID)
	if !l.Settled {
		t.Fatalf("rejected lease must tombstone after proofs: %+v", l)
	}
	stT, err := client.RejectUnstarted(context.Background(), spec)
	if err != nil {
		t.Fatalf("client tombstone RejectUnstarted: %v", err)
	}
	if !stT.Settled || stT.DeadlineUnixMs != 0 || len(stT.Resources) != 0 {
		t.Fatalf("tombstone client status: %+v", stT)
	}

	// Client-side scope mismatches against a fixture server.
	fixture := func(mutate func(*ciEnforcerStatus)) *ciEnforcerClient {
		return rejectUnstartedFixtureClient(t, spec.Plan, spec, mutate)
	}
	if _, err := fixture(nil).RejectUnstarted(context.Background(), spec); err != nil {
		t.Fatalf("well-formed reject ack must pass: %v", err)
	}
	for name, mutate := range map[string]func(*ciEnforcerStatus){
		"wrong_repo": func(s *ciEnforcerStatus) { s.GitHubRepositoryID = 778 },
		"wrong_spec_digest": func(s *ciEnforcerStatus) {
			s.SpecDigest = strings.Repeat("0", 64)
		},
		"not_stopped":        func(s *ciEnforcerStatus) { s.StopRequested = false },
		"not_create_settled": func(s *ciEnforcerStatus) { s.CreateSettled = false },
		"reg_attempted":      func(s *ciEnforcerStatus) { s.RegistrationAttempted = true },
		"server_resource": func(s *ciEnforcerStatus) {
			s.Resources = []ciOwnedResource{{Kind: "server", ID: "srv-fix-1", OperationID: spec.Plan.OperationID}}
		},
	} {
		t.Run("client_"+name, func(t *testing.T) {
			if _, err := fixture(mutate).RejectUnstarted(context.Background(), spec); !errors.Is(err, errCIEnforcerScope) {
				t.Fatalf("%s ack must be scope conflict, got %v", name, err)
			}
		})
	}
}

// rejectUnstartedFixtureClient serves a canned reject-unstarted ack so the
// typed client can be exercised against malformed bindings without a real
// engine.
func rejectUnstartedFixtureClient(t *testing.T, plan ciRunnerLifecyclePlan, spec ciEnforcerSpec, mutate func(*ciEnforcerStatus)) *ciEnforcerClient {
	t.Helper()
	_, specDigest := spec.digests()
	base := ciEnforcerStatus{
		LeaseID: plan.EnforcerLease, OperationID: plan.OperationID,
		PlanDigest: ciEnforcerPlanDigest(plan), SpecDigest: specDigest,
		DeadlineUnixMs:     plan.SetupDeadlineAt.UnixMilli(),
		CreateSettled:      true,
		StopRequested:      true,
		GitHubRepositoryID: spec.GitHubRepositoryID,
		RunnerName:         ciEnforcerRunnerName(plan.OperationID),
		Resources:          []ciOwnedResource{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/leases/"+plan.EnforcerLease+"/reject-unstarted", func(w http.ResponseWriter, r *http.Request) {
		st := base
		if mutate != nil {
			mutate(&st)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(st)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := newCIEnforcerClient(srv.URL, []byte(strings.Repeat("t", 40)), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
