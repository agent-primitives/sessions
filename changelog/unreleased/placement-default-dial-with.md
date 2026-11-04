---
category: Added
---

- `placement.DefaultDialWith` (new): a `Dialer` that dials as `placement.DefaultDial` does plus
  the host's `grpc.DialOption` values, for example
  `grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(n))` to accept harness frames over gRPC's
  4 MiB default. The incarnation's call metadata still replaces any value an interceptor among
  those options sets, and a `unix://` incarnation still dials its socket. `DefaultDial` is
  unchanged.
