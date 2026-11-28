---
category: Compatibility
---

- SQLite schema v11 is a stamp-only, one-way migration for hash-bound model-result correlation
  fields. Stop every older writer and embedded client and back up the database and WAL before the
  first v11 open; rollback requires restoring that backup. Existing records keep positional model
  result behavior and unchanged hashes. After a failed re-drive of an old positional call, immediate
  direct fallback output is refused as ambiguous; a fresh model-call retry remains supported.
