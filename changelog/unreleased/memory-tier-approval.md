---
category: Added
---

- `REQUIRES_MEMORY_SNAPSHOT` harnesses can request `REQUIRES_APPROVAL` tool calls. The call parks,
  the turn is suspended with a memory snapshot, and after `Approve` the restored harness continues
  from the gate: its first sink call must be the gated call, any other first call is
  `ErrReplayDiverged`, and a denial returns the `APPROVAL_DENIED` receipt without dispatch. Approved
  calls stay host-executed. Stateless replay is unchanged (spec `APR-16`).
