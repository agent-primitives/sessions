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
setup | probe | show | reset | teardown) ;;
*)
	echo "usage: $0 setup|probe|show|reset|teardown" >&2
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
# show writes its screens to stdout (through present.awk on a terminal) and each chapter's full
# output to its own log; the other modes tee everything to <mode>.log.
STEP_LOG="${LOGS}/${MODE}.log"
if [[ "${MODE}" == show ]]; then
	: >"${STEP_LOG}"
	if [[ -t 1 ]]; then exec > >(redact | awk -f "${DEMO}/present.awk") 2>&1; else exec > >(redact) 2>&1; fi
else
	exec > >(redact | tee -a "${STEP_LOG}") 2>&1
fi
REDACT_PID=$!
log() { printf '%s\n' "$@" | redact >>"${STEP_LOG}"; }
PIDS=()
cleanup() {
	local rc=$?
	if [[ "${MODE}" == show ]] && ((rc != 0)) && [[ -z "${FAILED:-}" ]]; then
		printf 'DEMO FAILED: exit %d, see %s\n' "${rc}" "${STEP_LOG##*/}"
	fi
	for p in "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done
	rm -f "${TOKEN_FILE}"
	# Let the redacting filter flush the last lines, for at most 10 seconds.
	exec 1>&- 2>&-
	for _ in $(seq 100); do
		kill -0 "${REDACT_PID}" 2>/dev/null || break
		sleep 0.1
	done
}
trap cleanup EXIT
fail() {
	FAILED=1
	if [[ "${MODE}" == show ]]; then
		printf 'DEMO FAILED: %s\n' "$*"
		log "DEMO FAILED: $*"
	else
		echo "COORDINATOR FAILED: $*"
	fi
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

# =================================================================================================
# reset: off camera, between two runs of show: deletes the session actors in the three agents'
# atespaces and restarts agentsessionsd, whose journal is an emptyDir, so show starts from an empty
# journal and no session actors.
# =================================================================================================
reset() {
	local agent name
	for agent in "${AGENTS[@]}"; do
		for name in $("${ATE[@]}" get actors -a "$(actors "${agent}")" -o name 2>/dev/null | sed -n 's|.*/||; /^sess-/p'); do
			"${ATE[@]}" delete actor "${name}" -a "$(actors "${agent}")" --any-state
		done
	done
	"${K[@]}" -n "${NS}" rollout restart deploy/agentsessionsd-coordinator
	"${K[@]}" -n "${NS}" rollout status deploy/agentsessionsd-coordinator --timeout=180s
	echo "RESET OK"
}

# =================================================================================================
# show: the recording (demo/coordinator.tape). Eight chapters of one-line captions and commands, as
# driver.sh show draws them (demo/screen.sh): every command on screen is the command line that runs,
# its full output goes to the chapter's log, and show stops on the first claim that does not hold.
# $API, $IMAGE and $BINDING are set from setup's state before the title card (show.log has their
# values); $S and $DENIED are set by commands on screen, and $CHILD is read from the writer result
# chapter 4 shows (the chapter's log records it).
# =================================================================================================
CHAPTERS=8
READ="${DEMO_READ:-4}" COLS="${DEMO_COLS:-129}" ROWS="${DEMO_ROWS:-39}"
# shellcheck source=screen.sh
source "${DEMO}/screen.sh"

# nth KIND N: the seq of the Nth record of KIND in RAW's journal (agentctl replay output).
nth() { python3 -c 'import re, sys; s = [m.group(1) for m in re.finditer(r"^seq=(\d+) +" + sys.argv[1] + r"\b", open(sys.argv[3]).read(), re.M)]; n = int(sys.argv[2]); print(s[n - 1] if len(s) >= n else "")' "$1" "$2" "${RAW}"; }
# line_of SEQ: RAW's journal line of that record.
line_of() { python3 -c 'import re, sys; m = re.search(r"^seq=" + sys.argv[1] + r" .*$", open(sys.argv[2]).read(), re.M); print(m.group(0) if m else "")' "$1" "${RAW}"; }
# sessions HARNESS: how many sessions agentctl list shows on HARNESS.
sessions() { "${WORK}/bin/agentctl" list --server "${SERVER}" | grep -c "harness=$1	" || true; }

show() {
	[[ -x "${WORK}/bin/agentctl" && -x "${WORK}/bin/emit" ]] || fail "run coordinator.sh setup first"
	rm -f "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv"
	rm -rf /tmp/render-coordinator /tmp/approve.txt /tmp/deny.txt
	"${K[@]}" -n "${NS}" port-forward svc/agentsessionsd-coordinator "${SERVER##*:}:8080" >/dev/null 2>&1 &
	PIDS+=($!)
	local i
	for i in $(seq 60); do
		if "${WORK}/bin/agentctl" list --server "${SERVER}" >/dev/null 2>&1; then break; fi
		((i < 60)) || fail "the Sessions API did not answer"
		sleep 1
	done
	[[ -z "$("${WORK}/bin/agentctl" list --server "${SERVER}")" ]] || fail "the journal holds sessions already; run coordinator.sh reset"
	export PATH="${WORK}/bin:${PATH}" BAT_CONFIG_PATH="${DEMO}/bat.conf"
	[[ "$(kubectl config current-context)" == "kind-${CLUSTER}" ]] || fail "the kubeconfig's current context is not kind-${CLUSTER}"
	cd "${HERE}"
	export API="${SERVER}" BINDING="${STATE}/binding-coordinator.json"
	IMAGE="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["containers"][0]["image"])' "${STATE}/render-coordinator/actortemplate.json")"
	[[ "${IMAGE}" =~ @sha256:[0-9a-f]{64}$ ]] || fail "the coordinator's harness image is not digest-pinned: ${IMAGE}"
	local template wtemplate rtemplate model="${AGENTSUITE_MODEL}" seq call res req out
	template="$(cat "${STATE}/render-coordinator/template-name")"
	wtemplate="$(cat "${STATE}/render-writer/template-name")"
	rtemplate="$(cat "${STATE}/render-reviewer/template-name")"
	log "API=${API} IMAGE=${IMAGE} BINDING=${BINDING} templates: ${template} ${wtemplate} ${rtemplate}"
	local input="Write a two-line poem about tides, then have it reviewed for clarity. Reply with the final poem and one line on the review."

	printf '\e[?25l\e[2J\e[3J\e[H\n'
	printf '%s  An AgentSuite coordinator on AgentSessions and micro-VMs%s\n\n' "${C}" "${N}"
	wrap "kaimahi's coordinator-workers suite: the coordinator delegates to writer and reviewer, each call a child session in its own micro-VM, with ${model} on Azure OpenAI. The call to reviewer waits for approval." "  " | sed "s/^/${W}/; s/\$/${N}/"
	sleep "${TITLE_HOLD}"

	# ---------------------------------------------------------------------------------------------
	chapter 1 "The suite" "kaimahi's coordinator-workers test suite (agentsuite/testdata at 75f27d6): three agents"
	run <<'EOF'
ls suites/coordinator-workers/agents suites/coordinator-workers/instructions
EOF
	page "The coordinator may invoke writer and reviewer: one call at a time each, and no deeper chain"
	ends_at suites/coordinator-workers/agents/coordinator.json 28
	run -f <<'EOF'
bat suites/coordinator-workers/agents/coordinator.json
EOF
	lines_hold suites/coordinator-workers/agents/coordinator.json 15:'"invokes"' 17:'"agent": "writer"' 18:'"maxConcurrent": 1' 19:'"maxDepth": 1' 22:'"agent": "reviewer"'
	page "emit is the PoC's translator, built on kaimahi's agentsuite package; first, kaimahi's validator"
	run <<'EOF'
emit validate -suite suites/coordinator-workers
EOF
	[[ "${SHOWN}" == "suite coordinator-workers valid: 3 agents"*"kaimahi/agentsuite v"*"-75f27d67f40b"* ]] || fail "emit validate did not report kaimahi 75f27d6's validator"

	# ---------------------------------------------------------------------------------------------
	chapter 2 "The translation" "emit invokes: the suite's edges as the daemon's policy; which call needs approval is this deployment's choice"
	run <<'EOF'
emit invokes -suite suites/coordinator-workers -approve reviewer
EOF
	[[ "${SHOWN}" == "coordinator=reviewer:1:1:approval,coordinator=writer:1:1" ]] || fail "emit invokes printed another policy"
	page "emit render: one ActorTemplate per agent; the coordinator's carries the approval choice"
	run <<EOF
emit render -suite suites/coordinator-workers -agent coordinator -platform linux/amd64 -binding \$BINDING -image \$IMAGE -model ${model} -actors agentsuite-coordinator -approve reviewer -out /tmp/render-coordinator
EOF
	offscreen "this render against the one setup created" diff -r /tmp/render-coordinator "${STATE}/render-coordinator" || fail "the render differs from setup's"
	page "In the template: the harness reads the delegation targets that need approval from its env"
	seq="$(python3 -c 'import sys; print(next(i for i, l in enumerate(open(sys.argv[1]), 1) if "AGENTSUITE_APPROVAL" in l))' /tmp/render-coordinator/actortemplate.json)"
	run -f <<EOF
bat -r $((seq - 4)):$((seq + 1)) /tmp/render-coordinator/actortemplate.json
EOF
	lines_hold /tmp/render-coordinator/actortemplate.json "${seq}:AGENTSUITE_APPROVAL" "$((seq + 1)):\"value\": \"reviewer\""
	page "And the flag that serves it from agentsessionsd, with its actors in the coordinator's own atespace"
	run <<'EOF'
cat /tmp/render-coordinator/agentsessionsd.args
EOF
	[[ "${SHOWN}" == "coordinator=agentsuite-coordinator/${template},actors=agentsuite-coordinator" ]] || fail "the coordinator's flag is not ${template}"

	# ---------------------------------------------------------------------------------------------
	chapter 3 "In the cluster" "Created by hand before recording, from the three renders: one ActorTemplate per agent, in its own atespace"
	run -n 12 <<'EOF'
kubectl ate get actor-templates -a agentsuite-coordinator && kubectl ate get actor-templates -a agentsuite-writer && kubectl ate get actor-templates -a agentsuite-reviewer
EOF
	[[ "${SHOWN}" == *"${template}"* && "${SHOWN}" == *"${wtemplate}"* && "${SHOWN}" == *"${rtemplate}"* ]] || fail "the three templates are not in the cluster"
	page "Manual step: three -substrate-harness flags register them; the HarnessRegistry does not take substrate placements"
	run <<'EOF'
kubectl get deploy agentsessionsd-coordinator -n agentsuite-demo -o jsonpath='{range .spec.template.spec.containers[0].args[6:9]}{@}{"\n"}{end}'
EOF
	[[ "$(printf '%s\n' "${SHOWN}" | grep -c '^-substrate-harness=')" == 3 ]] || fail "the Deployment does not carry three -substrate-harness flags at args 6-8"
	page "The suite's invokes edges reach agentsessionsd's invocation executor as one env value"
	run <<'EOF'
kubectl get deploy agentsessionsd-coordinator -n agentsuite-demo -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="AGENTSUITE_INVOKES")].value}'
EOF
	[[ "${SHOWN}" == "coordinator=reviewer:1:1:approval,coordinator=writer:1:1" ]] || fail "the Deployment's AGENTSUITE_INVOKES is not the suite's policy"

	# ---------------------------------------------------------------------------------------------
	chapter 4 "Delegation, approved" "How a delegation runs: a host-executed tool call that creates a child session" 3
	diagram green invoke
	page "A request to the coordinator, in a new session on its harness"
	run <<'EOF'
S=$(agentctl create --server $API --harness coordinator --name tides)
EOF
	run -w "the coordinator's turn: the writer call, then the reviewer call parks" "the turn parked" <<EOF
agentctl exec --server \$API --session \$S --input "${input}" >/tmp/approve.txt
EOF
	journal "${SERVER}" "${S}"
	call="$(nth EVENT_TOOL_CALL 1)" res="$(nth EVENT_TOOL_RESULT 1)"
	[[ -n "${call}" && -n "${res}" && "$(line_of "${call}")" == *'"agent":"writer"'* && "$(line_of "${call}")" == *"mediation=CONTROLLER_MEDIATED"* ]] ||
		fail "the coordinator's first delegation is not to writer"
	page "The delegation to writer: a tool call the host ran as a child session, and its reply"
	run -n 8 <<EOF
agentctl replay --server \$API --session \$S --from ${call} --to ${res}
EOF
	[[ "${SHOWN}" == *'"session":"sess-'* ]] || fail "the writer result names no child session"
	call="$(nth EVENT_TOOL_CALL 2)" req="$(last_seq EVENT_APPROVAL_REQUEST)"
	[[ -n "${call}" && -n "${req}" && "$(line_of "${call}")" == *'"agent":"reviewer"'* && "$(line_of "${call}")" == *"mediation=REQUIRES_APPROVAL"* ]] ||
		fail "the coordinator's second delegation is not a gated call to reviewer"
	[[ "$(sessions reviewer)" == 0 ]] || fail "a reviewer session exists before the approval"
	page "The call to reviewer needs approval: the host records the request and parks the turn"
	run -n 8 <<EOF
agentctl replay --server \$API --session \$S --from ${call}
EOF
	run <<'EOF'
agentctl get --server $API --session $S
EOF
	[[ "${SHOWN}" == *"compute=COLD"* ]] || fail "the parked session still holds compute"
	page "Manual step: an operator approves with agentctl, which commits the decision and resumes the turn"
	run -w "the decision, then the reviewer call and the rest of the turn" "approved and resumed" <<'EOF'
agentctl approve --server $API --session $S --reason "review is in scope" --actor operator
EOF
	[[ "${SHOWN}" == *"approved=true"* ]] || fail "the decision is not an approval"
	journal "${SERVER}" "${S}"
	seq="$(last_seq EVENT_APPROVAL_RESULT)" res="$(nth EVENT_TOOL_RESULT 2)" out="$(last_seq EVENT_OUTPUT)"
	[[ -n "${seq}" && -n "${res}" && -n "${out}" && -n "$(last_seq EVENT_END)" ]] || fail "the resumed turn did not finish"
	[[ "$(line_of "${seq}")" == *"approved=true"* && "$(line_of "${res}")" == *'"agent":"reviewer"'* ]] || fail "the journal does not hold the approval and the reviewer's reply"
	page "The decision and the reviewer's reply, appended to the same journal"
	run -n 8 <<EOF
agentctl replay --server \$API --session \$S --from ${seq} --to ${res}
EOF
	page "The coordinator's final answer"
	run -n 6 <<EOF
agentctl replay --server \$API --session \$S --from ${out} --to ${out}
EOF

	# ---------------------------------------------------------------------------------------------
	chapter 5 "Parent and child sessions" "Each delegation ran as a session of its own, on the target agent's harness"
	run <<'EOF'
agentctl list --server $API
EOF
	[[ "$(sessions writer)" == 1 && "$(sessions reviewer)" == 1 && "$(sessions coordinator)" == 1 ]] || fail "want one coordinator, one writer and one reviewer session"
	journal "${SERVER}" "${S}"
	CHILD="$(line_of "$(nth EVENT_TOOL_RESULT 1)" | python3 -c 'import re, sys; print(re.search(r"\"session\":\"(sess-[0-9a-f]+)\"", sys.stdin.read()).group(1))')"
	log "(CHILD=${CHILD}, read from the writer result on chapter 4's screen)"
	page "The writer session's first record names its parent session, the tool call and its place in the chain"
	run -n 6 <<'EOF'
agentctl replay --server $API --session $CHILD --to 1
EOF
	[[ "${SHOWN}" == *"\"parentSession\":\"${S}\""* && "${SHOWN}" == *'"caller":"coordinator"'* && "${SHOWN}" == *'"depth":1'* ]] || fail "the writer session does not record its parent"
	page "Each child session ran as an actor from its agent's template on the micro-VM worker pool"
	run -n 12 <<'EOF'
kubectl ate get actors -a agentsuite-writer && kubectl ate get actors -a agentsuite-reviewer
EOF
	[[ "${SHOWN}" == *"${CHILD}"*"${wtemplate}"* && "${SHOWN}" == *"${rtemplate}"* ]] || fail "the child sessions' actors are not from the workers' templates"

	# ---------------------------------------------------------------------------------------------
	chapter 6 "Delegation, denied" "The same request in a new session; this time the operator denies the reviewer call"
	run <<'EOF'
DENIED=$(agentctl create --server $API --harness coordinator --name tides-denied)
EOF
	run -w "the coordinator's turn: the writer call, then the reviewer call parks" "the turn parked" <<EOF
agentctl exec --server \$API --session \$DENIED --input "${input}" >/tmp/deny.txt
EOF
	journal "${SERVER}" "${DENIED}"
	[[ -n "$(last_seq EVENT_APPROVAL_REQUEST)" ]] || fail "the second turn did not park at an approval request"
	page "Manual step: the operator denies it with agentctl"
	run -w "the decision, then the rest of the turn without the reviewer" "denied and resumed" <<'EOF'
agentctl deny --server $API --session $DENIED --reason "no review this time" --actor operator
EOF
	[[ "${SHOWN}" == *"approved=false"* ]] || fail "the decision is not a denial"
	journal "${SERVER}" "${DENIED}"
	seq="$(last_seq EVENT_APPROVAL_RESULT)" res="$(nth EVENT_TOOL_RESULT 2)" out="$(last_seq EVENT_OUTPUT)"
	[[ -n "${seq}" && -n "${res}" && -n "${out}" && -n "$(last_seq EVENT_END)" ]] || fail "the denied turn did not finish"
	[[ "$(line_of "${res}")" == *"code=APPROVAL_DENIED"* ]] || fail "the reviewer result is not APPROVAL_DENIED"
	page "The denial is journaled, and the coordinator gets it as the reviewer call's result"
	run -n 8 <<EOF
agentctl replay --server \$API --session \$DENIED --from ${seq} --to ${res}
EOF
	page "The coordinator continues without the review"
	run -n 6 <<EOF
agentctl replay --server \$API --session \$DENIED --from ${out} --to ${out}
EOF
	page "A second writer session, and no second reviewer session"
	run <<'EOF'
agentctl list --server $API
EOF
	[[ "$(sessions writer)" == 2 && "$(sessions reviewer)" == 1 && "$(sessions coordinator)" == 2 ]] || fail "want two coordinator, two writer and one reviewer session"

	# ---------------------------------------------------------------------------------------------
	chapter 7 "The suite's bounds" "maxConcurrent and maxDepth are checked before a child session exists; the PoC's tests show both refusals"
	run -w "go test of the invocation executor" "tests done" -n 8 <<'EOF'
go test -count=1 -run 'TestInvokeEnforcesMax' -v ./invoke/
EOF
	[[ "${SHOWN}" == *"--- PASS: TestInvokeEnforcesMaxConcurrent"* && "${SHOWN}" == *"--- PASS: TestInvokeEnforcesMaxDepth"* ]] || fail "the bounds tests did not pass"

	# ---------------------------------------------------------------------------------------------
	chapter 8 "Open questions" "Open questions, each tagged with whose it is"
	show_lines "AgentSuite: an invokes edge has no approval field (Tools have one); here the deployment chooses, through emit -approve.
AgentSuite: maxConcurrent bounds the declaring agent's calls; across runtime replicas too? This PoC counts per daemon.
AgentSessions: agent-to-agent calls are a host tool call that creates a child session; should there be a first-class primitive?
AgentSessions: CreateSession takes no idempotency key, so recovery after a crash mid-call could create a second child.
AgentSessions: the HarnessRegistry does not take substrate placements yet, so the three harnesses are daemon flags.
AgentSessions: a child session keeps its micro-VM actor running after its one turn; who releases it?
substrate: is a template's golden snapshot usable from another atespace? Here each agent's template has its own." 16
	log "DEMO COMPLETE in $((SECONDS - SHOW_START)) s"
	printf '\n%s\n' "agentsuite-substrate coordinator demo."
}

case "${MODE}" in
setup) setup ;;
probe) probe ;;
show)
	SHOW_START=${SECONDS}
	show
	;;
reset) reset ;;
teardown) "${DEMO}/../down.sh" ;;
esac
