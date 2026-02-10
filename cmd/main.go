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

	corev1 "k8s.io/api/core/v1"
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

	"github.com/fiksn/ingress-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	DefaultPrivateInfraAnnotations = "service.beta.kubernetes.io/aws-load-balancer-internal=true,service.beta.kubernetes.io/aws-load-balancer-nlb-target-type=ip,service.beta.kubernetes.io/aws-load-balancer-type=nlb"
	DefaultGatewayInfraAnnotations = "cert-manager.io/acme-challenge-type=dns01,cert-manager.io/acme-dns01-provider=default,cert-manager.io/cluster-issuer=letsencrypt-cert-manager"
	DefaultGatewayAnnotations      = "cert-manager.io/acme-challenge-type=dns01,cert-manager.io/acme-dns01-provider=default,cert-manager.io/cluster-issuer=letsencrypt-cert-manager"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var gatewayNamespace string
	var gatewayName string
	var watchNamespace string
	var oneGatewayPerIngress bool
	var gatewayAnnotationFilters string
	var httpRouteAnnotationFilters string
	var enableDeletion bool
	var hostnameRewriteFrom string
	var hostnameRewriteTo string
	var disableSourceIngress bool
	var gatewayAnnotations string
	var gatewayInfraAnnotations string
	var private bool
	var privateAnnotations string
	var privateIngressClassPattern string
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "nginx-fabric", "The namespace where the Gateway resource will be created")
	flag.StringVar(&gatewayName, "gateway-name", "ingress-gateway", "The name of the Gateway resource (only used when one-gateway-per-ingress is false)")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "If specified, only watch Ingresses in this namespace (default: watch all namespaces)")
	flag.BoolVar(&oneGatewayPerIngress, "one-gateway-per-ingress", false, "If true, create a separate Gateway for each Ingress with the same name")
	flag.BoolVar(&enableDeletion, "enable-deletion", false, "If true, delete HTTPRoute (and Gateway in one-gateway-per-ingress mode) when Ingress is deleted")
	flag.StringVar(&hostnameRewriteFrom, "hostname-rewrite-from", "", "Domain suffix to match for rewriting (e.g., 'domain.cc'). Used with --hostname-rewrite-to.")
	flag.StringVar(&hostnameRewriteTo, "hostname-rewrite-to", "", "Replacement domain suffix (e.g., 'foo.domain.cc'). Transforms 'a.b.domain.cc' to 'a.b.foo.domain.cc' to avoid DNS conflicts.")
	flag.BoolVar(&disableSourceIngress, "disable-source-ingress", false, "If true, disable the source Ingress by removing its ingressClassName to prevent nginx-ingress from processing it")
	flag.StringVar(&gatewayAnnotations, "gateway-annotations", DefaultGatewayAnnotations, "Comma-separated key=value pairs for Gateway metadata annotations (applied to all Gateways)")
	flag.StringVar(&gatewayInfraAnnotations, "gateway-infrastructure-annotations", DefaultGatewayInfraAnnotations, "Comma-separated key=value pairs for Gateway infrastructure annotations (applied to all Gateways)")
	flag.StringVar(&privateAnnotations, "private-annotations", DefaultPrivateInfraAnnotations, "Comma-separated key=value pairs defining what 'private' means for Gateway infrastructure annotations")
	flag.BoolVar(&private, "private", false, "If true, apply private annotations to all Gateways")
	flag.StringVar(&privateIngressClassPattern, "private-ingress-class-pattern", "*private*", "Glob pattern for ingress class names (e.g., '*private') that should get private infrastructure annotations")
	flag.StringVar(&gatewayAnnotationFilters, "gateway-annotation-filters", controller.DefaultGatewayAnnotationFilters, "Comma-separated list of annotation prefixes to exclude from Gateway resources")
	flag.StringVar(&httpRouteAnnotationFilters, "httproute-annotation-filters", controller.DefaultHTTPRouteAnnotationFilters, "Comma-separated list of annotation prefixes to exclude from HTTPRoute resources")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
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

	// Verify that the Gateway namespace exists
	ctx := context.Background()
	var ns corev1.Namespace
	if err := mgr.GetAPIReader().Get(ctx, client.ObjectKey{Name: gatewayNamespace}, &ns); err != nil {
		setupLog.Error(err, "Gateway namespace does not exist", "namespace", gatewayNamespace)
		setupLog.Info("Please create the namespace first", "command", fmt.Sprintf("kubectl create namespace %s", gatewayNamespace))
		os.Exit(1)
	}
	setupLog.Info("Verified Gateway namespace exists", "namespace", gatewayNamespace)

	// Parse annotation filters
	var gatewayFilters []string
	if gatewayAnnotationFilters != "" {
		gatewayFilters = strings.Split(gatewayAnnotationFilters, ",")
	}
	var httpRouteFilters []string
	if httpRouteAnnotationFilters != "" {
		httpRouteFilters = strings.Split(httpRouteAnnotationFilters, ",")
	}

	// Parse gateway metadata annotations
	gwAnnotations := make(map[string]string)
	if gatewayAnnotations != "" {
		for _, pair := range strings.Split(gatewayAnnotations, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				gwAnnotations[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	// Parse base infrastructure annotations
	infraAnnotations := make(map[string]string)
	if gatewayInfraAnnotations != "" {
		for _, pair := range strings.Split(gatewayInfraAnnotations, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				infraAnnotations[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	// Parse private annotations
	privateInfraAnnotations := make(map[string]string)
	if privateAnnotations != "" {
		for _, pair := range strings.Split(privateAnnotations, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				privateInfraAnnotations[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	if err = (&controller.IngressReconciler{
		Client:                           mgr.GetClient(),
		Scheme:                           mgr.GetScheme(),
		GatewayNamespace:                 gatewayNamespace,
		GatewayName:                      gatewayName,
		WatchNamespace:                   watchNamespace,
		OneGatewayPerIngress:             oneGatewayPerIngress,
		EnableDeletion:                   enableDeletion,
		HostnameRewriteFrom:              hostnameRewriteFrom,
		HostnameRewriteTo:                hostnameRewriteTo,
		DisableSourceIngress:             disableSourceIngress,
		GatewayAnnotationFilters:         gatewayFilters,
		HTTPRouteAnnotationFilters:       httpRouteFilters,
		DefaultGatewayAnnotations:        gwAnnotations,
		GatewayInfrastructureAnnotations: infraAnnotations,
		PrivateInfrastructureAnnotations: privateInfraAnnotations,
		ApplyPrivateToAll:                private,
		PrivateIngressClassPattern:       privateIngressClassPattern,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ingress")
		os.Exit(1)
	}

	if watchNamespace != "" {
		setupLog.Info("Watching Ingresses in specific namespace only", "namespace", watchNamespace)
	} else {
		setupLog.Info("Watching Ingresses in all namespaces")
	}

	if oneGatewayPerIngress {
		setupLog.Info("Mode: One Gateway per Ingress")
	} else {
		setupLog.Info("Mode: Shared Gateway", "gatewayName", gatewayName)
	}

	if enableDeletion {
		setupLog.Info("Deletion enabled: HTTPRoute and Gateway resources will be deleted when Ingress is deleted")
	} else {
		setupLog.Info("Deletion disabled: HTTPRoute and Gateway resources will remain when Ingress is deleted")
	}

	if hostnameRewriteFrom != "" && hostnameRewriteTo != "" {
		setupLog.Info("Hostname rewriting enabled", "from", hostnameRewriteFrom, "to", hostnameRewriteTo)
	} else if hostnameRewriteFrom != "" || hostnameRewriteTo != "" {
		setupLog.Error(nil, "Both --hostname-rewrite-from and --hostname-rewrite-to must be specified together")
		os.Exit(1)
	}

	if disableSourceIngress {
		setupLog.Info("Source Ingress disabling enabled: ingressClassName will be removed from source Ingress")
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
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
