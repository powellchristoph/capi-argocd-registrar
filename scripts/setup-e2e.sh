#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-capi-e2e}"
KIND_CONFIG="${REPO_ROOT}/test/e2e/kind-config.yaml"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/drydock-dev/capi-argocd-registrar}"
IMAGE_TAG="${IMAGE_TAG:-e2e-test}"
CAPI_VERSION="v1.9.0"
CERT_MANAGER_VERSION="v1.16.2"
ARGOCD_VERSION="v2.13.3"

echo "==> [1/7] Creating Kind management cluster '${KIND_CLUSTER_NAME}'"
if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
  kind create cluster \
    --name "${KIND_CLUSTER_NAME}" \
    --config "${KIND_CONFIG}"
else
  echo "    Cluster '${KIND_CLUSTER_NAME}' already exists, skipping"
fi

KUBECONFIG="$(mktemp /tmp/kubeconfig-${KIND_CLUSTER_NAME}.XXXXXX)"
kind get kubeconfig --name "${KIND_CLUSTER_NAME}" > "${KUBECONFIG}"
export KUBECONFIG

echo "==> [2/7] Installing cert-manager ${CERT_MANAGER_VERSION} (CAPI dependency)"
kubectl apply -f \
  "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
kubectl wait --for=condition=Available deployment/cert-manager \
  -n cert-manager --timeout=120s
kubectl wait --for=condition=Available deployment/cert-manager-webhook \
  -n cert-manager --timeout=120s
kubectl wait --for=condition=Available deployment/cert-manager-cainjector \
  -n cert-manager --timeout=120s

echo "==> [3/7] Installing CAPI ${CAPI_VERSION} + CAPD via clusterctl"
# EXP_CLUSTER_RESOURCE_SET enables ClusterResourceSets, which CAPD uses to
# install kindnet CNI on workload clusters. Without it, cluster nodes never
# become Ready and the cluster never reaches Provisioned phase.
export EXP_CLUSTER_RESOURCE_SET=true
clusterctl init \
  --core "cluster-api:${CAPI_VERSION}" \
  --bootstrap "kubeadm:${CAPI_VERSION}" \
  --control-plane "kubeadm:${CAPI_VERSION}" \
  --infrastructure "docker:${CAPI_VERSION}" \
  --wait-providers

echo "==> [4/7] Installing ArgoCD ${ARGOCD_VERSION}"
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n argocd -f \
  "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"
kubectl wait --for=condition=Available deployment/argocd-server \
  -n argocd --timeout=300s

echo "==> [5/7] Building controller image ${IMAGE_REPO}:${IMAGE_TAG}"
(cd "${REPO_ROOT}" && \
  docker build -t "${IMAGE_REPO}:${IMAGE_TAG}" .)

echo "==> [6/7] Loading image into Kind cluster '${KIND_CLUSTER_NAME}'"
kind load docker-image \
  "${IMAGE_REPO}:${IMAGE_TAG}" \
  --name "${KIND_CLUSTER_NAME}"

echo "==> [7/7] Deploying controller via Helm"
helm upgrade --install capi-argocd-registrar \
  "${REPO_ROOT}/charts/capi-argocd-registrar" \
  --namespace capi-argocd-registrar \
  --create-namespace \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${IMAGE_TAG}" \
  --set "image.pullPolicy=Never" \
  --set "argocdNamespace=argocd" \
  --set "leaderElect=false" \
  --set "ignoredLabelPrefixes[0]=helm.sh" \
  --set "ignoredLabelPrefixes[1]=app.kubernetes.io" \
  --wait \
  --timeout=120s

echo ""
echo "E2E environment ready."
echo ""
echo "To run the full suite (including teardown):"
echo "  task e2e-full"
echo ""
echo "To run tests against this environment manually:"
echo "  task e2e"
echo "And to tear down when you are finished:"
echo "  task e2e-teardown"
