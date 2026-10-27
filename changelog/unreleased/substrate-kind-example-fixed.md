---
category: Fixed
---

- The durable Substrate kind example has its own live CI cluster, exercising fresh setup,
  unchanged reruns, restart/persistence smoke and teardown through its public scripts instead
  of borrowing the conformance cluster. Both live jobs gate `substrate-conformance`.
- The Substrate kind example bootstraps and pins a new managed checkout before inspecting clusters,
  so a first run does not require kind on `PATH`. Relative kubeconfig overrides resolve
  consistently across setup commands; stale installations still refuse checkout changes.
  Failed or interrupted bootstrap retains an explicit marker, preventing setup or teardown
  from running an unpinned checkout on retry, including through equivalent checkout paths.
- The Substrate kind example refuses leftover model credentials in Ollama/stub modes and
  validates model routes before cluster operations, preserving a working deployment when a
  rerun supplies invalid provider settings.
- SQLite-backed `Replay` preserves the stored protobuf event's field presence, so audit clients
  can detect presence-only tampering instead of verifying a normalized event. Stored bytes,
  hashes, and journal/wire schemas are unchanged; no journal migration is required.
- `agentctl verify` skips `Replay` for a captured empty prefix, avoiding false failures when a
  concurrent turn appends after the session head was read.
- Daemon startup logs identify the model without copying its provider endpoint, userinfo, or query.
