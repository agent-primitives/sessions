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
# shellcheck source=hack/substrate-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/substrate-lib.sh"

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
	e2e-egress
	agentsessions-templates # ACTORTEMPLATE_ATESPACE
)
ATE=("${KUBECTL_ATE:-kubectl-ate}" --context "kind-${KIND_CLUSTER_NAME:-kind}")

die() {
	echo "provision-e2e-access: $*" >&2
	exit 1
}
progress() { echo "$*"; }

for atespace in "${ATESPACES[@]}"; do
	ensure_atespace_editor "${atespace}" "${MEMBER}"
	echo "atespace ${atespace}: ${MEMBER} is an editor"
done
