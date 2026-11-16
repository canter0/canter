package controlplane

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ciJobLogCollector drains eligible completed-job log work with a fixed pair
// of workers. It performs no I/O at construction; every poll lists at most
// ciJobLogWorkers candidates, claims the first available, downloads outside
// any database lock, and publishes or marks the attempt unavailable. Every
// failure is retried on the fixed policy and never stops the runtime.
type ciJobLogCollector struct {
	store    *Store
	download func(context.Context, ciJobLogScope) (string, bool, error)
}

func newCIJobLogCollector(store *Store, gh *ciGitHubClient, allowLoopbackHTTP bool) *ciJobLogCollector {
	if store == nil || gh == nil {
		return nil
	}
	return &ciJobLogCollector{
		store: store,
		download: func(ctx context.Context, scope ciJobLogScope) (string, bool, error) {
			return gh.downloadCIJobLogs(ctx, scope, allowLoopbackHTTP)
		},
	}
}

// Run joins exactly ciJobLogWorkers polling goroutines and returns the
// context error once the parent context ends and every worker has exited.
func (c *ciJobLogCollector) Run(ctx context.Context) error {
	if c == nil || c.store == nil || c.download == nil {
		return errors.New("ci job log collector requires a store and downloader")
	}
	var wg sync.WaitGroup
	for i := 0; i < ciJobLogWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(ctx)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func (c *ciJobLogCollector) worker(ctx context.Context) {
	ticker := time.NewTicker(ciJobLogPollInterval)
	defer ticker.Stop()
	for {
		c.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// poll claims at most one job per call so each worker downloads serially.
func (c *ciJobLogCollector) poll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	items, err := c.store.ciJobLogCandidates(qctx)
	cancel()
	if err != nil {
		return
	}
	for _, item := range items {
		nonce, err := ciEnforcerNewLeaseID()
		if err != nil {
			return
		}
		cctx, ccancel := context.WithTimeout(ctx, ciJobLogDBBound)
		claim, err := c.store.claimCIJobLog(cctx, item.Workspace, item.Job, nonce)
		ccancel()
		if err != nil {
			continue
		}
		c.process(ctx, claim)
		return
	}
}

func (c *ciJobLogCollector) process(ctx context.Context, claim ciJobLogClaim) {
	text, truncated, err := c.download(ctx, claim.Scope)
	if err != nil {
		// A cancelled parent leaves the lease to expire; no detached writes.
		if ctx.Err() == nil {
			fctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
			_ = c.store.failCIJobLog(fctx, claim)
			cancel()
		}
		return
	}
	pctx, cancel := context.WithTimeout(ctx, ciJobLogDBBound)
	_ = c.store.publishCIJobLog(pctx, claim, text, truncated)
	cancel()
}
