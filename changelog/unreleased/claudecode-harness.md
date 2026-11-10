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
  turn records no `USAGE` and no `TOOL_RESULT` for a tool still running. A tool whose `tool_result`
  is malformed fails the turn and is reported as `no result observed`, like a tool that never got a
  result. The `claude` process gets only an allowlist of the harness's environment (`PATH`, `HOME`,
  `USER`, `SHELL`, `TMPDIR`, locale, the `ANTHROPIC_*` key and base URL variables,
  `CLAUDE_CONFIG_DIR`, and `Config.EnvAllowlist`; a deployment that needs proxy or CA variables must
  add them), and its key must be a placeholder (`EXE-40`). Every turn passes
  `--setting-sources user --strict-mcp-config`, so settings, hooks and `.mcp.json` in the work dir
  cannot widen the permission mode, and `bypassPermissions` also needs
  `Config.AllowBypassPermissions`. A turn whose process fails before `system/init`, such as a
  `--resume` whose transcript is gone, logs its exit status and first stderr line. A turn's process
  group is killed before the turn returns, a successful turn included, so a background process a
  tool started does not outlive the turn; that needs Unix process groups, so `New` refuses other
  platforms such as Windows. A re-run of a started execution fails (`EXE-42`), a turn on an instance
  that lost the session's memory fails rather than start an empty conversation (`EXE-43`), and
  approvals are not supported. Fork does not work yet: on a runtime that snapshots memory, forking a
  session that has run a turn creates a child, and every turn on the child fails with the same
  error, `claudecode: session has earlier turns but this harness holds no state for it`; a follow-up
  issue tracks it. `controller.Replay` does not refuse this harness and must not be pointed at it.
- `integrations/claudecode/cmd/harnessnode`: a harness server that serves only this harness, built
  on the root module's `harnessnode` and reading the same server settings. `HARNESS_KIND` must be
  unset or `claudecode`. The harness is configured by `HARNESS_CLAUDE_WORKDIR` and
  `HARNESS_CLAUDE_PERMISSION_MODE` (both required), and optionally `HARNESS_CLAUDE_BIN`,
  `HARNESS_MODEL`, `HARNESS_CLAUDE_MAX_TURNS`, `HARNESS_CLAUDE_MAX_BUDGET_USD`,
  `HARNESS_CLAUDE_ENV_ALLOWLIST` (further variable names `claude` receives) and
  `HARNESS_CLAUDE_ALLOW_BYPASS_PERMISSIONS` (the second opt-in `bypassPermissions` requires).
