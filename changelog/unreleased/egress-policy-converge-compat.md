---
category: Compatibility
---

- `runtime/substrate`: with `WithEgressPolicy` (or `-substrate-egress-policy-file`), the backend now
  converges each actor's stored egress policy on the configured one before every resume it issues on
  a `SUSPENDED` actor, instead of creating one only when the actor has none. A changed policy
  reaches existing sessions the next time the backend resumes them, enforced within Substrate's
  policy caches (a Substrate router wake does not converge). A policy edited in Substrate by
  anyone else is overwritten then, and a function that returns no policy clears a stored one to an
  empty rule set (deny-all) rather than leaving it. Without the option nothing changes. See
  [Actor egress policy](../../docs/substrate-conformance.md#actor-egress-policy).
- `runtime/substrate`: a custom `EgressPolicyClient` used with `WithEgressPolicy` must now implement
  `GetActorEgressPolicy` and `UpdateActorEgressPolicy` as well as `CreateActorEgressPolicy`: every
  resume of an existing actor reads its policy, even when the function returns none, and a change is
  written with an update preconditioned on the UID and version read. The `WithEgressPolicy` function
  may run more than once per call (a check before an actor is created, then once per attempt to
  converge), after the read, and must return the current policy each time. A `ControlClient` that
  does not implement `EgressPolicyClient` is now refused (`ErrEgressPolicyUnsupported`) on every
  resume of an existing actor and every `SyncEgressPolicy`, even when the function returns no
  policy; before, a function that returned none needed no egress calls.
- `runtime/substrate`: with `WithEgressPolicy`, a memory-tier `Fork` whose new clone already holds
  an egress policy when the backend creates the child's now fails with `ErrActorBusy` (`ABORTED`,
  retryable) and writes nothing, instead of keeping that policy. The clone is torn down if it is
  still the fork's; an actor that replaced it is left as it is. The configured policy is
  authoritative only while the backend is the only writer of egress policies in its atespace.
