---
category: Added
---

- `placement.DefaultDialWith` (new): a `Dialer` that dials as `placement.DefaultDial` does plus
  the host's `grpc.DialOption` values, for example
  `grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(n))` to accept harness frames over gRPC's
  4 MiB default. The incarnation's call metadata still replaces any value an interceptor among
  those options sets in the outgoing context, and a `unix://` incarnation still dials its socket.
  Nothing gRPC runs after the interceptors may set routing keys such as `ate-target-actor`:
  per-RPC credentials and a load balancer's pick metadata are sent alongside the incarnation's
  value, and a stats handler's `TagRPC` can replace it. `DefaultDial` is unchanged.
