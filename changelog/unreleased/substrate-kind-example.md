---
category: Added
---

- A self-contained [durable Substrate kind example](../../examples/substrate-kind/README.md),
  using the existing daemon and stock chat harness, declarative templates, persistent SQLite,
  optional Ollama, and a pod-restart smoke.
- `agentctl verify --session UID` verifies a session's committed hash chain locally from the
  Sessions API, including protobuf field presence. It refuses missing records, changed content, and
  broken links.
