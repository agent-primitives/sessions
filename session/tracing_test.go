package session_test

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/observability"
	"github.com/aramase/agentsessions/runtime/local"
	"github.com/aramase/agentsessions/session"
	"github.com/aramase/agentsessions/sqlitelog"
)

// TestSessionOperationsChainAcrossTraces checks the stitching contract: every mutating operation
// is its own trace, linked to the session's previous operation, so a session can be followed
// across idle gaps and suspensions without holding a trace open. It also checks that the harness's
// own spans join the turn's trace through the harness wire.
func TestSessionOperationsChainAcrossTraces(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	c := newClientWith(t, local.New(echoagent.Harness{}))
	ctx := context.Background()
	s, err := c.CreateSession(ctx, &v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sess := s.GetMetadata().GetUid()
	execOutputs(t, c, sess, "one", 0)
	if _, err := c.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx, &v1.ResumeRequest{Session: sess}); err != nil {
		t.Fatal(err)
	}
	cur, err := c.GetSession(ctx, &v1.GetSessionRequest{Uid: sess})
	if err != nil {
		t.Fatal(err)
	}
	execOutputs(t, c, sess, "two", cur.GetLastSeq())

	var ops []sdktrace.ReadOnlySpan
	harnessTraces := map[trace.TraceID]bool{}
	for _, sp := range rec.Ended() {
		switch sp.Name() {
		case "session.exec", "session.suspend", "session.resume":
			ops = append(ops, sp)
		case "harness.run":
			harnessTraces[sp.SpanContext().TraceID()] = true
		case "agentsessions.v1.Harness/Connect":
			// The host waits for the harness to close the stream after END rather than cancelling
			// it, so a completed turn is not reported as a failed RPC.
			if sp.Status().Code == otelcodes.Error {
				t.Fatalf("%s (%s) ended with error status %q", sp.Name(), sp.SpanKind(), sp.Status().Description)
			}
		}
	}
	// Spans end innermost-first, but the session operations are sequential, so end order is
	// operation order.
	want := []string{"session.exec", "session.suspend", "session.resume", "session.exec"}
	if len(ops) != len(want) {
		t.Fatalf("got %d session operation spans, want %d", len(ops), len(want))
	}
	for i, sp := range ops {
		if sp.Name() != want[i] {
			t.Fatalf("op %d = %s, want %s", i, sp.Name(), want[i])
		}
		links := sp.Links()
		if i == 0 {
			if len(links) != 0 {
				t.Fatalf("first operation has %d links, want 0", len(links))
			}
			continue
		}
		if len(links) != 1 || links[0].SpanContext.SpanID() != ops[i-1].SpanContext().SpanID() {
			t.Fatalf("%s is not linked to the previous operation %s", sp.Name(), ops[i-1].Name())
		}
		if sp.SpanContext().TraceID() == ops[i-1].SpanContext().TraceID() {
			t.Fatalf("%s shares a trace with the previous operation; each operation should be its own trace", sp.Name())
		}
	}
	for _, i := range []int{0, 3} {
		if !harnessTraces[ops[i].SpanContext().TraceID()] {
			t.Fatalf("exec %d has no harness.run span in its trace", i)
		}
	}
}

// TestUnsampledForkRecordsNoTraceLinks checks that tracing off means no trace-link writes. With a
// no-op provider or a sampler that drops the operation, the server context still carries the
// caller's span context, but nothing will be exported to link to.
func TestUnsampledForkRecordsNoTraceLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		tp   trace.TracerProvider
	}{
		{"noop", noop.NewTracerProvider()},
		{"never_sample", sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
			otel.SetTracerProvider(tc.tp)
			otel.SetTextMapPropagator(propagation.TraceContext{})
			t.Cleanup(func() {
				otel.SetTracerProvider(prevTP)
				otel.SetTextMapPropagator(prevProp)
			})

			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			backend := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = backend.Close() })
			lis := bufconn.Listen(1 << 20)
			srv := grpc.NewServer(observability.ServerTracing())
			v1.RegisterSessionsServer(srv, session.NewService(store, echoRegistry(t, backend)))
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)
			conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			c := v1.NewSessionsClient(conn)

			ctx := metadata.AppendToOutgoingContext(context.Background(),
				"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
			s, err := c.CreateSession(ctx, &v1.CreateSessionRequest{})
			if err != nil {
				t.Fatal(err)
			}
			parent := s.GetMetadata().GetUid()
			execOutputs(t, c, parent, "one", 0)
			forked, err := c.Fork(ctx, &v1.ForkRequest{Session: parent, Count: 2})
			if err != nil {
				t.Fatal(err)
			}
			uids := []string{parent}
			for _, child := range forked.GetChildren() {
				uids = append(uids, child.GetMetadata().GetUid())
			}
			if len(uids) != 3 {
				t.Fatalf("fork returned %d children, want 2", len(uids)-1)
			}
			for _, uid := range uids {
				if tp, err := store.LastTraceParent(uid); err != nil || tp != "" {
					t.Errorf("session %s has trace link %q (err %v), want none", uid, tp, err)
				}
			}
		})
	}
}
