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

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/metrics"
	"github.com/fiksn/ingress-doperator/internal/translator"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

const (
	IgnoreIngressAnnotation = "ingress-doperator.fiction.si/ignore-ingress"
	AllowIngressAnnotation  = "ingress-doperator.fiction.si/allow-ingress"
	WebhookAnnotation       = "ingress-doperator.fiction.si/webhook"
)

// +kubebuilder:webhook:path=/mutate-v1-ingress,mutating=true,failurePolicy=ignore,groups="networking.k8s.io",resources=ingresses,verbs=create;update,versions=v1,name=mingress.fiction.si,admissionReviewVersions=v1,sideEffects=None

// IngressMutator handles Ingress mutations
type IngressMutator struct {
	Client                          client.Client
	decoder                         admission.Decoder
	Scheme                          *runtime.Scheme
	Translator                      *translator.Translator
	IngressClassFilter              string
	IngressClassSnippetsFilters     []utils.IngressClassSnippetsFilter
	IngressNameSnippetsFilters      []utils.IngressClassSnippetsFilter
	IngressAnnotationSnippetsAdd    []utils.IngressAnnotationSnippetsRule
	IngressAnnotationSnippetsRemove []utils.IngressAnnotationSnippetsRule
	HTTPRouteManager                *utils.HTTPRouteManager
	Recorder                        record.EventRecorder
}

// Handle performs the mutation
func (m *IngressMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)
	logger.Info("Webhook called", "namespace", req.Namespace, "name", req.Name)

	ingress := &networkingv1.Ingress{}
	err := m.decoder.Decode(req, ingress)
	if err != nil {
		logger.Error(err, "failed to decode ingress")
		m.recordWarning(ingress, "DecodeFailed", "failed to decode ingress")
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if this Ingress should be ignored (skip all processing)
	if ingress.Annotations != nil && ingress.Annotations[IgnoreIngressAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress has ignore annotation, skipping mutation")
		return admission.Allowed("ignored")
	}

	// Check if this Ingress matches the ingress class filter
	if !m.matchesIngressClassFilter(ingress) {
		ingressClass := m.getIngressClass(ingress)
		logger.Info("Ingress class does not match filter, allowing without mutation",
			"ingressClass", ingressClass,
			"filter", m.IngressClassFilter)
		return admission.Allowed("ingress class filtered")
	}

	// Check if this Ingress should be allowed to be created (for compatibility)
	allowIngress := false
	if ingress.Annotations != nil && ingress.Annotations[AllowIngressAnnotation] == fmt.Sprintf("%t", true) {
		allowIngress = true
		logger.Info("Ingress has allow annotation, will permit creation after translation")
	}

	// Translate Ingress to Gateway API resources
	gateway, httpRoute, err := m.Translator.Translate(ingress)
	if err != nil {
		logger.Error(err, "failed to translate ingress")
		m.recordWarning(ingress, "TranslateFailed", "failed to translate ingress")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Resolve any named ports
	if err := m.HTTPRouteManager.ResolveNamedPorts(ctx, ingress, httpRoute); err != nil {
		logger.Error(err, "failed to resolve named ports")
		m.recordWarning(ingress, "ResolveNamedPortsFailed", "failed to resolve named ports")
		// Continue anyway with fallback ports
	}

	// Split HTTPRoute if it exceeds the Gateway API limit
	httpRoutes := m.HTTPRouteManager.SplitHTTPRouteIfNeeded(httpRoute)

	// Create the Gateway resource
	if err := m.Client.Create(ctx, gateway); err != nil {
		// If already exists, update it
		if err := m.Client.Update(ctx, gateway); err != nil {
			logger.Error(err, "failed to create/update Gateway")
			m.recordWarning(ingress, "GatewayApplyFailed", "failed to create/update Gateway")
			// Don't fail the admission, just log
		} else {
			logger.Info("Updated Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
			metrics.GatewayResourcesTotal.WithLabelValues("update", gateway.Namespace, gateway.Name).Inc()
		}
	} else {
		logger.Info("Created Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
		metrics.GatewayResourcesTotal.WithLabelValues("create", gateway.Namespace, gateway.Name).Inc()
	}

	// Apply all HTTPRoute(s) with proper cleanup of obsolete split routes
	metricRecorder := func(operation, namespace, name string) {
		metrics.HTTPRouteResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
	}

	m.applyIngressClassSnippetsFilters(ctx, ingress, httpRoutes)
	m.applyIngressNameSnippetsFilters(ctx, ingress, httpRoutes)
	m.applyIngressAnnotationSnippetsOverrides(ctx, ingress, httpRoutes)
	m.applyIngressAnnotationSnippetsFilter(ctx, ingress, httpRoutes)
	if err := m.HTTPRouteManager.ApplyHTTPRoutesAtomic(ctx, ingress, httpRoutes, metricRecorder); err != nil {
		logger.Error(err, "failed to apply HTTPRoutes")
		m.recordWarning(ingress, "HTTPRouteApplyFailed", "failed to apply HTTPRoutes")
		// Don't fail the admission, just log
	}
	m.finalizeIngressClassSnippetsFilters(ctx, ingress)
	m.finalizeIngressNameSnippetsFilters(ctx, ingress)
	m.finalizeIngressAnnotationSnippetsOverrides(ctx, ingress)
	m.finalizeIngressAnnotationSnippetsFilter(ctx, ingress)

	// Create ReferenceGrant if needed (when Ingress has TLS and Gateway is in different namespace)
	// Skip in ingress2gateway mode as it handles this itself
	if !m.Translator.Config.UseIngress2Gateway && len(ingress.Spec.TLS) > 0 &&
		ingress.Namespace != m.Translator.Config.GatewayNamespace {
		recordMetric := func(operation, namespace, name string) {
			metrics.ReferenceGrantResourcesTotal.WithLabelValues(operation, namespace, name).Inc()
		}
		if err := utils.ApplyReferenceGrant(ctx, m.Client, m.Scheme, m.Translator, ingress.Namespace, recordMetric); err != nil {
			logger.Error(err, "failed to apply ReferenceGrant", "namespace", ingress.Namespace)
			m.recordWarning(ingress, "ReferenceGrantApplyFailed", "failed to apply ReferenceGrant")
			// Don't fail the admission, just log
		} else {
			logger.Info("Applied ReferenceGrant", "namespace", ingress.Namespace)
		}
	}

	// By default, reject the Ingress after creating Gateway/HTTPRoute
	// This prevents the Ingress from being stored in the cluster
	if !allowIngress {
		logger.Info("Rejecting Ingress creation (Gateway/HTTPRoute created instead)",
			"ingress", ingress.Name,
			"gateway", gateway.Name,
			"httpRoute", httpRoute.Name)
		return admission.Denied("Ingress resources are not allowed - Gateway and HTTPRoute have been created instead. " +
			"Use annotation 'ingress-doperator.fiction.si/allow-ingress=true' to allow Ingress creation.")
	}

	// If allow annotation is set, mutate and allow the Ingress
	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
	}
	ingress.Annotations[WebhookAnnotation] = fmt.Sprintf("%t", true)

	// Marshal the modified ingress
	marshaledIngress, err := json.Marshal(ingress)
	if err != nil {
		logger.Error(err, "failed to marshal modified ingress")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	logger.Info("Allowing Ingress creation (allow annotation present)", "ingress", ingress.Name)
	// Return a patch response
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledIngress)
}

// InjectDecoder injects the decoder
func (m *IngressMutator) InjectDecoder(d *admission.Decoder) error {
	m.decoder = *d
	return nil
}

func (m *IngressMutator) recordWarning(ingress *networkingv1.Ingress, reason, message string) {
	if m.Recorder == nil || ingress == nil {
		return
	}
	m.Recorder.Event(ingress, "Warning", reason, message)
}

func (m *IngressMutator) applyIngressClassSnippetsFilters(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoutes []*gatewayv1.HTTPRoute,
) {
	if ingress == nil || len(httpRoutes) == 0 || len(m.IngressClassSnippetsFilters) == 0 {
		return
	}

	logger := log.FromContext(ctx)
	ingressClass := m.getIngressClass(ingress)
	matchedFilters := make([]utils.IngressClassSnippetsFilter, 0, len(m.IngressClassSnippetsFilters))
	for _, mapping := range m.IngressClassSnippetsFilters {
		if mapping.Pattern == "" {
			continue
		}
		matched, err := filepath.Match(mapping.Pattern, ingressClass)
		if err != nil {
			logger.Error(err, "invalid ingress class pattern for snippets filter", "pattern", mapping.Pattern)
			continue
		}
		if matched {
			matchedFilters = append(matchedFilters, mapping)
		}
	}

	if len(matchedFilters) == 0 {
		return
	}

	for _, route := range httpRoutes {
		for _, mapping := range matchedFilters {
			utils.AddExtensionRefFilterAfterExistingKind(route, utils.NginxGatewayGroup, utils.SnippetsFilterKind, mapping.Name)
		}
	}

	for _, mapping := range matchedFilters {
		if _, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			m.Client,
			m.Scheme,
			m.Translator.Config.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			mapping.Name,
			"",
		); err != nil {
			logger.Error(err, "failed to copy class-based SnippetsFilter", "name", mapping.Name, "namespace", ingress.Namespace)
		}
	}
}

func (m *IngressMutator) applyIngressNameSnippetsFilters(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoutes []*gatewayv1.HTTPRoute,
) {
	if ingress == nil || len(httpRoutes) == 0 || len(m.IngressNameSnippetsFilters) == 0 {
		return
	}

	logger := log.FromContext(ctx)
	matchedFilters := make([]utils.IngressClassSnippetsFilter, 0, len(m.IngressNameSnippetsFilters))
	for _, mapping := range m.IngressNameSnippetsFilters {
		if mapping.Pattern == "" {
			continue
		}
		matched, err := filepath.Match(mapping.Pattern, ingress.Name)
		if err != nil {
			logger.Error(err, "invalid ingress name pattern for snippets filter", "pattern", mapping.Pattern)
			continue
		}
		if matched {
			matchedFilters = append(matchedFilters, mapping)
		}
	}

	if len(matchedFilters) == 0 {
		return
	}

	for _, route := range httpRoutes {
		for _, mapping := range matchedFilters {
			utils.AddExtensionRefFilterAfterExistingKind(route, utils.NginxGatewayGroup, utils.SnippetsFilterKind, mapping.Name)
		}
	}

	for _, mapping := range matchedFilters {
		if _, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			m.Client,
			m.Scheme,
			m.Translator.Config.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			mapping.Name,
			"",
		); err != nil {
			logger.Error(err, "failed to copy name-based SnippetsFilter", "name", mapping.Name, "namespace", ingress.Namespace)
		}
	}
}

func (m *IngressMutator) finalizeIngressClassSnippetsFilters(ctx context.Context, ingress *networkingv1.Ingress) {
	if ingress == nil || len(m.IngressClassSnippetsFilters) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	ingressClass := m.getIngressClass(ingress)
	for _, mapping := range m.IngressClassSnippetsFilters {
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
		if _, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			m.Client,
			m.Scheme,
			m.Translator.Config.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			mapping.Name,
			ingress.Name,
		); err != nil {
			logger.Error(err, "failed to finalize class-based SnippetsFilter owner reference", "name", mapping.Name, "namespace", ingress.Namespace)
		}
	}
}

func (m *IngressMutator) finalizeIngressNameSnippetsFilters(ctx context.Context, ingress *networkingv1.Ingress) {
	if ingress == nil || len(m.IngressNameSnippetsFilters) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	for _, mapping := range m.IngressNameSnippetsFilters {
		if mapping.Pattern == "" {
			continue
		}
		matched, err := filepath.Match(mapping.Pattern, ingress.Name)
		if err != nil {
			logger.Error(err, "invalid ingress name pattern for snippets filter", "pattern", mapping.Pattern)
			continue
		}
		if !matched {
			continue
		}
		if _, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			m.Client,
			m.Scheme,
			m.Translator.Config.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			mapping.Name,
			ingress.Name,
		); err != nil {
			logger.Error(err, "failed to finalize name-based SnippetsFilter owner reference", "name", mapping.Name, "namespace", ingress.Namespace)
		}
	}
}

func (m *IngressMutator) applyIngressAnnotationSnippetsOverrides(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoutes []*gatewayv1.HTTPRoute,
) {
	if ingress == nil || len(httpRoutes) == 0 {
		return
	}
	addList := utils.MatchIngressAnnotationSnippetsRules(ingress.Annotations, m.IngressAnnotationSnippetsAdd)
	removeList := utils.MatchIngressAnnotationSnippetsRules(ingress.Annotations, m.IngressAnnotationSnippetsRemove)

	if len(addList) == 0 && len(removeList) == 0 {
		return
	}

	removeSet := make(map[string]struct{}, len(removeList))
	for _, name := range removeList {
		removeSet[name] = struct{}{}
	}

	for _, route := range httpRoutes {
		for _, name := range addList {
			if _, ok := removeSet[name]; ok {
				continue
			}
			utils.AddExtensionRefFilterAfterExistingKind(route, utils.NginxGatewayGroup, utils.SnippetsFilterKind, name)
		}
	}

	for _, name := range addList {
		if _, ok := removeSet[name]; ok {
			continue
		}
		if _, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			m.Client,
			m.Scheme,
			m.Translator.Config.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			name,
			"",
		); err != nil {
			log.FromContext(ctx).Error(err, "failed to copy annotation-based SnippetsFilter override", "name", name, "namespace", ingress.Namespace)
		}
	}
}

func (m *IngressMutator) finalizeIngressAnnotationSnippetsOverrides(ctx context.Context, ingress *networkingv1.Ingress) {
	if ingress == nil {
		return
	}
	addList := utils.MatchIngressAnnotationSnippetsRules(ingress.Annotations, m.IngressAnnotationSnippetsAdd)
	removeList := utils.MatchIngressAnnotationSnippetsRules(ingress.Annotations, m.IngressAnnotationSnippetsRemove)
	if len(addList) == 0 && len(removeList) == 0 {
		return
	}
	removeSet := make(map[string]struct{}, len(removeList))
	for _, name := range removeList {
		removeSet[name] = struct{}{}
	}
	for _, name := range addList {
		if _, ok := removeSet[name]; ok {
			continue
		}
		if _, err := utils.EnsureSnippetsFilterCopyForHTTPRoute(
			ctx,
			m.Client,
			m.Scheme,
			m.Translator.Config.GatewayNamespace,
			ingress.Namespace,
			ingress.Namespace,
			ingress.Name,
			name,
			ingress.Name,
		); err != nil {
			log.FromContext(ctx).Error(err, "failed to finalize annotation-based SnippetsFilter override", "name", name, "namespace", ingress.Namespace)
		}
	}
}

func (m *IngressMutator) applyIngressAnnotationSnippetsFilter(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoutes []*gatewayv1.HTTPRoute,
) {
	if ingress == nil || len(httpRoutes) == 0 {
		return
	}

	logger := log.FromContext(ctx)
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
		return
	}
	for _, warning := range warnings {
		logger.Info("nginx ingress annotation warning", "warning", warning, "namespace", ingress.Namespace, "name", ingress.Name)
	}

	filterName := utils.AutomaticSnippetsFilterName(ingress.Name)
	if _, err := utils.EnsureSnippetsFilterForIngress(
		ctx,
		m.Client,
		m.Scheme,
		httpRoutes[0],
		nil,
		ingress.Namespace,
		ingress.Name,
		filterName,
		snippets,
	); err != nil {
		logger.Error(err, "failed to apply annotation SnippetsFilter", "name", filterName, "namespace", ingress.Namespace)
		return
	}

	for _, route := range httpRoutes {
		utils.AddExtensionRefFilterAfterExistingKind(route, utils.NginxGatewayGroup, utils.SnippetsFilterKind, filterName)
	}
}

func (m *IngressMutator) finalizeIngressAnnotationSnippetsFilter(ctx context.Context, ingress *networkingv1.Ingress) {
	if ingress == nil {
		return
	}
	logger := log.FromContext(ctx)
	snippets, _, ok := utils.BuildNginxIngressSnippets(ingress.Annotations)
	if !ok {
		return
	}
	owner := &gatewayv1.HTTPRoute{}
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: ingress.Namespace, Name: ingress.Name}, owner); err != nil {
		return
	}
	filterName := utils.AutomaticSnippetsFilterName(ingress.Name)
	if _, err := utils.EnsureSnippetsFilterForIngress(
		ctx,
		m.Client,
		m.Scheme,
		owner,
		owner,
		ingress.Namespace,
		ingress.Name,
		filterName,
		snippets,
	); err != nil {
		logger.Error(err, "failed to finalize annotation SnippetsFilter owner reference", "name", filterName, "namespace", ingress.Namespace)
	}
}

// getIngressClass returns the ingress class from spec.ingressClassName or the legacy annotation
func (m *IngressMutator) getIngressClass(ingress *networkingv1.Ingress) string {
	// First check spec.ingressClassName
	if ingress.Spec.IngressClassName != nil && *ingress.Spec.IngressClassName != "" {
		return *ingress.Spec.IngressClassName
	}

	// Fallback to legacy annotation
	if ingress.Annotations != nil {
		if class, ok := ingress.Annotations["kubernetes.io/ingress.class"]; ok && class != "" {
			return class
		}
	}

	return ""
}

// matchesIngressClassFilter checks if the Ingress class matches the configured filter pattern
func (m *IngressMutator) matchesIngressClassFilter(ingress *networkingv1.Ingress) bool {
	// Default filter "*" matches everything
	if m.IngressClassFilter == "" {
		return false
	}
	if m.IngressClassFilter == "*" {
		return true
	}

	ingressClass := m.getIngressClass(ingress)

	// Empty ingress class only matches "*"
	if ingressClass == "" {
		return false
	}

	// Use filepath.Match for glob pattern matching
	matched, err := filepath.Match(m.IngressClassFilter, ingressClass)
	if err != nil {
		// If pattern is invalid, log error and don't match
		log.FromContext(context.Background()).Error(err, "Invalid ingress class filter pattern", "pattern", m.IngressClassFilter)
		return false
	}

	return matched
}
