---
category: Added
---

- Sessions now records durable, UID-backed cleanup ownership for Create, Restore, and Fork
  resources. Substrate actor and tag identities use UID-conditional teardown primitives; the
  cleanup reaper and DeleteSession completion behavior are not part of this change.
