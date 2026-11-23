package controlplane

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

type ciManagedStartupError struct {
	reason    string
	retryable bool
}

func (e *ciManagedStartupError) Error() string { return "managed CI: " + e.reason }
func (e *ciManagedStartupError) Unwrap() error { return ErrConflict }

type ciManagedRequestKey struct{}

type ciManagedHandle struct {
	config CIConfig
	ctx    context.Context
	active atomic.Bool
}

// CIManagedService contains managed startup and worker failures within a child
// lifetime. It activates at most one runtime; a returned worker error requires
// an explicit restart, never automatic replay of execution work.
type CIManagedService struct {
	mode       CIManagedMode
	initialize func(context.Context) (*CIManagedRuntime, error)
	mu         sync.Mutex
	started    bool
	state      string
	reason     string
	handle     *ciManagedHandle
	requests   sync.WaitGroup
	wait       func(context.Context, time.Duration) bool
	jitter     func(time.Duration) time.Duration
	run        func(context.Context, *CIManagedRuntime, func()) error
	logf       func(string, ...any)
}

// NewCIManagedService does not initialize dependencies or start workers.
// Call Run only after successfully binding the core HTTP listener.
func NewCIManagedService(mode CIManagedMode, initialize func(context.Context) (*CIManagedRuntime, error)) *CIManagedService {
	s := &CIManagedService{
		mode: mode, initialize: initialize, state: "starting", logf: log.Printf,
		wait: func(ctx context.Context, delay time.Duration) bool {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-timer.C:
				return true
			}
		},
		jitter: func(delay time.Duration) time.Duration {
			return delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
		},
		run: func(ctx context.Context, r *CIManagedRuntime, started func()) error {
			return r.run(ctx, started)
		},
	}
	switch mode {
	case "", CIManagedOff:
		s.state = "disabled"
	case CIManagedRun, CIManagedRecover:
		if initialize == nil {
			s.state, s.reason = "failed", "invalid_configuration"
		}
	default:
		s.state, s.reason = "failed", "invalid_configuration"
	}
	return s
}

func (s *CIManagedService) transition(state, reason string) {
	s.mu.Lock()
	changed := s.state != state || s.reason != reason
	s.state, s.reason = state, reason
	s.mu.Unlock()
	if changed {
		s.logf("component=managed_ci state=%s reason=%s", state, reason)
	}
}

// Run retries only typed transient initialization failures, with bounded probes
// and cancelable backoff. Neither initialization nor worker errors cancel the
// caller's core context. All managed requests join before Run returns.
func (s *CIManagedService) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started || ctx == nil {
		s.mu.Unlock()
		return ErrConflict
	}
	s.started = true
	state := s.state
	s.mu.Unlock()
	if state == "disabled" {
		return nil
	}
	if state == "failed" {
		s.logf("component=managed_ci state=failed reason=invalid_configuration")
		return ErrConflict
	}
	delay := time.Second
	var r *CIManagedRuntime
	for {
		if ctx.Err() != nil {
			s.transition("stopping", "shutdown_pending")
			return ctx.Err()
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		candidate, err := s.initialize(probeCtx)
		cancel()
		if ctx.Err() != nil {
			s.transition("stopping", "shutdown_pending")
			return ctx.Err()
		}
		if err == nil && candidate != nil && candidate.mode == s.mode {
			r = candidate
			break
		}
		var startup *ciManagedStartupError
		if !errors.As(err, &startup) {
			s.transition("failed", "invalid_configuration")
			return ErrConflict
		}
		if !startup.retryable {
			s.transition("failed", startup.reason)
			return startup
		}
		s.transition("waiting_dependency", startup.reason)
		if !s.wait(ctx, s.jitter(delay)) {
			s.transition("stopping", "shutdown_pending")
			return ctx.Err()
		}
		delay = min(2*delay, 30*time.Second)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	handle := &ciManagedHandle{config: r.CIConfig(CIConfig{}), ctx: workerCtx}
	if s.mode == CIManagedRun && (!ciAdmissionConfigValid(handle.config.Admission) ||
		handle.config.Bootstrap == nil || !ciProvisioningDigest.MatchString(handle.config.Bootstrap.ProfileDigest)) {
		s.transition("failed", "invalid_configuration")
		return ErrConflict
	}
	defer func() {
		// Revoke before Wait: no request may Add once teardown has begun.
		s.mu.Lock()
		handle.active.Store(false)
		s.handle = nil
		s.mu.Unlock()
		cancel()
		s.requests.Wait()
		if handle.config.Admission != nil {
			clear(handle.config.Admission.SigningKey)
			clear(handle.config.Admission.EnforcerToken)
		}
	}()
	_ = s.run(workerCtx, r, func() {
		s.mu.Lock()
		if workerCtx.Err() == nil {
			handle.active.Store(true)
			s.handle = handle
		}
		s.mu.Unlock()
		s.transition("running", "")
	})
	if ctx.Err() != nil {
		s.transition("stopping", "shutdown_pending")
		return ctx.Err()
	}
	s.transition("failed", "worker_stopped")
	// A raw worker error may contain provider or request material.
	return &ciManagedStartupError{reason: "worker_stopped"}
}

func (s *CIManagedService) acquire(ctx context.Context) (context.Context, func(), bool) {
	s.mu.Lock()
	h := s.handle
	if s.state != "running" || h == nil || !h.active.Load() ||
		h.ctx.Err() != nil || !h.config.Managed.running.Load() {
		s.mu.Unlock()
		return nil, nil, false
	}
	s.requests.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	return context.WithValue(ctx, ciManagedRequestKey{}, h), func() {
		stop()
		cancel()
		s.requests.Done()
	}, true
}

func (s *CIManagedService) pool(ctx context.Context) CIPool {
	s.mu.Lock()
	state, reason, h := s.state, s.reason, s.handle
	s.mu.Unlock()
	if state == "disabled" {
		return ciPoolOffline
	}
	if state == "running" && h != nil && h.active.Load() && h.ctx.Err() == nil {
		return h.config.Managed.Pool(ctx)
	}
	message := "Managed CI is unavailable; new execution is paused and existing runner deadlines are unchanged."
	switch reason {
	case "enforcer_unavailable":
		message = "Managed CI is waiting for its independent enforcer."
	case "enforcer_app_mismatch":
		message = "Managed CI requires matching GitHub App configuration on its independent enforcer."
	case "profile_invalid":
		message = "The managed runner profile is invalid; an operator must correct it and restart managed CI."
	case "invalid_configuration":
		message = "Managed CI configuration is invalid; an operator must correct it and restart managed CI."
	}
	return CIPool{Status: state, Message: message}
}

func (h *HTTPServer) ciExecutionConfig(ctx context.Context) CIConfig {
	if handle, ok := ctx.Value(ciManagedRequestKey{}).(*ciManagedHandle); ok {
		return handle.config
	}
	if h.config.CI.ManagedService != nil {
		// Never fall back to direct admission configuration when a service
		// owns execution. HTTP requests must first acquire its live handle.
		return CIConfig{}
	}
	return h.config.CI
}

func (h *HTTPServer) ciExecutionActive(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if h.config.CI.ManagedService == nil {
		return true
	}
	handle, ok := ctx.Value(ciManagedRequestKey{}).(*ciManagedHandle)
	return ok && handle.active.Load() && handle.ctx.Err() == nil && handle.config.Managed.running.Load()
}
