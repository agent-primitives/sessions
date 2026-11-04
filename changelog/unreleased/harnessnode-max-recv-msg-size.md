---
category: Added
---

- `cmd/harnessnode`: `HARNESS_MAX_RECV_MSG_SIZE` sets the largest message, in bytes, the harness
  server accepts from the host. `Start` carries the session's whole prior journal, and a `Start`
  over the limit is refused with `RESOURCE_EXHAUSTED` before the harness runs, so raise it for long
  histories. Unset keeps gRPC's 4 MiB default; an invalid value stops harnessnode before it
  listens. See the
  [harness authoring guide](../../docs/harness-authoring.md#package-chat-for-a-remote-runtime).
