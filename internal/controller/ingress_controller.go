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
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/metrics"
	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

const (
	IngressClassAnnotation             = "kubernetes.io/ingress.class"
	IngressDisabledAnnotation          = "ingress-doperator.fiction.si/disabled"
	IngressRemovedAnnotation           = "ingress-doperator.fiction.si/removed"
	IgnoreIngressAnnotation            = "ingress-doperator.fiction.si/ignore-ingress"
	OriginalIngressClassAnnotation     = "ingress-doperator.fiction.si/original-ingress-class"
	OriginalIngressClassNameAnnotation = "ingress-doperator.fiction.si/original-ingress-classname"
	FinalizerName                      = "ingress-doperator.fiction.si/finalizer"
	HTTPRouteSnippetsFilterAnnotation  = "ingress-doperator.fiction.si/httproute-snippets-filter"
	HTTPRouteAuthenticationAnnotation  = "ingress-doperator.fiction.si/httproute-authentication-filter"
	HTTPRouteRequestHeaderAnnotation   = "ingress-doperator.fiction.si/httproute-request-header-modifier-filter"
	DefaultGatewayAnnotationFilters    = "ingress.kubernetes.io,cert-manager.io," +
		"nginx.ingress.kubernetes.io,kubectl.kubernetes.io,kubernetes.io/ingress.class," +
		"traefik.ingress.kubernetes.io,ingress-doperator.fiction.si"
	DefaultHTTPRouteAnnotationFilters = "ingress.kubernetes.io,cert-manager.io," +
		"nginx.ingress.kubernetes.io,kubectl.kubernetes.io,kubernetes.io/ingress.class," +
		"traefik.ingress.kubernetes.io,ingress-doperator.fiction.si"
)

// ingressPostProcessing
type IngressPostProcessingMode string

const (
	// IngressPostProcessingModeNone leaves the source Ingress unchanged
	IngressPostProcessingModeNone IngressPostProcessingMode = "as-is"
	// IngressPostProcessingModeDisable removes the ingress class to disable processing
	IngressPostProcessingModeDisable IngressPostProcessingMode = "disable"
	// IngressPostProcessingModeRemove deletes the source Ingress resource
	IngressPostProcessingModeRemove IngressPostProcessingMode = "remove"
)

const requeueAfterError = 30 * time.Second

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
	IngressPostProcessingMode        IngressPostProcessingMode
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
	HTTPRouteManager                 *utils.HTTPRouteManager
	IngressClassSnippetsFilters      []utils.IngressClassSnippetsFilter
	ReconcileCache                   map[string]utils.ReconcileCacheEntry
	ReconcileCacheNamespace          string
	ReconcileCacheBaseName           string
	ReconcileCacheShards             int
	ReconcileCachePersist            bool
	reconcileCacheMu                 sync.Mutex
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
			logger.V(1).Info("Ingress not found, likely deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Ingress")
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	// Check if this Ingress should be ignored
	if ingress.Annotations != nil && ingress.Annotations[IgnoreIngressAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress has ignore annotation, skipping reconciliation")
		return ctrl.Result{}, nil
	}

	// Check if this Ingress matches the ingress class filter
	if !r.matchesIngressClassFilter(&ingress) {
		ingressClass := r.getIngressClass(&ingress)
		logger.V(1).Info("Ingress class does not match filter, skipping reconciliation",
			"ingressClass", ingressClass,
			"filter", r.IngressClassFilter)
		return ctrl.Result{}, nil
	}

	if r.shouldSkipReconcile(&ingress) {
		logger.V(3).Info("Ingress resourceVersion unchanged, skipping reconciliation",
			"namespace", ingress.Namespace,
			"name", ingress.Name,
			"resourceVersion", ingress.ResourceVersion)
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
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
		logger.V(1).Info("Added finalizer to Ingress")
	}

	// Handle source Ingress based on configured mode
	switch r.IngressPostProcessingMode {
	case IngressPostProcessingModeDisable:
		if err := r.disableIngress(ctx, &ingress); err != nil {
			logger.Error(err, "failed to disable source Ingress")
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
		if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
			logger.Error(err, "failed to refresh Ingress after disable")
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
	case IngressPostProcessingModeRemove:
		if err := r.removeIngress(ctx, &ingress); err != nil {
			logger.Error(err, "failed to remove source Ingress")
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
		// After removal, no further processing needed
		return ctrl.Result{}, nil
	case IngressPostProcessingModeNone:
		// Do nothing, continue with normal processing
	}

	// List Ingresses (all or filtered by namespace)
	var ingressList networkingv1.IngressList
	listOpts := []client.ListOption{}
	if r.WatchNamespace != "" {
		listOpts = append(listOpts, client.InNamespace(r.WatchNamespace))
	}

	if err := r.List(ctx, &ingressList, listOpts...); err != nil {
		logger.Error(err, "unable to list Ingresses")
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	if r.OneGatewayPerIngress {
		// Mode: One Gateway per Ingress
		result, err := r.reconcileOneGatewayPerIngress(ctx, ingressList.Items)
		r.maybeRecordReconcile(&ingress, result, err)
		return result, err
	}

	// Mode: Shared Gateways (group by IngressClass)
	result, err := r.reconcileSharedGateways(ctx, ingressList.Items)
	r.maybeRecordReconcile(&ingress, result, err)
	return result, err
}

func (r *IngressReconciler) reconcileOneGatewayPerIngress(
	ctx context.Context,
	ingresses []networkingv1.Ingress,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	trans := r.getTranslator()
	hadError := false

	for _, ingress := range ingresses {
		// Use translator to create Gateway and HTTPRoute
		// Override gateway name to match ingress name for one-per-ingress mode
		transConfig := trans.Config
		transConfig.GatewayName = ingress.Name
		singleTrans := translator.New(transConfig)

		gateway, httpRoute, err := singleTrans.Translate(&ingress)
		if err != nil {
			logger.Error(err, "failed to translate Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
			hadError = true
			continue
		}

		r.applyHTTPRouteExtensionRefs(ctx, &ingress, httpRoute)

		// Check if we can manage the Gateway resource
		gatewayNN := types.NamespacedName{
			Namespace: gateway.Namespace,
			Name:      gateway.Name,
		}
		existingGateway := &gatewayv1.Gateway{}
		canManageGateway, err := utils.CanUpdateResource(ctx, r.Client, existingGateway, gatewayNN)
		if err != nil {
			logger.Error(err, "error checking Gateway resource")
			hadError = true
			continue
		}

		if canManageGateway {
			logger.V(1).Info("Creating Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
			if err := r.applyGateway(ctx, gateway, existingGateway); err != nil {
				logger.Error(err, "failed to apply Gateway")
				hadError = true
				continue
			}
			logger.V(1).Info("Gateway applied successfully", "namespace", gateway.Namespace, "name", gateway.Name)
		}

		// Resolve any named ports before applying
		if err := r.HTTPRouteManager.ResolveNamedPorts(ctx, &ingress, httpRoute); err != nil {
			logger.Error(err, "failed to resolve named ports")
			// Continue anyway with fallback ports
		}

		// Split HTTPRoute if it exceeds the Gateway API limit
		httpRoutes := r.HTTPRouteManager.SplitHTTPRouteIfNeeded(httpRoute)

		// Apply all HTTPRoute(s) with proper cleanup of obsolete split routes
		metricRecorder := func(operation, namespace, name string) {
			metrics.HTTPRouteResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
		}
		if err := r.HTTPRouteManager.ApplyHTTPRoutesAtomic(ctx, &ingress, httpRoutes, metricRecorder); err != nil {
			logger.Error(err, "failed to apply HTTPRoutes")
			hadError = true
			continue
		}
	}

	if hadError {
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) reconcileSharedGateways(
	ctx context.Context,
	ingresses []networkingv1.Ingress,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	hadError := false

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
			hadError = true
			continue
		}

		if canManageGateway {
			logger.V(1).Info("Reconciling Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
			if err := r.applyGateway(ctx, gateway, existingGateway); err != nil {
				logger.Error(err, "failed to apply Gateway")
				hadError = true
				continue
			}
			logger.V(1).Info("Gateway applied successfully", "namespace", gateway.Namespace, "name", gateway.Name)
		} else {
			logger.V(1).Info("Skipping Gateway synthesis - resource exists and is not managed by us",
				"namespace", gatewayNN.Namespace, "name", gatewayNN.Name)
		}

		// Create HTTPRoutes using translator for each Ingress in this class
		for _, ingress := range classIngresses {
			// Override gateway name for this class
			transConfig := trans.Config
			transConfig.GatewayName = gatewayName
			classTrans := translator.New(transConfig)

			httpRoute := classTrans.TranslateToHTTPRoute(&ingress)

			r.applyHTTPRouteExtensionRefs(ctx, &ingress, httpRoute)

			// Resolve any named ports before applying
			if err := r.HTTPRouteManager.ResolveNamedPorts(ctx, &ingress, httpRoute); err != nil {
				logger.Error(err, "failed to resolve named ports")
				// Continue anyway with fallback ports
			}

			// Split HTTPRoute if it exceeds the Gateway API limit
			httpRoutes := r.HTTPRouteManager.SplitHTTPRouteIfNeeded(httpRoute)

			// Apply all HTTPRoute(s) with proper cleanup of obsolete split routes
			metricRecorder := func(operation, namespace, name string) {
				metrics.HTTPRouteResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
			}
			if err := r.HTTPRouteManager.ApplyHTTPRoutesAtomic(ctx, &ingress, httpRoutes, metricRecorder); err != nil {
				logger.Error(err, "failed to apply HTTPRoutes")
				hadError = true
				continue
			}
		}

		// Create ReferenceGrants for cross-namespace secret access
		// Only needed when Gateway is in different namespace than Ingress secrets
		// Skip in ingress2gateway mode as it handles this itself
		if !r.UseIngress2Gateway {
			trans := r.getTranslator()
			recordMetric := func(operation, namespace, name string) {
				metrics.ReferenceGrantResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
			}
			if err := utils.EnsureReferenceGrants(ctx, r.Client, r.Scheme, trans, classIngresses, recordMetric); err != nil {
				logger.Error(err, "failed to ensure ReferenceGrants")
				// Don't fail the reconciliation, just log the error
				hadError = true
			}
		}
	}

	if hadError {
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) applyHTTPRouteExtensionRefs(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoute *gatewayv1.HTTPRoute,
) {
	logger := log.FromContext(ctx)
	if ingress == nil || httpRoute == nil {
		return
	}

	extensionRefs := []struct {
		annotation string
		kind       string
		logName    string
	}{
		{HTTPRouteSnippetsFilterAnnotation, utils.SnippetsFilterKind, "SnippetsFilter"},
		{HTTPRouteAuthenticationAnnotation, utils.AuthenticationFilterKind, "AuthenticationFilter"},
		{HTTPRouteRequestHeaderAnnotation, utils.RequestHeaderModifierFilterKind, "RequestHeaderModifierFilter"},
	}

	for _, entry := range extensionRefs {
		names := utils.ParseCommaSeparatedAnnotation(ingress.Annotations, entry.annotation)
		if len(names) == 0 {
			if entry.kind != utils.SnippetsFilterKind {
				continue
			}
		}

		available := make([]string, 0, len(names))
		for _, name := range names {
			ok, err := utils.EnsureExtensionResource(
				ctx,
				r.Client,
				entry.kind,
				name,
				r.GatewayNamespace,
				ingress.Namespace,
				ingress.Namespace,
				ingress.Name,
				nil,
			)
			if err != nil {
				logger.Error(err, "failed to apply extension resource copy", "kind", entry.logName, "name", name, "namespace", ingress.Namespace)
				continue
			}
			if ok {
				available = append(available, name)
			}
		}

		if len(available) > 0 {
			utils.AddExtensionRefFilters(httpRoute, utils.NginxGatewayGroup, entry.kind, available)
		}

		if entry.kind == utils.SnippetsFilterKind {
			ingressClass := r.getIngressClass(ingress)
			for _, mapping := range r.IngressClassSnippetsFilters {
				if mapping.Pattern == "" {
					continue
				}
				matched, err := filepath.Match(mapping.Pattern, ingressClass)
				if err != nil {
					logger.Error(err, "invalid ingress class pattern for snippets filter", "pattern", mapping.Pattern)
					continue
				}
				if !matched {
					continue
				}
				ok, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
					ctx,
					r.Client,
					r.Scheme,
					r.GatewayNamespace,
					httpRoute.Namespace,
					ingress.Namespace,
					ingress.Name,
					mapping.Name,
					httpRoute.Name,
				)
				if err != nil {
					logger.Error(err, "failed to apply class-based SnippetsFilter copy", "name", mapping.Name, "namespace", ingress.Namespace)
					continue
				}
				if ok {
					utils.AddExtensionRefFilterAfterExistingKind(httpRoute, utils.NginxGatewayGroup, utils.SnippetsFilterKind, mapping.Name)
				}
			}

			if ingress.Annotations != nil {
				for _, fullKey := range utils.NginxIngressSnippetWarningAnnotations(ingress.Annotations) {
					logger.Info("Ignoring nginx ingress annotation; use ingress-doperator.fiction.si/httproute-snippets-filter instead",
						"annotation", fullKey,
						"namespace", ingress.Namespace,
						"name", ingress.Name)
				}
			}
			snippets, warnings, ok := utils.BuildNginxIngressSnippets(ingress.Annotations)
			if !ok {
				continue
			}
			for _, warning := range warnings {
				logger.Info("nginx ingress annotation warning", "warning", warning, "namespace", ingress.Namespace, "name", ingress.Name)
			}
			filterName := utils.AutomaticSnippetsFilterName(ingress.Name)
			ready, err := utils.EnsureSnippetsFilterForIngress(
				ctx,
				r.Client,
				r.Scheme,
				httpRoute,
				ingress,
				ingress.Namespace,
				ingress.Name,
				filterName,
				snippets,
			)
			if err != nil {
				logger.Error(err, "failed to apply annotation SnippetsFilter", "name", filterName, "namespace", httpRoute.Namespace)
				continue
			}
			if ready {
				utils.AddExtensionRefFilterAfterExistingKind(httpRoute, utils.NginxGatewayGroup, utils.SnippetsFilterKind, filterName)
			}
		}
	}
}

func (r *IngressReconciler) handleDeletion(ctx context.Context, ingress *networkingv1.Ingress) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("Handling Ingress deletion", "namespace", ingress.Namespace, "name", ingress.Name)

	if !r.EnableDeletion {
		// Deletion is disabled, just remove finalizer
		err := r.removeFinalizer(ctx, ingress)
		if err != nil {
			return ctrl.Result{}, err
		}

		logger.V(1).Info("Deletion disabled - HTTPRoute and Gateway will not be deleted")
		r.evictReconcileCache(ingress)
		return ctrl.Result{}, nil
	}

	routes, err := r.HTTPRouteManager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, route := range routes {
		httpRoute := &route
		if utils.IsManagedByUsForIngress(httpRoute, ingress.Namespace, ingress.Name) {
			logger.V(1).Info("Deleting managed HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
			if err := r.Delete(ctx, httpRoute); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete HTTPRoute")
				return ctrl.Result{}, err
			}
			metrics.HTTPRouteResourcesTotal.WithLabelValues("delete", httpRoute.Namespace, httpRoute.Name).Inc()
		} else {
			logger.V(1).Info("HTTPRoute exists but is not managed by us for this Ingress, skipping deletion",
				"namespace", httpRoute.Namespace, "name", httpRoute.Name)
		}
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
			// Gateway exists, check if we manage it for this specific ingress
			if utils.IsManagedByUsForIngress(gateway, ingress.Namespace, ingress.Name) {
				logger.V(1).Info("Deleting managed Gateway", "namespace", gateway.Namespace, "name", gateway.Name)
				if err := r.Delete(ctx, gateway); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "failed to delete Gateway")
					return ctrl.Result{}, err
				}
			} else {
				logger.V(1).Info("Gateway exists but is not managed by us for this Ingress, skipping deletion",
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

		if err == nil && utils.IsManagedByUsWithIngress(gateway, ingress.Namespace, ingress.Name) {
			// Remove listeners for hostnames from this Ingress
			trans := r.getTranslator()
			translator.RemoveIngressListeners(gateway, ingress, trans)

			if len(gateway.Spec.Listeners) == 0 {
				// No more listeners, delete the Gateway
				logger.V(1).Info("Deleting Gateway with no remaining listeners",
					"namespace", gateway.Namespace, "name", gateway.Name)
				if err := r.Delete(ctx, gateway); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "failed to delete Gateway")
					return ctrl.Result{}, err
				}
			} else {
				// Update Gateway with remaining listeners
				logger.V(1).Info("Removing listeners from shared Gateway",
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
			if err := utils.CleanupReferenceGrantIfNeeded(ctx, r.Client, ingress.Namespace, ingress.Name); err != nil {
				logger.Error(err, "failed to cleanup ReferenceGrant", "namespace", ingress.Namespace, "ingress", ingress.Name)
				// Don't fail the reconciliation, just log the error
			}
		}
	}

	err = r.removeFinalizer(ctx, ingress)
	if err != nil {
		logger.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	r.evictReconcileCache(ingress)

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) evictReconcileCache(ingress *networkingv1.Ingress) {
	if ingress == nil || r.ReconcileCache == nil {
		return
	}
	if string(ingress.UID) == "" {
		return
	}
	key := utils.ReconcileCacheKey(string(ingress.UID))
	r.reconcileCacheMu.Lock()
	delete(r.ReconcileCache, key)
	cacheCopyEntries := make(map[string]utils.ReconcileCacheEntry, len(r.ReconcileCache))
	for k, v := range r.ReconcileCache {
		cacheCopyEntries[k] = v
	}
	r.reconcileCacheMu.Unlock()

	if !r.ReconcileCachePersist {
		return
	}
	if r.ReconcileCacheBaseName == "" || r.ReconcileCacheNamespace == "" {
		return
	}
	if err := utils.SaveReconcileCacheSharded(
		context.Background(),
		r.Client,
		r.ReconcileCacheNamespace,
		r.ReconcileCacheBaseName,
		r.ReconcileCacheShards,
		cacheCopyEntries,
	); err != nil {
		log.FromContext(context.Background()).Error(err, "failed to persist reconcile cache eviction")
	}
}

func (r *IngressReconciler) removeFinalizer(ctx context.Context, ingress *networkingv1.Ingress) error {
	if utils.ContainsString(ingress.Finalizers, FinalizerName) {
		ingress.Finalizers = utils.RemoveString(ingress.Finalizers, FinalizerName)
		if err := r.Update(ctx, ingress); err != nil {
			return err
		}
	}
	return nil
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

func (r *IngressReconciler) removeIngress(ctx context.Context, ingress *networkingv1.Ingress) error {
	logger := log.FromContext(ctx)

	// Check if already marked for removal to avoid re-deletion
	if ingress.Annotations != nil && ingress.Annotations[IngressRemovedAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress already marked for removal, proceeding with deletion")
		// Delete the Ingress resource
		if err := r.Delete(ctx, ingress); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // Already deleted
			}
			return fmt.Errorf("failed to delete source Ingress: %w", err)
		}
		logger.Info("Successfully deleted source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)
		return nil
	}

	// First, mark the Ingress with removal annotation
	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
	}
	ingress.Annotations[IngressRemovedAnnotation] = fmt.Sprintf("%t", true)

	// Also add ignore annotation to prevent re-reconciliation
	ingress.Annotations[IgnoreIngressAnnotation] = fmt.Sprintf("%t", true)

	if err := r.Update(ctx, ingress); err != nil {
		return fmt.Errorf("failed to mark Ingress for removal: %w", err)
	}
	logger.Info("Marked source Ingress for removal", "namespace", ingress.Namespace, "name", ingress.Name)

	// Delete the Ingress resource
	if err := r.Delete(ctx, ingress); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Already deleted
		}
		return fmt.Errorf("failed to delete source Ingress: %w", err)
	}
	logger.Info("Successfully deleted source Ingress", "namespace", ingress.Namespace, "name", ingress.Name)

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

// matchesIngressClassFilter checks if the Ingress class matches the configured filter pattern
func (r *IngressReconciler) matchesIngressClassFilter(ingress *networkingv1.Ingress) bool {
	// Default filter "*" matches everything
	if r.IngressClassFilter == "" {
		return false
	}
	if r.IngressClassFilter == "*" {
		return true
	}

	ingressClass := r.getIngressClass(ingress)

	// Empty ingress class only matches "*"
	if ingressClass == "" {
		return false
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
			logger.V(1).Info("Creating Gateway", "namespace", desired.Namespace, "name", desired.Name)
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
	for attempt := 0; attempt < 3; attempt++ {
		translator.MergeGatewaySpec(existing, desired)

		logger.V(1).Info("Updating Gateway", "namespace", existing.Namespace, "name", existing.Name)
		if err := r.Update(ctx, existing); err != nil {
			if apierrors.IsConflict(err) {
				backoff := time.Duration(attempt+1) * 200 * time.Millisecond
				logger.V(1).Info("Gateway update conflict, retrying",
					"namespace", existing.Namespace,
					"name", existing.Name,
					"backoff", backoff.String())
				time.Sleep(backoff)
				if err := r.Get(ctx, types.NamespacedName{
					Namespace: desired.Namespace,
					Name:      desired.Name,
				}, existing); err != nil {
					return fmt.Errorf("failed to refresh Gateway after conflict: %w", err)
				}
				continue
			}
			return fmt.Errorf("failed to update Gateway: %w", err)
		}
		// Record metric for Gateway update
		metrics.GatewayResourcesTotal.WithLabelValues("update", existing.Namespace, existing.Name).Inc()
		return nil
	}

	return fmt.Errorf("failed to update Gateway after retries")
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

func (r *IngressReconciler) shouldSkipReconcile(ingress *networkingv1.Ingress) bool {
	if ingress == nil {
		return false
	}
	if r.ReconcileCache == nil {
		return false
	}
	if ingress.ResourceVersion == "" {
		return false
	}
	key := utils.ReconcileCacheKey(string(ingress.UID))
	r.reconcileCacheMu.Lock()
	defer r.reconcileCacheMu.Unlock()
	last, ok := r.ReconcileCache[key]
	if !ok {
		return false
	}
	if last.ResourceVersion != ingress.ResourceVersion {
		return false
	}
	if time.Since(time.Unix(last.UpdatedAtUnix, 0)) > utils.ReconcileCacheTTL {
		delete(r.ReconcileCache, key)
		return false
	}
	return true
}

func (r *IngressReconciler) maybeRecordReconcile(
	ingress *networkingv1.Ingress,
	result ctrl.Result,
	err error,
) {
	if ingress == nil {
		return
	}
	if err != nil || result.Requeue || result.RequeueAfter != 0 {
		return
	}
	if r.ReconcileCache == nil {
		return
	}
	if ingress.ResourceVersion == "" {
		return
	}
	key := utils.ReconcileCacheKey(string(ingress.UID))
	r.reconcileCacheMu.Lock()
	r.ReconcileCache[key] = utils.ReconcileCacheEntry{
		ResourceVersion: ingress.ResourceVersion,
		UpdatedAtUnix:   time.Now().Unix(),
	}
	cacheCopyEntries := make(map[string]utils.ReconcileCacheEntry, len(r.ReconcileCache))
	for k, v := range r.ReconcileCache {
		cacheCopyEntries[k] = v
	}
	r.reconcileCacheMu.Unlock()

	if !r.ReconcileCachePersist {
		return
	}
	if r.ReconcileCacheBaseName == "" || r.ReconcileCacheNamespace == "" {
		return
	}
	if err := utils.SaveReconcileCacheSharded(
		context.Background(),
		r.Client,
		r.ReconcileCacheNamespace,
		r.ReconcileCacheBaseName,
		r.ReconcileCacheShards,
		cacheCopyEntries,
	); err != nil {
		log.FromContext(context.Background()).Error(err, "failed to persist reconcile cache")
	}
}

func NamespaceFilter(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == namespace
	})
}
