package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/canter0/canter/internal/provider/compute"
	"github.com/jackc/pgx/v5"
)

// ciManagedProvisionCompute is the creation-capable seam; the production
// *compute.Client satisfies it and tests substitute fixtures.
type ciManagedProvisionCompute interface {
	ciLifecycleCompute
	CreateCIRunner(context.Context, compute.CIRunnerCreateRequest) (compute.CIRunnerCreateResult, error)
}

// ciManagedProvisioner is the private one-shot managed creator plus recovery
// path. It orchestrates only existing durable store, enforcer, bootstrap, and
// compute APIs: no scheduler, no new SQL, no funding authorization, and no
// registration or GitHub/JIT work; the guest bootstrap endpoint owns
// registration once the lease reaches the registering phase.
type ciManagedProvisioner struct {
	store      *Store
	profile    ciManagedRunnerProfile
	signingKey []byte
	enforcer   *ciEnforcerClient
	compute    ciManagedProvisionCompute
}

const (
	ciManagedProvisionBound = 2 * time.Minute
	ciManagedStageBusyRetry = 20 * time.Millisecond
	ciManagedDetachedBound  = 30 * time.Second
	ciManagedDBBound        = 5 * time.Second
)

var errCIManagedProvisioningUnavailable = errors.New("CI managed provisioning unavailable")

func newCIManagedProvisioner(s *Store, p ciManagedRunnerProfile, key []byte, e *ciEnforcerClient, c ciManagedProvisionCompute) (*ciManagedProvisioner, error) {
	if s == nil || s.pool == nil || e == nil || c == nil || len(key) < 32 {
		return nil, ErrConflict
	}
	if !p.valid() {
		return nil, ErrConflict
	}
	bundle, err := newCIGuestBundle()
	if err != nil || bundle.SHA256 != p.GuestBundleSHA256 {
		return nil, ErrConflict
	}
	return &ciManagedProvisioner{
		store:      s,
		profile:    p.normalized(),
		signingKey: append([]byte(nil), key...),
		enforcer:   e,
		compute:    c,
	}, nil
}

// ciProvisioningIdentity loads the exact funded provisioning identity even
// when no bootstrap grant exists and after stop or expiry. The caller still
// compares the returned spec against its pinned input on every use.
func (s *Store) ciProvisioningIdentity(ctx context.Context, workspace, runner, profile string, plan ciRunnerLifecyclePlan) (ciBootstrapScope, error) {
	var out ciBootstrapScope
	err := s.withCIProvisioningCreationTx(ctx, workspace, runner, profile, plan, false, func(tx pgx.Tx, snap ciRunnerLifecycleSnapshot) error {
		var err error
		out, err = s.ciBootstrapBaseTx(ctx, tx, workspace, runner, snap)
		return err
	})
	return out, err
}

func ciManagedProvisionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrConflict):
		return ErrConflict
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	case errors.Is(err, ErrCapacity):
		return ErrCapacity
	case errors.Is(err, errCIRunnerJobPending):
		return errCIRunnerJobPending
	default:
		return errCIManagedProvisioningUnavailable
	}
}

func ciManagedEnforcerErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errCIEnforcerScope):
		return ErrConflict
	case errors.Is(err, errCIEnforcerPending):
		return errCIRunnerJobPending
	default:
		return err
	}
}

func (p *ciManagedProvisioner) validateScope(workspace, runner string, spec ciEnforcerSpec) error {
	if p == nil || p.store == nil || p.store.pool == nil {
		return errCIManagedProvisioningUnavailable
	}
	if !ciAdmissionScopeIDValid(workspace) || !ciAdmissionScopeIDValid(runner) || !spec.valid() {
		return ErrConflict
	}
	if _, err := p.profile.machineFor(spec); err != nil {
		return ciManagedProvisionError(err)
	}
	return nil
}

func ciManagedScopeMatches(stored, input ciEnforcerSpec) bool {
	if !ciRunnerLifecyclePlanEqual(stored.Plan, input.Plan) ||
		stored.GitHubInstallationID != input.GitHubInstallationID ||
		stored.GitHubRepositoryID != input.GitHubRepositoryID ||
		stored.SizeID != input.SizeID {
		return false
	}
	planA, specA := stored.digests()
	planB, specB := input.digests()
	return planA == planB && specA == specB
}

func ciManagedStatusBinding(st ciEnforcerStatus, spec ciEnforcerSpec, specDigest string) error {
	planDigest, _ := spec.digests()
	if st.SpecDigest != specDigest || st.PlanDigest != planDigest ||
		st.GitHubRepositoryID != spec.GitHubRepositoryID ||
		st.RunnerName != spec.Plan.ServerName ||
		st.DeadlineUnixMs != spec.Plan.SetupDeadlineAt.UnixMilli() ||
		st.StopRequested || st.Settled || st.CreateSettled ||
		st.RegistrationAttempted || st.RegistrationSettled || st.GitHubRunnerID != 0 {
		return ErrConflict
	}
	return nil
}

func (p *ciManagedProvisioner) checkLiveStatus(ctx context.Context, spec ciEnforcerSpec, specDigest string) error {
	st, err := p.enforcer.Status(ctx, spec.Plan)
	if err != nil {
		return ciManagedEnforcerErr(err)
	}
	if err := ciManagedStatusBinding(st, spec, specDigest); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range st.Resources {
		if !ciResourceValid(r, spec.Plan.OperationID) || seen[r.Kind] {
			return ErrConflict
		}
		seen[r.Kind] = true
	}
	return nil
}

func ciManagedStageName(stage compute.CIRunnerCreateStage) (string, bool) {
	switch stage {
	case compute.CIRunnerCreateSecurityGroup:
		return "security_group", true
	case compute.CIRunnerCreatePort:
		return "port", true
	case compute.CIRunnerCreateServer:
		return "server", true
	}
	return "", false
}

func (p *ciManagedProvisioner) fenceStage(ctx context.Context, workspace, runner string, spec ciEnforcerSpec, profileDigest, specDigest string, stage compute.CIRunnerCreateStage, known []compute.CIRunnerResource) error {
	journalStage, ok := ciManagedStageName(stage)
	if !ok {
		return ErrConflict
	}
	owned := make([]ciOwnedResource, 0, len(known))
	for _, r := range known {
		owned = append(owned, ciOwnedResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
	}
	for {
		if err := p.checkLiveStatus(ctx, spec, specDigest); err != nil {
			return err
		}
		err := p.store.fenceCIProvisioningStage(ctx, workspace, runner, profileDigest, spec.Plan, journalStage, owned)
		if err == nil {
			break
		}
		if !errors.Is(err, errCIRunnerJobPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ciManagedStageBusyRetry):
		}
	}
	return p.checkLiveStatus(ctx, spec, specDigest)
}

// Provision claims the durable funding, lease, and bootstrap grant, fences
// the one-shot create, and performs the single SDK creation call. It never
// registers the runner, authorizes funding, or tracks a new plan.
func (p *ciManagedProvisioner) Provision(ctx context.Context, workspace, runner string, spec ciEnforcerSpec) error {
	if err := p.validateScope(workspace, runner, spec); err != nil {
		return err
	}
	now := p.store.now()
	remaining := spec.Plan.SetupDeadlineAt.Sub(now)
	if remaining <= 0 {
		return errCIRunnerJobPending
	}
	bound := ciManagedProvisionBound
	if remaining < bound {
		bound = remaining
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	profileDigest, err := p.profile.digest()
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if _, err = p.store.reserveCIProvisioningBudget(ctx, workspace, runner, profileDigest, spec.Plan); err != nil {
		return ciManagedProvisionError(err)
	}
	nonce, err := ciEnforcerNewLeaseID()
	if err != nil {
		return ciManagedProvisionError(err)
	}
	scope, err := p.store.ensureCIBootstrapGrant(ctx, workspace, runner, profileDigest, spec.Plan, nonce)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if !ciManagedScopeMatches(scope.Spec, spec) {
		return ErrConflict
	}
	snap := scope.Snapshot
	if snap.CreateAttemptedAt == nil {
		return p.firstAttempt(ctx, workspace, runner, spec, scope, profileDigest)
	}
	// A create was already fenced: never invoke the SDK again and never stop
	// a possibly live in-flight creation just because another call arrived.
	if snap.CreateSettled && snap.Phase == "registering" {
		return nil
	}
	journal, err := p.store.ciProvisioningCreationState(ctx, workspace, runner, profileDigest, spec.Plan)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if ciManagedJournalFullSuccess(journal) {
		return p.finishAcknowledgement(ctx, workspace, runner, spec, profileDigest, journal)
	}
	if journal.FinishedAt != nil && journal.Outcome != nil && !journal.Outcome.MutationAttempted {
		return p.abortFinishedUnstarted(ctx, workspace, runner, spec)
	}
	return errCIRunnerJobPending
}

func ciManagedJournalFullSuccess(j ciProvisioningCreationJournal) bool {
	return j.FinishedAt != nil && j.Outcome != nil && j.Outcome.MutationAttempted &&
		j.Outcome.CreationSettled && len(j.Outcome.Resources) == 3
}

func (p *ciManagedProvisioner) finishAcknowledgement(ctx context.Context, workspace, runner string, spec ciEnforcerSpec, profileDigest string, journal ciProvisioningCreationJournal) error {
	server := journal.Outcome.Resources[2]
	if server.Kind != "server" {
		return ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return p.stopAndRecover(ctx, workspace, runner, spec, err)
	}
	st, err := p.enforcer.CloseCreation(ctx, spec.Plan, "created", server.ID)
	if err != nil {
		if ctx.Err() != nil {
			return p.stopAndRecover(ctx, workspace, runner, spec, ctx.Err())
		}
		// The durable outcome is finished; a later call retries only this
		// acknowledgement and never replays the provider write.
		return ciManagedProvisionError(ciManagedEnforcerErr(err))
	}
	if err := ctx.Err(); err != nil {
		return p.stopAndRecover(ctx, workspace, runner, spec, err)
	}
	markCtx, markCancel := context.WithTimeout(ctx, ciManagedDBBound)
	defer markCancel()
	if err := p.store.markCIProvisioningRegistering(markCtx, workspace, runner, profileDigest, spec.Plan, st); err != nil {
		if ctx.Err() != nil {
			return p.stopAndRecover(ctx, workspace, runner, spec, ctx.Err())
		}
		return ciManagedProvisionError(err)
	}
	return nil
}

func (p *ciManagedProvisioner) abortFinishedUnstarted(ctx context.Context, workspace, runner string, spec ciEnforcerSpec) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ciManagedDBBound)
	stopped, err := p.store.stopUnstartedCIManagedRunner(stopCtx, workspace, runner, spec.Plan)
	cancel()
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if !stopped {
		// Another admission or cleanup already owns this runner; the abort
		// is no longer ours to force.
		return errCIRunnerJobPending
	}
	p.recoverQuiet(ctx, workspace, runner, spec)
	return nil
}

func (p *ciManagedProvisioner) firstAttempt(ctx context.Context, workspace, runner string, spec ciEnforcerSpec, scope ciBootstrapScope, profileDigest string) error {
	machine, err := p.profile.machineFor(spec)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	now := p.store.now()
	token, err := mintCIBootstrapCapability(p.signingKey, scope.claims(workspace, runner), now, spec.Plan.SetupDeadlineAt)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	userData, err := buildCIGuestUserData(p.profile, scope, token, now)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	_, specDigest := spec.digests()
	st, err := p.enforcer.PutLease(ctx, scope.Spec)
	if err != nil {
		return ciManagedProvisionError(ciManagedEnforcerErr(err))
	}
	if st.CreateSettled {
		// A concurrent creator already fenced and finished while this call
		// was between the snapshot read and the lease put; the journal
		// branch owns acknowledgement, never a second provider write.
		return errCIRunnerJobPending
	}
	if err := ciManagedStatusBinding(st, spec, specDigest); err != nil {
		return err
	}
	if st.Resources == nil || len(st.Resources) != 0 {
		return ErrConflict
	}
	fenced, err := p.store.fenceCIRunnerCreate(ctx, workspace, runner)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if !fenced {
		return errCIRunnerJobPending
	}
	request := compute.CIRunnerCreateRequest{
		Scope:     ciProviderScope(spec.Plan),
		FlavorID:  machine.FlavorID,
		ImageID:   p.profile.ImageID,
		VCPUs:     machine.VCPUs,
		MemoryMiB: machine.MemoryMiB,
		DiskGiB:   machine.DiskGiB,
		UserData:  userData,
		BeforeMutation: func(fctx context.Context, stage compute.CIRunnerCreateStage, known []compute.CIRunnerResource) error {
			return p.fenceStage(fctx, workspace, runner, spec, profileDigest, specDigest, stage, known)
		},
	}
	result, sdkErr := p.compute.CreateCIRunner(ctx, request)
	outcome := ciProvisioningCreateOutcome{
		MutationAttempted: result.MutationAttempted,
		CreationSettled:   result.CreationSettled,
		Resources:         ciManagedOutcomeResources(result.Resources),
	}
	dbCtx, dbCancel := context.WithTimeout(context.WithoutCancel(ctx), ciManagedDBBound)
	recErr := p.store.recordCIProvisioningCreationResult(dbCtx, workspace, runner, profileDigest, spec.Plan, outcome)
	dbCancel()
	if recErr != nil {
		return p.stopAndRecover(ctx, workspace, runner, spec, recErr)
	}
	if sdkErr != nil || !result.CreationSettled || !result.MutationAttempted || len(result.Resources) != 3 {
		cause := sdkErr
		if cause == nil {
			cause = errCIManagedProvisioningUnavailable
		}
		return p.stopAndRecover(ctx, workspace, runner, spec, cause)
	}
	// The SDK reported a full create, but a cancelled parent must never let
	// the lease reach registering: admission could already own the grant.
	if err := ctx.Err(); err != nil {
		return p.stopAndRecover(ctx, workspace, runner, spec, err)
	}
	serverID := ""
	for _, r := range result.Resources {
		if r.Kind == "server" {
			serverID = r.ID
		}
	}
	st, err = p.enforcer.CloseCreation(ctx, spec.Plan, "created", serverID)
	if err != nil {
		if ctx.Err() != nil {
			return p.stopAndRecover(ctx, workspace, runner, spec, ctx.Err())
		}
		// The durable outcome is finished; a later call retries only this
		// acknowledgement and never replays the provider write.
		return ciManagedProvisionError(ciManagedEnforcerErr(err))
	}
	if err := ctx.Err(); err != nil {
		return p.stopAndRecover(ctx, workspace, runner, spec, err)
	}
	markCtx, markCancel := context.WithTimeout(ctx, ciManagedDBBound)
	defer markCancel()
	if err := p.store.markCIProvisioningRegistering(markCtx, workspace, runner, profileDigest, spec.Plan, st); err != nil {
		if ctx.Err() != nil {
			return p.stopAndRecover(ctx, workspace, runner, spec, ctx.Err())
		}
		return ciManagedProvisionError(err)
	}
	return nil
}

func ciManagedOutcomeResources(in []compute.CIRunnerResource) []ciOwnedResource {
	out := make([]ciOwnedResource, 0, len(in))
	for _, r := range in {
		out = append(out, ciOwnedResource{Kind: r.Kind, ID: r.ID, OperationID: r.OperationID})
	}
	return out
}

// stopAndRecover performs the guarded pre-adoption stop, a best-effort
// enforcer stop, and a bounded best-effort Recover after an SDK failure or
// an unpersistable outcome. The root guard is the only stop authority: when
// the runner was adopted, already cleaned, or the stop itself fails, no
// enforcer stop or recovery runs and the caller still receives only the
// sanitized original error.
func (p *ciManagedProvisioner) stopAndRecover(ctx context.Context, workspace, runner string, spec ciEnforcerSpec, cause error) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ciManagedDBBound)
	stopped, err := p.store.stopUnstartedCIManagedRunner(stopCtx, workspace, runner, spec.Plan)
	cancel()
	if err != nil || !stopped {
		return ciManagedProvisionError(cause)
	}
	enfCtx, ecancel := context.WithTimeout(context.WithoutCancel(ctx), ciManagedDBBound)
	_, _ = p.enforcer.Stop(enfCtx, spec.Plan)
	ecancel()
	p.recoverQuiet(ctx, workspace, runner, spec)
	return ciManagedProvisionError(cause)
}

func (p *ciManagedProvisioner) recoverQuiet(ctx context.Context, workspace, runner string, spec ciEnforcerSpec) {
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ciManagedDetachedBound)
	defer cancel()
	_ = p.Recover(recCtx, workspace, runner, spec)
}

// Recover advances creation recovery for an already stopped or expired
// setup with no execution. It never invokes the SDK, never registers, and
// never releases funding; the lifecycle core owns later cleanup proofs.
func (p *ciManagedProvisioner) Recover(ctx context.Context, workspace, runner string, spec ciEnforcerSpec) error {
	if err := p.validateScope(workspace, runner, spec); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, ciManagedDetachedBound)
	defer cancel()
	profileDigest, err := p.profile.digest()
	if err != nil {
		return ciManagedProvisionError(err)
	}
	scope, err := p.store.ciProvisioningIdentity(ctx, workspace, runner, profileDigest, spec.Plan)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if !ciManagedScopeMatches(scope.Spec, spec) {
		return ErrConflict
	}
	snap := scope.Snapshot
	if snap.ExecutionStartedAt != nil {
		return ErrConflict
	}
	if snap.CreateSettled && snap.StopRequestedAt == nil {
		return nil
	}
	if snap.StopRequestedAt == nil {
		if spec.Plan.SetupDeadlineAt.After(p.store.now()) {
			return errCIRunnerJobPending
		}
		stopCtx, sc := context.WithTimeout(ctx, ciManagedDBBound)
		stopped, serr := p.store.stopUnstartedCIManagedRunner(stopCtx, workspace, runner, spec.Plan)
		sc()
		if serr != nil {
			return ciManagedProvisionError(serr)
		}
		if !stopped {
			// Another admission or cleanup already owns this runner.
			return nil
		}
		scope, err = p.store.ciProvisioningIdentity(ctx, workspace, runner, profileDigest, spec.Plan)
		if err != nil {
			return ciManagedProvisionError(err)
		}
		snap = scope.Snapshot
		if snap.ExecutionStartedAt != nil {
			return ErrConflict
		}
	}
	journal, err := p.store.ciProvisioningCreationState(ctx, workspace, runner, profileDigest, spec.Plan)
	if err != nil {
		return ciManagedProvisionError(err)
	}
	if journal.Stage == "not_started" && journal.FinishedAt == nil {
		sealCtx, sc := context.WithTimeout(ctx, ciManagedDBBound)
		err = p.store.sealUnstartedCIProvisioningCreation(sealCtx, workspace, runner, profileDigest, spec.Plan)
		sc()
		if err != nil {
			return ciManagedProvisionError(err)
		}
		journal, err = p.store.ciProvisioningCreationState(ctx, workspace, runner, profileDigest, spec.Plan)
		if err != nil {
			return ciManagedProvisionError(err)
		}
	}
	if journal.FinishedAt != nil && journal.Outcome != nil &&
		!journal.Outcome.MutationAttempted && journal.Outcome.CreationSettled {
		_, err = p.enforcer.RejectUnstarted(ctx, spec)
		return ciManagedProvisionError(ciManagedEnforcerErr(err))
	}
	_, _ = p.enforcer.Stop(ctx, spec.Plan)
	st, err := p.enforcer.Status(ctx, spec.Plan)
	if err != nil {
		return ciManagedProvisionError(ciManagedEnforcerErr(err))
	}
	_, specDigest := spec.digests()
	if st.SpecDigest != specDigest {
		return ErrConflict
	}
	if st.Settled {
		// A validated tombstone zeroes private metadata and inventory.
		return nil
	}
	if st.GitHubRepositoryID != spec.GitHubRepositoryID || st.RunnerName != spec.Plan.ServerName ||
		st.RegistrationAttempted || st.RegistrationSettled || st.GitHubRunnerID != 0 {
		return ErrConflict
	}
	if st.CreateSettled {
		return nil
	}
	if ciManagedJournalFullSuccess(journal) {
		server := journal.Outcome.Resources[2]
		if server.Kind != "server" {
			return ErrConflict
		}
		_, err = p.enforcer.CloseCreation(ctx, spec.Plan, "created", server.ID)
		return ciManagedProvisionError(ciManagedEnforcerErr(err))
	}
	return p.closeFromWitness(ctx, spec, journal, st)
}

// closeFromWitness settles an unfinished or ambiguous creation only from a
// positive owned witness retained by the enforcer: exactly one resource per
// kind, bound to this operation, consistent with every journal-acknowledged
// ID, and never later than the last fenced stage. Absence never settles a
// possible late POST; the caller holds capacity until a future pass proves it.
func (p *ciManagedProvisioner) closeFromWitness(ctx context.Context, spec ciEnforcerSpec, journal ciProvisioningCreationJournal, st ciEnforcerStatus) error {
	lastIndex := ciProvisioningStageIndex(journal.Stage)
	if lastIndex < 1 {
		return ErrConflict
	}
	candidates := map[string]string{}
	for _, r := range journal.KnownResources {
		candidates[r.Kind] = r.ID
	}
	if journal.Outcome != nil {
		for _, r := range journal.Outcome.Resources {
			candidates[r.Kind] = r.ID
		}
	}
	witness := map[string]string{}
	for _, r := range st.Resources {
		if !ciResourceValid(r, spec.Plan.OperationID) {
			return ErrConflict
		}
		kindIndex := ciProvisioningStageIndex(r.Kind)
		if kindIndex < 0 || kindIndex > lastIndex {
			return ErrConflict
		}
		if _, dup := witness[r.Kind]; dup {
			return ErrConflict
		}
		if prior, ok := candidates[r.Kind]; ok && prior != r.ID {
			return ErrConflict
		}
		witness[r.Kind] = r.ID
	}
	witnessID, ok := witness[journal.Stage]
	if !ok {
		return errCIRunnerJobPending
	}
	result, serverID := "rejected", ""
	if journal.Stage == "server" {
		result, serverID = "created", witnessID
	}
	_, err := p.enforcer.CloseCreation(ctx, spec.Plan, result, serverID)
	return ciManagedProvisionError(ciManagedEnforcerErr(err))
}
