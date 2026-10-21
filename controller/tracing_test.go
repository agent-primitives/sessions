package controller

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agent-primitives/sessions/api"
)

// RecordSpans installs a recording global tracer provider for the test and returns its recorder.
// It is exported so controller_test files can use it too.
func RecordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// SpanCount reports how many ended spans rec saw with the given name.
func SpanCount(rec *tracetest.SpanRecorder, name string) int {
	n := 0
	for _, s := range rec.Ended() {
		if s.Name() == name {
			n++
		}
	}
	return n
}

// An approved call runs its executor outside execTool; it must still get a tool_call span.
func TestApprovedToolCallIsTraced(t *testing.T) {
	rec := RecordSpans(t)
	log := decidedGateLog(t)
	c, err := New(log, nil, WithSessionUID("s"), WithToolExecutor(func(context.Context, ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		return api.ToolResult{Content: api.TextMessage("tool", "ok").Parts}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	h := &approvalHarness{fn: func(ctx context.Context, _ *api.Start, s api.EventSink) error {
		if _, err := s.ToolCall(ctx, *gateEvents()[1].ToolCall); err != nil {
			return err
		}
		return s.Output(ctx, "handled")
	}}
	if resumed, err := c.Resume(t.Context(), h); !resumed || err != nil {
		t.Fatalf("Resume = %v, %v", resumed, err)
	}
	if n := SpanCount(rec, "controller.tool_call"); n != 1 {
		t.Fatalf("controller.tool_call spans = %d, want 1", n)
	}
}
