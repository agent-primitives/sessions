---
category: Added
---

- Container image `ghcr.io/agent-primitives/sessions/claudecode-harnessnode`, for linux/amd64 and
  linux/arm64: the `integrations/claudecode` harness node alone, at `/ko-app/harnessnode`, built
  from the release's tagged commit and tagged with the root release tag. It does not include the
  `claude` CLI; copy the binary into an image that has it. See
  [integrations/claudecode](../../integrations/claudecode/README.md).
