#!/usr/bin/env bash
# The session lifecycle page, against what examples/agentsuite-substrate/up.sh brought up.
#
#   ui.sh          derive and deploy the VOLUMES-template daemon (once), forward the daemons, the
#                  router and the fake model to this machine's loopback, and serve the page on
#                  http://127.0.0.1:${LIFECYCLE_UI_PORT:-8090} until interrupted
#   ui.sh check    drive the running page through every lifecycle call and check each result
#
# The cluster settings (AGENTSUITE_DEMO_CLUSTER, AGENTSUITE_DEMO_WORK, AGENTSUITE_DEMO_SANDBOX) are
# the example's and must match the up.sh run. See README.md.
set -euo pipefail
EXAMPLE="$(cd "$(dirname "$0")/../agentsuite-substrate" && pwd)"
# shellcheck source=../agentsuite-substrate/common.sh
source "${EXAMPLE}/common.sh"

PORT="${LIFECYCLE_UI_PORT:-8090}"
if [[ ! "${PORT}" =~ ^[1-9][0-9]{0,4}$ ]] || ((PORT > 65535)); then
	echo "LIFECYCLE_UI_PORT=${PORT}: want a port number; nothing was changed" >&2
	exit 1
fi
URL="http://127.0.0.1:${PORT}"
BIN="${WORK}/bin/lifecycle-ui"
step() { printf '\n==> %s\n' "$*"; }
fail() {
	echo "ui.sh: $*" >&2
	exit 1
}
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# The forwards' fixed ports and the page's. A port someone else holds (another ui.sh, against
# another cluster) would answer for this run, so refuse before anything changes.
if [[ -z "${1:-}" ]]; then
	for p in 18180 18181 18182 18183 18184 "${PORT}"; do
		if listening "${p}"; then
			fail "127.0.0.1:${p} is already in use (another ui.sh or port-forward?); stop it first; nothing was changed"
		fi
	done
fi

# A cluster up.sh installed from another substrate commit cannot serve this checkout's daemons and
# templates; up.sh refuses it too.
refuse_stale_install "$(node_id)"

(cd "${EXAMPLE}" && go build -o "${BIN}" ./cmd/lifecycle-ui)

# The daemons call the fake model unless up.sh was given a real provider; read which from what is
# deployed, as demo.sh does, never from this shell's environment.
DEPLOYED_URL="$("${K[@]}" -n "${NS}" get deploy agentsessionsd \
	-o jsonpath='{range .spec.template.spec.containers[?(@.name=="agentsessionsd")].args[*]}{@}{"\n"}{end}' |
	sed -n 's/^-model-base-url=//p')" || fail "cannot read deployment ${NS}/agentsessionsd; run up.sh first"
[[ -n "${DEPLOYED_URL}" ]] || fail "deployment ${NS}/agentsessionsd has no -model-base-url; run up.sh first"
FAKE=false
if [[ "${DEPLOYED_URL}" == "${FAKE_MODEL_URL}" ]]; then FAKE=true; fi

if [[ "${1:-}" == check ]]; then
	exec "${BIN}" check -url "${URL}" -fake-model="${FAKE}"
fi
[[ -z "${1:-}" ]] || fail "usage: ui.sh [check]"

# The volumes lane: the memory-tier harness on a template that only gives VOLUMES fidelity, served
# by a third daemon, agentsessionsd-volumes. It is derived from up.sh's rendered memory tier, which
# lifecycle-ui inject checks before anything is applied. Micro-VM only: the derivation mounts a
# durable-dir volume that ateom-microvm needs for a VOLUMES checkpoint.
VOLUMES=""
if [[ "${SANDBOX}" == microvm ]]; then
	OUT="${STATE}/render-volumes"
	"${BIN}" inject -state "${STATE}" -out "${OUT}"
	TEMPLATE="$(cat "${OUT}/template-name")"
	# Only NotFound means the template is absent; any other failure stops here.
	if ! err="$("${ATE[@]}" get actor-template "${TEMPLATE}" -a "${ATESPACE}" -o json 2>&1 >/dev/null)"; then
		[[ "${err}" == *"code = NotFound"* ]] || fail "reading ActorTemplate ${ATESPACE}/${TEMPLATE}: ${err}"
		step "ActorTemplate ${ATESPACE}/${TEMPLATE} (VOLUMES); wait for its golden snapshot"
		"${ATE[@]}" create actor-template -f "${OUT}/actortemplate.json"
	fi
	golden=""
	deadline=$((SECONDS + 600))
	while [[ -z "${golden}" ]] && ((SECONDS < deadline)); do
		golden="$("${ATE[@]}" get actor-template "${TEMPLATE}" -a "${ATESPACE}" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin).get("status", {}).get("goldenSnapshotStatus", {})
print("ERROR " + s["errorMessage"] if s.get("errorMessage") else s.get("goldenTag", {}).get("name", ""))')" ||
			fail "reading golden snapshot status of ${TEMPLATE}"
		[[ -n "${golden}" ]] || sleep 5
	done
	[[ -n "${golden}" ]] || fail "timed out waiting for the golden snapshot of ${TEMPLATE}"
	[[ "${golden}" != ERROR* ]] || fail "golden snapshot of ${TEMPLATE} failed: ${golden}"
	step "agentsessionsd-volumes serves the memory-tier harness on ${TEMPLATE}"
	"${K[@]}" apply -f "${OUT}/agentsessionsd-volumes.yaml"
	"${K[@]}" -n "${NS}" rollout status deploy/agentsessionsd-volumes --timeout=180s
	VOLUMES=127.0.0.1:18182
else
	echo "note: AGENTSUITE_DEMO_SANDBOX=${SANDBOX}: the VOLUMES-template lane is micro-VM only, so the fidelity refusal is not shown"
fi

PIDS=()
FWD_PORTS=()
FWD_DIR="$(mktemp -d)"
cleanup() {
	for p in ${PIDS[@]+"${PIDS[@]}"}; do kill "${p}" 2>/dev/null || true; done
	rm -rf "${FWD_DIR}"
}
trap cleanup EXIT
forward() { # namespace service local:remote
	"${K[@]}" -n "$1" port-forward --address 127.0.0.1 "svc/$2" "$3" >"${FWD_DIR}/${3%%:*}.log" 2>&1 &
	PIDS+=($!)
	FWD_PORTS+=("${3%%:*}")
}
step "port-forwards to this machine's loopback"
forward "${NS}" agentsessionsd 18180:8080
forward "${NS}" agentsessionsd-memory 18181:8080
if [[ -n "${VOLUMES}" ]]; then forward "${NS}" agentsessionsd-volumes 18182:8080; fi
forward ate-system atenet-router 18183:80
MODEL_ARGS=()
if ${FAKE}; then
	forward "${NS}" fakemodel 18184:8080
	MODEL_ARGS=(-model-base-url http://127.0.0.1:18184/v1 -model-stats http://127.0.0.1:18184/stats)
fi
# A forward is up when its own kubectl says it bound the port; a listener alone could be another
# process's. A kubectl that exits (the port was taken after the check above) stops the run.
forwarding() { grep -q "^Forwarding from 127.0.0.1:$1 " "${FWD_DIR}/$1.log"; }
for i in "${!FWD_PORTS[@]}"; do
	p="${FWD_PORTS[i]}"
	for _ in $(seq 30); do
		if forwarding "${p}"; then break; fi
		kill -0 "${PIDS[i]}" 2>/dev/null || break
		sleep 1
	done
	kill -0 "${PIDS[i]}" 2>/dev/null ||
		fail "port-forward to 127.0.0.1:${p} exited: $(tail -n 1 "${FWD_DIR}/${p}.log")"
	forwarding "${p}" || fail "port-forward to 127.0.0.1:${p} did not come up"
done

step "the page: ${URL}"
args=(-listen "127.0.0.1:${PORT}" -stateless 127.0.0.1:18180 -memory 127.0.0.1:18181 -router 127.0.0.1:18183
	-harness "${AGENT}" -atespace "${ATESPACE}" -namespace "${NS}"
	-kubectl kubectl -kubectl-ate "${WORK}/bin/kubectl-ate" -kubeconfig "${KUBECONFIG}" -context "kind-${CLUSTER}")
if [[ -n "${VOLUMES}" ]]; then args+=(-volumes "${VOLUMES}"); fi
"${BIN}" "${args[@]}" ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"}
