# Claude Code stream-json fixtures

The fake `claude` in `fake_test.go` replays these files line by line, replacing `{{SESSION_ID}}`
with the id passed in `--session-id` or `--resume`.

## Recorded fixtures

`recorded_*.jsonl` are the stream-json output of Claude Code 2.1.311, the version these fixtures
were recorded with, captured on 2026-11-07:

- `recorded_tool_turn.jsonl`: a first turn (`--session-id`) that runs `ls` through the Bash tool and
  replies with the file names.
- `recorded_resume_turn.jsonl`: the next turn (`--resume` of the same id), a text reply.
- `recorded_resume_missing.jsonl`: `--resume` of an id with no transcript. Claude Code writes one
  `result` and no `system`/`init`, exits with status 1, and prints
  `No conversation found with session ID: <id>` on stderr.

They were captured with the harness's argv, in a work dir holding one file, under an empty `HOME`
and an environment of only `PATH`, `HOME`, `TMPDIR`, `LANG` and the API base URL and token:

```sh
env -i PATH="$PATH" HOME="$TMPHOME" TMPDIR="$TMPDIR" LANG="$LANG" \
  ANTHROPIC_BASE_URL="$ANTHROPIC_BASE_URL" ANTHROPIC_AUTH_TOKEN="$ANTHROPIC_AUTH_TOKEN" \
  claude -p --output-format stream-json --verbose --include-partial-messages \
  --permission-mode acceptEdits --setting-sources user --strict-mcp-config \
  --session-id "$ID" --model haiku --max-turns 3 --max-budget-usd 0.25 \
  <<< 'Use the Bash tool to run ls in the current directory, then reply with just the file names.'
# then the same with --resume "$ID" and 'Reply with the single word done.'
```

Each file was then scrubbed with `jq -c --arg sid "$ID" --arg dir "$WORKDIR" -f scrub.jq`, where
`scrub.jq` is:

```jq
walk(if type == "string" then gsub($sid; "{{SESSION_ID}}") | gsub("(/private)?" + $dir; "/work") else . end)
| if .type == "system" and .subtype == "init" then
    del(.slash_commands, .terminal_slash_commands, .skills, .plugins, .agents, .capabilities,
        .messaging_socket_path, .memory_paths)
    | .tools |= map(select(IN("Task", "Bash", "Edit", "Read", "Write", "WebFetch", "WebSearch", "NotebookEdit")))
  else . end
| if .type == "stream_event" then
    .event |= with_entries(select(.key | IN("type", "index", "message", "content_block", "delta", "usage", "context_management")))
  else . end
```

It replaces the session id and the work dir, drops the init event's lists of the local
installation's commands, skills, plugins and agents and its local paths, keeps the standard tools,
and keeps only the Anthropic streaming event fields in each `stream_event` (an API gateway on the
capture path added one of its own). The harness reads none of the dropped fields. Message, tool and
event ids, timestamps, token counts and costs are as recorded. The result was checked for the API
host, token, home and temp paths before commit.

## Documented fixtures

The other fixtures are shaped from Claude Code's documented headless and Agent SDK message formats;
they cover cases a recording cannot produce on demand (subagents, orphan and dangling tool results,
malformed and truncated streams). The fields the harness reads are from the documentation:

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
