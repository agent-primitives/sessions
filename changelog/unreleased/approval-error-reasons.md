---
category: Fixed
---

- `Approve` and `CloseApproval` attach the existing `SESSION_BUSY`, `CHECKPOINT_IN_PROGRESS`,
  `STALE_CURSOR` and `FENCED` error reasons for matching classified conditions. Status codes and
  messages are unchanged; error details remain additive. The API reference also documents reasons
  on approval methods and harness registration.
