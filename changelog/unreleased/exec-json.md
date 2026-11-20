---
category: Added
---

- `agentctl exec --json` prints the turn as JSON Lines: one protobuf `ExecUpdate` per line as it
  arrives, each a `session` or a committed `record`. The first line is the session, so a script
  learns the UID of a session `exec` created; a turn paused for approval exits successfully and
  ends with a `session` snapshot whose `pending_approval` names the call, unless a concurrent
  approval has already resolved it. Only a recorded `EVENT_END` shows completion. Streamed model
  chunks are left out; the `EVENT_OUTPUT` record carries the complete response.
