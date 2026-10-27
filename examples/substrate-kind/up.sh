#!/usr/bin/env bash
set -euo pipefail

# Load deployment settings and check prerequisites before touching the cluster.
# Preserve the caller's sandbox choice before common.sh supplies its default.
requested="${SANDBOX_CLASS:-auto}"
source "$(dirname "$0")/common.sh"
progress "Checking setup prerequisites and settings"
source "${HERE}/model.sh"
for tool in git go docker kubectl ko jq; do need "${tool}"; done
timeout_command
[[ "${SUBSTRATE_REF}" =~ ^[0-9a-f]{40}$ ]] || die "Invalid workflow substrate pin."
case "${requested}" in auto | microvm | gvisor) ;; *) die "SANDBOX_CLASS must be auto, microvm, or gvisor." ;; esac

# Build images for the Docker host, which may differ from the machine running this script.
progress "Detecting the Docker host architecture"
arch="$(docker info --format '{{.Architecture}}')"
case "${arch}" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) die "Unsupported Docker architecture." ;; esac
export KO_DEFAULTPLATFORMS="linux/${arch}"
export KO_DOCKER_REPO="${KO_DOCKER_REPO:-localhost:5001}"

# Prefer microVM when Docker can expose /dev/kvm. Only a missing device permits
# automatic gVisor fallback; other Docker errors must not change the selected runtime.
if [[ "${requested}" == gvisor ]]; then
	progress "Skipping the KVM probe; gVisor was selected explicitly"
	SANDBOX_CLASS=gvisor
else
	progress "Checking Docker access to /dev/kvm for microVM workers"
	probe="busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	docker pull --quiet "${probe}" >/dev/null
	if probe_error="$(docker run --rm --device /dev/kvm "${probe}" true 2>&1)"; then
		SANDBOX_CLASS=microvm
	else
		result=$?
		printf '%s\n' "${probe_error}" >&2
		# Docker versions report the missing-device error with either exit status.
		[[ ("${result}" == 125 || "${result}" == 127) && "${probe_error}" == *'error gathering device information while adding custom device "/dev/kvm": no such file or directory'* ]] ||
			die "Docker KVM probe failed; refusing to fall back."
		[[ "${requested}" == auto ]] || die "Explicit microvm requires /dev/kvm access."
		progress "Falling back to gVisor because /dev/kvm is missing in the Docker host."
		SANDBOX_CLASS=gvisor
	fi
fi
export SANDBOX_CLASS SANDBOX_CONFIG ACTOR_TEMPLATE PROJECT
progress "Sandbox: ${SANDBOX_CLASS}; images: linux/${arch}"

# Check ownership and the installed revision before changing the shared checkout.
# Older or unrecorded installations require explicit teardown, never an in-place upgrade.
progress "Checking ownership and setup state for cluster ${CLUSTER}"
lock_cluster
stage="$(cluster_state)"
[[ "${stage}" != creating ]] || die "Incomplete creation; run down.sh before retrying. Storage will not be recreated automatically."
if [[ "${stage}" == created || "${stage}" == installed ]]; then
	read -r _ _ installed_ref <"${OWNER}" || die "Cannot read the cluster's recorded Substrate revision."
	[[ "${installed_ref}" == "${SUBSTRATE_REF}" ]] ||
		die "Cluster Substrate revision is older or unrecorded; Substrate cannot be upgraded in place. Use a new CLUSTER, or run down.sh to delete this cluster and its journal before recreating it."
fi

# Pin the managed Substrate checkout to the CI revision. A caller-supplied checkout
# is read-only to setup: it must already be clean and at that same revision.
if [[ "${SUBSTRATE_DIR}" == "${WORK}/substrate" ]]; then
	progress "Preparing the managed Substrate checkout at the workflow revision"
	[[ ! -L "${SUBSTRATE_DIR}" ]] || die "Managed substrate checkout must not be a symlink."
	if [[ ! -d "${SUBSTRATE_DIR}/.git" ]]; then
		git clone --quiet https://github.com/agent-substrate/substrate "${SUBSTRATE_DIR}"
	fi
	checkout_status="$(git -C "${SUBSTRATE_DIR}" status --porcelain)" || die "Cannot inspect substrate checkout."
	[[ -z "${checkout_status}" ]] || die "Dirty managed substrate checkout."
	git -C "${SUBSTRATE_DIR}" fetch --quiet origin "${SUBSTRATE_REF}"
	git -C "${SUBSTRATE_DIR}" checkout --quiet --detach "${SUBSTRATE_REF}"
else
	progress "Checking the supplied Substrate checkout without modifying it"
	checkout_head="$(git -C "${SUBSTRATE_DIR}" rev-parse HEAD)" || die "Cannot inspect substrate checkout."
	checkout_status="$(git -C "${SUBSTRATE_DIR}" status --porcelain)" || die "Cannot inspect substrate checkout."
	[[ "${checkout_head}" == "${SUBSTRATE_REF}" && -z "${checkout_status}" ]] ||
		die "Custom SUBSTRATE_DIR must be clean and already at the workflow pin; it will not be changed."
fi

# Create only an absent cluster; interrupted creation requires explicit teardown.
if [[ "${stage}" == absent ]]; then
	progress "Creating kind cluster ${CLUSTER}"
	CREATE_PENDING=1
	(cd "${SUBSTRATE_DIR}" && hack/create-kind-cluster.sh)
	id="$(node_id)"
	[[ -n "${id}" ]] || die "New cluster has no control-plane container ID."
	printf '%s created %s\n' "${id}" "${SUBSTRATE_REF}" >"${OWNER}"
	CREATE_PENDING=0
	stage=created
fi

# The upstream creation helper probes KVM independently. Never install or reuse a
# microVM deployment if that helper omitted the node's required device mount.
if [[ "${SANDBOX_CLASS}" == microvm ]]; then
	progress "Checking the owned kind node's /dev/kvm mount"
	id="$(node_id)"
	[[ -n "${id}" ]] || die "Cluster has no control-plane container ID."
	mounts="$(docker inspect --format '{{json .Mounts}}' "${id}")" ||
		die "Cannot inspect the kind node's mounts."
	jq -e 'any(.[]; .Source == "/dev/kvm" and .Destination == "/dev/kvm" and .RW == true)' <<<"${mounts}" >/dev/null ||
		die "Kind node lacks the required writable /dev/kvm mount; run down.sh before recreating this cluster."
fi

# Install the Substrate control plane only on a newly created cluster. Keep an
# installed cluster intact on reruns, and record completion only if its identity still matches.
kind_command export kubeconfig --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}"
if [[ "${stage}" == created ]]; then
	progress "Installing the Substrate control plane in cluster ${CLUSTER}"
	(cd "${SUBSTRATE_DIR}" && env -u GCE_REGION -u CLUSTER_LOCATION -u NETWORK -u SUBNETWORK \
		-u MEMORYSTORE_INSTANCE -u PROJECT_ID \
		go run ./cmd/ate-setup --kind --no-dev-env --kind-cluster-name="${CLUSTER}" \
		--context="kind-${CLUSTER}" --kubeconfig="${KUBECONFIG}" --ko-default-platforms="linux/${arch}" \
		--credential-provider='{"name":"k8s.io"}' --rollout-timeout=10m deploy ate-system --setup-csi=none)
	stage="$(cluster_state)"
	[[ "${stage}" == created ]] || die "Cluster changed during installation."
	id="$(node_id)"
	[[ -n "${id}" ]] || die "Cluster disappeared during installation."
	printf '%s installed %s\n' "${id}" "${SUBSTRATE_REF}" >"${OWNER}"
else
	progress "Reusing owned cluster ${CLUSTER}; retaining its journal."
fi

# MicroVM workers need the guest runtime assets and SandboxConfig in addition to the control plane.
if [[ "${SANDBOX_CLASS}" == microvm ]]; then
	progress "Installing microVM runtime assets and sandbox configuration"
	(cd "${SUBSTRATE_DIR}" && NO_DEV_ENV=true ATE_INSTALL_KIND=true BUCKET_NAME=ate-snapshots \
		KUBECTL_CONTEXT="kind-${CLUSTER}" ARCH="${arch}" hack/install-microvm-deps.sh --install)
fi

# Build the matching Control API client and publish the daemon, harness and worker images.
# The deterministic provider image is needed only when explicitly selecting the stub.
progress "Building the Substrate Control API client"
(cd "${SUBSTRATE_DIR}" && go build -o "${STATE}/kubectl-ate" ./cmd/kubectl-ate)
progress "Building and publishing the Sessions daemon image"
DAEMON_IMAGE="$(cd "${ROOT}/integrations/substrate" && ko build ./cmd/agentsessionsd)"
progress "Building and publishing the chat harness image"
HARNESS_IMAGE="$(cd "${ROOT}" && ko build ./cmd/harnessnode)"
progress "Building and publishing the ${SANDBOX_CLASS} worker image"
WORKER_IMAGE="$(cd "${SUBSTRATE_DIR}" && ko build "./cmd/ateom-${SANDBOX_CLASS}")"
export DAEMON_IMAGE HARNESS_IMAGE WORKER_IMAGE
if [[ "${MODEL_SOURCE}" == stub ]]; then
	progress "Building and publishing the deterministic model stub image"
	MODELSTUB_IMAGE="$(cd "${HERE}" && ko build ./cmd/modelstub)"
	export MODELSTUB_IMAGE
fi

# Hand off application deployment to the same entry point used by CI.
progress "Deploying the Sessions application"
bash "${HERE}/deploy.sh"
progress "Setup complete. Run examples/substrate-kind/smoke.sh, or forward service/agentsessions in namespace ${NS}."
