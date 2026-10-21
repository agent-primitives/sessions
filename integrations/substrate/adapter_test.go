package ateadapter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/runtime/substrate"
	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// fakeControl is an ate-api Control double: it records calls and returns canned actors so the
// translation is tested without a real ate-api-server. Only the mapped methods are overridden; the
// rest are promoted from the embedded (nil) interface and never called.
type fakeControl struct {
	atepb.ControlClient
	calls      []string
	lastCreate *atepb.Actor
	lastTag    *atepb.Tag
	actor      *atepb.Actor
	getErr     error
	tagErr     error
	createErr  error
	resumeErr  error
	suspendErr error
	// lastSuspend is the last SuspendActor request; onSuspend, when set, runs before it answers.
	lastSuspend *atepb.SuspendActorRequest
	onSuspend   func()
	// onGet, when set, runs before GetActor answers its n-th call (1-based).
	onGet func(n int)
	gets  int
}

func (f *fakeControl) CreateActor(_ context.Context, in *atepb.CreateActorRequest, _ ...grpc.CallOption) (*atepb.Actor, error) {
	a := in.GetActor()
	f.calls = append(f.calls, "create:"+a.GetMetadata().GetAtespace()+"/"+a.GetMetadata().GetName()+
		":tmpl="+a.GetActorTemplate().GetAtespace()+"/"+a.GetActorTemplate().GetName())
	f.lastCreate = a
	if f.createErr != nil {
		return nil, f.createErr
	}
	// Substrate returns the actor it stored: the server assigned the UID, and it starts SUSPENDED.
	stored := proto.Clone(a).(*atepb.Actor)
	stored.Metadata.Uid = "uid-" + a.GetMetadata().GetName()
	stored.Status = &atepb.ActorStatus{State: atepb.ActorState_ACTOR_STATE_SUSPENDED}
	return stored, nil
}
func (f *fakeControl) ResumeActor(_ context.Context, in *atepb.ResumeActorRequest, _ ...grpc.CallOption) (*atepb.ResumeActorResponse, error) {
	f.calls = append(f.calls, "resume:"+in.GetActor().GetAtespace()+"/"+in.GetActor().GetName())
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	return &atepb.ResumeActorResponse{Actor: f.actor, Resumed: true}, nil
}
func (f *fakeControl) SuspendActor(_ context.Context, in *atepb.SuspendActorRequest, _ ...grpc.CallOption) (*atepb.SuspendActorResponse, error) {
	f.calls = append(f.calls, "suspend:"+in.GetActor().GetName())
	f.lastSuspend = in
	if f.onSuspend != nil {
		f.onSuspend()
	}
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	return &atepb.SuspendActorResponse{Actor: f.actor}, nil
}
func (f *fakeControl) DeleteActor(_ context.Context, in *atepb.DeleteActorRequest, _ ...grpc.CallOption) (*atepb.Actor, error) {
	f.calls = append(f.calls, "delete:"+in.GetActor().GetName())
	return &atepb.Actor{}, nil
}
func (f *fakeControl) GetActor(_ context.Context, in *atepb.GetActorRequest, _ ...grpc.CallOption) (*atepb.Actor, error) {
	f.calls = append(f.calls, "get:"+in.GetActor().GetName())
	f.gets++
	if f.onGet != nil {
		f.onGet(f.gets)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.actor, nil
}
func (f *fakeControl) CreateTag(_ context.Context, in *atepb.CreateTagRequest, _ ...grpc.CallOption) (*atepb.Tag, error) {
	tg := in.GetTag()
	f.calls = append(f.calls, "tag:"+tg.GetSourceActor().GetAtespace()+"/"+tg.GetSourceActor().GetName()+
		"->"+tg.GetMetadata().GetAtespace()+"/"+tg.GetMetadata().GetName())
	f.lastTag = tg
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	return tg, nil
}
func (f *fakeControl) DeleteTag(_ context.Context, in *atepb.DeleteTagRequest, _ ...grpc.CallOption) (*atepb.Tag, error) {
	f.calls = append(f.calls, "untag:"+in.GetTag().GetAtespace()+"/"+in.GetTag().GetName())
	return &atepb.Tag{}, nil
}

func runningActor() *atepb.Actor {
	return &atepb.Actor{
		Metadata: &atepb.ResourceMetadata{Atespace: "space", Name: "sess-x", Uid: "0b5c3c56-6d5e-4f0e-9b1a-2f4d8c1e7a90"},
		Status: &atepb.ActorStatus{
			State: atepb.ActorState_ACTOR_STATE_RUNNING,
			WorkerAssignment: &atepb.WorkerAssignment{
				WorkerPod:    "echo-harness-7d9f-abcde",
				WorkerPodIps: []string{"10.0.0.5"},
			},
			Snapshots:              []*atepb.Snapshot{durableSnapshot(1, "gs://b/atespaces/space/actors/u1/snapshots/s1", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)},
			LastAssignedGeneration: 1,
		},
	}
}

// pausedSnapshot is the generation substrate v0.5.0 records for a paused actor: checkpointed on
// its node (locality set) with no durable copy (latestSnapshotIsLocalOnly in
// cmd/ateapi/internal/controlapi/snapshot_helpers.go).
func pausedSnapshot(gen int32) *atepb.Snapshot {
	return &atepb.Snapshot{
		Uuid:       fmt.Sprintf("s%d", gen),
		Generation: gen,
		Owner:      atepb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR,
		Fidelity:   atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		Locality:   "node-a",
	}
}

func durableSnapshot(gen int32, uri string, st atepb.SnapshotStorageStatus) *atepb.Snapshot {
	return &atepb.Snapshot{
		Uuid:       fmt.Sprintf("s%d", gen),
		Generation: gen,
		Owner:      atepb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR,
		Fidelity:   atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		DurableSnapshot: &atepb.SnapshotStorage{
			Status: st,
			Object: &atepb.ObjectSnapshot{SnapshotUri: uri},
		},
	}
}

// The template is an atespaced substrate resource, referenced by (atespace, name) on the actor.
func TestCreateMapsTemplate(t *testing.T) {
	f := &fakeControl{}
	err := FromClient(f).CreateActor(context.Background(),
		substrate.ActorRef{Atespace: "space", Name: "sess-x"},
		substrate.ObjectRef{Atespace: "tmpl", Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"create:space/sess-x:tmpl=tmpl/echo"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls=%v want %v", f.calls, want)
	}
	if f.lastCreate.GetSourceTag() != nil {
		t.Fatalf("a plain create must not name a source tag, got %v", f.lastCreate.GetSourceTag())
	}
}

// A name another caller already took is reported as substrate.ErrActorExists, keeping the gRPC
// error, so the backend can attach to that actor; any other create error is returned unchanged.
func TestCreateReportsATakenName(t *testing.T) {
	ref := substrate.ActorRef{Atespace: "space", Name: "sess-x"}
	tmpl := substrate.ObjectRef{Atespace: "space", Name: "echo"}
	taken := status.Error(codes.AlreadyExists, "actor space/sess-x already exists")
	err := FromClient(&fakeControl{createErr: taken}).CreateActor(context.Background(), ref, tmpl)
	if !errors.Is(err, substrate.ErrActorExists) || !errors.Is(err, taken) {
		t.Fatalf("taken name: err=%v, want ErrActorExists wrapping the AlreadyExists error", err)
	}
	other := status.Error(codes.FailedPrecondition, "template has no golden snapshot")
	err = FromClient(&fakeControl{createErr: other}).CreateActor(context.Background(), ref, tmpl)
	if errors.Is(err, substrate.ErrActorExists) || !errors.Is(err, other) {
		t.Fatalf("other create error: err=%v, want it unchanged", err)
	}
}

// GetActor reports the actor's server-assigned UID, which an actor created again under the same name
// does not share.
func TestGetReportsTheActorUID(t *testing.T) {
	first := runningActor()
	again := runningActor()
	again.Metadata.Uid = "7f1e2d3c-4b5a-4968-8776-655443322110"
	ref := substrate.ActorRef{Atespace: "space", Name: "sess-x"}
	a, err := FromClient(&fakeControl{actor: first}).GetActor(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromClient(&fakeControl{actor: again}).GetActor(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if a.UID != first.GetMetadata().GetUid() || b.UID != again.GetMetadata().GetUid() || a.UID == b.UID {
		t.Fatalf("UIDs = %q and %q, want the two actors' distinct metadata UIDs", a.UID, b.UID)
	}
}

// ResumeActor reports the actor's state, its worker (diagnostics only) and the snapshot it holds.
func TestResumeMapsActorState(t *testing.T) {
	f := &fakeControl{actor: runningActor()}
	info, err := FromClient(f).ResumeActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	want := substrate.ActorInfo{
		Status:           substrate.StatusRunning,
		Worker:           "echo-harness-7d9f-abcde",
		Snapshot:         "gs://b/atespaces/space/actors/u1/snapshots/s1",
		SnapshotFidelity: substrate.SnapshotFidelityMemory,
		UID:              "0b5c3c56-6d5e-4f0e-9b1a-2f4d8c1e7a90",
	}
	if info != want {
		t.Fatalf("info=%+v want %+v", info, want)
	}
	if want := []string{"resume:space/sess-x"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls=%v want %v", f.calls, want)
	}
}

// Another host can attach after CreateActor stored a snapshot-less SUSPENDED actor but before
// its first ResumeActor. Only authoritative no-history evidence permits that cold start.
func TestNoSnapshotHistoryRequiresAuthoritativeEvidence(t *testing.T) {
	for _, kind := range []string{"fresh", "missing status", "missing UID", "generation assigned", "borrowed snapshot", "unknown legacy fields", "paused"} {
		t.Run(kind, func(t *testing.T) {
			a := runningActor()
			a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
			a.Status.Snapshots = nil
			a.Status.LastAssignedGeneration = 0
			switch kind {
			case "missing status":
				a.Status = nil
			case "missing UID":
				a.Metadata.Uid = ""
			case "generation assigned":
				a.Status.LastAssignedGeneration = 1
			case "borrowed snapshot":
				a.Status.Snapshots = runningActor().Status.Snapshots
			case "unknown legacy fields":
				a.Status.ProtoReflect().SetUnknown(wireMessage(4, wireString(1, "legacy")))
			case "paused":
				// Substrate v0.5.0 records a paused actor as SUSPENDED with a node-local generation.
				a.Status.Snapshots = []*atepb.Snapshot{pausedSnapshot(1)}
				a.Status.LastAssignedGeneration = 1
			}
			if got := actorInfo(a).NoSnapshotHistory; got != (kind == "fresh") {
				t.Fatalf("NoSnapshotHistory=%v for %s", got, kind)
			}
		})
	}
}

func TestFirstPlacementAttachesBeforeFirstResume(t *testing.T) {
	for _, memory := range []bool{false, true} {
		a := runningActor()
		a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
		a.Status.Snapshots = nil
		a.Status.LastAssignedGeneration = 0
		f := &fakeControl{actor: a, resumeErr: errors.New("first resume reached")}
		desc := api.Descriptor{ID: "echo"}
		opts := []substrate.Option{substrate.WithVolumesOnly()}
		if memory {
			desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
			opts = nil
		}
		b := substrate.New(FromClient(f), "space", substrate.ObjectRef{Name: "echo"}, desc, opts...)
		_, err := b.Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x"})
		if !errors.Is(err, f.resumeErr) || !reflect.DeepEqual(f.calls, []string{"get:sess-x", "resume:space/sess-x"}) {
			t.Fatalf("first placement memory=%v err=%v calls=%v", memory, err, f.calls)
		}
	}
}

// The bytes here are encoded by hand with the field numbers of ateapi.proto at substrate v0.5.0,
// not by the generated package, so a regenerated client whose numbering no longer matches that
// server fails here instead of silently on a cluster:
//
//	Actor.status = 7; ActorStatus.state = 1, worker_assignment = 2, snapshots = 11;
//	WorkerAssignment.worker_pod = 3; Snapshot.generation = 2, durable_snapshot = 7;
//	SnapshotStorage.status = 1, object = 2; ObjectSnapshot.snapshot_uri = 1;
//	ACTOR_STATE_RUNNING = 2; SNAPSHOT_STORAGE_STATUS_COMPLETED = 2.
func TestGetActorDecodesCurrentWireFormat(t *testing.T) {
	var st []byte
	st = protowire.AppendVarint(protowire.AppendTag(st, 1, protowire.VarintType), 2)
	st = append(st, wireMessage(2, wireString(3, "echo-harness-7d9f-abcde"))...)
	var storage []byte
	storage = protowire.AppendVarint(protowire.AppendTag(storage, 1, protowire.VarintType), 2)
	storage = append(storage, wireMessage(2, wireString(1, "gs://b/atespaces/space/actors/u1/snapshots/s1"))...)
	var snap []byte
	snap = protowire.AppendVarint(protowire.AppendTag(snap, 2, protowire.VarintType), 1)
	snap = append(snap, wireMessage(7, storage)...)
	st = append(st, wireMessage(11, snap)...)

	info := decodeActorInfo(t, wireMessage(7, st))
	if info.Status != substrate.StatusRunning || info.Worker != "echo-harness-7d9f-abcde" ||
		info.Snapshot != "gs://b/atespaces/space/actors/u1/snapshots/s1" {
		t.Fatalf("decoded info=%+v, want a running actor on its worker holding its snapshot", info)
	}
}

// A server older than substrate f03327bd sends the held snapshot as ActorStatus.external_snapshot
// (field 4), which this client does not read. It must report no snapshot rather than fail to decode,
// so a memory checkpoint against such a server refuses before recording SUSPEND, and a direct
// Backend.Fork with no handle refuses with ErrNoSnapshotToClone instead of tagging.
func TestGetActorIgnoresPreSnapshotsListField(t *testing.T) {
	var st []byte
	st = protowire.AppendVarint(protowire.AppendTag(st, 1, protowire.VarintType), 4) // SUSPENDED
	st = append(st, wireMessage(4, wireString(1, "gs://b/atespaces/space/actors/u1/snapshots/s1"))...)

	info := decodeActorInfo(t, wireMessage(7, st))
	if info.Status != substrate.StatusSuspended || info.Snapshot != "" {
		t.Fatalf("decoded info=%+v, want a suspended actor reporting no snapshot", info)
	}
}

func wireMessage(num protowire.Number, body []byte) []byte {
	b := protowire.AppendTag(nil, num, protowire.BytesType)
	return protowire.AppendBytes(b, body)
}

func wireString(num protowire.Number, v string) []byte {
	b := protowire.AppendTag(nil, num, protowire.BytesType)
	return protowire.AppendString(b, v)
}

func decodeActorInfo(t *testing.T, wire []byte) substrate.ActorInfo {
	t.Helper()
	var decoded atepb.Actor
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	info, err := FromClient(&fakeControl{actor: &decoded}).GetActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Pick the newest generation before readiness: pending/local-only state blocks older durable
// state, regardless of list order. A completed newest generation without a URI also stays empty,
// never exposing an older checkpoint that no longer represents the actor's latest state.
func TestHeldSnapshotDoesNotFallBack(t *testing.T) {
	const (
		completed = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
		pending   = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING
	)
	local := &atepb.Snapshot{Uuid: "s5", Generation: 5, Owner: atepb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, Locality: "node-a"}
	for _, tc := range []struct {
		name  string
		snaps []*atepb.Snapshot
		want  string
	}{
		{name: "none", want: ""},
		{name: "only pending", snaps: []*atepb.Snapshot{durableSnapshot(1, "uri-1", pending)}, want: ""},
		{name: "only local", snaps: []*atepb.Snapshot{local}, want: ""},
		{name: "newer local blocks completed", snaps: []*atepb.Snapshot{durableSnapshot(2, "uri-2", completed), local}, want: ""},
		{name: "pending first still blocks completed", snaps: []*atepb.Snapshot{durableSnapshot(3, "uri-3", pending), durableSnapshot(2, "uri-2", completed)}, want: ""},
		{name: "equal generation pending last blocks completed", snaps: []*atepb.Snapshot{durableSnapshot(3, "uri-3", completed), durableSnapshot(3, "pending", pending)}, want: ""},
		{name: "single completed", snaps: []*atepb.Snapshot{durableSnapshot(1, "uri-1", completed)}, want: "uri-1"},
		{
			name:  "newer pending blocks older completed",
			snaps: []*atepb.Snapshot{durableSnapshot(2, "uri-2", completed), durableSnapshot(3, "uri-3", pending)},
			want:  "",
		},
		{
			name:  "highest completed wins regardless of order",
			snaps: []*atepb.Snapshot{durableSnapshot(4, "uri-4", completed), durableSnapshot(3, "uri-3", pending), durableSnapshot(2, "uri-2", completed)},
			want:  "uri-4",
		},
		{
			name:  "newest completed without a uri is held, not an older one",
			snaps: []*atepb.Snapshot{durableSnapshot(2, "uri-2", completed), durableSnapshot(3, "", completed)},
			want:  "",
		},
		{
			name:  "equal generations: the later entry wins, as in substrate",
			snaps: []*atepb.Snapshot{durableSnapshot(3, "uri-3a", completed), durableSnapshot(3, "uri-3b", completed)},
			want:  "uri-3b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := actorInfo(&atepb.Actor{Status: &atepb.ActorStatus{Snapshots: tc.snaps}}).Snapshot; got != tc.want {
				t.Fatalf("held snapshot=%q want %q", got, tc.want)
			}
		})
	}
	if got := heldSnapshot(nil); got != nil {
		t.Fatalf("heldSnapshot(nil)=%v want nil", got)
	}
}

// A tag name that is already taken was not reserved by this call. The backend must be able to tell,
// so it does not delete someone else's tag while cleaning up its own failed attempt.
func TestTagActorReportsAlreadyExistsAsSentinel(t *testing.T) {
	f := &fakeControl{tagErr: status.Error(codes.AlreadyExists, "Tag space/fork-c already exists")}
	err := FromClient(f).TagActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "p"}, substrate.SnapshotID{Atespace: "space", Name: "fork-c"})
	if !errors.Is(err, substrate.ErrTagExists) {
		t.Fatalf("err=%v want ErrTagExists", err)
	}
	f.tagErr = status.Error(codes.Unavailable, "object store unavailable")
	err = FromClient(f).TagActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "p"}, substrate.SnapshotID{Atespace: "space", Name: "fork-c"})
	if err == nil || errors.Is(err, substrate.ErrTagExists) {
		t.Fatalf("err=%v want a failure that is not ErrTagExists", err)
	}
}

func TestGetActorReportsNotFoundAsSentinel(t *testing.T) {
	f := &fakeControl{getErr: status.Error(codes.NotFound, "actor not found")}
	_, err := FromClient(f).GetActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "gone"})
	if !errors.Is(err, substrate.ErrActorNotFound) {
		t.Fatalf("err=%v want ErrActorNotFound", err)
	}
}

// Substrate refuses SuspendActor of a missing actor with NOT_FOUND (RPCService.SuspendActor in
// cmd/ateapi/internal/controlapi/actor.go, from the store lookup in
// cmd/ateapi/internal/controlapi/workflow_suspend.go). The ControlClient contract requires
// ErrActorNotFound for it, as from GetActor, so a memory-tier Snapshot can report the session's
// in-RAM state as lost; it is not ErrActorBusy, and substrate's reason is kept.
func TestSuspendActorReportsNotFoundAsSentinel(t *testing.T) {
	f := &fakeControl{suspendErr: status.Error(codes.NotFound, "Actor gone not found")}
	_, err := FromClient(f).SuspendActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "gone"})
	if !errors.Is(err, substrate.ErrActorNotFound) || errors.Is(err, substrate.ErrActorBusy) || !strings.Contains(err.Error(), "Actor gone not found") {
		t.Fatalf("err=%v want ErrActorNotFound carrying substrate's reason", err)
	}
}

// Substrate refuses ResumeActor of a missing actor with NOT_FOUND (RPCService.ResumeActor in
// cmd/ateapi/internal/controlapi/actor.go). The adapter reports it as ErrActorNotFound, so a
// memory-tier Restore reports the session's in-RAM state as lost from the resume itself; substrate's
// reason is kept.
func TestResumeActorReportsNotFoundAsSentinel(t *testing.T) {
	f := &fakeControl{resumeErr: status.Error(codes.NotFound, "Actor space/gone not found")}
	_, err := FromClient(f).ResumeActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "gone"})
	if !errors.Is(err, substrate.ErrActorNotFound) || errors.Is(err, substrate.ErrActorBusy) || !strings.Contains(err.Error(), "Actor space/gone not found") {
		t.Fatalf("err=%v want ErrActorNotFound carrying substrate's reason", err)
	}
}

// An asynchronous suspend must not turn a pending generation into an older or empty checkpoint.
func TestPendingNewestSnapshotRefusesCaptureAndRestore(t *testing.T) {
	for _, mode := range []string{"default", "volumes", "memory"} {
		t.Run(mode, func(t *testing.T) {
			a := runningActor()
			a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
			old := durableSnapshot(1, "old", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)
			latest := durableSnapshot(2, "latest", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
			if mode == "volumes" {
				old.Fidelity, latest.Fidelity = atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
			}
			a.Status.Snapshots = []*atepb.Snapshot{latest, old}
			f := &fakeControl{actor: a}
			desc := api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}
			var opts []substrate.Option
			switch mode {
			case "volumes":
				opts = append(opts, substrate.WithVolumesOnly())
			case "memory":
				desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
			}
			b := substrate.New(FromClient(f), "space", substrate.ObjectRef{Name: "echo"}, desc, opts...)
			ref, err := b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
			if !errors.Is(err, substrate.ErrSnapshotNotReady) || !errors.Is(err, substrate.ErrSnapshotNotRestorable) || ref != (api.SnapshotRef{}) {
				t.Fatalf("pending capture returned ref=%+v err=%v", ref, err)
			}
			for _, uri := range []string{"old", "latest", ""} {
				f.calls = nil
				_, err := b.Restore(t.Context(), api.SnapshotRef{Local: "sess-x", ExternalURI: uri, Memory: mode != "volumes"})
				if !errors.Is(err, substrate.ErrSnapshotNotReady) || !reflect.DeepEqual(f.calls, []string{"get:sess-x"}) {
					t.Fatalf("pending restore: err=%v calls=%v", err, f.calls)
				}
			}
			f.calls = nil
			if _, err := b.Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x"}); !errors.Is(err, substrate.ErrSnapshotNotReady) || !reflect.DeepEqual(f.calls, []string{"get:sess-x"}) {
				t.Fatalf("pending attach: err=%v calls=%v", err, f.calls)
			}
			if mode == "memory" {
				f.calls = nil
				_, err := b.Fork(t.Context(), api.SnapshotRef{Local: "sess-x", ExternalURI: "old", Memory: true}, api.ForkOpts{ChildSessionUID: "child"})
				if !errors.Is(err, substrate.ErrSnapshotSuperseded) || !reflect.DeepEqual(f.calls, []string{"get:sess-x"}) {
					t.Fatalf("pending fork: err=%v calls=%v", err, f.calls)
				}
			}
			latest.DurableSnapshot.Status = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
			ref, err = b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
			if err != nil || ref.ExternalURI != "latest" {
				t.Fatalf("completed capture: ref=%+v err=%v", ref, err)
			}
			// The fake does not transition actors on resume; inject a recognizable failure
			// to prove completed upload now passes preflight and reaches ResumeActor.
			f.resumeErr = errors.New("resume reached")
			f.calls = nil
			if _, err := b.Restore(t.Context(), ref); !errors.Is(err, f.resumeErr) || !slices.Contains(f.calls, "resume:space/sess-x") {
				t.Fatalf("completed restore: err=%v calls=%v", err, f.calls)
			}
		})
	}
}

// Fidelity must follow the selected completed generation, never the template or another entry.
func TestSnapshotFidelityFollowsSelectedGeneration(t *testing.T) {
	const completed = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
	for _, tc := range []struct {
		name     string
		fidelity atepb.SnapshotFidelity
		want     substrate.SnapshotFidelity
	}{
		{name: "memory", fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, want: substrate.SnapshotFidelityMemory},
		{name: "volumes", fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, want: substrate.SnapshotFidelityVolumes},
		{name: "rootfs", fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS, want: substrate.SnapshotFidelityRootFS},
		{name: "unspecified", want: substrate.SnapshotFidelityUnknown},
		{name: "future", fidelity: atepb.SnapshotFidelity(99), want: substrate.SnapshotFidelityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			older := durableSnapshot(1, "older", completed)
			selected := durableSnapshot(2, "selected", completed)
			selected.Fidelity = tc.fidelity
			pending := durableSnapshot(0, "pending", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
			a := runningActor()
			a.Status.Snapshots = []*atepb.Snapshot{selected, pending, older}
			ad := FromClient(&fakeControl{actor: a})
			ref := substrate.ActorRef{Atespace: "space", Name: "sess-x"}
			for _, op := range []struct {
				name string
				call func(context.Context, substrate.ActorRef) (substrate.ActorInfo, error)
			}{{"suspend", ad.SuspendActor}, {"get", ad.GetActor}} {
				got, err := op.call(context.Background(), ref)
				if err != nil || got.Snapshot != "selected" || got.SnapshotFidelity != tc.want {
					t.Fatalf("%s: info=%+v err=%v, want selected URI and fidelity %v", op.name, got, err, tc.want)
				}
			}
		})
	}
}

func TestSnapshotFidelityDoesNotFallBackToAnotherEntry(t *testing.T) {
	const completed = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
	for _, tc := range []struct {
		name     string
		snaps    []*atepb.Snapshot
		wantURI  string
		want     substrate.SnapshotFidelity
		notReady bool
		pending  bool
	}{
		{name: "no snapshot", want: substrate.SnapshotFidelityUnknown},
		{name: "no completed snapshot", snaps: []*atepb.Snapshot{durableSnapshot(2, "pending", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)}, want: substrate.SnapshotFidelityUnknown, notReady: true, pending: true},
		{name: "unspecified durable status is not pending", snaps: []*atepb.Snapshot{durableSnapshot(2, "unknown", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_UNSPECIFIED)}, want: substrate.SnapshotFidelityUnknown, notReady: true},
		{name: "newer local-only blocks older pending", snaps: []*atepb.Snapshot{durableSnapshot(1, "pending", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING), {Generation: 2, Fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY}}, want: substrate.SnapshotFidelityUnknown, notReady: true},
		{name: "no durable snapshot", snaps: []*atepb.Snapshot{{Generation: 3, Fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY}}, want: substrate.SnapshotFidelityUnknown, notReady: true},
		{name: "tie uses later entry", snaps: []*atepb.Snapshot{durableSnapshot(2, "first", completed), {Generation: 2, Fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, DurableSnapshot: &atepb.SnapshotStorage{Status: completed, Object: &atepb.ObjectSnapshot{SnapshotUri: "second"}}}}, wantURI: "second", want: substrate.SnapshotFidelityVolumes},
		{name: "missing URI keeps selected fidelity", snaps: []*atepb.Snapshot{durableSnapshot(1, "older", completed), {Generation: 2, Fidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, DurableSnapshot: &atepb.SnapshotStorage{Status: completed}}}, want: substrate.SnapshotFidelityVolumes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := actorInfo(&atepb.Actor{Status: &atepb.ActorStatus{Snapshots: tc.snaps}})
			if got.Snapshot != tc.wantURI || got.SnapshotFidelity != tc.want || got.SnapshotNotReady != tc.notReady || got.SnapshotPending != tc.pending {
				t.Fatalf("info=%+v want URI=%q fidelity=%v notReady=%v pending=%v", got, tc.wantURI, tc.want, tc.notReady, tc.pending)
			}
		})
	}
}

// A suspend reports the held snapshot's URI and the actor's UID for later identity checks.
func TestSuspendReturnsTheHeldSnapshot(t *testing.T) {
	a := runningActor()
	a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
	a.Status.WorkerAssignment = nil
	f := &fakeControl{actor: a}
	got, err := FromClient(f).SuspendActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "gs://b/atespaces/space/actors/u1/snapshots/s1"; got.Snapshot != want {
		t.Fatalf("snapshot=%q want %q", got.Snapshot, want)
	}
	if want := a.GetMetadata().GetUid(); got.UID != want {
		t.Fatalf("uid=%q want %q", got.UID, want)
	}
}

// A snapshot still being written is not one the actor holds, so a suspend that has not completed
// its durable copy reports no handle.
func TestSuspendWithoutCompletedSnapshotReturnsEmpty(t *testing.T) {
	f := &fakeControl{actor: &atepb.Actor{
		Metadata: &atepb.ResourceMetadata{Atespace: "space", Name: "sess-x"},
		Status: &atepb.ActorStatus{
			State:                  atepb.ActorState_ACTOR_STATE_SUSPENDING,
			Snapshots:              []*atepb.Snapshot{durableSnapshot(1, "gs://b/atespaces/space/actors/u1/snapshots/s1", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)},
			LastAssignedGeneration: 1,
		},
	}}
	got, err := FromClient(f).SuspendActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot != "" {
		t.Fatalf("snapshot=%q want empty: an in-progress snapshot is not one the actor holds", got.Snapshot)
	}
}

// The clone path substrate requires: tag the suspended parent ACTOR, then create the child FROM THE
// TAG under the same template, then delete the tag on teardown.
func TestCloneTagsParentThenCreatesFromTag(t *testing.T) {
	f := &fakeControl{}
	ad := FromClient(f)
	parent := substrate.ActorRef{Atespace: "space", Name: "parent"}
	tag := substrate.SnapshotID{Atespace: "space", Name: "fork-child"}

	if err := ad.TagActor(context.Background(), parent, tag); err != nil {
		t.Fatal(err)
	}
	created, err := ad.CreateActorFromTag(context.Background(),
		substrate.ActorRef{Atespace: "space", Name: "child"},
		substrate.ObjectRef{Atespace: "tmpl", Name: "counter"}, tag)
	if err != nil {
		t.Fatal(err)
	}
	// Fork compares this UID with the one ResumeActor reports, so it must be the server-assigned
	// one.
	if created.UID != "uid-child" || created.Status != substrate.StatusSuspended {
		t.Fatalf("created=%+v want the stored actor (UID uid-child, SUSPENDED)", created)
	}
	if err := ad.DeleteTag(context.Background(), tag); err != nil {
		t.Fatal(err)
	}

	want := []string{"tag:space/parent->space/fork-child", "create:space/child:tmpl=tmpl/counter", "untag:space/fork-child"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls=%v want %v", f.calls, want)
	}
	if src := f.lastCreate.GetSourceTag(); src.GetAtespace() != "space" || src.GetName() != "fork-child" {
		t.Fatalf("child must be seeded from the fork tag, got %v", src)
	}
	// A fork's child is created in its parent's atespace, so the unpublished scope suffices.
	if f.lastTag.GetScope() != atepb.TagScope_TAG_SCOPE_ATESPACE {
		t.Fatalf("tag scope=%v want ATESPACE (no cross-atespace publication)", f.lastTag.GetScope())
	}
}

func TestStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		s    atepb.ActorState
		want substrate.ActorStatus
	}{
		{atepb.ActorState_ACTOR_STATE_RUNNING, substrate.StatusRunning},
		{atepb.ActorState_ACTOR_STATE_SUSPENDED, substrate.StatusSuspended},
		{atepb.ActorState_ACTOR_STATE_CRASHED, substrate.StatusTerminated},
		{atepb.ActorState_ACTOR_STATE_RESUMING, substrate.StatusTransitioning},
		{atepb.ActorState_ACTOR_STATE_SUSPENDING, substrate.StatusTransitioning},
		// Substrate v0.5.0 removed PAUSING (5) and PAUSED (6); a server that still sends them is
		// older than this client, which refuses to guess.
		{atepb.ActorState(5), substrate.StatusUnknown},
		{atepb.ActorState(6), substrate.StatusUnknown},
		{atepb.ActorState_ACTOR_STATE_DELETING, substrate.StatusUnknown},
		{atepb.ActorState_ACTOR_STATE_REVERTING, substrate.StatusUnknown},
		{atepb.ActorState_ACTOR_STATE_UNSPECIFIED, substrate.StatusUnknown},
	} {
		f := &fakeControl{actor: &atepb.Actor{
			Metadata: &atepb.ResourceMetadata{Name: "a", Atespace: "s"},
			Status:   &atepb.ActorStatus{State: tc.s},
		}}
		info, err := FromClient(f).GetActor(context.Background(), substrate.ActorRef{Atespace: "s", Name: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != tc.want {
			t.Fatalf("state %v -> %v, want %v", tc.s, info.Status, tc.want)
		}
	}
}

// Substrate refuses a resume, suspend or tag while another operation holds the actor's lease with
// ABORTED (another host resuming the actor it just created, say). The adapter reports that as
// substrate.ErrActorBusy, which the session service maps to a retryable ABORTED; any other failure
// keeps its own error.
func TestAbortedResumeAndSuspendAreBusy(t *testing.T) {
	ref := substrate.ActorRef{Atespace: "space", Name: "sess-x"}
	aborted := status.Error(codes.Aborted, "another operation is in progress for this actor")
	ops := map[string]func(*fakeControl) error{
		"resume": func(f *fakeControl) error {
			_, err := FromClient(f).ResumeActor(context.Background(), ref)
			return err
		},
		"suspend": func(f *fakeControl) error {
			_, err := FromClient(f).SuspendActor(context.Background(), ref)
			return err
		},
		"tag": func(f *fakeControl) error {
			return FromClient(f).TagActor(context.Background(), ref, substrate.SnapshotID{Atespace: "space", Name: "fork-c"})
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			f := &fakeControl{actor: runningActor(), resumeErr: aborted, suspendErr: aborted, tagErr: aborted}
			if err := op(f); !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, api.ErrComputeBusy) || !strings.Contains(err.Error(), "in progress") {
				t.Fatalf("aborted %s: err=%v, want ErrActorBusy carrying substrate's reason", name, err)
			}
			unavailable := status.Error(codes.Unavailable, "ate-api unavailable")
			f = &fakeControl{actor: runningActor(), resumeErr: unavailable, suspendErr: unavailable, tagErr: unavailable}
			if err := op(f); err == nil || errors.Is(err, substrate.ErrActorBusy) {
				t.Fatalf("unavailable %s: err=%v, want a failure that is not ErrActorBusy", name, err)
			}
		})
	}
}

// Substrate answers ResumeActor with NOT_FOUND for a missing actor and also for one that exists but
// whose template lacks a volume (createActorVolumes in cmd/ateapi/internal/controlapi/volumes.go).
// Over the adapter, a memory-tier Restore and Create report the session's state lost only when
// GetActor confirms the actor is gone (or, for Restore with no UID to compare, no longer holds the
// snapshot); an actor that still holds it keeps substrate's resume error.
func TestResumeNotFoundOnAnExistingActorIsNotLostState(t *testing.T) {
	const uri = "gs://b/atespaces/space/actors/u1/snapshots/s1"
	volume := status.Error(codes.NotFound, `volume "scratch" not found in template`)
	suspended := func() *atepb.Actor {
		a := runningActor()
		a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
		a.Status.WorkerAssignment = nil
		return a
	}
	desc := api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}}
	backend := func(f *fakeControl) *substrate.Backend {
		return substrate.New(FromClient(f), "space", substrate.ObjectRef{Atespace: "space", Name: "counter"}, desc)
	}
	turns := func() (int, error) { return 1, nil }
	ops := map[string]func(*substrate.Backend) error{
		"restore": func(b *substrate.Backend) error {
			_, err := b.Restore(context.Background(), api.SnapshotRef{Local: "sess-x", ExternalURI: uri, Memory: true})
			return err
		},
		"create": func(b *substrate.Backend) error {
			_, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x", StartedTurns: turns})
			return err
		},
	}
	for name, op := range ops {
		t.Run(name+" of an actor that still exists keeps the resume error", func(t *testing.T) {
			f := &fakeControl{actor: suspended(), resumeErr: volume}
			err := op(backend(f))
			if err == nil || errors.Is(err, api.ErrMemoryStateLost) || !strings.Contains(err.Error(), "not found in template") {
				t.Fatalf("err=%v, want substrate's resume error, not lost state", err)
			}
		})
		t.Run(name+" of an actor that is gone is lost state", func(t *testing.T) {
			f := &fakeControl{actor: suspended(), resumeErr: status.Error(codes.NotFound, "Actor space/sess-x not found")}
			if name == "restore" {
				f.getErr = status.Error(codes.NotFound, "Actor space/sess-x not found")
			} else {
				// Create's first GetActor finds it SUSPENDED; it is gone by the second.
				f.onGet = func(n int) {
					if n == 2 {
						f.getErr = status.Error(codes.NotFound, "Actor space/sess-x not found")
					}
				}
			}
			if err := op(backend(f)); !errors.Is(err, api.ErrMemoryStateLost) {
				t.Fatalf("err=%v, want api.ErrMemoryStateLost", err)
			}
		})
	}
}

// Substrate after v0.5.0 adds SuspendActorRequest.options and defaults its durability mode to
// asynchronous. At v0.5.0 the request carries only the actor and the suspend is synchronous, so the
// adapter sends nothing else. A substrate bump that adds any request field fails here until
// SuspendActor asks for a synchronous suspend explicitly and this test asserts that it does.
func TestSuspendActorRequestHasNoDurabilityOption(t *testing.T) {
	fields := (&atepb.SuspendActorRequest{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := range fields.Len() {
		names = append(names, string(fields.Get(i).Name()))
	}
	if !reflect.DeepEqual(names, []string{"actor"}) {
		t.Fatalf("SuspendActorRequest fields=%v want [actor]: set a synchronous durability mode in SuspendActor", names)
	}
	a := runningActor()
	a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
	a.Status.WorkerAssignment = nil
	f := &fakeControl{actor: a}
	if _, err := FromClient(f).SuspendActor(t.Context(), substrate.ActorRef{Atespace: "space", Name: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	want := &atepb.SuspendActorRequest{Actor: &atepb.ObjectRef{Atespace: "space", Name: "sess-x"}}
	if !proto.Equal(f.lastSuspend, want) || len(f.lastSuspend.ProtoReflect().GetUnknown()) != 0 {
		t.Fatalf("suspend request=%v want %v", f.lastSuspend, want)
	}
}

// A checkpoint is held only when the newest generation the suspend returns is durable. A pending
// upload is api.ErrCheckpointPending (UNAVAILABLE at the session API); a node-local generation, the
// shape substrate v0.5.0 gives a paused actor, is api.ErrInvalidCheckpoint (FAILED_PRECONDITION),
// because waiting does not upload it. Neither returns the older completed generation.
func TestSuspendHoldsOnlyADurableNewestGeneration(t *testing.T) {
	const completed = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
	for _, tc := range []struct {
		name    string
		newest  *atepb.Snapshot
		wantURI string
		wantErr error
		notErr  error
	}{
		{name: "completed", newest: durableSnapshot(2, "newest", completed), wantURI: "newest"},
		{name: "pending upload", newest: durableSnapshot(2, "newest", atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING), wantErr: api.ErrCheckpointPending, notErr: api.ErrInvalidCheckpoint},
		{name: "node-local", newest: pausedSnapshot(2), wantErr: api.ErrInvalidCheckpoint, notErr: api.ErrCheckpointPending},
	} {
		for _, memory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/memory=%v", tc.name, memory), func(t *testing.T) {
				a := runningActor()
				a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
				a.Status.WorkerAssignment = nil
				a.Status.Snapshots = []*atepb.Snapshot{durableSnapshot(1, "older", completed), tc.newest}
				a.Status.LastAssignedGeneration = 2
				f := &fakeControl{actor: a}
				desc := api.Descriptor{ID: "echo"}
				if memory {
					desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
				}
				b := substrate.New(FromClient(f), "space", substrate.ObjectRef{Name: "echo"}, desc)
				ref, err := b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
				if !slices.Contains(f.calls, "suspend:sess-x") {
					t.Fatalf("calls=%v want a SuspendActor", f.calls)
				}
				if tc.wantErr == nil {
					if err != nil || ref.ExternalURI != tc.wantURI {
						t.Fatalf("ref=%+v err=%v want URI %q", ref, err, tc.wantURI)
					}
					return
				}
				if !errors.Is(err, substrate.ErrSnapshotNotReady) || !errors.Is(err, tc.wantErr) || errors.Is(err, tc.notErr) || ref != (api.SnapshotRef{}) {
					t.Fatalf("ref=%+v err=%v want ErrSnapshotNotReady wrapping %v only", ref, err, tc.wantErr)
				}
			})
		}
	}
}

// Substrate v0.5.0 defines a paused actor by its snapshots (agent-substrate/substrate#2620): it is
// SUSPENDED and its newest generation is node-local. The adapter reports it suspended with no held
// checkpoint, so Restore and Create's attach refuse before ResumeActor rather than resume the
// older durable generation, and a pause in progress (SUSPENDING) is busy. A Suspend uploads the
// paused generation (a paused-origin suspend), after which Restore proceeds from it.
func TestPausedActorIsHeldOnlyAfterSuspendUploadsIt(t *testing.T) {
	const completed = atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
	for _, mode := range []string{"default", "volumes", "memory"} {
		t.Run(mode, func(t *testing.T) {
			older := durableSnapshot(1, "older", completed)
			paused := pausedSnapshot(2)
			a := runningActor()
			a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
			a.Status.WorkerAssignment = nil
			a.Status.Snapshots = []*atepb.Snapshot{older, paused}
			a.Status.LastAssignedGeneration = 2
			desc := api.Descriptor{ID: "echo"}
			var opts []substrate.Option
			switch mode {
			case "volumes":
				older.Fidelity, paused.Fidelity = atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
				opts = append(opts, substrate.WithVolumesOnly())
			case "memory":
				desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
			}
			f := &fakeControl{actor: a}
			ad := FromClient(f)
			info, err := ad.GetActor(t.Context(), substrate.ActorRef{Atespace: "space", Name: "sess-x"})
			if err != nil || info.Status != substrate.StatusSuspended || !info.SnapshotNotReady || info.SnapshotPending || info.Snapshot != "" {
				t.Fatalf("paused info=%+v err=%v want suspended, not ready, not pending, no URI", info, err)
			}
			b := substrate.New(ad, "space", substrate.ObjectRef{Name: "echo"}, desc, opts...)
			for _, uri := range []string{"older", ""} {
				f.calls = nil
				_, err := b.Restore(t.Context(), api.SnapshotRef{Local: "sess-x", ExternalURI: uri, Memory: mode != "volumes"})
				if !errors.Is(err, substrate.ErrSnapshotNotReady) || !errors.Is(err, api.ErrInvalidCheckpoint) || errors.Is(err, api.ErrCheckpointPending) ||
					!reflect.DeepEqual(f.calls, []string{"get:sess-x"}) {
					t.Fatalf("paused restore uri=%q: err=%v calls=%v", uri, err, f.calls)
				}
			}
			f.calls = nil
			if _, err := b.Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x"}); !errors.Is(err, substrate.ErrSnapshotNotReady) || !reflect.DeepEqual(f.calls, []string{"get:sess-x"}) {
				t.Fatalf("paused attach: err=%v calls=%v", err, f.calls)
			}

			// A pause in progress: SUSPENDING, newest generation neither local nor durable yet.
			a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDING
			paused.Locality = ""
			f.calls = nil
			if _, err := b.Restore(t.Context(), api.SnapshotRef{Local: "sess-x", ExternalURI: "older", Memory: mode != "volumes"}); !errors.Is(err, substrate.ErrActorBusy) || !reflect.DeepEqual(f.calls, []string{"get:sess-x"}) {
				t.Fatalf("pausing restore: err=%v calls=%v", err, f.calls)
			}

			// The paused-origin suspend promotes the local generation to a completed durable copy.
			a.Status.State = atepb.ActorState_ACTOR_STATE_SUSPENDED
			paused.Locality = "node-a"
			f.onSuspend = func() {
				paused.Locality = ""
				paused.DurableSnapshot = &atepb.SnapshotStorage{Status: completed, Object: &atepb.ObjectSnapshot{SnapshotUri: "promoted"}}
			}
			ref, err := b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
			if err != nil || ref.ExternalURI != "promoted" {
				t.Fatalf("suspend of paused actor: ref=%+v err=%v want the promoted generation", ref, err)
			}
			f.resumeErr = errors.New("resume reached")
			f.calls = nil
			if _, err := b.Restore(t.Context(), ref); !errors.Is(err, f.resumeErr) || !slices.Contains(f.calls, "resume:space/sess-x") {
				t.Fatalf("restore after upload: err=%v calls=%v", err, f.calls)
			}
		})
	}
}

// The paused shape decoded from v0.5.0 field numbers: ActorStatus.state = 1 (SUSPENDED = 4),
// snapshots = 11, last_assigned_generation = 12; Snapshot.generation = 2, locality = 6,
// durable_snapshot = 7. The newest generation is node-local, so the older completed one is not held.
func TestGetActorDecodesPausedWireFormat(t *testing.T) {
	var storage []byte
	storage = protowire.AppendVarint(protowire.AppendTag(storage, 1, protowire.VarintType), 2) // COMPLETED
	storage = append(storage, wireMessage(2, wireString(1, "older"))...)
	var gen1 []byte
	gen1 = protowire.AppendVarint(protowire.AppendTag(gen1, 2, protowire.VarintType), 1)
	gen1 = append(gen1, wireMessage(7, storage)...)
	var gen2 []byte
	gen2 = protowire.AppendVarint(protowire.AppendTag(gen2, 2, protowire.VarintType), 2)
	gen2 = append(gen2, wireString(6, "node-a")...)
	var st []byte
	st = protowire.AppendVarint(protowire.AppendTag(st, 1, protowire.VarintType), 4) // SUSPENDED
	st = append(st, wireMessage(11, gen1)...)
	st = append(st, wireMessage(11, gen2)...)
	st = protowire.AppendVarint(protowire.AppendTag(st, 12, protowire.VarintType), 2)

	info := decodeActorInfo(t, wireMessage(7, st))
	if info.Status != substrate.StatusSuspended || !info.SnapshotNotReady || info.SnapshotPending || info.Snapshot != "" {
		t.Fatalf("decoded info=%+v, want a suspended actor holding no durable checkpoint", info)
	}
}
