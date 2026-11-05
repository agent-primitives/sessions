#!/usr/bin/env bash
set -euo pipefail

# Validate immutable template inputs. The class selects the worker pool; the pool resolves the
# class's default SandboxConfig, so the template names no config.
source "$(dirname "$0")/common.sh"
refuse_template_overrides
need jq
timeout_command
: "${HARNESS_IMAGE:?digest-pinned HARNESS_IMAGE is required}"
: "${MODEL_NAME:?MODEL_NAME is required}"
[[ "${HARNESS_IMAGE}" =~ @sha256:[0-9a-f]{64}$ ]] || die "HARNESS_IMAGE must be digest-pinned."
[[ "${ACTOR_TEMPLATE}" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "Invalid ACTOR_TEMPLATE name."
case "${SANDBOX_CLASS}" in
microvm | gvisor) ;;
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
	--arg name "${ACTOR_TEMPLATE}" --arg class "${SANDBOX_CLASS}" '
	.metadata.name = $name |
	.containers[0].image = $image |
	.containers[0].env |= map(if .name == "HARNESS_MODEL" then .value = $model else . end) |
	.workerSelector.matchLabels.workload = ("sessions-example-" + $class)
' "${HERE}/deploy/actor-template.json" >"${desired}"

# Every kubectl-ate call (hack/substrate-lib.sh's ate) ends by the provisioning deadline, and a
# failed one stops provisioning with ATE_FAILURE.
# shellcheck disable=SC2034 # hack/substrate-lib.sh reads it
ATE_DEADLINE="${deadline}"
# shellcheck disable=SC2034 # hack/substrate-lib.sh reads it
ATE_FAILURE="kubectl-ate failed; template provisioning refused."

# The daemon needs the atespace editor role (or owner) in NS. Preflight never creates missing
# resources: it stops at an absent atespace, and reports an absent access policy.
check_only=()
if [[ "${1:-}" == --check-only ]]; then check_only=(--check-only); fi
ensure_atespace_editor ${check_only[@]+"${check_only[@]}"} "${NS}" "${DAEMON_PRINCIPAL}"
if [[ "${ATESPACE_ABSENT}" == true ]]; then
	progress "Atespace is absent; preflight complete without creating resources"
	exit 0
fi

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
