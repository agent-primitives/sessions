# Review guidance for AgentSessions

[CONTRIBUTING.md](../CONTRIBUTING.md) and [spec.md](../spec.md) are the rules. Review in this order:

1. **Fail closed.** A stale fencing token (`ErrFenced`), a lost `expectedLastSeq` compare-and-swap
   (`ErrConflict`), a failed chain verification or an ambiguous recorded state must refuse, never
   retry into success, fall back to a default or skip silently.
2. **Event log and provenance.** Preserve sequence numbers, RFC 8785 canonical bytes and the hash
   chain; changed canonical bytes need a migration story. Replay never calls the model. A
   host-executed tool re-drives at most once, only when its executor declares durable dedup,
   deduplicated by session UID plus idempotency key, and its receipt is journaled.
3. **Credentials and privacy.** No tokens, keys, private endpoints or session payloads in fixtures,
   logs, errors or traces. Model keys come from the environment, never argv or a URL query.
4. **Ownership and neutrality.** No cloud, sandbox or provider-specific code or naming in the root
   module `github.com/agent-primitives/sessions`; `go list -deps ./... | grep -c agent-substrate`
   stays `0`. The one exception is `runtime/substrate`, which reaches substrate only through its
   own `ControlClient` interface; flag anything that widens it. Vendor code belongs in its own
   module, such as `integrations/substrate`.
5. **Scope and size.** One change per PR; flag unrelated edits, timeout increases used to hide
   failures, hand edits to `api/genpb` or `docs/api-reference.md`, and tests that add no distinct
   behavioral coverage.
6. **Docs and compatibility.** Public contract changes update docs, `CHANGELOG.md` and `spec.md` in
   the same PR. Breaking proto, wire, journal or Runtime SPI behavior needs a documented v0 minor
   release and migration notes; `buf breaking` checks against the last `v*` tag.
7. **Verification.** Require gofmt, `go vet`, `go test -race`, golangci-lint, govulncheck, a tidy
   `go.mod`, `buf lint`, no diff after `buf generate` or `hack/gen-api-reference.sh`,
   `hack/verify_chain.py --golden`, `hack/verify_chain.sh`, `hack/check-doc-links.py`, and
   `integrations/substrate` tested as its own module. Also flag new conformance test skips, a
   committed `go.work`, or a `replace` beyond `integrations/substrate`'s `=> ../..`.

Skip style nits already enforced by tools. Prefer one concrete failing scenario
over speculative comments.
