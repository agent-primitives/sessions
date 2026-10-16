package placement_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

type placementGateHarness struct {
	run func(context.Context, *api.Start, api.EventSink) error
}

func (placementGateHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "gate", Version: "v1", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}
func (h placementGateHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if h.run != nil {
		return h.run(ctx, start, sink)
	}
	_, err := sink.ToolCall(ctx, placementGateCall())
	return err
}
func placementGateCall() api.ToolCall {
	return api.ToolCall{ID: "c1", Tool: "write", Args: map[string]any{"path": "file"}, IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}
}

// The runtime remains real; count only the placement-to-runtime boundary and inject operational
// failures there. Snapshot still updates the actor recorder used by ordinary lifecycle tests.
type approvalBackend struct {
	placement.Backend
	describes, creates, restores, snapshots, stops int
	beforeSnapshot                                 func(context.Context)
}

func (b *approvalBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	b.describes++
	return b.Backend.Describe(ctx)
}
func (b *approvalBackend) Create(ctx context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	b.creates++
	return b.Backend.Create(ctx, spec)
}
func (b *approvalBackend) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	b.restores++
	return b.Backend.Restore(ctx, ref)
}
func (b *approvalBackend) Snapshot(ctx context.Context, inc api.Incarnation, kind api.SnapshotKind) (api.SnapshotRef, error) {
	b.snapshots++
	if b.beforeSnapshot != nil {
		b.beforeSnapshot(ctx)
	}
	return b.Backend.Snapshot(ctx, inc, kind)
}
func (b *approvalBackend) Stop(ctx context.Context, inc api.Incarnation) error {
	b.stops++
	return b.Backend.Stop(ctx, inc)
}
func (b *approvalBackend) assertNoCompute(t *testing.T) {
	t.Helper()
	if b.describes+b.creates+b.restores+b.snapshots+b.stops != 0 {
		t.Fatalf("unexpected runtime IO: describe=%d create=%d restore=%d snapshot=%d stop=%d", b.describes, b.creates, b.restores, b.snapshots, b.stops)
	}
}

func seedPlacementApproval(t *testing.T, log eventlog.Store, harness string, cut api.EventKind) {
	t.Helper()
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	call := placementGateCall()
	events := []api.Event{
		{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: harness, HarnessVersion: "v1", InputCount: proto.Int64(0)}},
		{Kind: api.EventToolCall, ToolCall: &call},
		{Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "c1"}},
		{Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "c1", RequestSeq: 3, Approved: true}},
	}
	for i, ev := range events {
		ev.ExecutionID = "e1"
		if _, err := log.Append(int64(i), fence, ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == cut {
			return
		}
	}
	t.Fatalf("unknown cut %s", cut)
}
func placementApprovalRecord(t *testing.T, log eventlog.Store, kind api.EventKind) eventlog.Record {
	t.Helper()
	for _, rec := range suspendRecords(t, log) {
		if rec.Event.Kind == kind {
			return rec
		}
	}
	t.Fatalf("missing %s", kind)
	return eventlog.Record{}
}

type approvalFaultLog struct {
	eventlog.Store
	failKind      api.EventKind
	failSuspend   bool
	err           error
	fences        int
	beforeSuspend func()
}

func (l *approvalFaultLog) NewFence() (int64, error) { l.fences++; return l.Store.NewFence() }
func (l *approvalFaultLog) Append(head, fence int64, ev api.Event) (eventlog.Record, error) {
	if l.beforeSuspend != nil && ev.Kind == api.EventLifecycle && ev.Lifecycle.Kind == api.LifecycleSuspend {
		l.beforeSuspend()
	}
	if ev.Kind == l.failKind || l.failSuspend && ev.Kind == api.EventLifecycle && ev.Lifecycle.Kind == api.LifecycleSuspend {
		return eventlog.Record{}, l.err
	}
	return l.Store.Append(head, fence, ev)
}

func TestApprovalActivationBoundaries(t *testing.T) {
	t.Run("wire park same fence and observer", func(t *testing.T) {
		log := newSuspendStore(t).Session("s")
		runtime := local.New(placementGateHarness{})
		t.Cleanup(func() { _ = runtime.Close() })
		b := &approvalBackend{Backend: runtime}
		p := placement.New(b, nil)
		var observed []eventlog.Record
		inc, err := p.Exec(t.Context(), log, "s", nil, 0, placement.WithObserver(controller.Observer{OnRecord: func(r eventlog.Record) { observed = append(observed, r) }}))
		var park *api.ApprovalParkedError
		if !errors.As(err, &park) || park.Ref.RequestSeq != 3 {
			t.Fatalf("park=%v", err)
		}
		assertSuspendRef(t, log, api.SnapshotRef{Local: "s"})
		if b.snapshots != 1 || b.stops != 0 || !reflect.DeepEqual(observed, suspendRecords(t, log)) {
			t.Fatalf("handoff snapshot=%d stop=%d observed=%v", b.snapshots, b.stops, observed)
		}
		for _, r := range observed {
			if r.Fence != inc.FenceToken || r.Event.Kind == api.EventEnd || r.Event.Kind == api.EventError || r.Event.Kind == api.EventParked {
				t.Fatalf("bad handoff record=%+v", r)
			}
		}
	})
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest, api.EventApprovalResult} {
		t.Run(string(cut), func(t *testing.T) {
			log := newSuspendStore(t).Session("s")
			seedPlacementApproval(t, log, "recorded", cut)
			runtime := local.New(placementGateHarness{})
			t.Cleanup(func() { _ = runtime.Close() })
			b := &approvalBackend{Backend: runtime}
			p := placement.New(b, nil)
			r, err := placement.NewRegistry("recorded", map[string]*placement.Placer{"recorded": p})
			if err != nil {
				t.Fatal(err)
			}
			before := suspendRecords(t, log)
			for _, inputs := range [][]api.Message{nil, {*api.TextMessage("user", "new")}} {
				if _, err := p.Exec(t.Context(), log, "s", inputs, int64(len(before))); !errors.Is(err, controller.ErrApprovalBlocked) {
					t.Fatalf("Exec=%v", err)
				}
			}
			b.assertNoCompute(t)
			if !reflect.DeepEqual(before, suspendRecords(t, log)) {
				t.Fatal("blocked admission wrote")
			}
			if cut == api.EventApprovalResult {
				return
			}
			var observed []eventlog.Record
			err = r.Resume(t.Context(), log, "s", "wrong", placement.WithRegistryResumeObserver(controller.Observer{OnRecord: func(r eventlog.Record) { observed = append(observed, r) }}))
			if !errors.Is(err, api.ErrApprovalParked) {
				t.Fatal(err)
			}
			b.assertNoCompute(t)
			if cut == api.EventToolCall && (len(observed) != 1 || observed[0].Seq != 3) {
				t.Fatalf("repair observer=%v", observed)
			}
			before = suspendRecords(t, log)
			if err = r.Resume(t.Context(), log, "s", "wrong"); !errors.Is(err, api.ErrApprovalParked) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, suspendRecords(t, log)) {
				t.Fatal("query repaired twice")
			}
			if _, err = r.Approve(t.Context(), log, "s", api.ApprovalDecision{ExecutionID: "e1", ToolCallID: "c1", RequestSeq: 3}); err != nil {
				t.Fatal(err)
			}
			b.assertNoCompute(t)
		})
	}
}

func TestPlacementApprovalHandoffPhases(t *testing.T) {
	for _, mode := range []string{"success", "already parked", "request", "close", "snapshot", "append", "CAS", "fenced", "transport", "mismatched park"} {
		t.Run(mode, func(t *testing.T) {
			cause := errors.New(mode + " failed")
			ctl := newSuspendControl()
			sqlLog := newSuspendStore(t).Session("s")
			log := &approvalFaultLog{Store: sqlLog}
			har := placementGateHarness{}
			want := cause
			if mode == "request" {
				log.failKind, log.err = api.EventApprovalRequest, cause
			}
			if mode == "append" {
				log.failSuspend, log.err = true, cause
			}
			if mode == "snapshot" {
				ctl.snapshotErr = cause
			}
			if mode == "transport" || mode == "mismatched park" {
				har.run = func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
					_, _ = sink.ToolCall(ctx, placementGateCall())
					if mode == "mismatched park" {
						return &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "wrong", ToolCallID: "c1", RequestSeq: 3}}
					}
					return cause
				}
				if mode == "mismatched park" {
					want = controller.ErrReplayDiverged
				}
			}
			desc, _ := har.Describe(t.Context())
			b := &approvalBackend{Backend: substrate.New(ctl, "space", substrate.ObjectRef{Name: "gate"}, desc)}
			var pendingCheckpoint *placement.Placer
			closes := 0
			if mode == "success" || mode == "already parked" {
				checkpointBackend := &routedBackend{desc: memDescriptor}
				checkpoint := placement.New(checkpointBackend, nil)
				b.beforeSnapshot = func(ctx context.Context) {
					if err := ctx.Err(); err != nil {
						t.Fatalf("closing invocation canceled snapshot context: %v", err)
					}
					if closes != 1 {
						t.Fatalf("snapshot before exactly one close: %d", closes)
					}
					if err := forkChildren(t, checkpoint, sqlLog, "s", 1); !errors.Is(err, placement.ErrCheckpointing) {
						t.Fatalf("fork during snapshot=%v", err)
					}
					if checkpointBackend.snapshots != 0 {
						t.Fatal("refused fork touched compute")
					}
				}
				// Registry links the checkpoint guard across placement routes.
				pendingCheckpoint = checkpoint
			}
			p := placement.New(b, nil, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
				return har, func() error {
					closes++
					if mode == "close" {
						return cause
					}
					return nil
				}, nil
			}))
			if mode == "CAS" || mode == "fenced" {
				ctl.afterSuspend = func() {
					if mode == "fenced" {
						if _, err := log.Store.NewFence(); err != nil {
							t.Fatal(err)
						}
						return
					}
					request := placementApprovalRecord(t, log, api.EventApprovalRequest)
					if _, err := log.Store.Append(request.Seq, request.Fence, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleCancel}}); err != nil {
						t.Fatal(err)
					}
				}
				want = eventlog.ErrConflict
				if mode == "fenced" {
					want = eventlog.ErrFenced
				}
			}
			if pendingCheckpoint != nil {
				if _, err := placement.NewRegistry("checkpoint", map[string]*placement.Placer{"checkpoint": pendingCheckpoint, "turn": p}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := p.Exec(t.Context(), log, "s", nil, 0)
			if mode == "success" || mode == "already parked" {
				if !errors.Is(err, api.ErrApprovalParked) || closes != 1 || b.snapshots != 1 || b.stops != 0 || log.fences != 1 {
					t.Fatalf("park=%v close=%d snapshot=%d stop=%d fences=%d", err, closes, b.snapshots, b.stops, log.fences)
				}
				assertSuspendRef(t, log, api.SnapshotRef{Local: "s", ExternalURI: "snap-s-1", Memory: true})
				actor := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "s"}]
				if actor == nil || actor.worker || actor.status != substrate.StatusSuspended {
					t.Fatalf("actor not retained cold: %+v", actor)
				}
				if mode == "already parked" {
					before := suspendRecords(t, log)
					head, err := log.Head()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := p.Exec(t.Context(), log, "s", nil, head); err != controller.ErrApprovalBlocked {
						t.Fatalf("already parked Exec=%v", err)
					}
					afterHead, err := log.Head()
					if err != nil || afterHead != head || b.snapshots != 1 || log.fences != 1 || closes != 1 || !reflect.DeepEqual(before, suspendRecords(t, log)) {
						t.Fatalf("second Exec mutated handoff: head=%d err=%v snapshot=%d fences=%d close=%d", afterHead, err, b.snapshots, log.fences, closes)
					}
				}
				return
			}
			if !errors.Is(err, want) || errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("handoff=%v, want operational %v only", err, want)
			}
			if closes != 1 || b.stops != 0 || log.fences != 1 {
				t.Fatalf("close=%d stop=%d fences=%d", closes, b.stops, log.fences)
			}
			state, err := controller.InspectApproval(log, 0)
			if err != nil || state == nil || state.Receipt != nil || state.Decision != nil {
				t.Fatalf("lost awaiting evidence: %+v, %v", state, err)
			}
			if (state.Request == nil) != (mode == "request") {
				t.Fatalf("request=%+v", state.Request)
			}
			for _, rec := range suspendRecords(t, log) {
				if rec.Event.Kind == api.EventLifecycle && rec.Event.Lifecycle.Kind == api.LifecycleSuspend || rec.Event.Kind == api.EventEnd || rec.Event.Kind == api.EventError {
					t.Fatalf("false handoff record: %+v", rec)
				}
			}
			wantSnapshots := 1
			if mode == "request" || mode == "close" || mode == "transport" || mode == "mismatched park" {
				wantSnapshots = 0
			}
			if b.snapshots != wantSnapshots {
				t.Fatalf("snapshot=%d want=%d", b.snapshots, wantSnapshots)
			}
			// An operational failure releases the local guard. A public Suspend retry keeps the
			// retained actor; no invocation fence refresh is allowed during the automatic attempt.
			ctl.snapshotErr, log.failSuspend, log.failKind = nil, false, ""
			if _, err := p.Suspend(t.Context(), log, "s"); err != nil {
				t.Fatalf("retry Suspend=%v", err)
			}
		})
	}
}

// An inherited override call does not own the child's cloned memory-tier actor.
func TestInheritedApprovalSuspendUsesSessionHarness(t *testing.T) {
	for _, decided := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "decided"}[decided], func(t *testing.T) {
			ctl := newSuspendControl()
			memory, backend := newSuspendPlacer(t, ctl, true)
			runtime := local.New(placementGateHarness{})
			t.Cleanup(func() { _ = runtime.Close() })
			overrideBackend := &approvalBackend{Backend: runtime}
			override := placement.New(overrideBackend, nil)
			r, err := placement.NewRegistry("memory", map[string]*placement.Placer{"memory": memory, "override": override})
			if err != nil {
				t.Fatal(err)
			}
			store := newSuspendStore(t)
			parent, child := store.Session("parent"), store.Session("child")
			execSuspendTurn(t, memory, parent, "parent", "first")
			head, err := parent.Head()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := override.Exec(t.Context(), parent, "parent", nil, head, placement.WithHarness("override")); !errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("override Exec=%v", err)
			}
			if decided {
				request := placementApprovalRecord(t, parent, api.EventApprovalRequest)
				if _, err := r.Approve(t.Context(), parent, "parent", api.ApprovalDecision{ExecutionID: request.Event.ExecutionID, ToolCallID: "c1", RequestSeq: request.Seq, Approved: true}); err != nil {
					t.Fatal(err)
				}
			}
			head, err = parent.Head()
			if err != nil {
				t.Fatal(err)
			}
			if err := memory.Fork(t.Context(), parent, "parent", []placement.ForkChild{{UID: "child", Log: child}}, head); err != nil {
				t.Fatal(err)
			}
			state, err := controller.InspectApproval(child, 0)
			if err != nil || state == nil || !state.Inherited || state.Receipt != nil || state.Invocation.Harness != "override" {
				t.Fatalf("inherited approval=%+v, %v", state, err)
			}
			if live, err := backend.Status(t.Context(), api.Incarnation{ID: "child"}); err != nil || live != api.ComputeLive {
				t.Fatalf("cloned actor=%s, %v", live, err)
			}
			// Resume must refuse before selecting the inherited override or touching compute.
			before := suspendRecords(t, child)
			calls := len(ctl.calls)
			if err := r.Resume(t.Context(), child, "child", "memory"); !errors.Is(err, controller.ErrInheritedToolIntent) {
				t.Fatalf("inherited Resume=%v", err)
			}
			if !reflect.DeepEqual(before, suspendRecords(t, child)) || len(ctl.calls) != calls {
				t.Fatal("refused Resume changed the journal or actor")
			}
			snapshots := overrideBackend.snapshots
			var resolved string
			ref, err := r.Suspend(t.Context(), child, "child", "memory", placement.WithResolvedHarnessCheck(func(name string) error { resolved = name; return nil }))
			if err != nil {
				t.Fatal(err)
			}
			if resolved != "memory" || ref.Local != "child" || !ref.Memory || ref.ExternalURI == "" {
				t.Errorf("Suspend route=%q ref=%+v; want child's memory snapshot", resolved, ref)
			}
			assertSuspendRef(t, child, ref)
			if cold, err := backend.Status(t.Context(), api.Incarnation{ID: "child"}); err != nil || cold != api.ComputeCold {
				t.Errorf("suspended child=%s, %v; want cold", cold, err)
			}
			if overrideBackend.snapshots != snapshots {
				t.Error("Suspend snapshotted the inherited override")
			}
			actor := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "child"}]
			if actor == nil || actor.worker || ctl.snapshots[actor.snapshot].turns != 1 {
				t.Errorf("Suspend did not retain the child's RAM: %+v", actor)
			}
		})
	}
}

func TestApprovalRepairCASAndSharedGuard(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "CAS", true: "fenced"}[fenced], func(t *testing.T) {
			log := newSuspendStore(t).Session("s")
			seedPlacementApproval(t, log, "gate", api.EventToolCall)
			runtime := local.New(placementGateHarness{})
			t.Cleanup(func() { _ = runtime.Close() })
			b := &approvalBackend{Backend: runtime}
			p := placement.New(b, nil)
			other := placement.New(b, nil)
			r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": p, "other": other})
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &approvalCASLog{Store: log}
			wrapped.hook = func() {
				if _, err := r.Approve(t.Context(), log, "s", api.ApprovalDecision{}); !errors.Is(err, placement.ErrSessionBusy) {
					t.Fatalf("Approve overlap=%v", err)
				}
				if _, err := other.Exec(t.Context(), log, "s", nil, 2); !errors.Is(err, placement.ErrSessionBusy) {
					t.Fatalf("cross-route overlap=%v", err)
				}
				if fenced {
					_, err = log.NewFence()
				} else {
					_, err = log.Append(2, wrapped.fence, api.Event{ExecutionID: "e1", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "c1"}})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			err = r.Resume(t.Context(), wrapped, "s", "gate")
			want := eventlog.ErrConflict
			if fenced {
				want = eventlog.ErrFenced
			}
			if !errors.Is(err, want) || errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("repair=%v", err)
			}
			b.assertNoCompute(t)
			if _, err = r.Approve(t.Context(), log, "s", api.ApprovalDecision{}); !errors.Is(err, controller.ErrInvalidApprovalDecision) {
				t.Fatalf("guard leaked: %v", err)
			}
		})
	}
}

type approvalCASLog struct {
	eventlog.Store
	hook  func()
	fence int64
}

func (l *approvalCASLog) NewFence() (int64, error) {
	f, err := l.Store.NewFence()
	l.fence = f
	if err == nil && l.hook != nil {
		h := l.hook
		l.hook = nil
		h()
	}
	return f, err
}
