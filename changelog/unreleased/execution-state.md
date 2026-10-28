---
category: Fixed
---

- Session responses report the latest execution's journal-derived state instead of COMPLETED for
  any nonempty journal: PENDING before execution, AWAITING for valid owned pending approval,
  COMPLETED after END, FAILED after ERROR, and RUNNING for other progress or uncertain evidence.
  RUNNING does not prove controller liveness and includes interrupted executions and historical
  forks. Lifecycle records preserve the outcome. Modern executions may recover after FAILED;
  ERROR finishes legacy ID-less executions for recovery purposes. ID-less execution records after
  the legacy prefix mark reporting corruption, suppressing approval references and reporting
  RUNNING during both append and migration. Cancellation remains unimplemented; liveness is
  tracked in [#213](https://github.com/agent-primitives/sessions/issues/213).
- `agentctl get` and `list`, plus suspend, resume, approve and deny status output, print the
  execution state returned by the server alongside compute state.
