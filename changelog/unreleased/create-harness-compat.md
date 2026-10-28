---
category: Compatibility
---

- `CreateHarness` is a new RPC, not a field on `RegisterHarnessRequest`, so a server that predates
  it refuses with `UNIMPLEMENTED` and changes nothing, rather than ignoring a create-only flag and
  reactivating the name. `RegisterHarness` is unchanged. The generated Go `HarnessRegistryClient`
  and `HarnessRegistryServer` interfaces gain the method: a hand-written client implementation must
  add it; servers embedding `UnimplementedHarnessRegistryServer` compile unchanged. No journal
  schema change.
