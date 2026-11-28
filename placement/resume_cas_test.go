package placement_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type resumeHeadProbe struct {
	eventlog.Store
	onHead func()
}

func (s resumeHeadProbe) Head() (int64, error) {
	s.onHead()
	return s.Store.Head()
}

func TestResumeCursorCheckHoldsRegistryGuard(t *testing.T) {
	log := newSuspendStore(t).Session("s")
	seedPlacementApproval(t, log, "gate", api.EventExecutionStart)
	p := newLocalPlacer(t, placementGateHarness{run: func(context.Context, *api.Start, api.EventSink) error { return nil }})
	other := newLocalPlacer(t, placementGateHarness{})
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": p, "other": other})
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	probe := resumeHeadProbe{Store: log, onHead: func() {
		checks++
		if _, err := other.Exec(t.Context(), log, "s", nil, 1); !errors.Is(err, placement.ErrSessionBusy) {
			t.Errorf("Exec from Head = %v, want ErrSessionBusy", err)
		}
	}}
	if err := r.Resume(t.Context(), probe, "s", "gate", placement.WithResumeExpectedLastSeq(0)); !errors.Is(err, eventlog.ErrConflict) {
		t.Fatalf("stale Resume = %v, want ErrConflict", err)
	}
	if checks != 1 {
		t.Fatalf("Head checks=%d, want one entry check", checks)
	}
	if err := r.Resume(t.Context(), log, "s", "gate", placement.WithResumeExpectedLastSeq(1)); err != nil {
		t.Fatalf("retry after refusal retained the guard: %v", err)
	}
}

func TestResumeCursorRechecksAfterFenceBeforeEffects(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventExecutionStart, api.EventToolCall} {
		t.Run(string(cut), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shared.db")
			store, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			peer, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = peer.Close() })
			log, peerLog := store.Session("s"), peer.Session("s")
			seedPlacementApproval(t, log, "gate", cut)
			before := suspendRecords(t, log)
			expected, oldFence := int64(len(before)), before[len(before)-1].Fence
			runs := 0
			backend := local.New(placementGateHarness{run: func(context.Context, *api.Start, api.EventSink) error {
				runs++
				return nil
			}})
			t.Cleanup(func() { _ = backend.Close() })
			b := &approvalBackend{Backend: backend}
			r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": placement.New(b, nil)})
			if err != nil {
				t.Fatal(err)
			}
			var committed []eventlog.Record
			wrapped := &checkpointApprovalLog{Store: log, beforeFence: func() {
				// A different connection commits under the old fence after the entry check.
				ev := api.Event{Kind: api.EventExecutionStart, ExecutionID: "peer", ExecutionStart: &api.ExecutionStart{Harness: "gate", HarnessVersion: "v1", InputCount: proto.Int64(0)}}
				if _, err := peerLog.Append(expected, oldFence, ev); err != nil {
					t.Fatal(err)
				}
				committed = suspendRecords(t, peerLog)
			}}
			err = r.Resume(t.Context(), wrapped, "s", "gate", placement.WithResumeExpectedLastSeq(expected))
			values := fmt.Sprintf("expected_last_seq %d, head %d", expected, expected+1)
			if !errors.Is(err, eventlog.ErrConflict) || !strings.Contains(err.Error(), values) {
				t.Fatalf("Resume = %v, want ErrConflict with %q", err, values)
			}
			if wrapped.fences != 1 || runs != 0 || b.creates+b.restores+b.snapshots+b.stops != 0 {
				t.Fatalf("refusal effects: fences=%d runs=%d runtime=%+v", wrapped.fences, runs, b)
			}
			if !reflect.DeepEqual(committed, suspendRecords(t, log)) {
				t.Fatal("Resume changed records beyond the peer's committed write")
			}
			if _, err := peerLog.Append(expected+1, oldFence, api.Event{Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}); !errors.Is(err, eventlog.ErrFenced) {
				t.Fatalf("old writer after Resume fence = %v, want ErrFenced", err)
			}
		})
	}
}

func TestCheckedResumeFenceExcludesStatefulFork(t *testing.T) {
	store := newSuspendStore(t)
	log := store.Session("s")
	seedPlacementApproval(t, log, "gate", api.EventExecutionStart)
	backend := local.New(placementGateHarness{run: func(context.Context, *api.Start, api.EventSink) error { return nil }})
	t.Cleanup(func() { _ = backend.Close() })
	b := &approvalBackend{Backend: backend}
	memoryBackend := &routedBackend{desc: memDescriptor}
	memory := placement.New(memoryBackend, nil)
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": placement.New(b, nil), "memory": memory})
	if err != nil {
		t.Fatal(err)
	}
	fork := func(head int64) error {
		return memory.Fork(t.Context(), log, "s", []placement.ForkChild{{UID: "child", Log: store.Session("child")}}, head)
	}
	checked := false
	wrapped := &checkpointApprovalLog{Store: log, beforeFence: func() {
		if checked {
			return // the regular post-Restore mint is covered by the Restore/Fork regression
		}
		if b.restores != 0 {
			t.Error("Restore preceded the checked fence")
			return // avoid a nested checkpoint waiting on this old-style late mint
		}
		checked = true
		if err := fork(1); !errors.Is(err, placement.ErrCheckpointing) {
			t.Errorf("Fork during checked fence = %v, want ErrCheckpointing", err)
		}
		if memoryBackend.snapshots != 0 {
			t.Error("Fork snapshotted before the checked fence completed")
		}
	}}
	if err := r.Resume(t.Context(), wrapped, "s", "gate", placement.WithResumeExpectedLastSeq(1)); err != nil {
		t.Fatal(err)
	}
	if !checked || wrapped.fences != 2 {
		t.Fatalf("checked=%v fences=%d, want early and regular checked fences", checked, wrapped.fences)
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := fork(head); err != nil {
		t.Fatalf("checked Resume leaked its checkpoint mark: %v", err)
	}
}

type resumeRestoreProbe struct {
	placement.Backend
	beforeRestore func()
	restores      int
}

func (b *resumeRestoreProbe) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	b.restores++
	if b.beforeRestore != nil {
		b.beforeRestore()
	}
	return b.Backend.Restore(ctx, ref)
}

func TestCheckedResumeRestoreRefusesAnIncompleteMemoryFork(t *testing.T) {
	store := newSuspendStore(t)
	log := store.Session("s")
	seedPlacementApproval(t, log, "mem", api.EventExecutionStart)
	call := placementGateCall()
	call.Mediation = api.MediationControllerMediated
	fence := suspendRecords(t, log)[0].Fence
	if _, err := log.Append(1, fence, api.Event{Kind: api.EventToolCall, ExecutionID: "e1", ToolCall: &call}); err != nil {
		t.Fatal(err)
	}
	expected := int64(2)
	desc := memDescriptor
	desc.Version = "v1"
	var runs, effects atomic.Int32
	h := describeBoundaryHarness{Harness: placementGateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
		runs.Add(1)
		_, err := sink.ToolCall(ctx, call)
		return err
	}}, describe: func(context.Context) (api.Descriptor, error) { return desc, nil }}
	hs := startHarnessServer(t, h)
	backend := &routedBackend{address: hs.addr, desc: desc}
	b := &resumeRestoreProbe{Backend: backend}
	p := placement.New(b, nil, dedupAllTools, placement.WithToolExecutor(func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		effects.Add(1)
		return api.ToolResult{ID: call.ID}, nil
	}))
	r, err := placement.NewRegistry("mem", map[string]*placement.Placer{"mem": p})
	if err != nil {
		t.Fatal(err)
	}
	beforeFork := suspendRecords(t, log)
	var forkErr error
	b.beforeRestore = func() {
		// Restore has not opened a harness connection yet, but the durable incomplete turn is enough
		// to refuse the memory checkpoint before a fence, snapshot or child write.
		forkErr = p.Fork(t.Context(), log, "s", []placement.ForkChild{{UID: "child", Log: store.Session("child")}}, expected)
	}
	wrapped := &checkpointApprovalLog{Store: log}
	err = r.Resume(t.Context(), wrapped, "s", "mem", placement.WithResumeExpectedLastSeq(expected))
	if forkErr == nil || !errors.Is(forkErr, placement.ErrSessionBusy) {
		t.Fatalf("Fork during Restore = %v, want ErrSessionBusy", forkErr)
	}
	if err != nil {
		t.Fatalf("Resume = %v", err)
	}
	if wrapped.fences != 2 || b.restores != 1 || runs.Load() != 1 || effects.Load() != 1 || backend.snapshots != 0 {
		t.Fatalf("fences=%d restores=%d runs=%d tools=%d snapshots=%d", wrapped.fences, b.restores, runs.Load(), effects.Load(), backend.snapshots)
	}
	after := suspendRecords(t, log)
	if len(after) <= len(beforeFork) || after[len(after)-1].Event.Kind != api.EventLifecycle || after[len(after)-1].Event.Lifecycle.Kind != api.LifecycleResume {
		t.Fatalf("successful Resume records=%v", after)
	}
	child, childErr := store.Session("child").Read(1)
	if childErr != nil || len(child) != 0 {
		t.Fatalf("refused Fork child records=%v err=%v", child, childErr)
	}
	if err := log.Verify(); err != nil {
		t.Fatal(err)
	}
}
