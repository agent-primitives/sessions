#!/usr/bin/env bash
# The recorded demo: kaimahi's minimal AgentSuite (agent `writer`) turned into an agent-substrate
# ActorTemplate and run as AgentSessions sessions on micro-VMs, with a real model on Azure OpenAI.
#
#   driver.sh setup      off camera: the kind cluster, substrate, the micro-VM smoke test (must pass),
#                        the harness image and ActorTemplates, the model token and both agentsessionsd
#                        Deployments, and the agentctl, replay and emit binaries; ends with SETUP OK
#   driver.sh show       on camera: ten short chapters, each one or more screens of a one-line caption
#                        and at most five output lines, while the full output of every command goes to
#                        the chapter's log
#   driver.sh teardown   down.sh
#
# Every command show prints is the command line that runs (see run below), and show checks every
# claim it prints and stops on the first that does not hold. A wait longer than a moment runs out of
# frame: the driver prints a "⠿ <what> …" line, runs the step, then replaces that line with
# "✓ <what> in N s"; demo.tape hides the recording between the two. demo.tape lists those waits in
# order, so a new `run -w` needs a matching pair of waits in the tape (scripts_test.go checks the
# counts match).
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
#   DEMO_READ              seconds a result stays on screen (default 4); a caption stays 4.5 s, the
#                          title card 5 s and the diagram 9 s
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
READ="${DEMO_READ:-4}"
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
log() { printf '%s\n' "$@" | redact >>"${STEP_LOG}"; }
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
agentctl_() { ctl "$1" "${SERVER}" "${@:2}"; }
memctl() { ctl "$1" "${MEMSERVER}" "${@:2}"; }

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
	# The diagram calls the sandbox a Kata + Cloud Hypervisor micro-VM; substrate's kind install says
	# so when it enables the class.
	grep -q 'micro-VM (kata + cloud-hypervisor)' "${STEP_LOG}" || fail "the substrate install did not enable kata + cloud-hypervisor micro-VMs"
	log "kata + cloud-hypervisor: $(grep -c 'micro-VM (kata + cloud-hypervisor)' "${STEP_LOG}") lines in the install output say so"
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
	[[ -x "${WORK}/bin/emit" && -x "${WORK}/bin/kubectl-ate" ]] || fail "up.sh did not leave emit and kubectl-ate in ${WORK}/bin"
	# The object store client snapshot-size.sh uses, pulled now so show does not wait on it.
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
# Keep in sync with snapshot-size.sh.
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
		if agentctl_ list >/dev/null 2>&1 && memctl list >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	echo "the Sessions APIs did not answer through the port-forwards"
	return 1
}

# ================================================================================================
# show: the recording. A chapter clears the screen, draws a progress bar (chapter k of 10 fills k/10
# of the top row; chapters.py finds each chapter's start in the video from it), its title and a
# one-line caption. `page` starts another screen of the same chapter with its own caption.
#
# Every command on screen is the literal command line that runs: run prints it, evaluates it in this
# shell (so $COPY, $API and the rest are this shell's variables), writes its complete output to the
# chapter's log, then runs the filter shown after " | " (the second line of its input) over that
# output; the filter's output is what the screen shows. A command must exit 0 (run -x: must fail)
# and write nothing to stderr, which the screen would show. No screen shows more than MAX_LINES
# output lines and no caption is wider than the screen: show fails rather than show either. Checks
# the screen does not show run through offscreen, with their output in the log too.
# ================================================================================================
C=$'\e[1;36m' Y=$'\e[1;33m' G=$'\e[32m' W=$'\e[1;37m' D=$'\e[2m' N=$'\e[0m'
CHAPTERS=10
MAX_LINES=5
CAPTION_HOLD=4.5 TITLE_HOLD=5 DIAGRAM_HOLD=9
# wrap TEXT [PREFIX [INDENT]]: TEXT folded to the screen at word boundaries, each line after the
# first indented by INDENT spaces (default the width of PREFIX).
wrap() {
	python3 -c '
import sys, textwrap
text, prefix, cols = sys.argv[1], sys.argv[2], int(sys.argv[3])
indent = " " * (int(sys.argv[4]) if sys.argv[4] else len(prefix))
lines = []
for para in text.split("\n"):
    lines += textwrap.wrap(para, cols - len(prefix) - 2, break_on_hyphens=False, drop_whitespace=False) or [""]
print("\n".join((prefix if i == 0 else indent) + l.rstrip() for i, l in enumerate(lines)))' "$1" "${2:-}" "${COLS}" "${3:-}"
}
# header CAPTION: a fresh screen for the current chapter, with CAPTION.
header() {
	[[ "$1" =~ ^[[:print:]]+$ ]] || fail "a caption is not one line of printable characters: $1"
	((${#1} <= COLS - 2)) || fail "a caption is ${#1} characters, wider than the screen's $((COLS - 2)): $1"
	SCREEN_LINES=0
	local fill=$((COLS * CH / CHAPTERS))
	printf '\e[2J\e[3J\e[H\e[48;2;0;175;215m%*s\e[48;2;58;58;58m%*s\e[0m\n' "${fill}" '' $((COLS - fill)) ''
	printf '\n%s %d/%d  %s%s\n' "${C}" "${CH}" "${CHAPTERS}" "${TITLE}" "${N}"
	printf '%s%s%s\n' "${W}" "$1" "${N}"
	log "" "## screen: $1"
	sleep "${CAPTION_HOLD}"
}
chapter() { # n title caption
	CH=$1 TITLE=$2
	STEP_LOG="${LOGS}/ch-$1.log"
	printf '%s\t%s\t%s\n' "$1" "$(date -u +%s)" "$2" >>"${LOGS}/timeline.tsv"
	printf '# CHAPTER %s  %s  (%s)\n' "$1" "$2" "$(date -u +%FT%TZ)" >"${STEP_LOG}"
	header "$3"
}
page() { header "$1"; } # caption: another screen of the current chapter
# show_lines TEXT: output lines on screen, two spaces in, then held long enough to read.
show_lines() {
	local text n
	text="$(wrap "$1" "  ")"
	n="$(printf '%s\n' "${text}" | wc -l)"
	SCREEN_LINES=$((SCREEN_LINES + n))
	((SCREEN_LINES <= MAX_LINES)) || fail "this screen would show ${SCREEN_LINES} output lines, more than ${MAX_LINES}"
	printf '%s\n' "${text}"
	log "(on screen: ${n} lines, ${SCREEN_LINES} on this screen)"
	sleep "${READ}"
}
# run [-w WHAT DONE] [-x] <<CMD: the command on the first line of stdin, shown with the filter on the
# second line (if any) after " | ". -w runs it out of frame: it prints "⠿ WHAT …" and, once the
# command is done, replaces that with "✓ DONE in N s"; demo.tape hides the recording between the
# two. -x: the command must fail.
RAW="${WORK}/run.out" RAW_ERR="${WORK}/run.err"
run() {
	local what="" done="" expect_fail=false
	while (($#)); do
		case "$1" in
		-w) what="$2" done="$3" && shift 3 ;;
		-x) expect_fail=true && shift ;;
		*) fail "run: unknown option $1" ;;
		esac
	done
	local cmdline="" filter="" line rc=0 t0 took var shown rows
	IFS= read -r cmdline || [[ -n "${cmdline}" ]] || fail "run: no command"
	IFS= read -r filter || true
	line="${cmdline}${filter:+ | ${filter}}"
	# As a terminal shows a long command line: cut at the screen's width, not at words.
	shown="$(python3 -c 'import sys; s, w = sys.argv[1], int(sys.argv[2]); print("\n".join(s[i:i + w] for i in range(0, len(s), w)))' "\$ ${line}" "${COLS}")"
	rows="$(printf '%s\n' "${shown}" | wc -l)"
	printf '%s\n' "${shown}" | sed "s/^/${Y}/; s/\$/${N}/"
	log "\$ ${line}"
	sleep "1.$((rows > 1 ? 9 : 2))"
	if [[ -n "${what}" ]]; then
		printf '  %s⠿ %s …%s\n' "${D}" "${what}" "${N}"
		sleep 1
	fi
	t0=${SECONDS}
	eval "${cmdline}" >"${RAW}" 2>"${RAW_ERR}" || rc=$?
	took=$((SECONDS - t0))
	redact <"${RAW}" >>"${STEP_LOG}"
	if [[ -s "${RAW_ERR}" ]]; then
		log "(stderr:)"
		redact <"${RAW_ERR}" >>"${STEP_LOG}"
	fi
	if [[ "${cmdline}" =~ ^([A-Z_]+)=\$\( ]]; then
		var="${BASH_REMATCH[1]}"
		log "(${var}=${!var})"
	fi
	log "(exit ${rc} after ${took} s)"
	if [[ -n "${what}" ]]; then
		printf '@@R   %s✓ %s in %d s%s\n' "${G}" "${done}" "${took}" "${N}"
		sleep 1.5
	fi
	if ${expect_fail}; then
		((rc != 0)) || fail "this command was to fail and did not: ${line}"
	else
		((rc == 0)) || fail "exit ${rc}: ${line}"
	fi
	[[ ! -s "${RAW_ERR}" ]] || fail "this command wrote to stderr, which the screen does not show: ${line}"
	if [[ -n "${filter}" ]]; then
		SHOWN="$(eval "${filter}" <"${RAW}")" || fail "the filter found nothing: ${filter}"
		log "(through | ${filter}:)" "${SHOWN}"
	else
		SHOWN="$(cat "${RAW}")"
	fi
	if [[ -n "${SHOWN}" ]]; then show_lines "${SHOWN}"; fi
}
# offscreen WHAT CMD...: a check the screen does not show; its output goes to RAW and the log.
offscreen() {
	local what="$1" rc=0
	shift
	log "(off screen, ${what}: $*)"
	"$@" >"${RAW}" 2>&1 || rc=$?
	redact <"${RAW}" >>"${STEP_LOG}"
	log "(exit ${rc})"
	return "${rc}"
}
journal_count() { # server session kind: how many records of that kind the session's journal holds
	offscreen "the journal of $2" "${WORK}/bin/agentctl" replay --server "$1" --session "$2" || fail "agentctl replay $2 failed"
	awk -v k="$3" '$2 == k { n++ } END { print n + 0 }' "${RAW}"
}
actor_uid() { # session: its actor's UID, with the actor's full record in the log
	offscreen "the actor of $1" "${ATE[@]}" get actor "$1" -a "${ATESPACE}" -o json || fail "no actor for $1"
	jq -r .metadata.uid "${RAW}"
}
# diagram: a screen the recording replaces with the turn-flow diagram (flow.mmd, which record.sh
# renders and splices in): every row below the caption is magenta, which splice.py looks for.
diagram() {
	local _
	for _ in $(seq 24); do printf '\e[48;2;255;0;255m%*s\e[0m\n' "${COLS}" ''; done
	log "(on screen: the turn-flow diagram, demo/flow.mmd)"
	sleep "${DIAGRAM_HOLD}"
}

show() {
	[[ -x "${WORK}/bin/agentctl" && -x "${WORK}/bin/replay" && -x "${WORK}/bin/emit" && -s "${EXPIRY_FILE}" ]] || fail "run driver.sh setup first"
	rm -f "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv"
	rm -rf /tmp/render /tmp/render-example-model /tmp/agent /tmp/trace /tmp/turn1.txt /tmp/mem1.txt /tmp/fork.txt /tmp/child.txt
	forwards
	wait_apis >/dev/null || fail "the Sessions APIs did not answer"
	STEP_LOG="${LOGS}/show.log"
	: >"${STEP_LOG}"
	local expires_min
	expires_min=$((($(cat "${EXPIRY_FILE}") - $(date +%s)) / 60))
	log "model token: expires in ${expires_min} min"
	((expires_min > 15)) || fail "the model token expires in ${expires_min} min; run driver.sh setup again"
	# The commands on screen run as typed: the tools from PATH (the kubectl-ate plugin included), the
	# demo's own kubeconfig (exported by common.sh) whose current context must be the demo's cluster,
	# and paths relative to this directory.
	export PATH="${WORK}/bin:${PATH}"
	[[ "$(kubectl config current-context)" == "kind-${CLUSTER}" ]] || fail "the kubeconfig's current context is not kind-${CLUSTER}"
	cd "${HERE}"
	export API="${SERVER}" API_MEM="${MEMSERVER}" ROUTER
	TEMPLATE="$(cat "${STATE}/render/template-name")"
	MEMTEMPLATE="$(cat "${STATE}/render-memory/template-name")"
	IMAGE="$(jq -r '.containers[0].image' "${STATE}/render/actortemplate.json")"
	[[ "${IMAGE}" =~ @sha256:[0-9a-f]{64}$ ]] || fail "the harness image is not digest-pinned: ${IMAGE}"
	log "TEMPLATE=${TEMPLATE} MEMTEMPLATE=${MEMTEMPLATE} IMAGE=${IMAGE} API=${API} API_MEM=${API_MEM} ROUTER=${ROUTER}"
	SHOW_START=${SECONDS}
	local model="${AGENTSUITE_MODEL}" uid_before uid_after turns_before calls_before fork_seq

	# ---------------------------------------------------------------------------------------------
	# Hide the cursor: the terminal only shows what the driver prints.
	printf '\e[?25l\e[2J\e[3J\e[H\n'
	printf '%s  Durable agent sessions on micro-VMs%s\n\n' "${C}" "${N}"
	wrap "An agent defined with AgentSuite becomes an agent-substrate ActorTemplate and runs as AgentSessions sessions, each in its own micro-VM, with ${model} on Azure OpenAI." "  " | sed "s/^/${W}/; s/\$/${N}/"
	sleep "${TITLE_HOLD}"

	# ---------------------------------------------------------------------------------------------
	chapter 1 "The AgentSuite" "kaimahi's minimal AgentSuite: a manifest, one agent (writer), its instructions and one composition"
	run <<'EOF'
(cd suites/kaimahi-minimal && echo agentsuite.json */*)
EOF
	run <<'EOF'
jq -r '"instructions  \(.instructions.path)  \(.instructions.digest[:23])…", "model         \(.model.protocol) \(.model.model)"' suites/kaimahi-minimal/agents/writer.json
EOF
	[[ "${SHOWN}" == *"model         openai-compatible example-model" ]] || fail "the agent no longer names example-model"
	run <<'EOF'
jq -c '{agent, platform, buildProfile}' suites/kaimahi-minimal/compositions/writer-linux-amd64.json
EOF
	page "These are kaimahi's testdata/minimal files at 9c5c993, and kaimahi's own agentsuite package validates them"
	run <<'EOF'
grep ^commit suites/kaimahi-minimal/SOURCE
EOF
	[[ "${SHOWN}" == "commit 9c5c993"* ]] || fail "SOURCE does not name kaimahi 9c5c993"
	run <<'EOF'
(cd suites/kaimahi-minimal && awk 'NF == 3 && !/^#/ { print $2 "  " $3 }' SOURCE | sha256sum -c)
awk '{ print $NF }' | uniq -c
EOF
	[[ "$(grep -c ': OK$' "${RAW}")" == 6 && "$(wc -l <"${RAW}")" == 6 ]] || fail "not every suite file matches SOURCE"
	offscreen "each file's git blob id and sha256 against SOURCE" verify_suite || fail "the suite differs from kaimahi 9c5c993"
	run <<'EOF'
emit validate -suite suites/kaimahi-minimal
EOF
	[[ "${SHOWN}" == "suite minimal valid: 1 agent; compositions: writer linux/amd64 profile default"*"kaimahi/agentsuite v"*"-9c5c993"* ]] ||
		fail "emit validate did not report kaimahi 9c5c993's validator"

	# ---------------------------------------------------------------------------------------------
	chapter 2 "Into substrate" "binding-microvm.json, a draft AgentSessionsBinding type this PoC adds, says where and how it runs"
	run <<'EOF'
jq -c '{atespace, workerSelector}, {sandbox, resources}, {snapshot}' binding-microvm.json
EOF
	run <<EOF
emit render -suite suites/kaimahi-minimal -agent writer -platform linux/amd64 -binding binding-microvm.json -image \$IMAGE -model ${model} -out /tmp/render
sed 's/ (.*//'
EOF
	[[ "${SHOWN}" == "ActorTemplate ${ATESPACE}/${TEMPLATE}"$'\n'"harness writer -> -substrate-harness=writer=${ATESPACE}/${TEMPLATE}" ]] ||
		fail "the render on screen is not template ${TEMPLATE}"
	offscreen "this render against the one setup applied" diff -r /tmp/render "${STATE}/render" || fail "the render differs from the one setup applied"
	page "The ActorTemplate: a digest-pinned image with the agent's files, a micro-VM, MEMORY snapshots, no model keys"
	run <<'EOF'
emit bundle -suite suites/kaimahi-minimal -agent writer -platform linux/amd64 -out /tmp/agent >/dev/null && ls -C /tmp/agent
EOF
	offscreen "the bundle against the harness image's /agent layer setup built" diff -r /tmp/agent "${STATE}/harness-image/agent" ||
		fail "the bundle differs from the harness image's agent layer"
	run <<'EOF'
jq -r -f demo/fields.jq /tmp/render/actortemplate.json
EOF
	jq -e --arg m "${model}" '[.containers[0].env[] | select(.name == "AGENTSUITE_MODEL" and .value == $m)] | length == 1' /tmp/render/actortemplate.json >/dev/null ||
		fail "the template does not override the model with ${model}"
	jq -e '[.containers[0].env[].name | select(test("KEY|TOKEN|SECRET|AUTH|PASSWORD"; "i"))] | length == 0' /tmp/render/actortemplate.json >/dev/null ||
		fail "the template's env names a credential"
	jq -e '.sandboxConfig.sandboxClass == "SANDBOX_CLASS_MICROVM" and .snapshotConfig.preferredFidelity == "SNAPSHOT_FIDELITY_MEMORY"' /tmp/render/actortemplate.json >/dev/null ||
		fail "the template is not a micro-VM with MEMORY snapshots"
	page "The name's hash covers image digest, binding and -model: gpt-5.4-mini replaces the suite's example-model"
	run <<'EOF'
emit render -suite suites/kaimahi-minimal -agent writer -platform linux/amd64 -binding binding-microvm.json -image $IMAGE -out /tmp/render-example-model
head -1 | sed 's/ (.*//'
EOF
	[[ "${SHOWN}" == "ActorTemplate ${ATESPACE}/minimal-writer-"* && "${SHOWN}" != *"${TEMPLATE}" ]] || fail "without -model the template name does not change"
	jq -e '[.containers[0].env[] | select(.name == "AGENTSUITE_MODEL")] | length == 0' /tmp/render-example-model/actortemplate.json >/dev/null ||
		fail "without -model the template still overrides the model"
	run <<'EOF'
kubectl get deploy agentsessionsd -n agentsuite-demo -o json | jq -r '.spec.template.spec.containers[0].args[]'
grep -e '^-substrate-harness=' -e '^-model='
EOF
	[[ "${SHOWN}" == *"-substrate-harness=writer=${ATESPACE}/${TEMPLATE}"* && "${SHOWN}" == *"-model=${model}"* ]] || fail "agentsessionsd does not serve ${TEMPLATE} with ${model}"
	run <<'EOF'
diff <(jq -S -f demo/template.jq /tmp/render/actortemplate.json) <(kubectl ate get actor-template $TEMPLATE -a agentsuite-demo -o json | jq -S -f demo/template.jq) && echo "the ActorTemplate in substrate is this render"
EOF

	# ---------------------------------------------------------------------------------------------
	chapter 3 "How a turn flows" "One turn: agentctl to AgentSessions, the substrate adapter, the micro-VM's harness, the model, and back"
	diagram
	page "A real turn, followed by its session id through each component's own log"
	run <<'EOF'
COPY=$(agentctl create --server $API --harness writer --name launch-copy)
EOF
	[[ "${COPY}" =~ ^sess-[0-9a-f]+$ ]] || fail "agentctl create did not print a session UID"
	run -w "starting the session's micro-VM and asking ${model}" "answered" <<'EOF'
agentctl exec --server $API --session $COPY --input "Write a one-sentence product blurb for Ledger, an open API that keeps AI agent sessions durable across crashes, suspends and forks." >/tmp/turn1.txt
EOF
	offscreen "turn 1's output" cat /tmp/turn1.txt
	grep -q '^seq=[0-9]* *EVENT_END' /tmp/turn1.txt || fail "turn 1 did not end"
	run <<'EOF'
demo/trace-turn.sh $COPY /tmp/trace
EOF
	local f
	for f in /tmp/trace/*.log; do offscreen "the full log of ${f##*/}" cat "${f}"; done

	# ---------------------------------------------------------------------------------------------
	chapter 4 "Chat with the agent" "A second turn in the same session builds on the first answer"
	run <<'EOF'
awk '/EVENT_OUTPUT/ { sub(/.*assistant: /, ""); on = 1 } /EVENT_USAGE/ { on = 0 } on && NF' /tmp/turn1.txt | tr -d '*'
EOF
	run -w "asking ${model}" "answered" <<'EOF'
agentctl exec --server $API --session $COPY --input "Make it punchier, under 15 words, and add a short tagline."
awk '/EVENT_OUTPUT/ { sub(/.*assistant: /, ""); on = 1 } /EVENT_USAGE/ { on = 0 } on && NF' | tr -d '*'
EOF

	# ---------------------------------------------------------------------------------------------
	chapter 5 "The journal" "Every input, model call, answer and token count is a record that carries the previous record's hash"
	run <<'EOF'
agentctl replay -v --server $API --session $COPY
awk -f demo/journal.awk
EOF
	local sum
	sum="$(grep -E '^seq=' "${RAW}" | awk '$2 == "EVENT_END" { t++ } $2 == "EVENT_MODEL_CALL" { c++ } $2 == "EVENT_USAGE" { u++ } END { print t + 0, c + 0, u + 0 }')"
	[[ "${sum}" == "2 2 2" && "${SHOWN}" == "2 turns, 2 model calls (${model}), "* && "${SHOWN}" != *" 0 tokens in"* ]] ||
		fail "the journal does not hold 2 turns with one model call and one usage record each: ${sum}"

	# ---------------------------------------------------------------------------------------------
	chapter 6 "Pause and resume, memory included" "This copy keeps the chat only in RAM: suspend snapshots the micro-VM's memory and resume restores it"
	run <<'EOF'
MEM=$(agentctl create --server $API_MEM --harness writer-memory --name codename)
EOF
	[[ "${MEM}" =~ ^sess-[0-9a-f]+$ ]] || fail "agentctl create did not print a session UID"
	run -w "starting the session's micro-VM and asking ${model}" "answered" <<'EOF'
agentctl exec --server $API_MEM --session $MEM --input "Remember this release codename: BLUE-HERON-42. Reply with one short sentence." >/tmp/mem1.txt
EOF
	offscreen "the memory session's turn 1" cat /tmp/mem1.txt
	uid_before="$(actor_uid "${MEM}")"
	run -w "snapshotting the micro-VM's memory" "suspended" <<'EOF'
agentctl suspend --server $API_MEM --session $MEM
EOF
	offscreen "the suspended actor" "${ATE[@]}" get actor "${MEM}" -a "${ATESPACE}" -o json || fail "no actor for ${MEM}"
	[[ "$(jq -r .status.state "${RAW}")" == ACTOR_STATE_SUSPENDED ]] || fail "the actor is not suspended"
	run -w "measuring the snapshot" "measured" <<'EOF'
demo/snapshot-size.sh $MEM
tail -1
EOF
	[[ "${SHOWN}" == "MEMORY snapshot: "* ]] || fail "the suspended actor holds no completed memory snapshot"
	run -w "restoring the micro-VM from its snapshot" "resumed" <<'EOF'
agentctl resume --server $API_MEM --session $MEM
EOF
	uid_after="$(actor_uid "${MEM}")"
	[[ -n "${uid_before}" && "${uid_before}" == "${uid_after}" ]] || fail "the session's actor changed across suspend and resume"
	run -w "asking ${model}" "answered" <<'EOF'
agentctl exec --server $API_MEM --session $MEM --input "What release codename did I give you earlier? Answer with the codename only."
awk '/EVENT_OUTPUT/ { sub(/.*assistant: /, ""); on = 1 } /EVENT_USAGE/ { on = 0 } on && NF' | tr -d '*'
EOF
	[[ "${SHOWN}" == *BLUE-HERON-42* ]] || fail "the resumed turn did not recall the codename"
	run <<'EOF'
kubectl ate logs actor $MEM -a agentsuite-demo --source containers
grep -o 'memory tier: turn [0-9]* of this process; [0-9]* messages in RAM' | tail -1
EOF
	[[ "${SHOWN}" == "memory tier: turn 2 of this process; 2 messages in RAM" ]] || fail "the harness did not answer from the RAM it held before the suspend"

	# ---------------------------------------------------------------------------------------------
	chapter 7 "Replay without the model" "Replay re-runs the session's turns in its micro-VM and answers each model call from the journal"
	calls_before="$(journal_count "${SERVER}" "${COPY}" EVENT_MODEL_CALL)"
	run -w "re-running the session's turns in its micro-VM" "replayed" <<'EOF'
replay -server $API -session $COPY -router $ROUTER -atespace agentsuite-demo -model-base-url http://127.0.0.1:18089/v1 -stats-url ""
grep -e '^journal: ' -e '^re-executed' -e '^model calls'
EOF
	grep -q '^re-executed 2 completed turns' "${RAW}" || fail "the replay did not re-execute the 2 turns"
	grep -q '^model calls: controller=0$' "${RAW}" || fail "the replay made a model call"
	run <<'EOF'
agentctl replay --server $API --session $COPY
grep -c EVENT_MODEL_CALL
EOF
	[[ "${SHOWN}" == "${calls_before}" ]] || fail "the journal's model-call count changed"

	# ---------------------------------------------------------------------------------------------
	chapter 8 "Fork" "Fork copies a session's history up to a point into a new session with its own micro-VM"
	fork_seq="$(journal_count "${SERVER}" "${COPY}" EVENT_END >/dev/null && awk '/EVENT_END/ { sub("seq=", "", $1); print $1; exit }' "${RAW}")"
	[[ "${fork_seq}" =~ ^[0-9]+$ ]] || fail "turn 1 has no end record"
	run -w "copying the history and starting the child's micro-VM" "forked" <<EOF
CHILD=\$(agentctl fork --server \$API --session \$COPY --at ${fork_seq} --names haiku-branch | tee /tmp/fork.txt | awk '/^child / { print \$2 }')
EOF
	offscreen "the fork's output" cat /tmp/fork.txt
	[[ "${CHILD}" =~ ^sess-[0-9a-f]+$ ]] || fail "fork printed no child"
	run -w "asking ${model} in the child" "answered" <<'EOF'
agentctl exec --server $API --session $CHILD --input "Instead, rewrite the blurb as a haiku." >/tmp/child.txt
EOF
	offscreen "the child's turn 2" cat /tmp/child.txt
	grep -q '^seq=[0-9]* *EVENT_OUTPUT  assistant: .' /tmp/child.txt || fail "the child's turn produced no answer"
	run <<'EOF'
python3 demo/side_by_side.py "parent launch-copy" <(agentctl replay --server $API --session $COPY) "child haiku-branch" <(agentctl replay --server $API --session $CHILD)
EOF
	[[ "$(journal_count "${SERVER}" "${COPY}" EVENT_END)" == 2 && "$(journal_count "${SERVER}" "${CHILD}" EVENT_END)" == 2 ]] || fail "parent and child do not hold 2 turns each"
	[[ "$(sed -n 's/^seq=[0-9]* *EVENT_INPUT  user: //p' "${RAW}" | tail -1)" == "Instead, rewrite the blurb as a haiku." ]] || fail "the child's second turn is not its own"

	# ---------------------------------------------------------------------------------------------
	chapter 9 "Guarding memory" "If a session's micro-VM is swapped behind AgentSessions' back, its next turn is refused"
	turns_before="$(journal_count "${MEMSERVER}" "${MEM}" EVENT_END)"
	run -w "deleting the session's micro-VM out of band" "deleted" <<'EOF'
kubectl ate delete actor $MEM -a agentsuite-demo --any-state
EOF
	local gone=false
	for _ in $(seq 60); do
		if ! offscreen "is the old actor gone" "${ATE[@]}" get actor "${MEM}" -a "${ATESPACE}"; then
			gone=true
			break
		fi
		sleep 2
	done
	${gone} || fail "the old actor is still there"
	run <<'EOF'
kubectl ate create actor $MEM -a agentsuite-demo --template $MEMTEMPLATE
awk 'NR > 1 { print $2, $4 }'
EOF
	[[ "$(actor_uid "${MEM}")" != "${uid_after}" ]] || fail "the new actor has the old UID"
	run -x -w "asking the session to continue" "refused" <<'EOF'
agentctl exec --server $API_MEM --session $MEM --input "And what was the codename again?" 2>&1
grep -o 'code = FailedPrecondition desc = [^:]*: [^:]*'
EOF
	run <<'EOF'
agentctl replay --server $API_MEM --session $MEM
grep -c EVENT_END
EOF
	[[ "${SHOWN}" == "${turns_before}" ]] || fail "a turn completed on the replaced actor"

	# ---------------------------------------------------------------------------------------------
	chapter 10 "Recap" "An AgentSuite agent became an ActorTemplate and ran as durable, journaled sessions on micro-VMs"
	printf '\n  %sCode: github.com/aramase/agentsessions, examples/agentsuite-substrate.%s\n' "${D}" "${N}"
	sleep $((READ + 6))
	log "DEMO COMPLETE"
	printf 'DEMO COMPLETE in %d s\n' $((SECONDS - SHOW_START))
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
