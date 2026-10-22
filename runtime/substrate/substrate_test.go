package substrate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// mockControl records the ordered control-plane calls and returns canned actor state, so tests can
// assert the Runtime→substrate mapping without a real ate-api-server.
//
// It models substrate's actor REGISTRY, not just its replies: an actor that was never created is
// reported as ErrActorNotFound. That fidelity matters — a mock which accepted every CreateActor is
// what let an unconditional create-then-cold-boot look correct here while failing on a real cluster
// the moment a session took a second turn.
type mockControl struct {
	calls  []string
	status substrate.ActorStatus
	info   substrate.ActorInfo
	actors map[string]substrate.ActorStatus
	// snapshots is each actor's current external snapshot handle, as GetActor reports it.
	snapshots map[string]string
	// afterGet, when set, runs after GetActor answers, so a test can move an actor's state in the
	// window between a check and the call it guards. afterCreate runs after CreateActor creates an
	// actor, in the window before the backend resumes it.
	afterGet    func(name string)
	afterCreate func(name string)
	// resumeErr, when set, fails ResumeActor; getErr fails GetActor.
	resumeErr error
	getErr    error
	// uids is each actor's server-assigned UID, minted the first time the actor exists and dropped
	// when it is deleted, so an actor created again under a name reports a new one.
	uids    map[string]string
	created int
	// suspendErr fails SuspendActor; getNoUID and suspendNoUID make GetActor and SuspendActor report
	// no UID, as a ControlClient that breaks its contract would.
	suspendErr        error
	getNoUID          bool
	suspendNoUID      bool
	suspendNoSnapshot bool
	// gens counts each actor's captures and stamps records the runtime its newest one was taken
	// on; runtime is the sandbox runtime of the worker a running actor holds ("" is "rt-a"). They
	// model CaptureLineage the way the adapter derives it, so a capture by another caller, or a
	// move to another runtime, changes it. getNoLineage and suspendNoLineage report none.
	gens             map[string]int
	stamps           map[string]string
	runtime          string
	getNoLineage     bool
	suspendNoLineage bool
	// captureUnknown makes NoCapture false whatever the actor holds, as a ControlClient that cannot
	// tell whether a snapshot was captured from the actor reports it. resumeNoSnapshot makes
	// ResumeActor report no Snapshot, as a ControlClient whose ResumeActor omits it does.
	captureUnknown   bool
	resumeNoSnapshot bool
}

// workerRuntime is the sandbox runtime of the worker a running actor holds.
func (m *mockControl) workerRuntime() string {
	if m.runtime == "" {
		return "rt-a"
	}
	return m.runtime
}

// lineage is the CaptureLineage of an actor at st: the next capture's generation and runtime on a
// worker, the newest capture's otherwise.
func (m *mockControl) lineage(name string, st substrate.ActorStatus) string {
	if st == substrate.StatusRunning {
		return fmt.Sprintf("gen-%d@%s", m.gens[name]+1, m.workerRuntime())
	}
	return fmt.Sprintf("gen-%d@%s", m.gens[name], m.stamps[name])
}

// capture records a capture of a running actor on its worker's runtime, as a suspend or pause
// by any caller does.
func (m *mockControl) capture(name string) {
	if m.gens == nil {
		m.gens, m.stamps = map[string]int{}, map[string]string{}
	}
	m.gens[name]++
	m.stamps[name] = m.workerRuntime()
}

// statusOf is the actor's status as GetActor reports it.
func (m *mockControl) statusOf(name string) substrate.ActorStatus {
	if st, ok := m.actors[name]; ok {
		return st
	}
	return m.status
}

// exists records an actor at a status, so GetActor reports it as already placed.
func (m *mockControl) exists(name string, st substrate.ActorStatus) {
	if m.actors == nil {
		m.actors = map[string]substrate.ActorStatus{}
	}
	m.actors[name] = st
	m.uid(name)
}

// uid returns the actor's UID, minting one for an actor that has none yet.
func (m *mockControl) uid(name string) string {
	if m.uids == nil {
		m.uids = map[string]string{}
	}
	if _, ok := m.uids[name]; !ok {
		m.created++
		m.uids[name] = fmt.Sprintf("uid-%s-%d", name, m.created)
	}
	return m.uids[name]
}

func (m *mockControl) CreateActor(ctx context.Context, a substrate.ActorRef, _ substrate.ObjectRef) error {
	m.calls = append(m.calls, "create:"+a.Name)
	if _, ok := m.actors[a.Name]; ok {
		return fmt.Errorf("%w: %q", substrate.ErrActorExists, a.Name)
	}
	m.exists(a.Name, substrate.StatusRunning)
	if m.afterCreate != nil {
		m.afterCreate(a.Name)
	}
	return nil
}
func (m *mockControl) ResumeActor(ctx context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	m.calls = append(m.calls, "resume:"+a.Name)
	if m.resumeErr != nil {
		return substrate.ActorInfo{}, m.resumeErr
	}
	m.exists(a.Name, substrate.StatusRunning)
	info := m.info
	if info.UID == "" {
		info.UID = m.uid(a.Name)
	}
	// The actor as the resume left it, as GetActor would report it: the snapshot it restored from
	// stays its newest until its next capture.
	if info.Snapshot == "" && !m.resumeNoSnapshot {
		info.Snapshot = m.snapshots[a.Name]
	}
	info.CaptureLineage = m.lineage(a.Name, substrate.StatusRunning)
	info.NoCapture = m.noCapture(a.Name)
	return info, nil
}
func (m *mockControl) SuspendActor(ctx context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	if err := ctx.Err(); err != nil {
		return substrate.ActorInfo{}, err
	}
	m.calls = append(m.calls, "suspend:"+a.Name)
	if m.suspendErr != nil {
		return substrate.ActorInfo{}, m.suspendErr
	}
	snap := "gs://snap/" + a.Name
	if m.statusOf(a.Name) == substrate.StatusRunning {
		m.capture(a.Name)
		snap = m.captureURI(a.Name)
	} else if held := m.snapshots[a.Name]; held != "" {
		snap = held // suspending a suspended actor is a no-op that keeps its snapshot
	}
	m.exists(a.Name, substrate.StatusSuspended)
	if m.suspendNoSnapshot {
		snap = ""
	}
	m.holds(a.Name, snap)
	uid := m.uid(a.Name)
	if m.suspendNoUID {
		uid = ""
	}
	lineage := m.lineage(a.Name, substrate.StatusSuspended)
	if m.suspendNoLineage {
		lineage = ""
	}
	return substrate.ActorInfo{Status: substrate.StatusSuspended, Worker: m.info.Worker, Snapshot: snap, SnapshotFidelity: m.info.SnapshotFidelity, UID: uid, CaptureLineage: lineage, NoCapture: m.noCapture(a.Name)}, nil
}

// captureURI is the external snapshot of the actor's newest capture. Every capture gets its own, as
// substrate names each under a fresh snapshot name; the first keeps the plain form.
func (m *mockControl) captureURI(name string) string {
	if m.gens[name] <= 1 {
		return "gs://snap/" + name
	}
	return fmt.Sprintf("gs://snap/%s/gen-%d", name, m.gens[name])
}

// noCapture is ActorInfo.NoCapture: no capture of the actor was recorded (see capture), unless a
// test says it cannot tell.
func (m *mockControl) noCapture(name string) bool {
	return m.gens[name] == 0 && !m.captureUnknown
}

// holds records the external snapshot an actor currently holds.
func (m *mockControl) holds(name, snapshot string) {
	if m.snapshots == nil {
		m.snapshots = map[string]string{}
	}
	m.snapshots[name] = snapshot
}
func (m *mockControl) DeleteActor(ctx context.Context, a substrate.ActorRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.calls = append(m.calls, "delete:"+a.Name)
	m.delete(a.Name)
	return nil
}
func (m *mockControl) GetActor(ctx context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	m.calls = append(m.calls, "get:"+a.Name)
	if m.afterGet != nil {
		defer m.afterGet(a.Name)
	}
	if m.getErr != nil {
		return substrate.ActorInfo{}, m.getErr
	}
	lineage := ""
	if !m.getNoLineage {
		lineage = m.lineage(a.Name, m.statusOf(a.Name))
	}
	if st, ok := m.actors[a.Name]; ok {
		uid := m.uid(a.Name)
		if m.getNoUID {
			uid = ""
		}
		fidelity := m.info.SnapshotFidelity
		if m.info.NoSnapshotHistory && m.snapshots[a.Name] == "" {
			fidelity = substrate.SnapshotFidelityUnknown
		}
		return substrate.ActorInfo{Status: st, Worker: m.info.Worker, Snapshot: m.snapshots[a.Name], SnapshotFidelity: fidelity, SnapshotNotReady: m.info.SnapshotNotReady, SnapshotPending: m.info.SnapshotPending, NoSnapshotHistory: m.info.NoSnapshotHistory, MemoryNotRestored: m.info.MemoryNotRestored, UID: uid, CaptureLineage: lineage, NoCapture: m.noCapture(a.Name)}, nil
	}
	if m.status != substrate.StatusUnknown { // canned state for the suspend/restore cases
		return substrate.ActorInfo{Status: m.status, Worker: m.info.Worker, Snapshot: m.snapshots[a.Name], SnapshotFidelity: m.info.SnapshotFidelity, MemoryNotRestored: m.info.MemoryNotRestored, UID: m.uid(a.Name), CaptureLineage: lineage, NoCapture: m.noCapture(a.Name)}, nil
	}
	return substrate.ActorInfo{}, substrate.ErrActorNotFound
}

// running is what a successful ResumeActor reports: the actor on a worker.
func running(worker string) substrate.ActorInfo {
	return substrate.ActorInfo{Status: substrate.StatusRunning, Worker: worker, SnapshotFidelity: substrate.SnapshotFidelityMemory}
}

func newBackend(m *mockControl, opts ...substrate.Option) *substrate.Backend {
	return substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "echo"},
		api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, opts...)
}

// mockCloner is a control client that ALSO supports substrate's Tag clone APIs. It is a separate
// type from mockControl so tests can cover a control client without them.
type mockCloner struct {
	*mockControl
	cloneErr error
	tagErr   error
	// afterClone, when set, runs after CreateActorFromTag returns, so a test can delete or replace
	// the SUSPENDED clone before Fork resumes it. cloneNoUID makes CreateActorFromTag report no
	// UID.
	afterClone func(name string)
	cloneNoUID bool
}

func (m *mockCloner) TagActor(ctx context.Context, source substrate.ActorRef, tag substrate.SnapshotID) error {
	m.calls = append(m.calls, "tag:"+source.Name+"->"+tag.Name)
	return m.tagErr
}

func (m *mockCloner) CreateActorFromTag(ctx context.Context, a substrate.ActorRef, _ substrate.ObjectRef, tag substrate.SnapshotID) (substrate.ActorInfo, error) {
	if m.cloneErr != nil {
		return substrate.ActorInfo{}, m.cloneErr
	}
	m.calls = append(m.calls, "clone:"+a.Name+":from="+tag.Name)
	if _, ok := m.actors[a.Name]; ok {
		return substrate.ActorInfo{}, fmt.Errorf("%w: %q", substrate.ErrActorExists, a.Name)
	}
	m.exists(a.Name, substrate.StatusSuspended)
	// The clone borrows the tag's snapshot until its own first capture, as substrate's does.
	snap := "gs://snap/tags/" + tag.Name
	m.holds(a.Name, snap)
	if m.afterClone != nil {
		defer m.afterClone(a.Name)
	}
	if m.cloneNoUID {
		return substrate.ActorInfo{Status: substrate.StatusSuspended, Snapshot: snap}, nil
	}
	return substrate.ActorInfo{Status: substrate.StatusSuspended, Snapshot: snap, UID: m.uid(a.Name)}, nil
}

func (m *mockCloner) DeleteTag(ctx context.Context, tag substrate.SnapshotID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.calls = append(m.calls, "untag:"+tag.Name)
	return nil
}

// newMemoryBackend builds a backend for a REQUIRES_MEMORY_SNAPSHOT harness (the counter tier), whose
// live state exists only in RAM and therefore cannot be replay-forked.
func newMemoryBackend(ctl substrate.ControlClient) *substrate.Backend {
	return substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
		api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}})
}

func TestCreateBootsActor(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	in, err := newBackend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"get:sess-x", "create:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("create calls=%v want %v", m.calls, want)
	}
	// Substrate's only ingress to an actor is the atenet-router: the incarnation addresses the router
	// and names the actor in the metadata the router routes on, never a worker address.
	want := api.Incarnation{
		ID:           "sess-x",
		Worker:       "worker-a",
		Address:      substrate.DefaultRouterAddress,
		CallMetadata: map[string]string{"ate-target-actor": "space/sess-x"},
		Runtime:      "substrate",
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("incarnation=%+v want %+v", in, want)
	}
}

func TestWithRouterOverridesTheIngressAddress(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	in, err := newBackend(m, substrate.WithRouter("router.example:8080")).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Address != "router.example:8080" || in.CallMetadata[substrate.TargetActorHeader] != "space/sess-x" {
		t.Fatalf("incarnation=%+v want the configured router and the actor named in metadata", in)
	}
}

// A session is multi-turn by definition, and the Placer calls Create on every Exec. Substrate
// rejects a repeat CreateActor with AlreadyExists, so the second turn must ATTACH to the running
// actor rather than try to create it again.
func TestCreateAttachesToARunningActor(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	b := newBackend(m)
	if _, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	m.calls = nil
	in, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatalf("second turn on a live session: %v", err)
	}
	if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("second Create calls=%v want a bare attach %v (a re-create is AlreadyExists; a re-boot destroys live state)", m.calls, want)
	}
	if in.CallMetadata[substrate.TargetActorHeader] != "space/sess-x" {
		t.Fatalf("attach returned %+v, want the running actor's incarnation", in)
	}
}

// A forked child's actor is created from the parent's snapshot and resumed before its UID is ever
// handed out, so it is already RUNNING when the first Exec lands. Cold-booting it there would throw
// away the cloned RAM the fork exists to carry.
func TestCreateOnAForkedChildDoesNotBootOverTheClone(t *testing.T) {
	m := &mockControl{info: running("worker-b")}
	m.exists("child", substrate.StatusRunning)
	in, err := newBackend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"get:child"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want a bare attach %v (a re-create would discard the restored RAM)", m.calls, want)
	}
	if in.CallMetadata[substrate.TargetActorHeader] != "space/child" {
		t.Fatalf("attach returned %+v, want the clone's incarnation", in)
	}
}

// A session whose worker was freed by Suspend must come back through its snapshot, not a cold boot.
func TestCreateRestoresASuspendedActor(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	m.exists("sess-x", substrate.StatusSuspended)
	if _, err := newBackend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"get:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want a restore %v (a re-create would discard the memory snapshot)", m.calls, want)
	}
}

func TestCreateRejectsAnActorNotRunningAfterResume(t *testing.T) {
	// A resume that does not leave the actor RUNNING must fail loudly. Handing back an incarnation
	// anyway would let the first harness call resume it through the router instead, hiding the
	// failure from the backend that owns placement.
	m := &mockControl{info: substrate.ActorInfo{Status: substrate.StatusSuspended}}
	if _, err := newBackend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err == nil {
		t.Fatal("expected an error when the actor is not running after resume, got nil")
	}
}

func TestCreateValidatesSessionSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *api.SessionSpec
		want string
	}{
		{name: "nil spec", want: "session spec"},
		{name: "empty session uid", spec: &api.SessionSpec{}, want: "session uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{}
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			_, err := newBackend(m, substrate.WithLogger(logger)).Create(context.Background(), tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create() error = %v, want it to contain %q", err, tc.want)
			}
			if len(m.calls) != 0 {
				t.Fatalf("Create() made control-plane calls for invalid input: %v", m.calls)
			}
			if !strings.Contains(output.String(), `"error_kind":"invalid_spec"`) ||
				!strings.Contains(output.String(), `"level":"ERROR"`) {
				t.Fatalf("Create() did not log invalid spec at ERROR: %s", output.String())
			}
		})
	}
}

// Removing any preflight check must let its corresponding unsafe restore reach ResumeActor.
func TestRestoreRequiresHeldSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      api.SnapshotRef
		status   substrate.ActorStatus
		uri      string
		fidelity substrate.SnapshotFidelity
		volumes  bool
		memory   bool
		wantOK   bool
		notReady bool
		pending  bool
		precond  bool // FAILED_PRECONDITION through api.ErrInvalidCheckpoint
		busy     bool
	}{
		// ResumeActor is a no-op for RUNNING: repeated Resume, dial retry and interrupted turns.
		{name: "running no-held stateless local-only", ref: api.SnapshotRef{Local: "s"}, status: substrate.StatusRunning, notReady: true, wantOK: true},
		{name: "running no-held memory local-only", ref: api.SnapshotRef{Local: "s"}, status: substrate.StatusRunning, memory: true, notReady: true, wantOK: true},
		{name: "running held pending", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusRunning, notReady: true, pending: true, wantOK: true},
		{name: "running newer held snapshot", ref: api.SnapshotRef{Local: "s", ExternalURI: "wrong"}, status: substrate.StatusRunning, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, volumes: true, wantOK: true},
		{name: "suspended pending", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, notReady: true, pending: true},
		{name: "suspended local-only", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, notReady: true, precond: true},
		{name: "wrong URI", ref: api.SnapshotRef{Local: "s", ExternalURI: "wrong"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, precond: true},
		{name: "missing URI", ref: api.SnapshotRef{Local: "s"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, volumes: true, precond: true},
		{name: "legacy memory flag without checkpoint", ref: api.SnapshotRef{Local: "s", Memory: true}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityMemory, wantOK: true},
		{name: "missing actor name", ref: api.SnapshotRef{ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, precond: true},
		{name: "missing actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, uri: "held", fidelity: substrate.SnapshotFidelityVolumes},
		{name: "missing memory actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, uri: "held", fidelity: substrate.SnapshotFidelityMemory, memory: true},
		{name: "missing held snapshot", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, fidelity: substrate.SnapshotFidelityVolumes, precond: true},
		{name: "legacy memory flag false", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityMemory, wantOK: true},
		{name: "legacy memory flag true", ref: api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, wantOK: true},
		{name: "unknown fidelity", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", wantOK: true},
		{name: "unrecognized fidelity", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelity(99), wantOK: true},
		{name: "rootfs is not volumes", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityRootFS, volumes: true, precond: true},
		{name: "volumes mode refuses memory", ref: api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityMemory, volumes: true, precond: true},
		{name: "memory descriptor refuses files", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, memory: true, precond: true},
		{name: "running volumes actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusRunning, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, volumes: true, wantOK: true},
		{name: "running memory actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, status: substrate.StatusRunning, uri: "held", fidelity: substrate.SnapshotFidelityMemory, memory: true, wantOK: true},
		{name: "transitioning actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusTransitioning, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, busy: true},
		{name: "transitioning pending memory actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, status: substrate.StatusTransitioning, memory: true, notReady: true, pending: true, busy: true},
		{name: "terminated actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusTerminated, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, precond: true},
		{name: "terminated pending actor", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusTerminated, notReady: true, pending: true, precond: true},
		{name: "volumes", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityVolumes, volumes: true, wantOK: true},
		{name: "memory", ref: api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityMemory, memory: true, wantOK: true},
		{name: "default nonmemory", ref: api.SnapshotRef{Local: "s", ExternalURI: "held"}, status: substrate.StatusSuspended, uri: "held", fidelity: substrate.SnapshotFidelityRootFS, wantOK: true},
		{name: "no checkpoint stateless recovery", ref: api.SnapshotRef{Local: "s"}, status: substrate.StatusRunning, wantOK: true},
		{name: "no checkpoint memory recovery", ref: api.SnapshotRef{Local: "s"}, status: substrate.StatusRunning, memory: true, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a")}
			m.info.SnapshotFidelity = tc.fidelity
			m.info.SnapshotNotReady = tc.notReady
			m.info.SnapshotPending = tc.pending
			if tc.status != substrate.StatusUnknown {
				m.exists("s", tc.status)
			}
			m.holds("s", tc.uri)
			var opts []substrate.Option
			if tc.volumes {
				opts = append(opts, substrate.WithVolumesOnly())
			}
			b := newBackend(m, opts...)
			if tc.memory {
				b = newMemoryBackend(m)
			}
			inc, err := b.Restore(context.Background(), tc.ref)
			if tc.wantOK {
				if err != nil || inc.ID != "s" || m.actors["s"] != substrate.StatusRunning {
					t.Fatalf("restore=%+v err=%v actors=%v", inc, err, m.actors)
				}
				if got := m.calls[len(m.calls)-1]; got != "resume:s" {
					t.Fatalf("last call=%s, want resume:s", got)
				}
				return
			}
			if tc.busy {
				// Mid-transition settles on its own: retryable ABORTED, never a restore refusal.
				if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, api.ErrComputeBusy) || errors.Is(err, substrate.ErrSnapshotNotRestorable) ||
					errors.Is(err, api.ErrCheckpointPending) || !reflect.DeepEqual(m.calls, []string{"get:s"}) || !reflect.DeepEqual(inc, api.Incarnation{}) {
					t.Fatalf("transitioning restore=%+v err=%v calls=%v, want only ErrActorBusy", inc, err, m.calls)
				}
				return
			}
			if !errors.Is(err, substrate.ErrSnapshotNotRestorable) || !reflect.DeepEqual(inc, api.Incarnation{}) {
				t.Fatalf("restore=%+v err=%v, want ErrSnapshotNotRestorable", inc, err)
			}
			if tc.notReady && tc.status == substrate.StatusSuspended && (!errors.Is(err, substrate.ErrSnapshotNotReady) || errors.Is(err, api.ErrMemoryStateLost)) {
				t.Fatalf("not-ready classification: %v", err)
			}
			wantPending := tc.pending && tc.status == substrate.StatusSuspended
			if errors.Is(err, api.ErrCheckpointPending) != wantPending || errors.Is(err, api.ErrInvalidCheckpoint) != tc.precond {
				t.Fatalf("pending=%t precondition=%t classification: %v", tc.pending, tc.precond, err)
			}
			if tc.status == substrate.StatusTerminated && !strings.Contains(err.Error(), "in state terminated") {
				t.Fatalf("unreadable actor state: %v", err)
			}
			if tc.memory && tc.status == substrate.StatusUnknown && (!errors.Is(err, substrate.ErrMemoryStateLost) || !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrActorNotFound)) {
				t.Fatalf("missing memory actor lost error causes: %v", err)
			}
			for _, call := range m.calls {
				if call != "get:s" {
					t.Fatalf("refused restore made forbidden call %q", call)
				}
			}
			if strings.Contains(err.Error(), "held") || strings.Contains(err.Error(), "wrong") {
				t.Fatalf("error exposes snapshot URI: %v", err)
			}
		})
	}
}

func TestRestoreMemoryLossKeepsDebugClassification(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := substrate.New(&mockControl{}, "space", substrate.ObjectRef{Name: "counter"},
		api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}}, substrate.WithLogger(logger))
	_, err := b.Restore(t.Context(), api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true})
	if !errors.Is(err, api.ErrMemoryStateLost) || !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) {
		t.Fatalf("missing memory actor: err=%v logs=%s", err, output.String())
	}
}

func TestRestoreReadFailurePreservesCause(t *testing.T) {
	cause := errors.New("control unavailable")
	m := &mockControl{getErr: cause}
	_, err := newBackend(m).Restore(t.Context(), api.SnapshotRef{Local: "s", ExternalURI: "held"})
	if !errors.Is(err, cause) || !errors.Is(err, substrate.ErrSnapshotNotRestorable) || !reflect.DeepEqual(m.calls, []string{"get:s"}) {
		t.Fatalf("restore read failure: err=%v calls=%v", err, m.calls)
	}
}

func TestSuspendRestoreRoundtrip(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fidelity    substrate.SnapshotFidelity
		volumesOnly bool
		memory      bool
	}{
		{name: "memory", fidelity: substrate.SnapshotFidelityMemory, memory: true},
		{name: "volumes", fidelity: substrate.SnapshotFidelityVolumes, volumesOnly: true},
		{name: "default volumes", fidelity: substrate.SnapshotFidelityVolumes},
		{name: "rootfs", fidelity: substrate.SnapshotFidelityRootFS},
		{name: "unknown", fidelity: substrate.SnapshotFidelityUnknown},
		{name: "unrecognized", fidelity: substrate.SnapshotFidelity(99)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a")}
			m.info.SnapshotFidelity = tc.fidelity
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			opts := []substrate.Option{substrate.WithLogger(logger)}
			if tc.volumesOnly {
				opts = append(opts, substrate.WithVolumesOnly())
			}
			b := newBackend(m, opts...)
			ref, err := b.Snapshot(context.Background(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
			if err != nil {
				t.Fatal(err)
			}
			if ref.Memory != tc.memory || ref.Local != "sess-x" || ref.ExternalURI != "gs://snap/sess-x" {
				t.Fatalf("unexpected snapshot ref %+v, want Memory=%v", ref, tc.memory)
			}
			if !strings.Contains(output.String(), `"operation":"suspend_actor"`) ||
				!strings.Contains(output.String(), `"reason":"snapshot"`) {
				t.Fatalf("snapshot did not log its SuspendActor call: %s", output.String())
			}
			if _, err := b.Restore(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			want := []string{"suspend:sess-x", "get:sess-x", "resume:sess-x"}
			if !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("suspend/restore calls=%v want %v", m.calls, want)
			}
		})
	}
}

func TestRestoreOfMainEraStatelessCheckpointIgnoresLegacyMemoryFlag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fidelity substrate.SnapshotFidelity
	}{
		{name: "volumes", fidelity: substrate.SnapshotFidelityVolumes},
		{name: "unknown", fidelity: substrate.SnapshotFidelityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a")}
			m.info.SnapshotFidelity = tc.fidelity
			m.exists("sess-x", substrate.StatusSuspended)
			m.holds("sess-x", "gs://snap/sess-x")
			_, err := newBackend(m).RestoreInstance(context.Background(), api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x", Memory: true}, func() (string, error) {
				m.calls = append(m.calls, "read-record")
				return "", nil
			})
			if err != nil {
				t.Fatalf("RestoreInstance: %v", err)
			}
			if want := []string{"get:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("calls=%v want %v", m.calls, want)
			}
		})
	}
}

// A missing or mismatched observation must not become a usable checkpoint in either strict mode.
func TestSnapshotRequiresObservedFidelity(t *testing.T) {
	for _, mode := range []string{"volumes", "memory", "recorded memory instance"} {
		for _, tc := range []struct {
			name       string
			fidelity   substrate.SnapshotFidelity
			noSnapshot bool
		}{
			{name: "unknown"},
			{name: "unrecognized", fidelity: substrate.SnapshotFidelity(99)},
			{name: "rootfs", fidelity: substrate.SnapshotFidelityRootFS},
			{name: "memory", fidelity: substrate.SnapshotFidelityMemory},
			{name: "volumes", fidelity: substrate.SnapshotFidelityVolumes},
			{name: "missing memory URI", fidelity: substrate.SnapshotFidelityMemory, noSnapshot: true},
			{name: "missing volumes URI", fidelity: substrate.SnapshotFidelityVolumes, noSnapshot: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				m := &mockControl{info: running("worker-a"), suspendNoSnapshot: tc.noSnapshot}
				m.info.SnapshotFidelity = tc.fidelity
				b := newBackend(m, substrate.WithVolumesOnly())
				in := api.Incarnation{ID: "sess-x"}
				wantFidelity := substrate.SnapshotFidelityVolumes
				switch mode {
				case "memory":
					b = newMemoryBackend(m)
					m.exists(in.ID, substrate.StatusRunning) // a memory-tier snapshot looks first
					wantFidelity = substrate.SnapshotFidelityMemory
				case "recorded memory instance":
					b = newBackend(m)
					m.exists(in.ID, substrate.StatusRunning)
					in.ComputeInstance = m.uid(in.ID)
					wantFidelity = substrate.SnapshotFidelityMemory
				}
				ref, err := b.Snapshot(context.Background(), in, api.SnapshotExternal)
				if tc.fidelity == wantFidelity && !tc.noSnapshot {
					if err != nil || ref.ExternalURI == "" || ref.Memory != (mode != "volumes") {
						t.Fatalf("matching observation: ref=%+v err=%v", ref, err)
					}
				} else if err == nil || ref != (api.SnapshotRef{}) {
					t.Fatalf("unproven checkpoint: ref=%+v err=%v, want refusal with no ref", ref, err)
				} else if strings.Contains(err.Error(), "gs://snap/") {
					t.Fatalf("refusal exposes snapshot URI: %v", err)
				}
			})
		}
	}
}

func TestRefusedCheckpointCannotBeResumed(t *testing.T) {
	for _, mode := range []string{"volumes", "memory"} {
		for _, observation := range []string{"wrong fidelity", "missing URI", "unknown fidelity"} {
			t.Run(mode+"/"+observation, func(t *testing.T) {
				m := &mockControl{info: running("w")}
				makeBackend := func() *substrate.Backend { return newBackend(m, substrate.WithVolumesOnly()) }
				m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
				if mode == "memory" {
					makeBackend = func() *substrate.Backend { return newMemoryBackend(m) }
					m.info.SnapshotFidelity = substrate.SnapshotFidelityVolumes
					m.exists("s", substrate.StatusRunning) // a memory-tier snapshot looks first
				}
				switch observation {
				case "missing URI":
					m.suspendNoSnapshot = true
					if mode == "memory" {
						m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
					} else {
						m.info.SnapshotFidelity = substrate.SnapshotFidelityVolumes
					}
				case "unknown fidelity":
					m.info.SnapshotFidelity = substrate.SnapshotFidelityUnknown
				}
				ref, err := makeBackend().Snapshot(t.Context(), api.Incarnation{ID: "s"}, api.SnapshotExternal)
				if !errors.Is(err, api.ErrInvalidCheckpoint) || ref != (api.SnapshotRef{}) || m.actors["s"] != substrate.StatusSuspended {
					t.Fatalf("capture ref=%+v err=%v actors=%v", ref, err, m.actors)
				}
				for _, operation := range []string{"create", "restore"} {
					m.calls = nil
					m.exists("s", substrate.StatusSuspended)
					b := makeBackend() // a new host cannot rely on an in-memory latched refusal
					if operation == "create" {
						_, err = b.Create(t.Context(), &api.SessionSpec{SessionUID: "s"})
					} else {
						_, err = b.Restore(t.Context(), api.SnapshotRef{Local: "s"})
					}
					if !errors.Is(err, api.ErrInvalidCheckpoint) || !reflect.DeepEqual(m.calls, []string{"get:s"}) || m.actors["s"] != substrate.StatusSuspended {
						t.Errorf("%s after refusal: err=%v calls=%v actors=%v", operation, err, m.calls, m.actors)
					}
				}
			})
		}
	}
}

func TestRestoreReplacementPrecedesFidelityError(t *testing.T) {
	m := &mockControl{info: running("w")}
	m.info.SnapshotFidelity = substrate.SnapshotFidelityVolumes
	m.exists("s", substrate.StatusSuspended)
	m.holds("s", "replacement-snapshot")
	_, err := newMemoryBackend(m).RestoreInstance(t.Context(), api.SnapshotRef{Local: "s", Memory: true}, func() (string, error) { return "old-uid", nil })
	if !errors.Is(err, substrate.ErrMemoryStateLost) || !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrSnapshotNotRestorable) || errors.Is(err, api.ErrInvalidCheckpoint) || slices.Contains(m.calls, "resume:s") {
		t.Fatalf("replacement error=%v calls=%v", err, m.calls)
	}
}

// A RUNNING memory actor keeps main's no-op Resume while it holds the recorded snapshot, unless a
// nonempty observed UID differs from the recorded one. Missing UID evidence is not proof of loss.
// A proven replacement is loss before readiness. An actor, RUNNING or SUSPENDED, that holds a
// snapshot other than the held ref is loss too: the journal never recorded that capture (one a
// Snapshot refused, say), so its RAM cannot be shown to be the session's.
func TestRestoreOfMemoryActorIsGatedOnRecordedUID(t *testing.T) {
	for _, tc := range []struct {
		name, recorded string
		suspended      bool
		pending        bool
		noUID          bool
		unreadable     bool
		holds          string // the actor's snapshot: "" is the held ref's
		wantCalls      []string
		lost           bool
	}{
		{name: "suspended replaced", recorded: "old-uid", suspended: true, wantCalls: []string{"get:s", "read-record"}, lost: true},
		{name: "suspended record unreadable", suspended: true, unreadable: true, wantCalls: []string{"get:s", "read-record"}},
		{name: "running record unreadable", unreadable: true, wantCalls: []string{"get:s", "read-record"}},
		{name: "running matching", recorded: "uid-s", wantCalls: []string{"get:s", "read-record", "resume:s"}},
		{name: "running mismatched", recorded: "old-uid", wantCalls: []string{"get:s", "read-record"}, lost: true},
		{name: "running missing observed", recorded: "uid-s", noUID: true, wantCalls: []string{"get:s", "resume:s"}},
		{name: "running missing recorded", wantCalls: []string{"get:s", "read-record", "resume:s"}},
		{name: "suspended pending replaced", recorded: "old-uid", suspended: true, pending: true, wantCalls: []string{"get:s", "read-record"}, lost: true},
		{name: "suspended holding the held ref", recorded: "uid-s", suspended: true, wantCalls: []string{"get:s", "read-record", "resume:s"}},
		{name: "suspended stale matching", recorded: "uid-s", suspended: true, holds: "newer", wantCalls: []string{"get:s", "read-record"}, lost: true},
		{name: "suspended stale missing observed", recorded: "old-uid", suspended: true, noUID: true, holds: "newer", wantCalls: []string{"get:s"}, lost: true},
		{name: "running stale matching", recorded: "uid-s", holds: "newer", wantCalls: []string{"get:s", "read-record"}, lost: true},
		{name: "running stale missing observed", recorded: "uid-s", noUID: true, holds: "newer", wantCalls: []string{"get:s"}, lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("w"), getNoUID: tc.noUID, uids: map[string]string{"s": "uid-s"}}
			m.info.SnapshotNotReady, m.info.SnapshotPending = tc.pending, tc.pending
			status := substrate.StatusRunning
			if tc.suspended {
				status = substrate.StatusSuspended
			}
			m.exists("s", status)
			if tc.holds == "" {
				tc.holds = "held"
			}
			m.holds("s", tc.holds)
			inc, err := newMemoryBackend(m).RestoreInstance(t.Context(), api.SnapshotRef{Local: "s", ExternalURI: "held", Memory: true}, func() (string, error) {
				m.calls = append(m.calls, "read-record")
				if tc.unreadable {
					return "", errJournalUnreadable
				}
				return tc.recorded, nil
			})
			if !reflect.DeepEqual(m.calls, tc.wantCalls) {
				t.Fatalf("calls=%v want %v (err=%v)", m.calls, tc.wantCalls, err)
			}
			if tc.lost {
				if !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrSnapshotNotRestorable) || errors.Is(err, api.ErrCheckpointPending) || !reflect.DeepEqual(inc, api.Incarnation{}) {
					t.Fatalf("replaced actor: inc=%+v err=%v, want ErrMemoryStateLost only", inc, err)
				}
				return
			}
			if tc.unreadable {
				// Without the record, admission cannot rule out replacement: refuse (INTERNAL), not loss.
				if !errors.Is(err, errJournalUnreadable) || !errors.Is(err, substrate.ErrSnapshotNotRestorable) || errors.Is(err, api.ErrMemoryStateLost) ||
					errors.Is(err, api.ErrInvalidCheckpoint) || errors.Is(err, api.ErrComputeBusy) || !reflect.DeepEqual(inc, api.Incarnation{}) {
					t.Fatalf("unreadable record: inc=%+v err=%v, want a non-loss refusal wrapping the read error", inc, err)
				}
				return
			}
			if err != nil || inc.ComputeInstance != "uid-s" {
				t.Fatalf("RUNNING no-op: inc=%+v err=%v", inc, err)
			}
		})
	}
}

var errJournalUnreadable = errors.New("journal unreadable")

func TestActorStatusString(t *testing.T) {
	for st, want := range map[substrate.ActorStatus]string{
		substrate.StatusUnknown: "unknown", substrate.StatusRunning: "running", substrate.StatusSuspended: "suspended",
		substrate.StatusTerminated: "terminated", substrate.StatusTransitioning: "transitioning", substrate.ActorStatus(42): "ActorStatus(42)",
	} {
		if got := st.String(); got != want {
			t.Errorf("%d.String()=%q want %q", int(st), got, want)
		}
	}
}

func TestVolumesOnlyAdmissionAndConflictingDescriptor(t *testing.T) {
	mem := api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}
	stateless := api.Capabilities{Resumability: api.ResumabilityStatelessReplay}
	m := &mockControl{info: running("worker-a")}
	b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
		api.Descriptor{ID: "counter", Capabilities: mem}, substrate.WithVolumesOnly())
	if controller.CanPlace(mem, b.Capabilities()) {
		t.Fatal("files-only template must refuse memory-required admission")
	}
	if !controller.CanPlace(stateless, b.Capabilities()) {
		t.Fatal("files-only template must admit stateless replay")
	}
	for _, fidelity := range []substrate.SnapshotFidelity{substrate.SnapshotFidelityVolumes, substrate.SnapshotFidelityMemory} {
		m.info.SnapshotFidelity = fidelity
		ref, err := b.Snapshot(context.Background(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
		if !errors.Is(err, api.ErrInvalidCheckpoint) || ref != (api.SnapshotRef{}) {
			t.Fatalf("conflicting requirements: ref=%+v err=%v", ref, err)
		}
		if !strings.Contains(err.Error(), "actor space/sess-x:") {
			t.Errorf("conflicting requirements refusal %q does not name the actor", err)
		}
		if len(m.calls) != 0 {
			t.Fatalf("conflicting requirements called control plane: %v", m.calls)
		}
	}
}

func TestSnapshotLocalUnsupported(t *testing.T) {
	if _, err := newBackend(&mockControl{}).Snapshot(context.Background(), api.Incarnation{ID: "s"}, api.SnapshotLocal); err == nil {
		t.Fatal("expected LOCAL (warm) snapshot to be unsupported on substrate")
	}
}

func TestStopSuspendsThenDeletes(t *testing.T) {
	m := &mockControl{}
	if err := newBackend(m).Stop(context.Background(), api.Incarnation{ID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"suspend:sess-x", "delete:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("stop calls=%v want %v (delete requires SUSPENDED first)", m.calls, want)
	}
}

func TestStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status substrate.ActorStatus
		want   api.ComputeState
	}{
		{substrate.StatusRunning, api.ComputeLive},
		{substrate.StatusSuspended, api.ComputeCold},
		{substrate.StatusTerminated, api.ComputeTerminated},
	} {
		got, err := newBackend(&mockControl{status: tc.status}).Status(context.Background(), api.Incarnation{ID: "s"})
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("status %v -> %v, want %v", tc.status, got, tc.want)
		}
	}
}

// A STATELESS_REPLAY harness holds nothing the journal lacks, so its fork stays a replay-fork: a
// fresh cold child, no snapshot clone, and the parent is left running.
func TestForkIsReplayForkForStatelessHarness(t *testing.T) {
	m := &mockControl{info: running("worker-b")}
	if _, err := newBackend(m).Fork(context.Background(), api.SnapshotRef{Local: "parent"}, api.ForkOpts{ChildSessionUID: "child"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"get:child", "create:child", "resume:child"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("fork calls=%v want a fresh cold child actor %v", m.calls, want)
	}
}

// newClonerWithSuspendedParent is a cloning control client whose "parent" actor is SUSPENDED holding
// snap-parent-1, the state a stateful fork finds right after it checkpoints the parent.
func newClonerWithSuspendedParent() *mockCloner {
	m := &mockCloner{mockControl: &mockControl{info: running("worker-c")}}
	m.exists("parent", substrate.StatusSuspended)
	m.holds("parent", "snap-parent-1")
	return m
}

// A REQUIRES_MEMORY_SNAPSHOT harness's state lives only in RAM, so its fork must CLONE the parent's
// snapshot: tag the suspended parent, create the child from that tag, and resume it, which restores
// the cloned RAM. The parent is checked on both sides of the tag, because a tag captures whatever
// snapshot the parent holds when it runs.
func TestForkClonesSnapshotForMemoryHarness(t *testing.T) {
	m := newClonerWithSuspendedParent()
	in, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "resume:child"}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("fork calls=%v want a snapshot clone %v", m.calls, want)
	}
	if in.CallMetadata[substrate.TargetActorHeader] != "space/child" {
		t.Fatalf("cloned child incarnation=%+v want it to target the child actor", in)
	}
}

// The router resumes an actor on any request addressed to it, so the parent can be woken and
// suspended again between the fork's checkpoint and its tag. The tag would then copy a newer snapshot
// than the journal prefix the children inherit. Such a fork must be refused before it tags anything.
func TestForkRefusesAParentThatMovedPastItsCheckpoint(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.holds("parent", "snap-parent-2") // suspended again after the fork's checkpoint
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrSnapshotSuperseded) || !errors.Is(err, api.ErrSnapshotSuperseded) {
		t.Fatalf("err=%v want ErrSnapshotSuperseded", err)
	}
	if want := []string{"get:parent"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want the fork refused before tagging %v", m.calls, want)
	}
}

// The same race inside the window between the first check and the tag: the tag may have copied the
// newer snapshot, so it must be deleted and the fork refused.
func TestForkDeletesATagTakenAfterTheParentMoved(t *testing.T) {
	m := newClonerWithSuspendedParent()
	gets := 0
	m.afterGet = func(name string) {
		if name != "parent" {
			return
		}
		if gets++; gets == 1 {
			m.holds("parent", "snap-parent-2") // woken and suspended again right after the check
		}
	}
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrSnapshotSuperseded) || !errors.Is(err, api.ErrSnapshotSuperseded) {
		t.Fatalf("err=%v want ErrSnapshotSuperseded", err)
	}
	want := []string{"get:parent", "tag:parent->fork-child", "get:parent", "untag:fork-child"}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want the stale tag deleted and no child created %v", m.calls, want)
	}
}

// A parent deleted before or after the fork tags it took its snapshot with it: no child can be
// cloned and a retry cannot help, so the fork is api.ErrMemoryStateLost (FAILED_PRECONDITION at the
// Sessions API), not a plain error, and a tag taken before the parent vanished is released.
func TestForkOfADeletedParentIsMemoryStateLost(t *testing.T) {
	for _, tc := range []struct {
		name      string
		afterTag  bool // the parent is deleted between the first check and the tag, else before the fork
		wantCalls []string
	}{
		{name: "before the tag", wantCalls: []string{"get:parent"}},
		{name: "after the tag", afterTag: true, wantCalls: []string{"get:parent", "tag:parent->fork-child", "get:parent", "untag:fork-child"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newClonerWithSuspendedParent()
			if tc.afterTag {
				m.afterGet = func(name string) {
					delete(m.actors, name)
					m.afterGet = nil
				}
			} else {
				delete(m.actors, "parent")
			}
			_, err := newMemoryBackend(m).Fork(context.Background(),
				api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
				api.ForkOpts{ChildSessionUID: "child"})
			if !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrActorNotFound) {
				t.Fatalf("fork of a deleted parent: err=%v, want ErrMemoryStateLost wrapping ErrActorNotFound", err)
			}
			if !reflect.DeepEqual(m.calls, tc.wantCalls) {
				t.Fatalf("calls=%v want %v", m.calls, tc.wantCalls)
			}
		})
	}
}

// Forking a memory harness with no parent snapshot must fail loudly. Cold-booting the child instead
// would hand back a child whose RAM is empty while its copied journal prefix says otherwise — a
// silently divergent branch, since such a harness never rebuilds state from History (I4).
func TestForkMemoryHarnessWithoutSnapshotIsRefused(t *testing.T) {
	m := newClonerWithSuspendedParent()
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent"}, api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrNoSnapshotToClone) {
		t.Fatalf("err=%v want ErrNoSnapshotToClone", err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("a refused fork must not touch the control plane, got %v", m.calls)
	}
}

// A control client without the Tag APIs cannot clone. It must refuse a stateful fork rather than
// silently degrading to a replay-fork that loses the in-RAM state.
func TestForkMemoryHarnessWithoutClonerIsRefused(t *testing.T) {
	m := &mockControl{info: running("worker-c")}
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrNoSnapshotToClone) {
		t.Fatalf("err=%v want ErrNoSnapshotToClone", err)
	}
}

// Cloning is a full per-actor restore, not a shared copy-on-write image, so the backend must not
// advertise CoWFork. Capability claims gate placement decisions; an inflated one degrades dishonestly.
func TestCapabilitiesDoNotClaimCoWFork(t *testing.T) {
	if newBackend(&mockControl{}).Capabilities().CoWFork {
		t.Fatal("substrate clones via a full snapshot restore; CoWFork must stay false")
	}
}

// TestTwoBackendNeutrality is the spike §8 success criterion: substrate reports MemorySnapshot=true
// and can host a REQUIRES_MEMORY_SNAPSHOT harness, while a plain pod (MemorySnapshot=false) must
// refuse it — honest degradation across two Runtime backends behind one SPI. A STATELESS_REPLAY
// harness runs on both.
func TestTwoBackendNeutrality(t *testing.T) {
	sub := newBackend(&mockControl{}).Capabilities()
	pod := api.RuntimeCapabilities{MemorySnapshot: false}

	if !sub.MemorySnapshot {
		t.Fatal("substrate must report MemorySnapshot=true")
	}
	memHarness := api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}
	if !controller.CanPlace(memHarness, sub) {
		t.Fatal("substrate must host a REQUIRES_MEMORY_SNAPSHOT harness")
	}
	if controller.CanPlace(memHarness, pod) {
		t.Fatal("a plain pod must REFUSE a REQUIRES_MEMORY_SNAPSHOT harness (honest degradation)")
	}

	stateless := api.Capabilities{Resumability: api.ResumabilityStatelessReplay}
	if !controller.CanPlace(stateless, sub) || !controller.CanPlace(stateless, pod) {
		t.Fatal("a STATELESS_REPLAY harness must run on both backends")
	}
}

// A tag owns a full copy of the parent's snapshot, so a fork that fails after tagging must delete
// it. Otherwise every failed fork attempt strands one more snapshot copy in object storage.
func TestFailedCloneReleasesItsTag(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.cloneErr = errors.New("template mismatch")
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if err == nil {
		t.Fatal("expected the clone to fail")
	}
	want := []string{"get:parent", "tag:parent->fork-child", "get:parent", "untag:fork-child"}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want the tag to be released %v", m.calls, want)
	}
}

// Substrate reserves a tag before copying the snapshot into it, so a tag whose copy failed is left
// pending, holding partial data, and blocks deleting the atespace. The fork must release it. It then
// reads the parent again to classify the failure; a parent that still holds the snapshot leaves the
// tag's own error, untyped.
func TestFailedTagReleasesTheReservedTag(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.tagErr = errors.New("while copying the external snapshot: object store unavailable")
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, m.tagErr) || errors.Is(err, api.ErrMemoryStateLost) || errors.Is(err, api.ErrSnapshotSuperseded) {
		t.Fatalf("err=%v, want the tag error alone", err)
	}
	want := []string{"get:parent", "tag:parent->fork-child", "untag:fork-child", "get:parent"}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want the reserved tag released %v", m.calls, want)
	}
}

// A tag that already existed was not reserved by this attempt, so the fork must leave it alone.
func TestTagThatAlreadyExistsIsNotDeleted(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.tagErr = fmt.Errorf("%w: space/fork-child", substrate.ErrTagExists)
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, substrate.ErrTagExists) {
		t.Fatalf("err=%v want ErrTagExists", err)
	}
	if slices.Contains(m.calls, "untag:fork-child") {
		t.Fatalf("calls=%v deleted a tag this fork did not create", m.calls)
	}
}

// A clone that was created and resumed but is not reported RUNNING may still hold a worker. Fork
// returns no handle in that case, so the child UID is discarded upstream and this is the last moment
// the actor can be named: it must be torn down here, not left behind forever.
func TestForkTearsDownACloneThatIsNotRunning(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.info = substrate.ActorInfo{Status: substrate.StatusUnknown} // resumed, but not reported running
	_, err := newMemoryBackend(m).Fork(context.Background(),
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"})
	if err == nil {
		t.Fatal("a clone that is not running must not be returned as a usable incarnation")
	}
	want := []string{
		"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "resume:child",
		"suspend:child", "delete:child", "untag:fork-child",
	}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want the stranded clone torn down %v", m.calls, want)
	}
}

// A clone is SUSPENDED between CreateActorFromTag and ResumeActor, so it can be deleted, or deleted
// and another actor created under the child's name, before Fork resumes it. ResumeActor resolves
// the name, so Fork compares the UID it reports with the clone's: the child's FORK marker must
// never record a replacement's UID, which its first turn would then accept as the clone. A fork
// that lost its clone is retryable (the parent still holds its snapshot, and a retry mints a new
// child), and cleanup never suspends or deletes an actor this fork did not create.
func TestForkRefusesACloneReplacedBeforeItsResume(t *testing.T) {
	notFound := fmt.Errorf("%w: space/child", substrate.ErrActorNotFound)
	resumeFailed := errors.New("resume workflow failed")
	errRead := errors.New("control plane unavailable")
	head := []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "resume:child"}
	for _, tc := range []struct {
		name       string
		afterClone func(m *mockCloner, name string)
		noUID      bool
		resumeErr  error
		wantCalls  []string
		wantBusy   bool  // retryable ErrActorBusy, not ErrMemoryStateLost
		wantErr    error // also wrapped
		wantMsg    string
		wantLeft   bool // the actor under the child's name is the replacement, untouched
	}{
		{name: "replaced, then resumed: refused, replacement left", afterClone: replace,
			wantCalls: append(slices.Clone(head), "untag:fork-child"), wantBusy: true,
			wantMsg: "replacement may have been woken by this fork's resume and is left running", wantLeft: true},
		{name: "replaced, then the resume fails: refused, replacement left", afterClone: replace, resumeErr: resumeFailed,
			wantCalls: append(slices.Clone(head), "get:child", "untag:fork-child"), wantBusy: true, wantErr: resumeFailed,
			wantMsg: "the replacement was left in place", wantLeft: true},
		{name: "deleted: refused, tag released", afterClone: func(m *mockCloner, name string) { m.delete(name) },
			resumeErr: notFound, wantCalls: append(slices.Clone(head), "get:child", "untag:fork-child"), wantBusy: true,
			wantErr: notFound, wantMsg: "was deleted between its create and resume"},
		{name: "resume fails on the clone: the clone is torn down", resumeErr: resumeFailed,
			wantCalls: append(slices.Clone(head), "get:child", "suspend:child", "delete:child", "untag:fork-child"), wantErr: resumeFailed},
		{name: "the cleanup read fails: nothing is touched",
			afterClone: func(m *mockCloner, _ string) { m.getErr = errRead }, resumeErr: resumeFailed,
			wantCalls: append(slices.Clone(head), "get:child"), wantErr: resumeFailed, wantMsg: "were left in place"},
		{name: "a clone reported without a UID is torn down before its resume", noUID: true,
			wantCalls: []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child",
				"suspend:child", "delete:child", "untag:fork-child"}, wantMsg: "reported no actor UID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newClonerWithSuspendedParent()
			m.resumeErr = tc.resumeErr
			m.cloneNoUID = tc.noUID
			var cloneUID, replacementUID string
			m.afterClone = func(name string) {
				cloneUID = m.uids[name]
				if tc.afterClone != nil {
					tc.afterClone(m, name)
				}
				replacementUID = m.uids[name]
			}
			_, err := newMemoryBackend(m).Fork(context.Background(),
				api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
				api.ForkOpts{ChildSessionUID: "child"})
			if err == nil {
				t.Fatal("Fork succeeded, want it refused")
			}
			if !reflect.DeepEqual(m.calls, tc.wantCalls) {
				t.Errorf("calls=%v want %v", m.calls, tc.wantCalls)
			}
			if got := errors.Is(err, substrate.ErrActorBusy) && errors.Is(err, api.ErrComputeBusy); got != tc.wantBusy {
				t.Errorf("err=%v: retryable busy=%v want %v", err, got, tc.wantBusy)
			}
			if errors.Is(err, api.ErrMemoryStateLost) {
				t.Errorf("err=%v is ErrMemoryStateLost; the parent still holds its snapshot", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err=%v want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err=%q want it to say %q", err, tc.wantMsg)
			}
			if tc.wantLeft {
				if replacementUID == cloneUID || m.uids["child"] != replacementUID {
					t.Errorf("actor under the child's name has UID %q, want the replacement %q (clone %q) left in place",
						m.uids["child"], replacementUID, cloneUID)
				}
				if !strings.Contains(err.Error(), cloneUID) || !strings.Contains(err.Error(), replacementUID) {
					t.Errorf("err=%q want it to name the clone %q and the replacement %q", err, cloneUID, replacementUID)
				}
			}
		})
	}
}

// replace deletes the actor under name and creates another under it, as an atespace operator could.
func replace(m *mockCloner, name string) {
	m.delete(name)
	m.exists(name, substrate.StatusSuspended)
}

// delete drops an actor and its UID without recording a call: the deletion is someone else's.
func (m *mockControl) delete(name string) {
	delete(m.actors, name)
	delete(m.uids, name)
	delete(m.gens, name) // an actor created again under the name has no captures
	delete(m.stamps, name)
}

// The likeliest reason a fork fails partway is that the caller's deadline expired or it went away.
// Cleanup bound to that same context would do nothing in exactly that case, so it must be detached.
func TestForkCleanupSurvivesACancelledCaller(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.cloneErr = errors.New("deadline exceeded")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newMemoryBackend(m).Fork(ctx,
		api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
		api.ForkOpts{ChildSessionUID: "child"}); err == nil {
		t.Fatal("expected the fork to fail")
	}
	// The mock refuses any call carrying a cancelled context, so recording this proves the release
	// did NOT inherit the caller's context.
	if !slices.Contains(m.calls, "untag:fork-child") {
		t.Fatalf("cleanup must run on a detached context, calls=%v", m.calls)
	}
}

// A REQUIRES_MEMORY_SNAPSHOT session keeps state only in its actor's RAM. Once it has a started
// turn, an actor that no longer exists cannot be replaced by a fresh one from the template: that would
// continue the journal with a harness that remembers none of it. Create must refuse with
// ErrMemoryStateLost and create nothing. It counts the turns only after GetActor reports the actor
// missing ("count" follows "get"), so a turn journaled while the call was being placed is seen, and
// it fails closed when the count fails. Every other combination keeps the usual placement, without
// reading the journal.
func TestCreateRefusesToColdCreateAMemorySessionWithTurns(t *testing.T) {
	memory := func(m *mockControl) *substrate.Backend { return newMemoryBackend(m) }
	stateless := func(m *mockControl) *substrate.Backend { return newBackend(m) }
	errCount := errors.New("journal unreadable")
	for _, tc := range []struct {
		name      string
		backend   func(*mockControl) *substrate.Backend
		turns     int
		countErr  error
		existing  substrate.ActorStatus // StatusUnknown: the actor does not exist
		wantCalls []string
		wantErr   error
	}{
		{name: "memory tier with turns and no actor is refused", backend: memory, turns: 2,
			wantCalls: []string{"get:sess-x", "count"}, wantErr: substrate.ErrMemoryStateLost},
		{name: "memory tier whose count fails is refused", backend: memory, countErr: errCount,
			wantCalls: []string{"get:sess-x", "count"}, wantErr: errCount},
		{name: "memory tier with zero turns creates", backend: memory,
			wantCalls: []string{"get:sess-x", "count", "create:sess-x", "resume:sess-x"}},
		{name: "stateless tier with turns creates", backend: stateless, turns: 2,
			wantCalls: []string{"get:sess-x", "create:sess-x", "resume:sess-x"}},
		{name: "memory tier with turns attaches to a running actor", backend: memory, turns: 2, existing: substrate.StatusRunning,
			wantCalls: []string{"get:sess-x"}},
		{name: "memory tier with turns resumes a suspended actor", backend: memory, turns: 2, existing: substrate.StatusSuspended,
			wantCalls: []string{"get:sess-x", "resume:sess-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a")}
			if tc.existing != substrate.StatusUnknown {
				m.exists("sess-x", tc.existing)
				m.holds("sess-x", "gs://snap/sess-x")
			}
			count := func() (int, error) {
				m.calls = append(m.calls, "count")
				return tc.turns, tc.countErr
			}
			inc, err := tc.backend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x", StartedTurns: count})
			if !reflect.DeepEqual(m.calls, tc.wantCalls) {
				t.Errorf("calls=%v want %v", m.calls, tc.wantCalls)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if inc.CallMetadata[substrate.TargetActorHeader] != "space/sess-x" {
					t.Fatalf("incarnation=%+v want the session's actor", inc)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Create error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == substrate.ErrMemoryStateLost {
				if !errors.Is(err, api.ErrMemoryStateLost) {
					t.Fatalf("Create error = %v, want it to wrap api.ErrMemoryStateLost", err)
				}
				for _, want := range []string{`"sess-x"`, "2 started turn(s)", "cannot be reconstructed"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
			}
			if _, ok := m.actors["sess-x"]; ok {
				t.Error("refused Create left an actor behind")
			}
		})
	}
}

// A caller that leaves StartedTurns nil declares a session with no started turns, so a memory-tier
// session's actor is created as on a new session.
func TestCreateWithNoTurnCountCreates(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	if _, err := newMemoryBackend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := []string{"get:sess-x", "create:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Errorf("calls=%v want %v", m.calls, want)
	}
}

// Restore resumes the actor that holds the session's snapshot. When a REQUIRES_MEMORY_SNAPSHOT
// session's actor no longer exists, the snapshot went with it, so Restore reports ErrMemoryStateLost
// (FAILED_PRECONDITION at the Sessions API) instead of the resume error, and creates nothing. So
// does an actor whose UID is not the one the journal recorded, whatever the resume error and
// whatever snapshot it holds. Every other failure keeps the resume error, including a NOT_FOUND
// resume of the actor the journal recorded, which substrate also answers for an actor whose
// template volume is missing, even when that actor holds a newer snapshot than the session's last
// SUSPEND names.
func TestRestoreOfAGoneMemoryActorIsMemoryStateLost(t *testing.T) {
	memory := func(m *mockControl) *substrate.Backend { return newMemoryBackend(m) }
	stateless := func(m *mockControl) *substrate.Backend { return newBackend(m) }
	resumeErr := errors.New("rpc error: code = NotFound desc = actor not found")
	notFound := fmt.Errorf("%w: space/sess-x: %w", substrate.ErrActorNotFound, resumeErr)
	for _, tc := range []struct {
		name      string
		backend   func(*mockControl) *substrate.Backend
		existing  substrate.ActorStatus // StatusUnknown: the actor does not exist
		getErr    error
		holds     string // the snapshot the existing actor holds
		snapshot  string // the ExternalURI restored: "" is gs://snap/sess-x, "-" is none
		recorded  string // the UID RestoreInstance reads: "" calls Restore, "-" reads none, "!" fails the read after the resume
		resumeErr error  // nil: resumeErr below, which GetActor has to classify
		wantCalls []string
		wantLost  bool
		wantMsg   string // "" is "cannot be restored" alone
	}{
		{name: "memory tier without an actor", backend: memory,
			wantCalls: []string{"resume:sess-x", "get:sess-x"}, wantLost: true},
		{name: "memory tier whose actor exists keeps the resume error", backend: memory, existing: substrate.StatusSuspended,
			wantCalls: []string{"resume:sess-x", "get:sess-x"}},
		{name: "memory tier whose GetActor fails keeps the resume error", backend: memory, getErr: errors.New("unavailable"),
			wantCalls: []string{"resume:sess-x", "get:sess-x"}},
		{name: "stateless tier keeps the resume error", backend: stateless,
			wantCalls: []string{"resume:sess-x"}},
		// After a resume error that does not report the actor missing, the actor's UID decides,
		// with or without a snapshot: only one other than the UID the journal recorded is a
		// replacement.
		{name: "memory tier whose resume fails and which replaced the recorded one", backend: memory, existing: substrate.StatusSuspended,
			recorded: "uid-old", wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"},
			wantLost: true, wantMsg: `has UID "uid-sess-x-1", not the "uid-old"`},
		{name: "memory tier whose resume fails and which replaced the recorded one, with no snapshot", backend: memory, existing: substrate.StatusRunning,
			snapshot: "-", recorded: "uid-old", wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"},
			wantLost: true, wantMsg: `has UID "uid-sess-x-1", not the "uid-old"`},
		{name: "memory tier whose resume fails and which is busy and replaced the recorded one", backend: memory, existing: substrate.StatusSuspended,
			resumeErr: fmt.Errorf("%w: another operation is in progress", substrate.ErrActorBusy), recorded: "uid-old",
			wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}, wantLost: true, wantMsg: `has UID "uid-sess-x-1", not the "uid-old"`},
		{name: "memory tier whose resume fails on the recorded actor keeps the resume error", backend: memory, existing: substrate.StatusSuspended,
			recorded: "uid-sess-x-1", wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		{name: "memory tier whose resume fails with no recorded UID keeps the resume error", backend: memory, existing: substrate.StatusSuspended,
			recorded: "-", wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		{name: "memory tier whose resume fails and whose record cannot be read keeps the resume error", backend: memory, existing: substrate.StatusSuspended,
			recorded: "!", wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		{name: "stateless tier whose resume fails keeps the resume error whatever the record", backend: stateless, existing: substrate.StatusSuspended,
			recorded: "uid-old", wantCalls: []string{"resume:sess-x"}},
		// After a resume that reports the actor missing, GetActor decides: no actor is lost. An
		// actor that does not hold the snapshot is compared by UID when the journal records one,
		// and is lost by the snapshot alone (a replacement holds none) when it records none.
		{name: "memory tier whose resume reports the actor missing and which is gone", backend: memory,
			resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x"}, wantLost: true},
		{name: "memory tier whose resume reports the actor missing and which is replaced, with no UID to compare", backend: memory, existing: substrate.StatusSuspended,
			resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x"}, wantLost: true, wantMsg: "no longer holds the snapshot"},
		{name: "memory tier whose resume reports the actor missing and which holds another snapshot, with no UID recorded", backend: memory,
			existing: substrate.StatusSuspended, holds: "gs://snap/sess-x-B", recorded: "-", resumeErr: notFound,
			wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}, wantLost: true, wantMsg: "records no actor UID to compare"},
		// The session's own actor holding a newer snapshot than the journal's last SUSPEND (the
		// SUSPEND append failed after the snapshot was taken, or another host suspended it again)
		// still holds the session's RAM: it keeps the resume error. Only another UID is lost.
		{name: "memory tier whose resume reports the recorded actor missing and which holds a newer snapshot keeps the resume error", backend: memory,
			existing: substrate.StatusSuspended, holds: "gs://snap/sess-x-B", recorded: "uid-sess-x-1", resumeErr: notFound,
			wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		{name: "memory tier whose resume reports the actor missing and which replaced the recorded one, holding another snapshot", backend: memory,
			existing: substrate.StatusSuspended, holds: "gs://snap/sess-x-B", recorded: "uid-old", resumeErr: notFound,
			wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}, wantLost: true, wantMsg: `has UID "uid-sess-x-1", not the "uid-old"`},
		{name: "memory tier whose resume reports the actor missing and which replaced the recorded one, holding no snapshot", backend: memory,
			existing: substrate.StatusSuspended, recorded: "uid-old", resumeErr: notFound,
			wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}, wantLost: true, wantMsg: `has UID "uid-sess-x-1", not the "uid-old"`},
		{name: "memory tier whose resume reports the actor missing, which holds another snapshot and whose record cannot be read, keeps the resume error", backend: memory,
			existing: substrate.StatusSuspended, holds: "gs://snap/sess-x-B", recorded: "!", resumeErr: notFound,
			wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		// With no snapshot recorded (a host died during a turn), the actor's UID decides: only one
		// other than the UID the journal recorded is a replacement.
		{name: "memory tier whose resume reports the actor missing, with no snapshot and no UID to compare, keeps the resume error", backend: memory,
			existing: substrate.StatusRunning, snapshot: "-", resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x"}},
		{name: "memory tier whose resume reports the recorded actor missing, with no snapshot, keeps the resume error", backend: memory,
			existing: substrate.StatusRunning, snapshot: "-", recorded: "uid-sess-x-1", resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		{name: "memory tier whose resume reports the actor missing and which replaced the recorded one, with no snapshot", backend: memory,
			existing: substrate.StatusRunning, snapshot: "-", recorded: "uid-old", resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"},
			wantLost: true, wantMsg: `has UID "uid-sess-x-1", not the "uid-old"`},
		{name: "memory tier whose resume reports the actor missing and whose record cannot be read, with no snapshot, keeps the resume error", backend: memory,
			existing: substrate.StatusRunning, snapshot: "-", recorded: "!", resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x", "read-record"}},
		{name: "memory tier without an actor and no snapshot", backend: memory,
			snapshot: "-", recorded: "uid-sess-x-1", resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x"}, wantLost: true},
		// Substrate also answers NOT_FOUND for an actor that exists (a template volume it cannot
		// find); one that still holds the snapshot keeps the resume error.
		{name: "memory tier whose resume reports the actor missing but which holds the snapshot keeps the resume error", backend: memory,
			existing: substrate.StatusSuspended, holds: "gs://snap/sess-x", resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x"}},
		{name: "memory tier whose resume reports the actor missing and whose GetActor fails keeps the resume error", backend: memory,
			getErr: errors.New("unavailable"), resumeErr: notFound, wantCalls: []string{"resume:sess-x", "get:sess-x"}},
		{name: "stateless tier whose resume reports the actor missing keeps the resume error", backend: stateless,
			resumeErr: notFound, wantCalls: []string{"resume:sess-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumeErr := resumeErr
			if tc.resumeErr != nil {
				resumeErr = tc.resumeErr
			}
			m := &mockControl{info: running("worker-a"), resumeErr: resumeErr, getErr: tc.getErr}
			if tc.existing != substrate.StatusUnknown {
				m.exists("sess-x", tc.existing)
			}
			if tc.holds != "" {
				m.holds("sess-x", tc.holds)
			}
			uri := "gs://snap/sess-x"
			switch tc.snapshot {
			case "-":
				uri = ""
			case "":
			default:
				uri = tc.snapshot
			}
			ref := api.SnapshotRef{Local: "sess-x", ExternalURI: uri, Memory: uri != ""}
			// Start with valid held state (or live no-checkpoint state) on the recorded actor, then
			// race the lifecycle after preflight, which admits it, so these rows cover failed-resume
			// classification. Preflight loss and read refusal are covered by
			// TestRestoreOfMemoryActorIsGatedOnRecordedUID.
			actors, snapshots, getErr, uids := m.actors, m.snapshots, m.getErr, m.uids
			state := substrate.StatusRunning
			if uri != "" {
				state = substrate.StatusSuspended
			}
			m.actors = map[string]substrate.ActorStatus{"sess-x": state}
			m.snapshots = map[string]string{"sess-x": uri}
			m.getErr = nil
			preflightUID := "uid-sess-x-1"
			if tc.recorded != "" && tc.recorded != "-" && tc.recorded != "!" {
				preflightUID = tc.recorded
			}
			m.uids = map[string]string{"sess-x": preflightUID}
			m.afterGet = func(string) {
				m.actors, m.snapshots, m.getErr, m.uids = actors, snapshots, getErr, uids
				m.afterGet = nil
			}
			wantCalls := []string{"get:sess-x"}
			desc, derr := tc.backend(&mockControl{}).Describe(context.Background())
			if derr != nil {
				t.Fatal(derr)
			}
			preflightRead := tc.recorded != "" && desc.Capabilities.Resumability == api.ResumabilityRequiresMemorySnapshot
			if preflightRead {
				// The admission read sees the recorded actor; only a later read can see loss.
				wantCalls = append(wantCalls, "read-record")
			}
			wantCalls = append(wantCalls, tc.wantCalls...)
			reads := 0
			var err error
			switch tc.recorded {
			case "":
				_, err = tc.backend(m).Restore(context.Background(), ref)
			default:
				// The record is read after GetActor answers, when the backend decides; calls shows when.
				_, err = tc.backend(m).RestoreInstance(context.Background(), ref, func() (string, error) {
					m.calls = append(m.calls, "read-record")
					reads++
					switch tc.recorded {
					case "!":
						if preflightRead && reads == 1 {
							return preflightUID, nil
						}
						return "", errors.New("journal unreadable")
					case "-":
						return "", nil
					}
					return tc.recorded, nil
				})
			}
			if !reflect.DeepEqual(m.calls, wantCalls) {
				t.Errorf("calls=%v want %v", m.calls, wantCalls)
			}
			if !tc.wantLost {
				if !errors.Is(err, resumeErr) || errors.Is(err, api.ErrMemoryStateLost) {
					t.Fatalf("Restore error = %v, want the resume error and not ErrMemoryStateLost", err)
				}
				return
			}
			if !errors.Is(err, substrate.ErrMemoryStateLost) || !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, resumeErr) {
				t.Fatalf("Restore error = %v, want ErrMemoryStateLost wrapping api.ErrMemoryStateLost and the resume error", err)
			}
			for _, want := range []string{`"sess-x"`, "space/sess-x", "cannot be restored", tc.wantMsg} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if _, ok := m.actors["sess-x"]; ok && tc.existing == substrate.StatusUnknown {
				t.Error("Restore created an actor")
			}
		})
	}
}

// For a REQUIRES_MEMORY_SNAPSHOT harness the incarnation carries the actor's UID as ComputeInstance on
// every path that hands one out (create, attach, resume of a suspended actor, Restore), so the host can
// tell the actor apart from one created later under the same name. A stateless harness's state is in
// the journal, so its incarnation carries none.
func TestIncarnationCarriesTheActorUIDForAMemoryHarness(t *testing.T) {
	ctx := context.Background()
	m := &mockControl{info: running("worker-a")}
	mem := newMemoryBackend(m)
	created, err := mem.Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	uid := m.uids["sess-x"]
	if uid == "" || created.ComputeInstance != uid {
		t.Fatalf("created incarnation ComputeInstance=%q, want the actor's UID %q", created.ComputeInstance, uid)
	}
	attached, err := mem.Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil || attached.ComputeInstance != uid {
		t.Fatalf("attach: ComputeInstance=%q err=%v, want %q", attached.ComputeInstance, err, uid)
	}
	m.exists("sess-x", substrate.StatusSuspended)
	m.holds("sess-x", "gs://snap/sess-x")
	resumed, err := mem.Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil || resumed.ComputeInstance != uid {
		t.Fatalf("resume of a suspended actor: ComputeInstance=%q err=%v, want %q", resumed.ComputeInstance, err, uid)
	}
	restored, err := mem.Restore(ctx, api.SnapshotRef{Local: "sess-x"})
	if err != nil || restored.ComputeInstance != uid {
		t.Fatalf("restore: ComputeInstance=%q err=%v, want %q", restored.ComputeInstance, err, uid)
	}

	stateless, err := newBackend(&mockControl{info: running("worker-a")}).Create(ctx, &api.SessionSpec{SessionUID: "sess-y"})
	if err != nil || stateless.ComputeInstance != "" {
		t.Fatalf("stateless incarnation ComputeInstance=%q err=%v, want none", stateless.ComputeInstance, err)
	}
}

// uidless reports every actor without a UID, as a control client that does not read it would.
type uidless struct{ *mockControl }

func (u uidless) ResumeActor(ctx context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	info, err := u.mockControl.ResumeActor(ctx, a)
	info.UID = ""
	return info, err
}

func (u uidless) GetActor(ctx context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	info, err := u.mockControl.GetActor(ctx, a)
	info.UID = ""
	return info, err
}

// A memory-tier session is never placed on an actor that reports no UID: the host could not tell it
// from a replacement, so the guard against continuing on lost state would be off without anyone
// noticing. A stateless harness does not need the UID and is placed as before.
func TestMemoryHarnessRefusesAnActorWithoutAUID(t *testing.T) {
	ctx := context.Background()
	_, err := newMemoryBackend(uidless{&mockControl{info: running("worker-a")}}).Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	if err == nil || !strings.Contains(err.Error(), "reported no UID") {
		t.Fatalf("memory harness on a UID-less actor: err=%v, want a refusal naming the missing UID", err)
	}
	stateless := substrate.New(uidless{&mockControl{info: running("worker-a")}}, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "echo"},
		api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}})
	if _, err := stateless.Create(ctx, &api.SessionSpec{SessionUID: "sess-x"}); err != nil {
		t.Fatalf("stateless harness on a UID-less actor: %v", err)
	}
}

// Two hosts place a new session's first turn at once. Both find no actor; the other host's
// CreateActor wins, so this one's reports ErrActorExists. Create attaches to the winner's actor
// instead of failing the turn, and reports that actor's UID.
func TestCreateAttachesWhenAnotherHostCreatedTheActorFirst(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	var once bool
	m.afterGet = func(name string) {
		if !once {
			once = true
			m.exists(name, substrate.StatusRunning) // the other host's CreateActor and ResumeActor
		}
	}
	inc, err := newMemoryBackend(m).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatalf("Create after losing the create race: %v", err)
	}
	if want := []string{"get:sess-x", "create:sess-x", "get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v", m.calls, want)
	}
	if inc.ComputeInstance != m.uids["sess-x"] {
		t.Fatalf("ComputeInstance=%q, want the winner's actor UID %q", inc.ComputeInstance, m.uids["sess-x"])
	}

	// The winner's actor can be gone again by the time this host looks; that is reported, not retried.
	m = &mockControl{info: running("worker-a")}
	once = false
	m.afterGet = func(name string) {
		if !once {
			once = true
			m.exists(name, substrate.StatusRunning)
		}
	}
	_, err = newMemoryBackend(goneOnReread{m}).Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if !errors.Is(err, substrate.ErrActorExists) {
		t.Fatalf("taken then gone: err=%v, want ErrActorExists", err)
	}
}

// goneOnReread deletes the actor just before a GetActor that follows a CreateActor answers, so
// Create's re-read after ErrActorExists finds nothing.
type goneOnReread struct{ *mockControl }

func (g goneOnReread) GetActor(ctx context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	if slices.Contains(g.calls, "create:"+a.Name) {
		delete(g.actors, a.Name)
	}
	return g.mockControl.GetActor(ctx, a)
}

// A stateful fork passes the compute instance the journal last recorded, and Snapshot refuses an
// actor that replaced it (api.ErrMemoryStateLost): before suspending when it can see the
// replacement, and after, on the UID the suspend reports, when the actor was replaced between the
// look and the suspend. With no instance required, Snapshot suspends whatever actor the name holds,
// as before.
func TestSnapshotRefusesAReplacedActor(t *testing.T) {
	ctx := context.Background()
	newRunning := func() *mockControl {
		m := &mockControl{info: running("worker-a")}
		m.exists("sess-x", substrate.StatusRunning)
		return m
	}

	m := newRunning()
	uid := m.uids["sess-x"]
	ref, err := newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: uid}, api.SnapshotExternal)
	if err != nil || ref.ExternalURI == "" {
		t.Fatalf("snapshot of the recorded actor: ref=%+v err=%v, want a snapshot", ref, err)
	}

	m = newRunning()
	_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: "uid-recorded"}, api.SnapshotExternal)
	if !errors.Is(err, api.ErrMemoryStateLost) || !strings.Contains(err.Error(), "uid-recorded") {
		t.Fatalf("snapshot of a replacement: err=%v, want ErrMemoryStateLost naming the recorded instance", err)
	}
	if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v: a replacement seen before the suspend is not suspended", m.calls, want)
	}

	m = newRunning()
	uid = m.uids["sess-x"]
	m.afterGet = func(name string) {
		// The actor is deleted and created again under its name between the look and the suspend.
		delete(m.actors, name)
		delete(m.uids, name)
		m.exists(name, substrate.StatusRunning)
		m.afterGet = nil
	}
	_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: uid}, api.SnapshotExternal)
	if !errors.Is(err, api.ErrMemoryStateLost) {
		t.Fatalf("actor replaced between the look and the suspend: err=%v, want ErrMemoryStateLost", err)
	}

	// An actor that is gone took the session's state with it: that is ErrMemoryStateLost, as Exec and
	// Resume report it, whether it is missing at the look or by the time of the suspend.
	m = &mockControl{info: running("worker-a")}
	_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: "uid-recorded"}, api.SnapshotExternal)
	if !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrActorNotFound) || !strings.Contains(err.Error(), "uid-recorded") {
		t.Fatalf("snapshot of a missing actor: err=%v, want ErrMemoryStateLost wrapping ErrActorNotFound and naming the instance", err)
	}
	if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v: a missing actor is not suspended", m.calls, want)
	}
	m = newRunning()
	uid = m.uids["sess-x"]
	m.suspendErr = fmt.Errorf("deleted after the look: %w", substrate.ErrActorNotFound)
	_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: uid}, api.SnapshotExternal)
	if !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrActorNotFound) {
		t.Fatalf("actor deleted between the look and the suspend: err=%v, want ErrMemoryStateLost wrapping ErrActorNotFound", err)
	}
	// Any other control error stays what it was.
	m = newRunning()
	uid = m.uids["sess-x"]
	m.suspendErr = errors.New("control plane down")
	_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: uid}, api.SnapshotExternal)
	if err == nil || errors.Is(err, api.ErrMemoryStateLost) {
		t.Fatalf("suspend failure: err=%v, want a plain error", err)
	}
	// A memory-tier actor with no recorded instance (a fork child whose FORK marker records no clone,
	// before its first turn) that is gone took the cloned state with it too.
	m = &mockControl{info: running("worker-a"), suspendErr: substrate.ErrActorNotFound}
	_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
	if !errors.Is(err, api.ErrMemoryStateLost) || !errors.Is(err, substrate.ErrActorNotFound) {
		t.Fatalf("snapshot of a missing memory-tier actor with no recorded instance: err=%v, want ErrMemoryStateLost wrapping ErrActorNotFound", err)
	}
	// A stateless snapshot of a missing actor is not memory-state loss: the journal holds the state.
	m = &mockControl{info: running("worker-a"), suspendErr: substrate.ErrActorNotFound}
	_, err = newBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
	if !errors.Is(err, substrate.ErrActorNotFound) || errors.Is(err, api.ErrMemoryStateLost) {
		t.Fatalf("stateless snapshot of a missing actor: err=%v, want ErrActorNotFound only", err)
	}

	// An actor that reports no UID cannot be compared. That is a ControlClient fault, not a
	// replacement, so it is a plain error that does not claim the actor was replaced.
	for _, tc := range []struct {
		name      string
		set       func(*mockControl)
		wantCalls []string
	}{
		{"look", func(m *mockControl) { m.getNoUID = true }, []string{"get:sess-x"}},
		{"suspend", func(m *mockControl) { m.suspendNoUID = true }, []string{"get:sess-x", "suspend:sess-x"}},
	} {
		m = newRunning()
		uid = m.uids["sess-x"]
		tc.set(m)
		_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: uid}, api.SnapshotExternal)
		if err == nil || errors.Is(err, api.ErrMemoryStateLost) || !strings.Contains(err.Error(), "reported no UID") ||
			strings.Contains(err.Error(), "replaced") {
			t.Fatalf("no UID at the %s: err=%v, want a plain error saying the actor reported no UID", tc.name, err)
		}
		if !reflect.DeepEqual(m.calls, tc.wantCalls) {
			t.Fatalf("no UID at the %s: calls=%v want %v", tc.name, m.calls, tc.wantCalls)
		}
	}

	// With no recorded instance any UID is accepted, but a memory-tier backend still looks first,
	// for MemoryNotRestored.
	m = newRunning()
	if _, err := newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x"}, api.SnapshotExternal); err != nil {
		t.Fatalf("snapshot with no instance required: %v", err)
	}
	if want := []string{"get:sess-x", "suspend:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v", m.calls, want)
	}

	// A memory-tier actor with no recorded instance (a fork child whose FORK marker records no clone,
	// before its first turn) must still report a UID from the suspend: the ControlClient contract
	// requires one. A stateless backend does not depend on it.
	for _, tc := range []struct {
		name      string
		set       func(*mockControl)
		wantCalls []string
	}{
		{"look", func(m *mockControl) { m.getNoUID = true }, []string{"get:sess-x"}},
		{"suspend", func(m *mockControl) { m.suspendNoUID = true }, []string{"get:sess-x", "suspend:sess-x"}},
	} {
		m = newRunning()
		tc.set(m)
		_, err = newMemoryBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
		if err == nil || errors.Is(err, api.ErrMemoryStateLost) || !strings.Contains(err.Error(), "reported no UID") ||
			strings.Contains(err.Error(), "instance \"\"") {
			t.Fatalf("memory-tier %s with no UID and no recorded instance: err=%v, want a plain error saying the actor reported no UID", tc.name, err)
		}
		if !reflect.DeepEqual(m.calls, tc.wantCalls) {
			t.Fatalf("memory-tier %s with no UID and no recorded instance: calls=%v want %v", tc.name, m.calls, tc.wantCalls)
		}
	}
	m = newRunning()
	m.suspendNoUID = true
	if _, err := newBackend(m).Snapshot(ctx, api.Incarnation{ID: "sess-x"}, api.SnapshotExternal); err != nil {
		t.Fatalf("stateless suspend with no UID and no recorded instance: %v", err)
	}
}

// The lost-state message reads the same whether or not an instance was recorded.
func TestSnapshotOfAMissingActorMessage(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   api.Incarnation
		want string
	}{
		{"recorded instance", api.Incarnation{ID: "sess-x", ComputeInstance: "uid-1"},
			`actor space/sess-x, instance "uid-1", which held the session's in-RAM state, no longer exists, `},
		{"no recorded instance", api.Incarnation{ID: "sess-x"},
			`actor space/sess-x, which held the session's in-RAM state, no longer exists, `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a"), suspendErr: substrate.ErrActorNotFound}
			_, err := newMemoryBackend(m).Snapshot(ctx, tc.in, api.SnapshotExternal)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// An actor in the middle of another operation (usually another host resuming the actor it just
// created) settles on its own, so Create reports it as ErrActorBusy, which wraps api.ErrComputeBusy
// and is retryable, whether Create finds it on its first look, after losing the CreateActor race,
// or when substrate refuses the resume of a suspended actor because another operation holds it.
func TestCreateOnABusyActorIsRetryable(t *testing.T) {
	ctx := context.Background()
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, api.ErrComputeBusy) {
			t.Fatalf("%s: err=%v, want ErrActorBusy wrapping api.ErrComputeBusy", name, err)
		}
	}

	m := &mockControl{info: running("worker-a")}
	m.exists("sess-x", substrate.StatusTransitioning)
	_, err := newMemoryBackend(m).Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	check("actor mid-transition", err)

	m = &mockControl{info: running("worker-a")}
	once := false
	m.afterGet = func(name string) {
		if !once {
			once = true
			m.exists(name, substrate.StatusTransitioning) // the other host created it and is resuming it
		}
	}
	_, err = newMemoryBackend(m).Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	check("lost the create race to an actor mid-resume", err)
	if want := []string{"get:sess-x", "create:sess-x", "get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v", m.calls, want)
	}

	m = &mockControl{info: running("worker-a"), resumeErr: fmt.Errorf("%w: another operation is in progress", substrate.ErrActorBusy)}
	m.exists("sess-x", substrate.StatusSuspended)
	m.holds("sess-x", "gs://snap/sess-x")
	_, err = newMemoryBackend(m).Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	check("resume refused while another operation holds the actor", err)

	m = &mockControl{info: running("worker-a")}
	m.exists("sess-x", substrate.StatusUnknown)
	_, err = newMemoryBackend(m).Create(ctx, &api.SessionSpec{SessionUID: "sess-x"})
	if err == nil || errors.Is(err, api.ErrComputeBusy) {
		t.Fatalf("actor in an unknown state: err=%v, want a failure that is not retryable", err)
	}
}

// Create looks at the actor, then resumes it if it is SUSPENDED. When the resume reports the actor
// missing, Create reads it again rather than fail with the bare resume error: gone, it is a missing
// actor (a memory-tier session with turns is ErrMemoryStateLost, anything else gets a new actor);
// replaced under the name, or deleted again after CreateActor found the name taken, it is
// ErrActorBusy, so a retry looks afresh; still there (substrate also answers NOT_FOUND for a template
// volume it cannot find), the resume error stands.
func TestCreateWhoseActorChangesDuringTheResume(t *testing.T) {
	ctx := context.Background()
	notFound := fmt.Errorf("%w: space/sess-x: rpc error: code = NotFound", substrate.ErrActorNotFound)
	// gone deletes the actor out of band, without recording a call.
	gone := func(m *mockControl) {
		delete(m.actors, "sess-x")
		delete(m.uids, "sess-x")
	}
	for _, tc := range []struct {
		name string
		// memory selects the REQUIRES_MEMORY_SNAPSHOT tier; turns is what StartedTurns reports.
		memory bool
		turns  int
		// absent starts with no actor; otherwise the actor is SUSPENDED.
		absent bool
		// onGet runs after the n-th GetActor (1-based) answers.
		onGet     func(m *mockControl, n int)
		wantCalls []string
		wantErr   error // nil: success
		notErr    []error
	}{
		{
			name: "memory tier with turns whose actor is deleted is lost", memory: true, turns: 1,
			onGet: func(m *mockControl, n int) {
				if n == 1 {
					gone(m)
				}
			},
			wantCalls: []string{"get:sess-x", "resume:sess-x", "get:sess-x", "count"},
			wantErr:   substrate.ErrMemoryStateLost,
		},
		{
			name: "stateless tier whose actor is deleted gets a new actor",
			onGet: func(m *mockControl, n int) {
				switch n {
				case 1:
					gone(m)
				case 2:
					m.resumeErr = nil // the new actor resumes
				}
			},
			wantCalls: []string{"get:sess-x", "resume:sess-x", "get:sess-x", "create:sess-x", "resume:sess-x"},
		},
		{
			name: "memory tier whose actor is replaced is retryable", memory: true, turns: 1,
			onGet: func(m *mockControl, n int) {
				if n == 1 {
					gone(m)
					m.exists("sess-x", substrate.StatusSuspended)
				}
			},
			wantCalls: []string{"get:sess-x", "resume:sess-x", "get:sess-x"},
			wantErr:   substrate.ErrActorBusy,
			notErr:    []error{api.ErrMemoryStateLost},
		},
		{
			name: "memory tier whose actor is still there keeps the resume error", memory: true, turns: 1,
			wantCalls: []string{"get:sess-x", "resume:sess-x", "get:sess-x"},
			wantErr:   notFound,
			notErr:    []error{api.ErrMemoryStateLost, api.ErrComputeBusy},
		},
		{
			name: "an actor created by another host and deleted before its resume is retryable", memory: true, absent: true,
			onGet: func(m *mockControl, n int) {
				switch n {
				case 1:
					m.exists("sess-x", substrate.StatusSuspended) // the other host creates it
				case 2:
					gone(m)
				}
			},
			wantCalls: []string{"get:sess-x", "count", "create:sess-x", "get:sess-x", "resume:sess-x", "get:sess-x"},
			wantErr:   substrate.ErrActorBusy,
			notErr:    []error{api.ErrMemoryStateLost},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a"), resumeErr: notFound}
			if !tc.absent {
				m.exists("sess-x", substrate.StatusSuspended)
				m.holds("sess-x", "gs://snap/sess-x")
			} else {
				m.info.NoSnapshotHistory = true
			}
			gets := 0
			m.afterGet = func(string) {
				gets++
				if tc.onGet != nil {
					tc.onGet(m, gets)
				}
			}
			b := newBackend(m)
			if tc.memory {
				b = newMemoryBackend(m)
			}
			count := func() (int, error) {
				m.calls = append(m.calls, "count")
				return tc.turns, nil
			}
			_, err := b.Create(ctx, &api.SessionSpec{SessionUID: "sess-x", StartedTurns: count})
			if !reflect.DeepEqual(m.calls, tc.wantCalls) {
				t.Errorf("calls=%v want %v", m.calls, tc.wantCalls)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Create error = %v, want %v", err, tc.wantErr)
			}
			for _, not := range tc.notErr {
				if errors.Is(err, not) {
					t.Errorf("Create error = %v, want it not to wrap %v", err, not)
				}
			}
		})
	}
}

// A fidelity refusal is diagnosable from its message alone: Snapshot (Suspend), Restore (Resume)
// and Create's attach each name the actor and the required and observed fidelity, keep the error
// identities the session service maps to FAILED_PRECONDITION, and never quote the snapshot URI.
func TestCheckpointRefusalNamesActorAndFidelity(t *testing.T) {
	for _, tc := range []struct {
		mode            string
		observed        substrate.SnapshotFidelity
		required, saw   string
		memoryStateLost bool
		makeBackend     func(*mockControl) *substrate.Backend
	}{
		{
			mode: "memory tier on a VOLUMES template", observed: substrate.SnapshotFidelityVolumes,
			required: "required MEMORY", saw: "observed VOLUMES", memoryStateLost: true,
			makeBackend: func(m *mockControl) *substrate.Backend { return newMemoryBackend(m) },
		},
		{
			mode: "VOLUMES-only backend observing MEMORY", observed: substrate.SnapshotFidelityMemory,
			required: "required VOLUMES", saw: "observed MEMORY",
			makeBackend: func(m *mockControl) *substrate.Backend {
				return newBackend(m, substrate.WithVolumesOnly())
			},
		},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			m := &mockControl{info: running("w")}
			m.info.SnapshotFidelity = tc.observed
			check := func(operation string, err error) {
				t.Helper()
				lost := errors.Is(err, api.ErrMemoryStateLost)
				if !errors.Is(err, api.ErrInvalidCheckpoint) || lost != tc.memoryStateLost {
					t.Fatalf("%s: err=%v, want ErrInvalidCheckpoint, ErrMemoryStateLost=%t",
						operation, err, tc.memoryStateLost)
				}
				for _, want := range []string{"actor space/s:", tc.required, tc.saw} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: %q does not contain %q", operation, err, want)
					}
				}
				if strings.Contains(err.Error(), "gs://snap/") {
					t.Errorf("%s: refusal exposes the snapshot URI: %v", operation, err)
				}
			}
			m.exists("s", substrate.StatusRunning) // a memory-tier snapshot looks first
			_, err := tc.makeBackend(m).Snapshot(t.Context(), api.Incarnation{ID: "s"}, api.SnapshotExternal)
			check("snapshot", err)
			_, err = tc.makeBackend(m).Restore(t.Context(), api.SnapshotRef{Local: "s"})
			check("restore", err)
			_, err = tc.makeBackend(m).Create(t.Context(), &api.SessionSpec{SessionUID: "s"})
			check("create", err)
		})
	}
}

// A memory-tier actor that came back without its snapshot's memory (ActorInfo.MemoryNotRestored)
// is refused with api.ErrMemoryStateLost on every path that hands an actor out, and is never
// suspended into a checkpoint that would launder the loss. A stateless session is placed as before.
func TestMemoryNotRestoredIsLostStateForAMemoryHarness(t *testing.T) {
	lostRunning := running("worker-b")
	lostRunning.MemoryNotRestored = true
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, memory bool) (*mockControl, error)
		// calls is the control-plane sequence for a memory harness.
		calls []string
	}{
		{
			name:  "restore resumes a suspended actor",
			calls: []string{"get:sess-x", "resume:sess-x"},
			run: func(t *testing.T, memory bool) (*mockControl, error) {
				m := &mockControl{info: lostRunning}
				m.exists("sess-x", substrate.StatusSuspended)
				m.holds("sess-x", "gs://snap/sess-x")
				_, err := backendFor(m, memory).Restore(t.Context(), api.SnapshotRef{Local: "sess-x", ExternalURI: "gs://snap/sess-x", Memory: true})
				return m, err
			},
		},
		{
			name:  "restore of a running actor",
			calls: []string{"get:sess-x", "resume:sess-x"},
			run: func(t *testing.T, memory bool) (*mockControl, error) {
				m := &mockControl{info: lostRunning}
				m.exists("sess-x", substrate.StatusRunning)
				_, err := backendFor(m, memory).Restore(t.Context(), api.SnapshotRef{Local: "sess-x"})
				return m, err
			},
		},
		{
			name:  "create resumes a suspended actor",
			calls: []string{"get:sess-x", "resume:sess-x"},
			run: func(t *testing.T, memory bool) (*mockControl, error) {
				m := &mockControl{info: lostRunning}
				m.exists("sess-x", substrate.StatusSuspended)
				m.holds("sess-x", "gs://snap/sess-x")
				_, err := backendFor(m, memory).Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x"})
				return m, err
			},
		},
		{
			name:  "create attaches to a running actor",
			calls: []string{"get:sess-x"},
			run: func(t *testing.T, memory bool) (*mockControl, error) {
				m := &mockControl{info: lostRunning}
				m.exists("sess-x", substrate.StatusRunning)
				_, err := backendFor(m, memory).Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x"})
				return m, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := tc.run(t, true)
			if !errors.Is(err, api.ErrMemoryStateLost) {
				t.Fatalf("memory harness: err=%v, want api.ErrMemoryStateLost", err)
			}
			for _, part := range []string{`session "sess-x"`, "space/sess-x", `harness "counter"`, m.uid("sess-x")} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("refusal %q does not name %q", err, part)
				}
			}
			if !reflect.DeepEqual(m.calls, tc.calls) {
				t.Fatalf("calls=%v want %v", m.calls, tc.calls)
			}
			if _, err := tc.run(t, false); err != nil {
				t.Fatalf("stateless harness: %v", err)
			}
		})
	}
}

func TestSnapshotRefusesAMemoryActorThatLostItsRAM(t *testing.T) {
	m := &mockControl{info: running("worker-b")}
	m.info.MemoryNotRestored = true
	m.exists("sess-x", substrate.StatusRunning)
	in := api.Incarnation{ID: "sess-x", ComputeInstance: m.uid("sess-x")}
	ref, err := newMemoryBackend(m).Snapshot(t.Context(), in, api.SnapshotExternal)
	if !errors.Is(err, api.ErrMemoryStateLost) || ref != (api.SnapshotRef{}) {
		t.Fatalf("snapshot: ref=%+v err=%v, want api.ErrMemoryStateLost", ref, err)
	}
	if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v: the actor must not be suspended", m.calls, want)
	}
	if !strings.Contains(err.Error(), "nothing was snapshotted") {
		t.Fatalf("refusal %q does not say nothing was snapshotted", err)
	}
}

// With no recorded instance (a journal without compute_instance, or no turn yet), a
// REQUIRES_MEMORY_SNAPSHOT backend still looks before suspending and refuses an actor that lost
// its RAM; a stateless backend suspends as before, without the look.
func TestSnapshotRefusesLostRAMWithoutARecordedInstance(t *testing.T) {
	for _, memory := range []bool{true, false} {
		t.Run(fmt.Sprintf("memory=%v", memory), func(t *testing.T) {
			m := &mockControl{info: running("worker-b")}
			m.info.MemoryNotRestored = true
			m.exists("sess-x", substrate.StatusRunning)
			ref, err := backendFor(m, memory).Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
			if !memory {
				if err != nil || !reflect.DeepEqual(m.calls, []string{"suspend:sess-x"}) {
					t.Fatalf("stateless snapshot: ref=%+v err=%v calls=%v", ref, err, m.calls)
				}
				return
			}
			if !errors.Is(err, api.ErrMemoryStateLost) || ref != (api.SnapshotRef{}) {
				t.Fatalf("snapshot: ref=%+v err=%v, want api.ErrMemoryStateLost", ref, err)
			}
			if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("calls=%v want %v: the actor must not be suspended", m.calls, want)
			}
		})
	}
}

// The refused actor keeps its worker until the actor itself is deleted (DeleteSession does not stop
// it), so the first refusal for each actor UID is logged at Warn for operators; repeated turns do not
// log again, an actor with another UID does, and Stop forgets the session.
func TestMemoryNotRestoredWarnsOncePerActor(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := &mockControl{info: running("worker-b")}
	m.info.MemoryNotRestored = true
	m.exists("sess-x", substrate.StatusRunning)
	b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
		api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}},
		substrate.WithLogger(logger))
	refuse := func() {
		t.Helper()
		if _, err := b.Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x"}); !errors.Is(err, api.ErrMemoryStateLost) {
			t.Fatalf("create: err=%v, want api.ErrMemoryStateLost", err)
		}
		if _, err := b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal); !errors.Is(err, api.ErrMemoryStateLost) {
			t.Fatalf("snapshot: err=%v, want api.ErrMemoryStateLost", err)
		}
	}
	warnings := func() []map[string]any {
		t.Helper()
		var got []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			if rec["level"] == "WARN" && rec["error_kind"] == "memory_state_lost" {
				got = append(got, rec)
			}
		}
		return got
	}

	first := m.uid("sess-x")
	refuse()
	refuse()
	got := warnings()
	if len(got) != 1 {
		t.Fatalf("warnings=%v, want one for actor %s", got, first)
	}
	for key, want := range map[string]string{"atespace": "space", "actor": "sess-x", "actor_uid": first, "worker": "worker-b"} {
		if got[0][key] != want {
			t.Fatalf("warning %v: %s=%v, want %q", got[0], key, got[0][key], want)
		}
	}

	m.uids["sess-x"] = "uid-replacement"
	refuse()
	if got := warnings(); len(got) != 2 || got[1]["actor_uid"] != "uid-replacement" {
		t.Fatalf("warnings=%v, want a second one for the replacement", got)
	}

	if err := b.Stop(t.Context(), api.Incarnation{ID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	m.exists("sess-x", substrate.StatusRunning)
	m.uids["sess-x"] = "uid-replacement"
	refuse()
	if got := warnings(); len(got) != 3 {
		t.Fatalf("warnings=%v, want a third one after Stop forgot the session", got)
	}
}

func TestForkRefusesACloneThatLostTheParentRAM(t *testing.T) {
	m := newClonerWithSuspendedParent()
	m.info.MemoryNotRestored = true
	_, err := newMemoryBackend(m).Fork(t.Context(), api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true}, api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, api.ErrMemoryStateLost) {
		t.Fatalf("fork: err=%v, want api.ErrMemoryStateLost", err)
	}
	// Fork tears the clone down, so the refusal must say so, not that the actor was left running.
	if msg := err.Error(); !strings.Contains(msg, "tears the clone down") || strings.Contains(msg, "left running") {
		t.Fatalf("refusal %q must say the fork tears the clone down", err)
	}
	if _, ok := m.actors["child"]; ok || !slices.Contains(m.calls, "untag:fork-child") {
		t.Fatalf("refused clone was not torn down: calls=%v", m.calls)
	}
}

func backendFor(m *mockControl, memory bool) *substrate.Backend {
	if memory {
		return newMemoryBackend(m)
	}
	return substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
		api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}})
}

// The memory check and the suspend are separate calls, and SuspendActor takes no precondition, so
// another caller can suspend the checked actor and resume it on a worker that cannot restore its
// memory in between; the suspend then captures RAM that is not the session's. Snapshot compares the
// CaptureLineage the check saw with the one the suspend reports and refuses any change with
// api.ErrMemoryStateLost and no ref. It fails closed, so a capture in the window is refused even
// when the resume kept the memory. A stateless harness is not checked.
func TestSnapshotRefusesACaptureTheMemoryCheckDidNotSee(t *testing.T) {
	for _, tc := range []struct {
		name      string
		suspended bool // the actor is SUSPENDED when Snapshot checks it
		between   func(m *mockControl)
		refused   bool
	}{
		{name: "nothing in between"},
		{name: "checked suspended, nothing in between", suspended: true},
		{name: "suspended and resumed on another runtime", refused: true, between: func(m *mockControl) {
			m.capture("sess-x")
			m.runtime = "rt-b"
		}},
		{name: "suspended and resumed on the same runtime", refused: true, between: func(m *mockControl) {
			m.capture("sess-x")
		}},
		{name: "moved to another runtime without a capture", refused: true, between: func(m *mockControl) {
			m.runtime = "rt-b"
		}},
		{name: "checked suspended, resumed on another runtime", suspended: true, refused: true, between: func(m *mockControl) {
			m.actors["sess-x"] = substrate.StatusRunning
			m.runtime = "rt-b"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, memory := range []bool{true, false} {
				var output bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
				m := &mockControl{info: running("worker-a")}
				m.exists("sess-x", substrate.StatusRunning)
				in := api.Incarnation{ID: "sess-x"}
				if tc.suspended {
					// Suspended on a capture the journal recorded.
					m.capture("sess-x")
					m.exists("sess-x", substrate.StatusSuspended)
					m.holds("sess-x", m.captureURI("sess-x"))
					in.RecordedCheckpoint = api.SnapshotRef{Local: "sess-x", ExternalURI: m.captureURI("sess-x"), Memory: true}
				}
				m.afterGet = func(string) {
					m.afterGet = nil
					if tc.between != nil {
						tc.between(m)
					}
				}
				resumability := api.ResumabilityStatelessReplay
				if memory {
					resumability = api.ResumabilityRequiresMemorySnapshot
				}
				b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
					api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: resumability}},
					substrate.WithLogger(logger))
				if memory {
					in.ComputeInstance = m.uid("sess-x")
				} else if tc.between != nil {
					tc.between(m) // a stateless Snapshot does not look first
				}
				ref, err := b.Snapshot(t.Context(), in, api.SnapshotExternal)
				if !memory || !tc.refused {
					if err != nil || ref.ExternalURI == "" {
						t.Fatalf("memory=%v: snapshot: ref=%+v err=%v, want a snapshot", memory, ref, err)
					}
					if output.Len() != 0 {
						t.Fatalf("memory=%v: logged %s", memory, output.String())
					}
					continue
				}
				if !errors.Is(err, api.ErrMemoryStateLost) || ref != (api.SnapshotRef{}) {
					t.Fatalf("snapshot: ref=%+v err=%v, want api.ErrMemoryStateLost", ref, err)
				}
				if want := []string{"get:sess-x", "suspend:sess-x"}; !reflect.DeepEqual(m.calls, want) {
					t.Fatalf("calls=%v want %v", m.calls, want)
				}
				for _, part := range []string{"space/sess-x", m.uid("sess-x"), "no checkpoint was recorded", "actor remains suspended"} {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("refusal %q does not name %q", err, part)
					}
				}
				if strings.Contains(err.Error(), "gs://") {
					t.Fatalf("refusal %q names the snapshot URI", err)
				}
				if !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) || !strings.Contains(output.String(), m.uid("sess-x")) {
					t.Fatalf("refusal was not logged for operators: %s", output.String())
				}
			}
		})
	}
}

// CaptureLineage is part of the ControlClient contract for a memory-tier Snapshot. A check that
// reports none is refused before anything is suspended; a suspend that reports none is refused
// after it. Neither is evidence of lost state, so neither wraps api.ErrMemoryStateLost.
func TestSnapshotRequiresACaptureLineage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *mockControl)
		calls []string
	}{
		{name: "check", setup: func(m *mockControl) { m.getNoLineage = true }, calls: []string{"get:sess-x"}},
		{name: "suspend", setup: func(m *mockControl) { m.suspendNoLineage = true }, calls: []string{"get:sess-x", "suspend:sess-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a")}
			m.exists("sess-x", substrate.StatusRunning)
			tc.setup(m)
			ref, err := newMemoryBackend(m).Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
			if err == nil || errors.Is(err, api.ErrMemoryStateLost) || ref != (api.SnapshotRef{}) {
				t.Fatalf("snapshot: ref=%+v err=%v, want a plain refusal", ref, err)
			}
			if !strings.Contains(err.Error(), "no capture lineage") {
				t.Fatalf("refusal %q does not say the lineage is missing", err)
			}
			if !reflect.DeepEqual(m.calls, tc.calls) {
				t.Fatalf("calls=%v want %v", m.calls, tc.calls)
			}
		})
	}
	// A stateless harness does not need one.
	m := &mockControl{info: running("worker-a"), getNoLineage: true, suspendNoLineage: true}
	m.exists("sess-x", substrate.StatusRunning)
	if _, err := backendFor(m, false).Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal); err != nil {
		t.Fatalf("stateless snapshot: %v", err)
	}
}

// A memory-tier actor is attached to, resumed or captured only while it holds the checkpoint the
// session's journal last recorded: the snapshot that SUSPEND names or, with none recorded, no
// capture at all (NoCapture). Anything else is a capture the journal never accepted, such as one a
// Snapshot refused, and is refused with api.ErrMemoryStateLost before ResumeActor or SuspendActor,
// suspended or already resumed by something else. The refusal names no snapshot URI and is logged
// at Warn. A recorded checkpoint that cannot be read refuses without being loss. A stateless
// harness is not checked.
func TestMemoryActorMustHoldTheRecordedCheckpoint(t *testing.T) {
	const recordedURI = "gs://snap/sess-x"
	errRead := errors.New("journal unreadable")
	for _, tc := range []struct {
		name     string
		status   substrate.ActorStatus
		holds    string // the actor's snapshot; a non-empty one is a capture
		recorded string // the recorded checkpoint's URI, "" for none
		unknown  bool   // the client cannot tell whether the actor holds a capture
		readErr  bool
		want     error // nil: attached, resumed or captured
	}{
		{name: "suspended on the recorded checkpoint", status: substrate.StatusSuspended, holds: recordedURI, recorded: recordedURI},
		{name: "running from the recorded checkpoint", status: substrate.StatusRunning, holds: recordedURI, recorded: recordedURI},
		{name: "running, nothing recorded and no capture", status: substrate.StatusRunning},
		{name: "suspended on another capture", status: substrate.StatusSuspended, holds: "gs://snap/sess-x/gen-2", recorded: recordedURI, want: api.ErrMemoryStateLost},
		{name: "running from another capture", status: substrate.StatusRunning, holds: "gs://snap/sess-x/gen-2", recorded: recordedURI, want: api.ErrMemoryStateLost},
		{name: "suspended on a capture, nothing recorded", status: substrate.StatusSuspended, holds: recordedURI, want: api.ErrMemoryStateLost},
		{name: "running from a capture, nothing recorded", status: substrate.StatusRunning, holds: recordedURI, want: api.ErrMemoryStateLost},
		{name: "running, nothing recorded, capture unknown", status: substrate.StatusRunning, unknown: true, want: api.ErrMemoryStateLost},
		{name: "recorded checkpoint unreadable", status: substrate.StatusSuspended, holds: recordedURI, recorded: recordedURI, readErr: true, want: errRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, memory := range []bool{true, false} {
				for _, op := range []string{"create", "restore", "snapshot"} {
					if tc.readErr && op != "create" {
						continue // only Create reads the journal; Restore and Snapshot are handed it
					}
					var output bytes.Buffer
					logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
					m := &mockControl{info: running("worker-a"), captureUnknown: tc.unknown}
					m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
					m.exists("sess-x", tc.status)
					if tc.holds != "" {
						m.capture("sess-x")
						m.holds("sess-x", tc.holds)
					}
					resumability := api.ResumabilityStatelessReplay
					if memory {
						resumability = api.ResumabilityRequiresMemorySnapshot
					}
					b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
						api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: resumability}},
						substrate.WithLogger(logger))
					recorded := api.SnapshotRef{}
					if tc.recorded != "" {
						recorded = api.SnapshotRef{Local: "sess-x", ExternalURI: tc.recorded, Memory: true}
					}
					var err error
					switch op {
					case "create":
						_, err = b.Create(t.Context(), &api.SessionSpec{SessionUID: "sess-x", RecordedCheckpoint: func() (api.SnapshotRef, error) {
							m.calls = append(m.calls, "read-checkpoint")
							if tc.readErr {
								return api.SnapshotRef{}, errRead
							}
							return recorded, nil
						}})
					case "restore":
						ref := recorded
						if ref.Local == "" {
							ref.Local = "sess-x" // Placer.Resume's ref when the journal records no SUSPEND
						}
						_, err = b.Restore(t.Context(), ref)
					case "snapshot":
						_, err = b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x", RecordedCheckpoint: recorded}, api.SnapshotExternal)
					}
					acted := slices.ContainsFunc(m.calls, func(c string) bool {
						return strings.HasPrefix(c, "resume:") || strings.HasPrefix(c, "suspend:")
					})
					if !memory && op == "restore" && tc.status == substrate.StatusSuspended && tc.holds != tc.recorded && tc.recorded != "" {
						// A stateless Restore already refuses a held ref a suspended actor no longer holds.
						if !errors.Is(err, api.ErrInvalidCheckpoint) || errors.Is(err, api.ErrMemoryStateLost) {
							t.Fatalf("stateless restore of a stale ref: %v, want ErrInvalidCheckpoint only", err)
						}
						continue
					}
					if !memory || tc.want == nil {
						if err != nil {
							t.Fatalf("memory=%v %s: %v, want success", memory, op, err)
						}
						if !memory && slices.Contains(m.calls, "read-checkpoint") {
							t.Fatalf("stateless %s read the recorded checkpoint: %v", op, m.calls)
						}
						continue
					}
					if !errors.Is(err, tc.want) || acted {
						t.Fatalf("%s: err=%v calls=%v, want %v before any resume or suspend", op, err, m.calls, tc.want)
					}
					if tc.readErr {
						if errors.Is(err, api.ErrMemoryStateLost) {
							t.Fatalf("%s: an unreadable journal is not lost state: %v", op, err)
						}
						continue
					}
					if strings.Contains(err.Error(), "gs://") || !strings.Contains(err.Error(), "space/sess-x") {
						t.Fatalf("%s: refusal %q must name the actor and no snapshot URI", op, err)
					}
					if !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) || !strings.Contains(output.String(), m.uid("sess-x")) {
						t.Fatalf("%s: refusal was not logged for operators: %s", op, output.String())
					}
				}
			}
		})
	}
}

// outsideSuspend is a suspend by a caller other than sessions, such as a gateway that suspends idle
// actors (agent-substrate/always-on-agent): a running actor is captured on its worker and left
// holding that capture; a suspended one is left as it is.
func (m *mockControl) outsideSuspend(name string) {
	if m.statusOf(name) == substrate.StatusRunning {
		m.capture(name)
		m.holds(name, m.captureURI(name))
	}
	m.exists(name, substrate.StatusSuspended)
}

// outsideResume is the router resuming the actor for a request addressed to it, from the snapshot it
// holds, on a worker of the same sandbox runtime, so its memory comes back.
func (m *mockControl) outsideResume(name string) { m.exists(name, substrate.StatusRunning) }

// With the router resuming suspended actors on demand and a gateway suspending idle ones, a suspend
// or resume by another caller is routine, not a narrow window. Router resumes alone are harmless:
// the actor still holds the checkpoint the journal recorded. Any capture sessions did not take is
// refused for good: repeated outside suspend and resume cycles between Snapshot's GetActor and its
// SuspendActor, a router resume of a suspended actor in that window, and an idle suspend by someone
// else at any later time each leave the actor holding a snapshot no SUSPEND names. Every later
// memory-tier attach (Create), Restore and Snapshot (Suspend, and a stateful fork's capture of the
// parent) then refuses with api.ErrMemoryStateLost before resuming or suspending it, round after
// round of the router waking it and the gateway suspending it again. That holds even when the
// memory survived, as it does here (every resume is on the same runtime): the check fails closed.
// One outside suspend in the window with no resume after it is the capture sessions would have
// taken, and is recorded. A stateless harness is not checked.
func TestOutsideSuspendsAreRefusedForGood(t *testing.T) {
	cycles := func(m *mockControl, n int) {
		for range n {
			m.outsideSuspend("sess-x")
			m.outsideResume("sess-x")
		}
	}
	for _, tc := range []struct {
		name string
		// suspended leaves the actor suspended on the recorded checkpoint instead of resuming it.
		suspended, firstSuspend bool
		// window runs between Snapshot's GetActor and its SuspendActor; later runs with no sessions
		// operation in flight. Without a window there is no second Snapshot.
		window, later func(m *mockControl)
		refused       bool
	}{
		{name: "router resumes only", suspended: true, later: func(m *mockControl) { m.outsideResume("sess-x") }},
		{name: "one outside suspend in the window", window: func(m *mockControl) { m.outsideSuspend("sess-x") }},
		{name: "suspend and resume cycles in the window", refused: true, window: func(m *mockControl) { cycles(m, 3) }},
		{name: "cycles in the window, first suspend", firstSuspend: true, refused: true, window: func(m *mockControl) { cycles(m, 3) }},
		{name: "cycles in the window, then an idle suspend", refused: true, window: func(m *mockControl) {
			cycles(m, 3)
			m.outsideSuspend("sess-x")
		}},
		{name: "suspended session woken by the router in the window", suspended: true, refused: true, window: func(m *mockControl) { m.outsideResume("sess-x") }},
		{name: "idle suspend by someone else later", refused: true, later: func(m *mockControl) { m.outsideSuspend("sess-x") }},
		{name: "router resume, then an idle suspend later", suspended: true, refused: true, later: func(m *mockControl) {
			m.outsideResume("sess-x")
			m.outsideSuspend("sess-x")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, memory := range []bool{true, false} {
				ctx := t.Context()
				var output bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
				m := &mockControl{info: running("worker-a")}
				m.exists("sess-x", substrate.StatusRunning)
				resumability := api.ResumabilityStatelessReplay
				if memory {
					resumability = api.ResumabilityRequiresMemorySnapshot
				}
				b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
					api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: resumability}},
					substrate.WithLogger(logger))
				instance := ""
				if memory {
					instance = m.uid("sess-x")
				}
				// recorded is the checkpoint the journal last recorded, as the Placer passes it.
				var recorded api.SnapshotRef
				if !tc.firstSuspend {
					ref, err := b.Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: instance}, api.SnapshotExternal)
					if err != nil {
						t.Fatalf("memory=%v: first suspend: %v", memory, err)
					}
					recorded = ref
					if !tc.suspended {
						if _, err := b.Restore(ctx, recorded); err != nil {
							t.Fatalf("memory=%v: resume: %v", memory, err)
						}
					}
				}
				if tc.window != nil {
					m.afterGet = func(string) {
						m.afterGet = nil
						tc.window(m)
					}
					if !memory {
						m.afterGet = nil // a stateless Snapshot does not look first
						tc.window(m)
					}
					ref, err := b.Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: instance, RecordedCheckpoint: recorded}, api.SnapshotExternal)
					switch {
					case !memory || !tc.refused:
						if err != nil || ref.ExternalURI == "" {
							t.Fatalf("memory=%v: snapshot: ref=%+v err=%v, want a checkpoint", memory, ref, err)
						}
						recorded = ref
					case !errors.Is(err, api.ErrMemoryStateLost) || ref != (api.SnapshotRef{}):
						t.Fatalf("snapshot: ref=%+v err=%v, want api.ErrMemoryStateLost", ref, err)
					}
				}
				if tc.later != nil {
					tc.later(m)
				}
				paths := []struct {
					name string
					run  func() error
				}{
					{"next turn", func() error {
						_, err := b.Create(ctx, &api.SessionSpec{SessionUID: "sess-x", StartedTurns: func() (int, error) { return 2, nil },
							RecordedCheckpoint: func() (api.SnapshotRef, error) { return recorded, nil }})
						return err
					}},
					{"resume", func() error {
						ref := recorded
						if ref.Local == "" {
							ref.Local = "sess-x" // Placer.Resume's ref when the journal records no SUSPEND
						}
						_, err := b.Restore(ctx, ref)
						return err
					}},
					{"suspend or fork capture", func() error {
						_, err := b.Snapshot(ctx, api.Incarnation{ID: "sess-x", ComputeInstance: instance, RecordedCheckpoint: recorded}, api.SnapshotExternal)
						return err
					}},
				}
				if !memory || !tc.refused {
					// Each path still works: attach or resume, then suspend, which the Placer records.
					for _, path := range paths {
						if err := path.run(); err != nil {
							if !memory && path.name == "resume" && errors.Is(err, api.ErrInvalidCheckpoint) && !errors.Is(err, api.ErrMemoryStateLost) {
								continue // a stateless Restore refuses a held ref the actor no longer holds (#195)
							}
							t.Fatalf("memory=%v %s: %v, want success", memory, path.name, err)
						}
						if path.name == "suspend or fork capture" {
							recorded = api.SnapshotRef{Local: "sess-x", ExternalURI: m.snapshots["sess-x"], Memory: true}
						}
					}
					if memory && output.Len() != 0 {
						t.Fatalf("logged %s", output.String())
					}
					continue
				}
				// Round after round, the router wakes the actor for a message and the gateway suspends
				// it again once idle; every path is refused in both states, touching nothing.
				for round := range 3 {
					for _, woken := range []bool{false, true} {
						if woken {
							m.outsideResume("sess-x")
						} else if round > 0 {
							m.outsideSuspend("sess-x")
						}
						for _, path := range paths {
							m.calls = nil
							err := path.run()
							if !errors.Is(err, api.ErrMemoryStateLost) || strings.Contains(err.Error(), "gs://") {
								t.Fatalf("round %d woken=%v %s: %v, want api.ErrMemoryStateLost naming no snapshot URI", round, woken, path.name, err)
							}
							if slices.ContainsFunc(m.calls, func(c string) bool {
								return strings.HasPrefix(c, "resume:") || strings.HasPrefix(c, "suspend:") || strings.HasPrefix(c, "create:")
							}) {
								t.Fatalf("round %d woken=%v %s: calls=%v, want no create, resume or suspend", round, woken, path.name, m.calls)
							}
						}
					}
				}
				if !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) {
					t.Fatalf("refusals were not logged for operators: %s", output.String())
				}
			}
		})
	}
}

// A memory-tier actor paused outside sessions is suspended on a node-local capture: not ready, not
// pending, no durable URI. That capture is not the checkpoint the journal recorded, so Create's
// attach and Restore refuse it with api.ErrMemoryStateLost and log it for the operator, as Suspend
// and Fork do, not as "not ready", which would suggest a Suspend makes it usable. A pending upload
// stays "not ready", and a stateless actor in the same state is still only "not ready".
func TestPausedMemoryActorIsLostOnAttachAndRestore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded string // the journal's last SUSPEND, "" for none
		pending  bool
		memory   bool
		lost     bool
	}{
		{name: "paused after a recorded suspend", recorded: "gs://snap/s", memory: true, lost: true},
		{name: "paused before any suspend", memory: true, lost: true},
		{name: "upload pending", recorded: "gs://snap/s", pending: true, memory: true},
		{name: "stateless paused", recorded: "gs://snap/s"},
	} {
		for _, path := range []string{"create", "restore"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				var output bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
				m := &mockControl{info: running("w")}
				m.exists("s", substrate.StatusSuspended)
				m.capture("s")
				m.holds("s", "") // a not-ready generation exposes no URI
				m.info.SnapshotNotReady, m.info.SnapshotPending = true, tc.pending
				m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
				resumability := api.ResumabilityStatelessReplay
				if tc.memory {
					resumability = api.ResumabilityRequiresMemorySnapshot
				}
				b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
					api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: resumability}},
					substrate.WithLogger(logger))
				ref := api.SnapshotRef{Local: "s", ExternalURI: tc.recorded, Memory: true}
				var err error
				if path == "create" {
					_, err = b.Create(t.Context(), &api.SessionSpec{SessionUID: "s", RecordedCheckpoint: func() (api.SnapshotRef, error) { return ref, nil }})
				} else {
					_, err = b.Restore(t.Context(), ref)
				}
				for _, call := range m.calls {
					if strings.HasPrefix(call, "resume:") || strings.HasPrefix(call, "suspend:") {
						t.Fatalf("refused %s made call %q (calls=%v)", path, call, m.calls)
					}
				}
				logged := strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) && strings.Contains(output.String(), m.uid("s"))
				if tc.lost {
					if !errors.Is(err, api.ErrMemoryStateLost) || errors.Is(err, substrate.ErrSnapshotNotReady) || errors.Is(err, api.ErrInvalidCheckpoint) {
						t.Fatalf("%s: err=%v, want api.ErrMemoryStateLost, not a not-ready refusal", path, err)
					}
					if !logged {
						t.Fatalf("%s: refusal not logged for operators: %s", path, output.String())
					}
					return
				}
				if !errors.Is(err, substrate.ErrSnapshotNotReady) || errors.Is(err, api.ErrMemoryStateLost) || errors.Is(err, api.ErrCheckpointPending) != tc.pending || logged {
					t.Fatalf("%s: err=%v logs=%s, want only a not-ready refusal (pending=%v)", path, err, output.String(), tc.pending)
				}
			})
		}
	}
}

// Another host's Suspend leaves a memory-tier actor SUSPENDING, its newest generation's upload
// PENDING, until it completes and that host records the capture. Snapshot reports the actor as
// ErrActorBusy (ABORTED, retryable) before the checkpoint checks, which would read the pending
// capture as one the journal never recorded and refuse it as api.ErrMemoryStateLost. It does not
// suspend the actor or log a refusal for operators.
func TestSnapshotOfATransitioningMemoryActorIsBusy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool // the journal records an earlier SUSPEND
	}{
		{name: "after a recorded suspend", recorded: true},
		{name: "before any recorded suspend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
			m := &mockControl{info: running("worker-a")}
			m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
			m.exists("sess-x", substrate.StatusRunning)
			in := api.Incarnation{ID: "sess-x", ComputeInstance: m.uid("sess-x")}
			if tc.recorded {
				m.capture("sess-x")
				in.RecordedCheckpoint = api.SnapshotRef{Local: "sess-x", ExternalURI: m.captureURI("sess-x"), Memory: true}
			}
			// The other host's suspend: a new generation whose upload is pending exposes no URI.
			m.capture("sess-x")
			m.holds("sess-x", "")
			m.exists("sess-x", substrate.StatusTransitioning)
			m.info.SnapshotNotReady, m.info.SnapshotPending = true, true
			b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
				api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}},
				substrate.WithLogger(logger))
			ref, err := b.Snapshot(t.Context(), in, api.SnapshotExternal)
			if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, api.ErrComputeBusy) ||
				errors.Is(err, api.ErrMemoryStateLost) || errors.Is(err, api.ErrCheckpointPending) || ref != (api.SnapshotRef{}) {
				t.Fatalf("snapshot: ref=%+v err=%v, want only ErrActorBusy", ref, err)
			}
			if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("calls=%v want %v", m.calls, want)
			}
			if !strings.Contains(err.Error(), "space/sess-x") {
				t.Fatalf("refusal %q does not name the actor", err)
			}
			if strings.Contains(output.String(), `"level":"WARN"`) || strings.Contains(output.String(), "memory_state_lost") {
				t.Fatalf("a busy actor was logged as a refusal: %s", output.String())
			}
		})
	}
}

// With no recorded instance (a journal written before compute_instance, or a fork child before its
// first turn) requireInstance accepts any UID, and an actor deleted and created again under the
// name, from the same template, between the check and the suspend reports the lineage checked.
// Snapshot compares the UIDs as well and refuses the replacement's capture with
// api.ErrMemoryStateLost and no ref, leaving it suspended.
func TestSnapshotRefusesAnActorReplacedBetweenTheCheckAndTheSuspend(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m := &mockControl{info: running("worker-a")}
	m.exists("sess-x", substrate.StatusRunning)
	checked := m.uid("sess-x")
	m.afterGet = func(name string) {
		m.afterGet = nil
		m.delete(name)
		m.exists(name, substrate.StatusRunning)
	}
	b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
		api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}},
		substrate.WithLogger(logger))
	ref, err := b.Snapshot(t.Context(), api.Incarnation{ID: "sess-x"}, api.SnapshotExternal)
	replacement := m.uid("sess-x")
	if replacement == checked {
		t.Fatalf("the actor was not replaced: UID %q", replacement)
	}
	if !errors.Is(err, api.ErrMemoryStateLost) || ref != (api.SnapshotRef{}) {
		t.Fatalf("snapshot: ref=%+v err=%v, want api.ErrMemoryStateLost", ref, err)
	}
	if want := []string{"get:sess-x", "suspend:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want %v", m.calls, want)
	}
	for _, part := range []string{"space/sess-x", checked, replacement, "no checkpoint was recorded", "actor remains suspended"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("refusal %q does not name %q", err, part)
		}
	}
	if strings.Contains(err.Error(), "gs://") {
		t.Fatalf("refusal %q names the snapshot URI", err)
	}
	if !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) || !strings.Contains(output.String(), replacement) {
		t.Fatalf("refusal was not logged for operators: %s", output.String())
	}
}

// Between the check before a resume and the resume, another operation can leave the actor holding
// a capture the journal never accepted: here another host's Snapshot, which captured foreign RAM,
// refused it and left the actor SUSPENDED on it. ResumeActor restores whatever the actor holds by
// then, on the runtime that capture is stamped with, so the actor comes back with the same UID and
// MemoryNotRestored false. Restore and Create's attach check the actor the resume reports against
// the recorded checkpoint (attach reads it again) and refuse it with api.ErrMemoryStateLost: no
// incarnation, the actor left running and neither suspended nor deleted, the refusal logged for
// operators. Every later attach, Restore and Snapshot then refuses it before any resume or suspend.
// A SUSPEND another host recorded in that window is read by attach and accepted; Restore, which
// holds the ref it was given, refuses it, and a retry with the new ref gets the running actor. A
// stateless session is not checked.
func TestResumeThatRestoresAnUnrecordedCaptureIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string // "restore" or "attach"
		// recordedMeanwhile: the capture in the window is another host's, and its SUSPEND is in
		// the journal by the time attach reads it again.
		recordedMeanwhile bool
	}{
		{name: "restore", op: "restore"},
		{name: "attach", op: "attach"},
		{name: "attach after another host recorded the capture", op: "attach", recordedMeanwhile: true},
		{name: "restore after another host recorded the capture", op: "restore", recordedMeanwhile: true},
	} {
		for _, memory := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/memory=%v", tc.name, memory), func(t *testing.T) {
				ctx := t.Context()
				var output bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
				m := &mockControl{info: running("worker-a")}
				m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
				// The session was suspended once; the journal records that capture.
				m.exists("sess-x", substrate.StatusSuspended)
				m.capture("sess-x")
				m.holds("sess-x", m.captureURI("sess-x"))
				recorded := api.SnapshotRef{Local: "sess-x", ExternalURI: m.captureURI("sess-x"), Memory: true}
				var other api.SnapshotRef
				m.afterGet = func(name string) {
					m.afterGet = nil
					// The other operation: woken, captured and left SUSPENDED on the capture.
					m.capture(name)
					m.holds(name, m.captureURI(name))
					other = api.SnapshotRef{Local: name, ExternalURI: m.captureURI(name), Memory: true}
				}
				resumability := api.ResumabilityStatelessReplay
				if memory {
					resumability = api.ResumabilityRequiresMemorySnapshot
				}
				b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
					api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: resumability}},
					substrate.WithLogger(logger))
				reads := 0
				spec := &api.SessionSpec{SessionUID: "sess-x", StartedTurns: func() (int, error) { return 1, nil },
					RecordedCheckpoint: func() (api.SnapshotRef, error) {
						// The other host's SUSPEND lands after attach's first read.
						if reads++; tc.recordedMeanwhile && reads > 1 {
							return other, nil
						}
						return recorded, nil
					}}
				var inc api.Incarnation
				var err error
				if tc.op == "restore" {
					inc, err = b.Restore(ctx, recorded)
				} else {
					inc, err = b.Create(ctx, spec)
				}
				if want := []string{"get:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
					t.Fatalf("calls=%v want %v", m.calls, want)
				}
				if memory && tc.recordedMeanwhile && tc.op == "restore" {
					// Restore's ref was read before the other host's SUSPEND: refused. A retry
					// reads the new ref and gets the actor the resume left running.
					if !errors.Is(err, api.ErrMemoryStateLost) || !reflect.DeepEqual(inc, api.Incarnation{}) {
						t.Fatalf("restore: inc=%+v err=%v, want api.ErrMemoryStateLost and no incarnation", inc, err)
					}
					m.calls = nil
					if inc, err = b.Restore(ctx, other); err != nil || inc.ID != "sess-x" {
						t.Fatalf("restore retried with the new ref: inc=%+v err=%v, want the actor handed out", inc, err)
					}
					if want := []string{"get:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
						t.Fatalf("retry calls=%v want %v", m.calls, want)
					}
					return
				}
				if !memory || tc.recordedMeanwhile {
					if err != nil || inc.ID != "sess-x" {
						t.Fatalf("%s: inc=%+v err=%v, want the actor handed out", tc.op, inc, err)
					}
					return
				}
				if !errors.Is(err, api.ErrMemoryStateLost) || !reflect.DeepEqual(inc, api.Incarnation{}) {
					t.Fatalf("%s: inc=%+v err=%v, want api.ErrMemoryStateLost and no incarnation", tc.op, inc, err)
				}
				if tc.op == "restore" && !errors.Is(err, substrate.ErrSnapshotNotRestorable) {
					t.Fatalf("restore: %v, want it to wrap ErrSnapshotNotRestorable", err)
				}
				if st := m.statusOf("sess-x"); st != substrate.StatusRunning {
					t.Fatalf("actor is %v, want it left running", st)
				}
				for _, part := range []string{"space/sess-x", m.uid("sess-x"), "left running", "delete it"} {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("refusal %q does not say %q", err, part)
					}
				}
				if strings.Contains(err.Error(), "gs://") {
					t.Fatalf("refusal %q names a snapshot URI", err)
				}
				if !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) || !strings.Contains(output.String(), `"actor_status":"running"`) {
					t.Fatalf("refusal was not logged for operators: %s", output.String())
				}
				// It stays refused, before any resume or suspend.
				for name, path := range map[string]func() error{
					"attach":  func() error { _, err := b.Create(ctx, spec); return err },
					"restore": func() error { _, err := b.Restore(ctx, recorded); return err },
					"snapshot": func() error {
						_, err := b.Snapshot(ctx, api.Incarnation{ID: "sess-x", RecordedCheckpoint: recorded}, api.SnapshotExternal)
						return err
					},
				} {
					m.calls = nil
					if err := path(); !errors.Is(err, api.ErrMemoryStateLost) {
						t.Fatalf("later %s: %v, want api.ErrMemoryStateLost", name, err)
					}
					if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
						t.Fatalf("later %s: calls=%v want %v", name, m.calls, want)
					}
				}
			})
		}
	}
}

// A clone is SUSPENDED on the tag's snapshot between CreateActorFromTag and ResumeActor, so another
// caller can resume it and capture it (suspend or pause) in that window. Fork's resume then
// restores that capture: the response reports the clone's UID and no MemoryNotRestored, but the
// clone holds RAM that is not the parent's at the fork point, and the child's journal, which
// records no checkpoint, could never accept it. Fork refuses it with ErrActorBusy (ABORTED), as
// for a clone replaced before its resume: it returns no incarnation, logs a Warn naming the clone
// and its UID, and tears the clone down (suspend, delete, untag). The parent is untouched, so a
// retry clones a new child. A client that cannot show the clone holds no capture, while the clone
// still holds the snapshot it was created with or reports none to compare (its ResumeActor omits
// Snapshot too), is torn down the same way, but a retry fails alike, so it is
// api.ErrMemoryStateLost (FAILED_PRECONDITION), not busy, and the refusal and the Warn say the
// client cannot tell rather than that the clone was captured. When the teardown's suspend is
// refused (another caller holds the clone's lease), the refusal and a second Warn say the clone and
// its tag were left in place; when the clone is already gone (another caller deleted it), it counts
// as torn down and its tag is released.
func TestForkRefusesACloneThatHoldsItsOwnCapture(t *testing.T) {
	captured := func(m *mockCloner, name string) {
		m.capture(name)
		m.holds(name, m.captureURI(name))
	}
	const (
		heldText    = "holds a capture of its own"
		unknownText = "cannot show it holds no capture of its own"
	)
	for _, tc := range []struct {
		name       string
		afterClone func(m *mockCloner, name string)
		refused    string // the refusal's account of the capture, "" when the clone is handed out
		stuck      bool   // the teardown's suspend is refused, so the clone is left in place
		gone       bool   // another caller deleted the clone before the teardown's suspend
		noSnapshot bool   // ResumeActor reports no Snapshot to compare with the created one
	}{
		{name: "nothing in between"},
		{name: "resumed and captured by another caller", refused: heldText, afterClone: captured},
		{name: "captured with no completed durable copy", refused: heldText, afterClone: func(m *mockCloner, name string) {
			// As the adapter reports it: the newest snapshot is not ready, so none is exposed.
			m.capture(name)
			m.holds(name, "")
			m.info.SnapshotNotReady = true
		}},
		{name: "the client cannot tell", refused: unknownText, afterClone: func(m *mockCloner, _ string) { m.captureUnknown = true }},
		{name: "the client reports neither NoCapture nor the resumed Snapshot", refused: unknownText, noSnapshot: true, afterClone: func(m *mockCloner, _ string) {
			m.captureUnknown = true
			m.resumeNoSnapshot = true
		}},
		{name: "captured, and the teardown is refused busy", refused: heldText, stuck: true, afterClone: func(m *mockCloner, name string) {
			captured(m, name)
			m.suspendErr = fmt.Errorf("lease held: %w", substrate.ErrActorBusy)
		}},
		{name: "captured, and the clone is deleted before the teardown", refused: heldText, gone: true, afterClone: func(m *mockCloner, name string) {
			captured(m, name)
			m.suspendErr = fmt.Errorf("deleted: %w", substrate.ErrActorNotFound)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
			m := newClonerWithSuspendedParent()
			var cloneUID string
			m.afterClone = func(name string) {
				cloneUID = m.uids[name]
				if tc.afterClone != nil {
					tc.afterClone(m, name)
				}
			}
			b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
				api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}},
				substrate.WithLogger(logger))
			inc, err := b.Fork(t.Context(), api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true},
				api.ForkOpts{ChildSessionUID: "child"})
			head := []string{"get:parent", "tag:parent->fork-child", "get:parent", "clone:child:from=fork-child", "resume:child"}
			if tc.refused == "" {
				if err != nil || inc.ComputeInstance != cloneUID {
					t.Fatalf("fork: inc=%+v err=%v, want the clone %q handed out", inc, err, cloneUID)
				}
				if !reflect.DeepEqual(m.calls, head) {
					t.Fatalf("calls=%v want %v", m.calls, head)
				}
				if output.Len() != 0 {
					t.Fatalf("an accepted clone was logged: %s", output.String())
				}
				return
			}
			if !reflect.DeepEqual(inc, api.Incarnation{}) {
				t.Fatalf("fork: inc=%+v err=%v, want no incarnation", inc, err)
			}
			parts := []string{"space/child", cloneUID, tc.refused}
			kind := "actor_busy"
			if tc.refused == heldText {
				// A retry clones a new child: busy (ABORTED), not a lost session.
				if !errors.Is(err, substrate.ErrActorBusy) || !errors.Is(err, api.ErrComputeBusy) || errors.Is(err, api.ErrMemoryStateLost) {
					t.Fatalf("fork: %v, want substrate.ErrActorBusy and not api.ErrMemoryStateLost", err)
				}
				parts = append(parts, "the parent is unaffected and a retry clones a new child")
			} else {
				// A retry is refused alike, so it must not be busy (ABORTED invites a retry).
				if !errors.Is(err, api.ErrMemoryStateLost) || errors.Is(err, api.ErrComputeBusy) {
					t.Fatalf("fork: %v, want api.ErrMemoryStateLost and not api.ErrComputeBusy", err)
				}
				kind = "memory_state_lost"
				parts = append(parts, "the parent is unaffected, but a retry is refused the same way while the client cannot report it")
				if tc.noSnapshot {
					parts = append(parts, "nor the snapshot it holds to compare with the one it was created with")
				} else {
					parts = append(parts, "although it still holds the snapshot it was created with")
				}
			}
			if !tc.stuck {
				parts = append(parts, "the fork tears the clone down and creates no child")
			}
			for _, part := range parts {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("refusal %q does not say %q", err, part)
				}
			}
			for _, other := range []string{heldText, unknownText} {
				if other != tc.refused && strings.Contains(err.Error(), other) {
					t.Fatalf("refusal %q says %q", err, other)
				}
			}
			if strings.Contains(err.Error(), "gs://") {
				t.Fatalf("refusal %q names a snapshot URI", err)
			}
			if st := m.statusOf("parent"); st != substrate.StatusSuspended || m.snapshots["parent"] != "snap-parent-1" {
				t.Fatalf("parent is %v holding %q, want it untouched", st, m.snapshots["parent"])
			}
			logs := output.String()
			for _, part := range []string{`"msg":"refused a fork clone that holds a capture of its own"`, `"atespace":"space"`,
				`"actor":"child"`, `"actor_uid":"` + cloneUID + `"`, `"error_kind":"` + kind + `"`, `"capture":"` + map[string]string{heldText: "held", unknownText: "unknown"}[tc.refused] + `"`} {
				if !strings.Contains(logs, part) {
					t.Fatalf("refusal was not logged for operators with %s: %s", part, logs)
				}
			}
			if strings.Contains(logs, "gs://") {
				t.Fatalf("the log names a snapshot URI: %s", logs)
			}
			if tc.stuck {
				if want := append(slices.Clone(head), "suspend:child"); !reflect.DeepEqual(m.calls, want) {
					t.Fatalf("calls=%v want %v", m.calls, want)
				}
				if _, ok := m.actors["child"]; !ok {
					t.Fatal("the clone is gone although its teardown's suspend was refused")
				}
				if !strings.Contains(err.Error(), "the fork creates no child, but tearing the clone down failed") || !strings.Contains(err.Error(), `"fork-child"`) ||
					!strings.Contains(err.Error(), "lease held") || strings.Contains(err.Error(), "tears the clone down") {
					t.Fatalf("refusal %q does not say, and only say, that the clone and its tag fork-child were left in place, and why", err)
				}
				if !strings.Contains(logs, `"msg":"failed to tear down a refused fork clone"`) || !strings.Contains(logs, `"tag":"fork-child"`) {
					t.Fatalf("the failed teardown was not logged for operators: %s", logs)
				}
				return
			}
			if strings.Contains(err.Error(), "tearing the clone down failed") || strings.Contains(logs, "failed to tear down") {
				t.Fatalf("refusal %q reports a failed teardown: %s", err, logs)
			}
			want := append(slices.Clone(head), "suspend:child", "delete:child", "untag:fork-child")
			if tc.gone {
				m.delete("child") // what the other caller did; the mock reports NotFound from suspendErr only
				want = append(slices.Clone(head), "suspend:child", "untag:fork-child")
			}
			if !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("calls=%v want the clone torn down %v", m.calls, want)
			}
			if _, ok := m.actors["child"]; ok {
				t.Fatal("the refused clone still exists")
			}
		})
	}
}

// Between CreateActor and its ResumeActor, the actor Create just made is SUSPENDED under the
// session's name, so another host's attach or the router can resume it and any caller can then
// capture it; ResumeActor restores that capture with the new actor's UID. For a memory-tier session
// Create checks the actor the resume reports against the recorded checkpoint, read after the
// resume, as attach does: with none recorded the capture is refused with api.ErrMemoryStateLost
// (not ErrActorBusy), the actor is left running and is neither suspended nor deleted, the refusal
// is logged for operators, and the next turn's attach refuses it the same way before any resume.
// A SUSPEND another host recorded for that capture in the window is accepted. A client that cannot
// show the new actor holds no capture is refused too. A stateless session is not checked.
func TestCreateRefusesANewActorCapturedBeforeItsResume(t *testing.T) {
	for _, tc := range []struct {
		name string
		// captured: another caller resumed and captured the new actor before Create's resume.
		captured bool
		// recordedMeanwhile: that capture is another host's, and its SUSPEND is in the journal by
		// the time Create reads the recorded checkpoint.
		recordedMeanwhile bool
		// captureUnknown: the client cannot tell whether the actor holds a capture.
		captureUnknown bool
		memory         bool
		refused        bool
	}{
		{name: "memory/not captured", memory: true},
		{name: "memory/captured", captured: true, memory: true, refused: true},
		{name: "memory/captured and recorded by another host", captured: true, recordedMeanwhile: true, memory: true},
		{name: "memory/client cannot tell", captureUnknown: true, memory: true, refused: true},
		{name: "stateless/captured", captured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}))
			m := &mockControl{info: running("worker-a"), captureUnknown: tc.captureUnknown}
			m.info.SnapshotFidelity = substrate.SnapshotFidelityMemory
			var other api.SnapshotRef
			if tc.captured {
				m.afterCreate = func(name string) {
					m.afterCreate = nil
					// The other caller: woke the new actor, captured it and left it SUSPENDED on it.
					m.capture(name)
					m.holds(name, m.captureURI(name))
					m.exists(name, substrate.StatusSuspended)
					other = api.SnapshotRef{Local: name, ExternalURI: m.captureURI(name), Memory: true}
				}
			}
			resumability := api.ResumabilityStatelessReplay
			if tc.memory {
				resumability = api.ResumabilityRequiresMemorySnapshot
			}
			b := substrate.New(m, "space", substrate.ObjectRef{Atespace: "tmpl", Name: "counter"},
				api.Descriptor{ID: "counter", Capabilities: api.Capabilities{Resumability: resumability}},
				substrate.WithLogger(logger))
			spec := &api.SessionSpec{SessionUID: "sess-x", StartedTurns: func() (int, error) { return 0, nil },
				RecordedCheckpoint: func() (api.SnapshotRef, error) {
					if tc.recordedMeanwhile {
						return other, nil
					}
					return api.SnapshotRef{}, nil
				}}
			inc, err := b.Create(ctx, spec)
			if want := []string{"get:sess-x", "create:sess-x", "resume:sess-x"}; !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("calls=%v want %v", m.calls, want)
			}
			if !tc.refused {
				if err != nil || inc.ID != "sess-x" {
					t.Fatalf("create: inc=%+v err=%v, want the actor handed out", inc, err)
				}
				return
			}
			if !errors.Is(err, api.ErrMemoryStateLost) || errors.Is(err, substrate.ErrActorBusy) || !reflect.DeepEqual(inc, api.Incarnation{}) {
				t.Fatalf("create: inc=%+v err=%v, want api.ErrMemoryStateLost, not ErrActorBusy, and no incarnation", inc, err)
			}
			if st := m.statusOf("sess-x"); st != substrate.StatusRunning {
				t.Fatalf("actor is %v, want it left running", st)
			}
			for _, part := range []string{"space/sess-x", m.uid("sess-x"), "left running", "delete it"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("refusal %q does not say %q", err, part)
				}
			}
			if strings.Contains(err.Error(), "gs://") {
				t.Fatalf("refusal %q names a snapshot URI", err)
			}
			if !strings.Contains(output.String(), `"error_kind":"memory_state_lost"`) || !strings.Contains(output.String(), `"actor_status":"running"`) {
				t.Fatalf("refusal was not logged for operators: %s", output.String())
			}
			// The next turn's attach refuses it the same way, before any resume.
			m.calls = nil
			if _, err := b.Create(ctx, spec); !errors.Is(err, api.ErrMemoryStateLost) {
				t.Fatalf("next turn: %v, want api.ErrMemoryStateLost", err)
			}
			if want := []string{"get:sess-x"}; !reflect.DeepEqual(m.calls, want) {
				t.Fatalf("next turn: calls=%v want %v", m.calls, want)
			}
		})
	}
}
