package placement_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
)

type checkpointApprovalLog struct {
	eventlog.Store
	fences      int
	beforeFence func()
}

func (l *checkpointApprovalLog) NewFence() (int64, error) {
	l.fences++
	if l.beforeFence != nil {
		l.beforeFence()
	}
	return l.Store.NewFence()
}

func TestApproveAndStatefulForkDoNotOverlap(t *testing.T) {
	for _, phase := range []string{"snapshot", "fan-out", "decision"} {
		t.Run(phase, func(t *testing.T) {
			store := newSuspendStore(t)
			parent, child := store.Session("parent"), store.Session("child")
			seedPlacementApproval(t, parent, "override", api.EventApprovalRequest)
			backend := &routedBackend{desc: memDescriptor}
			memory := placement.New(backend, nil)
			// The decision must not resolve the recorded override to find the shared mark.
			r, err := placement.NewRegistry("memory", map[string]*placement.Placer{"memory": memory})
			if err != nil {
				t.Fatal(err)
			}
			log := &checkpointApprovalLog{Store: parent}
			decision := api.ApprovalDecision{ExecutionID: "e1", ToolCallID: "c1", RequestSeq: 3, Approved: true}
			fork := func() error {
				return memory.Fork(t.Context(), parent, "parent", []placement.ForkChild{{UID: "child", Log: child}}, 3)
			}
			called := false
			if phase == "decision" {
				log.beforeFence = func() {
					called = true
					if err := fork(); !errors.Is(err, placement.ErrCheckpointing) {
						t.Errorf("Fork during Approve=%v, want ErrCheckpointing", err)
					}
					if backend.snapshots != 0 {
						t.Error("refused Fork snapshotted compute")
					}
				}
				if _, err := r.Approve(t.Context(), log, "parent", decision); err != nil {
					t.Fatal(err)
				}
			} else {
				nested := func() {
					called = true
					before := suspendRecords(t, parent)
					_, err := r.Approve(t.Context(), log, "parent", decision)
					if !errors.Is(err, placement.ErrCheckpointing) {
						t.Errorf("Approve during Fork %s=%v, want ErrCheckpointing", phase, err)
					}
					if log.fences != 0 || !reflect.DeepEqual(before, suspendRecords(t, parent)) {
						t.Error("refused Approve minted a fence or changed the journal")
					}
				}
				if phase == "snapshot" {
					backend.onSnapshot = nested
				} else {
					backend.onFork = func(string) { nested() }
				}
				if err := fork(); err != nil {
					t.Fatalf("outer Fork=%v", err)
				}
			}
			if !called {
				t.Fatal("overlap hook did not run")
			}
			// Both the guard and checkpoint mark must be released for a later decision/retry.
			log.beforeFence = nil
			if _, err := r.Approve(t.Context(), log, "parent", decision); err != nil {
				t.Fatalf("Approve after overlap=%v", err)
			}
		})
	}
}
