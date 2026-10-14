#!/usr/bin/env bash
# Creates or removes the production-shaped agentsessions kind deployment.
#
# Usage:
#   USE_OLLAMA=1 hack/kind-production.sh setup
#   MODEL_NAME=... MODEL_BASE_URL=... hack/kind-production.sh setup
#   hack/kind-production.sh port-forward
#   hack/kind-production.sh smoke
#   hack/kind-production.sh teardown
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACTION="${1:-}"
CLUSTER="${CLUSTER:-agentsessions-production}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="ate-agentsessions"
SUBSTRATE_REF="${SUBSTRATE_REF:-fc0e3586a6de7cf1b896b948d05df7d5ef8b8870}"
SUBSTRATE_DIR="${SUBSTRATE_DIR:-${ROOT}/_substrate}"
KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"
KO_DEFAULTBASEIMAGE="${KO_DEFAULTBASEIMAGE:-gcr.io/distroless/static-debian13}"
export ATE_CREDENTIAL_PROVIDER="${ATE_CREDENTIAL_PROVIDER:-{\"name\":\"k8s.io\"}}"
export ATE_INSTALL_ROLLOUT_TIMEOUT="${ATE_INSTALL_ROLLOUT_TIMEOUT:-10m}"

kubectl_kind() {
	kubectl --context "${CONTEXT}" "$@"
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "missing required command: $1" >&2
		exit 1
	}
}

render() {
	local file="$1"
	shift
	local args=()
	while (($#)); do
		local name="$1"
		local value="$2"
		shift 2
		value="${value//\\/\\\\}"
		value="${value//|/\\|}"
		value="${value//&/\\&}"
		args+=("-e" "s|${name}|${value}|g")
	done
	sed "${args[@]}" "${file}"
}

setup() {
	# The pinned Substrate checkout runs its own kind version through hack/kind.sh.
	for command in git docker kubectl ko; do
		require_command "${command}"
	done
	USE_OLLAMA="${USE_OLLAMA:-0}"
	USE_MODELSTUB="${USE_MODELSTUB:-0}"
	if [[ "${USE_OLLAMA}" == "1" && "${USE_MODELSTUB}" == "1" ]]; then
		echo "USE_OLLAMA and USE_MODELSTUB are mutually exclusive" >&2
		exit 2
	fi
	if [[ "${USE_OLLAMA}" == "1" ]]; then
		MODEL_NAME="${MODEL_NAME:-gemma3:270m}"
		MODEL_BASE_URL="http://ollama.${NAMESPACE}.svc:11434/v1"
		MODEL_PATH="/chat/completions"
	elif [[ "${USE_MODELSTUB}" == "1" ]]; then
		MODEL_NAME="${MODEL_NAME:-smoke-model}"
		MODEL_BASE_URL="http://modelstub.${NAMESPACE}.svc:8080/v1"
		MODEL_PATH="/chat/completions"
	else
		: "${MODEL_NAME:?MODEL_NAME is required}"
		: "${MODEL_BASE_URL:?MODEL_BASE_URL must be reachable from inside the kind cluster}"
		MODEL_PATH="${MODEL_PATH:-/chat/completions}"
	fi
	MODEL_AUTH_HEADER="${MODEL_AUTH_HEADER:-Authorization}"
	PROJECT="${PROJECT:-default}"

	if [[ ! -d "${SUBSTRATE_DIR}/.git" ]]; then
		git clone https://github.com/agent-substrate/substrate "${SUBSTRATE_DIR}"
	fi
	git -C "${SUBSTRATE_DIR}" fetch origin --quiet
	git -C "${SUBSTRATE_DIR}" checkout --detach "${SUBSTRATE_REF}" --quiet

	echo "==> create kind cluster ${CLUSTER} and install pinned ate-system"
	(cd "${SUBSTRATE_DIR}" && KIND_CLUSTER_NAME="${CLUSTER}" hack/create-kind-cluster.sh)
	(cd "${SUBSTRATE_DIR}" && KIND_CLUSTER_NAME="${CLUSTER}" KUBECTL_CONTEXT="${CONTEXT}" hack/install-ate-kind.sh --deploy-ate-system)

	echo "==> build and publish control-plane, harness, and worker images"
	export KO_DOCKER_REPO KO_DEFAULTBASEIMAGE
	HARNESS_IMG="$(cd "${ROOT}" && ko build ./cmd/harnessnode)"
	CONTROL_IMG="$(cd "${ROOT}/integrations/substrate" && ko build ./cmd/controlplane)"
	ATEOM_IMG="$(cd "${SUBSTRATE_DIR}" && ko build ./cmd/ateom-gvisor)"
	if [[ "${USE_MODELSTUB}" == "1" ]]; then
		MODELSTUB_IMG="$(cd "${ROOT}/integrations/substrate" && ko build ./cmd/modelstub)"
	fi

	echo "==> configure host-side model mediation"
	kubectl_kind create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl_kind apply -f -
	if [[ "${USE_OLLAMA}" == "1" ]]; then
		kubectl_kind apply -f "${ROOT}/deploy/substrate/kind/ollama.yaml"
		kubectl_kind rollout status deployment/ollama -n "${NAMESPACE}" --timeout=600s
		echo "==> pull Ollama model ${MODEL_NAME}"
		kubectl_kind exec -n "${NAMESPACE}" deployment/ollama -- ollama pull "${MODEL_NAME}"
	elif [[ "${USE_MODELSTUB}" == "1" ]]; then
		render "${ROOT}/deploy/substrate/kind/modelstub.yaml" \
			"MODELSTUB_IMAGE" "${MODELSTUB_IMG}" |
			kubectl_kind apply -f -
		kubectl_kind rollout status deployment/modelstub -n "${NAMESPACE}" --timeout=300s
	fi
	kubectl_kind create configmap agentsessions-model -n "${NAMESPACE}" \
		--from-literal=project="${PROJECT}" \
		--from-literal=model="${MODEL_NAME}" \
		--from-literal=base-url="${MODEL_BASE_URL}" \
		--from-literal=path="${MODEL_PATH}" \
		--from-literal=auth-header="${MODEL_AUTH_HEADER}" \
		--dry-run=client -o yaml | kubectl_kind apply -f -
	if [[ -n "${MODEL_API_KEY:-}" ]]; then
		printf '%s' "${MODEL_API_KEY}" | kubectl_kind create secret generic agentsessions-model -n "${NAMESPACE}" \
			--from-file=api-key=/dev/stdin \
			--dry-run=client -o yaml | kubectl_kind apply -f -
	fi

	echo "==> deploy chat worker pool"
	render "${ROOT}/deploy/substrate/kind/chat-workerpool.yaml" \
		"ko://github.com/agent-substrate/substrate/cmd/ateom-gvisor" "${ATEOM_IMG}" |
		kubectl_kind apply -f -
	KIND_CLUSTER_NAME="${CLUSTER}" "${ROOT}/hack/wait-worker-pool.sh" chat-harness "${NAMESPACE}"

	echo "==> deploy persistent Sessions control plane"
	render "${ROOT}/deploy/substrate/kind/control-plane.yaml" \
		"ghcr.io/aramase/agentsessions/controlplane:latest" "${CONTROL_IMG}" \
		"HARNESS_IMAGE" "${HARNESS_IMG}" |
		kubectl_kind apply -f -
	kubectl_kind rollout status deployment/agentsessions -n "${NAMESPACE}" --timeout=660s

	echo
	echo "Deployment ready. In another terminal:"
	echo "  CLUSTER=${CLUSTER} ${ROOT}/hack/kind-production.sh port-forward"
	echo "Then use: agentctl exec --server 127.0.0.1:8080 --harness chat --input 'hello'"
}

case "${ACTION}" in
setup)
	setup
	;;
port-forward)
	kubectl_kind port-forward -n "${NAMESPACE}" service/agentsessions 8080:8080
	;;
smoke)
	CLUSTER="${CLUSTER}" "${ROOT}/hack/smoke-production-kind.sh"
	;;
teardown)
	if [[ -x "${SUBSTRATE_DIR}/hack/kind.sh" ]]; then
		(cd "${SUBSTRATE_DIR}" && hack/kind.sh delete cluster --name "${CLUSTER}")
	else
		require_command kind
		kind delete cluster --name "${CLUSTER}"
	fi
	echo "Shared kind-registry left intact for other clusters."
	;;
*)
	echo "usage: $0 <setup|port-forward|smoke|teardown>" >&2
	exit 2
	;;
esac
