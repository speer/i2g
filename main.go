package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"i2g/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

// checkGatewayAPICRDs verifies that the gateway.networking.k8s.io/v1 API
// serves the kinds this controller manages.
func checkGatewayAPICRDs(restConfig *rest.Config) error {
	dc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}
	groupVersion := gatewayv1.GroupVersion.String()
	resources, err := dc.ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		return fmt.Errorf("API group %s is not served: %w", groupVersion, err)
	}
	for _, required := range []string{"listenersets", "httproutes", "gateways"} {
		if !slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool { return r.Name == required }) {
			return fmt.Errorf("resource %q not found in %s", required, groupVersion)
		}
	}
	return nil
}

func main() {
	var (
		ingressClass           string
		namespaceSelector      string
		watchNamespaces        string
		gatewayName            string
		gatewayNamespace       string
		defaultClusterIssuer   string
		listenerHTTPSPort      int
		listenerHTTPPort       int
		updateIngressStatus    bool
		warnAnnotationPrefixes string
		metricsAddr            string
		probeAddr              string
		enableLeaderElection   bool
	)

	flag.StringVar(&ingressClass, "ingress-class", "",
		"Name of the IngressClass to reconcile. Ingresses with a different class (spec.ingressClassName "+
			"or the legacy kubernetes.io/ingress.class annotation) are ignored. Empty considers all "+
			"Ingresses regardless of their class.")
	flag.StringVar(&namespaceSelector, "namespace-selector", "",
		"Label selector restricting the namespaces whose Ingresses are reconciled, "+
			"e.g. 'team=platform,env in (prod,staging)'. Empty selects all namespaces.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated list of namespaces to watch for Ingresses, Services, and generated "+
			"resources. Empty watches all namespaces (requires cluster-wide RBAC); with explicit "+
			"namespaces only namespaced RBAC is needed. Mutually exclusive with --namespace-selector.")
	flag.StringVar(&gatewayName, "gateway-name", "",
		"Name of the Gateway the generated ListenerSets attach to. Required.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "",
		"Namespace of the Gateway. If empty, the Gateway is assumed to live in the same namespace as the Ingress.")
	flag.StringVar(&defaultClusterIssuer, "default-cluster-issuer", "",
		"Value for the cert-manager.io/cluster-issuer annotation on generated ListenerSets when the Ingress "+
			"carries kubernetes.io/tls-acme=\"true\" but no cert-manager issuer annotation.")
	flag.IntVar(&listenerHTTPSPort, "listener-https-port", 443,
		"Port the generated HTTPS listeners bind to.")
	flag.IntVar(&listenerHTTPPort, "listener-http-port", 80,
		"Port the generated plain HTTP listeners bind to (used e.g. for ACME HTTP-01 challenges).")
	flag.StringVar(&warnAnnotationPrefixes, "warn-annotation-prefixes",
		"nginx.ingress.kubernetes.io/,ingress.kubernetes.io/",
		"Comma-separated annotation prefixes that carried traffic semantics for the previous ingress "+
			"controller. Untranslated annotations with these prefixes raise a warning Event on the "+
			"Ingress. Empty disables the warnings.")
	flag.BoolVar(&updateIngressStatus, "update-ingress-status", false,
		"Mirror the Gateway's addresses into the status.loadBalancer of reconciled Ingresses (for "+
			"external-dns, kubectl, ...). Enable only once the original ingress controller no longer "+
			"manages the Ingresses, otherwise both fight over the status.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the metrics endpoint binds to. Use '0' to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager.")

	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	setupLog := ctrl.Log.WithName("setup")

	if gatewayName == "" {
		setupLog.Error(nil, "--gateway-name is required")
		os.Exit(1)
	}
	for flagName, port := range map[string]int{
		"--listener-https-port": listenerHTTPSPort,
		"--listener-http-port":  listenerHTTPPort,
	} {
		if port < 1 || port > 65535 {
			setupLog.Error(nil, flagName+" must be between 1 and 65535", "value", port)
			os.Exit(1)
		}
	}
	if listenerHTTPSPort == listenerHTTPPort {
		setupLog.Error(nil, "--listener-https-port and --listener-http-port must differ")
		os.Exit(1)
	}

	if watchNamespaces != "" && namespaceSelector != "" {
		setupLog.Error(nil, "--watch-namespaces and --namespace-selector are mutually exclusive: "+
			"either pin the controller to fixed namespaces or select namespaces by label")
		os.Exit(1)
	}

	nsSelector := labels.Everything()
	if namespaceSelector != "" {
		var err error
		nsSelector, err = labels.Parse(namespaceSelector)
		if err != nil {
			setupLog.Error(err, "invalid --namespace-selector")
			os.Exit(1)
		}
	}

	managerOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ingress2gateway.i2g.dev",
	}
	if watchNamespaces != "" {
		// Restrict the cache so the controller works with namespaced RBAC.
		// Namespace objects are never read in this mode (the namespace
		// selector is excluded above), so no cluster-wide access is needed.
		namespaces := map[string]cache.Config{}
		for ns := range strings.SplitSeq(watchNamespaces, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces[ns] = cache.Config{}
			}
		}
		managerOpts.Cache = cache.Options{DefaultNamespaces: namespaces}

		// Status writeback reads the Gateway, which may live outside the
		// watched namespaces. Scope only the Gateway informer to it so the
		// watched set for everything else stays untouched.
		if updateIngressStatus && gatewayNamespace != "" {
			if _, ok := namespaces[gatewayNamespace]; !ok {
				gatewayNamespaces := map[string]cache.Config{gatewayNamespace: {}}
				for ns, cfg := range namespaces {
					gatewayNamespaces[ns] = cfg
				}
				managerOpts.Cache.ByObject = map[client.Object]cache.ByObject{
					&gatewayv1.Gateway{}: {Namespaces: gatewayNamespaces},
				}
			}
		}
	}

	restConfig := ctrl.GetConfigOrDie()

	// Fail fast with a clear message when the Gateway API CRDs are missing;
	// otherwise the symptom is an endless stream of "no matches for kind"
	// watch errors after startup.
	if err := checkGatewayAPICRDs(restConfig); err != nil {
		setupLog.Error(err, "Gateway API v1.5+ CRDs (standard channel) are required, see https://gateway-api.sigs.k8s.io/guides/#installing-gateway-api")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(restConfig, managerOpts)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	var warnPrefixes []string
	for prefix := range strings.SplitSeq(warnAnnotationPrefixes, ",") {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			warnPrefixes = append(warnPrefixes, prefix)
		}
	}

	r := &controller.IngressReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		Recorder:               mgr.GetEventRecorderFor("ingress2gateway"),
		IngressClass:           ingressClass,
		NamespaceSelector:      nsSelector,
		GatewayName:            gatewayName,
		GatewayNamespace:       gatewayNamespace,
		DefaultClusterIssuer:   defaultClusterIssuer,
		ListenerHTTPSPort:      int32(listenerHTTPSPort),
		ListenerHTTPPort:       int32(listenerHTTPPort),
		UpdateIngressStatus:    updateIngressStatus,
		WarnAnnotationPrefixes: warnPrefixes,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ingress")
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

	setupLog.Info("starting manager",
		"ingressClass", ingressClass,
		"namespaceSelector", nsSelector.String(),
		"watchNamespaces", watchNamespaces,
		"gateway", gatewayNamespace+"/"+gatewayName,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
