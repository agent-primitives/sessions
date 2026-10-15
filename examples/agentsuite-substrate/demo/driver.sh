#!/usr/bin/env bash
# The recorded demo: kaimahi's minimal AgentSuite (agent `writer`) as an AgentSessions harness on
# agent-substrate micro-VMs, driven by a real model on Azure OpenAI.
#
#   driver.sh setup      off camera: the kind cluster, substrate, the micro-VM smoke test (must pass),
#                        the harness image and ActorTemplates, the model token and both agentsessionsd
#                        Deployments, and the agentctl and replay binaries; ends with SETUP OK
#   driver.sh show       on camera: eight short chapters, each a caption and the few lines that prove
#                        it, while the full output of every command goes to the chapter's log
#   driver.sh teardown   down.sh
#
# show checks every claim it prints and stops on the first that does not hold. Every wait longer than
# a moment runs out of frame: the driver prints a "⠿ <what> …" line, runs the step, then replaces
# that line with "✓ <what> in N s"; demo.tape hides the recording between the two, so the video cuts
# the wait and the line says how long it took. demo.tape lists those waits in order, so a new offcam
# call needs a matching pair of waits in the tape (scripts_test.go checks the counts match).
#
# Output protocol: show prints plain lines, plus lines starting with "@@R " that replace the line
# before them. present.awk renders them, and holds each line without its newline until the next one
# arrives, so vhs's Wait+Line sees the newest line (Docker's log stream, which hq remote-test relays,
# only passes whole lines). On a terminal the driver applies present.awk itself.
#
# It runs where Docker, kind and the cluster run (the recording runs it on a Linux host with KVM
# through `hq remote-test`, see record.sh), with the tools Dockerfile.tools installs. Settings:
#
#   AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL, AGENTSUITE_MODEL_PATH  the Azure OpenAI deployment
#   AZURE_MI_CLIENT_ID     the user-assigned managed identity whose token is the model API key; setup
#                          fetches it from the instance metadata endpoint into a mode-0600 file, hands
#                          it to up.sh (the daemon's Secret) and deletes the file; only its expiry is
#                          kept, and nothing prints the token
#   DEMO_LOGS              directory for the logs (default ${WORK}/demo-logs)
#   DEMO_KEEP_ON_FAILURE   set to keep the cluster when setup fails (by default down.sh deletes it)
#   DEMO_READ              seconds a result stays on screen before the next command (default 7)
#   DEMO_COLS              the terminal's width in columns (default 112: demo.tape is 113 wide)
#
# Every line the script prints, on the terminal and in the logs, goes through redact first.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
export AGENTSUITE_DEMO_SUITE=suites/kaimahi-minimal AGENTSUITE_DEMO_AGENT=writer AGENTSUITE_DEMO_SANDBOX=microvm
source "${DEMO}/../common.sh"

MODE="${1:-}"
case "${MODE}" in
setup | show | teardown) ;;
*)
	echo "usage: $0 setup|show|teardown" >&2
	exit 2
	;;
esac
LOGS="${DEMO_LOGS:-${WORK}/demo-logs}"
READ="${DEMO_READ:-7}"
COLS="${DEMO_COLS:-112}"
[[ "${READ}" =~ ^[0-9]+$ && "${COLS}" =~ ^[0-9]+$ ]] || {
	echo "DEMO_READ and DEMO_COLS are whole numbers" >&2
	exit 2
}
mkdir -p "${LOGS}"
# The display names of the daemons' addresses in the commands show prints.
SERVER=127.0.0.1:18080 MEMSERVER=127.0.0.1:18083 ROUTER=127.0.0.1:18081
TOKEN_FILE="${WORK}/model-token"
EXPIRY_FILE="${WORK}/model-token-expiry"

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
if [[ "${MODE}" == show && -t 1 ]]; then
	exec > >(redact | awk -f "${DEMO}/present.awk") 2>&1
else
	exec > >(redact) 2>&1
fi
REDACT_PID=$!

STEP_LOG="${LOGS}/${MODE}.log"
: >"${STEP_LOG}"
log() { printf '%s\n' "$*" | redact >>"${STEP_LOG}"; }
fail() {
	FAILED=1
	printf 'DEMO FAILED: %s\n' "$*"
	log "DEMO FAILED: $*"
	exit 1
}

PIDS=()
cleanup() {
	local rc=$?
	if ((rc != 0)) && [[ -z "${FAILED:-}" ]]; then
		printf 'DEMO FAILED: exit %d, see %s\n' "${rc}" "${STEP_LOG##*/}"
		log "DEMO FAILED: exit ${rc}"
	fi
	for p in "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done
	rm -f "${TOKEN_FILE}"
	# A failed setup deletes the cluster it created (down.sh refuses one it did not), so a run on a
	# shared host leaves nothing behind; a failed show keeps it, so show can be run again.
	if [[ "${MODE}" == setup ]] && ((rc != 0)) && [[ -z "${DEMO_KEEP_ON_FAILURE:-}" ]]; then
		printf '# failed setup: down.sh\n'
		"${DEMO}/../down.sh" 2>&1 | redact | tee -a "${STEP_LOG}" || true
	fi
	# Let the redacting filter flush the last lines, for at most 10 seconds: a process that inherited
	# stdout and outlived its step must not keep the job, and the recording, from ending.
	exec 1>&- 2>&-
	for _ in $(seq 100); do
		kill -0 "${REDACT_PID}" 2>/dev/null || break
		sleep 0.1
	done
}
trap cleanup EXIT

ctl() { "${WORK}/bin/agentctl" "$1" --server "$2" "${@:3}"; }
agentctl() { ctl "$1" "${SERVER}" "${@:2}"; }
memctl() { ctl "$1" "${MEMSERVER}" "${@:2}"; }
actor_json() { "${ATE[@]}" get actor "$1" -a "${ATESPACE}" -o json; }
records() { # ctl session kind -> how many records of that kind its journal holds
	"$1" replay --session "$2" | awk -v k="$3" '$2 == k { n++ } END { print n + 0 }'
}

# ================================================================================================
# setup: everything the recording does not show. The full output is setup.log.
# ================================================================================================
setup() {
	local v
	for v in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AZURE_MI_CLIENT_ID; do
		[[ -n "${!v:-}" ]] || fail "${v} is not set"
	done
	# phase NAME CMD...: one setup phase, its output in setup.log, a one-line status on stdout.
	phase() {
		local name="$1" t0=${SECONDS} rc=0
		shift
		printf '==> %s\n' "${name}"
		log "" "==> ${name}  ($(date -u +%FT%TZ))"
		"$@" 2>&1 | redact >>"${STEP_LOG}" || rc=$?
		((rc == 0)) || {
			tail -30 "${STEP_LOG}"
			fail "${name} (exit ${rc})"
		}
		printf '    ok in %d s\n' $((SECONDS - t0))
	}
	phase "KVM in the Docker host" docker run --rm --device /dev/kvm busybox ls -l /dev/kvm
	phase "up.sh --substrate-only: kind, ate-system, the microvm SandboxConfig and WorkerPool" "${DEMO}/../up.sh" --substrate-only
	phase "micro-VM smoke: an actor boots, suspends to a memory snapshot and resumes with its RAM" "${DEMO}/smoke-microvm.sh"
	grep -q '^SMOKE OK' "${STEP_LOG}" || fail "the smoke test did not print SMOKE OK"
	grep '^SMOKE OK' "${STEP_LOG}" | tail -1
	phase "the vendored suite against kaimahi 9c5c993" verify_suite
	phase "up.sh --templates-only: harness image and both ActorTemplates with their golden snapshots" "${DEMO}/../up.sh" --templates-only
	template_check "${HERE}/${SUITE_DIR}/agents/writer.json" 't.get("model", {}).get("model") == "example-model"' || fail "the suite's agent no longer names example-model"
	template_check "${STATE}/render/actortemplate.json" 'any(isinstance(v, dict) and v.get("name") == "AGENTSUITE_MODEL" and v.get("value") == model for v in walk(t))' ||
		fail "the template does not override the model with ${AGENTSUITE_MODEL}"
	template_check "${STATE}/render/actortemplate.json" 't.get("sandboxConfig", {}).get("configName") == "microvm"' || fail "the template is not on the microvm SandboxConfig"
	phase "a model token for the VM's managed identity (never printed)" fetch_token
	MODEL_API_KEY="$(head -1 "${TOKEN_FILE}")"
	export MODEL_API_KEY
	phase "up.sh: the model Secret and both agentsessionsd Deployments" "${DEMO}/../up.sh"
	unset MODEL_API_KEY
	rm -f "${TOKEN_FILE}"
	phase "agentctl and replay binaries" build_tools
	# The object store client show uses to size a snapshot, pulled now so show does not wait on it.
	phase "pull the S3 client image" docker pull -q "${AWS_CLI_IMAGE}"
	forwards
	phase "both Sessions APIs answer" wait_apis
	printf 'SETUP OK in %d s\n' "${SECONDS}"
	log "SETUP OK in ${SECONDS} s"
}
verify_suite() {
	local suite="${HERE}/${SUITE_DIR}" blob sum rel bad=0 n=0
	while read -r blob sum rel; do
		[[ "${blob}" == "#"* || "${blob}" == commit ]] && continue
		n=$((n + 1))
		if [[ "$(git hash-object "${suite}/${rel}")" == "${blob}" && "$(sha256sum "${suite}/${rel}" | cut -d' ' -f1)" == "${sum}" ]]; then
			echo "OK   ${rel}  blob ${blob:0:12}  sha256 ${sum:0:16}"
		else
			echo "BAD  ${rel}"
			bad=1
		fi
	done <"${suite}/SOURCE"
	[[ "$(find "${suite}" -type f ! -name SOURCE | wc -l)" == "${n}" ]] || {
		echo "the suite holds files SOURCE does not list"
		bad=1
	}
	return "${bad}"
}
# template_check FILE EXPR: EXPR over the parsed JSON t (protojson's whitespace is not stable).
template_check() {
	python3 -c '
import json, sys
def walk(v):
    yield v
    for c in (v.values() if isinstance(v, dict) else v if isinstance(v, list) else []):
        yield from walk(c)
t = json.load(open(sys.argv[1])); model = sys.argv[3]
sys.exit(0 if eval(sys.argv[2]) else 1)' "$1" "$2" "${AGENTSUITE_MODEL}"
}
fetch_token() {
	local url="http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://cognitiveservices.azure.com&client_id=${AZURE_MI_CLIENT_ID}"
	(
		umask 077
		curl -fsS -H Metadata:true "${url}" | python3 -c "import json, sys; d = json.load(sys.stdin); print(d['${FIELD}'] + chr(10) + d['expires_on'])" >"${TOKEN_FILE}"
	)
	sed -n 2p "${TOKEN_FILE}" >"${EXPIRY_FILE}"
	echo "token: $(head -1 "${TOKEN_FILE}" | wc -c | tr -d ' ') bytes, expires $(date -u -d "@$(cat "${EXPIRY_FILE}")" +%FT%TZ)"
}
build_tools() {
	(cd "${ROOT}" && go build -o "${WORK}/bin/agentctl" ./cmd/agentctl)
	(cd "${HERE}" && go build -o "${WORK}/bin/replay" ./cmd/replay)
}
# Keep in sync with the image substrate's kind install uses for its bucket (manifests/ate-install/kind/rustfs.yaml).
AWS_CLI_IMAGE="amazon/aws-cli:2.17.0@sha256:643507c10ada7964ca6157b3d799f030b90577643da9955d319a77399ed80d73"
forwards() {
	local spec
	for spec in "${NS} agentsessionsd 18080:8080" "${NS} agentsessionsd-memory 18083:8080" "ate-system atenet-router 18081:80"; do
		# shellcheck disable=SC2086 # three words
		set -- ${spec}
		"${K[@]}" -n "$1" port-forward "svc/$2" "$3" >/dev/null 2>&1 &
		PIDS+=($!)
	done
}
wait_apis() {
	for _ in $(seq 60); do
		if agentctl list >/dev/null 2>&1 && memctl list >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	echo "the Sessions APIs did not answer through the port-forwards"
	return 1
}

# ================================================================================================
# show: the recording. Each chapter clears the screen, draws a progress bar (chapter k of 8 fills
# k/8 of the top row; record.sh finds the chapter starts in the video from it), a title and a
# caption, then runs its commands. The full output of each command goes to that chapter's log.
# ================================================================================================
C=$'\e[1;36m' Y=$'\e[1;33m' G=$'\e[32m' W=$'\e[1;37m' D=$'\e[2m' N=$'\e[0m'
CHAPTERS=8
chapter() { # n title caption
	STEP_LOG="${LOGS}/$(printf 'ch-%d' "$1").log"
	printf '%s\t%s\t%s\n' "$1" "$(date -u +%s)" "$2" >>"${LOGS}/timeline.tsv"
	printf '# CHAPTER %s  %s  (%s)\n# %s\n' "$1" "$2" "$(date -u +%FT%TZ)" "$3" >"${STEP_LOG}"
	local fill=$((COLS * $1 / CHAPTERS))
	printf '\e[2J\e[3J\e[H\e[48;2;0;175;215m%*s\e[48;2;58;58;58m%*s\e[0m\n' "${fill}" '' $((COLS - fill)) ''
	printf '\n%s %d/%d  %s%s\n' "${C}" "$1" "${CHAPTERS}" "$2" "${N}"
	wrap "$3" "" | sed "s/^/${W}/; s/\$/${N}/"
	printf '\n'
	sleep $((READ + 4))
}
# wrap TEXT [PREFIX [INDENT]]: TEXT folded to the screen at word boundaries, each line after the
# first indented by INDENT spaces (default the width of PREFIX).
wrap() {
	python3 -c '
import sys, textwrap
text, prefix, cols = sys.argv[1], sys.argv[2], int(sys.argv[3])
indent = " " * (int(sys.argv[4]) if sys.argv[4] else len(prefix))
lines = []
for para in text.split("\n"):
    lines += textwrap.wrap(para, cols - len(prefix) - 2, break_on_hyphens=False) or [""]
print("\n".join((prefix if i == 0 else indent) + l for i, l in enumerate(lines)))' "$1" "${2:-}" "${COLS}" "${3:-}"
}
# cmd TEXT: the command the next lines run, as typed.
cmd() {
	wrap "$1" "\$ " 6 | sed "s/^/${Y}/; s/\$/${N}/"
	log "\$ $1"
	sleep 1.5
}
# say LINE...: result lines, indented.
say() {
	local l text=""
	for l in "$@"; do text+="$(wrap "${l}" "  ")"$'\n'; done
	printf '%s' "${text}"
	hold "${text}"
}
# hold TEXT: pause for READ seconds, plus one for each line of TEXT after the first.
hold() { sleep $((READ + $(printf '%s' "$1" | grep -c '') - 1)); }
# offcam WHAT DONE FN ARGS...: run FN out of frame (demo.tape hides the wait) with its output in OUT
# and the chapter log, then print how long it took. FN's exit status is returned.
offcam() {
	local what="$1" done="$2" t0=${SECONDS} rc=0
	shift 2
	printf '  %s⠿ %s …%s\n' "${D}" "${what}" "${N}"
	sleep 1
	OUT="$("$@" 2>&1)" || rc=$?
	printf '%s\n' "${OUT}" | redact >>"${STEP_LOG}"
	log "(exit ${rc} after $((SECONDS - t0)) s)"
	# The tape shows the first half second of the wait line and cuts the rest.
	HIDDEN=$((HIDDEN + SECONDS - t0))
	printf '@@R   %s✓ %s in %d s%s\n' "${G}" "${done}" $((SECONDS - t0)) "${N}"
	sleep 2
	return "${rc}"
}
# answer: the assistant text of the exec output in OUT (the streamed lines between the model call
# and the output record), wrapped under "agent │".
answer() {
	REPLY="$(awk '/^seq=[0-9]+ +EVENT_MODEL_CALL/ { on = 1; next } /^seq=/ { on = 0 } on' <<<"${OUT}" | sed '/^[[:space:]]*$/d')"
	[[ -n "${REPLY}" ]] || fail "the turn produced no assistant output"
	local text
	text="$(wrap "${REPLY}" "agent │ " | head -6)"
	printf '%s\n' "${text}" | sed "s/^/  /; s/agent │/${G}agent │${N}/"
	hold "${text}"
	sleep 1
}
exec_turn() { # ctl session input
	"$1" exec --session "$2" --input "$3"
}
quote() { printf '"%s"' "$1"; }
mib() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1048576 }'; }
short() { printf '%.13s…' "$1"; }

show() {
	[[ -x "${WORK}/bin/agentctl" && -x "${WORK}/bin/replay" && -s "${EXPIRY_FILE}" ]] || fail "run driver.sh setup first"
	rm -f "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv"
	forwards
	wait_apis >/dev/null || fail "the Sessions APIs did not answer"
	local template memtemplate
	SHOW_START=${SECONDS} HIDDEN=0
	template="$(cat "${STATE}/render/template-name")"
	memtemplate="$(cat "${STATE}/render-memory/template-name")"

	# ---------------------------------------------------------------------------------------------
	# Hide the cursor: the terminal only shows what the driver prints.
	printf '\e[?25l\e[2J\e[3J\e[H\n'
	printf '%s  Durable agent sessions on micro-VMs%s\n\n' "${C}" "${N}"
	wrap "An agent defined with AgentSuite runs as AgentSessions sessions, each in its own agent-substrate micro-VM on Kubernetes, talking to a real model." "  " | sed "s/^/${W}/; s/\$/${N}/"
	printf '\n'
	wrap "Everything runs live on a kind cluster on an Azure VM with KVM. Waits longer than a moment are cut from the video; each one says how long it took." "  "
	sleep $((READ + 5))

	# ---------------------------------------------------------------------------------------------
	chapter 1 "What's running" "One agent definition, one model and one kind of sandbox. Everything that follows uses exactly these."
	local files expires_min sandbox
	cmd "verify_suite   # each file's git blob id and sha256 against kaimahi commit 9c5c993"
	verify_suite >"${WORK}/verify.out" 2>&1 || fail "the vendored suite differs from kaimahi 9c5c993"
	log "$(cat "${WORK}/verify.out")"
	files="$(grep -c '^OK' "${WORK}/verify.out")"
	say "suite    kaimahi testdata/minimal, agent \"writer\": ${files} files, all match upstream"
	cmd "kubectl -n ${NS} get deploy agentsessionsd -o json | jq -r '.spec.template.spec.containers[0].args[]' | grep model="
	"${K[@]}" -n "${NS}" get deploy agentsessionsd -o json >"${WORK}/deploy.json"
	jq -r '.spec.template.spec.containers[0].args[]' "${WORK}/deploy.json" | redact >>"${STEP_LOG}"
	jq -e --arg m "-model=${AGENTSUITE_MODEL}" '.spec.template.spec.containers[0].args | index($m)' "${WORK}/deploy.json" >/dev/null ||
		fail "agentsessionsd does not run model ${AGENTSUITE_MODEL}"
	expires_min=$((($(cat "${EXPIRY_FILE}") - $(date +%s)) / 60))
	((expires_min > 10)) || fail "the model token expires in ${expires_min} min"
	say "model    ${AGENTSUITE_MODEL} on Azure OpenAI; token from the VM's managed identity: acquired, expires in ${expires_min} min"
	cmd "kubectl ate get actor-template ${template} -o json | jq -c .sandboxConfig"
	"${ATE[@]}" get actor-template "${template}" -a "${ATESPACE}" -o json >"${WORK}/template.json"
	log "$(cat "${WORK}/template.json")"
	sandbox="$(jq -r '"\(.sandboxConfig.configName) \(.sandboxConfig.sandboxClass)"' "${WORK}/template.json")"
	[[ "${sandbox}" == "microvm SANDBOX_CLASS_MICROVM" ]] || fail "ActorTemplate ${template} is on sandbox '${sandbox}', want microvm SANDBOX_CLASS_MICROVM"
	say "sandbox  ActorTemplate ${template}, rendered from the suite: SandboxConfig microvm, class SANDBOX_CLASS_MICROVM (Kata + Cloud Hypervisor micro-VMs)"

	# ---------------------------------------------------------------------------------------------
	chapter 2 "Chat with the agent" "Start a session and talk to the agent. The session gets its own micro-VM, and every model call goes through AgentSessions, which records it."
	local copy t1 t2
	cmd "COPY=\$(agentctl create --server \$API --harness writer --name launch-copy)"
	copy="$(agentctl create --harness writer --name launch-copy | tail -1 | tr -d '[:space:]')"
	log "${copy}"
	[[ "${copy}" =~ ^sess-[0-9a-f]+$ ]] || fail "agentctl create did not print a session UID"
	say "session launch-copy: ${copy}"
	t1="Write a two-sentence product blurb for Ledger, an open API that keeps AI agent sessions durable across crashes, suspends and forks."
	t2="Make it punchier and add a one-line tagline."
	cmd "agentctl exec --server \$API --session \$COPY --input $(quote "${t1}")"
	offcam "starting the session's micro-VM and asking ${AGENTSUITE_MODEL}" "answered" exec_turn agentctl "${copy}" "${t1}" || fail "turn 1 failed"
	answer
	cmd "agentctl exec --server \$API --session \$COPY --input $(quote "${t2}")"
	offcam "asking ${AGENTSUITE_MODEL}" "answered" exec_turn agentctl "${copy}" "${t2}" || fail "turn 2 failed"
	answer
	cmd "kubectl ate get actor \$COPY -o json | jq -r '.status.state, .status.workerAssignment.workerPod'"
	actor_json "${copy}" >"${WORK}/actor.json"
	log "$(cat "${WORK}/actor.json")"
	[[ "$(jq -r .status.state "${WORK}/actor.json")" == ACTOR_STATE_RUNNING ]] || fail "the session's actor is not running"
	say "actor ${copy} (named after the session): RUNNING on worker $(jq -r .status.workerAssignment.workerPod "${WORK}/actor.json"), from template $(jq -r .actorTemplate.name "${WORK}/actor.json")"

	# ---------------------------------------------------------------------------------------------
	chapter 3 "The journal" "Every input, model call, answer and token count is one record in an append-only journal. Each record carries the hash of the one before it, so the history cannot be edited quietly."
	cmd "agentctl replay -v --server \$API --session \$COPY | summarize"
	agentctl replay -v --session "${copy}" >"${WORK}/journal.out"
	log "$(cat "${WORK}/journal.out")"
	local sum turns calls usage tin tout nrec head
	sum="$(summarize <"${WORK}/journal.out")"
	read -r nrec turns calls usage tin tout head <<<"${sum}"
	((turns == 2 && calls == 2 && usage == 2 && tin > 0 && tout > 0)) ||
		fail "the journal does not hold 2 turns with one model call and one usage record each: ${sum}"
	say "${turns} turns, ${calls} model calls (${AGENTSUITE_MODEL}), ${tin} tokens in / ${tout} out, ${nrec} records, chain head ${head:0:16}"
	cmd "agentctl replay -v --server \$API --session \$COPY | grep ^seq= | tail -4   # turn 2's last records"
	grep '^seq=' "${WORK}/journal.out" | grep -E 'EVENT_(MODEL_CALL|OUTPUT|USAGE|END) ' | tail -4 |
		python3 -c '
import re, sys
for l in sys.stdin:
    f = l.split()
    kv = dict(x.split("=", 1) for x in f if "=" in x and not x.startswith("seq="))
    kind = f[1].replace("EVENT_", "")
    text = l.split("assistant: ", 1)[1].strip().replace("**", "") if "assistant: " in l else ""
    extra = {"MODEL_CALL": "model=%s" % kv.get("model", ""), "OUTPUT": "\"%s\"" % (text[:32] + "…" if len(text) > 32 else text),
             "USAGE": "input_tokens=%s output_tokens=%s" % (kv.get("input_tokens", ""), kv.get("output_tokens", ""))}.get(kind, "")
    print("  %-8s %-11s hash=%s  prev=%s  %s" % (f[0], kind, kv.get("hash", "")[:12], kv.get("prev", "")[:12], extra))'
	sleep $((READ + 2))

	# ---------------------------------------------------------------------------------------------
	chapter 4 "Pause and resume, memory included" "This copy of the agent keeps the conversation only in its memory. We snapshot the micro-VM's memory and free its machine, resume it, and the conversation picks up where it left off."
	local mem m1 m3 uid_before uid_after snap size
	cmd "MEM=\$(agentctl create --server \$API_MEM --harness writer-memory --name codename)"
	mem="$(memctl create --harness writer-memory --name codename | tail -1 | tr -d '[:space:]')"
	log "${mem}"
	[[ "${mem}" =~ ^sess-[0-9a-f]+$ ]] || fail "agentctl create did not print a session UID"
	m1="Remember this release codename: BLUE-HERON-42. Reply with one short sentence."
	cmd "agentctl exec --server \$API_MEM --session \$MEM --input $(quote "${m1}")"
	offcam "starting the session's micro-VM and asking ${AGENTSUITE_MODEL}" "answered" exec_turn memctl "${mem}" "${m1}" || fail "the memory session's first turn failed"
	answer
	uid_before="$(actor_json "${mem}" | jq -r .metadata.uid)"
	cmd "agentctl suspend --server \$API_MEM --session \$MEM"
	offcam "snapshotting the micro-VM's memory and freeing its machine" "suspended" memctl suspend --session "${mem}" || fail "suspend failed"
	actor_json "${mem}" >"${WORK}/actor.json"
	log "$(cat "${WORK}/actor.json")"
	[[ "$(jq -r .status.state "${WORK}/actor.json")" == ACTOR_STATE_SUSPENDED ]] || fail "the actor is not suspended"
	snap="$(jq -r '[.status.snapshots[]? | select(.durableSnapshot.status == "SNAPSHOT_STORAGE_STATUS_COMPLETED")] | max_by(.generation) // empty | "\(.fidelity) \(.durableSnapshot.object.snapshotUri)"' "${WORK}/actor.json")"
	[[ "${snap}" == SNAPSHOT_FIDELITY_MEMORY\ * ]] || fail "the suspended actor holds no completed memory snapshot: ${snap}"
	cmd "aws s3 ls --recursive --summarize s3://<the actor's snapshotUri>/"
	offcam "measuring the snapshot" "measured" snapshot_size "${snap#* }" || fail "could not size the snapshot"
	local objects mem_bytes
	size="$(sed -n 's/^ *Total Size: *\([0-9][0-9]*\).*/\1/p' <<<"${OUT}" | tail -1)"
	objects="$(sed -n 's/^ *Total Objects: *\([0-9][0-9]*\).*/\1/p' <<<"${OUT}" | tail -1)"
	mem_bytes="$(awk '$4 ~ /\/memory-ranges[^/]*$/ { print $3 }' <<<"${OUT}" | tail -1)"
	if [[ ! "${size}" =~ ^[0-9]+$ ]] || ((size == 0)) || [[ ! "${mem_bytes}" =~ ^[0-9]+$ ]]; then fail "the snapshot has no memory object: ${OUT}"; fi
	say "snapshot: MEMORY, ${objects} objects, $(mib "${size}") MiB in the cluster's object store; $(mib "${mem_bytes}") MiB of it is memory-ranges.zstd, the guest's memory, compressed" \
		"actor: SUSPENDED, no worker assigned"
	cmd "agentctl resume --server \$API_MEM --session \$MEM"
	offcam "restoring the micro-VM from its snapshot" "resumed" memctl resume --session "${mem}" || fail "resume failed"
	uid_after="$(actor_json "${mem}" | jq -r .metadata.uid)"
	[[ -n "${uid_before}" && "${uid_before}" == "${uid_after}" ]] || fail "the actor UID changed across suspend and resume"
	say "actor uid ${uid_after} before the suspend and after the resume: the same micro-VM"
	m3="What release codename did I give you earlier? Answer with the codename only."
	cmd "agentctl exec --server \$API_MEM --session \$MEM --input $(quote "${m3}")"
	offcam "asking ${AGENTSUITE_MODEL}" "answered" exec_turn memctl "${mem}" "${m3}" || fail "the turn after resume failed"
	answer
	[[ "${REPLY}" == *BLUE-HERON-42* ]] || fail "the resumed turn did not recall the codename: ${REPLY}"
	cmd "kubectl ate logs actor \$MEM --source containers | grep 'memory tier' | tail -1"
	local memline
	memline="$("${ATE[@]}" logs actor "${mem}" -a "${ATESPACE}" --source containers 2>&1 | redact | tee -a "${STEP_LOG}" | grep -o 'memory tier: turn [0-9]* of this process; [^"]*' | tail -1)"
	[[ "${memline}" == "memory tier: turn 2 of this process; 2 messages in RAM"* ]] || fail "the harness did not answer from the RAM it held before the suspend: ${memline}"
	say "harness: ${memline}" "(turn 2 of the same process: the answer came from memory the snapshot carried, not from the journal)"

	# ---------------------------------------------------------------------------------------------
	chapter 5 "Replay without the model" "Replay re-runs the agent's turns in its micro-VM and answers every model call from the journal. The model is never called, so a session can be re-checked for free and with the same result."
	local before after
	before="$(records agentctl "${copy}" EVENT_MODEL_CALL)"
	cmd "replay -session \$COPY -model-base-url http://127.0.0.1:18089/v1  # nothing listens there, no key"
	offcam "re-running the session's turns in its micro-VM" "replayed" "${WORK}/bin/replay" -server "${SERVER}" -session "${copy}" -router "${ROUTER}" \
		-atespace "${ATESPACE}" -model-base-url "http://127.0.0.1:18089/v1" -stats-url "" || fail "replay failed"
	grep -q '^re-executed 2 completed turns' <<<"${OUT}" || fail "the replay did not re-execute the 2 turns"
	grep -q '^model calls: controller=0$' <<<"${OUT}" || fail "the replay made a model call"
	after="$(records agentctl "${copy}" EVENT_MODEL_CALL)"
	[[ "${before}" == "${after}" ]] || fail "the journal's model-call count changed"
	local verified
	verified="$(grep '^journal: ' <<<"${OUT}" | sed 's/ (head.*//')"
	say "${verified}" "re-executed 2 completed turns" "model calls: 0   (the journal still records ${after}, as before the replay)"

	# ---------------------------------------------------------------------------------------------
	chapter 6 "Fork" "Fork copies a session's history up to any point into a new session with its own micro-VM. Both go on independently: here, two different second turns."
	local fork_seq child c2
	fork_seq="$(agentctl replay --session "${copy}" | awk '/EVENT_END/ { sub("seq=", "", $1); print $1; exit }')"
	cmd "CHILD=\$(agentctl fork --server \$API --session \$COPY --at ${fork_seq} --names haiku-branch)   # seq ${fork_seq} = end of turn 1"
	offcam "copying the history and starting the child's micro-VM" "forked" agentctl fork --session "${copy}" --at "${fork_seq}" --names haiku-branch || fail "fork failed"
	child="$(awk '/^child / { print $2 }' <<<"${OUT}" | tail -1)"
	[[ "${child}" =~ ^sess-[0-9a-f]+$ ]] || fail "fork printed no child"
	c2="Instead, rewrite the blurb as a haiku."
	cmd "agentctl exec --server \$API --session \$CHILD --input $(quote "${c2}")"
	offcam "asking ${AGENTSUITE_MODEL} in the child" "answered" exec_turn agentctl "${child}" "${c2}" || fail "the child's turn failed"
	grep -q '^seq=[0-9]* *EVENT_OUTPUT  assistant: .' <<<"${OUT}" || fail "the child's turn produced no answer"
	cmd "agentctl replay --server \$API --session \$COPY; … --session \$CHILD   # side by side"
	agentctl replay --session "${copy}" >"${WORK}/parent.out"
	agentctl replay --session "${child}" >"${WORK}/child.out"
	log "parent:" "$(cat "${WORK}/parent.out")" "child:" "$(cat "${WORK}/child.out")"
	[[ "$(records agentctl "${copy}" EVENT_END)" == 2 && "$(records agentctl "${child}" EVENT_END)" == 2 ]] || fail "parent and child do not hold 2 turns each"
	[[ "$(sed -n 's/^seq=[0-9]* *EVENT_INPUT  user: //p' "${WORK}/child.out" | head -1)" == "${t1}" ]] || fail "the child does not start with the parent's turn 1"
	[[ "$(sed -n 's/^seq=[0-9]* *EVENT_INPUT  user: //p' "${WORK}/child.out" | tail -1)" == "${c2}" ]] || fail "the child's second turn is not its own"
	side_by_side "parent launch-copy" "${WORK}/parent.out" "child haiku-branch" "${WORK}/child.out"
	sleep $((READ + 8))

	# ---------------------------------------------------------------------------------------------
	chapter 7 "Guarding memory" "If the micro-VM holding a session's memory is swapped behind AgentSessions' back, the next turn is refused instead of answered from an empty memory."
	local new_uid turns_before m4 refusal
	cmd "kubectl ate delete actor \$MEM --any-state; kubectl ate create actor \$MEM --template ${memtemplate}"
	offcam "replacing the session's micro-VM out of band" "replaced" replace_actor "${mem}" "${memtemplate}" || fail "could not replace the actor"
	new_uid="$(actor_json "${mem}" | jq -r .metadata.uid)"
	[[ -n "${new_uid}" && "${new_uid}" != "${uid_after}" ]] || fail "the replacement kept the UID"
	say "actor uid the session's turns ran on ${uid_after}, actor under that name now ${new_uid}"
	turns_before="$(records memctl "${mem}" EVENT_END)"
	m4="And what was the codename again?"
	cmd "agentctl exec --server \$API_MEM --session \$MEM --input $(quote "${m4}")"
	if offcam "asking the session to continue" "refused" exec_turn memctl "${mem}" "${m4}"; then
		fail "the turn ran on the replaced actor"
	fi
	[[ "${OUT}" == *"code = FailedPrecondition"* ]] || fail "the refusal is not FAILED_PRECONDITION: ${OUT}"
	refusal="$(grep -o 'code = FailedPrecondition desc = [^:]*: [^:]*' <<<"${OUT}" | head -1)"
	[[ "$(records memctl "${mem}" EVENT_END)" == "${turns_before}" ]] || fail "a turn completed on the replaced actor"
	say "${refusal}" "completed turns in the journal: ${turns_before} before, ${turns_before} after: nothing ran on the empty micro-VM"

	# ---------------------------------------------------------------------------------------------
	chapter 8 "Recap" "One AgentSuite agent ran as durable sessions on micro-VMs: its chat was journaled with token counts, paused and resumed with its memory, replayed with zero model calls, forked, and protected from a swapped micro-VM."
	printf '  %sThe full output of every command is in the logs that come with this video.%s\n' "${D}" "${N}"
	sleep $((READ + 6))
	log "DEMO COMPLETE"
	printf 'DEMO COMPLETE in %d s, about %d s of it on screen\n' $((SECONDS - SHOW_START)) $((SECONDS - SHOW_START - HIDDEN))
}
# summarize: totals over the journal's records (the seq= lines; an assistant message can span lines).
summarize() {
	awk '
		!/^seq=/ { next }
		{ n++; for (i = 3; i <= NF; i++) if ($i ~ /^hash=/) head = substr($i, 6) }
		$2 == "EVENT_END" { turns++ }
		$2 == "EVENT_MODEL_CALL" { calls++ }
		$2 == "EVENT_USAGE" { usage++; for (i = 3; i <= NF; i++) { split($i, kv, "="); if (kv[1] == "input_tokens") in_ += kv[2]; if (kv[1] == "output_tokens") out += kv[2] } }
		END { printf "%d %d %d %d %d %d %s\n", n, turns + 0, calls + 0, usage + 0, in_ + 0, out + 0, head }'
}
# snapshot_size URI: the object store's listing of the snapshot's objects, with their total size.
# The kind install's rustfs is reached from the node's network namespace, as substrate's
# hack/microvm-assets/stage-to-rustfs.sh does; its credentials are the install's fixed defaults.
snapshot_size() {
	local path="${1#*://}" endpoint
	endpoint="http://$("${K[@]}" -n ate-system get svc rustfs -o jsonpath='{.spec.clusterIP}'):9000"
	docker run --rm --network "container:${CLUSTER}-control-plane" -e AWS_ACCESS_KEY_ID=rustfsadmin \
		-e AWS_SECRET_ACCESS_KEY=rustfsadmin -e AWS_REGION=us-east-1 -e "AWS_ENDPOINT_URL=${endpoint}" \
		"${AWS_CLI_IMAGE}" s3 ls --recursive --summarize "s3://${path%/}/"
}
replace_actor() { # name template
	"${ATE[@]}" delete actor "$1" -a "${ATESPACE}" --any-state || return
	for _ in $(seq 60); do
		"${ATE[@]}" get actor "$1" -a "${ATESPACE}" >/dev/null 2>&1 || break
		sleep 2
	done
	"${ATE[@]}" create actor "$1" -a "${ATESPACE}" --template "$2"
}
# side_by_side LTITLE LFILE RTITLE RFILE: each session's turns (inputs) and its last answer, in two
# columns.
side_by_side() {
	python3 -c '
import re, sys, textwrap
cols = int(sys.argv[5]); w = (cols - 7) // 2
def turns(path):
    inputs, outputs, cur = [], [], None
    for line in open(path):
        line = line.rstrip("\n")
        m = re.match(r"seq=\d+ +EVENT_(\w+)(?:  (\w+): (.*))?$", line)
        if m:
            cur = None
            if m.group(1) == "INPUT": inputs.append(m.group(3))
            if m.group(1) == "OUTPUT": outputs.append(m.group(3)); cur = len(outputs) - 1
        elif cur is not None:
            outputs[cur] += "\n" + line
    return inputs, outputs
def column(title, path):
    inputs, outputs = turns(path)
    rows = [title, "-" * min(w, len(title))]
    for i, t in enumerate(inputs):
        rows += textwrap.wrap("turn %d: %s" % (i + 1, t), w, subsequent_indent="  ")
    rows.append("")
    rows.append("last answer:")
    for para in outputs[-1].replace("**", "").split("\n"):
        rows += textwrap.wrap(para.strip(), w - 2, initial_indent="  ", subsequent_indent="  ") if para.strip() else []
    return rows[:16]
l, r = column(sys.argv[1], sys.argv[2]), column(sys.argv[3], sys.argv[4])
for i in range(max(len(l), len(r))):
    a = l[i] if i < len(l) else ""
    b = r[i] if i < len(r) else ""
    print("  %s │ %s" % (a.ljust(w), b))' "$1" "$2" "$3" "$4" "${COLS}"
}

case "${MODE}" in
setup) setup ;;
show) show ;;
teardown)
	rm -f "${EXPIRY_FILE}"
	"${DEMO}/../down.sh" 2>&1 | tee -a "${STEP_LOG}"
	kind get clusters 2>&1 | tee -a "${STEP_LOG}"
	if cluster_listed; then fail "kind still lists ${CLUSTER}"; fi
	[[ -z "$(docker ps -aq --filter "name=${CLUSTER}")" ]] || fail "containers of ${CLUSTER} remain"
	echo "TEARDOWN OK" | tee -a "${STEP_LOG}"
	;;
esac
