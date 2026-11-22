---
category: Security
---

- Harnesses now advertise whether their model calls are mediated or unmediated. Reconstruction
  replay refuses an explicitly unmediated harness, or an unknown future class, before `Harness.Run`
  can repeat billed provider calls or tools. `Harness.Connect` binds that check to the exact stream
  before `Start`, so heterogeneous routing cannot bypass it. Raw journal replay and chain
  verification are unaffected. The host also rejects an explicit `UNMEDIATED` descriptor unless it
  declares `REQUIRES_MEMORY_SNAPSHOT`.
