---
category: Fixed
---

- Memory-tier Suspend and Fork refuse an incomplete execution before snapshotting. This prevents a
  snapshot from capturing an unjournaled RAM mutation that recovery could apply a second time.
- A stateful Fork no longer cancels a running parent turn. Active contention is `ABORTED`; an
  abandoned incomplete turn is `FAILED_PRECONDITION` with `CHECKPOINT_INCOMPLETE_TURN`.
- Memory-tier Suspend fences before capture and commits `SUSPEND` only at the journal head it
  validated, refusing peer movement instead of accepting it under the captured RAM.
- Memory-capable Suspend now describes the harness before fencing or snapshotting to apply the
  memory-tier rule. Custom memory Runtime implementations must quarantine captures the journal did
  not record by refusing a mismatched `RecordedCheckpoint`.
