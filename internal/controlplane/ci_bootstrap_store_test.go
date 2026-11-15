package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ciBootstrapAck builds the fake authenticated enforcer acknowledgement a
// verified RPC would return, bound to the trusted scope's own digests. It is
// a Store unit caller only; no real enforcer integration exists yet.
func ciBootstrapAck(scope ciBootstrapScope, plan ciRunnerLifecyclePlan, resources []ciOwnedResource) ciEnforcerStatus {
	pd, sd := scope.Spec.digests()
	return ciEnforcerStatus{
		LeaseID:            plan.EnforcerLease,
		OperationID:        plan.OperationID,
		PlanDigest:         pd,
		SpecDigest:         sd,
		DeadlineUnixMs:     plan.SetupDeadlineAt.UnixMilli(),
		CreateSettled:      true,
		GitHubRepositoryID: scope.Spec.GitHubRepositoryID,
		RunnerName:         plan.ServerName,
		Resources:          resources,
	}
}

func ciBootstrapEnsure(t *testing.T, s *Store, workspace, runner string, policy ciProvisioningBudgetPolicy, plan ciRunnerLifecyclePlan, nonce string) ciBootstrapScope {
	t.Helper()
	scope, err := s.ensureCIBootstrapGrant(context.Background(), workspace, runner, policy.ProfileDigest, plan, nonce)
	if err != nil {
		t.Fatalf("ensure bootstrap grant: %v", err)
	}
	return scope
}

func ciBootstrapScopeGet(t *testing.T, s *Store, workspace, runner string, policy ciProvisioningBudgetPolicy, plan ciRunnerLifecyclePlan, ready bool) ciBootstrapScope {
	t.Helper()
	scope, err := s.ciBootstrapScope(context.Background(), workspace, runner, policy.ProfileDigest, plan, ready)
	if err != nil {
		t.Fatalf("bootstrap scope: %v", err)
	}
	return scope
}

// ciBootstrapEnsureRaw returns the raw ensure error for denial assertions.
func ciBootstrapEnsureRaw(s *Store, ctx context.Context, workspace, runner, profile string, plan ciRunnerLifecyclePlan, nonce string) error {
	_, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, profile, plan, nonce)
	return err
}

// ciBootstrapRegistering drives one funded runner through the global create
// fence, the full SDK journal outcome, the bootstrap grant, and a valid
// enforcer acknowledgement, returning the registering-phase scope.
func ciBootstrapRegistering(t *testing.T, s *Store, workspace, op string, number int, now time.Time, nonce string) (string, ciRunnerLifecyclePlan, ciProvisioningBudgetPolicy, ciBootstrapScope, []ciOwnedResource) {
	t.Helper()
	runner, plan, policy := ciCreationSetup(t, s, workspace, op, number, now, 1000)
	resources := ciCreationComplete(t, s, workspace, runner, policy, plan)
	scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, nonce)
	status := ciBootstrapAck(scope, plan, resources)
	if err := s.markCIProvisioningRegistering(context.Background(), workspace, runner, policy.ProfileDigest, plan, status); err != nil {
		t.Fatalf("mark registering: %v", err)
	}
	return runner, plan, policy, scope, resources
}

func TestCIBootstrapStoreEnsureGrant(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")

	// Funded and reserved but never globally fenced: the grant is allowed
	// before or after the create fence.
	runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_ensure", 1, now, 1000)
	scope, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce)
	if err != nil {
		t.Fatalf("ensure before fence: %v", err)
	}
	g := scope.Grant
	if g.BootstrapID != nonce || g.MaximumSeconds != 3600 ||
		!g.ExpiresAt.Equal(time.Unix(plan.SetupDeadlineAt.Unix(), 0)) ||
		g.AttemptedAt != nil || g.DeliveredAt != nil {
		t.Fatalf("grant: %+v", g)
	}
	spec := scope.Spec
	if spec.GitHubInstallationID <= 0 || spec.GitHubRepositoryID <= 0 ||
		spec.SizeID != "linux-x64-2" ||
		spec.Plan.OperationID != plan.OperationID ||
		spec.Plan.EnforcerLease != plan.EnforcerLease ||
		!spec.Plan.SetupDeadlineAt.Equal(plan.SetupDeadlineAt) ||
		!scope.CreatedAt.Equal(now.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("scope: %+v created=%v", spec, scope.CreatedAt)
	}
	// Funding is untouched: one active reservation of 100, no customer rows.
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("funding moved: %+v", state)
	}
	ciBudgetTotalsZero(t, s, workspace)

	// A different valid nonce and a later clock cannot rotate or extend the
	// stored grant; the original nonce and fixed expiry are returned.
	s.now = func() time.Time { return now.Add(10 * time.Second) }
	again, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, ciAdmissionNonce("b"))
	if err != nil {
		t.Fatalf("ensure retry: %v", err)
	}
	if again.Grant.BootstrapID != nonce || !again.Grant.ExpiresAt.Equal(g.ExpiresAt) ||
		again.Grant.MaximumSeconds != g.MaximumSeconds {
		t.Fatalf("grant rotated: %+v", again.Grant)
	}
	// Malformed proposed nonces reject before any write.
	for _, bad := range []string{"", "xyz", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, bad); !errors.Is(err, ErrConflict) {
			t.Fatalf("nonce %q: %v", bad, err)
		}
	}
	// The live getter reads the same grant.
	got := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, true)
	if got.Grant.BootstrapID != nonce || got.Grant.MaximumSeconds != 3600 {
		t.Fatalf("getter grant: %+v", got.Grant)
	}
}

func TestCIBootstrapStoreEnsureGuards(t *testing.T) {
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")

	t.Run("no funding or reservation", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		policy := ciBudgetPolicy(now)
		runner, plan := ciBudgetSeed(t, s, workspace, "bootstrap_op_nofund", 1, now)
		// The budget singleton is absent entirely.
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ensure without budget: %v", err)
		}
		// Funded globally but this runner holds no reservation.
		ciBudgetGrant(t, s, "credit_1", 1000, policy)
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ensure without reservation: %v", err)
		}
	})

	t.Run("scope profile plan workspace runner drift", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_drift", 1, now, 1000)
		digest := policy.ProfileDigest
		denied := func(err error) {
			t.Helper()
			if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
				t.Fatalf("expected denial, got %v", err)
			}
		}
		denied(ciBootstrapEnsureRaw(s, ctx, workspace, runner, ciBudgetDigestB(), plan, nonce))
		changedPlan := plan
		changedPlan.FlavorID = "f2"
		denied(ciBootstrapEnsureRaw(s, ctx, workspace, runner, digest, changedPlan, nonce))
		otherPlan := plan
		otherPlan.OperationID = "bootstrap_op_other"
		otherPlan.ServerName = ciEnforcerRunnerName(otherPlan.OperationID)
		denied(ciBootstrapEnsureRaw(s, ctx, workspace, runner, digest, otherPlan, nonce))
		denied(ciBootstrapEnsureRaw(s, ctx, "missing-ws", runner, digest, plan, nonce))
		denied(ciBootstrapEnsureRaw(s, ctx, workspace, "ci_financial_runner_999", digest, plan, nonce))
		// Nothing was written for any denial.
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, digest, plan, nonce); err != nil {
			t.Fatalf("valid ensure after denials: %v", err)
		}
	})

	t.Run("product disabled blocks ensure", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_off", 1, now, 1000)
		ciAdmissionProductOff(t, s, workspace)
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("ensure with product off: %v", err)
		}
	})

	t.Run("stop blocks ensure", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_stop", 1, now, 1000)
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("ensure after stop: %v", err)
		}
	})

	t.Run("setup expiry blocks ensure", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_exp", 1, now, 1000)
		s.now = func() time.Time { return now.Add(700 * time.Second) }
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("ensure after setup expiry: %v", err)
		}
	})

	t.Run("cleanup getter ignores live guards", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_clean", 1, now, 1000)
		scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, nonce)
		// Stop, disable the product, and expire setup: the cleanup-identity
		// getter still returns the original grant without resurrecting it.
		if _, err := s.requestCIRunnerStop(ctx, workspace, runner, "cancelled", nil); err != nil {
			t.Fatal(err)
		}
		ciAdmissionProductOff(t, s, workspace)
		s.now = func() time.Time { return now.Add(700 * time.Second) }
		got := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, false)
		if got.Grant.BootstrapID != nonce || got.Grant.MaximumSeconds != scope.Grant.MaximumSeconds ||
			!got.Grant.ExpiresAt.Equal(scope.Grant.ExpiresAt) ||
			got.Grant.AttemptedAt != nil || got.Grant.DeliveredAt != nil {
			t.Fatalf("cleanup grant: %+v", got.Grant)
		}
		state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
		if state.Active != 1 || state.CommittedMicros != 100 {
			t.Fatalf("funding moved: %+v", state)
		}
	})
}

func TestCIBootstrapStoreEnsureConcurrent(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_race", 1, now, 1000)
	digest := policy.ProfileDigest

	const n = 16
	nonces := make([]string, n)
	scopes := make([]ciBootstrapScope, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		nonces[i] = fmt.Sprintf("%064x", i+1)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			scopes[i], errs[i] = s.ensureCIBootstrapGrant(ctx, workspace, runner, digest, plan, nonces[i])
		}(i)
	}
	wg.Wait()
	successes := 0
	var stored string
	for i, err := range errs {
		switch {
		case err == nil:
			successes++
			if stored == "" {
				stored = scopes[i].Grant.BootstrapID
			}
			if scopes[i].Grant.BootstrapID != stored || scopes[i].Grant.MaximumSeconds != 3600 {
				t.Fatalf("concurrent ensure %d returned divergent grant: %+v", i, scopes[i].Grant)
			}
		case errors.Is(err, errCIRunnerJobPending):
		default:
			t.Fatalf("concurrent ensure %d unexpected error: %v", i, err)
		}
	}
	if successes < 1 {
		t.Fatal("no concurrent ensure succeeded")
	}
	state := ciBudgetPeek(t, s, "credit_1", 1000, policy)
	if state.Active != 1 || state.CommittedMicros != 100 {
		t.Fatalf("funding moved: %+v", state)
	}
	ciBudgetTotalsZero(t, s, workspace)
}

func TestCIBootstrapStoreMarkRegistering(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")

	t.Run("denied before fence journal and outcome", func(t *testing.T) {
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_reg1", 1, now, 1000)
		scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, nonce)
		_, port, server := ciCreationResources(plan.OperationID)
		ack := ciBootstrapAck(scope, plan, []ciOwnedResource{port, server})

		// Before the global fence there is no create attempt at all.
		ciCreationExpectConflict(t, s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, ack))
		snap := ciCreationLifecycle(t, s, workspace, runner)
		if snap.Phase != "queued" || snap.CreateSettled {
			t.Fatalf("lifecycle after pre-fence ack: %+v", snap)
		}

		fenced, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
		if err != nil || !fenced {
			t.Fatalf("global fence: fenced=%v err=%v", fenced, err)
		}
		ciCreationExpectConflict(t, s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, ack))
		snap = ciCreationLifecycle(t, s, workspace, runner)
		if snap.Phase != "provisioning" || snap.CreateSettled {
			t.Fatalf("lifecycle after no-journal ack: %+v", snap)
		}

		sg, _, _ := ciCreationResources(plan.OperationID)
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "security_group", nil)
		ciCreationExpectConflict(t, s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, ack))
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "port", []ciOwnedResource{sg})
		ciCreationExpectConflict(t, s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, ack))
		ciCreationFenceStage(t, s, workspace, runner, policy, plan, "server", []ciOwnedResource{sg, port})
		ciCreationExpectConflict(t, s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, ack))
		snap = ciCreationLifecycle(t, s, workspace, runner)
		if snap.Phase != "provisioning" || snap.CreateSettled {
			t.Fatalf("lifecycle after stage-only ack: %+v", snap)
		}
	})

	t.Run("ack binding drifts denied then valid ack", func(t *testing.T) {
		runner, plan, policy := ciCreationSetup(t, s, workspace, "bootstrap_op_reg2", 2, now, 1000)
		resources := ciCreationComplete(t, s, workspace, runner, policy, plan)
		scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, ciAdmissionNonce("b"))
		valid := ciBootstrapAck(scope, plan, resources)
		sg, port, server := resources[0], resources[1], resources[2]
		op := plan.OperationID

		drifts := []struct {
			name   string
			mutate func(*ciEnforcerStatus)
		}{
			{"spec digest", func(a *ciEnforcerStatus) { a.SpecDigest = strings.Repeat("d", 64) }},
			{"plan digest", func(a *ciEnforcerStatus) { a.PlanDigest = strings.Repeat("d", 64) }},
			{"lease", func(a *ciEnforcerStatus) { a.LeaseID = strings.Repeat("e", 64) }},
			{"operation", func(a *ciEnforcerStatus) { a.OperationID = "bootstrap_op_other" }},
			{"repository", func(a *ciEnforcerStatus) { a.GitHubRepositoryID = 790 }},
			{"runner name", func(a *ciEnforcerStatus) { a.RunnerName = "other-runner" }},
			{"deadline", func(a *ciEnforcerStatus) { a.DeadlineUnixMs++ }},
			{"stop requested", func(a *ciEnforcerStatus) { a.StopRequested = true }},
			{"settled", func(a *ciEnforcerStatus) { a.Settled = true }},
			{"create unsettled", func(a *ciEnforcerStatus) { a.CreateSettled = false }},
			{"github runner id", func(a *ciEnforcerStatus) { a.GitHubRunnerID = 7 }},
			{"registration attempted", func(a *ciEnforcerStatus) { a.RegistrationAttempted = true }},
			{"registration settled", func(a *ciEnforcerStatus) { a.RegistrationSettled = true }},
			{"nil inventory", func(a *ciEnforcerStatus) { a.Resources = nil }},
			{"missing server", func(a *ciEnforcerStatus) { a.Resources = []ciOwnedResource{sg, port} }},
			{"wrong server id", func(a *ciEnforcerStatus) {
				a.Resources = []ciOwnedResource{sg, port, {Kind: "server", ID: "server_other", OperationID: op}}
			}},
			{"unexpected resource id", func(a *ciEnforcerStatus) {
				a.Resources = []ciOwnedResource{{Kind: "security_group", ID: "sg_other", OperationID: op}, port, server}
			}},
		}
		for _, d := range drifts {
			bad := valid
			d.mutate(&bad)
			ciCreationExpectConflict(t, s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, bad))
		}
		snap := ciCreationLifecycle(t, s, workspace, runner)
		if snap.Phase != "provisioning" || snap.CreateSettled {
			t.Fatalf("lifecycle after ack drifts: %+v", snap)
		}

		// The real wire shape: the enforcer's CloseCreation ack initially
		// knows only the server while the durable CP journal retains SG+port.
		// The merged lifecycle inventory must still hold all three IDs.
		serverOnly := valid
		serverOnly.Resources = []ciOwnedResource{server}
		if err := s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, serverOnly); err != nil {
			t.Fatalf("valid ack: %v", err)
		}
		snap = ciCreationLifecycle(t, s, workspace, runner)
		if snap.Phase != "registering" || !snap.CreateSettled || len(snap.Resources) != 3 {
			t.Fatalf("lifecycle after valid ack: %+v", snap)
		}
		// An exact repeat is idempotent and never rewinds the phase.
		if err := s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, serverOnly); err != nil {
			t.Fatalf("ack repeat: %v", err)
		}
		snap = ciCreationLifecycle(t, s, workspace, runner)
		if snap.Phase != "registering" || !snap.CreateSettled {
			t.Fatalf("lifecycle after ack repeat: %+v", snap)
		}
		// No further creation stage may be posted once the journal finished.
		ciCreationExpectConflict(t, s.fenceCIProvisioningStage(ctx, workspace, runner, policy.ProfileDigest, plan, "security_group", nil))
	})
}

func TestCIBootstrapStoreClaim(t *testing.T) {
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")

	t.Run("pending before ack then claim succeeds", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_claim1", 1, now, 1000)
		scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, nonce)
		claims := scope.claims(workspace, runner)

		// Queued and fenced-but-unacknowledged phases are pending, never
		// consumed: AttemptedAt stays nil.
		if _, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims); !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("claim before fence: %v", err)
		}
		fenced, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
		if err != nil || !fenced {
			t.Fatalf("global fence: fenced=%v err=%v", fenced, err)
		}
		if _, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims); !errors.Is(err, errCIRunnerJobPending) {
			t.Fatalf("claim while provisioning: %v", err)
		}
		if got := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, true).Grant; got.AttemptedAt != nil {
			t.Fatalf("pending claim consumed grant: %+v", got)
		}

		resources := ciCreationComplete(t, s, workspace, runner, policy, plan)
		status := ciBootstrapAck(scope, plan, resources)
		if err := s.markCIProvisioningRegistering(ctx, workspace, runner, policy.ProfileDigest, plan, status); err != nil {
			t.Fatalf("mark registering: %v", err)
		}

		// Every binding drift denies while every other readiness guard is
		// already valid, so only the claim binding itself can reject. Each
		// failed claim must leave the live grant completely unconsumed.
		drifts := []struct {
			name   string
			mutate func(*ciBootstrapClaims)
			scope  bool // workspace/runner identity failures surface as NotFound too
		}{
			{"version", func(c *ciBootstrapClaims) { c.Version = 2 }, false},
			{"workspace", func(c *ciBootstrapClaims) { c.Workspace = "missing-ws" }, true},
			{"runner", func(c *ciBootstrapClaims) { c.Runner = "ci_financial_runner_999" }, true},
			{"operation", func(c *ciBootstrapClaims) { c.OperationID = "bootstrap_op_other" }, false},
			{"bootstrap id", func(c *ciBootstrapClaims) { c.BootstrapID = strings.Repeat("f", 64) }, false},
			{"repository", func(c *ciBootstrapClaims) { c.GitHubRepositoryID = 790 }, false},
			{"plan digest", func(c *ciBootstrapClaims) { c.PlanDigest = strings.Repeat("d", 64) }, false},
			{"maximum seconds", func(c *ciBootstrapClaims) { c.MaximumSeconds = 3601 }, false},
			{"expiry", func(c *ciBootstrapClaims) { c.ExpiresAt++ }, false},
		}
		for _, d := range drifts {
			bad := claims
			d.mutate(&bad)
			_, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, bad)
			if d.scope {
				if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
					t.Fatalf("claim drift %s: %v", d.name, err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatalf("claim drift %s: %v", d.name, err)
			}
			grant := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, true).Grant
			if grant.AttemptedAt != nil || grant.DeliveredAt != nil {
				t.Fatalf("claim drift %s consumed grant: %+v", d.name, grant)
			}
		}

		claimed, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.Grant.AttemptedAt == nil {
			t.Fatal("claim did not stamp attempted_at")
		}
		snap := ciCreationLifecycle(t, s, workspace, runner)
		if snap.ExecutionStartedAt != nil {
			t.Fatalf("claim started execution: %+v", snap)
		}
		ciBudgetTotalsZero(t, s, workspace)

		// The used nonce is never reset and the exact replay conflicts.
		if _, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims); !errors.Is(err, ErrConflict) {
			t.Fatalf("claim replay: %v", err)
		}
		// A fresh proposed nonce still returns the original used grant.
		again := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, ciAdmissionNonce("b"))
		if again.Grant.BootstrapID != nonce || again.Grant.AttemptedAt == nil {
			t.Fatalf("grant after claim: %+v", again.Grant)
		}
	})

	t.Run("sixteen concurrent claims exactly one wins", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy, scope, _ := ciBootstrapRegistering(t, s, workspace, "bootstrap_op_claim2", 1, now, nonce)
		claims := scope.claims(workspace, runner)
		const n = 16
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims)
			}(i)
		}
		wg.Wait()
		successes := 0
		for i, err := range errs {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrConflict) || errors.Is(err, errCIRunnerJobPending):
			default:
				t.Fatalf("claim %d unexpected error: %v", i, err)
			}
		}
		if successes != 1 {
			t.Fatalf("concurrent claims: %d succeeded", successes)
		}
		grant := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, true).Grant
		if grant.AttemptedAt == nil {
			t.Fatal("winning claim did not stamp attempted_at")
		}
		ciBudgetTotalsZero(t, s, workspace)
	})
}

func TestCIBootstrapStoreDelivery(t *testing.T) {
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")

	// setup builds a fresh fixture per subtest: a registering runner whose
	// grant has been claimed and whose admission is registered (idle phase).
	setup := func(t *testing.T, op string) (*Store, string, string, ciRunnerLifecyclePlan, ciProvisioningBudgetPolicy, ciBootstrapScope, ciBootstrapClaims, time.Time) {
		t.Helper()
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy, scope, _ := ciBootstrapRegistering(t, s, workspace, op, 1, now, nonce)
		claims := scope.claims(workspace, runner)
		if _, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.registerCIRunnerAdmission(ctx, workspace, runner, claims.BootstrapID, ciRunnerJobScope{
			RepositoryID: scope.Spec.GitHubRepositoryID,
			RunnerID:     42,
			RunnerName:   plan.ServerName,
			SizeID:       scope.Spec.SizeID,
			CreatedAt:    scope.CreatedAt,
		}); err != nil {
			t.Fatalf("admission registration: %v", err)
		}
		return s, workspace, runner, plan, policy, scope, claims, now
	}

	t.Run("delivery requires claim and admission", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy, scope, _ := ciBootstrapRegistering(t, s, workspace, "bootstrap_op_del0", 1, now, nonce)
		claims := scope.claims(workspace, runner)
		// Before any claim the delivery marker conflicts.
		ciCreationExpectConflict(t, s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 42))
		if _, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims); err != nil {
			t.Fatalf("claim: %v", err)
		}
		// Claimed but not yet registered: the admission row is absent, so the
		// delivery marker still denies and nothing is stamped.
		if err := s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 42); err == nil {
			t.Fatal("delivery before admission registration succeeded")
		}
		if got := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, true).Grant; got.DeliveredAt != nil {
			t.Fatalf("premature delivery stamped: %+v", got)
		}
	})

	t.Run("delivery requires claim even after admission registered", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy, scope, _ := ciBootstrapRegistering(t, s, workspace, "bootstrap_op_del2", 1, now, nonce)
		claims := scope.claims(workspace, runner)
		// Wrong parent RPC order: the admission is registered while the
		// grant was never claimed. The runner still reaches idle phase.
		if err := s.registerCIRunnerAdmission(ctx, workspace, runner, claims.BootstrapID, ciRunnerJobScope{
			RepositoryID: scope.Spec.GitHubRepositoryID,
			RunnerID:     42,
			RunnerName:   plan.ServerName,
			SizeID:       scope.Spec.SizeID,
			CreatedAt:    scope.CreatedAt,
		}); err != nil {
			t.Fatalf("admission registration: %v", err)
		}
		if snap := ciCreationLifecycle(t, s, workspace, runner); snap.Phase != "idle" {
			t.Fatalf("phase after early admission: %+v", snap)
		}
		if got := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, true).Grant; got.AttemptedAt != nil {
			t.Fatalf("unclaimed grant attempted: %+v", got)
		}
		// Every other delivery condition is ready; only the one-shot claim
		// guard remains, and it must conflict without stamping anything.
		ciCreationExpectConflict(t, s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 42))
		grant := ciBootstrapScopeGet(t, s, workspace, runner, policy, plan, false).Grant
		if grant.AttemptedAt != nil || grant.DeliveredAt != nil {
			t.Fatalf("grant after denied delivery: %+v", grant)
		}
	})

	t.Run("full delivery flow preserves first timestamp", func(t *testing.T) {
		s, w, runner, plan, policy, _, claims, now := setup(t, "bootstrap_op_del1")
		// A mismatched GitHub runner identity denies.
		ciCreationExpectConflict(t, s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 43))
		if err := s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 42); err != nil {
			t.Fatalf("delivery: %v", err)
		}
		first := ciBootstrapScopeGet(t, s, w, runner, policy, plan, true).Grant
		if first.DeliveredAt == nil || first.AttemptedAt == nil {
			t.Fatalf("grant after delivery: %+v", first)
		}
		// No billing or execution clock started.
		snap := ciCreationLifecycle(t, s, w, runner)
		if snap.ExecutionStartedAt != nil {
			t.Fatalf("delivery started execution: %+v", snap)
		}
		ciBudgetTotalsZero(t, s, w)
		// A repeat after the clock advances keeps the exact first timestamp.
		s.now = func() time.Time { return now.Add(time.Second) }
		if err := s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 42); err != nil {
			t.Fatalf("delivery repeat: %v", err)
		}
		second := ciBootstrapScopeGet(t, s, w, runner, policy, plan, true).Grant
		if second.DeliveredAt == nil || !second.DeliveredAt.Equal(*first.DeliveredAt) {
			t.Fatalf("delivered_at moved: first=%v second=%v", first.DeliveredAt, second.DeliveredAt)
		}
	})

	for _, tc := range []struct {
		name string
		op   string
		brk  func(t *testing.T, s *Store, workspace, runner string, now time.Time)
	}{
		{"cancelled", "bootstrap_op_delc", func(t *testing.T, s *Store, workspace, runner string, now time.Time) {
			if _, err := s.requestCIRunnerStop(context.Background(), workspace, runner, "cancelled", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"product disabled", "bootstrap_op_delp", func(t *testing.T, s *Store, workspace, runner string, now time.Time) {
			ciAdmissionProductOff(t, s, workspace)
		}},
		{"setup expired", "bootstrap_op_dele", func(t *testing.T, s *Store, workspace, runner string, now time.Time) {
			s.now = func() time.Time { return now.Add(700 * time.Second) }
		}},
	} {
		t.Run("final delivery fails closed after "+tc.name, func(t *testing.T) {
			s, w, runner, plan, policy, _, claims, now := setup(t, tc.op)
			tc.brk(t, s, w, runner, now)
			if err := s.markCIBootstrapDelivered(ctx, policy.ProfileDigest, plan, claims, 42); err == nil {
				t.Fatal("delivery succeeded after guard failure")
			}
			// The cleanup-identity read still shows the consumed-but-
			// undelivered grant; no replay or second claim is possible.
			grant := ciBootstrapScopeGet(t, s, w, runner, policy, plan, false).Grant
			if grant.AttemptedAt == nil || grant.DeliveredAt != nil {
				t.Fatalf("grant after failed delivery: %+v", grant)
			}
			if _, err := s.claimCIBootstrap(ctx, policy.ProfileDigest, plan, claims); !errors.Is(err, ErrConflict) {
				t.Fatalf("second claim: %v", err)
			}
			// No replay or second grant: the live-guarded ensure now
			// conflicts, and the consumed grant stays untouched.
			if err := ciBootstrapEnsureRaw(s, ctx, w, runner, policy.ProfileDigest, plan, ciAdmissionNonce("b")); !errors.Is(err, ErrConflict) {
				t.Fatalf("ensure after guard failure: %v", err)
			}
		})
	}
}

func TestCIBootstrapStorePaidMaximum(t *testing.T) {
	ctx := context.Background()
	nonce := ciAdmissionNonce("a")

	t.Run("paid runner gets paid maximum", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_paid1", 1, now, 1000)
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET paid_overrun_enabled=true WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
			t.Fatal(err)
		}
		scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, nonce)
		if scope.Grant.MaximumSeconds != ciMaxRuntimeSeconds {
			t.Fatalf("paid maximum: %+v", scope.Grant)
		}
	})

	t.Run("paid flip never upgrades stored grant", func(t *testing.T) {
		s, workspace, _ := ciFinancialFixture(t)
		ciBudgetReset(t, s)
		ciAdmissionEnable(t, s, workspace)
		now := s.now()
		runner, plan, policy := ciCreationFunded(t, s, workspace, "bootstrap_op_paid2", 1, now, 1000)
		scope := ciBootstrapEnsure(t, s, workspace, runner, policy, plan, nonce)
		if scope.Grant.MaximumSeconds != 3600 {
			t.Fatalf("free maximum: %+v", scope.Grant)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE ci_runners SET paid_overrun_enabled=true WHERE workspace_id=$1 AND id=$2`, workspace, runner); err != nil {
			t.Fatal(err)
		}
		// The actor policy snapshot is immutable: the stored free grant can
		// never be silently upgraded, and both paths conflict.
		if _, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, policy.ProfileDigest, plan, nonce); !errors.Is(err, ErrConflict) {
			t.Fatalf("ensure after paid flip: %v", err)
		}
		if _, err := s.ciBootstrapScope(ctx, workspace, runner, policy.ProfileDigest, plan, true); !errors.Is(err, ErrConflict) {
			t.Fatalf("scope after paid flip: %v", err)
		}
	})
}

func TestCIBootstrapStoreMaxConnsOne(t *testing.T) {
	s, workspace, _ := ciFinancialFixture(t)
	ciBudgetReset(t, s)
	ciAdmissionEnable(t, s, workspace)
	now := s.now()
	policy := ciBudgetPolicy(now)
	// Fixture funding may complete before the single-connection swap.
	ciBudgetGrant(t, s, "credit_1", 1000, policy)
	runner, plan := ciBudgetSeed(t, s, workspace, "bootstrap_op_max1", 1, now)
	ciBudgetReserve(t, s, workspace, runner, policy.ProfileDigest, plan)

	old := s.pool
	cfg := *old.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.pool = pool
	defer func() {
		s.pool = old
		pool.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	digest := policy.ProfileDigest
	nonce := ciAdmissionNonce("a")

	fenced, err := s.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil || !fenced {
		t.Fatalf("fence under MaxConns=1: fenced=%v err=%v", fenced, err)
	}
	sg, port, server := ciCreationResources(plan.OperationID)
	for _, st := range []struct {
		stage string
		known []ciOwnedResource
	}{
		{"security_group", nil},
		{"port", []ciOwnedResource{sg}},
		{"server", []ciOwnedResource{sg, port}},
	} {
		if err = s.fenceCIProvisioningStage(ctx, workspace, runner, digest, plan, st.stage, st.known); err != nil {
			t.Fatalf("stage %s under MaxConns=1: %v", st.stage, err)
		}
	}
	if err = s.recordCIProvisioningCreationResult(ctx, workspace, runner, digest, plan, ciProvisioningCreateOutcome{
		MutationAttempted: true, CreationSettled: true,
		Resources: []ciOwnedResource{sg, port, server},
	}); err != nil {
		t.Fatalf("record under MaxConns=1: %v", err)
	}
	scope, err := s.ensureCIBootstrapGrant(ctx, workspace, runner, digest, plan, nonce)
	if err != nil {
		t.Fatalf("ensure under MaxConns=1: %v", err)
	}
	if err = s.markCIProvisioningRegistering(ctx, workspace, runner, digest, plan,
		ciBootstrapAck(scope, plan, []ciOwnedResource{sg, port, server})); err != nil {
		t.Fatalf("mark registering under MaxConns=1: %v", err)
	}
	claims := scope.claims(workspace, runner)
	if _, err = s.claimCIBootstrap(ctx, digest, plan, claims); err != nil {
		t.Fatalf("claim under MaxConns=1: %v", err)
	}
	if err = s.registerCIRunnerAdmission(ctx, workspace, runner, claims.BootstrapID, ciRunnerJobScope{
		RepositoryID: scope.Spec.GitHubRepositoryID,
		RunnerID:     42,
		RunnerName:   plan.ServerName,
		SizeID:       scope.Spec.SizeID,
		CreatedAt:    scope.CreatedAt,
	}); err != nil {
		t.Fatalf("admission under MaxConns=1: %v", err)
	}
	if err = s.markCIBootstrapDelivered(ctx, digest, plan, claims, 42); err != nil {
		t.Fatalf("delivery under MaxConns=1: %v", err)
	}
	got, err := s.ciBootstrapScope(ctx, workspace, runner, digest, plan, true)
	if err != nil || got.Grant.DeliveredAt == nil {
		t.Fatalf("scope under MaxConns=1: %+v err=%v", got.Grant, err)
	}
}
