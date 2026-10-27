#!/usr/bin/env bash
# Tests hack/release-tags.sh against a throwaway git repository whose history mixes root release
# tags with integrations/substrate module tags, as this repository's does.
#
# Usage: hack/release-tags-test.sh
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release-tags.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
cases=0

g() {
	git -C "${repo}" -c user.name=release-tags-test -c user.email=test@example.invalid \
		-c commit.gpgsign=false -c tag.gpgsign=false "$@"
}

commit() {
	g commit -q --allow-empty -m "$1"
}

# expect <description> <want output, or "fail"> <ref to check out> [current-tag]
expect() {
	local desc="$1" want="$2" ref="$3" got
	shift 3
	cases=$((cases + 1))
	g checkout -q --detach "${ref}"
	if ! got="$(cd "${repo}" && "${script}" "$@" 2>/dev/null)"; then
		got="fail"
	fi
	if [[ "${got}" != "${want}" ]]; then
		echo "FAIL ${desc}: got [${got//$'\n'/ }], want [${want//$'\n'/ }]"
		failures=$((failures + 1))
	else
		echo "ok   ${desc}: ${got//$'\n'/ }"
	fi
}

repo="${work}/repo"
mkdir "${repo}"
g init -q
commit one
g tag v0.1.0
commit two
g tag -a -m v0.2.0 v0.2.0
commit three
g tag -a -m v0.3.0 v0.3.0
# The adapter's require bump lands after the root tag and is tagged on its own, as for v0.3.0.
commit "build(substrate): require sessions v0.3.0"
g tag -a -m adapter integrations/substrate/v0.3.0
# A tag with a slash is never a root release, even when it starts with v and a digit.
g tag v9/not-a-release
adapter="$(g rev-parse HEAD)"
commit "a pull request"
pr="$(g rev-parse HEAD)"
commit "the next release"
g tag -a -m v0.3.1 v0.3.1
g tag -a -m adapter integrations/substrate/v0.3.1
next="$(g rev-parse HEAD)"

expect "snapshot past an adapter tag" \
	$'GORELEASER_CURRENT_TAG=v0.3.0\nGORELEASER_PREVIOUS_TAG=v0.2.0' "${pr}"
expect "snapshot on a commit only an adapter tag names" \
	$'GORELEASER_CURRENT_TAG=v0.3.0\nGORELEASER_PREVIOUS_TAG=v0.2.0' "${adapter}"
expect "snapshot on a commit both modules tag" \
	$'GORELEASER_CURRENT_TAG=v0.3.1\nGORELEASER_PREVIOUS_TAG=v0.3.0' "${next}"
expect "release whose parent carries an adapter tag" \
	$'GORELEASER_CURRENT_TAG=v0.3.1\nGORELEASER_PREVIOUS_TAG=v0.3.0' "${next}" v0.3.1
expect "release of a lightweight tag" \
	$'GORELEASER_CURRENT_TAG=v0.2.0\nGORELEASER_PREVIOUS_TAG=v0.1.0' v0.2.0 v0.2.0
expect "first release has no previous tag" \
	'GORELEASER_CURRENT_TAG=v0.1.0' v0.1.0 v0.1.0
expect "adapter tag is refused as the current tag" fail "${next}" integrations/substrate/v0.3.1
expect "slash tag is refused as the current tag" fail "${adapter}" v9/not-a-release
expect "missing tag is refused" fail "${next}" v9.9.9
expect "too many arguments" fail "${next}" v0.3.1 v0.3.0

# No root release tag at all.
repo="${work}/untagged"
mkdir "${repo}"
g init -q
commit one
g tag -a -m adapter integrations/substrate/v0.1.0
expect "snapshot with only an adapter tag" fail HEAD

echo "${cases} cases, ${failures} failed"
((failures == 0))
