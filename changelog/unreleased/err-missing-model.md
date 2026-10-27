---
category: Added
---

- `controller.ErrMissingModel`: a live model call on a controller with neither a `ModelFunc` nor
  `WithStreamingModel` returns it (match with `errors.Is`) instead of dereferencing a nil model.
  Replay and streaming-only controllers still accept a nil model.
