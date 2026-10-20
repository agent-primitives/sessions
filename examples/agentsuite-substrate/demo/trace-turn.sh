#!/usr/bin/env bash
# trace-turn.sh SESSION DIR: the first turn of SESSION, followed through each component's own log.
#
# Saves the full logs of every component the turn passes through to DIR, one file each, then prints
# one row per hop, each the earliest real log line about the session in that component's log (or
# the journal record, for the model call), shortened by trace-hop.py to its time, message and a few
# fields:
#   agentsessionsd   the Sessions.Exec call from agentctl (the daemon's "operation finished" line;
#                    the substrate adapter runs inside agentsessionsd and logs nothing of its own)
#   ate-api          the adapter's CreateActor (or ResumeActor) call for the session, in the ate-api
#                    server's log, with the actor it names
#   worker           the worker pod's ateom restoring the session's actor from the golden snapshot
#                    (RestoreWorkload, sent by the node's atelet), with the actor it names
#   micro-VM         the actor's own lifecycle log: the actor starting in its micro-VM
#   atenet-router    the harnesswire stream routed to the actor's worker
#   harness          the model call the harness made through sink.Model, with its token counts
#   model call       the call agentsessionsd made to the provider, as its journal records it
# Fails when a hop has no line for the session. Every pod's full log stays in DIR, atelet's included.
#
# Runs where the demo's kubeconfig is KUBECONFIG and the kubectl-ate plugin and agentctl are on PATH
# (driver.sh show sets both); API is the Sessions API address.
set -euo pipefail
session="${1:?usage: trace-turn.sh SESSION DIR}"
dir="${2:?usage: trace-turn.sh SESSION DIR}"
here="$(cd "$(dirname "$0")" && pwd)"
atespace=agentsuite-demo
mkdir -p "${dir}"

# What the fetching tools print on stderr goes to DIR/fetch.stderr, kept with the logs, so it is not
# lost: the kubectl-ate plugin reaches the cluster through a port-forward whose teardown can log a
# harmless "Error copying from remote stream" line. A fetch that fails still stops the script (set -e).
exec 3>&2 2>>"${dir}/fetch.stderr"
kubectl ate get actor "${session}" -a "${atespace}" -o json >"${dir}/actor.json"
kubectl -n agentsuite-demo logs deploy/agentsessionsd >"${dir}/agentsessionsd.log"
# Every pod of substrate's control plane and of the demo's namespace but agentsessionsd: the ate-api
# server, the router, each node's atelet, and the worker pods of the micro-VM pool.
for ns in ate-system agentsuite-demo; do
	for pod in $(kubectl -n "${ns}" get pods -o name); do
		pod="${pod#pod/}"
		[[ "${pod}" != agentsessionsd* ]] || continue
		kubectl -n "${ns}" logs "${pod}" --all-containers --prefix >"${dir}/${ns}.${pod}.log"
	done
done
kubectl ate logs actor "${session}" -a "${atespace}" --source lifecycle >"${dir}/actor-lifecycle.log"
kubectl ate logs actor "${session}" -a "${atespace}" --source containers >"${dir}/harness.log"
agentctl replay -v --server "${API:?}" --session "${session}" >"${dir}/journal.log"
exec 2>&3 3>&-

hop() { python3 "${here}/trace-hop.py" "$@"; }
shopt -s nullglob
hop agentsessionsd "operation=exec session_uid=${session} .*phase=finish" operation,session_uid,outcome "${dir}/agentsessionsd.log"
hop ate-api "\"method\":\"/ateapi\\.Control/(CreateActor|ResumeActor)\".*\"${session}\"" method,actor=req.actor.metadata.name,actor=req.actor.name "${dir}"/ate-system.ate-api*.log
hop worker "\"method\":\"/ateom\\.Ateom/(RestoreWorkload|RunWorkload)\".*\"actor_name\":\"${session}\"" method,actor=req.actor_name "${dir}"/agentsuite-demo.*.log
hop micro-VM '"Actor (started|restored)' "" "${dir}/actor-lifecycle.log"
hop atenet-router "Route ok.*${session}" actor.name,targetAddr "${dir}"/ate-system.atenet-router*.log
hop harness 'harness: model .* used [0-9]+ input' "" "${dir}/harness.log"
hop "model call" '^seq=[0-9]+ +EVENT_MODEL_CALL ' seq,model,input_hash "${dir}/journal.log"
