package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/placement"
)

const cleanupAttemptTimeout = 30 * time.Second

// ReapOnce attempts cleanup of all currently PENDING sessions, in keyset pages. It neither
// reconstructs a harness nor infers resource ownership from the event log. Independent owned
// allocations make progress even when another fails; durable acknowledgements survive restart.
// ErrCleanupPending from sqlitelog and placement.ErrCleanupUnavailable leave diagnostic PENDING.
// Callers may retry any returned error; successful release must be idempotent.
func (s *Service) ReapOnce(ctx context.Context) error {
	var failures cleanupFailures
	var after string
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures.err(), err)
		}
		uids, err := s.store.DeletionCandidates(ctx, after, 100)
		if err != nil {
			return errors.Join(failures.err(), err)
		}
		if len(uids) == 0 {
			return failures.err()
		}
		for _, uid := range uids {
			if err := ctx.Err(); err != nil {
				return errors.Join(failures.err(), err)
			}
			attemptCtx, cancel := context.WithTimeout(ctx, cleanupAttemptTimeout)
			err := s.registry.WithSessionGuard(uid, func() error {
				err := s.reapSession(attemptCtx, uid)
				if err != nil {
					// Keep exclusion through diagnostics. The worker context survives an attempt
					// timeout; no late failure may overwrite a successful retry.
					err = errors.Join(err, s.store.RecordCleanupError(ctx, uid, err.Error()))
				}
				return err
			})
			cancel()
			if errors.Is(err, placement.ErrSessionBusy) {
				continue
			}
			if err != nil {
				failures.add(fmt.Errorf("cleanup session %q: %w", uid, err))
			}
		}
		after = uids[len(uids)-1]
	}
}

func (s *Service) reapSession(ctx context.Context, uid string) error {
	info, err := s.store.SessionInfo(uid)
	if err != nil {
		return err
	}
	if info.DeletionState != api.DeletionPending {
		return nil
	}
	var failures cleanupFailures
	var after string
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures.err(), err)
		}
		allocations, err := s.store.AllocationPage(ctx, uid, after, 100)
		if err != nil {
			return errors.Join(failures.err(), err)
		}
		if len(allocations) == 0 {
			break
		}
		for _, allocation := range allocations {
			if err := ctx.Err(); err != nil {
				return errors.Join(failures.err(), err)
			}
			// Only no-dedicated-resource backends opt in. Their detach is safe even when
			// acknowledgement was lost; identity-bearing resource cleanup is unsupported.
			if err := s.registry.ReleaseSession(ctx, allocation.Backend, uid); err != nil {
				failures.add(err)
				continue
			}
			if err := s.store.ReleaseAllocation(ctx, uid, allocation.ID); err != nil {
				failures.add(err)
			}
		}
		after = allocations[len(allocations)-1].ID
	}
	if err := failures.err(); err != nil {
		return err
	}
	return s.store.CompleteDeletion(ctx, uid)
}

// Retain one representative cause and a count, not an error tree proportional to all pending
// sessions/attempts. Context cancellation is joined explicitly by callers so it stays detectable.
type cleanupFailures struct {
	count int
	first error
}

func (f *cleanupFailures) add(err error) {
	if err == nil {
		return
	}
	f.count++
	if f.first == nil {
		f.first = err
	}
}

func (f cleanupFailures) err() error {
	if f.count == 0 {
		return nil
	}
	return f
}

func (f cleanupFailures) Error() string {
	message := f.first.Error()
	if len(message) > 1024 {
		message = message[:1024]
	}
	return fmt.Sprintf("%d cleanup failure(s); first: %s", f.count, message)
}

func (f cleanupFailures) Unwrap() error { return f.first }

// RunReaper recovers unfinished fan-outs before its first cleanup pass, then retries at interval.
// It must start before this single host admits requests: recovery cannot distinguish another
// live host's PREPARING fan-out from a crashed one. Context cancellation stops and joins all work
// before returning nil. Errors during a pass are logged and retried, not forgotten or marked done.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration) error {
	return s.runReaper(ctx, interval, nil)
}

// StartReaper waits for startup recovery and returns an idempotent cancel-and-join function.
// Call it before serving requests; call stop before closing the store or runtime backends.
// No goroutine is started implicitly by NewService, including for embedded hosts.
func (s *Service) StartReaper(ctx context.Context, interval time.Duration) (stop func(), err error) {
	workerCtx, cancel := context.WithCancel(ctx)
	ready := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.runReaper(workerCtx, interval, ready)
	}()
	if err := <-ready; err != nil {
		cancel()
		<-done
		return nil, err
	}
	return sync.OnceFunc(func() { cancel(); <-done }), nil
}

func (s *Service) runReaper(ctx context.Context, interval time.Duration, ready chan<- error) error {
	notify := func(err error) {
		if ready != nil {
			ready <- err
		}
	}
	if interval <= 0 {
		err := errors.New("session: reaper interval must be positive")
		notify(err)
		return err
	}
	if err := s.store.RecoverForks(ctx); err != nil {
		notify(err)
		return err
	}
	notify(nil)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.ReapOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.WarnContext(ctx, "session cleanup remains pending", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
