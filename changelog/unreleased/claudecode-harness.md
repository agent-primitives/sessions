---
category: Added
---

- `integrations/claudecode`: a new Go module with an unmediated (spec §8.8, `EXE-37`),
  `REQUIRES_MEMORY_SNAPSHOT` (`EXE-38`) harness that runs one headless Claude Code turn
  (`claude -p --output-format stream-json`) per execution and resumes the same Claude Code session
  on later turns. It reports the turn's final text as `OUTPUT`, each tool Claude Code ran as a
  reported `TOOL_RESULT` with a bounded receipt, and the turn's token counts as one `USAGE`
  (`EXE-39`); there is no `MODEL_CALL`, and spend, model-call audit and replay are given up
  (`EXE-41`). Claude Code's reported cost (`total_cost_usd`) is not recorded, because `USAGE` has
  no cost field. A turn that ends without a `result` records input tokens only, and a cancelled
  turn records no `USAGE` and no `TOOL_RESULT` for a tool still running. The harness never handles
  a model credential: the `claude` process inherits the harness's environment, which must hold only
  a placeholder key (`EXE-40`). A turn's process group is killed before the turn returns, a
  successful turn included, so a background process a tool started does not outlive the turn. A
  re-run of a started execution fails (`EXE-42`), a turn on an instance that lost the session's
  memory fails rather than start an empty conversation (`EXE-43`), and approvals are not
  supported. `controller.Replay` does not refuse this harness and must not be pointed at it.
