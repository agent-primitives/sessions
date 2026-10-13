#!/usr/bin/env bash
# Deletes the demo's kind cluster (AGENTSUITE_DEMO_CLUSTER, default agentsuite-demo), and only if
# up.sh created it: a cluster up.sh merely reused is refused. The kind-registry container is shared
# with other substrate kind clusters, so it stays.
set -euo pipefail
source "$(dirname "$0")/common.sh"

# Only a successful listing that lacks the cluster shows it is gone; cluster_listed exits on a failed one.
if ! cluster_listed; then
	echo "no kind cluster ${CLUSTER}; nothing to delete"
	rm -f "${OWNED}" "${SETUP}"
	exit 0
fi
id="$(node_id)"
if [[ -z "${id}" || "$(owned_id)" != "${id}" ]]; then
	echo "refusing to delete kind cluster ${CLUSTER}: up.sh did not create it (no matching record in ${OWNED})." >&2
	echo "If it is yours to delete, run: kind delete cluster --name ${CLUSTER}" >&2
	exit 1
fi
echo "deleting kind cluster ${CLUSTER}, created by up.sh"
kind delete cluster --name "${CLUSTER}"
rm -f "${OWNED}" "${SETUP}"
