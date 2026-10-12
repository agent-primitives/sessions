// Command harnessnode is the harness packaged to run INSIDE a compute sandbox (a substrate actor, or
// any runtime). It serves a selected harness over the harnesswire gRPC Harness.Connect stream on :80 —
// the port the atenet mesh routes to — so the external agentsessions controller drives it remotely,
// exactly as it drives the in-process runtime/local harness over a unix socket. A separate HTTP
// /readyz on :8081 lets ResumeActor block until the harness is live (ActorTemplate readyz probe).
//
// This is the substrate realization of the harness half of the step-6 dial path: the transport moves
// from a unix socket to atenet-router, which forwards to this port; the harnesswire server is unchanged.
package main

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
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
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

func main() {
	harness, err := selectHarness()
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
	srv := newServer(harness)

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

	log.Printf("harnessnode: %q harness serving harnesswire on %s, readyz on %s", env("HARNESS_KIND", "echo"), grpcAddr, readyzAddr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("harnessnode: serve: %v", err)
	}
}

// selectHarness picks the harness this node serves, by HARNESS_KIND (default "echo"). One image
// serves both conformance tiers: "echo", "chat" and "tool" are STATELESS_REPLAY; "counter" requires
// a memory snapshot. "tool" offers the model the approval-gated publish tool. The model ID for chat
// and tool is supplied by HARNESS_MODEL; provider configuration stays on the host.
func selectHarness() (api.Harness, error) {
	switch kind := env("HARNESS_KIND", "echo"); kind {
	case "chat", "tool":
		model := os.Getenv("HARNESS_MODEL")
		if model == "" {
			return nil, fmt.Errorf("HARNESS_MODEL is required when HARNESS_KIND=%s", kind)
		}
		if kind == "tool" {
			return toolagent.Harness{Model: model, Tools: []api.ToolDefinition{toolagent.PublishTool()}}, nil
		}
		return chatagent.Harness{Model: model}, nil
	case "counter":
		return &counteragent.Harness{}, nil
	default:
		return echoagent.Harness{}, nil
	}
}

// newServer builds the harnesswire gRPC server. otelgrpc traces everything but Connect; Connect gets
// its span from traceConnect, which can end and export it before the turn completes.
func newServer(harness api.Harness) *grpc.Server {
	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(observability.ThirdPartyTracerProvider()),
			otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool { return info.FullMethodName != v1.Harness_Connect_FullMethodName }),
		)),
		grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor(slog.Default())),
		grpc.ChainStreamInterceptor(traceConnect, observability.StreamServerInterceptor(slog.Default())),
	)
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

	code := status.Code(err)
	span.SetAttributes(attribute.Int64("rpc.grpc.status_code", int64(code)))
	if err != nil {
		// The code, never the message: a status message can quote payload data.
		span.SetStatus(otelcodes.Error, code.String())
	}
	span.End()
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = otelsetup.Flush(flushCtx)
	return err
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
