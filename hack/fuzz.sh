#!/usr/bin/env bash
# Runs the Go native fuzz targets.
#
#   hack/fuzz.sh --packages          print the package directories that have fuzz targets, as JSON
#   hack/fuzz.sh <dir> [fuzztime]    run every Fuzz target in ./<dir> for fuzztime each (default 30s)
#
# go test runs one fuzz target per invocation, so the targets in a package run in turn. On a failure
# go test writes the failing input under <dir>/testdata/fuzz/<Target>/ and this script exits non-zero.
#
# One failure is not a finding: before Go 1.27, the fuzzing coordinator can report its own fuzztime
# deadline as the failure when the deadline races its cancellation of the workers
# (https://github.com/golang/go/issues/75804; Go 1.26 backport: golang/go#82362). go test then prints
# only "context deadline exceeded" and writes no input. A target whose output is exactly that, with
# no new file under its testdata/fuzz/<Target>/, counts as a pass, and the log says so. Any other
# failure, or that one with a new input written, fails the script. Drop this once the toolchain in
# go.mod carries the fix.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ "${1:-}" = "--packages" ]; then
	git grep --untracked -l '^func Fuzz' -- '*_test.go' | xargs -n1 dirname | sort -u |
		python3 -c 'import json, sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))'
	exit 0
fi

dir="${1:?usage: hack/fuzz.sh --packages | <dir> [fuzztime]}"
fuzztime="${2:-30s}"
targets="$(go test -list '^Fuzz' "./${dir}" | grep '^Fuzz' || true)"
if [ -z "$targets" ]; then
	echo "no fuzz targets in ./${dir}" >&2
	exit 1
fi

# corpus_files <target> lists the inputs under the target's corpus directory.
corpus_files() {
	if [ -d "${dir}/testdata/fuzz/$1" ]; then
		find "${dir}/testdata/fuzz/$1" -type f | LC_ALL=C sort
	fi
}

# deadline_only <target> <output> reports whether the output, without its progress lines, is
# exactly the coordinator's own deadline error for that target.
deadline_only() {
	local target="$1" rest nl=$'\n' tab=$'\t'
	rest="$(grep -v '^fuzz: elapsed: ' "$2" || true)"
	local want="^--- FAIL: ${target} \([0-9.]+s\)${nl}    context deadline exceeded${nl}FAIL${nl}"
	want+="exit status 1${nl}FAIL${tab}[^[:space:]]+${tab}[0-9.]+s\$"
	[[ "$rest" =~ $want ]]
}

log="$(mktemp)"
trap 'rm -f "$log"' EXIT
for target in $targets; do
	echo "==> ./${dir} ${target} for ${fuzztime}"
	before="$(corpus_files "$target")"
	if go test "./${dir}" -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" 2>&1 | tee "$log"; then
		continue
	fi
	if [ "$(corpus_files "$target")" = "$before" ] && deadline_only "$target" "$log"; then
		echo "::warning::${dir} ${target}: the fuzzing coordinator reported its own fuzztime deadline" \
			"and wrote no input (golang/go#75804); counting this run as a pass"
		continue
	fi
	exit 1
done
