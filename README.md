# capi-argocd-registrar

Automatically registers [Cluster API](https://cluster-api.sigs.k8s.io/) provisioned clusters into [ArgoCD](https://argo-cd.readthedocs.io/) as cluster secrets.

## How it works

The controller watches `cluster.cluster.x-k8s.io` objects across all namespaces. When a cluster transitions to `Provisioned`:

1. Reads the `{clusterName}-kubeconfig` secret created by CAPI (provider-agnostic)
2. Creates an `argocd-manager` ServiceAccount and ClusterRoleBinding on the workload cluster
3. Provisions a long-lived service account token
4. Creates an ArgoCD cluster secret in the ArgoCD namespace with:
   - All CAPI Cluster labels copied (excluding any `ignoredLabelPrefixes`)
   - Bearer token authentication for ArgoCD -> workload cluster connectivity

When the cluster is deleted, the controller removes the ArgoCD cluster secret before releasing the finalizer.

## Why

ArgoCD ApplicationSet cluster generators work by discovering cluster secrets. By automatically creating and labelling those secrets from CAPI Cluster objects, fleet ApplicationSets (cert-manager, metrics-server, platform add-ons, etc.) begin deploying to a new cluster the moment it is provisioned - no manual `argocd cluster add` step.

## Provider support

Any CAPI infrastructure provider that creates a `{clusterName}-kubeconfig` secret (CAPA, CAPZ, CAPG, CAPV, etc.) is supported. No provider-specific code exists in this controller.

## Install

```bash
helm install capi-argocd-registrar charts/capi-argocd-registrar \
  --namespace capi-argocd-registrar \
  --create-namespace \
  --set argocdNamespace=argocd
```

## Configuration

| Parameter | Default | Description |
|---|---|---|
| `replicaCount` | `1` | Number of controller replicas. |
| `leaderElect` | `true` | Enable leader election. Required for HA (`replicaCount > 1`). |
| `pdb.create` | `false` | If true, create a PodDisruptionBudget. Automatically set to true if `replicaCount > 1`. |
| `argocdNamespace` | `argocd` | Namespace where ArgoCD is installed. |
| `extraSecretLabels` | `{}` | Additional labels applied to every ArgoCD cluster secret. |
| `ignoredLabelPrefixes` | `[]` | Label key prefixes to exclude when copying CAPI Cluster labels. |
| `image.repository` | `ghcr.io/drydock-dev/capi-argocd-registrar` | Controller image. |
| `image.tag` | `latest` | Image tag. |

## Monitoring

The controller exposes Prometheus metrics on port `8080` at the `/metrics` endpoint. These include standard `controller-runtime` metrics for reconciliations, errors, and queue lengths, as well as Go process metrics.

## Development

### Prerequisites

- Go (1.21+)
- Docker
- Task
- Helm
- kubectl
- clusterctl
- golangci-lint

The `task install-deps` command can install most of these tools on macOS via Homebrew. Contributions for a Linux/Windows equivalent are welcome.

### Common Tasks

```sh
task test      # Run unit tests
task lint      # Run golangci-lint
task e2e-full  # Run the full end-to-end test suite
task build     # Compile the controller
```
