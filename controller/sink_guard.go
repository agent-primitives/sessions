package controller

import (
	"context"
	"errors"

	"github.com/aramase/agentsessions/internal/ctxlock"
)

// ErrSinkClosed rejects effects from a retained sink after its Run has returned.
var ErrSinkClosed = errors.New("controller: invocation sink is closed")

// sinkGuard serializes one invocation's effects, including its resume/live tail. Private effect
// helpers run under this guard; they must not reacquire it. A stop is sticky even if Run handles it.
type sinkGuard struct {
	mu     ctxlock.Mutex
	closed bool
	stop   error
}

func (g *sinkGuard) err() error {
	if g.stop != nil {
		return g.stop
	}
	if g.closed {
		return ErrSinkClosed
	}
	return nil
}

func (g *sinkGuard) close() error {
	_ = g.mu.Lock(context.Background()) // Run closure must drain the active effect.
	defer g.mu.Unlock()
	g.closed = true
	return g.stop
}
