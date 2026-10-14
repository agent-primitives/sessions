#!/usr/bin/env bash
# Brings up the demo: a kind cluster with agent-substrate installed by its own kind lane (ate-setup),
# a gVisor WorkerPool, the agent's harness image and ActorTemplate rendered from the AgentSuite
# composition, the fake model, and agentsessionsd (substrate build) serving the agent as a harness.
#
# Idempotent: an existing cluster, template or deployment is reused. See README.md for the tools.
set -euo pipefail
source "$(dirname "$0")/common.sh"

step() { printf '\n==> %s\n' "$*"; }

for tool in docker kind ko go git python3 kubectl curl; do
	command -v "${tool}" >/dev/null || {
		echo "missing ${tool}" >&2
		exit 1
	}
done
case "$(docker info --format '{{.Architecture}}')" in
aarch64 | arm64) ARCH=arm64 ;;
x86_64 | amd64) ARCH=amd64 ;;
*)
	echo "unsupported docker architecture" >&2
	exit 1
	;;
esac
export KO_DOCKER_REPO=localhost:5001 KO_DEFAULTPLATFORMS="linux/${ARCH}"

# Validate every input before anything touches the cluster, so a misconfigured rerun exits without
# changing the cluster, the existing deployment or its agentsessionsd-model Secret.
refuse() {
	echo "$*; nothing was changed" >&2
	exit 1
}

# The placeholders in deploy/agentsessionsd.yaml. Each is replaced literally, never through a sed
# expression, so a value is copied as written ('&', '#' and '\' included).
DAEMON_PLACEHOLDERS=(MODEL_ID MODEL_BASE_URL DAEMON_IMAGE SUBSTRATE_HARNESS_FLAG MODEL_SECRET_VERSION)
# unfilled prints the placeholders the text $1 still contains, in DAEMON_PLACEHOLDERS order.
unfilled() {
	local p left=""
	for p in "${DAEMON_PLACEHOLDERS[@]}"; do
		if [[ "$1" == *"${p}"* ]]; then left="${left:+${left} }${p}"; fi
	done
	echo "${left}"
}

if [[ -n "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then
	# A real OpenAI-compatible provider.
	MODEL_BASE_URL="${AGENTSUITE_MODEL_BASE_URL}"
	MODEL_ID="${AGENTSUITE_MODEL:?set AGENTSUITE_MODEL with AGENTSUITE_MODEL_BASE_URL}"
	# The harness requests the provider's model, not the fixture's.
	HARNESS_MODEL="${MODEL_ID}"
	# Both land in a YAML plain scalar: whitespace or a control character would change the YAML
	# around them, and a placeholder name would be replaced again further down.
	unsafe='[[:space:][:cntrl:]]'
	for var in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL; do
		if [[ "${!var}" =~ ${unsafe} ]]; then
			refuse "${var} contains whitespace or a control character"
		fi
		if [[ -n "$(unfilled "${!var}")" ]]; then
			refuse "${var} contains $(unfilled "${!var}"), a placeholder of deploy/agentsessionsd.yaml"
		fi
	done
	# agentsessionsd refuses a base URL or model its model client cannot be built from at startup.
	# The Deployment recreates its pod, and the journal is an emptyDir, so letting such a value
	# through would delete the running daemon's sessions and leave a pod that never starts. emit
	# check-model builds the client with the daemon's own constructor, and also requires http(s).
	if ! reason="$(cd "${HERE}" && go run ./cmd/emit check-model -base-url "${AGENTSUITE_MODEL_BASE_URL}" -model "${MODEL_ID}" 2>&1)"; then
		refuse "AGENTSUITE_MODEL_BASE_URL '${AGENTSUITE_MODEL_BASE_URL}' with AGENTSUITE_MODEL '${MODEL_ID}' would not start agentsessionsd: ${reason}"
	fi
else
	MODEL_BASE_URL="http://fakemodel.${NS}.svc:8080/v1"
	MODEL_ID=triage-model
	HARNESS_MODEL=""
fi
# Render the model settings into the agentsessionsd manifest now, so a value that cannot be
# rendered stops the run here. The image, harness flag and Secret version are filled in later.
DAEMON_MANIFEST="$(<"${HERE}/deploy/agentsessionsd.yaml")"
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_ID/"${MODEL_ID}"}
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_BASE_URL/"${MODEL_BASE_URL}"}
if [[ "$(unfilled "${DAEMON_MANIFEST}")" != "DAEMON_IMAGE SUBSTRATE_HARNESS_FLAG MODEL_SECRET_VERSION" ]]; then
	refuse "deploy/agentsessionsd.yaml: after the model settings it holds the placeholders '$(unfilled "${DAEMON_MANIFEST}")', want DAEMON_IMAGE, SUBSTRATE_HARNESS_FLAG and MODEL_SECRET_VERSION"
fi

# deploy/ and this script create the atespace ${ATESPACE}, and agentsessionsd's
# -substrate-atespace names it, so the binding the template is rendered from must name it too.
BINDING_ATESPACE="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("atespace", ""))' "${HERE}/binding.json")"
if [[ "${BINDING_ATESPACE}" != "${ATESPACE}" ]]; then
	refuse "binding.json names atespace '${BINDING_ATESPACE}', but the demo deploys into atespace ${ATESPACE}"
fi

step "agent-substrate ${SUBSTRATE_REF:0:8} checkout in ${SUBSTRATE}"
if [[ ! -d "${SUBSTRATE}/.git" ]]; then
	git clone -q https://github.com/agent-substrate/substrate "${SUBSTRATE}"
fi
if [[ "$(git -C "${SUBSTRATE}" rev-parse HEAD)" != "${SUBSTRATE_REF}" ]]; then
	git -C "${SUBSTRATE}" fetch -q https://github.com/agent-substrate/substrate "${SUBSTRATE_REF}"
	git -C "${SUBSTRATE}" checkout -q "${SUBSTRATE_REF}"
fi

# Where the cluster stands: absent; foreign (up.sh did not create it); incomplete (up.sh created it
# but create-kind-cluster.sh did not finish); created (ate-system not installed yet); installed.
if cluster_listed; then
	id="$(node_id)"
	if [[ -n "${id}" && "$(owned_id)" == "${id}" ]]; then
		stage="$(setup_stage "${id}")"
		stage="${stage:-incomplete}"
	else
		stage=foreign
	fi
elif [[ -n "$(node_id)" ]]; then
	# create-kind-cluster.sh deletes a cluster of this name before it creates one.
	echo "refusing to create kind cluster ${CLUSTER}: kind does not list it, but container ${CLUSTER}-control-plane exists." >&2
	echo "Delete that container yourself, or set AGENTSUITE_DEMO_CLUSTER to another name." >&2
	exit 1
else
	stage=absent
fi

if [[ "${stage}" == absent || "${stage}" == incomplete ]]; then
	if [[ "${stage}" == incomplete ]]; then
		step "kind cluster ${CLUSTER}: up.sh created it, but its setup did not finish; recreating it"
	else
		step "kind cluster ${CLUSTER} (substrate's kind lane: create-kind-cluster.sh)"
	fi
	rm -f "${SETUP}"
	# Record ownership as soon as the node exists, even when the script fails, or up.sh is
	# interrupted, after kind created it, so a rerun can finish the cluster and down.sh can delete it.
	record_owner() {
		local id
		id="$(node_id)"
		if [[ -n "${id}" ]]; then
			echo "${id}" >"${OWNED}"
		else
			rm -f "${OWNED}"
		fi
	}
	trap record_owner EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	rc=0
	(cd "${SUBSTRATE}" && hack/create-kind-cluster.sh) || rc=$?
	trap - EXIT INT TERM
	record_owner
	id="$(node_id)"
	if ((rc != 0)); then
		echo "create-kind-cluster.sh failed (exit ${rc}); rerun up.sh to recreate the cluster" >&2
		exit "${rc}"
	fi
	echo "${id} created" >"${SETUP}"
	stage=created
fi
kind export kubeconfig --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}"
if [[ "${stage}" == created ]]; then
	step "ate-system (install-ate-kind.sh, which runs ate-setup)"
	(cd "${SUBSTRATE}" && ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}' ATE_INSTALL_ROLLOUT_TIMEOUT=10m \
		hack/install-ate-kind.sh --deploy-ate-system)
	echo "${id} installed" >"${SETUP}"
else
	step "reusing kind cluster ${CLUSTER}"
	if [[ "${stage}" == foreign ]]; then
		echo "note: up.sh did not create kind cluster ${CLUSTER}, so it does not install into it and down.sh will not delete it"
	fi
	"${K[@]}" -n ate-system rollout status deploy/ate-api-server --timeout=120s
fi

step "kubectl-ate, built from the same substrate checkout"
(cd "${SUBSTRATE}" && go build -o "${WORK}/bin/kubectl-ate" ./cmd/kubectl-ate)

step "gVisor WorkerPool"
"${K[@]}" apply -f "${HERE}/deploy/namespace.yaml"
ATEOM_IMAGE="$(cd "${SUBSTRATE}" && ko build --base-import-paths ./cmd/ateom-gvisor)"
sed -e "s#ATEOM_IMAGE#${ATEOM_IMAGE}#" "${HERE}/deploy/workerpool.yaml" | "${K[@]}" apply -f -
"${ROOT}/hack/wait-worker-pool.sh" agentsuite-demo-gvisor "${NS}" 600

step "harness image for agent ${AGENT} (linux/${ARCH} composition)"
CTXDIR="${STATE}/harness-image"
rm -rf "${CTXDIR}" && mkdir -p "${CTXDIR}"
(cd "${HERE}" && go run ./cmd/emit bundle -suite suite -agent "${AGENT}" -platform "linux/${ARCH}" -out "${CTXDIR}/agent")
(cd "${HERE}" && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${CTXDIR}/harness" ./cmd/harness)
docker build -q --platform "linux/${ARCH}" -f "${HERE}/Dockerfile.harness" -t "localhost:5001/agentsuite-harness:${AGENT}" "${CTXDIR}" >/dev/null
docker push -q "localhost:5001/agentsuite-harness:${AGENT}" >/dev/null
HARNESS_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "localhost:5001/agentsuite-harness:${AGENT}")"
echo "harness image: ${HARNESS_IMAGE}"

step "render the composition: ActorTemplate + harness registration"
(cd "${HERE}" && go run ./cmd/emit render -suite suite -agent "${AGENT}" -platform "linux/${ARCH}" \
	-binding binding.json -image "${HARNESS_IMAGE}" -model "${HARNESS_MODEL}" -out "${STATE}/render")
TEMPLATE="$(cat "${STATE}/render/template-name")"
cat "${STATE}/render/harness-registration.json"

step "atespace ${ATESPACE} and ActorTemplate ${TEMPLATE}; wait for its golden snapshot"
"${ATE[@]}" get atespace "${ATESPACE}" >/dev/null 2>&1 || "${ATE[@]}" create atespace "${ATESPACE}"
if ! "${ATE[@]}" get actor-template "${TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1; then
	"${ATE[@]}" create actor-template -f "${STATE}/render/actortemplate.json"
fi
golden_tag() {
	"${ATE[@]}" get actor-template "${TEMPLATE}" -a "${ATESPACE}" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin).get("status", {}).get("goldenSnapshotStatus", {})
print("ERROR " + s["errorMessage"] if s.get("errorMessage") else s.get("goldenTag", {}).get("name", ""))'
}
deadline=$((SECONDS + 600))
golden=""
while [[ -z "${golden}" ]]; do
	golden="$(golden_tag)"
	if [[ -z "${golden}" ]]; then
		((SECONDS < deadline)) || {
			echo "timed out waiting for the golden snapshot" >&2
			exit 1
		}
		sleep 5
	fi
done
[[ "${golden}" != ERROR* ]] || {
	echo "golden snapshot failed: ${golden}" >&2
	exit 1
}
echo "golden snapshot tag: ${golden}"

# The fake model serves only the fixture run; with a real provider it is neither built nor deployed.
FAKE_MODEL=true
if [[ -n "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then FAKE_MODEL=false; fi
if ${FAKE_MODEL}; then
	step "fake model and agentsessionsd (substrate build)"
	FAKEMODEL_IMAGE="$(cd "${HERE}" && ko build --base-import-paths ./cmd/fakemodel)"
else
	step "agentsessionsd (substrate build) for ${AGENTSUITE_MODEL_BASE_URL}"
fi
DAEMON_IMAGE="$(cd "${ROOT}/integrations/substrate" && ko build --base-import-paths ./cmd/agentsessionsd)"
# Finish rendering everything but the Secret's version before the Secret changes. The model settings
# were rendered at the top.
DAEMON_MANIFEST=${DAEMON_MANIFEST//DAEMON_IMAGE/"${DAEMON_IMAGE}"}
DAEMON_MANIFEST=${DAEMON_MANIFEST//SUBSTRATE_HARNESS_FLAG/"$(cat "${STATE}/render/agentsessionsd.args")"}
if [[ "$(unfilled "${DAEMON_MANIFEST}")" != MODEL_SECRET_VERSION ]]; then
	echo "rendered agentsessionsd.yaml: unexpected placeholders left: $(unfilled "${DAEMON_MANIFEST}"); the agentsessionsd-model Secret and the Deployment were not changed" >&2
	exit 1
fi
if [[ -n "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then
	if [[ -n "${MODEL_API_KEY:-}" ]]; then
		# Replace the key in place: render the Secret client-side and apply it, so a failed apply
		# leaves the stored key as it was. The key reaches kubectl over stdin, never argv or a file.
		# Server-side apply keeps no last-applied annotation, so the key is not copied into metadata.
		printf '%s' "${MODEL_API_KEY}" |
			"${K[@]}" -n "${NS}" create secret generic agentsessionsd-model --from-file=api-key=/dev/stdin --dry-run=client -o yaml |
			"${K[@]}" -n "${NS}" apply --server-side --force-conflicts --field-manager=agentsuite-demo -f -
	else
		# An endpoint that takes no key: drop a stored one, so it is not sent there.
		"${K[@]}" -n "${NS}" delete secret agentsessionsd-model --ignore-not-found
	fi
else
	# Drop any key a previous real-provider run left, so the fake model never receives it.
	"${K[@]}" -n "${NS}" delete secret agentsessionsd-model --ignore-not-found
fi
# agentsessionsd reads the key into its environment once, at container start, so a Secret change
# alone never reaches it. Stamp the Secret's resourceVersion ("none" once deleted) into the pod
# template: a rotated or deleted key changes the template and restarts the pod, and an unchanged one
# leaves the pod (and its emptyDir journal) alone.
MODEL_SECRET_VERSION="$("${K[@]}" -n "${NS}" get secret agentsessionsd-model --ignore-not-found -o jsonpath='{.metadata.resourceVersion}')"
MODEL_SECRET_VERSION="${MODEL_SECRET_VERSION:-none}"
if ${FAKE_MODEL}; then
	sed -e "s#FAKEMODEL_IMAGE#${FAKEMODEL_IMAGE}#" "${HERE}/deploy/fakemodel.yaml" | "${K[@]}" apply -f -
fi
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_SECRET_VERSION/"${MODEL_SECRET_VERSION}"}
printf '%s\n' "${DAEMON_MANIFEST}" >"${STATE}/agentsessionsd.yaml"
"${K[@]}" apply -f "${STATE}/agentsessionsd.yaml"
if ${FAKE_MODEL}; then
	"${K[@]}" -n "${NS}" rollout status deploy/fakemodel --timeout=180s
fi
"${K[@]}" -n "${NS}" rollout status deploy/agentsessionsd --timeout=180s
"${K[@]}" -n "${NS}" logs deploy/agentsessionsd | grep -E 'listening|substrate harness' || true

step "up; now run ./demo.sh"
