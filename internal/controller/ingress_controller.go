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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

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

		// Create a merged Gateway from all ingresses in this class using translator
		trans := r.getTranslator()
		gateway := trans.TranslateMultipleToSharedGateway(classIngresses, gatewayName)

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

			// Create ReferenceGrants for cross-namespace secret access
			// Only needed when Gateway is in different namespace than Ingress secrets
			// Skip in ingress2gateway mode as it handles this itself
			if !r.UseIngress2Gateway {
				if err := r.ensureReferenceGrants(ctx, classIngresses); err != nil {
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

		if err == nil && r.isManagedByUs(gateway) {
			// Remove listeners for hostnames from this Ingress
			r.removeIngressListeners(gateway, ingress)

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
			if err := r.cleanupReferenceGrantIfNeeded(ctx, ingress.Namespace); err != nil {
				logger.Error(err, "failed to cleanup ReferenceGrant", "namespace", ingress.Namespace)
				// Don't fail the reconciliation, just log the error
			}
		}
	}

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

	// Merge with existing Gateway instead of replacing
	// This allows multiple operator instances watching different namespaces to share the same Gateway
	r.mergeGatewaySpec(existing, desired)

	logger.Info("Updating Gateway", "namespace", existing.Namespace, "name", existing.Name)
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update Gateway: %w", err)
	}

	return nil
}

// removeIngressListeners removes listeners from the Gateway that correspond to hostnames in the Ingress
// Also cleans up certificate-mismatch annotation entries for removed hostnames
func (r *IngressReconciler) removeIngressListeners(gateway *gatewayv1.Gateway, ingress *networkingv1.Ingress) {
	trans := r.getTranslator()

	// Build set of hostnames from this Ingress (with transformation applied)
	hostnamesToRemove := make(map[string]bool)
	originalToTransformed := make(map[string]string)
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			transformedHostname := trans.TransformHostname(rule.Host)
			hostnamesToRemove[transformedHostname] = true
			originalToTransformed[rule.Host] = transformedHostname
		}
	}

	// Filter out listeners matching these hostnames
	remainingListeners := make([]gatewayv1.Listener, 0)
	for _, listener := range gateway.Spec.Listeners {
		// Listener name is the hostname (as per synthesizeSharedGateway)
		listenerHostname := string(listener.Name)
		if !hostnamesToRemove[listenerHostname] {
			remainingListeners = append(remainingListeners, listener)
		}
	}

	gateway.Spec.Listeners = remainingListeners

	// Clean up certificate-mismatch annotation entries for removed hostnames
	if gateway.Annotations != nil {
		if certMismatch, exists := gateway.Annotations[MismatchedCertAnnotation]; exists {
			gateway.Annotations[MismatchedCertAnnotation] =
				r.removeCertMismatchEntries(certMismatch, originalToTransformed)
		}
	}
}

// mergeGatewaySpec merges the desired Gateway spec into the existing Gateway
// Listeners are merged by hostname (unique by listener name)
// Annotations are merged (desired overwrites existing on conflict, except for special cases)
func (r *IngressReconciler) mergeGatewaySpec(existing, desired *gatewayv1.Gateway) {
	// Merge annotations - desired annotations overwrite existing ones
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	for k, v := range desired.Annotations {
		// Special handling for certificate-mismatch annotation - merge instead of overwrite
		if k == MismatchedCertAnnotation {
			existing.Annotations[k] = r.mergeCertificateMismatchAnnotation(
				existing.Annotations[k], v)
		} else {
			existing.Annotations[k] = v
		}
	}

	// Update GatewayClassName
	existing.Spec.GatewayClassName = desired.Spec.GatewayClassName

	// Merge Infrastructure annotations
	if desired.Spec.Infrastructure != nil {
		if existing.Spec.Infrastructure == nil {
			existing.Spec.Infrastructure = &gatewayv1.GatewayInfrastructure{
				Annotations: make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue),
			}
		}
		if existing.Spec.Infrastructure.Annotations == nil {
			existing.Spec.Infrastructure.Annotations = make(map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue)
		}
		for k, v := range desired.Spec.Infrastructure.Annotations {
			existing.Spec.Infrastructure.Annotations[k] = v
		}
	}

	// Merge listeners - use listener name as unique key
	// Build map of existing listeners by name
	existingListeners := make(map[gatewayv1.SectionName]gatewayv1.Listener)
	for _, listener := range existing.Spec.Listeners {
		existingListeners[listener.Name] = listener
	}

	// Add or update listeners from desired
	for _, desiredListener := range desired.Spec.Listeners {
		existingListeners[desiredListener.Name] = desiredListener
	}

	// Convert map back to slice
	mergedListeners := make([]gatewayv1.Listener, 0, len(existingListeners))
	for _, listener := range existingListeners {
		mergedListeners = append(mergedListeners, listener)
	}

	existing.Spec.Listeners = mergedListeners
}

// mergeCertificateMismatchAnnotation merges certificate mismatch values from multiple reconciliations
// Values are semicolon-separated, and we deduplicate them
func (r *IngressReconciler) mergeCertificateMismatchAnnotation(existing, desired string) string {
	if existing == "" {
		return desired
	}
	if desired == "" {
		return existing
	}

	// Split both by semicolon and collect unique values
	valuesMap := make(map[string]bool)

	// Add existing values
	for _, val := range strings.Split(existing, ";") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			valuesMap[trimmed] = true
		}
	}

	// Add desired values
	for _, val := range strings.Split(desired, ";") {
		trimmed := strings.TrimSpace(val)
		if trimmed != "" {
			valuesMap[trimmed] = true
		}
	}

	// Convert back to slice and sort for consistent ordering
	values := make([]string, 0, len(valuesMap))
	for val := range valuesMap {
		values = append(values, val)
	}

	// Join with semicolon-space separator
	return strings.Join(values, "; ")
}

// removeCertMismatchEntries removes certificate mismatch entries that match the given hostname mappings
// Format: "original->transformed: namespace/secret->namespace/newsecret"
func (r *IngressReconciler) removeCertMismatchEntries(
	certMismatch string, hostnameMappings map[string]string,
) string {
	if certMismatch == "" {
		return ""
	}

	// Split by semicolon
	entries := strings.Split(certMismatch, ";")
	remainingEntries := make([]string, 0, len(entries))

	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}

		// Check if this entry matches any hostname mapping to remove
		// Format: "original->transformed: ..."
		shouldKeep := true
		for original, transformed := range hostnameMappings {
			prefix := original + "->" + transformed + ":"
			if strings.HasPrefix(trimmed, prefix) {
				shouldKeep = false
				break
			}
		}

		if shouldKeep {
			remainingEntries = append(remainingEntries, trimmed)
		}
	}

	if len(remainingEntries) == 0 {
		return ""
	}

	return strings.Join(remainingEntries, "; ")
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

// ensureReferenceGrants creates ReferenceGrant resources to allow Gateway to access Secrets in Ingress namespaces
func (r *IngressReconciler) ensureReferenceGrants(ctx context.Context, ingresses []networkingv1.Ingress) error {
	logger := log.FromContext(ctx)
	trans := r.getTranslator()

	// Get namespaces that need ReferenceGrants using translator
	namespacesWithTLS := trans.GetNamespacesWithTLS(ingresses)

	// Create ReferenceGrant in each namespace
	for _, namespace := range namespacesWithTLS {
		if err := r.applyReferenceGrant(ctx, namespace); err != nil {
			logger.Error(err, "failed to apply ReferenceGrant", "namespace", namespace)
			return err
		}
	}

	return nil
}

// cleanupReferenceGrantIfNeeded deletes the ReferenceGrant if no Ingresses with TLS remain in the namespace
func (r *IngressReconciler) cleanupReferenceGrantIfNeeded(ctx context.Context, namespace string) error {
	logger := log.FromContext(ctx)

	// List all Ingresses in this namespace
	var ingressList networkingv1.IngressList
	if err := r.List(ctx, &ingressList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list Ingresses: %w", err)
	}

	// Check if any Ingress has TLS configuration
	for _, ingress := range ingressList.Items {
		if len(ingress.Spec.TLS) > 0 {
			// Still have Ingresses with TLS, keep the ReferenceGrant
			logger.V(3).Info("ReferenceGrant still needed", "namespace", namespace,
				"reason", "Ingress with TLS exists")
			return nil
		}
	}

	// No Ingresses with TLS found, delete the ReferenceGrant if it exists and is managed by us
	refGrant := &gatewayv1beta1.ReferenceGrant{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      translator.ReferenceGrantName,
	}, refGrant)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted, nothing to do
			return nil
		}
		return err
	}

	// Delete if managed by us
	if r.isManagedByUs(refGrant) {
		logger.Info("Deleting ReferenceGrant (no Ingresses with TLS remain)",
			"namespace", namespace, "name", translator.ReferenceGrantName)
		if err := r.Delete(ctx, refGrant); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete ReferenceGrant: %w", err)
		}
	} else {
		logger.Info("ReferenceGrant exists but is not managed by us, skipping deletion",
			"namespace", namespace, "name", translator.ReferenceGrantName)
	}

	return nil
}

// applyReferenceGrant creates or updates a ReferenceGrant in the given namespace
func (r *IngressReconciler) applyReferenceGrant(ctx context.Context, ingressNamespace string) error {
	logger := log.FromContext(ctx)
	trans := r.getTranslator()

	// Create ReferenceGrant using translator
	refGrant := trans.CreateReferenceGrant(ingressNamespace)

	// Check if ReferenceGrant exists
	existing := &gatewayv1beta1.ReferenceGrant{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: ingressNamespace,
		Name:      translator.ReferenceGrantName,
	}, existing)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new ReferenceGrant
			logger.Info("Creating ReferenceGrant", "namespace", ingressNamespace, "name", translator.ReferenceGrantName)
			if err := r.Create(ctx, refGrant); err != nil {
				return fmt.Errorf("failed to create ReferenceGrant: %w", err)
			}
			return nil
		}
		return err
	}

	// Update existing ReferenceGrant if managed by us
	if r.isManagedByUs(existing) {
		existing.Spec = refGrant.Spec
		logger.Info("Updating ReferenceGrant", "namespace", ingressNamespace, "name", translator.ReferenceGrantName)
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update ReferenceGrant: %w", err)
		}
	} else {
		logger.Info("ReferenceGrant exists but is not managed by us, skipping",
			"namespace", ingressNamespace, "name", translator.ReferenceGrantName)
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
