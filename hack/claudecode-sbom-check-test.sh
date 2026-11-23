#!/usr/bin/env bash
# Tests the SBOM assertion of hack/claudecode-sbom-check.sh against SPDX documents shaped like the
# ones ko writes, so the check needs neither ko nor a build here.
#
# Usage: hack/claudecode-sbom-check-test.sh
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/claudecode-sbom-check.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
cases=0

# sbom <file> <root module versionInfo>...: an SPDX document with the claudecode main module, one
# root module package per version given ("-" for a package without versionInfo), and grpc.
sbom() {
	local file="$1" pkgs v
	shift
	pkgs='{"name": "github.com/agent-primitives/sessions/integrations/claudecode"}'
	for v in "$@"; do
		if [[ "${v}" == - ]]; then
			pkgs+=', {"name": "github.com/agent-primitives/sessions"}'
		else
			pkgs+=", {\"name\": \"github.com/agent-primitives/sessions\", \"versionInfo\": \"${v}\"}"
		fi
	done
	pkgs+=', {"name": "google.golang.org/grpc", "versionInfo": "v1.85.0"}'
	printf '{"spdxVersion": "SPDX-2.3", "packages": [%s]}\n' "${pkgs}" >"${work}/${file}"
}

# expect <description> <pass|fail> <sbom file> <args>...
expect() {
	local desc="$1" want="$2" file="$3" got=pass
	shift 3
	cases=$((cases + 1))
	if ! SBOM="${work}/${file}" "${script}" "$@" >/dev/null 2>&1; then
		got=fail
	fi
	if [[ "${got}" != "${want}" ]]; then
		echo "FAIL ${desc}: got ${got}, want ${want}"
		failures=$((failures + 1))
	else
		echo "ok   ${desc}: ${got}"
	fi
}

sbom tagged.json v0.5.0
sbom previous.json v0.4.0
sbom missing.json
sbom duplicate.json v0.5.0 v0.5.0
sbom noversion.json -
sbom prerelease.json v0.5.0-rc.1

expect "root module at the tag" pass tagged.json v0.5.0
expect "root module at the previous release" fail previous.json v0.5.0
expect "root module missing" fail missing.json v0.5.0
expect "root module listed twice" fail duplicate.json v0.5.0
expect "root module without a version" fail noversion.json v0.5.0
expect "prerelease tag" pass prerelease.json v0.5.0-rc.1
expect "version is a prefix of the tag" fail tagged.json v0.5.0-rc.1
expect "module tag is refused" fail tagged.json integrations/claudecode/v0.5.0
expect "no tag" fail tagged.json
expect "too many arguments" fail tagged.json v0.5.0 v0.4.0
expect "unreadable SBOM" fail absent.json v0.5.0

echo "${cases} cases, ${failures} failed"
((failures == 0))
