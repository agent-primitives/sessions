---
category: Added
---

- `agentctl` session commands support protobuf `--json`; `replay --json` emits one complete
  `LogRecord` per line. `get` has labeled details, `list` has an aligned table with headings, and
  human state values use consistent short names. All CLI JSON uses one protobuf writer with
  snake_case field names, full enum names, omitted default values and string-encoded int64s.
  `AGENTSESSIONS_SERVER` supplies the session endpoint unless `--server` is explicit, and
  environment-selected targets are announced on stderr before requests.
