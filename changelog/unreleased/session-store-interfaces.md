---
category: Added
---

- `session.SessionStore` and `session.HarnessStore`: `NewService` takes a `SessionStore`, and
  `NewHarnessRegistry` and `ReserveStaticHarnessNames` take a `HarnessStore`, instead of
  `*sqlitelog.Store`, so a host can supply another backend. `*sqlitelog.Store` implements both
  through the new `SessionLog` and `ForkSessionLog` methods and stays the only backend; callers that
  pass it compile and behave unchanged.
