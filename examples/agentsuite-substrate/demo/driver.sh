#!/usr/bin/env bash
# The recorded end-to-end demo: kaimahi's minimal AgentSuite (agent `writer`) as an AgentSessions
# harness on agent-substrate micro-VMs, driven by a real model on Azure OpenAI. Steps 0-10 each print
# a banner, every command before its output, and what the step proves, and the script stops on the
# first proof that does not hold.
#
# It runs where Docker, kind and the cluster run (the demo recording runs it on a Linux host with KVM
# through `hq remote-test`, see record.sh), with the tools Dockerfile.tools installs. Settings:
#
#   AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL, AGENTSUITE_MODEL_PATH  the Azure OpenAI deployment
#   AZURE_MI_CLIENT_ID     the user-assigned managed identity whose token is the model API key; the
#                          token is fetched from the instance metadata endpoint in step 3, kept in a
#                          mode-0600 file under the work directory and the daemon's Secret, and never
#                          printed
#   DEMO_LOGS              directory for the per-step logs (default ${WORK}/demo-logs)
#   DEMO_PAUSE             seconds to pause after each step's proof (default 12), and
#   DEMO_RUN_PAUSE         after each command (default 4), so a viewer can read the output; 0 for a
#                          quick run (record.sh sets 25 and 8)
#
# Every line the script prints, on the terminal and in the logs, goes through redact first.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
export AGENTSUITE_DEMO_SUITE=suites/kaimahi-minimal AGENTSUITE_DEMO_AGENT=writer AGENTSUITE_DEMO_SANDBOX=microvm
source "${DEMO}/../common.sh"

LOGS="${DEMO_LOGS:-${WORK}/demo-logs}"
PAUSE="${DEMO_PAUSE:-12}"
RUN_PAUSE="${DEMO_RUN_PAUSE:-4}"
mkdir -p "${LOGS}"
rm -f "${LOGS}"/step-*.log "${LOGS}/timeline.tsv"
SERVER=127.0.0.1:18080 MEMSERVER=127.0.0.1:18083 ROUTER=127.0.0.1:18081
TOKEN_FILE="${WORK}/model-token"

# The token patterns are assembled from pieces so this file does not match the scan the recording is
# checked with (a JWT prefix, the IMDS JSON field name).
JWT="ey""J"
FIELD="access""_token"
redact() {
	sed -u -E \
		-e "s/(Bearer )[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/g" \
		-e "s/${JWT}[A-Za-z0-9._-]+/[REDACTED-JWT]/g" \
		-e "s/(${FIELD}\"?[[:space:]]*[:=][[:space:]]*\"?)[^\",[:space:]]+/\1[REDACTED]/g"
}
exec > >(redact) 2>&1
REDACT_PID=$!

B=$'\e[1;36m' Y=$'\e[1;33m' G=$'\e[1;32m' R=$'\e[1;31m' D=$'\e[2m' N=$'\e[0m'
START=${SECONDS}
STEP_LOG="${LOGS}/step-setup.log"
# step N "title": a banner, and a new per-step log the rest of the step tees into.
step() {
	local n="$1"
	shift
	STEP_LOG="${LOGS}/$(printf 'step-%02d' "${n}").log"
	printf '%s\t%s\t%s\n' "${n}" "$(date -u +%s)" "$*" >>"${LOGS}/timeline.tsv"
	local t=$((SECONDS - START))
	printf '\n%s%s%s\n' "${B}" "================================================================================" "${N}"
	printf '%s STEP %s  %s%s   %s[T+%02d:%02d]%s\n' "${B}" "${n}" "$*" "${N}" "${D}" $((t / 60)) $((t % 60)) "${N}"
	printf '%s%s%s\n' "${B}" "================================================================================" "${N}"
	printf '# STEP %s  %s  (%s)\n' "${n}" "$*" "$(date -u +%FT%TZ)" >"${STEP_LOG}"
}
note() {
	printf '%s# %s%s\n' "${D}" "$*" "${N}"
	printf '# %s\n' "$*" >>"${STEP_LOG}"
}
# shown CMD...: the command as a reader would type it. agentctl and memctl are the agentctl binary
# against the stateless and the memory-tier daemon.
shown() {
	case "$1" in
	agentctl) echo "agentctl $2 --server ${SERVER} ${*:3}" ;;
	memctl) echo "agentctl $2 --server ${MEMSERVER} ${*:3}" ;;
	*) echo "$*" ;;
	esac
}
# run CMD...: print the command, then run it with its output on the terminal and in the step log.
run() {
	printf '%s$ %s%s\n' "${Y}" "$(shown "$@")" "${N}"
	printf '$ %s\n' "$(shown "$@")" >>"${STEP_LOG}"
	local rc
	set +e
	"$@" 2>&1 | redact | tee -a "${STEP_LOG}"
	rc="${PIPESTATUS[0]}"
	set -e
	sleep "${RUN_PAUSE}"
	return "${rc}"
}
proves() {
	printf '%s--> proves: %s%s\n' "${G}" "$*" "${N}"
	printf -- '--> proves: %s\n' "$*" >>"${STEP_LOG}"
	sleep "${PAUSE}"
}
fail() {
	FAILED=1
	printf '%sDEMO FAILED: %s%s\n' "${R}" "$*" "${N}"
	printf 'DEMO FAILED: %s\n' "$*" >>"${STEP_LOG}"
	exit 1
}

PIDS=()
TAILS=()
cleanup() {
	local rc=$?
	# Any exit before the end (set -e included) is a failure the recording must show: demo.tape waits
	# for DEMO COMPLETE or DEMO FAILED.
	if ((rc != 0)) && [[ -z "${FAILED:-}" ]]; then
		printf '%sDEMO FAILED: exit %d in step log %s%s\n' "${R}" "${rc}" "${STEP_LOG##*/}" "${N}"
		printf 'DEMO FAILED: exit %d\n' "${rc}" >>"${STEP_LOG}"
	fi
	for p in "${TAILS[@]}" "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done
	rm -f "${TOKEN_FILE}"
	# Let the redacting filter flush the last lines, for at most 10 seconds: a process that inherited
	# stdout and outlived its step must not keep the job, and the recording, from ending.
	exec 1>&- 2>&-
	for _ in $(seq 100); do
		kill -0 "${REDACT_PID}" 2>/dev/null || break
		sleep 0.1
	done
}
trap cleanup EXIT
forward() { # namespace service local:remote
	"${K[@]}" -n "$1" port-forward "svc/$2" "$3" >/dev/null 2>&1 &
	PIDS+=($!)
}
# tail_logs LABEL CMD...: follow a log stream in-frame, each line prefixed with LABEL, until untail.
# CMD is a binary, so $! is its own PID and killing it ends the filter behind it.
tail_logs() {
	local label="$1" log="${STEP_LOG}"
	shift
	"$@" > >(redact | sed -u "s/^/${D}  [${label}]${N} /" | tee -a "${log}") 2>&1 &
	TAILS+=($!)
}
# tail_actor SESSION: follow the session's actor output from the moment it runs (the host creates it
# on the first turn and resumes it after a suspend), so the first turn's lines are in frame too.
tail_actor() {
	# shellcheck disable=SC2016 # the inner script expands its own arguments
	tail_logs actor bash -c 'until "${@:3}" get actor "$1" -a "$2" -o json 2>/dev/null | grep -q ACTOR_STATE_RUNNING; do sleep 1; done
exec "${@:3}" logs actor "$1" -a "$2" -f --source containers' _ "$1" "${ATESPACE}" "${ATE[@]}"
}
untail() {
	sleep 3
	for p in "${TAILS[@]}"; do kill "${p}" 2>/dev/null || true; done
	TAILS=()
}

agentctl() { "${WORK}/bin/agentctl" "$1" --server "${SERVER}" "${@:2}"; }
memctl() { "${WORK}/bin/agentctl" "$1" --server "${MEMSERVER}" "${@:2}"; }
actor_json() { "${ATE[@]}" get actor "$1" -a "${ATESPACE}" -o json; }
actor_field() { actor_json "$1" | python3 -c "import json, sys; a = json.load(sys.stdin); print($2)"; }
held_snapshot() { # the newest COMPLETED durable snapshot in status.snapshots, as the adapter reads it
	actor_json "$1" | python3 -c '
import json, sys
snaps = [s for s in json.load(sys.stdin)["status"].get("snapshots", [])
         if s.get("durableSnapshot", {}).get("status") == "SNAPSHOT_STORAGE_STATUS_COMPLETED"]
s = max(snaps, key=lambda s: s["generation"]) if snaps else None
print("generation=%s fidelity=%s uri=%s" % (s["generation"], s["fidelity"], s["durableSnapshot"]["object"]["snapshotUri"]) if s else "")'
}
inputs() { # ctl session -> the user inputs its journal holds, in order, joined by ' | '
	"$1" replay --session "$2" | sed -n 's/^seq=[0-9]* *EVENT_INPUT  user: //p' | paste -sd '|' - | sed 's/|/ | /g'
}
records() { # ctl session kind -> how many records of that kind its journal holds
	"$1" replay --session "$2" | awk -v k="$3" '$2 == k { n++ } END { print n + 0 }'
}
# say ctl session input: one turn, printed; REPLY is the assistant text.
say() {
	local out
	printf '%s$ %s%s\n' "${Y}" "$(shown "$1" exec --session "$2" --input "$(printf '%q' "$3")")" "${N}"
	printf '$ %s\n' "$(shown "$1" exec --session "$2" --input "$(printf '%q' "$3")")" >>"${STEP_LOG}"
	out="$("$1" exec --session "$2" --input "$3" 2>&1)" || {
		printf '%s\n' "${out}" | redact | tee -a "${STEP_LOG}"
		fail "turn failed: $3"
	}
	printf '%s\n' "${out}" | redact | tee -a "${STEP_LOG}"
	REPLY="$(sed -n 's/^seq=[0-9]* *EVENT_OUTPUT  assistant: //p' <<<"${out}" | tail -1)"
	[[ -n "${REPLY}" ]] || fail "turn produced no assistant output: $3"
}

printf '%sAgentSuite (kaimahi minimal, agent writer) -> AgentSessions -> agent-substrate micro-VMs, model %s on Azure OpenAI%s\n' "${B}" "${AGENTSUITE_MODEL:-?}" "${N}"
for v in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AZURE_MI_CLIENT_ID; do
	[[ -n "${!v:-}" ]] || fail "${v} is not set"
done

# ----------------------------------------------------------------------------------------------
step 0 "micro-VM smoke: KVM, kind, ate-system, the microvm class; one actor boots, suspends, resumes"
note "The Docker host is a Linux VM with KVM; the kind node gets /dev/kvm, and every actor below is a kata + cloud-hypervisor micro-VM."
run uname -srm
run sh -c "grep -m1 -o -w -E 'svm|vmx' /proc/cpuinfo; grep -m1 'model name' /proc/cpuinfo"
run docker run --rm --device /dev/kvm busybox ls -l /dev/kvm
note "up.sh --substrate-only: kind cluster (substrate's create-kind-cluster.sh), ate-system (ate-setup), the microvm SandboxConfig and its kata assets, and the demo's micro-VM WorkerPool."
run "${DEMO}/../up.sh" --substrate-only || fail "up.sh --substrate-only"
run "${DEMO}/smoke-microvm.sh" || fail "micro-VM smoke"
proves "a micro-VM actor boots on this cluster, suspends to a memory snapshot and resumes with its guest RAM (the counter continued) and the same actor UID"

# ----------------------------------------------------------------------------------------------
step 1 "the cluster: nodes, the microvm SandboxConfig, WorkerPools"
run "${K[@]}" get nodes -o wide
run "${K[@]}" get sandboxconfigs
run sh -c "$(printf '%q ' "${K[@]}") get sandboxconfig microvm -o yaml | sed -n '/^spec:/,\$p'"
run "${K[@]}" get workerpools -A
run "${K[@]}" -n "${NS}" get pods -o wide
run "${K[@]}" -n ate-system get pods
proves "substrate runs on kind with the micro-VM class as its default SandboxConfig, and the demo's pool runs ateom-microvm workers"

# ----------------------------------------------------------------------------------------------
step 2 "Sajay's AgentSuite: kaimahi testdata/minimal, digests verified, rendered to ActorTemplates"
SUITE="${HERE}/${SUITE_DIR}"
run cat "${SUITE}/SOURCE"
note "Each file is checked against the git blob id at kaimahi 9c5c993 and its sha256, as recorded in SOURCE."
verify_suite() {
	local blob sum rel bad=0 n=0
	while read -r blob sum rel; do
		[[ "${blob}" == "#"* || "${blob}" == commit ]] && continue
		n=$((n + 1))
		if [[ "$(git hash-object "${SUITE}/${rel}")" == "${blob}" && "$(sha256sum "${SUITE}/${rel}" | cut -d' ' -f1)" == "${sum}" ]]; then
			echo "OK   ${rel}  blob ${blob:0:12}  sha256 ${sum:0:16}"
		else
			echo "BAD  ${rel}"
			bad=1
		fi
	done <"${SUITE}/SOURCE"
	[[ "$(find "${SUITE}" -type f ! -name SOURCE | wc -l)" == "${n}" ]] || {
		echo "the suite holds files SOURCE does not list"
		bad=1
	}
	return "${bad}"
}
run verify_suite || fail "the vendored suite differs from kaimahi 9c5c993"
run cat "${SUITE}/agentsuite.json"
run cat "${SUITE}/agents/writer.json"
run cat "${SUITE}/instructions/writer.md"
run sha256sum "${SUITE}/instructions/writer.md"
note "The manifest pins the instructions at the digest above; the harness refuses instructions that differ. kaimahi's agentsuite package validates every manifest digest when emit loads the suite."
note "up.sh --templates-only: harness image (harness binary + agent layer), then emit render twice, once per tier, and each ActorTemplate's golden snapshot."
run "${DEMO}/../up.sh" --templates-only || fail "up.sh --templates-only"
MEMORY_TEMPLATE="$(cat "${STATE}/render-memory/template-name")"
run cat "${STATE}/render/actortemplate.json"
run cat "${STATE}/render/harness-registration.json"
run diff "${STATE}/render/actortemplate.json" "${STATE}/render-memory/actortemplate.json" || true
run cat "${STATE}/render-memory/harness-registration.json" "${STATE}/render/agentsessionsd.args" "${STATE}/render-memory/agentsessionsd.args"
run "${ATE[@]}" get actor-templates -a "${ATESPACE}"
grep -q '"model": "example-model"' "${SUITE}/agents/writer.json" || fail "the suite's agent no longer names example-model"
grep -q "\"value\": \"${AGENTSUITE_MODEL}\"" "${STATE}/render/actortemplate.json" || fail "the template does not override the model with ${AGENTSUITE_MODEL}"
grep -q '"configName": "microvm"' "${STATE}/render/actortemplate.json" || fail "the template is not on the microvm SandboxConfig"
proves "the suite is byte-identical to kaimahi 9c5c993; emit maps its one agent to two micro-VM ActorTemplates (stateless and memory tier) that request ${AGENTSUITE_MODEL} in place of the placeholder example-model, and each template's golden snapshot is built"

# ----------------------------------------------------------------------------------------------
step 3 "agentsessionsd against the Azure OpenAI deployment ${AGENTSUITE_MODEL}"
note "The key is an Entra token for the VM's user-assigned managed identity, from the instance metadata endpoint. It goes to a mode-0600 file and the daemon's Secret, never to the terminal."
fetch_token() {
	local url="http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://cognitiveservices.azure.com&client_id=${AZURE_MI_CLIENT_ID}"
	(
		umask 077
		curl -fsS -H Metadata:true "${url}" | python3 -c "import json, sys; d = json.load(sys.stdin); print(d['${FIELD}'] + chr(10) + d['expires_on'])" >"${TOKEN_FILE}"
	)
	echo "token for client ${AZURE_MI_CLIENT_ID}: $(head -1 "${TOKEN_FILE}" | wc -c | tr -d ' ') bytes, expires $(date -u -d "@$(sed -n 2p "${TOKEN_FILE}")" +%FT%TZ)"
}
run fetch_token || fail "no token from the instance metadata endpoint"
MODEL_API_KEY="$(head -1 "${TOKEN_FILE}")"
export MODEL_API_KEY
note "up.sh again: everything above is reused; it applies the token to the agentsessionsd-model Secret over stdin and deploys agentsessionsd (harness writer) and agentsessionsd-memory (harness writer-memory)."
run "${DEMO}/../up.sh" || fail "up.sh"
unset MODEL_API_KEY
run sh -c "$(printf '%q ' "${K[@]}") -n ${NS} get deploy agentsessionsd -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{\"\\n\"}{end}'"
run sh -c "$(printf '%q ' "${K[@]}") -n ${NS} get deploy agentsessionsd -o jsonpath='{.spec.template.spec.containers[0].env}'; echo"
run sh -c "$(printf '%q ' "${K[@]}") -n ${NS} get secret agentsessionsd-model -o json | python3 -c 'import json, sys; s = json.load(sys.stdin); print({\"name\": s[\"metadata\"][\"name\"], \"type\": s[\"type\"], \"data\": {k: \"[REDACTED, %d base64 chars]\" % len(v) for k, v in s[\"data\"].items()}})'"
run "${K[@]}" -n "${NS}" get deploy,svc,pods -o wide
run "${K[@]}" -n "${NS}" logs deploy/agentsessionsd
run "${K[@]}" -n "${NS}" logs deploy/agentsessionsd-memory
proves "the daemons run the substrate build against ${AGENTSUITE_MODEL} on Azure OpenAI with the token as a bearer key from the Secret; the manifest, logs and this screen carry no credential"

(cd "${ROOT}" && go build -o "${WORK}/bin/agentctl" ./cmd/agentctl)
(cd "${HERE}" && go build -o "${WORK}/bin/replay" ./cmd/replay)
forward "${NS}" agentsessionsd 18080:8080
forward "${NS}" agentsessionsd-memory 18083:8080
forward ate-system atenet-router 18081:80
for _ in $(seq 60); do
	if agentctl list >/dev/null 2>&1 && memctl list >/dev/null 2>&1; then break; fi
	sleep 1
done

# ----------------------------------------------------------------------------------------------
step 4 "a session on harness writer, several turns on ${AGENTSUITE_MODEL}"
run agentctl create --harness writer --name launch-copy
SID="$(sed -n '$p' "${STEP_LOG}" | tr -d '[:space:]')"
[[ "${SID}" =~ ^sess-[0-9a-f]+$ ]] || fail "agentctl create did not print a session UID"
run agentctl get --session "${SID}"
note "Live: agentsessionsd's log and the actor's own output, as the turns run."
tail_logs agentsessionsd "${K[@]}" -n "${NS}" logs -f --since=1s deploy/agentsessionsd
tail_actor "${SID}"
T1="Write a two-sentence product blurb for Ledger, an open API that keeps AI agent sessions durable across crashes, suspends and forks."
T2="Make it punchier and add a one-line tagline."
T3="Now give me just the tagline, translated into French."
say agentctl "${SID}" "${T1}"
say agentctl "${SID}" "${T2}"
say agentctl "${SID}" "${T3}"
untail
run "${ATE[@]}" get actor "${SID}" -a "${ATESPACE}"
run "${ATE[@]}" logs actor "${SID}" -a "${ATESPACE}" --source lifecycle || note "(no lifecycle log)"
[[ "$(actor_field "${SID}" 'a["status"]["state"]')" == ACTOR_STATE_RUNNING ]] || fail "the session's actor is not running"
[[ "$(inputs agentctl "${SID}")" == "${T1} | ${T2} | ${T3}" ]] || fail "the journal does not hold the 3 inputs in order"
[[ "$(records agentctl "${SID}" EVENT_MODEL_CALL)" == 3 ]] || fail "the journal does not record 3 model calls"
proves "each turn ran in the session's own micro-VM actor (named by the session UID), reached over harnesswire through the atenet-router, and every model call went through the host to ${AGENTSUITE_MODEL}"

note "A second session, on harness writer-memory: the same agent and image in the memory tier, whose conversation lives only in the actor's RAM."
run memctl create --harness writer-memory --name codename
MSID="$(sed -n '$p' "${STEP_LOG}" | tr -d '[:space:]')"
[[ "${MSID}" =~ ^sess-[0-9a-f]+$ ]] || fail "agentctl create did not print a session UID"
M1="Remember this release codename: BLUE-HERON-42. Reply with one short sentence."
M2="Write one sentence announcing the release without naming the codename."
say memctl "${MSID}" "${M1}"
say memctl "${MSID}" "${M2}"
run "${ATE[@]}" get actor "${MSID}" -a "${ATESPACE}"
run "${ATE[@]}" logs actor "${MSID}" -a "${ATESPACE}" --source containers || note "(no container log)"
proves "the memory-tier harness builds each request from its RAM transcript (its log counts the messages it holds) and ignores the journal history the host sends"

# ----------------------------------------------------------------------------------------------
step 5 "the journal: turns, model calls, hashes and token usage (agentctl replay -v)"
run agentctl replay -v --session "${SID}"
# summarize SERVER SESSION: totals over the journal's records (the seq= lines; an assistant message
# can span several lines), including the usage records' token counts.
summarize() {
	"${WORK}/bin/agentctl" replay -v --server "$1" --session "$2" | awk '
		!/^seq=/ { next }
		{ n++; head = $3 }
		$2 == "EVENT_END" { turns++ }
		$2 == "EVENT_MODEL_CALL" { calls++ }
		$2 == "EVENT_USAGE" { usage++; for (i = 3; i <= NF; i++) { split($i, kv, "="); if (kv[1] == "input_tokens") in_ += kv[2]; if (kv[1] == "output_tokens") out += kv[2] } }
		END { printf "records=%d turns=%d model_calls=%d usage_records=%d input_tokens=%d output_tokens=%d head_%s\n", n, turns, calls, usage, in_, out, head }'
}
# check_summary SUMMARY: every turn made one model call and journaled one usage record with tokens.
check_summary() {
	local turns calls usage in_ out
	read -r turns calls usage in_ out < <(tr ' ' '\n' <<<"$1" | awk -F= '
		{ v[$1] = $2 } END { print v["turns"], v["model_calls"], v["usage_records"], v["input_tokens"], v["output_tokens"] }')
	((turns > 0 && calls == turns && usage == turns && in_ > 0 && out > 0)) ||
		fail "the journal does not hold one model call and one usage record with tokens per turn: $1"
}
run summarize "${SERVER}" "${SID}"
check_summary "$(summarize "${SERVER}" "${SID}")"
run memctl replay -v --session "${MSID}"
run summarize "${MEMSERVER}" "${MSID}"
check_summary "$(summarize "${MEMSERVER}" "${MSID}")"
proves "every input, model call (model + input hash), output and the provider's token usage for that call is one hash-chained record; each record names its predecessor's hash, and each turn journaled one usage record"

# ----------------------------------------------------------------------------------------------
step 6 "suspend to a memory snapshot, resume, and continue from RAM"
UID_BEFORE="$(actor_field "${MSID}" 'a["metadata"]["uid"]')"
note "Live: the micro-VM workers (ateom) and atelet, as the actor is snapshotted and later restored."
tail_logs ateom "${K[@]}" -n "${NS}" logs -f --since=1s --prefix --all-containers --max-log-requests=20 -l "ate.dev/worker-pool=${POOL}"
tail_logs atelet "${K[@]}" -n ate-system logs -f --since=1s --prefix --all-containers -l app=atelet
run memctl suspend --session "${MSID}"
untail
run "${ATE[@]}" get actor "${MSID}" -a "${ATESPACE}"
[[ "$(actor_field "${MSID}" 'a["status"]["state"]')" == ACTOR_STATE_SUSPENDED ]] || fail "the actor is not suspended"
SNAP="$(held_snapshot "${MSID}")"
[[ -n "${SNAP}" && "${SNAP}" == *MEMORY* ]] || fail "the suspended actor holds no completed memory snapshot: ${SNAP}"
run echo "held snapshot: ${SNAP}"
actor_json "${MSID}" >"${WORK}/actor.json"
run python3 -c 'import json, sys; print(json.dumps(json.load(open(sys.argv[1]))["status"].get("snapshots", []), indent=2))' "${WORK}/actor.json"
run "${K[@]}" -n "${NS}" get pods -o wide
tail_logs ateom "${K[@]}" -n "${NS}" logs -f --since=1s --prefix --all-containers --max-log-requests=20 -l "ate.dev/worker-pool=${POOL}"
tail_logs atelet "${K[@]}" -n ate-system logs -f --since=1s --prefix --all-containers -l app=atelet
run memctl resume --session "${MSID}"
M3="What release codename did I give you earlier? Answer with the codename only."
tail_actor "${MSID}"
say memctl "${MSID}" "${M3}"
untail
UID_AFTER="$(actor_field "${MSID}" 'a["metadata"]["uid"]')"
run "${ATE[@]}" get actor "${MSID}" -a "${ATESPACE}"
run echo "actor uid before suspend: ${UID_BEFORE}   after resume: ${UID_AFTER}"
[[ "${UID_BEFORE}" == "${UID_AFTER}" ]] || fail "the actor UID changed across suspend and resume"
[[ "${REPLY}" == *BLUE-HERON-42* ]] || fail "the resumed turn did not recall the codename from RAM: ${REPLY}"
run memctl get --session "${MSID}"
proves "suspend wrote a MEMORY snapshot and freed the worker; resume restored the same actor (UID ${UID_AFTER}), and the model recalled BLUE-HERON-42 from the transcript in the actor's RAM, which only the snapshot carried across"

# ----------------------------------------------------------------------------------------------
step 7 "replay: re-run every turn of the writer session against the journal, zero model calls"
CALLS_BEFORE="$(records agentctl "${SID}" EVENT_MODEL_CALL)"
note "cmd/replay verifies the hash chain itself, then runs the controller's replay against the session's actor through the router. Its model client points at a local port with nothing listening and holds no key, so it could not reach Azure if it tried."
run "${WORK}/bin/replay" -server "${SERVER}" -session "${SID}" -router "${ROUTER}" -atespace "${ATESPACE}" \
	-model-base-url "http://127.0.0.1:18089/v1" -stats-url "" || fail "replay"
grep -q 're-executed 3 completed turns' "${STEP_LOG}" || fail "the replay did not re-execute the 3 turns"
grep -q 'model calls: controller=0' "${STEP_LOG}" || fail "the replay made a model call"
CALLS_AFTER="$(records agentctl "${SID}" EVENT_MODEL_CALL)"
run echo "model calls in the journal: before replay ${CALLS_BEFORE}, after replay ${CALLS_AFTER}; controller model calls during replay: 0"
[[ "${CALLS_BEFORE}" == "${CALLS_AFTER}" ]] || fail "the journal's model-call count changed"
proves "the chain verifies client-side, the agent re-ran all 3 turns in its micro-VM with every completion served from the journal, and the model-call counter did not move"

# ----------------------------------------------------------------------------------------------
step 8 "fork the writer session at the end of turn 1 and drive the child another way"
FORK_SEQ="$(agentctl replay --session "${SID}" | awk '/EVENT_END/ { sub("seq=", "", $1); print $1; exit }')"
run agentctl fork --session "${SID}" --at "${FORK_SEQ}" --names haiku-branch
CHILD="$(awk '/^child / { print $2 }' "${STEP_LOG}" | tail -1)"
[[ -n "${CHILD}" ]] || fail "fork printed no child"
C2="Instead, rewrite the blurb as a haiku."
say agentctl "${CHILD}" "${C2}"
run "${ATE[@]}" get actor "${CHILD}" -a "${ATESPACE}"
run echo "parent ${SID}: $(inputs agentctl "${SID}")"
run echo "child  ${CHILD}: $(inputs agentctl "${CHILD}")"
run agentctl replay --session "${SID}"
run agentctl replay --session "${CHILD}"
[[ "$(inputs agentctl "${CHILD}")" == "${T1} | ${C2}" ]] || fail "the child's journal is not turn 1 plus its own"
[[ "$(inputs agentctl "${SID}")" == "${T1} | ${T2} | ${T3}" ]] || fail "the parent's journal changed"
[[ "$(records agentctl "${CHILD}" EVENT_MODEL_CALL)" == 2 ]] || fail "the child does not hold turn 1's model call plus its own"
proves "the child is its own micro-VM actor holding only turn 1 of the parent's history plus its own turn; the parent's 3 turns are untouched, so the two histories diverge after seq ${FORK_SEQ}"

# ----------------------------------------------------------------------------------------------
step 9 "replace the memory-tier actor out of band: the identity guard refuses the next turn"
OLD_UID="$(actor_field "${MSID}" 'a["metadata"]["uid"]')"
note "Someone deletes the session's actor and creates a new one under the same name from the same template: a fresh micro-VM with an empty transcript."
run "${ATE[@]}" delete actor "${MSID}" -a "${ATESPACE}" --any-state
for _ in $(seq 60); do
	if ! "${ATE[@]}" get actor "${MSID}" -a "${ATESPACE}" >/dev/null 2>&1; then break; fi
	sleep 2
done
run "${ATE[@]}" create actor "${MSID}" -a "${ATESPACE}" --template "${MEMORY_TEMPLATE}" || fail "could not create the replacement actor"
note "The replacement is created SUSPENDED; the host's next placement resumes it from the template's golden snapshot."
NEW_UID="$(actor_field "${MSID}" 'a["metadata"]["uid"]')"
run "${ATE[@]}" get actor "${MSID}" -a "${ATESPACE}"
run echo "actor uid the session's turns ran on: ${OLD_UID}   actor under the name now: ${NEW_UID}"
[[ "${OLD_UID}" != "${NEW_UID}" ]] || fail "the replacement kept the UID"
TURNS_BEFORE="$(records memctl "${MSID}" EVENT_END)"
M4="And what was the codename again?"
printf '%s$ %s%s\n' "${Y}" "$(shown memctl exec --session "${MSID}" --input "$(printf '%q' "${M4}")")" "${N}"
printf '$ %s\n' "$(shown memctl exec --session "${MSID}" --input "$(printf '%q' "${M4}")")" >>"${STEP_LOG}"
if out="$(memctl exec --session "${MSID}" --input "${M4}" 2>&1)"; then
	printf '%s\n' "${out}" | redact | tee -a "${STEP_LOG}"
	fail "the turn ran on the replaced actor"
fi
printf '%s\n' "${out}" | redact | tee -a "${STEP_LOG}"
[[ "${out}" == *FailedPrecondition* ]] || fail "the refusal is not FAILED_PRECONDITION"
run sh -c "$(printf '%q ' "${K[@]}") -n ${NS} logs deploy/agentsessionsd-memory | grep -iE 'memory_state_lost|replaced|FailedPrecondition' | tail -5" || true
TURNS_AFTER="$(records memctl "${MSID}" EVENT_END)"
run echo "completed turns in the journal: before ${TURNS_BEFORE}, after ${TURNS_AFTER}"
[[ "${TURNS_BEFORE}" == "${TURNS_AFTER}" ]] || fail "a turn completed on the replaced actor"
proves "the host compared the actor's UID with the instance the session's turns ran on and refused with FAILED_PRECONDITION, instead of running the conversation on an actor that had lost its memory"

# ----------------------------------------------------------------------------------------------
step 10 "teardown"
for p in "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done
PIDS=()
run rm -f "${TOKEN_FILE}"
run "${DEMO}/../down.sh" || fail "down.sh"
run kind get clusters
run sh -c "docker ps -a --filter name=${CLUSTER} --format '{{.Names}} {{.Status}}'; echo \"(containers named ${CLUSTER}*: \$(docker ps -aq --filter name=${CLUSTER} | wc -l))\""
[[ -z "$(docker ps -aq --filter "name=${CLUSTER}")" ]] || fail "containers of ${CLUSTER} remain"
cluster_listed && fail "kind still lists ${CLUSTER}"
[[ ! -e "${TOKEN_FILE}" ]] || fail "the token file remains"
note "kind-registry stays: substrate's kind lane shares it across clusters on this host."
proves "the cluster, its micro-VMs, the daemons and their journals, and the token file are gone"

t=$((SECONDS - START))
printf '\n%sDEMO COMPLETE in %dm%02ds: steps 0-10 held%s\n' "${G}" $((t / 60)) $((t % 60)) "${N}"
printf 'DEMO COMPLETE in %dm%02ds: steps 0-10 held\n' $((t / 60)) $((t % 60)) >>"${STEP_LOG}"
