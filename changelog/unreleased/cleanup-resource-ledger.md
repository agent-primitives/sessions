---
category: Added
---

- Placement records which Substrate actor and fork tag UIDs each Create, Restore and Fork created,
  including when the call then fails. Nothing releases them yet: teardown, the cleanup reaper and
  DeleteSession completion are unchanged.
- The substrate `agentsessionsd` takes `-substrate-control-plane-id`, a name for the control plane
  folded into the cleanup key, so journals that see more than one cluster under the same ate-api
  name can tell their resources apart. Without it the key is unchanged.
