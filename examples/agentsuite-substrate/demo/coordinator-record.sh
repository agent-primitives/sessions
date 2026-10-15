#!/usr/bin/env bash
# Records the coordinator demo: coordinator.sh show under vhs (coordinator.tape), the invocation
# diagram spliced in, then the video, logs and README. Setup, probe and teardown are coordinator.sh's
# own modes; this script runs show, reset and the recording around them.
#
#   coordinator-record.sh show      coordinator.sh show without recording it (a dry run); run reset after
#   coordinator-record.sh reset     coordinator.sh reset: an empty journal and no session actors
#   coordinator-record.sh record    vhs coordinator.tape, then invoke.mmd rendered and spliced in: demo.mp4
#                                   (needs a finished setup and a reset)
#   coordinator-record.sh collect   the logs, the scripts, the diagram, demo.mp4.sha256, the checks and
#                                   README.md with chapter times
#   coordinator-record.sh run       what coordinator.tape types: show through present.awk, then the end marker
#
# coordinator.sh runs on a Linux host with KVM, Docker, kind and the cluster: through `hq remote-test`
# (a private tool that syncs the checkout to its host and runs the command in a container there) in
# the tools image Dockerfile.tools describes, as record.sh's DEMO_REMOTE=hq does. The model settings
# come from AZURE_OPENAI_ENV (non-secret NAME=value lines); the token is fetched on the host by
# coordinator.sh setup and never reaches this machine. Recording needs vhs, ffmpeg and npx.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${DEMO}/../../.." && pwd)"
OUT="${DEMO_OUT:-${HOME}/.local/state/hq/demos/$(date +%F)-coordinator-poc}"
ENV_FILE="${AZURE_OPENAI_ENV:-${HOME}/.config/hq/azure-openai.env}"
export DEMO_COLS=129 DEMO_ROWS=39 DEMO_READ="${DEMO_READ:-3}"
CHAPTERS=8
IMAGE=agentsuite-demo-tools:3
MERMAID_CLI="@mermaid-js/mermaid-cli@11.17.0"
CLUSTER=agentsuite-coord
WORK_REL=_local/coord-work
LOGS="${ROOT}/${WORK_REL}/logs"
MODEL_VARS=(AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AZURE_MI_CLIENT_ID)
DEMO_FILES=(coordinator.sh screen.sh model-token.sh coordinator.tape coordinator-record.sh present.awk chapters.py splice.py invoke.mmd bat.conf)

load_env() {
	set -a
	# shellcheck disable=SC1090
	source "${ENV_FILE}"
	set +a
	local v
	for v in "${MODEL_VARS[@]}"; do
		[[ -n "${!v:-}" ]] || {
			echo "${v} is not set in ${ENV_FILE}" >&2
			exit 1
		}
	done
}

# coord MODE DEADLINE: coordinator.sh MODE on the host, its logs fetched to ${LOGS}.
coord() {
	local env_args=() v
	for v in "${MODEL_VARS[@]}" DEMO_READ DEMO_COLS DEMO_ROWS; do env_args+=(-e "${v}"); done
	mkdir -p "${ROOT}/_local" && touch "${ROOT}/_local/.hq-remote-keep"
	(cd "${ROOT}" && hq remote-test --image "${IMAGE}" --timeout "$2" "${env_args[@]}" \
		-e "AGENTSUITE_DEMO_CLUSTER=${CLUSTER}" -e "AGENTSUITE_DEMO_WORK=/work/${WORK_REL}" -e "COORD_LOGS=/work/${WORK_REL}/logs" \
		--fetch "${WORK_REL}/logs" -- examples/agentsuite-substrate/demo/coordinator.sh "$1")
}

completed() { grep -q 'DEMO COMPLETE' "${LOGS}/ch-${CHAPTERS}.log" 2>/dev/null; }

run_show() {
	load_env
	rm -f "${LOGS}"/ch-*.log
	local rc=0 status=FAILED
	coord show 1800 2>&1 | awk -f "${DEMO}/present.awk" || rc=$?
	if ((rc == 0)) && completed; then status=COMPLETE; fi
	printf '\n\e[1;%smRECORDING %s: driver exit %d\e[0m' "$([[ "${status}" == COMPLETE ]] && echo 32 || echo 31)" "${status}" "${rc}"
	sleep 120
}

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

record() {
	command -v vhs >/dev/null && command -v ffprobe >/dev/null && command -v npx >/dev/null || {
		echo "needs vhs, ffmpeg and npx" >&2
		exit 1
	}
	grep -q '^SETUP OK' "${LOGS}/setup.log" 2>/dev/null || {
		echo "no finished setup in ${LOGS}/setup.log; run coordinator.sh setup" >&2
		exit 1
	}
	load_env
	mkdir -p "${OUT}"
	local base dirty tmp="${OUT}/.recording"
	base="$(git -C "${ROOT}" rev-parse HEAD)"
	dirty="$(git -C "${ROOT}" status --porcelain | wc -l | tr -d ' ')"
	if ((dirty == 0)); then echo "commit ${base}"; else echo "a working tree based on ${base} (${dirty} files changed or new)"; fi >"${OUT}/source.txt"
	(cd "${DEMO}" && sha256 "${DEMO_FILES[@]}") >"${OUT}/source.sha256"
	rm -rf "${tmp}" "${OUT}/demo.mp4"
	mkdir -p "${tmp}"
	(cd "${ROOT}" && vhs -o "${tmp}/raw.mp4" "${DEMO}/coordinator.tape")
	completed || {
		echo "the driver did not finish; see ${tmp}/raw.mp4 and ${LOGS}" >&2
		exit 1
	}
	(cd "${tmp}" && npx -y "${MERMAID_CLI}" -q -i "${DEMO}/invoke.mmd" -o "${tmp}/invoke.png" -b transparent -s 3)
	python3 -I "${DEMO}/splice.py" "${tmp}/raw.mp4" "${OUT}/demo.mp4" "green=${tmp}/invoke.png" | tee "${tmp}/splice.tsv"
	cp "${tmp}/invoke.png" "${tmp}/splice.tsv" "${OUT}/"
	rm -rf "${tmp}"
}

mmss() { awk -v s="$1" 'BEGIN { printf "%d:%02d", s / 60, s % 60 }'; }
utc() { date -u -d "@$1" +%FT%TZ 2>/dev/null || date -u -r "$1" +%FT%TZ; }
token_scan() {
	local jwt="ey""J" field="access""_token"
	grep -rlE -e "${jwt}[A-Za-z0-9_-]{8,}" -e "${field}" -e "Bearer ${jwt}" "$@" || true
}

collect() {
	local mp4="${OUT}/demo.mp4" dur n at title f
	mkdir -p "${OUT}/logs" "${OUT}/frames" "${OUT}/scripts"
	rm -f "${OUT}"/logs/*.log "${OUT}"/logs/*.tsv
	cp "${LOGS}"/setup.log "${LOGS}"/probe.log "${LOGS}"/reset.log "${LOGS}"/show.log "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv" "${OUT}/logs/"
	(cd "${DEMO}" && cp "${DEMO_FILES[@]}" "${OUT}/scripts/")
	local same=0 changed=() sum
	while read -r sum f; do
		if [[ "$(sha256 "${DEMO}/${f}" | cut -d' ' -f1)" == "${sum}" ]]; then same=$((same + 1)); else changed+=("${f}"); fi
	done <"${OUT}/source.sha256"
	(cd "${OUT}" && sha256 demo.mp4 >demo.mp4.sha256)
	dur="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${mp4}")"
	python3 -I "${DEMO}/chapters.py" "${mp4}" "${CHAPTERS}" >"${OUT}/chapters.tsv"
	[[ "$(cut -f1 "${OUT}/chapters.tsv" | paste -sd ' ' -)" == "$(seq 1 "${CHAPTERS}" | paste -sd ' ' -)" ]] || {
		echo "chapters.tsv does not list chapters 1-${CHAPTERS}" >&2
		exit 1
	}
	while IFS=$'\t' read -r n at; do
		ffmpeg -nostdin -v error -y -ss "$(awk -v t="${at}" 'BEGIN { print t + 1.5 }')" -i "${mp4}" -frames:v 1 "${OUT}/frames/ch-${n}.png"
	done <"${OUT}/chapters.tsv"
	local decode_rc=0 decode_out video source scan mp4_hits head_short
	decode_out="$(ffmpeg -v error -i "${mp4}" -f null - 2>&1)" || decode_rc=$?
	video="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height,r_frame_rate -of csv=p=0 "${mp4}")"
	source="$(cat "${OUT}/source.txt")"
	scan="$(token_scan "${OUT}/logs" "${OUT}/scripts" "${OUT}/source.txt")"
	mp4_hits="$({ LC_ALL=C grep -a -o -E "ey""J[A-Za-z0-9_-]{8,}" "${mp4}" || true; } | wc -l | tr -d ' ')"
	head_short="$(git -C "${ROOT}" rev-parse --short HEAD)"
	load_env
	{
		echo "# AgentSuite coordinator on AgentSessions and agent-substrate micro-VMs: demo"
		echo
		echo "To share:"
		echo
		echo "> A PoC of the coordinator suite on AgentSessions and agent-substrate micro-VMs. It starts from"
		echo "> kaimahi's coordinator-workers test suite (agentsuite/testdata at 75f27d6), where agent \`coordinator\`"
		echo "> may invoke \`writer\` and \`reviewer\`, each with maxConcurrent 1 and maxDepth 1, validated by kaimahi's"
		echo "> own package. \`emit\`, the PoC's translator, renders one substrate ActorTemplate per agent and the"
		echo "> daemon's invocation policy; which delegation needs approval is a deployment choice (\`-approve"
		echo "> reviewer\`). One request to the coordinator (${AGENTSUITE_MODEL} on Azure OpenAI) delegates to writer"
		echo "> and then reviewer: each delegation is a host-executed tool call that creates a child session on the"
		echo "> target agent's harness, in its own micro-VM, and is journaled in the coordinator's session. The"
		echo "> reviewer call parks on AgentSessions' approval gate until an operator approves it with agentctl; in a"
		echo "> second run the operator denies it, the denial is journaled, and the coordinator finishes without the"
		echo "> review. The PoC's tests show maxConcurrent and maxDepth refusals, and the video ends with the open"
		echo "> questions for AgentSuite, AgentSessions and substrate. Everything runs live."
		echo
		echo "- Video: demo.mp4, $(mmss "${dur}") ($(awk -v d="${dur}" 'BEGIN { printf "%.1f", d }') s), sha256 \`$(cut -d' ' -f1 "${OUT}/demo.mp4.sha256")\`"
		echo "- Recorded $(utc "$(head -1 "${LOGS}/timeline.tsv" | cut -f2)") with \`coordinator-record.sh\` from ${source}."
		if ((${#changed[@]} == 0)); then
			echo "  All ${same} demo files in source.sha256 are byte-identical at ${head_short}."
		else
			echo "  At ${head_short}, ${#changed[@]} of the demo files differ from the recording's: ${changed[*]}."
		fi
		echo "- Heads used: the branch is aramase/agentsessions main b0d9e94 plus Tom's approval engine (#131 at"
		echo "  b9eeb5f) and approval controls (#132 at de57e31), with the v3 demo branch merged (87855f1); the"
		echo "  suite is kaimahi 75f27d6 (suites/coordinator-workers/SOURCE has each file's blob id and sha256)."
		echo "- logs/: setup.log, probe.log (an approve and a deny run through agentctl, off camera), reset.log,"
		echo "  show.log, one ch-N.log per chapter with the full output of every command, timeline.tsv; frames/:"
		echo "  a frame 1.5 s into each chapter; invoke.png: the diagram as spliced; scripts/: the demo files."
		echo
		echo "| Chapter | Starts at | Title |"
		echo "|---|---|---|"
		while IFS=$'\t' read -r n _ title; do
			at="$(awk -v k="${n}" '$1 == k { print $2 }' "${OUT}/chapters.tsv")"
			printf '| %s | %s | %s |\n' "${n}" "$([[ -n "${at}" ]] && mmss "${at}" || echo '?')" "${title}"
		done <"${LOGS}/timeline.tsv"
		echo
		echo "Chapter k starts at the first frame whose top-row progress bar is k/${CHAPTERS} full (chapters.py)."
		echo
		echo "## How it was made"
		echo
		echo "- Off camera (\`coordinator.sh setup\`, logs/setup.log): a kind cluster on a Linux VM with KVM,"
		echo "  substrate with Kata + Cloud Hypervisor micro-VMs, the suite validated, one harness image and one"
		echo "  ActorTemplate per agent (each in its own atespace), a model token from the VM's managed identity"
		echo "  (only its size is logged), and agentsessionsd with the three -substrate-harness flags and the"
		echo "  invocation executor. \`$(grep -m1 '^SETUP OK' "${LOGS}/setup.log")\`; probe: \`$(grep -m1 '^PROBE OK' "${LOGS}/probe.log" || echo 'no PROBE OK')\`."
		echo "- On camera (\`coordinator-record.sh record\`): \`coordinator.sh show\` on the VM through \`hq remote-test\`,"
		echo "  rendered by vhs here. Each command on screen is the command line that runs; the driver stops on"
		echo "  any claim that does not hold. \$API, \$IMAGE and \$BINDING are set from setup's state (show.log);"
		echo "  \$S and \$DENIED by commands on screen; \$CHILD from the writer result chapter 4 shows (ch-5.log)."
		echo "- The diagram (invoke.mmd) is rendered with ${MERMAID_CLI} and spliced over the block chapter 4"
		echo "  draws for it (splice.py): $(awk -F'\t' '{ printf "%s-%s s", $2, $3 }' "${OUT}/splice.tsv")."
		echo "- Waits are hidden between a status line \`⠿ <what> …\` and its \`✓ <what> in N s\`."
		echo
		echo "## Checks"
		echo
		echo "- \`ffprobe\`: ${video}, ${dur} s. \`ffmpeg -v error -i demo.mp4 -f null -\`: exit ${decode_rc}, $(printf '%s' "${decode_out}" | grep -c '') lines of output."
		echo "- Chapter starts: chapters.py found chapters 1-${CHAPTERS}, in order."
		echo "- Token scan (a JWT prefix, the metadata token field, a bearer header) over logs/, scripts/ and"
		echo "  source.txt: ${scan:-no matches}. demo.mp4: ${mp4_hits} JWT-shaped strings."
	} >"${OUT}/README.md"
	echo "wrote ${OUT}/README.md"
}

case "${1:-}" in
show)
	load_env
	rm -f "${LOGS}"/ch-*.log
	coord show 1800 2>&1 | awk -f "${DEMO}/present.awk"
	completed
	;;
reset)
	load_env
	coord reset 600
	;;
record) record ;;
collect) collect ;;
run) run_show ;;
*)
	echo "usage: $0 show|reset|record|collect|run" >&2
	exit 2
	;;
esac
