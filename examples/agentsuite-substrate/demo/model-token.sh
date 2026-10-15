# shellcheck shell=bash
# Sourced by coordinator.sh (and scripts_test.go). Not executable on its own.

# model_secret NAMESPACE CLIENT_ID TOKEN_FILE KUBECTL...: fetches a token for the user-assigned
# managed identity CLIENT_ID from the instance metadata endpoint and stores it as key api-key of
# Secret agentsessionsd-model in NAMESPACE, through the kubectl command KUBECTL... The token is
# written to TOKEN_FILE (mode 0600; the caller removes it) without a trailing newline: kubectl
# stores the file verbatim, agentsessionsd sends MODEL_API_KEY as given, and Go's net/http refuses
# an Authorization header that ends in a newline. The token is never printed.
model_secret() {
	local ns="$1" client="$2" file="$3" field="access""_token"
	shift 3
	(
		umask 077
		curl -fsS -H Metadata:true "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://cognitiveservices.azure.com&client_id=${client}" |
			python3 -c 'import json, sys; sys.stdout.write(json.load(sys.stdin)[sys.argv[1]].strip())' "${field}" >"${file}"
	) || {
		echo "model_secret: no token from the instance metadata endpoint" >&2
		return 1
	}
	if [[ ! -s "${file}" ]]; then
		echo "model_secret: the instance metadata endpoint returned an empty token" >&2
		return 1
	fi
	echo "token: $(wc -c <"${file}" | tr -d ' ') bytes"
	"$@" -n "${ns}" create secret generic agentsessionsd-model --from-file=api-key="${file}" --dry-run=client -o yaml |
		"$@" -n "${ns}" apply --server-side --force-conflicts --field-manager=agentsuite-demo -f - >/dev/null
}
