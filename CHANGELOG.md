# Changelog

Release notes are generated from commits and published on the
[releases page](https://github.com/agent-primitives/sessions/releases). This file records what a
reader needs to know beyond a commit list: what a release means for compatibility, and what it does
not provide.

## Unreleased

### Added

- A self-contained [durable Substrate kind example](examples/substrate-kind/README.md), using the
  existing daemon and stock chat harness, declarative templates, persistent SQLite, optional Ollama,
  and a pod-restart smoke.
- `agentctl verify --session UID` verifies a session's committed hash chain locally from the Sessions
  API, including protobuf field presence. It refuses missing records, changed content, and broken links.
- Durable allocation intent and session-scoped handle acknowledgements for SQLite-backed Create and
  Restore, plus optional non-destructive release contracts for local and remote runtimes. No reaper
  runs yet; Fork retains its existing allocation and rollback behavior.
- `controller.ErrMissingModel`: a live model call on a controller with neither a `ModelFunc` nor
  `WithStreamingModel` returns it (match with `errors.Is`) instead of dereferencing a nil model.
  Replay and streaming-only controllers still accept a nil model.
- `HarnessRegistry.CreateHarness`: atomic create-only registration (spec `REG-3a`), needing the
  `registry.register` action. A create that timed out may have committed: inspect with
  `GetHarness` rather than retry. New in Go: `sqlitelog.Store.CreateHarness`,
  `sqlitelog.Store.HarnessNameReserved` and `sqlitelog.ErrHarnessExists`.
- Optional pre-dispatch tool policy hook, `controller.WithToolPolicy` and `placement.WithToolPolicy`
  (new), for custom Go hosts. It runs right before the executor for ordinary host-executed calls,
  live or re-driven by Resume, and for approved `REQUIRES_APPROVAL` calls on recovery, and returns
  allow, a transient refusal or a definitive refusal. A call whose journaled arguments differ from
  what the executor receives (an integer with more than 53 significant bits, such as 2^53+1, in any
  spelling; a `json.Number` such as `0.30000000000000001` that the journal records as 0.3; a
  `float32` such as 1e20 whose JSON text reads as a different float64; a string that is not valid
  UTF-8) is refused transiently without asking the hook, as is an argument the guard
  cannot compare: a Go struct that `encoding/json` writes field by field, or a value or non-string
  map key whose type has a `MarshalJSON` or `MarshalText` method (`time.Time`, `big.Int`,
  `json.RawMessage`). A transient refusal runs nothing and writes no receipt: an approved call's
  Resume fails with `FAILED_PRECONDITION` (`controller.ErrToolPolicyRefused`, new) and its
  decision stays recoverable. A definitive refusal records a `POLICY_DENIED` receipt (new
  `ToolResult.Code`), which recovery and replay serve without asking again. Only the hook writes
  the code: an executor result carrying it is rejected with
  `controller.ErrApprovalReceiptReport` and no receipt, on ordinary and approved calls; an approved
  call's Resume fails with `FAILED_PRECONDITION` and its decision stays recoverable. Without a hook,
  nothing changes. The stock daemon installs none. See
  [pre-dispatch tool policy](docs/security.md#pre-dispatch-tool-policy) and spec §9.4, now version
  `0.4.0-draft`.

### Fixed

- The durable Substrate kind example has its own live CI cluster, exercising fresh setup,
  unchanged reruns, restart/persistence smoke and teardown through its public scripts instead
  of borrowing the conformance cluster. Both live jobs gate `substrate-conformance`.
- The Substrate kind example bootstraps and pins a new managed checkout before inspecting clusters,
  so a first run does not require kind on `PATH`. Relative kubeconfig overrides resolve
  consistently across setup commands; stale installations still refuse checkout changes.
  Failed or interrupted bootstrap retains an explicit marker, preventing setup or teardown
  from running an unpinned checkout on retry, including through equivalent checkout paths.
- The Substrate kind example refuses leftover model credentials in Ollama/stub modes and
  validates model routes before cluster operations, preserving a working deployment when a
  rerun supplies invalid provider settings.
- SQLite-backed `Replay` preserves the stored protobuf event's field presence, so audit clients
  can detect presence-only tampering instead of verifying a normalized event. Stored bytes,
  hashes, and journal/wire schemas are unchanged; no journal migration is required.
- `agentctl verify` skips `Replay` for a captured empty prefix, avoiding false failures when a
  concurrent turn appends after the session head was read.
- Daemon startup logs identify the model without copying its provider endpoint, userinfo, or query.

### Compatibility

- `CreateHarness` is a new RPC, not a field on `RegisterHarnessRequest`, so a server that predates
  it refuses with `UNIMPLEMENTED` and changes nothing, rather than ignoring a create-only flag and
  reactivating the name. `RegisterHarness` is unchanged. The generated Go `HarnessRegistryClient`
  and `HarnessRegistryServer` interfaces gain the method: a hand-written client implementation must
  add it; servers embedding `UnimplementedHarnessRegistryServer` compile unchanged. No journal
  schema change.
- `POLICY_DENIED` adds no SQLite schema version: the schema stays at v7 and the spec moves to
  `0.4.0-draft`. Only a host whose policy hook refuses a call definitively records the code. An
  older build that reads v7 still opens the database and serves every session without the code. For
  a session holding a `POLICY_DENIED` receipt, hash-chain verification fails (`DATA_LOSS`), so its
  Exec, Resume, VerifyJournal and `controller.Replay` refuse the session; its `Sessions.Replay` RPC
  does not verify and streams the record unverified, with the code as the unknown number 3. An older
  harness rejects the receipt instead of reading it. v0.3.0 already refuses this release's SQLite
  journal at open. Upgrade every host sharing a journal and every harness before installing such a
  hook; after one is recorded, rolling a host back leaves that session unusable to it except through
  raw Replay.

- The SQLite journal gains the allocation ledger and inventory completeness/evidence columns.
  Stop all old writers and back up the database and WAL before migration. There is no downgrade to
  v0.3.0 or older; an already-open old writer is not fenced by the schema stamp. Every inventory,
  including fresh rows, stays incomplete until explicit operator reconciliation. See
  [allocation ownership](docs/allocation-ownership.md).
- Custom runtimes must return a nonempty `Incarnation.ID` from SQLite-tracked Create/Restore.
  An empty ID now refuses placement after the runtime call and before dialing, retaining an
  unresolved attempt. This handle is not compute-instance identity; non-recording logs are unchanged.

- A stateful substrate `Fork` now refuses a clone whose `ResumeActor` leaves `NoCapture` false but
  shows no capture of its own with `FAILED_PRECONDITION` (`api.ErrMemoryStateLost`), where v0.3.0
  returned `ABORTED` (`substrate.ErrActorBusy`): a retry is refused the same way until the client
  reports `NoCapture`, so the refusal no longer reads as retryable (`WARN` log `capture` `unknown`,
  `error_kind` `memory_state_lost`). A clone holds a capture of its own, and stays `ABORTED`
  (`capture` `held`, `error_kind` `actor_busy`) with a retried Fork cloning a new child, only when
  `CreateActorFromTag` reported a `Snapshot` and the resume reports `SnapshotNotReady` or another,
  non-empty `Snapshot`. A resume that reports neither a `Snapshot` nor `SnapshotNotReady`, which
  v0.3.0 counted as held, is `unknown`. Either way the clone is torn down and the parent is
  unaffected. See
  [a refused capture stays refused](docs/substrate-conformance.md#substrate-v050-a-refused-capture-stays-refused).

## v0.3.0 - 2026-10-22

### Compatibility

v0.3.0 migrates the SQLite journal to schema v6 with no downgrade path, adds opt-in caller
verification and RPC authorization that stay off by default, and refuses several operations that
used to run or returned `INTERNAL`. To upgrade from v0.2.0:

1. Stop every v0.2.0 process that shares a journal and back the journal up before v0.3.0 opens it
   ([Journal schema v6](#journal-schema-v6)).
2. Upgrade every host sharing a journal before any of them records the new journal fields, and
   audit journals with this release's verifier ([Journal record fields](#journal-record-fields)).
3. Declare durable dedup for each tool whose interrupted call `Resume` must re-drive, and expect
   `DATA_LOSS` for a journal that fails hash-chain verification
   ([Sessions API refusals](#sessions-api-refusals)).
4. Authentication and authorization are opt-in Go APIs; defaults are unchanged and the reference
   daemon enforces neither ([Authentication and authorization](#authentication-and-authorization)).
5. Expect `model/openai` to refuse requests and completions it used to pass through, and Chat
   Completions status errors without provider text ([model/openai changes](#modelopenai-changes)).
6. On substrate, expect new `FAILED_PRECONDITION`, `UNAVAILABLE` and `ABORTED` refusals, Suspend
   a stateless session again before Resume when its actor's checkpoint changed outside Sessions,
   and update custom `ControlClient`s ([Substrate backend changes](#substrate-backend-changes)).
7. The substrate adapter now pins agent-substrate v0.5.0: reinstall Substrate with fresh state
   rather than upgrading it in place, deploy the adapter from this release with it, and label the
   namespace that runs `agentsessionsd` `ate.dev/api-access=true`. A memory-tier session is refused
   with `api.ErrMemoryStateLost`, and stays refused, once its actor is resumed on another sandbox
   runtime or holds a capture Sessions did not record, such as a suspend or pause by any other
   caller. Do not let a gateway or operator suspend a memory-tier session's actor
   ([Substrate v0.5.0](#substrate-v050)).

This release has two tags, cut on the same day. `v0.3.0` tags the root module. A one-line PR then
bumps the `integrations/substrate` module's require of `github.com/agent-primitives/sessions` from
`v0.0.0` to `v0.3.0`, and `integrations/substrate/v0.3.0` tags the adapter module at that commit.
The bump is needed because Go ignores the module's `replace` of the root module with `../..` when
another module depends on it. The adapter builds against agent-substrate v0.5.0 (`e8d40c33`).
Release archives and images carry the neutral `agentsessionsd`, as in v0.2.0. The substrate build
(`integrations/substrate/cmd/agentsessionsd`) is not published as a release binary or image.

#### Journal schema v6

- <a name="sqlite-v6-rollout"></a>
  The SQLite journal schema moves from version 4, which v0.2.0 writes, to version 6. The first open
  by a v0.3.0 binary, `agentsessionsd` or any `agentctl` command on an embedded journal, migrates
  the database in place in one transaction, as in the [v4 rollout](#sqlite-v4-rollout). Existing
  journal bytes, hashes and the `ModelCall` format are unchanged.
  - Version 5 adds indexed exact caller issuer/subject ownership and a journal authorization
    marker, written as `off`. Existing sessions and unenforced RPC-created sessions stay unbound;
    ordinary metadata writes and copied history cannot assign or replace an owner. This build
    refuses a journal whose marker is required, unknown or missing, because it cannot enforce
    caller access.
  - Version 6 adds a per-session model-policy flag. Existing sessions, including empty ones, stay
    legacy; Fork inherits the flag atomically with the child.
  - v0.2.0 and older builds refuse the migrated database with "unsupported database schema", and
    only when they open it. An older process that already has the journal open keeps writing
    without enforcing model policy or caller ownership.
  - Rollout: stop every older host and embedded `agentctl` process sharing the journal, back up the
    database (with its `-wal` file, if present), then open it with v0.3.0, and upgrade all writers
    before restarting them. There is no downgrade migration: rollback means restoring that backup
    and losing what was written since.

#### Journal record fields

- Three new fields are part of a record's canonical bytes when set. The hash algorithm is
  unchanged, and an empty field is omitted, so records that leave them unset hash as before (new
  golden vectors in `canon` and `hack/verify_chain.py`):
  - `EXECUTION_START.compute_instance`: the actor UID of a memory-tier substrate turn.
  - `Lifecycle.compute_instance` on a `FORK` marker: the actor a memory-tier fork child was cloned
    onto.
  - `EXECUTION_START.model`: the host-resolved default of a model-policy turn, delivered to the
    harness as the new `Start.model`.
- A reader built from older protos drops these fields and fails verification with a
  `content_hash` mismatch on those records. v0.2.0 binaries cannot open a v6 journal, but an
  external auditor can still hold old protos: audit with this release's
  `hack/verify_chain.py --journal`, which builds its descriptor from the checked-out protos. Every
  host sharing a journal must run v0.3.0 before any of them runs a memory-tier substrate turn,
  forks one, or runs a model-policy turn; rollback is not supported for sessions that recorded
  these fields.

#### Sessions API refusals

- A journal that fails hash-chain verification is refused where it used to run: `Exec` and
  `Resume` (including input-less `Exec` recovery) are `DATA_LOSS` before the harness runs, a model
  or tool is called, or anything is appended to the session. `Fork` checks only the records it
  copies (seq <= `at_seq`) and is `DATA_LOSS` when one fails, before anything is appended to a
  child, a memory-tier parent is checkpointed or any child compute is provisioned. `Resume` of a
  completed turn on such a journal fails rather than returning as a no-op. Find affected sessions
  with `python3 hack/verify_chain.py --journal <db>`; the check writes nothing, so a rollback
  restores the old behavior. The `Replay` RPC and `Approve` do not check, and `Replay` still
  returns the stored hashes. In Go, `eventlog.ErrTampered` is new and `sqlitelog.ErrTampered` is
  the same value; `sqlitelog` `Verify` messages now begin `eventlog:`. On the in-memory store, a
  harness that changes a value the host returned in place (a recorded completion) now fails the
  session's next `Exec`, `Replay` or `Resume` with `controller.ErrJournalTampered`. Each controller
  recanonicalizes the whole journal on its first read and only new records afterwards (about 23 us
  per 1.2 KB record on an Apple M1 Pro), so a turn on a long session costs one more pass over its
  journal.
- Embedders using `controller.WithToolExecutor` or `placement.WithToolExecutor` must declare
  durable dedup to keep `Resume` re-driving an interrupted tool call, including one whose executor
  returned an error the harness did not handle. `Resume` re-runs a trailing `TOOL_CALL` that has
  no `TOOL_RESULT` and is not approval-gated only for tools declared by name with the new
  `controller.WithDurableDedup` or `placement.WithDurableDedup`. For any other tool it returns
  `controller.ErrUnansweredToolCall` before running the harness, without calling the executor or
  appending to the journal, and `Sessions.Resume` reports `FAILED_PRECONDITION`; it used to re-run
  the call. Declare only tools whose executor deduplicates on (session UID, idempotency key) or
  that are safe to repeat. A refused turn can be resumed once its tool is declared, or superseded
  by a new `Exec`. First attempts, recorded results, the inherited-intent refusal after `Fork`,
  approved `REQUIRES_APPROVAL` calls and hosts without a tool executor are unchanged.
  `agentsessionsd -tool-executors` declares its `durable_dedup: true` routes. Journal, wire and
  hash formats are unchanged; a rollback restores the unconditional re-drive.
- An `ExecRequest.harness` override cannot involve a `REQUIRES_MEMORY_SNAPSHOT` harness: when
  either the session's stored harness or the override requires memory snapshots, `Exec` is
  `FAILED_PRECONDITION` (`placement.ErrStatefulOverride`) before any compute is provisioned, fence
  minted or journal written, because Suspend and Fork follow the session's own harness and would
  neither checkpoint nor clone the override's RAM. `Resume` and input-less `Exec` recovery also
  refuse an older interrupted override before compute changes or journal repair; supersede it with
  a new `Exec` on the stored harness. An override onto a name that aliases the session's own
  registry entry still runs, and stateless-to-stateless overrides are unchanged.

#### Authentication and authorization

- Caller verification (`authn`) and Sessions and registry RPC authorization are opt-in Go APIs for
  embedders. Defaults are unchanged: the Sessions service and the reference daemon remain
  unauthenticated and unenforced, there is no daemon required-mode switch, and authentication
  alone is not isolation. No wire fields or canonical event format change.
- In required mode the `authn` interceptors reject unverified calls and expire authenticated
  streams. Unary authentication is admission-only; credential expiry does not end an admitted call.
- The authorization options do not activate a required-mode deployment. Journals still require an
  `off` marker, and unenforced writers sharing the journal bypass embedded checks. Enforced Fork
  refuses before IO; enforced Approve clears the request actor without claiming verified audit.
  Owned fork publication, versioned approval audit, journal policy and startup validation, daemon
  activation and TLS are not provided.
- `NewHarnessRegistry` now accepts variadic options: ordinary calls remain source-compatible, but
  a function-value assignment using its previous exact signature must wrap or adapt it.
- Enforced deletion probes take and release admission before owner SQL, so a probe can race a
  later Delete; later guards remain authoritative. A verified caller who knows a UID can receive
  `ABORTED` if exclusive deletion is present at the probe, even without ownership; this grants no
  access. Policies own their failure logs; services never log raw policy causes. Enforced-mode
  `INTERNAL` diagnostics log `request_id`, `method`, `error_kind` and a stable `error_category`,
  never model, provider or harness message text, credentials or session payloads; see the
  [diagnostic-only log exception](docs/observability.md#data-policy).

#### model/openai changes

- Chat Completions status errors, streaming and journaled ones included, no longer carry the
  provider's message or response body (see [Security](#security)). A 4xx such as a wrong model ID
  or a missing key now reads as the model and HTTP status plus bounded `error.code`/`error.param`
  identifiers, so diagnose from those or from provider-side logs. Transport, body-read and decode
  errors are unchanged.
- `model/openai` now refuses nonempty `ModelRequest.Params`, previously silently ignored. A turn
  interrupted under an older build whose harness sets Params fails if recovery must re-drive the
  model call; completed recorded model effects remain replayable without invoking the adapter.
- Unmapped parts or choices, duplicate tool names, nonempty completion refusals and malformed or
  incomplete tool calls fail closed. Chat Completions and Responses refuse a completion that calls
  an unoffered tool, violates a named REQUIRED choice, calls a tool under NONE, or returns
  text-only output under REQUIRED. Integer arguments outside ±2^53 are refused rather than
  rounded. Tool results require explicit text Content when receipt data is populated; the adapter
  does not guess text from Output or error metadata or fetch external URIs. A named REQUIRED
  choice also accepts `stop` for exactly one complete matching function call; AUTO and unnamed
  REQUIRED still require `tool_calls`.
- Streaming validates and freezes a completion after the finish-bearing frame's own delta, refusing
  later semantic deltas or contradictory finish reasons while allowing trailing usage. Text-only
  missing-finish responses and EOF without `[DONE]` remain supported. Response, stream chunk and
  argument decode errors retain only syntax or type/range diagnostics and declared field names,
  never raw provider values. The adapter does not register or execute tools or add executor
  deduplication.

#### agentsessionsd changes

- `agentsessionsd` refuses positional arguments, naming them, and exits with status 2. It used to
  ignore them, along with every flag after the first one, since flag parsing stops there.
- `integrations/substrate/cmd/agentsessionsd` (see [Added](#added)) is built from a source
  checkout of that module; neither its binary nor an image is published with this release. The
  neutral `cmd/agentsessionsd` in the release archives and images does not offer the substrate
  flags.

#### Substrate backend changes

- <a name="v030-held-checkpoint-restore"></a>
  Restore reads the actor before every resume, and a failed read refuses (`INTERNAL`) without
  resuming. A `RESUMING` or `SUSPENDING` actor is `ABORTED`, and Resume of a `RUNNING` actor still
  succeeds as a no-op. A `SUSPENDED` actor must hold exactly the checkpoint the journal recorded:
  - A recorded checkpoint the actor no longer holds is `FAILED_PRECONDITION`
    (new `api.ErrInvalidCheckpoint`), before any harness is connected, tool re-driven or record
    appended. A suspended session hits this when its actor was resumed and suspended again outside
    Sessions, or when the `SUSPEND` append failed after the snapshot: inspect the actor and Suspend
    again to record its current checkpoint before Resume. Retrying the stale ref cannot succeed. A
    memory-tier session cannot recover this way: it is refused with `api.ErrMemoryStateLost` before
    the resume, and Suspend no longer records the capture (see
    [captures Sessions did not record](#v030-unrecorded-capture)).
  - A newest snapshot still uploading is `UNAVAILABLE` (new `api.ErrCheckpointPending`) for `Exec`,
    `Suspend`, `Resume` and `Fork`; once it completes, Suspend again to record it before Resume.
    A `Suspend` refused this way records nothing, though the compute may already be released. A
    memory-tier session cannot recover this way: the completed upload is a capture the journal did
    not record and is refused as `api.ErrMemoryStateLost`. v0.5.0's `SuspendActor` returns only
    once the upload completed, so a memory-tier session reaches this only through a client or
    substrate that suspends asynchronously.
  - A newest local-only or unrecognized checkpoint, including an out-of-band paused actor, is
    `FAILED_PRECONDITION`; it may never upload, so establish a durable checkpoint rather than wait.
    For a memory-tier session it is `api.ErrMemoryStateLost`, which does not clear.

  See [restore checks](docs/substrate-conformance.md#held-checkpoint-restore-and-readiness).
- `runtime/substrate` reports `SnapshotRef.Memory` from the completed snapshot's observed fidelity
  (new `ActorInfo.SnapshotFidelity`) instead of always true. ROOTFS and VOLUMES report false;
  missing or unrecognized fidelity is accepted as false for default stateless replay, which is not
  proof of files-only capture. Invalid checkpoints wrap `api.ErrInvalidCheckpoint` and are
  `FAILED_PRECONDITION` from Suspend and Fork instead of `INTERNAL` (stateful Fork keeps
  `placement.ErrUnplaceable`). A memory-required descriptor or recorded compute instance requires
  observed MEMORY and a durable URI; `WithVolumesOnly()` requires observed VOLUMES and a durable URI
  and disables memory-snapshot admission. A refusal after capture leaves the actor suspended but
  records no `SUSPEND`, and Create and Restore check that the suspended actor's checkpoint has a
  durable URI and the required fidelity before resuming it. Restore decides whether MEMORY is
  required from the backend descriptor, not the legacy `SnapshotRef.Memory` bit, so stateless
  checkpoints from older journals, which recorded `Memory: true`, still resume. See
  [snapshot fidelity](docs/substrate-conformance.md#snapshot-fidelity-and-files-only-recovery).
- Checkpoint refusals, new ones included, read `<op>: session "<uid>": <runtime error>`, where
  `<op>` is `exec`, `suspend`, `resume` or `fork` (Fork names the parent). This covers each
  `FAILED_PRECONDITION` for an invalid, stale or local-only checkpoint, and the `UNAVAILABLE`
  pending-upload refusals share the prefix. A refused observed checkpoint's runtime error names the
  actor (`atespace/name`) and the required and observed fidelity, never the snapshot URI. Codes are
  as listed above; update tools that match these messages.
- Memory-tier compute that is gone or replaced is refused instead of silently losing RAM state:
  - `Exec` of a `REQUIRES_MEMORY_SNAPSHOT` session with a started turn on that harness but no actor
    (deleted out of band, or a daemon restarted with another `-substrate-atespace`) is
    `FAILED_PRECONDITION` (new `api.ErrMemoryStateLost`) and creates nothing; it used to
    cold-create an actor. `Resume` of any memory-tier session whose actor is gone is
    `FAILED_PRECONDITION` too, where it was `INTERNAL`.
  - `Exec`, `Resume`, `Suspend` and stateful `Fork` on an actor whose UID differs from the one the
    journal recorded (`EXECUTION_START.compute_instance`, or a fork child's `FORK` marker before its
    first turn) are `FAILED_PRECONDITION` and journal, suspend and clone nothing. `Suspend` of a
    replaced actor used to succeed. Turns and fork markers recorded before this release carry no
    UID and are not compared.
  - A fork whose clone is replaced before its resume is `ABORTED` (`substrate.ErrActorBusy`) and
    records no child; the replacement is left running. A stateful `Fork` whose parent actor is
    deleted around the tag is `FAILED_PRECONDITION`.
  - The started-turn count is per harness, so hosts sharing a journal and an atespace must serve
    the same one substrate harness; see
    [Substrate placement binding](docs/substrate-conformance.md#substrate-placement-binding).
- An actor another operation holds is retryable: `Exec`, `Resume`, `Suspend` and `Fork` are
  `ABORTED` (new `api.ErrComputeBusy`) where they were `INTERNAL` when the actor is `RESUMING` or
  `SUSPENDING` (a pause in progress included), or when substrate refuses a resume or suspend because
  another operation holds its lease. This is the usual outcome for the host that loses a race for a
  session's first turn. A fork tag refused because another operation holds the parent is
  `ABORTED` too. An `Exec` whose `SUSPENDED` actor is replaced before its resume, or deleted again
  after `CreateActor` found the name taken, is `ABORTED`, and one whose actor is deleted before its
  resume is treated as a missing actor; both were `INTERNAL`. When `CreateActor` finds the name
  taken, `Create` attaches to that actor where it used to fail the turn with `INTERNAL`.
- Custom `ControlClient` implementations must change: `SuspendActor` and
  `SnapshotCloner.CreateActorFromTag` return the actor's `substrate.ActorInfo`; `GetActor`,
  `ResumeActor`, `SuspendActor` and `CreateActorFromTag` report the actor UID for memory-tier
  actors; `GetActor` and `SuspendActor` report a missing actor as `substrate.ErrActorNotFound`
  (and should from `ResumeActor`); `CreateActor` reports a taken name as
  `substrate.ErrActorExists`; and `GetActor` and `SuspendActor` report observed fidelity.
  `GetActor` should report an actor that is `RESUMING` or `SUSPENDING` (a pause in progress
  included) as the new `substrate.StatusTransitioning`, which the backend refuses as `ABORTED`.
  `ActorInfo.UID`, `SnapshotFidelity`, `NoSnapshotHistory`, `SnapshotNotReady`, `SnapshotPending`,
  `MemoryNotRestored`, `CaptureLineage` and `NoCapture` are new. A client that leaves
  `MemoryNotRestored` false keeps the old behavior on a runtime fallback, but one that leaves
  `CaptureLineage` empty can no longer suspend or fork a memory-tier session, and one that leaves
  `NoCapture` false refuses every memory-tier session with no `SUSPEND` recorded once its actor
  exists. `ResumeActor` must now report `Snapshot` and `NoCapture` as `GetActor` does: a client that
  does not refuses every memory-tier `Restore` and `Create`, and every stateful `Fork` (`ABORTED`,
  which a retry does not fix). The `integrations/substrate` adapter does all of this, including
  mapping substrate's lease refusal to `substrate.ErrActorBusy`; a client that returns a raw
  transport status still gets `INTERNAL`. New SPI pieces: `api.SessionSpec.StartedTurns`,
  `api.SessionSpec.RecordedCheckpoint`, `api.Incarnation.ComputeInstance`,
  `api.Incarnation.RecordedCheckpoint`, `api.Lifecycle.ComputeInstance`, the optional
  `api.InstanceRestorer`, and `controller.ForkWithComputeInstance`. Through `RecordedCheckpoint`
  the Placer passes the checkpoint the journal last recorded; other backends may ignore it.
  A `Placer` outside a `placement.Registry` counts every started turn and compares every recorded
  UID, whatever harness name the turn recorded.
- Known limits: these are client-side checks, not atomic lifecycle preconditions, and they race
  lifecycle operations and direct router traffic. Substrate can resume from a source other than
  the durable checkpoint validated here (an actor-local checkpoint, or VOLUMES scope after its
  `ActorTemplate` was recreated), so validation can falsely refuse recoverable state or accept a
  fidelity below the backend's requirement; see
  [issue #192](https://github.com/agent-primitives/sessions/issues/192).

#### Substrate v0.5.0

- The substrate adapter (`integrations/substrate`), the AgentSuite example and the substrate e2e
  build and deploy agent-substrate v0.5.0 (`e8d40c33`), where v0.2.0 pinned `96dffb28`. Journal and
  schema are unchanged by the move. See
  [Substrate v0.5.0](docs/substrate-conformance.md#substrate-v050-paused-actors-suspend-durability-and-runtime-fallback).
- Moving ate-system from `96dffb28` to v0.5.0 is not an in-place upgrade. Substrate supports no
  upgrade between minor releases before 1.0 (its `docs/upgrade.md` says to reinstall), and v0.5.0
  turned `SandboxConfig.sandbox_class` from an enum into a string under the same field number: it
  reads an ActorTemplate stored by the old pin with an empty sandbox class, so no worker matches
  that template's actors and they cannot resume. Reinstall Substrate with fresh state and create
  templates and actors on v0.5.0; the old state's actors, and the session checkpoints held on
  them, do not carry over. Deploy an adapter built from this release with it: the client decodes
  actors by field number and the two must match. Against an older server, this adapter reads a
  `PAUSED` or `PAUSING` actor as an unknown state rather than as suspended or busy: Exec refuses
  it, Restore of a held checkpoint refuses it with `FAILED_PRECONDITION`, and Restore with no held
  checkpoint passes it to `ResumeActor`. Rolling back is likewise a reinstall of the older
  Substrate with an older adapter, not a downgrade over v0.5.0 state. An AgentSuite template
  rendered for v0.5.0 names its sandbox class as `microvm` or `gvisor`, not
  `SANDBOX_CLASS_MICROVM`, so re-render templates for the matching substrate. The AgentSuite
  example's `up.sh` and the lifecycle page's `ui.sh` refuse a kind cluster `up.sh` installed from
  another substrate commit, or before it recorded the commit; run `down.sh`, then `up.sh`.
- On v0.5.0, the namespace that runs `agentsessionsd` must carry the label
  `ate.dev/api-access=true`. v0.5.0 denies ingress to `ate-system` by default and admits
  ate-api-server only from its own components and from pods in namespaces with that label, worker
  pods (`ate.dev/worker-pool`) excluded (substrate
  `manifests/ate-install/ate-system-networkpolicies.yaml`). Without it every Control call fails
  `Unavailable` with a dial `i/o timeout`. `deploy/substrate/namespace.yaml` and the AgentSuite
  example's `deploy/namespace.yaml` carry the label; label any other namespace whose pods call the
  ate-api. The policy only takes effect where the network plugin enforces NetworkPolicy. The
  previous pin installs no such policy, so the label has no effect there. See
  [namespace access](docs/substrate-conformance.md#substrate-v050-namespace-access-to-ate-api-server).
- v0.5.0 removes the `PAUSED` and `PAUSING` actor states: a paused actor is `SUSPENDED` with a
  node-local newest snapshot, and a pause in progress is `SUSPENDING`. The adapter treats them as
  it treated `PAUSED` and `PAUSING`: Resume and Exec refuse a paused actor with
  `FAILED_PRECONDITION` until a Suspend uploads its snapshot, and a pausing one is `ABORTED`. For a
  memory-tier session, Suspend refuses a paused actor too, and the session stays refused (see
  [captures Sessions did not record](#v030-unrecorded-capture)).
  Suspend stays synchronous: v0.5.0's `SuspendActor` has no durability option and returns once the
  newest generation is `COMPLETED`. The adapter still records a checkpoint only when that
  generation is `COMPLETED`; a pending upload fails Suspend with `UNAVAILABLE`.
- v0.5.0 can resume a memory snapshot on a worker of another sandbox runtime, restoring only its
  files, when no worker matching the snapshot's recorded runtime has room. On a cluster with more
  than one sandbox runtime, a memory-tier session whose actor was resumed that way is now refused
  with `FAILED_PRECONDITION` (`api.ErrMemoryStateLost`) on Exec, Resume, Suspend and Fork rather
  than run its turns without the session's RAM, and stays refused while that actor runs. Suspend
  refuses it whether or not the journal recorded a compute instance. The first refusal per actor
  is logged at `WARN`. The actor keeps its worker until it is deleted, and `DeleteSession` does
  not stop it (#88). To release the worker, delete the substrate actor named in the warning with
  `DeleteActor` and `any_state: true`
  (`kubectl ate delete actor <actor> -a <atespace> --any-state`): the actor is still `RUNNING`,
  and v0.5.0 refuses a plain delete of it with `FAILED_PRECONDITION`. Set `options.uid` to the
  warning's `actor_uid` so a later actor created under the same name is refused with `ABORTED`
  instead of deleted. `kubectl ate` sends no UID: checking the UID with `get` first is a separate
  read and cannot stop a replacement created between the `get` and the delete. Stateless sessions
  are unaffected, and clusters whose workers share one runtime do not hit it. The adapter reports
  the fallback through `ActorInfo.MemoryNotRestored`; another `runtime/substrate.ControlClient`
  implementation that leaves it false keeps the old behavior. The refusal covers only this runtime
  fallback: the effective restore-source cases in the known limits above are not detected and stay
  open in [issue #192](https://github.com/agent-primitives/sessions/issues/192).
- <a name="v030-unrecorded-capture"></a>
  A memory-tier session is refused when its actor holds a capture Sessions did not record. v0.5.0's
  `SuspendActor` and `ResumeActor` take no precondition, and a capture the backend refuses stays on
  the suspended actor, so for a `REQUIRES_MEMORY_SNAPSHOT` harness the backend compares the actor
  with the checkpoint the session's latest `SUSPEND` names (with none recorded, the actor must hold
  no capture of its own) and refuses a mismatch with `FAILED_PRECONDITION`
  (`api.ErrMemoryStateLost`), logged at `WARN`, before resuming, attaching to or suspending it:
  - `Suspend`, and a stateful `Fork`'s capture of the parent. Both also refuse after the suspend
    when the actor was captured or paused and then resumed, resumed on another runtime, or replaced
    between the check and the suspend; the actor is left suspended and no checkpoint is recorded.
    A refused Fork tags and clones nothing. This fails closed even when the memory survived.
  - `Exec` (`Create`'s attach to an existing actor) and `Resume` (`Restore`), whether the actor is
    suspended or the router already woke it. Both check again on the actor a successful
    `ResumeActor` reports; a mismatch there leaves the actor `RUNNING` and unused.
  - `Create`, for the actor it just made: between its `CreateActor` and `ResumeActor` another
    caller can resume and capture it, so after the resume the actor must hold no capture of its
    own unless a `SUSPEND` for it is recorded by then. The actor is left running, not torn down,
    because another host can be using an actor under the session's name.

  A `SUSPENDING` or `RESUMING` actor, such as one another host's Suspend is still capturing, is
  `ABORTED` before this check. Between that host's capture and its `SUSPEND` append the refusal is
  `api.ErrMemoryStateLost`, and a retry after the append succeeds. Journal, wire and the
  Sessions API are unchanged: the check reads the `SUSPEND` refs the journal already holds. One
  window stays open: a crash and resume onto a worker of the same runtime between the check and
  the suspend is not caught, since v0.5.0 has no precondition on `SuspendActor` that would close
  it. See
  [a refused capture stays refused](docs/substrate-conformance.md#substrate-v050-a-refused-capture-stays-refused).
- A stateful `Fork` checks its clone the same way against the child's empty checkpoint: a clone
  another caller resumed and captured before Fork's resume is refused with `ABORTED`
  (`substrate.ErrActorBusy`), logged at `WARN`, and torn down, with no child written. The parent is
  unaffected and a retried Fork clones a new child. The teardown is best effort: if the teardown
  fails (for example, another caller holds the clone's lease), the refusal names the clone and its
  tag, left in place; delete that actor with `options.uid` and `any_state: true`, and the tag.
- The cost of failing closed: any capture the journal missed ends a memory-tier session for good,
  even when its memory survived. That covers a `SUSPEND` append that failed after the snapshot, a
  suspend or pause by an operator or another client (a gateway's idle suspend included), and suspend
  and resume cycles during a Suspend. Every later turn, Resume, Suspend and stateful Fork of the
  session returns `api.ErrMemoryStateLost`, and Suspend again no longer records the capture. Nothing
  in Sessions recovers the session: delete or abandon it, and delete the actor named in the warning
  with `options.uid` (and `any_state: true` if it is running) to release its snapshot or worker;
  `DeleteSession` does not touch the actor (#88). A router resume alone is harmless. This release
  does not support another caller suspending a memory-tier session's actor, so a gateway that
  suspends idle actors (as `agent-substrate/always-on-agent`'s does for its own) must not suspend
  one Sessions manages. Stateless sessions are unaffected. See
  [outside suspends](docs/substrate-conformance.md#substrate-v050-outside-suspends-and-router-auto-resume).

### Added

- An approval-gated tool runs on the stock daemon with no custom host: the reference harness
  `harness/toolagent` (served by `harnessnode` with `HARNESS_KIND=tool`) offers the model a
  `publish` tool, turns each model tool call into a `REQUIRES_APPROVAL` call keyed by execution and
  call ID, and feeds results back until the model answers. See the
  [approval-gated tool example](docs/approval-gated-tool.md).
- `agentctl exec` and `replay` print a tool call's tool, ID, mediation and arguments (not its
  idempotency key), so an approver can read the call before deciding. Each record prints on one
  line: control and format characters in message text and arguments print as escapes (`\n`,
  `\x1b`, `\u202e`), so a multi-line reply now prints as one line. `exec` escapes streamed model
  text the same way and prints it on its own line starting `stream:`.
- Go embedders can configure per-route model defaults and allowed IDs with
  `placement.WithModelPolicy`. Registration validates the descriptor against policy; new sessions
  validate their default at creation (`INVALID_ARGUMENT`) and Exec/Resume (`FAILED_PRECONDITION`).
  Live model-policy refusals are execution-fatal; replay keeps recorded defaults and results.
  Stock Chat Completions and Responses calls send the exact validated model ID. Policy-enabled
  registration requires a successful bounded Describe, including at startup; a failed stored route
  needs a restart after its endpoint is restored. Unconfigured registration remains probe-free.
  Daemon bindings and model-to-deployment mappings are not included. See
  [model defaults](spec.md#model-defaults).
- `authn.VerifiedCaller`, `CallerVerifier`, `Caller` and opt-in unary/stream `NewInterceptors`
  with off/required modes. Credential adapters, TLS assembly and authorization are not bundled.
- Verified-caller ownership: `api.CallerIdentity`, `Authorizer` and owner-only `OwnerAuthorizer`,
  plus insert-only `CreateOwnedSession` and indexed owner-filtered listings. These are trusted-host
  Go APIs, not RPC authentication or enforcement.
- Opt-in embedded Sessions and registry RPC authorization with verified callers, independent approve
  permission, exact-caller allowlists, owned creation and SQL owner-filtered pagination. Inputless
  Exec approval recovery requires resume as well as exec; ordinary Exec can restore suspended
  compute with exec alone, while Resume always requires both. Policy requests carry the originating
  RPC path in Go `AuthorizationRequest.Method`, including compound, recovery and global checks and
  direct handler calls; built-in owner policies still decide by action. Existing unbound rows reach
  custom policies with a zero Owner; `OwnerAuthorizer` still denies them.
  `session.WithRegistryLogger` routes registry diagnostics and startup warnings to an
  embedder-supplied logger, independently of the Sessions service's `WithLogger`; absent or nil, it
  uses `slog.Default()` captured at construction. See the
  [action mapping and limits](docs/security.md#optional-go-rpc-authorization).
- `model/openai` maps Chat Completions function tools and tool choices, assistant tool-call parts
  and text `ToolResult.Content`. Streaming assembles indexed argument deltas into finalized tool
  parts without emitting tool JSON through the text callback. Replay and completed-effect recovery
  preserve full parts and usage through the existing neutral request hash and completion journal.
- `model/openai.NewResponses` adds an explicit stateless Responses client with function tools,
  opaque encrypted reasoning retention, usage, and text streaming. Existing Chat Completions,
  daemon/CLI selection, neutral request hashes and journal schemas are unchanged. Unsupported
  content and controls refuse rather than being dropped. Assistant text history uses `output_text`;
  system/user text uses `input_text`. Model-call diagnostics retain categories, HTTP status and
  bounded provider `error.code`/`error.param` identifiers, not messages or transport URLs/details;
  see [mapping and limits](docs/responses-adapter.md).
- `integrations/substrate/cmd/agentsessionsd` is `agentsessionsd` with the substrate backend linked
  in. `-substrate-harness name=atespace/template[,memory]` serves a harness as one actor per session
  in `-substrate-atespace`, controlled over the ate-api (`-ateapi-addr`, TLS, with
  `-ateapi-token-file` re-read on every call, `-ateapi-ca-file` or `-ateapi-insecure-skip-verify`)
  and reached through `-atenet-router`. Each ate-api call has a fixed two-minute deadline, or the
  caller's earlier one: a hung ate-api fails the session operation instead of holding it, and so
  does a call that takes longer, such as a `SuspendActor` or `ResumeActor` waiting on a large RAM
  snapshot. It requires `-model`, refuses the other substrate flags without a
  `-substrate-harness`, and exits with status 2 on a second `-substrate-harness`, since actors are
  named by session UID in the one atespace. Both binaries run the same `internal/agentsessionsd`
  server through a neutral `WithHarnessBackends` hook, so the root module links no substrate
  package. See the
  [flags](docs/substrate-conformance.md#running-sessions-on-substrate-with-agentsessionsd).
- `examples/agentsuite-substrate` runs an AgentSuite agent as a substrate-hosted harness on kind.
- `examples/session-lifecycle-ui` is a browser page for that example's cluster: it creates a
  session, runs turns, suspends, resumes, forks and replays it through the Sessions gRPC API, and
  shows each call's path, the actor as substrate reports it, the journal and any refusal. It adds no
  Sessions API. The project site's landing page shows a recording of it on micro-VMs.

### Changed

- `agentsessionsd` logs at Warn for tool-executor routes that cannot dispatch because
  `durable_dedup` is false or unknown.
- The substrate adapter builds against agent-substrate v0.5.0 (`e8d40c33`) instead of `96dffb28`;
  see [Substrate v0.5.0](#substrate-v050).

### Fixed

- On substrate, `Resume` now refuses a suspended actor whose reported durable checkpoint URI
  differs from the one the session recorded. For a stateless session, an `Exec` after `Suspend`
  checks the checkpoint's readiness and fidelity only; a memory-tier `Exec` also compares the
  actor with the session's recorded `SUSPEND` checkpoint (see
  [captures Sessions did not record](#v030-unrecorded-capture)). `Exec`, `Suspend`, `Resume` and
  `Fork` no longer treat an older completed snapshot as current while the newest one is pending or
  local-only. These are client-side checks before the call, subject to the race and
  restore-source limits in
  [Substrate backend changes](#v030-held-checkpoint-restore) and
  [issue #192](https://github.com/agent-primitives/sessions/issues/192).
- Cancelling `Resume` while an approved `REQUIRES_APPROVAL` call is in flight no longer records a
  permanent `EXECUTOR_ERROR` receipt. When `Resume`'s context has ended and the executor's
  error is that context's error, no receipt, `ERROR` or `END` is written: the call stays pending and
  the next `Resume` runs it again under the same session UID and idempotency key, as after a crash.
  The same holds when `Resume`'s deadline has passed and the executor's error is a deadline error,
  which covers a deadline that surfaces through I/O just before the context reports it. An
  executor error while `Resume` is running, including an executor's own timeout or a harness
  cancelling the context it passed to that one `ToolCall`, is still an `EXECUTOR_ERROR` receipt.
  Receipts already journaled are unchanged.
- An approved call's `EXECUTOR_ERROR` receipt for an executor Go error now carries model-facing
  `Content` ("Tool execution failed."), like the denial receipt; the error text stays in `Error`,
  and a partial result returned with the error is dropped. Before, it had no `Content`.
- Socket executor write/read errors, including journaled approval receipts, no longer expose
  the Unix socket path.
- Tool-executor refusal receipts no longer reference an issue number.
- `sqlitelog.Open` now uses `BEGIN IMMEDIATE` for `file:` URIs and paths with a `?` query, not
  only plain paths, so concurrent writers from separate stores on one database wait for the write
  lock (up to the 5s busy timeout) instead of failing with `SQLITE_BUSY`. A non-empty `_txlock`
  in the DSN is kept; an empty one, which the driver ignores, gets `immediate`.

### Security

- The toolchain floor moves to Go 1.26.10 for the root module, which builds the release binaries
  and images, and to Go 1.27.3 for `integrations/substrate` and the AgentSuite example, fixing
  [GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809). Language versions and dependencies are
  unchanged.
- Before an approved `REQUIRES_APPROVAL` call reaches its executor, the controller recomputes the
  `content_hash` of the call's journaled `TOOL_CALL` record over the call it sends and refuses a
  mismatch with `controller.ErrJournalTampered` (`DATA_LOSS`), without calling the executor or
  writing a receipt. The executor receives a copy of the arguments. Before, an in-process harness
  on the in-memory store could change the approved arguments after the journal was verified,
  because the store kept the harness's argument map. Journals and receipts are unchanged.
- Chat Completions status errors, including streaming calls and their journaled errors, now retain
  only the model, HTTP status and bounded `error.code`/`error.param` identifiers, never provider
  messages or raw response bodies. Identifier limits and ordering match the Responses adapter.
  This change does not alter transport, body-read or decode errors.
- `Exec`, `Resume` and replay reconstruction (`controller.Controller.Replay`, also `Outputs`) verify
  the journal's hash chain before using it and refuse one that fails with the new
  `controller.ErrJournalTampered`; the Sessions API reports `DATA_LOSS`. Before, a record changed in
  storage without its `content_hash` (disk corruption, a manual edit, a bad migration) was served by
  replay with no model call, re-driven by `Resume` (the model ran on a changed `INPUT`), extended by
  `Exec`, and copied by `Fork` into a child whose re-hashed chain then verified. `Resume` of a
  pending approval (its park and its approval-request repair) and `Fork` (the copied prefix) check
  the chain too. The chain is unkeyed: this catches corruption and edits that skip rehashing, not a
  writer that recomputes every hash after its edit, and not records removed from the end.
- The AgentSuite demo (`examples/agentsuite-substrate`) checks its model endpoint with
  `internal/modelconfig.New`, the check `agentsessionsd` runs at startup: `up.sh` refuses a model
  API key with a plain-`http` base URL to a host that is not loopback, `localhost` or a `.svc` name
  before it deploys anything, rather than leaving the daemon to refuse it in the cluster.

## v0.2.0 - 2026-10-16

### Compatibility

v0.2.0 moves the Go module, migrates the journal with no downgrade path, and tightens several API
and harness checks. To upgrade from v0.1.x:

1. Change imports and `go install` paths to `github.com/agent-primitives/sessions`, and images to
   `ghcr.io/agent-primitives/sessions` ([Module path and images](#module-path-and-images)).
2. Stop every v0.1.x process that shares a journal and back the journal up before v0.2.0 opens it
   ([Journal schema v4](#journal-schema-v4)).
3. Check `agentsessionsd`'s new startup refusals: a model API key with a plain-`http`
   `-model-base-url` to a non-local host, a base URL that is not absolute, or a query parameter set
   in both the base URL and `-model-path`
   ([agentsessionsd and model/openai](#agentsessionsd-and-modelopenai)).
4. Stop sending `ForkRequest.identity`, retry `ABORTED`, and expect `FAILED_PRECONDITION` or
   `UNAVAILABLE` where some calls returned `INTERNAL` ([Sessions API](#sessions-api)).
5. Update custom `controller.ToolFunc` executors and `placement.Dialer`s to their new Go signatures
   ([Go API](#go-api)), and upgrade a substrate backend together with its cluster
   ([Substrate backend](#substrate-backend)).

#### Module path and images

- The Go module path is now `github.com/agent-primitives/sessions`, and every package path under
  it moves with it: the generated stubs are `github.com/agent-primitives/sessions/api/genpb` (still
  package `agentsessionsv1`), and the substrate adapter is
  `github.com/agent-primitives/sessions/integrations/substrate`. v0.1.x stays at
  `github.com/aramase/agentsessions`; nothing newer is published there. To update a module that
  imports it, rewrite the import paths, then require the new module and drop the old one:

  ```bash
  grep -rl --include='*.go' github.com/aramase/agentsessions . \
    | xargs perl -pi -e 's#github\.com/aramase/agentsessions#github.com/agent-primitives/sessions#g'
  go get github.com/agent-primitives/sessions@v0.2.0
  go mod tidy
  ```

  One binary cannot link both module paths: both register the same proto files, and protobuf-go
  panics at init (`file "common.proto" is already registered`). Move every dependency together.
  The proto package (`agentsessions.v1`), services, messages and field numbers are unchanged, so
  gRPC method names and the wire encoding are the same under both paths; the behavior changes
  below still apply.
- Container images move to `ghcr.io/agent-primitives/sessions/agentsessionsd` and
  `ghcr.io/agent-primitives/sessions/harnessnode`; v0.1.x images stay at
  `ghcr.io/aramase/agentsessions`. Release archives keep their names
  (`agentsessions_<version>_<os>_<arch>.tar.gz`).

#### Journal schema v4

- <a name="sqlite-v4-rollout"></a>
  The SQLite journal schema moves from version 1, which v0.1.0 through v0.1.2 write, to version 4.
  The first open by a v0.2.0 binary, `agentsessionsd` or any `agentctl` command on an embedded
  journal (including `get`), migrates the database in place in one transaction, so a crash leaves
  it at its old version. Existing event bytes and hashes are not rewritten. A database at version 2
  or 3, written by an unreleased build, takes the remaining steps the same way. A database stamped
  with a negative version is refused.
  - Version 2 adds the `harnesses` and `reserved_harness_names` tables and an index on
    `sessions(harness)`. It reserves every harness name an existing session or recorded
    `EXECUTION_START` uses, since all of them were static, so no host can register one later. An
    event that does not decode fails the migration, and the database stays at its old version.
  - Version 3 adds the approval projection and captured cursor, backfilled from the journal. On a
    database already at version 2, a session whose events do not decode is isolated with no
    actionable approval reference; healthy sessions remain readable. SQL faults roll back the
    migration. Direct out-of-band edits do not refresh the projection.
  - Version 4 adds the deletion state, requested time and audit anchor; existing sessions stay
    `ACTIVE`. The `deletion_completed_at` column is reserved for cleanup (#88): this build never
    reads or writes it, and no completion state or time is exposed in Go or on the wire.
  - Older builds, including v0.1.0 through v0.1.2, refuse the migrated database with "unsupported
    database schema", and only when they open it. An older process that already has the journal
    open keeps writing after the migration: it appends, mints fences and updates metadata without
    maintaining the approval projection (references stay suppressed for that session) and without
    enforcing deletion tombstones, including after a `DeleteSession`.
  - Rollout: stop every older host and embedded `agentctl` process sharing the journal, back up the
    database (with its `-wal` file, if present), then open it with v0.2.0, and upgrade all writers
    before restarting them. Rollback means restoring that backup and losing what was written
    since: older binaries cannot open the migrated database, nor verify the records v0.2.0 writes
    ([Journal records](#journal-records)).

#### Journal records

- Every new execution records `EXECUTION_START`, including default-config turns, so interrupted
  recovery can reject partially committed inputs. Binaries older than this release fail chain
  verification with a `content_hash` mismatch for sessions containing this event. Rollback is not
  supported for any session that executed a turn on this release, including existing markerless
  sessions that execute another turn after upgrading. No SQLite schema migration or log rewrite is
  required. Replay streams and harness `History` now carry one extra `EXECUTION_START` event per
  turn (in `History`, only prior turns).
- New `EXECUTION_START` records include the resolved registry harness name and optional advertised
  harness version (`HarnessDescriptor.version`). Interrupted `Sessions.Resume` uses the recorded
  registry entry, not the session default; an unserved recorded entry or known-version mismatch is
  `FAILED_PRECONDITION`. Session metadata and later default Exec selection are unchanged, as are
  completed Resume and Suspend/Fork routing. Older markers without a name fall back to session
  metadata; missing/empty recorded versions keep existing unversioned behavior. New Exec still
  accepts unversioned harnesses. Nonempty recorded versions must exactly match the served version,
  including rejection of served empty. Controller Replay preflights all completed invocations
  before running its single supplied harness; raw `Sessions.Replay` is unchanged. Remote checks
  remain Describe-based, not bound to the Connect stream. The reference harnesses advertise `"1"`,
  independent of host build/release versions. No journal rewrite or SQLite migration is required;
  binaries missing these new fields cannot verify hashes of records where they are populated.
- `DeleteSession` appends a `LIFECYCLE_DELETE` record. Its hash requires a deletion-aware reader.
- Controller Replay and Resume accept the ID-less journals written by v0.1.2 without rewriting
  existing records or hashes. Pure legacy Replay retains v0.1.2's whole-log harness invocation,
  including its multi-turn replay limitation. Resume retains the last-INPUT rule and continues
  interrupted legacy turns with ID-less events; END or ERROR finishes a legacy turn. Original
  config, resume cursor and input completeness remain unavailable. Harness invocations use a
  deterministic `legacy-<sha256>` transport-only ID derived from host-bound session UID, original
  record position and canonical content; it is not journaled. Direct controller callers must supply
  a nonempty `WithSessionUID` for selected legacy invocations, or receive `ErrMissingSessionUID`
  before the harness runs. Exec permits a new modern turn after an unfinished or errored legacy
  tail. Its EXECUTION_START establishes the boundary: mixed Replay preserves the complete legacy
  prefix's whole-log semantics and skips the abandoned tail, while modern History retains the
  original bytes. Only the ID-less event kinds the released writer persisted qualify: ID-less
  start markers and non-legacy kinds are invalid. Once a modern start marker or ID-bearing
  execution event appears, later ID-less execution events still fail closed. Remaining invalid-log
  refusals are FAILED_PRECONDITION. Inherited-tool safety checks and rollback restrictions remain.
- Chat executions written on `main` since execution-config journaling was added in #76 with a
  non-empty `system_prompt` no longer pass deterministic controller replay or interrupted resume
  when their recorded model-input hashes exclude the prompt. Non-JSON config now fails whenever
  the harness re-runs that execution. The chat harness previously ignored both. Reading recorded
  events through `Sessions.Replay` is unchanged, as is Resume of an already-completed turn.
  No tagged release is affected.

#### Wire decoding of empty bytes

- Breaking for turns journaled from a remote harness that sent an empty bytes source. A remote
  harness model request whose file part sets `bytes`, or whose reasoning part sets `opaque_bytes`,
  to empty bytes now hashes the same as one with no source. A turn such a harness journaled before
  this change recorded its model input hash (I0) over the empty value. `Replay` of that turn once
  completed now fails with "model input hash mismatch", as does a `Resume` of such a turn cut after
  its model call was recorded; `Resume` of a completed turn is a no-op and does not check. The
  reverse also holds: such a turn journaled after this change fails the same checks on a host
  without it. No known harness sends an empty source: `harnesswire` omits it, so only a harness that
  builds the request proto itself could. There is no migration for such a turn.

#### Sessions API

- `ForkRequest.identity` is now rejected with `INVALID_ARGUMENT`, including an empty identity
  message, because per-child principals are not enforced. v0.1.0–v0.1.2 accepted this field,
  stored it on the child, and returned it from `GetSession` and `ListSessions`; callers must now
  omit it to fork. `Session.identity` on `CreateSession` remains recorded provenance, not
  authorization, and is unchanged.
- `ResumeRequest.boot` is deprecated. The service never read it, and substrate no longer has a
  per-resume boot flag; the backend decides how a resumed session comes back. The field keeps its
  number, so existing clients still encode. `client.Client.Resume` still sends its `boot` argument
  in the field; the reference service ignores it.
- Optional `ResumeRequest.expected_last_seq` checks the journal head under the session guard
  before inspection, after an early fence before restore/approval repair, and after a restored
  turn's regular writer fence before Run. A mismatch is `ABORTED` with expected/head values;
  explicit zero requires an empty journal. A late mismatch may already have restored compute,
  but does not run the harness or append records. This catches other hosts' commits before each
  acquired fence. Acquiring a fence supersedes older writers even if Resume then refuses: their
  remaining writes fail with `ErrFenced`, leaving any unanswered tool intent eligible for retry.
  Fencing cannot cancel effects already in flight or exclude a later writer; this is not an
  execution lease or exactly-once tool execution. Omission preserves existing behavior.
- Older servers silently ignore `ResumeRequest.expected_last_seq`; clients must not assume the
  check ran without operator-confirmed deployment of a supporting server version/build. Sessions
  exposes no server-version/capability query, so clients need out-of-band deployment provenance;
  automatic support discovery is follow-up work.
- `Resume` and `Fork` now run the same `CanPlace` check as `Exec` before touching compute or the
  journal, and a refusal is `FAILED_PRECONDITION` (`Resume` previously returned `INTERNAL` for any
  placement error). A harness that cannot be reached to describe itself when the call is admitted is
  `UNAVAILABLE` rather than `INTERNAL` on `Exec`, `Resume`, and `Fork`, and nothing is journaled;
  one that has not answered within 10 seconds counts as unreachable, so a call with no deadline
  cannot be held by a harness that never answers. If the call's own deadline runs out, or the caller
  cancels, while the harness is being described, the call is `DEADLINE_EXCEEDED` or `CANCELLED`
  instead, also with nothing journaled. A harness lost mid-turn is still `INTERNAL` and leaves an
  interrupted turn to `Resume`. Forking a `REQUIRES_MEMORY_SNAPSHOT` harness on a backend without
  memory snapshots is refused before the parent is checkpointed, so it no longer leaves a `SUSPEND`
  event on the parent.
- Ordinary `Resume` and `Suspend` now return `ABORTED`, rather than `INTERNAL`, for journal CAS
  conflicts (`eventlog.ErrConflict`) and superseded fences (`eventlog.ErrFenced`).
- Overlapping `Exec`, `Suspend`, and `Resume` calls for one session across all Placers in one
  Registry now return gRPC `ABORTED` without changing compute or the journal. Retry after the
  in-flight operation finishes; other sessions remain independent. Direct Placer calls return
  `placement.ErrSessionBusy`. Standalone Placers have private guards; separate Registries or hosts
  are not fenced by this guard. Construct the Registry before using its Placers; registering
  already-used Placers or sharing them between Registries is unsupported. Idle session guard entries
  are reclaimed.
- `Approve` and a stateful `Fork` on the same session now exclude each other through the shared
  checkpoint mark. A refused approval returns `ABORTED` without minting a fence or writing a
  decision; retry after the fork finishes. This coordination covers one Registry/host only.
- `ExecRequest.harness` cannot move a session onto or off a registered harness for one turn; that is
  `FAILED_PRECONDITION`. Overrides between static harnesses (built-in or given with `--harness`)
  are unchanged.
- `Exec` refuses a stateless harness override when the session's served stored harness requires
  memory snapshots (`FAILED_PRECONDITION`, before any fence or journal write). `Resume` and
  no-input Exec recovery also refuse an older interrupted override before compute changes or
  journal repair; an ordinary interrupted turn can be superseded by a new Exec on the stored
  harness. Parking only the override cannot checkpoint the stored harness's live memory.
  Stateless-to-stateless overrides are unchanged: a stored runtime without memory-snapshot
  support is not contacted for this check, even when its remote harness is down.
- Owned unreceipted approval calls block new input, including after a decision. No-input Exec
  recovers the existing current approval invocation with its recorded config, inputs, route and
  cursor, checking CAS/deadline under the guard before runtime IO or request repair. Unrelated
  inputless Exec retains ordinary new-turn behavior. Pending Resume queries do not touch compute;
  recorded-route Suspend can retry failed automatic handoff for owned approvals, with no
  stored-default fallback. Inherited approvals suspend through the child's stored-default harness
  so its own compute is checkpointed; Resume still refuses unresolved inherited tool intents.
  Gated executor failures become handled EXECUTOR_ERROR receipts for model continuation; a
  missing executor is a recoverable precondition, not a receipt. Ordinary tool errors are unchanged.
- `DeleteSession` permanently retains the UID, metadata and hash-chained journal for audit, hides
  the session from List and fences further mutations, including historical approval retries and
  forks. Get and raw Replay remain available; children are independent. PENDING acknowledges only
  durable intent: no Runtime.Stop, cleanup or resource reclamation is attempted. Actors, snapshots
  and fork pins remain retained until ownership/cleanup work (#88).
- Resume now rejects a forked unfinished execution with unresolved inherited tool intents with
  `controller.ErrInheritedToolIntent`, rather than re-driving a parent's effect under the child's
  UID. This applies to existing journals, including legacy markerless turns; Fork still copies the
  prefix, and completed inherited intent/result pairs remain recoverable without invoking the
  executor, including legacy results recorded after the fork marker. `Sessions.Resume` reports
  `FAILED_PRECONDITION` for unresolved inherited intents with guidance to fork at or after the
  `TOOL_RESULT`, or Exec a new turn. No wire or journal schema change is required.
- `Resume` can now spend a provider call where it previously failed: a turn whose last effect is a
  `MODEL_CALL` with no recorded completion is re-driven with one provider call instead of failing
  with "recorded completion missing". This includes the child of a fork taken at a `MODEL_CALL`. No
  wire or journal schema change.

#### Harness protocol and replay checks

- Controller Start now carries the session UID on live, replay and resumed invocations, and the
  bridge includes it on host control frames. Hosts serving tool results must echo the Start
  session as well as execution/call correlation; omitted or different tool-frame namespaces
  are rejected, including for ordinary tools. Model reply namespace validation is unchanged.
  Harness-emitted host-owned request/decision/execution-start records and unsolicited PARKED
  acknowledgements are rejected by the bridge rather than forwarded to the host sink.
- Approval additions preserve old field numbers, default receipt/decision bytes and model-input
  hashes. New durable receipt status/correlation fields are hash-bound; transient Go approval
  carriers are excluded from wire receipts and model-input hashing. Low-level Append refuses
  transport-only `PARKED` events but otherwise remains permissive; projection suppression does
  not replace validation at command boundaries. Remote harness reports with host-owned approval
  receipt fields or codes other than `CODE_UNSPECIFIED`/`EXECUTOR_ERROR` are rejected by
  approval-aware Report validation; `EXECUTOR_ERROR` remains accepted on harness reports.
- Replay and interrupted-turn resume now reject tool-call identity and result-correlation mismatches
  that v0.1.0–v0.1.2 previously accepted. Resume also rejects harnesses that leave recorded effects
  unconsumed instead of marking the turn complete. Direct controller sinks can recover handled tool
  executor failures without re-executing nonterminal intents that have no result; Harness.Connect
  still ends the turn on a sink tool-call error. An immediately following in-harness TOOL_RESULT
  remains ambiguous without a failure receipt and is rejected if its ID differs from the preceding
  call. On direct controller sinks, handled pre-intent mediation rejections remain recoverable when
  no recorded tool call is next, or the next call has a different ID: reject the unsupported call
  without consuming the fallback's recorded intent. Same-ID mismatches and malformed evidence
  remain fatal, without relaxing identity checks on actual recorded calls. No wire/schema change.
- Controller and bridge sink methods are serialized per invocation and refuse use after Run
  closes. A recorded-prefix identity/correlation mismatch remains fatal even if the harness
  handles the error; it cannot enable later effects or successful completion. Callbacks must
  not re-enter the same sink. Ordinary handled pre-intent rejections and operational executor
  errors retain their continuation behavior.

#### Go API

- `api.DeletionComplete` and `sqlitelog.SessionInfo.DeletionCompletedAt` are removed. Both were
  added after v0.1.2 and never released, written, or sent on the wire; Go code built against an
  unreleased build that referenced them no longer compiles. Go and the wire know only the `ACTIVE`
  and `PENDING` deletion states. A stored deletion state other than those two is unrecognized and
  treated as deleted, never as `ACTIVE`.
- `controller.ToolFunc` now takes `controller.ToolCallContext` between `ctx` and `call`. Custom tool
  executors must update their Go signatures and scope authorization and durable deduplication to
  `ToolCallContext.SessionUID` plus the harness-chosen idempotency key. Placement binds the UID for
  Exec and Resume; `controller.New` rejects a non-nil tool executor without a nonempty
  `controller.WithSessionUID` with `controller.ErrMissingSessionUID`, before advancing the fence.
  This is a Go source compatibility change, not a wire or journal schema change.
- `api.Incarnation` gains `CallMetadata`, which the host attaches to every harness call.
  `placement.Dialer` now takes the `api.Incarnation` instead of its address, so custom dialers
  passed to `placement.WithDialer` must change signature; `placement.DefaultDial` is the stock one.
- `Runtime.Snapshot(..., SnapshotExternal)` owns the cold transition: capture state, release
  dedicated compute where applicable, and retain any handle needed by `Restore`. Out-of-tree
  runtimes that only capture state must implement that transition themselves; the Placer no longer
  calls `Stop` afterward. Suspended actors and fork-child snapshot pins are retained indefinitely:
  `DeleteSession` records durable intent only and no session teardown calls `Stop`. Operators must
  reclaim them directly in Substrate. No signatures or wire formats change, and restoring a snapshot
  after destructive `Stop` is not guaranteed.
- A stateful `Placer.Fork` now fences the parent's log and closes its open harness streams before
  checkpointing it, and refuses new turns on the parent until every child is cloned from the
  checkpoint, so nothing holds the checkpoint up or replaces it before the last clone; on kind, a
  checkpoint under an idle stream took 5m30s. A fan-out that fails partway releases the parent
  before it tears the cloned children down. This holds across every Placer of a
  `placement.Registry`, so a turn that `ExecRequest.harness` routed to another harness is covered:
  `placement.NewRegistry` gives its Placers one shared per-session state and session guard, and
  `placement.Registry.Add` gives a registered harness's Placer the same, so a fork through a static
  harness covers a turn on a registered one and the reverse. Both now fail if a Placer already
  belongs to another registry or has an operation in progress. Five behavior changes follow:
  - Before it snapshots, the fork waits until every turn whose stream it closed has minted its
    fence or returned, bounded by the caller's context. If the context ends first, the fork fails
    before snapshotting and records nothing.
  - A fork that reaches its checkpoint supersedes the parent's in-flight turn even if the checkpoint
    then fails. The turn stops as an incomplete execution (no `ERROR` event), the parent stays live,
    and the caller runs the next turn or retries the fork at the new head.
  - A turn superseded by a fork's checkpoint, or started while one runs, returns an error wrapping
    the new `placement.ErrCheckpointing` (the superseded turn also wraps `eventlog.ErrFenced`).
    `Exec` and `Resume` report it as `ABORTED`, which callers retry.
  - Stateful forks and `Suspend` of one session do not overlap; they hold the same checkpoint
    mark. A fork or `Suspend` that arrives while a fork checkpoints the parent or clones its
    children, or a fork that arrives while a `Suspend` runs, is refused before it fences or
    snapshots anything, with an error wrapping `placement.ErrCheckpointing`, which `Fork` and
    `Suspend` report as `ABORTED`.
  - The hold starts when the fork installs its checkpoint mark and covers the turns this
    `placement.Registry` drives. A turn whose `Runtime.Create` or `Runtime.Restore` was already in
    flight when the mark was installed, or a request from outside this Registry, can still wake the
    parent before a child is cloned. A runtime that finds the fork source superseded returns an
    error wrapping the new `api.ErrSnapshotSuperseded`, which `Fork` reports as `ABORTED`; callers
    retry at the parent's new head.
- Controller and bridge sink calls now honor their own context cancellation, both while queued
  behind another effect and when an uncontended call arrives with an already-canceled context.
  Canceled calls start no effects; Replay and Resume also refuse such calls against older journals
  without consuming recorded evidence. An uncontended call still returns an existing sticky
  park/failure before cancellation. Request/reply serialization and Run shutdown drainage are
  unchanged.

#### agentsessionsd and model/openai

- `agentsessionsd` exits at startup with a model API key and a plain-`http` `-model-base-url` to a
  host that is not loopback, `localhost` or a `.svc` name (see Security). Switch such an
  endpoint to `https`, or unset the key, before upgrading.
- `openai.New` refuses a base URL that is not absolute (no scheme or host), so `agentsessionsd`
  exits at startup with `-model-base-url` set to one. Such a URL used to fail every request.
  It also refuses a query parameter that both the base URL and the path (`WithPath`,
  `-model-path`) set, such as an `api-version` in each, which was sent twice; set it in one of
  them. The error names the parameter, never its value.
- `model/openai` refuses a redirect to another origin instead of following it. Set the base URL
  to the endpoint that serves the request. A client passed to `WithHTTPClient` is copied, not
  modified; its own `CheckRedirect` still runs for same-origin redirects.
- `agentsessionsd -registry-addr` is off by default. Enabling it creates a plaintext,
  unauthenticated **local admin surface** on a separate literal-loopback listener; Sessions still
  refuses registry RPCs, and neither listener gains TLS or authentication. Any local or same-pod
  process that can reach it can administer harnesses. The daemon now loads persisted registrations
  even when the listener is off, so existing sessions can still use retired harnesses after restart
  with `-model` set. Both new and stored remote registrations require `-model`; without it, stored
  remote registrations load as unservable and their existing sessions fail `FAILED_PRECONDITION`,
  whether active or retired. Inspecting, listing and retiring do not require `-model`; substrate
  placements are unsupported by this reference daemon. No Sessions transport/access guarantees
  change.
- An unsupported stored harness registration (unknown spec fields or enum values, or a local
  placement/factory failure) no longer prevents other harnesses from starting. Its name stays
  reserved, and Get/List/Retire retain its metadata, original digest and ACTIVE/RETIRED state.
  `HarnessRegistration.unservable_reason` explains this host's inability to run it; an undecodable
  spec is unset, not partially returned. Existing sessions on a registration isolated during this
  host's startup load and new attempts to use it fail `FAILED_PRECONDITION` until this host restarts
  on a compatible version. A registration added to a shared journal after this host started remains
  visible but is unknown to this host's Sessions routing: Create and auto-create Exec selecting it
  remain `INVALID_ARGUMENT` (a pending recorded Resume retains its `FAILED_PRECONDITION` recovery
  rule) until this host restarts or loads it through `RegisterHarness` with the identical compatible
  spec. For a RETIRED row, restart this host to serve its existing sessions without reactivating it:
  `RegisterHarness` reactivates the row for every host sharing the journal.
- The `echo` and `chat` names are reserved for the built-in harnesses, including `chat` on a host
  started without `--model`, and so is every name given to `agentsessionsd --harness`.
  `agentsessionsd` and embedded `agentctl` record the names they run in the journal at startup, so
  no host sharing it can register them later, and refuse to start on a journal that already holds a
  registration, active or retired, under one of them. Opening a journal with either binary
  therefore writes to it. A reservation is permanent.

#### Substrate backend

- The substrate backend now builds against agent-substrate `96dffb28` (v0.1.2 pinned `b1bd558a`)
  and needs an ate-api at or after `f03327bd`. The two pins are not wire-compatible: the old client
  decodes a current `Actor` without an error but with the wrong state and garbage for the worker
  address. Upgrade the backend and the substrate cluster together. `integrations/substrate` now
  requires Go 1.27, following substrate.
- The substrate backend reaches the harness through `atenet-router` (h2c, `ate-target-actor`
  metadata) instead of dialing the worker pod's IP on port 80, which current substrate no longer
  serves. The host must be able to reach `atenet-router.ate-system.svc:80`
  (`substrate.WithRouter` overrides it). The router does not authenticate callers; see
  [docs/security.md](docs/security.md) before running it outside a fully trusted cluster.
- Through `atenet-router`, a harness stream that stays idle for about 5m30s (the router's default
  5m route timeout plus 30s) is reset with `RST_STREAM` (gRPC `Internal`; measured on kind at
  substrate `fc0e3586` with the stream held idle for 6m30s), so a turn parked on a slower model
  call fails. Raise the router's `--route-timeout` if turns can run that long.
- `runtime/substrate.ControlClient.ResumeActor` drops its `boot` argument (substrate removed
  `ResumeActorRequest.boot`); a new actor starts from its template's golden snapshot, or cold-boots
  if none is built. `SnapshotCloner` follows substrate's Tag API (`TagActor`,
  `CreateActorFromTag`, `DeleteTag`); `TagActor` must report a name already taken as the new
  `ErrTagExists`, so a fork whose tag create fails deletes only a tag it reserved.
  `ObjectRef.Namespace` is now `ObjectRef.Atespace`, and
  `ActorInfo` reports `Worker` and `Snapshot` instead of `PodIP` and `MeshDNS`.
  `integrations/substrate`'s `New` and `FromClient` drop their `dnsSuffix` argument, since nothing
  dials an actor's mesh DNS name any more. A stateful fork now checks on both sides of the tag that
  the parent still holds the checkpoint the fork took, and otherwise fails with
  `ErrSnapshotSuperseded`, which wraps `api.ErrSnapshotSuperseded`.
- `integrations/substrate` now reads the held snapshot from `ActorStatus.snapshots` (substrate
  `f03327bd`) and no longer reads `ActorStatus.external_snapshot`, so upgrade substrate and the
  adapter together. Against an older ate-api the adapter reports no held snapshot: a stateful
  `Placer.Fork` checkpoints the parent, records the `SUSPEND`, and then refuses as unplaceable (no
  external snapshot handle), which `Fork` reports as `FAILED_PRECONDITION`; a direct
  `runtime/substrate.Backend.Fork` returns `ErrNoSnapshotToClone`.
- The substrate conformance suite (`integrations/substrate/e2e`) runs both tiers on the micro-VM
  sandbox class by default; the stateless tier used gVisor before. A host without KVM opts each
  tier back into gVisor with `ECHO_SANDBOX_CLASS=gvisor` and `COUNTER_SANDBOX_CLASS=gvisor`, after
  applying `deploy/substrate/namespace.yaml`, `echo-gvisor-workerpool.yaml` (formerly
  `echo-workerpool.yaml`, which also created the Namespace) and `counter-gvisor-workerpool.yaml`
  with the worker image resolved, as `docs/substrate-conformance.md` shows.
  `TestStatelessReplayOnGVisor` is now `TestStatelessReplay`, and `TestSessionSuspendResumeOnGVisor`
  is now `TestSessionSuspendResume`.

### Added

- Reference chat harness (`harness/chatagent`): a `STATELESS_REPLAY` text harness that sends the
  recorded conversation and the turn's inputs to the host-mediated model on every turn.
  `agentsessionsd` serves it as `chat` when `-model` is set; `harnessnode` serves it with
  `HARNESS_KIND=chat` and `HARNESS_MODEL`.
- Tool calls in model messages: `Part` carries a model's `tool_call` and a tool's `tool_result`,
  and `ModelCall` (`api.ModelRequest`) carries the `tools` offered to the model and a
  `tool_choice`. A request that leaves the new fields unset hashes as before, so existing journals
  keep replaying. `model/openai` does not map them yet and refuses such a request with
  `ErrUnsupportedPart` instead of dropping them.
- `placement.WithToolExecutor` supplies the host executor for controller-mediated tools in `Exec`
  and `Resume`. Without one, tool calls fail closed.
- Optional `ResumeRequest.expected_last_seq` rejects a moved journal head before re-driving a turn.
- Go SDK `Client.DeleteSession(ctx, uid)` and `agentctl delete --session <uid>` request permanent
  soft deletion. Get/delete CLI report deletion state and requested time; PENDING output explicitly
  says durable intent only, with compute and fork pins retained.
- Controller `InspectApproval` and decision-only `Approve` validate durable request/decision/receipt
  evidence, including exact retries and session-owned versus inherited effects. The internal
  approval engine supports captured-head repair and correlated denial/executor-error receipts.
  Live `REQUIRES_APPROVAL` calls now park stateless executions at a durable host-owned request.
  Placement closes the invocation, snapshots externally and commits SUSPEND before reporting
  a successful pause; transport/close/snapshot/append failures remain errors with the request
  recoverable.
- `Harness.Connect` bridge supports the stateless approval exchange: full session/execution/call
  correlation, park acknowledgement plus actual stream closure, and decision-before-receipt order.
  A transport park alone is not a successful public Session pause.
- `Sessions.Approve` and `client.Client.Approve` commit only a decision; call Resume separately.
  CLI `approve` and `deny` commit then Resume, explicitly preserving the committed decision on
  recovery failure. Identity is recorded provenance, not authorization. Cancel remains
  `UNIMPLEMENTED`; unresolved requests and retained snapshots have no expiry.
- Every Session reply and initial/final approval Exec frame uses captured metadata/cursor and
  the storage projection. Pending owned requests report AWAITING with `pending_approval`;
  clearing a reference after a decision does not prove a receipt or completion. SDK Exec retains
  the latest Session and maximum committed cursor, including partial failure.
- Approval wire types provide correlated request references, explicit receipt statuses/sequences
  and transport-only park acknowledgements without changing old field numbers or default hashes.
- SQLite `SessionInfo` captures the journal-derived `last_seq` and a reporting-only
  `PendingApproval` with metadata, using an incremental approval projection in the append
  transaction. Calls whose `TOOL_CALL` precedes the child's latest `FORK` are excluded, as are
  invalid approval-bearing histories and projections lagging the journal head; the reference
  is not command authority or a runtime/integrity probe.
- `agentctl replay -v` also prints each record's content hash and previous hash (16 hex), the
  execution id, a model call's model and input hash, and a usage record's token counts. Without
  `-v` the output is unchanged.
- Go SDK callers can pass opaque per-execution config through `client.ExecOptions.Config`.
  Omitting it keeps the existing empty-config behavior. Config is durable journal content exposed
  by Replay; do not include credentials. No wire or journal schema change.
- The chat harness accepts a per-execution `system_prompt` in its JSON config, prepended before
  conversation history and current inputs.
- `agentsessionsd --harness name=address` registers a harness that is already running, over TCP
  (`host:port`, `dns:///host:port` or `dns://resolver/host:port`) or a unix socket (`unix:` or
  `unix://` followed by an absolute or relative path), instead of compiling it into the server. Any
  other address form is refused at startup. It is served by the new `runtime/remote` backend, which
  owns no sandbox: such a harness must be `STATELESS_REPLAY`, and one that declares
  `REQUIRES_MEMORY_SNAPSHOT` is refused, including when it is only the replica that answers on a
  turn's own connection behind a load balancer. The check is not bound to the turn's `Connect`
  stream, so a harness that takes over the address between the two calls is not detected; the
  operator must control what serves the address. `--harness` requires `--model`, so a remote
  harness's model calls never fall back to the built-in echo stub.
  The connection is cleartext and unauthenticated; see `docs/security.md`. On unix platforms,
  `harnessnode` serves on a unix socket when `HARNESS_ADDR` is `unix:///path`. It holds a lock on
  `path.lock` while it serves, so a second instance on the same address fails, and on restart it
  reclaims a socket file left behind by a crashed instance. It never removes a socket another
  process created, and it refuses a lock file that is a symlink, a hard link, or owned by another
  user.
- `HarnessRegistry` API (`api/harness_registry.proto`): register, get, list and retire harnesses on
  a running host. A registration is immutable and identified by its name and `spec_digest`. Retiring
  a harness refuses new sessions while existing sessions keep running, resuming and forking;
  registering the same spec again reactivates it. When a spec sets `descriptor_id`, `Exec`, `Resume`
  and `Fork` are refused with `FAILED_PRECONDITION`, before anything is journaled, if the harness
  reports another id (`placement.WithDescriptorID`). A harness added with `placement.Registry.Add`
  shares the Registry's session guard, so overlapping calls for one session through it and a static
  harness are `ABORTED` too. It is an admin API available in-process through
  `session.NewHarnessRegistry` and, optionally, on `agentsessionsd`'s separate loopback operator
  listener (see [security.md](docs/security.md)). `agentctl harness register|get|list|retire` uses
  that listener with an explicit `--server` address.
- `agentsessionsd -tool-executors <file>` routes controller-mediated and approval-gated tools of
  static `-harness` entries to operator-run executors on Unix sockets, from a versioned JSON config
  loaded at startup (see [tool-executors.md](docs/tool-executors.md)). Only a route that declares
  `durable_dedup: true` dispatches; false or unknown refuses before dialing. Socket paths longer
  than 103 bytes are rejected at startup on every platform (macOS's stricter `sun_path` limit,
  reserving its NUL terminator). Dial errors omit the socket path from harness-visible errors and
  approval receipts, and oversized replies report the 1 MiB frame limit rather than EOF. Without the
  flag, behavior is unchanged.

### Fixed

- A harness can now journal the usage of a host-mediated model call. `harnesswire` dropped
  `ModelResponse.Usage` in both directions, so a remote harness always saw zero usage, and Replay
  and Resume served a recorded completion with no usage, so a harness reporting the usage it was
  given diverged from its record. The host now sends `ModelResult.usage` (already in the proto) and
  the harness side reads it; Replay and Resume serve the `USAGE` recorded directly after the
  completion. A harness built against an older `harnesswire` ignores the field, and an older host
  sends none, which reads as zero usage. If a turn is cut between its `OUTPUT` and its `USAGE`,
  Resume serves zero usage and does not re-invoke the model, so that call's usage is journaled as
  zero (a harness that always reports the usage it was given), or not at all (one that skips zero).
  v0.1.2 journals replay as before, with no usage served.
- `model/openai` joins the completions path to the base URL's path instead of appending it to the
  string, so a base URL with a query (`-model-base-url 'https://h/v1?api-version=2'`) reaches
  `/v1/chat/completions` with the query kept, ahead of any query `-model-path` carries. A fragment
  is dropped. Before, the path landed inside the query or fragment and every turn went to `/v1`.
  `agentsessionsd`'s startup log names that joined endpoint (`openai.Client.Endpoint`), with a
  userinfo password masked, instead of the base URL and path concatenated.
- The substrate adapter (`integrations/substrate`) builds against agent-substrate `96dffb28`, which
  replaced `ActorStatus.external_snapshot` with a `snapshots` list (substrate `f03327bd`) and
  `SnapshotConfig.on_commit` with `preferred_fidelity`. The held snapshot is now the
  highest-generation entry whose durable copy is `COMPLETED`, the one substrate restores and tags,
  so a stateful fork keeps cloning the snapshot its suspend produced. Against an ate-api older than
  `f03327bd` the adapter reads no snapshot, so a stateful fork refuses as unplaceable (no external
  snapshot handle); see Compatibility. The CI cluster moves to the same commit, and the
  `deploy/substrate` WorkerPools use its `sandboxClasses` list in place of `sandboxClass`.
- Session-level suspend no longer destroys the Substrate actor needed by resume. Both replay-based
  and memory-snapshot sessions retain their restore handle after `Placer.Suspend`; explicit `Stop`
  remains destructive teardown.
- `Resume` recovers a turn cut between `MODEL_CALL` and its recorded completion, whether by a
  provider error or a process death. Previously every attempt failed with "recorded completion
  missing" and appended another `ERROR`, so the session could never resume. When the harness
  re-issues a request with the recorded input hash, the host invokes the provider once and records
  the completion after the existing `MODEL_CALL`; a different hash still fails as divergence. Each
  cut during a model call costs at most one extra provider call within one host, where the
  per-session guard serializes `Exec` and `Resume`; the bound does not hold across hosts sharing a
  journal. A fork at a `MODEL_CALL` costs one provider call when the child resumes. A recovering
  call that fails is itself a cut, and the next `Resume` re-drives it again. Journals with
  execution IDs that are in this state recover on the next `Resume`; their earlier `ERROR` events
  remain as audit records. A v0.1.2 journal without execution IDs is not re-driven and still fails
  with "recorded completion missing". No journal or wire schema change.
- The `Start` comment in `harness.proto`, `api.Start.History`, the API reference, and the harness
  authoring guide said `Start.history` is empty after a memory restore. The host sends every event
  before the execution's start on every turn, including after a restore; `Exec` has done so since
  v0.1.0. A harness must not treat an empty history as a sign that it was restored. Documentation
  only.
- `wire` decodes a file part's `bytes` source or a reasoning part's `opaque_bytes` that is set to
  empty bytes as no bytes (nil), the same as the encoders, which never send an empty source.
  Previously it decoded to a non-nil empty slice, while the journal stores the encoded form without
  it, so an `Exec` input carrying such a part (an empty file) gave the live turn a different model
  input hash (I0) from the one recomputed on the journal's copy. For a harness running in the host
  that sends its inputs to the model (`chatagent`), the turn ran, but `Replay` of it once completed,
  or a `Resume` of it cut after its model call was recorded, failed with "model input hash
  mismatch". New turns replay; a turn journaled before this change still fails those checks, because
  its recorded hash covers the empty value. No journal or wire schema change. The hash algorithms
  and the hashes of existing records are unchanged; a new turn with such a part records a different
  model input hash than it would have before, and so a different `content_hash` on that record and
  every later one in the chain.
- `harnesswire` keeps the tools a harness declares in `Describe`. They were dropped in both
  directions, so the host never saw a remote harness's declared tools.

### Security

- `model/openai` no longer sends configured headers (`WithHeader`, `WithAPIKey`) to another host on
  a redirect. net/http copied custom headers such as `api-key` to any redirect target, so a provider
  or proxy answering with a cross-host redirect received the credential. A redirect that changes
  scheme, host or port is now refused; same-origin redirects are followed as before.
- `agentsessionsd` refuses to start when a model API key is set (`MODEL_API_KEY` or
  `OPENAI_API_KEY`) and `-model-base-url` is plain `http` to a host other than a loopback address,
  `localhost`, or a name ending in `.svc` or `.svc.cluster.local` (case-insensitive, port ignored):
  the key and every prompt would cross the network unencrypted. The service-name allowance is a
  trusted-network exception: it checks the name only, not that it resolves to a cluster service or
  that the traffic stays in the cluster, and that traffic is still cleartext on the pod network. The
  error names the host and asks for `https`; it never prints the key. `https`, and `http` with no
  key, are unchanged. The check is in `internal/modelconfig.New`.

## v0.1.2

A patch release. No API change; it exists because the v0.1.1 images were unusable.

### Fixed

- Container images are published under the documented names. v0.1.1 pushed them as
  `agentsessionsd-<md5>` and `harnessnode-<md5>`, because ko derives a repository from the Go import
  path and appends a hash unless told not to, so every name in the README and release notes returned
  404.
- Image tags carry the `v` prefix, matching the git tag and the release archives, so one string
  identifies a release everywhere.

## v0.1.1

A patch release. No API change; the reason to take it is the toolchain and the images.

### Security

- Built with Go 1.26.6. The v0.1.0 binaries were built with 1.26.0, which carries 23 reachable
  standard-library vulnerabilities including TLS, x509, and asn1 issues. Both modules now pin a
  toolchain floor so a release cannot ship an unpatched runtime again.
- gRPC 1.83.2, fixing two reachable denial-of-service vulnerabilities against a gRPC server
  (`GO-2026-6443`, `GO-2026-6348`).

### Added

- Container images, so a deployment no longer has to build its own:
  `ghcr.io/aramase/agentsessions/agentsessionsd` and `.../harnessnode`. Multi-arch, built with ko
  from the tagged source, each with an SBOM. The server runs as uid 65532.
- `golangci-lint` and `govulncheck` gate every change, on both modules.

### Fixed

- A failed `Serve` in the embedded CLI server and the local runtime backend discarded its error,
  leaving a dead server that surfaced later as a confusing client dial failure.
- The substrate manifests named a `ko://` placeholder that only the conformance workflow could
  resolve; they now name the published image and apply as written.
- Release notes link each commit. A bare 40-character SHA is not a link in a release body.

## v0.1.0

The first public release. Everything is new, so the useful summary is what the project does and what
it does not promise.

### What it does

- **Sessions API** over gRPC: create, list, get, exec, replay, fork, suspend, resume.
- **Deterministic replay.** Reconstructing a session serves recorded model results from the journal
  and invokes the model zero times, so replay is exact and free.
- **Tamper-evident provenance.** Every record is hash-chained over RFC 8785 JCS, which is
  language-neutral: an independent Python verifier reproduces the Go hashes.
- **Fork.** Branch a session into children that each continue independently from one checkpoint.
- **Bring your own harness.** Implement two methods, in process or over the `Harness.Connect`
  stream.
- **Pluggable compute** through the `Runtime` SPI, with capability-gated placement across
  replay-based and memory-snapshot-based resumption.
- **Live streaming.** Output relays as the model produces it. Deltas are transport only: never
  logged, never hash-chained, never produced on replay.
- **Model access** for any endpoint speaking the chat-completions body, with no vendor SDK.

### Compatibility

`agentsessions.v1` is a proto namespace, not a stability promise. The Go module is pre-1.0 and makes
no backward-compatibility guarantee; the wire contract and the Go SPI may both change. From this
release onward, the schema is gated against the previous release and breaking changes are called out
here.

### Not provided

- No authentication, authorization, or transport security. `IdentityRef` is recorded as provenance
  and never enforced, and `project` is a filter rather than a tenancy boundary. A host is not safe
  to expose to an untrusted network or to treat as multi-tenant. See
  [`docs/security.md`](docs/security.md).
- `DeleteSession` and `Cancel` are declared in the contract and not implemented.
- Harnesses register at build time, so adding one means building a server.
- The full session history is passed to the harness on every turn, which does not scale to long
  sessions.
