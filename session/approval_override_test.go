package session_test

import (
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/local"
	"github.com/aramase/agentsessions/runtime/substrate"
)

// An override must not park only B while A still holds the session's live memory.
func TestApprovalOverrideRefusesLiveMemorySession(t *testing.T) {
	store := openStore(t, ":memory:")
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	desc, err := (memHarness{}).Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	memory := substrate.New(ctl, "space", substrate.ObjectRef{Name: "memory"}, desc)
	h := &publicGateHarness{}
	override := &publicGateBackend{Backend: local.New(h)}
	t.Cleanup(func() { _ = override.Close() })
	r, err := placement.NewRegistry("memory", map[string]*placement.Placer{
		"memory": placement.New(memory, nil, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
			return memHarness{}, func() error { return nil }, nil
		})),
		"override": placement.New(override, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	if _, err := collectPublicExec(c, &v1.ExecRequest{Session: uid}); err != nil {
		t.Fatalf("initial memory turn: %v", err)
	}
	actor := substrate.ActorRef{Atespace: "space", Name: uid}
	if info, err := ctl.GetActor(t.Context(), actor); err != nil || info.Status != substrate.StatusRunning {
		t.Fatalf("initial actor=%+v, %v", info, err)
	}
	before, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil || before.GetComputeState() != v1.ComputeState_COMPUTE_LIVE {
		t.Fatalf("initial session=%v, %v", before, err)
	}
	log := store.Session(uid)
	records := routingRecords(t, log)
	fence := records[len(records)-1].Fence
	_, err = collectPublicExec(c, &v1.ExecRequest{Session: uid, Harness: "override"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("memory session override: %v, want FailedPrecondition", err)
	}
	if !reflect.DeepEqual(records, routingRecords(t, log)) {
		t.Fatal("refused override changed the journal")
	}
	after, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil || !proto.Equal(before, after) {
		t.Fatalf("refusal changed session projection: before=%v after=%v err=%v", before, after, err)
	}
	// A fence changes independently of the head; the next token must be its immediate successor.
	if next, err := log.NewFence(); err != nil || next != fence+1 {
		t.Fatalf("refused override advanced the fence: next=%d, previous=%d, err=%v", next, fence, err)
	}
	if h.runs.Load() != 0 || override.creates.Load() != 0 || override.restores.Load() != 0 || override.snapshots.Load() != 0 {
		t.Fatal("refused override ran or changed B's compute")
	}
	if info, err := ctl.GetActor(t.Context(), actor); err != nil || info.Status != substrate.StatusRunning || info.Snapshot != "" {
		t.Fatalf("refusal changed A's live actor: %+v, %v", info, err)
	}
	// With no B approval written, Suspend must retain A's route and checkpoint its memory.
	suspended, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: uid})
	if err != nil || suspended.GetComputeState() != v1.ComputeState_COMPUTE_COLD {
		t.Fatalf("Suspend=%v, %v", suspended, err)
	}
	info, err := ctl.GetActor(t.Context(), actor)
	if err != nil || info.Status != substrate.StatusSuspended || info.Snapshot == "" {
		t.Fatalf("Suspend did not checkpoint A: %+v, %v", info, err)
	}
	var snapshot *api.SnapshotRef
	for _, rec := range routingRecords(t, log) {
		if rec.Event.Kind == api.EventLifecycle && rec.Event.Lifecycle.Kind == api.LifecycleSuspend {
			snapshot = rec.Event.Lifecycle.Snapshot
		}
	}
	if snapshot == nil || !snapshot.Memory || override.snapshots.Load() != 0 {
		t.Fatalf("Suspend snapshot=%+v, B snapshots=%d; want A memory checkpoint", snapshot, override.snapshots.Load())
	}
}
