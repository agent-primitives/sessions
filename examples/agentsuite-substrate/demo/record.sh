#!/usr/bin/env bash
# Records driver.sh to an MP4 with vhs, and collects what the run left: the per-step logs, a README
# with each step's offset in the video, and the video's sha256.
#
#   record.sh            build the tools image, record demo.tape, then collect (run on the Mac)
#   record.sh run        what demo.tape types: run driver.sh on the KVM host and fetch its logs
#
# The driver needs Docker with /dev/kvm, kind and the cluster on one host. With DEMO_REMOTE=hq (the
# default) it runs there through `hq remote-test`, in the image Dockerfile.tools builds on that host;
# with DEMO_REMOTE=local it runs here. The model settings come from AZURE_OPENAI_ENV, a file of
# non-secret NAME=value lines (AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL, AGENTSUITE_MODEL_PATH,
# AZURE_MI_CLIENT_ID); the token is fetched on the host by the driver and never reaches this machine.
set -euo pipefail
DEMO="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "${DEMO}/../../.." && pwd)"
OUT="${DEMO_OUT:-${HOME}/.local/state/hq/demos/$(date +%F)-poc}"
ENV_FILE="${AZURE_OPENAI_ENV:-${HOME}/.config/hq/azure-openai.env}"
REMOTE="${DEMO_REMOTE:-hq}"
HOST="${DEMO_HOST:-zephyr01.southcentralus.cloudapp.azure.com}"
IMAGE=hq-agentsuite-demo-tools:2026-10-14
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
	if [[ "${REMOTE}" == local ]]; then
		AGENTSUITE_DEMO_WORK="${ROOT}/${WORK_REL}" DEMO_LOGS="${ROOT}/${WORK_REL}/demo-logs" "${DEMO}/driver.sh"
		return
	fi
	touch "${ROOT}/_local/.hq-remote-keep"
	local env_args=()
	for v in "${MODEL_VARS[@]}"; do env_args+=(-e "${v}"); done
	cd "${ROOT}"
	hq remote-test --image "${IMAGE}" --timeout 7200 "${env_args[@]}" \
		-e "AGENTSUITE_DEMO_WORK=/work/${WORK_REL}" -e "DEMO_LOGS=/work/${WORK_REL}/demo-logs" \
		--fetch "${WORK_REL}/demo-logs" -- examples/agentsuite-substrate/demo/driver.sh
}

collect() {
	local logs="${ROOT}/${WORK_REL}/demo-logs" start mp4="${OUT}/demo.mp4"
	mkdir -p "${OUT}/logs"
	cp "${logs}"/step-*.log "${logs}/timeline.tsv" "${OUT}/logs/"
	cp "${DEMO}/driver.sh" "${DEMO}/demo.tape" "${DEMO}/record.sh" "${DEMO}/smoke-microvm.sh" "${DEMO}/Dockerfile.tools" "${OUT}/"
	(cd "${OUT}" && shasum -a 256 demo.mp4 >demo.mp4.sha256)
	start="$(cat "${OUT}/record-start")"
	{
		echo "# AgentSuite + AgentSessions + agent-substrate micro-VMs: recorded demo"
		echo
		echo "- Video: demo.mp4, $(ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${mp4}" | awk '{ printf "%d:%02d", $1 / 60, $1 % 60 }'), sha256 \`$(cut -d' ' -f1 "${OUT}/demo.mp4.sha256")\`"
		echo "- Recorded $(date -u -r "${start}" +%FT%TZ) from \`record.sh\` ($(git -C "${ROOT}" rev-parse --short HEAD)); logs in logs/, one per step, redacted"
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
	command -v vhs >/dev/null && command -v ffprobe >/dev/null || {
		echo "needs vhs and ffmpeg" >&2
		exit 1
	}
	load_env
	mkdir -p "${OUT}"
	if [[ "${REMOTE}" == hq ]]; then
		ssh -o BatchMode=yes "${HOST}" "docker build -q -t ${IMAGE} -" <"${DEMO}/Dockerfile.tools"
	fi
	rm -rf "${ROOT}/${WORK_REL}/demo-logs"
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
