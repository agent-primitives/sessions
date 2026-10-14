#!/usr/bin/env bash
# Step 0 of the recorded demo: one micro-VM actor boots, suspends to a memory snapshot and resumes,
# with an in-RAM counter carried across. It runs substrate's own counter-microvm demo (an HTTP counter
# held only in guest RAM) on the cluster `up.sh --substrate-only` brought up, and fails unless the
# count continues after the resume and the actor keeps its UID.
set -euo pipefail
source "$(dirname "$0")/../common.sh"

CNS=ate-demo-counter-microvm ACTOR=smoke-counter-1 PORT=18090
fail() {
	echo "SMOKE FAILED: $*" >&2
	exit 1
}
show() {
	printf '$ %s\n' "$*"
	"$@"
}
actor() { "${ATE[@]}" get actor "${ACTOR}" -a "${CNS}" -o json | python3 -c "import json, sys; a = json.load(sys.stdin); print($1)"; }
wait_state() {
	local state=""
	for _ in $(seq 150); do
		state="$(actor 'a["status"]["state"]')"
		if [[ "${state}" == "$1" ]]; then return 0; fi
		sleep 2
	done
	fail "actor ${ACTOR} is ${state}, want $1"
}

[[ "${SANDBOX}" == microvm ]] || fail "AGENTSUITE_DEMO_SANDBOX=${SANDBOX}: the smoke test is for the micro-VM class"
echo "# substrate's counter-microvm demo: its WorkerPool and ActorTemplate on the microvm SandboxConfig"
(cd "${SUBSTRATE}" && KUBECTL_CONTEXT="kind-${CLUSTER}" KO_DOCKER_REPO=localhost:5001 KO_DEFAULTPLATFORMS=linux/amd64 \
	NO_DEV_ENV=true ATE_INSTALL_KIND=true hack/install-ate-kind.sh --deploy-demo-counter-microvm) >"${STATE}/counter-demo.log" 2>&1 ||
	{
		tail -30 "${STATE}/counter-demo.log"
		fail "deploying substrate's counter-microvm demo failed"
	}
show "${ATE[@]}" get actor-templates -a "${CNS}"
if ! "${ATE[@]}" get actor "${ACTOR}" -a "${CNS}" >/dev/null 2>&1; then
	show "${ATE[@]}" create actor "${ACTOR}" -a "${CNS}" --template counter-microvm
fi
# A new actor is created SUSPENDED; resuming it boots the micro-VM from the template's golden snapshot.
if [[ "$(actor 'a["status"]["state"]')" == ACTOR_STATE_SUSPENDED ]]; then
	show "${ATE[@]}" resume actor "${ACTOR}" -a "${CNS}"
fi
wait_state ACTOR_STATE_RUNNING
"${K[@]}" -n ate-system port-forward svc/atenet-router "${PORT}:80" >/dev/null 2>&1 &
PF=$!
trap 'kill ${PF} 2>/dev/null || true' EXIT
count() { curl -fsS -X POST -H "ate-target-actor: ${CNS}/${ACTOR}" "http://127.0.0.1:${PORT}"; }
for _ in $(seq 30); do
	if count >/dev/null 2>&1; then break; fi
	sleep 1
done
# The probe above counted once; these continue from there.
for _ in 1 2; do printf '$ curl -X POST -H "ate-target-actor: %s/%s" atenet-router\n%s\n' "${CNS}" "${ACTOR}" "$(count)"; done
BEFORE="$(count)"
printf '$ curl -X POST -H "ate-target-actor: %s/%s" atenet-router\n%s\n' "${CNS}" "${ACTOR}" "${BEFORE}"
UID_BEFORE="$(actor 'a["metadata"].get("uid", "")')"
show "${ATE[@]}" get actor "${ACTOR}" -a "${CNS}"
show "${ATE[@]}" suspend actor "${ACTOR}" -a "${CNS}"
wait_state ACTOR_STATE_SUSPENDED
show "${ATE[@]}" get actor "${ACTOR}" -a "${CNS}"
echo "snapshots held: $(actor 'json.dumps([{k: s.get(k) for k in ("generation", "fidelity")} for s in a["status"].get("snapshots", [])])')"
show "${ATE[@]}" resume actor "${ACTOR}" -a "${CNS}"
wait_state ACTOR_STATE_RUNNING
AFTER="$(count)"
printf '$ curl -X POST -H "ate-target-actor: %s/%s" atenet-router   (after resume)\n%s\n' "${CNS}" "${ACTOR}" "${AFTER}"
UID_AFTER="$(actor 'a["metadata"].get("uid", "")')"
show "${ATE[@]}" get actor "${ACTOR}" -a "${CNS}"
echo "actor uid before suspend: ${UID_BEFORE}; after resume: ${UID_AFTER}"
[[ -n "${UID_BEFORE}" && "${UID_BEFORE}" == "${UID_AFTER}" ]] || fail "actor UID changed across suspend/resume"
# The counter replies "... | preserved memory count: N | ...".
memcount() { sed -n 's/.*preserved memory count: \([0-9][0-9]*\).*/\1/p' <<<"$1"; }
nb="$(memcount "${BEFORE}")" na="$(memcount "${AFTER}")"
[[ -n "${nb}" && "${na}" == "$((nb + 1))" ]] || fail "count after resume is '${AFTER}', want $((nb + 1)): the guest RAM did not come back"
echo "SMOKE OK: the micro-VM actor booted, suspended to a memory snapshot and resumed with its RAM (count ${nb} -> ${na}) and the same UID"
