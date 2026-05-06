package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	finalizerName     = "capi-argocd-registrar.drydock-dev.github.com/finalizer"
	argoCDSecretType  = "cluster"
	argoCDManagerSA   = "argocd-manager"
	argoCDManagerCRB  = "argocd-manager-role-binding"
	tokenSecretSuffix = "-argocd-manager-token"
	requeueAfter      = 15 * time.Second
	reconcileInterval = 2 * time.Minute

	// Standard labels applied to every ArgoCD cluster secret managed by this operator.
	// These are applied last and take precedence over cluster and extra labels.
	labelManagedBy  = "app.kubernetes.io/managed-by"
	labelComponent  = "app.kubernetes.io/component"
	operatorName    = "capi-argocd-registrar"
	componentSecret = "argocd-cluster-secret"
)

// argoCDClusterConfig is the JSON structure for the ArgoCD cluster secret config field.
type argoCDClusterConfig struct {
	TLSClientConfig tlsClientConfig `json:"tlsClientConfig"`
	BearerToken     string          `json:"bearerToken"`
}

type tlsClientConfig struct {
	Insecure bool   `json:"insecure"`
	CAData   []byte `json:"caData,omitempty"`
}

// ClusterReconciler watches CAPI Cluster objects and manages ArgoCD cluster secrets.
type ClusterReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	ArgoCDNamespace      string
	ExtraSecretLabels    map[string]string
	IgnoredLabelPrefixes []string
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Deletion path ---
	if !cluster.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(cluster, finalizerName) {
			logger.Info("cluster is being deleted, removing ArgoCD secret")
			if err := r.deleteArgoCDSecret(ctx, cluster); err != nil {
				r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "DeleteFailed",
					"Failed to delete ArgoCD cluster secret: %v", err)
				return ctrl.Result{}, err
			}
			r.Recorder.Event(cluster, corev1.EventTypeNormal, "Deregistered",
				"ArgoCD cluster secret deleted")
			patch := client.MergeFrom(cluster.DeepCopy())
			controllerutil.RemoveFinalizer(cluster, finalizerName)
			if err := r.Patch(ctx, cluster, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// --- Not yet provisioned ---
	if cluster.Status.Phase != string(clusterv1.ClusterPhaseProvisioned) {
		logger.Info("cluster not yet provisioned, waiting", "phase", cluster.Status.Phase)
		return ctrl.Result{}, nil
	}

	// --- Ensure finalizer ---
	if !controllerutil.ContainsFinalizer(cluster, finalizerName) {
		patch := client.MergeFrom(cluster.DeepCopy())
		controllerutil.AddFinalizer(cluster, finalizerName)
		if err := r.Patch(ctx, cluster, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- Get CAPI kubeconfig secret ---
	kubeconfigSecret := &corev1.Secret{}
	kubeconfigKey := types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      fmt.Sprintf("%s-kubeconfig", cluster.Name),
	}
	if err := r.Get(ctx, kubeconfigKey, kubeconfigSecret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("kubeconfig secret not yet available, requeueing")
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
		return ctrl.Result{}, err
	}

	kubeconfigBytes, ok := kubeconfigSecret.Data["value"]
	if !ok {
		logger.Info("kubeconfig secret missing 'value' key, requeueing")
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// --- Build rest.Config for the workload cluster ---
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building rest config from kubeconfig: %w", err)
	}

	workloadClient, err := client.New(restConfig, client.Options{Scheme: r.Scheme})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating workload cluster client: %w", err)
	}

	// --- Bootstrap argocd-manager on the workload cluster ---
	token, err := r.ensureArgoCDManager(ctx, workloadClient, cluster.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring argocd-manager: %w", err)
	}
	if token == "" {
		// Token secret exists but hasn't been populated yet by the token controller.
		logger.Info("waiting for service account token to be populated")
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// --- Build ArgoCD cluster config ---
	cfg := argoCDClusterConfig{
		BearerToken: token,
		TLSClientConfig: tlsClientConfig{
			Insecure: false,
			CAData:   restConfig.CAData,
		},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshaling argocd cluster config: %w", err)
	}

	// --- Create/update ArgoCD cluster secret on management cluster ---
	result, err := r.upsertArgoCDSecret(ctx, cluster, restConfig.Host, cfgJSON)
	if err != nil {
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "UpsertFailed",
			"Failed to upsert ArgoCD cluster secret: %v", err)
		return ctrl.Result{}, err
	}
	if result == controllerutil.OperationResultCreated {
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "Registered",
			"ArgoCD cluster secret created")
	}

	logger.Info("ArgoCD cluster secret reconciled", "cluster", cluster.Name)
	return ctrl.Result{RequeueAfter: reconcileInterval}, nil
}

// ensureArgoCDManager creates the argocd-manager ServiceAccount, ClusterRoleBinding,
// and long-lived token Secret on the workload cluster. Returns the token value when ready.
func (r *ClusterReconciler) ensureArgoCDManager(ctx context.Context, wc client.Client, clusterName string) (string, error) {
	// ServiceAccount
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      argoCDManagerSA,
			Namespace: "kube-system",
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, wc, sa, func() error { return nil }); err != nil {
		return "", fmt.Errorf("upserting argocd-manager ServiceAccount: %w", err)
	}

	// ClusterRole (cluster-admin)
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: argoCDManagerCRB},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, wc, crb, func() error {
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		}
		crb.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      argoCDManagerSA,
			Namespace: "kube-system",
		}}
		return nil
	}); err != nil {
		return "", fmt.Errorf("upserting argocd-manager ClusterRoleBinding: %w", err)
	}

	// Long-lived token Secret
	tokenSecretName := clusterName + tokenSecretSuffix
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenSecretName,
			Namespace: "kube-system",
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: argoCDManagerSA,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, wc, tokenSecret, func() error {
		if tokenSecret.Annotations == nil {
			tokenSecret.Annotations = map[string]string{}
		}
		tokenSecret.Annotations[corev1.ServiceAccountNameKey] = argoCDManagerSA
		tokenSecret.Type = corev1.SecretTypeServiceAccountToken
		return nil
	}); err != nil {
		return "", fmt.Errorf("upserting argocd-manager token Secret: %w", err)
	}

	// Re-fetch to get the populated token
	fetched := &corev1.Secret{}
	if err := wc.Get(ctx, types.NamespacedName{Name: tokenSecretName, Namespace: "kube-system"}, fetched); err != nil {
		return "", err
	}
	return string(fetched.Data["token"]), nil
}

// upsertArgoCDSecret creates or updates the ArgoCD cluster secret on the management cluster.
func (r *ClusterReconciler) upsertArgoCDSecret(ctx context.Context, cluster *clusterv1.Cluster, server string, cfgJSON []byte) (controllerutil.OperationResult, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: r.ArgoCDNamespace,
		},
	}
	return controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		// 1. Cluster labels, filtered by ignored prefixes.
		for k, v := range cluster.Labels {
			if !r.isIgnoredLabel(k) {
				secret.Labels[k] = v
			}
		}
		// 2. Extra labels from Helm config, override cluster labels.
		for k, v := range r.ExtraSecretLabels {
			secret.Labels[k] = v
		}
		// 3. Standard operator labels, always win.
		secret.Labels[labelManagedBy] = operatorName
		secret.Labels[labelComponent] = componentSecret
		// 4. ArgoCD required label, always win.
		secret.Labels["argocd.argoproj.io/secret-type"] = argoCDSecretType
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			"name":   []byte(cluster.Name),
			"server": []byte(server),
			"config": cfgJSON,
		}
		return nil
	})
}

// deleteArgoCDSecret removes the ArgoCD cluster secret for the given cluster.
func (r *ClusterReconciler) deleteArgoCDSecret(ctx context.Context, cluster *clusterv1.Cluster) error {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: cluster.Name, Namespace: r.ArgoCDNamespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.Delete(ctx, secret)
}

func (r *ClusterReconciler) isIgnoredLabel(key string) bool {
	for _, prefix := range r.IgnoredLabelPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("capi-argocd-registrar")
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(clusterChangedPredicate{})).
		Complete(r)
}

// clusterChangedPredicate enqueues only when something meaningful changed:
// the deletion timestamp was set, the phase changed, or labels changed.
// Filters out the high-frequency CAPI status-subresource writes that don't
// affect what the controller does.
type clusterChangedPredicate struct {
	predicate.Funcs
}

func (clusterChangedPredicate) Update(e event.UpdateEvent) bool {
	oldCluster, ok1 := e.ObjectOld.(*clusterv1.Cluster)
	newCluster, ok2 := e.ObjectNew.(*clusterv1.Cluster)
	if !ok1 || !ok2 {
		return true
	}
	// Deletion started.
	if newCluster.DeletionTimestamp != nil && oldCluster.DeletionTimestamp == nil {
		return true
	}
	// Phase transition (e.g. Provisioning → Provisioned).
	if oldCluster.Status.Phase != newCluster.Status.Phase {
		return true
	}
	// Label change affects what gets copied to the ArgoCD secret.
	if !mapsEqual(oldCluster.Labels, newCluster.Labels) {
		return true
	}
	return false
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
