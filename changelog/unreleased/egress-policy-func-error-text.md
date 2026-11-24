---
category: Security
---

- A Substrate `WithEgressPolicy` function error no longer reaches callers with its text: `Exec`,
  `Resume` and `Fork` publish it under the code its wrapped sentinel selects (for example `ABORTED`
  or `FAILED_PRECONDITION`) with fixed text naming the session, so a credential reference in the
  function's message is not returned. With authorization off, an unclassified function error
  (`INTERNAL`) also loses its text. Go callers still reach the function's error through `errors.Is`,
  `errors.As` and `Unwrap`.
