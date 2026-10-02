package session_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/runtime/local"
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
