/*
Copyright Gregor Pogacnik 2026.

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

package translator

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw"
	"github.com/kubernetes-sigs/ingress2gateway/pkg/i2gw/providers/ingressnginx"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	ManagedByAnnotation      = "ingress-operator.fiction.si/managed-by"
	ManagedByValue           = "ingress-controller"
	MismatchedCertAnnotation = "ingress-operator.fiction.si/certificate-mismatch"
	IngressClassAnnotation   = "kubernetes.io/ingress.class"
)

// Config holds configuration for the translator
type Config struct {
	GatewayNamespace                 string
	GatewayName                      string
	GatewayClassName                 string
	HostnameRewriteFrom              string
	HostnameRewriteTo                string
	DefaultGatewayAnnotations        map[string]string
	GatewayInfrastructureAnnotations map[string]string
	PrivateInfrastructureAnnotations map[string]string
	ApplyPrivateToAll                bool
	PrivateIngressClassPattern       string
	GatewayAnnotationFilters         []string
	HTTPRouteAnnotationFilters       []string
	UseIngress2Gateway               bool
	Ingress2GatewayProvider          string
	Ingress2GatewayIngressClass      string
}

// Translator handles the conversion from Ingress to Gateway API resources
type Translator struct {
	Config Config
}

// New creates a new Translator with the given configuration
func New(cfg Config) *Translator {
	return &Translator{Config: cfg}
}

// Translate converts an Ingress to Gateway API resources using the configured method
func (t *Translator) Translate(
	ingress *networkingv1.Ingress,
) (*gatewayv1.Gateway, *gatewayv1.HTTPRoute, error) {
	if t.Config.UseIngress2Gateway {
		return t.translateWithIngress2Gateway(ingress)
	}
	// Use built-in translation logic
	gateway := t.TranslateToGateway(ingress)
	httpRoute := t.TranslateToHTTPRoute(ingress)
	return gateway, httpRoute, nil
}

// translateWithIngress2Gateway uses the ingress2gateway library for translation
// and applies annotation copying on top. NO hostname/certificate mangling in this mode.
func (t *Translator) translateWithIngress2Gateway(
	ingress *networkingv1.Ingress,
) (*gatewayv1.Gateway, *gatewayv1.HTTPRoute, error) {
	logger := log.Log.WithName("translator")
	ctx := context.Background()

	// Create a scheme with the types we need
	scheme := runtime.NewScheme()
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	gatewayv1.Install(scheme)

	// Create a fake client with our Ingress and mock Services
	// We need to create Service objects for the backends referenced by the Ingress
	services := t.extractServicesFromIngress(ingress)
	objects := []runtime.Object{ingress}
	for i := range services {
		objects = append(objects, &services[i])
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		Build()

	// Build provider-specific flags
	providerFlags := make(map[string]map[string]string)
	if t.Config.Ingress2GatewayProvider != "" {
		providerFlags[t.Config.Ingress2GatewayProvider] = make(map[string]string)
		if t.Config.Ingress2GatewayProvider == ingressnginx.Name && t.Config.Ingress2GatewayIngressClass != "" {
			providerFlags[ingressnginx.Name][ingressnginx.NginxIngressClassFlag] = t.Config.Ingress2GatewayIngressClass
		}
	}

	// Create provider configuration
	providerConf := &i2gw.ProviderConf{
		Client:                fakeClient,
		Namespace:             ingress.Namespace,
		ProviderSpecificFlags: providerFlags,
	}

	// Select the appropriate provider
	var provider i2gw.Provider
	switch t.Config.Ingress2GatewayProvider {
	case ingressnginx.Name, "":
		provider = ingressnginx.NewProvider(providerConf)
	default:
		return nil, nil, fmt.Errorf("unsupported ingress2gateway provider: %s", t.Config.Ingress2GatewayProvider)
	}

	// Read resources from the fake client
	if err := provider.ReadResourcesFromCluster(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to read resources: %w", err)
	}

	// Convert to intermediate representation
	ir, errs := provider.ToIR()
	if len(errs) > 0 {
		logger.Info("ingress2gateway conversion had errors", "errors", errs)
	}

	// Convert IR to Gateway API resources
	gwResources, errs := provider.ToGatewayResources(ir)
	if len(errs) > 0 {
		logger.Info("ingress2gateway Gateway API conversion had errors", "errors", errs)
	}

	// Extract Gateway and HTTPRoute for this specific ingress
	var gateway *gatewayv1.Gateway
	var httpRoute *gatewayv1.HTTPRoute

	ingressKey := types.NamespacedName{Namespace: ingress.Namespace, Name: ingress.Name}

	// Find the HTTPRoute matching our ingress
	if hr, ok := gwResources.HTTPRoutes[ingressKey]; ok {
		httpRoute = &hr
	}

	// ingress2gateway may create a single shared Gateway - grab the first one
	for _, gw := range gwResources.Gateways {
		gateway = &gw
		break // Take the first gateway
	}

	if gateway == nil || httpRoute == nil {
		return nil, nil, fmt.Errorf("ingress2gateway did not produce expected Gateway/HTTPRoute resources")
	}

	// Apply annotation copying (but NOT hostname/certificate mangling!)
	if gateway.Annotations == nil {
		gateway.Annotations = make(map[string]string)
	}
	filteredGwAnnotations := t.filterAnnotations(ingress.Annotations, t.Config.GatewayAnnotationFilters)
	for k, v := range filteredGwAnnotations {
		gateway.Annotations[k] = v
	}

	// Apply default gateway annotations
	for k, v := range t.Config.DefaultGatewayAnnotations {
		gateway.Annotations[k] = v
	}
	gateway.Annotations[ManagedByAnnotation] = ManagedByValue

	// Override gateway name/namespace/class to our configured values
	gateway.Name = t.Config.GatewayName
	gateway.Namespace = t.Config.GatewayNamespace
	if t.Config.GatewayClassName != "" {
		gateway.Spec.GatewayClassName = gatewayv1.ObjectName(t.Config.GatewayClassName)
	}

	// Apply infrastructure annotations even in ingress2gateway mode
	ingressClass := t.getIngressClass(ingress)
	applyPrivateAnnotations := t.shouldApplyPrivateAnnotations(ingressClass)

	if len(t.Config.GatewayInfrastructureAnnotations) > 0 ||
		(applyPrivateAnnotations && len(t.Config.PrivateInfrastructureAnnotations) > 0) {
		if gateway.Spec.Infrastructure == nil {
			gateway.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
				Annotations: make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue),
			}
		}
		if gateway.Spec.Infrastructure.Annotations == nil {
			gateway.Spec.Infrastructure.Annotations = make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue)
		}

		// Apply base infrastructure annotations (applied to all)
		for key, value := range t.Config.GatewayInfrastructureAnnotations {
			gateway.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)] = gatewayv1.AnnotationValue(value)
		}

		// Apply private annotations if conditions are met
		if applyPrivateAnnotations {
			for key, value := range t.Config.PrivateInfrastructureAnnotations {
				gateway.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)] = gatewayv1.AnnotationValue(value)
			}
		}
	}

	if httpRoute.Annotations == nil {
		httpRoute.Annotations = make(map[string]string)
	}
	filteredHrAnnotations := t.filterAnnotations(ingress.Annotations, t.Config.HTTPRouteAnnotationFilters)
	for k, v := range filteredHrAnnotations {
		httpRoute.Annotations[k] = v
	}
	httpRoute.Annotations[ManagedByAnnotation] = ManagedByValue

	// Update parent refs to point to our configured gateway
	for i := range httpRoute.Spec.ParentRefs {
		httpRoute.Spec.ParentRefs[i].Name = gatewayv1.ObjectName(t.Config.GatewayName)
		httpRoute.Spec.ParentRefs[i].Namespace = (*gatewayv1.Namespace)(&t.Config.GatewayNamespace)
	}

	return gateway, httpRoute, nil
}

// extractServicesFromIngress creates mock Service objects for backends referenced in the Ingress
func (t *Translator) extractServicesFromIngress(ingress *networkingv1.Ingress) []corev1.Service {
	serviceMap := make(map[string]*corev1.Service)

	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil {
					svcName := path.Backend.Service.Name
					if _, exists := serviceMap[svcName]; !exists {
						// Create a minimal Service object
						svc := &corev1.Service{
							ObjectMeta: metav1.ObjectMeta{
								Name:      svcName,
								Namespace: ingress.Namespace,
							},
							Spec: corev1.ServiceSpec{
								Ports: []corev1.ServicePort{
									{
										Port: path.Backend.Service.Port.Number,
										Name: fmt.Sprintf("port-%d", path.Backend.Service.Port.Number),
									},
								},
							},
						}
						serviceMap[svcName] = svc
					}
				}
			}
		}
	}

	services := make([]corev1.Service, 0, len(serviceMap))
	for _, svc := range serviceMap {
		services = append(services, *svc)
	}
	return services
}

// TranslateToGateway converts an Ingress to a Gateway resource
func (t *Translator) TranslateToGateway(ingress *networkingv1.Ingress) *gatewayv1.Gateway {
	gateway := &gatewayv1.Gateway{}
	gateway.Name = t.Config.GatewayName
	gateway.Namespace = t.Config.GatewayNamespace

	// Set gateway class name
	gatewayClassName := t.Config.GatewayClassName
	if gatewayClassName == "" {
		gatewayClassName = "nginx"
	}
	gateway.Spec.GatewayClassName = gatewayv1.ObjectName(gatewayClassName)

	// Copy and filter annotations from Ingress
	gateway.Annotations = t.filterAnnotations(ingress.Annotations, t.Config.GatewayAnnotationFilters)
	if gateway.Annotations == nil {
		gateway.Annotations = make(map[string]string)
	}

	// Apply default gateway annotations
	for key, value := range t.Config.DefaultGatewayAnnotations {
		gateway.Annotations[key] = value
	}

	gateway.Annotations[ManagedByAnnotation] = ManagedByValue

	// Determine if private annotations should be applied
	ingressClass := t.getIngressClass(ingress)
	applyPrivateAnnotations := t.shouldApplyPrivateAnnotations(ingressClass)

	// Add infrastructure annotations if any are configured
	if len(t.Config.GatewayInfrastructureAnnotations) > 0 ||
		(applyPrivateAnnotations && len(t.Config.PrivateInfrastructureAnnotations) > 0) {
		gateway.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
			Annotations: make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue),
		}

		// Apply base infrastructure annotations (applied to all)
		for key, value := range t.Config.GatewayInfrastructureAnnotations {
			gateway.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)] = gatewayv1.AnnotationValue(value)
		}

		// Apply private annotations if conditions are met
		if applyPrivateAnnotations {
			for key, value := range t.Config.PrivateInfrastructureAnnotations {
				gateway.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)] = gatewayv1.AnnotationValue(value)
			}
		}
	}

	// Collect hostnames with their TLS configurations
	hostnameMap := make(map[string]*networkingv1.IngressTLS)
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			var tlsConfig *networkingv1.IngressTLS
			for _, tls := range ingress.Spec.TLS {
				for _, host := range tls.Hosts {
					if host == rule.Host {
						tlsConfig = &tls
						break
					}
				}
				if tlsConfig != nil {
					break
				}
			}
			hostnameMap[rule.Host] = tlsConfig
		}
	}

	// Create listeners for each unique hostname
	listeners := make([]gatewayv1.Listener, 0, len(hostnameMap))
	certMismatches := make([]string, 0)
	logger := log.Log.WithName("translator")

	for hostname, tlsConfig := range hostnameMap {
		transformedHostname := t.transformHostname(hostname)

		listener := gatewayv1.Listener{
			Name:     gatewayv1.SectionName(transformedHostname),
			Hostname: (*gatewayv1.Hostname)(&transformedHostname),
			Port:     gatewayv1.PortNumber(443),
			Protocol: gatewayv1.HTTPSProtocolType,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: func() *gatewayv1.FromNamespaces {
						from := gatewayv1.NamespacesFromSelector
						return &from
					}(),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": ingress.Namespace,
						},
					},
				},
			},
		}

		if tlsConfig != nil && tlsConfig.SecretName != "" {
			secretName := tlsConfig.SecretName

			if hostname != transformedHostname &&
				!t.checkCertificateMatch(hostname, transformedHostname, tlsConfig.Hosts) {
				newSecretName := strings.ReplaceAll(transformedHostname, ".", "-") + "-tls"
				logger.Info("Certificate doesn't match transformed hostname, using new secret name",
					"original", hostname,
					"transformed", transformedHostname,
					"originalSecret", tlsConfig.SecretName,
					"newSecret", newSecretName,
					"certHosts", tlsConfig.Hosts)
				certMismatches = append(certMismatches,
					fmt.Sprintf("%s->%s: %s->%s", hostname, transformedHostname,
						tlsConfig.SecretName, newSecretName))
				secretName = newSecretName
			}

			mode := gatewayv1.TLSModeTerminate
			listener.TLS = &gatewayv1.ListenerTLSConfig{
				Mode: &mode,
				CertificateRefs: []gatewayv1.SecretObjectReference{
					{
						Name:      gatewayv1.ObjectName(secretName),
						Namespace: (*gatewayv1.Namespace)(&ingress.Namespace),
					},
				},
			}
		}

		listeners = append(listeners, listener)
	}

	if len(certMismatches) > 0 {
		gateway.Annotations[MismatchedCertAnnotation] = strings.Join(certMismatches, "; ")
	}

	gateway.Spec.Listeners = listeners
	return gateway
}

// TranslateToHTTPRoute converts an Ingress to an HTTPRoute resource
func (t *Translator) TranslateToHTTPRoute(ingress *networkingv1.Ingress) *gatewayv1.HTTPRoute {
	logger := log.Log.WithName("translator")
	httpRoute := &gatewayv1.HTTPRoute{}
	httpRoute.Name = ingress.Name
	httpRoute.Namespace = ingress.Namespace

	httpRoute.Annotations = t.filterAnnotations(ingress.Annotations, t.Config.HTTPRouteAnnotationFilters)
	if httpRoute.Annotations == nil {
		httpRoute.Annotations = make(map[string]string)
	}
	httpRoute.Annotations[ManagedByAnnotation] = ManagedByValue

	// Collect hostnames from ingress and apply transformation
	hostnames := make([]gatewayv1.Hostname, 0)
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			transformedHostname := t.transformHostname(rule.Host)
			hostnames = append(hostnames, gatewayv1.Hostname(transformedHostname))
		}
	}
	httpRoute.Spec.Hostnames = hostnames

	// Create parent refs to the Gateway
	parentRefs := make([]gatewayv1.ParentReference, 0, len(hostnames))
	for _, hostname := range hostnames {
		sectionName := gatewayv1.SectionName(hostname)
		parentRef := gatewayv1.ParentReference{
			Name:        gatewayv1.ObjectName(t.Config.GatewayName),
			Namespace:   (*gatewayv1.Namespace)(&t.Config.GatewayNamespace),
			SectionName: &sectionName,
		}
		parentRefs = append(parentRefs, parentRef)
	}
	httpRoute.Spec.ParentRefs = parentRefs

	// Convert Ingress rules to HTTPRoute rules
	var rules []gatewayv1.HTTPRouteRule
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				var backendRefs []gatewayv1.HTTPBackendRef

				if path.Backend.Service != nil {
					port := path.Backend.Service.Port.Number
					backendRef := gatewayv1.HTTPBackendRef{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: gatewayv1.ObjectName(path.Backend.Service.Name),
								Port: &port,
							},
						},
					}
					backendRefs = append(backendRefs, backendRef)
				}

				var matches []gatewayv1.HTTPRouteMatch
				if path.Path != "" {
					pathMatchType := gatewayv1.PathMatchPathPrefix
					if path.PathType != nil {
						switch *path.PathType {
						case networkingv1.PathTypeExact:
							pathMatchType = gatewayv1.PathMatchExact
						case networkingv1.PathTypeImplementationSpecific:
							logger.Info("Converting PathType ImplementationSpecific to PathPrefix",
								"ingress", ingress.Name,
								"namespace", ingress.Namespace,
								"path", path.Path)
							pathMatchType = gatewayv1.PathMatchPathPrefix
						case networkingv1.PathTypePrefix:
							pathMatchType = gatewayv1.PathMatchPathPrefix
						}
					}
					match := gatewayv1.HTTPRouteMatch{
						Path: &gatewayv1.HTTPPathMatch{
							Type:  &pathMatchType,
							Value: &path.Path,
						},
					}
					matches = append(matches, match)
				}

				httpRouteRule := gatewayv1.HTTPRouteRule{
					Matches:     matches,
					BackendRefs: backendRefs,
				}
				rules = append(rules, httpRouteRule)
			}
		}
	}
	httpRoute.Spec.Rules = rules

	return httpRoute
}

func isValidHostname(hostname string) bool {
	// simple regex: labels separated by dots, each label 1-63 chars, letters/digits/hyphen, no empty labels
	pattern := `^([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)*([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?)$`
	match, _ := regexp.MatchString(pattern, hostname)
	return match
}

func (t *Translator) transformHostname(hostname string) string {
	from := t.Config.HostnameRewriteFrom
	to := t.Config.HostnameRewriteTo

	if from == "" || to == "" {
		return hostname
	}

	if strings.HasSuffix(hostname, from) {
		prefix := strings.TrimSuffix(hostname, from)
		prefix = strings.TrimSuffix(prefix, ".")

		var newHost string
		if prefix != "" {
			newHost = prefix + "." + to
		} else {
			newHost = to
		}

		newHost = strings.ReplaceAll(newHost, "..", ".")
		newHost = strings.TrimSuffix(newHost, ".")

		if isValidHostname(newHost) {
			return newHost
		}

		return hostname
	}

	return hostname
}

func (t *Translator) checkCertificateMatch(
	originalHostname, transformedHostname string, tlsHosts []string,
) bool {
	if originalHostname == transformedHostname {
		return true
	}

	for _, host := range tlsHosts {
		if host == transformedHostname {
			return true
		}
		if strings.HasPrefix(host, "*.") {
			wildcard := host[2:]
			if strings.HasSuffix(transformedHostname, wildcard) {
				return true
			}
		}
	}

	return false
}

func (t *Translator) filterAnnotations(annotations map[string]string, filters []string) map[string]string {
	if annotations == nil {
		return nil
	}
	filtered := make(map[string]string)
	for key, value := range annotations {
		shouldInclude := true
		for _, filter := range filters {
			if strings.HasPrefix(key, filter) {
				shouldInclude = false
				break
			}
		}
		if shouldInclude {
			filtered[key] = value
		}
	}
	return filtered
}

func (t *Translator) getIngressClass(ingress *networkingv1.Ingress) string {
	// Check spec.ingressClassName first
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		return *ingress.Spec.IngressClassName
	}
	// Fall back to annotation
	if ingress.Annotations != nil {
		if class, ok := ingress.Annotations[IngressClassAnnotation]; ok && class != "" {
			return class
		}
	}
	// Default class
	return "default"
}

func (t *Translator) shouldApplyPrivateAnnotations(ingressClass string) bool {
	// Apply to all if flag is set
	if t.Config.ApplyPrivateToAll {
		return true
	}
	// Apply if ingress class matches pattern
	if t.Config.PrivateIngressClassPattern == "" {
		return false
	}
	matched, err := filepath.Match(t.Config.PrivateIngressClassPattern, ingressClass)
	if err != nil {
		// Invalid pattern, log and return false
		logger := log.Log.WithName("shouldApplyPrivateAnnotations")
		logger.Error(err, "Invalid pattern", "pattern", t.Config.PrivateIngressClassPattern)
		return false
	}
	return matched
}
