---
category: Compatibility
---

- `integrations/claudecode` declares harness version `2`, since it now reads `Start.Config`.
  Across an upgrade or rollback of the harness node, a `Resume` of an unfinished execution and a
  `Controller.Replay` of a finished one fail with `ErrHarnessVersionMismatch` (for a finished one,
  instead of `ErrReconstructionUnsupported`); `Sessions.Replay` still streams the records, and new
  turns are unaffected. Older harness nodes, and nodes without a turn allowlist, ignore `env` and
  run the turn with their own environment.
