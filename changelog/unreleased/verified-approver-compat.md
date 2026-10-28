---
category: Compatibility
---

- The SQLite journal moves to schema v8, which changes no table and rewrites no event; the stamp
  marks journals that can hold a verified approver, so a v7 build refuses them at Open. Stop every
  older writer, including embedded `agentctl`, and back up the database and WAL before the first v8
  open: Open stamps the journal even for a metadata-only read, and there is no downgrade to v7.
  An older writer already holding the journal open drops the approver on read: its `Exec`,
  `Resume`, `VerifyJournal` and `controller.Replay` fail with `DATA_LOSS` on a session holding
  one, and its `Approve` RPC, like the `Replay` RPC of v0.3.0, omits it.
- On a host with authentication, an `Approve` retry of a decision recorded without a verified
  approver (before this release, or by an unauthenticated host) now fails `FAILED_PRECONDITION`
  instead of returning the decision, as does a retry by any caller other than the recorded approver.
  Decisions recorded without authentication still retry as before on a host without it.
