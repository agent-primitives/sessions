# Shared settings for up.sh, demo.sh and down.sh. Sourced, not run.
# shellcheck shell=bash disable=SC2034 # the variables are for the scripts that source this file
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
TMP="${TMPDIR:-/tmp}"
WORK="${AGENTSUITE_DEMO_WORK:-${TMP%/}/agentsuite-demo}"
# The demo's cluster is named by its own variable. KIND_CLUSTER_NAME is what substrate's hack/
# scripts read, and docs/substrate-conformance.md exports it for the conformance cluster, so the demo
# sets it rather than inherit a name meant for another cluster.
CLUSTER="${AGENTSUITE_DEMO_CLUSTER:-agentsuite-demo}"
export KIND_CLUSTER_NAME="${CLUSTER}"
# up.sh records the control-plane container ID of a cluster it creates as soon as the container
# exists; down.sh deletes a cluster only when its container still has that ID, so it never deletes a
# cluster up.sh only reused. SETUP records how far up.sh got with that cluster ("<id> created" once
# substrate's create-kind-cluster.sh finished, "<id> installed" once ate-system is installed), so a
# rerun after a failed first setup finishes it instead of reusing a half-built cluster.
OWNED="${WORK}/created-cluster-${CLUSTER}"
SETUP="${WORK}/setup-cluster-${CLUSTER}"
node_id() { docker inspect --format '{{.Id}}' "${CLUSTER}-control-plane" 2>/dev/null || true; }
owned_id() { if [[ -f "${OWNED}" ]]; then cat "${OWNED}"; fi; }
# setup_stage prints the stage SETUP records for the container ID $1, or nothing.
setup_stage() {
	local id stage
	if [[ -f "${SETUP}" ]] && read -r id stage <"${SETUP}" && [[ "${id}" == "$1" ]]; then
		echo "${stage}"
	fi
}
# cluster_listed reports whether kind lists the demo's cluster. A failed listing exits the script: it
# does not show that the cluster is absent, and treating it as absent would let up.sh run substrate's
# create-kind-cluster.sh, which deletes any cluster of that name, or let down.sh drop the records.
cluster_listed() {
	local clusters
	if ! clusters="$(kind get clusters)"; then
		echo "kind get clusters failed, so whether kind cluster ${CLUSTER} exists is unknown; nothing was changed" >&2
		exit 1
	fi
	grep -qx "${CLUSTER}" <<<"${clusters}"
}
# A dedicated kubeconfig: kind would otherwise switch the caller's current context.
export KUBECONFIG="${WORK}/kubeconfig"
NS=agentsuite-demo
ATESPACE=agentsuite-demo
AGENT=triage
# Every cluster call names the demo's kind context explicitly; nothing here uses the caller's
# current context.
K=(kubectl --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
ATE=("${WORK}/bin/kubectl-ate" --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
# The substrate commit the adapter's go.mod pins; the cluster runs the same one.
SUBSTRATE_REF="$(sed -n 's/^  SUBSTRATE_REF: //p' "${ROOT}/.github/workflows/substrate-e2e.yml")"
SUBSTRATE="${SUBSTRATE_DIR:-${WORK}/substrate}"
mkdir -p "${WORK}/bin"
