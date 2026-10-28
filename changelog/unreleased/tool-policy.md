---
category: Added
---

- Optional pre-dispatch tool policy hook, `controller.WithToolPolicy` and `placement.WithToolPolicy`
  (new), for custom Go hosts. It runs right before the executor for ordinary host-executed calls,
  live or re-driven by Resume, and for approved `REQUIRES_APPROVAL` calls on recovery, and returns
  allow, a transient refusal or a definitive refusal. A call whose journaled arguments differ from
  what the executor receives (an integer with more than 53 significant bits, such as 2^53+1, in any
  spelling; a `json.Number` such as `0.30000000000000001` that the journal records as 0.3; a
  `float32` such as 1e20 whose JSON text reads as a different float64; a string that is not valid
  UTF-8) is refused transiently without asking the hook, as is an argument the guard
  cannot compare: a Go struct that `encoding/json` writes field by field, or a value or non-string
  map key whose type has a `MarshalJSON` or `MarshalText` method (`time.Time`, `big.Int`,
  `json.RawMessage`). A transient refusal runs nothing and writes no receipt: an approved call's
  Resume fails with `FAILED_PRECONDITION` (`controller.ErrToolPolicyRefused`, new) and its
  decision stays recoverable. A definitive refusal records a `POLICY_DENIED` receipt (new
  `ToolResult.Code`), which recovery and replay serve without asking again. Only the hook writes
  the code: an executor result carrying it is rejected with
  `controller.ErrApprovalReceiptReport` and no receipt, on ordinary and approved calls; an approved
  call's Resume fails with `FAILED_PRECONDITION` and its decision stays recoverable. Without a hook,
  nothing changes. The stock daemon installs none. See
  [pre-dispatch tool policy](../../docs/security.md#pre-dispatch-tool-policy) and spec §9.4, now
  version `0.4.0-draft`.
