---
category: Fixed
---

- Stateful Substrate `Fork` now retains the original `TagActor` failure in the Go error chain when
  the parent disappeared or its snapshot was superseded while tagging. Lifecycle code and ErrorInfo
  reason precedence are unchanged.
