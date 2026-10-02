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
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"

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
	srv := grpc.NewServer(
		grpc.StatsHandler(flushingStatsHandler{otelgrpc.NewServerHandler()}),
		grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor(slog.Default())),
		grpc.ChainStreamInterceptor(flushTracesAfterTurn, observability.StreamServerInterceptor(slog.Default())),
	)
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(harness))

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

// flushingStatsHandler flushes again once otelgrpc has ended the RPC's own span, which happens after
// the handler (and flushTracesAfterTurn) returns. Without it the harness-side Connect span would be
// the one span of the turn still buffered at suspend time, orphaning harness.run in the trace.
type flushingStatsHandler struct{ stats.Handler }

func (h flushingStatsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	h.Handler.HandleRPC(ctx, s)
	if _, ok := s.(*stats.End); ok {
		flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = otelsetup.Flush(flushCtx)
	}
}

// flushTracesAfterTurn exports the turn's spans before Connect returns. The host waits for Connect
// to finish and may then suspend this actor, and spans still buffered in a suspended actor are only
// exported if that snapshot is ever restored. The flush is bounded so an unreachable collector
// cannot hold a turn open.
func flushTracesAfterTurn(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	err := handler(srv, ss)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = otelsetup.Flush(ctx)
	return err
}
