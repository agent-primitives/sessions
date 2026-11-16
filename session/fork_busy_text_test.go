package session_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// A replay-fork (STATELESS_REPLAY) whose child's resume is refused busy is ABORTED with
// COMPUTE_BUSY. The service publishes the refusal's text, so the message names the fork child but
// not substrate's own text from the resume's error, which the handler's error still wraps.
func TestStatelessForkBusyOmitsSubstrateText(t *testing.T) {
	ctx := context.Background()
	const substrateText = "substrate-said: detail-7f3a"
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	desc, err := echoagent.Harness{}.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	desc.Capabilities.Resumability = api.ResumabilityStatelessReplay
	backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "echo"}, desc)
	dial := placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return echoagent.Harness{}, func() error { return nil }, nil
	})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placement.New(backend, echoagent.Model, dial)})
	if err != nil {
		t.Fatal(err)
	}
	c, handled := serveCapturing(t, store, r)
	sess := mustCreate(t, c)
	if err := execHi(c, sess); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	busy := fmt.Errorf("%w: space/child: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, substrateText)
	ctl.resumeErr = func(actor string) error {
		if actor == sess {
			return nil
		}
		return busy
	}

	_, err = c.Fork(ctx, &v1.ForkRequest{Session: sess, Count: 1})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("fork: want Aborted, got %v", err)
	}
	msg := status.Convert(err).Message()
	if strings.Contains(msg, substrateText) || !strings.Contains(msg, "create fork child space/") || !strings.Contains(msg, "is busy with another operation") {
		t.Errorf("error %q names substrate's text, or not the fork child and that it is busy", msg)
	}
	details := status.Convert(err).Details()
	if len(details) != 1 {
		t.Fatalf("details=%v, want one agentsessions/COMPUTE_BUSY ErrorInfo", details)
	}
	if info, ok := details[0].(*errdetails.ErrorInfo); !ok || info.Domain != "agentsessions" || info.Reason != "COMPUTE_BUSY" {
		t.Fatalf("details=%v, want agentsessions/COMPUTE_BUSY", details)
	}
	if handled := handled(v1.Sessions_Fork_FullMethodName); !errors.Is(handled, busy) || !errors.Is(handled, api.ErrComputeBusy) {
		t.Fatalf("the Fork handler's error %v does not retain the resume's error %v", handled, busy)
	}
}

// serveCapturing serves the Sessions API on store and registry as serveRegistry does, and also
// returns the error the last handler of a method (its full name, unary or streaming) returned,
// before gRPC turns it into a status.
func serveCapturing(t *testing.T, store *sqlitelog.Store, registry *placement.Registry) (v1.SessionsClient, func(method string) error) {
	t.Helper()
	var (
		mu      sync.Mutex
		handled = map[string]error{}
	)
	capture := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		mu.Lock()
		handled[info.FullMethod] = err
		mu.Unlock()
		return resp, err
	}
	captureStream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, ss)
		mu.Lock()
		handled[info.FullMethod] = err
		mu.Unlock()
		return err
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnaryInterceptor(capture), grpc.StreamInterceptor(captureStream))
	v1.RegisterSessionsServer(srv, session.NewService(store, registry))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return v1.NewSessionsClient(conn), func(method string) error {
		mu.Lock()
		defer mu.Unlock()
		return handled[method]
	}
}
