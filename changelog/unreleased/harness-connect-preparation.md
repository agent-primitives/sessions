---
category: Fixed
---

- A new `Exec` over `Harness.Connect` now opens its actual stream before recording
  `EXECUTION_START`. If Connect cannot be opened, no execution records are appended and retrying the
  same session starts its first turn normally. The host still sends `Start` only after the start and
  every input are durable; cancellation after successful preparation and send or receive failures
  remain uncertain and keep any recorded attempt in later history. Go harness adapters can opt into
  the same boundary with `api.HarnessRunPreparer` (`EXE-45`).
