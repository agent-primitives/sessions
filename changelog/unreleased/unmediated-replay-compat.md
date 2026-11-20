---
category: Compatibility
---

- `HarnessDescriptor.harness_class` is additive. An omitted class retains the legacy mediated
  behavior for in-process harnesses and upgraded remote servers; new harnesses should declare
  `MEDIATED` or `UNMEDIATED`. Upgrade remote harness servers before hosts that reconstruct over
  `Harness.Connect`: older servers remain usable for live turns and recovery but cannot reconstruct
  for a new host. Reconstruction now calls `Describe` for identity-less legacy executions too.
  Older hosts ignore the declaration and cannot enforce the refusal. Adding `HarnessClass` to the
  Go `Descriptor` also requires downstream unkeyed struct literals to become keyed. `Describe` must
  remain side-effect-free and is now also required for identity-less legacy reconstruction.
