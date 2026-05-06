//go:build e2e

package e2e_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workloadClusterName = "e2e-workload"
	workloadNamespace   = "e2e-test"
	argoCDNamespace     = "argocd"

	provisioningTimeout = 12 * time.Minute
	pollingInterval     = 15 * time.Second
	deletionTimeout     = 3 * time.Minute
)

func fixtureDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "fixtures")
}

var _ = Describe("capi-argocd-registrar", Ordered, func() {

	Describe("registers a provisioned CAPD cluster", func() {
		BeforeAll(func() {
			By("Waiting for any leftover e2e-test namespace to be fully removed")
			ns := &corev1.Namespace{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: workloadNamespace}, ns); err == nil {
				Eventually(func(g Gomega) {
					err := k8sClient.Get(ctx, types.NamespacedName{Name: workloadNamespace}, ns)
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
						"e2e-test namespace is still terminating after 8 minutes; "+
							"run 'task e2e-teardown && task e2e-setup' to reset the environment")
				}).WithTimeout(8 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
			}

			By("Applying workload cluster manifests")
			cmd := exec.CommandContext(ctx, "kubectl", "apply",
				"-f", filepath.Join(fixtureDir(), "workload-cluster.yaml"))
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "kubectl apply failed:\n%s", string(out))
		})

		It("should see the CAPI Cluster reach Provisioned phase", func() {
			By(fmt.Sprintf("Waiting up to %s for CAPI Cluster to become Provisioned", provisioningTimeout))
			Eventually(func(g Gomega) {
				cluster := &clusterv1.Cluster{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      workloadClusterName,
					Namespace: workloadNamespace,
				}, cluster)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cluster.Status.Phase).To(Equal(string(clusterv1.ClusterPhaseProvisioned)),
					"cluster phase is %q", cluster.Status.Phase)
			}).WithTimeout(provisioningTimeout).WithPolling(pollingInterval).Should(Succeed())
		})

		It("should create an ArgoCD cluster secret with the correct content", func() {
			By("Waiting for the ArgoCD cluster secret to appear in the argocd namespace")
			Eventually(func(g Gomega) {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      workloadClusterName,
					Namespace: argoCDNamespace,
				}, secret)
				g.Expect(err).NotTo(HaveOccurred(), "ArgoCD cluster secret not yet present")

				g.Expect(secret.Labels).To(HaveKeyWithValue(
					"argocd.argoproj.io/secret-type", "cluster"),
					"ArgoCD secret-type label must be set")

				g.Expect(secret.Data).To(HaveKey("name"))
				g.Expect(secret.Data).To(HaveKey("server"))
				g.Expect(secret.Data).To(HaveKey("config"))
				g.Expect(string(secret.Data["name"])).To(Equal(workloadClusterName))
				g.Expect(secret.Data["server"]).NotTo(BeEmpty(), "server field must not be empty")
				g.Expect(secret.Data["config"]).NotTo(BeEmpty(), "config field must not be empty")

				By("Verifying platform.io/ labels were propagated from the CAPI Cluster")
				g.Expect(secret.Labels).To(HaveKeyWithValue("platform.io/env", "e2e"))
				g.Expect(secret.Labels).To(HaveKeyWithValue("platform.io/region", "local"))
			}).WithTimeout(2 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
		})

		It("should restore the ArgoCD secret if it is manually deleted", func() {
			By("Deleting the ArgoCD cluster secret manually")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      workloadClusterName,
				Namespace: argoCDNamespace,
			}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())

			By("Waiting for the controller to restore the ArgoCD secret")
			Eventually(func(g Gomega) {
				restored := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      workloadClusterName,
					Namespace: argoCDNamespace,
				}, restored)
				g.Expect(err).NotTo(HaveOccurred(), "ArgoCD secret not yet restored")
				g.Expect(restored.Data).To(HaveKey("server"))
				g.Expect(restored.Data["server"]).NotTo(BeEmpty())
			}).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
		})
	})

	Describe("label propagation", func() {
		It("should ignore labels with specified prefixes", func() {
			By("Adding an ignorable label to the CAPI Cluster")
			cluster := &clusterv1.Cluster{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: workloadClusterName, Namespace: workloadNamespace}, cluster)).To(Succeed())

			patch := client.MergeFrom(cluster.DeepCopy())
			if cluster.Labels == nil {
				cluster.Labels = make(map[string]string)
			}
			cluster.Labels["helm.sh/chart"] = "should-be-ignored"
			Expect(k8sClient.Patch(ctx, cluster, patch)).To(Succeed())

			By("Verifying the ArgoCD secret does not contain the ignored label")
			Eventually(func(g Gomega) {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: workloadClusterName, Namespace: argoCDNamespace}, secret)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(secret.Labels).NotTo(HaveKey("helm.sh/chart"))
				g.Expect(secret.Labels).To(HaveKeyWithValue("platform.io/env", "e2e"))
			}).WithTimeout(3 * time.Minute).WithPolling(10 * time.Second).Should(Succeed())
		})
	})

	Describe("deregisters a deleted cluster", func() {
		It("should remove the ArgoCD secret when the CAPI cluster is deleted", func() {
			By("Deleting the workload cluster manifests")
			cmd := exec.CommandContext(ctx, "kubectl", "delete",
				"-f", filepath.Join(fixtureDir(), "workload-cluster.yaml"),
				"--ignore-not-found=true", "--wait=false")
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "kubectl delete failed:\n%s", string(out))

			By(fmt.Sprintf("Waiting up to %s for CAPI Cluster to be fully deleted", deletionTimeout))
			Eventually(func(g Gomega) {
				cluster := &clusterv1.Cluster{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      workloadClusterName,
					Namespace: workloadNamespace,
				}, cluster)
				if err != nil {
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
						"unexpected error waiting for cluster deletion: %v", err)
					return
				}
				g.Expect(cluster.DeletionTimestamp).NotTo(BeNil(),
					"cluster should be terminating")
			}).WithTimeout(deletionTimeout).WithPolling(10 * time.Second).Should(Succeed())

			By("Verifying the ArgoCD cluster secret was removed")
			Eventually(func(g Gomega) {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      workloadClusterName,
					Namespace: argoCDNamespace,
				}, secret)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
					"ArgoCD cluster secret should have been deleted by the controller")
			}).WithTimeout(60 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})
	})

	Describe("does not register a cluster not yet provisioned", func() {
		const (
			pendingCluster = "e2e-pending"
			pendingNS      = "default"
		)

		It("should NOT create an ArgoCD secret for a Pending cluster", func() {
			By("Creating a CAPI Cluster with no infrastructure ref (stays in Pending)")
			cluster := &clusterv1.Cluster{}
			cluster.Name = pendingCluster
			cluster.Namespace = pendingNS
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

			By("Confirming no ArgoCD secret is created within 30s")
			Consistently(func(g Gomega) {
				secret := &corev1.Secret{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      pendingCluster,
					Namespace: argoCDNamespace,
				}, secret)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
					"ArgoCD secret must not exist for a non-Provisioned cluster")
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("Cleaning up the pending cluster")
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
			Eventually(func(g Gomega) {
				c := &clusterv1.Cluster{}
				err := k8sClient.Get(ctx, client.ObjectKey{Name: pendingCluster, Namespace: pendingNS}, c)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())
		})
	})
})
