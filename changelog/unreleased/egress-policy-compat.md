---
category: Compatibility
---

- `runtime/substrate`: with `WithEgressPolicy` set, a `ControlClient` that does not implement
  `substrate.EgressPolicyClient` is refused (`ErrEgressPolicyUnsupported`) before any actor is
  created, and a failed install fails the call with the actor left suspended. The missing
  interface and a policy Substrate refuses as invalid (`substrate.ErrEgressPolicyInvalid`) reach
  Sessions callers as `FAILED_PRECONDITION` with reason `COMPUTE_CONFIG_INVALID`. An actor that
  already has a policy keeps it, so a changed policy reaches only actors created after the change,
  and an actor found running is not given one: it stays deny-all until the backend next resumes
  it. Without the option, the backend makes no egress call.
