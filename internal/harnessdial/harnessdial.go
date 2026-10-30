// Package harnessdial is the one harnesswire client dial path. placement.DefaultDial uses it with
// plaintext credentials, and a backend build whose router needs TLS (integrations/substrate) uses
// it with its own, so both keep the same interceptors, tracing and routing metadata.
package harnessdial

import (
	"context"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/observability"
)

// Dial reaches a harnesswire server by address form: a unix-socket address (unix://…) or a TCP
// host:port, with creds as the transport security. The incarnation's CallMetadata is attached to
// every call.
func Dial(inc api.Incarnation, creds credentials.TransportCredentials) (api.Harness, func() error, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithChainUnaryInterceptor(observability.UnaryClientInterceptor),
		grpc.WithChainStreamInterceptor(observability.StreamClientInterceptor),
		observability.ClientTracing(),
	}
	if len(inc.CallMetadata) > 0 {
		md := metadata.New(inc.CallMetadata)
		opts = append(opts,
			grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, callOpts ...grpc.CallOption) error {
				return invoker(withCallMetadata(ctx, md), method, req, reply, cc, callOpts...)
			}),
			grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, callOpts ...grpc.CallOption) (grpc.ClientStream, error) {
				return streamer(withCallMetadata(ctx, md), desc, cc, method, callOpts...)
			}),
		)
	}
	if sock, ok := strings.CutPrefix(inc.Address, "unix://"); ok {
		return unixDial(sock, opts)
	}
	return tcpDial(inc.Address, opts)
}

// withCallMetadata attaches the incarnation's routing metadata to an outgoing call, replacing any
// value the caller set for the same keys: the incarnation, not the call site, decides which sandbox
// a call reaches.
func withCallMetadata(ctx context.Context, md metadata.MD) context.Context {
	out, _ := metadata.FromOutgoingContext(ctx)
	out = out.Copy()
	for k, v := range md {
		out[k] = v
	}
	return metadata.NewOutgoingContext(ctx, out)
}

// unixDial connects to a harnesswire server on a unix socket (runtime/local).
func unixDial(sock string, opts []grpc.DialOption) (api.Harness, func() error, error) {
	conn, err := grpc.NewClient(
		"passthrough:///agentlocal",
		append(opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}))...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("placement: dial unix %s: %w", sock, err)
	}
	return harnesswire.NewClientHarness(v1.NewHarnessClient(conn)), conn.Close, nil
}

// tcpDial connects to a harnesswire server at a TCP host:port. For substrate that is the
// atenet-router, which selects the actor from the call metadata and carries the gRPC stream on to
// the actor.
func tcpDial(address string, opts []grpc.DialOption) (api.Harness, func() error, error) {
	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("placement: dial %s: %w", address, err)
	}
	return harnesswire.NewClientHarness(v1.NewHarnessClient(conn)), conn.Close, nil
}
