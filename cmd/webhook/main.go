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
	"flag"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	_ "github.com/fiksn/ingress-doperator/internal/metrics" // Import to register metrics
	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
	webhookhandler "github.com/fiksn/ingress-doperator/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var webhookPort int
	var certDir string
	var gatewayNamespace string
	var gatewayName string
	var gatewayClassName string
	var hostnameRewriteFrom string
	var hostnameRewriteTo string
	var gatewayAnnotations string
	var gatewayAnnotationFilters string
	var httpRouteAnnotationFilters string
	var ingressClassSnippetsFilters string
	var ingressNameSnippetsFilters string
	var ingressAnnotationSnippetsAdd string
	var ingressAnnotationSnippetsRemove string
	var useIngress2Gateway bool
	var ingressClassFilter string
	var verbosity int

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the webhook server binds to.")
	flag.StringVar(&certDir, "cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"The directory containing the webhook TLS certificates.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "default",
		"The namespace where Gateway resources will be created")
	flag.StringVar(&gatewayName, "gateway-name", "ingress-gateway",
		"The name of the Gateway resource")
	flag.StringVar(&gatewayClassName, "gateway-class-name", "nginx",
		"The GatewayClass to use for created Gateway resources")
	flag.StringVar(&hostnameRewriteFrom, "hostname-rewrite-from", "",
		"Comma-separated list of domain suffixes to match for rewriting")
	flag.StringVar(&hostnameRewriteTo, "hostname-rewrite-to", "",
		"Comma-separated list of replacement domain suffixes (must match count of --hostname-rewrite-from)")
	flag.StringVar(&gatewayAnnotations, "gateway-annotations", "",
		"Comma-separated key=value pairs for Gateway metadata annotations")
	flag.StringVar(&ingressClassSnippetsFilters, "ingress-class-snippets-filter", "",
		"Comma-separated list of pattern:snippetsFilterName entries. "+
			"If ingress class matches the glob, the SnippetsFilter is copied from the Gateway namespace and attached.")
	flag.StringVar(&ingressNameSnippetsFilters, "ingress-name-snippets-filter", "",
		"Comma-separated list of pattern:snippetsFilterName entries. "+
			"If ingress name matches the glob, the SnippetsFilter is copied from the Gateway namespace and attached.")
	flag.StringVar(&ingressAnnotationSnippetsAdd, "ingress-annotation-snippets-add", "",
		"Semicolon-separated list of key=value:filter1,filter2 entries. "+
			"If annotation value matches glob, add SnippetsFilter(s).")
	flag.StringVar(&ingressAnnotationSnippetsRemove, "ingress-annotation-snippets-remove", "",
		"Semicolon-separated list of key=value:filter1,filter2 entries. "+
			"If annotation value matches glob, remove SnippetsFilter(s).")
	flag.StringVar(&gatewayAnnotationFilters, "gateway-annotation-filters",
		"ingress.kubernetes.io,cert-manager.io,nginx.ingress.kubernetes.io,"+
			"kubectl.kubernetes.io,kubernetes.io/ingress.class,ingress-doperator.fiction.si",
		"Comma-separated list of annotation prefixes to exclude from Gateway resources")
	flag.StringVar(&httpRouteAnnotationFilters, "httproute-annotation-filters",
		"ingress.kubernetes.io,cert-manager.io,nginx.ingress.kubernetes.io,"+
			"kubectl.kubernetes.io,kubernetes.io/ingress.class,ingress-doperator.fiction.si",
		"Comma-separated list of annotation prefixes to exclude from HTTPRoute resources")
	flag.BoolVar(&useIngress2Gateway, "use-ingress2gateway", false,
		"If true, use the ingress2gateway library for translation (disables hostname/certificate mangling)")
	var ingress2GatewayProvider string
	var ingress2GatewayIngressClass string
	flag.StringVar(&ingress2GatewayProvider, "ingress2gateway-provider", "ingress-nginx",
		"Provider to use with ingress2gateway (e.g., ingress-nginx, istio, kong)")
	flag.StringVar(&ingress2GatewayIngressClass, "ingress2gateway-ingress-class", "nginx",
		"Ingress class name for provider-specific filtering in ingress2gateway")
	flag.StringVar(&ingressClassFilter, "ingress-class-filter", "*",
		"Glob pattern to filter which ingress classes to process (e.g., '*private*', 'nginx', '*'). "+
			"Default '*' processes all classes.")
	flag.IntVar(&verbosity, "v", 0, "Log verbosity (0 = info, higher = more verbose)")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if verbosity > 0 {
		opts.Development = false
		opts.Level = zapcore.Level(-verbosity)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	parsedSnippetsFilters, err := utils.ParseIngressClassSnippetsFilters(ingressClassSnippetsFilters)
	if err != nil {
		setupLog.Error(err, "Invalid ingress-class-snippets-filter value")
		os.Exit(1)
	}
	parsedNameSnippetsFilters, err := utils.ParseIngressClassSnippetsFilters(ingressNameSnippetsFilters)
	if err != nil {
		setupLog.Error(err, "Invalid ingress-name-snippets-filter value")
		os.Exit(1)
	}
	parsedAnnotationAddRules, err := utils.ParseIngressAnnotationSnippetsRules(ingressAnnotationSnippetsAdd)
	if err != nil {
		setupLog.Error(err, "Invalid ingress-annotation-snippets-add value")
		os.Exit(1)
	}
	parsedAnnotationRemoveRules, err := utils.ParseIngressAnnotationSnippetsRules(ingressAnnotationSnippetsRemove)
	if err != nil {
		setupLog.Error(err, "Invalid ingress-annotation-snippets-remove value")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: certDir,
		}),
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start webhook")
		os.Exit(1)
	}

	for _, mapping := range parsedSnippetsFilters {
		if err := utils.ValidateSnippetsFilterExists(
			context.Background(),
			mgr.GetAPIReader(),
			gatewayNamespace,
			mapping.Name,
		); err != nil {
			setupLog.Error(err, "SnippetsFilter not found in gateway namespace",
				"name", mapping.Name,
				"namespace", gatewayNamespace)
			os.Exit(1)
		}
	}

	// Parse gateway annotations
	gwAnnotations := make(map[string]string)
	if gatewayAnnotations != "" {
		for _, pair := range strings.Split(gatewayAnnotations, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				gwAnnotations[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	// Parse annotation filters
	var gatewayFilters []string
	if gatewayAnnotationFilters != "" {
		gatewayFilters = strings.Split(gatewayAnnotationFilters, ",")
	}
	var httpRouteFilters []string
	if httpRouteAnnotationFilters != "" {
		httpRouteFilters = strings.Split(httpRouteAnnotationFilters, ",")
	}

	// Create translator
	translatorConfig := translator.Config{
		GatewayNamespace:            gatewayNamespace,
		GatewayName:                 gatewayName,
		GatewayClassName:            gatewayClassName,
		HostnameRewriteFrom:         hostnameRewriteFrom,
		HostnameRewriteTo:           hostnameRewriteTo,
		DefaultGatewayAnnotations:   gwAnnotations,
		GatewayAnnotationFilters:    gatewayFilters,
		HTTPRouteAnnotationFilters:  httpRouteFilters,
		UseIngress2Gateway:          useIngress2Gateway,
		Ingress2GatewayProvider:     ingress2GatewayProvider,
		Ingress2GatewayIngressClass: ingress2GatewayIngressClass,
	}
	trans := translator.New(translatorConfig)

	if useIngress2Gateway {
		setupLog.Info("Using ingress2gateway library for translation",
			"provider", ingress2GatewayProvider,
			"ingressClass", ingress2GatewayIngressClass)
	} else {
		setupLog.Info("Using built-in translation logic")
	}

	// Register webhook
	mutator := &webhookhandler.IngressMutator{
		Client:                          mgr.GetClient(),
		Scheme:                          mgr.GetScheme(),
		Translator:                      trans,
		IngressClassFilter:              ingressClassFilter,
		IngressClassSnippetsFilters:     parsedSnippetsFilters,
		IngressNameSnippetsFilters:      parsedNameSnippetsFilters,
		IngressAnnotationSnippetsAdd:    parsedAnnotationAddRules,
		IngressAnnotationSnippetsRemove: parsedAnnotationRemoveRules,
		HTTPRouteManager: &utils.HTTPRouteManager{
			Client: mgr.GetClient(),
		},
		Recorder: mgr.GetEventRecorderFor("ingress-doperator-webhook"),
	}

	mgr.GetWebhookServer().Register("/mutate-v1-ingress",
		&webhook.Admission{Handler: mutator})

	// Setup decoder
	decoder := admission.NewDecoder(mgr.GetScheme())
	if err := mutator.InjectDecoder(&decoder); err != nil {
		setupLog.Error(err, "unable to inject decoder")
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

	setupLog.Info("starting webhook server")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running webhook")
		os.Exit(1)
	}
}
