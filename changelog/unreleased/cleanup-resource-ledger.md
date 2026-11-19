---
category: Added
---

- Runtime allocation attempts now record immutable backend resource identity in SQLite. The
  ledger is evidence for later cleanup work; DeleteSession does not consume it yet.
