#!/usr/bin/env bash
# Records the short demo: driver.sh setup off camera, then driver.sh show under vhs (demo.tape), the
# turn-flow diagram spliced in, then the video, logs and README, then teardown.
#
#   record.sh            setup, record, collect, then teardown (DEMO_KEEP=1 keeps the cluster)
#   record.sh setup      driver.sh setup: cluster, substrate, the micro-VM smoke test, the templates and
#                        daemons; fails unless setup.log holds SMOKE OK and SETUP OK
#   record.sh show       driver.sh show without recording it, rendered here (a dry run)
#   record.sh record     vhs demo.tape, then flow.mmd rendered and spliced in: demo.mp4 (needs a
#                        finished setup)
#   record.sh collect    the logs, the scripts, demo.mp4.sha256, the checks and README.md with chapter
#                        times
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
# is fetched on the host by the driver and never reaches this machine. DEMO_READ (driver.sh) is how
# long a result stays on screen. Recording needs vhs, ffmpeg and npx (for @mermaid-js/mermaid-cli).
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${DEMO}/../../.." && pwd)"
OUT="${DEMO_OUT:-${HOME}/.local/state/hq/demos/$(date +%F)-poc-short}"
ENV_FILE="${AZURE_OPENAI_ENV:-${HOME}/.config/hq/azure-openai.env}"
REMOTE="${DEMO_REMOTE:-local}"
# demo.tape's terminal is 113 columns wide; the driver draws one fewer, so a full row never wraps.
export DEMO_COLS=112 DEMO_READ="${DEMO_READ:-4}"
CHAPTERS=10
IMAGE=agentsuite-demo-tools:2
MERMAID_CLI="@mermaid-js/mermaid-cli@11.17.0"
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

completed() { grep -q 'DEMO COMPLETE' "${LOGS}/ch-${CHAPTERS}.log" 2>/dev/null; }

dry_run() {
	load_env
	rm -f "${LOGS}"/ch-*.log
	driver show 1800 2>&1 | awk -f "${DEMO}/present.awk"
	completed
}

run_show() {
	load_env
	rm -f "${LOGS}"/ch-*.log
	local rc=0
	driver show 1800 2>&1 | awk -f "${DEMO}/present.awk" || rc=$?
	# The end marker demo.tape waits for, held on the cursor's line until vhs stops the recording.
	local status=FAILED
	if ((rc == 0)) && completed; then
		status=COMPLETE
	fi
	printf '\n\e[1;%smRECORDING %s: driver exit %d\e[0m' "$([[ "${status}" == COMPLETE ]] && echo 32 || echo 31)" "${status}" "${rc}"
	sleep 120
}

record() {
	if ! command -v vhs >/dev/null || ! command -v ffprobe >/dev/null || ! command -v npx >/dev/null; then
		echo "needs vhs, ffmpeg and npx" >&2
		exit 1
	fi
	grep -q '^SETUP OK' "${LOGS}/setup.log" 2>/dev/null || {
		echo "no finished setup in ${LOGS}/setup.log; run record.sh setup" >&2
		exit 1
	}
	load_env
	mkdir -p "${OUT}"
	# What the recording runs, taken now: collect may run after later commits.
	local base dirty
	base="$(git -C "${ROOT}" rev-parse --short HEAD)"
	dirty="$(git -C "${ROOT}" status --porcelain | wc -l | tr -d ' ')"
	if ((dirty == 0)); then echo "commit ${base}"; else echo "a working tree based on ${base} (${dirty} files changed or new)"; fi >"${OUT}/source.txt"
	local tmp="${OUT}/.recording"
	rm -rf "${tmp}" "${OUT}/demo.mp4"
	mkdir -p "${tmp}"
	(cd "${ROOT}" && vhs -o "${tmp}/raw.mp4" "${DEMO}/demo.tape")
	completed || {
		echo "the driver did not finish; see ${tmp}/raw.mp4 and ${LOGS}" >&2
		exit 1
	}
	# The diagram on a transparent background; splice.py puts it on the terminal's own background.
	(cd "${tmp}" && npx -y "${MERMAID_CLI}" -q -i "${DEMO}/flow.mmd" -o "${tmp}/flow.png" -b transparent -s 3)
	python3 -I "${DEMO}/splice.py" "${tmp}/raw.mp4" "${tmp}/flow.png" "${OUT}/demo.mp4" | tee "${tmp}/splice.tsv"
	cp "${tmp}/flow.png" "${tmp}/splice.tsv" "${OUT}/"
	rm -rf "${tmp}"
}

# sha256 FILE: "<hex>  FILE", with GNU coreutils or BSD shasum.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi
}
# utc EPOCH: the time in RFC 3339, with GNU or BSD date.
utc() { date -u -d "@$1" +%FT%TZ 2>/dev/null || date -u -r "$1" +%FT%TZ; }
mmss() { awk -v s="$1" 'BEGIN { printf "%d:%02d", s / 60, s % 60 }'; }

# token_scan PATH...: the files under PATH that hold a JWT prefix, the instance metadata token field
# or a bearer header with a JWT (the patterns are assembled so this file does not match them).
token_scan() {
	local jwt="ey""J" field="access""_token"
	grep -rlE -e "${jwt}[A-Za-z0-9_-]{8,}" -e "${field}" -e "Bearer ${jwt}" "$@" || true
}

collect() {
	local mp4="${OUT}/demo.mp4" dur n at title
	mkdir -p "${OUT}/logs" "${OUT}/frames"
	rm -f "${OUT}"/logs/ch-*.log
	cp "${LOGS}"/setup.log "${LOGS}"/show.log "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv" "${OUT}/logs/"
	cp "${DEMO}"/driver.sh "${DEMO}"/present.awk "${DEMO}"/demo.tape "${DEMO}"/record.sh "${DEMO}"/chapters.py \
		"${DEMO}"/splice.py "${DEMO}"/flow.mmd "${DEMO}"/trace-turn.sh "${DEMO}"/snapshot-size.sh "${DEMO}"/journal.awk \
		"${DEMO}"/side_by_side.py "${DEMO}"/template.jq "${DEMO}"/fields.jq "${DEMO}"/smoke-microvm.sh "${DEMO}"/Dockerfile.tools "${OUT}/"
	(cd "${OUT}" && sha256 demo.mp4 >demo.mp4.sha256)
	dur="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${mp4}")"
	# Chapter k starts at the first frame whose progress bar is k/10 full; a frame 1.5 s later is kept
	# for each, to check against the table.
	python3 -I "${DEMO}/chapters.py" "${mp4}" >"${OUT}/chapters.tsv"
	while IFS=$'\t' read -r n at; do
		ffmpeg -nostdin -v error -y -ss "$(awk -v t="${at}" 'BEGIN { print t + 1.5 }')" -i "${mp4}" -frames:v 1 "${OUT}/frames/ch-${n}.png"
	done <"${OUT}/chapters.tsv"
	local decode_rc=0 decode_out
	decode_out="$(ffmpeg -v error -i "${mp4}" -f null - 2>&1)" || decode_rc=$?
	local video
	video="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height,r_frame_rate -of csv=p=0 "${mp4}")"
	local source
	source="$(cat "${OUT}/source.txt" 2>/dev/null || echo "an unrecorded source (no source.txt)")"
	local scan
	scan="$(token_scan "${OUT}/logs" "${OUT}"/*.sh "${OUT}"/*.py "${OUT}"/*.awk "${OUT}"/*.tape "${OUT}"/*.mmd "${OUT}"/*.jq)"
	local mp4_hits
	mp4_hits="$(LC_ALL=C grep -c "ey""J" "${mp4}" || true)"
	local max_lines max_caption
	max_lines="$(sed -n 's/^(on screen: [0-9]* lines, \([0-9]*\) on this screen)$/\1/p' "${OUT}"/logs/ch-*.log | sort -n | tail -1)"
	max_caption="$(sed -n 's/^## screen: //p' "${OUT}"/logs/ch-*.log | awk '{ print length($0) }' | sort -n | tail -1)"
	load_env
	{
		echo "# AgentSuite + AgentSessions + agent-substrate micro-VMs: short demo"
		echo
		echo "To share:"
		echo
		echo "> How an agent defined with AgentSuite becomes an agent-substrate workload and runs as durable"
		echo "> AgentSessions sessions on micro-VMs. It starts from kaimahi's AgentSuite test suite"
		echo "> (testdata/minimal, agent \`writer\`), validated by kaimahi's own package, and shows how \`emit render\`"
		echo "> turns the suite plus a draft AgentSessionsBinding into a substrate ActorTemplate and the harness"
		echo "> flag for agentsessionsd. A diagram and a trace then follow one real turn from agentctl through"
		echo "> agentsessionsd, the substrate adapter and router, to the harness in its Kata + Cloud Hypervisor"
		echo "> micro-VM and the model (${AGENTSUITE_MODEL} on Azure OpenAI) and back. The rest is the session lifecycle: a"
		echo "> chat, the hash-chained journal with token counts, a pause to a memory snapshot and a resume with the"
		echo "> conversation still in RAM, a replay with zero model calls, a fork, and AgentSessions refusing a turn"
		echo "> when a session's micro-VM is swapped underneath it. Everything runs live."
		echo
		echo "- Video: demo.mp4, $(mmss "${dur}") ($(awk -v d="${dur}" 'BEGIN { printf "%.1f", d }') s), sha256 \`$(cut -d' ' -f1 "${OUT}/demo.mp4.sha256")\`"
		echo "- Recorded $(utc "$(head -1 "${LOGS}/timeline.tsv" | cut -f2)") with \`record.sh\` from ${source}"
		echo "- logs/: setup.log (off camera), show.log, one ch-N.log per chapter with the full output of every"
		echo "  command, timeline.tsv; frames/: a frame 1.5 s into each chapter; flow.png: the diagram as spliced"
		echo
		echo "| Chapter | Starts at | Title |"
		echo "|---|---|---|"
		while IFS=$'\t' read -r n _ title; do
			at="$(awk -v k="${n}" '$1 == k { print $2 }' "${OUT}/chapters.tsv")"
			printf '| %s | %s | %s |\n' "${n}" "$([[ -n "${at}" ]] && mmss "${at}" || echo '?')" "${title}"
		done <"${LOGS}/timeline.tsv"
		echo
		echo "Chapter times come from the video itself: chapter k starts at the first frame whose top-row progress"
		echo "bar is k/${CHAPTERS} full (chapters.py)."
		echo
		echo "## How it was made"
		echo
		echo "- Off camera, before recording (\`record.sh setup\`, logs/setup.log): a kind cluster on a Linux VM"
		echo "  with KVM, substrate's install with Kata + Cloud Hypervisor micro-VMs, the micro-VM smoke test,"
		echo "  the suite against kaimahi 9c5c993, the harness image and both ActorTemplates, a model token from"
		echo "  the VM's managed identity (only its size and expiry are logged), both agentsessionsd Deployments."
		echo "  \`$(grep -m1 '^SMOKE OK' "${LOGS}/setup.log")\`; \`$(grep -m1 '^SETUP OK' "${LOGS}/setup.log")\`."
		echo "- On camera (\`record.sh record\`): \`driver.sh show\` on the VM through \`hq remote-test\`, rendered by"
		echo "  vhs here. Each command on screen is the command line that runs, with the filter after \`|\`"
		echo "  applied to its saved output; the driver stops on any claim that does not hold, and writes the"
		echo "  full output of each command to logs/ch-N.log. Waits longer than a moment are hidden between a"
		echo "  \`⠿\` line and its \`✓ ... in N s\` line."
		echo "- The diagram (flow.mmd) is rendered with ${MERMAID_CLI} and spliced over the block chapter 3"
		echo "  draws for it (splice.py): $(cut -f2- "${OUT}/splice.tsv" | tr '\t' ' ')."
		echo "- After recording (\`record.sh teardown\`, logs/teardown.log): the cluster is deleted."
		echo
		echo "## Checks"
		echo
		echo "- \`ffprobe\`: ${video}, ${dur} s. \`ffmpeg -v error -i demo.mp4 -f null -\`: exit ${decode_rc}, $(printf '%s' "${decode_out}" | grep -c '') lines of output."
		echo "- Chapter starts: $(cut -f1 "${OUT}/chapters.tsv" | paste -sd ' ' -) found in order; frames/ch-k.png shows chapter k's banner."
		echo "- Screens: at most ${max_lines} output lines on any screen; the longest caption is ${max_caption} of $((DEMO_COLS - 2)) columns."
		echo "- Token scan (a JWT prefix, the metadata token field, a bearer header) over logs/, the scripts, the tape"
		echo "  and the diagram: ${scan:-no matches}. The 3-byte JWT prefix in demo.mp4: ${mp4_hits} matches."
		echo "- Smoke pre-check: \`$(grep -m1 '^SMOKE OK' "${LOGS}/setup.log")\`"
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
show) dry_run ;;
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
	echo "usage: $0 [setup|show|record|collect|teardown|run]" >&2
	exit 2
	;;
esac
