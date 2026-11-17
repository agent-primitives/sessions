---
category: Compatibility
---

- `Harness.Connect` now fails an execution for four wire shapes the host previously accepted: clean
  closure without `EVENT_END`, a host-owned or unsupported harness event, any frame after
  `EVENT_END`, and a harness-owned non-OK final RPC status after `EVENT_END{COMPLETED}` while the
  caller remains live. Caller cancellation or deadline during the bounded post-END telemetry flush
  does not replace a completed harness result (`WIRE-12`).
