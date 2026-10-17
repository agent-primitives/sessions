#!/usr/bin/env bash
# Runs the Go native fuzz targets.
#
#   hack/fuzz.sh --packages          print the package directories that have fuzz targets, as JSON
#   hack/fuzz.sh <dir> [fuzztime]    run every Fuzz target in ./<dir> for fuzztime each (default 30s)
#
# go test runs one fuzz target per invocation, so the targets in a package run in turn. On a failure
# go test writes the failing input under <dir>/testdata/fuzz/<Target>/ and this script exits non-zero.
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
for target in $targets; do
	echo "==> ./${dir} ${target} for ${fuzztime}"
	go test "./${dir}" -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime"
done
