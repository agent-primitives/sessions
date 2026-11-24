---
category: Fixed
---

- `agentctl exec` no longer exits successfully when writing to stdout fails. It stops writing, reads
  the turn to its end so the turn is not cut short, then exits with an error noting that the turn's
  records are committed. `exec --json` treats a closed pipe the same way, so a reader that stops
  early, such as `| head -1`, no longer cancels the turn. Without `--json`, a closed pipe still ends
  `exec` with SIGPIPE, as before.
