---
category: Compatibility
---

- The SQLite store creates a `session_traces` table at Open, holding each session's latest trace
  context for linking traces. It does not change `user_version`: the table is telemetry only,
  outside the hash chain, never read by replay, and ignored by older builds. A read-only Open of a
  journal without the table still succeeds and records no links.
