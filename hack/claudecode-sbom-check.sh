#!/usr/bin/env bash
# Checks that the claudecode-harnessnode image SBOM names the root module at the release tag.
#
# integrations/claudecode requires github.com/agent-primitives/sessions at some version and
# replaces it with ../.., so the image is built from the root module of the tagged tree. ko's SPDX
# describes each dependency by its required version and ignores the replacement, so the image SBOM
# names whatever version go.mod requires. A release pull request therefore sets that requirement to
# the release tag (CONTRIBUTING.md, Releasing); this check fails the release before anything is
# published when it does not.
#
# Usage: hack/claudecode-sbom-check.sh <tag>
#
# Builds the image for one platform with ko, without pushing, and reads the SPDX ko writes, the same
# document goreleaser pushes beside the image. With SBOM=<file> set, checks that file instead of
# building, which hack/claudecode-sbom-check-test.sh uses.
set -euo pipefail

root_module=github.com/agent-primitives/sessions

if (($# != 1)); then
	echo "usage: hack/claudecode-sbom-check.sh <tag>" >&2
	exit 2
fi
tag="$1"
if [[ "${tag}" != v[0-9]* || "${tag}" == */* ]]; then
	echo "claudecode-sbom-check: ${tag} is not a root release tag (v<digit>..., no /)" >&2
	exit 1
fi

sbom="${SBOM:-}"
if [[ -z "${sbom}" ]]; then
	repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
	work="$(mktemp -d)"
	trap 'rm -rf "${work}"' EXIT
	# The repository and base image match the claudecode-harnessnode kos entry in .goreleaser.yaml.
	# --push=false builds the image and writes its SBOM without publishing or loading it anywhere.
	(
		cd "${repo}/integrations/claudecode"
		KO_DOCKER_REPO=ghcr.io/agent-primitives/sessions/claudecode-harnessnode \
			KO_DEFAULTBASEIMAGE=gcr.io/distroless/static-debian13 \
			ko build --bare --push=false --platform=linux/amd64 \
			--sbom=spdx --sbom-dir="${work}" ./cmd/harnessnode >/dev/null
	)
	shopt -s nullglob
	files=("${work}"/*.spdx.json)
	if ((${#files[@]} != 1)); then
		echo "claudecode-sbom-check: ko wrote ${#files[@]} SPDX files, want 1" >&2
		exit 1
	fi
	sbom="${files[0]}"
fi

versions="$(jq -r --arg m "${root_module}" '[.packages[] | select(.name == $m) | .versionInfo // "none"] | join(" ")' "${sbom}")"
if [[ "${versions}" != "${tag}" ]]; then
	echo "claudecode-sbom-check: the claudecode-harnessnode SBOM names ${root_module} at [${versions}], want ${tag}" >&2
	echo "The release pull request sets the requirement to the release tag:" >&2
	echo "  (cd integrations/claudecode && go mod edit -require=${root_module}@${tag})" >&2
	exit 1
fi
echo "claudecode-harnessnode SBOM names ${root_module} ${tag}"
