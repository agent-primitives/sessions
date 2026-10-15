#!/usr/bin/env bash
# Records the short demo: driver.sh setup off camera, then driver.sh show under vhs (demo.tape), then
# the video, logs and README, then teardown.
#
#   record.sh            setup, record, collect, then teardown (DEMO_KEEP=1 keeps the cluster)
#   record.sh setup      driver.sh setup: cluster, substrate, the micro-VM smoke test, the templates and
#                        daemons; fails unless setup.log holds SMOKE OK and SETUP OK
#   record.sh record     vhs demo.tape to demo.mp4 (needs a finished setup)
#   record.sh collect    the logs, the scripts, demo.mp4.sha256 and README.md with chapter times
#   record.sh teardown   driver.sh teardown
#   record.sh run        what demo.tape types: driver.sh show through present.awk, then the end marker
#
# The driver needs Docker with /dev/kvm, kind and the cluster on one Linux host. DEMO_REMOTE picks
# where it runs:
#   local (default)  on this machine, which is then that host
#   hq               on DEMO_HOST (required, no default) through `hq remote-test`, a private tool that
#                    syncs the checkout to its configured host and runs the command in a container
#                    there; DEMO_HOST must be that host, since the tools image Dockerfile.tools
#                    describes is built on it over ssh first
# The model settings come from AZURE_OPENAI_ENV, a file of non-secret NAME=value lines
# (AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL, AGENTSUITE_MODEL_PATH, AZURE_MI_CLIENT_ID); the token
# is fetched on the host by the driver and never reaches this machine. DEMO_READ (driver.sh) paces
# the chapters; the default of 7 seconds a result gives a video of about five and a half minutes.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${DEMO}/../../.." && pwd)"
OUT="${DEMO_OUT:-${HOME}/.local/state/hq/demos/$(date +%F)-poc-short}"
ENV_FILE="${AZURE_OPENAI_ENV:-${HOME}/.config/hq/azure-openai.env}"
REMOTE="${DEMO_REMOTE:-local}"
# demo.tape's terminal is 113 columns wide; the driver draws one fewer, so a full row never wraps.
export DEMO_COLS=112 DEMO_READ="${DEMO_READ:-7}"
IMAGE=agentsuite-demo-tools:2
# The work directory on the host, relative to the checkout: git-ignored, so hq remote-test keeps it
# between jobs when the same ignored directory exists here.
WORK_REL=_local/demo-work
LOGS="${ROOT}/${WORK_REL}/demo-logs"
MODEL_VARS=(AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AZURE_MI_CLIENT_ID)

load_env() {
	set -a
	# shellcheck disable=SC1090
	source "${ENV_FILE}"
	set +a
	for v in "${MODEL_VARS[@]}"; do
		[[ -n "${!v:-}" ]] || {
			echo "${v} is not set in ${ENV_FILE}" >&2
			exit 1
		}
	done
}
check_remote() {
	case "${REMOTE}" in
	local) ;;
	hq)
		[[ -n "${DEMO_HOST:-}" ]] || {
			echo "DEMO_REMOTE=hq needs DEMO_HOST, the host hq remote-test runs on" >&2
			exit 1
		}
		;;
	*)
		echo "DEMO_REMOTE must be local or hq" >&2
		exit 1
		;;
	esac
}

# driver MODE DEADLINE: driver.sh MODE on the host, its logs fetched to ${LOGS}.
driver() {
	mkdir -p "${LOGS}"
	if [[ "${REMOTE}" == local ]]; then
		AGENTSUITE_DEMO_WORK="${ROOT}/${WORK_REL}" DEMO_LOGS="${LOGS}" timeout "$2" "${DEMO}/driver.sh" "$1"
	else
		touch "${ROOT}/_local/.hq-remote-keep"
		local env_args=()
		for v in "${MODEL_VARS[@]}" DEMO_READ DEMO_COLS; do env_args+=(-e "${v}"); done
		(cd "${ROOT}" && hq remote-test --image "${IMAGE}" --timeout "$2" "${env_args[@]}" \
			-e "AGENTSUITE_DEMO_WORK=/work/${WORK_REL}" -e "DEMO_LOGS=/work/${WORK_REL}/demo-logs" \
			--fetch "${WORK_REL}/demo-logs" -- examples/agentsuite-substrate/demo/driver.sh "$1")
	fi
}

setup() {
	load_env
	if [[ "${REMOTE}" == hq ]]; then
		ssh -o BatchMode=yes "${DEMO_HOST}" "docker build -q -t ${IMAGE} -" <"${DEMO}/Dockerfile.tools"
	fi
	rm -f "${LOGS}/setup.log"
	driver setup 3000
	if ! grep -q '^SMOKE OK' "${LOGS}/setup.log" || ! grep -q '^SETUP OK' "${LOGS}/setup.log"; then
		echo "setup did not finish with SMOKE OK and SETUP OK; see ${LOGS}/setup.log" >&2
		exit 1
	fi
}

run_show() {
	load_env
	rm -f "${LOGS}"/ch-*.log
	local rc=0
	driver show 1800 2>&1 | awk -f "${DEMO}/present.awk" || rc=$?
	# The end marker demo.tape waits for, held on the cursor's line until vhs stops the recording.
	local status=FAILED
	if ((rc == 0)) && grep -q 'DEMO COMPLETE' "${LOGS}/ch-8.log" 2>/dev/null; then
		status=COMPLETE
	fi
	printf '\n\e[1;%smRECORDING %s: driver exit %d\e[0m' "$([[ "${status}" == COMPLETE ]] && echo 32 || echo 31)" "${status}" "${rc}"
	sleep 120
}

record() {
	if ! command -v vhs >/dev/null || ! command -v ffprobe >/dev/null; then
		echo "needs vhs and ffmpeg" >&2
		exit 1
	fi
	grep -q '^SETUP OK' "${LOGS}/setup.log" 2>/dev/null || {
		echo "no finished setup in ${LOGS}/setup.log; run record.sh setup" >&2
		exit 1
	}
	load_env
	mkdir -p "${OUT}"
	rm -f "${OUT}/demo.mp4"
	(cd "${ROOT}" && vhs -o "${OUT}/demo.mp4" "${DEMO}/demo.tape")
	grep -q 'DEMO COMPLETE' "${LOGS}/ch-8.log" 2>/dev/null || {
		echo "the driver did not finish; see the video and ${LOGS}" >&2
		exit 1
	}
}

# sha256 FILE: "<hex>  FILE", with GNU coreutils or BSD shasum.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi
}
# utc EPOCH: the time in RFC 3339, with GNU or BSD date.
utc() { date -u -d "@$1" +%FT%TZ 2>/dev/null || date -u -r "$1" +%FT%TZ; }
mmss() { awk -v s="$1" 'BEGIN { printf "%d:%02d", s / 60, s % 60 }'; }

collect() {
	local mp4="${OUT}/demo.mp4" dur
	mkdir -p "${OUT}/logs"
	cp "${LOGS}"/setup.log "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv" "${OUT}/logs/"
	cp "${DEMO}/driver.sh" "${DEMO}/present.awk" "${DEMO}/demo.tape" "${DEMO}/record.sh" "${DEMO}/chapters.py" \
		"${DEMO}/smoke-microvm.sh" "${DEMO}/Dockerfile.tools" "${OUT}/"
	(cd "${OUT}" && sha256 demo.mp4 >demo.mp4.sha256)
	dur="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${mp4}")"
	# Chapter k starts at the first frame whose progress bar is k/8 full.
	python3 -I "${DEMO}/chapters.py" "${mp4}" >"${OUT}/chapters.tsv"
	load_env
	{
		echo "# AgentSuite + AgentSessions + agent-substrate micro-VMs: short demo"
		echo
		echo "To share:"
		echo
		echo "> A $(mmss "${dur}") walkthrough of durable agent sessions on micro-VMs. An agent from kaimahi's AgentSuite"
		echo "> test suite (testdata/minimal, agent \`writer\`) runs as AgentSessions sessions on agent-substrate, each"
		echo "> session in its own Kata + Cloud Hypervisor micro-VM on a kind cluster, talking to ${AGENTSUITE_MODEL} on Azure"
		echo "> OpenAI. It shows a chat, the hash-chained journal with token counts, a session paused to a memory"
		echo "> snapshot and resumed with its conversation still in RAM, a replay that re-runs the turns with zero"
		echo "> model calls, a fork, and AgentSessions refusing to continue when a session's micro-VM is swapped"
		echo "> underneath it. Everything runs live; long waits are cut and each says how long it took."
		echo
		echo "- Video: demo.mp4, $(mmss "${dur}") ($(awk -v d="${dur}" 'BEGIN { printf "%.1f", d }') s), sha256 \`$(cut -d' ' -f1 "${OUT}/demo.mp4.sha256")\`"
		echo "- Recorded $(utc "$(head -1 "${LOGS}/timeline.tsv" | cut -f2)") from \`record.sh\` at $(git -C "${ROOT}" rev-parse --short HEAD); setup (cluster, substrate,"
		echo "  images, the micro-VM smoke test, the daemons) ran before recording, see logs/setup.log"
		echo "- logs/: setup.log, one ch-N.log per chapter with the full output of every command, timeline.tsv"
		echo
		echo "| Chapter | Starts at | Title |"
		echo "|---|---|---|"
		local n at title
		while IFS=$'\t' read -r n _ title; do
			at="$(awk -v k="${n}" '$1 == k { print $2 }' "${OUT}/chapters.tsv")"
			printf '| %s | %s | %s |\n' "${n}" "$([[ -n "${at}" ]] && mmss "${at}" || echo '?')" "${title}"
		done <"${LOGS}/timeline.tsv"
		echo
		echo "Chapter times come from the video itself: chapter k starts at the first frame whose top-row progress bar is k/8 full."
	} >"${OUT}/README.md"
}

teardown() {
	load_env
	driver teardown 900
	[[ ! -d "${OUT}" ]] || cp "${LOGS}/teardown.log" "${OUT}/logs/" 2>/dev/null || true
}

check_remote
case "${1:-}" in
setup) setup ;;
record) record ;;
collect) collect ;;
teardown) teardown ;;
run) run_show ;;
"")
	setup
	record
	collect
	if [[ -z "${DEMO_KEEP:-}" ]]; then teardown; fi
	echo "wrote ${OUT}"
	;;
*)
	echo "usage: $0 [setup|record|collect|teardown|run]" >&2
	exit 2
	;;
esac
