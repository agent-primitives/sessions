# AgentSuite agent as an AgentSessions harness on agent-substrate

A proof of concept. One AgentSuite agent is rendered into a substrate ActorTemplate and registered
with `agentsessionsd` as a harness. Its session then runs in a substrate actor, and the demo
suspends, resumes, verifies, replays and forks it. Everything runs on a local kind cluster with no
credentials.

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
| `suite/` | An AgentSuite (`support-desk`) with one tool-free agent, `triage`, and compositions for `linux/amd64` and `linux/arm64`. |
| `binding.json` | A draft `AgentSessionsBinding` (`kmx.kaimahi.dev/v1alpha1`) for kind: atespace, gVisor sandbox, MEMORY snapshots, worker selector. `up.sh` refuses an atespace other than `agentsuite-demo`, the one `deploy/` uses. |
| `render/`, `cmd/emit` | `emit bundle` stages the agent layer (manifest and instructions) for the image. `emit render` writes the ActorTemplate, the harness registration and the daemon flag. |
| `agent/`, `cmd/harness`, `Dockerfile.harness` | The harness image. It serves harnesswire on :80 and `/readyz` on :8081. The instructions are the system prompt, and the harness checks them against the digest in the agent manifest. Each model call is `sink.Model`. |
| `fakemodel/`, `cmd/fakemodel` | A deterministic OpenAI-compatible endpoint. Its reply lists the user's issues so far, and `GET /stats` reports how many calls it has served. |
| `cmd/replay` | Reads the journal over the Sessions API, verifies the chain, and runs the controller's replay against the session's actor through the router. |
| `deploy/` | The gVisor WorkerPool, the fake model, and `agentsessionsd` (substrate build). |

The suite is validated with kaimahi's `agentsuite` package, imported as a Go module at kaimahi
`ccde3079`, not copied. `render` also loads kaimahi's own `testdata/minimal` suite (see the tests).
The demo uses its own fixture because kaimahi's fixtures declare only `linux/amd64`, and a kind node
on an arm64 Mac needs an arm64 image.

### The mapping that `emit render` applies

| ActorTemplate field | Value |
|---|---|
| `metadata.name` | `<suite>-<agent>-<hash8>`, where `hash8` is the first 8 hex of `sha256(image digest + "\n" + binding digest)`, with `"\n" + model` appended when `emit render -model` overrides the agent's model. The binding digest is the SHA-256 of its JCS form. The name is a DNS label of at most 63 characters, and only suite and agent are truncated. |
| `metadata.atespace` | `binding.atespace` |
| `containers[0].image` | The harness image, which must be digest-pinned. |
| `containers[0].env` | `HARNESS_ADDR=:80`, `HARNESS_READYZ=:8081`, `AGENT_DIR=/agent`, and `AGENTSUITE_MODEL` when `-model` is given. No model credentials: they stay with the host. |
| `containers[0].wakeupProbe` | `GET /readyz` on 8081 |
| `containers[0].securityContext` | `capabilities: {}`, adding none |
| `containers[0].resources` | `binding.resources` |
| `sandboxConfig` | `binding.sandbox`: `SANDBOX_CLASS_GVISOR`, `gvisor-default` |
| `snapshotConfig` | `preferredFidelity` from `binding.snapshot.fidelity`. `storageLocation` is `binding.snapshot.storageLocation` + `<name>/`. |
| `workerSelector` | `binding.workerSelector` |

The harness registration is a `HarnessRegistration` with `spec.substrate {atespace, template}`,
`STATELESS_REPLAY` and `descriptor_id: triage`. `agentsessionsd` does not serve the registry, so
the registration is applied as `-substrate-harness=triage=<atespace>/<template>`.

## Requirements

- docker, kind, kubectl, ko, go 1.27, git, python3 and curl.
- The kind lane of agent-substrate at the commit `.github/workflows/substrate-e2e.yml` pins. `up.sh`
  clones it into the work directory, `$TMPDIR/agentsuite-demo` (set `AGENTSUITE_DEMO_WORK` to move it).
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
  not create.

## A real model

```bash
AGENTSUITE_MODEL_BASE_URL=https://api.openai.com/v1 AGENTSUITE_MODEL=<model> MODEL_API_KEY=<key> ./up.sh
AGENTSUITE_MODEL_BASE_URL=https://api.openai.com/v1 ./demo.sh
```

`up.sh` refuses `AGENTSUITE_MODEL_BASE_URL` without `AGENTSUITE_MODEL`, or either one with
whitespace or a control character in it, before it changes the cluster, so a misconfigured rerun
leaves an existing key in place. Both are copied into the Deployment as written. A query in the URL
(an `api-version`, say) is sent with every completion request, after `/chat/completions` is joined
to the URL's path; a fragment is dropped. It applies the key to the `agentsessionsd-model` Secret
through stdin, so it never appears in argv or a file, and a failed apply leaves the stored key
unchanged. `agentsessionsd` reads the key only when it starts, so `up.sh` stamps the Secret's
version into the Deployment's pod template: a new or deleted key restarts the pod, which also drops
its emptyDir journal, and an unchanged key leaves it running. It does not build or deploy the fake
model. It points `agentsessionsd` at the endpoint, and the harness requests `AGENTSUITE_MODEL`
instead of the fixture's `triage-model`: the ActorTemplate carries it as an env var, so each model
gets its own template. `demo.sh` then skips the checks that depend on the fake's replies and
counter, and does not port-forward the fake. The replay still fails if the controller sees a model
call.

The Secret, and so the running daemon, follows the latest `up.sh`. A real-provider `up.sh` without
`MODEL_API_KEY` deletes any stored key, for endpoints that take none, so export `MODEL_API_KEY`
again on every real-provider run. A fake-provider `up.sh` deletes it too, so the fake never receives
a key.

This path was run against the in-cluster fake standing in as the endpoint, with a dummy key; it has
not been run against a hosted provider.

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
- **gVisor, not micro-VM.** An arm64 Mac's kind node has no KVM. The adapter's micro-VM path is
  covered by the repo's substrate-e2e CI job, not by this demo.
- **ate-api trust.** `agentsessionsd` verifies the ate-api certificate against the
  `servicedns.podcert.ate.dev` ClusterTrustBundle that substrate's kind install creates, through a
  projected volume. It authenticates with the pod's projected ServiceAccount token. Which identities
  the kind install's ate-api accepts was not examined beyond this working.
- **Port-forwards.** `demo.sh` reaches agentsessionsd, the router (for the replay) and, with the
  fake provider, the fake model through `port-forward`.
