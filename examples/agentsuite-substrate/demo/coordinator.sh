#!/usr/bin/env bash
# The coordinator suite on agent-substrate micro-VMs, with a real model: the version of kaimahi's
# coordinator-workers that pins gpt-5.4-mini (suites/coordinator-workers-v2) and this environment's
# AgentSessionsBinding, which says where that model is served and which delegations need approval.
#
#   coordinator.sh setup      kind + substrate (up.sh --substrate-only), one harness image and one
#                             ActorTemplate per agent (emit render), and one agentsessionsd serving all
#                             three; ends with SETUP OK
#   coordinator.sh probe      one coordinator request approved and one denied, through agentctl; ends
#                             with PROBE OK
#   coordinator.sh teardown   down.sh
#
# Runs on a Linux host with KVM and Docker (kind, the cluster and the port-forward all there), as
# driver.sh does. Settings: AGENTSUITE_MODEL_BASE_URL (Azure OpenAI's v1 endpoint,
# https://<resource>.openai.azure.com/openai/v1) and AGENTSUITE_MODEL (the deployment name), the
# served model, written into this environment's binding: binding-coordinator.json with them, see
# common.sh write_binding; and AZURE_MI_CLIENT_ID, the user-assigned managed identity whose token is
# the model API key; it is fetched from the instance metadata endpoint, piped into the
# agentsessionsd-model Secret with no file on the host, and never printed; the daemon sends it in
# Authorization, so AGENTSUITE_MODEL_AUTH_HEADER (which up.sh honors) must be unset or Authorization
# (in any case).
# Logs go to COORD_LOGS
# (default ${WORK}/coordinator-logs), one file per step, through a filter that redacts tokens.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
export AGENTSUITE_DEMO_SUITE=suites/coordinator-workers-v2 AGENTSUITE_DEMO_AGENT=coordinator AGENTSUITE_DEMO_SANDBOX=microvm
source "${DEMO}/../common.sh"
source "${DEMO}/model-token.sh"

MODE="${1:-}"
case "${MODE}" in
setup | probe | lifecycle | crash | show | reset | teardown) ;;
*)
	echo "usage: $0 setup|probe|lifecycle|crash|show|reset|teardown" >&2
	exit 2
	;;
esac
AGENTS=(coordinator writer reviewer)
# The committed binding for this environment: the served model (its endpoint is filled in from the
# settings above) and the approval edges.
COORD_BINDING="${HERE}/binding-coordinator.json"
SERVER=127.0.0.1:18090
LOGS="${COORD_LOGS:-${WORK}/coordinator-logs}"
mkdir -p "${LOGS}"
JWT="ey""J"
FIELD="access""_token"
redact() {
	sed -u -E \
		-e "s/(Bearer )[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/g" \
		-e "s/${JWT}[A-Za-z0-9._-]+/[REDACTED-JWT]/g" \
		-e "s/(${FIELD}\"?[[:space:]]*[:=][[:space:]]*\"?)[^\",[:space:]]+/\1[REDACTED]/g"
}
# show writes its screens to stdout (through present.awk on a terminal) and each chapter's full
# output to its own log; the other modes tee everything to <mode>.log. Each run starts its log
# afresh, so a log holds the run that last used that mode, not every run on this host.
STEP_LOG="${LOGS}/${MODE}.log"
: >"${STEP_LOG}"
if [[ "${MODE}" == show ]]; then
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
# ate_names KIND ATESPACE [PREFIX]: the names of the KIND objects in ATESPACE that start with PREFIX.
ate_names() {
	"${ATE[@]}" get "$1" -a "$2" -o json | python3 -c '
import json, sys
d = json.load(sys.stdin)
items = d if isinstance(d, list) else next((v for v in d.values() if isinstance(v, list)), [])
print(" ".join(a["metadata"]["name"] for a in items if a["metadata"]["name"].startswith(sys.argv[1])))' "${3:-}"
}

# The placeholders of deploy/coordinator-agentsessionsd.yaml. render_manifest fills them with literal
# (not sed) substitution, the computed values first, so an operator value cannot be read as one.
COORDINATOR_PLACEHOLDERS=(DAEMON_IMAGE MODEL_SECRET_VERSION COORDINATOR_FLAG WRITER_FLAG REVIEWER_FLAG INVOKES_VALUE MEMBERS_VALUE MODEL_BASE_URL MODEL_AUTH_HEADER MODEL_ID)
# unfilled prints the placeholders the text $1 still contains.
unfilled() {
	local p left=""
	for p in "${COORDINATOR_PLACEHOLDERS[@]}"; do
		if [[ "$1" == *"${p}"* ]]; then left="${left:+${left} }${p}"; fi
	done
	echo "${left}"
}
# render_manifest DAEMON_IMAGE SECRET_VERSION COORDINATOR_FLAG WRITER_FLAG REVIEWER_FLAG INVOKES MODEL
# BASE_URL: the last two are the served model, from the binding (emit render's
# agentsessionsd.model-args). The daemon keeps its default -model-path.
render_manifest() {
	local m
	m="$(<"${HERE}/deploy/coordinator-agentsessionsd.yaml")"
	m=${m//DAEMON_IMAGE/"$1"}
	m=${m//MODEL_SECRET_VERSION/"$2"}
	m=${m//COORDINATOR_FLAG/"$3"}
	m=${m//WRITER_FLAG/"$4"}
	m=${m//REVIEWER_FLAG/"$5"}
	m=${m//INVOKES_VALUE/"$6"}
	m=${m//MEMBERS_VALUE/"${9:-}"}
	m=${m//MODEL_BASE_URL/"$8"}
	m=${m//MODEL_AUTH_HEADER/Authorization}
	m=${m//MODEL_ID/"$7"}
	printf '%s\n' "${m}"
}
# model_arg NAME FILE: the value of -NAME= in an agentsessionsd.model-args FILE.
model_arg() { sed -n "s/^-$1=//p" "$2"; }

setup() {
	local v
	# Before anything else: a run that still sets the retired AGENTSUITE_MODEL_PATH (common.sh).
	[[ -z "$(retired_model_path)" ]] || fail "$(retired_model_path)"
	for v in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AZURE_MI_CLIENT_ID; do
		[[ -n "${!v:-}" ]] || fail "${v} is not set"
	done
	for v in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL; do
		[[ -z "$(unfilled "${!v:-}")" ]] || fail "${v} contains $(unfilled "${!v:-}"), a placeholder of deploy/coordinator-agentsessionsd.yaml"
	done
	# The key is a managed identity's Entra token, which Azure OpenAI takes as a bearer token in
	# Authorization; the daemon is rendered with that header, so refuse another one (up.sh's api-key).
	# Header names are case-insensitive, so any spelling of Authorization is that header.
	[[ "$(printf '%s' "${AGENTSUITE_MODEL_AUTH_HEADER:-Authorization}" | tr '[:upper:]' '[:lower:]')" == authorization ]] ||
		fail "AGENTSUITE_MODEL_AUTH_HEADER names a header other than Authorization; this demo sends the managed identity's token in Authorization, so unset it"
	# Before the first cluster change: the manifest renders with no placeholder left.
	[[ -z "$(unfilled "$(render_manifest image 1 c w r i m u)")" ]] ||
		fail "deploy/coordinator-agentsessionsd.yaml holds placeholders this script does not fill: $(unfilled "$(render_manifest image 1 c w r i m u)")"
	[[ "$(go env GOARCH)" == amd64 ]] || fail "the suite declares linux/amd64 compositions only; run on an amd64 host"
	(cd "${HERE}" && go build -o "${WORK}/bin/emit" ./cmd/emit)
	(cd "${ROOT}" && go build -o "${WORK}/bin/agentctl" ./cmd/agentctl)
	# This environment's binding, one per agent: each agent's template and session actors live in its
	# own atespace (actors are named by session UID, so harnesses on one daemon need separate actor
	# atespaces, and a template's golden tag is scoped to its atespace). emit checks it against the
	# suite before anything changes: the served model must be the one every agent pins.
	local agent
	for agent in "${AGENTS[@]}"; do
		write_binding "${COORD_BINDING}" "${STATE}/binding-${agent}.json" "$(actors "${agent}")"
	done
	"${WORK}/bin/emit" check-binding -suite "${HERE}/${SUITE_DIR}" -binding "${STATE}/binding-coordinator.json" ||
		fail "this environment's binding does not serve the suite's model"
	[[ "$(python3 -c 'import json, sys; r = json.load(open(sys.argv[1]))["model"]["secretRef"]; print(r["name"], r["key"])' "${STATE}/binding-coordinator.json")" == "agentsessionsd-model api-key" ]] ||
		fail "the binding's model.secretRef is not agentsessionsd-model/api-key, the Secret this script writes"
	# up.sh validates the model settings before its first cluster change, then brings up kind,
	# ate-system and the micro-VM WorkerPool.
	step "up.sh --substrate-only"
	"${DEMO}/../up.sh" --substrate-only

	step "the suite, validated by kaimahi's agentsuite package"
	"${WORK}/bin/emit" validate -suite "${HERE}/${SUITE_DIR}"
	INVOKES="$("${WORK}/bin/emit" invokes -suite "${HERE}/${SUITE_DIR}" -binding "${STATE}/binding-coordinator.json")"
	echo "AGENTSUITE_INVOKES=${INVOKES}"

	local ctx="${STATE}/coordinator-image" out binding flags=() shared="" members=""
	case "${COORD_TEMPLATE:-per-agent}" in
	per-agent) ;;
	shared)
		# One image without an agent and one ActorTemplate in its own atespace for all three agents;
		# each agent's member (instructions, model, approval targets) reaches the harness at session
		# start from the daemon (AGENTSUITE_MEMBERS_JSON), checked against the digests the template pins.
		shared=1
		write_binding "${COORD_BINDING}" "${STATE}/binding-members.json" agentsuite-members
		members="$("${WORK}/bin/emit" members -suite "${HERE}/${SUITE_DIR}" -binding "${STATE}/binding-members.json")"
		;;
	*) fail "COORD_TEMPLATE=${COORD_TEMPLATE}: want per-agent or shared" ;;
	esac
	rm -rf "${ctx}" && mkdir -p "${ctx}"
	(cd "${HERE}" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "${ctx}/harness" ./cmd/harness)
	for agent in "${AGENTS[@]}"; do
		step "agent ${agent}: harness image, binding, ActorTemplate"
		local tspace tag rflags=()
		tspace="$(actors "${agent}")" tag="${agent}" binding="${STATE}/binding-${agent}.json"
		rm -rf "${ctx}/agent"
		if [[ -n "${shared}" ]]; then
			tspace=agentsuite-members tag=shared binding="${STATE}/binding-members.json" rflags=(-shared)
			mkdir -p "${ctx}/agent"
		else
			"${WORK}/bin/emit" bundle -suite "${HERE}/${SUITE_DIR}" -agent "${agent}" -platform linux/amd64 -out "${ctx}/agent"
		fi
		docker build -q --platform linux/amd64 -f "${HERE}/Dockerfile.harness" -t "localhost:5001/agentsuite-harness:${tag}" "${ctx}" >/dev/null
		docker push -q "localhost:5001/agentsuite-harness:${tag}" >/dev/null
		local image
		image="$(docker inspect --format '{{index .RepoDigests 0}}' "localhost:5001/agentsuite-harness:${tag}")"
		out="${STATE}/render-${agent}"
		"${WORK}/bin/emit" render -suite "${HERE}/${SUITE_DIR}" -agent "${agent}" -platform linux/amd64 -binding "${binding}" \
			-image "${image}" -actors "$(actors "${agent}")" "${rflags[@]}" -out "${out}"
		local space
		for space in "${tspace}" "$(actors "${agent}")"; do
			"${ATE[@]}" get atespace "${space}" >/dev/null 2>&1 || "${ATE[@]}" create atespace "${space}"
		done
		local template
		template="$(cat "${out}/template-name")"
		"${ATE[@]}" get actor-template "${template}" -a "${tspace}" >/dev/null 2>&1 || "${ATE[@]}" create actor-template -f "${out}/actortemplate.json"
		wait_golden "${tspace}" "${template}"
		# A template an earlier setup created for this agent (another image or option) is left over;
		# the atespace holds only the current one, which is what show lists.
		local old
		for old in $(ate_names actor-templates "${tspace}"); do
			[[ "${old}" == "${template}" ]] || "${ATE[@]}" delete actor-template "${old}" -a "${tspace}"
		done
		flags+=("$(cat "${out}/agentsessionsd.args")")
	done

	# One daemon serves the three agents, so it serves one model: the three renders must agree on it.
	local margs="${STATE}/render-coordinator/agentsessionsd.model-args"
	for agent in writer reviewer; do
		cmp -s "${margs}" "${STATE}/render-${agent}/agentsessionsd.model-args" || fail "the ${agent} render serves another model than the coordinator's"
	done
	local model base
	model="$(model_arg model "${margs}")" base="$(model_arg model-base-url "${margs}")"
	[[ -n "${model}" && -n "${base}" ]] || fail "the binding does not give the model and base URL the daemon needs"
	[[ -z "$(model_arg model-path "${margs}")" ]] || fail "the render sets -model-path; the daemon keeps its default"

	step "a model token for the VM's managed identity (never printed, never in a file)"
	model_secret "${NS}" "${AZURE_MI_CLIENT_ID}" "${K[@]}"
	local version
	version="$("${K[@]}" -n "${NS}" get secret agentsessionsd-model -o jsonpath='{.metadata.resourceVersion}')"

	step "agentsessionsd with the invocation executor (examples/agentsuite-substrate/cmd/agentsessionsd)"
	local daemon manifest
	daemon="$(cd "${HERE}" && KO_DOCKER_REPO=localhost:5001 ko build --base-import-paths --platform=linux/amd64 ./cmd/agentsessionsd)"
	manifest="$(render_manifest "${daemon}" "${version}" "${flags[0]}" "${flags[1]}" "${flags[2]}" "${INVOKES}" "${model}" "${base}" "${members}")"
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
	ctl exec --harness coordinator --input "Have the writer write a two-line poem about tides, then have the reviewer give a one-sentence clarity review. Reply with the final poem and that sentence." | tee "${log}-exec.txt"
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
	# An approved reviewer call must have run a reviewer child: a result naming one, not a refusal.
	if [[ "${decision}" == approve ]]; then
		grep 'EVENT_TOOL_RESULT' "${log}-journal.txt" | grep -q '"agent":"reviewer"' || fail "session ${uid}: the approved reviewer call ran no reviewer child"
	fi
	echo "${uid}" >"${log}-uid"
}

probe() {
	# Only this probe's files: a previous probe's would read as this one's evidence.
	rm -f "${LOGS}"/probe-*
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
# lifecycle: LIFECYCLE_RUNS (default 5) coordinator requests that each delegate to writer and then
# send that same writer session a follow-up, so each run suspends the child twice and wakes it once.
# Prints the actors per state before and after, and every delegation's lifecycle from the journals.
# crash: kills the daemon's process (SIGKILL on its kind node) while a writer child is mid-call, lets
# the container restart (the journal's emptyDir survives a container restart), resumes the
# coordinator's turn, and checks the retry reached the same writer child. Both end with ... OK.
# =================================================================================================
forward() {
	local p
	for p in "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done
	PIDS=()
	"${K[@]}" -n "${NS}" port-forward svc/agentsessionsd-coordinator "${SERVER##*:}:8080" >/dev/null 2>&1 &
	PIDS+=($!)
	for _ in $(seq 60); do
		if ctl list >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	fail "the Sessions API did not answer"
}
# actor_states: the session actors per state in each agent's atespace.
actor_states() {
	local agent
	for agent in "${AGENTS[@]}"; do
		printf '%s: ' "$(actors "${agent}")"
		"${ATE[@]}" get actors -a "$(actors "${agent}")" -o json | python3 -c '
import collections, json, sys
d = json.load(sys.stdin)
items = d if isinstance(d, list) else next((v for v in d.values() if isinstance(v, list)), [])
c = collections.Counter(a.get("status", {}).get("state", "?") for a in items if a["metadata"]["name"].startswith("sess-"))
print(" ".join("%s=%d" % kv for kv in sorted(c.items())) or "none")'
	done
}
# lifecycles JOURNAL...: one row per delegation result in agentctl replay -v output.
lifecycles() {
	python3 -c '
import json, re, sys
rows = []
for f in sys.argv[1:]:
    for line in open(f):
        if "EVENT_TOOL_RESULT" not in line:
            continue
        m = re.search(r"\"lifecycle\":(\{[^{}]*\})", line)
        if m:
            lc = json.loads(m.group(1))
            a = re.search(r"\"agent\":\"([a-z]+)\"", line)
            rows.append((f.rsplit("/", 1)[-1], a.group(1) if a else "?", lc))
for f, agent, lc in rows:
    print("%-26s %-8s origin=%-9s turn=%-9s wakeMs=%-6s turnMs=%-6s suspendMs=%-6s suspended=%s" % (f, agent, lc.get("origin"), lc.get("turn"), lc.get("wakeMs", "-"), lc.get("turnMs", "-"), lc.get("suspendMs", "-"), lc.get("suspended")))
def stats(k):
    v = sorted(float(lc[k]) for _, _, lc in rows if k in lc)
    return "n=%d min=%d median=%d max=%d" % (len(v), v[0], v[len(v) // 2], v[-1]) if v else "n=0"
print("wakeMs " + stats("wakeMs")); print("suspendMs " + stats("suspendMs")); print("turnMs " + stats("turnMs"))
' "$@"
}

lifecycle() {
	rm -f "${LOGS}"/lifecycle-*
	forward
	step "session actors before"
	actor_states | tee "${LOGS}/lifecycle-actors-before.txt"
	local i uid runs="${LIFECYCLE_RUNS:-8}"
	for i in $(seq "${runs}"); do
		step "run ${i}: a writer delegation, then a follow-up to the same writer session"
		ctl exec --harness coordinator --input "Have the writer write a two-line poem about tides. Then delegate a follow-up to that same writer session (give its session id) asking it to make the two lines rhyme. Do not use the reviewer. Reply with the final poem." >"${LOGS}/lifecycle-${i}-exec.txt"
		uid="$(sed -n 's/^session //p' "${LOGS}/lifecycle-${i}-exec.txt" | head -1)"
		[[ -n "${uid}" ]] || fail "run ${i}: no session uid"
		ctl replay --session "${uid}" -v >"${LOGS}/lifecycle-${i}-journal.txt"
		actor_states
		# The deployment's rate limit is low; space the runs out.
		sleep "${LIFECYCLE_PAUSE:-30}"
	done
	step "session actors after"
	actor_states | tee "${LOGS}/lifecycle-actors-after.txt"
	ctl list >"${LOGS}/lifecycle-list.txt"
	step "every delegation's child lifecycle, from the coordinators' journals"
	lifecycles "${LOGS}"/lifecycle-*-journal.txt | tee "${LOGS}/lifecycle-table.txt"
	"${K[@]}" -n "${NS}" logs deploy/agentsessionsd-coordinator 2>&1 | grep 'agentsuite invocation' >"${LOGS}/lifecycle-daemon.txt" || true
	# Root (coordinator) sessions keep their actors; every child's must be suspended.
	grep -E '^agentsuite-(writer|reviewer):.*RUNNING' "${LOGS}/lifecycle-actors-after.txt" && fail "a child session's actor is still running after its turn"
	local woke want="${LIFECYCLE_MIN_WAKES:-5}"
	woke="$(grep -c 'origin=follow-up' "${LOGS}/lifecycle-table.txt" || true)"
	# The model chooses whether to follow up; a run where it starts a new writer session instead
	# wakes nothing, so there are more runs than wakes wanted.
	((woke >= want)) || fail "only ${woke} follow-ups woke a child; want ${want}"
	echo "LIFECYCLE OK"
}

crash() {
	rm -f "${LOGS}"/crash-*
	forward
	local before pod node uid restarts
	before="$(ctl list | grep -c 'harness=writer	' || true)"
	pod="$("${K[@]}" -n "${NS}" get pod -l app=agentsessionsd-coordinator -o jsonpath='{.items[0].metadata.name}')"
	node="$("${K[@]}" -n "${NS}" get pod "${pod}" -o jsonpath='{.spec.nodeName}')"
	restarts="$("${K[@]}" -n "${NS}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}')"
	step "a coordinator request; the daemon is killed once its writer child exists"
	ctl exec --harness coordinator --input "Have the writer write a two-line poem about tides and reply with it. Do not use the reviewer." >"${LOGS}/crash-exec.txt" 2>&1 &
	local exec_pid=$! i
	for i in $(seq 240); do
		(($(ctl list | grep -c 'harness=writer	' || true) > before)) && break
		((i < 240)) || fail "no writer child appeared"
		sleep 0.5
	done
	ctl list | grep 'harness=writer	' >"${LOGS}/crash-writers-at-kill.txt" || true
	docker exec "${node}" pkill -9 -f /ko-app/agentsessionsd
	echo "killed agentsessionsd (SIGKILL) on ${node} at $(date -u +%T)"
	wait "${exec_pid}" || true
	uid="$(sed -n 's/^session //p' "${LOGS}/crash-exec.txt" | head -1)"
	[[ -n "${uid}" ]] || fail "no coordinator session uid in the exec output"
	for i in $(seq 120); do
		(($("${K[@]}" -n "${NS}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}') > restarts)) &&
			"${K[@]}" -n "${NS}" wait --for=condition=Ready "pod/${pod}" --timeout=5s >/dev/null 2>&1 && break
		((i < 120)) || fail "the daemon container did not restart"
		sleep 1
	done
	forward
	ctl replay --session "${uid}" -v >"${LOGS}/crash-journal-before-resume.txt"
	step "resume the coordinator's turn on the restarted daemon"
	ctl resume --session "${uid}" | tee "${LOGS}/crash-resume.txt"
	ctl replay --session "${uid}" -v >"${LOGS}/crash-journal.txt"
	ctl list >"${LOGS}/crash-list.txt"
	lifecycles "${LOGS}/crash-journal.txt" | tee "${LOGS}/crash-table.txt"
	actor_states | tee "${LOGS}/crash-actors.txt"
	local after
	after="$(grep -c 'harness=writer	' "${LOGS}/crash-list.txt" || true)"
	((after == before + 1)) || fail "writer sessions went from ${before} to ${after}; want exactly one new child"
	grep -q 'origin=retry' "${LOGS}/crash-table.txt" || fail "the writer result is not a retry of the first child"
	grep -q 'EVENT_END' "${LOGS}/crash-journal.txt" || fail "the resumed turn did not end"
	echo "CRASH OK"
}

# =================================================================================================
# reset: off camera, between two runs of show: deletes the session actors in the three agents'
# atespaces and restarts agentsessionsd, whose journal is an emptyDir, so show starts from an empty
# journal and no session actors.
# =================================================================================================
reset() {
	local agent name
	for agent in "${AGENTS[@]}"; do
		for name in $(ate_names actors "$(actors "${agent}")" sess-); do
			"${ATE[@]}" delete actor "${name}" -a "$(actors "${agent}")" --any-state
		done
		[[ -z "$(ate_names actors "$(actors "${agent}")" sess-)" ]] || fail "session actors remain in $(actors "${agent}")"
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
# shellcheck source=SCRIPTDIR/screen.sh
source "${DEMO}/screen.sh"
# Longer holds than driver.sh's: this demo has fewer screens, most of them journal records to read.
CAPTION_HOLD=4 FILE_HOLD=9 DIAGRAM_HOLD=15 TITLE_HOLD=8

# nth KIND N: the seq of the Nth record of KIND in RAW's journal (agentctl replay output).
nth() { python3 -c 'import re, sys; s = [m.group(1) for m in re.finditer(r"^seq=(\d+) +" + sys.argv[1] + r"\b", open(sys.argv[3]).read(), re.M)]; n = int(sys.argv[2]); print(s[n - 1] if len(s) >= n else "")' "$1" "$2" "${RAW}"; }
# line_of SEQ: RAW's journal line of that record.
line_of() { python3 -c 'import re, sys; m = re.search(r"^seq=" + sys.argv[1] + r" .*$", open(sys.argv[2]).read(), re.M); print(m.group(0) if m else "")' "$1" "${RAW}"; }
# sessions HARNESS: how many sessions agentctl list shows on HARNESS.
sessions() { "${WORK}/bin/agentctl" list --server "${SERVER}" | grep -c "harness=$1	" || true; }

show() {
	[[ -x "${WORK}/bin/agentctl" && -x "${WORK}/bin/emit" ]] || fail "run coordinator.sh setup first"
	rm -f "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv"
	rm -rf /tmp/render-coordinator /tmp/render-refused /tmp/approve.txt /tmp/deny.txt
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
	local template wtemplate rtemplate model seq call res req out
	model="$(model_arg model "${STATE}/render-coordinator/agentsessionsd.model-args")"
	[[ -n "${model}" ]] || fail "setup's render names no model"
	template="$(cat "${STATE}/render-coordinator/template-name")"
	wtemplate="$(cat "${STATE}/render-writer/template-name")"
	rtemplate="$(cat "${STATE}/render-reviewer/template-name")"
	log "API=${API} IMAGE=${IMAGE} BINDING=${BINDING} templates: ${template} ${wtemplate} ${rtemplate}"
	local input="Have the writer write a two-line poem about tides, then have the reviewer give a one-sentence clarity review. Reply with the final poem and that sentence."

	printf '\e[?25l\e[2J\e[3J\e[H\n'
	printf '%s  An AgentSuite coordinator on AgentSessions and micro-VMs%s\n\n' "${C}" "${N}"
	wrap "kaimahi's coordinator-workers suite, in a version that pins ${model}: the coordinator delegates to writer and reviewer, each call a child session in its own micro-VM, with the model served on Azure OpenAI as this environment's binding says. The call to reviewer waits for approval." "  " | sed "s/^/${W}/; s/\$/${N}/"
	sleep "${TITLE_HOLD}"

	# ---------------------------------------------------------------------------------------------
	chapter 1 "The suite" "kaimahi's coordinator-workers test suite (agentsuite/testdata at 75f27d6): three agents"
	page "The coordinator may invoke writer and reviewer: one call at a time each, and no deeper chain"
	ends_at suites/coordinator-workers/agents/coordinator.json 28
	run -f <<'EOF'
bat suites/coordinator-workers/agents/coordinator.json
EOF
	lines_hold suites/coordinator-workers/agents/coordinator.json 12:'"model": "example-model"' 15:'"invokes"' 17:'"agent": "writer"' 18:'"maxConcurrent": 1' 19:'"maxDepth": 1' 22:'"agent": "reviewer"'
	page "Its agents name example-model. A new version of the suite pins the model they run with: four files change"
	run -x <<'EOF'
diff -rq -x SOURCE -x README.md suites/coordinator-workers suites/coordinator-workers-v2
EOF
	[[ "$(printf '%s\n' "${SHOWN}" | grep -c ' differ$')" == 4 ]] || fail "the new version does not differ in four files"
	page "Each agent's model, and the digests of the agents in agentsuite.json"
	run -x <<'EOF'
diff suites/coordinator-workers/agents/coordinator.json suites/coordinator-workers-v2/agents/coordinator.json
EOF
	[[ "${SHOWN}" == *'"model": "example-model"'*'"model": "gpt-5.4-mini"'* ]] || fail "the new version does not pin gpt-5.4-mini"
	page "kaimahi's validator accepts the new version; its digest, which covers every manifest, is the suite version"
	run <<'EOF'
emit validate -suite suites/coordinator-workers-v2
EOF
	[[ "${SHOWN}" == "suite coordinator-workers valid: 3 agents"*"digest sha256:306685d5375bb021ae1424f610012441b4381d0d49feab3b4c208ac2ced96c60"*"kaimahi/agentsuite v"*"-75f27d67f40b"* ]] ||
		fail "emit validate did not report the new version's digest and kaimahi 75f27d6's validator"

	# ---------------------------------------------------------------------------------------------
	chapter 2 "The translation" "This environment's AgentSessionsBinding, outside the suite: where the model is served, which calls need approval"
	run -f <<'EOF'
bat $BINDING
EOF
	python3 -c '
import json, sys
b = json.load(open(sys.argv[1]))
ok = b["model"]["name"] == sys.argv[2] and b["approval"]["invokes"] == [{"caller": "coordinator", "agent": "reviewer"}] and "path" not in b["model"] and b["model"]["baseURL"].rstrip("/").endswith("/openai/v1")
sys.exit(0 if ok else 1)' "${BINDING}" "${model}" || fail "the binding does not serve ${model} from an Azure OpenAI v1 endpoint with no model.path and gate coordinator -> reviewer"
	page "kaimahi's original suite pins example-model, which this binding does not serve: emit render refuses it"
	run -x <<'EOF'
emit render -suite suites/coordinator-workers -agent coordinator -platform linux/amd64 -binding $BINDING -image $IMAGE -out /tmp/render-refused 2>&1
EOF
	[[ "${SHOWN}" == *'pins model "example-model" but the binding serves "'"${model}"'"; a different model needs a new suite version'* ]] ||
		fail "emit render did not refuse the model mismatch"
	[[ ! -e /tmp/render-refused ]] || fail "the refused render wrote files"
	page "emit invokes: the suite's edges as the daemon's policy, gated by the binding's approval edge"
	run <<'EOF'
emit invokes -suite suites/coordinator-workers-v2 -binding $BINDING
EOF
	[[ "${SHOWN}" == "coordinator=reviewer:1:1:approval,coordinator=writer:1:1" ]] || fail "emit invokes printed another policy"
	page "emit render of the new version: one ActorTemplate per agent; the model is the suite's, with no override"
	run -n 8 <<'EOF'
emit render -suite suites/coordinator-workers-v2 -agent coordinator -platform linux/amd64 -binding $BINDING -image $IMAGE -actors agentsuite-coordinator -out /tmp/render-coordinator
EOF
	[[ "${SHOWN}" == *"model ${model}, pinned by the suite and served by the binding"* ]] || fail "emit render did not name the pinned model"
	offscreen "this render against the one setup created" diff -r /tmp/render-coordinator "${STATE}/render-coordinator" || fail "the render differs from setup's"
	page "In the template: the delegation targets that need approval, from the binding"
	seq="$(python3 -c 'import sys; print(next(i for i, l in enumerate(open(sys.argv[1]), 1) if "AGENTSUITE_APPROVAL" in l))' /tmp/render-coordinator/actortemplate.json)"
	run -f <<EOF
bat -r $((seq - 4)):$((seq + 1)) /tmp/render-coordinator/actortemplate.json
EOF
	lines_hold /tmp/render-coordinator/actortemplate.json "${seq}:AGENTSUITE_APPROVAL" "$((seq + 1)):\"value\": \"reviewer\""
	page "And the flags that serve it from agentsessionsd: the harness, its actors' atespace, and the binding's model"
	run -n 8 <<'EOF'
cat /tmp/render-coordinator/agentsessionsd.args /tmp/render-coordinator/agentsessionsd.model-args
EOF
	[[ "${SHOWN}" == "-substrate-harness=coordinator=agentsuite-coordinator/${template},actors=agentsuite-coordinator"$'\n'"-model=${model}"$'\n'"-model-base-url="* && "${SHOWN}" != *-model-path=* ]] ||
		fail "the coordinator's flags are not ${template} and the binding's model (with the daemon's default -model-path)"

	# ---------------------------------------------------------------------------------------------
	chapter 3 "In the cluster" "Created before recording with kubectl ate create, from the three renders: one ActorTemplate per agent"
	run -n 12 <<'EOF'
kubectl ate get actor-templates -a agentsuite-coordinator && kubectl ate get actor-templates -a agentsuite-writer && kubectl ate get actor-templates -a agentsuite-reviewer
EOF
	[[ "${SHOWN}" == *"${template}"* && "${SHOWN}" == *"${wtemplate}"* && "${SHOWN}" == *"${rtemplate}"* ]] || fail "the three templates are not in the cluster"
	page "Manual step: three -substrate-harness flags register them; the HarnessRegistry does not take substrate placements"
	run <<'EOF'
kubectl get deploy agentsessionsd-coordinator -n agentsuite-demo -o jsonpath='{range .spec.template.spec.containers[0].args[5:8]}{@}{"\n"}{end}'
EOF
	[[ "$(printf '%s\n' "${SHOWN}" | grep -c '^-substrate-harness=')" == 3 ]] || fail "the Deployment does not carry three -substrate-harness flags at args 5-7"
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
	local gated
	gated="$(nth EVENT_TOOL_CALL 2)" req="$(last_seq EVENT_APPROVAL_REQUEST)"
	[[ -n "${gated}" && -n "${req}" && "$(line_of "${gated}")" == *'"agent":"reviewer"'* && "$(line_of "${gated}")" == *"mediation=REQUIRES_APPROVAL"* ]] ||
		fail "the coordinator's second delegation is not a gated call to reviewer"
	page "The delegation to writer: a tool call the host ran as a child session, and its reply"
	run -n 8 <<EOF
agentctl replay --server \$API --session \$S --from ${call} --to ${res}
EOF
	[[ "${SHOWN}" == *'"session":"sess-'* ]] || fail "the writer result names no child session"
	[[ "$(sessions reviewer)" == 0 ]] || fail "a reviewer session exists before the approval"
	page "The call to reviewer needs approval: the host records the request and parks the turn"
	run -n 8 <<EOF
agentctl replay --server \$API --session \$S --from ${gated}
EOF
	run -n 8 <<'EOF'
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
	run -n 12 <<EOF
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
	SUSPEND_MS="$(line_of "$(nth EVENT_TOOL_RESULT 1)" | python3 -c 'import re, sys; m = re.search(r"\"suspendMs\":\s*([0-9]+)", sys.stdin.read()); print(m.group(1) if m else "")')"
	[[ -n "${SUSPEND_MS}" ]] || fail "the writer result records no suspend of its child"
	log "(CHILD=${CHILD}, read from the writer result on chapter 4's screen; its suspendMs=${SUSPEND_MS})"
	page "The writer session's first record names its parent session, the tool call and its place in the chain"
	run -n 6 <<'EOF'
agentctl replay --server $API --session $CHILD --to 1
EOF
	[[ "${SHOWN}" == *"\"parentSession\":\"${S}\""* && "${SHOWN}" == *'"caller":"coordinator"'* && "${SHOWN}" == *'"depth":1'* ]] || fail "the writer session does not record its parent"
	page "Each child ran as an actor from its agent's template on micro-VMs; after its turn the host suspended it"
	run -n 12 <<'EOF'
kubectl ate get actors -a agentsuite-writer && kubectl ate get actors -a agentsuite-reviewer
EOF
	[[ "${SHOWN}" == *"${CHILD}"*"${wtemplate}"* && "${SHOWN}" == *"${rtemplate}"* ]] || fail "the child sessions' actors are not from the workers' templates"
	# The invocation executor suspends every child before it records the child's result (chapter 8):
	# the writer child's actor is suspended and has no worker pod.
	local row suspended=""
	while IFS= read -r row; do
		if [[ "${row}" == *"${CHILD}"*"${wtemplate}"*ACTOR_STATE_SUSPENDED*"<none>"* ]]; then suspended=1; fi
	done <<<"${SHOWN}"
	[[ -n "${suspended}" ]] || fail "the writer child's actor is not suspended without a worker after its turn"

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
	# Five sessions; a child's name carries its key (<caller>-<target>-<key[:12]>), so the reviewer's
	# row is one column too wide and wraps.
	run -n 6 <<'EOF'
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
	chapter 8 "Open questions" "Open questions, each tagged with whose it is, and one this PoC answered"
	show_lines "kmx: the binding has no approval field; this PoC adds approval.invokes per environment. Add it, or declare it in AgentSuite?
kmx: no model.path needed: Azure's v1 endpoint (<resource>/openai/v1) takes the deployment as model and no api-version.
kmx: eval and lift take one chat member; a suite with invokes edges has no eval yet, so this render has no eval evidence.
kmx: a binding names one atespace; here each agent's actors need their own, so setup writes one binding per agent.
AgentSuite: maxConcurrent bounds the declaring agent's calls; across runtime replicas too? This PoC counts per daemon.
AgentSessions: agent-to-agent calls are a host tool call that creates a child session; should there be a first-class primitive?
AgentSessions: CreateSession takes no idempotency key; this PoC keys each child by parent + tool call in a label (README).
AgentSessions: the HarnessRegistry does not take substrate placements yet, so the three harnesses are daemon flags.
Answered: the host suspends each child after its turn (writer: ${SUSPEND_MS} ms here) and wakes it for a follow-up (README).
substrate: is a template's golden snapshot usable from another atespace? Here each agent's template has its own." 16
	sleep $((READ * 3))
	log "DEMO COMPLETE in $((SECONDS - SHOW_START)) s"
	printf '\n%s\n' "agentsuite-substrate coordinator demo."
}

case "${MODE}" in
setup) setup ;;
probe) probe ;;
lifecycle) lifecycle ;;
crash) crash ;;
show)
	SHOW_START=${SECONDS}
	show
	;;
reset) reset ;;
teardown) "${DEMO}/../down.sh" ;;
esac
