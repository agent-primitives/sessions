---
category: Compatibility
---

- SQLite schema v10 adds allocation resource and borrower rows and is one-way. Stop every writer and
  back up the database and WAL before the first open; see
  [spec §18](../../spec.md#18-versioning-and-compatibility).
