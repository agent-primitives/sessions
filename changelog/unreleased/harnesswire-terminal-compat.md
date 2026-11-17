---
category: Compatibility
---

- `Harness.Connect` now fails an execution for four wire shapes the host previously accepted: clean
  closure without `EVENT_END`; harness-sent `EVENT_INPUT`, `EVENT_LIFECYCLE`, `EVENT_ERROR`, or an
  unspecified/unknown kind; any frame after `EVENT_END`; and a harness-owned non-OK final RPC status
  after `EVENT_END{COMPLETED}` while the caller remains live. Caller cancellation or deadline during
  the bounded post-END telemetry flush does not replace a completed harness result (`WIRE-12`).
- A new `Exec` whose Connect preparation fails with `UNAVAILABLE` or `DEADLINE_EXCEEDED` while the
  caller remains live now returns `UNAVAILABLE` with `agentsessions/HARNESS_UNAVAILABLE` and fixed
  host text. Before this change it returned `INTERNAL` with the raw Connect diagnostic.
