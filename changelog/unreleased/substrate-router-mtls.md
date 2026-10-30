---
category: Security
---

- The substrate build of `agentsessionsd` no longer reaches harnesses over the router's
  unauthenticated plaintext :80. It authenticates to substrate's `atenet-router` with its pod's
  SPIFFE certificate and verifies the router's serving certificate against the servicedns trust
  bundle and the name `atenet-router.ate-system.svc` (`-atenet-router-cert-file`,
  `-atenet-router-ca-file`, `-atenet-router-server-name`). Both files are re-read on every router
  connection, so kubelet's rotations apply without a restart, and a startup probe logs whether the
  router admits the daemon's ID. See [security.md](../../docs/security.md) for what is and is not
  verified.
