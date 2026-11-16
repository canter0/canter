package controlplane

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ciManagedScheduler is the private autoscheduler for managed CI runners.
// Private constructors leave it disabled; only the managed runtime factory
// enables its own instance, and Run refuses while enabled is false.
type ciManagedScheduler struct {
	provisioner *ciManagedProvisioner
	lifecycle   *ciRunnerLifecycleWorker
	enabled     bool
	// admissionReady is an optional private gate consulted after completed
	// work drains and before new lookups or claims. The managed runtime sets
	// it to a bounded enforcer health probe; private test constructors leave
	// it nil.
	admissionReady func(context.Context) bool
	running        atomic.Bool
}

var errCIManagedSchedulingDisabled = errors.New("CI managed scheduling is disabled")

func newCIManagedScheduler(p *ciManagedProvisioner, github *ciGitHubClient) (*ciManagedScheduler, error) {
	if p == nil || p.store == nil || p.store.pool == nil || p.compute == nil ||
		p.enforcer == nil || len(p.signingKey) < 32 || !p.profile.valid() || github == nil {
		return nil, ErrConflict
	}
	return &ciManagedScheduler{
		provisioner: p,
		lifecycle: &ciRunnerLifecycleWorker{
			Store:             p.store,
			Backend:           &ciLifecycleProviderBackend{Compute: p.compute, Enforcer: p.enforcer},
			GitHub:            github,
			UnstartedEnforcer: p.enforcer,
		},
	}, nil
}

// Run drives lifecycle reconciliation and managed creation until ctx ends.
// It returns the context error; every worker and the lifecycle goroutine is
// joined before return so store cleanup never races in-flight work.
func (s *ciManagedScheduler) Run(ctx context.Context) error {
	if s == nil || !s.enabled {
		return errCIManagedSchedulingDisabled
	}
	if !s.running.CompareAndSwap(false, true) {
		return ErrConflict
	}
	defer s.running.Store(false)
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.lifecycle.Run(ctx)
	}()
	s.dispatchLoop(ctx, &wg)
	cancel()
	wg.Wait()
	return ctx.Err()
}

// dispatchLoop owns the inflight identity map, the completion channel, and
// the rotating work cursor. The cursor is local to one Run: durable fences
// make a restart revisit identities safe, never replayed.
func (s *ciManagedScheduler) dispatchLoop(ctx context.Context, wg *sync.WaitGroup) {
	profileDigest, err := s.provisioner.profile.digest()
	if err != nil {
		<-ctx.Done()
		return
	}
	active := map[string]bool{}
	completions := make(chan ciManagedWorkItem, ciManagedConcurrency)
	var cursorWorkspace, cursorRunner string
	tick := func() {
		s.tick(ctx, wg, active, completions, &cursorWorkspace, &cursorRunner, profileDigest)
	}
	tick()
	ticker := time.NewTicker(ciManagedPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// tick drains finished workers, dispatches listed work into free slots, then
// claims new funded demand only while a worker slot is immediately owned.
// Transient lookup and claim failures simply wait for the next poll.
func (s *ciManagedScheduler) tick(ctx context.Context, wg *sync.WaitGroup, active map[string]bool, completions chan ciManagedWorkItem, cursorWorkspace, cursorRunner *string, profileDigest string) {
	for {
		select {
		case item := <-completions:
			delete(active, item.Workspace+"/"+item.Runner)
		default:
			goto drained
		}
	}
drained:
	if s.admissionReady != nil && !s.admissionReady(ctx) {
		return
	}
	free := ciManagedConcurrency - len(active)
	if free <= 0 {
		return
	}
	lookupCtx, lookupCancel := context.WithTimeout(ctx, ciManagedDBBound)
	items, err := s.provisioner.store.listCIManagedWork(lookupCtx, profileDigest, *cursorWorkspace, *cursorRunner)
	lookupCancel()
	if err != nil {
		return
	}
	for _, item := range items {
		// The cursor advances for every selected identity, including ones
		// already inflight, so ambiguous pending work cannot starve fresh
		// demand behind it.
		*cursorWorkspace, *cursorRunner = item.Workspace, item.Runner
		key := item.Workspace + "/" + item.Runner
		if active[key] || free <= 0 {
			continue
		}
		active[key] = true
		s.dispatch(ctx, wg, completions, item, profileDigest)
		free--
	}
	for free > 0 {
		claimCtx, claimCancel := context.WithTimeout(ctx, ciManagedDBBound)
		claim, err := s.provisioner.store.claimCIManagedRunner(claimCtx, s.provisioner.profile)
		claimCancel()
		if err != nil {
			return
		}
		item := ciManagedWorkItem{Workspace: claim.Workspace, Runner: claim.Runner, Plan: claim.Spec.Plan}
		key := item.Workspace + "/" + item.Runner
		if active[key] {
			// Already owned by a worker: the work query rediscovers it.
			return
		}
		active[key] = true
		s.dispatch(ctx, wg, completions, item, profileDigest)
		free--
	}
}

func (s *ciManagedScheduler) dispatch(ctx context.Context, wg *sync.WaitGroup, completions chan<- ciManagedWorkItem, item ciManagedWorkItem, profileDigest string) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { completions <- item }()
		s.runWork(ctx, item, profileDigest)
	}()
}

// runWork performs one bounded unit of creator work for a claimed or
// discovered identity. Worker outcomes are never logged raw; the durable
// store, journal, and enforcer records carry every fact a later pass needs.
func (s *ciManagedScheduler) runWork(ctx context.Context, item ciManagedWorkItem, profileDigest string) {
	p := s.provisioner
	identityCtx, identityCancel := context.WithTimeout(ctx, ciManagedDBBound)
	scope, err := p.store.ciProvisioningIdentity(identityCtx, item.Workspace, item.Runner, profileDigest, item.Plan)
	identityCancel()
	if err != nil {
		return
	}
	snap := scope.Snapshot
	if snap.CleanupVerifiedAt != nil {
		releaseCtx, releaseCancel := context.WithTimeout(ctx, ciManagedDBBound)
		_ = p.store.releaseCIProvisioningCapacity(releaseCtx, item.Workspace, item.Runner)
		releaseCancel()
		return
	}
	if snap.ExecutionStartedAt != nil {
		// An admitted job belongs exclusively to the lifecycle worker.
		return
	}
	spec := scope.Spec
	if snap.StopRequestedAt != nil || !spec.Plan.SetupDeadlineAt.After(p.store.now()) {
		_ = p.Recover(ctx, item.Workspace, item.Runner, spec)
		return
	}
	err = p.Provision(ctx, item.Workspace, item.Runner, spec)
	if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
		return
	}
	// A conflict or missing identity may be a late eligibility failure;
	// only the guarded pre-adoption stop may force recovery, and only when
	// it actually commits.
	stopCtx, stopCancel := context.WithTimeout(ctx, ciManagedDBBound)
	stopped, stopErr := p.store.stopUnstartedCIManagedRunner(stopCtx, item.Workspace, item.Runner, spec.Plan)
	stopCancel()
	if stopErr == nil && stopped {
		_ = p.Recover(ctx, item.Workspace, item.Runner, spec)
	}
}
