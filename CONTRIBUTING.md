# Contributing

Thanks for your interest. This project is pre-1.0 and the contract is still moving, so open an issue
before starting anything large.

## Build and test

```bash
go build ./...
go test -race -shuffle=on ./...
```

`integrations/substrate` is a separate Go module and is not reached by `./...` from the root. If you
touch it, test it on its own:

```bash
cd integrations/substrate && go test -race -shuffle=on ./...
```

## What CI checks

Run these before pushing; they are the same gates the `ci` workflow enforces.

| Gate | Command |
|---|---|
| Formatting | `gofmt -l .` must print nothing |
| Build and vet | `go build ./...`, `go vet ./...` |
| Tests | `go test -race -shuffle=on ./...` (a failure prints the seed; rerun with `-shuffle=<seed>`) |
| Vendor neutrality | `go list -deps ./... \| grep -c agent-substrate` must be `0` |
| Provenance self-check | `python3 hack/verify_chain.py --golden` |
| Cross-language chain audit | `./hack/verify_chain.sh` (also audits every journal in `sqlitelog/testdata`) |
| Daemon smoke test (local runtime, no substrate) | `go test ./cmd/agentsessionsd -run TestSessionLifecycleRealBinaries` (part of Tests) |
| Persisted-format compatibility | `go test ./session ./sqlitelog -run 'SchemaV4Fixture\|MigratedAndFresh'` (part of Tests) |
| Proto lint | `buf lint` |
| Go blocks in docs (`docs` workflow, every pull request) | `go run ./hack/doccheck` |
| Release build (`release-check`, on changes to Go, `Dockerfile`, `LICENSE`, `README.md`, `.goreleaser.yaml`, `.ko.yaml`) | `goreleaser check`, then `goreleaser release --snapshot --clean --skip=sbom` (needs `ko`; publishes nothing) and a check that `dist/artifacts.json` lists the `agentsessionsd` and `harnessnode` ko images; `docker build` of `Dockerfile` after `CGO_ENABLED=0 GOOS=linux go build -o agentnode ./cmd/agentnode` |
| Fuzzing (30s per target; nightly longer) | `./hack/fuzz.sh <dir> 30s` for each `<dir>` in `./hack/fuzz.sh --packages` |

Two gates catch stale generated files, and both fail the build rather than regenerating for you:

```bash
buf generate                 # api/genpb must not change afterwards
./hack/gen-api-reference.sh  # docs/api-reference.md must not change afterwards
```

Run both after any change to `api/*.proto`.

`buf breaking` compares the schema against the most recent release tag. There is no tag yet, so it
currently skips. It arms itself at the first release.

## Conventions

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/). Keep the
  description to one imperative line.
- **The core stays vendor-neutral.** No cloud, sandbox, or provider-specific code or naming in the
  root module. Concrete backends live in their own modules or repositories, which is what the vendor
  neutrality gate enforces.
- **Go examples in docs get compile-checked** before merging. A snippet that does not build is worse
  than no snippet, and several already sit directly on top of SPIs that are still changing. Every
  fenced block tagged `go` needs a `<!-- doccheck: ... -->` marker on the line before it that says
  how `hack/doccheck` checks it (`decls`, `file`, `body(PARAMS)`, or `skip REASON` for an excerpt);
  see the comment at the top of `hack/doccheck/main.go`.
- **Docs land with the code they describe**, not ahead of it. If a change alters an interface that a
  document shows, update the document in the same pull request.

## Governance

Decisions rest with the maintainers, `@agent-primitives/sessions-maintainers`. There is no wider
governance structure yet; that will change if and when it needs to.

## Releasing

A release is cut by pushing a tag. The tag is the trigger, so the artifacts and the git history
cannot disagree about what shipped.

```bash
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The `release` workflow re-runs the full suite against the tagged commit before publishing anything,
then builds `agentctl` and `agentsessionsd` for linux and darwin on amd64 and arm64, attaches an
SBOM per archive, and signs a build-provenance attestation. Release notes are generated from the
commits since the previous tag, which is what conventional commit messages buy.

Add anything a commit list cannot convey, especially compatibility notes, to `CHANGELOG.md` before
tagging.

Note that the first tag also arms `buf breaking`, which compares the schema against the most recent
release and is inactive until one exists.

## Reporting security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).
