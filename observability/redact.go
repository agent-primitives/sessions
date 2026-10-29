package observability

import (
	"context"
	"fmt"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// redactingProvider applies the logging data policy to spans: no raw error text and no URL
// credentials. Stock instrumentation does not follow that policy on its own. otelgrpc puts the gRPC
// status message in the span status, otelhttp puts err.Error() there and the request URL, query
// included, in url.full, and the SDK records a panic's value when a span's End is deferred directly.
//
// Errors returned to callers are unchanged; only what the span records is redacted.
type redactingProvider struct {
	embedded.TracerProvider
	tp trace.TracerProvider
	// trusted keeps status descriptions. Only agentsessions' own spans are trusted, because they set a
	// stable error_kind there and never err.Error().
	trusted bool
}

// redacted wraps the global provider. The global provider delegates to whatever provider a process
// installs later, so this can be built before otelsetup.Init runs.
func redacted(trusted bool) trace.TracerProvider {
	return redactingProvider{tp: otel.GetTracerProvider(), trusted: trusted}
}

func (p redactingProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return redactingTracer{t: p.tp.Tracer(name, opts...), p: p}
}

type redactingTracer struct {
	embedded.Tracer
	t trace.Tracer
	p redactingProvider
}

func (t redactingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	clean := []trace.SpanStartOption{
		trace.WithAttributes(redactAttrs(cfg.Attributes())...),
		trace.WithLinks(redactLinks(cfg.Links())...),
		trace.WithSpanKind(cfg.SpanKind()),
	}
	if ts := cfg.Timestamp(); !ts.IsZero() {
		clean = append(clean, trace.WithTimestamp(ts))
	}
	if cfg.NewRoot() {
		clean = append(clean, trace.WithNewRoot())
	}
	ctx, span := t.t.Start(ctx, name, clean...)
	rs := redactingSpan{Span: span, p: t.p}
	return trace.ContextWithSpan(ctx, rs), rs
}

// redactingSpan wraps a span. Because End is a method of the wrapper, a deferred span.End() calls
// the SDK's End indirectly, so the SDK's recover() sees no panic and records no panic value.
type redactingSpan struct {
	trace.Span
	p redactingProvider
}

func (s redactingSpan) SetStatus(code codes.Code, description string) {
	if !s.p.trusted {
		description = ""
	}
	s.Span.SetStatus(code, description)
}

// RecordError records the error's type only: its message may carry payload data or credentials.
func (s redactingSpan) RecordError(err error, opts ...trace.EventOption) {
	if err == nil {
		return
	}
	cfg := trace.NewEventConfig(opts...)
	attrs := append([]attribute.KeyValue{attribute.String("exception.type", fmt.Sprintf("%T", err))}, redactAttrs(cfg.Attributes())...)
	s.Span.AddEvent("exception", eventOptions(cfg, attrs)...)
}

func (s redactingSpan) AddEvent(name string, opts ...trace.EventOption) {
	cfg := trace.NewEventConfig(opts...)
	s.Span.AddEvent(name, eventOptions(cfg, redactAttrs(cfg.Attributes()))...)
}

func (s redactingSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.Span.SetAttributes(redactAttrs(kv)...)
}

func (s redactingSpan) AddLink(link trace.Link) {
	link.Attributes = redactAttrs(link.Attributes)
	s.Span.AddLink(link)
}

// End drops WithStackTrace: a stack trace is only useful for a panic, and the panic is not recorded.
func (s redactingSpan) End(opts ...trace.SpanEndOption) {
	cfg := trace.NewSpanEndConfig(opts...)
	if ts := cfg.Timestamp(); !ts.IsZero() {
		s.Span.End(trace.WithTimestamp(ts))
		return
	}
	s.Span.End()
}

func (s redactingSpan) TracerProvider() trace.TracerProvider { return s.p }

func eventOptions(cfg trace.EventConfig, attrs []attribute.KeyValue) []trace.EventOption {
	opts := []trace.EventOption{trace.WithAttributes(attrs...)}
	if ts := cfg.Timestamp(); !ts.IsZero() {
		opts = append(opts, trace.WithTimestamp(ts))
	}
	return opts
}

func redactLinks(links []trace.Link) []trace.Link {
	for i := range links {
		links[i].Attributes = redactAttrs(links[i].Attributes)
	}
	return links
}

// redactAttrs drops free-form error text and strips credentials from URLs.
func redactAttrs(kv []attribute.KeyValue) []attribute.KeyValue {
	out := kv[:0:0]
	for _, a := range kv {
		switch a.Key {
		case "exception.message", "exception.stacktrace", "rpc.grpc.status_message":
			continue
		case "url.full", "http.url":
			a = attribute.String(string(a.Key), redactURL(a.Value.AsString()))
		}
		out = append(out, a)
	}
	return out
}

// redactURL keeps scheme, host and path. The query and userinfo are where credentials go (an
// api-key query parameter, for one), and the fragment is never needed to identify an endpoint.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}
