---
category: Added
---

- `runtime/substrate`: `WithEgressPolicy` installs a Substrate egress policy on each session's actor
  before its compute starts: after `CreateActor` and before the first `ResumeActor`, before every
  later `ResumeActor` the backend issues, and on a fork child's clone before its resume. The policy
  is a neutral `substrate.EgressPolicy` (`http`, `https` and `tls_passthrough` rules, with
  credential-header effects from a credential URI or an actor JWT), installed through the new
  optional `substrate.EgressPolicyClient`, which the ate-api adapter implements. See
  [Actor egress policy](../../docs/substrate-conformance.md#actor-egress-policy).
- `agentsessionsd` (substrate build): `-substrate-egress-policy-file` reads a Substrate `ateapi.v1`
  `EgressPolicy` in protojson once at startup and installs it on every session actor.
