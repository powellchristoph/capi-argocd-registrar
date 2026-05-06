package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(clusterv1.AddToScheme(s))
	return s
}

func newReconciler(objs ...client.Object) (*ClusterReconciler, client.Client) {
	s := testScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &ClusterReconciler{
		Client:          c,
		Scheme:          s,
		Recorder:        record.NewFakeRecorder(10),
		ArgoCDNamespace: "argocd",
	}, c
}

func req(name, ns string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
}

// --- Reconcile path tests ---

func TestReconcile_ClusterNotFound(t *testing.T) {
	r, _ := newReconciler()
	result, err := r.Reconcile(context.Background(), req("missing", "default"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %v", result)
	}
}

func TestReconcile_ClusterNotProvisioned(t *testing.T) {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Status:     clusterv1.ClusterStatus{Phase: "Provisioning"},
	}
	r, c := newReconciler(cluster)

	result, err := r.Reconcile(context.Background(), req("test", "default"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %v", result)
	}

	// No ArgoCD secret should be created for a non-provisioned cluster.
	secret := &corev1.Secret{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "argocd"}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no ArgoCD secret, got err=%v", err)
	}
}

func TestReconcile_ClusterDeleting_NoFinalizer(t *testing.T) {
	now := metav1.Now()
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			Namespace:         "default",
			DeletionTimestamp: &now,
			// A foreign finalizer keeps the object alive; our finalizer is absent.
			Finalizers: []string{"other-controller/finalizer"},
		},
	}
	r, _ := newReconciler(cluster)

	result, err := r.Reconcile(context.Background(), req("test", "default"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %v", result)
	}
}

func TestReconcile_ClusterDeleting_WithFinalizer(t *testing.T) {
	now := metav1.Now()
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
	}
	argoCDSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "argocd"},
	}
	r, c := newReconciler(cluster, argoCDSecret)

	result, err := r.Reconcile(context.Background(), req("test", "default"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %v", result)
	}

	// ArgoCD secret must be deleted.
	secret := &corev1.Secret{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "argocd"}, secret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected ArgoCD secret to be deleted, got err=%v", err)
	}

	// The fake client garbage-collects the object when the last finalizer is
	// removed from a deleting object - NotFound is the expected outcome.
	updated := &clusterv1.Cluster{}
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, updated)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error getting cluster after finalizer removal: %v", err)
	}
	if err == nil {
		for _, f := range updated.Finalizers {
			if f == finalizerName {
				t.Fatal("expected finalizer to be removed, but it is still present")
			}
		}
	}
}

func TestReconcile_ClusterDeleting_ArgoCDSecretAlreadyGone(t *testing.T) {
	// Controller should not error when the ArgoCD secret is already deleted.
	now := metav1.Now()
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{finalizerName},
		},
	}
	r, _ := newReconciler(cluster)

	_, err := r.Reconcile(context.Background(), req("test", "default"))
	if err != nil {
		t.Fatalf("expected no error when ArgoCD secret is already gone, got %v", err)
	}
}

func TestReconcile_KubeconfigSecretMissing(t *testing.T) {
	// A provisioned cluster with no kubeconfig secret should requeue.
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Status:     clusterv1.ClusterStatus{Phase: string(clusterv1.ClusterPhaseProvisioned)},
	}
	r, _ := newReconciler(cluster)

	result, err := r.Reconcile(context.Background(), req("test", "default"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.RequeueAfter != requeueAfter {
		t.Fatalf("expected RequeueAfter=%v, got %v", requeueAfter, result.RequeueAfter)
	}
}

func TestReconcile_KubeconfigMissingValueKey(t *testing.T) {
	// A kubeconfig secret that lacks the 'value' key should requeue.
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Status: clusterv1.ClusterStatus{Phase: string(clusterv1.ClusterPhaseProvisioned)},
	}
	kubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-kubeconfig", Namespace: "default"},
		Data:       map[string][]byte{"wrong-key": []byte("data")},
	}
	r, _ := newReconciler(cluster, kubeconfigSecret)

	result, err := r.Reconcile(context.Background(), req("test", "default"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.RequeueAfter != requeueAfter {
		t.Fatalf("expected RequeueAfter=%v, got %v", requeueAfter, result.RequeueAfter)
	}
}

// --- upsertArgoCDSecret tests ---

func TestUpsertArgoCDSecret_CreatesSecret(t *testing.T) {
	r, c := newReconciler()
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster",
			Namespace: "default",
			Labels: map[string]string{
				"platform.io/env":    "prod",
				"platform.io/region": "us-east",
				"other/label":        "other-value",
			},
		},
	}
	cfgJSON := []byte(`{"bearerToken":"tok","tlsClientConfig":{"insecure":false}}`)

	if _, err := r.upsertArgoCDSecret(ctx, cluster, "https://1.2.3.4:6443", cfgJSON); err != nil {
		t.Fatalf("upsertArgoCDSecret: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, secret); err != nil {
		t.Fatalf("getting ArgoCD secret: %v", err)
	}

	if got := secret.Labels["argocd.argoproj.io/secret-type"]; got != "cluster" {
		t.Errorf("argocd secret-type label: got %q, want %q", got, "cluster")
	}
	if got := secret.Labels["platform.io/env"]; got != "prod" {
		t.Errorf("platform.io/env label: got %q, want %q", got, "prod")
	}
	if got := secret.Labels["platform.io/region"]; got != "us-east" {
		t.Errorf("platform.io/region label: got %q, want %q", got, "us-east")
	}
	if got := secret.Labels["other/label"]; got != "other-value" {
		t.Errorf("other/label: got %q, want %q", got, "other-value")
	}
	if got := string(secret.Data["name"]); got != "my-cluster" {
		t.Errorf("data[name]: got %q, want %q", got, "my-cluster")
	}
	if got := string(secret.Data["server"]); got != "https://1.2.3.4:6443" {
		t.Errorf("data[server]: got %q, want %q", got, "https://1.2.3.4:6443")
	}
	if got := string(secret.Data["config"]); got != string(cfgJSON) {
		t.Errorf("data[config]: got %q, want %q", got, string(cfgJSON))
	}
}

func TestUpsertArgoCDSecret_UpdatesExistingSecret(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "argocd"},
		Data:       map[string][]byte{"server": []byte("https://old-server")},
	}
	r, c := newReconciler(existing)
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
	}
	if _, err := r.upsertArgoCDSecret(ctx, cluster, "https://new-server", []byte(`{}`)); err != nil {
		t.Fatalf("upsertArgoCDSecret: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, secret); err != nil {
		t.Fatalf("getting ArgoCD secret: %v", err)
	}
	if got := string(secret.Data["server"]); got != "https://new-server" {
		t.Errorf("data[server]: got %q, want %q", got, "https://new-server")
	}
}

func TestUpsertArgoCDSecret_StandardLabels(t *testing.T) {
	r, c := newReconciler()
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
	}
	if _, err := r.upsertArgoCDSecret(ctx, cluster, "https://1.2.3.4:6443", []byte(`{}`)); err != nil {
		t.Fatalf("upsertArgoCDSecret: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, secret); err != nil {
		t.Fatalf("getting ArgoCD secret: %v", err)
	}
	if got := secret.Labels["app.kubernetes.io/managed-by"]; got != "capi-argocd-registrar" {
		t.Errorf("managed-by: got %q, want %q", got, "capi-argocd-registrar")
	}
	if got := secret.Labels["app.kubernetes.io/component"]; got != "argocd-cluster-secret" {
		t.Errorf("component: got %q, want %q", got, "argocd-cluster-secret")
	}
}

func TestUpsertArgoCDSecret_StandardLabelsWinOverExtra(t *testing.T) {
	r, c := newReconciler()
	r.ExtraSecretLabels = map[string]string{
		"app.kubernetes.io/managed-by": "something-else",
	}
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
	}
	if _, err := r.upsertArgoCDSecret(ctx, cluster, "https://1.2.3.4:6443", []byte(`{}`)); err != nil {
		t.Fatalf("upsertArgoCDSecret: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, secret); err != nil {
		t.Fatalf("getting ArgoCD secret: %v", err)
	}
	if got := secret.Labels["app.kubernetes.io/managed-by"]; got != "capi-argocd-registrar" {
		t.Errorf("standard label should win over extra label: got %q, want %q", got, "capi-argocd-registrar")
	}
}

func TestUpsertArgoCDSecret_IgnoredLabelPrefixes(t *testing.T) {
	r, c := newReconciler()
	r.IgnoredLabelPrefixes = []string{"app.kubernetes.io", "helm.sh"}
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "helm",
				"helm.sh/chart":                "my-chart-1.0",
				"platform.io/env":              "prod",
			},
		},
	}
	if _, err := r.upsertArgoCDSecret(ctx, cluster, "https://1.2.3.4:6443", []byte(`{}`)); err != nil {
		t.Fatalf("upsertArgoCDSecret: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, secret); err != nil {
		t.Fatalf("getting ArgoCD secret: %v", err)
	}
	if _, ok := secret.Labels["helm.sh/chart"]; ok {
		t.Error("helm.sh/chart must be excluded by ignored prefix")
	}
	if got := secret.Labels["app.kubernetes.io/managed-by"]; got != "capi-argocd-registrar" {
		t.Errorf("standard label must override ignored cluster label: got %q", got)
	}
	if got := secret.Labels["platform.io/env"]; got != "prod" {
		t.Errorf("non-ignored label must be copied: got %q, want %q", got, "prod")
	}
}

func TestUpsertArgoCDSecret_ExtraLabels(t *testing.T) {
	r, c := newReconciler()
	r.ExtraSecretLabels = map[string]string{
		"managed-by":      "platform-team",
		"platform.io/env": "forced-value", // should override cluster label
	}
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster",
			Namespace: "default",
			Labels:    map[string]string{"platform.io/env": "staging"},
		},
	}
	if _, err := r.upsertArgoCDSecret(ctx, cluster, "https://1.2.3.4:6443", []byte(`{}`)); err != nil {
		t.Fatalf("upsertArgoCDSecret: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, secret); err != nil {
		t.Fatalf("getting ArgoCD secret: %v", err)
	}
	if got := secret.Labels["managed-by"]; got != "platform-team" {
		t.Errorf("managed-by label: got %q, want %q", got, "platform-team")
	}
	if got := secret.Labels["platform.io/env"]; got != "forced-value" {
		t.Errorf("platform.io/env: extra label should override cluster label, got %q", got)
	}
}

// --- deleteArgoCDSecret tests ---

func TestDeleteArgoCDSecret_DeletesSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "argocd"},
	}
	r, c := newReconciler(secret)
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
	}
	if err := r.deleteArgoCDSecret(ctx, cluster); err != nil {
		t.Fatalf("deleteArgoCDSecret: %v", err)
	}

	fetched := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: "my-cluster", Namespace: "argocd"}, fetched)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected secret to be deleted, got err=%v", err)
	}
}

func TestDeleteArgoCDSecret_IgnoresNotFound(t *testing.T) {
	r, _ := newReconciler()
	ctx := context.Background()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default"},
	}
	if err := r.deleteArgoCDSecret(ctx, cluster); err != nil {
		t.Fatalf("deleteArgoCDSecret should ignore not-found, got %v", err)
	}
}
