#!/usr/bin/env bash
# snapshot-size.sh SESSION: the object store's listing of the newest completed snapshot of SESSION's
# actor, then one line: its fidelity, object count and size, and how much of it is the guest's
# memory (memory-ranges.zstd).
#
# The kind install's rustfs bucket is reached from the node's network namespace, as substrate's
# hack/microvm-assets/stage-to-rustfs.sh does; its credentials are the install's fixed defaults.
# Runs where the demo's kubeconfig is KUBECONFIG and the kubectl-ate plugin is on PATH.
set -euo pipefail
session="${1:?usage: snapshot-size.sh SESSION}"
cluster="${AGENTSUITE_DEMO_CLUSTER:-agentsuite-demo}"
# Keep in sync with the image substrate's kind install uses for its bucket (manifests/ate-install/kind/rustfs.yaml).
aws_cli="amazon/aws-cli:2.17.0@sha256:643507c10ada7964ca6157b3d799f030b90577643da9955d319a77399ed80d73"

read -r fidelity uri < <(kubectl ate get actor "${session}" -a agentsuite-demo -o json | jq -r '
	[.status.snapshots[]? | select(.durableSnapshot.status == "SNAPSHOT_STORAGE_STATUS_COMPLETED")]
	| max_by(.generation) // empty | "\(.fidelity) \(.durableSnapshot.object.snapshotUri)"')
[[ -n "${uri:-}" ]] || {
	echo "snapshot-size.sh: ${session} has no completed snapshot" >&2
	exit 1
}
path="${uri#*://}"
endpoint="http://$(kubectl -n ate-system get svc rustfs -o jsonpath='{.spec.clusterIP}'):9000"
listing="$(docker run --rm --network "container:${cluster}-control-plane" -e AWS_ACCESS_KEY_ID=rustfsadmin \
	-e AWS_SECRET_ACCESS_KEY=rustfsadmin -e AWS_REGION=us-east-1 -e "AWS_ENDPOINT_URL=${endpoint}" \
	"${aws_cli}" s3 ls --recursive --summarize "s3://${path%/}/")"
printf '%s\n' "${listing}"
awk -v fidelity="${fidelity#SNAPSHOT_FIDELITY_}" '
	$4 ~ /\/memory-ranges[^\/]*$/ { mem = $3 }
	/Total Objects:/ { objects = $3 }
	/Total Size:/ { size = $3 }
	END {
		if (size == 0 || mem == "") { print "snapshot-size.sh: no memory object in the listing" > "/dev/stderr"; exit 1 }
		share = mem / size
		what = share >= 0.99 ? "almost all of it" : sprintf("%.0f%% of it", 100 * share)
		printf "%s snapshot: %d objects, %.1f MiB; %s memory-ranges.zstd, the guest memory compressed\n", fidelity, objects, size / 1048576, what
	}' <<<"${listing}"
