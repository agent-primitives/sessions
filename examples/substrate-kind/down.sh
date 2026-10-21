#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/common.sh"
need docker

# Share setup's lock and require the recorded Docker identity before deleting a cluster.
progress "Checking ownership before tearing down cluster ${CLUSTER}"
lock_cluster
stage="$(cluster_state)"
if [[ "${stage}" != absent ]]; then
	progress "Deleting kind cluster ${CLUSTER}, including its workloads and stored session data"
	kind_command delete cluster --name "${CLUSTER}"
	stage="$(cluster_state)"
	[[ "${stage}" == absent ]] || die "Deletion incomplete; preserving ownership record."
else
	progress "Cluster ${CLUSTER} is already absent"
fi

# Forget ownership only after confirming absence. Leave the shared registry and local tools intact.
progress "Clearing the cluster ownership record"
rm -f "${OWNER}"
progress "Teardown complete for ${CLUSTER}; shared kind-registry and local tools preserved."
