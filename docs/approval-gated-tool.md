# Approval-gated tool on the stock daemon

A model calls a tool that needs a human decision. The session parks, survives a daemon restart,
and after approval the tool runs exactly once. Everything here is a stock binary from this
repository: `agentsessionsd`, `harnessnode` and `agentctl`, plus the
[Python executor example](../docs/tool-executor-example.py). No custom host is built.

```mermaid
sequenceDiagram
  participant U as agentctl
  participant D as agentsessionsd
  participant H as harnessnode (tool)
  participant M as model endpoint
  participant E as executor (publish.sock)
  U->>D: exec "Publish the text: hello"
  D->>H: Start
  H->>D: Model (tools: publish)
  D->>M: chat completion
  M-->>D: tool call publish {"text":"hello"}
  H->>D: ToolCall (REQUIRES_APPROVAL)
  D-->>U: parked: TOOL_CALL, APPROVAL_REQUEST
  Note over D: restart; the request stays pending
  U->>D: approve, then resume
  D->>H: Start (replay)
  H->>D: same ToolCall
  D->>E: publish {"text":"hello"} (once)
  D->>M: completion with the tool result
```

## What you need

- Go and Python 3 on a Unix host.
- An OpenAI-compatible Chat Completions endpoint and a model that supports function tools. Use a
  non-reasoning model: tool calls on reasoning models are not supported yet
  ([#155](https://github.com/agent-primitives/sessions/issues/155)). The key comes from
  `MODEL_API_KEY`, never a flag.

## Run it

From the repository root, build the binaries:

```sh
go build -o bin/ ./cmd/agentsessionsd ./cmd/agentctl ./cmd/harnessnode
```

Start the executor in a private directory, as in [tool executors](tool-executors.md). Its
`publish` effect inserts one row per call, deduplicated on (session UID, idempotency key):

```sh
umask 077
mkdir -p "$HOME/.local/run/sessions-tools"
python3 docs/tool-executor-example.py \
  "$HOME/.local/run/sessions-tools/publish.sock" \
  "$HOME/.local/run/sessions-tools/receipts.db"
```

In another terminal, start the reference tool harness. `HARNESS_KIND=tool` offers the model one
tool, `publish`, and marks every call `REQUIRES_APPROVAL`; the harness never runs a tool itself.
Pass the model ID the daemon will use:

```sh
HARNESS_KIND=tool HARNESS_MODEL=your-model \
  HARNESS_ADDR=127.0.0.1:9000 HARNESS_READYZ=127.0.0.1:9001 bin/harnessnode
```

Create `tools.json` with the actual absolute socket path (no `$HOME` or `~` expansion). The
`harness` field is the name the daemon gives the harness with `-harness`:

```json
{
  "version": 1,
  "executors": [
    {
      "name": "publishd",
      "type": "unix",
      "socket": "/home/you/.local/run/sessions-tools/publish.sock"
    }
  ],
  "tools": [
    {"harness": "publisher", "tool": "publish", "executor": "publishd", "durable_dedup": true}
  ]
}
```

Start the daemon in a third terminal:

```sh
MODEL_API_KEY=... bin/agentsessionsd -addr 127.0.0.1:8080 -journal sessions.db \
  -model your-model -model-base-url https://your-endpoint/v1 \
  -harness publisher=127.0.0.1:9000 -tool-executors tools.json
```

Run a turn. The model calls `publish`, and the turn parks before the executor is dialed:

```console
$ bin/agentctl exec -server 127.0.0.1:8080 -harness publisher -input 'Publish the text: hello'
session sess-795307d5d3e863610eddda6b
seq=1   EVENT_EXECUTION_START
seq=2   EVENT_INPUT  user: Publish the text: hello
seq=3   EVENT_MODEL_CALL
seq=4   EVENT_OUTPUT  assistant:
seq=5   EVENT_TOOL_CALL  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args={"text":"hello"}
seq=6   EVENT_APPROVAL_REQUEST
seq=7   EVENT_LIFECYCLE
```

The model's call ID and text vary by model. Each record prints on one line. `agentctl` prints
a tool name or call ID that contains a space, quote, control or format character as a quoted,
escaped string. In message text (`EVENT_INPUT`, `EVENT_OUTPUT`) and in the arguments, it writes
control, format and other non-printing characters as escapes such as `\n`, `\x1b` or `\u202e`.
`exec` prints text the model streams the same way, on its own line starting `stream:`. So the
model's text cannot add a line that looks like a call, or use terminal escapes to hide part of the
call.

Stop the daemon and the harness (Ctrl-C) and start them again with the same commands. Nothing
has run, and the request is still pending; `replay` shows the exact call you are deciding on:

```console
$ bin/agentctl replay -server 127.0.0.1:8080 -session sess-795307d5d3e863610eddda6b
...
seq=5   EVENT_TOOL_CALL  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args={"text":"hello"}
seq=6   EVENT_APPROVAL_REQUEST
seq=7   EVENT_LIFECYCLE
```

Approve it. `agentctl approve` commits the decision, then resumes the session: the harness
replays, re-emits the same call, and the daemon sends it to the executor once. Use
`agentctl deny` instead to record a denial; a denied call never dials the executor.

```console
$ bin/agentctl approve -server 127.0.0.1:8080 -reason "checked the text" sess-795307d5d3e863610eddda6b
session sess-795307d5d3e863610eddda6b decision committed seq=8 approved=true compute=COMPUTE_LIVE last_seq=13
$ bin/agentctl replay -server 127.0.0.1:8080 -session sess-795307d5d3e863610eddda6b
...
seq=8   EVENT_APPROVAL_RESULT
seq=9   EVENT_TOOL_RESULT
seq=10  EVENT_MODEL_CALL
seq=11  EVENT_OUTPUT  assistant: Published: {"text":"published"}
seq=12  EVENT_END
seq=13  EVENT_LIFECYCLE
```

The executor's database has one row, keyed by the session UID and the call's idempotency key
(`<execution id>/<call id>`). The turn is complete, so further `agentctl resume` calls, before or
after another restart, run nothing and add no row:

```console
$ python3 -c 'import sqlite3, os; print(sqlite3.connect(os.path.expanduser("~/.local/run/sessions-tools/receipts.db")).execute("select count(*) from publications").fetchone())'
(1,)
```

The transcript above came from these binaries with a local Chat Completions stub standing in for
the model endpoint, which always calls `publish` with `{"text":"hello"}`. It was not run against
a hosted provider.

## What the approval authorizes

A decision names `(execution_id, tool_call_id, request_seq)`. The request at `request_seq`
follows the call's `TOOL_CALL` record in the session's hash chain, and that record's
`content_hash` covers the call ID, tool, mediation, idempotency key and the RFC 8785 canonical
arguments. So the approval binds the exact journaled call:

- **Another call cannot use it.** `Approve` accepts only the pending request of the latest
  execution; any other combination of execution, call and request is `FAILED_PRECONDITION`, and
  an exact retry of an earlier decision returns that decision's record and authorizes nothing new.
  A new turn cannot start while a request is pending.
- **Changed arguments are refused.** Resume verifies the hash chain before it runs the harness, so
  arguments edited in storage fail with `DATA_LOSS`. A harness that re-emits the call with other
  arguments diverges from the journal (`controller.ErrReplayDiverged`) and Resume fails. Right
  before the executor is called, the daemon recomputes the `TOOL_CALL` record's `content_hash` over
  the call it is about to send and refuses a mismatch (`DATA_LOSS`). None of these refusals dials
  the executor or writes a receipt.
- **The executor gets the journaled call**, not a new one: the original session UID, call ID,
  arguments and idempotency key.

`TestApprovalGatedModelToolOnStockDaemon` (`internal/agentsessionsd`) runs this flow on the
daemon's own `Run` with the documented Python executor and a stub model endpoint: it restarts the
daemon and harness while parked and after the decision, checks one publication, tries every mix
of two calls' identities, and edits a pending call's arguments in the journal.

## Limits

- The chain is unkeyed. Someone who can rewrite the journal and recompute every hash can change a
  call and its decision together, and neither check below detects it; that needs signing. See
  [security](security.md).
- `APPROVAL_RESULT` carries no separate arguments digest, by design for now. The decision binds the
  arguments through the chain (`request_seq` follows the `TOOL_CALL` record whose `content_hash`
  covers the canonical arguments) plus the hash recheck right before dispatch, so an exported
  decision is meaningful only together with its journal. An explicit digest is left to the journal
  export and verifier work and to executor-held credentials, where a decision is checked away from
  its journal.
- The daemon, not the executor, refuses changed arguments: the executor checks nothing itself and
  is never dialed for a mismatched call.
- The harness chooses mediation. The reference tool harness always requests approval; a harness
  you run yourself could emit `CONTROLLER_MEDIATED` and skip the gate for a configured route.
- The daemon is unauthenticated: anyone who reaches its port can approve. Run it on loopback or a
  trusted network only.
