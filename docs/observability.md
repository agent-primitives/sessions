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

Library components default to a discard logger. The process that assembles them must inject one:

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
- raw error strings, which may contain payload data, except for the diagnostic-only case below

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
