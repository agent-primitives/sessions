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
| `trace_id`, `span_id` | Present when the request has a valid span context, including one the caller propagated while tracing is off |

Operations add bounded fields such as `runtime`, `incarnation_id`, `actor`, `atespace`,
`expected_last_seq`, and record/effect counts. Placement records its capability decision and runtime
backends record external lifecycle calls such as actor creation, resume, suspend, and deletion.

## Enabling logs

Most library components default to a discard logger. The process that assembles them must inject one:

<!-- doccheck: body(harness api.Harness, model controller.ModelFunc, store *sqlitelog.Store) -->
```go
logger := slog.Default()
backend := local.New(harness, local.WithLogger(logger))
placer := placement.New(backend, model, placement.WithLogger(logger))
registry, err := placement.NewRegistry("my-agent", map[string]*placement.Placer{"my-agent": placer})
if err != nil {
    return err
}
service := session.NewService(store, registry, session.WithLogger(logger))
```

`HarnessRegistry` instead defaults to `slog.Default()` (captured at construction), including for
startup warnings and enforced-mode diagnostics. Configure it separately with
`session.WithRegistryLogger`; `session.WithLogger` only configures the Sessions service. A nil
registry logger uses the default too. An embedder can route both services to the same logger:

<!-- doccheck: body(store *sqlitelog.Store, registry *placement.Registry, factory session.PlacerFactory) -->
```go
logger := slog.Default()
registryService, err := session.NewHarnessRegistry(store, registry, factory,
    session.WithRegistryLogger(logger))
if err != nil {
    return err
}
defer registryService.Close()
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
`observability/otelsetup.Init`. It always installs the W3C TraceContext propagator, and exports
over OTLP/gRPC only when the standard environment asks for it. Baggage is not propagated, so
caller-supplied baggage does not reach substrate or the harness.

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
export OTEL_EXPORTER_OTLP_INSECURE=true      # plaintext collector
export OTEL_TRACES_SAMPLER=parentbased_always_on
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=dev
```

The daemon parents each RPC on the caller's `traceparent` when one arrives. The caller therefore
picks the trace ID that `session_traces` records and, under the default `parentbased_*` samplers,
whether the operation is sampled. Callers outside your trust boundary can steer both; use a
non-parent-based sampler, or strip `traceparent` at the edge, if that matters.

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

To stitch a session back together, every mutating operation except delete (`exec`, `fork`, `suspend`,
`resume`, `approve`) adds a span link to the session's previous operation. The link has the attribute
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

A harness that its runtime can suspend must export its spans before the turn ends. Spans still
buffered when a snapshot is taken are exported only if that snapshot is restored. `harnessnode`
ends its `Connect` server span and flushes before the handler returns, so the export finishes
before gRPC sends the final status the host waits for. Ending the span in a gRPC stats handler, as
`otelgrpc` does, is too late: grpc-go reports the end of an RPC to stats handlers only after it has
written the status. A custom harness server that can be suspended needs the same ordering: end its
spans and call `otelsetup.Flush` inside the handler. The flush is bounded to one second, so an
unreachable collector costs spans rather than holding the turn open.

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
- raw peer error strings, which may contain payload data, except for the diagnostic-only case below

Model-policy `Describe` failures retain `request_id`, `method`, `error_kind` and the stable
`error_category=external` in the configured Placer logger, never the backend's message, path or
endpoint. This includes in-process harnesses and errors wrapping storage sentinels. Registration
and startup failures are also correlated in HarnessRegistry's configured logger. Only errors known
at its direct store boundary include the full cause with `error_category=host`; arbitrary factory
and runtime errors stay `external` without message text. Startup diagnostics create a correlation
ID when there is no RPC context. Public policy-Describe errors remain sanitized.

For an unavailable stored route, the administrative registry metadata's `unservable_reason` retains
its existing diagnostic detail, including factory/configuration errors. This administrative
channel can contain sensitive details: keep it access-controlled and do not forward its contents
to general logs. The startup summary deliberately omits that raw cause; host-built parsing errors
can still quote untrusted stored values.

Use `error_kind`, gRPC status, identifiers, counts, and durations for diagnosis. Spans never
record exception messages, stack traces, or raw error strings; a recorded error carries only its
type.

Third-party instrumentation does not follow this policy by default. `otelgrpc` puts the gRPC status
message in the span status, `otelhttp` records `err.Error()` and the full request URL, query
included, and the SDK records a panic's value when a span's `End` is deferred directly. Spans from
`observability.Tracer()`, `observability.ServerTracing()`, `observability.ClientTracing()`, and the
OpenAI-compatible client go through a redacting provider. It keeps status codes but drops
third-party status descriptions, records only an error's type, drops `exception.message` and
`exception.stacktrace`, and strips the query, user info, and fragment from `url.full`. The errors
returned to callers are unchanged. Instrument other libraries with
`observability.ThirdPartyTracerProvider()`. Spans a harness author starts with their own tracer
are outside this policy.

With tracing off, or when the sampler drops an operation, nothing is written to `session_traces`.

With optional Go RPC authorization enforced, `INTERNAL` diagnostics include `request_id`, `method`,
`error_kind` and stable `error_category` before the wire error becomes a generic status:

- Known trusted host/store errors log the full cause with `error_category=host`.
- Model/provider/harness and other unknown-origin errors never log message text. Category is
  `execution` for Exec/Resume, `runtime` for Suspend/Fork, or `external` for other methods.

Unknown mixed execution/runtime phases fail closed. A store fault deep inside runtime is not
trusted unless known at the service storage boundary. No heuristic redaction is used. This
diagnostic-only exception does not permit request metadata, credentials or session payloads in
logs, or raw errors in ordinary operational events. Trusted/off behavior is unchanged. Policies
own their failure logs and metrics, without sensitive data; the service never logs raw policy
causes. See [optional RPC authorization](security.md#optional-go-rpc-authorization).
