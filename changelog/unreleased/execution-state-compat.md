---
category: Compatibility
---

- Execution-state reporting targets v0.4.0 and adds SQLite schema v9 after the allocation ledger's
  v7 and verified-approver v8 migrations. It extends the existing execution/approval projection
  with an outcome maintained atomically during append, rebuilding from the journal without
  rewriting events, canonical bytes, hashes, sequence numbers or fences. Read-only opens requiring
  migration fail without partial changes; undecodable sessions report RUNNING without approval
  references, and SQL faults roll back migration.
- Stop every older host and embedded CLI sharing a journal and back up its database and WAL
  before upgrading. Older binaries refuse v9 on open. Already-open older writers can advance
  the shared approval cursor without updating outcomes, making stale outcomes appear current:
  journal-head comparison detects lagging projections, not all mixed-version writes. No downgrade
  migration is provided; restore the backup to roll back. Clients must handle RUNNING and FAILED
  rather than treating COMPLETED as a compatibility fallback.
