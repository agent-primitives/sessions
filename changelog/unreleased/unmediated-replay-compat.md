---
category: Compatibility
---

- `HarnessDescriptor.harness_class` is additive. An omitted class retains the legacy mediated
  behavior so existing harnesses keep replaying; new harnesses should declare `MEDIATED` or
  `UNMEDIATED`. Reconstruction now calls `Describe` for identity-less legacy executions too. A new
  host also requires the new stream-bound class handshake from a remote harness before
  reconstruction; older harness servers therefore remain usable for live turns and recovery but
  cannot reconstruct over `Harness.Connect`. Older hosts ignore the declaration and cannot enforce
  the refusal. Adding `HarnessClass` to the Go `Descriptor` also requires downstream unkeyed struct
  literals to become keyed. `Describe` must remain side-effect-free and is now also required for
  identity-less legacy reconstruction.
