---
category: Added
---

- `integrations/substrate/routertls` (new): the mTLS client to substrate's `atenet-router` that the
  substrate build of `agentsessionsd` uses, for hosts that embed `runtime/substrate` from another
  module. `routertls.Load` reads the pod's SPIFFE credential bundle and the router's trust bundle,
  re-reading both on every connection, and `routertls.Dialer` returns the `placement.Dialer` to
  pass to `placement.WithDialer`. `Credentials.ClientHandshake` checks the router's certificate
  against the configured server name whatever authority the caller passes.
