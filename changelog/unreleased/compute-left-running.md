---
category: Fixed
---

- A failed `Exec` or `Resume` no longer drops compute it allocated without a word (spec `RT-7`).
  When `Create` or `Restore` succeeded and a later step failed before the journal recorded anything
  that owns the compute, the host releases it through `api.CleanupRuntime.ReleaseSession`. When the
  backend has no `api.CleanupRuntime` (`runtime/substrate`), the release fails, another owner may
  hold the compute, or the journal cannot establish who owns it, the error is the new
  `placement.ComputeLeftRunningError`, matched by the new `placement.ErrComputeLeftRunning`: it
  names the incarnation and keeps the original error for `errors.Is` and `errors.As`, while the
  cleanup outcome is in its `Cleanup` field and the message.
