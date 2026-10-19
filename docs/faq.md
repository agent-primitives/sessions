# FAQ

Common questions about what `agentsessions` is, what it is not, and how it behaves. See
[`concepts.md`](concepts.md) for the model and [`architecture.md`](architecture.md) for the
implementation.

## What is `agentsessions` in one sentence?

A neutral, open contract and working reference implementation for durable agent sessions: create, run,
suspend, resume, fork, and replay an agent session, with a bring-your-own-harness SPI on top and a
pluggable compute backend underneath.

## How is this different from LangGraph, Temporal, or Durable Task?

It is a different layer, not a competitor. Those are workflow or durable-execution frameworks you build
an application inside. `agentsessions` is the neutral session contract that sits between a producer (what
starts and drives sessions) and a compute backend (what runs them). It defines the wire form and the
determinism guarantees, and it composes with durable engines: a Durable Task or Dapr backend can sit
underneath as an event-log or state store. You do not rewrite your agent to adopt it; you implement a
thin harness adapter.

## What does it compose with?

`agentsessions` defines the session contract and composes with existing OSS at its seams:

- **Tools:** MCP — `ToolCall` mirrors an MCP tool call
- **Agent-to-agent:** A2A
- **Event envelope:** CloudEvents — `Origin` = `source` / `subject`
- **Durable engine:** Durable Task / Dapr — event-log / state backends
- **Telemetry:** OpenTelemetry GenAI
- **Compute backends:** agent-substrate, agent-sandbox, Kata, Cloud Hypervisor, pods
- **Harnesses:** any agent framework via the `Harness` SPI, or a custom agent

## Do I need Kubernetes?

No. The default backend (`runtime/local`) is filesystem-only and runs entirely on your machine, which is
what the [quickstart](quickstart.md) uses. Kubernetes and agent-substrate are one compute backend among
several (pod, Kata, Cloud Hypervisor, agent-substrate). Compute is pluggable through the `Runtime` SPI,
and the core knows about none of these specifically.

## Does Session.model select the model?

For new sessions, an explicit embedder-configured host policy makes it a validated default: empty
uses the selected route's explicit default; other values must be allowed. Creation on an
unconfigured route does not bypass policy on a later restricted override or route reconfiguration. The effective value is delivered in `Start.Model`
and recorded in `ExecutionStart.model`. Per-call overrides are allowed within that set. An invalid
live request fails the execution even if caught, calls no provider, records no `MODEL_CALL`, and
refuses later effects. Recorded replay/recovery prefixes keep their original default and hashes.
An unconfigured selected route and genuine pre-policy sessions/journals retain existing behavior;
legacy sessions are not inferred from creation on an unconfigured route.
The daemon does not yet expose binding policy configuration. See
[model defaults](concepts.md#model-defaults) and [embedder setup](harness-authoring.md#host-owned-model-policy).

## Why replay instead of always snapshotting memory?

Because replay runs anywhere, including a plain pod with no snapshot support, and costs nothing special.
A session's truth is its durable log, so a killed session is reconstructed byte-for-byte by replaying
that log, invoking the model zero times. Memory snapshot is the opt-in tier for harnesses whose state
genuinely cannot be replayed (a live REPL, a browser, a warm process). Those declare
`REQUIRES_MEMORY_SNAPSHOT` and are scheduled only on memory-capable compute. You pay for the heavier
capability only when you actually need it.

## What is a fence, and why does it exist?

A fence is a monotonic token bound to a live incarnation (a running sandbox). The event log is the fence
authority: a new incarnation mints a higher fence, and the log rejects any append carrying a lower fence.
This is how a session with automatic failover still has exactly one writer. If a supposedly-dead
incarnation wakes up and tries to write (a zombie), its stale fence is refused. Fencing plus compare-and-
swap on append is what makes "single writer" enforceable rather than a hope.

## Can Exec, Fork, Delete, Suspend, and Resume overlap?

Across all Placers in one Registry, Exec, Suspend and Resume refuse conflicting operations for one
session with gRPC `ABORTED`, without changing compute or the journal. Suspend will not interrupt a
running turn. Delete also refuses overlap with those operations or a fork's fan-out and rollback.
Fork holds shared deletion admission instead of the exclusive turn guard: a stateful fork can
supersede a running turn, and its checkpoint mark remains held through the last child clone.
Retry refused calls after the holder finishes, using the current cursor for Exec. Other sessions
remain independent. A standalone Placer has its own guard; separate Registries or hosts are not
fenced by this local guard.

## Does deleting a session stop its actor or erase its history?

No. `DeleteSession` permanently tombstones the UID and returns `PENDING`: durable intent only, not
resource release. Until #88 defines ownership and cleanup, actors, snapshots and fork-child pins
remain retained indefinitely. Delete does not call Stop or report cleanup completion. Get and raw
Replay still expose the UID, metadata and journal, retained forever; List hides it. There is no
cascade, erasure or UID reuse. Existing children and grandchildren remain usable and can fork
themselves.

Exec, historical Fork, Resume, Suspend and Approve (including exact retries) on the tombstone
return `FAILED_PRECONDITION`. Repeated Delete preserves the original request timestamp and audit
chain; Get/delete CLI show the request time. Unknown UID is `NOT_FOUND`; empty UID is
`INVALID_ARGUMENT`. Local busy is `ABORTED`, without interrupting the holder. The Registry guard is
local only: another host can provision before its first fence. See
[deletion semantics](concepts.md#permanent-soft-deletion), including migration and downgrade limits.
`Cancel` remains unimplemented.

## Is the provenance chain Go-specific?

No. The `content_hash` is RFC 8785 JCS (JSON Canonicalization Scheme) computed over the proto3-JSON form
of each event, not over any language's serializer. A roughly 30-line Python verifier
(`hack/verify_chain.py`) reproduces the exact Go hash and audits a full journal, and `hack/verify_chain.sh`
demonstrates it end to end, including rejecting a tampered record. Any implementation or auditor can
verify the chain.

## What model providers are supported?

It is model-agnostic. The harness selects a model by id and the host mediates the call; the core has no
provider-specific code. Opaque provider reasoning parts are carried in one neutral `ReasoningPart` type
and replayed verbatim, which was validated across Anthropic, OpenAI, and Gemini in the reasoning-
continuity work. Because the harness routes model calls through the sink and not a provider SDK, swapping
providers does not change the replay path. The bundled protocol clients are Chat Completions
(`openai.New`) and explicit Responses (`openai.NewResponses`); see the [Responses mapping and
limits](responses-adapter.md). The daemon/CLI still select Chat Completions.

## Can I bring my existing agent framework?

Yes. Implement the `api.Harness` SPI (two methods, `Describe` and `Run`) as a thin adapter over your
framework. A Microsoft Agent Framework agent, a GitHub Copilot agent, a LangChain loop, or a custom agent
all plug in the same way. See [`harness-authoring.md`](harness-authoring.md).

## What happens if the harness crashes in the middle of a turn?

Use `Resume` to re-drive an interrupted execution. For an existing current approval-bearing execution,
no-input `Exec` also continues that recorded invocation, ignoring request config/harness/cursor
overrides. Other inputless Exec calls keep ordinary new-turn behavior. Host-executed tools carry an
idempotency key; a durable executor must deduplicate on session UID plus that key and retrieve the
original receipt after a crash. The journal alone does not make external effects exactly once.

## What is fork actually for?

Fork makes a session a branch point. Four patterns the implementation supports today:

- **Parallel exploration from identical state.** `Fork(count: N)` takes one checkpoint and clones N
  children from it, so a fleet of agents branches from exactly the same base instead of N separate
  re-derivations that can drift. Run several strategies against one task, keep the best, discard the
  rest.
- **Reuse a warmed base session.** A session that has already installed dependencies, cloned a repo, or
  built an index can act as the base for many later jobs, paying that setup once rather than per job.
  Fork skips re-deriving the state; it does not make copying it free, and where that beats a cold boot
  depends on setup cost versus snapshot size, which is not yet measured.
- **Retry from before a bad turn.** For a `STATELESS_REPLAY` harness `--at N` may name any historical
  seq, so a session that went wrong at turn 12 can branch at turn 11 and take a different path without
  discarding the work before it.
- **Auditable A/B.** Each branch is its own hash-chained log carrying a `FORK` marker that names its
  parent and seq. Two branches differing only in a prompt or a model stay independently verifiable, so
  a result traces back to the exact branch that produced it.

The cost is real: no runtime forks copy-on-write today, so an N-way fan-out is N restores rather than N
references to one image.

## What are fork semantics?

`fork --at N` creates a child session that inherits the parent's log up to seq `N`, records a `FORK`
lifecycle marker, and then evolves independently. Because each session is a hash-chained log, a fork is
a branch in a verifiable hash tree.

What happens to compute depends on the harness's declared resumability:

- **`STATELESS_REPLAY`** — the child gets a fresh incarnation and the host replays the inherited prefix
  into it. The parent is untouched, and `--at N` may name any historical seq.
- **`REQUIRES_MEMORY_SNAPSHOT`** — the child's RAM has to come from somewhere, so the parent is first
  checkpointed: it is suspended, its worker is freed, and a `SUSPEND` event is appended to its chain.
  Each child is then cloned from that snapshot. The parent is recoverable with `resume`, but it is *not*
  untouched. A memory snapshot captures RAM as of now, so forking at a historical seq is refused.

A child inherits the parent's project, harness and model, and records `parent_uid` and `fork_seq` so
its lineage survives beyond the fork response. It does not inherit the parent's name: pass
`--names` (`ForkRequest.child_names`) to label children, or leave them unnamed and render them by
uid. See [concepts](concepts.md#metadata-and-listing) for why the server does not invent one.

No runtime forks copy-on-write today. On substrate each child is a full snapshot restore, so an N-way
fan-out costs N restores rather than one shared image — see
[substrate conformance](substrate-conformance.md#fork-cloning-an-actor-from-a-durable-snapshot).

## How do tool calls work, and can I gate sensitive ones?

Each tool declares a mediation tier. `IN_HARNESS_REPORTED` (the default) runs the tool in the sandbox and
reports the result for audit. `CONTROLLER_MEDIATED` has the host execute the tool so it can apply authz,
policy, and audit centrally in a custom executor. `REQUIRES_APPROVAL` parks a stateless-replay execution
for an explicit caller decision. RPC/SDK `Approve` commits only that decision; call `Resume` separately.
CLI approve and deny do both, and say "decision committed" if recovery fails. Get/List and final paused
Exec frames expose the pending tuple at their captured cursor. A decision clears that reference, but
new input stays blocked until its receipt commits. Identity is not authorization; there is no policy
hook, timeout or expiry. Snapshots and unresolved requests are retained indefinitely, and Cancel
remains `UNIMPLEMENTED`. DeleteSession commits permanent intent without reclaiming resources.
Tool calls align with the MCP shape (name plus structured arguments). The bundled Chat Completions
adapter maps function tools, tool choices, assistant tool calls and text tool results; see the
[harness tool loop](harness-authoring.md#rule-6-carry-the-tool-loop-in-the-model-request).
The stock daemon can route host-mediated tools for static CLI harnesses to
[configured Unix-socket executors](tool-executors.md); without configuration, it has no host executor.
A harness must still implement the tool loop and use a suitably configured executor for host effects.
Responses tools require an explicitly composed Go host; see the
[Responses mapping and limits](responses-adapter.md).
This is not a complete MCP integration. See
[approval recovery](interaction-model.md#approval-decisions-and-recovery).

## How do I list sessions, and can I trust the compute state I get back?

`ListSessions` reads a metadata row written at create time, not the event log, which is why a session
with no turns and no compute still appears and why a listing is complete after a process restart. It
filters on an exact `project` and pages with a cursor over `(create time, uid)`, so a session created
while you are walking pages neither skips nor duplicates a row.

`last_seq` is read straight from the log and is exact. `compute_state` is not a live probe: it records
what the log implies about compute. Any ordinary event moves a session to `LIVE`, since something had
to be running to produce it; `SUSPEND` and `FORK` land it `COLD`. So `LIVE` goes stale if the process
behind it later died, and a session that was created but never run stays `NONE`. Treat it as recorded
intent. Making it exact needs the backend to enumerate its own incarnations, which the `Runtime` SPI
does not yet expose.

## Is it production-ready?

It is a working reference implementation (Go 1.26), not yet a productized service. The Sessions API, the
single-writer event-sourced controller, the durable hash-chained log, bring-your-own-harness over
`Harness.Connect`, the `Runtime` SPI with local and substrate backends, the replay-conformance suite, and
real-substrate conformance across both capability tiers all run today. Session-level suspend and resume
use the `Placer` for both resumability tiers. See the
[Substrate verification boundary](substrate-conformance.md#verification-boundary) for live-test coverage
and limitations. The reference daemon has no authentication, authorization, or transport security;
custom Go hosts can opt into [caller authentication](security.md#optional-go-caller-authentication),
not isolation. There is no managed control plane, enterprise identity and provenance, or confidential
and GPU snapshots. See the Status
section of the root README.

## Where is the wire format?

In `api/*.proto` (Sessions, Harness, Runtime, and the shared Event). The hand-written Go SPI in
`api/*.go` is what hosts and backends code against. One neutral wire form, one Go SPI.

## How do I run it on real agent-substrate?

See [`substrate-conformance.md`](substrate-conformance.md). It runs in CI across both capability tiers
(stateless-replay and memory-snapshot continuity, both on a micro-VM), against a real `ate-system` in
kind, with the core importing zero substrate code. On a host without KVM, both tiers can run on gVisor
instead.
