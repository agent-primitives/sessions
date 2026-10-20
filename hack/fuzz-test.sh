#!/usr/bin/env bash
# Tests hack/fuzz.sh's verdict on go test's fuzzing output. A run that fails only with the fuzzing
# coordinator's own fuzztime deadline (golang/go#75804) passes; every real finding still fails.
#
# The deadline race cannot be triggered on demand, so a stub go replays go test's output for those
# cases (the deadline output is copied from a CI run on go1.26.9). A real crasher also goes through
# the real toolchain.
#
# Usage: hack/fuzz-test.sh
set -euo pipefail

src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fuzz.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
real_path="${PATH}"

failures=0
cases=0

# new_root copies fuzz.sh into a fresh tree with one package, pkg, so the script runs there.
new_root() {
	root="$(mktemp -d "${work}/root.XXXXXX")"
	mkdir -p "${root}/hack" "${root}/pkg"
	cp "${src}" "${root}/hack/fuzz.sh"
}

# The stub go lists the targets in ./targets. A fuzz run of <Target> prints ./<Target>.out, copies
# ./<Target>.input into pkg/testdata/fuzz/<Target>/ when it exists, and exits with ./<Target>.rc.
mkdir -p "${work}/bin"
cat >"${work}/bin/go" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
"test -list ^Fuzz ./pkg")
	cat targets
	printf 'ok  \tfuzz/pkg\t0.010s\n'
	;;
"test ./pkg -run ^\$ -fuzz ^"*"\$ -fuzztime 30s")
	target="${6#^}"
	target="${target%\$}"
	cat "${target}.out"
	if [[ -f "${target}.input" ]]; then
		mkdir -p "pkg/testdata/fuzz/${target}"
		cp "${target}.input" "pkg/testdata/fuzz/${target}/4cdf0014b41299fb"
	fi
	exit "$(cat "${target}.rc")"
	;;
*)
	echo "stub go: unexpected arguments: $*" >&2
	exit 2
	;;
esac
STUB
chmod +x "${work}/bin/go"

# run_case <Target> <exit status> <output> [writes-input] adds a target to the stub's package.
run_case() {
	echo "$1" >>"${root}/targets"
	echo "$2" >"${root}/$1.rc"
	printf '%s\n' "$3" >"${root}/$1.out"
	if [[ "${4:-}" == writes-input ]]; then
		echo 'go test fuzz v1' >"${root}/$1.input"
	fi
}

progress() {
	printf 'fuzz: elapsed: 0s, gathering baseline coverage: 0/5 completed\n'
	printf 'fuzz: elapsed: 0s, gathering baseline coverage: 5/5 completed, now fuzzing with 4 workers\n'
	printf 'fuzz: elapsed: 3s, execs: 8634 (2878/sec), new interesting: 35 (total: 40)\n'
	printf 'fuzz: elapsed: 31s, execs: 129836 (0/sec), new interesting: 129 (total: 134)\n'
}

# deadline <Target> is go test's output when the coordinator reports its own deadline.
deadline() {
	progress
	printf -- '--- FAIL: %s (31.01s)\n    context deadline exceeded\nFAIL\nexit status 1\n' "$1"
	printf 'FAIL\tfuzz/pkg\t31.034s\n'
}

passed() {
	progress
	printf 'PASS\nok  \tfuzz/pkg\t31.021s\n'
}

crashed() {
	progress
	printf -- '--- FAIL: %s (2.10s)\n    --- FAIL: %s (0.00s)\n        fuzz_test.go:10: boom\n' "$1" "$1"
	printf '    \n    Failing input written to testdata/fuzz/%s/4cdf0014b41299fb\n' "$1"
	printf '    To re-run:\n    go test -run=%s/4cdf0014b41299fb\nFAIL\nexit status 1\n' "$1"
	printf 'FAIL\tfuzz/pkg\t2.130s\n'
}

# expect <pass|fail> <description> [text the output must contain]
expect() {
	local want="$1" desc="$2" grep_for="${3:-}" got out
	cases=$((cases + 1))
	out="${root}/out"
	if (cd "${root}" && hack/fuzz.sh pkg 30s) >"${out}" 2>&1; then got=pass; else got=fail; fi
	if [[ "${got}" != "${want}" ]]; then
		echo "FAIL ${desc}: got ${got}, want ${want}"
		sed 's/^/     | /' "${out}"
		failures=$((failures + 1))
	elif [[ -n "${grep_for}" ]] && ! grep -qF -- "${grep_for}" "${out}"; then
		echo "FAIL ${desc}: output does not contain ${grep_for}"
		sed 's/^/     | /' "${out}"
		failures=$((failures + 1))
	else
		echo "ok   ${desc}: ${got}"
	fi
}

PATH="${work}/bin:${real_path}"

new_root
run_case FuzzA 0 "$(passed)"
expect pass "a clean run"

new_root
run_case FuzzA 1 "$(deadline FuzzA)"
expect pass "only the coordinator's deadline" "counting this run as a pass"

new_root
mkdir -p "${root}/pkg/testdata/fuzz/FuzzA"
echo 'go test fuzz v1' >"${root}/pkg/testdata/fuzz/FuzzA/checked-in"
run_case FuzzA 1 "$(deadline FuzzA)"
expect pass "the coordinator's deadline beside a checked-in input"

new_root
run_case FuzzA 0 "$(passed)"
run_case FuzzB 1 "$(deadline FuzzB)"
run_case FuzzC 0 "$(passed)"
expect pass "the coordinator's deadline, then the next target runs" "==> ./pkg FuzzC"

new_root
run_case FuzzA 1 "$(crashed FuzzA)" writes-input
expect fail "a crasher" "Failing input written to"

new_root
run_case FuzzA 1 "$(deadline FuzzA)" writes-input
expect fail "the coordinator's deadline with a new input written"

new_root
run_case FuzzA 1 "$(
	printf 'fuzz: minimizing 34-byte failing input file\n'
	deadline FuzzA
)"
expect fail "the coordinator's deadline while minimizing a crasher"

new_root
run_case FuzzA 1 "$(
	progress
	printf -- '--- FAIL: FuzzA (31.01s)\n    fuzzing process hung or terminated unexpectedly: exit status 2\n'
	printf 'FAIL\nexit status 1\nFAIL\tfuzz/pkg\t31.034s\n'
)"
expect fail "a hung worker"

new_root
run_case FuzzA 1 "$(
	printf '    fuzz_test.go:12: decoded 3 frames\n'
	deadline FuzzA
)"
expect fail "the coordinator's deadline after other output"

new_root
run_case FuzzA 1 "$(deadline FuzzB)"
expect fail "a deadline reported for another target"

new_root
run_case FuzzA 1 "$(deadline FuzzA)"
run_case FuzzB 1 "$(crashed FuzzB)" writes-input
expect fail "a crasher after a tolerated deadline" "Failing input written to"

# The real toolchain: a target that fails on any input longer than 8 bytes.
PATH="${real_path}"
new_root
printf 'module fuzz\n\ngo 1.26\n' >"${root}/go.mod"
cat >"${root}/pkg/pkg_test.go" <<'SRC'
package pkg

import "testing"

func FuzzCrash(f *testing.F) {
	f.Add([]byte("a"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 8 {
			t.Fatalf("crashed on %d bytes", len(b))
		}
	})
}
SRC
expect fail "a real crasher (go test)" "Failing input written to testdata/fuzz/FuzzCrash/"
if ! compgen -G "${root}/pkg/testdata/fuzz/FuzzCrash/*" >/dev/null; then
	echo "FAIL a real crasher (go test): no input under pkg/testdata/fuzz/FuzzCrash/"
	failures=$((failures + 1))
fi

echo "${cases} cases, ${failures} failed"
[[ "${failures}" -eq 0 ]]
