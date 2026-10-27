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
| Changelog fragment (`changelog` workflow, every pull request, rerun when a label changes) | `go run ./hack/changelog check -base "$(git merge-base upstream/main HEAD)" -head HEAD`, with `origin/main` in a clone without an `upstream` remote (see [Changelog](#changelog)) |
| Release build (`release-check`, on changes to Go, `Dockerfile`, `LICENSE`, `README.md`, `.goreleaser.yaml`, `.ko.yaml`, the `release` and `release-check` workflows, `hack/release-tags.sh`, `hack/release-tags-test.sh`) | `hack/release-tags-test.sh`, `goreleaser check`, then `tags="$(hack/release-tags.sh)" && env ${tags} goreleaser release --snapshot --clean --skip=sbom` (bash; needs `ko`; publishes nothing) and a check that `dist/artifacts.json` lists the `agentsessionsd` and `harnessnode` ko images; `docker build` of `Dockerfile` after `CGO_ENABLED=0 GOOS=linux go build -o agentnode ./cmd/agentnode` |
| Fuzzing (30s per target; nightly longer) | `./hack/fuzz.sh <dir> 30s` for each `<dir>` in `./hack/fuzz.sh --packages`; `hack/fuzz-test.sh` checks the one failure `fuzz.sh` lets pass, the Go 1.26 fuzzing coordinator's own deadline ([golang/go#75804](https://github.com/golang/go/issues/75804)) |

Two gates catch stale generated files, and both fail the build rather than regenerating for you:

```bash
buf generate                 # api/genpb must not change afterwards
./hack/gen-api-reference.sh  # docs/api-reference.md must not change afterwards
```

Run both after any change to `api/*.proto`.

`buf breaking` compares the schema against the most recent `v*` release tag.

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

## Changelog

Do not edit `CHANGELOG.md` in a feature pull request. Add a fragment instead, one file under
`changelog/unreleased/` with a category and an entry; a release assembles the fragments into
`CHANGELOG.md`. Separate files mean concurrent pull requests do not conflict. The format, the
categories and the release step are in [changelog/README.md](changelog/README.md).

The `changelog` workflow fails a pull request that changes a public contract path (`spec.md`,
`api/`, a `.proto` file, or the non-test Go source of a package outside `internal/`, `cmd/`,
`testdata/`, `hack/` and `examples/`) without adding or editing a fragment. Anything else a commit
list cannot convey, especially compatibility notes, also needs one. When a public contract change
needs no release note, such as a refactor, a maintainer adds the `skip-changelog` label.

## Governance

Decisions rest with the maintainers, `@agent-primitives/sessions-maintainers`. There is no wider
governance structure yet; that will change if and when it needs to.

## Releasing

First merge a release pull request that assembles the changelog fragments into a new
`CHANGELOG.md` section, and edit that section: add anything a commit list cannot convey,
especially a compatibility preamble with upgrade steps
([release assembly](changelog/README.md#release-assembly)).

```bash
go run ./hack/changelog assemble vX.Y.Z
```

A release is cut by pushing a tag. The tag is the trigger, so the artifacts and the git history
cannot disagree about what shipped.

```bash
git tag -s v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The `release` workflow first checks that the tagged commit has no changelog fragments left
(`go run ./hack/changelog verify-assembled`); if it has, nothing is published: delete the tag, merge
the release pull request and tag its commit. It re-runs the full suite against the tagged commit
before publishing anything, then builds `agentctl` and `agentsessionsd` for linux and darwin on
amd64 and arm64, attaches an SBOM per archive, publishes the `agentsessionsd` and `harnessnode`
images to `ghcr.io/agent-primitives/sessions`, and signs a build-provenance attestation. Release
notes are generated from the commits since the previous release tag, which is what conventional
commit messages buy.

Only `v<digit>...` tags without a `/` are root-module releases. The nested `integrations/substrate`
module is tagged `integrations/substrate/vX.Y.Z`, which does not trigger the `release` workflow.
goreleaser on its own would still take the nearest tag of either module as the version and as the
start of the changelog, so both workflows set `GORELEASER_CURRENT_TAG` and `GORELEASER_PREVIOUS_TAG`
from `hack/release-tags.sh`. Run goreleaser locally the same way, so that a failure of the script
stops the run instead of leaving goreleaser to pick the tags itself:

```bash
tags="$(hack/release-tags.sh)" && env ${tags} goreleaser release --snapshot --clean
```

In zsh, write `${=tags}`: zsh does not split an unquoted parameter, so `env` would get one argument.

## Reporting security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).
