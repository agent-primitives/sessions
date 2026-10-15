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
# The name is also a path component below, so it is held to what kind accepts, and may not start
# with '.' or '-'.
cluster_name='^[a-z0-9][a-z0-9.-]*$'
if [[ ! "${CLUSTER}" =~ ${cluster_name} ]]; then
	echo "AGENTSUITE_DEMO_CLUSTER=${CLUSTER}: want lowercase letters, digits, '.' and '-', starting with a letter or digit" >&2
	exit 1
fi
export KIND_CLUSTER_NAME="${CLUSTER}"
# up.sh records the control-plane container ID of a cluster it creates as soon as the container
# exists; down.sh deletes a cluster only when its container still has that ID, so it never deletes a
# cluster up.sh only reused. SETUP records how far up.sh got with that cluster ("<id> creating" while
# substrate's create-kind-cluster.sh runs, "<id> created" once it finished, "<id> installed" once
# ate-system is installed), so a rerun after a failed first setup finishes it instead of reusing a
# half-built cluster. up.sh refuses an owned cluster with no record for its node.
OWNED="${WORK}/created-cluster-${CLUSTER}"
SETUP="${WORK}/setup-cluster-${CLUSTER}"
# What up.sh generates for one cluster (its kubeconfig, the rendered composition and manifests)
# lives in STATE, so two clusters never read each other's. The tools in bin/ and the substrate
# checkout are shared.
STATE="${WORK}/cluster-${CLUSTER}"
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
	grep -qxF "${CLUSTER}" <<<"${clusters}"
}
# A dedicated kubeconfig: kind would otherwise switch the caller's current context.
export KUBECONFIG="${STATE}/kubeconfig"
NS=agentsuite-demo
ATESPACE=agentsuite-demo
# The suite (a directory under this one) and the agent in it that becomes the harness. The default
# is the demo's own fixture, which has compositions for amd64 and arm64; kaimahi's minimal suite
# (suites/kaimahi-minimal, agent writer) has only linux/amd64.
SUITE_DIR="${AGENTSUITE_DEMO_SUITE:-suite}"
AGENT="${AGENTSUITE_DEMO_AGENT:-triage}"
if [[ ! "${AGENT}" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
	echo "AGENTSUITE_DEMO_AGENT=${AGENT}: want lowercase letters, digits and '-'" >&2
	exit 1
fi
if [[ "${SUITE_DIR}" == /* || "${SUITE_DIR}" == *..* || ! -f "${HERE}/${SUITE_DIR}/agentsuite.json" ]]; then
	echo "AGENTSUITE_DEMO_SUITE=${SUITE_DIR}: want a suite directory under ${HERE}" >&2
	exit 1
fi
# The sandbox class of the agent's actors: microvm (kata + cloud-hypervisor; needs /dev/kvm in the
# Docker host) unless gvisor is asked for, which is the only class a kind node without KVM (an arm64
# Mac) can run. up.sh refuses microvm without KVM rather than fall back.
SANDBOX="${AGENTSUITE_DEMO_SANDBOX:-microvm}"
case "${SANDBOX}" in
microvm) BINDING_FILE=binding-microvm.json WORKERPOOL_FILE=workerpool-microvm.yaml ;;
gvisor) BINDING_FILE=binding.json WORKERPOOL_FILE=workerpool.yaml ;;
*)
	echo "AGENTSUITE_DEMO_SANDBOX=${SANDBOX}: want microvm or gvisor" >&2
	exit 1
	;;
esac
POOL="agentsuite-demo-${SANDBOX}"
# write_binding SRC OUT [ATESPACE] writes this environment's AgentSessionsBinding: SRC with
# model.name, model.baseURL and model.path set from AGENTSUITE_MODEL, AGENTSUITE_MODEL_BASE_URL and
# AGENTSUITE_MODEL_PATH (no path when that is unset, so the daemon's default applies), and atespace
# set to ATESPACE when one is given. python writes the JSON, so no value is read as markup; emit
# validates the result (check-binding, render) with the code that consumes it.
write_binding() {
	python3 -c '
import json, os, sys
b = json.load(open(sys.argv[1]))
old = b.get("model", {})
m = {"name": os.environ["AGENTSUITE_MODEL"], "provider": old.get("provider"), "baseURL": os.environ["AGENTSUITE_MODEL_BASE_URL"]}
if os.environ.get("AGENTSUITE_MODEL_PATH"):
    m["path"] = os.environ["AGENTSUITE_MODEL_PATH"]
m["secretRef"] = old.get("secretRef")
b["model"] = m
if len(sys.argv) > 3:
    b["atespace"] = sys.argv[3]
with open(sys.argv[2], "w") as f:
    json.dump(b, f, indent=2)
    f.write("\n")' "$@"
}
# Every cluster call names the demo's kind context explicitly; nothing here uses the caller's
# current context.
K=(kubectl --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
ATE=("${WORK}/bin/kubectl-ate" --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
# The substrate commit the adapter's go.mod pins; the cluster runs the same one.
SUBSTRATE_REF="$(sed -n 's/^  SUBSTRATE_REF: //p' "${ROOT}/.github/workflows/substrate-e2e.yml")"
SUBSTRATE="${SUBSTRATE_DIR:-${WORK}/substrate}"
mkdir -p "${WORK}/bin" "${STATE}"
