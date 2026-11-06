#!/usr/bin/env bash
# Step 0 of the recorded demo: one micro-VM actor boots, suspends to a memory snapshot and resumes,
# with an in-RAM counter carried across. It reached the actor through a port-forward of the router
# from this machine, which Substrate v1's envoy router does not admit, so it refuses before it
# deploys anything until router clients outside the cluster are supported.
set -euo pipefail
source "$(dirname "$0")/../common.sh"

fail() {
	echo "SMOKE FAILED: $*" >&2
	exit 1
}

[[ "${SANDBOX}" == microvm ]] || fail "AGENTSUITE_DEMO_SANDBOX=${SANDBOX}: the smoke test is for the micro-VM class"
# Substrate's envoy router admits only mTLS clients with an allowlisted SPIFFE ID, and this script
# has no certificate.
fail "the smoke test reaches the actor from outside the cluster, which substrate's envoy router does not admit yet; see docs/substrate-conformance.md#router-clients-outside-the-cluster"
