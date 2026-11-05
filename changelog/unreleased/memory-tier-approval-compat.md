---
category: Compatibility
---

- A `REQUIRES_MEMORY_SNAPSHOT` harness's `REQUIRES_APPROVAL` call now parks instead of failing with
  `ErrApprovalUnavailable`, which remains for in-process harnesses with an empty or unknown tier (a
  wire harness's omitted or unknown tier is read as `STATELESS_REPLAY`, which parks). The error's
  message text is unchanged, so a journal in which a harness recorded that text still replays. A
  host that predates this release still refuses the memory tier. Journals holding an earlier refusal
  replay unchanged, and no journal schema or wire format changes. A mismatched memory-tier recovery
  at a decided gate now reports `error_kind` `replay_diverged` rather than `approval_unavailable`,
  and Exec and Resume return it as `INTERNAL` rather than `FAILED_PRECONDITION`, as they already do
  for a stateless-tier divergence.
