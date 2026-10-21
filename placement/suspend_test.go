package placement_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// This control fake models the external actor registry, not the Runtime: suspending releases
// compute but retains the actor and its captured state; deleting makes restoration impossible.
// Snapshots and tags are actual registry entries so fork-pin assertions cannot pass vacuously.
// ResumeActor picks its source as substrate does: the actor's own snapshot if it holds one, else a
// fresh harness (the template's golden snapshot or a cold boot), so there is no per-call boot flag.
type suspendControl struct {
	actors           map[substrate.ActorRef]*suspendActor
	snapshots        map[substrate.SnapshotID]suspendSnapshot
	tags             map[substrate.SnapshotID]substrate.SnapshotID
	calls            []string
	snapshotErr      error
	snapshotFidelity substrate.SnapshotFidelity
	afterSuspend     func()
	afterResume      func()
	// beforeSuspend, when set, runs once as SuspendActor starts: after the backend's check and
	// before this suspend acts, the window another caller's suspend and resume can land in.
	beforeSuspend func()
	created       int // actors created so far, for server-assigned UIDs
}

type suspendActor struct {
	status    substrate.ActorStatus
	worker    bool
	turns     int
	snapshot  substrate.SnapshotID
	sourceTag substrate.SnapshotID
	template  substrate.ObjectRef
	uid       string
	// memoryLost models a resume that landed on a worker of another sandbox runtime and restored
	// the snapshot's volumes only; GetActor reports it as MemoryNotRestored while the actor runs.
	memoryLost bool
	// captures counts the actor's captures, for CaptureLineage and NoCapture.
	captures int
	// paused models a pause: the newest capture is node-local, so GetActor reports it not ready
	// and not pending, with no durable URI, as the adapter does on v0.5.0. A suspend uploads it.
	paused bool
}

// lineage is the actor's CaptureLineage: the capture its next suspend would take, or the one it
// holds when that suspend is a no-op.
func (a *suspendActor) lineage() string {
	n := a.captures
	if a.status != substrate.StatusSuspended || a.snapshot.Name == "" {
		n++
	}
	return fmt.Sprintf("capture-%d", n)
}

type suspendSnapshot struct {
	turns    int
	template substrate.ObjectRef
}

var _ substrate.ControlClient = (*suspendControl)(nil)
var _ substrate.SnapshotCloner = (*suspendControl)(nil)

func newSuspendControl() *suspendControl {
	return &suspendControl{
		actors:           make(map[substrate.ActorRef]*suspendActor),
		snapshots:        make(map[substrate.SnapshotID]suspendSnapshot),
		tags:             make(map[substrate.SnapshotID]substrate.SnapshotID),
		snapshotFidelity: substrate.SnapshotFidelityMemory,
	}
}

func (c *suspendControl) CreateActor(_ context.Context, ref substrate.ActorRef, template substrate.ObjectRef) error {
	c.calls = append(c.calls, "create:"+ref.Name)
	if _, exists := c.actors[ref]; exists {
		return fmt.Errorf("%w: %s", substrate.ErrActorExists, ref.Name)
	}
	c.actors[ref] = &suspendActor{status: substrate.StatusSuspended, template: template, uid: c.newUID()}
	return nil
}

func (c *suspendControl) ResumeActor(_ context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	c.calls = append(c.calls, "resume:"+ref.Name)
	a, exists := c.actors[ref]
	if !exists {
		return substrate.ActorInfo{}, fmt.Errorf("%w: %w", substrate.ErrActorNotFound, status.Error(codes.NotFound, "actor does not exist"))
	}
	if a.status == substrate.StatusSuspended && a.snapshot.Name == "" {
		a.turns = 0 // no snapshot of its own yet: a fresh harness
	} else if a.status == substrate.StatusSuspended {
		snapshot, exists := c.snapshots[a.snapshot]
		if !exists {
			return substrate.ActorInfo{}, status.Error(codes.NotFound, "snapshot does not exist")
		}
		if a.sourceTag.Name != "" {
			source, exists := c.tags[a.sourceTag]
			if _, captured := c.snapshots[source]; !exists || !captured {
				return substrate.ActorInfo{}, status.Error(codes.NotFound, "clone source does not exist")
			}
		}
		a.turns = snapshot.turns
	}
	a.status, a.worker = substrate.StatusRunning, true
	if c.afterResume != nil {
		hook := c.afterResume
		c.afterResume = nil
		hook()
	}
	return c.info(ref, a), nil
}

func (c *suspendControl) SuspendActor(_ context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	c.calls = append(c.calls, "suspend:"+ref.Name)
	if c.beforeSuspend != nil {
		hook := c.beforeSuspend
		c.beforeSuspend = nil
		hook()
	}
	if c.snapshotErr != nil {
		return substrate.ActorInfo{}, c.snapshotErr
	}
	a, exists := c.actors[ref]
	if !exists {
		return substrate.ActorInfo{}, status.Error(codes.NotFound, "actor does not exist")
	}
	if a.status == substrate.StatusSuspended && a.snapshot.Name != "" {
		a.paused = false // a paused-origin suspend uploads the paused generation
		return substrate.ActorInfo{Status: substrate.StatusSuspended, Snapshot: a.snapshot.Name, SnapshotFidelity: c.snapshotFidelity, UID: a.uid, CaptureLineage: a.lineage(), NoCapture: a.captures == 0}, nil
	}
	a.captures++
	a.snapshot = substrate.SnapshotID{Atespace: ref.Atespace, Name: fmt.Sprintf("snap-%s-%d", ref.Name, len(c.snapshots)+1)}
	c.snapshots[a.snapshot] = suspendSnapshot{turns: a.turns, template: a.template}
	a.status, a.worker = substrate.StatusSuspended, false
	if c.afterSuspend != nil {
		hook := c.afterSuspend
		c.afterSuspend = nil
		hook()
	}
	return substrate.ActorInfo{Status: substrate.StatusSuspended, Snapshot: a.snapshot.Name, SnapshotFidelity: c.snapshotFidelity, UID: a.uid, CaptureLineage: a.lineage(), NoCapture: a.captures == 0}, nil
}

func (c *suspendControl) DeleteActor(_ context.Context, ref substrate.ActorRef) error {
	c.calls = append(c.calls, "delete:"+ref.Name)
	a, exists := c.actors[ref]
	if !exists {
		return status.Error(codes.NotFound, "actor does not exist")
	}
	if a.worker || a.status != substrate.StatusSuspended {
		return status.Error(codes.FailedPrecondition, "actor must be suspended before deletion")
	}
	delete(c.actors, ref)
	return nil
}

// newUID mints the UID substrate assigns an actor at creation.
func (c *suspendControl) newUID() string {
	c.created++
	return fmt.Sprintf("uid-%d", c.created)
}

func (c *suspendControl) GetActor(_ context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	a, exists := c.actors[ref]
	if !exists {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	return c.info(ref, a), nil
}

func (c *suspendControl) info(ref substrate.ActorRef, a *suspendActor) substrate.ActorInfo {
	info := substrate.ActorInfo{Status: a.status, Snapshot: a.snapshot.Name, SnapshotFidelity: c.snapshotFidelity, UID: a.uid, CaptureLineage: a.lineage(), NoCapture: a.captures == 0}
	if a.paused {
		info.Snapshot, info.SnapshotNotReady = "", true
	}
	if a.worker {
		info.Worker = ref.Name
		info.MemoryNotRestored = a.memoryLost
	}
	return info
}

// TagActor tags the snapshot the suspended source actor holds now, as substrate's CreateTag does.
func (c *suspendControl) TagActor(_ context.Context, ref substrate.ActorRef, tag substrate.SnapshotID) error {
	c.calls = append(c.calls, "tag:"+ref.Name+"->"+tag.Name)
	a, exists := c.actors[ref]
	if !exists {
		return status.Error(codes.NotFound, "actor does not exist")
	}
	if a.status != substrate.StatusSuspended || a.worker {
		return status.Error(codes.FailedPrecondition, "source actor must be suspended")
	}
	if _, exists := c.snapshots[a.snapshot]; !exists {
		return status.Error(codes.NotFound, "snapshot does not exist")
	}
	if _, exists := c.tags[tag]; exists {
		return fmt.Errorf("%w: %s", substrate.ErrTagExists, tag.Name)
	}
	c.tags[tag] = a.snapshot
	return nil
}

func (c *suspendControl) CreateActorFromTag(_ context.Context, ref substrate.ActorRef, template substrate.ObjectRef, tag substrate.SnapshotID) (substrate.ActorInfo, error) {
	c.calls = append(c.calls, "clone:"+ref.Name+":from="+tag.Name)
	source, exists := c.tags[tag]
	if !exists {
		return substrate.ActorInfo{}, status.Error(codes.NotFound, "tag does not exist")
	}
	snapshot, exists := c.snapshots[source]
	if !exists {
		return substrate.ActorInfo{}, status.Error(codes.NotFound, "snapshot does not exist")
	}
	if template != snapshot.template {
		return substrate.ActorInfo{}, status.Error(codes.FailedPrecondition, "snapshot template mismatch")
	}
	if _, exists := c.actors[ref]; exists {
		return substrate.ActorInfo{}, status.Error(codes.AlreadyExists, "actor exists")
	}
	a := &suspendActor{
		status: substrate.StatusSuspended, snapshot: source, sourceTag: tag, template: template, uid: c.newUID(),
	}
	c.actors[ref] = a
	return substrate.ActorInfo{Status: a.status, UID: a.uid}, nil
}

func (c *suspendControl) DeleteTag(_ context.Context, tag substrate.SnapshotID) error {
	c.calls = append(c.calls, "untag:"+tag.Name)
	if _, exists := c.tags[tag]; !exists {
		return status.Error(codes.NotFound, "tag does not exist")
	}
	delete(c.tags, tag)
	return nil
}

// The dial seam substitutes only the external transport. A turn requires a registered, running
// actor. Memory-harness turns include a small RAM counter that Snapshot and clone must preserve.
type suspendHarness struct {
	ctl  *suspendControl
	ref  substrate.ActorRef
	desc api.Descriptor
}

func (h suspendHarness) Describe(context.Context) (api.Descriptor, error) { return h.desc, nil }

func (h suspendHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	a, exists := h.ctl.actors[h.ref]
	if !exists || a.status != substrate.StatusRunning || !a.worker {
		return status.Error(codes.Unavailable, "actor has no running worker")
	}
	if h.desc.Capabilities.Resumability != api.ResumabilityRequiresMemorySnapshot {
		return (echoagent.Harness{}).Run(ctx, start, sink)
	}
	a.turns++
	text := ""
	if len(start.Inputs) > 0 {
		text = start.Inputs[len(start.Inputs)-1].Text()
	}
	_, err := sink.Model(ctx, api.ModelRequest{Model: "echo", Messages: []api.Message{
		*api.TextMessage("user", fmt.Sprintf("%s:%d", text, a.turns)),
	}})
	return err
}

func newSuspendPlacer(t *testing.T, ctl *suspendControl, memory bool) (*placement.Placer, *substrate.Backend) {
	t.Helper()
	desc, err := (echoagent.Harness{}).Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if memory {
		desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	}
	backend := substrate.New(ctl, "space", substrate.ObjectRef{Name: "echo"}, desc)
	p := placement.New(backend, echoagent.Model, placement.WithDialer(func(inc api.Incarnation) (api.Harness, func() error, error) {
		// Every incarnation addresses the router; the call metadata names the actor.
		target := inc.CallMetadata[substrate.TargetActorHeader]
		atespace, name, ok := strings.Cut(target, "/")
		if inc.Address != substrate.DefaultRouterAddress || !ok || atespace != "space" {
			return nil, nil, fmt.Errorf("unexpected harness route %q to %q", inc.Address, target)
		}
		ref := substrate.ActorRef{Atespace: "space", Name: name}
		a, exists := ctl.actors[ref]
		if !exists || !a.worker {
			return nil, nil, status.Error(codes.Unavailable, "actor has no worker")
		}
		return suspendHarness{ctl: ctl, ref: ref, desc: desc}, func() error { return nil }, nil
	}))
	return p, backend
}

func newSuspendStore(t *testing.T) *sqlitelog.Store {
	t.Helper()
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func execSuspendTurn(t *testing.T, p *placement.Placer, log eventlog.Store, uid, text string) {
	t.Helper()
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(context.Background(), log, uid, []api.Message{*api.TextMessage("user", text)}, head); err != nil {
		t.Fatalf("exec %s: %v", uid, err)
	}
}

func suspendRecords(t *testing.T, log eventlog.Store) []eventlog.Record {
	t.Helper()
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("verify chain: %v", err)
	}
	return records
}

func assertSuspendOutputs(t *testing.T, log eventlog.Store, want []string) {
	t.Helper()
	var got []string
	for _, record := range suspendRecords(t, log) {
		if record.Event.Kind == api.EventOutput && record.Event.Message != nil {
			got = append(got, record.Event.Message.Text())
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outputs=%v want %v", got, want)
	}
}

func assertSuspendRef(t *testing.T, log eventlog.Store, ref api.SnapshotRef) {
	t.Helper()
	records := suspendRecords(t, log)
	last := records[len(records)-1].Event
	if last.Kind != api.EventLifecycle || last.Lifecycle == nil || last.Lifecycle.Kind != api.LifecycleSuspend ||
		last.Lifecycle.Snapshot == nil || *last.Lifecycle.Snapshot != ref {
		t.Fatalf("last record must carry the returned SUSPEND ref: %+v", last)
	}
}

// Overlapping operations must fail before changing compute or the journal. In particular, Exec
// inside SuspendActor must not restart the actor before the Placer records SUSPEND.
func TestPlacerSessionOperationsRejectOverlap(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, outer := range []string{"Exec", "Suspend", "Resume"} {
			for _, overlap := range []string{"Exec", "Suspend", "Resume"} {
				t.Run(fmt.Sprintf("memory=%v/%s/%s", memory, outer, overlap), func(t *testing.T) {
					ctx := context.Background()
					ctl := newSuspendControl()
					p, backend := newSuspendPlacer(t, ctl, memory)
					store := newSuspendStore(t)
					log, independent := store.Session("session"), store.Session("independent")
					execSuspendTurn(t, p, independent, "independent", "other")
					if outer != "Exec" {
						execSuspendTurn(t, p, log, "session", "first")
					}
					if outer == "Resume" {
						if _, err := p.Suspend(ctx, log, "session"); err != nil {
							t.Fatal(err)
						}
					}
					invoke := func(operation string, log eventlog.Store, uid string) error {
						switch operation {
						case "Exec":
							head, err := log.Head()
							if err != nil {
								return err
							}
							_, err = p.Exec(ctx, log, uid, []api.Message{*api.TextMessage("user", "next")}, head)
							return err
						case "Suspend":
							_, err := p.Suspend(ctx, log, uid)
							return err
						default:
							return p.Resume(ctx, log, uid)
						}
					}
					called := false
					hook := func() {
						called = true
						before := suspendRecords(t, log)
						actor := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "session"}]
						state := *actor
						if err := invoke(overlap, log, "session"); !errors.Is(err, placement.ErrSessionBusy) {
							t.Fatalf("overlapping %s during %s: want ErrSessionBusy, got %v", overlap, outer, err)
						}
						if *actor != state || !reflect.DeepEqual(suspendRecords(t, log), before) {
							t.Fatal("rejected overlap changed the actor or journal")
						}
						// One session's operation must not prevent another session from progressing.
						if err := invoke(overlap, independent, "independent"); err != nil {
							t.Fatalf("independent session %s during %s: %v", overlap, outer, err)
						}
					}
					if outer == "Suspend" {
						ctl.afterSuspend = hook
					} else {
						ctl.afterResume = hook
					}
					if err := invoke(outer, log, "session"); err != nil {
						t.Fatalf("outer %s: %v", outer, err)
					}
					if !called {
						t.Fatal("operation did not reach the overlap window")
					}
					state, err := backend.Status(ctx, api.Incarnation{ID: "session"})
					want := api.ComputeLive
					if outer == "Suspend" {
						want = api.ComputeCold
					}
					if err != nil || state != want {
						t.Fatalf("outer %s left compute=%s, err=%v; want %s", outer, state, err, want)
					}
					// The same operation can retry after the outer call releases the session guard.
					if err := invoke(overlap, log, "session"); err != nil {
						t.Fatalf("retry %s after %s: %v", overlap, outer, err)
					}
					suspendRecords(t, log)
					suspendRecords(t, independent)
				})
			}
		}
	}
}

func TestSubstrateSuspendResumePreservesActor(t *testing.T) {
	ctx := context.Background()
	ctl := newSuspendControl()
	p, backend := newSuspendPlacer(t, ctl, false)
	log := newSuspendStore(t).Session("session")
	execSuspendTurn(t, p, log, "session", "first")

	ref, err := p.Suspend(ctx, log, "session")
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if ref != (api.SnapshotRef{Local: "session", ExternalURI: "snap-session-1", Memory: true}) {
		t.Fatalf("snapshot ref=%+v", ref)
	}
	assertSuspendRef(t, log, ref)
	cold, coldErr := backend.Status(ctx, api.Incarnation{ID: "session"})
	a, retained := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "session"}]
	workerReleased := retained && !a.worker

	// Restore must make the retained actor usable by the next turn.
	if err := p.Resume(ctx, log, "session"); err != nil {
		t.Fatalf("resume suspended session (gRPC code %s): %v", status.Code(err), err)
	}
	if coldErr != nil || cold != api.ComputeCold || !retained || !workerReleased {
		t.Fatalf("suspension must retain a cold actor without a worker: state=%s err=%v retained=%v released=%v", cold, coldErr, retained, workerReleased)
	}
	execSuspendTurn(t, p, log, "session", "second")
	assertSuspendOutputs(t, log, []string{"echo:first", "echo:second"})
	var kinds []string
	for _, record := range suspendRecords(t, log) {
		if record.Event.Lifecycle != nil {
			kinds = append(kinds, string(record.Event.Lifecycle.Kind))
		} else {
			kinds = append(kinds, string(record.Event.Kind))
		}
	}
	wantKinds := []string{"EXECUTION_START", "INPUT", "MODEL_CALL", "OUTPUT", "END", "SUSPEND", "RESUME", "EXECUTION_START", "INPUT", "MODEL_CALL", "OUTPUT", "END"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("record order=%v want %v", kinds, wantKinds)
	}
	wantCalls := []string{"create:session", "resume:session", "suspend:session", "resume:session"}
	if !reflect.DeepEqual(ctl.calls, wantCalls) {
		t.Fatalf("lifecycle calls=%v want %v", ctl.calls, wantCalls)
	}
}

func TestResumeAcceptsMainEraStatelessSubstrateCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fidelity substrate.SnapshotFidelity
	}{
		{name: "volumes", fidelity: substrate.SnapshotFidelityVolumes},
		{name: "unknown", fidelity: substrate.SnapshotFidelityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := newSuspendControl()
			ctl.snapshotFidelity = tc.fidelity
			p, backend := newSuspendPlacer(t, ctl, false)
			log := newSuspendStore(t).Session("session")
			execSuspendTurn(t, p, log, "session", "first")

			// Main-era journals recorded Memory=true for every Substrate suspend, even for stateless
			// actors. Build that exact shape with one legacy SUSPEND record, not by layering it over a
			// newer journaled checkpoint.
			ref, err := backend.Snapshot(ctx, api.Incarnation{ID: "session"}, api.SnapshotExternal)
			if err != nil {
				t.Fatalf("backend snapshot: %v", err)
			}
			if ref.Memory {
				t.Fatalf("premise broken: ref=%+v, want current code to record Memory=false for %s", ref, tc.name)
			}
			head, err := log.Head()
			if err != nil {
				t.Fatalf("Head: %v", err)
			}
			recs := suspendRecords(t, log)
			last := recs[len(recs)-1]
			legacy := ref
			legacy.Memory = true
			if _, err := log.Append(head, last.Fence, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend, Snapshot: &legacy}}); err != nil {
				t.Fatalf("append legacy suspend: %v", err)
			}

			ctl.calls = nil
			if err := p.Resume(ctx, log, "session"); err != nil {
				t.Fatalf("resume legacy checkpoint (gRPC code %s): %v", status.Code(err), err)
			}
			if want := []string{"resume:session"}; !reflect.DeepEqual(ctl.calls, want) {
				t.Fatalf("resume calls=%v want %v", ctl.calls, want)
			}
			execSuspendTurn(t, p, log, "session", "second")
			assertSuspendOutputs(t, log, []string{"echo:first", "echo:second"})
		})
	}
}

func TestSubstrateSuspendThenExecWithoutResume(t *testing.T) {
	for _, tc := range []struct {
		name        string
		memory      bool
		wantOutputs []string
	}{
		{name: "stateless", wantOutputs: []string{"echo:first", "echo:second", "echo:third"}},
		{name: "memory", memory: true, wantOutputs: []string{"echo:first:1", "echo:second:2", "echo:third:3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctl := newSuspendControl()
			p, backend := newSuspendPlacer(t, ctl, tc.memory)
			log := newSuspendStore(t).Session("session")
			execSuspendTurn(t, p, log, "session", "first")
			ref, err := p.Suspend(ctx, log, "session")
			if err != nil {
				t.Fatalf("suspend: %v", err)
			}
			assertSuspendRef(t, log, ref)
			cold, coldErr := backend.Status(ctx, api.Incarnation{ID: "session"})
			actor := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "session"}]
			workerReleased := actor != nil && !actor.worker

			// Exec must restore via Create without an explicit Resume or a RESUME journal marker.
			execSuspendTurn(t, p, log, "session", "second")
			if coldErr != nil || cold != api.ComputeCold || !workerReleased {
				t.Fatalf("suspend must retain a cold actor without a worker: state=%s err=%v released=%v", cold, coldErr, workerReleased)
			}
			live, err := backend.Status(ctx, api.Incarnation{ID: "session"})
			if err != nil || live != api.ComputeLive || actor != ctl.actors[substrate.ActorRef{Atespace: "space", Name: "session"}] {
				t.Fatalf("Exec must restore the retained actor: state=%s err=%v", live, err)
			}
			execSuspendTurn(t, p, log, "session", "third")
			assertSuspendOutputs(t, log, tc.wantOutputs)
			var kinds []string
			for _, record := range suspendRecords(t, log) {
				if record.Event.Lifecycle != nil {
					kinds = append(kinds, string(record.Event.Lifecycle.Kind))
				} else {
					kinds = append(kinds, string(record.Event.Kind))
				}
			}
			wantKinds := []string{
				"EXECUTION_START", "INPUT", "MODEL_CALL", "OUTPUT", "END", "SUSPEND",
				"EXECUTION_START", "INPUT", "MODEL_CALL", "OUTPUT", "END",
				"EXECUTION_START", "INPUT", "MODEL_CALL", "OUTPUT", "END",
			}
			if !reflect.DeepEqual(kinds, wantKinds) {
				t.Fatalf("record order=%v want %v (no RESUME)", kinds, wantKinds)
			}
			wantCalls := []string{"create:session", "resume:session", "suspend:session", "resume:session"}
			if !reflect.DeepEqual(ctl.calls, wantCalls) {
				t.Fatalf("lifecycle calls=%v want %v", ctl.calls, wantCalls)
			}
		})
	}
}

func TestSuspendRetainsForkPinUntilExplicitStop(t *testing.T) {
	ctx := context.Background()
	ctl := newSuspendControl()
	p, backend := newSuspendPlacer(t, ctl, true)
	store := newSuspendStore(t)
	parent, child, sibling := store.Session("parent"), store.Session("child"), store.Session("sibling")
	execSuspendTurn(t, p, parent, "parent", "parent")
	head, err := parent.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Fork(ctx, parent, "parent", []placement.ForkChild{{UID: "child", Log: child}, {UID: "sibling", Log: sibling}}, head); err != nil {
		t.Fatalf("fork two children: %v", err)
	}
	childActor := substrate.ActorRef{Atespace: "space", Name: "child"}
	siblingActor := substrate.ActorRef{Atespace: "space", Name: "sibling"}
	childTag := substrate.SnapshotID{Atespace: "space", Name: "fork-child"}
	siblingTag := substrate.SnapshotID{Atespace: "space", Name: "fork-sibling"}
	source := substrate.SnapshotID{Atespace: "space", Name: "snap-parent-1"}
	for _, pair := range []struct {
		actor substrate.ActorRef
		tag   substrate.SnapshotID
	}{{childActor, childTag}, {siblingActor, siblingTag}} {
		a, exists := ctl.actors[pair.actor]
		if !exists || !a.worker || a.turns != 1 || a.sourceTag != pair.tag || a.snapshot != source || ctl.tags[pair.tag] != source {
			t.Fatalf("fork must establish a running clone and its own source pin: actor=%s state=%+v tags=%v", pair.actor.Name, a, ctl.tags)
		}
	}
	if _, exists := ctl.snapshots[source]; !exists {
		t.Fatal("fork source snapshot was not captured")
	}

	ref, err := p.Suspend(ctx, child, "child")
	if err != nil {
		t.Fatalf("suspend child: %v", err)
	}
	assertSuspendRef(t, child, ref)
	a, retained := ctl.actors[childActor]
	cold := retained && !a.worker && a.status == substrate.StatusSuspended
	pinRetained := ctl.tags[childTag] == source
	if err := p.Resume(ctx, child, "child"); err != nil {
		t.Fatalf("resume fork child (gRPC code %s): %v", status.Code(err), err)
	}
	if !cold || !pinRetained {
		t.Fatalf("suspend must retain the child's cold actor and established pin: cold=%v pin=%v", cold, pinRetained)
	}
	execSuspendTurn(t, p, child, "child", "child")
	assertSuspendOutputs(t, child, []string{"echo:parent:1", "echo:child:2"})

	ctl.calls = nil
	if err := backend.Stop(ctx, api.Incarnation{ID: "child"}); err != nil {
		t.Fatalf("explicit stop: %v", err)
	}
	if _, exists := ctl.actors[childActor]; exists {
		t.Fatal("explicit Stop must destroy the child actor")
	}
	if _, exists := ctl.tags[childTag]; exists {
		t.Fatal("explicit Stop must release the child's own pin")
	}
	if want := []string{"suspend:child", "delete:child", "untag:fork-child"}; !reflect.DeepEqual(ctl.calls, want) {
		t.Fatalf("destructive teardown=%v want %v", ctl.calls, want)
	}
	if _, err := backend.Restore(ctx, ref); !errors.Is(err, substrate.ErrSnapshotNotRestorable) || !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrActorNotFound) {
		t.Fatalf("Stop must remain destructive, restore err=%v", err)
	}
	if a, exists := ctl.actors[siblingActor]; !exists || !a.worker || a.sourceTag != siblingTag || ctl.tags[siblingTag] != source {
		t.Fatalf("stopping child invalidated sibling or pin: actor=%+v tags=%v", a, ctl.tags)
	}
	if _, exists := ctl.snapshots[source]; !exists {
		t.Fatal("stopping child invalidated the sibling's source snapshot")
	}
	// Actually restore and use the sibling, rather than only inspecting pin metadata.
	if _, err := p.Suspend(ctx, sibling, "sibling"); err != nil {
		t.Fatal(err)
	}
	if err := p.Resume(ctx, sibling, "sibling"); err != nil {
		t.Fatalf("sibling resume after child teardown: %v", err)
	}
	execSuspendTurn(t, p, sibling, "sibling", "sibling")
	assertSuspendOutputs(t, sibling, []string{"echo:parent:1", "echo:sibling:2"})
	for _, log := range []eventlog.Store{parent, child, sibling} {
		suspendRecords(t, log)
	}
}

// Only the selected persistence boundary fails; all other log operations use real SQLite.
type suspendFailStore struct {
	eventlog.Store
	operation string
	err       error
}

func (s suspendFailStore) NewFence() (int64, error) {
	if s.operation == "fence" {
		return 0, s.err
	}
	return s.Store.NewFence()
}

func (s suspendFailStore) Head() (int64, error) {
	if s.operation == "head" {
		return 0, s.err
	}
	return s.Store.Head()
}

func (s suspendFailStore) Read(from int64) ([]eventlog.Record, error) {
	if s.operation == "read" {
		return nil, s.err
	}
	return s.Store.Read(from)
}

func (s suspendFailStore) Append(seq, fence int64, ev api.Event) (eventlog.Record, error) {
	if s.operation == "append" {
		return eventlog.Record{}, s.err
	}
	return s.Store.Append(seq, fence, ev)
}

func TestSuspendFailuresPreserveActorAndJournal(t *testing.T) {
	for _, operation := range []string{"read", "snapshot", "fence", "head", "append"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			ctl := newSuspendControl()
			p, _ := newSuspendPlacer(t, ctl, false)
			log := newSuspendStore(t).Session("session")
			execSuspendTurn(t, p, log, "session", "first")
			before := suspendRecords(t, log)
			original := errors.New("injected " + operation + " failure")
			var failing eventlog.Store = log
			if operation == "snapshot" {
				ctl.snapshotErr = original
			} else {
				failing = suspendFailStore{Store: log, operation: operation, err: original}
			}
			ctl.calls = nil
			ref, err := p.Suspend(ctx, failing, "session")
			if !errors.Is(err, original) || ref != (api.SnapshotRef{}) {
				t.Fatalf("suspend must return the original failure and no ref: ref=%+v err=%v", ref, err)
			}
			if after := suspendRecords(t, log); !reflect.DeepEqual(after, before) {
				t.Fatal("failed suspend changed the journal or invented a SUSPEND record")
			}
			// A journal that cannot be read gives no instance to check, so compute is not touched.
			wantCalls := []string{"suspend:session"}
			if operation == "read" {
				wantCalls = nil
			}
			if !reflect.DeepEqual(ctl.calls, wantCalls) {
				t.Fatalf("failure must not invoke destructive teardown: calls=%v want %v", ctl.calls, wantCalls)
			}
			a, exists := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "session"}]
			if !exists {
				t.Fatal("failed suspend destroyed the actor")
			}
			if operation == "snapshot" || operation == "read" {
				if !a.worker || a.status != substrate.StatusRunning || len(ctl.snapshots) != 0 {
					t.Fatal("failed snapshot changed compute or captured a false snapshot")
				}
			} else {
				if a.worker || a.status != substrate.StatusSuspended {
					t.Fatal("journal failure must preserve the captured cold actor, without automatic compensation")
				}
				if _, exists := ctl.snapshots[a.snapshot]; !exists {
					t.Fatal("journal failure lost captured state")
				}
				// The existing session-handle fallback can still restore this retained actor.
				if err := p.Resume(ctx, log, "session"); err != nil {
					t.Fatalf("resume after failed SUSPEND persistence: %v", err)
				}
				execSuspendTurn(t, p, log, "session", "second")
				assertSuspendOutputs(t, log, []string{"echo:first", "echo:second"})
			}
		})
	}
}

// A memory-tier session whose journal records no compute instance (a journal written before
// EXECUTION_START carried compute_instance, or one with no turn yet) is still checked for lost
// memory before the suspend: after a Restore refused on another sandbox runtime, the actor runs
// without its RAM, and suspending it would record that as the session's MEMORY checkpoint, which
// the next Restore would accept. A stateless session has nothing in RAM to lose and suspends.
func TestSuspendRefusesLostMemoryWithoutARecordedInstance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory bool
		legacy bool
	}{
		{name: "memory, journal without compute_instance", memory: true, legacy: true},
		{name: "memory, no turn yet", memory: true},
		{name: "stateless, journal without compute_instance", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := newSuspendControl()
			p, _ := newSuspendPlacer(t, ctl, tc.memory)
			log := newSuspendStore(t).Session("session")
			if tc.legacy {
				desc, err := (echoagent.Harness{}).Describe(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				fence, err := log.NewFence()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := log.Append(0, fence, api.Event{ExecutionID: "e1", Kind: api.EventExecutionStart,
					ExecutionStart: &api.ExecutionStart{Harness: desc.ID, HarnessVersion: desc.Version}}); err != nil {
					t.Fatal(err)
				}
			}
			ref := substrate.ActorRef{Atespace: "space", Name: "session"}
			ctl.actors[ref] = &suspendActor{status: substrate.StatusRunning, worker: true, uid: ctl.newUID(), memoryLost: true}
			before := len(suspendRecords(t, log))

			got, err := p.Suspend(t.Context(), log, "session")
			if !tc.memory {
				if err != nil {
					t.Fatalf("stateless suspend: %v", err)
				}
				assertSuspendRef(t, log, got)
				return
			}
			if !errors.Is(err, api.ErrMemoryStateLost) || got != (api.SnapshotRef{}) {
				t.Fatalf("suspend: ref=%+v err=%v, want api.ErrMemoryStateLost", got, err)
			}
			if len(ctl.calls) != 0 {
				t.Fatalf("calls=%v: the actor must not be suspended", ctl.calls)
			}
			if a := ctl.actors[ref]; a.status != substrate.StatusRunning || !a.worker || a.snapshot.Name != "" {
				t.Fatalf("actor=%+v: no snapshot may be taken", a)
			}
			if after := len(suspendRecords(t, log)); after != before {
				t.Fatalf("journal grew from %d to %d records: no SUSPEND may be recorded", before, after)
			}
		})
	}
}

// A memory-tier Suspend refuses a capture it did not check (another caller suspended the actor and
// resumed it on a worker that restored only its files, in between), and the actor is left holding
// that capture. The refusal sticks: Exec, Resume, a new Suspend and a stateful Fork each compare
// the actor with the checkpoint the journal last recorded and refuse it with
// api.ErrMemoryStateLost, before resuming, suspending or tagging it, whether it is still suspended
// or something (the router) resumed it since, and nothing is journaled. That holds for a session
// suspended before and for one whose first Suspend was the refused one. A stateless session has no
// such check and carries on.
func TestRefusedCaptureStaysRefusedOnEveryPath(t *testing.T) {
	for _, tc := range []struct {
		name              string
		memory, suspended bool // suspended: the session has a recorded SUSPEND before the refused one
	}{
		{name: "memory, suspended before", memory: true, suspended: true},
		{name: "memory, first suspend", memory: true},
		{name: "stateless", suspended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			ctl := newSuspendControl()
			p, _ := newSuspendPlacer(t, ctl, tc.memory)
			store := newSuspendStore(t)
			log := store.Session("session")
			ref := substrate.ActorRef{Atespace: "space", Name: "session"}
			execSuspendTurn(t, p, log, "session", "first")
			if tc.suspended {
				if _, err := p.Suspend(ctx, log, "session"); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				execSuspendTurn(t, p, log, "session", "second")
			}
			// Between the check and this suspend, another caller captures the actor and resumes it on
			// a worker that restores only its files: its RAM no longer holds the session's turns.
			ctl.beforeSuspend = func() {
				a := ctl.actors[ref]
				a.captures++
				a.snapshot = substrate.SnapshotID{Atespace: "space", Name: "snap-foreign"}
				ctl.snapshots[a.snapshot] = suspendSnapshot{turns: a.turns, template: a.template}
				a.turns = 0
			}
			_, err := p.Suspend(ctx, log, "session")
			if !tc.memory {
				if err != nil {
					t.Fatalf("stateless suspend: %v", err)
				}
				execSuspendTurn(t, p, log, "session", "third")
				return
			}
			if !errors.Is(err, api.ErrMemoryStateLost) {
				t.Fatalf("suspend: %v, want api.ErrMemoryStateLost", err)
			}
			head, err := log.Head()
			if err != nil {
				t.Fatal(err)
			}
			child := store.Session("child")
			paths := map[string]func() error{
				"exec": func() error {
					_, err := p.Exec(ctx, log, "session", []api.Message{*api.TextMessage("user", "next")}, head)
					return err
				},
				"resume":  func() error { return p.Resume(ctx, log, "session") },
				"suspend": func() error { _, err := p.Suspend(ctx, log, "session"); return err },
				"fork": func() error {
					return p.Fork(ctx, log, "session", []placement.ForkChild{{UID: "child", Log: child}}, head)
				},
			}
			for _, woken := range []bool{false, true} {
				if woken {
					// The router resumes an actor on any request addressed to it, from the snapshot
					// it holds: the refused capture.
					a := ctl.actors[ref]
					a.status, a.worker = substrate.StatusRunning, true
				}
				for _, name := range []string{"exec", "resume", "suspend", "fork"} {
					ctl.calls = nil
					if err := paths[name](); !errors.Is(err, api.ErrMemoryStateLost) {
						t.Fatalf("woken=%v %s: %v, want api.ErrMemoryStateLost", woken, name, err)
					}
					if len(ctl.calls) != 0 {
						t.Fatalf("woken=%v %s: calls=%v, want no resume, suspend or tag", woken, name, ctl.calls)
					}
					if got, err := log.Head(); err != nil || got != head {
						t.Fatalf("woken=%v %s: head %d (err %v), want %d: nothing may be journaled", woken, name, got, err, head)
					}
					if _, exists := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "child"}]; exists {
						t.Fatalf("woken=%v %s created a fork child", woken, name)
					}
				}
			}
		})
	}
}

// outsideSuspend is a suspend by a caller other than sessions, such as a gateway that suspends idle
// actors (agent-substrate/always-on-agent): a running actor is captured and left holding that
// capture, which no SUSPEND names; a suspended one is left as it is.
func (c *suspendControl) outsideSuspend(ref substrate.ActorRef) {
	a := c.actors[ref]
	if a.status == substrate.StatusSuspended {
		return
	}
	a.captures++
	a.snapshot = substrate.SnapshotID{Atespace: ref.Atespace, Name: fmt.Sprintf("snap-%s-%d", ref.Name, len(c.snapshots)+1)}
	c.snapshots[a.snapshot] = suspendSnapshot{turns: a.turns, template: a.template}
	a.status, a.worker = substrate.StatusSuspended, false
}

// outsidePause is a pause by a caller other than sessions: a running actor is captured to a
// node-local snapshot and left suspended on it; a suspended one is left as it is.
func (c *suspendControl) outsidePause(ref substrate.ActorRef) {
	a := c.actors[ref]
	if a.status == substrate.StatusSuspended {
		return
	}
	c.outsideSuspend(ref)
	a.paused = true
}

// outsideResume is the router resuming the actor for a request addressed to it, from the snapshot
// it holds, on a worker of the same sandbox runtime: its memory comes back.
func (c *suspendControl) outsideResume(ref substrate.ActorRef) {
	a := c.actors[ref]
	if a.status == substrate.StatusRunning {
		return
	}
	if snapshot, ok := c.snapshots[a.snapshot]; ok {
		a.turns = snapshot.turns
	}
	a.status, a.worker, a.paused = substrate.StatusRunning, true, false
}

// Where the router resumes suspended actors on demand and a gateway suspends idle ones, another
// caller's suspend and resume are the normal path. A router resume of the recorded checkpoint is
// harmless: the next turn attaches and the session carries on. Any capture sessions did not take
// ends a memory-tier session: repeated suspend and resume cycles between Suspend's check and its
// SuspendActor, a router resume of a suspended session in that window, or a later idle suspend by
// someone else. Exec, Resume, Suspend and Fork each refuse with api.ErrMemoryStateLost, with no
// control call and nothing journaled, and keep refusing round after round of the router waking the
// actor and the gateway suspending it again. The memory survived every resume here: the refusal is
// the cost of failing closed. A stateless session carries on.
func TestOutsideSuspendsEndAMemorySession(t *testing.T) {
	ref := substrate.ActorRef{Atespace: "space", Name: "session"}
	cycles := func(ctl *suspendControl, n int) {
		for range n {
			ctl.outsideSuspend(ref)
			ctl.outsideResume(ref)
		}
	}
	for _, tc := range []struct {
		name string
		// window runs in Suspend's window, between its check and its SuspendActor; later runs with no
		// sessions operation in flight. The session was suspended and resumed once before either,
		// unless firstSuspend, and is left suspended if suspended.
		window, later           func(ctl *suspendControl)
		firstSuspend, suspended bool
		refused                 bool
		// memoryOnly skips the stateless run: a paused actor is not ready for any tier until a
		// Suspend uploads it (TestPausedActorIsHeldOnlyAfterSuspendUploadsIt in the adapter).
		memoryOnly bool
	}{
		{name: "router resumes only", suspended: true, later: func(ctl *suspendControl) { ctl.outsideResume(ref) }},
		{name: "one outside suspend in the window", window: func(ctl *suspendControl) { ctl.outsideSuspend(ref) }},
		{name: "suspend and resume cycles in the window", refused: true, window: func(ctl *suspendControl) { cycles(ctl, 3) }},
		{name: "cycles in the window, first suspend", firstSuspend: true, refused: true, window: func(ctl *suspendControl) { cycles(ctl, 3) }},
		{name: "suspended session woken by the router in the window", suspended: true, refused: true, window: func(ctl *suspendControl) { ctl.outsideResume(ref) }},
		{name: "idle suspend by someone else later", refused: true, later: func(ctl *suspendControl) { ctl.outsideSuspend(ref) }},
		{name: "pause by someone else later", refused: true, memoryOnly: true, later: func(ctl *suspendControl) { ctl.outsidePause(ref) }},
	} {
		for _, memory := range []bool{true, false} {
			if tc.memoryOnly && !memory {
				continue
			}
			t.Run(fmt.Sprintf("%s/memory=%v", tc.name, memory), func(t *testing.T) {
				ctx := t.Context()
				ctl := newSuspendControl()
				p, _ := newSuspendPlacer(t, ctl, memory)
				store := newSuspendStore(t)
				log := store.Session("session")
				execSuspendTurn(t, p, log, "session", "first")
				if !tc.firstSuspend {
					if _, err := p.Suspend(ctx, log, "session"); err != nil {
						t.Fatalf("suspend: %v", err)
					}
					if !tc.suspended {
						if err := p.Resume(ctx, log, "session"); err != nil {
							t.Fatalf("resume: %v", err)
						}
						execSuspendTurn(t, p, log, "session", "second")
					}
				}
				if tc.window != nil {
					ctl.beforeSuspend = func() { tc.window(ctl) }
					_, err := p.Suspend(ctx, log, "session")
					if memory && tc.refused {
						if !errors.Is(err, api.ErrMemoryStateLost) {
							t.Fatalf("suspend: %v, want api.ErrMemoryStateLost", err)
						}
					} else if err != nil {
						t.Fatalf("suspend: %v", err)
					}
				}
				if tc.later != nil {
					tc.later(ctl)
				}
				if !memory || !tc.refused {
					// The session carries on: its next turn, a Suspend and Resume, and a Fork.
					execSuspendTurn(t, p, log, "session", "next")
					if _, err := p.Suspend(ctx, log, "session"); err != nil {
						t.Fatalf("suspend after: %v", err)
					}
					if err := p.Resume(ctx, log, "session"); err != nil {
						t.Fatalf("resume after: %v", err)
					}
					head, err := log.Head()
					if err != nil {
						t.Fatal(err)
					}
					if err := p.Fork(ctx, log, "session", []placement.ForkChild{{UID: "child", Log: store.Session("child")}}, head); err != nil {
						t.Fatalf("fork after: %v", err)
					}
					if memory {
						// The RAM counter shows the turns ran on the session's state, not on a fresh harness.
						outputs := []string{"echo:first:1"}
						if !tc.firstSuspend && !tc.suspended {
							outputs = append(outputs, "echo:second:2")
						}
						outputs = append(outputs, fmt.Sprintf("echo:next:%d", len(outputs)+1))
						assertSuspendOutputs(t, log, outputs)
					}
					return
				}
				head, err := log.Head()
				if err != nil {
					t.Fatal(err)
				}
				child := store.Session("child")
				paths := []struct {
					name string
					run  func() error
				}{
					{"exec", func() error {
						_, err := p.Exec(ctx, log, "session", []api.Message{*api.TextMessage("user", "next")}, head)
						return err
					}},
					{"resume", func() error { return p.Resume(ctx, log, "session") }},
					{"suspend", func() error { _, err := p.Suspend(ctx, log, "session"); return err }},
					{"fork", func() error {
						return p.Fork(ctx, log, "session", []placement.ForkChild{{UID: "child", Log: child}}, head)
					}},
				}
				for round := range 3 {
					for _, woken := range []bool{false, true} {
						if woken {
							ctl.outsideResume(ref) // a message for the actor reaches the router
						} else if round > 0 {
							ctl.outsideSuspend(ref) // the gateway suspends it once idle
						}
						for _, path := range paths {
							ctl.calls = nil
							if err := path.run(); !errors.Is(err, api.ErrMemoryStateLost) {
								t.Fatalf("round %d woken=%v %s: %v, want api.ErrMemoryStateLost", round, woken, path.name, err)
							}
							if len(ctl.calls) != 0 {
								t.Fatalf("round %d woken=%v %s: calls=%v, want no resume, suspend or tag", round, woken, path.name, ctl.calls)
							}
							if got, err := log.Head(); err != nil || got != head {
								t.Fatalf("round %d woken=%v %s: head %d (err %v), want %d: nothing may be journaled", round, woken, path.name, got, err, head)
							}
							if _, exists := ctl.actors[substrate.ActorRef{Atespace: "space", Name: "child"}]; exists {
								t.Fatalf("round %d woken=%v %s created a fork child", round, woken, path.name)
							}
						}
					}
				}
			})
		}
	}
}
