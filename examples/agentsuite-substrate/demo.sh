#!/usr/bin/env bash
# The five-minute demo, against what up.sh brought up. Each step prints what it proves and fails the
# script if the proof does not hold.
set -euo pipefail
source "$(dirname "$0")/common.sh"

SERVER=127.0.0.1:18080 ROUTER=127.0.0.1:18081 MODEL=127.0.0.1:18082
# With a real provider (up.sh with AGENTSUITE_MODEL_BASE_URL) the replies are not the fake's, so the
# checks on reply text and on the fake's call counter are skipped; the replay still fails if the
# controller sees a model call.
REAL="${AGENTSUITE_MODEL_BASE_URL:+1}"
STATS_URL="http://${MODEL}/stats"
if [[ -n "${REAL}" ]]; then STATS_URL=""; fi
PIDS=()
cleanup() { for p in "${PIDS[@]}"; do kill "${p}" 2>/dev/null || true; done; }
trap cleanup EXIT

step() { printf '\n=== %s\n' "$*"; }
proves() { printf -- '--> proves: %s\n' "$*"; }
fail() {
	echo "DEMO FAILED: $*" >&2
	exit 1
}
forward() { # namespace service local:remote
	"${K[@]}" -n "$1" port-forward "svc/$2" "$3" >/dev/null 2>&1 &
	PIDS+=($!)
}
calls() { curl -fsS "http://${MODEL}/stats" | python3 -c 'import json,sys; print(json.load(sys.stdin)["calls"])'; }
held_snapshot() { # the newest COMPLETED durable snapshot in status.snapshots, as the adapter reads it
	"${ATE[@]}" get actor "$1" -a "${ATESPACE}" -o json | python3 -c '
import json, sys
snaps = [s for s in json.load(sys.stdin)["status"].get("snapshots", [])
         if s.get("durableSnapshot", {}).get("status") == "SNAPSHOT_STORAGE_STATUS_COMPLETED"]
s = max(snaps, key=lambda s: s["generation"]) if snaps else None
print("generation %s %s %s" % (s["generation"], s["fidelity"], s["durableSnapshot"]["object"]["snapshotUri"]) if s else "")'
}
actor_state() { "${ATE[@]}" get actor "$1" -a "${ATESPACE}" -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["state"])'; }
agentctl() { "${WORK}/bin/agentctl" "$1" --server "${SERVER}" "${@:2}"; }
say() { # session input -> prints the turn, sets REPLY to the assistant text
	local out
	out="$(agentctl exec --session "$1" --input "$2")"
	echo "${out}"
	REPLY="$(sed -n 's/^seq=[0-9]* *EVENT_OUTPUT  assistant: //p' <<<"${out}" | tail -1)"
}

(cd "${ROOT}" && go build -o "${WORK}/bin/agentctl" ./cmd/agentctl)
(cd "${HERE}" && go build -o "${WORK}/bin/replay" ./cmd/replay)
forward "${NS}" agentsessionsd 18080:8080
forward ate-system atenet-router 18081:80
forward "${NS}" fakemodel 18082:8080
for _ in $(seq 60); do
	if calls >/dev/null 2>&1 && agentctl list >/dev/null 2>&1; then break; fi
	sleep 1
done
TEMPLATE="$(cat "${WORK}/render/template-name")"
BASE_CALLS="$(calls)"
echo "agent ${AGENT} of suite support-desk -> ActorTemplate ${ATESPACE}/${TEMPLATE}; fake model calls so far: ${BASE_CALLS}"

step "1. Create a session on the AgentSuite agent's harness"
SID="$(agentctl create --harness "${AGENT}" --name support-demo)"
agentctl get --session "${SID}"
proves "the agent is registered as an AgentSessions harness; the session is pinned to it"

step "2. Chat three turns; each runs in the session's own substrate actor"
say "${SID}" "my laptop will not boot"
say "${SID}" "the VPN drops every hour"
say "${SID}" "I was billed twice this month"
LAST3="${REPLY}"
[[ -n "${REAL}" || "${LAST3}" == *"3) I was billed twice"* ]] || fail "turn 3 did not see the conversation"
"${ATE[@]}" get actor "${SID}" -a "${ATESPACE}"
[[ "$(actor_state "${SID}")" == ACTOR_STATE_RUNNING ]] || fail "actor is not running"
[[ -n "${REAL}" || "$(($(calls) - BASE_CALLS))" == 3 ]] || fail "expected 3 model calls"
proves "harnesswire reaches the actor through atenet-router; every model call went through the host (3 calls on the fake provider)"

step "3. Suspend: the actor is snapshotted and its worker freed"
agentctl suspend --session "${SID}"
"${ATE[@]}" get actor "${SID}" -a "${ATESPACE}"
[[ "$(actor_state "${SID}")" == ACTOR_STATE_SUSPENDED ]] || fail "actor is not suspended"
SNAP="$(held_snapshot "${SID}")"
[[ -n "${SNAP}" ]] || fail "suspended actor holds no completed snapshot"
echo "held snapshot: ${SNAP}"
proves "session suspend maps to substrate SuspendActor: the actor holds no worker and a completed snapshot in status.snapshots, the field the adapter now reads"

step "4. Resume and continue: the conversation state is intact"
agentctl resume --session "${SID}"
say "${SID}" "also, my password expired"
[[ -n "${REAL}" || "${REPLY}" == *"1) my laptop will not boot; 2) the VPN drops every hour; 3) I was billed twice this month; 4) also, my password expired"* ]] ||
	fail "resumed turn lost state: ${REPLY}"
[[ "$(actor_state "${SID}")" == ACTOR_STATE_RUNNING ]] || fail "actor is not running after resume"
proves "resume brought the same actor back (substrate ResumeActor restores its snapshot), and the 4th turn carries all 3 earlier issues: the host rebuilt the context from the journal"

step "5. The journal: every event of the session, in order"
agentctl replay --session "${SID}"
proves "inputs, model calls, outputs and the suspend/resume transitions (the two LIFECYCLE records) are one ordered, hash-chained log; step 6 verifies the chain"

step "6. Replay: re-run every turn of the agent against the journal, zero model calls"
BEFORE="$(calls)"
"${WORK}/bin/replay" -server "${SERVER}" -session "${SID}" -router "${ROUTER}" -atespace "${ATESPACE}" \
	-model-base-url "http://${MODEL}/v1" -stats-url "${STATS_URL}"
[[ "$(calls)" == "${BEFORE}" ]] || fail "replay called the model"
proves "the chain verifies client-side, and the agent re-executed all 4 turns in its actor with every completion served from the journal: fake provider calls stayed at ${BEFORE}"

step "7. Fork at the end of turn 1 and diverge"
FORK_SEQ="$(agentctl replay --session "${SID}" | awk '/EVENT_END/ { sub("seq=", "", $1); print $1; exit }')"
CHILD="$(agentctl fork --session "${SID}" --at "${FORK_SEQ}" --names after-turn-1 | awk '{ print $2 }')"
echo "child ${CHILD} forked at seq ${FORK_SEQ}"
say "${CHILD}" "actually, the printer is on fire"
[[ -n "${REAL}" || "${REPLY}" == *"1) my laptop will not boot; 2) actually, the printer is on fire | next"* ]] || fail "child did not diverge from turn 1: ${REPLY}"
"${ATE[@]}" get actor "${CHILD}" -a "${ATESPACE}"
PARENT_LAST="$(agentctl replay --session "${SID}" | sed -n 's/^seq=[0-9]* *EVENT_OUTPUT  assistant: //p' | tail -1)"
[[ -n "${REAL}" || "${PARENT_LAST}" == *"4) also, my password expired"* ]] || fail "parent changed"
proves "the child is its own actor with only turn 1 of history; the parent's 4 turns are untouched"

if [[ -n "${REAL}" ]]; then
	step "Done in ${SECONDS}s: 5 turns called the provider; the replay called it 0 times (controller count)"
else
	step "Done in ${SECONDS}s: fake model calls during the demo: $(($(calls) - BASE_CALLS)) (3 turns + 1 after resume + 1 on the fork; 0 for replay)"
fi
