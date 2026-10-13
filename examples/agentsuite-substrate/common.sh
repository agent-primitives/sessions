# Shared settings for up.sh, demo.sh and down.sh. Sourced, not run.
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
TMP="${TMPDIR:-/tmp}"
WORK="${AGENTSUITE_DEMO_WORK:-${TMP%/}/agentsuite-demo}"
export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-agentsuite-demo}"
# A dedicated kubeconfig: kind would otherwise switch the caller's current context.
export KUBECONFIG="${WORK}/kubeconfig"
NS=agentsuite-demo
ATESPACE=agentsuite-demo
AGENT=triage
# Every cluster call names the demo's kind context explicitly; nothing here uses the caller's
# current context.
K=(kubectl --context "kind-${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}")
ATE=("${WORK}/bin/kubectl-ate" --context "kind-${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}")
# The substrate commit the adapter's go.mod pins; the cluster runs the same one.
SUBSTRATE_REF="$(sed -n 's/^  SUBSTRATE_REF: //p' "${ROOT}/.github/workflows/substrate-e2e.yml")"
SUBSTRATE="${SUBSTRATE_DIR:-${WORK}/substrate}"
mkdir -p "${WORK}/bin"
