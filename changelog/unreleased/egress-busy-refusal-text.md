---
category: Security
---

- A Substrate egress policy read or write that Substrate refuses because another operation holds
  the actor, published by `Exec` or `Resume` as `ABORTED` with `COMPUTE_BUSY`, no longer copies the
  control client's error text into its message. Go callers still reach the backend's error through
  `errors.Is`.
