---
category: Changed
---

- After `EVENT_END` the reference host half-closes the harness Connect stream and waits up to 2
  seconds for the harness to end the RPC, instead of cancelling it at once, so the harness can
  flush its spans and return its own status. A harness that never closes costs at most the 2
  seconds. The host waits the same way when a sink call fails on its side, such as a model
  provider error, unless the caller has cancelled. A park already waited for the harness to close.
