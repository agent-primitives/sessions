---
category: Compatibility
---

- The human output of `agentctl exec`, `replay` and `fork` is for people and will change in a later
  release. Scripts that parse it should move to `--json`; `exec` now supports it.
