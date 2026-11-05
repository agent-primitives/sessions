#!/usr/bin/env bash
# Tests hack/substrate-lib.sh's ensure_atespace_editor and ate against a stub kubectl-ate, so it runs
# without a cluster. hack/provision-e2e-access.sh and the substrate-kind and agentsuite-substrate
# examples call them; their own tests cover only that wiring.
#
# Usage: hack/substrate-lib-test.sh
set -euo pipefail

lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/substrate-lib.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
stub="${work}/kubectl-ate"
member=user:daemon
remedy="${stub} get access-policy -a demo -o yaml > policy.yaml, add the member to an editor binding, then ${stub} update access-policy -a demo -f policy.yaml"
refusal="atespace demo has an access policy that does not grant ${member} editor; add it with: ${remedy}"

# The stub logs its arguments, keeps the atespace and its policy as files in the work directory, and
# misbehaves per STUB_SPACE (for get atespace, and race for create atespace) and STUB_POLICY (for
# the access policy). A created policy grants the members its manifest lists; on a race, the policy
# in STUB_WINNER wins it instead.
cat >"${stub}" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"${W}/calls"
rpc() {
	echo "rpc error: code = $1 desc = $2" >&2
	exit 1
}
case " $* " in
*" get atespace demo "*)
	case "${STUB_SPACE:-}" in
	transport) rpc Unavailable unavailable ;;
	ambiguous) rpc Unavailable 'nested rpc error: code = NotFound desc = absent' ;;
	auth) rpc PermissionDenied denied ;;
	null) echo null && exit ;;
	other) echo '{"metadata":{"name":"other"}}' && exit ;;
	stall) exec sleep 5 ;;
	esac
	[[ -e "${W}/atespace" ]] || rpc NotFound 'atespace "demo" not found'
	echo '{"metadata":{"name":"demo"}}'
	;;
*" create atespace demo "*)
	touch "${W}/atespace"
	[[ "${STUB_SPACE:-}" != race ]] || rpc AlreadyExists exists
	echo '{"metadata":{"name":"demo"}}'
	;;
*" get access-policy -a demo "*)
	case "${STUB_POLICY:-}" in
	transport) rpc Unavailable unavailable ;;
	garbage) echo 'not json' && exit ;;
	esac
	# A missing policy is a successful get with no output, as kubectl-ate reports it.
	if [[ -e "${W}/policy" ]]; then cat "${W}/policy"; else echo 'atespace "demo" has no access policy' >&2; fi
	;;
*" create access-policy -a demo -f - "*)
	cat >"${W}/manifest"
	case "${STUB_POLICY:-}" in
	race)
		printf '%s\n' "${STUB_WINNER}" >"${W}/policy"
		rpc AlreadyExists exists
		;;
	vanish) echo '{"bindings":[]}' && exit ;;
	esac
	sed -n 's/^  - //p' "${W}/manifest" | jq -R . | jq -sc '{bindings: [{role: "editor", members: .}]}' >"${W}/policy"
	cat "${W}/policy"
	;;
*) rpc Unimplemented "unexpected call: $*" ;;
esac
EOF
chmod +x "${stub}"

failures=0
cases=0

# reset [absent] empties the call log and the policy; the atespace exists unless absent is given.
reset() {
	rm -f "${work}/calls" "${work}/policy" "${work}/manifest"
	touch "${work}/calls" "${work}/atespace"
	[[ "${1:-}" != absent ]] || rm -f "${work}/atespace"
}

# run [VAR=value...] [--check-only] runs ensure_atespace_editor for member in atespace demo, as the
# callers do, and sets rc and out (stdout and stderr). DEADLINE_IN=<seconds> sets ATE_DEADLINE that
# far ahead.
run() {
	local vars=()
	while [[ "${1:-}" == *=* ]]; do
		vars+=("$1")
		shift
	done
	rc=0
	# shellcheck disable=SC2016 # the script expands its own variables
	out="$(env W="${work}" ${vars[@]+"${vars[@]}"} bash -c '
set -euo pipefail
source "$1"
shift
ATE=("${W}/kubectl-ate" --context kind-test)
die() { echo "$*" >&2; exit 1; }
progress() { echo "$*"; }
if [[ -n "${DEADLINE_IN:-}" ]]; then
	ATE_DEADLINE=$((SECONDS + DEADLINE_IN))
	TIMEOUT="$(command -v timeout || command -v gtimeout)"
fi
ensure_atespace_editor "$@"
echo "ATESPACE_ABSENT=${ATESPACE_ABSENT}"
' _ "${lib}" "$@" demo "${member}" 2>&1)" || rc=$?
}

fail() {
	echo "FAIL: $1"
	printf '%s\n' "${out}" | sed 's/^/    /'
	echo "    calls:"
	sed 's/^/    /' "${work}/calls"
	failures=$((failures + 1))
}

# expect_ok <desc> <want output substring>
expect_ok() {
	cases=$((cases + 1))
	if ((rc != 0)) || [[ "${out}" != *"$2"* ]]; then fail "$1: want success with '$2', got rc=${rc}"; fi
}

# expect_refused <desc> <want output substring>: exit 1, and nothing created or updated.
expect_refused() {
	cases=$((cases + 1))
	if ((rc != 1)) || [[ "${out}" != *"$2"* ]]; then
		fail "$1: want exit 1 with '$2', got rc=${rc}"
	elif has_call ' (create|update) '; then
		fail "$1: a refusal wrote to substrate"
	fi
}

has_call() { grep -Eq -- "$1" "${work}/calls"; }
count_calls() { grep -c -- "$1" "${work}/calls" || true; }

grant() { printf '{"bindings":[{"role":"%s","members":["%s"]}]}' "$1" "$2"; }

# A missing atespace and policy are created once, with a policy that grants the member alone; the
# rerun creates nothing.
reset absent
run
expect_ok "absent atespace and policy" "Granting ${member} the editor role in atespace demo"
want_manifest="$(printf 'bindings:\n- role: editor\n  members:\n  - %s' "${member}")"
cases=$((cases + 1))
[[ "$(cat "${work}/manifest" 2>/dev/null)" == "${want_manifest}" ]] || fail "created policy manifest: $(cat "${work}/manifest" 2>/dev/null)"
cases=$((cases + 1))
[[ "$(head -1 "${work}/calls")" == "--context kind-test -o json get atespace demo" ]] || fail "ate puts -o json after ATE"
run
expect_ok "rerun" "ATESPACE_ABSENT=false"
cases=$((cases + 1))
[[ "$(count_calls 'create atespace')$(count_calls 'create access-policy')" == 11 ]] || fail "rerun created again"

# A create that loses a race to another writer reads back what won.
reset absent
run STUB_SPACE=race
expect_ok "atespace create race" "ATESPACE_ABSENT=false"
reset
run STUB_POLICY=race STUB_WINNER="$(grant owner "${member}")"
expect_ok "policy create race won by a grant" "ATESPACE_ABSENT=false"
reset
run STUB_POLICY=race STUB_WINNER="$(grant editor user:other)"
cases=$((cases + 1))
if ((rc != 1)) || [[ "${out}" != *"${refusal}"* ]] || has_call ' update '; then
	fail "policy create race won by another member: want exit 1 with the refusal, and no update"
fi

# An existing grant, editor or owner, is kept as it is, with and without --check-only.
for policy in \
	'{"bindings":[{"role":"viewer","members":["user:someone"]},{"role":"editor","members":["user:other","user:daemon"]}]}' \
	"$(grant owner "${member}")"; do
	for mode in --check-only ""; do
		reset
		printf '%s' "${policy}" >"${work}/policy"
		run ${mode:+"${mode}"}
		expect_ok "existing grant ${policy} ${mode}" "ATESPACE_ABSENT=false"
		cases=$((cases + 1))
		if has_call ' (create|update) ' || [[ "$(cat "${work}/policy")" != "${policy}" ]]; then
			fail "existing grant ${policy} ${mode}: policy rewritten"
		fi
	done
done

# A policy that does not grant the member editor is refused, never rewritten.
for policy in "$(grant editor user:other)" "$(grant viewer "${member}")" '{"metadata":{"name":"default"}}'; do
	for mode in --check-only ""; do
		reset
		printf '%s' "${policy}" >"${work}/policy"
		run ${mode:+"${mode}"}
		expect_refused "policy ${policy} ${mode}" "${refusal}"
	done
done

# A failed or malformed read stops before any create.
reset
run STUB_POLICY=transport
expect_refused "policy read transport error" "kubectl-ate get access-policy -a demo failed"
reset
run STUB_POLICY=transport ATE_FAILURE="kubectl-ate failed; provisioning refused."
expect_refused "policy read transport error, caller's message" "kubectl-ate failed; provisioning refused."
reset
run STUB_POLICY=garbage
expect_refused "policy read that is not JSON" "kubectl-ate returned an ambiguous resource."
reset
run STUB_POLICY=vanish
cases=$((cases + 1))
if ((rc != 1)) || [[ "${out}" != *"The access policy of atespace demo disappeared after creation."* ]]; then
	fail "policy gone after creation: want exit 1"
fi
for space in transport ambiguous auth null other; do
	reset absent
	run STUB_SPACE="${space}"
	case "${space}" in
	null) want="kubectl-ate returned an ambiguous resource." ;;
	other) want="kubectl-ate returned a different atespace." ;;
	*) want="kubectl-ate get atespace demo failed" ;;
	esac
	expect_refused "atespace read ${space}" "${want}"
done

# --check-only creates nothing: it stops at an absent atespace, and reports an absent policy.
reset absent
run --check-only
expect_ok "check-only, absent atespace" "ATESPACE_ABSENT=true"
cases=$((cases + 1))
if has_call 'create |access-policy'; then fail "check-only, absent atespace: read or wrote past the atespace"; fi
reset
run --check-only
expect_ok "check-only, absent policy" "Access policy is absent; provisioning will grant ${member} editor"
cases=$((cases + 1))
if has_call 'create '; then fail "check-only, absent policy: created a resource"; fi

# ATE_DEADLINE bounds each call, and one already past makes no call.
reset
start="${SECONDS}"
run STUB_SPACE=stall DEADLINE_IN=1
cases=$((cases + 1))
if ((rc == 0 || SECONDS - start > 3)); then fail "stalled call: want failure by the deadline, got rc=${rc} after $((SECONDS - start))s"; fi
reset
run DEADLINE_IN=0
expect_refused "deadline already past" "Template provisioning deadline exceeded."
cases=$((cases + 1))
[[ ! -s "${work}/calls" ]] || fail "deadline already past: called kubectl-ate"

# substrate_pin reads the workflow's pins after the caller, which sourced it by a relative path, has
# changed directory, and prints nothing for a key the workflow does not set.
workflow="$(dirname "${lib}")/../.github/workflows/substrate-e2e.yml"
for key in SUBSTRATE_VERSION SUBSTRATE_REF NOT_A_PIN; do
	cases=$((cases + 1))
	# shellcheck disable=SC2016 # the script expands its own variables
	got="$(cd "$(dirname "${lib}")/.." && bash -c 'source hack/substrate-lib.sh && cd / && substrate_pin "$1"' _ "${key}")" ||
		got="failed: $?"
	if [[ "${key}" == NOT_A_PIN ]]; then
		[[ -z "${got}" ]] || fail "substrate_pin ${key}: want nothing, got '${got}'"
	elif [[ -z "${got}" ]] || ! grep -qxF "  ${key}: ${got}" "${workflow}"; then
		fail "substrate_pin ${key}: got '${got}', which the workflow does not pin"
	fi
done

# kind_ate_setup runs the checkout's ate-setup with the release's images, with no GCP variable set,
# and passes its failure on.
mkdir -p "${work}/go-bin" "${work}/checkout"
cat >"${work}/go-bin/go" <<'EOF'
#!/usr/bin/env bash
{
	pwd
	env | grep -E '^(GCE_REGION|CLUSTER_LOCATION|NETWORK|SUBNETWORK|MEMORYSTORE_INSTANCE|PROJECT_ID)=' || true
	printf '%s\n' "$@"
} >"${W}/go-call"
exit "${GO_RC:-0}"
EOF
chmod +x "${work}/go-bin/go"
# setup <GO_RC> runs kind_ate_setup with one deploy argument holding a space, and sets rc.
setup() {
	rc=0
	# shellcheck disable=SC2016 # the script expands its own variables
	W="${work}" GO_RC="$1" PATH="${work}/go-bin:${PATH}" GCE_REGION=r CLUSTER_LOCATION=l NETWORK=n \
		SUBNETWORK=s MEMORYSTORE_INSTANCE=m PROJECT_ID=p bash -c '
set -euo pipefail
source "$1"
kind_ate_setup "$2" demo /k/config v1.2.3 deploy "two words"
' _ "${lib}" "${work}/checkout" || rc=$?
}
setup 0
want="$(printf '%s\n' "${work}/checkout" run ./cmd/ate-setup --kind --no-dev-env \
	--kind-cluster-name=demo --context=kind-demo --kubeconfig=/k/config \
	--image-repo=ghcr.io/agent-substrate/substrate --image-tag=v1.2.3 \
	'--credential-provider={"name":"k8s.io"}' deploy "two words")"
cases=$((cases + 1))
got="$(cat "${work}/go-call")"
if ((rc != 0)) || [[ "${got}" != "${want}" ]]; then
	out="$(diff <(echo "${want}") <(echo "${got}") || true)"
	fail "kind_ate_setup: want ate-setup run from the checkout with the release images and no GCP variable, got rc=${rc}"
fi
setup 3
cases=$((cases + 1))
((rc == 3)) || fail "kind_ate_setup: want ate-setup's exit 3 passed on, got rc=${rc}"

if ((failures > 0)); then
	echo "${failures} of ${cases} cases failed"
	exit 1
fi
echo "ok: ${cases} cases"
