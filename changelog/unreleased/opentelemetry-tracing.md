---
category: Added
---

- OpenTelemetry tracing. Session, placement, controller, runtime, and harness-wire operations emit
  spans, and gRPC clients and servers propagate W3C trace context, including into substrate. Each
  session operation is linked to the session's previous one, so a session can be followed across
  suspensions. The reference binaries export over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set.
  See [docs/observability.md](../../docs/observability.md#tracing). Spans follow the logging data
  policy: gRPC status messages, error messages, panic values, and URL queries are redacted, including
  from `otelgrpc` and `otelhttp` spans.
- `observability.Begin` and `observability.BeginDebug`, which return the span's context. `Start`
  and `StartDebug` are deprecated but unchanged.
