---
category: Compatibility
---

- Substrate compute-start failures with ate-api UNAVAILABLE or RESOURCE_EXHAUSTED now return
  the corresponding Sessions code and an `agentsessions` ErrorInfo reason instead of INTERNAL.
  Classified messages omit peer diagnostics; direct Go callers retain the original cause.
  Ambiguous configuration failures remain INTERNAL without a reason. See
  [compute-start errors](../../docs/substrate-conformance.md#compute-start-errors).
