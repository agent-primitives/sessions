#!/usr/bin/env bash
# Tests hack/ci-path-filter.sh against throwaway git repositories, one commit per case.
#
# A wrong `skip` lets a pull request merge without the checks it needed, so each profile is driven
# through the pull request shapes that matter: prose only, site only, Go, workflow, a rename out of
# code, and a git error.
#
# Usage: hack/ci-path-filter-test.sh
set -euo pipefail

filter="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ci-path-filter.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
cases=0

g() {
	git -C "${repo}" -c user.name=ci-path-filter-test -c user.email=test@example.invalid \
		-c commit.gpgsign=false "$@"
}

# Starts a repository whose base commit holds every path a case modifies, renames or deletes.
new_repo() {
	repo="$(mktemp -d "${work}/repo.XXXXXX")"
	g init -q
	mkdir -p "${repo}/controller" "${repo}/docs" "${repo}/site/src" "${repo}/.github/workflows"
	echo 'package controller' >"${repo}/controller/controller.go"
	echo '# controller' >"${repo}/controller/README.md"
	echo '# docs' >"${repo}/docs/concepts.md"
	echo '# api' >"${repo}/docs/api-reference.md"
	echo 'site' >"${repo}/site/src/index.mdx"
	echo 'name: ci' >"${repo}/.github/workflows/ci.yml"
	echo 'license' >"${repo}/LICENSE"
	g add -A
	g commit -q -m base
}

# expect <profile> <run|skip> <description> <command that edits the tree>
expect() {
	local profile="$1" want="$2" desc="$3" got
	shift 3
	cases=$((cases + 1))
	new_repo
	(cd "${repo}" && "$@")
	g add -A
	g commit -q --allow-empty -m change
	if ! got="$(cd "${repo}" && "${filter}" "${profile}" HEAD^1 HEAD 2>/dev/null)"; then
		echo "FAIL ${profile}: ${desc}: filter exited non-zero"
		failures=$((failures + 1))
	elif [[ "${got}" != "${want}" ]]; then
		echo "FAIL ${profile}: ${desc}: got ${got}, want ${want}"
		failures=$((failures + 1))
	else
		echo "ok   ${profile}: ${desc}: ${got}"
	fi
}

edit() {
	local path
	for path in "$@"; do
		mkdir -p "$(dirname "${path}")"
		echo edit >>"${path}"
	done
}

# substrate-conformance
expect substrate skip "docs only" edit docs/concepts.md docs/diagram.svg
expect substrate skip "markdown outside docs" edit README.md controller/README.md
expect substrate skip "site only" edit site/src/index.mdx site/package.json
expect substrate skip "agentui and demos" edit cmd/agentui/main.go demos/demo.tape
expect substrate skip "license, codeowners, dependabot, copilot instructions" \
	edit LICENSE .github/CODEOWNERS .github/dependabot.yml .github/copilot-instructions.md
expect substrate skip "no changes" true
expect substrate run "go change" edit controller/controller.go
expect substrate run "go change alongside docs" edit controller/controller.go docs/concepts.md
expect substrate run "workflow change" edit .github/workflows/ci.yml
expect substrate run "new workflow" edit .github/workflows/new.yml
expect substrate run "other .github file" edit .github/pull_request_template.yaml
expect substrate run "filter script change" edit hack/ci-path-filter.sh
expect substrate run "filter test change" edit hack/ci-path-filter-test.sh
expect substrate run "deploy manifest" edit deploy/substrate/namespace.yaml
expect substrate run "go deleted" git rm -q controller/controller.go
expect substrate run "go renamed into docs" git mv controller/controller.go docs/controller.md
expect substrate run "path with a space and a newline" edit $'controller/a b\nc.go'
expect substrate run "docs-like name outside docs" edit docsite/main.go
expect substrate run "LICENSE-like name" edit LICENSE.go

# ci
expect ci skip "markdown only" edit README.md docs/concepts.md controller/README.md
expect ci skip "license" edit LICENSE
expect ci skip "no changes" true
expect ci run "api reference" edit docs/api-reference.md
expect ci run "api reference alongside docs" edit docs/api-reference.md docs/concepts.md
expect ci run "site" edit site/src/index.mdx
expect ci run "non-markdown docs" edit docs/diagram.svg
expect ci run "go change" edit controller/controller.go
expect ci run "agentui" edit cmd/agentui/main.go
expect ci run "workflow change" edit .github/workflows/ci.yml
expect ci run "filter script change" edit hack/ci-path-filter.sh
expect ci run "filter test change" edit hack/ci-path-filter-test.sh
expect ci run "dependabot config" edit .github/dependabot.yml
expect ci run "go renamed to markdown" git mv controller/controller.go controller/notes.md

# Errors must not read as `skip`.
cases=$((cases + 1))
new_repo
if out="$(cd "${repo}" && "${filter}" substrate HEAD no-such-ref 2>/dev/null)"; then
	echo "FAIL unknown ref: exited zero with ${out}"
	failures=$((failures + 1))
else
	echo "ok   unknown ref: exited non-zero"
fi
for args in "nope HEAD HEAD" "substrate HEAD"; do
	cases=$((cases + 1))
	# shellcheck disable=SC2086 # split the argument list on purpose
	if (cd "${repo}" && "${filter}" ${args} >/dev/null 2>&1); then
		echo "FAIL usage '${args}': exited zero"
		failures=$((failures + 1))
	else
		echo "ok   usage '${args}': exited non-zero"
	fi
done

echo "${cases} cases, ${failures} failed"
((failures == 0))
