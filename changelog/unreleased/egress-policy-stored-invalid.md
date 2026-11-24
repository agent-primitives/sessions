---
category: Changed
---

- `integrations/substrate`: `GetActorEgressPolicy` reports a stored `EgressPolicy` the adapter
  cannot convert (a rule type it does not know, or one with no protocol, both port forms, or a
  header with both or neither credential source) as `substrate.ErrEgressPolicyInvalid` instead of
  an unclassified error. The error names the rule by index only.
