// Package otelsetup installs the OpenTelemetry SDK for an agentsessions process.
//
// Libraries in this module depend only on the OpenTelemetry API and emit no-op spans until a
// process opts in. Commands call Init once at startup; it is configured entirely by the standard
// OTEL_* environment variables, so the same binary runs with tracing off by default and on in a
// deployment that sets an exporter endpoint.
package otelsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/agent-primitives/sessions/internal/version"
)

// Init installs the W3C trace-context propagator and, when an OTLP endpoint is configured, a
// batching OTLP/gRPC tracer provider. It returns a shutdown function that flushes buffered spans;
// call it before the process exits.
//
// Tracing is enabled when OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set
// and OTEL_TRACES_EXPORTER is not "none". Sampling follows OTEL_TRACES_SAMPLER (default
// parentbased_always_on), and OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override the resource.
//
// The propagator is installed even when export is off, so a process that only forwards requests
// still passes a caller's trace context through to the next hop.
func Init(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, fmt.Errorf("otelsetup: create OTLP trace exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version.Get().Version),
		),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		// Last, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the defaults above.
		resource.WithFromEnv(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return noop, fmt.Errorf("otelsetup: build resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Enabled reports whether the environment asks for trace export.
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Flush exports every span the global provider has buffered, if it is an SDK provider. A process
// that can be frozen between requests, such as a harness that its runtime snapshots on suspend,
// calls it at the end of each request: spans still buffered at snapshot time are only exported if
// that snapshot is restored, and are lost if it never is.
func Flush(ctx context.Context) error {
	if p, ok := otel.GetTracerProvider().(interface{ ForceFlush(context.Context) error }); ok {
		return p.ForceFlush(ctx)
	}
	return nil
}
