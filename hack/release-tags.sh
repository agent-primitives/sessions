#!/usr/bin/env bash
# Prints the root-module release tags goreleaser must use, as GORELEASER_CURRENT_TAG and
# GORELEASER_PREVIOUS_TAG lines for $GITHUB_ENV.
#
# The repository also tags the nested integrations/substrate module (integrations/substrate/vX.Y.Z,
# which Go requires for a nested module). Left to itself, goreleaser takes the nearest tag reachable
# from HEAD as the current tag and the nearest one before it as the previous tag, whichever module
# it belongs to. An adapter tag then becomes the version, the archive names and the image tag, where
# its `/` is not a valid image tag, and bounds the changelog range. goreleaser's OSS edition cannot
# exclude a tag pattern: `git.ignore_tags` matches exact names (globs are Pro-only), and
# `git.ignore_tag_prefixes` and `monorepo.tag_prefix` are Pro-only. Its documented override is these
# two variables, which it takes as given.
#
# A root release tag is `v` followed by a digit, with no `/`.
#
# Usage: hack/release-tags.sh [current-tag]
#
# With a tag (the release workflow passes the pushed tag), that tag is the current tag; it must be a
# root release tag. Without one (a snapshot), the current tag is the newest root release tag
# reachable from HEAD. The previous tag is the newest root release tag reachable from the current
# tag's parent; the line is omitted when there is none, which only the first release can hit.
set -euo pipefail

match=(--match 'v[0-9]*' --exclude '*/*')

if (($# > 1)); then
	echo "usage: hack/release-tags.sh [current-tag]" >&2
	exit 2
fi

if (($# == 1)); then
	current="$1"
	if [[ "${current}" != v[0-9]* || "${current}" == */* ]]; then
		echo "release-tags: ${current} is not a root release tag (v<digit>..., no /)" >&2
		exit 1
	fi
	if ! git rev-parse -q --verify "refs/tags/${current}" >/dev/null; then
		echo "release-tags: tag ${current} does not exist" >&2
		exit 1
	fi
else
	if ! current="$(git describe --tags --abbrev=0 "${match[@]}" HEAD 2>/dev/null)"; then
		echo "release-tags: no root release tag (v<digit>..., no /) is reachable from HEAD" >&2
		exit 1
	fi
fi

echo "GORELEASER_CURRENT_TAG=${current}"
parent="refs/tags/${current}^"
if previous="$(git describe --tags --abbrev=0 "${match[@]}" "${parent}" 2>/dev/null)"; then
	echo "GORELEASER_PREVIOUS_TAG=${previous}"
fi
