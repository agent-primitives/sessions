// Package ctxlock provides cancellation-aware serialization for invocation sinks.
package ctxlock

import (
	"context"
	"sync"
)

// Mutex is a zero-value-ready mutex. It must not be copied after first use.
// Callers must check ctx.Err after checking terminal state and before starting effects.
type Mutex struct {
	once sync.Once
	held chan struct{}
}

// Lock acquires the mutex or returns the contended caller's context error.
// An uncontended acquisition succeeds even if ctx is canceled, so the caller
// can preserve terminal-error precedence while holding the mutex.
func (m *Mutex) Lock(ctx context.Context) error {
	m.once.Do(func() { m.held = make(chan struct{}, 1) })
	select {
	case m.held <- struct{}{}:
		return nil
	default:
	}
	select {
	case m.held <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases a held mutex. It panics if the mutex is not locked.
func (m *Mutex) Unlock() {
	select {
	case <-m.held:
	default:
		panic("ctxlock: unlock of unlocked mutex")
	}
}
