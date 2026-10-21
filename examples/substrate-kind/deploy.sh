#!/usr/bin/env bash
# Deploy into an already-installed substrate cluster; never create or adopt a cluster here.
set -euo pipefail

# Load the selected runtime/provider and require the images produced by local setup or CI.
source "$(dirname "$0")/common.sh"
progress "Checking deployment settings and image inputs"
source "${HERE}/model.sh"
need jq
need kubectl
: "${DAEMON_IMAGE:?DAEMON_IMAGE is required}"
: "${HARNESS_IMAGE:?HARNESS_IMAGE is required}"
: "${WORKER_IMAGE:?WORKER_IMAGE is required}"
export SANDBOX_CLASS SANDBOX_CONFIG ACTOR_TEMPLATE HARNESS_IMAGE MODEL_NAME

# Refuse an incompatible immutable template before changing workers or the daemon.
progress "Checking ActorTemplate compatibility before changing the deployment"
bash "${HERE}/provision.sh" --check-only

# Bring up the workers before creating the template and waiting for its golden snapshot.
progress "Deploying the ${SANDBOX_CLASS} worker pool and waiting for readiness"
render "${HERE}/deploy/workerpool.yaml" WORKER_IMAGE "${WORKER_IMAGE}" SANDBOX_CLASS "${SANDBOX_CLASS}" |
	"${K[@]}" apply -f -
"${ROOT}/hack/wait-worker-pool.sh" "chat-harness-${SANDBOX_CLASS}" "${NS}"
bash "${HERE}/provision.sh"

# Start an in-cluster provider when selected. External providers need configuration only.
case "${MODEL_SOURCE}" in
ollama)
	timeout_command
	progress "Deploying Ollama and waiting for readiness"
	"${K[@]}" apply -f "${HERE}/deploy/ollama.yaml"
	"${K[@]}" rollout status deployment/ollama -n "${NS}" --timeout=600s
	progress "Downloading the configured Ollama model into its persistent cache"
	if ! "${TIMEOUT}" --kill-after=5s 600s "${K[@]}" exec -n "${NS}" deployment/ollama -- ollama pull "${MODEL_NAME}"; then
		die "Ollama model download failed or exceeded its ten-minute deadline."
	fi
	;;
stub)
	progress "Deploying the deterministic model stub and waiting for readiness"
	: "${MODELSTUB_IMAGE:?MODELSTUB_IMAGE is required for MODEL_SOURCE=stub}"
	render "${HERE}/deploy/modelstub.yaml" MODELSTUB_IMAGE "${MODELSTUB_IMAGE}" | "${K[@]}" apply -f -
	"${K[@]}" rollout status deployment/modelstub -n "${NS}" --timeout=300s
	;;
external)
	progress "Using the configured external model provider; no provider deployment is needed"
	;;
esac

# Configure the daemon's project and model route separately from provider credentials.
progress "Applying the Sessions project and model configuration"
"${K[@]}" create configmap model -n "${NS}" \
	--from-literal=PROJECT="${PROJECT}" --from-literal=MODEL_NAME="${MODEL_NAME}" \
	--from-literal=MODEL_BASE_URL="${MODEL_BASE_URL}" --from-literal=MODEL_PATH="${MODEL_PATH}" \
	--from-literal=MODEL_AUTH_HEADER="${MODEL_AUTH_HEADER}" --dry-run=client -o yaml | "${K[@]}" apply -f -

# Send the key through stdin, not argv. Removing a key also removes a stale Secret on reruns.
if [[ -n "${MODEL_API_KEY:-}" ]]; then
	progress "Updating the provider credential Secret"
	printf '%s' "${MODEL_API_KEY}" | "${K[@]}" create secret generic model -n "${NS}" \
		--from-file=MODEL_API_KEY=/dev/stdin --dry-run=client -o yaml | "${K[@]}" apply -f -
else
	progress "Removing any stale provider credential Secret; no key was supplied"
	"${K[@]}" delete secret model -n "${NS}" --ignore-not-found
fi

# Resource-version annotations restart the daemon when its configuration changes,
# without putting secret values or their hashes into the pod template.
progress "Deploying the Sessions daemon with its persistent journal and waiting for readiness"
config_version="$("${K[@]}" get configmap model -n "${NS}" -o jsonpath='{.metadata.resourceVersion}')"
secret_version="$("${K[@]}" get secret model -n "${NS}" --ignore-not-found -o jsonpath='{.metadata.resourceVersion}')"
[[ -n "${config_version}" ]] || die "Missing model ConfigMap version."
render "${HERE}/deploy/agentsessionsd.yaml" DAEMON_IMAGE "${DAEMON_IMAGE}" ACTOR_TEMPLATE "${ACTOR_TEMPLATE}" \
	MODEL_CONFIG_VERSION "${config_version}" MODEL_SECRET_VERSION "${secret_version:-absent}" | "${K[@]}" apply -f -
"${K[@]}" rollout status deployment/agentsessions -n "${NS}" --timeout=300s
progress "Sessions application deployment is ready"
