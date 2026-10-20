#!/usr/bin/env bash
# Records the demo: driver.sh setup off camera, then driver.sh show under vhs (demo.tape), the two
# diagrams spliced in, then the video, logs and README, then teardown.
#
#   record.sh            setup, record, collect, then teardown (DEMO_KEEP=1 keeps the cluster)
#   record.sh setup      driver.sh setup: cluster, substrate, the micro-VM smoke test, the memory
#                        tier's template and the daemons; fails unless setup.log holds SMOKE OK and
#                        SETUP OK
#   record.sh show       driver.sh show without recording it, rendered here (a dry run); run
#                        record.sh reset after it
#   record.sh reset      driver.sh reset: removes what show created, so show can run again
#   record.sh record     vhs demo.tape, then build.mmd and run.mmd rendered and spliced in: demo.mp4
#                        (needs a finished setup and a reset cluster)
#   record.sh collect    the logs, the scripts, the diagrams, demo.mp4.sha256, the checks and
#                        README.md with chapter times
#   record.sh teardown   driver.sh teardown; refused when DEMO_OUT holds a recording whose cluster
#                        ran elsewhere (another DEMO_REMOTE, or another DEMO_HOST for hq)
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
OUT="${DEMO_OUT:-${HOME}/.local/state/hq/demos/$(date +%F)-poc}"
ENV_FILE="${AZURE_OPENAI_ENV:-${HOME}/.config/hq/azure-openai.env}"
REMOTE="${DEMO_REMOTE:-local}"
# demo.tape's terminal is 130 columns by 39 rows; the driver draws one column fewer, so a full row
# never wraps.
export DEMO_COLS=129 DEMO_ROWS=39 DEMO_READ="${DEMO_READ:-3}"
CHAPTERS=11
IMAGE=agentsuite-demo-tools:3
MERMAID_CLI="@mermaid-js/mermaid-cli@11.17.0"
# The work directory on the host, relative to the checkout: git-ignored, so hq remote-test keeps it
# between jobs when the same ignored directory exists here.
WORK_REL=_local/demo-work
LOGS="${ROOT}/${WORK_REL}/demo-logs"
MODEL_VARS=(AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AZURE_MI_CLIENT_ID)
# The files the recording runs or shows, hashed at record time (source.sha256) and copied by collect.
DEMO_FILES=(driver.sh present.awk demo.tape record.sh chapters.py splice.py build.mmd run.mmd bat.conf
	split-view.py trace-turn.sh trace-hop.py image-files.sh wait-golden.sh smoke-microvm.sh Dockerfile.tools diagrams.md)

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
		for v in "${MODEL_VARS[@]}" DEMO_READ DEMO_COLS DEMO_ROWS; do env_args+=(-e "${v}"); done
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
	# A reset.log is only ever from a reset after the current setup (record notes it as such);
	# driver.sh setup removes the one in DEMO_LOGS, this the local copy.
	rm -f "${LOGS}/setup.log" "${LOGS}/reset.log"
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

# sha256 FILE...: "<hex>  FILE" lines, with GNU coreutils or BSD shasum.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# target: where driver.sh runs, as record saves it in remote.txt: DEMO_REMOTE, then for hq the
# sha256 of DEMO_HOST, so teardown can match the host without the artifacts naming it.
target() {
	echo "${REMOTE}"
	if [[ "${REMOTE}" == hq ]]; then printf '%s' "${DEMO_HOST}" | sha256 | cut -d' ' -f1; fi
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
	# What the recording runs, taken now: collect may run after later commits. source.sha256 holds the
	# demo files as they were; collect compares them with the commit it runs at.
	local base dirty
	base="$(git -C "${ROOT}" rev-parse HEAD)"
	dirty="$(git -C "${ROOT}" status --porcelain | wc -l | tr -d ' ')"
	if ((dirty == 0)); then echo "commit ${base}"; else echo "a working tree based on ${base} (${dirty} files changed or new)"; fi >"${OUT}/source.txt"
	(cd "${DEMO}" && sha256 "${DEMO_FILES[@]}") >"${OUT}/source.sha256"
	# Whether a driver.sh reset succeeded on this cluster since setup, taken now, before the
	# recording: the README says so only when logs/reset.log is there.
	mkdir -p "${OUT}/logs"
	rm -f "${OUT}/logs/reset.log"
	# Where driver.sh runs (DEMO_REMOTE, and DEMO_HOST for hq), taken now: collect and teardown may
	# run later with other settings. A teardown log in ${OUT} is from an earlier recording; teardown
	# copies this one's.
	target >"${OUT}/remote.txt"
	rm -f "${OUT}/logs/teardown.log"
	if grep -q '^RESET OK' "${LOGS}/reset.log" 2>/dev/null; then cp "${LOGS}/reset.log" "${OUT}/logs/"; fi
	local tmp="${OUT}/.recording"
	rm -rf "${tmp}" "${OUT}/demo.mp4"
	mkdir -p "${tmp}"
	(cd "${ROOT}" && vhs -o "${tmp}/raw.mp4" "${DEMO}/demo.tape")
	completed || {
		echo "the driver did not finish; see ${tmp}/raw.mp4 and ${LOGS}" >&2
		exit 1
	}
	# The diagrams on a transparent background; splice.py puts them on the terminal's own background.
	local d
	for d in build run; do
		(cd "${tmp}" && npx -y "${MERMAID_CLI}" -q -i "${DEMO}/${d}.mmd" -o "${tmp}/${d}.png" -b transparent -s 3)
	done
	python3 -I "${DEMO}/splice.py" "${tmp}/raw.mp4" "${OUT}/demo.mp4" "magenta=${tmp}/build.png" "green=${tmp}/run.png" | tee "${tmp}/splice.tsv"
	cp "${tmp}/build.png" "${tmp}/run.png" "${tmp}/splice.tsv" "${OUT}/"
	rm -rf "${tmp}"
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
	local mp4="${OUT}/demo.mp4" dur n at title f
	mkdir -p "${OUT}/logs" "${OUT}/frames" "${OUT}/scripts"
	rm -rf "${OUT}"/logs/ch-*.log "${OUT}/logs/trace"
	cp "${LOGS}"/setup.log "${LOGS}"/show.log "${LOGS}"/ch-*.log "${LOGS}/timeline.tsv" "${OUT}/logs/"
	(cd "${DEMO}" && cp "${DEMO_FILES[@]}" "${OUT}/scripts/")
	# Each demo file as recorded against the same file at this commit.
	local same=0 changed=()
	while read -r sum f; do
		if [[ "$(sha256 "${DEMO}/${f}" | cut -d' ' -f1)" == "${sum}" ]]; then same=$((same + 1)); else changed+=("${f}"); fi
	done <"${OUT}/source.sha256"
	(cd "${OUT}" && sha256 demo.mp4 >demo.mp4.sha256)
	dur="$(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${mp4}")"
	# Chapter k starts at the first frame whose progress bar is k/11 full; a frame 1.5 s later is kept
	# for each, to check against the table. collect fails unless every chapter is found, in order.
	python3 -I "${DEMO}/chapters.py" "${mp4}" >"${OUT}/chapters.tsv"
	[[ "$(cut -f1 "${OUT}/chapters.tsv" | paste -sd ' ' -)" == "$(seq 1 "${CHAPTERS}" | paste -sd ' ' -)" ]] || {
		echo "chapters.tsv does not list chapters 1-${CHAPTERS}" >&2
		exit 1
	}
	awk -F'\t' 'NR > 1 && $2 + 0 <= prev { bad = 1 } { prev = $2 + 0 } END { exit bad }' "${OUT}/chapters.tsv" || {
		echo "the chapters do not start in order" >&2
		exit 1
	}
	while IFS=$'\t' read -r n at; do
		ffmpeg -nostdin -v error -y -ss "$(awk -v t="${at}" 'BEGIN { print t + 1.5 }')" -i "${mp4}" -frames:v 1 "${OUT}/frames/ch-${n}.png"
	done <"${OUT}/chapters.tsv"
	local decode_rc=0 decode_out
	decode_out="$(ffmpeg -v error -i "${mp4}" -f null - 2>&1)" || decode_rc=$?
	local video source
	video="$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height,r_frame_rate -of csv=p=0 "${mp4}")"
	source="$(cat "${OUT}/source.txt" 2>/dev/null || echo "an unrecorded source (no source.txt)")"
	# Where setup and show ran, as record noted it in remote.txt's first line.
	local cluster_host show_host
	case "$(head -1 "${OUT}/remote.txt" 2>/dev/null)" in
	hq)
		cluster_host="on a Linux VM with KVM, through \`hq remote-test\`"
		show_host="on the VM through \`hq remote-test\`"
		;;
	local)
		cluster_host="on the machine that ran record.sh"
		show_host="on the machine that ran record.sh"
		;;
	*)
		cluster_host="on a host record.sh did not note (no remote.txt)"
		show_host="on a host record.sh did not note (no remote.txt)"
		;;
	esac
	local scan mp4_hits mp4_prefix
	scan="$(token_scan "${OUT}/logs" "${OUT}/scripts" "${OUT}/source.txt")"
	# A JWT in the MP4's bytes is its prefix and at least 8 more base64url characters; the bare 3-byte
	# prefix also turns up by chance in compressed video, so it is counted but is not a finding.
	mp4_hits="$({ LC_ALL=C grep -a -o -E "ey""J[A-Za-z0-9_-]{8,}" "${mp4}" || true; } | wc -l | tr -d ' ')"
	mp4_prefix="$({ LC_ALL=C grep -a -o "ey""J" "${mp4}" || true; } | wc -l | tr -d ' ')"
	local max_rows max_trace max_caption
	# Each screen's row count is logged with its limit: 5, or 7 on the trace screen (run -n 7).
	max_rows="$(sed -n 's/^(on screen: [0-9]* rows, \([0-9]*\) on this screen, limit 5)$/\1/p' "${OUT}"/logs/ch-*.log | sort -n | tail -1)"
	max_trace="$(sed -n 's/^(on screen: [0-9]* rows, \([0-9]*\) on this screen, limit 7)$/\1/p' "${OUT}/logs/ch-4.log" | sort -n | tail -1)"
	[[ -n "${max_rows}" && -n "${max_trace}" ]] || {
		echo "the chapter logs record no screen with limit 5, or no trace screen with limit 7" >&2
		exit 1
	}
	if sed -n 's/^(on screen: [0-9]* rows, [0-9]* on this screen, limit \([0-9]*\))$/\1/p' "${OUT}"/logs/ch-*.log | grep -qv -e '^5$' -e '^7$'; then
		echo "a chapter log records a screen limit other than 5 or 7" >&2
		exit 1
	fi
	max_caption="$(sed -n 's/^## screen: //p' "${OUT}"/logs/ch-*.log | awk '{ print length($0) }' | sort -n | tail -1)"
	load_env
	{
		echo "# AgentSuite + AgentSessions + agent-substrate micro-VMs: demo"
		echo
		echo "To share:"
		echo
		echo "> How an agent defined with AgentSuite becomes an agent-substrate workload and runs as durable"
		echo "> AgentSessions sessions on micro-VMs. It starts from kaimahi's AgentSuite test suite (testdata/minimal,"
		echo "> agent \`writer\`), shown file by file and validated by kaimahi's own package, plus the one input this"
		echo "> PoC adds, a draft AgentSessionsBinding. \`emit\`, the PoC's translator built on kaimahi's agentsuite"
		echo "> package, renders them into a substrate ActorTemplate, and a split screen maps each input line to the"
		echo "> template line it became. The same suite renders a second, memory-tier template; a composition that"
		echo "> does not exist is refused; and the harness refuses tampered instructions by their digest. The"
		echo "> template is then created in the cluster and registered with agentsessionsd by hand, a step that will"
		echo "> be automated. A real turn is traced through every component's own log, from agentctl through"
		echo "> agentsessionsd, ate-api, the worker and the router to the harness in its Kata + Cloud Hypervisor"
		echo "> micro-VM and the model (${AGENTSUITE_MODEL} on Azure OpenAI). The rest is the session lifecycle: a chat, the"
		echo "> hash-chained journal, a pause to a memory snapshot and a resume with the conversation still in RAM, a"
		echo "> replay with zero model calls, a fork, AgentSessions refusing a turn when a session's micro-VM is"
		echo "> swapped underneath it, and what the AgentSuite proposal needs. Everything runs live."
		echo
		echo "- Video: demo.mp4, $(mmss "${dur}") ($(awk -v d="${dur}" 'BEGIN { printf "%.1f", d }') s), sha256 \`$(cut -d' ' -f1 "${OUT}/demo.mp4.sha256")\`"
		echo "- Recorded $(utc "$(head -1 "${LOGS}/timeline.tsv" | cut -f2)") with \`record.sh\` from ${source}."
		local head_short scripts_note
		head_short="$(git -C "${ROOT}" rev-parse --short HEAD)"
		if ((${#changed[@]} == 0)); then
			echo "  All ${same} demo files in source.sha256 are byte-identical at ${head_short}."
			scripts_note="the demo files at ${head_short}, the same bytes the recording ran"
		else
			echo "  At ${head_short}, ${#changed[@]} of the demo files differ from the recording's: ${changed[*]}."
			scripts_note="the demo files at ${head_short}, which differ from the recording's only in ${changed[*]} (source.sha256 has the recording's hashes)"
		fi
		echo "- logs/: setup.log (off camera), show.log, one ch-N.log per chapter with the full output of every"
		echo "  command (ch-4.log holds every component's full log for the traced turn), timeline.tsv; frames/: a"
		echo "  frame 1.5 s into each chapter; build.png and run.png: the diagrams as spliced; scripts/:"
		echo "  ${scripts_note}, including the Mermaid sources build.mmd and run.mmd"
		echo
		echo "| Chapter | Starts at | Title |"
		echo "|---|---|---|"
		while IFS=$'\t' read -r n _ title; do
			at="$(awk -v k="${n}" '$1 == k { print $2 }' "${OUT}/chapters.tsv")"
			printf '| %s | %s | %s |\n' "${n}" "$([[ -n "${at}" ]] && mmss "${at}" || echo '?')" "${title}"
		done <"${LOGS}/timeline.tsv"
		echo
		echo "Chapter times come from the video itself: chapter k starts at the first frame whose top-row progress"
		echo "bar is k/${CHAPTERS} full (chapters.py); frames/ch-k.png is 1.5 s after that frame."
		echo
		echo "## How it was made"
		echo
		echo "- Off camera, before recording (\`record.sh setup\`, logs/setup.log): a kind cluster ${cluster_host},"
		echo "  substrate's install with Kata + Cloud Hypervisor micro-VMs, the micro-VM smoke test,"
		echo "  the suite against kaimahi 9c5c993, the harness image, the memory tier's ActorTemplate (the"
		echo "  stateless one is only rendered: chapter 3 creates it), a model token from the host's managed"
		echo "  identity (only its size and expiry are logged), both agentsessionsd Deployments, and the"
		echo "  harness binary copied out of the harness image."
		if [[ -f "${OUT}/logs/reset.log" ]]; then
			echo "  \`driver.sh reset\` (logs/reset.log) ran after setup and before the recording."
		else
			echo "  No successful \`driver.sh reset\` ran between setup and the recording."
		fi
		echo "  \`$(grep -m1 '^SMOKE OK' "${LOGS}/setup.log")\`; \`$(grep -m1 '^SETUP OK' "${LOGS}/setup.log")\`."
		echo "- On camera (\`record.sh record\`): \`driver.sh show\` ${show_host}, rendered by vhs on the"
		echo "  machine that ran record.sh. Each command on screen is the command line that runs; the driver"
		echo "  stops on any claim that does not hold, and writes the full output of each command to"
		echo "  logs/ch-N.log. Files are printed by bat with scripts/bat.conf (colors, line numbers, wrapping at"
		echo "  the terminal's width); split screens are scripts/split-view.py. \$IMAGE, \$TEMPLATE, \$MEMTEMPLATE,"
		echo "  \$API, \$API_MEM and \$ROUTER are set from setup's state before the title card (show.log has their"
		echo "  values); \$CHILD is read from the line \`agentctl fork\` prints on screen (ch-9.log has it); the"
		echo "  other variables are set by a command on screen."
		echo "- The diagrams (build.mmd, run.mmd) are rendered with ${MERMAID_CLI} and spliced over the blocks"
		echo "  chapters 2 and 4 draw for them (splice.py): $(awk -F'\t' '{ printf "%sthe %s diagram %s-%s s", (NR > 1 ? ", " : ""), ($1 == "magenta" ? "build" : $1 == "green" ? "run" : $1), $2, $3 }' "${OUT}/splice.tsv")."
		echo "- Waits longer than a moment are hidden between a status line \`⠿ <what> …\` and its \`✓ <what> in N s\`;"
		echo "  that line is the driver's, not a command's output, so it does not count toward a screen's output rows."
		teardown_line
		echo
		cat "${DEMO}/diagrams.md"
		echo
		echo "## Checks"
		echo
		echo "- \`ffprobe\`: ${video}, ${dur} s. \`ffmpeg -v error -i demo.mp4 -f null -\`: exit ${decode_rc}, $(printf '%s' "${decode_out}" | grep -c '') lines of output."
		echo "- Chapter starts: chapters.py found chapters 1-${CHAPTERS}, in order (collect fails otherwise)."
		echo "- Screens: at most ${max_rows} output rows on any screen outside a file screen and the trace (limit 5),"
		echo "  ${max_trace} on chapter 4's trace screen (limit 7); file screens show their file whole. The longest"
		echo "  caption is ${max_caption} of $((DEMO_COLS - 2)) columns."
		echo "- Token scan (a JWT prefix, the metadata token field, a bearer header) over logs/, scripts/ and"
		echo "  source.txt: ${scan:-no matches}. demo.mp4: ${mp4_hits} JWT-shaped strings (the prefix and 8 or more"
		echo "  base64url characters); the bare 3-byte prefix occurs ${mp4_prefix} times, by chance in the video data."
		echo "- Smoke pre-check: \`$(grep -m1 '^SMOKE OK' "${LOGS}/setup.log")\`"
	} >"${OUT}/README.md"
	echo "wrote ${OUT}/README.md"
}

# teardown_line: the README's teardown line, from what ${OUT}/logs holds now. collect writes it, and
# teardown rewrites it once the cluster is gone, so it never states a teardown that has not run.
teardown_line() {
	if grep -q '^TEARDOWN OK' "${OUT}/logs/teardown.log" 2>/dev/null; then
		echo "- Teardown (\`record.sh teardown\`, logs/teardown.log): the cluster was deleted (TEARDOWN OK)."
	elif [[ -n "${DEMO_KEEP:-}" ]]; then
		echo "- Teardown: none ran, since DEMO_KEEP is set; the cluster was kept."
	else
		echo "- Teardown: none had run when this README was written; \`record.sh teardown\` rewrites this line."
	fi
}

teardown() {
	load_env
	# The recording in ${OUT} names where its cluster runs (remote.txt). A teardown anywhere else can
	# print TEARDOWN OK without touching that cluster (down.sh finds nothing to delete there), so it
	# is refused before it runs rather than recorded as this recording's teardown.
	if [[ -f "${OUT}/remote.txt" ]]; then
		local recorded
		recorded="$(head -1 "${OUT}/remote.txt")"
		if [[ "${recorded}" != "${REMOTE}" ]]; then
			echo "the recording in ${OUT} ran driver.sh with DEMO_REMOTE=${recorded}, not ${REMOTE}; run teardown with its DEMO_REMOTE (and DEMO_HOST), or another DEMO_OUT" >&2
			exit 1
		fi
		if [[ "$(cat "${OUT}/remote.txt")" != "$(target)" ]]; then
			echo "the recording in ${OUT} ran driver.sh on another DEMO_HOST (or remote.txt does not record its host); run teardown with that DEMO_HOST, or another DEMO_OUT" >&2
			exit 1
		fi
	fi
	driver teardown 900
	# Without remote.txt, nothing ties this teardown to what ${OUT} holds, so it is left as it is.
	[[ -f "${OUT}/remote.txt" ]] || return 0
	mkdir -p "${OUT}/logs"
	rm -f "${OUT}/logs/teardown.log"
	if grep -q '^TEARDOWN OK' "${LOGS}/teardown.log" 2>/dev/null; then cp "${LOGS}/teardown.log" "${OUT}/logs/"; fi
	if [[ -f "${OUT}/README.md" ]]; then
		# teardown_line is one line, and the only one in the README that starts "- Teardown".
		awk -v line="$(teardown_line)" '/^- Teardown/ { print line; next } { print }' \
			"${OUT}/README.md" >"${OUT}/README.md.tmp"
		mv "${OUT}/README.md.tmp" "${OUT}/README.md"
	fi
}

check_remote
case "${1:-}" in
setup) setup ;;
show) dry_run ;;
reset)
	load_env
	driver reset 600
	;;
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
	echo "usage: $0 [setup|show|reset|record|collect|teardown|run]" >&2
	exit 2
	;;
esac
