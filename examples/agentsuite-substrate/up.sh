#!/usr/bin/env bash
# Brings up the demo: a kind cluster with agent-substrate installed by its own kind lane (ate-setup),
# the micro-VM sandbox class (or gVisor, AGENTSUITE_DEMO_SANDBOX=gvisor) and a WorkerPool for it, the
# agent's harness image and two ActorTemplates rendered from the AgentSuite composition (the
# stateless tier and the memory tier), the fake model, and two agentsessionsd Deployments (substrate
# build): agentsessionsd serves the agent as harness <agent>, agentsessionsd-memory as
# <agent>-memory.
#
# --substrate-only stops once ate-system, the sandbox class and the WorkerPool are up;
# --templates-only once the ActorTemplates hold their golden snapshots, before the model Secret and
# the Deployments change.
#
# Idempotent: an existing cluster, template or deployment is reused. See README.md for the tools.
set -euo pipefail
source "$(dirname "$0")/common.sh"

SUBSTRATE_ONLY=false TEMPLATES_ONLY=false
case "${1:-}" in
"") ;;
--substrate-only) SUBSTRATE_ONLY=true ;;
--templates-only) TEMPLATES_ONLY=true ;;
*)
	echo "usage: $0 [--substrate-only | --templates-only]" >&2
	exit 2
	;;
esac

# AGENTSUITE_DEMO_SKIP_TEMPLATE names a tier (stateless or memory) whose ActorTemplate is rendered
# but not created, for a caller that creates it itself (the recorded demo creates the stateless one
# on camera). agentsessionsd is still deployed with that tier's -substrate-harness flag: it reads the
# template only when it creates a session's actor, not at startup.
SKIP_TEMPLATE="${AGENTSUITE_DEMO_SKIP_TEMPLATE:-}"
case "${SKIP_TEMPLATE}" in
"" | stateless | memory) ;;
*)
	echo "AGENTSUITE_DEMO_SKIP_TEMPLATE=${SKIP_TEMPLATE}: want stateless or memory; nothing was changed" >&2
	exit 1
	;;
esac

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
DAEMON_PLACEHOLDERS=(MODEL_ID MODEL_BASE_URL MODEL_PATH MODEL_AUTH_HEADER DAEMON_NAME DAEMON_IMAGE SUBSTRATE_HARNESS_FLAG MODEL_SECRET_VERSION)
# unfilled prints the placeholders the text $1 still contains, in DAEMON_PLACEHOLDERS order.
unfilled() {
	local p left=""
	for p in "${DAEMON_PLACEHOLDERS[@]}"; do
		if [[ "$1" == *"${p}"* ]]; then left="${left:+${left} }${p}"; fi
	done
	echo "${left}"
}

# Only AGENTSUITE_MODEL_BASE_URL selects a real provider. Any other provider setting without it is a
# provider run that lost its URL: going on would switch to the fake model, delete the stored key and
# replace the daemon's pod, dropping its emptyDir journal.
if [[ -z "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then
	for var in AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AGENTSUITE_MODEL_AUTH_HEADER MODEL_API_KEY; do
		if [[ -n "${!var:-}" ]]; then
			refuse "${var} is set without AGENTSUITE_MODEL_BASE_URL; set both for a real provider, or unset ${var} for the fake model"
		fi
	done
fi
if [[ -n "${AGENTSUITE_MODEL_BASE_URL:-}" ]]; then
	# A real OpenAI-compatible provider.
	MODEL_BASE_URL="${AGENTSUITE_MODEL_BASE_URL}"
	if [[ -z "${AGENTSUITE_MODEL:-}" ]]; then
		refuse "AGENTSUITE_MODEL_BASE_URL is set without AGENTSUITE_MODEL; set the provider's model"
	fi
	MODEL_ID="${AGENTSUITE_MODEL}"
	# The completions path under the base URL and the header that carries MODEL_API_KEY, which
	# agentsessionsd takes as -model-path and -model-auth-header. An Azure OpenAI deployment needs
	# both: /openai/deployments/<name>/chat/completions?api-version=<date> and api-key. Unset, they
	# are the daemon's defaults.
	MODEL_PATH="${AGENTSUITE_MODEL_PATH:-/chat/completions}"
	MODEL_AUTH_HEADER="${AGENTSUITE_MODEL_AUTH_HEADER:-Authorization}"
	# The harness requests the provider's model, not the fixture's.
	HARNESS_MODEL="${MODEL_ID}"
	# Each lands in a YAML plain scalar ("- -model=VALUE"): whitespace or a control character would
	# change the YAML around it, a trailing ':' would turn the item into a map that kubectl refuses
	# only after the Secret has changed, and a placeholder name would be replaced again further down.
	# With no whitespace allowed, a trailing ':' is the one remaining way to end a plain scalar
	# early (": " and " #" both need a space).
	unsafe='[[:space:][:cntrl:]]'
	for var in AGENTSUITE_MODEL_BASE_URL AGENTSUITE_MODEL AGENTSUITE_MODEL_PATH AGENTSUITE_MODEL_AUTH_HEADER; do
		if [[ "${!var:-}" =~ ${unsafe} ]]; then
			refuse "${var} contains whitespace or a control character"
		fi
		if [[ "${!var:-}" == *: ]]; then
			refuse "${var} ends with ':', which deploy/agentsessionsd.yaml would read as a map key"
		fi
		if [[ -n "$(unfilled "${!var:-}")" ]]; then
			refuse "${var} contains $(unfilled "${!var:-}"), a placeholder of deploy/agentsessionsd.yaml"
		fi
	done
	# agentsessionsd refuses model settings its model client cannot be built from at startup.
	# check-model also refuses a URL with userinfo, a query or a fragment, a path whose query holds
	# anything but a date-shaped api-version, and an auth header that is not a header name: the URL
	# is printed below, the daemon logs the URL and path, and both are rendered into the
	# Deployment's arguments and ${STATE}/agentsessionsd.yaml, so a credential in either would
	# bypass the Secret that holds MODEL_API_KEY.
	# The Deployment recreates its pod, and the journal is an emptyDir, so letting such a value
	# through would delete the running daemon's sessions and leave a pod that never starts. emit
	# check-model builds the client with the daemon's own constructor, and also requires http(s).
	# It is built first and run as a binary: go run would append its own "exit status 1" to the
	# reason, and a build failure is not a verdict on the values.
	if ! reason="$(cd "${HERE}" && go build -o "${WORK}/bin/emit" ./cmd/emit 2>&1)"; then
		refuse "cannot build cmd/emit to check AGENTSUITE_MODEL_BASE_URL and AGENTSUITE_MODEL: ${reason}"
	fi
	# With a key, check-model refuses a plain-http URL whose host is not loopback, localhost or a
	# *.svc or *.svc.cluster.local name (a trusted-network exception: a name check, not proof the
	# traffic stays in the cluster), as the daemon would. It is told only that a key is set, never
	# the key.
	key_set=()
	if [[ -n "${MODEL_API_KEY:-}" ]]; then key_set=(-api-key-set); fi
	if ! reason="$("${WORK}/bin/emit" check-model -base-url "${AGENTSUITE_MODEL_BASE_URL}" -model "${MODEL_ID}" \
		-path "${MODEL_PATH}" -auth-header "${MODEL_AUTH_HEADER}" ${key_set[@]+"${key_set[@]}"} 2>&1)"; then
		# Neither the URL nor the path is repeated here: check-model refuses one that could carry a
		# credential before anything prints it, and its reasons do not quote them either.
		refuse "the model settings (AGENTSUITE_MODEL_BASE_URL, AGENTSUITE_MODEL '${MODEL_ID}', AGENTSUITE_MODEL_PATH, AGENTSUITE_MODEL_AUTH_HEADER) would not start agentsessionsd: ${reason}"
	fi
else
	MODEL_BASE_URL="${FAKE_MODEL_URL}"
	MODEL_ID=triage-model
	MODEL_PATH=/chat/completions
	MODEL_AUTH_HEADER=Authorization
	HARNESS_MODEL=""
fi
# Render the model settings into the agentsessionsd manifest now, so a value that cannot be
# rendered stops the run here. The image, harness flag and Secret version are filled in later.
DAEMON_MANIFEST="$(<"${HERE}/deploy/agentsessionsd.yaml")"
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_ID/"${MODEL_ID}"}
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_BASE_URL/"${MODEL_BASE_URL}"}
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_PATH/"${MODEL_PATH}"}
DAEMON_MANIFEST=${DAEMON_MANIFEST//MODEL_AUTH_HEADER/"${MODEL_AUTH_HEADER}"}
if [[ "$(unfilled "${DAEMON_MANIFEST}")" != "DAEMON_NAME DAEMON_IMAGE SUBSTRATE_HARNESS_FLAG MODEL_SECRET_VERSION" ]]; then
	refuse "deploy/agentsessionsd.yaml: after the model settings it holds the placeholders '$(unfilled "${DAEMON_MANIFEST}")', want DAEMON_NAME, DAEMON_IMAGE, SUBSTRATE_HARNESS_FLAG and MODEL_SECRET_VERSION"
fi

# deploy/ and this script create the atespace ${ATESPACE}, and agentsessionsd's
# -substrate-atespace names it, so the binding the template is rendered from must name it too.
BINDING_ATESPACE="$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("atespace", ""))' "${HERE}/${BINDING_FILE}")"
if [[ "${BINDING_ATESPACE}" != "${ATESPACE}" ]]; then
	refuse "${BINDING_FILE} names atespace '${BINDING_ATESPACE}', but the demo deploys into atespace ${ATESPACE}"
fi

# The micro-VM class needs /dev/kvm in the containers Docker runs (the kind node is one). Probe it the
# way substrate's create-kind-cluster.sh does, which mounts it into the node only when the probe
# passes; without it, refuse instead of falling back to gVisor.
if [[ "${SANDBOX}" == microvm ]] && ! docker run --rm --device /dev/kvm busybox true >/dev/null 2>&1; then
	refuse "AGENTSUITE_DEMO_SANDBOX=microvm needs /dev/kvm in the Docker host, and 'docker run --device /dev/kvm' failed; set AGENTSUITE_DEMO_SANDBOX=gvisor on a host without KVM (an arm64 Mac)"
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
# An owned cluster with no setup record for its node is refused: it may be installed and serving,
# and recreating it would delete the daemon's emptyDir session journal.
if cluster_listed; then
	id="$(node_id)"
	if [[ -n "${id}" && "$(owned_id)" == "${id}" ]]; then
		stage="$(setup_stage "${id}")"
		case "${stage}" in
		creating) stage=incomplete ;;
		created | installed) ;;
		*)
			echo "refusing to reuse or recreate kind cluster ${CLUSTER}: up.sh created it, but ${SETUP} does not record how far its setup got." >&2
			echo "Recreating it would delete agentsessionsd's session journal if it is installed. Run ./down.sh to delete it, then rerun up.sh." >&2
			exit 1
			;;
		esac
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
	# The "creating" stage is what lets a rerun recreate it; "created" replaces it once the script
	# succeeds.
	record_owner() {
		local id
		id="$(node_id)"
		if [[ -n "${id}" ]]; then
			echo "${id}" >"${OWNED}"
			echo "${id} creating" >"${SETUP}"
		else
			rm -f "${OWNED}" "${SETUP}"
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
	step "ate-system (ate-setup deploy ate-system, the command install-ate-kind.sh runs)"
	# ate-setup is called directly, not through hack/install-ate-kind.sh: on Kind it builds the
	# control-plane images for linux/<its own GOARCH> unless --ko-default-platforms is given (the
	# wrapper's KO_DEFAULTPLATFORMS is ambient and ignored), so an arm64 Mac with an amd64 Docker VM
	# would push arm64 images to an amd64 node. The flags below are the wrapper's settings plus the
	# platform Docker reports; the GCP variables it unsets are unset here too.
	(cd "${SUBSTRATE}" && env -u GCE_REGION -u CLUSTER_LOCATION -u NETWORK -u SUBNETWORK \
		-u MEMORYSTORE_INSTANCE -u PROJECT_ID \
		go run ./cmd/ate-setup --kind --no-dev-env --kind-cluster-name="${CLUSTER}" \
		--context="kind-${CLUSTER}" --kubeconfig="${KUBECONFIG}" --ko-default-platforms="linux/${ARCH}" \
		--credential-provider='{"name":"k8s.io"}' --rollout-timeout=10m \
		deploy ate-system --setup-csi=none)
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

if [[ "${SANDBOX}" == microvm ]] && ! "${K[@]}" get sandboxconfig microvm >/dev/null 2>&1; then
	step "micro-VM sandbox class: kata assets and the microvm SandboxConfig (substrate's install-microvm-deps.sh)"
	# Run once per cluster: assets already assembled in the substrate checkout are reused. ARCH is
	# passed, since the script otherwise builds for its own GOARCH.
	(cd "${SUBSTRATE}" && export ARCH && KO_DEFAULTPLATFORMS="linux/${ARCH}" KUBECTL_CONTEXT="kind-${CLUSTER}" \
		ATE_INSTALL_KIND=true NO_DEV_ENV=true hack/install-microvm-deps.sh --install)
fi

step "${SANDBOX} WorkerPool ${POOL}"
"${K[@]}" apply -f "${HERE}/deploy/namespace.yaml"
ATEOM_IMAGE="$(cd "${SUBSTRATE}" && ko build --base-import-paths "./cmd/ateom-${SANDBOX}")"
sed -e "s#ATEOM_IMAGE#${ATEOM_IMAGE}#" "${HERE}/deploy/${WORKERPOOL_FILE}" | "${K[@]}" apply -f -
"${ROOT}/hack/wait-worker-pool.sh" "${POOL}" "${NS}" 600
if ${SUBSTRATE_ONLY}; then
	step "substrate is up (--substrate-only); run ./up.sh again for the agent and agentsessionsd"
	exit 0
fi

step "harness image for agent ${AGENT} (linux/${ARCH} composition)"
CTXDIR="${STATE}/harness-image"
rm -rf "${CTXDIR}" && mkdir -p "${CTXDIR}"
(cd "${HERE}" && go run ./cmd/emit bundle -suite "${SUITE_DIR}" -agent "${AGENT}" -platform "linux/${ARCH}" -out "${CTXDIR}/agent")
(cd "${HERE}" && CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${CTXDIR}/harness" ./cmd/harness)
docker build -q --platform "linux/${ARCH}" -f "${HERE}/Dockerfile.harness" -t "localhost:5001/agentsuite-harness:${AGENT}" "${CTXDIR}" >/dev/null
docker push -q "localhost:5001/agentsuite-harness:${AGENT}" >/dev/null
HARNESS_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "localhost:5001/agentsuite-harness:${AGENT}")"
echo "harness image: ${HARNESS_IMAGE}"

# Two tiers of the same agent and image: render/ is the stateless one (harness ${AGENT}, rebuilt from
# the journal), render-memory/ the memory one (harness ${AGENT}-memory, conversation in RAM).
# agentsessionsd serves one substrate harness, so each tier gets its own Deployment below.
for tier in stateless memory; do
	out="${STATE}/render"
	if [[ "${tier}" == memory ]]; then out="${STATE}/render-memory"; fi
	step "render the composition (${tier} tier): ActorTemplate + harness registration"
	(cd "${HERE}" && go run ./cmd/emit render -suite "${SUITE_DIR}" -agent "${AGENT}" -platform "linux/${ARCH}" \
		-binding "${BINDING_FILE}" -image "${HARNESS_IMAGE}" -model "${HARNESS_MODEL}" -tier "${tier}" -out "${out}")
	cat "${out}/harness-registration.json"
done

"${ATE[@]}" get atespace "${ATESPACE}" >/dev/null 2>&1 || "${ATE[@]}" create atespace "${ATESPACE}"
for out in "${STATE}/render" "${STATE}/render-memory"; do
	TEMPLATE="$(cat "${out}/template-name")"
	tier=stateless
	if [[ "${out}" == "${STATE}/render-memory" ]]; then tier=memory; fi
	if [[ "${SKIP_TEMPLATE}" == "${tier}" ]]; then
		step "ActorTemplate ${ATESPACE}/${TEMPLATE} (${tier} tier) rendered, not created: AGENTSUITE_DEMO_SKIP_TEMPLATE=${tier}"
		continue
	fi
	step "ActorTemplate ${ATESPACE}/${TEMPLATE}; wait for its golden snapshot"
	if ! "${ATE[@]}" get actor-template "${TEMPLATE}" -a "${ATESPACE}" >/dev/null 2>&1; then
		"${ATE[@]}" create actor-template -f "${out}/actortemplate.json"
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
done
TEMPLATE="$(cat "${STATE}/render/template-name")"
if ${TEMPLATES_ONLY}; then
	step "ActorTemplates ready (--templates-only); run ./up.sh again for agentsessionsd"
	exit 0
fi

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
# One manifest per tier, differing in the Deployment's name and its -substrate-harness flag.
MEMORY_MANIFEST=${DAEMON_MANIFEST//DAEMON_NAME/agentsessionsd-memory}
MEMORY_MANIFEST=${MEMORY_MANIFEST//SUBSTRATE_HARNESS_FLAG/"$(cat "${STATE}/render-memory/agentsessionsd.args")"}
DAEMON_MANIFEST=${DAEMON_MANIFEST//DAEMON_NAME/agentsessionsd}
DAEMON_MANIFEST=${DAEMON_MANIFEST//SUBSTRATE_HARNESS_FLAG/"$(cat "${STATE}/render/agentsessionsd.args")"}
for manifest in "${DAEMON_MANIFEST}" "${MEMORY_MANIFEST}"; do
	if [[ "$(unfilled "${manifest}")" != MODEL_SECRET_VERSION ]]; then
		echo "rendered agentsessionsd.yaml: unexpected placeholders left: $(unfilled "${manifest}"); the agentsessionsd-model Secret and the Deployments were not changed" >&2
		exit 1
	fi
done
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
MEMORY_MANIFEST=${MEMORY_MANIFEST//MODEL_SECRET_VERSION/"${MODEL_SECRET_VERSION}"}
printf '%s\n' "${DAEMON_MANIFEST}" >"${STATE}/agentsessionsd.yaml"
printf '%s\n' "${MEMORY_MANIFEST}" >"${STATE}/agentsessionsd-memory.yaml"
"${K[@]}" apply -f "${STATE}/agentsessionsd.yaml"
"${K[@]}" apply -f "${STATE}/agentsessionsd-memory.yaml"
if ${FAKE_MODEL}; then
	"${K[@]}" -n "${NS}" rollout status deploy/fakemodel --timeout=180s
fi
for d in agentsessionsd agentsessionsd-memory; do
	"${K[@]}" -n "${NS}" rollout status "deploy/${d}" --timeout=180s
	"${K[@]}" -n "${NS}" logs "deploy/${d}" | grep -E 'listening|substrate harness' || true
done

step "up; now run ./demo.sh"
