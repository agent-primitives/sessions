---
category: Added
---

- `runtime/substrate`: `Backend.SyncEgressPolicy(ctx, sessionUID)` converges a session actor's
  egress policy on the one `WithEgressPolicy` returns now, without resuming or suspending the actor,
  so a changed policy reaches running compute without waiting for its next resume. Substrate applies
  it within its policy caches (about 10 seconds for requests and new connections, 5 seconds for
  DNS on v1.0.0); an open TLS passthrough connection is not cut. The daemon does not call it. See
  [Syncing a running actor](../../docs/substrate-conformance.md#syncing-a-running-actor).
- `runtime/substrate`: a Sync always writes: an empty policy when none is stored and none is
  configured, and the same policy again when the stored one is equal. Every write is preconditioned,
  so a Sync that returns no policy cannot be undone by a resume, Create, Fork or Sync that read the
  stored policy before it wrote. Change what `WithEgressPolicy` returns, then call Sync.
