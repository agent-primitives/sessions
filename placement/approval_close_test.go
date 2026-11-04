package placement_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

var closeRef = api.ApprovalRef{ExecutionID: "e1", ToolCallID: "c1", RequestSeq: 3}

// CloseApproval takes the shared session guard: during a live Resume it is refused with
// ErrSessionBusy before it reads or writes, and the guard is released for a later close.
func TestCloseApprovalSharesSessionGuard(t *testing.T) {
	log := newSuspendStore(t).Session("s")
	seedPlacementApproval(t, log, "gate", api.EventToolCall)
	runtime := local.New(placementGateHarness{})
	t.Cleanup(func() { _ = runtime.Close() })
	b := &approvalBackend{Backend: runtime}
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": placement.New(b, nil)})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	wrapped := &approvalCASLog{Store: log}
	wrapped.hook = func() {
		called = true
		before := suspendRecords(t, log)
		if _, _, err := r.CloseApproval(t.Context(), log, "s", closeRef, api.CallerIdentity{}); !errors.Is(err, placement.ErrSessionBusy) {
			t.Errorf("CloseApproval during Resume = %v, want ErrSessionBusy", err)
		}
		if !reflect.DeepEqual(before, suspendRecords(t, log)) {
			t.Error("refused CloseApproval changed the journal")
		}
	}
	// Resume repairs the call-only cut into an undecided gate and parks.
	if err := r.Resume(t.Context(), wrapped, "s", "gate"); !errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("Resume = %v, want a park", err)
	}
	if !called {
		t.Fatal("overlap hook did not run")
	}
	decision, receipt, err := r.CloseApproval(t.Context(), log, "s", closeRef, api.CallerIdentity{})
	if err != nil || decision.Event.ApprovalResult.Approved || receipt.Event.Result.Code != api.ToolResultCodeNotExecuted {
		t.Fatalf("CloseApproval after Resume = %+v, %+v, %v", decision.Event, receipt.Event, err)
	}
	b.assertNoCompute(t)
}

// CloseApproval and a stateful Fork exclude each other through the checkpoint mark, in both
// orders, and a refused close mints no fence and writes nothing.
func TestCloseApprovalAndStatefulForkDoNotOverlap(t *testing.T) {
	for _, phase := range []string{"snapshot", "fan-out", "close"} {
		t.Run(phase, func(t *testing.T) {
			store := newSuspendStore(t)
			parent, child := store.Session("parent"), store.Session("child")
			seedPlacementApproval(t, parent, "override", api.EventApprovalRequest)
			backend := &routedBackend{desc: memDescriptor}
			memory := placement.New(backend, nil)
			r, err := placement.NewRegistry("memory", map[string]*placement.Placer{"memory": memory})
			if err != nil {
				t.Fatal(err)
			}
			log := &checkpointApprovalLog{Store: parent}
			fork := func() error {
				return memory.Fork(t.Context(), parent, "parent", []placement.ForkChild{{UID: "child", Log: child}}, 3)
			}
			called := false
			if phase == "close" {
				log.beforeFence = func() {
					called = true
					if err := fork(); !errors.Is(err, placement.ErrCheckpointing) {
						t.Errorf("Fork during CloseApproval = %v, want ErrCheckpointing", err)
					}
					if backend.snapshots != 0 {
						t.Error("refused Fork snapshotted compute")
					}
				}
				if _, _, err := r.CloseApproval(t.Context(), log, "parent", closeRef, api.CallerIdentity{}); err != nil {
					t.Fatal(err)
				}
			} else {
				nested := func() {
					called = true
					before := suspendRecords(t, parent)
					if _, _, err := r.CloseApproval(t.Context(), log, "parent", closeRef, api.CallerIdentity{}); !errors.Is(err, placement.ErrCheckpointing) {
						t.Errorf("CloseApproval during Fork %s = %v, want ErrCheckpointing", phase, err)
					}
					if log.fences != 0 || !reflect.DeepEqual(before, suspendRecords(t, parent)) {
						t.Error("refused CloseApproval minted a fence or changed the journal")
					}
				}
				if phase == "snapshot" {
					backend.onSnapshot = nested
				} else {
					backend.onFork = func(string) { nested() }
				}
				if err := fork(); err != nil {
					t.Fatalf("outer Fork = %v", err)
				}
			}
			if !called {
				t.Fatal("overlap hook did not run")
			}
			// The guard and checkpoint mark are released for a later close or retry.
			log.beforeFence = nil
			if _, receipt, err := r.CloseApproval(t.Context(), log, "parent", closeRef, api.CallerIdentity{}); err != nil || receipt.Event.Result.Code != api.ToolResultCodeNotExecuted {
				t.Fatalf("CloseApproval after overlap = %+v, %v", receipt.Event, err)
			}
		})
	}
}
