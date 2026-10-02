# Observability

AgentSessions emits structured `log/slog` events across the Sessions gRPC boundary, placement,
controller, harness transport, and runtime backends. A request ID is propagated through gRPC metadata
so one turn can be reconstructed across the host and harness processes. The same operations also
emit OpenTelemetry spans (see [Tracing](#tracing)). The reusable helpers and gRPC interceptors are
exported from the public `observability` package.

## Event schema

Boundary operations (`grpc` and `session`) emit start and finish events at `INFO`. Internal placement,
controller, and runtime operations emit the same pairs at `DEBUG`; their failures are promoted based on
`error_kind`. At an `INFO` threshold, an internal failure can therefore appear without its debug start
record. Stable fields are:

| Field | Meaning |
|---|---|
| `component` | `grpc`, `session`, `placement`, `controller`, or a runtime such as `runtime.substrate` |
| `operation` | Stable machine-readable operation name |
| `phase` | `start` or `finish` |
| `outcome` | `success` or `error` on finish events |
| `duration_ms` | End-to-end operation duration on finish events |
| `request_id` | Correlation ID propagated over gRPC |
| `session_uid` | Session correlation where available |
| `error_kind` | Stable error category; logs do not depend on parsing error text |
| `trace_id`, `span_id` | Present when tracing is enabled, so a log line opens its trace |

Operations add bounded fields such as `runtime`, `incarnation_id`, `actor`, `atespace`,
`expected_last_seq`, and record/effect counts. Placement records its capability decision and runtime
backends record external lifecycle calls such as actor creation, resume, suspend, and deletion.

## Enabling logs

Library components default to a discard logger. The process that assembles them must inject one:

```go
logger := slog.Default()
backend := local.New(harness, local.WithLogger(logger))
placer := placement.New(backend, model, placement.WithLogger(logger))
service := session.NewService(store, placer, session.WithLogger(logger))
```

Use `observability.UnaryServerInterceptor` and `observability.StreamServerInterceptor` on gRPC
servers, and the corresponding client interceptors, to preserve `request_id` across process
boundaries. `cmd/agentctl` and `cmd/harnessnode` provide reference wiring.

Use the `grpc.Chain*Interceptor` and `grpc.WithChain*Interceptor` options when composing these with
authentication, tracing, or other interceptors.

## Tracing

Every operation that logs a start/finish pair also opens a span named `<component>.<operation>`, such
as `session.exec`, `placement.create_compute`, `controller.model_call`, or
`runtime.substrate.resume_actor`. The span carries the same bounded fields as `agentsessions.*`
attributes and, on failure, an error status whose description is the `error_kind`. Libraries depend
only on the OpenTelemetry API, so spans are no-ops until a process installs an SDK.

### Enabling tracing

The reference binaries (`agentsessionsd`, `harnessnode`, and `agentctl`) call
`observability/otelsetup.Init`. It always installs the W3C TraceContext and Baggage propagators, and
exports over OTLP/gRPC only when the standard environment asks for it:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
export OTEL_EXPORTER_OTLP_INSECURE=true      # plaintext collector
export OTEL_TRACES_SAMPLER=parentbased_always_on
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=dev
```

`OTEL_TRACES_EXPORTER=none` turns export off. A process that assembles the libraries itself installs
its own provider and adds `observability.ServerTracing()` to its gRPC servers and
`observability.ClientTracing()` to its gRPC clients, including the connection it hands to
`runtime/substrate`. Substrate already propagates W3C trace context, and its node components sample
by parent, so `ateapi`, `atelet`, and `ateom` spans for actor lifecycle calls join the session's trace.

### One trace per operation, linked per session

Each Sessions RPC is its own trace: `agentctl.exec` → `session.exec` → `placement.*` and
`runtime.*` → `controller.exec` → `controller.model_call`, `controller.tool_call`, and the harness's
spans. A trace is never held open across an idle gap or a suspension, which would leave spans
unexported and break on a host restart.

To stitch a session back together, every mutating operation (`exec`, `fork`, `suspend`, `resume`)
adds a span link to the session's previous operation. The link has the attribute
`agentsessions.link=session.previous_operation`. Each operation then records its own `traceparent`
in the store's `session_traces` table. Fork children start their chain at the fork. That table is
telemetry only: it is outside the hash chain and never replayed, and losing it only loses a link.
Search by the `agentsessions.session_uid` attribute to find every trace of a session, and follow
the links to put them in order.

Links are only as durable as the tracing backend's retention, and a sampler that drops one
operation leaves a dangling link. Session-consistent sampling is not implemented yet.

### Harness spans

`harnesswire` continues the host's trace over the `Harness.Connect` stream, so harness-side spans
belong to the turn's trace. `harness.run` wraps the whole turn. `harness.sink.model` and
`harness.sink.tool_call` show how long the harness waited on the host, and the host-side
`controller.model_call` and `controller.tool_call` spans explain that wait. A harness author adds
their own spans by starting them from the `ctx` that `Run` receives; `harness/chatagent` shows the
pattern.

Harness spans are reported by the harness, so treat them as untrusted, just as
`IN_HARNESS_REPORTED` tool results are. They also need a network path from the sandbox to the
collector. During replay or resume, harness spans sit under `controller.replay` or
`controller.resume`, which distinguishes re-driven work from new work.

The OpenAI-compatible model client records an HTTP client span per completion. It does not send
`traceparent` to the model endpoint, because the endpoint is usually a third party.

## Data policy

Operational logs and span attributes deliberately exclude:

- message, prompt, and response contents
- tool arguments and results
- fence-token values
- snapshot URIs
- raw error strings, which may contain payload data

Use `error_kind`, gRPC status, identifiers, counts, and durations for diagnosis. Spans never
record exception events or error strings.
