---
category: Added
---

- `session.SessionStore` and `session.HarnessStore`: `NewService` takes a `SessionStore`, and
  `NewHarnessRegistry` and `ReserveStaticHarnessNames` take a `HarnessStore`, instead of
  `*sqlitelog.Store`, so a host can supply another backend. `*sqlitelog.Store` implements both
  through the new `SessionLog` and `ForkSessionLog` methods and stays the only backend; callers that
  pass it compile and behave unchanged. Those methods return the new `eventlog.SessionJournal`, an
  `eventlog.Store` that must also implement the `CheckWritable` deletion preflight, so a journal
  wrapper cannot drop it; `eventlog.Store` is unchanged. `Replay` keeps the stored protobuf field
  presence only for a journal that also implements `ReadProto`, as `*sqlitelog.Log` does. Another
  backend's obligations are spec §6.7 (`STORE-1` to `STORE-7`).
