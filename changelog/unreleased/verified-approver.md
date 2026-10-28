---
category: Added
---

- `APPROVAL_RESULT` records the verified approver: when the host's authentication interceptors
  verified the caller, `Approve` journals its exact issuer and subject as the new
  `ApprovalResult.verified_approver` (`CallerIdentity`, also new), never a request field. Exact
  retries are bound to it (`APR-7`, `APR-8`). Harnesses do not receive it: the host clears it in the
  decision frame, the receipt and every earlier decision sent in `Start.History` (`EXE-7`). A
  pre-dispatch tool policy hook, which is host code, does: `controller.ToolPolicyApproval.Decision`
  carries it (`TOOL-13`). New `api.ApprovalDecision.VerifiedApprover` is host-set; `client.Approve`
  refuses a decision that sets it with the new `client.ErrVerifiedApproverNotSendable`.
