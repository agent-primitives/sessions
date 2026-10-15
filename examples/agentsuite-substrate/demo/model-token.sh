# shellcheck shell=bash
# Sourced by coordinator.sh (and scripts_test.go). Not executable on its own.

# model_secret NAMESPACE CLIENT_ID KUBECTL...: fetches a token for the user-assigned managed identity
# CLIENT_ID from the instance metadata endpoint and stores it as key api-key of Secret
# agentsessionsd-model in NAMESPACE, through the kubectl command KUBECTL... The token never touches a
# file on the host: it is held in a shell variable and piped to kubectl as --from-file=/dev/stdin.
# An empty or missing token stops before kubectl runs. The token is stored without a trailing
# newline (agentsessionsd sends MODEL_API_KEY as given, and Go's net/http refuses an Authorization
# header that ends in a newline) and never printed; only its size is.
model_secret() {
	local ns="$1" client="$2" field="access""_token" token
	shift 2
	if ! token="$(
		set -o pipefail
		curl -fsS -H Metadata:true "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://cognitiveservices.azure.com&client_id=${client}" |
			python3 -c 'import json, sys; sys.stdout.write(json.load(sys.stdin)[sys.argv[1]].strip())' "${field}"
	)"; then
		echo "model_secret: no token from the instance metadata endpoint" >&2
		return 1
	fi
	if [[ -z "${token}" ]]; then
		echo "model_secret: the instance metadata endpoint returned an empty token" >&2
		return 1
	fi
	echo "token: ${#token} bytes"
	# printf is a bash builtin, so the token is not in any process's arguments.
	(
		set -o pipefail
		printf '%s' "${token}" |
			"$@" -n "${ns}" create secret generic agentsessionsd-model --from-file=api-key=/dev/stdin --dry-run=client -o yaml |
			"$@" -n "${ns}" apply --server-side --force-conflicts --field-manager=agentsuite-demo -f - >/dev/null
	)
}
