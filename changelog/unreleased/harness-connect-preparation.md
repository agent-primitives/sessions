---
category: Fixed
---

- A new `Exec` over `Harness.Connect` now opens its actual stream before recording
  `EXECUTION_START`. If Connect cannot be opened, no execution records are appended and retrying the
  same session starts its first turn normally. The host still sends `Start` only after the start and
  every input are durable; send and receive failures remain uncertain and keep the attempted
  execution in later history. A clean close without `EVENT_END`, or a host-owned or unsupported
  harness event, is now a failed execution rather than implicit success. Go harness adapters can opt
  into the same boundary with `api.HarnessRunPreparer` (`EXE-45`). Caller cancellation during the
  bounded post-END telemetry flush does not replace a completed harness result.
