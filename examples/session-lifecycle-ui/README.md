# Session lifecycle in a browser

A one-page view of an AgentSessions session on agent-substrate micro-VMs: create it, run turns,
suspend, resume, fork and replay it, and see a snapshot-fidelity refusal. Each call lights the path
it took, from this page through `agentsessionsd` and substrate to the session's actor. Beside the
path are the session's timeline, its actor as substrate reports it, and its journal.

It drives the [AgentSuite substrate example](../agentsuite-substrate) through the real Sessions gRPC
API. It adds no Sessions API and changes no product code. The page is served by `lifecycle-ui`
([`cmd/lifecycle-ui`](../agentsuite-substrate/cmd/lifecycle-ui)), a small Go bridge on this
machine's loopback.

```bash
cd examples/agentsuite-substrate
./up.sh                              # the example's cluster: kind, substrate, micro-VMs
../session-lifecycle-ui/ui.sh        # the third daemon (once), port-forwards, the page on :8090
# then open http://127.0.0.1:8090 in a browser
../session-lifecycle-ui/ui.sh check  # optional, in another shell: every call, checked
./down.sh
```

`ui.sh` uses the example's settings (`AGENTSUITE_DEMO_CLUSTER`, `AGENTSUITE_DEMO_WORK`,
`AGENTSUITE_DEMO_SANDBOX`), so give it the ones `up.sh` ran with. `LIFECYCLE_UI_PORT` moves the page
off 8090. The port-forwards use 18180 to 18184; `ui.sh` stops before changing anything if one of
them, or the page's port, is already taken, so it never talks to another run's forwards. The
requirements are the example's, so micro-VMs need `/dev/kvm` in the Docker host and an arm64 Mac
cannot run them.

Not supported on Substrate v0.6.0 yet: the page's replay port-forwards the router to this machine,
and v0.6.0's envoy router admits only clients with an allowlisted pod certificate, so `ui.sh`
refuses to start. `ui.sh check` against a page that is already running is unaffected. See
[router clients outside the
cluster](../../docs/substrate-conformance.md#router-clients-outside-the-cluster).

## What the page does

| Control | Call | Shows |
|---|---|---|
| Create, on a tier | `Sessions.CreateSession`, harness `triage` (stateless), `triage-memory` (memory) or `triage-memory` on the VOLUMES template | the session; no actor yet |
| Send turn | `Sessions.Exec`, streamed to the page record by record | the reply, then the actor `RUNNING` |
| Suspend, Resume | `Sessions.Suspend`, `Sessions.Resume` | the actor `SUSPENDED` with its newest snapshot's generation and fidelity, then `RUNNING` with the same actor UID |
| Fork at seq N | `Sessions.Fork` at the journal row you select (the head by default) | the child session, which you can open |
| Replay | `Sessions.Replay` reads the journal; the bridge verifies the hash chain, then runs the controller's replay against the session's actor through the atenet-router, as [`cmd/replay`](../agentsuite-substrate/cmd/replay) does | the turns re-run, the controller's model-call count and the fake model's counter before and after; it fails on any model call the controller counts, and when the fake counter moves |
| A refused call | the call's `FAILED_PRECONDITION` | a card with the session, actor, required and observed fidelity, and the daemon's message unchanged |

Replay is not a Sessions RPC: the API streams the journal, and re-execution exists only in process
(the example's [gaps](../agentsuite-substrate/README.md#gaps-this-poc-works-around)). The bridge
does what `cmd/replay` does, on the stateless tier only, since a memory-tier harness keeps its
conversation in RAM. Only a replay that completes claims the re-run and zero model calls. One that
stops after the journal read (a dropped port-forward, an actor the router cannot reach) is marked as
failing in the bridge, lists the counters it read, and draws the model as unknown, or as called if
the controller counted a call. Every daemon calls the same fake model, so its counter also counts
turns from another tab or `ui.sh check`. A replay during which that counter moved while the
controller counted no call fails too, but the move is not attributed to it: the model is drawn as
unknown.

The bridge also replays only a session that Sessions reports `COMPUTE_LIVE`, and the Replay button
is off otherwise. The replay's turns go through atenet-router, which resumes a suspended actor on
any routed request (substrate's router ingress). Replaying a suspended session would wake its actor
outside Sessions, which would still report it `COMPUTE_COLD` with no Resume in the journal. Resume
the session first. The bridge reads the session with `GetSession` just before the replay; that read
is not atomic with it, so a Suspend from another client in between is not caught.

## The refusal

`up.sh` deploys two daemons. `ui.sh` adds a third, `agentsessionsd-volumes`, which serves the
memory-tier harness (`REQUIRES_MEMORY_SNAPSHOT`) from a template that prefers VOLUMES snapshots.
`lifecycle-ui inject` derives that template from the memory tier `up.sh` rendered, with three
changes: `preferredFidelity: VOLUMES`, one `durableDir` volume (at substrate v0.6.0, ateworker-microvm
refuses a VOLUMES checkpoint without one and the actor crashes), and a `-vol` name. A first turn
passes, because the template's golden snapshot is MEMORY. The session's first suspend then completes
a VOLUMES snapshot, and the daemon refuses it:

```text
suspend: session "<uid>": runtime: checkpoint does not meet the required contract: actor
agentsuite-demo/<uid>: required MEMORY with a durable URI, observed VOLUMES (URI present: true);
actor remains suspended: ...
```

The page splits that message into the card's fields and shows it whole beneath them. The lane is
micro-VM only. With `AGENTSUITE_DEMO_SANDBOX=gvisor`, `ui.sh` skips it and says so.

## How the path is lit

The diagram is the path the example deploys.
[`demo/diagrams.md`](../agentsuite-substrate/demo/diagrams.md) cites the code for each arrow of a
turn. For suspend, the adapter calls `SuspendActor` (`integrations/substrate/adapter.go:113`), and
ate-api's suspend workflow calls the node's atelet (substrate
`cmd/ateapi/internal/controlapi/workflow_suspend.go:270`). The atelet then sends
`CheckpointWorkload` to the worker pod's ateworker (`cmd/atelet/main.go:1057`).

While a call runs, the page animates the path it expects. Once the call returns, the bridge asks the
components what they saw during the call:

| Hop | Evidence |
|---|---|
| agentsessionsd | its own `operation finished` line for the call and session (`kubectl logs deploy/<daemon> --since-time`) |
| ate-api | the `CreateActor`, `ResumeActor` or `SuspendActor` lines naming the session, else its `GetActor` line |
| worker pod | ateworker's `RestoreWorkload`, `RunWorkload` or `CheckpointWorkload` line naming the actor |
| atenet-router | its `Route ok` line for the actor (turns and replay) |
| actor | its state and newest snapshot, read from ate-api (`kubectl ate get actor`) |
| model | the turn's `MODEL_CALL` records; for replay, the controller's count and the fake model's counter |

The control and atelet arrows are labelled with the methods those lines name. A turn on an actor
that is already running shows only `GetActor`: the substrate runtime attaches to a running actor
without creating, resuming or restoring it. That is every turn after a session's first, and a fork
child's first turn. The atelet arrow then stays off, and the worker pod is drawn as passed through:
the router's line is the evidence for the turn's hops through the pod's atunnel to the actor, since
atunnel logs nothing per request. Two kinds of line name the session without being the daemon's
call, and the trace leaves both out. atenet-router calls `ResumeActor` on every routed request, and
ate-api answers at once, resuming nothing, when the actor runs; outside a Resume, only a
`ResumeActor` whose logged response says `"resumed":true` counts. And the bridge's own actor reads
(`kubectl ate get actor`, after every call) log `GetActor` lines.

Resume on a session whose actor is already running is allowed and restores nothing: the daemon reads
the actor with `GetActor`, still calls `ResumeActor`, and ate-api answers with `resumed` unset
(substrate `cmd/ateapi/internal/controlapi/workflow_resume.go`). No turn is routed during a Resume,
so that line is the daemon's. The control arrow reads `GetActor + ResumeActor (already RUNNING)`,
the atelet arrow and the arrow into the actor stay off, and the worker pod is dimmed as skipped. For
suspend, resume and fork, only the worker's restore or checkpoint line lights the arrow into the
actor: the actor's state shows what it is now, not that this call changed it.

`Route ok` is the envoy router's line (substrate `cmd/atenet/internal/router/ingress`).

A hop with no matching line is drawn dashed: no line was found, not proof that the step did not run.
A hop with a pod whose logs could not be read (a permission denial, a timeout) is unknown, with the
error, unless a pod that was read has the call's own line; it is never drawn as not seen or skipped.
Select a box to see its line and the command that fetched it. Lines are matched by their `kubectl
logs --timestamps` time against the bridge's clock, from 250 ms before the call to 2 s after it.
That holds for kind on this machine; against a cluster whose clock is further off, hops show as not
seen.

The bridge runs only `get` and `logs`, with kubectl and the kubectl-ate plugin `up.sh` builds, so
the page cannot change the cluster. `ui.sh` changes the cluster only to add the VOLUMES template and
its daemon.

## What it does not protect

- The page authenticates nobody. It listens on loopback only and refuses another host name, a
  cross-origin request and a non-JSON POST, which keeps other web pages out. Anything running as you
  on this machine can still drive every session and spend the model budget.
- It sits on the example's unauthenticated path, the plaintext Sessions API. See the example's
  [gaps](../agentsuite-substrate/README.md#gaps-this-poc-works-around).
- The timeline lives in the bridge's memory and is lost when it restarts; the journal and actors are
  not.

## Tested

`go test ./cmd/lifecycle-ui` (bridge, against an in-memory Sessions server and canned cluster
output) and `node --test` in `cmd/lifecycle-ui/web` (the diagram's rules, and the page's session
selection in a small fake DOM). A live run against the example cluster is recorded in the pull
request, not here.

## Credit

The request-flow view is a port of Sajay Antony's (@sajayantony) session client from his private
substrate demo, used with his permission: the browser-to-gRPC bridge, the per-call flow diagram, and
lighting each hop from the components' own logs. This version drives the merged AgentSuite example
on micro-VMs instead of his Podman and gVisor deployment, and needs no demo-only session database.
