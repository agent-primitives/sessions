# Configured tool executors

`agentsessionsd -tool-executors tools.json` routes host-mediated tools to operator-run
processes on Unix sockets. No custom daemon binary or language binding is needed; tool
credentials stay in the executor, not the daemon. This flag is not available in v0.1.x.
Without it, the daemon retains its existing behavior: controller-mediated tools refuse;
approval-gated tools park, but approved Resume requires an executor. Denial needs none.

## Start an executor and configure the daemon

The [tiny Python example](../docs/tool-executor-example.py) uses only Python's standard library.
Its `publish` effect inserts a row in a local SQLite database, atomically with its durable
receipt. From the repository root, in a private directory on a Unix host:

```sh
umask 077
mkdir -p "$HOME/.local/run/sessions-tools"
python3 docs/tool-executor-example.py \
  "$HOME/.local/run/sessions-tools/publish.sock" \
  "$HOME/.local/run/sessions-tools/receipts.db"
```

Leave that process running. Create `tools.json` with the **actual absolute path** to the
socket (the config does not expand `$HOME`, `~`, or other environment expressions):

```json
{
  "version": 1,
  "executors": [
    {
      "name": "publishd",
      "type": "unix",
      "socket": "/home/you/.local/run/sessions-tools/publish.sock",
      "timeout_ms": 30000
    }
  ],
  "tools": [
    {
      "harness": "publisher",
      "tool": "publish",
      "executor": "publishd",
      "durable_dedup": true
    }
  ]
}
```

Start your stateless remote harness separately, then the daemon:

```sh
agentsessionsd -addr 127.0.0.1:8080 -journal sessions.db \
  -model your-model -harness publisher=127.0.0.1:9000 \
  -tool-executors tools.json
```

The harness emits `publish` using `CONTROLLER_MEDIATED` or `REQUIRES_APPROVAL`, with a
nonempty call ID and idempotency key; it must not perform the gated effect itself.
For approval, Exec parks **before any socket connection**. RPC/SDK Approve only commits
the decision; Resume executes. CLI approve/deny do both. Denial never dials. See
[approval recovery](interaction-model.md#approval-decisions-and-recovery). For the whole flow
with a model calling the tool, see the [approval-gated tool example](approval-gated-tool.md).

**Only static CLI `-harness` entries receive these executors.** Built-in echo/chat and
HarnessRegistry-registered harnesses do not. A route naming one fails startup, rather than
silently gaining privileges. There is no wildcard, prefix matching, fallback executor or
case folding: `(publisher, publish)` is distinct from `(Publisher, publish)` and
`(publisher, Publish)`. Unconfigured tools refuse without dialing.

## Config validation and dedup declarations

The file is versioned JSON, maximum **1 MiB**, loaded once before opening the journal or
binding listeners. Restart to change it. Invalid config fails startup even with no
`-harness` entries. Unknown fields, case-mismatched field names, duplicate JSON members,
trailing JSON, invalid UTF-8, wrong types, unsupported versions/types, empty required
strings, duplicate executor names/routes, unknown references and nonabsolute/NUL socket
paths are refused. Socket paths are limited to 103 bytes on every platform, leaving room
for the NUL terminator in macOS's stricter 104-byte `sun_path` (Linux has 108 bytes).
JSON nesting is limited to 100. No commands or credentials belong here.
`version` must be integer 1. `executors` and `tools` may be omitted/empty. Each executor
requires `name`, `type: "unix"`, and `socket`. Each tool requires `harness`, `tool`, and
`executor`. `timeout_ms` defaults to 30,000; explicit values must be integers from 1 through
300,000, not null. The flag conflicts with programmatic `WithToolExecutors`; neither
silently overrides the other. Startup never probes/dials the socket.

`durable_dedup` is true, false, or null; missing/null means unknown. Each route's declaration
is surfaced in structured startup logs alongside harness, tool and executor names: true at
Info, false/unknown at Warn.
**Only explicit true dispatches today, even for a first attempt.** False/unknown remain
representable but refuse before dialing. This restriction is deliberate: an approved call runs on
Resume, where its first attempt and a crash redrive look the same. Approval does not override it.
The daemon declares durable dedup to the controller (`controller.WithDurableDedup`) for true
routes, and for the tools it refuses before dialing: repeating a refusal repeats no action.

True is an operator assertion, not something the daemon can prove. **Read-only or naturally
idempotent tools can honestly declare true when rerunning them is harmless:** they do not
need a receipt database merely to avoid repeating a harmful effect. For other tools, use
durable dedup on **(session UID, idempotency key)**, not key alone or tool/key. Different
sessions may reuse a key. Within a session, the harness must not reuse a key for a different
operation, including across tools/executors. Preserve dedup state across restarts and endpoint
changes; concurrent calls and crash-after-effect recovery must not repeat the action.

## Socket protocol v1

One new AF_UNIX stream connection per invocation, one **UTF-8 JSON line** each way.
Each frame, including its terminating newline, is at most **1 MiB**. No handshake,
notifications, streaming, pooling or automatic retries. The client reads one response
line without waiting for EOF. JSON fields are case-sensitive; unknown response fields,
duplicate members, invalid UTF-8/types and nesting beyond 100 are rejected.

Request (shown on one line, including `arguments: null` when no args were supplied):

```json
{"version":1,"harness":"publisher","session_uid":"session-a","call_id":"call-1","tool":"publish","arguments":{"text":"hello"},"idempotency_key":"key-1"}
```

The daemon forwards the controller-supplied session UID and original call ID, tool,
arguments and key unchanged. Arguments use the existing JSON-shaped tool domain and its
[number limits](harness-authoring.md#host-composition-for-controller-mediated-tools), not
opaque JSON bytes. The daemon does not generate keys or add tool names to the dedup tuple.

Successful response:

```json
{"version":1,"call_id":"call-1","is_error":false,"output":{"text":"published"}}
```

Tool/domain error:

```json
{"version":1,"call_id":"call-1","is_error":true,"error":"resource not permitted"}
```

`version: 1`, matching `call_id`, and boolean `is_error` are required and cannot be null.
`output` is optional (object/null); `error` is an optional non-null string. There are no
content/URI/approval fields. The daemon maps output/error to ToolResult; the controller
owns result ID, approval sequence numbers and receipt codes. Executors must authorize
resources themselves. Return a correlated response even when serving a stored receipt.

## Timeouts, failures and recovery

A single deadline covers dial, write and read: the earlier of `timeout_ms` or the caller's
deadline. Cancellation closes the connection promptly, including blocked writes. Oversized
requests are refused before dialing; response reads are bounded. Missing sockets, timeouts,
cancellation, malformed/truncated/oversized replies and version/correlation errors are
transport/protocol Go errors, distinct from a valid executor `is_error` response. The
adapter includes route/stage context, not raw request/reply bytes, and never retries itself.

Ordinary controller-mediated transport errors leave unresolved intent. Resume can redrive it
with the same identity, relying on the executor's true declaration. For a false/unknown
route or an unconfigured tool, Resume re-sends the call to the same refusal without dialing
and the turn fails again; Exec a new turn to continue. Completed journal receipts are
replayed without dialing. Approved calls instead turn executor errors into **durable
EXECUTOR_ERROR receipts**, including false/unknown/unconfigured-tool refusals. Changing
configuration afterward does not retry such a receipted failure.

Cancelling Resume, or Resume's own deadline passing, while an approved call is in flight closes
its socket connection. When the call's error is of the same kind as Resume's ending (a
cancellation after Resume was cancelled, a deadline error after Resume's deadline passed), that
is not an executor failure: the controller records no receipt and leaves the approved call
pending, as a hard crash before the receipt commits would, so the next Resume redials it with
the original session UID and idempotency key. The executor may already have applied the effect,
so it must deduplicate on that key. A `timeout_ms` expiry or any other failure while Resume is
still running is a durable EXECUTOR_ERROR receipt, and so is a `timeout_ms` or per-call
deadline error returned after Resume was cancelled while Resume's own deadline, if any, has not
passed. Graceful daemon shutdown (`SIGINT`/`SIGTERM`) drains in-flight calls with
`GracefulStop`, so a call can still succeed.

**EXECUTOR_ERROR does not prove no effect occurred.** An executor can commit its effect and
lose the response; approved recovery records an error and will not redial to retrieve the
success receipt. Timeouts/disconnects cannot roll back an external action. Inspect/reconcile
the executor's durable state before retrying; **a new key can duplicate an already committed
effect**. Losing the daemon's journal receipt after a successful response is different:
recovery resends the original key, so a durable executor returns its stored result.

The Python example demonstrates the latter guarantee only for its transactional **local**
SQLite insert. It serializes requests and refuses changed arguments under a reused key.
Storage failures log only the SQLite error class/code to stderr, not arguments or raw errors.
Do not replace the insert with an HTTP request and assume the database makes remote delivery
atomic; the remote service needs its own idempotency/reconciliation mechanism.

## Security and operations

Keep the socket in a **private directory (0700)** with mode **0600**, or a deliberately
restricted group. Protect its parent directory against replacement: Unix transport alone
does not authenticate whoever answers. The example uses umask 077 but does not repair
permissions on preexisting directories/databases. Do not put credentials in config, arguments
or error strings; executor responses and arguments may enter the journal/model history.
Provision executor credentials separately, with least privilege.

The operator owns the process and socket lifecycle; the daemon never starts, unlinks or
supervises it. After a crash, verify the old executor is stopped before removing a stale
socket and restarting with the **same durable database**. Secure backups/retention of that
state are part of safe recovery.

The daemon is still plaintext and unauthenticated: **loopback or a wholly trusted network
only**. Exact routing is an allowlist, not user authentication, resource authorization or
multi-tenant isolation. Approval is not a security policy, nor does it constrain a trusted
harness's unmediated work. See [security posture](security.md).
