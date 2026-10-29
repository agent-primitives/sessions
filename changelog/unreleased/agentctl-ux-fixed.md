---
category: Fixed
---

- Embedded `agentctl` servers suppress routine INFO logs so session views stay readable; warnings,
  errors and command failure diagnostics remain visible on stderr. Standalone daemon logging is
  unchanged.
- `agentctl list --page-size` now actually controls page requests (negative values are refused,
  large values clamped); both human and JSON views still include every page.
- `agentctl` rejects an explicit `--journal` with a remote target instead of silently ignoring it.
  Deletion warnings go to stderr, including with `--json`, keeping stdout machine-readable.
