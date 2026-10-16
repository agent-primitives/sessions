package placement_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/local"
	"github.com/aramase/agentsessions/sqlitelog"
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
	if !checked || wrapped.fences != 1 {
		t.Fatalf("checked=%v fences=%d, want one checked fence", checked, wrapped.fences)
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := fork(head); err != nil {
		t.Fatalf("checked Resume leaked its checkpoint mark: %v", err)
	}
}
