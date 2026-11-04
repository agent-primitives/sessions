package placement_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/placement"
)

// largeOutputHarness emits one OUTPUT delta of size bytes, a harness-to-host frame over gRPC's
// default 4 MiB receive limit when size is large enough.
type largeOutputHarness struct{ size int }

func (largeOutputHarness) Describe(context.Context) (api.Descriptor, error) {
	return statelessEcho, nil
}

func (h largeOutputHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	return sink.Output(ctx, strings.Repeat("x", h.size))
}

// outputSink records the OUTPUT deltas it receives.
type outputSink struct {
	echoSink
	outputs []int
}

func (s *outputSink) Output(_ context.Context, delta string) error {
	s.outputs = append(s.outputs, len(delta))
	return nil
}

// The host's receive limit bounds every harness-to-host frame. DefaultDial keeps gRPC's 4 MiB
// default; DefaultDialWith lets a host raise it without reimplementing the dial.
func TestDefaultDialWithRaisesTheReceiveLimit(t *testing.T) {
	const size = 5 << 20
	for _, tt := range []struct {
		name string
		dial placement.Dialer
		want codes.Code
	}{
		{"DefaultDial refuses a 5 MiB frame", placement.DefaultDial, codes.ResourceExhausted},
		{"DefaultDialWith a 16 MiB limit accepts it", placement.DefaultDialWith(
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16 << 20))), codes.OK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hs := startHarnessServer(t, largeOutputHarness{size: size})
			har, closeHarness, err := tt.dial(api.Incarnation{
				Address:      hs.addr,
				CallMetadata: map[string]string{"ate-target-actor": "space/s1"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeHarness() }()
			sink := &outputSink{}
			err = har.Run(t.Context(), &api.Start{ExecutionID: "e1"}, sink)
			if got := status.Code(err); got != tt.want {
				t.Fatalf("Run = %v (code %s), want %s", err, got, tt.want)
			}
			if tt.want == codes.OK && !slices.Equal(sink.outputs, []int{size}) {
				t.Fatalf("host received outputs of %v bytes, want [%d]", sink.outputs, size)
			}
			if got := hs.seenTargets(); !slices.Equal(got, []string{"space/s1"}) {
				t.Fatalf("Connect stream carried ate-target-actor=%v, want [space/s1]", got)
			}
		})
	}
}

// A host's interceptor cannot pick the sandbox through the outgoing context: one it passes to
// DefaultDialWith, in any form, runs before the incarnation's, so every call still carries only the
// incarnation's ate-target-actor.
func TestDefaultDialWithCallMetadataOverridesHostInterceptors(t *testing.T) {
	const mine, other = "space/mine", "space/someone-else"
	unary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "ate-target-actor", other)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	stream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
		streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, "ate-target-actor", other)
		return streamer(ctx, desc, cc, method, opts...)
	}
	for _, tt := range []struct {
		name string
		opts []grpc.DialOption
	}{
		{"chained interceptors", []grpc.DialOption{
			grpc.WithChainUnaryInterceptor(unary), grpc.WithChainStreamInterceptor(stream),
		}},
		{"outermost interceptors", []grpc.DialOption{
			grpc.WithUnaryInterceptor(unary), grpc.WithStreamInterceptor(stream),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hs := startHarnessServer(t, echoagent.Harness{})
			har, closeHarness, err := placement.DefaultDialWith(tt.opts...)(api.Incarnation{
				Address:      hs.addr,
				CallMetadata: map[string]string{"ate-target-actor": mine},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeHarness() }()
			if _, err := har.Describe(t.Context()); err != nil {
				t.Fatal(err)
			}
			start := &api.Start{ExecutionID: "e1", Inputs: []api.Message{*api.TextMessage("user", "hi")}}
			if err := har.Run(t.Context(), start, echoSink{}); err != nil {
				t.Fatal(err)
			}
			if got := hs.seenUnaryTargets(); len(got) != 1 || !slices.Equal(got[0], []string{mine}) {
				t.Errorf("Describe carried ate-target-actor=%v, want only %s", got, mine)
			}
			if got := hs.seenTargets(); !slices.Equal(got, []string{mine}) {
				t.Errorf("Connect stream carried ate-target-actor=%v, want only %s", got, mine)
			}
		})
	}
}

// routingCreds is per-RPC credentials that set ate-target-actor, which DefaultDialWith's godoc says
// credentials must not do.
type routingCreds struct{ target string }

func (c routingCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"ate-target-actor": c.target}, nil
}

func (routingCreds) RequireTransportSecurity() bool { return false }

// routingStats is a stats handler whose TagRPC sets ate-target-actor in the outgoing context, which
// DefaultDialWith's godoc says a stats handler must not do. replace sets the key instead of
// appending a value.
type routingStats struct {
	target  string
	replace bool
}

func (s routingStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	if !s.replace {
		return metadata.AppendToOutgoingContext(ctx, "ate-target-actor", s.target)
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set("ate-target-actor", s.target)
	return metadata.NewOutgoingContext(ctx, md)
}

func (routingStats) HandleRPC(context.Context, stats.RPCStats) {}

func (routingStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (routingStats) HandleConn(context.Context, stats.ConnStats) {}

// The documented limit of the replacement: gRPC adds per-RPC credentials metadata in the transport
// and runs a stats handler's TagRPC after the incarnation's interceptor has rewritten the outgoing
// context, so a routing key either sets is not replaced. Credentials put a second ate-target-actor
// value on the wire; a TagRPC can add one or replace the incarnation's outright.
func TestDefaultDialWithDoesNotReplaceMetadataAddedAfterInterceptors(t *testing.T) {
	const mine, other = "space/mine", "space/someone-else"
	creds := routingCreds{target: other}
	// A host interceptor that hands the credentials to the invoker as a call option.
	unary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(ctx, method, req, reply, cc, append(opts, grpc.PerRPCCredentials(creds))...)
	}
	stream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
		streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(ctx, desc, cc, method, append(opts, grpc.PerRPCCredentials(creds))...)
	}
	for _, tt := range []struct {
		name string
		opts []grpc.DialOption
		want []string
	}{
		{"credentials dial option", []grpc.DialOption{
			grpc.WithPerRPCCredentials(creds),
		}, []string{mine, other}},
		{"credentials call option", []grpc.DialOption{
			grpc.WithDefaultCallOptions(grpc.PerRPCCredentials(creds)),
		}, []string{mine, other}},
		{"credentials from an interceptor", []grpc.DialOption{
			grpc.WithChainUnaryInterceptor(unary), grpc.WithChainStreamInterceptor(stream),
		}, []string{mine, other}},
		{"stats handler appends", []grpc.DialOption{
			grpc.WithStatsHandler(routingStats{target: other}),
		}, []string{mine, other}},
		{"stats handler replaces", []grpc.DialOption{
			grpc.WithStatsHandler(routingStats{target: other, replace: true}),
		}, []string{other}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hs := startHarnessServer(t, echoagent.Harness{})
			har, closeHarness, err := placement.DefaultDialWith(tt.opts...)(api.Incarnation{
				Address:      hs.addr,
				CallMetadata: map[string]string{"ate-target-actor": mine},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closeHarness() }()
			if _, err := har.Describe(t.Context()); err != nil {
				t.Fatal(err)
			}
			start := &api.Start{ExecutionID: "e1", Inputs: []api.Message{*api.TextMessage("user", "hi")}}
			if err := har.Run(t.Context(), start, echoSink{}); err != nil {
				t.Fatal(err)
			}
			// The values' order is gRPC's header order, not part of the documented limit.
			unary := hs.seenUnaryTargets()
			if len(unary) != 1 || !slices.Equal(slices.Sorted(slices.Values(unary[0])), tt.want) {
				t.Errorf("Describe carried ate-target-actor=%v, want %v", unary, tt.want)
			}
			if got := slices.Sorted(slices.Values(hs.seenTargets())); !slices.Equal(got, tt.want) {
				t.Errorf("Connect stream carried ate-target-actor=%v, want %v", got, tt.want)
			}
		})
	}
}

// A unix:// incarnation names its socket; a context dialer the host passes cannot redirect it.
func TestDefaultDialWithKeepsTheUnixSocket(t *testing.T) {
	// A short directory: a unix socket path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dial")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(echoagent.Harness{}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	redirected := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return nil, errors.New("redirected by the host's dialer")
	})
	dial := placement.DefaultDialWith(redirected)
	har, closeHarness, err := dial(api.Incarnation{Address: "unix://" + sock})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeHarness() }()
	if _, err := har.Describe(t.Context()); err != nil {
		t.Fatalf("Describe over the incarnation's socket: %v", err)
	}
}
