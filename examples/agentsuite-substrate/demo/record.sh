#!/usr/bin/env bash
# Records driver.sh to an MP4 with vhs, and collects what the run left: the per-step logs, a README
# with each step's offset in the video, and the video's sha256.
#
#   record.sh            record demo.tape, then collect
#   record.sh run        what demo.tape types: run driver.sh, fetch its logs, then print the end
#                        marker the tape waits for
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
# is fetched on the host by the driver and never reaches this machine. DEMO_PAUSE and DEMO_RUN_PAUSE
# (see driver.sh) default to 25 and 8 seconds here, which paces the run at about 25 minutes.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${DEMO}/../../.." && pwd)"
OUT="${DEMO_OUT:-${HOME}/.local/state/hq/demos/$(date +%F)-poc}"
ENV_FILE="${AZURE_OPENAI_ENV:-${HOME}/.config/hq/azure-openai.env}"
REMOTE="${DEMO_REMOTE:-local}"
export DEMO_PAUSE="${DEMO_PAUSE:-25}" DEMO_RUN_PAUSE="${DEMO_RUN_PAUSE:-8}"
# The deadline for the driver, and so for the tape's wait (demo.tape waits a little longer).
DEADLINE=3000
IMAGE=agentsuite-demo-tools:2
# The work directory on the host, relative to the checkout: git-ignored, so hq remote-test keeps it
# between jobs when the same ignored directory exists here.
WORK_REL=_local/demo-work
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

run_driver() {
	load_env
	mkdir -p "${OUT}" "${ROOT}/${WORK_REL}"
	date -u +%s >"${OUT}/record-start"
	local rc=0
	if [[ "${REMOTE}" == local ]]; then
		AGENTSUITE_DEMO_WORK="${ROOT}/${WORK_REL}" DEMO_LOGS="${ROOT}/${WORK_REL}/demo-logs" \
			timeout "${DEADLINE}" "${DEMO}/driver.sh" || rc=$?
	else
		touch "${ROOT}/_local/.hq-remote-keep"
		local env_args=()
		for v in "${MODEL_VARS[@]}" DEMO_PAUSE DEMO_RUN_PAUSE; do env_args+=(-e "${v}"); done
		(cd "${ROOT}" && hq remote-test --image "${IMAGE}" --timeout "${DEADLINE}" "${env_args[@]}" \
			-e "AGENTSUITE_DEMO_WORK=/work/${WORK_REL}" -e "DEMO_LOGS=/work/${WORK_REL}/demo-logs" \
			--fetch "${WORK_REL}/demo-logs" -- examples/agentsuite-substrate/demo/driver.sh) || rc=$?
	fi
	# The end marker demo.tape waits for. vhs 0.12.1's Wait+Screen stops the terminal from drawing
	# streamed output, so the tape waits with Wait+Line, which matches the cursor's line: the marker is
	# printed without a newline and held until vhs stops the recording.
	local status=FAILED
	if ((rc == 0)) && grep -q 'DEMO COMPLETE' "${ROOT}/${WORK_REL}/demo-logs/step-10.log" 2>/dev/null; then
		status=COMPLETE
	fi
	printf '\n\e[1;%smRECORDING %s: driver exit %d, logs fetched to %s\e[0m' \
		"$([[ "${status}" == COMPLETE ]] && echo 32 || echo 31)" "${status}" "${rc}" "${WORK_REL}/demo-logs"
	sleep 120
}

# sha256 FILE: "<hex>  FILE", with GNU coreutils or BSD shasum.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi
}
# utc EPOCH: the time in RFC 3339, with GNU or BSD date.
utc() { date -u -d "@$1" +%FT%TZ 2>/dev/null || date -u -r "$1" +%FT%TZ; }

collect() {
	local logs="${ROOT}/${WORK_REL}/demo-logs" start mp4="${OUT}/demo.mp4"
	mkdir -p "${OUT}/logs"
	cp "${logs}"/step-*.log "${logs}/timeline.tsv" "${OUT}/logs/"
	cp "${DEMO}/driver.sh" "${DEMO}/demo.tape" "${DEMO}/record.sh" "${DEMO}/smoke-microvm.sh" "${DEMO}/Dockerfile.tools" "${OUT}/"
	(cd "${OUT}" && sha256 demo.mp4 >demo.mp4.sha256)
	start="$(cat "${OUT}/record-start")"
	{
		echo "# AgentSuite + AgentSessions + agent-substrate micro-VMs: recorded demo"
		echo
		echo "- Video: demo.mp4, $(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${mp4}" | awk '{ printf "%d:%02d", $1 / 60, $1 % 60 }'), sha256 \`$(cut -d' ' -f1 "${OUT}/demo.mp4.sha256")\`"
		echo "- Recorded $(utc "${start}") from \`record.sh\` ($(git -C "${ROOT}" rev-parse --short HEAD)); logs in logs/, one per step, redacted"
		echo
		echo "| Step | Starts at | What |"
		echo "|---|---|---|"
		while IFS=$'\t' read -r n at title; do
			local off=$((at - start))
			printf '| %s | %d:%02d | %s |\n' "${n}" $((off / 60)) $((off % 60)) "${title}"
		done <"${logs}/timeline.tsv"
		echo
		echo "Offsets are the remote host's step start minus this machine's recording start (vhs starts typing about a second later), so they are accurate to a few seconds."
	} >"${OUT}/README.md"
}

case "${1:-}" in
run) run_driver ;;
"")
	if ! command -v vhs >/dev/null || ! command -v ffprobe >/dev/null; then
		echo "needs vhs and ffmpeg" >&2
		exit 1
	fi
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
	load_env
	mkdir -p "${OUT}"
	if [[ "${REMOTE}" == hq ]]; then
		ssh -o BatchMode=yes "${DEMO_HOST}" "docker build -q -t ${IMAGE} -" <"${DEMO}/Dockerfile.tools"
	fi
	rm -rf "${ROOT}/${WORK_REL}/demo-logs" "${OUT}/demo.mp4"
	(cd "${ROOT}" && DEMO_OUT="${OUT}" vhs -o "${OUT}/demo.mp4" "${DEMO}/demo.tape")
	grep -q 'DEMO COMPLETE' "${ROOT}/${WORK_REL}/demo-logs/step-10.log" 2>/dev/null || {
		echo "the driver did not finish; see the video and ${ROOT}/${WORK_REL}/demo-logs" >&2
		exit 1
	}
	collect
	echo "wrote ${OUT}"
	;;
*)
	echo "usage: $0 [run]" >&2
	exit 2
	;;
esac
