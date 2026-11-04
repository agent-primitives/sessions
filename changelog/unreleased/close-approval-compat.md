---
category: Compatibility
---

- `NOT_EXECUTED` and `OUTCOME_UNKNOWN` add no SQLite schema version. Only `CloseApproval` writes
  them; journals without a close are unchanged. Released builds already refuse this release's
  journal at open (see the schema v8 and v9 entries). An unreleased schema v9 build without the
  codes still serves every session without a close. For a session holding a close receipt, its
  hash-chain verification fails: `VerifyJournal` and its controller `Exec`, `Resume` and
  `controller.Replay` refuse the session (`DATA_LOSS`), its Exec and Resume RPCs fail `INTERNAL`
  with a replay divergence, and its `Sessions.Replay` RPC streams the record unverified with the
  code as the number 4 or 5. An older harness rejects the receipt instead of reading it, and
  receives the code as a number in `Start.history`. Upgrade every host sharing a journal and every
  harness before calling `CloseApproval`; after a close, rolling a host back leaves that session
  unusable to it except through raw Replay.
