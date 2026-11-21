---
category: Security
---

- A Substrate `Exec` refused because the session's suspended actor was deleted and created again
  while its egress policy was converged or it was resumed, published as `ABORTED` with
  `COMPUTE_BUSY`, no longer carries Substrate's message. Go callers still reach the failed step's
  error through `errors.Is`.
