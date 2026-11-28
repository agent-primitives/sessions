---
category: Added
---

- `integrations/claudecode`: a session can set environment variables for one turn through an `env`
  object in `Start.Config`, for names the operator allowlists with `Config.TurnEnvAllowlist` or
  `HARNESS_CLAUDE_TURN_ENV_ALLOWLIST` (exact names or `PREFIX*`). Off by default. Reserved names
  such as `PATH`, `HOME`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_*`,
  `NODE_OPTIONS`, `LD_*` and shell code-loading variables (`BASH_ENV`, `BASH_FUNC_*`, `PS4`,
  `PROMPT_COMMAND`, `ZDOTDIR`) cannot be allowlisted or set, nor can a prefix covering one, such as
  `ANTHROPIC_*` or `CLAUDE_CODE_*`. An invalid, non-UTF-8, reserved, unlisted or oversized `env`
  fails the turn before `claude` starts, naming the variable when the refusal concerns one, never
  a value. `Start.Config` is journaled, so it must not carry credentials; see the
  [harness README](../../integrations/claudecode/README.md#per-turn-environment).
