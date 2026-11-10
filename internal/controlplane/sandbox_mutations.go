package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wrencomputer "github.com/canter0/canter/internal/wrencomputer"
)

// sandboxMutationRejected marks a pre-RPC failure — the remote operation
// definitely did not launch (unlike transport-uncertain errors).
type sandboxMutationRejected struct{ err error }

func (e *sandboxMutationRejected) Error() string { return e.err.Error() }
func (e *sandboxMutationRejected) Unwrap() error { return e.err }

// sandboxMutation is one durable claim on a sandbox: it exists from claim
// through close, blocks sleep while unexpired, and survives an uncertain
// remote result until its bound expires.
type sandboxMutation struct {
	store       *Store
	sandboxID   string
	workspaceID string
	token       string
	generation  int
	started     time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	uncertain   bool
}

const sandboxMutationMaxBound = 10 * time.Minute

// beginSandboxMutation claims a mutation token after confirming generation,
// ready phase, and no sleep/delete request — all under the sandbox row lock.
// Concurrent mutations are allowed; each carries its own token.
func (s *Store) beginSandboxMutation(ctx context.Context, v Sandbox, bound time.Duration) (*sandboxMutation, error) {
	return s.beginSandboxMutationWithControl(ctx, v, bound, nil)
}

// beginSandboxMutationWithControl is the mutation core: a nonnil claim marks a
// desktop-input mutation fenced against the live human/agent desktop control
// lease. nil is an ordinary mutation (unchanged semantics).
func (s *Store) beginSandboxMutationWithControl(ctx context.Context, v Sandbox, bound time.Duration, claim *sandboxDesktopInputClaim) (*sandboxMutation, error) {
	if bound <= 0 || bound > sandboxMutationMaxBound || bound != bound.Round(time.Second) {
		return nil, &sandboxMutationRejected{err: fmt.Errorf("invalid sandbox mutation bound %s", bound)}
	}
	started := time.Now()
	operationCtx, cancel := context.WithDeadline(ctx, started.Add(bound))
	token, err := newID("sop_")
	if err != nil {
		cancel()
		return nil, &sandboxMutationRejected{err: err}
	}
	tx, err := s.pool.Begin(operationCtx)
	if err != nil {
		cancel()
		return nil, &sandboxMutationRejected{err: err}
	}
	defer tx.Rollback(operationCtx)
	// Human claims lock workspace→writer before the sandbox row (consistent
	// order with POST /desktop/control).
	if claim != nil && claim.AccountID != "" {
		if _, err := s.lockWorkspace(operationCtx, tx, v.WorkspaceID, claim.AccountID); err != nil {
			cancel()
			return nil, &sandboxMutationRejected{err: err}
		}
		if err := requireWorkspaceWriter(operationCtx, tx, v.WorkspaceID, claim.AccountID, true); err != nil {
			cancel()
			return nil, &sandboxMutationRejected{err: err}
		}
	}
	var generation int
	var phase string
	var sleepRequested, deleteRequested, desktop bool
	var setupJSON []byte
	err = tx.QueryRow(operationCtx,
		`SELECT generation,phase,sleep_requested,delete_requested,desktop,COALESCE(setup::text,'{}') FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		v.WorkspaceID, v.ID).Scan(&generation, &phase, &sleepRequested, &deleteRequested, &desktop, &setupJSON)
	if err == nil {
		if generation != v.Generation || phase != "ready" || sleepRequested || deleteRequested {
			err = ErrConflict
		}
		var setup SandboxSetup
		if uerr := json.Unmarshal(setupJSON, &setup); uerr != nil {
			err = uerr
		} else if sandboxSetupPending(setup) {
			err = fmt.Errorf("%w: sandbox setup is still running", ErrConflict)
		}
	}
	if err == nil && claim != nil && !desktop {
		err = fmt.Errorf("%w: this sandbox has no desktop", ErrConflict)
	}
	if err == nil && desktop {
		// Active human control gates ordinary mutations and all desktop input.
		control, ctlErr := sandboxDesktopControlState(operationCtx, tx, v.WorkspaceID, v.ID, generation, func() string {
			if claim != nil {
				return claim.AccountID
			}
			return ""
		}())
		if ctlErr != nil {
			err = ctlErr
		} else if control.Mode == "human" {
			if claim == nil || claim.AccountID == "" {
				err = fmt.Errorf("%w: Return desktop control before starting commands or changing files.", ErrConflict)
			} else if claim.AccountID != control.AccountID || claim.LeaseToken == "" || claim.LeaseToken != control.LeaseToken {
				err = fmt.Errorf("%w: Return desktop control before starting commands or changing files.", ErrConflict)
			}
		} else if claim != nil && claim.AccountID != "" {
			// Human claim without a live own lease cannot impersonate.
			err = fmt.Errorf("%w: no active desktop lease for this account", ErrConflict)
		}
	}
	if err == nil && desktop {
		// Every desktop mutation is exclusive against live input rows;
		// input is exclusive against all live mutations.
		var busy bool
		if busyErr := tx.QueryRow(operationCtx,
			`SELECT EXISTS(SELECT 1 FROM sandbox_mutations
			 WHERE sandbox_id=$1 AND expires_at>clock_timestamp() AND ($2 OR desktop_input))`,
			v.ID, claim != nil).Scan(&busy); busyErr != nil {
			err = busyErr
		} else if busy {
			err = fmt.Errorf("%w: another desktop operation is in flight", ErrConflict)
		}
	}
	if err == nil && claim != nil && claim.AccountID != "" {
		// Renew the live human lease on accepted input.
		renewed, renewErr := tx.Exec(operationCtx,
			`UPDATE sandbox_desktop_control SET expires_at=clock_timestamp()+interval '60 seconds'
			 WHERE sandbox_id=$1 AND account_id=$2 AND generation=$3 AND lease_token=$4 AND expires_at>clock_timestamp()`,
			v.ID, claim.AccountID, generation, claim.LeaseToken)
		if renewErr != nil {
			err = renewErr
		} else if renewed.RowsAffected() != 1 {
			err = fmt.Errorf("%w: desktop lease changed", ErrConflict)
		}
	}
	if err == nil {
		_, err = tx.Exec(operationCtx,
			`INSERT INTO sandbox_mutations(token,sandbox_id,generation,expires_at,desktop_input) VALUES($1,$2,$3,clock_timestamp()+($4::bigint*interval '1 second'),$5)`,
			token, v.ID, generation, int64(bound/time.Second)+15, claim != nil)
	}
	if err != nil {
		cancel()
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, &sandboxMutationRejected{err: err}
	}
	if err := tx.Commit(operationCtx); err != nil {
		cancel()
		return nil, &sandboxMutationRejected{err: err}
	}
	return &sandboxMutation{
		store: s, sandboxID: v.ID, workspaceID: v.WorkspaceID, token: token,
		generation: generation, started: started, ctx: operationCtx, cancel: cancel,
	}, nil
}

// run wraps a remote operation: uncertain is set before the call and only
// cleared on nil or a definitive client error — transport-unknown results
// retain the claim until its bound expires.
func (o *sandboxMutation) run(action func(context.Context) error) error {
	if o.ctx.Err() != nil {
		return &sandboxMutationRejected{err: o.ctx.Err()}
	}
	o.uncertain = true
	err := action(o.ctx)
	var remote *wrencomputer.Error
	if err == nil || (errors.As(err, &remote) && (strings.HasPrefix(remote.Code, "invalid_") || remote.Code == "forbidden" || remote.Code == "conflict")) {
		o.uncertain = false
	}
	return err
}

// close releases the claim; a known (not uncertain) operation removes its
// token immediately. Never removes a replacement's token — tokens are unique.
func (o *sandboxMutation) close() {
	o.cancel()
	if o.uncertain {
		return // claim persists until its bound expires
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = o.store.pool.Exec(cleanup,
		`DELETE FROM sandbox_mutations WHERE sandbox_id=$1 AND token=$2`,
		o.sandboxID, o.token)
}

// enterSandboxSleep is the atomic ready→sleeping barrier: it requests sleep
// (blocking new mutations), waits out unexpired claims, then flips phase.
// Idempotent — an already-sleeping sandbox may re-run the barrier safely.
func (s *Store) enterSandboxSleep(ctx context.Context, v Sandbox) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var generation int
	var phase string
	var deleteRequested bool
	var setupRaw []byte
	err = tx.QueryRow(ctx,
		`SELECT generation,phase,delete_requested,setup FROM sandboxes WHERE workspace_id=$1 AND id=$2 FOR UPDATE`,
		v.WorkspaceID, v.ID).Scan(&generation, &phase, &deleteRequested, &setupRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if generation != v.Generation || deleteRequested || phase == "deleted" || phase == "deleting" || phase == "asleep" || phase == "failed" {
		return ErrConflict
	}
	var setup SandboxSetup
	if err := json.Unmarshal(setupRaw, &setup); err != nil {
		return err
	}
	if setup.Status == "running" {
		setup.Status, setup.Error = "failed", setupInterruptMessage
		raw, err := sandboxSetupJSON(setup)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sandboxes SET setup=$3 WHERE workspace_id=$1 AND id=$2`, v.WorkspaceID, v.ID, raw); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sandboxes SET sleep_requested=true,sleep_forced_at=COALESCE(sleep_forced_at,$2),next_attempt_at=now()+interval '3 seconds',updated_at=now() WHERE workspace_id=$1 AND id=$3`,
		v.WorkspaceID, v.SleepForcedAt, v.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM sandbox_mutations WHERE sandbox_id=$1 AND expires_at<=clock_timestamp()`, v.ID); err != nil {
		return err
	}
	var pending bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM sandbox_mutations WHERE sandbox_id=$1 AND expires_at>clock_timestamp())`, v.ID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		// Persist sleep_requested (blocks new mutations) then signal the
		// outstanding claims — do NOT roll back the flag.
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sandboxes SET phase='sleeping',updated_at=now() WHERE workspace_id=$1 AND id=$2`, v.WorkspaceID, v.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// fencedSandboxClient wraps a private client with durable mutation claims:
// every mutating RPC carries its own token through sandboxMutatingCall.
type fencedSandboxClient struct {
	sandboxAgentClient
	store   *Store
	sandbox Sandbox
}

func sandboxMutatingCall[T any](ctx context.Context, s *Store, v Sandbox, bound time.Duration, call func(context.Context) (T, error)) (T, error) {
	var result T
	op, err := s.beginSandboxMutation(ctx, v, bound)
	if err != nil {
		return result, err
	}
	defer op.close()
	err = op.run(func(active context.Context) error {
		var e error
		result, e = call(active)
		return e
	})
	return result, err
}

func (f *fencedSandboxClient) StartJob(ctx context.Context, request wrencomputer.JobRequest) (wrencomputer.Job, error) {
	return sandboxMutatingCall(ctx, f.store, f.sandbox, 45*time.Second, func(c context.Context) (wrencomputer.Job, error) {
		return f.sandboxAgentClient.StartJob(c, request)
	})
}
func (f *fencedSandboxClient) StopJob(ctx context.Context, id string) (wrencomputer.Job, error) {
	return sandboxMutatingCall(ctx, f.store, f.sandbox, 45*time.Second, func(c context.Context) (wrencomputer.Job, error) {
		return f.sandboxAgentClient.StopJob(c, id)
	})
}
func (f *fencedSandboxClient) WriteFile(ctx context.Context, path string, data []byte) (wrencomputer.FileChunk, error) {
	return sandboxMutatingCall(ctx, f.store, f.sandbox, 45*time.Second, func(c context.Context) (wrencomputer.FileChunk, error) {
		return f.sandboxAgentClient.WriteFile(c, path, data)
	})
}
func (f *fencedSandboxClient) BeginUpload(ctx context.Context, id, path string, size int64, sha256 string) (wrencomputer.FileUpload, error) {
	return sandboxMutatingCall(ctx, f.store, f.sandbox, 45*time.Second, func(c context.Context) (wrencomputer.FileUpload, error) {
		return f.sandboxAgentClient.BeginUpload(c, id, path, size, sha256)
	})
}
func (f *fencedSandboxClient) UploadChunk(ctx context.Context, id string, offset int64, data []byte) (wrencomputer.FileUpload, error) {
	return sandboxMutatingCall(ctx, f.store, f.sandbox, 45*time.Second, func(c context.Context) (wrencomputer.FileUpload, error) {
		return f.sandboxAgentClient.UploadChunk(c, id, offset, data)
	})
}
func (f *fencedSandboxClient) CompleteUpload(ctx context.Context, id string) (wrencomputer.FileUpload, error) {
	return sandboxMutatingCall(ctx, f.store, f.sandbox, 10*time.Minute, func(c context.Context) (wrencomputer.FileUpload, error) {
		return f.sandboxAgentClient.CompleteUpload(c, id)
	})
}
func (f *fencedSandboxClient) AbortUpload(ctx context.Context, id string) error {
	_, err := sandboxMutatingCall(ctx, f.store, f.sandbox, 45*time.Second, func(c context.Context) (struct{}, error) {
		return struct{}{}, f.sandboxAgentClient.AbortUpload(c, id)
	})
	return err
}

// sandboxLiveLease owns a mutation token for a long-lived resource (terminal,
// preview): it renews only while the sandbox stays ready, same generation,
// no sleep/delete requested, and authority still validates.
type sandboxLiveLease struct {
	op          *sandboxMutation
	done        chan struct{}
	cancel      context.CancelFunc
	once        sync.Once
	activity    atomic.Bool
	closeRemote func()
	validate    func(context.Context) error
}

// keepSandboxMutation promotes a started mutation into a live lease after the
// remote open succeeded. The startup context is cancelled; lifetime is a
// detached background context so the lease outlives the request that opened it.
func keepSandboxMutation(op *sandboxMutation, v Sandbox, validate func(context.Context) error, closeRemote func()) *sandboxLiveLease {
	op.cancel() // startup context bound already served its purpose
	lifetime, cancel := context.WithCancel(context.Background())
	l := &sandboxLiveLease{op: op, done: make(chan struct{}), cancel: cancel, closeRemote: closeRemote, validate: validate}
	lifetimeCtx := lifetime
	go l.loop(lifetimeCtx)
	return l
}

func (l *sandboxLiveLease) loop(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	watchdog := time.NewTimer(time.Until(l.op.started.Add(45 * time.Second)))
	defer watchdog.Stop()
	for {
		select {
		case <-ctx.Done():
			l.Close()
			return
		case <-watchdog.C:
			l.Close()
			return
		case <-tick.C:
		}
		if l.validate != nil {
			vctx, vc := context.WithTimeout(ctx, 1*time.Second)
			err := l.validate(vctx)
			vc()
			if err != nil {
				l.Close()
				return
			}
		}
		rctx, rc := context.WithTimeout(ctx, 2*time.Second)
		renewStart := time.Now()
		result, err := l.op.store.pool.Exec(rctx,
			`UPDATE sandbox_mutations o SET expires_at=clock_timestamp()+interval '60 seconds'
			 WHERE o.token=$1 AND o.sandbox_id=$2 AND o.generation=$3 AND o.expires_at>clock_timestamp()
			 AND EXISTS(SELECT 1 FROM sandboxes b WHERE b.id=o.sandbox_id AND b.workspace_id=$4 AND b.generation=o.generation AND b.phase='ready' AND NOT b.sleep_requested AND NOT b.delete_requested)`,
			l.op.token, l.op.sandboxID, l.op.generation, l.op.workspaceID)
		rc()
		if err != nil || result.RowsAffected() != 1 {
			l.Close()
			return
		}
		if l.activity.Swap(false) {
			tctx, tc := context.WithTimeout(ctx, 1*time.Second)
			l.op.store.touchSandbox(tctx, l.op.sandboxID)
			tc()
		}
		if !watchdog.Stop() {
			select {
			case <-watchdog.C:
			default:
			}
		}
		watchdog.Reset(time.Until(renewStart.Add(45 * time.Second)))
	}
}

// Activity marks real bytes/input so idle connections don't hold a sandbox awake.
func (l *sandboxLiveLease) Activity() { l.activity.Store(true) }

// Close once: stop the loop, close the remote resource BEFORE removing the
// claim, then release the mutation token (uncertain=false — known outcome).
func (l *sandboxLiveLease) Close() {
	l.once.Do(func() {
		close(l.done)
		l.cancel()
		if l.closeRemote != nil {
			l.closeRemote()
		}
		l.op.uncertain = false
		l.op.close()
	})
}
