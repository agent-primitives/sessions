package placement_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// memoryGateHarness is placementGateHarness declaring REQUIRES_MEMORY_SNAPSHOT.
type memoryGateHarness struct{ placementGateHarness }

func (h memoryGateHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	desc, err := h.placementGateHarness.Describe(ctx)
	desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	return desc, err
}

// restoreRecorder records the refs placement restores.
type restoreRecorder struct {
	placement.Backend
	restored []api.SnapshotRef
}

func (b *restoreRecorder) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	b.restored = append(b.restored, ref)
	return b.Backend.Restore(ctx, ref)
}

func newMemoryGatePlacer(t *testing.T, har api.Harness, executed *int) (*placement.Placer, *restoreRecorder, *suspendControl) {
	t.Helper()
	desc, err := har.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctl := newSuspendControl()
	b := &restoreRecorder{Backend: substrate.New(ctl, "space", substrate.ObjectRef{Name: "gate"}, desc)}
	p := placement.New(b, nil, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return har, func() error { return nil }, nil
	}), placement.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		*executed++
		return api.ToolResult{Content: api.TextMessage("tool", "ok").Parts}, nil
	}))
	return p, b, ctl
}

// APR-16 through placement: a memory-tier park suspends at the gate's captured head, and after
// the decision Resume restores that snapshot and the restored harness continues at the gated
// call. The prefix is not re-emitted.
func TestMemoryTierApprovalSuspendsAtGateAndResumesFromIt(t *testing.T) {
	log := newSuspendStore(t).Session("s")
	runs := 0
	har := memoryGateHarness{placementGateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
		runs++
		if runs == 1 {
			if err := sink.Output(ctx, "Writing the file."); err != nil {
				return err
			}
		}
		if _, err := sink.ToolCall(ctx, placementGateCall()); err != nil {
			return err
		}
		return sink.Output(ctx, "Written.")
	}}}
	executed := 0
	p, b, _ := newMemoryGatePlacer(t, har, &executed)

	if _, err := p.Exec(t.Context(), log, "s", []api.Message{*api.TextMessage("user", "write it")}, 0); !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Exec = %v, want park", err)
	}
	state, err := controller.InspectApproval(log, 0)
	if err != nil || state == nil || state.Request == nil || state.Decision != nil {
		t.Fatalf("gate=%+v, %v", state, err)
	}
	records := suspendRecords(t, log)
	suspend := records[len(records)-1]
	if suspend.Seq != state.Request.Seq+1 || suspend.Event.Lifecycle == nil || suspend.Event.Lifecycle.Snapshot == nil || !suspend.Event.Lifecycle.Snapshot.Memory {
		t.Fatalf("park suspend=%+v, want a memory SUSPEND right after APPROVAL_REQUEST %d", suspend, state.Request.Seq)
	}
	ref := *suspend.Event.Lifecycle.Snapshot
	assertSuspendRef(t, log, ref)

	if _, err := controller.Approve(log, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: true}); err != nil {
		t.Fatal(err)
	}
	if err := p.Resume(t.Context(), log, "s"); err != nil {
		t.Fatalf("Resume = %v", err)
	}
	if len(b.restored) != 1 || b.restored[0] != ref {
		t.Fatalf("restored %+v, want the park's suspend ref %+v", b.restored, ref)
	}
	if runs != 2 || executed != 1 {
		t.Fatalf("runs=%d executed=%d, want one continuation and one host execution", runs, executed)
	}
	var tail []api.EventKind
	for _, rec := range suspendRecords(t, log)[suspend.Seq:] {
		tail = append(tail, rec.Event.Kind)
		if rec.Event.Kind == api.EventLifecycle && rec.Event.Lifecycle.Kind != api.LifecycleResume {
			t.Fatalf("unexpected lifecycle %s after the park", rec.Event.Lifecycle.Kind)
		}
	}
	// The decision, the continuation (receipt first: no prefix re-emitted), then the RESUME marker.
	want := []api.EventKind{api.EventApprovalResult, api.EventToolResult, api.EventOutput, api.EventEnd, api.EventLifecycle}
	if len(tail) != len(want) {
		t.Fatalf("after the park: %v, want %v", tail, want)
	}
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("after the park: %v, want %v", tail, want)
		}
	}
	if state, err := controller.InspectApproval(log, 0); err != nil || state == nil || state.Receipt == nil || !state.Completed {
		t.Fatalf("gate=%+v, %v, want a receipted, completed gate", state, err)
	}
}

// OPS-25 and APR-15 for a parked memory-tier gate: a fork below the head is refused without side
// effects, and a child forked at the head inherits the gate but cannot resume it, decided or not.
func TestMemoryTierApprovalParkedForkAndInheritedGate(t *testing.T) {
	for _, decided := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "decided"}[decided], func(t *testing.T) {
			executed := 0
			p, _, ctl := newMemoryGatePlacer(t, memoryGateHarness{}, &executed)
			store := newSuspendStore(t)
			parent, child := store.Session("parent"), store.Session("child")
			if _, err := p.Exec(t.Context(), parent, "parent", nil, 0); !errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("Exec = %v, want park", err)
			}
			if decided {
				state, err := controller.InspectApproval(parent, 0)
				if err != nil || state == nil {
					t.Fatalf("gate=%+v, %v", state, err)
				}
				if _, err := controller.Approve(parent, api.ApprovalDecision{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq, Approved: true}); err != nil {
					t.Fatal(err)
				}
			}
			head, err := parent.Head()
			if err != nil {
				t.Fatal(err)
			}
			calls := len(ctl.calls)
			if err := p.Fork(t.Context(), parent, "parent", []placement.ForkChild{{UID: "child", Log: child}}, head-1); !errors.Is(err, placement.ErrUnplaceable) {
				t.Fatalf("fork below head = %v, want ErrUnplaceable", err)
			}
			if len(ctl.calls) != calls {
				t.Fatal("refused fork touched compute")
			}
			if err := p.Fork(t.Context(), parent, "parent", []placement.ForkChild{{UID: "child", Log: child}}, head); err != nil {
				t.Fatalf("fork at head = %v", err)
			}
			state, err := controller.InspectApproval(child, 0)
			if err != nil || state == nil || !state.Inherited || state.Receipt != nil {
				t.Fatalf("child gate=%+v, %v, want an inherited gate", state, err)
			}
			before := suspendRecords(t, child)
			calls = len(ctl.calls)
			if err := p.Resume(t.Context(), child, "child"); !errors.Is(err, controller.ErrInheritedToolIntent) {
				t.Fatalf("child Resume = %v, want ErrInheritedToolIntent", err)
			}
			if len(suspendRecords(t, child)) != len(before) || len(ctl.calls) != calls || executed != 0 {
				t.Fatalf("refused Resume changed the child: records %d->%d compute calls %d->%d executed=%d",
					len(before), len(suspendRecords(t, child)), calls, len(ctl.calls), executed)
			}
		})
	}
}
