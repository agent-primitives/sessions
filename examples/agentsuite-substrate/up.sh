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

# Validate the model settings before anything touches the cluster, so a misconfigured rerun exits
# without changing the existing deployment or its agentsessionsd-model Secret.
if [[ -n "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then
	# A real OpenAI-compatible provider.
	MODEL_BASE_URL="${AGENTSUITE_MODEL_BASE_URL}"
	MODEL_ID="${AGENTSUITE_MODEL:?set AGENTSUITE_MODEL with AGENTSUITE_MODEL_BASE_URL}"
else
	MODEL_BASE_URL="http://fakemodel.${NS}.svc:8080/v1"
	MODEL_ID=triage-model
fi

step "agent-substrate ${SUBSTRATE_REF:0:8} checkout in ${SUBSTRATE}"
if [[ ! -d "${SUBSTRATE}/.git" ]]; then
	git clone -q https://github.com/agent-substrate/substrate "${SUBSTRATE}"
fi
if [[ "$(git -C "${SUBSTRATE}" rev-parse HEAD)" != "${SUBSTRATE_REF}" ]]; then
	git -C "${SUBSTRATE}" fetch -q https://github.com/agent-substrate/substrate "${SUBSTRATE_REF}"
	git -C "${SUBSTRATE}" checkout -q "${SUBSTRATE_REF}"
fi

if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
	step "kind cluster ${KIND_CLUSTER_NAME} and ate-system (substrate's kind lane: create-kind-cluster.sh, then install-ate-kind.sh, which runs ate-setup)"
	(cd "${SUBSTRATE}" && hack/create-kind-cluster.sh)
	(cd "${SUBSTRATE}" && ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}' ATE_INSTALL_ROLLOUT_TIMEOUT=10m \
		hack/install-ate-kind.sh --deploy-ate-system)
else
	step "reusing kind cluster ${KIND_CLUSTER_NAME}"
	kind export kubeconfig --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"
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
CTXDIR="${WORK}/harness-image"
rm -rf "${CTXDIR}" && mkdir -p "${CTXDIR}"
(cd "${HERE}" && go run ./cmd/emit bundle -suite suite -agent "${AGENT}" -platform "linux/${ARCH}" -out "${CTXDIR}/agent")
(cd "${HERE}" && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${CTXDIR}/harness" ./cmd/harness)
docker build -q --platform "linux/${ARCH}" -f "${HERE}/Dockerfile.harness" -t "localhost:5001/agentsuite-harness:${AGENT}" "${CTXDIR}" >/dev/null
docker push -q "localhost:5001/agentsuite-harness:${AGENT}" >/dev/null
HARNESS_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "localhost:5001/agentsuite-harness:${AGENT}")"
echo "harness image: ${HARNESS_IMAGE}"

step "render the composition: ActorTemplate + harness registration"
(cd "${HERE}" && go run ./cmd/emit render -suite suite -agent "${AGENT}" -platform "linux/${ARCH}" \
	-binding binding.json -image "${HARNESS_IMAGE}" -out "${WORK}/render")
TEMPLATE="$(cat "${WORK}/render/template-name")"
cat "${WORK}/render/harness-registration.json"

step "atespace ${ATESPACE} and ActorTemplate ${TEMPLATE}; wait for its golden snapshot"
"${ATE[@]}" get atespace "${ATESPACE}" >/dev/null 2>&1 || "${ATE[@]}" create atespace "${ATESPACE}"
if ! "${ATE[@]}" get actor-template "${TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1; then
	"${ATE[@]}" create actor-template -f "${WORK}/render/actortemplate.json"
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

step "fake model and agentsessionsd (substrate build)"
FAKEMODEL_IMAGE="$(cd "${HERE}" && ko build --base-import-paths ./cmd/fakemodel)"
DAEMON_IMAGE="$(cd "${ROOT}/integrations/substrate" && ko build --base-import-paths ./cmd/agentsessionsd)"
# MODEL_ID and MODEL_BASE_URL were set and validated at the top, before any cluster change.
if [[ -n "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then
	# Replace the key. It reaches the Secret over stdin, never argv or a file.
	"${K[@]}" -n "${NS}" delete secret agentsessionsd-model --ignore-not-found
	if [[ -n "${MODEL_API_KEY:-}" ]]; then
		printf '%s' "${MODEL_API_KEY}" | "${K[@]}" -n "${NS}" create secret generic agentsessionsd-model --from-file=api-key=/dev/stdin
	fi
else
	# Drop any key a previous real-provider run left, so the fake model never receives it.
	"${K[@]}" -n "${NS}" delete secret agentsessionsd-model --ignore-not-found
fi
sed -e "s#FAKEMODEL_IMAGE#${FAKEMODEL_IMAGE}#" "${HERE}/deploy/fakemodel.yaml" | "${K[@]}" apply -f -
sed -e "s#DAEMON_IMAGE#${DAEMON_IMAGE}#" -e "s#MODEL_ID#${MODEL_ID}#" -e "s#MODEL_BASE_URL#${MODEL_BASE_URL}#" \
	-e "s#SUBSTRATE_HARNESS_FLAG#$(cat "${WORK}/render/agentsessionsd.args")#" \
	"${HERE}/deploy/agentsessionsd.yaml" >"${WORK}/agentsessionsd.yaml"
if grep -nE 'DAEMON_IMAGE|MODEL_ID|MODEL_BASE_URL|SUBSTRATE_HARNESS_FLAG' "${WORK}/agentsessionsd.yaml" >&2; then
	echo "unreplaced placeholder in the rendered agentsessionsd.yaml" >&2
	exit 1
fi
"${K[@]}" apply -f "${WORK}/agentsessionsd.yaml"
"${K[@]}" -n "${NS}" rollout status deploy/fakemodel --timeout=180s
"${K[@]}" -n "${NS}" rollout status deploy/agentsessionsd --timeout=180s
"${K[@]}" -n "${NS}" logs deploy/agentsessionsd | grep -E 'listening|substrate harness' || true

step "up; now run ./demo.sh"
