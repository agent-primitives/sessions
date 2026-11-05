#!/usr/bin/env bash
# Tests hack/substrate-image.sh against a stub docker, so it runs without a registry or network.
#
# Usage: hack/substrate-image-test.sh
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/substrate-image.sh"
workflow="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.github/workflows/substrate-e2e.yml"
pinned="$(sed -n 's/^  SUBSTRATE_VERSION: //p' "${workflow}")"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

digest="sha256:$(printf 'a%.0s' {1..64})"
# The stub logs its arguments and answers per STUB_MODE: ok prints an image index as buildx's
# `{{json .Manifest}}` does, nobuildx fails `docker buildx version`, missing fails the lookup,
# garbage prints something that holds no digest, text prints buildx's default text output.
mkdir "${work}/bin"
cat >"${work}/bin/docker" <<EOF
#!/usr/bin/env bash
echo "\$*" >>"${work}/calls"
[[ "\$1 \$2" != "buildx version" || "\${STUB_MODE}" != nobuildx ]] || exit 1
[[ "\$1 \$2" == "buildx version" ]] && exit 0
case "\${STUB_MODE}" in
ok) printf '{\n  "mediaType": "application/vnd.oci.image.index.v1+json",\n  "digest": "%s",\n  "manifests": [\n    {\n      "digest": "sha256:%s"\n    }\n  ]\n}\n' "${digest}" "$(printf 'b%.0s' {1..64})" ;;
missing) echo "ERROR: not found" >&2; exit 1 ;;
garbage) printf '{\n  "digest": "not-a-digest",\n  "size": 1\n}\n' ;;
text) printf 'Name:      x\nDigest:    %s\n' "${digest}" ;;
esac
EOF
chmod +x "${work}/bin/docker"

failures=0
cases=0

# expect <description> <want stdout, or "fail:<stderr substring>"> <mode> <SUBSTRATE_VERSION> <args...>
expect() {
	local desc="$1" want="$2" mode="$3" version="$4" got err rc=0
	shift 4
	cases=$((cases + 1))
	: >"${work}/calls"
	got="$(PATH="${work}/bin:${PATH}" STUB_MODE="${mode}" SUBSTRATE_VERSION="${version}" \
		"${script}" "$@" 2>"${work}/stderr")" || rc=$?
	err="$(cat "${work}/stderr")"
	if [[ "${want}" == fail:* ]]; then
		if ((rc == 0)) || [[ "${err}" != *"${want#fail:}"* || -n "${got}" ]]; then
			echo "FAIL: ${desc}: want failure mentioning '${want#fail:}', got rc=${rc} stdout='${got}' stderr='${err}'"
			failures=$((failures + 1))
		fi
	elif ((rc != 0)) || [[ "${got}" != "${want}" ]]; then
		echo "FAIL: ${desc}: want '${want}', got rc=${rc} stdout='${got}' stderr='${err}'"
		failures=$((failures + 1))
	fi
}

repo=ghcr.io/agent-substrate/substrate
expect "the workflow's version by default" "${repo}/ateworker-microvm:${pinned}@${digest}" ok "" ateworker-microvm
grep -qxF "buildx imagetools inspect ${repo}/ateworker-microvm:${pinned} --format {{json .Manifest}}" "${work}/calls" ||
	{
		echo "FAIL: the lookup did not name the pinned tag: $(cat "${work}/calls")"
		failures=$((failures + 1))
	}
expect "SUBSTRATE_VERSION overrides the workflow" "${repo}/ateworker-gvisor:v9.9.9-rc.1@${digest}" ok v9.9.9-rc.1 ateworker-gvisor
expect "an image the release does not publish" "fail:cannot resolve ${repo}/counter:${pinned}" missing "" counter
expect "a lookup that returns no digest" "fail:returned digest 'not-a-digest'" garbage "" ateworker-microvm
expect "text output instead of JSON" "fail:returned digest ''" text "" ateworker-microvm
expect "docker without buildx" "fail:docker buildx is required" nobuildx "" ateworker-microvm
expect "a version that is not a release tag" "fail:is not a release tag" ok main ateworker-microvm
expect "a commit is not a release tag" "fail:is not a release tag" ok b34fbee742906494b5eb4f1f36987176bf8770a4 ateworker-microvm
expect "no image" "fail:usage" ok "" ""
expect "an image name with a path" "fail:usage" ok "" ../ateworker-microvm
expect "two images" "fail:usage" ok "" ateworker-microvm ateworker-gvisor

[[ -n "${pinned}" ]] || {
	echo "FAIL: substrate-e2e.yml sets no SUBSTRATE_VERSION"
	failures=$((failures + 1))
}

if ((failures > 0)); then
	echo "${failures} of ${cases} cases failed"
	exit 1
fi
echo "ok: ${cases} cases"
