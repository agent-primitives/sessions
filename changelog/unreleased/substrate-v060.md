---
category: Compatibility
---

- The substrate adapter and examples require substrate v0.6.0 (`go.ate.dev/substrate`, formerly
  `github.com/agent-substrate/substrate`). Moving from v0.5.0 is a Substrate reinstall with fresh
  state, which loses every actor and its checkpoints: v0.6.0 renumbers proto fields with no
  migration. Re-run the atespace provisioning afterwards, and upgrade `agentsessionsd` together
  with Substrate; a mismatched pair is unsupported. The worker binaries are `ateworker-gvisor` and
  `ateworker-microvm`, so a WorkerPool's `workerImage` is
  `ko://go.ate.dev/substrate/cmd/ateworker-<class>`. See
  [Running on agent-substrate](../../docs/substrate-conformance.md#substrate-v060-asynchronous-suspends-suspend-durability-and-runtime-fallback).
- Install Substrate v0.6.0 with its default envoy router and list each `agentsessionsd`'s SPIFFE ID
  (`spiffe://cluster.local/ns/<namespace>/sa/<serviceaccount>`) in ate-setup's
  `--ingress-allowed-spiffe-ids`. The substrate build of `agentsessionsd` now reaches harnesses
  over mTLS on the router's :443 (`-atenet-router` and `runtime/substrate.DefaultRouterAddress`
  default to `atenet-router.ate-system.svc:443`), and with `-substrate-harness` it refuses to start
  without `-atenet-router-cert-file` and `-atenet-router-ca-file`: mount a `podCertificate` and a
  `clusterTrustBundle` volume as `deploy/substrate/e2e-job.yaml` does. The plaintext h2c path to the
  router's :80 is gone. Tools that port-forward the router from a laptop (`cmd/replay`, the
  agentsuite `demo.sh` replay step, `demo/driver.sh`, `demo/smoke-microvm.sh` and the session
  lifecycle UI) do not work on v0.6.0 yet; see
  [the transport](../../docs/substrate-conformance.md#the-transport-harnesswire-through-atenet-router).
- ActorTemplates need `spec.resources.limits` (`cpu_millicores` and `memory_bytes`, both
  nonzero) and select their sandbox through the WorkerPool, not a per-template `SandboxConfig`,
  so the substrate-kind example now refuses `SANDBOX_CONFIG` and the agentsuite renderer a
  binding `sandbox.configName` other than the class default: name a custom config in the
  WorkerPool's `sandboxClasses[].configRef` instead. WorkerPools need pod `resources.limits`,
  which workers advertise as scheduling capacity; a pool with none admits no sized actor. The
  agentsuite renderer converts a binding's `resources.cpu` and `resources.memory` to these
  limits and refuses fractional quantities such as `0.5` CPU or `1.5Gi` memory: write whole
  millicores or bytes instead (`500m`, `1536Mi`). See
  [templates and worker pools](../../docs/substrate-conformance.md#substrate-v060-templates-and-worker-pools).
- Each atespace needs an AccessPolicy that grants the `agentsessionsd` principal the `editor` role.
  Without it Substrate denies every Control call with `PERMISSION_DENIED`; Sessions currently
  reports a Resume denied this way as `INTERNAL`. See
  [authorization](../../docs/substrate-conformance.md#substrate-v060-authorization-and-atespace-provisioning).
- After a reinstall and re-provisioning, a session whose latest `SUSPEND` recorded a checkpoint has
  lost it with its actor (an operator delete has the same effect; see
  [a lost actor refuses Resume](../../docs/substrate-conformance.md#a-lost-actor-refuses-resume)):
  - Resume is refused with `FAILED_PRECONDITION` (`api.ErrInvalidCheckpoint`), creates no actor and
    journals nothing. A stateless session previously got an `INTERNAL` get-actor error.
  - Exec starts new compute and replays the journal; files the checkpoint held are lost. Exec of a
    `REQUIRES_MEMORY_SNAPSHOT` session that has run a turn is refused, since its in-RAM state is
    gone.
