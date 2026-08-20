// Command manager runs the brewlet-operator node lifecycle controller
// (https://github.com/brewlet/specs): it watches nodes opted into provisioning,
// brewlet-node-provisioner DaemonSet and the brewlet RuntimeClass, and surfaces
// each node's provisioning state via annotations and events.
package main

import (
	"flag"
	"os"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/controller"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	// client-go's scheme registers core/v1, apps/v1, autoscaling/v1 and node/v1,
	// which covers the node controller and everything the JavaApplication
	// controller generates. Add our own apps.brewlet.sh/v1alpha1 types on top.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(nodev1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		cfg              controller.Config
		metricsAddr      string
		probeAddr        string
		enableLeaderElec bool
	)
	flag.StringVar(&cfg.Namespace, "namespace", "brewlet", "namespace to manage the provisioner DaemonSet in")
	flag.StringVar(&cfg.ProvisionerImage, "provisioner-image", "ghcr.io/brewlet/node-provisioner:0.1.0", "brewlet-node-provisioner image to run")
	flag.StringVar(&cfg.JDKs, "jdks", "temurin-21", "comma-separated <dist>-<feature> JDK roots to install on nodes")
	flag.StringVar(&cfg.Launchers, "launchers", "", "comma-separated launcher layers to install (e.g. jaz)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	flag.BoolVar(&enableLeaderElec, "leader-elect", false, "enable leader election for HA (single active manager)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElec,
		LeaderElectionID:       "brewlet-operator.brewlet.sh",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.NodeReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("brewlet-operator"),
		Config:   cfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	if err := (&controller.NodeProfileReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("brewlet-operator"),
		Config:   cfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NodeProfile")
		os.Exit(1)
	}

	if err := (&controller.JavaApplicationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("brewlet-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "JavaApplication")
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

	setupLog.Info("starting brewlet-operator",
		"namespace", cfg.Namespace, "provisionerImage", cfg.ProvisionerImage,
		"jdks", cfg.JDKs, "launchers", cfg.Launchers)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
