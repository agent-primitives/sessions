package computestate

import (
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

func TestEvent(t *testing.T) {
	lifecycle := func(kind api.LifecycleKind) api.Event {
		return api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: kind}}
	}
	for _, tc := range []struct {
		name  string
		ev    api.Event
		want  api.ComputeState
		moves bool
	}{
		{"suspend", lifecycle(api.LifecycleSuspend), api.ComputeCold, true},
		{"fork", lifecycle(api.LifecycleFork), api.ComputeCold, true},
		{"resume", lifecycle(api.LifecycleResume), api.ComputeLive, true},
		{"baseline", lifecycle(api.LifecycleBaseline), api.ComputeNone, false},
		{"cancel", lifecycle(api.LifecycleCancel), api.ComputeNone, false},
		{"delete", lifecycle(api.LifecycleDelete), api.ComputeNone, false},
		{"lifecycle without payload", api.Event{Kind: api.EventLifecycle}, api.ComputeNone, false},
		{"approval request", api.Event{Kind: api.EventApprovalRequest}, api.ComputeNone, false},
		{"approval result", api.Event{Kind: api.EventApprovalResult}, api.ComputeNone, false},
		{"execution start", api.Event{Kind: api.EventExecutionStart}, api.ComputeLive, true},
		{"output", api.Event{Kind: api.EventOutput}, api.ComputeLive, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, moves := Event(tc.ev)
			if got != tc.want || moves != tc.moves {
				t.Fatalf("Event = (%q, %v), want (%q, %v)", got, moves, tc.want, tc.moves)
			}
		})
	}
}

func TestOf(t *testing.T) {
	rec := func(ev api.Event) eventlog.Record { return eventlog.Record{Event: ev} }
	start := rec(api.Event{Kind: api.EventExecutionStart})
	lifecycle := func(kind api.LifecycleKind) eventlog.Record {
		return rec(api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: kind}})
	}
	suspend, deleted := lifecycle(api.LifecycleSuspend), lifecycle(api.LifecycleDelete)
	approval := rec(api.Event{Kind: api.EventApprovalResult})
	for _, tc := range []struct {
		name string
		recs []eventlog.Record
		want api.ComputeState
	}{
		{"empty", nil, api.ComputeNone},
		{"turn", []eventlog.Record{start}, api.ComputeLive},
		{"suspended", []eventlog.Record{start, suspend}, api.ComputeCold},
		{"approval after suspend", []eventlog.Record{start, suspend, approval}, api.ComputeCold},
		{"delete keeps the prior state", []eventlog.Record{start, deleted}, api.ComputeLive},
		{"approval only", []eventlog.Record{approval}, api.ComputeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Of(tc.recs); got != tc.want {
				t.Fatalf("Of = %q, want %q", got, tc.want)
			}
		})
	}
}
