#!/usr/bin/env bash
# Exercises the production-shaped deployment exclusively through the Sessions API.
set -euo pipefail

SMOKE_TIMEOUT_SECONDS="${SMOKE_TIMEOUT_SECONDS:-900}"
READY_TIMEOUT_SECONDS="${READY_TIMEOUT_SECONDS:-30}"
PROBE_TIMEOUT_SECONDS="${PROBE_TIMEOUT_SECONDS:-3}"
for value in "${SMOKE_TIMEOUT_SECONDS}" "${READY_TIMEOUT_SECONDS}" "${PROBE_TIMEOUT_SECONDS}"; do
	if [[ ! "${value}" =~ ^[1-9][0-9]*$ ]]; then
		echo "smoke and readiness timeouts must be positive integer seconds" >&2
		exit 2
	fi
done
if [[ "${1:-}" != "--bounded" ]]; then
	exec timeout --kill-after=5s "${SMOKE_TIMEOUT_SECONDS}s" bash "$0" --bounded
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${CLUSTER:-kind}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="${NAMESPACE:-ate-agentsessions}"
LOCAL_PORT="${LOCAL_PORT:-18080}"
EXPECT_MODELSTUB="${EXPECT_MODELSTUB:-0}"
SERVER="127.0.0.1:${LOCAL_PORT}"
PROJECT="${PROJECT:-default}"
WORK="$(mktemp -d)"
PF_PID=""

cleanup() {
	if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
		kill "${PF_PID}"
		wait "${PF_PID}" 2>/dev/null || true
	fi
	rm -rf "${WORK}"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

kubectl_kind() {
	kubectl --context "${CONTEXT}" "$@"
}

agentctl() {
	local command="$1"
	shift
	"${WORK}/agentctl" "${command}" --project "${PROJECT}" "$@"
}

start_forward() {
	kubectl --context "${CONTEXT}" port-forward -n "${NAMESPACE}" service/agentsessions "${LOCAL_PORT}:8080" \
		>"${WORK}/port-forward.log" 2>&1 &
	PF_PID=$!
	local deadline=$((SECONDS + READY_TIMEOUT_SECONDS))
	while ((SECONDS < deadline)); do
		if ! kill -0 "${PF_PID}" 2>/dev/null; then
			cat "${WORK}/port-forward.log" >&2
			return 1
		fi
		local remaining=$((deadline - SECONDS))
		if ((remaining <= 0)); then
			break
		fi
		if grep -q '^Forwarding from ' "${WORK}/port-forward.log" &&
			timeout --kill-after=1s "$((remaining < PROBE_TIMEOUT_SECONDS ? remaining : PROBE_TIMEOUT_SECONDS))s" \
				"${WORK}/agentctl" list --project "${PROJECT}" --server "${SERVER}" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	echo "Sessions readiness timed out after ${READY_TIMEOUT_SECONDS}s" >&2
	cat "${WORK}/port-forward.log" >&2
	return 1
}

stop_forward() {
	if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
		kill "${PF_PID}"
		wait "${PF_PID}" 2>/dev/null || true
	fi
	PF_PID=""
}

echo "==> build agentctl"
(cd "${ROOT}" && go build -o "${WORK}/agentctl" ./cmd/agentctl)
start_forward

echo "==> execute the first chat turn through Sessions"
first="$(agentctl exec --server "${SERVER}" --harness chat --input first)"
echo "${first}"
session="$(awk '$1 == "session" { print $2; exit }' <<<"${first}")"
if [[ -z "${session}" ]]; then
	echo "agentctl did not report a session UID" >&2
	exit 1
fi
if [[ "${EXPECT_MODELSTUB}" == "1" ]]; then
	grep -q 'context=1 last=first' <<<"${first}"
fi
agentctl get --server "${SERVER}" --session "${session}"
agentctl verify --server "${SERVER}" --session "${session}"

echo "==> replace the Sessions pod while retaining the PVC and substrate actor"
old_pod="$(kubectl_kind get pods -n "${NAMESPACE}" -l app=agentsessions -o jsonpath='{.items[0].metadata.name}')"
stop_forward
kubectl_kind delete pod -n "${NAMESPACE}" "${old_pod}" --wait=true
kubectl_kind rollout status deployment/agentsessions -n "${NAMESPACE}" --timeout=600s
new_pod="$(kubectl_kind get pods -n "${NAMESPACE}" -l app=agentsessions -o jsonpath='{.items[0].metadata.name}')"
if [[ "${new_pod}" == "${old_pod}" ]]; then
	echo "control-plane pod was not replaced" >&2
	exit 1
fi
start_forward

echo "==> prove metadata and committed history survived"
agentctl get --server "${SERVER}" --session "${session}"
replay="$(agentctl replay --server "${SERVER}" --session "${session}")"
echo "${replay}"
grep -q 'first' <<<"${replay}"

echo "==> continue the same chat session after restart"
second="$(agentctl exec --server "${SERVER}" --session "${session}" --input second)"
echo "${second}"
if [[ "${EXPECT_MODELSTUB}" == "1" ]]; then
	grep -q 'context=3 last=second' <<<"${second}"
fi
agentctl verify --server "${SERVER}" --session "${session}"

echo "==> correlation evidence"
kubectl_kind logs -n "${NAMESPACE}" "${new_pod}" --tail=200 | grep -F "session_uid=${session}"
kubectl_kind get pods -n "${NAMESPACE}" -o wide
echo "production-shaped smoke passed for session ${session}"
