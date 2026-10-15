#!/usr/bin/env bash
# wait-golden.sh TEMPLATE [ATESPACE]: waits, for at most 10 minutes, until substrate has built the
# ActorTemplate's golden snapshot (the snapshot new actors of the template start from), then prints
# its tag. Fails with the template's own error message if the build failed.
#
# Runs where the kubectl-ate plugin is on PATH and the demo's kubeconfig is KUBECONFIG.
set -euo pipefail
template="${1:?usage: wait-golden.sh TEMPLATE [ATESPACE]}"
atespace="${2:-agentsuite-demo}"
deadline=$((SECONDS + 600))
while :; do
	status="$(kubectl ate get actor-template "${template}" -a "${atespace}" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin).get("status", {}).get("goldenSnapshotStatus", {})
print("ERROR " + s["errorMessage"] if s.get("errorMessage") else s.get("goldenTag", {}).get("name", ""))')"
	case "${status}" in
	ERROR*)
		echo "golden snapshot of ${template} failed: ${status#ERROR }" >&2
		exit 1
		;;
	"") ;;
	*)
		echo "golden snapshot of ${atespace}/${template}: ${status}"
		exit 0
		;;
	esac
	if ((SECONDS >= deadline)); then
		echo "no golden snapshot of ${template} after 10 minutes" >&2
		exit 1
	fi
	sleep 3
done
