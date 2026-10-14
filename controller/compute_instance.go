package controller

import (
	"fmt"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/eventlog"
)

// WithComputeInstance binds the controller to the compute instance the caller placed the session on
// (api.Incarnation.ComputeInstance). Exec records it in the turn's EXECUTION_START. Exec and Resume
// first compare it with the latest instance the journal recorded for the same harness since the
// session's FORK marker, and refuse with api.ErrMemoryStateLost when the two differ, before they
// append anything or run the harness.
//
// sameHarness reports whether a turn recorded under a harness name ran on the harness this controller
// drives (the Placer resolves names through its registry); nil treats every recorded name as this
// harness. An empty instance records nothing and skips the comparison.
//
// Exec compares against the journal it then appends to under its compare-and-swap, so a turn another
// host journaled before this one cannot be missed: either it is in the history the comparison read,
// or the append fails.
func WithComputeInstance(instance string, sameHarness func(recorded string) bool) Option {
	return func(c *Controller) {
		c.computeInstance = instance
		c.sameHarness = sameHarness
	}
}

// checkComputeInstance returns an error wrapping api.ErrMemoryStateLost when recs record, for this
// controller's harness and after the last FORK marker, a latest compute instance other than
// c.computeInstance. Turns that recorded no instance are skipped. A forked child's own compute is a
// clone, not the parent's instance, so instances its copied prefix recorded do not count.
func (c *Controller) checkComputeInstance(recs []eventlog.Record) error {
	if c.computeInstance == "" {
		return nil
	}
	var last *eventlog.Record
	for i := range recs {
		ev := recs[i].Event
		switch {
		case ev.Kind == api.EventLifecycle && ev.Lifecycle != nil && ev.Lifecycle.Kind == api.LifecycleFork:
			last = nil
		case ev.Kind == api.EventExecutionStart && ev.ExecutionStart != nil && ev.ExecutionStart.ComputeInstance != "":
			if c.sameHarness == nil || c.sameHarness(ev.ExecutionStart.Harness) {
				last = &recs[i]
			}
		}
	}
	if last == nil || last.Event.ExecutionStart.ComputeInstance == c.computeInstance {
		return nil
	}
	return fmt.Errorf("%w: session %q was placed on compute instance %q, but its turn at seq %d ran on instance %q, "+
		"which has been replaced, so its in-RAM state cannot be reconstructed; nothing was journaled",
		api.ErrMemoryStateLost, c.sessionUID, c.computeInstance, last.Seq, last.Event.ExecutionStart.ComputeInstance)
}
