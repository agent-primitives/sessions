package session_test

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// These tests run a harness registered at runtime (HarnessRegistry and a PlacerFactory) on a
// backend shaped like substrate behind atenet-router: every incarnation has the same address, and
// CallMetadata names the session's sandbox. The factory builds its Placer with placement.New and
// no dialer, as a host would, so the Placer uses placement.DefaultDial.

const routerTargetKey = "ate-target-actor"

// routedCall is one call the harness server received: its method and the target metadata it
// arrived with.
type routedCall struct {
	method  string
	targets []string
}

// routedHarnessServer serves a harness over harnesswire on loopback TCP and records every call.
type routedHarnessServer struct {
	addr string

	mu    sync.Mutex
	calls []routedCall
}

func startRoutedHarnessServer(t *testing.T, h api.Harness) *routedHarnessServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &routedHarnessServer{addr: lis.Addr().String()}
	record := func(ctx context.Context, method string) {
		md, _ := metadata.FromIncomingContext(ctx)
		hs.mu.Lock()
		hs.calls = append(hs.calls, routedCall{method: method, targets: md.Get(routerTargetKey)})
		hs.mu.Unlock()
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			record(ctx, info.FullMethod)
			return handler(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			record(ss.Context(), info.FullMethod)
			return handler(srv, ss)
		}))
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(h))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return hs
}

func (hs *routedHarnessServer) seen() []routedCall {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return slices.Clone(hs.calls)
}

// requireRoutedTo fails unless every call reached the server with exactly the given target, and
// returns the methods called, in order.
func (hs *routedHarnessServer) requireRoutedTo(t *testing.T, target string) []string {
	t.Helper()
	var methods []string
	for _, c := range hs.seen() {
		if !slices.Equal(c.targets, []string{target}) {
			t.Errorf("%s arrived with %s %q, want only %q", c.method, routerTargetKey, c.targets, target)
		}
		methods = append(methods, c.method)
	}
	return methods
}

// versionedEcho is the echo harness advertising a chosen version.
type versionedEcho struct {
	echoagent.Harness
	version string
}

func (h versionedEcho) Describe(ctx context.Context) (api.Descriptor, error) {
	desc, err := h.Harness.Describe(ctx)
	desc.Version = h.version
	return desc, err
}

// routerBackend is a stateless backend whose incarnations sit behind one shared address, with
// CallMetadata naming the session's sandbox. Describe returns a fixed declaration, as substrate's
// does. live makes it a placement.LiveDescriber, so each turn also checks the harness that answers
// on its own connection.
type routerBackend struct {
	address string
	desc    api.Descriptor
	live    bool
}

func (b *routerBackend) Describe(context.Context) (api.Descriptor, error) { return b.desc, nil }
func (b *routerBackend) DescribesLiveHarness() bool                       { return b.live }

func (b *routerBackend) Create(_ context.Context, s *api.SessionSpec) (api.Incarnation, error) {
	return api.Incarnation{
		ID:           s.SessionUID,
		Address:      b.address,
		CallMetadata: map[string]string{routerTargetKey: "space/" + s.SessionUID},
		Runtime:      "routed",
	}, nil
}

func (b *routerBackend) Snapshot(_ context.Context, in api.Incarnation, _ api.SnapshotKind) (api.SnapshotRef, error) {
	return api.SnapshotRef{Local: in.ID}, nil
}

func (b *routerBackend) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	return b.Create(ctx, &api.SessionSpec{SessionUID: ref.Local})
}

func (b *routerBackend) Fork(ctx context.Context, _ api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	return b.Create(ctx, &api.SessionSpec{SessionUID: opts.ChildSessionUID})
}

func (b *routerBackend) Stop(context.Context, api.Incarnation) error { return nil }

func (b *routerBackend) Status(context.Context, api.Incarnation) (api.ComputeState, error) {
	return api.ComputeLive, nil
}

func (b *routerBackend) Capabilities() api.RuntimeCapabilities { return api.RuntimeCapabilities{} }

// routedRegistryHost serves Sessions over a static echo harness plus every harness registered
// through HarnessRegistry, each on backend.
func routedRegistryHost(t *testing.T, backend *routerBackend) (*sqlitelog.Store, *session.HarnessRegistry, v1.SessionsClient) {
	t.Helper()
	store := openStore(t, ":memory:")
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": newEchoPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := session.NewHarnessRegistry(store, registry, func(_ string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
		return placement.New(backend, echoagent.Model, placement.WithDescriptorID(spec.GetDescriptorId())), nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return store, reg, serveRegistry(t, store, registry)
}

func newEchoPlacer(t *testing.T) *placement.Placer {
	t.Helper()
	return placement.New(&routerBackend{desc: statelessEchoDescriptor}, echoagent.Model)
}

var statelessEchoDescriptor = api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}

func registerRouted(t *testing.T, reg *session.HarnessRegistry, name, descriptorID string) {
	t.Helper()
	spec := remoteSpec("router.invalid:80")
	spec.DescriptorId = descriptorID
	if _, err := reg.RegisterHarness(t.Context(), &v1.RegisterHarnessRequest{Name: name, Spec: spec}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func executionStarts(t *testing.T, log eventlog.Store) []*api.ExecutionStart {
	t.Helper()
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var out []*api.ExecutionStart
	for _, r := range recs {
		if r.Event.Kind == api.EventExecutionStart {
			out = append(out, r.Event.ExecutionStart)
		}
	}
	return out
}

// A turn on a registered harness reaches its own sandbox: the controller's Describe and the
// Connect stream both carry the session's target, and the turn records the registry name and the
// version the routed harness advertised.
func TestRegisteredHarnessTurnCarriesCallMetadata(t *testing.T) {
	hs := startRoutedHarnessServer(t, versionedEcho{version: "7"})
	store, reg, c := routedRegistryHost(t, &routerBackend{address: hs.addr, desc: statelessEchoDescriptor})
	registerRouted(t, reg, "routed", "echo")

	sess, err := createOn(t.Context(), c, "routed")
	if err != nil {
		t.Fatal(err)
	}
	if err := execOn(t.Context(), c, sess, ""); err != nil {
		t.Fatalf("exec on the registered harness: %v", err)
	}
	methods := hs.requireRoutedTo(t, "space/"+sess)
	if want := []string{v1.Harness_Describe_FullMethodName, v1.Harness_Connect_FullMethodName}; !slices.Equal(methods, want) {
		t.Fatalf("harness calls = %q, want %q", methods, want)
	}
	starts := executionStarts(t, store.Session(sess))
	if len(starts) != 1 || starts[0].Harness != "routed" || starts[0].HarnessVersion != "7" {
		t.Fatalf("EXECUTION_START = %+v, want harness \"routed\" at version \"7\"", starts)
	}
}

// descriptor_id holds on the router path. With a fixed declaration (substrate's shape) a mismatch is
// refused at admission, before the backend or the harness is called. With a live describer it is
// refused on the turn's own routed connection, before the Connect stream opens. Either way nothing
// is journaled.
func TestRegisteredHarnessDescriptorMismatchOnTheRouterPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
		// declared is what the backend's own Describe returns.
		declared    string
		wantMethods []string
	}{
		{name: "declared", declared: "other"},
		{name: "on the turn's connection", live: true, declared: "chat", wantMethods: []string{v1.Harness_Describe_FullMethodName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := startRoutedHarnessServer(t, versionedEcho{version: "1"}) // reports id "echo"
			desc := statelessEchoDescriptor
			desc.ID = tc.declared
			store, reg, c := routedRegistryHost(t, &routerBackend{address: hs.addr, desc: desc, live: tc.live})
			registerRouted(t, reg, "routed", "chat")

			sess, err := createOn(t.Context(), c, "routed")
			if err != nil {
				t.Fatal(err)
			}
			if err := execOn(t.Context(), c, sess, ""); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("exec on a mismatched harness: want FailedPrecondition, got %v", err)
			}
			if methods := hs.requireRoutedTo(t, "space/"+sess); !slices.Equal(methods, tc.wantMethods) {
				t.Fatalf("harness calls = %q, want %q", methods, tc.wantMethods)
			}
			if head, err := store.Session(sess).Head(); err != nil || head != 0 {
				t.Fatalf("journal head = %d, %v; want nothing journaled", head, err)
			}
		})
	}
}

// Resume of an interrupted turn on a registered harness routes to the recorded registry name and
// reaches the session's sandbox; the controller checks the recorded version against the version the
// routed harness reports, and refuses a different one before the Connect stream opens.
func TestResumeOfARegisteredHarnessOnTheRouterPathChecksTheRecordedVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		served      string
		wantCode    codes.Code
		wantMethods []string
	}{
		{name: "same version", served: "1", wantCode: codes.OK,
			wantMethods: []string{v1.Harness_Describe_FullMethodName, v1.Harness_Connect_FullMethodName}},
		{name: "other version", served: "2", wantCode: codes.FailedPrecondition,
			wantMethods: []string{v1.Harness_Describe_FullMethodName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := startRoutedHarnessServer(t, versionedEcho{version: tc.served})
			store, reg, c := routedRegistryHost(t, &routerBackend{address: hs.addr, desc: statelessEchoDescriptor})
			registerRouted(t, reg, "routed", "echo")

			const uid = "routed-resume"
			if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: "routed"}); err != nil {
				t.Fatal(err)
			}
			log := store.Session(uid)
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			for i, ev := range []api.Event{
				{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1), Harness: "routed", HarnessVersion: "1"}},
				{Kind: api.EventInput, Message: api.TextMessage("user", "hi")},
			} {
				ev.ExecutionID = "interrupted"
				if _, err := log.Append(int64(i), fence, ev); err != nil {
					t.Fatal(err)
				}
			}

			_, err = c.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("Resume: want %v, got %v", tc.wantCode, err)
			}
			if methods := hs.requireRoutedTo(t, "space/"+uid); !slices.Equal(methods, tc.wantMethods) {
				t.Fatalf("harness calls = %q, want %q", methods, tc.wantMethods)
			}
			if err := log.Verify(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
