#!/usr/bin/env bash
# trace-turn.sh SESSION DIR: the first turn of SESSION, followed through each component's own log.
#
# Prints one line per hop, prefixed with the component's name, each a short excerpt of a real log
# line that names the session (or its actor):
#   agentsessionsd   the Sessions.Exec call from agentctl (the daemon's "operation finished" line)
#   ateom            the session's actor starting in its micro-VM (the actor's lifecycle log)
#   atenet-router    the harnesswire stream routed to the actor's harness on port 80
#   harness          the model call the harness made through sink.Model, with its token counts
#   journal          the model call agentsessionsd made to the provider, as its journal records it
# The full logs of every component go to DIR, one file each, for the chapter's log. Fails when a
# hop has no line for the session.
#
# Runs where the demo's kubeconfig is KUBECONFIG and the kubectl-ate plugin and agentctl are on PATH
# (driver.sh show sets both); API is the Sessions API address.
set -euo pipefail
session="${1:?usage: trace-turn.sh SESSION DIR}"
dir="${2:?usage: trace-turn.sh SESSION DIR}"
atespace=agentsuite-demo
mkdir -p "${dir}"

kubectl -n agentsuite-demo logs deploy/agentsessionsd >"${dir}/agentsessionsd.log"
kubectl ate logs actor "${session}" -a "${atespace}" --source lifecycle >"${dir}/ateom.log"
kubectl -n ate-system logs deploy/atenet-router --all-containers --prefix >"${dir}/atenet-router.log"
kubectl ate logs actor "${session}" -a "${atespace}" --source containers >"${dir}/harness.log"
agentctl replay -v --server "${API:?}" --session "${session}" >"${dir}/journal.log"

# hop NAME FILE PATTERN SED: the first line of FILE matching the ERE PATTERN, shortened by the sed -E
# script SED, after NAME.
hop() {
	local line
	line="$(grep -E -m1 -- "$3" "$2" | sed -E "$4")" || true
	if [[ -z "${line}" ]]; then
		echo "trace-turn.sh: no ${1} line for ${session} in ${2##*/}" >&2
		exit 1
	fi
	printf '%-15s %s\n' "$1" "${line}"
}
hop agentsessionsd "${dir}/agentsessionsd.log" "operation=exec session_uid=${session} .*phase=finish" \
	's/.*(operation=exec session_uid=[^ ]+).*(outcome=[a-z]+ duration_ms=[0-9]+).*/\1 … \2/'
hop ateom "${dir}/ateom.log" '"Actor (started|restored)' 's/^.*(\{"time":"[^"]*","message":"[^"]*").*/\1 …/'
hop atenet-router "${dir}/atenet-router.log" "Route ok.*${session}" 's/.*"(Route ok)".*("name":"[^"]*"\}).*("targetAddr":"[^"]*").*/\1 … \2,\3/'
hop harness "${dir}/harness.log" 'harness: model .* used [0-9]+ input' 's/.*(model .* used [0-9]+ input and [0-9]+ output tokens).*/\1/'
hop journal "${dir}/journal.log" '^seq=[0-9]+ +EVENT_MODEL_CALL ' 's/^(seq=[0-9]+) +EVENT_MODEL_CALL .*(model=[^ ]+).*/\1 EVENT_MODEL_CALL \2/'
