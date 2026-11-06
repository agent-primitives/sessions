---
category: Compatibility
---

- `agentctl get` and `list` human formats have changed; lifecycle and approval summaries now
  use short state names and include execution state. Scripts parsing these views should migrate to
  `--json`. Session JSON uses protobuf snake_case names, omits default-valued fields and
  encodes int64 values as strings. Approval JSON changes from lowerCamelCase to snake_case:
  use `decision.content_hash` instead of `decision.contentHash` and `session.last_seq` instead of
  `session.lastSeq`. Harness JSON content is unchanged. JSON content, not bytes, is the contract;
  whitespace and object-key order are not guaranteed.
  `create` still returns a bare UID by default; `exec` and default replay output are unchanged.
  `--server=` explicitly selects embedded mode even with `AGENTSESSIONS_SERVER` set; harness operator
  commands never inherit it. Remote commands must omit an explicit `--journal`. Non-JSON diagnostics,
  including deletion warnings, appear on stderr even in JSON mode. No wire, journal, hash-chain or
  Runtime SPI changes.
