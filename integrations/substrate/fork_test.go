package ateadapter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/runtime/substrate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lifecycleControl is a stateful ate-api Control double for the stateful fork path: it keeps one
// Actor per name and moves it through the states substrate reports, with the held snapshot in
// ActorStatus.snapshots the way substrate at f03327bd and later records it. A suspend appends a new
// COMPLETED generation (a PENDING one when completeOnSuspend is false); a CreateActor from a tag
// borrows the tag's snapshot as generation 1.
//
// CreateTag copies the snapshot completed, which the double records as each durable copy completes
// rather than reading it back from the status list with the adapter's heldSnapshot, so a wrong choice
// in heldSnapshot cannot pass by being made on both sides.
type lifecycleControl struct {
	atepb.ControlClient

	mu                sync.Mutex
	actors            map[string]*atepb.Actor
	tags              map[string]string // tag name -> snapshot uri it copied
	completed         map[string]string // actor name -> uri of its most recent COMPLETED durable copy
	calls             []string
	completeOnSuspend bool
}

func newLifecycleControl() *lifecycleControl {
	return &lifecycleControl{actors: map[string]*atepb.Actor{}, tags: map[string]string{}, completed: map[string]string{}, completeOnSuspend: true}
}

func (l *lifecycleControl) record(call string) { l.calls = append(l.calls, call) }

func (l *lifecycleControl) CreateActor(_ context.Context, in *atepb.CreateActorRequest, _ ...grpc.CallOption) (*atepb.Actor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := in.GetActor().GetMetadata().GetName()
	l.record("create:" + name)
	if _, ok := l.actors[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Actor %s already exists", name)
	}
	st := &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDED}
	if src := in.GetActor().GetSourceTag(); src != nil {
		uri, ok := l.tags[src.GetName()]
		if !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "Tag %s not found", src.GetName())
		}
		snap := durableSnapshot(1, uri, atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)
		snap.Owner = atepb.SnapshotOwner_SNAPSHOT_OWNER_TAG
		st.Snapshots = []*atepb.Snapshot{snap}
		st.LastAssignedGeneration = 1
		l.completed[name] = uri
	}
	a := &atepb.Actor{Metadata: in.GetActor().GetMetadata(), ActorTemplate: in.GetActor().GetActorTemplate(), Status: st}
	l.actors[name] = a
	return a, nil
}

func (l *lifecycleControl) ResumeActor(_ context.Context, in *atepb.ResumeActorRequest, _ ...grpc.CallOption) (*atepb.ResumeActorResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := in.GetActor().GetName()
	l.record("resume:" + name)
	a, ok := l.actors[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Actor %s not found", name)
	}
	a.Status.State = atepb.ActorState_ACTOR_STATE_RUNNING
	a.Status.WorkerAssignment = &atepb.WorkerAssignment{WorkerPod: "worker-" + name}
	return &atepb.ResumeActorResponse{Actor: a, Resumed: true}, nil
}

func (l *lifecycleControl) SuspendActor(_ context.Context, in *atepb.SuspendActorRequest, _ ...grpc.CallOption) (*atepb.SuspendActorResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := in.GetActor().GetName()
	l.record("suspend:" + name)
	a, ok := l.actors[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Actor %s not found", name)
	}
	if a.Status.State == atepb.ActorState_ACTOR_STATE_RUNNING {
		gen := a.Status.LastAssignedGeneration + 1
		st := atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING
		if l.completeOnSuspend {
			st = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
		}
		uri := fmt.Sprintf("gs://b/atespaces/space/actors/%s/snapshots/s%d", name, gen)
		a.Status.Snapshots = append(a.Status.Snapshots, durableSnapshot(gen, uri, st))
		if l.completeOnSuspend {
			l.completed[name] = uri
		}
		a.Status.LastAssignedGeneration = gen
	}
	a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
	a.Status.WorkerAssignment = nil
	return &atepb.SuspendActorResponse{Actor: a}, nil
}

func (l *lifecycleControl) DeleteActor(_ context.Context, in *atepb.DeleteActorRequest, _ ...grpc.CallOption) (*atepb.Actor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record("delete:" + in.GetActor().GetName())
	delete(l.actors, in.GetActor().GetName())
	delete(l.completed, in.GetActor().GetName())
	return &atepb.Actor{}, nil
}

func (l *lifecycleControl) GetActor(_ context.Context, in *atepb.GetActorRequest, _ ...grpc.CallOption) (*atepb.Actor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.actors[in.GetActor().GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Actor %s not found", in.GetActor().GetName())
	}
	return a, nil
}

// CreateTag copies the source's latest COMPLETED durable snapshot, as substrate's TagActorSnapshot
// does, and refuses an actor that holds none. It does not use heldSnapshot: see lifecycleControl.
func (l *lifecycleControl) CreateTag(_ context.Context, in *atepb.CreateTagRequest, _ ...grpc.CallOption) (*atepb.Tag, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tg := in.GetTag()
	l.record("tag:" + tg.GetSourceActor().GetName() + "->" + tg.GetMetadata().GetName())
	if _, ok := l.actors[tg.GetSourceActor().GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "Actor %s not found", tg.GetSourceActor().GetName())
	}
	uri := l.completed[tg.GetSourceActor().GetName()]
	if uri == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "Actor %s holds no snapshot", tg.GetSourceActor().GetName())
	}
	l.tags[tg.GetMetadata().GetName()] = uri
	return tg, nil
}

func (l *lifecycleControl) DeleteTag(_ context.Context, in *atepb.DeleteTagRequest, _ ...grpc.CallOption) (*atepb.Tag, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record("untag:" + in.GetTag().GetName())
	delete(l.tags, in.GetTag().GetName())
	return &atepb.Tag{}, nil
}

func memoryBackend(ctl *lifecycleControl) *substrate.Backend {
	desc := api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}}
	return substrate.New(FromClient(ctl), "space", substrate.ObjectRef{Atespace: "space", Name: "counter"}, desc)
}

// The stateful fork path end to end over the adapter: create, suspend (the snapshot handle comes
// from ActorStatus.snapshots), then fork. The child is seeded from a tag of exactly the snapshot the
// suspend produced, resumed, and reported RUNNING.
func TestStatefulForkOverSnapshotsList(t *testing.T) {
	ctx := context.Background()
	ctl := newLifecycleControl()
	b := memoryBackend(ctl)

	parent, err := b.Create(ctx, &api.SessionSpec{SessionUID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx, parent, api.SnapshotExternal)
	if err != nil {
		t.Fatal(err)
	}
	if want := "gs://b/atespaces/space/actors/parent/snapshots/s1"; snap.ExternalURI != want {
		t.Fatalf("snapshot uri=%q want %q", snap.ExternalURI, want)
	}

	child, err := b.Fork(ctx, snap, api.ForkOpts{ChildSessionUID: "child"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child.ID != "child" {
		t.Fatalf("child incarnation=%+v", child)
	}
	if got, want := ctl.tags["fork-child"], snap.ExternalURI; got != want {
		t.Fatalf("fork tag copied %q, want the parent's suspended snapshot %q", got, want)
	}
	if got := ctl.actors["child"].GetStatus().GetState(); got != atepb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("child state=%v want RUNNING", got)
	}
	want := []string{"create:parent", "resume:parent", "suspend:parent", "tag:parent->fork-child", "create:child", "resume:child"}
	if !reflect.DeepEqual(ctl.calls, want) {
		t.Fatalf("calls=%v\nwant %v", ctl.calls, want)
	}

	// The child's own first suspend writes a snapshot under its own prefix, which is now the one it
	// holds instead of the borrowed tag copy.
	childSnap, err := b.Snapshot(ctx, child, api.SnapshotExternal)
	if err != nil {
		t.Fatal(err)
	}
	if want := "gs://b/atespaces/space/actors/child/snapshots/s2"; childSnap.ExternalURI != want {
		t.Fatalf("child snapshot uri=%q want %q", childSnap.ExternalURI, want)
	}
}

// A suspend whose durable copy has not completed yields no handle, so the stateful fork refuses with
// ErrNoSnapshotToClone before it tags anything, instead of cloning a snapshot it cannot name.
func TestStatefulForkWithoutCompletedSnapshotRefuses(t *testing.T) {
	ctx := context.Background()
	ctl := newLifecycleControl()
	ctl.completeOnSuspend = false
	b := memoryBackend(ctl)

	parent, err := b.Create(ctx, &api.SessionSpec{SessionUID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx, parent, api.SnapshotExternal)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ExternalURI != "" {
		t.Fatalf("snapshot uri=%q want empty for a pending durable copy", snap.ExternalURI)
	}
	_, err = b.Fork(ctx, snap, api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrNoSnapshotToClone) {
		t.Fatalf("fork err=%v want ErrNoSnapshotToClone", err)
	}
	for _, c := range ctl.calls {
		if c == "tag:parent->fork-child" || c == "create:child" {
			t.Fatalf("fork without a held snapshot must not tag or create, calls=%v", ctl.calls)
		}
	}
}

// A parent suspended again after the fork's checkpoint holds a newer generation, so the fork's
// before-tag check sees a different handle and refuses as superseded rather than cloning RAM that
// does not match the child's journal prefix.
func TestStatefulForkDetectsNewerGeneration(t *testing.T) {
	ctx := context.Background()
	ctl := newLifecycleControl()
	b := memoryBackend(ctl)

	parent, err := b.Create(ctx, &api.SessionSpec{SessionUID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx, parent, api.SnapshotExternal)
	if err != nil {
		t.Fatal(err)
	}
	// The router wakes the parent and something suspends it again: generation 2 completes.
	if _, err := b.Restore(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Snapshot(ctx, parent, api.SnapshotExternal); err != nil {
		t.Fatal(err)
	}
	_, err = b.Fork(ctx, snap, api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrSnapshotSuperseded) {
		t.Fatalf("fork err=%v want ErrSnapshotSuperseded", err)
	}
	if _, ok := ctl.actors["child"]; ok {
		t.Fatal("a superseded fork must not create the child")
	}
}
