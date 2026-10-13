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
# up.sh records the control-plane container ID of a cluster it creates; down.sh deletes a cluster only
# when its container still has that ID, so it never deletes a cluster up.sh only reused.
OWNED="${WORK}/created-cluster-${CLUSTER}"
node_id() { docker inspect --format '{{.Id}}' "${CLUSTER}-control-plane" 2>/dev/null || true; }
owned_id() { if [[ -f "${OWNED}" ]]; then cat "${OWNED}"; fi; }
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
