#!/usr/bin/env bash
# The coordinator suite (suites/coordinator-workers) on agent-substrate micro-VMs, with a real model.
#
#   coordinator.sh setup      kind + substrate (up.sh --substrate-only), one harness image and one
#                             ActorTemplate per agent (emit render), and one agentsessionsd serving all
#                             three; ends with SETUP OK
#   coordinator.sh probe      one coordinator request approved and one denied, through agentctl; ends
#                             with PROBE OK
#   coordinator.sh teardown   down.sh
#
# Runs on a Linux host with KVM and Docker (kind, the cluster and the port-forward all there), as
# driver.sh does. Settings: AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL, AGENTSUITE_MODEL_PATH,
# AGENTSUITE_MODEL_AUTH_HEADER (as for up.sh) and AZURE_MI_CLIENT_ID, the user-assigned managed
# identity whose token is the model API key; it is fetched from the instance metadata endpoint,
# stored only in the agentsessionsd-model Secret, and never printed. Logs go to COORD_LOGS
# (default ${WORK}/coordinator-logs), one file per step, through a filter that redacts tokens.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
export AGENTSUITE_DEMO_SUITE=suites/coordinator-workers AGENTSUITE_DEMO_AGENT=coordinator AGENTSUITE_DEMO_SANDBOX=microvm
source "${DEMO}/../common.sh"
source "${DEMO}/model-token.sh"

MODE="${1:-}"
case "${MODE}" in
setup | probe | teardown) ;;
*)
	echo "usage: $0 setup|probe|teardown" >&2
	exit 2
	;;
esac
AGENTS=(coordinator writer reviewer)
APPROVE=reviewer
SERVER=127.0.0.1:18090
LOGS="${COORD_LOGS:-${WORK}/coordinator-logs}"
mkdir -p "${LOGS}"
TOKEN_FILE="${WORK}/model-token"
JWT="ey""J"
FIELD="access""_token"
redact() {
	sed -u -E \
		-e "s/(Bearer )[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/g" \
		-e "s/${JWT}[A-Za-z0-9._-]+/[REDACTED-JWT]/g" \
		-e "s/(${FIELD}\"?[[:space:]]*[:=][[:space:]]*\"?)[^\",[:space:]]+/\1[REDACTED]/g"
}
exec > >(redact | tee -a "${LOGS}/${MODE}.log") 2>&1
PIDS=()
cleanup() {
	for p in "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done
	rm -f "${TOKEN_FILE}"
}
trap cleanup EXIT
fail() {
	echo "COORDINATOR FAILED: $*"
	exit 1
}
step() { printf '\n==> %s\n' "$*"; }
actors() { echo "agentsuite-$1"; }

# The placeholders of deploy/coordinator-agentsessionsd.yaml. render_manifest fills them with literal
# (not sed) substitution, the computed values first, so an operator value cannot be read as one.
COORDINATOR_PLACEHOLDERS=(DAEMON_IMAGE MODEL_SECRET_VERSION COORDINATOR_FLAG WRITER_FLAG REVIEWER_FLAG INVOKES_VALUE MODEL_BASE_URL MODEL_PATH MODEL_AUTH_HEADER MODEL_ID)
# unfilled prints the placeholders the text $1 still contains.
unfilled() {
	local p left=""
	for p in "${COORDINATOR_PLACEHOLDERS[@]}"; do
		if [[ "$1" == *"${p}"* ]]; then left="${left:+${left} }${p}"; fi
	done
	echo "${left}"
}
# render_manifest DAEMON_IMAGE SECRET_VERSION COORDINATOR_FLAG WRITER_FLAG REVIEWER_FLAG INVOKES
render_manifest() {
	local m
	m="$(<"${HERE}/deploy/coordinator-agentsessionsd.yaml")"
	m=${m//DAEMON_IMAGE/"$1"}
	m=${m//MODEL_SECRET_VERSION/"$2"}
	m=${m//COORDINATOR_FLAG/"$3"}
	m=${m//WRITER_FLAG/"$4"}
	m=${m//REVIEWER_FLAG/"$5"}
	m=${m//INVOKES_VALUE/"$6"}
	m=${m//MODEL_BASE_URL/"${AGENTSUITE_MODEL_BASE_URL}"}
	m=${m//MODEL_PATH/"${AGENTSUITE_MODEL_PATH}"}
	m=${m//MODEL_AUTH_HEADER/"${AGENTSUITE_MODEL_AUTH_HEADER:-Authorization}"}
	m=${m//MODEL_ID/"${AGENTSUITE_MODEL}"}
	printf '%s\n' "${m}"
}

setup() {
	local v
	for v in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AZURE_MI_CLIENT_ID; do
		[[ -n "${!v:-}" ]] || fail "${v} is not set"
	done
	for v in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AGENTSUITE_MODEL_AUTH_HEADER; do
		[[ -z "$(unfilled "${!v:-}")" ]] || fail "${v} contains $(unfilled "${!v:-}"), a placeholder of deploy/coordinator-agentsessionsd.yaml"
	done
	# Before the first cluster change: the manifest renders with no placeholder left.
	[[ -z "$(unfilled "$(render_manifest image 1 c w r i)")" ]] ||
		fail "deploy/coordinator-agentsessionsd.yaml holds placeholders this script does not fill: $(unfilled "$(render_manifest image 1 c w r i)")"
	[[ "$(go env GOARCH)" == amd64 ]] || fail "the suite declares linux/amd64 compositions only; run on an amd64 host"
	# up.sh validates the model settings before its first cluster change, then brings up kind,
	# ate-system and the micro-VM WorkerPool.
	step "up.sh --substrate-only"
	"${DEMO}/../up.sh" --substrate-only
	(cd "${HERE}" && go build -o "${WORK}/bin/emit" ./cmd/emit)
	(cd "${ROOT}" && go build -o "${WORK}/bin/agentctl" ./cmd/agentctl)

	step "the suite, validated by kaimahi's agentsuite package"
	"${WORK}/bin/emit" validate -suite "${HERE}/${SUITE_DIR}"
	INVOKES="$("${WORK}/bin/emit" invokes -suite "${HERE}/${SUITE_DIR}" -approve "${APPROVE}")"
	echo "AGENTSUITE_INVOKES=${INVOKES}"

	local ctx="${STATE}/coordinator-image" agent out binding flags=()
	rm -rf "${ctx}" && mkdir -p "${ctx}"
	(cd "${HERE}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "${ctx}/harness" ./cmd/harness)
	for agent in "${AGENTS[@]}"; do
		step "agent ${agent}: harness image, binding, ActorTemplate"
		rm -rf "${ctx}/agent"
		"${WORK}/bin/emit" bundle -suite "${HERE}/${SUITE_DIR}" -agent "${agent}" -platform linux/amd64 -out "${ctx}/agent"
		docker build -q --platform linux/amd64 -f "${HERE}/Dockerfile.harness" -t "localhost:5001/agentsuite-harness:${agent}" "${ctx}" >/dev/null
		docker push -q "localhost:5001/agentsuite-harness:${agent}" >/dev/null
		local image
		image="$(docker inspect --format '{{index .RepoDigests 0}}' "localhost:5001/agentsuite-harness:${agent}")"
		# Each agent's template and session actors live in its own atespace: actors are named by session
		# UID, so harnesses on one daemon need separate actor atespaces, and a template's golden tag is
		# scoped to its atespace.
		binding="${STATE}/binding-${agent}.json"
		python3 -c 'import json, sys; b = json.load(open(sys.argv[1])); b["atespace"] = sys.argv[2]; json.dump(b, open(sys.argv[3], "w"), indent=2)' \
			"${HERE}/${BINDING_FILE}" "$(actors "${agent}")" "${binding}"
		out="${STATE}/render-${agent}"
		local approve=()
		if [[ "${agent}" == coordinator ]]; then approve=(-approve "${APPROVE}"); fi
		"${WORK}/bin/emit" render -suite "${HERE}/${SUITE_DIR}" -agent "${agent}" -platform linux/amd64 -binding "${binding}" \
			-image "${image}" -model "${AGENTSUITE_MODEL}" -actors "$(actors "${agent}")" ${approve[@]+"${approve[@]}"} -out "${out}"
		"${ATE[@]}" get atespace "$(actors "${agent}")" >/dev/null 2>&1 || "${ATE[@]}" create atespace "$(actors "${agent}")"
		local template
		template="$(cat "${out}/template-name")"
		"${ATE[@]}" get actor-template "${template}" -a "$(actors "${agent}")" >/dev/null 2>&1 || "${ATE[@]}" create actor-template -f "${out}/actortemplate.json"
		wait_golden "$(actors "${agent}")" "${template}"
		flags+=("$(cat "${out}/agentsessionsd.args")")
	done

	step "a model token for the VM's managed identity (never printed)"
	model_secret "${NS}" "${AZURE_MI_CLIENT_ID}" "${TOKEN_FILE}" "${K[@]}"
	rm -f "${TOKEN_FILE}"
	local version
	version="$("${K[@]}" -n "${NS}" get secret agentsessionsd-model -o jsonpath='{.metadata.resourceVersion}')"

	step "agentsessionsd with the invocation executor (examples/agentsuite-substrate/cmd/agentsessionsd)"
	local daemon manifest
	daemon="$(cd "${HERE}" && KO_DOCKER_REPO=localhost:5001 ko build --base-import-paths --platform=linux/amd64 ./cmd/agentsessionsd)"
	manifest="$(render_manifest "${daemon}" "${version}" "${flags[0]}" "${flags[1]}" "${flags[2]}" "${INVOKES}")"
	[[ -z "$(unfilled "${manifest}")" ]] || fail "deploy/coordinator-agentsessionsd.yaml still holds the placeholders '$(unfilled "${manifest}")' after rendering"
	printf '%s\n' "${manifest}" >"${STATE}/coordinator-agentsessionsd.yaml"
	"${K[@]}" apply -f "${STATE}/coordinator-agentsessionsd.yaml"
	"${K[@]}" -n "${NS}" rollout status deploy/agentsessionsd-coordinator --timeout=180s
	"${K[@]}" -n "${NS}" logs deploy/agentsessionsd-coordinator | grep -E 'listening|substrate harness'
	echo "SETUP OK in ${SECONDS} s"
}

wait_golden() { # atespace template
	local deadline=$((SECONDS + 600)) golden=""
	while [[ -z "${golden}" ]]; do
		golden="$("${ATE[@]}" get actor-template "$2" -a "$1" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin).get("status", {}).get("goldenSnapshotStatus", {})
print("ERROR " + s["errorMessage"] if s.get("errorMessage") else s.get("goldenTag", {}).get("name", ""))')"
		if [[ -z "${golden}" ]]; then
			((SECONDS < deadline)) || fail "timed out waiting for the golden snapshot of $1/$2"
			sleep 5
		fi
	done
	[[ "${golden}" != ERROR* ]] || fail "golden snapshot of $1/$2: ${golden}"
	echo "golden snapshot of $1/$2: ${golden}"
}

ctl() { "${WORK}/bin/agentctl" "$1" --server "${SERVER}" "${@:2}"; }

# run_case DECISION: one coordinator request that parks at the reviewer call, then the decision.
run_case() {
	local decision="$1" log="${LOGS}/probe-$1"
	step "a coordinator request; the reviewer call needs approval (${decision})"
	ctl exec --harness coordinator --input "Write a two-line poem about tides, then have it reviewed for clarity. Reply with the final poem and one line on the review." | tee "${log}-exec.txt"
	local uid
	uid="$(sed -n 's/^session //p' "${log}-exec.txt" | head -1)"
	[[ -n "${uid}" ]] || fail "no session uid in the exec output"
	ctl get --session "${uid}" | tee "${log}-get.txt"
	# agentctl get prints no exec state; the turn parked when its exec stream committed the approval
	# request (and approve/deny below refuse a session with no pending approval).
	grep -q 'EVENT_APPROVAL_REQUEST' "${log}-exec.txt" || fail "session ${uid}: the turn did not park at an approval request"
	ctl "${decision}" --session "${uid}" --reason "probe ${decision}" --actor operator | tee "${log}-decision.txt"
	ctl replay --session "${uid}" -v | tee "${log}-journal.txt"
	ctl list | tee "${log}-list.txt"
	for kind in EVENT_TOOL_CALL EVENT_APPROVAL_REQUEST EVENT_APPROVAL_RESULT EVENT_TOOL_RESULT EVENT_END; do
		grep -q "${kind}" "${log}-journal.txt" || fail "session ${uid}: no ${kind} in its journal"
	done
	echo "${uid}" >"${log}-uid"
}

probe() {
	"${K[@]}" -n "${NS}" port-forward svc/agentsessionsd-coordinator "${SERVER##*:}:8080" >/dev/null 2>&1 &
	PIDS+=($!)
	for _ in $(seq 60); do
		if ctl list >/dev/null 2>&1; then break; fi
		sleep 1
	done
	run_case approve
	run_case deny
	step "actors per agent atespace"
	local agent
	for agent in "${AGENTS[@]}"; do
		"${ATE[@]}" get actors -a "$(actors "${agent}")" || true
	done
	"${K[@]}" -n "${NS}" logs deploy/agentsessionsd-coordinator >"${LOGS}/agentsessionsd.log" 2>&1 || true
	echo "PROBE OK"
}

case "${MODE}" in
setup) setup ;;
probe) probe ;;
teardown) "${DEMO}/../down.sh" ;;
esac
