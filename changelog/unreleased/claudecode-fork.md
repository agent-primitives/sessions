---
category: Fixed
---

- `integrations/claudecode`: fork works on a runtime that snapshots memory. A fork child's turns no
  longer all fail with `claudecode: session has earlier turns but this harness holds no state for
  it`. The child's first turn adopts its parent's Claude Code session from the cloned harness memory
  and continues it under the child's own id with `--resume <parent id> --fork-session --session-id
  <child id>`; the parent's transcript is left untouched. The child adopts only when its History
  ends with the fork marker, the harness started the last turn before the marker (a turn journaled
  without an execution start counts), and exactly one session on the clone last ran that turn;
  otherwise it still fails closed with the same error. That includes a fork of a child whose turn
  never reached the harness, and a fork whose last turn the harness refused before starting it
  (context already done, another turn running, or a fork of it in flight). Until Claude Code
  confirms the child's own session, every child attempt, a retry after a cancelled or failed one
  included, checks again that the parent has not started a turn since the fork, and fails the same
  way if it has. While that attempt runs, a turn of the parent, and of any fork not yet confirmed
  between the child and the session it copies, fails with `claudecode: a fork of this session is
  running a turn that may still read this session's conversation; retry when it ends`, so continuing
  the parent right after a fork on an instance that serves both fails until the child's turn ends.
  Claude Code's transcript directory must be on the filesystem the runtime clones.
