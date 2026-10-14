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
| `binding-microvm.json`, `binding.json` | Draft `AgentSessionsBinding`s (`kmx.kaimahi.dev/v1alpha1`) for kind: atespace, the micro-VM (`microvm`) or gVisor sandbox, MEMORY snapshots, worker selector. `up.sh` refuses an atespace other than `agentsuite-demo`, the one `deploy/` uses. |
| `render/`, `cmd/emit` | `emit bundle` stages the agent layer (manifest and instructions) for the image. `emit render` writes the ActorTemplate, the harness registration and the daemon flag; `-tier memory` renders the memory tier. `emit check-model` refuses model settings `agentsessionsd` would refuse at startup. |
| `agent/`, `cmd/harness`, `Dockerfile.harness` | The harness image. It serves harnesswire on :80 and `/readyz` on :8081. The instructions are the system prompt, and the harness checks them against the digest in the agent manifest. Each model call is `sink.Model`. With `AGENTSUITE_MEMORY=ram` it keeps the conversation in RAM, ignores the history the host sends, and describes itself as `REQUIRES_MEMORY_SNAPSHOT`. |
| `demo/` | The recorded demo: `driver.sh`, `smoke-microvm.sh`, `demo.tape`, `record.sh` and the tools image (`Dockerfile.tools`). |
| `fakemodel/`, `cmd/fakemodel` | A deterministic OpenAI-compatible endpoint. Its reply lists the user's issues so far, and `GET /stats` reports how many calls it has served. |
| `cmd/replay` | Reads the journal over the Sessions API, verifies the chain, and runs the controller's replay against the session's actor through the router. |
| `deploy/` | The micro-VM and gVisor WorkerPools, the fake model, and `agentsessionsd` (substrate build), rendered once per tier. |

The suite is validated with kaimahi's `agentsuite` package, imported as a Go module at kaimahi
`ccde3079`, not copied. `render` also loads kaimahi's own `testdata/minimal` suite (see the tests);
its files are identical at `ccde3079` and `9c5c993`. `demo.sh` uses its own fixture by default
because kaimahi's fixtures declare only `linux/amd64`, and a kind node on an arm64 Mac needs an arm64
image. On amd64, `AGENTSUITE_DEMO_SUITE=suites/kaimahi-minimal AGENTSUITE_DEMO_AGENT=writer` selects
kaimahi's.

### The mapping that `emit render` applies

| ActorTemplate field | Value |
|---|---|
| `metadata.name` | `<suite>-<agent>-<hash8>`, where `hash8` is the first 8 hex of `sha256(image digest + "\n" + binding digest)`, with `"\n" + model` appended when `emit render -model` overrides the agent's model and `"\nmemory"` for `-tier memory`. The binding digest is the SHA-256 of its JCS form. The name is a DNS label of at most 63 characters, and only suite and agent are truncated. |
| `metadata.atespace` | `binding.atespace` |
| `containers[0].image` | The harness image, which must be digest-pinned. |
| `containers[0].env` | `HARNESS_ADDR=:80`, `HARNESS_READYZ=:8081`, `AGENT_DIR=/agent`, `AGENTSUITE_MODEL` when `-model` is given, and `AGENTSUITE_MEMORY=ram` for `-tier memory`. No model credentials: they stay with the host. |
| `containers[0].wakeupProbe` | `GET /readyz` on 8081 |
| `containers[0].securityContext` | `capabilities: {}`, adding none |
| `containers[0].resources` | `binding.resources` |
| `sandboxConfig` | `binding.sandbox`: `SANDBOX_CLASS_MICROVM`, `microvm` (or `SANDBOX_CLASS_GVISOR`, `gvisor-default`) |
| `snapshotConfig` | `preferredFidelity` from `binding.snapshot.fidelity`. `storageLocation` is `binding.snapshot.storageLocation` + `<name>/`. |
| `workerSelector` | `binding.workerSelector` |

The harness registration is a `HarnessRegistration` with `spec.substrate {atespace, template}`,
`STATELESS_REPLAY` and `descriptor_id: triage`. `agentsessionsd` does not serve the registry, so
the registration is applied as `-substrate-harness=triage=<atespace>/<template>`. The memory tier's
is named `triage-memory`, is `REQUIRES_MEMORY_SNAPSHOT`, and is applied as
`-substrate-harness=triage-memory=<atespace>/<template>,memory`.

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
points `agentsessionsd` at the endpoint, and the harness requests `AGENTSUITE_MODEL` instead of the
fixture's `triage-model`: the ActorTemplate carries it as an env var, so each model gets its own
template. `demo.sh` then skips the checks that depend on the fake's replies and counter, and does
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
- **The AgentSessionsBinding is a draft.** kaimahi does not define the kind yet. `binding.json`
  follows the draft field list, and `snapshot.fidelity` replaces the draft's `onCommit`, which
  substrate no longer has.
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

`demo/driver.sh` is a longer demo for a recording: kaimahi's minimal suite (agent `writer`) on
micro-VMs, with a real model on Azure OpenAI. It runs on a Linux host with KVM and Docker (all of it
there: kind, the cluster, the port-forwards), and prints a banner, each command and its output for
every step:

| Step | What it shows |
|---|---|
| 0 | `/dev/kvm` in Docker, `up.sh --substrate-only`, then `smoke-microvm.sh`: substrate's counter-microvm actor boots, suspends to a MEMORY snapshot and resumes on another worker with its RAM count and UID |
| 1 | Nodes, the `microvm` SandboxConfig, the WorkerPools |
| 2 | The suite's files against `SOURCE` (git blob id and sha256), its manifests, `up.sh --templates-only`, both rendered ActorTemplates |
| 3 | An Entra token from the instance metadata endpoint (never printed), `up.sh`, the daemons' arguments and env, the Secret with its data redacted |
| 4 | A `writer` session: three turns with the daemon's and the actor's logs live; a `writer-memory` session told a codename |
| 5 | `agentctl replay -v`: each record's hash and predecessor, model calls, token usage, and totals |
| 6 | The memory session suspended (a MEMORY snapshot, the worker freed), resumed, and asked for the codename: same actor UID, answered from RAM |
| 7 | `cmd/replay` of the `writer` session: chain verified, 3 turns re-run in the actor, 0 model calls, the journal's model-call count unchanged |
| 8 | A fork at the end of turn 1, driven another way; both histories |
| 9 | The memory session's actor deleted and recreated under its name out of band: the next turn is refused with `FAILED_PRECONDITION`, and no turn completes |
| 10 | `down.sh`; no cluster or container left |

```bash
# On the KVM host, in a checkout, with the tools Dockerfile.tools installs:
export AGENTSUITE_MODEL_BASE_URL=https://<resource>.openai.azure.com AGENTSUITE_MODEL=<deployment> \
  AGENTSUITE_MODEL_PATH='/openai/deployments/<deployment>/chat/completions?api-version=2025-04-01-preview' \
  AZURE_MI_CLIENT_ID=<client id of a managed identity on the host with access to the resource>
examples/agentsuite-substrate/demo/driver.sh
```

The token comes from the host's instance metadata endpoint, so the driver needs an Azure VM with
that user-assigned identity. Every line it prints, and every per-step log (`DEMO_LOGS`), passes
through a filter that redacts bearer tokens, JWTs and the metadata response's token field.

`demo/record.sh` records it with vhs (`demo.tape`) from a Mac that drives the host through
`hq remote-test` (a private tool; `DEMO_REMOTE=local` runs the driver on the machine vhs runs on),
then writes the video, the logs, a README with each step's offset in the video and the video's
sha256 to `DEMO_OUT`.
