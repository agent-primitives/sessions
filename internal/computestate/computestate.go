// Package computestate is the SES-8 compute-state projection: what a session's committed events
// say about whether its compute is running. sqlitelog stores it per session as each event commits.
package computestate

import (
	"github.com/agent-primitives/sessions/api"
)

// Event maps a committed event to the compute state it implies, reporting false for
// events that say nothing about the incarnation.
//
// Except for host-only approval request/decision writes, a non-lifecycle event is evidence that
// compute was running: a live incarnation produces a turn's input and output records. Host repair
// and decision writes may happen while compute is cold. This is load-bearing rather than incidental.
// Placer.Exec appends no lifecycle marker of its own — RESUME is written only by Placer.Resume — so
// a session that has only ever been Exec'd, which is every session before its first suspend, would
// otherwise sit at NONE forever and never look live to a caller.
//
// FORK lands the child at COLD. controller.Fork copies the parent's prefix through child.Append, so
// without this the copied records would leave a child looking live when nothing is running for it;
// the FORK marker is always the child's last event, so it settles the projection. COLD is also the
// truthful state: a child begins from its parent's fork-point snapshot with no compute of its own,
// and is placed on its first Exec.
func Event(ev api.Event) (api.ComputeState, bool) {
	if ev.Kind == api.EventApprovalRequest || ev.Kind == api.EventApprovalResult {
		return api.ComputeNone, false
	}
	if ev.Kind != api.EventLifecycle {
		return api.ComputeLive, true
	}
	if ev.Lifecycle == nil {
		return api.ComputeNone, false
	}
	switch ev.Lifecycle.Kind {
	case api.LifecycleSuspend, api.LifecycleFork:
		return api.ComputeCold, true
	case api.LifecycleResume:
		return api.ComputeLive, true
	default:
		return api.ComputeNone, false
	}
}
