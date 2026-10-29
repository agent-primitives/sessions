package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans installs an in-memory provider for the duration of the test.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestBeginNestsSpansAndKeepsRawErrorsOut(t *testing.T) {
	rec := recordSpans(t)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ctx := WithRequestID(context.Background(), "request-1")

	ctx, finishOuter := Begin(ctx, logger, "session", "exec", "session_uid", "s-1")
	_, finishInner := Begin(ctx, logger, "controller", "exec")
	finishInner(errors.New("secret detail"), "error_kind", "harness_failed")
	finishOuter(nil)

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	inner, outer := spans[0], spans[1]
	if inner.Name() != "controller.exec" || outer.Name() != "session.exec" {
		t.Fatalf("span names = %q, %q", inner.Name(), outer.Name())
	}
	if inner.Parent().SpanID() != outer.SpanContext().SpanID() {
		t.Fatal("controller.exec is not a child of session.exec")
	}
	if inner.Status().Code != codes.Error || inner.Status().Description != "harness_failed" {
		t.Fatalf("inner status = %+v, want Error/harness_failed", inner.Status())
	}
	for _, s := range spans {
		for _, ev := range s.Events() {
			if ev.Name == "exception" {
				t.Fatalf("span %s recorded a raw error event", s.Name())
			}
		}
	}

	var first map[string]any
	if err := json.NewDecoder(&output).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first["trace_id"] != outer.SpanContext().TraceID().String() {
		t.Fatalf("log trace_id = %v, want %s", first["trace_id"], outer.SpanContext().TraceID())
	}
}

func TestLinkTraceParentRoundTrips(t *testing.T) {
	rec := recordSpans(t)
	ctx, first := Tracer().Start(context.Background(), "first")
	tp := TraceParent(ctx)
	first.End()
	if tp == "" {
		t.Fatal("TraceParent returned empty inside a recording span")
	}

	ctx, second := Tracer().Start(context.Background(), "second")
	LinkTraceParent(ctx, tp)
	LinkTraceParent(ctx, "not-a-traceparent")
	second.End()

	links := rec.Ended()[1].Links()
	if len(links) != 1 || links[0].SpanContext.SpanID() != first.SpanContext().SpanID() {
		t.Fatalf("links = %+v, want one link to the first span", links)
	}
}

func TestTraceParentEmptyWithoutSpan(t *testing.T) {
	if tp := TraceParent(context.Background()); tp != "" {
		t.Fatalf("TraceParent = %q, want empty", tp)
	}
}
