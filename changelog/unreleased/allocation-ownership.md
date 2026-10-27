---
category: Added
---

- Durable allocation intent and session-scoped handle acknowledgements for SQLite-backed Create and
  Restore, plus optional non-destructive release contracts for local and remote runtimes. No reaper
  runs yet; Fork retains its existing allocation and rollback behavior.
