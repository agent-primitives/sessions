// Package harnessnode is the harness server packaged to run INSIDE a compute sandbox (a substrate
// actor, or any runtime). It serves a selected harness over the harnesswire gRPC Harness.Connect
// stream on :80 — the port the atenet mesh routes to — so the external agentsessions controller
// drives it remotely, exactly as it drives the in-process runtime/local harness over a unix socket.
// A separate HTTP /readyz on :8081 lets ResumeActor block until the harness is live (ActorTemplate
// readyz probe).
//
// This is the substrate realization of the harness half of the step-6 dial path: the transport
// moves from a unix socket to atenet-router, which forwards to this port; the harnesswire server is
// unchanged.
//
// cmd/harnessnode serves the built-in harnesses. A build in another module serves its own harness
// through WithHarness, so this module links no provider-specific harness.
package harnessnode

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/chatagent"
	"github.com/agent-primitives/sessions/harness/counteragent"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harness/toolagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/observability/otelsetup"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Option configures Main.
type Option func(*options)

type options struct {
	kind       string
	newHarness HarnessFunc
	err        error
}

// Settings is the server configuration Main has read, for a harness that sizes itself against it.
type Settings struct {
	// MaxRecvMsgSize is the largest message, in bytes, the server accepts from the host
	// (HARNESS_MAX_RECV_MSG_SIZE). 0 means gRPC's default, 4 MiB.
	MaxRecvMsgSize int
}

// HarnessFunc builds the harness a WithHarness build serves. An error stops Main before it listens.
type HarnessFunc func(Settings) (api.Harness, error)

// WithHarness makes Main serve the harness newHarness builds instead of the built-in ones, for a
// build in another module that links a harness this module does not. kind names that harness:
// HARNESS_KIND must be unset or kind, and any other value, a built-in kind included, fails the
// start rather than serve a harness the operator did not ask for. An empty kind, a nil newHarness,
// or WithHarness given more than once also fails the start.
func WithHarness(kind string, newHarness HarnessFunc) Option {
	return func(o *options) {
		switch {
		case o.err != nil:
		case o.newHarness != nil:
			o.err = errors.New("WithHarness given more than once")
		case kind == "" || newHarness == nil:
			o.err = errors.New("WithHarness needs a kind and a HarnessFunc")
		default:
			o.kind, o.newHarness = kind, newHarness
		}
	}
}

func newOptions(opts []Option) (options, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o, o.err
}

// Main serves the selected harness until SIGINT or SIGTERM. A configuration error exits with
// status 1 before it listens.
func Main(opts ...Option) {
	o, err := newOptions(opts)
	if err != nil {
		log.Fatalf("harnessnode: %v", err)
	}
	maxRecv, err := maxRecvMsgSize()
	if err != nil {
		log.Fatalf("harnessnode: configure server: %v", err)
	}
	harness, kind, err := selectHarness(o, Settings{MaxRecvMsgSize: maxRecv})
	if err != nil {
		log.Fatalf("harnessnode: configure harness: %v", err)
	}
	grpcAddr := env("HARNESS_ADDR", ":"+substrate.HarnessPort) // harnesswire gRPC; atenet-router forwards here
	readyzAddr := env("HARNESS_READYZ", ":8081")               // HTTP readyz for the ActorTemplate wakeup probe

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Inside a sandbox the collector is whatever OTEL_EXPORTER_OTLP_ENDPOINT names (typically a
	// node-local agent). Unset, the harness still continues the host's trace context in-process
	// but exports nothing.
	shutdownTracing, err := otelsetup.Init(ctx, "harnessnode")
	if err != nil {
		log.Printf("harnessnode: tracing disabled: %v", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flushCtx)
	}()

	// harnesswire gRPC server (h2c: plain TCP, no TLS — the mesh terminates/originates HTTP/2).
	// HARNESS_ADDR=unix:///path serves on a unix socket instead, for a host that registers this
	// harness by address on the same machine (agentsessionsd -harness name=unix:///path).
	var lis net.Listener
	if sock, ok := strings.CutPrefix(grpcAddr, "unix://"); ok {
		ulis, lock, err := listenUnix(sock)
		if err != nil {
			log.Fatalf("harnessnode: listen %s: %v", grpcAddr, err)
		}
		// Held until the process exits, however it exits: the kernel drops the lock, and the next
		// harnessnode on this address can then reclaim the socket file.
		defer func() { _ = lock.Close() }()
		lis = ulis
	} else if lis, err = net.Listen("tcp", grpcAddr); err != nil {
		log.Fatalf("harnessnode: listen %s: %v", grpcAddr, err)
	}
	srv := newServer(harness, maxRecv)

	// readyz on a side port so the actor is reported live once the gRPC server is accepting.
	readyz := &http.Server{
		Addr:    readyzAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
	}
	go func() {
		if err := readyz.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("harnessnode: readyz: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
		_ = readyz.Close()
	}()

	recvLimit := "gRPC default"
	if maxRecv > 0 {
		recvLimit = strconv.Itoa(maxRecv) + " bytes"
	}
	log.Printf("harnessnode: %q harness serving harnesswire on %s (max receive message size: %s), "+
		"readyz on %s", kind, grpcAddr, recvLimit, readyzAddr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("harnessnode: serve: %v", err)
	}
}

// selectHarness picks the harness this node serves and the kind it logs. A WithHarness build serves
// its own harness. Otherwise HARNESS_KIND picks a built-in one (default "echo"; an unknown kind
// also serves echo). One image serves both conformance tiers: "echo", "chat" and "tool" are
// STATELESS_REPLAY; "counter" requires a memory snapshot. "tool" offers the model the
// approval-gated publish tool. The model ID for chat and tool is supplied by HARNESS_MODEL;
// provider configuration stays on the host.
func selectHarness(o options, s Settings) (api.Harness, string, error) {
	kind := env("HARNESS_KIND", "echo")
	if o.newHarness != nil {
		if v := os.Getenv("HARNESS_KIND"); v != "" && v != o.kind {
			return nil, "", fmt.Errorf("HARNESS_KIND=%q: this build serves only %q", v, o.kind)
		}
		h, err := o.newHarness(s)
		if err != nil {
			return nil, "", err
		}
		if h == nil {
			return nil, "", fmt.Errorf("%s harness: HarnessFunc returned no harness", o.kind)
		}
		return h, o.kind, nil
	}
	switch kind {
	case "chat", "tool":
		model := os.Getenv("HARNESS_MODEL")
		if model == "" {
			return nil, "", fmt.Errorf("HARNESS_MODEL is required when HARNESS_KIND=%s", kind)
		}
		if kind == "tool" {
			return toolagent.Harness{Model: model, Tools: []api.ToolDefinition{toolagent.PublishTool()}}, kind, nil
		}
		return chatagent.Harness{Model: model}, kind, nil
	case "counter":
		return &counteragent.Harness{}, kind, nil
	default:
		return echoagent.Harness{}, kind, nil
	}
}

// maxRecvMsgSize reads HARNESS_MAX_RECV_MSG_SIZE: the largest message, in bytes, the harnesswire
// server accepts from the host. It bounds every host-to-harness frame on Connect, Start included,
// and Start carries the session's whole prior journal (History). Unset or empty returns 0, which
// keeps gRPC's default (4 MiB). Any other value must be a positive decimal integer.
func maxRecvMsgSize() (int, error) {
	v := os.Getenv("HARNESS_MAX_RECV_MSG_SIZE")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("HARNESS_MAX_RECV_MSG_SIZE=%q: want a positive number of bytes", v)
	}
	return n, nil
}

// newServer builds the harnesswire gRPC server. otelgrpc traces everything but Connect; Connect gets
// its span from traceConnect, which can end and export it before the turn completes. A positive
// maxRecvMsgSize replaces gRPC's default receive limit; 0 keeps it.
func newServer(harness api.Harness, maxRecvMsgSize int) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(observability.ThirdPartyTracerProvider()),
			otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool { return info.FullMethodName != v1.Harness_Connect_FullMethodName }),
		)),
		grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor(slog.Default())),
		grpc.ChainStreamInterceptor(traceConnect, observability.StreamServerInterceptor(slog.Default())),
	}
	if maxRecvMsgSize > 0 {
		opts = append(opts, grpc.MaxRecvMsgSize(maxRecvMsgSize))
	}
	srv := grpc.NewServer(opts...)
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(harness))
	return srv
}

// traceConnect is Connect's server span, ended and exported before the handler returns. The host
// waits for Connect's final status and may then suspend this actor, and spans still buffered in a
// suspended actor are only exported if that snapshot is ever restored. otelgrpc cannot provide
// that ordering: grpc-go writes the final status before it reports the RPC's end to stats handlers,
// so otelgrpc's span ends, and could only be flushed, after the host has already moved on.
//
// The flush is bounded so an unreachable collector cannot hold a turn open.
func traceConnect(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if info.FullMethod != v1.Harness_Connect_FullMethodName {
		return handler(srv, ss)
	}
	ctx := ss.Context()
	md, _ := metadata.FromIncomingContext(ctx)
	ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(md))
	name := strings.TrimPrefix(info.FullMethod, "/")
	service, method, _ := strings.Cut(name, "/")
	ctx, span := observability.Tracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.service", service),
			attribute.String("rpc.method", method),
		))

	err := handler(srv, &contextStream{ServerStream: ss, ctx: ctx})

	code := connectCode(err)
	span.SetAttributes(attribute.Int64("rpc.grpc.status_code", int64(code)))
	if serverError(code) {
		// The code, never the message: a status message can quote payload data.
		span.SetStatus(otelcodes.Error, code.String())
	}
	span.End()
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = otelsetup.Flush(flushCtx)
	return err
}

// connectCode is the code grpc-go sends for err: a host that ends a turn early makes the handler
// return the stream context's error, which grpc-go writes as DeadlineExceeded or Canceled.
func connectCode(err error) codes.Code {
	st, ok := status.FromError(err)
	if !ok {
		st = status.FromContextError(err)
	}
	return st.Code()
}

// serverError reports whether code marks a server span as an error, following otelgrpc's server
// convention: codes the caller caused, such as Canceled or InvalidArgument, leave the status unset.
func serverError(code codes.Code) bool {
	switch code {
	case codes.Unknown, codes.DeadlineExceeded, codes.Unimplemented, codes.Internal,
		codes.Unavailable, codes.DataLoss:
		return true
	}
	return false
}

type contextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *contextStream) Context() context.Context { return s.ctx }

// metadataCarrier reads W3C trace context from incoming gRPC metadata.
type metadataCarrier metadata.MD

func (c metadataCarrier) Get(key string) string {
	if v := metadata.MD(c).Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (c metadataCarrier) Set(key, value string) { metadata.MD(c).Set(key, value) }

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
