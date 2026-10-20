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
SUBSTRATE_DIR="${SUBSTRATE_DIR:-${ROOT}/_substrate}"
KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"
KO_DEFAULTBASEIMAGE="${KO_DEFAULTBASEIMAGE:-gcr.io/distroless/static-debian13}"
if [[ -z "${ATE_CREDENTIAL_PROVIDER:-}" ]]; then
	ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}'
fi
export ATE_CREDENTIAL_PROVIDER="${ATE_CREDENTIAL_PROVIDER}"
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
	for command in git docker kubectl ko go python3; do
		require_command "${command}"
	done
	SUBSTRATE_REF="${SUBSTRATE_REF:-$(sed -n 's/^  SUBSTRATE_REF: //p' "${ROOT}/.github/workflows/substrate-e2e.yml")}"
	if [[ ! "${SUBSTRATE_REF}" =~ ^[0-9a-f]{40}$ ]]; then
		echo "SUBSTRATE_REF must be a full commit SHA" >&2
		exit 2
	fi
	local kvm_error
	if kvm_error="$(python3 - 2>&1 <<'PY'
import fcntl
import os
import sys
try:
    fd = os.open("/dev/kvm", os.O_RDWR)
    try:
        if fcntl.ioctl(fd, 0xAE00, 0) != 12:
            raise OSError("unsupported KVM API version")
        vm = fcntl.ioctl(fd, 0xAE01, 0)
        os.close(vm)
    finally:
        os.close(fd)
except OSError as e:
    print(e, file=sys.stderr)
    sys.exit(1)
PY
	)"; then
		SANDBOX_CLASS="${SANDBOX_CLASS:-microvm}"
	else
		if [[ "${SANDBOX_CLASS:-}" == "microvm" ]]; then
			echo "microvm requires usable /dev/kvm: ${kvm_error}" >&2
			exit 2
		fi
		if [[ -z "${SANDBOX_CLASS:-}" ]]; then
			echo "KVM unavailable (${kvm_error}); falling back to gvisor." >&2
			SANDBOX_CLASS=gvisor
		fi
	fi
	case "${SANDBOX_CLASS}" in
	microvm) SANDBOX_CONFIG="${SANDBOX_CONFIG:-microvm}" ;;
	gvisor) SANDBOX_CONFIG="${SANDBOX_CONFIG:-gvisor-default}" ;;
	*) echo "SANDBOX_CLASS must be microvm or gvisor" >&2; exit 2 ;;
	esac
	echo "==> selected sandbox: ${SANDBOX_CLASS} (${SANDBOX_CONFIG})"
	export KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/$(go env GOARCH)}"
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

	if [[ "$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "${SUBSTRATE_DIR}")" == "${ROOT}/_substrate" ]]; then
		if [[ ! -d "${SUBSTRATE_DIR}/.git" ]]; then
			git clone https://github.com/agent-substrate/substrate "${SUBSTRATE_DIR}"
		fi
		if [[ -n "$(git -C "${SUBSTRATE_DIR}" status --porcelain)" ]]; then
			echo "Refusing to change dirty Substrate checkout ${SUBSTRATE_DIR}" >&2
			exit 2
		fi
		git -C "${SUBSTRATE_DIR}" fetch origin --quiet
		git -C "${SUBSTRATE_DIR}" checkout --detach "${SUBSTRATE_REF}" --quiet
	elif [[ "$(git -C "${SUBSTRATE_DIR}" rev-parse HEAD)" != "${SUBSTRATE_REF}" ]] ||
		[[ -n "$(git -C "${SUBSTRATE_DIR}" status --porcelain)" ]]; then
		echo "Custom SUBSTRATE_DIR must be clean and already at ${SUBSTRATE_REF}; it will not be fetched or checked out." >&2
		exit 2
	fi

	echo "==> create kind cluster ${CLUSTER} and install pinned ate-system"
	(cd "${SUBSTRATE_DIR}" && KIND_CLUSTER_NAME="${CLUSTER}" hack/create-kind-cluster.sh)
	(cd "${SUBSTRATE_DIR}" && KIND_CLUSTER_NAME="${CLUSTER}" KUBECTL_CONTEXT="${CONTEXT}" hack/install-ate-kind.sh --deploy-ate-system)
	if [[ "${SANDBOX_CLASS}" == "microvm" ]]; then
		(cd "${SUBSTRATE_DIR}" && NO_DEV_ENV=true ATE_INSTALL_KIND=true BUCKET_NAME=ate-snapshots \
			KO_DOCKER_REPO="${KO_DOCKER_REPO}" KUBECTL_CONTEXT="${CONTEXT}" \
			hack/install-microvm-deps.sh --install)
	fi

	echo "==> build and publish control-plane, harness, and worker images"
	export KO_DOCKER_REPO KO_DEFAULTBASEIMAGE
	HARNESS_IMG="$(cd "${ROOT}" && ko build ./cmd/harnessnode)"
	CONTROL_IMG="$(cd "${ROOT}/integrations/substrate" && ko build ./cmd/controlplane)"
	ATEOM_IMG="$(cd "${SUBSTRATE_DIR}" && ko build "./cmd/ateom-${SANDBOX_CLASS}")"
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
		"WORKER_IMAGE" "${ATEOM_IMG}" \
		"SANDBOX_CLASS" "${SANDBOX_CLASS}" |
		kubectl_kind apply -f -
	KIND_CLUSTER_NAME="${CLUSTER}" "${ROOT}/hack/wait-worker-pool.sh" chat-harness "${NAMESPACE}"

	echo "==> deploy persistent Sessions control plane"
	render "${ROOT}/deploy/substrate/kind/control-plane.yaml" \
		"ghcr.io/agent-primitives/sessions/controlplane:latest" "${CONTROL_IMG}" \
		"HARNESS_IMAGE" "${HARNESS_IMG}" \
		"SANDBOX_CLASS" "${SANDBOX_CLASS}" \
		"SANDBOX_CONFIG" "${SANDBOX_CONFIG}" |
		kubectl_kind apply -f -
	kubectl_kind rollout status deployment/agentsessions -n "${NAMESPACE}" --timeout=660s

	echo
	echo "Deployment ready. In another terminal:"
	echo "  CLUSTER=${CLUSTER} ${ROOT}/hack/kind-production.sh port-forward"
	echo "Then use: agentctl exec --server 127.0.0.1:8080 --project '${PROJECT}' --harness chat --input 'hello'"
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
