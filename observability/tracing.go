package observability

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// TracerName is the instrumentation scope for spans emitted by agentsessions libraries.
const TracerName = "github.com/aramase/agentsessions"

// Tracer returns the agentsessions tracer from the global provider. Libraries only depend on the
// OpenTelemetry API: until a process installs an SDK provider (see observability/otelsetup), every
// span is a no-op and nothing leaves the process. Its spans never record a panic value (see
// redactingProvider).
func Tracer() trace.Tracer { return redacted(true).Tracer(TracerName) }

// ThirdPartyTracerProvider is the provider to hand to third-party instrumentation (otelgrpc,
// otelhttp). It drops status descriptions and error messages and strips URL credentials, because
// that instrumentation records raw error text and full request URLs.
func ThirdPartyTracerProvider() trace.TracerProvider { return redacted(false) }

// ServerTracing instruments a gRPC server so it continues a caller's W3C trace context.
func ServerTracing() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler(otelgrpc.WithTracerProvider(ThirdPartyTracerProvider())))
}

// ClientTracing instruments a gRPC client connection so calls carry the caller's trace context.
func ClientTracing() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler(otelgrpc.WithTracerProvider(ThirdPartyTracerProvider())))
}

// TraceParent encodes ctx's span context as a W3C traceparent, or "" when ctx carries no valid
// span (tracing is disabled, or the caller is outside any span).
func TraceParent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// LinkTraceParent links the current span to the span identified by traceparent. It is how one
// session's operations are chained across waits and suspensions without holding a trace open: each
// operation is its own trace, linked to the one before it.
func LinkTraceParent(ctx context.Context, traceparent string, attrs ...attribute.KeyValue) {
	if traceparent == "" {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	linked := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceparent})
	sc := trace.SpanContextFromContext(linked)
	if !sc.IsValid() {
		return
	}
	span.AddLink(trace.Link{SpanContext: sc, Attributes: attrs})
}

// spanAttributes converts slog-style alternating key/value pairs (or slog.Attr values) into span
// attributes. Only the bounded fields the logging schema already permits are passed here, so the
// span inherits the same data policy: no payloads, tool arguments, fence tokens, or raw errors.
func spanAttributes(kv []any) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(kv)/2)
	for i := 0; i < len(kv); {
		if a, ok := kv[i].(slog.Attr); ok {
			out = append(out, attributeOf("agentsessions."+a.Key, a.Value.Any()))
			i++
			continue
		}
		key, ok := kv[i].(string)
		if !ok || i+1 >= len(kv) {
			i++
			continue
		}
		out = append(out, attributeOf("agentsessions."+key, kv[i+1]))
		i += 2
	}
	return out
}

func attributeOf(key string, v any) attribute.KeyValue {
	switch v := v.(type) {
	case string:
		return attribute.String(key, v)
	case bool:
		return attribute.Bool(key, v)
	case int:
		return attribute.Int(key, v)
	case int32:
		return attribute.Int64(key, int64(v))
	case int64:
		return attribute.Int64(key, v)
	case float64:
		return attribute.Float64(key, v)
	case fmt.Stringer:
		return attribute.String(key, v.String())
	default:
		return attribute.String(key, fmt.Sprint(v))
	}
}

func finishSpan(span trace.Span, err error, resultAttrs []any) {
	if !span.IsRecording() {
		span.End()
		return
	}
	span.SetAttributes(spanAttributes(resultAttrs)...)
	if err != nil {
		// The status carries the stable error_kind, never err.Error(): raw error strings may
		// contain payload data, which the data policy keeps out of telemetry.
		kind := stringAttr(resultAttrs, "error_kind")
		if kind == "" {
			kind = "internal"
		}
		span.SetStatus(codes.Error, kind)
	}
	span.End()
}
