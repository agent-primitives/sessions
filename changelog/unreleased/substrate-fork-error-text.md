---
category: Security
---

- Substrate `Fork` refusals published as `FAILED_PRECONDITION` or `ABORTED` no longer carry the
  Substrate control plane's own error text. This covers a stateful fork whose parent is gone before
  or during the tag, or is superseded during it; a busy tag, create from tag, egress install or
  resume of the clone; a clone deleted or replaced before its resume; a refused clone teardown; and
  a busy create or resume of a replay-fork's (`STATELESS_REPLAY`) child. Each message names the
  typed reason and the parent, tag, clone or child involved. Go callers still reach the control
  client's error through `errors.Is` and `errors.As`, except a failed clone teardown's, which the
  message names only by its `error_kind` and which is logged at Warn. Status codes and ErrorInfo
  reasons are unchanged.
