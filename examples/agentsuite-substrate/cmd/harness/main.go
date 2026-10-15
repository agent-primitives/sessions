// Command harness serves one AgentSuite agent over harnesswire on :80, the port the atenet-router
// forwards to, with an HTTP /readyz on :8081 for the ActorTemplate wakeup probe. The agent manifest
// and instructions are baked into the image under AGENT_DIR (default /agent).
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
	"github.com/aramase/agentsessions/harnesswire"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	h, err := agent.Load(env("AGENT_DIR", "/agent"))
	if err != nil {
		log.Fatalf("harness: load agent: %v", err)
	}
	h = h.WithModel(os.Getenv(agent.ModelEnv))
	tier := "stateless replay"
	switch m := os.Getenv(agent.MemoryEnv); m {
	case "":
	case agent.MemoryRAM:
		if len(h.Invokes) > 0 {
			// Delegations are host-mediated tool calls, and the approval gate is stateless-replay only.
			log.Fatalf("harness: agent %q has invokes edges; it runs in the stateless tier only", h.ID)
		}
		h, tier = h.WithMemory(), "memory (conversation in RAM)"
	default:
		log.Fatalf("harness: %s=%q: the only value is %q", agent.MemoryEnv, m, agent.MemoryRAM)
	}
	if v := os.Getenv(agent.ApprovalEnv); v != "" {
		if h, err = h.WithApproval(strings.Split(v, ",")); err != nil {
			log.Fatalf("harness: %s: %v", agent.ApprovalEnv, err)
		}
	}
	addr, readyzAddr := env("HARNESS_ADDR", ":80"), env("HARNESS_READYZ", ":8081")
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("harness: listen %s: %v", addr, err)
	}
	srv := grpc.NewServer()
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(h))
	readyz := &http.Server{
		Addr:    readyzAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
	}
	go func() {
		if err := readyz.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("harness: readyz: %v", err)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
		_ = readyz.Close()
	}()
	log.Printf("harness: agent %q (model %q, tier %s, invokes %v, approval %v) serving harnesswire on %s, readyz on %s", h.ID, h.Model, tier, h.Invokes, h.Approval, addr, readyzAddr)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("harness: serve: %v", err)
	}
}
