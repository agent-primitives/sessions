---
category: Compatibility
---

- The SQLite journal gains the allocation ledger and inventory completeness/evidence columns.
  Stop all old writers and back up the database and WAL before migration. There is no downgrade to
  v0.3.0 or older; an already-open old writer is not fenced by the schema stamp. Every inventory,
  including fresh rows, stays incomplete until explicit operator reconciliation. See
  [allocation ownership](../../docs/allocation-ownership.md).
- Custom runtimes must return a nonempty `Incarnation.ID` from SQLite-tracked Create/Restore.
  An empty ID now refuses placement after the runtime call and before dialing, retaining an
  unresolved attempt. This handle is not compute-instance identity; non-recording logs are
  unchanged.
