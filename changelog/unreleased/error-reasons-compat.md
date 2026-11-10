---
category: Compatibility
---

- Compute-start failures explicitly classified by a runtime use UNAVAILABLE, RESOURCE_EXHAUSTED
  or FAILED_PRECONDITION. Caller cancellation during compute start now keeps
  CANCELED/DEADLINE_EXCEEDED. Classified runtime diagnostics are visible to RPC callers even with
  authorization enabled and must be credential-safe; only INTERNAL responses are sanitized.
