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

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-operator/internal/translator"
)

const (
	ManagedByAnnotation                = "ingress-operator.fiction.si/managed-by"
	ManagedByValue                     = "ingress-controller"
	IngressClassAnnotation             = "kubernetes.io/ingress.class"
	IngressDisabledAnnotation          = "ingress-operator.fiction.si/disabled"
	IgnoreIngressAnnotation            = "ingress-operator.fiction.si/ignore-ingress"
	OriginalIngressClassAnnotation     = "ingress-operator.fiction.si/original-ingress-class"
	OriginalIngressClassNameAnnotation = "ingress-operator.fiction.si/original-ingress-classname"
	MismatchedCertAnnotation           = "ingress-operator.fiction.si/certificate-mismatch"
	FinalizerName                      = "ingress-operator.fiction.si/finalizer"
	DefaultGatewayAnnotationFilters    = "ingress.kubernetes.io,cert-manager.io," +
		"nginx.ingress.kubernetes.io,kubectl.kubernetes.io,kubernetes.io/ingress.class," +
		"traefik.ingress.kubernetes.io,ingress-operator.fiction.si"
	DefaultHTTPRouteAnnotationFilters = "ingress.kubernetes.io,cert-manager.io," +
		"nginx.ingress.kubernetes.io,kubectl.kubernetes.io,kubernetes.io/ingress.class," +
		"traefik.ingress.kubernetes.io,ingress-operator.fiction.si"
)

type IngressReconciler struct {
	client.Client
	Scheme                           *runtime.Scheme
	GatewayNamespace                 string
	GatewayName                      string
	GatewayClassName                 string
	WatchNamespace                   string
	OneGatewayPerIngress             bool
	EnableDeletion                   bool
	HostnameRewriteFrom              string
	HostnameRewriteTo                string
	DisableSourceIngress             bool
	GatewayAnnotationFilters         []string
	HTTPRouteAnnotationFilters       []string
	DefaultGatewayAnnotations        map[string]string
	GatewayInfrastructureAnnotations map[string]string
	PrivateInfrastructureAnnotations map[string]string
	ApplyPrivateToAll                bool
	PrivateIngressClassPattern       string
	UseIngress2Gateway               bool
	Ingress2GatewayProvider          string
	Ingress2GatewayIngressClass      string
}

// getTranslator creates a translator instance with the reconciler's configuration
func (r *IngressReconciler) getTranslator() *translator.Translator {
	gatewayClassName := r.GatewayClassName
	if gatewayClassName == "" {
		gatewayClassName = "nginx"
	}
	return translator.New(translator.Config{
		GatewayNamespace:                 r.GatewayNamespace,
		GatewayName:                      r.GatewayName,
		GatewayClassName:                 gatewayClassName,
		HostnameRewriteFrom:              r.HostnameRewriteFrom,
		HostnameRewriteTo:                r.HostnameRewriteTo,
		DefaultGatewayAnnotations:        r.DefaultGatewayAnnotations,
		GatewayInfrastructureAnnotations: r.GatewayInfrastructureAnnotations,
		PrivateInfrastructureAnnotations: r.PrivateInfrastructureAnnotations,
		ApplyPrivateToAll:                r.ApplyPrivateToAll,
		PrivateIngressClassPattern:       r.PrivateIngressClassPattern,
		GatewayAnnotationFilters:         r.GatewayAnnotationFilters,
		HTTPRouteAnnotationFilters:       r.HTTPRouteAnnotationFilters,
		UseIngress2Gateway:               r.UseIngress2Gateway,
		Ingress2GatewayProvider:          r.Ingress2GatewayProvider,
		Ingress2GatewayIngressClass:      r.Ingress2GatewayIngressClass,
	})
}

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling Ingress", "namespace", req.Namespace, "name", req.Name)

	// Get the specific Ingress that triggered this reconciliation
	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		if apierrors.IsNotFound(err) {
			// Ingress was deleted - this is normal, no error
			logger.Info("Ingress not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Ingress")
		return ctrl.Result{}, err
	}

	// Check if this Ingress should be ignored
	if ingress.Annotations != nil && ingress.Annotations[IgnoreIngressAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress has ignore annotation, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !ingress.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ingress)
	}

	// Add finalizer if deletion is enabled and not already present
	if r.EnableDeletion && !containsString(ingress.Finalizers, FinalizerName) {
		ingress.Finalizers = append(ingress.Finalizers, FinalizerName)
		if err := r.Update(ctx, &ingress); err != nil {
			logger.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Added finalizer to Ingress")
	}

	// Disable source Ingress if requested
	if r.DisableSourceIngress {
		if err := r.disableIngress(ctx, &ingress); err != nil {
			logger.Error(err, "failed to disable source Ingress")
			return ctrl.Result{}, err
		}
	}

	// List Ingresses (all or filtered by namespace)
	var ingressList networkingv1.IngressList
	listOpts := []client.ListOption{}
	if r.WatchNamespace != "" {
		listOpts = append(listOpts, client.InNamespace(r.WatchNamespace))
	}

	if err := r.List(ctx, &ingressList, listOpts...); err != nil {
		logger.Error(err, "unable to list Ingresses")
		return ctrl.Result{}, err
	}

	if r.OneGatewayPerIngress {
		// Mode: One Gateway per Ingress
		return r.reconcileOneGatewayPerIngress(ctx, ingressList.Items)
	}

	// Mode: Shared Gateways (group by IngressClass)
	return r.reconcileSharedGateways(ctx, ingressList.Items)
}

func (r *IngressReconciler) reconcileOneGatewayPerIngress(
	ctx context.Context,
	ingresses []networkingv1.Ingress,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	trans := r.getTranslator()

	for _, ingress := range ingresses {
		// Use translator to create Gateway and HTTPRoute
		// Override gateway name to match ingress name for one-per-ingress mode
		transConfig := trans.Config
		transConfig.GatewayName = ingress.Name
		singleTrans := translator.New(transConfig)

		gateway, httpRoute, err := singleTrans.Translate(&ingress)
		if err != nil {
			logger.Error(err, "failed to translate Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
			continue
		}

		// Check if we can manage the Gateway resource
		gatewayNN := types.NamespacedName{
			Namespace: gateway.Namespace,
			Name:      gateway.Name,
		}
		existingGateway := &gatewayv1.Gateway{}
		canManageGateway, err := r.canUpdateResource(ctx, existingGateway, gatewayNN)
		if err != nil {
			logger.Error(err, "error checking Gateway resource")
			continue
		}

		if canManageGateway {
			logger.V(3).Info("Creating Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
			if err := r.applyGateway(ctx, gateway, existingGateway); err != nil {
				logger.Error(err, "failed to apply Gateway")
				continue
			}
			logger.Info("Gateway applied successfully", "namespace", gateway.Namespace, "name", gateway.Name)
		}

		// Create HTTPRoute
		if err := r.applyHTTPRouteIfManaged(ctx, httpRoute); err != nil {
			logger.Error(err, "failed to apply HTTPRoute")
			continue
		}
	}

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) reconcileSharedGateways(
	ctx context.Context,
	ingresses []networkingv1.Ingress,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Group Ingresses by IngressClass
	ingressesByClass := r.groupIngressesByClass(ingresses)

	// For each IngressClass, create a Gateway by merging listeners from all ingresses
	for ingressClass, classIngresses := range ingressesByClass {
		gatewayName := r.getGatewayNameForClass(ingressClass)

		// Create a merged Gateway from all ingresses in this class
		gateway := r.synthesizeSharedGateway(classIngresses, gatewayName)

		// Check if we can manage the Gateway resource
		gatewayNN := types.NamespacedName{
			Namespace: gateway.Namespace,
			Name:      gateway.Name,
		}
		existingGateway := &gatewayv1.Gateway{}
		canManageGateway, err := r.canUpdateResource(ctx, existingGateway, gatewayNN)
		if err != nil {
			logger.Error(err, "error checking Gateway resource")
			continue
		}

		if canManageGateway {
			logger.V(3).Info("Reconciling Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
			if err := r.applyGateway(ctx, gateway, existingGateway); err != nil {
				logger.Error(err, "failed to apply Gateway")
				continue
			}
			logger.Info("Gateway applied successfully", "namespace", gateway.Namespace, "name", gateway.Name)
		} else {
			logger.Info("Skipping Gateway synthesis - resource exists and is not managed by us",
				"namespace", gatewayNN.Namespace, "name", gatewayNN.Name)
		}

		// Create HTTPRoutes using translator for each Ingress in this class
		trans := r.getTranslator()
		for _, ingress := range classIngresses {
			// Override gateway name for this class
			transConfig := trans.Config
			transConfig.GatewayName = gatewayName
			classTrans := translator.New(transConfig)

			httpRoute := classTrans.TranslateToHTTPRoute(&ingress)
			if err := r.applyHTTPRouteIfManaged(ctx, httpRoute); err != nil {
				logger.Error(err, "failed to apply HTTPRoute")
				continue
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) handleDeletion(ctx context.Context, ingress *networkingv1.Ingress) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Ingress deletion", "namespace", ingress.Namespace, "name", ingress.Name)

	if !r.EnableDeletion {
		// Deletion is disabled, just remove finalizer
		if containsString(ingress.Finalizers, FinalizerName) {
			ingress.Finalizers = removeString(ingress.Finalizers, FinalizerName)
			if err := r.Update(ctx, ingress); err != nil {
				logger.Error(err, "failed to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		logger.Info("Deletion disabled - HTTPRoute and Gateway will not be deleted")
		return ctrl.Result{}, nil
	}

	// Delete HTTPRoute
	httpRouteName := ingress.Name
	httpRoute := &gatewayv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: ingress.Namespace,
		Name:      httpRouteName,
	}, httpRoute)

	if err == nil {
		// HTTPRoute exists, check if we manage it
		if r.isManagedByUs(httpRoute) {
			logger.Info("Deleting managed HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
			if err := r.Delete(ctx, httpRoute); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete HTTPRoute")
				return ctrl.Result{}, err
			}
		} else {
			logger.Info("HTTPRoute exists but is not managed by us, skipping deletion",
				"namespace", httpRoute.Namespace, "name", httpRoute.Name)
		}
	} else if !apierrors.IsNotFound(err) {
		logger.Error(err, "error checking HTTPRoute")
		return ctrl.Result{}, err
	}

	// Delete Gateway (only in one-gateway-per-ingress mode)
	if r.OneGatewayPerIngress {
		gatewayName := ingress.Name
		gateway := &gatewayv1.Gateway{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: r.GatewayNamespace,
			Name:      gatewayName,
		}, gateway)

		if err == nil {
			// Gateway exists, check if we manage it
			if r.isManagedByUs(gateway) {
				logger.Info("Deleting managed Gateway", "namespace", gateway.Namespace, "name", gateway.Name)
				if err := r.Delete(ctx, gateway); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "failed to delete Gateway")
					return ctrl.Result{}, err
				}
			} else {
				logger.Info("Gateway exists but is not managed by us, skipping deletion",
					"namespace", gateway.Namespace, "name", gateway.Name)
			}
		} else if !apierrors.IsNotFound(err) {
			logger.Error(err, "error checking Gateway")
			return ctrl.Result{}, err
		}
	}
	// Note: In shared gateway mode, we don't delete the Gateway as other Ingresses may be using it

	// Remove finalizer
	if containsString(ingress.Finalizers, FinalizerName) {
		ingress.Finalizers = removeString(ingress.Finalizers, FinalizerName)
		if err := r.Update(ctx, ingress); err != nil {
			logger.Error(err, "failed to remove finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Removed finalizer from Ingress")
	}

	return ctrl.Result{}, nil
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := []string{}
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}

func (r *IngressReconciler) disableIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	logger := log.FromContext(ctx)

	// Check if already disabled
	if ingress.Annotations != nil && ingress.Annotations[IngressDisabledAnnotation] == fmt.Sprintf("%t", true) {
		return nil // Already disabled
	}

	modified := false

	// Save original ingressClassName if it exists
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != fmt.Sprintf("%t", true) {
		if ingress.Annotations == nil {
			ingress.Annotations = make(map[string]string)
		}
		// Save the original value
		if _, exists := ingress.Annotations[OriginalIngressClassNameAnnotation]; !exists {
			ingress.Annotations[OriginalIngressClassNameAnnotation] = *ingress.Spec.IngressClassName
			logger.Info("Saved original ingressClassName", "value", *ingress.Spec.IngressClassName)
		}
		// Remove the ingressClassName
		ingress.Spec.IngressClassName = nil
		modified = true
		logger.Info("Removed spec.ingressClassName to disable Ingress")
	}

	// Save original ingress.class annotation if it exists
	if ingress.Annotations != nil {
		if class, exists := ingress.Annotations[IngressClassAnnotation]; exists && class != "" {
			// Save the original value
			if _, saved := ingress.Annotations[OriginalIngressClassAnnotation]; !saved {
				ingress.Annotations[OriginalIngressClassAnnotation] = class
				logger.Info("Saved original ingress.class annotation", "value", class)
			}
			// Remove the annotation
			delete(ingress.Annotations, IngressClassAnnotation)
			modified = true
			logger.Info("Removed kubernetes.io/ingress.class annotation to disable Ingress")
		}
	}

	// Mark as disabled
	if modified {
		if ingress.Annotations == nil {
			ingress.Annotations = make(map[string]string)
		}
		ingress.Annotations[IngressDisabledAnnotation] = fmt.Sprintf("%t", true)

		if err := r.Update(ctx, ingress); err != nil {
			return fmt.Errorf("failed to update Ingress to disable it: %w", err)
		}
		logger.Info("Successfully disabled source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
	}

	return nil
}

// Helper methods for synthesizeSharedGateway
func (r *IngressReconciler) getIngressClass(ingress *networkingv1.Ingress) string {
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		return *ingress.Spec.IngressClassName
	}
	if ingress.Annotations != nil {
		if class, ok := ingress.Annotations[IngressClassAnnotation]; ok && class != "" {
			return class
		}
	}
	return "default"
}

func (r *IngressReconciler) shouldApplyPrivateAnnotations(ingressClass string) bool {
	if r.ApplyPrivateToAll {
		return true
	}
	if r.PrivateIngressClassPattern == "" {
		return false
	}
	matched, err := filepath.Match(r.PrivateIngressClassPattern, ingressClass)
	if err != nil {
		log.Log.WithName("shouldApplyPrivateAnnotations").Error(
			err, "Invalid pattern", "pattern", r.PrivateIngressClassPattern)
		return false
	}
	return matched
}

func (r *IngressReconciler) transformHostname(hostname string) string {
	if r.HostnameRewriteFrom == "" || r.HostnameRewriteTo == "" {
		return hostname
	}
	if strings.HasSuffix(hostname, r.HostnameRewriteFrom) {
		prefix := strings.TrimSuffix(hostname, r.HostnameRewriteFrom)
		prefix = strings.TrimSuffix(prefix, ".")
		if prefix != "" {
			return prefix + "." + r.HostnameRewriteTo
		}
		return r.HostnameRewriteTo
	}
	return hostname
}

func (r *IngressReconciler) checkCertificateMatch(originalHostname, transformedHostname string,
	tlsHosts []string) bool {

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

func (r *IngressReconciler) filterAnnotations(annotations map[string]string,
	filters []string) map[string]string {
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

func (r *IngressReconciler) groupIngressesByClass(ingresses []networkingv1.Ingress) map[string][]networkingv1.Ingress {
	result := make(map[string][]networkingv1.Ingress)
	for _, ingress := range ingresses {
		class := r.getIngressClass(&ingress)
		result[class] = append(result[class], ingress)
	}
	return result
}

func (r *IngressReconciler) getGatewayNameForClass(ingressClass string) string {
	if ingressClass == "" || ingressClass == "default" {
		return r.GatewayName
	}
	return ingressClass
}

func (r *IngressReconciler) applyHTTPRouteIfManaged(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	httpRouteNN := types.NamespacedName{
		Namespace: httpRoute.Namespace,
		Name:      httpRoute.Name,
	}
	existingHTTPRoute := &gatewayv1.HTTPRoute{}
	canManage, err := r.canUpdateResource(ctx, existingHTTPRoute, httpRouteNN)
	if err != nil {
		return err
	}

	if !canManage {
		logger.Info("Skipping HTTPRoute synthesis - resource exists and is not managed by us",
			"namespace", httpRouteNN.Namespace, "name", httpRouteNN.Name)
		return nil
	}

	logger.V(3).Info("Creating HTTPRoute", "name", httpRoute.Name, "namespace", httpRoute.Namespace)

	// Apply HTTPRoute resource
	if err := r.applyHTTPRoute(ctx, httpRoute, existingHTTPRoute); err != nil {
		return err
	}
	logger.Info("HTTPRoute applied successfully", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
	return nil
}

// synthesizeSharedGateway creates a single Gateway by merging listeners from multiple ingresses
// This is used in shared gateway mode where multiple ingresses share one gateway
func (r *IngressReconciler) synthesizeSharedGateway(ingresses []networkingv1.Ingress,
	gatewayName string) *gatewayv1.Gateway {
	gateway := &gatewayv1.Gateway{}
	gateway.Name = gatewayName
	gateway.Namespace = r.GatewayNamespace
	gateway.Spec.GatewayClassName = "nginx"

	// Merge annotations from all ingresses (with filtering)
	gateway.Annotations = make(map[string]string)
	for _, ingress := range ingresses {
		filtered := r.filterAnnotations(ingress.Annotations, r.GatewayAnnotationFilters)
		for k, v := range filtered {
			gateway.Annotations[k] = v
		}
	}

	// Apply default gateway annotations
	for key, value := range r.DefaultGatewayAnnotations {
		gateway.Annotations[key] = value
	}

	gateway.Annotations[ManagedByAnnotation] = ManagedByValue

	// Check if any ingress in this group should have private annotations
	applyPrivateAnnotations := false
	for _, ingress := range ingresses {
		ingressClass := r.getIngressClass(&ingress)
		if r.shouldApplyPrivateAnnotations(ingressClass) {
			applyPrivateAnnotations = true
			break
		}
	}

	// Add infrastructure annotations if any are configured
	if len(r.GatewayInfrastructureAnnotations) > 0 ||
		(applyPrivateAnnotations && len(r.PrivateInfrastructureAnnotations) > 0) {
		gateway.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
			Annotations: make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue),
		}

		// Apply base infrastructure annotations (applied to all)
		for key, value := range r.GatewayInfrastructureAnnotations {
			gateway.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)] = gatewayv1.AnnotationValue(value)
		}

		// Apply private annotations if any ingress matches conditions
		if applyPrivateAnnotations {
			for key, value := range r.PrivateInfrastructureAnnotations {
				gateway.Spec.Infrastructure.Annotations[gatewayv1.AnnotationKey(key)] = gatewayv1.AnnotationValue(value)
			}
		}
	}

	// Collect unique hostnames with their TLS configurations, namespace, and unique namespaces
	type hostnameInfo struct {
		tlsConfig *networkingv1.IngressTLS
		namespace string
	}
	hostnameMap := make(map[string]hostnameInfo)
	namespacesSet := make(map[string]bool)

	for _, ingress := range ingresses {
		namespacesSet[ingress.Namespace] = true
		for _, rule := range ingress.Spec.Rules {
			if rule.Host != "" {
				// Find TLS config for this host
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
				hostnameMap[rule.Host] = hostnameInfo{
					tlsConfig: tlsConfig,
					namespace: ingress.Namespace,
				}
			}
		}
	}

	// Convert namespaces set to slice for label selector
	namespacesList := make([]string, 0, len(namespacesSet))
	for ns := range namespacesSet {
		namespacesList = append(namespacesList, ns)
	}

	// Create listeners for each unique hostname
	listeners := make([]gatewayv1.Listener, 0, len(hostnameMap))
	certMismatches := make([]string, 0)
	logger := log.Log.WithName("synthesizeGateway")

	for hostname, info := range hostnameMap {
		// Apply hostname transformation for Gateway listener if configured
		transformedHostname := r.transformHostname(hostname)

		listener := gatewayv1.Listener{
			Name:     gatewayv1.SectionName(transformedHostname), // Use transformed hostname as section name
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
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "kubernetes.io/metadata.name",
								Operator: metav1.LabelSelectorOpIn,
								Values:   namespacesList,
							},
						},
					},
				},
			},
		}

		if info.tlsConfig != nil && info.tlsConfig.SecretName != "" {
			secretName := info.tlsConfig.SecretName

			// Check if hostname was transformed and certificate doesn't match
			if hostname != transformedHostname && !r.checkCertificateMatch(hostname, transformedHostname, info.tlsConfig.Hosts) {
				// Generate new secret name for transformed hostname
				// Replace dots with dashes and append -tls suffix
				newSecretName := strings.ReplaceAll(transformedHostname, ".", "-") + "-tls"
				logger.Info("Certificate doesn't match transformed hostname, using new secret name",
					"original", hostname,
					"transformed", transformedHostname,
					"originalSecret", info.tlsConfig.SecretName,
					"newSecret", newSecretName,
					"certHosts", info.tlsConfig.Hosts)
				certMismatches = append(certMismatches,
					fmt.Sprintf("%s->%s: %s->%s", hostname, transformedHostname,
						info.tlsConfig.SecretName, newSecretName))
				secretName = newSecretName
			}

			mode := gatewayv1.TLSModeTerminate
			listener.TLS = &gatewayv1.ListenerTLSConfig{
				Mode: &mode,
				CertificateRefs: []gatewayv1.SecretObjectReference{
					{
						Name:      gatewayv1.ObjectName(secretName),
						Namespace: (*gatewayv1.Namespace)(&info.namespace),
					},
				},
			}
		}

		listeners = append(listeners, listener)
	}

	// Add info annotation if there are certificate changes
	if len(certMismatches) > 0 {
		gateway.Annotations[MismatchedCertAnnotation] = strings.Join(certMismatches, "; ")
	}

	gateway.Spec.Listeners = listeners
	return gateway
}

func (r *IngressReconciler) isManagedByUs(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	return annotations[ManagedByAnnotation] == ManagedByValue
}

func (r *IngressReconciler) canUpdateResource(
	ctx context.Context, obj client.Object, namespacedName types.NamespacedName,
) (bool, error) {
	logger := log.FromContext(ctx)

	// Try to get the existing resource
	err := r.Get(ctx, namespacedName, obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource doesn't exist, we can create it
			return true, nil
		}
		// Some other error occurred
		return false, err
	}

	// Resource exists, check if it's managed by us
	if !r.isManagedByUs(obj) {
		logger.Info("Resource exists but is not managed by us, skipping",
			"resource", obj.GetObjectKind().GroupVersionKind().Kind,
			"namespace", namespacedName.Namespace,
			"name", namespacedName.Name)
		return false, nil
	}

	// Resource exists and is managed by us, we can update it
	return true, nil
}

func (r *IngressReconciler) applyGateway(
	ctx context.Context, desired *gatewayv1.Gateway, existing *gatewayv1.Gateway,
) error {
	logger := log.FromContext(ctx)

	// Check if Gateway exists
	err := r.Get(ctx, types.NamespacedName{
		Namespace: desired.Namespace,
		Name:      desired.Name,
	}, existing)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new Gateway
			logger.Info("Creating Gateway", "namespace", desired.Namespace, "name", desired.Name)
			if err := r.Create(ctx, desired); err != nil {
				return fmt.Errorf("failed to create Gateway: %w", err)
			}
			return nil
		}
		return err
	}

	// Update existing Gateway
	existing.Annotations = desired.Annotations
	existing.Spec = desired.Spec
	logger.Info("Updating Gateway", "namespace", existing.Namespace, "name", existing.Name)
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update Gateway: %w", err)
	}

	return nil
}

func (r *IngressReconciler) applyHTTPRoute(
	ctx context.Context, desired *gatewayv1.HTTPRoute, existing *gatewayv1.HTTPRoute,
) error {
	logger := log.FromContext(ctx)

	// Check if HTTPRoute exists
	err := r.Get(ctx, types.NamespacedName{
		Namespace: desired.Namespace,
		Name:      desired.Name,
	}, existing)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new HTTPRoute
			logger.Info("Creating HTTPRoute", "namespace", desired.Namespace, "name", desired.Name)
			if err := r.Create(ctx, desired); err != nil {
				return fmt.Errorf("failed to create HTTPRoute: %w", err)
			}
			return nil
		}
		return err
	}

	// Update existing HTTPRoute
	existing.Annotations = desired.Annotations
	existing.Spec = desired.Spec
	logger.Info("Updating HTTPRoute", "namespace", existing.Namespace, "name", existing.Name)
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update HTTPRoute: %w", err)
	}

	return nil
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{})

	// If watching specific namespace, add namespace filter
	if r.WatchNamespace != "" {
		builder = builder.WithEventFilter(NamespaceFilter(r.WatchNamespace))
	}

	return builder.Complete(r)
}

func NamespaceFilter(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == namespace
	})
}
