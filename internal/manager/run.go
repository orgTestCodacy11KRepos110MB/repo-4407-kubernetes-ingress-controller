// Package manager implements the controller manager for all controllers
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/blang/semver/v4"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	knativev1alpha1 "knative.dev/networking/pkg/apis/networking/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/controllers/gateway"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/dataplane"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/dataplane/sendconfig"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/manager/metadata"
	mgrutils "github.com/kong/kubernetes-ingress-controller/v2/internal/manager/utils"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/manager/utils/kongconfig"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/util"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/util/kubernetes/object/status"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/versions"
	konghqcomv1 "github.com/kong/kubernetes-ingress-controller/v2/pkg/apis/configuration/v1"
	konghqcomv1alpha1 "github.com/kong/kubernetes-ingress-controller/v2/pkg/apis/configuration/v1alpha1"
	configurationv1beta1 "github.com/kong/kubernetes-ingress-controller/v2/pkg/apis/configuration/v1beta1"
)

// -----------------------------------------------------------------------------
// Controller Manager - Setup & Run
// -----------------------------------------------------------------------------

// Run starts the controller manager and blocks until it exits.
func Run(ctx context.Context, c *Config, diagnostic util.ConfigDumpDiagnostic, deprecatedLogger logrus.FieldLogger) error {
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting controller manager", "release", metadata.Release, "repo", metadata.Repo, "commit", metadata.Commit)
	setupLog.Info("the ingress class name has been set", "value", c.IngressClassName)

	setupLog.Info("getting enabled options and features")
	featureGates, err := setupFeatureGates(setupLog, c.FeatureGates)
	if err != nil {
		return fmt.Errorf("failed to configure feature gates: %w", err)
	}

	setupLog.Info("getting the kubernetes client configuration")
	kubeconfig, err := c.GetKubeconfig()
	if err != nil {
		return fmt.Errorf("get kubeconfig from file %q: %w", c.KubeconfigPath, err)
	}
	setupLog.Info("getting the kong admin api client configuration")
	if c.KongAdminToken != "" {
		c.KongAdminAPIConfig.Headers = append(c.KongAdminAPIConfig.Headers, "kong-admin-token:"+c.KongAdminToken)
	}
	kongClients, err := getKongClients(ctx, c)
	if err != nil {
		return fmt.Errorf("unable to build kong api client(s): %w", err)
	}

	// Get Kong configuration root(s) to validate them and extract Kong's version.
	kongRoots, err := kongconfig.GetRoots(ctx, setupLog, c.KongAdminInitializationRetries, c.KongAdminInitializationRetryDelay, kongClients)
	if err != nil {
		return fmt.Errorf("could not retrieve Kong admin root(s): %w", err)
	}
	dbMode, v, err := kongconfig.ValidateRoots(kongRoots, c.SkipCACertificates)
	if err != nil {
		return fmt.Errorf("could not validate Kong admin root(s) configuration: %w", err)
	}
	semV := semver.Version{Major: v.Major(), Minor: v.Minor(), Patch: v.Patch()}
	versions.SetKongVersion(semV)
	kongConfig := sendconfig.New(ctx, setupLog, kongClients, semV, dbMode, c.Concurrency, c.FilterTags)

	setupLog.Info("configuring and building the controller manager")
	controllerOpts, err := setupControllerOptions(setupLog, c, dbMode)
	if err != nil {
		return fmt.Errorf("unable to setup controller options: %w", err)
	}
	mgr, err := ctrl.NewManager(kubeconfig, controllerOpts)
	if err != nil {
		return fmt.Errorf("unable to start controller manager: %w", err)
	}

	setupLog.Info("Starting Admission Server")
	if err := setupAdmissionServer(ctx, c, mgr.GetClient(), deprecatedLogger); err != nil {
		return err
	}

	setupLog.Info("Initializing Dataplane Client")
	eventRecorder := mgr.GetEventRecorderFor(KongClientEventRecorderComponentName)
	dataplaneClient, err := dataplane.NewKongClient(
		deprecatedLogger,
		time.Duration(c.ProxyTimeoutSeconds*float32(time.Second)),
		c.IngressClassName,
		c.EnableReverseSync,
		c.SkipCACertificates,
		diagnostic,
		kongConfig,
		eventRecorder,
		dbMode,
	)
	if err != nil {
		return fmt.Errorf("failed to initialize kong data-plane client: %w", err)
	}

	setupLog.Info("Initializing Dataplane Synchronizer")
	synchronizer, err := setupDataplaneSynchronizer(setupLog, deprecatedLogger, mgr, dataplaneClient, c.ProxySyncSeconds)
	if err != nil {
		return fmt.Errorf("unable to initialize dataplane synchronizer: %w", err)
	}

	if enabled, ok := featureGates[combinedRoutesFeature]; ok && enabled {
		dataplaneClient.EnableCombinedServiceRoutes()
		setupLog.Info("combined routes mode has been enabled")
	}

	var kubernetesStatusQueue *status.Queue
	if c.UpdateStatus {
		setupLog.Info("Starting Status Updater")
		kubernetesStatusQueue = status.NewQueue()
		dataplaneClient.EnableKubernetesObjectReports(kubernetesStatusQueue)
	} else {
		setupLog.Info("status updates disabled, skipping status updater")
	}

	setupLog.Info("Initializing Dataplane Address Discovery")
	dataplaneAddressFinder, udpDataplaneAddressFinder, err := setupDataplaneAddressFinder(mgr.GetClient(), c, setupLog)
	if err != nil {
		return err
	}

	gateway.ControllerName = gatewayv1beta1.GatewayController(c.GatewayAPIControllerName)

	setupLog.Info("Starting Enabled Controllers")
	controllers, err := setupControllers(mgr, dataplaneClient,
		dataplaneAddressFinder, udpDataplaneAddressFinder, kubernetesStatusQueue, c, featureGates)
	if err != nil {
		return fmt.Errorf("unable to setup controller as expected %w", err)
	}
	for _, c := range controllers {
		if err := c.MaybeSetupWithManager(mgr); err != nil {
			return fmt.Errorf("unable to create controller %q: %w", c.Name(), err)
		}
	}

	// BUG: kubebuilder (at the time of writing - 3.0.0-rc.1) does not allow this tag anywhere else than main.go
	// See https://github.com/kubernetes-sigs/kubebuilder/issues/932
	// +kubebuilder:scaffold:builder

	setupLog.Info("Starting health check servers")
	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		return fmt.Errorf("unable to setup healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("check", func(_ *http.Request) error {
		if !synchronizer.IsReady() {
			return errors.New("synchronizer not yet configured")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("unable to setup readyz: %w", err)
	}

	if c.AnonymousReports {
		setupLog.Info("Starting anonymous reports")
		// the argument checking the watch namespaces length enables or disables mesh detection. the mesh detect client
		// attempts to use all namespaces and can't utilize a manager multi-namespaced cache, so if we need to limit
		// namespace access we just disable mesh detection altogether.
		if err := mgrutils.RunReport(ctx, kubeconfig, kongConfig, c.PublishService.String(), metadata.Release,
			len(c.WatchNamespaces) == 0, featureGates); err != nil {
			setupLog.Error(err, "anonymous reporting failed")
		}
	} else {
		setupLog.Info("anonymous reports disabled, skipping")
	}

	setupLog.Info("Starting manager")
	return mgr.Start(ctx)
}

func getScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()

	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := konghqcomv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := konghqcomv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := configurationv1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := knativev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := gatewayv1alpha2.Install(scheme); err != nil {
		return nil, err
	}
	if err := gatewayv1beta1.Install(scheme); err != nil {
		return nil, err
	}

	return scheme, nil
}
