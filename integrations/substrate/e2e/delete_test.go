package e2e

import (
	"context"
	"sync"
	"testing"
	"time"

	atepb "go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	ateadapter "github.com/agent-primitives/sessions/integrations/substrate"
	"github.com/agent-primitives/sessions/integrations/substrate/internal/routertls"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

func TestSessionDeletePending(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-delete-stateless"))
	for _, cold := range []bool{false, true} {
		name := "running"
		if cold {
			name = "suspended"
		}
		t.Run(name, func(t *testing.T) {
			runDeleteHierarchy(t, f, echoTemplate, api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, cold)
		})
	}
}

func TestSessionDeletePendingOnMicroVM(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-delete-microvm"))
	for _, cold := range []bool{false, true} {
		name := "running"
		if cold {
			name = "suspended"
		}
		t.Run(name, func(t *testing.T) { runDeleteHierarchy(t, f, counterTemplate, counterDescriptor, cold) })
	}
}

// Retain the actual tag RPC response from the unmodified adapter. Read-only GetTag checks
// that Delete and descendant activity preserve the exact tag-owned snapshot.
type deleteControl struct {
	grpc.ClientConnInterface
	mu   sync.Mutex
	tags map[string]*atepb.Tag
}

func (c *deleteControl) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	err := c.ClientConnInterface.Invoke(ctx, method, args, reply, opts...)
	if err == nil && method == atepb.Control_CreateTag_FullMethodName {
		tag := reply.(*atepb.Tag)
		c.mu.Lock()
		c.tags[tag.GetMetadata().GetName()] = tag
		c.mu.Unlock()
	}
	return err
}
func (c *deleteControl) tag(name string) *atepb.Tag {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tags[name]
}

func runDeleteHierarchy(t *testing.T, f *fixture, template string, desc api.Descriptor, cold bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	if template == echoTemplate {
		f.ensureTemplate(t, echoTemplateSpec)
	} else {
		f.ensureTemplate(t, counterTemplateSpec)
	}
	observed := &deleteControl{ClientConnInterface: f.conn, tags: map[string]*atepb.Tag{}}
	backend := substrate.New(ateadapter.New(observed), f.atespace, substrate.ObjectRef{Atespace: f.tmplAtespace, Name: template}, desc, substrate.WithRouter(f.router))
	b := &deleteRuntime{Backend: backend}
	a := newDeleteAPI(t, b, placement.WithDialer(routertls.Dialer(routerCreds(t))))
	ctl := atepb.NewControlClient(f.conn)
	owned := []string{}
	// Cleanup is test-owned Control work AFTER assertions, never product Delete or a change to Stop.
	// Track only this case's UIDs; cold actors are deleted directly, running ones are first suspended.
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cleanupCancel()
		for i := len(owned) - 1; i >= 0; i-- {
			ref := &atepb.ObjectRef{Atespace: f.atespace, Name: owned[i]}
			actor, err := ctl.GetActor(cleanupCtx, &atepb.GetActorRequest{Actor: ref})
			if status.Code(err) == codes.NotFound {
				continue
			}
			if err != nil {
				t.Errorf("owned cleanup GetActor %s: %v", owned[i], err)
				continue
			}
			if actor.GetStatus().GetState() == atepb.ActorState_ACTOR_STATE_RUNNING {
				if _, err := ctl.SuspendActor(cleanupCtx, &atepb.SuspendActorRequest{Actor: ref}); err != nil {
					t.Errorf("owned cleanup Suspend %s: %v", owned[i], err)
					continue
				}
			}
			if _, err := ctl.DeleteActor(cleanupCtx, &atepb.DeleteActorRequest{Actor: ref}); err != nil {
				t.Errorf("owned cleanup DeleteActor %s: %v", owned[i], err)
			}
			if desc.Capabilities.Resumability == api.ResumabilityRequiresMemorySnapshot && observed.tag("fork-"+owned[i]) != nil {
				if _, err := ctl.DeleteTag(cleanupCtx, &atepb.DeleteTagRequest{Tag: &atepb.ObjectRef{Atespace: f.atespace, Name: "fork-" + owned[i]}}); err != nil {
					t.Errorf("owned cleanup pin %s: %v", owned[i], err)
				}
			}
		}
	}()
	parent := a.create(t, ctx, "delete-parent")
	owned = append(owned, parent)
	if got := a.exec(t, ctx, parent, "inc"); template == counterTemplate && got != "1" {
		t.Fatalf("initial RAM count=%q want 1", got)
	}
	// The existing echo pool has only two workers. Park setup actors through the product Suspend
	// operation while ACTIVE, without deleting them or changing the template's resource budget.
	if template == echoTemplate {
		if _, err := a.client.Suspend(ctx, parent); err != nil {
			t.Fatal(err)
		}
	}
	children := a.fork(t, ctx, parent, 2)
	owned = append(owned, children...)
	middle, sibling := children[0], children[1]
	if template == echoTemplate {
		if _, err := a.client.Suspend(ctx, middle); err != nil {
			t.Fatal(err)
		}
	}
	grandchild := a.fork(t, ctx, middle, 1)[0]
	owned = append(owned, grandchild)
	prefixSibling, prefixGrandchild := a.records(t, sibling), a.records(t, grandchild)
	if template == echoTemplate {
		for _, uid := range []string{sibling, grandchild} {
			if _, err := a.client.Suspend(ctx, uid); err != nil {
				t.Fatal(err)
			}
		}
	}
	deletionStatus(t, ctx, a, parent, cold)
	deletionStatus(t, ctx, a, middle, cold)
	actors := map[string]*atepb.Actor{}
	pins := map[string]*atepb.Tag{}
	for _, uid := range owned {
		actor, err := ctl.GetActor(ctx, &atepb.GetActorRequest{Actor: &atepb.ObjectRef{Atespace: f.atespace, Name: uid}})
		if err != nil {
			t.Fatalf("control actor %s: %v", uid, err)
		}
		if actor.GetMetadata().GetUid() == "" || actor.GetActorTemplate().GetName() != template || actor.GetActorTemplate().GetAtespace() != f.tmplAtespace {
			t.Fatalf("actor identity/template %s: %v", uid, actor)
		}
		if uid == parent || uid == middle {
			want := atepb.ActorState_ACTOR_STATE_RUNNING
			if cold {
				want = atepb.ActorState_ACTOR_STATE_SUSPENDED
			}
			if actor.GetStatus().GetState() != want || (!cold && (actor.GetStatus().GetWorkerAssignment().GetWorkerPodUid() == "" || len(actor.GetStatus().GetWorkerAssignment().GetWorkerPodIps()) == 0)) || (cold && (deleteHeldSnapshotURI(actor.GetStatus()) == "" || actor.GetStatus().GetWorkerAssignment() != nil)) {
				t.Fatalf("control state %s: %v", uid, actor)
			}
		}
		actors[uid] = actor
		if template == counterTemplate && uid != parent {
			pins[uid] = readDeletePin(t, ctx, ctl, observed, f.atespace, uid)
		}
	}
	if template == counterTemplate {
		for child, source := range map[string]string{middle: parent, sibling: parent, grandchild: middle} {
			if pins[child].GetSourceActor().GetAtespace() != f.atespace || pins[child].GetSourceActor().GetName() != source {
				t.Fatalf("pin %s does not retain actual source actor %s", child, source)
			}
		}
	}
	a.pending(t, ctx, b, parent)
	assertDeleteResources(t, ctx, ctl, observed, f.atespace, actors, pins)
	// Delete an intermediate ancestor too: its incoming pin AND its child's pin stay retained.
	a.pending(t, ctx, b, middle)
	assertDeleteResources(t, ctx, ctl, observed, f.atespace, actors, pins)
	listed, err := a.client.ListSessions(ctx, "delete-fixture")
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, s := range listed {
		visible[s.GetMetadata().GetUid()] = true
	}
	if visible[parent] || visible[middle] || !visible[sibling] || !visible[grandchild] {
		t.Fatalf("noncascade List=%v", visible)
	}
	// After proving running actors unchanged by Delete/retry/refusals, test-owned Control parking
	// releases the echo pool's two workers for descendants. This is NOT an effect of product Delete;
	// the actors and pins remain, and the journal's compute projection is deliberately not repaired.
	if template == echoTemplate && !cold {
		for _, uid := range []string{parent, middle} {
			ref := &atepb.ObjectRef{Atespace: f.atespace, Name: uid}
			if _, err := ctl.SuspendActor(ctx, &atepb.SuspendActorRequest{Actor: ref}); err != nil {
				t.Fatalf("fixture park retained actor %s: %v", uid, err)
			}
			actor, err := ctl.GetActor(ctx, &atepb.GetActorRequest{Actor: ref})
			if err != nil || actor.GetStatus().GetState() != atepb.ActorState_ACTOR_STATE_SUSPENDED || actor.GetMetadata().GetUid() != actors[uid].GetMetadata().GetUid() {
				t.Fatalf("fixture parking lost actor %s: %v %v", uid, actor, err)
			}
			actors[uid] = actor
		}
	}
	for _, uid := range []string{sibling, grandchild} {
		if template == counterTemplate {
			// These clones are already running; snapshot before Resume to test actual RAM recovery,
			// rather than relying on ResumeActor accepting an already-running actor.
			if _, err := a.client.Suspend(ctx, uid); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.client.Resume(ctx, uid, false); err != nil {
			t.Fatalf("surviving Resume %s: %v", uid, err)
		}
		got := a.exec(t, ctx, uid, "inc")
		want := "echo:inc"
		if template == counterTemplate {
			want = "2"
		}
		if got != want {
			t.Fatalf("surviving descendant %s output=%q want %q", uid, got, want)
		}
	}
	assertDeletePrefix(t, prefixSibling, a.records(t, sibling))
	assertDeletePrefix(t, prefixGrandchild, a.records(t, grandchild))
	if template == echoTemplate {
		if _, err := a.client.Suspend(ctx, sibling); err != nil {
			t.Fatal(err)
		}
	}
	ownFork := a.fork(t, ctx, grandchild, 1)[0]
	owned = append(owned, ownFork)
	wantOwn := "echo:inc"
	if template == counterTemplate {
		wantOwn = "3"
	}
	if got := a.exec(t, ctx, ownFork, "inc"); got != wantOwn {
		t.Fatalf("descendant own fork output=%q want %q", got, wantOwn)
	}
	if template == counterTemplate {
		pins[ownFork] = readDeletePin(t, ctx, ctl, observed, f.atespace, ownFork)
		if pins[ownFork].GetSourceActor().GetAtespace() != f.atespace || pins[ownFork].GetSourceActor().GetName() != grandchild {
			t.Fatal("own fork pin lost grandchild source identity")
		}
		for uid, before := range pins {
			after := readDeletePin(t, ctx, ctl, observed, f.atespace, uid)
			if !sameDeleteMessage(t, before, after) {
				t.Fatalf("descendant activity released/replaced pin %s", uid)
			}
		}
	}
	for _, uid := range []string{parent, middle} {
		actor, err := ctl.GetActor(ctx, &atepb.GetActorRequest{Actor: &atepb.ObjectRef{Atespace: f.atespace, Name: uid}})
		if err != nil || !sameDeleteMessage(t, actors[uid], actor) {
			t.Fatalf("ancestor actor changed during descendant execution: %v %v", actor, err)
		}
	}
	t.Logf("retained parent/intermediate actors, %d clone pins, independent sibling/grandchild/own-fork; owned actors=%v", len(pins), owned)
}

// The held snapshot is the newest completed durable copy, matching the pinned Control API.
func deleteHeldSnapshotURI(st *atepb.ActorStatus) string {
	var held *atepb.Snapshot
	for _, snap := range st.GetSnapshots() {
		if snap.GetDurableSnapshot().GetStatus() == atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED &&
			(held == nil || snap.GetGeneration() >= held.GetGeneration()) {
			held = snap
		}
	}
	return held.GetDurableSnapshot().GetObject().GetSnapshotUri()
}

func readDeletePin(t *testing.T, ctx context.Context, ctl atepb.ControlClient, observed *deleteControl, space, uid string) *atepb.Tag {
	t.Helper()
	tag := observed.tag("fork-" + uid)
	if tag == nil || tag.GetMetadata().GetAtespace() != space || tag.GetMetadata().GetUid() == "" || tag.GetStatus().GetSnapshot().GetDurableSnapshot().GetObject().GetSnapshotUri() == "" || tag.GetStatus().GetActorTemplateUid() == "" || tag.GetScope() != atepb.TagScope_TAG_SCOPE_ATESPACE {
		t.Fatalf("actual clone tag metadata %s: %v", uid, tag)
	}
	current, err := ctl.GetTag(ctx, &atepb.GetTagRequest{Tag: &atepb.ObjectRef{Atespace: space, Name: "fork-" + uid}})
	if err != nil || !sameDeleteMessage(t, tag, current) {
		t.Fatalf("pin no longer resolves exact tag-owned snapshot %s: %v %v", uid, current, err)
	}
	t.Logf("retained pin %s tagUID=%s snapshotURI=%s sourceActor=%s templateUID=%s", uid, tag.GetMetadata().GetUid(), tag.GetStatus().GetSnapshot().GetDurableSnapshot().GetObject().GetSnapshotUri(), tag.GetSourceActor().GetName(), tag.GetStatus().GetActorTemplateUid())
	return current
}
func assertDeleteResources(t *testing.T, ctx context.Context, ctl atepb.ControlClient, observed *deleteControl, space string, actors map[string]*atepb.Actor, pins map[string]*atepb.Tag) {
	t.Helper()
	for uid, before := range actors {
		after, err := ctl.GetActor(ctx, &atepb.GetActorRequest{Actor: &atepb.ObjectRef{Atespace: space, Name: uid}})
		if err != nil || !sameDeleteMessage(t, before, after) {
			t.Fatalf("Delete changed/removed actor %s: %v %v", uid, after, err)
		}
	}
	for uid, before := range pins {
		if after := readDeletePin(t, ctx, ctl, observed, space, uid); !sameDeleteMessage(t, before, after) {
			t.Fatalf("Delete released/changed pin %s", uid)
		}
	}
}
