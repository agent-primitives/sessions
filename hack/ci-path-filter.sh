#!/usr/bin/env bash
# Decides whether a pull request changes anything a gated CI workflow checks.
#
# ci, integrations, integrity and substrate-conformance are required status checks, and a workflow
# a `paths` or `paths-ignore` trigger filter skips never reports, so a pull request touching only
# filtered paths could never merge. The gated workflows therefore always run, and their first job
# calls this script: on `skip` the required jobs skip their steps and report success in seconds; on
# `run` they run in full.
#
# Usage: hack/ci-path-filter.sh <ci|substrate> <base> <head>
#
# Prints `run` or `skip` on stdout, and each changed path with its verdict on stderr. Exits non-zero
# on a usage or git error; the gated jobs run in full when the filter job fails.
#
# Each file changed between <base> and <head> is matched against the profile's patterns in order,
# and the last match wins: a plain pattern marks the path ignored, a `!pattern` marks it relevant
# again. The patterns are bash globs matched against the whole path, so `*` also matches `/`
# (`docs/*` is GitHub's `docs/**`, and `*.md` is `**/*.md`). One relevant path means `run`. Renames
# are listed as a deletion plus an addition, so moving code into an ignored path still runs.
set -euo pipefail

usage() {
	echo "usage: hack/ci-path-filter.sh <ci|substrate> <base> <head>" >&2
	exit 2
}

(($# == 3)) || usage
profile="$1"
base="$2"
head="$3"

case "${profile}" in
ci)
	# Prose cannot break the build, except the API reference: it is generated from the protos, and
	# ci fails when the checked-in copy drifts from them, so editing it alone must still run ci.
	patterns=(
		'*.md'
		'LICENSE'
		'!docs/api-reference.md'
	)
	;;
substrate)
	# Nothing here is read by the conformance suite, the images it builds, or the workflow. Any
	# other path, including every workflow file and this script, runs the suite.
	patterns=(
		'docs/*'
		'*.md'
		'site/*'
		'cmd/agentui/*'
		'demos/*'
		'LICENSE'
		'.github/CODEOWNERS'
		'.github/dependabot.yml'
		'.github/copilot-instructions.md'
	)
	;;
*)
	usage
	;;
esac

# Returns 0 when the last pattern matching the path ignores it.
ignored() {
	local path="$1" pattern verdict=1
	for pattern in "${patterns[@]}"; do
		if [[ "${pattern}" == '!'* ]]; then
			# shellcheck disable=SC2053 # the pattern is a glob on purpose
			if [[ "${path}" == ${pattern:1} ]]; then verdict=1; fi
		else
			# shellcheck disable=SC2053 # the pattern is a glob on purpose
			if [[ "${path}" == ${pattern} ]]; then verdict=0; fi
		fi
	done
	return "${verdict}"
}

# Through a file rather than a pipe, so a git failure (an unknown ref) fails the script instead of
# reading as an empty diff, which would mean `skip`.
list="$(mktemp)"
trap 'rm -f "${list}"' EXIT
git diff --name-only --no-renames -z "${base}" "${head}" -- >"${list}"

decision=skip
count=0
while IFS= read -r -d '' path; do
	count=$((count + 1))
	if ignored "${path}"; then
		echo "ignored:  ${path}" >&2
	else
		echo "relevant: ${path}" >&2
		decision=run
	fi
done <"${list}"

echo "${profile}: ${count} changed path(s), decision: ${decision}" >&2
echo "${decision}"
