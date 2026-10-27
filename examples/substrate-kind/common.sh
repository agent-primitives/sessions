# shellcheck shell=bash disable=SC2034

# Shared settings and helpers for the example's scripts; source this file rather than execute it.
# Keep generated tools, ownership records and kubeconfig separate for each cluster.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
WORK="${SUBSTRATE_KIND_WORK:-${ROOT}/_local/substrate-kind}"
CLUSTER="${CLUSTER:-sessions-example}"
[[ "${CLUSTER}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || {
	echo "CLUSTER must be a lowercase kind cluster name" >&2
	exit 2
}
mkdir -p "${WORK}"
WORK="$(cd "${WORK}" && pwd -P)"
STATE="${WORK}/${CLUSTER}"
mkdir -p "${STATE}"
OWNER="${STATE}/owner"
BOOTSTRAP="${WORK}/substrate-bootstrap-incomplete"
export KIND_CLUSTER_NAME="${CLUSTER}"
# Commands run from both the caller's directory and Substrate's checkout.
KUBECONFIG="${SUBSTRATE_KIND_KUBECONFIG:-${STATE}/kubeconfig}"
[[ "${KUBECONFIG}" == /* ]] || KUBECONFIG="${PWD}/${KUBECONFIG}"
export KUBECONFIG

# Use the workflow's Substrate revision for compatible tools and control-plane components.
SUBSTRATE_DIR="${SUBSTRATE_DIR:-${WORK}/substrate}"
SUBSTRATE_REF="$(sed -n 's/^  SUBSTRATE_REF: //p' "${ROOT}/.github/workflows/substrate-e2e.yml")"
NS=sessions-example
PROJECT="${PROJECT:-example}"
ACTOR_TEMPLATE="${ACTOR_TEMPLATE:-chat-harness}"
SANDBOX_CLASS="${SANDBOX_CLASS:-microvm}"
SANDBOX_CONFIG="${SANDBOX_CONFIG:-}"
SNAPSHOT_LOCATION="${SNAPSHOT_LOCATION:-gs://ate-snapshots/sessions-example/}"

# Every cluster command names its context and kubeconfig instead of relying on the caller's default.
K=(kubectl --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
ATE=("${STATE}/kubectl-ate" --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}" -o json)
CREATE_PENDING=0

# Separate script-level phase messages from command output without contaminating captured stdout.
progress() { printf '\n==> [substrate-kind/%s] %s\n' "${0##*/}" "$*" >&2; }
die() { progress "ERROR: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

# GNU coreutils installs timeout as gtimeout on macOS.
timeout_command() {
	if command -v timeout >/dev/null 2>&1; then
		TIMEOUT=timeout
	elif command -v gtimeout >/dev/null 2>&1; then
		TIMEOUT=gtimeout
	else
		die "GNU timeout is required (macOS: install coreutils)"
	fi
}

# An unsuccessful bootstrap must never expose its unpinned tools on a later invocation.
check_managed_bootstrap() {
	if [[ "${SUBSTRATE_DIR}" == "${WORK}/substrate" && (-e "${BOOTSTRAP}" || -L "${BOOTSTRAP}") ]]; then
		die "Incomplete managed Substrate bootstrap; inspect ${SUBSTRATE_DIR} and ${BOOTSTRAP}. Remove the incomplete checkout and bootstrap marker explicitly before retrying."
	fi
}

# Prefer Substrate's pinned kind wrapper when its checkout is available.
kind_command() {
	check_managed_bootstrap
	if [[ -x "${SUBSTRATE_DIR}/hack/kind.sh" ]]; then
		(cd "${SUBSTRATE_DIR}" && hack/kind.sh "$@")
	else
		kind "$@"
	fi
}

# The full Docker ID distinguishes an owned cluster from a replacement with the same name.
node_id() {
	local name="${CLUSTER//./\\.}"
	docker ps --all --no-trunc --filter "name=^/${name}-control-plane$" --format '{{.ID}}'
}

# Reconcile kind, Docker and the ownership record. Refuse contradictory or foreign
# state rather than allowing setup or teardown to guess which cluster it owns.
cluster_state() {
	local clusters id owner stage ref
	clusters="$(kind_command get clusters)" || die "Cannot list clusters; nothing was changed."
	id="$(node_id)" || die "Cannot inspect Docker containers; nothing was changed."
	if grep -qxF "${CLUSTER}" <<<"${clusters}"; then
		if [[ -z "${id}" || ! -f "${OWNER}" ]] ||
			! read -r owner stage ref <"${OWNER}" || [[ "${owner}" != "${id}" ]]; then
			die "Refusing foreign or replaced cluster ${CLUSTER}: no matching container-ID ownership record."
		fi
		case "${stage}" in
		creating | created | installed) echo "${stage}" ;;
		*) die "Ambiguous ownership state for ${CLUSTER}." ;;
		esac
	elif [[ -n "${id}" ]]; then
		die "Unlisted control-plane container for ${CLUSTER}; nothing was changed."
	else
		echo absent
	fi
}

# If creation failed partway through, retain its container identity for explicit teardown.
# Release the lock without replacing the original command's exit status.
unlock() {
	local result=$? id
	if [[ "${CREATE_PENDING}" == 1 ]]; then
		if id="$(node_id)"; then
			if [[ -n "${id}" ]]; then printf '%s creating %s\n' "${id}" "${SUBSTRATE_REF}" >"${OWNER}"; fi
		else
			echo "Cannot inspect interrupted creation; refusing to claim ownership." >&2
		fi
	fi
	rmdir "${STATE}/lock"
	exit "${result}"
}

# Setup and teardown share this lock; exit and signal traps also cover interrupted creation.
lock_cluster() {
	mkdir "${STATE}/lock" 2>/dev/null || die "Another setup/teardown holds ${STATE}/lock."
	trap unlock EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
}

# Replace manifest placeholders literally, escaping sed's replacement syntax.
# Reject multiline values so substitutions cannot introduce extra YAML fields.
render() {
	local file="$1" key value
	shift
	local args=()
	while (($#)); do
		key="$1"; value="$2"; shift 2
		[[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || die "Multiline manifest replacement refused."
		value="${value//\\/\\\\}"; value="${value//|/\\|}"; value="${value//&/\\&}"
		args+=(-e "s|${key}|${value}|g")
	done
	sed "${args[@]}" "${file}"
}
