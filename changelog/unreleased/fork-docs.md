---
category: Fixed
---

- The `ForkRequest.count`, `ForkRequest.child_names` and `Sessions.Fork` comments now describe what
  the server does: a `count` of zero or less forks one child instead of being `INVALID_ARGUMENT`,
  and `child_names` is checked against that effective count. A `REQUIRES_MEMORY_SNAPSHOT` parent is
  checkpointed once for the whole fan-out; if the fan-out then fails, the checkpoint is not rolled
  back (its `SUSPEND` stays committed) and the parent is not resumed automatically. Behavior is
  unchanged.
