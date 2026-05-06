#!/usr/bin/env bash
set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-capi-e2e}"
WORKLOAD_CLUSTER="${WORKLOAD_CLUSTER:-e2e-workload}"

echo "==> Deleting Kind cluster '${KIND_CLUSTER_NAME}'"
kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true

echo "==> Removing orphaned CAPD workload cluster containers"
docker ps -a --filter "name=${WORKLOAD_CLUSTER}" -q | xargs -r docker rm -f 2>/dev/null || true
