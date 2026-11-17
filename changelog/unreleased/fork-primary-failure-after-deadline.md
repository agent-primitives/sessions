---
category: Fixed
---

- A Substrate `Fork` whose child failed while the caller was still waiting keeps that failure's
  status code and ErrorInfo reason when the caller's deadline or cancellation lapses during the
  backend's teardown of the child. It was reported as `DEADLINE_EXCEEDED` or `CANCELED`. A caller
  context that ended before the failure is still reported as before.
