#!/usr/bin/env bash
set -euo pipefail

# Validate immutable template inputs and select the SandboxConfig for the worker class.
source "$(dirname "$0")/common.sh"
need jq
timeout_command
: "${HARNESS_IMAGE:?digest-pinned HARNESS_IMAGE is required}"
: "${MODEL_NAME:?MODEL_NAME is required}"
[[ "${HARNESS_IMAGE}" =~ @sha256:[0-9a-f]{64}$ ]] || die "HARNESS_IMAGE must be digest-pinned."
[[ "${ACTOR_TEMPLATE}" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "Invalid ACTOR_TEMPLATE name."
case "${SANDBOX_CLASS}" in
microvm) SANDBOX_CONFIG="${SANDBOX_CONFIG:-microvm}" ;;
gvisor) SANDBOX_CONFIG="${SANDBOX_CONFIG:-gvisor-default}" ;;
*) die "SANDBOX_CLASS must be microvm or gvisor." ;;
esac

# All Control API calls and golden-snapshot polling share one provisioning deadline.
budget="${PROVISION_TIMEOUT_SECONDS:-600}"
[[ "${budget}" =~ ^[1-9][0-9]*$ ]] || die "PROVISION_TIMEOUT_SECONDS must be positive."
deadline=$((SECONDS + budget))

# Parameterize the declarative ActorTemplate as JSON, preserving escaping in image/model values.
# ActorTemplates belong to the Substrate Control API, so kubectl cannot apply them directly.
progress "Rendering the desired ActorTemplate ${NS}/${ACTOR_TEMPLATE}"
desired="${STATE}/actor-template.json"
jq --arg image "${HARNESS_IMAGE}" --arg model "${MODEL_NAME}" \
	--arg name "${ACTOR_TEMPLATE}" --arg class "${SANDBOX_CLASS}" --arg config "${SANDBOX_CONFIG}" \
	--arg location "${SNAPSHOT_LOCATION%/}/${ACTOR_TEMPLATE}/" '
	.metadata.name = $name |
	.containers[0].image = $image |
	.containers[0].env |= map(if .name == "HARNESS_MODEL" then .value = $model else . end) |
	.workerSelector.matchLabels.workload = ("sessions-example-" + $class) |
	.sandboxConfig = {sandboxClass: $class, configName: $config} |
	.snapshotConfig.storageLocation = $location
' "${HERE}/deploy/actor-template.json" >"${desired}"

# Bound each CLI call by the remaining budget and accept only an explicitly allowed RPC error.
# Represent expected absence as null; malformed responses and ambiguous errors stop provisioning.
ate() {
	local allowed="$1" output remaining codes
	shift
	remaining=$((deadline - SECONDS))
	((remaining > 0)) || die "Template provisioning deadline exceeded."
	((remaining <= 120)) || remaining=120
	if output="$("${TIMEOUT}" --kill-after=5s "${remaining}s" "${ATE[@]}" "$@" 2>"${STATE}/ate-error")"; then
		jq -e 'type == "object" and length > 0' <<<"${output}" >/dev/null ||
			die "kubectl-ate returned an ambiguous resource."
		printf '%s\n' "${output}"
	else
		codes="$(grep -o 'rpc error: code = [A-Za-z]* desc =' "${STATE}/ate-error" | cut -d ' ' -f 5)" || codes=""
		if [[ -n "${allowed}" && "${codes}" == "${allowed}" ]]; then
			echo null
		else
			cat "${STATE}/ate-error" >&2
			die "kubectl-ate failed; template provisioning refused."
		fi
	fi
}

# Ensure the atespace exists and verify its identity. Preflight never creates missing resources.
progress "Checking Substrate atespace ${NS}"
space="$(ate NotFound get atespace "${NS}")"
if [[ "${space}" == null ]]; then
	if [[ "${1:-}" == --check-only ]]; then
		progress "Atespace is absent; preflight complete without creating resources"
		exit 0
	fi
	progress "Creating Substrate atespace ${NS}"
	ate AlreadyExists create atespace "${NS}" >/dev/null
	space="$(ate "" get atespace "${NS}")"
fi
jq -e --arg name "${NS}" '.metadata.name == $name' <<<"${space}" >/dev/null ||
	die "kubectl-ate returned a different atespace."

# Create the template only when absent; an AlreadyExists race is checked by reading it back.
progress "Checking ActorTemplate ${NS}/${ACTOR_TEMPLATE}"
current="$(ate NotFound get actor-template "${ACTOR_TEMPLATE}" -a "${NS}")"
if [[ "${current}" == null ]]; then
	if [[ "${1:-}" == --check-only ]]; then
		progress "ActorTemplate is absent; preflight complete without creating resources"
		exit 0
	fi
	progress "Creating ActorTemplate ${NS}/${ACTOR_TEMPLATE}"
	ate AlreadyExists create actor-template -f "${desired}" >/dev/null
fi

# Require an exact immutable specification match before using an existing template.
# Preflight stops after validation; full provisioning also waits for a usable golden snapshot.
if [[ "${1:-}" == --check-only ]]; then
	progress "Validating the existing ActorTemplate specification"
else
	progress "Validating the ActorTemplate and waiting for its golden snapshot"
fi
while :; do
	current="$(ate "" get actor-template "${ACTOR_TEMPLATE}" -a "${NS}")"
	jq -e --arg name "${ACTOR_TEMPLATE}" --arg ns "${NS}" --slurpfile wanted "${desired}" '
		.metadata.name == $name and .metadata.atespace == $ns and
		(del(.metadata, .status) == ($wanted[0] | del(.metadata, .status)))
	' <<<"${current}" >/dev/null || die "Immutable template mismatch; choose a new ACTOR_TEMPLATE before changing the deployment."
	error="$(jq -r '.status.goldenSnapshotStatus.errorMessage // empty' <<<"${current}")"
	[[ -z "${error}" ]] || die "Golden snapshot failed: ${error}"
	if [[ "${1:-}" == --check-only ]]; then
		progress "ActorTemplate compatibility check passed"
		exit 0
	fi
	if jq -e '.status.goldenSnapshotStatus.goldenTag.name | strings | length > 0' <<<"${current}" >/dev/null; then
		progress "ActorTemplate ${NS}/${ACTOR_TEMPLATE} ready."
		break
	fi
	((SECONDS < deadline)) || die "Golden snapshot deadline exceeded."
	sleep 1
done
