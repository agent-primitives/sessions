#!/usr/bin/env bash
# Verify journal persistence and continuation, not reconstruction of the surviving actor.
set -euo pipefail
source "$(dirname "$0")/common.sh"
timeout_command

# Bound the entire smoke, including builds and rollouts, in addition to individual API calls.
budget="${SMOKE_TIMEOUT_SECONDS:-900}"
[[ "${budget}" =~ ^[1-9][0-9]*$ ]] || die "SMOKE_TIMEOUT_SECONDS must be positive."
if [[ "${1:-}" != --bounded ]]; then
	exec "${TIMEOUT}" --kill-after=5s "${budget}s" bash "$0" --bounded
fi

# Keep the client binary and port-forward temporary, and clean them up on exit or interruption.
tmp="$(mktemp -d)"
pf=""
cleanup() {
	if [[ -n "${pf}" ]] && kill -0 "${pf}" 2>/dev/null; then
		kill "${pf}"
		wait "${pf}" 2>/dev/null || true
	fi
	rm -rf "${tmp}"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
progress "Building the Sessions client for the restart/persistence smoke"
go -C "${ROOT}" build -o "${tmp}/agentctl" ./cmd/agentctl
port="${LOCAL_PORT:-18080}"
ctl() { local cmd="$1"; shift; "${TIMEOUT}" --kill-after=1s 120s "${tmp}/agentctl" "${cmd}" --project "${PROJECT}" --server "127.0.0.1:${port}" "$@"; }

# Wait for a successful Sessions RPC, not just an open local forwarding socket.
forward() {
	progress "Opening a local port-forward and waiting for the Sessions API"
	"${K[@]}" port-forward -n "${NS}" service/agentsessions "${port}:8080" >"${tmp}/forward.log" 2>&1 &
	pf=$!
	local deadline=$((SECONDS + 30))
	while ((SECONDS < deadline)); do
		kill -0 "${pf}" 2>/dev/null || { cat "${tmp}/forward.log" >&2; die "Port-forward exited."; }
		if grep -q '^Forwarding from ' "${tmp}/forward.log" &&
			"${TIMEOUT}" --kill-after=1s 3s "${tmp}/agentctl" list --project "${PROJECT}" --server "127.0.0.1:${port}" >/dev/null 2>&1; then
			return
		fi
		sleep 1
	done
	cat "${tmp}/forward.log" >&2
	die "Sessions readiness deadline exceeded."
}

# Create a remote chat session and verify the committed chain before restarting the daemon.
forward
progress "Creating a remote chat session and running its first turn"
first="$(ctl exec --harness substrate-chat --input first)"
echo "${first}"
uid="$(awk '$1 == "session" {print $2; exit}' <<<"${first}")"
[[ -n "${uid}" ]] || die "No session UID returned."
if [[ "${MODEL_SOURCE:-}" == stub ]]; then grep -q 'context=1 last=first' <<<"${first}"; fi
progress "Verifying the initial journal chain"
ctl verify --session "${uid}"

# Replace only the daemon pod. The journal volume and session actor remain in place.
# The old port-forward targets the deleted pod, so it must be recreated afterward.
progress "Replacing the Sessions daemon pod while retaining the journal and session actor"
old="$("${K[@]}" get pod -n "${NS}" -l app=agentsessions -o jsonpath='{.items[0].metadata.name}')"
kill "${pf}"
wait "${pf}" 2>/dev/null || true
pf=""
"${K[@]}" delete pod -n "${NS}" "${old}" --wait=true
"${K[@]}" rollout status deployment/agentsessions -n "${NS}" --timeout=300s
new="$("${K[@]}" get pod -n "${NS}" -l app=agentsessions -o jsonpath='{.items[0].metadata.name}')"
[[ -n "${new}" && "${new}" != "${old}" ]] || die "Daemon pod was not replaced."

# Read the retained history and continue the same session. The stub's context count
# confirms the second turn includes the earlier user and assistant messages.
forward
progress "Reading the persisted session and journal from the replacement daemon"
ctl get --session "${uid}"
history="$(ctl replay --session "${uid}")"
grep -q 'first' <<<"${history}"
progress "Continuing the same session with its prior conversation"
second="$(ctl exec --session "${uid}" --input second)"
echo "${second}"
if [[ "${MODEL_SOURCE:-}" == stub ]]; then grep -q 'context=3 last=second' <<<"${second}"; fi
progress "Verifying the journal chain after continuation"
ctl verify --session "${uid}"

# Show the resumed session's correlation evidence from the replacement daemon.
progress "Checking session correlation in the replacement daemon logs"
"${K[@]}" logs -n "${NS}" "${new}" --tail=200 | grep -F "session_uid=${uid}"
progress "Restart smoke passed: ${uid}"
