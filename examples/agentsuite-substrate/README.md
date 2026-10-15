# AgentSuite agent as an AgentSessions harness on agent-substrate

A proof of concept. One AgentSuite agent is rendered into substrate ActorTemplates and registered
with `agentsessionsd` as a harness. Its session then runs in a substrate actor, and the demo
suspends, resumes, verifies, replays and forks it. Everything runs on a local kind cluster with no
credentials. The actors are micro-VMs (kata + cloud-hypervisor), which need `/dev/kvm` in the Docker
host; where there is none, such as an arm64 Mac, set `AGENTSUITE_DEMO_SANDBOX=gvisor` for every
script (`up.sh` refuses micro-VM without KVM rather than fall back).

`up.sh` renders the agent twice, in two resumability tiers, and deploys one `agentsessionsd` per
tier, since a daemon serves one substrate harness:

| Harness | Deployment | Tier | The conversation lives in |
|---|---|---|---|
| `<agent>` | `agentsessionsd` | `STATELESS_REPLAY` | the journal; each turn is rebuilt from it |
| `<agent>-memory` | `agentsessionsd-memory` | `REQUIRES_MEMORY_SNAPSHOT` | the actor's RAM (`AGENTSUITE_MEMORY=ram`); only a memory snapshot carries it |

`demo.sh` uses the stateless tier. The [recorded demo](#the-recorded-demo) uses both.

```bash
cd examples/agentsuite-substrate
./up.sh     # cluster, ate-system, images, template, agentsessionsd (2.5 minutes in the recorded run)
./demo.sh   # the demo itself (under a minute of wall clock; about 5 minutes narrated)
./down.sh   # delete the cluster, only if up.sh created it
```

The recipe is for a disposable kind cluster you trust: nothing in it authenticates callers (see
[the gaps](#gaps-this-poc-works-around)).

## What the demo shows

| Step | What happens | What it proves |
|---|---|---|
| 1 | `agentctl create --harness triage` | The suite's agent is an AgentSessions harness; the session is pinned to it. |
| 2 | Three chat turns | Each turn runs in the session's own actor, reached with harnesswire through the atenet-router. Every model call goes through the host: the fake provider counts 3. |
| 3 | `agentctl suspend` | The actor is `SUSPENDED` and holds a `COMPLETED` snapshot in `status.snapshots`, the field the adapter reads since substrate `f03327bd`. |
| 4 | `agentctl resume`, a fourth turn | The same actor is `RUNNING` again, and the reply lists all four issues, so the conversation state survived. For this stateless harness the host rebuilds the context from the journal; the actor's RAM is not what carries it. |
| 5 | `agentctl replay` | The journal: inputs, model calls, outputs and the two lifecycle records, in one ordered log. |
| 6 | `cmd/replay` | The client verifies the hash chain itself. It then re-runs all four turns of the agent in its actor against the journal. The controller's model count is 0 and the fake provider's counter does not move. |
| 7 | `agentctl fork --at <end of turn 1>`, a child turn | The child is a new actor holding only turn 1. Its reply diverges (`1) laptop; 2) printer`), and the parent's four turns are unchanged. |

`demo.sh` fails on the first step whose proof does not hold.

## The pieces

| Path | What it is |
|---|---|
| `suite/` | An AgentSuite (`support-desk`) with one tool-free agent, `triage`, and compositions for `linux/amd64` and `linux/arm64`. The default suite (`AGENTSUITE_DEMO_SUITE`, `AGENTSUITE_DEMO_AGENT`). |
| `suites/kaimahi-minimal/` | kaimahi's `agentsuite/testdata/minimal` (agent `writer`, `linux/amd64` only), copied byte for byte from kaimahi `9c5c993`. `SOURCE` lists each file's git blob id at that commit and its sha256. |
| `suites/coordinator-workers/`, `suites/coordinator-workers-v2/` | kaimahi's `agentsuite/testdata/coordinator-workers` at `75f27d6`, byte for byte, and a new version of it whose agents pin `gpt-5.4-mini`; `coordinator-workers-v2/README.md` lists every change and both suite digests. |
| `binding-microvm.json`, `binding.json`, `binding-coordinator.json` | `AgentSessionsBinding`s (`kmx.kaimahi.dev/v1alpha1`), one per environment: atespace, the micro-VM (`microvm`) or gVisor sandbox, MEMORY snapshots, worker selector, and the model the environment serves. The first two serve `triage-model` from the fake model; `binding-coordinator.json` serves `gpt-5.4-mini` on Azure OpenAI and gates the coordinator's calls to reviewer. See [The binding and kmx](#the-binding-and-kmx). `up.sh` refuses an atespace other than `agentsuite-demo`, the one `deploy/` uses. |
| `render/`, `cmd/emit` | `emit bundle` stages the agent layer (manifest and instructions) for the image. `emit render` writes the ActorTemplate, the harness registration, the daemon's harness flag and its model flags (`agentsessionsd.model-args`, from the binding), and refuses a binding whose served model is not exactly the one the agent pins; `-tier memory` renders the memory tier. `emit check-binding` runs the same binding checks before an image exists. `emit invokes` prints the daemon's invocation policy, with the binding's approval edges. `emit validate` runs kaimahi's `agentsuite.ValidateDirectory` over a suite and prints the suite's digest and the kaimahi module version it ran. `emit check-model` refuses model settings `agentsessionsd` would refuse at startup. |
| `agent/`, `cmd/harness`, `Dockerfile.harness` | The harness image. It serves harnesswire on :80 and `/readyz` on :8081. The instructions are the system prompt, and the harness checks them against the digest in the agent manifest. Each model call is `sink.Model`. With `AGENTSUITE_MEMORY=ram` it keeps the conversation in RAM, ignores the history the host sends, and describes itself as `REQUIRES_MEMORY_SNAPSHOT`. |
| `demo/` | The recorded demo: `driver.sh`, `smoke-microvm.sh`, `present.awk`, `demo.tape`, `record.sh`, `chapters.py`, the build and run diagrams (`build.mmd`, `run.mmd`, checked arrow by arrow in `diagrams.md`, spliced in by `splice.py`), the helpers its screens run (`split-view.py`, `trace-turn.sh`, `trace-hop.py`, `image-files.sh`, `wait-golden.sh`), `bat.conf` and the tools image (`Dockerfile.tools`). |
| `fakemodel/`, `cmd/fakemodel` | A deterministic OpenAI-compatible endpoint. Its reply lists the user's issues so far, and `GET /stats` reports how many calls it has served. |
| `cmd/replay` | Reads the journal over the Sessions API, verifies the chain, and runs the controller's replay against the session's actor through the router. |
| `deploy/` | The micro-VM and gVisor WorkerPools, the fake model, and `agentsessionsd` (substrate build), rendered once per tier. |

The suite is validated with kaimahi's `agentsuite` package, imported as a Go module at kaimahi
`9c5c993`, not copied. `render` also loads kaimahi's own `testdata/minimal` suite (see the tests),
the files `suites/kaimahi-minimal` copies from that commit. `demo.sh` uses its own fixture by default
because kaimahi's fixtures declare only `linux/amd64`, and a kind node on an arm64 Mac needs an arm64
image. On amd64, `AGENTSUITE_DEMO_SUITE=suites/kaimahi-minimal AGENTSUITE_DEMO_AGENT=writer` selects
kaimahi's.

### The mapping that `emit render` applies

| ActorTemplate field | Value |
|---|---|
| `metadata.name` | `<suite>-<agent>-<hash8>`, where `hash8` is the first 8 hex of `sha256(rendererVersion + "\n" + image + "\n" + binding digest)` (kmx's formula), `rendererVersion` is `agentsessions-poc.actortemplate/v2` and `image` the full digest-pinned reference (the same digest under another registry is another template), with `"\nmemory"` appended for `-tier memory`. The binding digest covers the served model, the resources and the approval edges. The name is a DNS label of at most 63 characters, and only suite and agent are truncated. |
| `metadata.atespace` | `binding.atespace` |
| `containers[0].image` | The harness image: an OCI reference of at most 512 bytes, pinned by a sha256 digest, without a tag. |
| `containers[0].env` | `HARNESS_ADDR=:80`, `HARNESS_READYZ=:8081`, `AGENT_DIR=/agent`, `AGENTSUITE_APPROVAL` (the binding's gated targets for this agent) and `AGENTSUITE_MEMORY=ram` for `-tier memory`. No model override: the harness requests the model its agent pins, and refuses to start if the retired `AGENTSUITE_MODEL` is set. No model credentials: they stay with the host. |
| `containers[0].wakeupProbe` | `GET /readyz` on 8081 |
| `containers[0].securityContext` | `capabilities: {}`, adding none |
| `resources` | `binding.resources` (`cpu` and `memory`, both required), on the template, not the container: substrate books these limits against worker capacity and sizes the sandbox by them (a micro-VM gets that many vCPUs, rounded up, and that much RAM less the VMM reserve, 128 MiB by default; a gVisor sandbox gets them as its cgroup limits), per `cmd/ateapi/internal/controlapi/workflow_resume.go` `actorResourceLimits` and `internal/sizing/sizing.go` at the pinned substrate. |
| `sandboxConfig` | `binding.sandbox`: `SANDBOX_CLASS_MICROVM`, `microvm` (or `SANDBOX_CLASS_GVISOR`, `gvisor-default`) |
| `snapshotConfig` | `preferredFidelity` from `binding.snapshot.fidelity`; `-tier memory` refuses any fidelity but `MEMORY`, since substrate cold-boots `ROOTFS` and `VOLUMES` snapshots without the RAM that tier keeps its conversation in. `storageLocation` is `binding.snapshot.storageLocation` + `<name>/`. |
| `workerSelector` | `binding.workerSelector` |

The harness registration is a `HarnessRegistration` with `spec.substrate {atespace, template}`,
`STATELESS_REPLAY` and `descriptor_id: triage`. `agentsessionsd` serves the HarnessRegistry API
(`-registry-addr`) but refuses a substrate placement there (`registeredPlacer` in
`internal/agentsessionsd/registry_listener.go`), so
the registration is applied as `-substrate-harness=triage=<atespace>/<template>`. The memory tier's
is named `triage-memory`, is `REQUIRES_MEMORY_SNAPSHOT`, and is applied as
`-substrate-harness=triage-memory=<atespace>/<template>,memory`.

## Agent-to-agent invocation: the coordinator suite

`suites/coordinator-workers/` is kaimahi's `agentsuite/testdata/coordinator-workers` at kaimahi
`75f27d6`, copied byte for byte (`SOURCE` lists each file's blob id and sha256). `coordinator`
declares `invokes` edges to `writer` and `reviewer`, each `maxConcurrent: 1`, `maxDepth: 1`
(kaimahi `agentsuite/spec.md` section 8.1). This section is the design; it was written before the code.

### The primitive: a host-executed tool call that drives a child session

- The coordinator harness asks for a delegation as a tool call, `agentsuite.invoke`, with arguments
  `{"agent": "<target>", "task": "<text>"}`, through `sink.ToolCall`. The call and its result are
  `TOOL_CALL` and `TOOL_RESULT` records in the coordinator's journal.
- The host executes the call (`placement.WithToolExecutor`, a `controller.ToolFunc`). The executor
  creates a child session on the target's harness through this daemon's own Sessions API and runs one
  turn there with the task as input. The tool result carries the child's session uid, harness and
  final output. The child's `EXECUTION_START` config records the parent session, the tool call id, the
  edge and the remaining depth, so each journal names the other.
- The executor enforces the suite: an undeclared edge, a call that would exceed the edge's
  `maxConcurrent` (in-flight invocations of the target by the declaring agent, across all of its
  sessions: kaimahi `agentsuite/spec.md` 8.1 bounds "concurrent invocations of this target by the
  declaring agent"), and a chain deeper than `maxDepth` are refused with
  an error result before any child session exists.
- The child session is created in the calling session's project. Its creation and turn are bounded
  by the executor's timeout (`invoke.DefaultTimeout`, 5 minutes), so a hung worker cannot hold the
  caller's turn and its `maxConcurrent` slot indefinitely; the caller's model gets an error result.
- Replay and resume never re-run a child: the recorded `TOOL_RESULT` is served from the journal, as
  for any host-mediated tool.

Alternatives considered:

| Option | Why not here |
|---|---|
| Exec on a long-lived session of the target, shared across calls | The worker would see every caller's history; one session per call keeps the child's journal to the delegated task. A follow-up call could reuse its child; not needed for this suite. |
| Fork of the coordinator onto the target harness | A fork carries the parent's history; a worker should get the task only. Fork is also a lineage (`parent_uid`) that means "same conversation", not "called by". |
| A new Sessions RPC (`Invoke`) or a child-session field on `Session` | A wire and storage change for a PoC; the tool-call path already gives a journaled, replay-safe, approval-capable effect. Recorded as an open question. |
| The harness calls the target's Sessions API itself | The call would bypass the journal (no `TOOL_CALL`/`TOOL_RESULT`), the approval gate and the edge checks, and the harness would need network access and credentials to the daemon. |
| Native model function calling (`ModelRequest.Tools`) | `model/openai` refuses tool definitions today (`buildRequest`: "tool definitions and tool choice are not supported yet"). The coordinator asks the model for a one-line JSON delegation instead and turns it into the tool call. |

Registration: the three agents are three `-substrate-harness` entries on one `agentsessionsd`, not
`HarnessRegistry` registrations. On this base the registry's placer factory refuses a substrate
placement (`internal/agentsessionsd/registry_listener.go`, `registeredPlacer`: "substrate placement is
not supported by agentsessionsd"). A daemon used to take one `-substrate-harness`, because actors are
named by session uid in one atespace; each entry now names its own actor atespace (`,actors=<atespace>`),
and two entries may not share one.

### Approval of a delegation

- Each environment's binding chooses which edges need approval (`approval.invokes`, each a
  `caller` and an `agent`). `emit render` sets the caller's gated targets in its template, so the
  coordinator harness sends that call with mediation `REQUIRES_APPROVAL`; others are
  `CONTROLLER_MEDIATED`. `emit invokes` marks the same edges in the daemon's policy. Both refuse a
  gate on an edge the suite does not declare.
- The host's executor refuses a call to an approval edge that arrives without `REQUIRES_APPROVAL`,
  so a harness cannot skip the gate by choosing its own mediation.
- On the gated call the host (#131/#132) journals `TOOL_CALL` then `APPROVAL_REQUEST`, closes the
  harness stream, snapshots the actor and commits `SUSPEND`. The Exec ends with the session
  `EXEC_AWAITING` and `pending_approval` set.
- `agentctl approve` or `agentctl deny` commits `APPROVAL_RESULT`, then resumes. On approval the
  executor runs the reviewer child session and the receipt is the reviewer's output; on denial the
  receipt is `APPROVAL_DENIED`, no child session is created, and the coordinator's model gets the
  denial as the tool result and finishes without the review.
- Tier: the gate exists only for `STATELESS_REPLAY` harnesses (`api/harness.proto`: "The host and
  bridge implement durable approval/park for STATELESS_REPLAY only"; `controller.ErrApprovalUnavailable`
  otherwise). The coordinator is therefore rendered stateless. The workers are stateless too: their
  sessions are one-turn children and nothing needs their RAM.

### What implements it

| Path | What it is |
|---|---|
| `invoke/` | The host executor: `Policy` (edges from the suite, gates from the binding), `Executor.For(agent)` as a `controller.ToolFunc`, the edge, mediation, `maxConcurrent` and `maxDepth` checks, and the child session. `suite_test.go` runs the pinned suite's three agents in process (local backends, the real controller and approval engine, gates from `binding-coordinator.json`) through an approve and a deny. |
| `agent/coordinator.go` | The coordinator loop: the model asks for a delegation as one line of JSON; the harness makes the `agentsuite.invoke` call with id `<execution>-invoke-<n>`, stable across replay and resume. |
| `cmd/agentsessionsd` | The substrate build of `agentsessionsd` plus the executor (`internal/agentsessionsd.WithToolExecutors`). The edges come from `AGENTSUITE_INVOKES`, which `emit invokes -binding` prints. |
| `emit render -actors` | Sets `AGENTSUITE_APPROVAL` in the coordinator's template from the binding and `,actors=<atespace>` on each daemon flag. |
| `demo/coordinator.sh` | `setup`: this environment's binding (one copy per agent atespace) checked against the suite, kind and substrate (`up.sh --substrate-only`), one image and ActorTemplate per agent, one daemon with three `-substrate-harness` flags and the binding's model flags. `probe`: one request approved and one denied through `agentctl`; ends with `PROBE OK`. `show`: the recorded demo's eight chapters (`demo/screen.sh` draws them as `driver.sh` does). `reset`: deletes the session actors and restarts the daemon (its journal is an `emptyDir`), so `show` can run again. |
| `demo/coordinator-record.sh`, `demo/coordinator.tape`, `demo/invoke.mmd` | The recording: `coordinator.sh show` on the host through `hq remote-test` under vhs, the invocation diagram spliced in, then `collect` writes the video's README, logs and checks. |

### Gaps in the invocation PoC

- A child session keeps its actor running after its one turn: nothing suspends it, so each
  delegation holds a micro-VM until the session is suspended or its actor deleted
  (`coordinator.sh reset` deletes them). Observed in the probe: every child actor was still
  `ACTOR_STATE_RUNNING` after its turn.
- Child creation is not idempotent: if the host crashes after the child turn but before the
  `TOOL_RESULT` commits, recovery runs the executor again and creates a second child.
  `CreateSession` has no client-chosen id or idempotency key to deduplicate on.
- `maxConcurrent` is counted in memory per declaring agent and per daemon. Two daemons sharing a
  journal each allow `maxConcurrent`, and a call in flight is forgotten when its daemon exits.
- The executor reaches the daemon's own Sessions API over plaintext loopback without
  authentication, like every other caller of this PoC's daemon. The child session's labels
  (`agentsuite.invoked-by`) are client-set metadata; the tamper-evident link is the child's first
  `EXECUTION_START` config and the parent's `TOOL_RESULT`.
- Tool definitions are not offered to the model (`model/openai` refuses them), so a delegation is a
  JSON reply the harness parses. A reply that is not exactly one JSON object is the final answer.

## Requirements

- docker, kind, kubectl, ko, go 1.27, git, python3 and curl.
- The kind lane of agent-substrate at the commit `.github/workflows/substrate-e2e.yml` pins. `up.sh`
  clones it into the work directory, `$TMPDIR/agentsuite-demo` (set `AGENTSUITE_DEMO_WORK` to move it).
- Every image is built for the architecture `docker info` reports, not the host's, so an amd64
  Docker VM on an arm64 Mac gets amd64 images (not tested). For this, `up.sh` runs substrate's
  `ate-setup` itself with `--ko-default-platforms`; `hack/install-ate-kind.sh` would build
  ate-system for the host.
- Docker must publish ports on this machine's loopback. A remote daemon (`DOCKER_HOST`) is not
  supported: the registry (`localhost:5001`) and the kind API server publish only on the Docker
  host's loopback, and `ko` pushes and `kubectl` connects from this machine.
- For micro-VMs, `/dev/kvm` in the Docker host: `up.sh` probes it with
  `docker run --device /dev/kvm`, as substrate's `create-kind-cluster.sh` does before it mounts it
  into the node. The first micro-VM `up.sh` on a machine assembles the kata assets (downloads), then
  stages them in the cluster's object store and applies the `microvm` SandboxConfig
  (`hack/install-microvm-deps.sh`); later runs skip that while the SandboxConfig exists.
- About 4 GiB free in the Docker VM. The recorded run used an 8 GiB VM with no other kind cluster
  running. The first `up.sh` on a machine also pulls the ate-system images, so it takes longer than
  the 2.5 minutes recorded.
- `up.sh` writes its own kubeconfig and never changes your current context. The kubeconfig, the
  rendered composition and the rendered `agentsessionsd.yaml` are in `cluster-<name>/` under the
  work directory, one per cluster; the tools in `bin/` and the substrate checkout are shared.
- The cluster is `agentsuite-demo`; set `AGENTSUITE_DEMO_CLUSTER` to use another name (lowercase
  letters, digits, `.` and `-`). The scripts ignore an exported `KIND_CLUSTER_NAME` (the
  conformance recipe in [docs/substrate-conformance.md](../../docs/substrate-conformance.md) sets
  one for its own cluster).
  `up.sh` reuses a cluster that already has the name; `down.sh` deletes it only if `up.sh` created
  it, which it records in the work directory, and otherwise refuses and prints the `kind delete`
  command. Both scripts stop without changing anything when `kind get clusters` fails, and `up.sh`
  refuses to create the cluster when kind does not list it but its `-control-plane` container
  exists, since substrate's `create-kind-cluster.sh` deletes a cluster of that name first.
- `up.sh` records its cluster as soon as the container exists, also when `create-kind-cluster.sh`
  fails or `up.sh` is interrupted (`INT`, `TERM`) after kind created it, and how far its setup got.
  A rerun after a failed first `up.sh` installs ate-system if only that step failed, or recreates
  the cluster if `create-kind-cluster.sh` did not finish; it does not install into a cluster it did
  not create. A cluster it created whose setup record is missing or unreadable is refused rather
  than recreated, since recreating an installed cluster would delete the daemon's session journal:
  run `./down.sh`, then `up.sh` again.

## A real model

```bash
AGENTSUITE_MODEL_BASE_URL=https://api.openai.com/v1 AGENTSUITE_MODEL=<model> MODEL_API_KEY=<key> ./up.sh
AGENTSUITE_MODEL_BASE_URL=https://api.openai.com/v1 ./demo.sh
```

Azure OpenAI serves a deployment at `/openai/deployments/<deployment>/chat/completions` and pins
its API version in an `api-version` query parameter, with the key in an `api-key` header. Put the
path and the query in `AGENTSUITE_MODEL_PATH` and the header name in `AGENTSUITE_MODEL_AUTH_HEADER`;
`up.sh` passes them to `agentsessionsd` as `-model-path` and `-model-auth-header`, and the base URL
stays free of a query:

```bash
export AGENTSUITE_MODEL_BASE_URL=https://<resource>.openai.azure.com
AGENTSUITE_MODEL=<deployment> \
  AGENTSUITE_MODEL_PATH='/openai/deployments/<deployment>/chat/completions?api-version=2024-10-21' \
  AGENTSUITE_MODEL_AUTH_HEADER=api-key \
  MODEL_API_KEY=<key> ./up.sh
./demo.sh
```

Quote the path, since `?` is a shell glob character. `AGENTSUITE_MODEL` is the deployment name;
the harness sends it as the request's `model`, and Azure routes by the deployment in the path. The
path's only query parameter may be one date-shaped `api-version` (`2024-10-21`,
`2025-04-01-preview`), and it may not contain `@` or a fragment, start with `//` or end with `:`.
Unset, the path is `/chat/completions` and the header is `Authorization` (a bearer token), the
daemon's defaults.

A resource with key authentication disabled takes a Microsoft Entra token for the
`https://cognitiveservices.azure.com` resource as a bearer token: leave
`AGENTSUITE_MODEL_AUTH_HEADER` unset and pass the token as `MODEL_API_KEY`. The daemon reads it once
at start and does not refresh it, so turns fail once the token expires (about 24 hours for a
managed-identity token); rerun `up.sh` with a fresh one, which restarts the daemons and drops their
journals. The [recorded demo](#the-recorded-demo) runs this way, with a managed identity's token.

`up.sh` refuses `AGENTSUITE_MODEL_BASE_URL` without `AGENTSUITE_MODEL`; `AGENTSUITE_MODEL`,
`AGENTSUITE_MODEL_PATH`, `AGENTSUITE_MODEL_AUTH_HEADER` or `MODEL_API_KEY` without
`AGENTSUITE_MODEL_BASE_URL` (only the URL selects a real provider, so the run would otherwise switch
to the fake model); any of the four settings with whitespace or a control character in it, or ending
with `:` (the manifest would read it as a YAML map key); a URL that is not `http` or `https` or that
carries userinfo, a query or a fragment; with `MODEL_API_KEY` set, a plain `http` URL whose host is
not a loopback address, `localhost`, or an in-cluster service ending in `.svc` or
`.svc.cluster.local`, as `agentsessionsd` does; a path that carries a query other than one
date-shaped `api-version`; a header that is not a header name; and any combination `agentsessionsd`
would refuse at startup. It does so before it changes the cluster, so a misconfigured rerun leaves
the running daemon, its journal and an existing key in place. `emit check-model` makes the last
checks: it builds the model client with the daemon's own constructor (`internal/modelconfig`), so it
refuses the same URLs and paths (no scheme or host, a bad port or `%` escape). The values are copied
into the Deployment as written. The URL is also printed, the daemon logs the URL and the path, and
both are written to `${STATE}/agentsessionsd.yaml`, so a credential in either would sit outside the
Secret. `check-model` refuses these forms, which can carry one: userinfo, a query or a fragment in
the URL; a query other than `api-version`, `@` or a fragment in the path. Its refusals do not repeat
the URL or the path. It cannot tell a credential placed in a path segment, which is copied like any
other. `up.sh` applies the key to the `agentsessionsd-model` Secret through stdin, so it never
appears in argv or a file, and a failed apply leaves the stored key unchanged.
`agentsessionsd` reads the key only when it starts, so `up.sh` stamps the Secret's version into the
Deployment's pod template: a new or deleted key restarts the pod, which also drops its emptyDir
journal, and an unchanged key leaves it running. It does not build or deploy the fake model. It
points `agentsessionsd` at the endpoint and renders from this environment's binding: the committed
binding with its model set to `AGENTSUITE_MODEL`, `AGENTSUITE_MODEL_BASE_URL` and
`AGENTSUITE_MODEL_PATH` (`common.sh` `write_binding`). The harness requests the model its agent
pins, so `up.sh` refuses, before any cluster change, a provider model the agent does not pin
(`emit check-binding`): the suite pins the model, and a different one is a new suite version and a
new evaluation, not a render-time override. `driver.sh`, the v3 recording, ran
`suites/kaimahi-minimal` (`example-model`) against Azure through the override this version removed,
so its real-model run now stops there until that suite has a version pinning the served model.
`demo.sh` then skips the checks that depend on the fake's replies and counter, and does
not port-forward the fake. It still checks the journal in both modes: the inputs in order, one model
call per turn, the suspend and resume records, the fork child's prefix, and that the replay
re-executed every completed turn; in real-provider mode it claims only those, not what the replies
say. The replay still fails if the controller sees a model call.

The Secret, and so the running daemon, follows the latest `up.sh`. A real-provider `up.sh` without
`MODEL_API_KEY` deletes any stored key, for endpoints that take none, so export `MODEL_API_KEY`
again on every real-provider run. A fake-provider `up.sh` deletes it too, so the fake never receives
a key.

This path was run against the in-cluster fake standing in as the endpoint, with a dummy key, and by
the recorded demo against an Azure OpenAI deployment (`gpt-5.4-mini`, `api-version`
`2025-04-01-preview`) with an Entra token in the default `Authorization` header. The `api-key`
header has not been run against a live deployment.

## The binding and kmx

The binding is the per-environment half of a deployment, outside the suite: the suite pins the
exact model, the binding says where it is served and where sessions are placed, and `emit render`
refuses a binding whose `model.name` is not exactly the agent's pinned model (no case folding,
prefix or version matching). A different model is a new suite version and a new evaluation
(`suites/coordinator-workers-v2`). The shape follows kmx's `AgentSessionsBinding`
(kaimahi `internal/kmx/runtime/agentsessions_binding.go` and `internal/kmx/agentsessions/render.go`
on branch `aramase/f/kmx_agentsessions_binding`, compared at `fc03a7c`, `render.KmxCommit`): the same
`apiVersion`, `kind` and fields, the same required `model` (`name`, `provider: openai`, optional
`baseURL`, `secretRef`) and required `resources` (`cpu` and `memory`, each a positive quantity of at
most 32 bytes within substrate's 999.999 cores and 2^63-1 bytes, parsed by apimachinery's
`resource.ParseQuantity`, as kmx and substrate parse them), rendered as the ActorTemplate's
`resources` as kmx renders them, the same `workerSelector` rule (at most ten labels, qualified names,
label values), the same cross-field and `snapshot.storageLocation` refusals (`ROOTFS` only on
`microvm`; an object-storage URI of at most 1024-65 bytes with no credentials, query, fragment or
`\`, and no empty segments or segments of only `.` and spaces), the same object name, Secret key,
base URL and harness image rules, the same template name formula (fed this renderer's own version, so
the names differ; see the table), and the same digest (JCS of the binding's fields, with an empty
`workerSelector` the same as an omitted one). The results were checked by running kmx's code at
`fc03a7c` on the same inputs: `AgentSessionsBindingDigest` on `binding-microvm.json` and
`binding.json`, `TemplateName` on a pinned image and its mirror, and `ValidateAgentSessionsBinding`
on the boundary values `render_test.go` uses (959 and 960 storage bytes, quantity and label limits);
`render_test.go` pins the digests and names. Every difference that remains:

| Difference | Why |
|---|---|
| `model.path` (PoC only) | An Azure OpenAI deployment's completions path carries its `api-version` query, and kmx's `model.baseURL` refuses a query. `TODO(spike): does Azure's v1 endpoint (<resource>/openai/v1, model in the body) work for this deployment with the managed-identity token, so kmx's shape needs no path?` |
| `approval.invokes` (PoC only) | kmx has no approval field, and AgentSuite has none on an invokes edge. The PoC treats which delegation needs an operator as per-environment policy. kmx's strict parser refuses both PoC-only fields. |
| `approval.invokes` is sorted before the digest | It is a set; kmx has no such field to order. |
| Template names differ from kmx's for the same image and binding | The formula is kmx's, `sha256(rendererVersion + "\n" + image + "\n" + bindingDigest)` over the full image reference, but the renderer version is the PoC's own (`agentsessions-poc.actortemplate/v2`, kmx's is `kmx.agentsessions-actortemplate/v2`): this renderer's ActorTemplate is not kmx's (its container env sets `AGENT_DIR` and `AGENTSUITE_APPROVAL`; kmx's sets `HARNESS_KIND` and `HARNESS_MODEL`), and kmx folds the version in so that two mappings never share a name. The memory tier also appends `"\nmemory"`; kmx has no memory tier. |
| The memory tier refuses a `ROOTFS` or `VOLUMES` binding | kmx renders one stateless chat member, for which any fidelity is valid. The PoC's `-tier memory` keeps the conversation only in RAM, which substrate's `ROOTFS` and `VOLUMES` snapshots do not carry, so it requires `MEMORY`. |
| The registration names the agent, not `chat` | kmx registers its member with `descriptor_id: chat` (the built-in chat harness in harnessnode); the PoC's harness serves the suite's agent, so `descriptor_id` is the agent id. |
| Render output, and the substrate revision it targets | kmx lift is plan-only: it prints one JSON document (the ActorTemplate in proto field names, its `templateDigest`, the canonical registration and its `registrationSpecDigest`, and the host model references) checked against substrate `f4f7b806` (`SubstrateAPICommit`). The PoC writes `actortemplate.json` (protojson of the pinned substrate `96dffb2`'s `ateapipb.ActorTemplate`), the registration, the daemon flag and the model flags, and `up.sh`/`coordinator.sh` apply them. Both templates set the same fields; their values differ only in the name and the container env, as above. |
| JSON only, no credential-shape scan | kmx reads YAML or JSON, refuses anchors, aliases, invalid UTF-8 and credential-shaped values in every field; the PoC decodes strict JSON (which replaces invalid UTF-8) and keeps credentials out by field shape: names, URLs, the model path, the storage location and the Secret key are checked, and errors never repeat a URL or location. |
| One binding per agent atespace | kmx lift renders one chat member into `binding.atespace`. The coordinator's three agents share one daemon, whose session actors are named by session uid, so `coordinator.sh setup` writes one copy of the binding per agent with `atespace` set to `agentsuite-<agent>` (three digests). |
| The binding's endpoint is filled in at setup | `binding-coordinator.json` names `https://example.openai.azure.com`; `setup` writes this environment's copy with the operator's `AGENTSUITE_MODEL*` values, so no Azure resource name is committed. |
| No eval evidence | kmx eval takes one chat member and records the binding digest in its evidence, and kmx lift requires that evidence. A suite with invokes edges has no kmx eval path yet, so this PoC renders without evidence. |
| `model.secretRef` is checked, not rendered | `coordinator.sh` requires `agentsessionsd-model`/`api-key`, the Secret it writes; the Deployment's `secretKeyRef` is fixed. |

## Gaps this PoC works around

- **No declarative harness registration.** `agentsessionsd` does not serve the `HarnessRegistry` API
  (agentsessions#70), so `emit` writes the registration and the daemon flag that applies it, and
  `up.sh` puts that flag in the Deployment. Registering another agent means a redeploy.
- **Replay is not a Sessions API call.** The `Replay` RPC streams the journal; it does not re-run the
  harness. The re-execution (`controller.Replay`) only exists in-process, so `cmd/replay` runs it on
  the client against the session's actor.
- **Nothing authenticates the host to the harness.** The harness serves harnesswire on :80 with no
  caller authentication, and `agentsessionsd` reaches it over cleartext h2c through the
  atenet-router, naming the actor only by the `ate-target-actor` header. Substrate's kind install
  runs the router in its default `--ingress-auth-mode=deprecated-insecure`, which keeps that
  plaintext listener open to any client. So anything that can reach the router can act as the host
  for any session's actor and feed it model results. `cmd/replay` relies on exactly this: it drives
  the session's actor from the laptop through a port-forward to the router. Substrate's threat
  model says the router reaches the actor through `atunnel` over mTLS, which accepts only the
  router; this demo did not test that path. What would close it:
  - router authentication: run the router with `--ingress-auth-mode=static-mtls`, which closes the
    plaintext listeners and requires a client certificate with an allowed SPIFFE ID, and give
    `agentsessionsd` (and `cmd/replay`) a TLS client certificate for the router connection, which
    neither supports today. At substrate 96dffb28, `ate-setup` has no option for this mode: the
    `ate-setup --ingress-auth-mode` flag and `components/router-static-mtls` that substrate's
    AGENTS.md names are not in the tree, so it means editing the router Deployment's args;
  - host authentication on the harness: a per-session credential the host presents and the harness
    checks. The router's allowlist lets any listed client reach any actor, so it cannot keep one
    session's host from driving another session's actor.
- **Nothing authenticates callers of the Sessions API either.** The `agentsessionsd` ClusterIP
  Service serves every Sessions RPC over plaintext gRPC with no credentials, as
  [docs/security.md](../../docs/security.md#what-is-not-protected) says of every path in the
  reference implementation. Any pod that can reach the Service can read, drive, fork and suspend
  every session, and with a real provider spend its model budget. `demo.sh` reaches it through a
  port-forward. Run this only on a disposable cluster whose workloads you trust; what would close it
  is authentication and authorization on the Sessions API, which the repo does not have yet.
- **The substrate binding is configuration, not session state.** The `-substrate-atespace` and
  template come from the daemon's flags at each start, and the session store does not record them.
  Changing them while sessions exist strands those sessions' actors in the old atespace. The tier is
  declared in the flag (`,memory` for `REQUIRES_MEMORY_SNAPSHOT`) and not checked against the image's
  `Describe`, so a stateful image declared without it loses its RAM state on fork. This demo's
  harness is stateless and declared so. A memory-tier session that has started a turn and lost its
  actor is refused (`FAILED_PRECONDITION`) rather than given a fresh actor. See
  [Substrate placement binding](../../docs/substrate-conformance.md#substrate-placement-binding).
- **The journal lives in an emptyDir.** It lasts as long as the `agentsessionsd` pod.
- **The AgentSessionsBinding follows kmx's, with two additions.** See
  [The binding and kmx](#the-binding-and-kmx).
- **There is no build profile.** The image is the harness binary plus the agent layer on `scratch`.
  It is not built from the suite's build profile (`runtimeBase` and `harness` are placeholder
  digests), and nothing records that it came from the composition.
- **The emitter is v0.** It accepts single tool-free agents only, whose model protocol is
  `openai-compatible` (the protocol of `agentsessionsd -model`). The agent id becomes the harness
  name, so `chat` and `echo`, which `agentsessionsd` serves itself, are refused. No `durable_dir`
  volume is rendered, since this agent writes nothing, and no EgressPolicy is rendered, since it
  calls nothing.
- **gVisor where there is no KVM.** An arm64 Mac's kind node has no KVM, so it runs the gVisor
  class (`AGENTSUITE_DEMO_SANDBOX=gvisor`); micro-VM is the default and the class the recorded demo
  ran on.
- **One substrate harness per daemon.** `agentsessionsd` serves one `-substrate-harness`, since
  actors are named by session UID alone, so each tier gets its own Deployment, journal and Service.
- **The memory tier replays and forks less.** Its conversation is in RAM, so `cmd/replay` (which
  re-runs turns on the session's live actor) is not run against it, and a fork can only be taken at
  the log head, by cloning the parent's memory snapshot (the placer refuses an earlier seq). Fork at
  an earlier turn, and replay, use the stateless tier.
- **The model token is not refreshed.** With an Entra token as `MODEL_API_KEY`, the daemon sends the
  token it started with until it expires; a token source in the daemon would close this.
- **ate-api trust.** `agentsessionsd` verifies the ate-api certificate against the
  `servicedns.podcert.ate.dev` ClusterTrustBundle that substrate's kind install creates, through a
  projected volume. It authenticates with the pod's projected ServiceAccount token. Which identities
  the kind install's ate-api accepts was not examined beyond this working.
- **Port-forwards.** `demo.sh` reaches agentsessionsd, the router (for the replay) and, with the
  fake provider, the fake model through `port-forward`.

## The recorded demo

This section describes the v3 recording. Its real-model run does not run on this branch:
`driver.sh setup` stops at its first `up.sh`, whose `emit check-binding` refuses the binding before
any cluster change, because `suites/kaimahi-minimal` pins `example-model` and the Azure deployment
serves `gpt-5.4-mini`, and the render no longer overrides the model. It runs again once
kaimahi-minimal has a version pinning the served model. Its `show` also predates the v2 renderer:
it checks `actortemplate.json` by line number and captions the binding's resources as container
limits, where the template now carries them as its own `resources`. The current recording is the
coordinator demo (`demo/coordinator.sh`, `demo/coordinator-record.sh`).

`demo/driver.sh` is a short demo for a recording: how kaimahi's minimal suite (agent `writer`)
becomes a substrate ActorTemplate, how one turn flows, and the session lifecycle on micro-VMs, with a
real model on Azure OpenAI. It runs on a Linux host with KVM and Docker (all of it there: kind, the
cluster, the port-forwards), in two parts:

- `driver.sh setup`, off camera: `up.sh --substrate-only` (its output must say substrate enabled
  `micro-VM (kata + cloud-hypervisor)`), the micro-VM smoke test (`smoke-microvm.sh`: substrate's
  counter-microvm actor boots, suspends to a MEMORY snapshot and resumes with its RAM count and UID;
  it must print `SMOKE OK`), the suite's files against `SOURCE`, `up.sh --templates-only`, an Entra
  token from the instance metadata endpoint (never printed; only its expiry is kept), `up.sh`, and the
  `agentctl` and `replay` binaries, and the harness binary copied out of the harness image. The
  stateless tier's ActorTemplate is only rendered (`AGENTSUITE_DEMO_SKIP_TEMPLATE=stateless`):
  agentsessionsd starts with its `-substrate-harness` flag while the template does not exist, since
  it reads the template only when it creates a session's actor, and setup checks the Deployment is
  ready without it. It ends with `SETUP OK`; the full output is `setup.log`.
- `driver.sh show`, on camera: eleven chapters. Each screen is a one-line caption and the commands,
  with at most five output rows (seven on the trace screen); a file screen prints its file whole with
  `bat` (`demo/bat.conf`), a page at a time if it is taller than the terminal. The full output of
  every command goes to `ch-N.log`. `driver.sh reset` removes what show created (the sessions'
  actors and the stateless template), so show can run again on the same cluster.

| Chapter | What it shows |
|---|---|
| 1 | What we start with: kaimahi's minimal suite file by file (manifest, agent, instructions and their sha256, composition), `emit validate` (kaimahi's own `agentsuite` package), and the draft `AgentSessionsBinding` |
| 2 | The translation: `emit render` live, the generated `actortemplate.json` whole, six split screens (`split-view.py`) pairing input lines with the template lines they became, the `-substrate-harness` flag, a second template from `-tier memory`, a refused `-platform linux/arm64`, the image's `/agent` files (`image-files.sh`), the harness refusing tampered instructions by digest, and the build diagram (`build.mmd`) |
| 3 | In the cluster, by hand: the template created and its golden snapshot (`wait-golden.sh`), the template in substrate, agentsessionsd's flag, the micro-VM worker pool |
| 4 | A turn, end to end: the run diagram (`run.mmd`), one real turn and the actor it runs in, then the turn followed through each component's own log (`trace-turn.sh`, `trace-hop.py`) |
| 5 | Chat: a second turn that builds on the first |
| 6 | The journal's last records, with token usage and the hash chain |
| 7 | A session on chapter 2's memory-tier template told a codename, suspended to a MEMORY snapshot and resumed: the codename recalled from RAM |
| 8 | `cmd/replay` of the first session: 2 turns re-run, 0 model calls, the journal unchanged |
| 9 | A fork at the end of turn 1 with a different second turn; both journals side by side |
| 10 | The memory session's actor replaced out of band: the next turn is refused with `FAILED_PRECONDITION` and nothing is journaled |
| 11 | What the AgentSuite proposal needs, then a one-line recap |

Every command on screen is the command line that runs: the driver evaluates it as shown and saves
its full output to the chapter's log. Nothing is filtered after the fact: output is cropped only by
the tool's own flags (`bat -r`, `agentctl replay --from`, `-o jsonpath`) or by a committed helper
named for what it does (`split-view.py`, `trace-turn.sh`, `image-files.sh`, `wait-golden.sh`).
Every claim on screen is also checked, off screen, with the raw output in the log (the highlighted
template lines, the actor's record before and after the resume, the journal before and after the
replay and the guard): the driver stops on the first that does not hold. A wait runs out of
frame: the driver prints `⠿ <what> …`, runs the step, then replaces the line with `✓ <what> in N s`,
and `demo.tape` hides the recording between the two. That status line takes a row on screen but is
the driver's, not the command's output, so it does not count toward the five output rows. The tape
lists those waits in the order the
driver makes them (`scripts_test.go` checks the counts match). `present.awk` renders the driver's
output for vhs: it holds each line on the cursor's line until the next arrives, since vhs's
`Wait+Line` matches the cursor's line and Docker's log stream (which `hq remote-test` relays) passes
only whole lines.

### The two diagrams

`demo/build.mmd` (how the suite becomes a template and a flag) and `demo/run.mmd` (how one turn
travels) are the Mermaid sources; `demo/diagrams.md` lists, for each arrow, where the code does it.
The harness registration `emit render` writes is not applied as an API object: the v3 recording
predates the HarnessRegistry listener, and on this base that listener refuses substrate placements,
so only the flag reaches agentsessionsd. The trace shows one log line per
component of the run diagram for the same turn: agentsessionsd (the substrate adapter runs in it and
logs nothing of its own), ate-api, the worker, the actor's lifecycle in its micro-VM, the router, the
harness's token line, and the journal's `EVENT_MODEL_CALL` record for the model call.

```bash
# On the KVM host, in a checkout, with the tools Dockerfile.tools installs:
export AGENTSUITE_MODEL_BASE_URL=https://<resource>.openai.azure.com AGENTSUITE_MODEL=<deployment> \
  AGENTSUITE_MODEL_PATH='/openai/deployments/<deployment>/chat/completions?api-version=2025-04-01-preview' \
  AZURE_MI_CLIENT_ID=<client id of a managed identity on the host with access to the resource>
examples/agentsuite-substrate/demo/driver.sh setup
examples/agentsuite-substrate/demo/driver.sh show       # on a terminal it renders through present.awk
examples/agentsuite-substrate/demo/driver.sh teardown
```

The token comes from the host's instance metadata endpoint, so the driver needs an Azure VM with
that user-assigned identity. Every line it prints, and every log (`DEMO_LOGS`), passes through a
filter that redacts bearer tokens, JWTs and the metadata response's token field. The on-screen
commands find the tools on `PATH` and
the cluster through the demo's own kubeconfig (`KUBECONFIG`), whose current context the driver checks
is the demo's kind cluster before the first chapter.

`demo/record.sh` runs setup, records `show` with vhs (`demo.tape`, 1920x1080, font size 22, a 130 x
39 terminal), renders `build.mmd` and `run.mmd` with mermaid-cli (through `npx`) and splices them
over the blocks chapters 2 and 4 draw for them (`splice.py`), then writes the video, the logs, its
sha256, a frame from each chapter, the demo files with their sha256 at record time, and a README with
each chapter's start and the checks to `DEMO_OUT`, and tears the cluster down (`DEMO_KEEP=1` keeps
it; `record.sh setup|show|reset|record|collect|teardown` runs one part, `show` being a dry run that
`reset` undoes). The chapter starts come from the video: each chapter draws a progress bar across the
top row, k/11 full for chapter k, and `chapters.py` finds the first frame of each. By default it runs the driver on the
machine vhs runs on (`DEMO_REMOTE=local`); `DEMO_REMOTE=hq DEMO_HOST=<host>` drives a remote host
through `hq remote-test`, a private tool, in the image `Dockerfile.tools` builds there. `DEMO_READ`
(default 3) is how many seconds a result stays on screen; a caption stays 2.5 s before its first
command (and on screen through the rest), each page of a file 6.5 s, the title card 5 s and each
diagram 9 s.
