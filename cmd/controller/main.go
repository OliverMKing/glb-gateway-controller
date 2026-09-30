package main

import (
	"flag"
	"os"

	api "github.com/olivermking/glb-gateway-controller/api/v1alpha1"
	"github.com/olivermking/glb-gateway-controller/internal/azure"
	controller "github.com/olivermking/glb-gateway-controller/internal/controller"
	clusterv1 "go.goms.io/fleet/apis/cluster/v1"
	placementv1 "go.goms.io/fleet/apis/placement/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Register hub APIs once so the manager can cache and apply typed objects.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(placementv1.AddToScheme(scheme))
}

func main() {
	// Expose standard controller-runtime endpoints and optional leader election.
	var metricsAddr string
	var probeAddr string
	var leaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election.")
	zapOptions := zap.Options{Development: true}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	// The process runs against the Fleet hub supplied by in-cluster configuration.
	config := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElection,
		LeaderElectionID:       "glb-gateway-controller.gateway.glb.azure.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// DefaultAzureCredential supports the demo secret and production workload identity.
	azureManager, err := azure.NewARMManager(nil)
	if err != nil {
		setupLog.Error(err, "create Azure manager")
		os.Exit(1)
	}

	if err := (&controller.GlobalGatewayPolicyReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Azure: azureManager, FrontendProber: controller.NewTCPRegionalFrontendProber(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
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
