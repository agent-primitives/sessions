---
category: Compatibility
---

- A stateful substrate `Fork` now refuses a clone whose `ResumeActor` leaves `NoCapture` false but
  shows no capture of its own with `FAILED_PRECONDITION` (`api.ErrMemoryStateLost`), where v0.3.0
  returned `ABORTED` (`substrate.ErrActorBusy`): a retry is refused the same way until the client
  reports `NoCapture`, so the refusal no longer reads as retryable (`WARN` log `capture` `unknown`,
  `error_kind` `memory_state_lost`). A clone holds a capture of its own, and stays `ABORTED`
  (`capture` `held`, `error_kind` `actor_busy`) with a retried Fork cloning a new child, only when
  `CreateActorFromTag` reported a `Snapshot` and the resume reports `SnapshotNotReady` or another,
  non-empty `Snapshot`. A resume that reports neither a `Snapshot` nor `SnapshotNotReady`, which
  v0.3.0 counted as held, is `unknown`. Either way the clone is torn down and the parent is
  unaffected. See
  [a refused capture stays refused](../../docs/substrate-conformance.md#substrate-v050-a-refused-capture-stays-refused).
