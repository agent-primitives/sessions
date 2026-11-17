---
category: Added
---

- Go harness adapters can implement `api.HarnessRunPreparer` to acquire the transport for one
  execution before it is recorded, without invoking the harness or sending `Start`. The returned
  `api.PreparedHarnessRun` is used at most once or closed; a concurrent close either prevents Run
  or cancels it. `harnesswire.ClientHarness` implements the extension for its `Harness.Connect`
  stream (`EXE-45`).
