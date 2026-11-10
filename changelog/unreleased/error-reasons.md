---
category: Compatibility
---

- Exec, Resume, Suspend, Fork and DeleteSession report recognized failure reasons in
  `google.rpc.ErrorInfo` under the `agentsessions` domain. Compute-start failures explicitly
  classified by a runtime use UNAVAILABLE, RESOURCE_EXHAUSTED or FAILED_PRECONDITION; caller
  cancellation during compute start now keeps CANCELED/DEADLINE_EXCEEDED. Unknown backend
  failures stay INTERNAL without a reason. Direct Go calls retain causes; RPC clients use details.
