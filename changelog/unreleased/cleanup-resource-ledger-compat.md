---
category: Compatibility
---

- SQLite schema v10 adds normalized allocation resource and borrower rows. Stop all daemon and
  embedded writers and back up the database and WAL before first open with this build. Older
  binaries refuse a v10 journal on a fresh open, but an already-open writer is not fenced. There is
  no downgrade migration; restore the backup to roll back.
