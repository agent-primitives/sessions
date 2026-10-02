// Package observability provides structured operation logging and gRPC request correlation.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"
)

type requestIDKey struct{}

// WithRequestID adds a request correlation ID to ctx.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID returns the request correlation ID carried by ctx.
func RequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

// EnsureRequestID preserves an existing request ID or creates one.
func EnsureRequestID(ctx context.Context) context.Context {
	if validRequestID(RequestID(ctx)) {
		return ctx
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	return WithRequestID(ctx, hex.EncodeToString(id[:]))
}

func validRequestID(requestID string) bool {
	if requestID == "" || len(requestID) > 128 {
		return false
	}
	for _, r := range requestID {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// Logger returns logger enriched with context correlation fields: request_id, and trace_id/span_id
// when ctx carries a valid span, so a log line can be pivoted to its trace.
func Logger(ctx context.Context, logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if requestID := RequestID(ctx); validRequestID(requestID) {
		logger = logger.With("request_id", requestID)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		logger = logger.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	return logger
}

// Start records an info-level operation start and returns its completion recorder.
//
// Deprecated: use Begin, which also returns the operation's span context so nested operations
// become child spans. Start still emits a span, but work done under the caller's ctx is not nested
// beneath it.
func Start(ctx context.Context, logger *slog.Logger, component, operation string, attrs ...any) func(error, ...any) {
	_, finish := begin(ctx, logger, slog.LevelInfo, true, component, operation, attrs...)
	return finish
}

// StartDebug records a debug-level operation start and returns its completion recorder.
//
// Deprecated: use BeginDebug.
func StartDebug(ctx context.Context, logger *slog.Logger, component, operation string, attrs ...any) func(error, ...any) {
	_, finish := begin(ctx, logger, slog.LevelDebug, true, component, operation, attrs...)
	return finish
}

// Begin records an info-level operation start, opens a span named "<component>.<operation>", and
// returns a context carrying that span plus the completion recorder that logs the finish and ends
// the span. Pass the returned context to nested work so it is traced as a child.
func Begin(ctx context.Context, logger *slog.Logger, component, operation string, attrs ...any) (context.Context, func(error, ...any)) {
	return begin(ctx, logger, slog.LevelInfo, true, component, operation, attrs...)
}

// BeginDebug is Begin at debug level. Failures are promoted according to error_kind so internal
// faults remain visible at error level. The span is recorded regardless of the log level.
func BeginDebug(ctx context.Context, logger *slog.Logger, component, operation string, attrs ...any) (context.Context, func(error, ...any)) {
	return begin(ctx, logger, slog.LevelDebug, true, component, operation, attrs...)
}

// begin logs the operation start and, when withSpan is set, opens its span. The gRPC interceptors
// pass withSpan=false: otelgrpc's stats handler already owns the RPC span, and a second span per
// request would only duplicate it.
func begin(ctx context.Context, logger *slog.Logger, successLevel slog.Level, withSpan bool, component, operation string, attrs ...any) (context.Context, func(error, ...any)) {
	span := trace.SpanFromContext(ctx)
	if withSpan {
		spanAttrs := spanAttributes(attrs)
		if requestID := RequestID(ctx); validRequestID(requestID) {
			spanAttrs = append(spanAttrs, attributeOf("agentsessions.request_id", requestID))
		}
		ctx, span = Tracer().Start(ctx, component+"."+operation, trace.WithAttributes(spanAttrs...))
	} else if requestID := RequestID(ctx); validRequestID(requestID) {
		span.SetAttributes(attributeOf("agentsessions.request_id", requestID))
	}

	baseAttrs := make([]any, 0, len(attrs)+4)
	baseAttrs = append(baseAttrs,
		"component", component,
		"operation", operation,
	)
	baseAttrs = append(baseAttrs, attrs...)
	logger = Logger(ctx, logger).With(baseAttrs...)
	started := time.Now()
	logger.Log(ctx, successLevel, "operation started", "phase", "start")

	return ctx, func(err error, resultAttrs ...any) {
		if withSpan {
			defer finishSpan(span, err, resultAttrs)
		}
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		attrs := make([]any, 0, len(resultAttrs)+6)
		attrs = append(attrs, resultAttrs...)
		attrs = append(attrs,
			"phase", "finish",
			"outcome", outcome,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		logger.Log(ctx, completionLevel(successLevel, err, resultAttrs), "operation finished", attrs...)
	}
}

func completionLevel(successLevel slog.Level, err error, attrs []any) slog.Level {
	if err == nil {
		return successLevel
	}
	switch stringAttr(attrs, "error_kind") {
	case "invalid_argument", "conflict", "fenced", "canceled", "deadline_exceeded",
		"failed_precondition", "unplaceable", "not_found", "already_exists":
		return slog.LevelInfo
	case "unavailable":
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func stringAttr(attrs []any, key string) string {
	for i := 0; i < len(attrs); {
		switch attr := attrs[i].(type) {
		case slog.Attr:
			if attr.Key == key && attr.Value.Kind() == slog.KindString {
				return attr.Value.String()
			}
			i++
		case string:
			if i+1 < len(attrs) && attr == key {
				value, _ := attrs[i+1].(string)
				return value
			}
			i += 2
		default:
			i++
		}
	}
	return ""
}
