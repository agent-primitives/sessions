# Observability

AgentSessions emits structured `log/slog` events across the Sessions gRPC boundary, placement,
controller, harness transport, and runtime backends. A request ID is propagated through gRPC metadata
so one turn can be reconstructed across the host and harness processes. The reusable helpers and gRPC
interceptors are exported from the public `observability` package.

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

## Data policy

Operational logs deliberately exclude:

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

Use `error_kind`, gRPC status, identifiers, counts, and durations for diagnosis.

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
