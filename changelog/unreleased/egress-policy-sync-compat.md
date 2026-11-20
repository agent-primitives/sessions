---
category: Compatibility
---

- `runtime/substrate`: with `WithEgressPolicy` and an `EgressPolicyClient`, Create and a memory-tier
  Fork whose function returns no policy now store an empty rule set on the new actor
  (`CreateActorEgressPolicy`), where they made no egress call before. Substrate v1.0.0 enforces it
  as no policy (deny-all) and deletes it with the actor; it shows in Substrate's CLI and in the
  egress gateway's 403 log line. A failure of that create now fails the call: Create leaves the
  actor `SUSPENDED` and a retry resumes it, and Fork discards the child's clone.
- `runtime/substrate`: rolling back to a release before this change keeps the empty policies it
  stored, and those releases treat any existing policy as installed, so such an actor stays
  deny-all even after the function returns rules, until its policy is deleted in Substrate. This
  fails closed. A policy cleared to an empty rule set by a resume or Sync has the same effect after
  a rollback.
