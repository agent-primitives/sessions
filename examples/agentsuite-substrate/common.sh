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
# substrate's create-kind-cluster.sh runs, "<id> created" once it finished, "<id> installed <commit>"
# once ate-system is installed from substrate <commit>), so a rerun after a failed first setup
# finishes it instead of reusing a half-built cluster. up.sh refuses an owned cluster with no record
# for its node, and one installed from another substrate commit (refuse_stale_install).
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
	if [[ -f "${SETUP}" ]] && read -r id stage _ <"${SETUP}" && [[ "${id}" == "$1" ]]; then
		echo "${stage}"
	fi
}
# refuse_stale_install exits when SETUP records that up.sh installed ate-system on the container ID
# $1 from a substrate commit other than SUBSTRATE_REF, or from one it did not record (an install
# from before up.sh kept the commit). Substrate supports no in-place upgrade between releases before
# 1.0 (its docs/upgrade.md), and v0.5.0 reads an ActorTemplate an older install stored with no
# sandbox class, so that template's actors cannot resume: the cluster is reinstalled, not reused. A
# cluster up.sh did not install is not checked.
refuse_stale_install() {
	local id stage ref
	if [[ -f "${SETUP}" ]] && read -r id stage ref <"${SETUP}" && [[ "${id}" == "$1" && "${stage}" == installed ]] &&
		[[ "${ref}" != "${SUBSTRATE_REF}" ]]; then
		echo "refusing to use kind cluster ${CLUSTER}: up.sh installed agent-substrate ${ref:-from a commit it did not record} in it, and this checkout pins ${SUBSTRATE_REF}." >&2
		echo "Substrate cannot be upgraded in place before 1.0. Run down.sh to delete the cluster and its sessions, then rerun up.sh (both in ${HERE})." >&2
		exit 1
	fi
}
# cluster_stage prints where the demo's cluster stands, as up.sh and ui.sh read it: absent; orphaned
# (kind does not list it, but its control-plane container exists); foreign (up.sh did not create the
# node that runs now); or, for the node up.sh created, the stage SETUP records for it (creating,
# created or installed), or unrecorded when SETUP records none for that node. It exits when the kind
# listing fails (cluster_listed) and refuses an install from another substrate commit
# (refuse_stale_install), so call it as stage="$(cluster_stage)" || exit. It reads only kind, Docker
# and the records, and changes nothing.
cluster_stage() {
	local id stage
	if ! cluster_listed; then
		if [[ -n "$(node_id)" ]]; then echo orphaned; else echo absent; fi
		return
	fi
	id="$(node_id)"
	if [[ -z "${id}" || "$(owned_id)" != "${id}" ]]; then
		echo foreign
		return
	fi
	stage="$(setup_stage "${id}")"
	if [[ "${stage}" == installed ]]; then
		refuse_stale_install "${id}"
	fi
	echo "${stage:-unrecorded}"
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
# The in-cluster fake model's base URL: what up.sh deploys agentsessionsd with when no real provider
# is set, and how demo.sh tells which one it was deployed with.
FAKE_MODEL_URL="http://fakemodel.${NS}.svc:8080/v1"
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
# Every cluster call names the demo's kind context explicitly; nothing here uses the caller's
# current context.
K=(kubectl --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
ATE=("${WORK}/bin/kubectl-ate" --context "kind-${CLUSTER}" --kubeconfig "${KUBECONFIG}")
# The substrate commit the adapter's go.mod pins; the cluster runs the same one.
SUBSTRATE_REF="$(sed -n 's/^  SUBSTRATE_REF: //p' "${ROOT}/.github/workflows/substrate-e2e.yml")"
SUBSTRATE="${SUBSTRATE_DIR:-${WORK}/substrate}"
mkdir -p "${WORK}/bin" "${STATE}"
