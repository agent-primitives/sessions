#!/usr/bin/env bash
# Deletes the demo's kind cluster. The kind-registry container is shared with other substrate kind
# clusters, so it stays.
set -euo pipefail
source "$(dirname "$0")/common.sh"
kind delete cluster --name "${KIND_CLUSTER_NAME}"
