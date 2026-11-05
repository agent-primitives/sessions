# shellcheck shell=bash
# Helpers the Substrate scripts share: hack/provision-e2e-access.sh, hack/substrate-image.sh and the
# substrate-kind and agentsuite-substrate examples. Sourced, not run; hack/substrate-lib-test.sh
# tests it.
#
# The functions that call kubectl-ate need from the sourcing script:
#   ATE       the kubectl-ate command and its cluster flags (ate adds -o json)
#   die       prints its arguments and exits 1
#   progress  prints a status line
# and, to bound every kubectl-ate call, ATE_DEADLINE (the $SECONDS value by which each call must
# end) with TIMEOUT naming GNU timeout. ATE_FAILURE, when set, is the message ate stops with when
# kubectl-ate fails, instead of "kubectl-ate <args> failed".
#
# Bash runs a command substitution without errexit, and these functions are called from them, so
# every failure here is checked explicitly instead of left to set -e.

# substrate_pin <key> prints the value .github/workflows/substrate-e2e.yml pins for <key>
# (SUBSTRATE_VERSION, the release whose images run, or SUBSTRATE_REF, the commit its tag names), or
# nothing when the workflow sets none.
SUBSTRATE_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
substrate_pin() {
	sed -n "s/^  $1: //p" "${SUBSTRATE_LIB_DIR}/../.github/workflows/substrate-e2e.yml"
}

# ate <allowed> <args...> runs kubectl-ate with -o json and prints the resource it returns. A failure
# whose only gRPC code is <allowed> prints null instead; with <allowed> NoOutput, so does a successful
# call with no output, which is how `get access-policy` reports a missing policy. Any other failure,
# and output that is not a JSON object, stops the script. Call it as x="$(ate ...)" || exit.
ate() {
	local allowed="$1" output codes err rc=0 remaining
	local run=()
	shift
	if [[ -n "${ATE_DEADLINE:-}" ]]; then
		remaining=$((ATE_DEADLINE - SECONDS))
		((remaining > 0)) || die "Template provisioning deadline exceeded."
		((remaining <= 120)) || remaining=120
		run=("${TIMEOUT}" --kill-after=5s "${remaining}s")
	fi
	err="$(mktemp)" || die "Cannot create a file for kubectl-ate's errors."
	output="$(${run[@]+"${run[@]}"} "${ATE[@]}" -o json "$@" 2>"${err}")" || rc=$?
	if ((rc == 0)); then
		rm -f "${err}"
		if [[ "${allowed}" == NoOutput && -z "${output}" ]]; then
			echo null
			return 0
		fi
		jq -e 'type == "object" and length > 0' <<<"${output}" >/dev/null ||
			die "kubectl-ate returned an ambiguous resource."
		printf '%s\n' "${output}"
		return 0
	fi
	# A nested error can carry a second code; only a lone allowed one counts as that outcome.
	codes="$(grep -o 'rpc error: code = [A-Za-z]* desc =' "${err}" | cut -d ' ' -f 5)" || codes=""
	if [[ -n "${allowed}" && "${codes}" == "${allowed}" ]]; then
		rm -f "${err}"
		echo null
		return 0
	fi
	cat "${err}" >&2
	rm -f "${err}"
	die "${ATE_FAILURE:-kubectl-ate $* failed}"
}

# ensure_atespace_editor [--check-only] <atespace> <principal> makes sure the atespace exists and its
# AccessPolicy grants <principal> the editor role, or owner: either covers every Control API call
# agentsessionsd and the e2e suite make. A missing atespace is created. A missing policy is created
# with that one binding, then read back, since a concurrent create may have won. An existing policy
# is never rewritten, since update replaces it whole and it may carry bindings someone else relies
# on: one that does not grant <principal> editor stops the script with the remedy.
#
# --check-only creates nothing: when the atespace is absent it sets ATESPACE_ABSENT=true and returns
# without reading the policy, and a missing policy is reported, not created. ATESPACE_ABSENT is false
# otherwise.
ensure_atespace_editor() {
	local check_only=false space policy
	if [[ "${1:-}" == --check-only ]]; then
		check_only=true
		shift
	fi
	local atespace="$1" member="$2"
	ATESPACE_ABSENT=false

	progress "Checking Substrate atespace ${atespace}"
	space="$(ate NotFound get atespace "${atespace}")" || exit
	if [[ "${space}" == null ]]; then
		if ${check_only}; then
			# shellcheck disable=SC2034 # the caller reads it
			ATESPACE_ABSENT=true
			return 0
		fi
		progress "Creating Substrate atespace ${atespace}"
		ate AlreadyExists create atespace "${atespace}" >/dev/null || exit
		space="$(ate "" get atespace "${atespace}")" || exit
	fi
	jq -e --arg name "${atespace}" '.metadata.name == $name' <<<"${space}" >/dev/null ||
		die "kubectl-ate returned a different atespace."

	progress "Checking the access policy of atespace ${atespace} for ${member}"
	policy="$(ate NoOutput get access-policy -a "${atespace}")" || exit
	if [[ "${policy}" == null ]]; then
		if ${check_only}; then
			progress "Access policy is absent; provisioning will grant ${member} editor"
			return 0
		fi
		progress "Granting ${member} the editor role in atespace ${atespace}"
		printf 'bindings:\n- role: editor\n  members:\n  - %s\n' "${member}" |
			ate AlreadyExists create access-policy -a "${atespace}" -f - >/dev/null || exit
		# Read it back: an AlreadyExists race leaves whatever policy won it.
		policy="$(ate NoOutput get access-policy -a "${atespace}")" || exit
		[[ "${policy}" != null ]] ||
			die "The access policy of atespace ${atespace} disappeared after creation."
	fi
	jq -e --arg member "${member}" \
		'any(.bindings[]?; (.role == "editor" or .role == "owner") and any(.members[]?; . == $member))' \
		<<<"${policy}" >/dev/null ||
		die "atespace ${atespace} has an access policy that does not grant ${member} editor; add it with: ${ATE[0]} get access-policy -a ${atespace} -o yaml > policy.yaml, add the member to an editor binding, then ${ATE[0]} update access-policy -a ${atespace} -f policy.yaml"
}

# kind_ate_setup <substrate checkout> <cluster> <kubeconfig> <release tag> <ate-setup args...> runs
# the checkout's ate-setup against kind cluster <cluster> with the release's images, each pinned to
# its digest, so ate-setup builds none of them. The GCP variables are unset so a shell configured
# for GKE cannot redirect the install.
kind_ate_setup() {
	local dir="$1" cluster="$2" kubeconfig="$3" version="$4"
	shift 4
	(cd "${dir}" && env -u GCE_REGION -u CLUSTER_LOCATION -u NETWORK -u SUBNETWORK \
		-u MEMORYSTORE_INSTANCE -u PROJECT_ID \
		go run ./cmd/ate-setup --kind --no-dev-env --kind-cluster-name="${cluster}" \
		--context="kind-${cluster}" --kubeconfig="${kubeconfig}" \
		--image-repo=ghcr.io/agent-substrate/substrate --image-tag="${version}" \
		--credential-provider='{"name":"k8s.io"}' "$@")
}
