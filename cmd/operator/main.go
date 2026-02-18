/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/fiksn/ingress-doperator/internal/controller"
	_ "github.com/fiksn/ingress-doperator/internal/metrics" // Import to register metrics
	"github.com/fiksn/ingress-doperator/internal/utils"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	DefaultPrivateInfraAnnotations = "service.beta.kubernetes.io/aws-load-balancer-internal=true," +
		"service.beta.kubernetes.io/aws-load-balancer-nlb-target-type=ip," +
		"service.beta.kubernetes.io/aws-load-balancer-type=nlb"
	DefaultGatewayInfraAnnotations = ""
	DefaultGatewayAnnotations      = "cert-manager.io/acme-challenge-type=dns01," +
		"cert-manager.io/acme-dns01-provider=default," +
		"cert-manager.io/cluster-issuer=letsencrypt-cert-manager"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	cfg, opts, err := parseOperatorConfig()
	if err != nil {
		setupLog.Error(err, "Invalid configuration")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	tlsOpts := buildTLSOptions(cfg.EnableHTTP2)

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServer := buildWebhookServer(webhookTLSOpts, cfg.WebhookCertPath, cfg.WebhookCertName, cfg.WebhookCertKey)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := buildMetricsServerOptions(
		cfg.MetricsAddr,
		cfg.SecureMetrics,
		tlsOpts,
		cfg.MetricsCertPath,
		cfg.MetricsCertName,
		cfg.MetricsCertKey,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "94203fac.fiction.si",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := context.Background()
	if cfg.IngressPostProcessingMode == controller.IngressPostProcessingModeDisable {
		if err := ensureDisabledIngressClass(ctx, mgr.GetAPIReader(), mgr.GetClient()); err != nil {
			setupLog.Error(err, "failed to ensure disabled IngressClass")
			os.Exit(1)
		}
	}

	// Verify that the Gateway namespace exists
	if err := ensureGatewayNamespace(ctx, mgr.GetAPIReader(), cfg.GatewayNamespace); err != nil {
		setupLog.Error(err, "Gateway namespace does not exist", "namespace", cfg.GatewayNamespace)
		cmd := fmt.Sprintf("kubectl create namespace %s", cfg.GatewayNamespace)
		setupLog.Info("Please create the namespace first", "command", cmd)
		os.Exit(1)
	}
	setupLog.Info("Verified Gateway namespace exists", "namespace", cfg.GatewayNamespace)

	reconcileCache := make(map[string]utils.ReconcileCacheEntry)
	if cfg.ReconcileCachePersist {
		reconcileCache = loadReconcileCache(ctx, mgr.GetAPIReader(), cfg.GatewayNamespace)
	}

	for _, mapping := range cfg.ParsedClassSnippetsFilters {
		if err := utils.ValidateSnippetsFilterExists(
			ctx,
			mgr.GetAPIReader(),
			cfg.GatewayNamespace,
			mapping.Name,
		); err != nil {
			setupLog.Error(err, "SnippetsFilter not found in gateway namespace",
				"name", mapping.Name,
				"namespace", cfg.GatewayNamespace)
			os.Exit(1)
		}
	}

	if err = (&controller.IngressReconciler{
		Client:                           mgr.GetClient(),
		Scheme:                           mgr.GetScheme(),
		Recorder:                         mgr.GetEventRecorder("ingress-doperator"),
		GatewayNamespace:                 cfg.GatewayNamespace,
		GatewayName:                      cfg.GatewayName,
		GatewayClassName:                 cfg.GatewayClassName,
		WatchNamespace:                   cfg.WatchNamespace,
		OneGatewayPerIngress:             cfg.OneGatewayPerIngress,
		EnableDeletion:                   cfg.EnableDeletion,
		HostnameRewriteFrom:              cfg.HostnameRewriteFrom,
		HostnameRewriteTo:                cfg.HostnameRewriteTo,
		IngressPostProcessingMode:        cfg.IngressPostProcessingMode,
		GatewayAnnotationFilters:         cfg.GatewayFilters,
		HTTPRouteAnnotationFilters:       cfg.HTTPRouteFilters,
		DefaultGatewayAnnotations:        cfg.GatewayAnnotationsMap,
		GatewayInfrastructureAnnotations: cfg.GatewayInfraAnnotationsMap,
		PrivateInfrastructureAnnotations: cfg.PrivateInfraAnnotationsMap,
		ApplyPrivateToAll:                cfg.Private,
		PrivateIngressClassPattern:       cfg.PrivateIngressClassPattern,
		IngressClassFilter:               cfg.IngressClassFilter,
		IngressClassSnippetsFilters:      cfg.ParsedClassSnippetsFilters,
		IngressNameSnippetsFilters:       cfg.ParsedNameSnippetsFilters,
		IngressAnnotationSnippetsAdd:     cfg.ParsedAnnotationSnippetsAdd,
		IngressAnnotationSnippetsRemove:  cfg.ParsedAnnotationSnippetsRemove,
		ClearIngressStatusOnDisable:      cfg.ClearIngressStatusOnDisable,
		ReconcileCache:                   reconcileCache,
		ReconcileCacheNamespace:          cfg.GatewayNamespace,
		ReconcileCacheBaseName:           utils.ReconcileCacheConfigMapBaseName,
		ReconcileCacheShards:             utils.ReconcileCacheShardCount,
		ReconcileCachePersist:            cfg.ReconcileCachePersist,
		ReconcileCacheMaxEntries:         cfg.ReconcileCacheMaxEntries,
		UseIngress2Gateway:               cfg.UseIngress2Gateway,
		Ingress2GatewayProvider:          cfg.Ingress2GatewayProvider,
		Ingress2GatewayIngressClass:      cfg.Ingress2GatewayIngressClass,
		HTTPRouteManager: &utils.HTTPRouteManager{
			Client: mgr.GetClient(),
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ingress")
		os.Exit(1)
	}

	if cfg.WatchNamespace != "" {
		setupLog.Info("Watching Ingresses in specific namespace only", "namespace", cfg.WatchNamespace)
	} else {
		setupLog.Info("Watching Ingresses in all namespaces")
	}

	if cfg.OneGatewayPerIngress {
		setupLog.Info("Mode: One Gateway per Ingress")
	} else {
		setupLog.Info("Mode: Shared Gateway", "gatewayName", cfg.GatewayName)
	}

	if cfg.EnableDeletion {
		setupLog.Info("Deletion enabled: HTTPRoute and Gateway resources will be deleted when Ingress is deleted")
	} else {
		setupLog.Info("Deletion disabled: HTTPRoute and Gateway resources will remain when Ingress is deleted")
	}

	ruleCount, err := validateHostnameRewrite(cfg.HostnameRewriteFrom, cfg.HostnameRewriteTo)
	if err != nil {
		setupLog.Error(err, "Invalid hostname rewrite configuration")
		os.Exit(1)
	}
	if ruleCount > 0 {
		setupLog.Info("Hostname rewriting enabled",
			"from", cfg.HostnameRewriteFrom,
			"to", cfg.HostnameRewriteTo,
			"rules", ruleCount)
	}

	switch cfg.IngressPostProcessingMode {
	case controller.IngressPostProcessingModeNone:
		setupLog.Info("Ingress post processing mode: none")
	case controller.IngressPostProcessingModeDisable:
		setupLog.Info("Ingress post processing mode: disable")
	case controller.IngressPostProcessingModeRemove:
		setupLog.Info("Ingress post processing mode: remove")
	case controller.IngressPostProcessingModeDisableExternalDNS:
		setupLog.Info("Ingress post processing mode: disable-external-dns")
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")

	// Log when metrics server is ready
	if cfg.MetricsAddr != "0" {
		setupLog.Info("Serving metrics server", "addr", cfg.MetricsAddr, "secure", cfg.SecureMetrics)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type operatorConfig struct {
	MetricsAddr                     string
	MetricsCertPath                 string
	MetricsCertName                 string
	MetricsCertKey                  string
	WebhookCertPath                 string
	WebhookCertName                 string
	WebhookCertKey                  string
	EnableLeaderElection            bool
	ProbeAddr                       string
	SecureMetrics                   bool
	EnableHTTP2                     bool
	Verbosity                       int
	GatewayNamespace                string
	GatewayName                     string
	GatewayClassName                string
	WatchNamespace                  string
	OneGatewayPerIngress            bool
	GatewayAnnotationFilters        string
	HTTPRouteAnnotationFilters      string
	EnableDeletion                  bool
	HostnameRewriteFrom             string
	HostnameRewriteTo               string
	IngressPostProcessing           string
	GatewayAnnotations              string
	GatewayInfraAnnotations         string
	Private                         bool
	PrivateAnnotations              string
	PrivateIngressClassPattern      string
	IngressClassFilter              string
	IngressClassSnippetsFilters     string
	IngressNameSnippetsFilters      string
	IngressAnnotationSnippetsAdd    string
	IngressAnnotationSnippetsRemove string
	ReconcileCachePersist           bool
	ReconcileCacheMaxEntries        int
	ClearIngressStatusOnDisable     bool
	UseIngress2Gateway              bool
	Ingress2GatewayProvider         string
	Ingress2GatewayIngressClass     string

	ParsedClassSnippetsFilters     []utils.IngressClassSnippetsFilter
	ParsedNameSnippetsFilters      []utils.IngressClassSnippetsFilter
	ParsedAnnotationSnippetsAdd    []utils.IngressAnnotationSnippetsRule
	ParsedAnnotationSnippetsRemove []utils.IngressAnnotationSnippetsRule
	IngressPostProcessingMode      controller.IngressPostProcessingMode
	GatewayFilters                 []string
	HTTPRouteFilters               []string
	GatewayAnnotationsMap          map[string]string
	GatewayInfraAnnotationsMap     map[string]string
	PrivateInfraAnnotationsMap     map[string]string
}

func parseOperatorConfig() (operatorConfig, zap.Options, error) {
	var cfg operatorConfig
	opts := zap.Options{
		Development: true,
	}

	flag.StringVar(&cfg.GatewayNamespace, "gateway-namespace", "nginx-fabric",
		"The namespace where the Gateway resource will be created")
	flag.StringVar(&cfg.GatewayName, "gateway-name", "ingress-gateway",
		"The name of the Gateway resource (only used when one-gateway-per-ingress is false)")
	flag.StringVar(&cfg.GatewayClassName, "gateway-class-name", "nginx",
		"The GatewayClass to use for created Gateway resources")
	flag.StringVar(&cfg.WatchNamespace, "watch-namespace", "",
		"If specified, only watch Ingresses in this namespace (default: watch all namespaces)")
	flag.StringVar(&cfg.IngressClassFilter, "ingress-class-filter", "*",
		"Glob pattern to filter which ingress classes to process (e.g., '*private*', 'nginx', '*'). "+
			"Default '*' processes all classes.")
	flag.BoolVar(&cfg.OneGatewayPerIngress, "one-gateway-per-ingress", false,
		"If true, create a separate Gateway for each Ingress with the same name")
	flag.BoolVar(&cfg.EnableDeletion, "enable-deletion", false,
		"If true, delete HTTPRoute (and Gateway in one-gateway-per-ingress mode) when Ingress is deleted")
	flag.StringVar(&cfg.HostnameRewriteFrom, "hostname-rewrite-from", "",
		"Comma-separated list of domain suffixes to match for rewriting (e.g., 'domain.cc,other.com'). "+
			"Used with --hostname-rewrite-to.")
	flag.StringVar(&cfg.HostnameRewriteTo, "hostname-rewrite-to", "",
		"Comma-separated list of replacement domain suffixes (e.g., 'foo.domain.cc,bar.other.com'). "+
			"Transforms 'a.b.domain.cc' to 'a.b.foo.domain.cc'. "+
			"Must have same number of items as --hostname-rewrite-from.")
	flag.StringVar(&cfg.IngressPostProcessing, "ingress-postprocessing", "none",
		"How to handle the post processing of ingress: 'none' (no action), "+
			"'disable' (remove ingress class), 'remove' (delete ingress), "+
			"'disable-external-dns' (force external-dns to read annotations only)")
	flag.StringVar(&cfg.GatewayAnnotations, "gateway-annotations", DefaultGatewayAnnotations,
		"Comma-separated key=value pairs for Gateway metadata annotations (applied to all Gateways)")
	flag.StringVar(&cfg.GatewayInfraAnnotations, "gateway-infrastructure-annotations",
		DefaultGatewayInfraAnnotations,
		"Comma-separated key=value pairs for Gateway infrastructure annotations (applied to all Gateways)")
	flag.StringVar(&cfg.PrivateAnnotations, "private-annotations", DefaultPrivateInfraAnnotations,
		"Comma-separated key=value pairs defining what 'private' means for Gateway infrastructure annotations")
	flag.BoolVar(&cfg.Private, "private", false, "If true, apply private annotations to all Gateways")
	flag.StringVar(&cfg.PrivateIngressClassPattern, "private-ingress-class-pattern", "*private*",
		"Glob pattern for ingress class names (e.g., '*private') that should get private infrastructure annotations")
	flag.StringVar(&cfg.IngressClassSnippetsFilters, "ingress-class-snippets-filter", "",
		"Comma-separated list of pattern:snippetsFilterName entries. "+
			"If ingress class matches the glob, the SnippetsFilter is copied from the Gateway namespace and attached.")
	flag.StringVar(&cfg.IngressNameSnippetsFilters, "ingress-name-snippets-filter", "",
		"Comma-separated list of pattern:snippetsFilterName entries. "+
			"If ingress name matches the glob, the SnippetsFilter is copied from the Gateway namespace and attached.")
	flag.StringVar(&cfg.IngressAnnotationSnippetsAdd, "ingress-annotation-snippets-add", "",
		"Semicolon-separated list of key=value:filter1,filter2 entries. "+
			"If annotation value matches glob, add SnippetsFilter(s).")
	flag.StringVar(&cfg.IngressAnnotationSnippetsRemove, "ingress-annotation-snippets-remove", "",
		"Semicolon-separated list of key=value:filter1,filter2 entries. "+
			"If annotation value matches glob, remove SnippetsFilter(s).")
	flag.BoolVar(&cfg.ReconcileCachePersist, "reconcile-cache-persist", true,
		"If false, do not persist the reconcile cache to ConfigMaps.")
	flag.IntVar(&cfg.ReconcileCacheMaxEntries, "reconcile-cache-max-entries", 0,
		"Maximum number of entries to keep in reconcile cache (0 = unlimited).")
	flag.BoolVar(&cfg.ClearIngressStatusOnDisable, "clear-ingress-status-on-disable", true,
		"If true, clear status.loadBalancer when disabling an Ingress (requires update on ingresses/status).")
	flag.StringVar(&cfg.GatewayAnnotationFilters, "gateway-annotation-filters",
		controller.DefaultGatewayAnnotationFilters,
		"Comma-separated list of annotation prefixes to exclude from Gateway resources")
	flag.StringVar(&cfg.HTTPRouteAnnotationFilters, "httproute-annotation-filters",
		controller.DefaultHTTPRouteAnnotationFilters,
		"Comma-separated list of annotation prefixes to exclude from HTTPRoute resources")
	flag.BoolVar(&cfg.UseIngress2Gateway, "use-ingress2gateway", false,
		"If true, use the ingress2gateway library for translation (disables hostname/certificate mangling)")
	flag.StringVar(&cfg.Ingress2GatewayProvider, "ingress2gateway-provider", "ingress-nginx",
		"Provider to use with ingress2gateway (e.g., ingress-nginx, istio, kong)")
	flag.StringVar(&cfg.Ingress2GatewayIngressClass, "ingress2gateway-ingress-class", "nginx",
		"Ingress class name for provider-specific filtering in ingress2gateway")
	flag.StringVar(&cfg.MetricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.ProbeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.EnableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&cfg.SecureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&cfg.WebhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&cfg.WebhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.WebhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.MetricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.MetricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	flag.StringVar(&cfg.MetricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&cfg.EnableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.IntVar(&cfg.Verbosity, "v", 0, "Log verbosity (0 = info, higher = more verbose)")
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if cfg.Verbosity > 0 {
		opts.Development = false
		opts.Level = zapcore.Level(-cfg.Verbosity)
	}

	var err error
	cfg.ParsedClassSnippetsFilters, err = utils.ParseIngressClassSnippetsFilters(cfg.IngressClassSnippetsFilters)
	if err != nil {
		return cfg, opts, fmt.Errorf("invalid ingress-class-snippets-filter value: %w", err)
	}
	cfg.ParsedNameSnippetsFilters, err = utils.ParseIngressClassSnippetsFilters(cfg.IngressNameSnippetsFilters)
	if err != nil {
		return cfg, opts, fmt.Errorf("invalid ingress-name-snippets-filter value: %w", err)
	}
	cfg.ParsedAnnotationSnippetsAdd, err = utils.ParseIngressAnnotationSnippetsRules(cfg.IngressAnnotationSnippetsAdd)
	if err != nil {
		return cfg, opts, fmt.Errorf("invalid ingress-annotation-snippets-add value: %w", err)
	}
	cfg.ParsedAnnotationSnippetsRemove, err =
		utils.ParseIngressAnnotationSnippetsRules(cfg.IngressAnnotationSnippetsRemove)
	if err != nil {
		return cfg, opts, fmt.Errorf("invalid ingress-annotation-snippets-remove value: %w", err)
	}

	cfg.IngressPostProcessingMode, err = parseIngressPostProcessingMode(cfg.IngressPostProcessing)
	if err != nil {
		return cfg, opts, err
	}

	cfg.GatewayFilters = splitCSV(cfg.GatewayAnnotationFilters)
	cfg.HTTPRouteFilters = splitCSV(cfg.HTTPRouteAnnotationFilters)
	if cfg.IngressPostProcessingMode == controller.IngressPostProcessingModeDisableExternalDNS {
		cfg.GatewayFilters = appendFilterIfMissing(cfg.GatewayFilters, controller.ExternalDNSIngressHostnameSource)
		cfg.GatewayFilters = appendFilterIfMissing(cfg.GatewayFilters, controller.ExternalDNSHostnameAnnotation)
		cfg.HTTPRouteFilters = appendFilterIfMissing(cfg.HTTPRouteFilters, controller.ExternalDNSIngressHostnameSource)
		cfg.HTTPRouteFilters = appendFilterIfMissing(cfg.HTTPRouteFilters, controller.ExternalDNSHostnameAnnotation)
	}
	cfg.GatewayAnnotationsMap = parseKeyValueCSV(cfg.GatewayAnnotations)
	cfg.GatewayInfraAnnotationsMap = parseKeyValueCSV(cfg.GatewayInfraAnnotations)
	cfg.PrivateInfraAnnotationsMap = parseKeyValueCSV(cfg.PrivateAnnotations)

	return cfg, opts, nil
}

func parseIngressPostProcessingMode(value string) (controller.IngressPostProcessingMode, error) {
	switch value {
	case "none":
		return controller.IngressPostProcessingModeNone, nil
	case "disable":
		return controller.IngressPostProcessingModeDisable, nil
	case "remove":
		return controller.IngressPostProcessingModeRemove, nil
	case "disable-external-dns":
		return controller.IngressPostProcessingModeDisableExternalDNS, nil
	default:
		return controller.IngressPostProcessingModeNone,
			fmt.Errorf("invalid ingress-post-processing value %q (allowed: none, disable, remove, disable-external-dns)", value)
	}
}

func buildTLSOptions(enableHTTP2 bool) []func(*tls.Config) {
	if enableHTTP2 {
		return nil
	}
	return []func(*tls.Config){
		func(c *tls.Config) {
			setupLog.Info("disabling http/2")
			c.NextProtos = []string{"http/1.1"}
		},
	}
}

func buildWebhookServer(
	tlsOpts []func(*tls.Config),
	webhookCertPath string,
	webhookCertName string,
	webhookCertKey string,
) webhook.Server {
	webhookServerOptions := webhook.Options{
		TLSOpts: tlsOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	return webhook.NewServer(webhookServerOptions)
}

func buildMetricsServerOptions(
	metricsAddr string,
	secureMetrics bool,
	tlsOpts []func(*tls.Config),
	metricsCertPath string,
	metricsCertName string,
	metricsCertKey string,
) metricsserver.Options {
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	return metricsServerOptions
}

func ensureGatewayNamespace(ctx context.Context, reader client.Reader, namespace string) error {
	var ns corev1.Namespace
	return reader.Get(ctx, client.ObjectKey{Name: namespace}, &ns)
}

func ensureDisabledIngressClass(ctx context.Context, reader client.Reader, c client.Client) error {
	ingressClass := &networkingv1.IngressClass{}
	err := reader.Get(ctx, client.ObjectKey{Name: controller.DisabledIngressClassName}, ingressClass)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	ingressClass = &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: controller.DisabledIngressClassName,
		},
		Spec: networkingv1.IngressClassSpec{
			Controller: controller.DisabledIngressClassController,
		},
	}
	if err := c.Create(ctx, ingressClass); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create disabled IngressClass: %w", err)
	}
	return nil
}

func loadReconcileCache(
	ctx context.Context,
	reader client.Reader,
	gatewayNamespace string,
) map[string]utils.ReconcileCacheEntry {
	reconcileCache, err := utils.LoadReconcileCacheSharded(
		ctx,
		reader,
		gatewayNamespace,
		utils.ReconcileCacheConfigMapBaseName,
		utils.ReconcileCacheShardCount,
	)
	if err != nil {
		setupLog.Error(err, "Failed to load reconcile cache, continuing with empty cache")
		return make(map[string]utils.ReconcileCacheEntry)
	}
	return reconcileCache
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func appendFilterIfMissing(filterList []string, value string) []string {
	for _, item := range filterList {
		if strings.TrimSpace(item) == value {
			return filterList
		}
	}
	return append(filterList, value)
}

func parseKeyValueCSV(raw string) map[string]string {
	out := make(map[string]string)
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return out
}

func validateHostnameRewrite(from, to string) (int, error) {
	if from == "" && to == "" {
		return 0, nil
	}
	if from == "" || to == "" {
		return 0, fmt.Errorf("both --hostname-rewrite-from and --hostname-rewrite-to must be specified together")
	}
	fromParts := strings.Split(from, ",")
	toParts := strings.Split(to, ",")
	if len(fromParts) != len(toParts) {
		return 0, fmt.Errorf("--hostname-rewrite-from and --hostname-rewrite-to must have same number of items")
	}
	return len(fromParts), nil
}
