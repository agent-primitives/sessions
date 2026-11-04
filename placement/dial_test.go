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

// A host's own options cannot pick the sandbox: an interceptor it passes to DefaultDialWith, in any
// form, runs before the incarnation's, so every call still carries only the incarnation's
// ate-target-actor.
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
