package computestate

import (
	"testing"

	"github.com/agent-primitives/sessions/api"
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
