---
category: Compatibility
---

- `POLICY_DENIED` adds no SQLite schema version of its own; the spec moves to `0.4.0-draft`. Only a
  host whose policy hook refuses a call definitively records the code. Builds older than this
  release refuse its journal at open (see the schema v8 entry). A writer built before
  `POLICY_DENIED` (v0.3.0 and older) that already holds the journal open still serves every session
  without the code. For a session holding a `POLICY_DENIED` receipt, that writer's hash-chain
  verification fails (`DATA_LOSS`), so its Exec, Resume, VerifyJournal and `controller.Replay`
  refuse the session; its `Sessions.Replay` RPC does not verify and streams the record unverified,
  with the code as the unknown number 3. An unreleased schema v7 build that already includes the
  code verifies and serves these receipts, and fails only on a session holding a verified approver
  (see the schema v8 entry). An older harness rejects the receipt instead of reading it. Upgrade
  every host sharing a journal and every harness before installing such a hook; after one is
  recorded, rolling a host back leaves that session unusable to it except through raw Replay.
