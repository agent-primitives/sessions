package placement_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// routedBackend is a placement.Backend whose incarnations sit behind a shared ingress, the shape
// substrate's atenet-router gives them: one address for every session, and call metadata that names
// the session's sandbox.
type routedBackend struct {
	address string
	desc    api.Descriptor

	mu        sync.Mutex
	snapshots int
	// onSnapshot runs inside Snapshot, standing in for the runtime's checkpoint.
	onSnapshot func()
	// snapshotErr, when set, fails the next Snapshot (after onSnapshot) and is then cleared.
	snapshotErr error
	// onFork runs inside Fork with the child's session UID, standing in for the runtime's clone.
	onFork func(child string)
	// forkErr fails Fork, after onFork, for the child session UIDs it names.
	forkErr map[string]error
	// onStop runs inside Stop, standing in for the runtime's teardown.
	onStop func()
}

func (b *routedBackend) Describe(context.Context) (api.Descriptor, error) { return b.desc, nil }

func (b *routedBackend) Create(_ context.Context, s *api.SessionSpec) (api.Incarnation, error) {
	return api.Incarnation{
		ID:           s.SessionUID,
		Address:      b.address,
		CallMetadata: map[string]string{"ate-target-actor": "space/" + s.SessionUID},
		Runtime:      "routed",
	}, nil
}

func (b *routedBackend) Snapshot(_ context.Context, in api.Incarnation, _ api.SnapshotKind) (api.SnapshotRef, error) {
	b.mu.Lock()
	b.snapshots++
	failWith := b.snapshotErr
	b.snapshotErr = nil
	b.mu.Unlock()
	if b.onSnapshot != nil {
		b.onSnapshot()
	}
	if failWith != nil {
		return api.SnapshotRef{}, failWith
	}
	return api.SnapshotRef{Local: in.ID, ExternalURI: "snap-" + in.ID, Memory: true}, nil
}

func (b *routedBackend) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	return b.Create(ctx, &api.SessionSpec{SessionUID: ref.Local})
}

func (b *routedBackend) Fork(ctx context.Context, _ api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	if b.onFork != nil {
		b.onFork(opts.ChildSessionUID)
	}
	if err := b.forkErr[opts.ChildSessionUID]; err != nil {
		return api.Incarnation{}, err
	}
	return b.Create(ctx, &api.SessionSpec{SessionUID: opts.ChildSessionUID})
}

func (b *routedBackend) Stop(context.Context, api.Incarnation) error {
	if b.onStop != nil {
		b.onStop()
	}
	return nil
}

func (b *routedBackend) Status(context.Context, api.Incarnation) (api.ComputeState, error) {
	return api.ComputeLive, nil
}

func (b *routedBackend) Capabilities() api.RuntimeCapabilities {
	return api.RuntimeCapabilities{MemorySnapshot: true}
}

// harnessServer serves a harness over harnesswire on loopback TCP and records, per Connect stream, the
// metadata the stream arrived with and when its handler returned.
type harnessServer struct {
	addr string

	mu      sync.Mutex
	targets []string
	// unaryTargets is the ate-target-actor metadata of each unary call (Describe), in order.
	unaryTargets [][]string
	started      chan struct{}
	finished     chan struct{}
}

func startHarnessServer(t *testing.T, h api.Harness) *harnessServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &harnessServer{addr: lis.Addr().String(), started: make(chan struct{}, 8), finished: make(chan struct{}, 8)}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		hs.mu.Lock()
		hs.unaryTargets = append(hs.unaryTargets, md.Get("ate-target-actor"))
		hs.mu.Unlock()
		return handler(ctx, req)
	}), grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		hs.mu.Lock()
		hs.targets = append(hs.targets, md.Get("ate-target-actor")...)
		hs.mu.Unlock()
		hs.started <- struct{}{}
		defer func() { hs.finished <- struct{}{} }()
		return handler(srv, ss)
	}))
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(h))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return hs
}

func (hs *harnessServer) seenTargets() []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]string(nil), hs.targets...)
}

func (hs *harnessServer) seenUnaryTargets() [][]string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return slices.Clone(hs.unaryTargets)
}

var statelessEcho = api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}

// A routed incarnation shares its address with every other session, so the metadata is the only
// thing that picks the sandbox. The default dialer must put it on the harness stream itself, not
// just on the connection's first call.
func TestDefaultDialAttachesCallMetadataToTheHarnessStream(t *testing.T) {
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	p := placement.New(&routedBackend{address: hs.addr, desc: statelessEcho}, echoagent.Model)
	if _, err := p.Exec(context.Background(), store.Session("s1"), "s1", []api.Message{*api.TextMessage("user", "hi")}, 0); err != nil {
		t.Fatalf("exec through the routed address: %v", err)
	}
	if got := hs.seenTargets(); len(got) != 1 || got[0] != "space/s1" {
		t.Fatalf("Connect stream carried ate-target-actor=%v, want [space/s1]", got)
	}
}

// The incarnation, not the caller, decides which sandbox a call reaches. Whatever the caller already
// put on the context for the same key, in any letter case and with any number of values, every call
// must carry exactly one ate-target-actor: the incarnation's. Two values would let a router that
// reads the last one route to an actor an upstream check that read the first one never approved.
func TestDefaultDialCallMetadataOverridesTheCaller(t *testing.T) {
	const mine, other = "space/mine", "space/someone-else"
	cases := []struct {
		name   string
		caller func(context.Context) context.Context
	}{
		{"appended value", func(ctx context.Context) context.Context {
			return metadata.AppendToOutgoingContext(ctx, "ate-target-actor", other)
		}},
		{"both values, other last", func(ctx context.Context) context.Context {
			return metadata.AppendToOutgoingContext(ctx, "ate-target-actor", mine, "ate-target-actor", other)
		}},
		{"mixed-case key", func(ctx context.Context) context.Context {
			return metadata.NewOutgoingContext(ctx, metadata.MD{"Ate-Target-Actor": {other}})
		}},
		{"mixed-case key and appended value", func(ctx context.Context) context.Context {
			ctx = metadata.NewOutgoingContext(ctx, metadata.MD{"ATE-TARGET-ACTOR": {other}})
			return metadata.AppendToOutgoingContext(ctx, "Ate-Target-Actor", other)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := startHarnessServer(t, echoagent.Harness{})
			har, closeHarness, err := placement.DefaultDial(api.Incarnation{
				Address:      hs.addr,
				CallMetadata: map[string]string{"ate-target-actor": mine},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeHarness() }()
			ctx := tc.caller(context.Background())

			if _, err := har.Describe(ctx); err != nil {
				t.Fatal(err)
			}
			start := &api.Start{ExecutionID: "e1", Inputs: []api.Message{*api.TextMessage("user", "hi")}}
			if err := har.Run(ctx, start, echoSink{}); err != nil {
				t.Fatal(err)
			}
			if got := hs.seenUnaryTargets(); len(got) != 1 || !slices.Equal(got[0], []string{mine}) {
				t.Errorf("Describe carried ate-target-actor=%v, want only the incarnation's [[%s]]", got, mine)
			}
			if got := hs.seenTargets(); !slices.Equal(got, []string{mine}) {
				t.Errorf("Connect stream carried ate-target-actor=%v, want only the incarnation's [%s]", got, mine)
			}
		})
	}
}

// A caller of the Sessions API cannot pick the actor either: an ate-target-actor header on its Exec
// call must not reach the harness call, which carries only the session's own actor.
func TestExecCallerHeaderDoesNotReachTheHarnessCall(t *testing.T) {
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": placement.New(&routedBackend{address: hs.addr, desc: statelessEcho}, echoagent.Model),
	})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, session.NewService(store, registry))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := client.Dial(lis.Addr().String(), client.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := metadata.AppendToOutgoingContext(t.Context(), "ate-target-actor", "space/victim")
	turn, err := c.Exec(ctx, client.ExecOptions{Inputs: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"space/" + turn.Session.GetMetadata().GetUid()}
	if got := hs.seenTargets(); !slices.Equal(got, want) {
		t.Fatalf("Connect stream carried ate-target-actor=%v, want only the session's %v", got, want)
	}
}

// echoSink answers model calls with the echo model and accepts everything else.
type echoSink struct{}

func (echoSink) Model(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	return echoagent.Model(ctx, req)
}
func (echoSink) Output(context.Context, string) error { return nil }
func (echoSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{}, errors.New("no tools")
}
func (echoSink) Report(context.Context, api.ToolResult) error { return nil }
func (echoSink) Usage(context.Context, api.Usage) error       { return nil }

// A turn parked on a slow model call leaves its Connect stream open and idle. Substrate drains an
// actor's in-flight requests before it checkpoints, so checkpointing under that stream stalls until
// the drain times out. Suspend does not interrupt the turn: it is refused with ErrSessionBusy and
// leaves the turn, its stream, compute and the journal alone. Once the turn has returned its stream
// is closed, so the retried Suspend never checkpoints under it.
func TestSuspendRefusesASessionWithAnOpenHarnessStream(t *testing.T) {
	testSuspendRefusesOpenStream(t, samePlacer)
}

// session.Service routes a turn by ExecRequest.harness but a Suspend or Fork by the session's
// recorded harness, so the turn and the checkpoint can run on different Placers of one Registry.
// Suspend must still be refused.
func TestSuspendRefusesAStreamOpenedByAnotherPlacerOfTheRegistry(t *testing.T) {
	testSuspendRefusesOpenStream(t, registryPair)
}

func testSuspendRefusesOpenStream(t *testing.T, pair placerPair) {
	t.Helper()
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	modelCalled := make(chan struct{})
	model := func(ctx context.Context, _ api.ModelRequest) (api.ModelResponse, error) {
		close(modelCalled)
		<-ctx.Done()
		return api.ModelResponse{}, context.Cause(ctx)
	}
	streamEnded := make(chan bool, 1)
	backend := &routedBackend{
		address: hs.addr,
		desc:    memDescriptor,
		onSnapshot: func() {
			select {
			case <-hs.finished:
				streamEnded <- true
			case <-time.After(5 * time.Second):
				streamEnded <- false
			}
		},
	}
	p, turn := pair(t, backend, model)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execErr := make(chan error, 1)
	go func() {
		_, err := turn.Exec(ctx, log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	select {
	case <-modelCalled:
	case err := <-execErr:
		t.Fatalf("turn returned before parking on the model call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the model call")
	}
	<-hs.started
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := p.Suspend(context.Background(), log, "s1"); !errors.Is(err, placement.ErrSessionBusy) {
		t.Fatalf("Suspend with a running turn returned %v, want ErrSessionBusy", err)
	}
	backend.mu.Lock()
	snapshots := backend.snapshots
	backend.mu.Unlock()
	if snapshots != 0 {
		t.Fatalf("refused Suspend took %d snapshot(s)", snapshots)
	}
	if after, err := log.Head(); err != nil || after != head {
		t.Fatalf("refused Suspend moved the journal head from %d to %d (err %v)", head, after, err)
	}
	select {
	case err := <-execErr:
		t.Fatalf("refused Suspend ended the running turn: %v", err)
	default:
	}
	if n := len(hs.finished); n != 0 {
		t.Fatalf("refused Suspend closed %d harness stream(s)", n)
	}

	cancel()
	select {
	case err := <-execErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled turn returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled turn never returned")
	}
	if _, err := p.Suspend(context.Background(), log, "s1"); !errors.Is(err, controller.ErrCheckpointIncompleteTurn) {
		t.Fatalf("Suspend after the interrupted turn returned %v, want ErrCheckpointIncompleteTurn", err)
	}
	backend.mu.Lock()
	snapshots = backend.snapshots
	backend.mu.Unlock()
	if snapshots != 0 {
		t.Fatalf("Suspend of the interrupted turn took %d snapshot(s)", snapshots)
	}
	select {
	case <-streamEnded:
		t.Fatal("refused Suspend reached the runtime checkpoint")
	default:
	}
}

// A stateful fork must leave the parent's running turn and stream alone. The turn may already have
// changed RAM before its next journal write, so cancelling it and snapshotting would let recovery
// apply that mutation twice.
func TestForkRefusesTheParentsOpenHarnessStreamBeforeTheCheckpoint(t *testing.T) {
	testForkRefusesOpenStream(t)
}

type blockedPrepareHarness struct {
	api.Harness
	started chan struct{}
	done    chan struct{}
}

func (h *blockedPrepareHarness) PrepareRun(ctx context.Context) (api.PreparedHarnessRun, error) {
	close(h.started)
	<-ctx.Done()
	close(h.done)
	return nil, ctx.Err()
}

func TestForkRefusesPreparedHarnessBeforeExecutionIsRecorded(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")
	h := &blockedPrepareHarness{Harness: echoagent.Harness{}, started: make(chan struct{}), done: make(chan struct{})}
	backend := &routedBackend{desc: memDescriptor}
	p := placement.New(backend, echoagent.Model, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return h, func() error { return nil }, nil
	}))

	execErr := make(chan error, 1)
	go func() {
		_, err := p.Exec(context.Background(), log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	select {
	case <-h.started:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never began Connect preparation")
	}

	if err := forkCheckpoint(p, log, "s1"); !errors.Is(err, placement.ErrSessionBusy) {
		t.Fatalf("Fork = %v, want ErrSessionBusy", err)
	}
	select {
	case <-h.done:
		t.Fatal("refused Fork cancelled Connect preparation")
	default:
	}
	select {
	case err := <-execErr:
		t.Fatalf("refused Fork ended Connect preparation: %v", err)
	default:
	}
	if kinds := journalKinds(t, log); len(kinds) != 0 {
		t.Fatalf("journal=%v, want no checkpoint record", kinds)
	}
}

// The refusal applies whichever Placer of the Registry runs the turn.
func TestForkRefusesAParentStreamOpenedByAnotherPlacerOfTheRegistry(t *testing.T) {
	testForkRefusesOpenStream(t, registryPair)
}

// placerPair returns the Placer a checkpoint runs on and the one a turn runs on.
type placerPair func(t *testing.T, b placement.Backend, model func(context.Context, api.ModelRequest) (api.ModelResponse, error)) (checkpoint, turn *placement.Placer)

// samePlacer runs the turn and the checkpoint on one Placer.
func samePlacer(_ *testing.T, b placement.Backend, model func(context.Context, api.ModelRequest) (api.ModelResponse, error)) (*placement.Placer, *placement.Placer) {
	p := placement.New(b, model)
	return p, p
}

// registryPair runs them on two Placers of one Registry: the session's default harness and an
// override, as ExecRequest.harness selects.
func registryPair(t *testing.T, b placement.Backend, model func(context.Context, api.ModelRequest) (api.ModelResponse, error)) (*placement.Placer, *placement.Placer) {
	t.Helper()
	def, other := placement.New(b, model), placement.New(b, model)
	if _, err := placement.NewRegistry("default", map[string]*placement.Placer{"default": def, "other": other}); err != nil {
		t.Fatal(err)
	}
	return def, other
}

func testForkRefusesOpenStream(t *testing.T, pair ...placerPair) {
	t.Helper()
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	// The model call never answers on its own: the stream stays open and idle until something ends it.
	modelCalled := make(chan struct{})
	model := func(ctx context.Context, _ api.ModelRequest) (api.ModelResponse, error) {
		close(modelCalled)
		<-ctx.Done()
		return api.ModelResponse{}, ctx.Err() // what a real model client returns: not the cause
	}
	backend := &routedBackend{address: hs.addr, desc: memDescriptor}
	mk := placerPair(samePlacer)
	if len(pair) > 0 {
		mk = pair[0]
	}
	p, turn := mk(t, backend, model)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execErr := make(chan error, 1)
	go func() {
		_, err := turn.Exec(ctx, log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	select {
	case <-modelCalled:
	case err := <-execErr:
		t.Fatalf("turn returned before parking on the model call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the model call")
	}
	<-hs.started

	before, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := forkCheckpoint(p, log, "s1"); !errors.Is(err, placement.ErrSessionBusy) {
		t.Fatalf("Fork = %v, want ErrSessionBusy", err)
	}
	select {
	case err := <-execErr:
		t.Fatalf("refused Fork ended the running turn: %v", err)
	default:
	}
	if n := len(hs.finished); n != 0 {
		t.Fatalf("refused Fork closed %d harness stream(s)", n)
	}
	backend.mu.Lock()
	snapshots := backend.snapshots
	backend.mu.Unlock()
	if snapshots != 0 {
		t.Fatalf("refused Fork took %d snapshot(s)", snapshots)
	}
	if after, err := log.Head(); err != nil || after != before {
		t.Fatalf("refused Fork moved the journal head from %d to %d (err %v)", before, after, err)
	}
	cancel()
	select {
	case err := <-execErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled turn returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled turn never returned")
	}
}

func journalKinds(t *testing.T, log eventlog.Store) []api.EventKind {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]api.EventKind, 0, len(recs))
	for _, r := range recs {
		kinds = append(kinds, r.Event.Kind)
	}
	return kinds
}

// memDescriptor is a REQUIRES_MEMORY_SNAPSHOT harness, the tier whose fork checkpoints the parent.
var memDescriptor = api.Descriptor{ID: "mem", Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}}

func suspendCheckpoint(p *placement.Placer, log *sqlitelog.Log, uid string) error {
	_, err := p.Suspend(context.Background(), log, uid)
	return err
}

func forkCheckpoint(p *placement.Placer, log *sqlitelog.Log, uid string) error {
	head, err := log.Head()
	if err != nil {
		return err
	}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		return err
	}
	defer store.Close()
	return p.Fork(context.Background(), log, uid, []placement.ForkChild{{UID: uid + "-child", Log: store.Session(uid + "-child")}}, head)
}

// A turn that starts while the session is being checkpointed must not open a harness stream: on
// substrate that stream would stall the very checkpoint that is running. Suspend refuses it through
// the session guard it holds; a stateful fork, which the guard does not cover, through its
// checkpoint mark.
func TestSuspendRefusesATurnStartedDuringTheCheckpoint(t *testing.T) {
	testCheckpointRefusesANewTurn(t, suspendCheckpoint, placement.ErrSessionBusy)
}

func TestForkRefusesATurnOnTheParentDuringTheCheckpoint(t *testing.T) {
	testCheckpointRefusesANewTurn(t, forkCheckpoint, placement.ErrCheckpointing)
}

// The refusal is per session, not per Placer: a turn routed to another harness of the same
// Registry must not open a stream under the checkpoint either.
func TestSuspendRefusesATurnOnAnotherPlacerOfTheRegistry(t *testing.T) {
	testCheckpointRefusesANewTurn(t, suspendCheckpoint, placement.ErrSessionBusy, registryPair)
}

func TestForkRefusesAParentTurnOnAnotherPlacerOfTheRegistry(t *testing.T) {
	testCheckpointRefusesANewTurn(t, forkCheckpoint, placement.ErrCheckpointing, registryPair)
}

func testCheckpointRefusesANewTurn(t *testing.T, checkpoint func(*placement.Placer, *sqlitelog.Log, string) error, want error, pair ...placerPair) {
	t.Helper()
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	var turn *placement.Placer
	var execErr error
	backend := &routedBackend{address: hs.addr, desc: memDescriptor}
	backend.onSnapshot = func() {
		_, execErr = turn.Exec(context.Background(), log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
	}
	mk := placerPair(samePlacer)
	if len(pair) > 0 {
		mk = pair[0]
	}
	var p *placement.Placer
	p, turn = mk(t, backend, echoagent.Model)
	if err := checkpoint(p, log, "s1"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if !errors.Is(execErr, want) {
		t.Fatalf("turn started during the checkpoint returned %v, want %v", execErr, want)
	}
	if n := len(hs.started); n != 0 {
		t.Fatalf("%d harness stream(s) opened while the session was being checkpointed", n)
	}
}

// NewRegistry replaces each Placer's session set. Replacing it under a running turn would orphan
// the turn's connection, so a checkpoint through the registry would neither end nor refuse it.
// NewRegistry must reject a Placer with a turn in progress and accept it again once the turn ends.
func TestNewRegistryRejectsAPlacerWithATurnInProgress(t *testing.T) {
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	modelCalled := make(chan struct{})
	model := func(ctx context.Context, _ api.ModelRequest) (api.ModelResponse, error) {
		close(modelCalled)
		<-ctx.Done()
		return api.ModelResponse{}, ctx.Err()
	}
	p := placement.New(&routedBackend{address: hs.addr, desc: memDescriptor}, model)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execErr := make(chan error, 1)
	go func() {
		_, err := p.Exec(ctx, log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	select {
	case <-modelCalled:
	case err := <-execErr:
		t.Fatalf("turn returned before parking on the model call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the model call")
	}

	if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p}); err == nil {
		t.Fatal("NewRegistry accepted a placer with a turn in progress")
	}
	cancel()
	select {
	case <-execErr:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled turn never returned")
	}
	if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p}); err != nil {
		t.Fatalf("NewRegistry rejected a placer whose only turn has ended: %v", err)
	}
}

// A stateful fork's checkpoint holds a mark in the set that refuses new turns, and a Suspend holds
// the session guard that refuses overlapping operations; NewRegistry must not drop either.
func TestNewRegistryRejectsAPlacerWithACheckpointInProgress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint func(*placement.Placer, *sqlitelog.Log, string) error
	}{
		{"suspend", suspendCheckpoint},
		{"fork", forkCheckpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			log := store.Session("s1")

			backend := &routedBackend{desc: memDescriptor}
			p := placement.New(backend, echoagent.Model)
			var regErr error
			backend.onSnapshot = func() {
				_, regErr = placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
			}
			if err := tc.checkpoint(p, log, "s1"); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if regErr == nil {
				t.Fatalf("NewRegistry accepted a placer with a %s in progress", tc.name)
			}
			if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p}); err != nil {
				t.Fatalf("NewRegistry rejected a placer whose %s has ended: %v", tc.name, err)
			}
		})
	}
}

// The other interleaving: the turn passed every early check and is about to register its connection
// when the fork's checkpoint begins. Registration must still be refused. (Suspend cannot begin while
// a turn is past its early checks: the turn holds the session guard.)
func TestForkRefusesAParentTurnRegisteringAsTheCheckpointBegins(t *testing.T) {
	testForkRefusesARegisteringTurn(t)
}

func testForkRefusesARegisteringTurn(t *testing.T) {
	t.Helper()
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	dialing, release := make(chan struct{}), make(chan struct{})
	dial := func(inc api.Incarnation) (api.Harness, func() error, error) {
		close(dialing)
		<-release
		return placement.DefaultDial(inc)
	}
	execErr := make(chan error, 1)
	backend := &routedBackend{address: hs.addr, desc: memDescriptor}
	p := placement.New(backend, echoagent.Model, placement.WithDialer(dial))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := p.Exec(ctx, log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	<-dialing
	if err := forkCheckpoint(p, log, "s1"); !errors.Is(err, placement.ErrSessionBusy) {
		t.Fatalf("Fork = %v, want ErrSessionBusy", err)
	}
	select {
	case err := <-execErr:
		t.Fatalf("refused Fork ended the registering turn: %v", err)
	default:
	}
	close(release)
	cancel()
	select {
	case <-execErr:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled registering turn did not return")
	}
}

// A stateful fork must refuse an in-flight turn before reaching a checkpoint that could capture
// unjournaled RAM. The turn remains usable; once it completes, a retry reaches the runtime and keeps
// the runtime's checkpoint failure as the result.
func TestForkRefusesTheParentTurnBeforeAFailedCheckpoint(t *testing.T) {
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	// The first model call parks until its turn is ended; later calls (the re-drive) answer.
	modelCalled := make(chan struct{})
	var calls int
	var callsMu sync.Mutex
	model := func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		callsMu.Lock()
		calls++
		first := calls == 1
		callsMu.Unlock()
		if !first {
			return echoagent.Model(ctx, req)
		}
		close(modelCalled)
		<-ctx.Done()
		return api.ModelResponse{}, ctx.Err() // what a real model client returns: not the cause
	}
	checkpointErr := errors.New("checkpoint failed")
	backend := &routedBackend{address: hs.addr, desc: memDescriptor, snapshotErr: checkpointErr}
	p := placement.New(backend, model)

	ctx, cancel := context.WithCancel(context.Background())
	execErr := make(chan error, 1)
	go func() {
		_, err := p.Exec(ctx, log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	<-modelCalled

	if err := forkCheckpoint(p, log, "s1"); !errors.Is(err, placement.ErrSessionBusy) {
		t.Fatalf("Fork = %v, want ErrSessionBusy", err)
	}
	select {
	case err := <-execErr:
		t.Fatalf("refused Fork ended the in-flight turn: %v", err)
	default:
	}
	backend.mu.Lock()
	snapshots := backend.snapshots
	backend.mu.Unlock()
	if snapshots != 0 {
		t.Fatalf("refused Fork took %d snapshot(s)", snapshots)
	}
	cancel()
	select {
	case err := <-execErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled turn returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled turn never returned")
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(context.Background(), log, "s1", []api.Message{*api.TextMessage("user", "again")}, head); err != nil {
		t.Fatalf("new turn after the interrupted one: %v", err)
	}
	if err := forkCheckpoint(p, log, "s1"); !errors.Is(err, checkpointErr) {
		t.Fatalf("Fork after the parent completed = %v, want %v", err, checkpointErr)
	}
	if err := forkCheckpoint(p, log, "s1"); err != nil {
		t.Fatalf("Fork retry after the runtime failure: %v", err)
	}
}

// With no turn in flight, a checkpoint has nothing to end and must not fail for it.
func TestSuspendWithNoOpenStreamStillCheckpoints(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := &routedBackend{address: "127.0.0.1:1", desc: statelessEcho}
	if _, err := placement.New(backend, echoagent.Model).Suspend(context.Background(), store.Session("idle"), "idle"); err != nil {
		t.Fatal(err)
	}
	if backend.snapshots != 1 {
		t.Fatalf("snapshots=%d want 1", backend.snapshots)
	}
}
