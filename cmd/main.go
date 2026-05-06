package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/drydock-dev/capi-argocd-registrar/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		leaderElect          bool
		argoCDNamespace      string
		extraLabels          = map[string]string{}
		ignoredLabelPrefixes []string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&argoCDNamespace, "argocd-namespace", "argocd", "Namespace where ArgoCD is installed.")
	flag.Func("extra-label", "Additional label (key=value) to set on every ArgoCD cluster secret. May be repeated.", func(s string) error {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("invalid format %q, expected key=value", s)
		}
		extraLabels[k] = v
		return nil
	})
	flag.Func("ignore-label-prefix", "Label key prefix to exclude when copying CAPI Cluster labels. May be repeated.", func(s string) error {
		ignoredLabelPrefixes = append(ignoredLabelPrefixes, s)
		return nil
	})

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "capi-argocd-registrar.drydock-dev.github.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.ClusterReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		ArgoCDNamespace:      argoCDNamespace,
		ExtraSecretLabels:    extraLabels,
		IgnoredLabelPrefixes: ignoredLabelPrefixes,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Cluster")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
