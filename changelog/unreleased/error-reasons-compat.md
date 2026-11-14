---
category: Compatibility
---

- Compute-start failures explicitly classified by a runtime use UNAVAILABLE, RESOURCE_EXHAUSTED
  or FAILED_PRECONDITION. Caller cancellation during compute start now keeps
  CANCELED/DEADLINE_EXCEEDED based on the caller outcome at failure, before Fork rollback.
  A Suspend journal failure after a successful snapshot keeps its own status even if the caller
  context has ended.
  Request diagnostics honor the selected RPC status, not a nested peer timeout.
  `CallerContextError` omits backend diagnostics from status text but retains Go causes.
  Compute-category diagnostics are visible to RPC callers even with authorization enabled and
  must be credential-safe; only INTERNAL responses receive the authorization sanitizer.
