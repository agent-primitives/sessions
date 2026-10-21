# shellcheck shell=bash disable=SC2034

# Shared provider settings for setup and deployment. Local Ollama is the default;
# the deterministic stub and operator-managed external endpoint are explicit choices.
MODEL_SOURCE="${MODEL_SOURCE:-ollama}"
case "${MODEL_SOURCE}" in
ollama)
	MODEL_NAME="${MODEL_NAME:-gemma3:270m}"
	MODEL_BASE_URL="http://ollama.${NS}.svc:11434/v1"
	;;
stub)
	MODEL_NAME="${MODEL_NAME:-smoke-model}"
	MODEL_BASE_URL="http://modelstub.${NS}.svc:8080/v1"
	;;
external)
	: "${MODEL_NAME:?MODEL_NAME is required for an external provider}"
	: "${MODEL_BASE_URL:?MODEL_BASE_URL is required for an external provider}"
	;;
*) die "MODEL_SOURCE must be ollama, external, or stub." ;;
esac

# The daemon uses these OpenAI-compatible route settings for host-mediated model calls.
# MODEL_API_KEY stays in the caller's environment until deploy.sh writes the Secret.
MODEL_PATH="${MODEL_PATH:-/chat/completions}"
MODEL_AUTH_HEADER="${MODEL_AUTH_HEADER:-Authorization}"
export MODEL_SOURCE MODEL_NAME MODEL_BASE_URL MODEL_PATH MODEL_AUTH_HEADER
