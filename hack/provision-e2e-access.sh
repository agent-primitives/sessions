#!/usr/bin/env bash
# Provisions the substrate access the conformance suite runs with: every fixed atespace the suite
# uses, and in each an AccessPolicy that grants the suite's ServiceAccount the atespace `editor`
# role. That is the role docs/substrate-conformance.md tells operators to grant agentsessionsd, so
# the suite proves the documented minimum is enough. It runs as kubectl-ate's default identity
# (ate-client, the bootstrap owner of a kind install), since creating an atespace needs a global
# owner and the suite's principal is not one.
#
# The suite reads its atespace and never creates it with this principal: a test whose atespace is
# missing from ATESPACES below fails with a message naming this script. The offline
# TestProvisionScriptListsEveryFixedAtespace in integrations/substrate/e2e fails the per-PR
# integrations job first, when a test names an atespace this list lacks.
#
# Usage: hack/provision-e2e-access.sh
#
# Environment:
#   KUBECTL_ATE        kubectl-ate binary (default kubectl-ate on PATH)
#   KIND_CLUSTER_NAME  kind cluster to provision (default kind)
set -euo pipefail

NAMESPACE="ate-agentsessions"
SERVICE_ACCOUNT="substrate-e2e" # deploy/substrate/e2e-job.yaml
MEMBER="user:system:serviceaccount:${NAMESPACE}:${SERVICE_ACCOUNT}"
ATESPACES=(
	e2e-replay
	e2e-concurrent
	e2e-memory
	e2e-suspend-idle
	e2e-fork
	e2e-suspend
	e2e-idle
	e2e-delete-stateless
	e2e-delete-microvm
	e2e-execution-state
	agentsessions-templates # ACTORTEMPLATE_ATESPACE
)
ATE=("${KUBECTL_ATE:-kubectl-ate}" --context "kind-${KIND_CLUSTER_NAME:-kind}" -o json)

errfile="$(mktemp)"
trap 'rm -f "${errfile}"' EXIT

die() {
	echo "provision-e2e-access: $*" >&2
	exit 1
}

# ate <allowed-code> <args...> prints the command's stdout. A failure whose gRPC code is
# <allowed-code> prints nothing and succeeds; any other failure stops provisioning.
ate() {
	local allowed="$1" output
	shift
	if output="$("${ATE[@]}" "$@" 2>"${errfile}")"; then
		printf '%s' "${output}"
		return 0
	fi
	if [[ -n "${allowed}" ]] && grep -q "rpc error: code = ${allowed} desc =" "${errfile}"; then
		return 0
	fi
	cat "${errfile}" >&2
	die "kubectl-ate $* failed"
}

# A role that covers every RPC the adapter calls: an atespace editor, or an owner.
grants_member() {
	jq -e --arg member "${MEMBER}" \
		'any(.bindings[]?; (.role == "editor" or .role == "owner") and any(.members[]?; . == $member))' \
		>/dev/null
}

for atespace in "${ATESPACES[@]}"; do
	if [[ -z "$(ate NotFound get atespace "${atespace}")" ]]; then
		ate AlreadyExists create atespace "${atespace}" >/dev/null
		echo "created atespace ${atespace}"
	fi

	# `get access-policy` succeeds with empty output when the atespace has no policy.
	policy="$(ate "" get access-policy -a "${atespace}")"
	if [[ -z "${policy}" ]]; then
		printf 'bindings:\n- role: editor\n  members:\n  - %s\n' "${MEMBER}" |
			ate AlreadyExists create access-policy -a "${atespace}" -f - >/dev/null
		# Read it back: an AlreadyExists race leaves whatever policy won it.
		policy="$(ate "" get access-policy -a "${atespace}")"
	fi
	# Never rewrite a policy someone else manages: update replaces the whole policy.
	grants_member <<<"${policy}" ||
		die "atespace ${atespace} has an access policy that does not grant ${MEMBER} editor; add it with: ${ATE[0]} get access-policy -a ${atespace} -o yaml > policy.yaml, add the member to an editor binding, then ${ATE[0]} update access-policy -a ${atespace} -f policy.yaml"
	echo "atespace ${atespace}: ${MEMBER} is an editor"
done
