---
category: Security
---

- Substrate `Fork` and `Suspend` refusals, and those of an `Exec` or `Resume` that parks for
  approval, published as `FAILED_PRECONDITION` or `ABORTED` no longer carry the Substrate control
  plane's own error text or snapshot URIs. This covers the checkpoint that `Suspend`, a stateful
  fork and the park's suspend take of the actor, when the actor is gone or busy at the look before
  the suspend or at the suspend itself; a stateful fork whose parent is gone, superseded or busy
  when it is read before or after the tag; a busy tag, create from tag, egress install or resume of
  the clone; a clone deleted or replaced before its resume; a refused clone teardown; and a busy
  create or resume of a replay-fork's (`STATELESS_REPLAY`) child. A stateful fork's read of its
  parent that fails in any other way, published as `INTERNAL` or `UNAVAILABLE`, no longer carries
  that text either. Each message names the typed reason and the actor, parent, tag, clone or child
  involved, and a superseded parent's status. Go callers still reach the control client's error
  through `errors.Is` and `errors.As`, except a failed clone teardown's, which the message names
  only by its `error_kind` and which is logged at Warn. Status codes and ErrorInfo reasons are
  unchanged.
