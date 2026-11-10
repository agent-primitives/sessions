---
category: Added
---

- Exec, Resume, Suspend, Fork and DeleteSession report recognized failure reasons in
  `google.rpc.ErrorInfo` under the `agentsessions` domain. Neutral runtime sentinels classify
  compute availability, capacity and configuration failures. Unknown backend failures stay
  INTERNAL without a reason. Direct Go calls retain causes; RPC clients use details.
