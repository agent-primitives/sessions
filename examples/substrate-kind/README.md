# Durable Sessions on Substrate and kind

This example demonstrates what a Sessions deployment looks like in a Kubernetes cluster: how the
Sessions service, isolated session compute, persistent storage, and model provider fit together.
It uses kind to make that deployment runnable locally, with manifests and scripts you can inspect
as a starting point for your own cluster deployment. The limitations below describe what needs
further work before production use.

Deploy a conversational Sessions service on a local kind cluster using Substrate for session
isolation. The example runs
[`integrations/substrate/cmd/agentsessionsd`](../../integrations/substrate/cmd/agentsessionsd)
with `harnessnode`/`chatagent`, a PVC-backed SQLite journal, and an isolated actor per session.
Model calls run in the daemon; provider credentials never enter the actor. The included smoke
script demonstrates conversation continuity and journal integrity across daemon pod replacement.

## Run

Requires Docker, Go, `kubectl`, `ko`, `jq`, and GNU `timeout` (`gtimeout` from Homebrew coreutils also
works). Setup builds the pinned kind and kubectl-ate tools from the workflow's Substrate revision.
Docker must publish its registry and Kubernetes API on this machine's loopback; a remote Docker
daemon is not supported. Docker Desktop's Linux VM is supported.

```bash
cd examples/substrate-kind
./up.sh
./smoke.sh
./down.sh
```

By default, Ollama serves `gemma3:270m` locally without provider credentials. The first image/model
download is large and inference is CPU-only. The model download has a ten-minute deadline and
setup refuses a failed or timed-out pull. The Ollama volume declares 1Gi of storage for this
model; increase both its PV capacity and PVC request in `deploy/ollama.yaml` for larger models or
multiple cached models. The hostPath volume uses disk space as files are written and does not
enforce that capacity as a quota.

For deterministic infrastructure smoke tests, select the lightweight model stub:

```bash
MODEL_SOURCE=stub ./up.sh
MODEL_SOURCE=stub ./smoke.sh
```

For an OpenAI-compatible provider reachable from inside the cluster:

```bash
export MODEL_SOURCE=external
export MODEL_NAME=gpt-4o-mini
export MODEL_BASE_URL=https://api.openai.com/v1
export MODEL_API_KEY=...
./up.sh
```

Optional `MODEL_PATH` and `MODEL_AUTH_HEADER` configure other provider routes/headers. Keys come
from the environment and are submitted to a Kubernetes Secret through stdin, never argv. Omitting
the key removes any previously configured key. ConfigMap/Secret resource versions restart the
daemon when its settings change.

`MODEL_API_KEY` is accepted only with `MODEL_SOURCE=external`. Before switching back to Ollama or
the stub, run `unset MODEL_API_KEY`; setup refuses a leftover key instead of sending it to a local
provider. A successful unkeyed deployment removes the previous credential Secret.

Both setup and direct deployment validate the effective model route before cluster operations,
using the daemon's model constructor without contacting the provider. The base URL must use HTTP
or HTTPS with a host and no userinfo, query or fragment. The model path must start with a single
`/`; its only supported query is one date-shaped `api-version` (for example,
`/chat/completions?api-version=2025-04-01-preview`). Header names accept letters, digits and hyphens.
Refusals do not print configuration values. This catches invalid configuration, not provider
availability or model access.

Setup defaults to microVM when Docker can expose `/dev/kvm` to a short-lived BusyBox container.
It visibly falls back to gVisor when Docker reports that device is missing; image download,
other Docker, installation, or startup failures stop setup. This checks device access, not VM
creation. `SANDBOX_CLASS=microvm` requires the device check to pass; `SANDBOX_CLASS=gvisor` skips
this initial probe. The upstream cluster-creation helper probes KVM independently; before installing
or reusing a microVM deployment, setup also requires a writable `/dev/kvm` mount on the kind node.
If the helper omitted that mount, setup refuses rather than installing incompatible workers; use
`down.sh` before recreating the cluster. Images and runtime assets target the Docker server's
architecture.

## Deployment layout

| File | Responsibility |
|---|---|
| `deploy/actor-template.json` | Declarative Substrate ActorTemplate, not a Kubernetes CRD |
| `provision.sh` | Render JSON parameters with jq; refuse immutable mismatches; wait for a golden snapshot |
| `deploy/agentsessionsd.yaml` | Sessions daemon, persistent journal, projected trust/token and restricted ingress |
| `deploy/workerpool.yaml` | Two workers of the selected sandbox class |
| `deploy/ollama.yaml`, `deploy/modelstub.yaml` | Local Ollama provider and deterministic CI provider |
| `up.sh`, `common.sh`, `down.sh` | Owned cluster setup/teardown and common settings |
| `deploy.sh` | Apply the example to an already-installed cluster; also used by CI |
| `smoke.sh` | Client-only restart/persistence demonstration |
| `cmd/modelstub`, `scripts_test.go` | Example-local fixture and regression tests; separate Go module |

`provision.sh` uses `jq --arg` to parameterize image/model values in the checked-in JSON.
Templates are created through `kubectl-ate`, because they belong to Substrate's Control API,
not `kubectl apply`. The complete existing specification, including volumes, containers, resource
limits, selectors, sandbox configuration and snapshot configuration, must match exactly.
Server-owned metadata and status are excluded. `--check-only` performs this
comparison before worker/daemon changes. Each CLI invocation is bounded to two minutes, and the
whole provisioning/golden wait to ten minutes.

`ACTOR_TEMPLATE` defaults to `chat-harness`. Intentionally changing an immutable image/model/sandbox
requires a new name or a fresh cluster; setup never silently keeps an incompatible template.
WorkerPools are named `chat-harness-microvm` and `chat-harness-gvisor`, so changing sandbox class
with a new template does not mutate the previous pool's immutable class or remove its actors.
Switching to microVM also requires the existing kind node's KVM mount; otherwise use a fresh cluster.
`SANDBOX_CONFIG` overrides the class's default config (`microvm` or `gvisor-default`), which must
already exist when custom. `SNAPSHOT_LOCATION` defaults to `gs://ate-snapshots/sessions-example/`;
the template name is appended. Generated JSON and tools live under
`_local/substrate-kind/${CLUSTER}/`, not in source files.

For manual access:

```bash
export KUBECONFIG=../../_local/substrate-kind/sessions-example/kubeconfig
kubectl --context kind-sessions-example port-forward -n sessions-example service/agentsessions 8080:8080
# In another terminal, using an agentctl built from this checkout:
agentctl exec --server 127.0.0.1:8080 --project example --harness substrate-chat --input hello
```

The daemon's built-in `chat` and `echo` remain local harnesses. Select `substrate-chat` to use the
remote actor. The binary's normal listener default is loopback; the manifest explicitly binds
`:8080` for its Service and readiness probe.

## Persistence, ownership and limits

The smoke creates a session, verifies its chain, replaces the daemon pod, reads the same journal,
and continues the session with its prior conversation. With the stub it verifies context lengths
one and three and a ten-record chain. Replay here reads committed records and does not call a
model. The actor survives the pod restart: this does **not** demonstrate harness reconstruction or
memory restoration. The full smoke is bounded to fifteen minutes; readiness probes are individually
bounded to three seconds within a thirty-second readiness budget.

`up.sh` records the kind control-plane container ID, not just its name. Reruns reuse that cluster
and retain its SQLite volume. `down.sh` refuses foreign/replaced clusters or uncertain inspection
results and preserves the shared `kind-registry`. Interrupted creation is recorded but never
automatically recreated. Keep the state directory; losing its ownership record does not authorize
automatic adoption or deletion. Override `CLUSTER` and `SUBSTRATE_KIND_WORK` consistently for
independent deployments. A custom `SUBSTRATE_DIR` must already be clean and at the workflow pin;
setup does not fetch or switch it.

This is **production-shaped, not production-ready**:

- SQLite and the static hostPath volume support one daemon replica, not HA. The volume survives
  pod replacement but disappears when the kind node is deleted. Ollama's cache has the same limit.
- Sessions has no authentication or TLS termination here. Access is through local port-forward;
  deny-all pod ingress requires an enforcing CNI. The pinned kind setup supplies one.
- The projected ate-api token has a dedicated audience and the default Kubernetes API token is
  disabled. At this Substrate pin, the audience token is not least-privilege service-account
  authorization or a tenant boundary.
- Harness traffic is h2c through atenet-router; routing metadata is not authorization. Long model
  calls can exceed the router's stream/idle timeouts.
- The actor runs the reference chat context policy, which sends the full recorded conversation
  on every turn, not a production context-window strategy.

Run example regressions independently:

```bash
go test -race ./...
```

The Substrate workflow calls the same `deploy.sh` and `smoke.sh` against its installed microVM
cluster. CI uses the explicit model stub, not model credentials.

## Related example

[agentsuite-substrate](../agentsuite-substrate/README.md) demonstrates AgentSuite agent composition,
memory snapshots, forks and harness reconstruction replay. Both examples use the Substrate
Sessions daemon, adapter, verified ate-api TLS and router.
