# integrations/claudecode

An [unmediated](../../docs/harness-authoring.md#unmediated-harnesses) harness that runs Claude Code,
and the `harnessnode` that serves it over harnesswire. It is a separate Go module so the root module
names no model provider.

## Image

Releases after v0.4.0 publish the harness node as an image:

```
ghcr.io/agent-primitives/sessions/claudecode-harnessnode:<tag>
```

- It holds only the statically linked Go binary, at `/ko-app/harnessnode` (the entrypoint), on
  `gcr.io/distroless/static-debian13`, for linux/amd64 and linux/arm64. It runs as root, like the
  `harnessnode` image, because the default `HARNESS_ADDR` is port 80.
- **It does not include the `claude` CLI.** Install the CLI in your own image and copy the binary
  into it:

  ```dockerfile
  FROM ghcr.io/agent-primitives/sessions/claudecode-harnessnode:<tag> AS harnessnode

  FROM <a base image with the claude CLI on PATH>
  COPY --from=harnessnode /ko-app/harnessnode /usr/local/bin/harnessnode
  ENTRYPOINT ["/usr/local/bin/harnessnode"]
  ```

  Run on its own, the image exits with status 1 and
  `configure harness: claudecode: find claude binary`.
- `<tag>` is the root release tag (`vX.Y.Z`), the same as the other images. The root release builds
  this module from the tagged commit, and the module's
  `replace github.com/agent-primitives/sessions => ../..` builds it against the root module of that
  same commit. A release pull request sets the version this module's `go.mod` requires to the
  release tag, so the image SBOM, which names the required version rather than the replacement,
  describes the root module at `<tag>`. An `integrations/claudecode/vX.Y.Z` module tag publishes no
  image.

`go install github.com/agent-primitives/sessions/integrations/claudecode/cmd/harnessnode@<version>`
does not work: `go install` refuses a module whose `go.mod` has a `replace` directive. To build
from source, run `go build ./cmd/harnessnode` in this directory of a checkout.

## Configuration

The harness node reads only its environment, and refuses to start on a configuration error. The
package documentation of [`cmd/harnessnode`](cmd/harnessnode/main.go) is the reference.

Server:

- `HARNESS_KIND`: unset or `claudecode`; any other value is refused.
- `HARNESS_ADDR`: the harnesswire gRPC address, default `:80`; `unix:///path` serves on a unix
  socket.
- `HARNESS_READYZ`: the HTTP readiness address, default `:8081`.
- `HARNESS_MAX_RECV_MSG_SIZE`: the largest harnesswire message in bytes, default gRPC's 4 MiB. It
  also sizes the harness's tool receipts against History.
- `OTEL_EXPORTER_OTLP_ENDPOINT` and the other `OTEL_*` variables: tracing, off when unset.

Harness:

- `HARNESS_CLAUDE_BIN`: the `claude` executable, default `claude` looked up in `PATH`. A relative
  path is resolved against the working directory.
- `HARNESS_CLAUDE_WORKDIR`: the directory Claude Code runs in. Required.
- `HARNESS_CLAUDE_PERMISSION_MODE`: passed as `--permission-mode`. Required.
- `HARNESS_CLAUDE_ALLOW_BYPASS_PERMISSIONS`: a boolean, default false. It must be true when the
  permission mode is `bypassPermissions`, and must not be otherwise.
- `HARNESS_CLAUDE_ENV_ALLOWLIST`: optional comma-separated names of further variables `claude`
  receives from this process's environment. By default it gets only `PATH`, `HOME`, `USER`,
  `SHELL`, `TMPDIR`, `LANG`, `LC_ALL`, `LC_CTYPE`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
  `ANTHROPIC_BASE_URL` and `CLAUDE_CONFIG_DIR`.
- `HARNESS_MODEL`: optional; pins `--model` and the harness's declared model.
- `HARNESS_CLAUDE_MAX_TURNS`, `HARNESS_CLAUDE_MAX_BUDGET_USD`: optional per-turn limits.

No model credential is configured here. `claude` reads `ANTHROPIC_API_KEY` or
`ANTHROPIC_AUTH_TOKEN` from the environment, which must hold only a placeholder that the runtime's
egress replaces; see [security](../../docs/security.md#provider-credentials-and-harness-classes).
