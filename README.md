# agentsessions

**A neutral, open contract — and working reference implementation — for durable agent sessions.**
Create, run, suspend, resume, **fork**, and replay an agent session, with a Bring-Your-Own-Harness
SPI on top and a pluggable compute backend underneath.

[Docs site](https://sessions.agent-primitives.dev/) · [Quickstart](docs/quickstart.md) ·
[Concepts](docs/concepts.md) · [Specification](spec.md) ·
[Releases](https://github.com/agent-primitives/sessions/releases)

## What it is

The **narrow waist** for agent runtimes: producers plug in on top (an issue tracker, a chat
platform, a custom app), compute plugs in underneath (pod, Kata, Cloud Hypervisor, agent-substrate),
and the core knows about neither. Build an agent once; run it managed, self-hosted, or in CI, with
the same identity, provenance, and suspend / resume / fork, and no rewrite when it moves.

Kubernetes has no durable-session primitive, and most agent runtimes bind you to one framework, one
compute model, or one vendor. `agentsessions` makes this layer an open, vendor-neutral contract
anyone can implement and target, plus a reference implementation that proves the contract holds.

## Quickstart

```bash
go install github.com/agent-primitives/sessions/cmd/agentctl@v0.2.0
go install github.com/agent-primitives/sessions/cmd/agentsessionsd@v0.2.0
```

Signed archives for linux/darwin on amd64/arm64, with checksums and SBOMs, are on the
[latest release](https://github.com/agent-primitives/sessions/releases/latest). Container images:

```
ghcr.io/agent-primitives/sessions/agentsessionsd:v0.2.0
ghcr.io/agent-primitives/sessions/harnessnode:v0.2.0
```

Then follow [`docs/quickstart.md`](docs/quickstart.md): exec a turn, replay it with zero model
calls, fork it, suspend and resume it, and verify its provenance chain with a non-Go verifier. No
Kubernetes, cloud, or model key required.

## The three contracts

- **Sessions** (`api/session.proto`): the client-facing lifecycle: create / list / exec / suspend /
  resume / fork / replay / approve. Producers attach context through `Origin` and `annotations`,
  with no producer-specific fields in the core; identity is OIDC-neutral (`issuer` / `subject` /
  `principal`).
- **Harness** (`api/harness.proto`): Bring-Your-Own-Harness. The host drives one execution; the
  harness streams typed events over the `Harness.Connect` gRPC stream. Implement it directly, or
  adapt an agent framework with a thin shim.
- **Runtime** (`api/runtime.go`): compute / sandbox: create / snapshot / restore / stop on any
  backend that implements the SPI; no kernel-isolation technology is assumed. Sessions resume by
  replay by default, so they run even on a plain pod; `CanPlace` places a harness that needs memory
  snapshots only on a runtime that has them.

See [`docs/architecture.md`](docs/architecture.md) for how they fit together.

## What's proven

The load-bearing claims are demonstrated end to end, and a replay-conformance suite
(`conformance/`) exercises the determinism guarantees:

- **Survives pod death, resumes by replay.** A different incarnation reconstructs the session
  byte-identically from the durable journal, invoking the model zero times
  ([quickstart](docs/quickstart.md#3-replay-reconstruct-the-session-model-called-zero-times)).
- **Tamper-evident, language-neutral provenance.** Every event is hash-chained over an RFC 8785
  canonical form, and a non-Go verifier reproduces the Go hash
  ([quickstart](docs/quickstart.md#7-verify-the-provenance-chain-with-a-non-go-verifier)).
- **Bring-your-own-harness, out of process.** The host mediates the model over `Harness.Connect`,
  so the determinism guarantees hold across the process boundary
  ([harness authoring](docs/harness-authoring.md#running-out-of-process)).
- **Both capability tiers on real agent-substrate, in CI.** Byte-identical replay, and memory
  snapshot suspend / restore with in-RAM state continuing, against a real `ate-system` on a
  micro-VM ([substrate conformance](docs/substrate-conformance.md)).

## Documentation

The docs are published, rendered and searchable, at
[sessions.agent-primitives.dev](https://sessions.agent-primitives.dev/), along with a walkthrough of
how a turn runs and a recording of the full lifecycle on a real cluster. Start with:

- [Concepts](docs/concepts.md): sessions, typed events, the log, fences, resumability.
- [Quickstart](docs/quickstart.md): exec, replay, fork, suspend, resume, and verify, locally.
- [Writing a harness](docs/harness-authoring.md): plug your agent into the `api.Harness` SPI.
- [Architecture](docs/architecture.md): how the neutral core is built, and the
  [repository layout](docs/architecture.md#repository-layout).
- [API reference](docs/api-reference.md) and the draft [specification](spec.md).

Every doc, indexed by goal: [`docs/README.md`](docs/README.md).

For the complete client → persistent Sessions service → substrate actor → chat harness path, including
a control-plane pod replacement and continued execution, see the
[production-shaped kind deployment](docs/production-shaped-kind.md).

## Status

Working reference implementation (Go 1.26). The Sessions API, the single-writer event-sourced
controller, the durable hash-chained log, BYOH over `Harness.Connect`, the `Runtime` SPI with local
and substrate backends, the replay-conformance suite, and real-substrate conformance across both
capability tiers all run today. See the
[verification boundary](docs/substrate-conformance.md#verification-boundary) for what the live tests
cover, and [how it behaves](docs/interaction-model.md) for approvals, recovery, and known limits.

The reference daemon has no authentication, authorization, or transport security. Custom Go hosts
can opt into [caller authentication](docs/security.md#optional-go-caller-authentication) and
[RPC authorization](docs/security.md#optional-go-rpc-authorization), not complete required-mode
isolation.
A managed control plane, enterprise identity and provenance, and confidential or GPU snapshots are
not built. Read [`docs/security.md`](docs/security.md) before exposing a host.

### API stability

`agentsessions.v1` is a proto namespace, not a stability promise. The Go module is pre-1.0 and makes
no backward-compatibility guarantee; pin a commit if you need stability today. Breaking changes
are called out in [`CHANGELOG.md`](CHANGELOG.md), and CI gates the schema against the most recent
release tag, so the contract cannot break silently
([stability](docs/interaction-model.md#stability)).

## Ecosystem

`agentsessions` composes with existing OSS at its seams: MCP, A2A, CloudEvents, Durable Task / Dapr,
and OpenTelemetry GenAI. See [what it composes with](docs/faq.md#what-does-it-compose-with).

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [License](LICENSE)
