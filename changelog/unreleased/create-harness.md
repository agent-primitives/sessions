---
category: Added
---

- `HarnessRegistry.CreateHarness`: atomic create-only registration (spec `REG-3a`), needing the
  `registry.register` action. A create that timed out may have committed: inspect with
  `GetHarness` rather than retry. New in Go: `sqlitelog.Store.CreateHarness`,
  `sqlitelog.Store.HarnessNameReserved` and `sqlitelog.ErrHarnessExists`.
