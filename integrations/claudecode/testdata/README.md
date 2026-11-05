# Claude Code stream-json fixtures

The fake `claude` in `fake_test.go` replays these files line by line, replacing `{{SESSION_ID}}`
with the id passed in `--session-id` or `--resume`.

All fixtures are shaped from Claude Code's documented headless and Agent SDK message formats; none
is a recording of a real run. The fields the harness reads are from the documentation:

- `system`/`init`: `type`, `subtype`, `session_id`, `model`.
- `stream_event`: `type` (the `event` body, including `text_delta`, is ignored).
- `assistant`: `message.id`, `message.content[]` (`tool_use` `id`, `name`, `input`),
  `message.usage` (`input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`),
  `parent_tool_use_id`, `session_id`.
- `user`: `message.content[]` (`tool_result` `tool_use_id`, `content` as a string or text blocks,
  `is_error`), `parent_tool_use_id`, `session_id`.
- `result`: `subtype`, `is_error`, `result`, `session_id`, `usage`.

Fields the harness ignores (`uuid`, `cwd`, `tools`, `duration_ms`, `total_cost_usd`,
`modelUsage` and others) are filled in for realism only; their values are invented.
