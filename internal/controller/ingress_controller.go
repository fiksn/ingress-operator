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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-operator/internal/metrics"
	"github.com/fiksn/ingress-operator/internal/translator"
	"github.com/fiksn/ingress-operator/internal/utils"
)

const (
	IngressClassAnnotation             = "kubernetes.io/ingress.class"
	IngressDisabledAnnotation          = "ingress-operator.fiction.si/disabled"
	IgnoreIngressAnnotation            = "ingress-operator.fiction.si/ignore-ingress"
	OriginalIngressClassAnnotation     = "ingress-operator.fiction.si/original-ingress-class"
	OriginalIngressClassNameAnnotation = "ingress-operator.fiction.si/original-ingress-classname"
	FinalizerName                      = "ingress-operator.fiction.si/finalizer"
	MaxHTTPRouteRules                  = 16 // Gateway API limit
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
	IngressClassFilter               string
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

	// Check if this Ingress matches the ingress class filter
	if !r.matchesIngressClassFilter(&ingress) {
		ingressClass := r.getIngressClass(&ingress)
		logger.Info("Ingress class does not match filter, skipping reconciliation",
			"ingressClass", ingressClass,
			"filter", r.IngressClassFilter)
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !ingress.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ingress)
	}

	// Add finalizer if deletion is enabled and not already present
	if r.EnableDeletion && !utils.ContainsString(ingress.Finalizers, FinalizerName) {
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
		canManageGateway, err := utils.CanUpdateResource(ctx, r.Client, existingGateway, gatewayNN)
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

		// Resolve any named ports before applying
		if err := r.resolveNamedPorts(ctx, &ingress, httpRoute); err != nil {
			logger.Error(err, "failed to resolve named ports")
			// Continue anyway with fallback ports
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

		// Create a merged Gateway from all ingresses in this class using translator
		trans := r.getTranslator()
		gateway := trans.TranslateMultipleToSharedGateway(classIngresses, gatewayName)

		// Check if we can manage the Gateway resource
		gatewayNN := types.NamespacedName{
			Namespace: gateway.Namespace,
			Name:      gateway.Name,
		}
		existingGateway := &gatewayv1.Gateway{}
		canManageGateway, err := utils.CanUpdateResource(ctx, r.Client, existingGateway, gatewayNN)
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

			// Create ReferenceGrants for cross-namespace secret access
			// Only needed when Gateway is in different namespace than Ingress secrets
			// Skip in ingress2gateway mode as it handles this itself
			if !r.UseIngress2Gateway {
				trans := r.getTranslator()
				recordMetric := func(operation, namespace, name string) {
					metrics.ReferenceGrantResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
				}
				if err := utils.EnsureReferenceGrants(ctx, r.Client, trans, classIngresses, recordMetric); err != nil {
					logger.Error(err, "failed to ensure ReferenceGrants")
					// Don't fail the reconciliation, just log the error
				}
			}
		} else {
			logger.Info("Skipping Gateway synthesis - resource exists and is not managed by us",
				"namespace", gatewayNN.Namespace, "name", gatewayNN.Name)
		}

		// Create HTTPRoutes using translator for each Ingress in this class
		for _, ingress := range classIngresses {
			// Override gateway name for this class
			transConfig := trans.Config
			transConfig.GatewayName = gatewayName
			classTrans := translator.New(transConfig)

			httpRoute := classTrans.TranslateToHTTPRoute(&ingress)

			// Resolve any named ports before applying
			if err := r.resolveNamedPorts(ctx, &ingress, httpRoute); err != nil {
				logger.Error(err, "failed to resolve named ports")
				// Continue anyway with fallback ports
			}

			// Split HTTPRoute if it exceeds the Gateway API limit
			httpRoutes := r.splitHTTPRouteIfNeeded(httpRoute)

			// Apply all HTTPRoute(s)
			for _, hr := range httpRoutes {
				if err := r.applyHTTPRouteIfManaged(ctx, hr); err != nil {
					logger.Error(err, "failed to apply HTTPRoute")
					continue
				}
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
		if utils.ContainsString(ingress.Finalizers, FinalizerName) {
			ingress.Finalizers = utils.RemoveString(ingress.Finalizers, FinalizerName)
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
		if utils.IsManagedByUs(httpRoute) {
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

	// Handle Gateway cleanup
	if r.OneGatewayPerIngress {
		// In one-gateway-per-ingress mode, delete the entire Gateway
		gatewayName := ingress.Name
		gateway := &gatewayv1.Gateway{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: r.GatewayNamespace,
			Name:      gatewayName,
		}, gateway)

		if err == nil {
			// Gateway exists, check if we manage it
			if utils.IsManagedByUs(gateway) {
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
	} else {
		// In shared gateway mode, remove listeners associated with this Ingress
		trans := r.getTranslator()
		ingressClass := trans.GetIngressClass(ingress)
		gatewayName := r.getGatewayNameForClass(ingressClass)

		gateway := &gatewayv1.Gateway{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: r.GatewayNamespace,
			Name:      gatewayName,
		}, gateway)

		if err == nil && utils.IsManagedByUs(gateway) {
			// Remove listeners for hostnames from this Ingress
			trans := r.getTranslator()
			translator.RemoveIngressListeners(gateway, ingress, trans)

			if len(gateway.Spec.Listeners) == 0 {
				// No more listeners, delete the Gateway
				logger.Info("Deleting Gateway with no remaining listeners",
					"namespace", gateway.Namespace, "name", gateway.Name)
				if err := r.Delete(ctx, gateway); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "failed to delete Gateway")
					return ctrl.Result{}, err
				}
			} else {
				// Update Gateway with remaining listeners
				logger.Info("Removing listeners from shared Gateway",
					"namespace", gateway.Namespace, "name", gateway.Name,
					"remainingListeners", len(gateway.Spec.Listeners))
				if err := r.Update(ctx, gateway); err != nil {
					logger.Error(err, "failed to update Gateway")
					return ctrl.Result{}, err
				}
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "error checking Gateway")
			return ctrl.Result{}, err
		}

		// Check if ReferenceGrant should be cleaned up
		// Only if this Ingress had TLS and Gateway is in different namespace
		if !r.UseIngress2Gateway && len(ingress.Spec.TLS) > 0 && ingress.Namespace != r.GatewayNamespace {
			if err := utils.CleanupReferenceGrantIfNeeded(ctx, r.Client, ingress.Namespace); err != nil {
				logger.Error(err, "failed to cleanup ReferenceGrant", "namespace", ingress.Namespace)
				// Don't fail the reconciliation, just log the error
			}
		}
	}

	// Remove finalizer
	if utils.ContainsString(ingress.Finalizers, FinalizerName) {
		ingress.Finalizers = utils.RemoveString(ingress.Finalizers, FinalizerName)
		if err := r.Update(ctx, ingress); err != nil {
			logger.Error(err, "failed to remove finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Removed finalizer from Ingress")
	}

	return ctrl.Result{}, nil
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

func (r *IngressReconciler) groupIngressesByClass(ingresses []networkingv1.Ingress) map[string][]networkingv1.Ingress {
	trans := r.getTranslator()
	result := make(map[string][]networkingv1.Ingress)
	for _, ingress := range ingresses {
		class := trans.GetIngressClass(&ingress)
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

// getIngressClass returns the ingress class from spec.ingressClassName or the legacy annotation
func (r *IngressReconciler) getIngressClass(ingress *networkingv1.Ingress) string {
	// First check spec.ingressClassName
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		return *ingress.Spec.IngressClassName
	}

	// Fallback to legacy annotation
	if ingress.Annotations != nil {
		if class, ok := ingress.Annotations[IngressClassAnnotation]; ok && class != "" {
			return class
		}
	}

	return ""
}

// resolveNamedPorts resolves named ports in HTTPRoute by looking up the actual Services
func (r *IngressReconciler) resolveNamedPorts(ctx context.Context, ingress *networkingv1.Ingress, httpRoute *gatewayv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	for i := range httpRoute.Spec.Rules {
		for j := range httpRoute.Spec.Rules[i].BackendRefs {
			backendRef := &httpRoute.Spec.Rules[i].BackendRefs[j]

			// Check if port is 0 (indicates named port)
			if backendRef.Port != nil && *backendRef.Port == 0 {
				serviceName := string(backendRef.Name)

				// Find the named port from the Ingress spec
				portName := r.findPortNameInIngress(ingress, serviceName)
				if portName == "" {
					logger.Info("Could not find port name in Ingress for service, using fallback port 80",
						"service", serviceName,
						"namespace", ingress.Namespace)
					fallbackPort := int32(80)
					backendRef.Port = &fallbackPort
					continue
				}

				// Look up the Service to resolve the named port
				resolvedPort, err := r.resolveServicePort(ctx, ingress.Namespace, serviceName, portName)
				if err != nil {
					logger.Info("Could not resolve named port from Service, using fallback port 80",
						"service", serviceName,
						"portName", portName,
						"namespace", ingress.Namespace,
						"error", err.Error())
					fallbackPort := int32(80)
					backendRef.Port = &fallbackPort
					continue
				}

				logger.Info("Resolved named port from Service",
					"service", serviceName,
					"portName", portName,
					"resolvedPort", resolvedPort,
					"namespace", ingress.Namespace)
				backendRef.Port = &resolvedPort
			}
		}
	}

	return nil
}

// findPortNameInIngress finds the port name for a given service in the Ingress spec
func (r *IngressReconciler) findPortNameInIngress(ingress *networkingv1.Ingress, serviceName string) string {
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil && path.Backend.Service.Name == serviceName {
					return path.Backend.Service.Port.Name
				}
			}
		}
	}
	return ""
}

// resolveServicePort looks up a Service and resolves a named port to its numeric value
func (r *IngressReconciler) resolveServicePort(ctx context.Context, namespace, serviceName, portName string) (int32, error) {
	var service corev1.Service
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      serviceName,
	}, &service)
	if err != nil {
		return 0, fmt.Errorf("failed to get Service: %w", err)
	}

	// Find the port by name
	for _, port := range service.Spec.Ports {
		if port.Name == portName {
			return port.Port, nil
		}
	}

	return 0, fmt.Errorf("port %s not found in Service %s", portName, serviceName)
}

// matchesIngressClassFilter checks if the Ingress class matches the configured filter pattern
func (r *IngressReconciler) matchesIngressClassFilter(ingress *networkingv1.Ingress) bool {
	// Default filter "*" matches everything
	if r.IngressClassFilter == "" || r.IngressClassFilter == "*" {
		return true
	}

	ingressClass := r.getIngressClass(ingress)

	// Empty ingress class matches empty filter or "*"
	if ingressClass == "" {
		return r.IngressClassFilter == "" || r.IngressClassFilter == "*"
	}

	// Use filepath.Match for glob pattern matching
	matched, err := filepath.Match(r.IngressClassFilter, ingressClass)
	if err != nil {
		// If pattern is invalid, log error and don't match
		log.Log.Error(err, "Invalid ingress class filter pattern", "pattern", r.IngressClassFilter)
		return false
	}

	return matched
}

// splitHTTPRouteIfNeeded splits an HTTPRoute into multiple routes if it exceeds the Gateway API limit
func (r *IngressReconciler) splitHTTPRouteIfNeeded(httpRoute *gatewayv1.HTTPRoute) []*gatewayv1.HTTPRoute {
	// If the HTTPRoute has <= MaxHTTPRouteRules, no split needed
	if len(httpRoute.Spec.Rules) <= MaxHTTPRouteRules {
		return []*gatewayv1.HTTPRoute{httpRoute}
	}

	logger := log.Log.WithName("splitHTTPRouteIfNeeded")
	logger.Info("HTTPRoute exceeds max rules, splitting",
		"name", httpRoute.Name,
		"namespace", httpRoute.Namespace,
		"totalRules", len(httpRoute.Spec.Rules),
		"maxRules", MaxHTTPRouteRules)

	// Split into multiple HTTPRoutes
	var result []*gatewayv1.HTTPRoute
	rules := httpRoute.Spec.Rules
	partNum := 1

	for i := 0; i < len(rules); i += MaxHTTPRouteRules {
		end := i + MaxHTTPRouteRules
		if end > len(rules) {
			end = len(rules)
		}

		// Create a copy of the HTTPRoute for this chunk
		part := httpRoute.DeepCopy()
		part.Spec.Rules = rules[i:end]

		// Update the name with a suffix (except for the first part to maintain compatibility)
		if partNum > 1 {
			part.Name = fmt.Sprintf("%s-part-%d", httpRoute.Name, partNum)
		}

		logger.Info("Created HTTPRoute part",
			"originalName", httpRoute.Name,
			"partName", part.Name,
			"partNum", partNum,
			"rulesInPart", len(part.Spec.Rules))

		result = append(result, part)
		partNum++
	}

	return result
}

func (r *IngressReconciler) applyHTTPRouteIfManaged(ctx context.Context, httpRoute *gatewayv1.HTTPRoute) error {
	logger := log.FromContext(ctx)

	httpRouteNN := types.NamespacedName{
		Namespace: httpRoute.Namespace,
		Name:      httpRoute.Name,
	}
	existingHTTPRoute := &gatewayv1.HTTPRoute{}
	canManage, err := utils.CanUpdateResource(ctx, r.Client, existingHTTPRoute, httpRouteNN)
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
			// Record metric for Gateway creation
			metrics.GatewayResourcesTotal.WithLabelValues("create", desired.Namespace, desired.Name).Inc()
			return nil
		}
		return err
	}

	// Merge with existing Gateway instead of replacing
	// This allows multiple operator instances watching different namespaces to share the same Gateway
	translator.MergeGatewaySpec(existing, desired)

	logger.Info("Updating Gateway", "namespace", existing.Namespace, "name", existing.Name)
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update Gateway: %w", err)
	}
	// Record metric for Gateway update
	metrics.GatewayResourcesTotal.WithLabelValues("update", existing.Namespace, existing.Name).Inc()

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
			// Record metric for HTTPRoute creation
			metrics.HTTPRouteResourcesTotal.WithLabelValues("create", desired.Namespace, desired.Name).Inc()
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
	// Record metric for HTTPRoute update
	metrics.HTTPRouteResourcesTotal.WithLabelValues("update", existing.Namespace, existing.Name).Inc()

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
