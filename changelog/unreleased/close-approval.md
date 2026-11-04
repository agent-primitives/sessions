---
category: Added
---

- `Sessions.CloseApproval` RPC and `client.CloseApproval` (new) close an approval gate the harness
  can no longer re-issue, so the session can take a new turn. An undecided gate gets a host denial
  (reason "approval closed", `verified_approver` set from the authenticated caller) and a
  `NOT_EXECUTED` receipt, a denied gate gets `NOT_EXECUTED`, and an approved gate without a receipt
  gets `OUTCOME_UNKNOWN`, because the journal cannot prove the executor never ran. A gate that
  already has a receipt is returned unchanged, so retries are safe. The close calls no executor,
  policy hook, harness or runtime and appends no `END`; `Exec` with input is admitted afterwards,
  and `Resume` serves the decision and receipt to a harness that re-issues the call, except that a
  `REQUIRES_MEMORY_SNAPSHOT` harness restored at the gate cannot continue there unless the gate is
  the turn's first effect (spec `APR-16`, `APR-19`). Both codes are
  new `ToolResult.Code` values only the host writes: a harness report or ordinary executor result
  carrying one is rejected with `controller.ErrApprovalReceiptReport`, and an approved call's
  executor result carrying one, whatever its `is_error`, is recorded as `EXECUTOR_ERROR`. With
  authorization enabled the RPC checks `read` and then `api.ActionApprovalClose` (`approval.close`,
  new), which `OwnerAuthorizer` grants the owner and `OwnerAllowlistAuthorizer` delegates unchanged;
  `approve` does not imply it. Go hosts can call `controller.CloseApproval` or
  `placement.Registry.CloseApproval` (new). See [spec §10.5](../../spec.md#105-close) and
  [security](../../docs/security.md#approval-is-not-a-security-policy).
