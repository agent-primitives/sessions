#!/usr/bin/env bash
# Prints the reference of one image a Substrate release publishes, pinned to the digest its tag
# names now: ghcr.io/agent-substrate/substrate/<image>:<version>@sha256:<digest>. The version is
# SUBSTRATE_VERSION when set, else the one .github/workflows/substrate-e2e.yml pins. Pinning the
# digest keeps a WorkerPool on the bytes it was installed with even if the tag is pushed again.
#
# Needs `docker buildx` and read access to the registry (the release packages are public). Fails,
# naming the image, rather than fall back to building it.
#
# Usage: hack/substrate-image.sh <image>    (for example ateworker-microvm)
set -euo pipefail
# shellcheck source=hack/substrate-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/substrate-lib.sh"

REPO=ghcr.io/agent-substrate/substrate

fail() {
	echo "substrate-image.sh: $*" >&2
	exit 1
}

[[ $# -eq 1 && "$1" =~ ^[a-z0-9][a-z0-9-]*$ ]] || {
	echo "usage: hack/substrate-image.sh <image>" >&2
	exit 2
}
image="$1"

version="${SUBSTRATE_VERSION:-}"
[[ -n "${version}" ]] || version="$(substrate_pin SUBSTRATE_VERSION)"
[[ "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
	fail "SUBSTRATE_VERSION '${version}' is not a release tag (vX.Y.Z or vX.Y.Z-pre)"

ref="${REPO}/${image}:${version}"
docker buildx version >/dev/null 2>&1 ||
	fail "cannot resolve ${ref}: docker buildx is required"
manifest="$(docker buildx imagetools inspect "${ref}" --format '{{json .Manifest}}')" ||
	fail "cannot resolve ${ref}; Substrate may not publish ${image} for ${version}"
# The tag's own digest is the only top-level (two-space indented) "digest"; the platform
# manifests an index lists are indented further. Older buildx releases (v0.19.3 among them)
# ignore --format '{{.Manifest.Digest}}' and print their default text, so parse the JSON.
digest="$(sed -n 's/^  "digest": "\([^"]*\)",*$/\1/p' <<<"${manifest}")"
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] ||
	fail "cannot resolve ${ref}: the registry returned digest '${digest}'"
echo "${ref}@${digest}"
