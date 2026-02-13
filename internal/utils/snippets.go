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

package utils

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	NginxGatewayGroup               = "gateway.nginx.org"
	SnippetsFilterKind              = "SnippetsFilter"
	SnippetsPolicyKind              = "SnippetsPolicy"
	AuthenticationFilterKind        = "AuthenticationFilter"
	RequestHeaderModifierFilterKind = "RequestHeaderModifierFilter"
	RateLimitPolicyKind             = "RateLimitPolicy"
	SnippetsFilterCRDName           = "snippetsfilters.gateway.nginx.org"
	SnippetsPolicyCRDName           = "snippetspolicies.gateway.nginx.org"
	AuthenticationFilterCRDName     = "authenticationfilters.gateway.nginx.org"
	RequestHeaderModifierCRDName    = "requestheadermodifierfilters.gateway.nginx.org"
	RateLimitPolicyCRDName          = "ratelimitpolicies.gateway.nginx.org"
)

const (
	nginxIngressAnnotationPrefix = "nginx.ingress.kubernetes.io/"
	ingressAnnotationPrefix      = "ingress.kubernetes.io/"
	maxK8sNameLength             = 253
)

const (
	sslRedirectKey           = "ssl-redirect"
	forceSSLRedirectKey      = "force-ssl-redirect"
	preserveTrailingSlashKey = "preserve-trailing-slash"
	configurationSnippetKey  = "configuration-snippet"
	serverSnippetKey         = "server-snippet"
	authSnippetKey           = "auth-snippet"
	proxyBodySizeKey         = "proxy-body-size"
	clientMaxBodySizeKey     = "client-max-body-size"
	proxyRedirectFromKey     = "proxy-redirect-from"
	proxyRedirectToKey       = "proxy-redirect-to"
	proxyBuffersNumberKey    = "proxy-buffers-number"
	browserXssFilterKey      = "browser-xss-filter"
	contentTypeNosniffKey    = "content-type-nosniff"
	referrerPolicyKey        = "referrer-policy"
	sslProxyHeadersKey       = "ssl-proxy-headers"
)

var nginxIngressDirectiveWhitelist = map[string]struct{}{
	"client-body-buffer-size":     {},
	"http2-push-preload":          {},
	"proxy-buffer-size":           {},
	"proxy-buffering":             {},
	"proxy-busy-buffers-size":     {},
	"proxy-connect-timeout":       {},
	"proxy-cookie-domain":         {},
	"proxy-cookie-path":           {},
	"proxy-http-version":          {},
	"proxy-max-temp-file-size":    {},
	"proxy-next-upstream":         {},
	"proxy-next-upstream-timeout": {},
	"proxy-next-upstream-tries":   {},
	"proxy-read-timeout":          {},
	"proxy-request-buffering":     {},
	"proxy-send-timeout":          {},
	"proxy-ssl-ciphers":           {},
	"proxy-ssl-name":              {},
	"proxy-ssl-protocols":         {},
	"proxy-ssl-server-name":       {},
	"proxy-ssl-verify":            {},
	"proxy-ssl-verify-depth":      {},
	"satisfy":                     {},
	"ssl-ciphers":                 {},
	"ssl-prefer-server-ciphers":   {},
}

func isWhitelistedNginxIngressDirective(suffix string) bool {
	_, ok := nginxIngressDirectiveWhitelist[suffix]
	return ok
}

func collectSnippetWarnings(annotations map[string]string) []string {
	if annotations == nil {
		return nil
	}
	warnings := make([]string, 0, 2)
	for key := range annotations {
		if !strings.HasPrefix(key, nginxIngressAnnotationPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(key, nginxIngressAnnotationPrefix)
		if suffix == "" {
			continue
		}
		if strings.HasSuffix(suffix, "-snippet") {
			warnings = append(warnings, key)
		}
	}
	sort.Strings(warnings)
	return warnings
}

type ingressAnnotationKey struct {
	fullKey string
	prefix  string
	suffix  string
}

type sslProxyHeader struct {
	headerVar string
	value     string
}

func parseSSLProxyHeaders(raw string) ([]sslProxyHeader, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, "||")
	seen := make(map[string]struct{})
	headers := make([]sslProxyHeader, 0, len(parts))
	warnings := make([]string, 0, 1)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pieces := strings.SplitN(part, ":", 2)
		if len(pieces) != 2 {
			warnings = append(warnings, "ssl-proxy-headers entry must be HEADER:value")
			continue
		}
		header := strings.ToLower(strings.TrimSpace(pieces[0]))
		value := strings.TrimSpace(pieces[1])
		if header == "" || value == "" {
			warnings = append(warnings, "ssl-proxy-headers entry must be HEADER:value")
			continue
		}
		headerVar := strings.ReplaceAll(header, "-", "_")
		key := headerVar + "\x00" + value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		headers = append(headers, sslProxyHeader{
			headerVar: headerVar,
			value:     value,
		})
	}

	return headers, warnings
}

type crdVersionCacheEntry struct {
	version string
}

var crdVersionCache sync.Map

type IngressClassSnippetsFilter struct {
	Pattern string
	Name    string
}

func ParseIngressClassSnippetsFilters(raw string) ([]IngressClassSnippetsFilter, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	entries := strings.Split(raw, ",")
	out := make([]IngressClassSnippetsFilter, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid entry %q, expected pattern:name", trimmed)
		}
		pattern := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		if pattern == "" || name == "" {
			return nil, fmt.Errorf("invalid entry %q, pattern and name must be non-empty", trimmed)
		}
		out = append(out, IngressClassSnippetsFilter{
			Pattern: pattern,
			Name:    name,
		})
	}
	return out, nil
}

type IngressAnnotationSnippetsRule struct {
	AnnotationKey  string
	ValuePattern   string
	SnippetsFilter []string
}

func ParseIngressAnnotationSnippetsRules(raw string) ([]IngressAnnotationSnippetsRule, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	entries := strings.Split(raw, ";")
	out := make([]IngressAnnotationSnippetsRule, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid entry %q, expected key=value:filter1,filter2", trimmed)
		}
		keyValue := strings.TrimSpace(parts[0])
		filtersRaw := strings.TrimSpace(parts[1])
		kv := strings.SplitN(keyValue, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid entry %q, expected key=value:filter1,filter2", trimmed)
		}
		key := strings.TrimSpace(kv[0])
		valuePattern := strings.TrimSpace(kv[1])
		if key == "" || valuePattern == "" || filtersRaw == "" {
			return nil, fmt.Errorf("invalid entry %q, key, value and filters must be non-empty", trimmed)
		}
		filters := ParseCommaSeparatedAnnotation(map[string]string{"value": filtersRaw}, "value")
		if len(filters) == 0 {
			return nil, fmt.Errorf("invalid entry %q, filters list must be non-empty", trimmed)
		}
		out = append(out, IngressAnnotationSnippetsRule{
			AnnotationKey:  key,
			ValuePattern:   valuePattern,
			SnippetsFilter: filters,
		})
	}
	return out, nil
}

func MatchIngressAnnotationSnippetsRules(
	annotations map[string]string,
	rules []IngressAnnotationSnippetsRule,
) []string {
	if annotations == nil || len(rules) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, rule := range rules {
		if rule.AnnotationKey == "" || rule.ValuePattern == "" {
			continue
		}
		value, ok := annotations[rule.AnnotationKey]
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		matched, err := filepath.Match(rule.ValuePattern, value)
		if err != nil || !matched {
			continue
		}
		for _, name := range rule.SnippetsFilter {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// ParseCommaSeparatedAnnotation splits a comma-separated annotation value into unique entries.
func ParseCommaSeparatedAnnotation(annotations map[string]string, key string) []string {
	if annotations == nil {
		return nil
	}
	raw, ok := annotations[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// BuildNginxIngressSnippets builds SnippetsFilter entries from NGINX Ingress annotations.
// nolint:gocyclo
func BuildNginxIngressSnippets(annotations map[string]string) ([]map[string]interface{}, []string, bool) {
	if annotations == nil {
		return nil, nil, false
	}

	keys := make([]ingressAnnotationKey, 0, len(annotations))
	for key := range annotations {
		if strings.HasPrefix(key, nginxIngressAnnotationPrefix) {
			suffix := strings.TrimPrefix(key, nginxIngressAnnotationPrefix)
			if suffix != "" {
				keys = append(keys, ingressAnnotationKey{
					fullKey: key,
					prefix:  nginxIngressAnnotationPrefix,
					suffix:  suffix,
				})
			}
			continue
		}
		if strings.HasPrefix(key, ingressAnnotationPrefix) {
			suffix := strings.TrimPrefix(key, ingressAnnotationPrefix)
			if suffix != "" {
				keys = append(keys, ingressAnnotationKey{
					fullKey: key,
					prefix:  ingressAnnotationPrefix,
					suffix:  suffix,
				})
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil, false
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].fullKey < keys[j].fullKey
	})

	state := ingestNginxIngressAnnotations(annotations, keys)
	lines, warnings := buildNginxDirectiveLines(state)
	snippets := buildNginxSnippetBlocks(lines, state)

	if len(snippets) == 0 {
		return nil, warnings, false
	}

	return snippets, warnings, true
}

type nginxIngressSnippetState struct {
	lines                 []string
	sslRedirectOff        bool
	forceSSLRedirect      bool
	preserveTrailingSlash bool
	proxyBodySizeValue    string
	clientMaxBodySize     string
	proxyRedirectFrom     string
	proxyRedirectTo       string
	proxyBuffersNumber    string
	proxyBufferSize       string
	browserXssFilter      bool
	contentTypeNosniff    bool
	referrerPolicy        string
	sslProxyHeaders       []sslProxyHeader
	warnings              []string
}

func ingestNginxIngressAnnotations(annotations map[string]string, keys []ingressAnnotationKey) nginxIngressSnippetState {
	state := nginxIngressSnippetState{
		lines: make([]string, 0, len(keys)),
	}

	for _, entry := range keys {
		raw := annotations[entry.fullKey]
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if entry.prefix == nginxIngressAnnotationPrefix {
			if handled := applyIngressAnnotationValue(&state, entry.suffix, value); handled {
				continue
			}

			if !isWhitelistedNginxIngressDirective(entry.suffix) {
				continue
			}
			if entry.suffix == "proxy-buffer-size" {
				state.proxyBufferSize = value
			}
			directive := strings.ReplaceAll(entry.suffix, "-", "_")
			state.lines = append(state.lines, fmt.Sprintf("%s %s;", directive, value))
			continue
		}
		if entry.prefix == ingressAnnotationPrefix {
			applyLegacyIngressAnnotationValue(&state, entry.suffix, value)
		}
	}

	return state
}

func applyIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case configurationSnippetKey, serverSnippetKey, authSnippetKey:
		return true
	case proxyBodySizeKey:
		state.proxyBodySizeValue = value
		return true
	case clientMaxBodySizeKey:
		state.clientMaxBodySize = value
		return true
	case proxyRedirectFromKey:
		state.proxyRedirectFrom = value
		return true
	case proxyRedirectToKey:
		state.proxyRedirectTo = value
		return true
	case proxyBuffersNumberKey:
		state.proxyBuffersNumber = value
		return true
	case sslRedirectKey:
		if strings.EqualFold(value, "false") {
			state.sslRedirectOff = true
		}
		return true
	case forceSSLRedirectKey:
		if strings.EqualFold(value, "true") {
			state.forceSSLRedirect = true
		}
		return true
	case preserveTrailingSlashKey:
		if strings.EqualFold(value, "true") {
			state.preserveTrailingSlash = true
		}
		return true
	default:
		return false
	}
}

func applyLegacyIngressAnnotationValue(state *nginxIngressSnippetState, suffix, value string) bool {
	switch suffix {
	case browserXssFilterKey:
		if strings.EqualFold(value, "true") {
			state.browserXssFilter = true
		}
		return true
	case contentTypeNosniffKey:
		if strings.EqualFold(value, "true") {
			state.contentTypeNosniff = true
		}
		return true
	case referrerPolicyKey:
		state.referrerPolicy = value
		return true
	case sslProxyHeadersKey:
		headers, warnings := parseSSLProxyHeaders(value)
		if len(headers) > 0 {
			state.sslProxyHeaders = headers
		}
		if len(warnings) > 0 {
			state.warnings = append(state.warnings, warnings...)
		}
		return true
	case forceSSLRedirectKey:
		if strings.EqualFold(value, "true") {
			state.forceSSLRedirect = true
		}
		return true
	default:
		return false
	}
}

func buildNginxDirectiveLines(state nginxIngressSnippetState) ([]string, []string) {
	lines := append([]string{}, state.lines...)
	warnings := append([]string{}, state.warnings...)

	if state.clientMaxBodySize != "" {
		lines = append(lines, fmt.Sprintf("client_max_body_size %s;", state.clientMaxBodySize))
	} else if state.proxyBodySizeValue != "" {
		lines = append(lines, fmt.Sprintf("client_max_body_size %s;", state.proxyBodySizeValue))
	}

	if state.proxyRedirectFrom != "" {
		lowerFrom := strings.ToLower(state.proxyRedirectFrom)
		if lowerFrom == "off" || lowerFrom == "default" {
			lines = append(lines, fmt.Sprintf("proxy_redirect %s;", state.proxyRedirectFrom))
		} else if state.proxyRedirectTo != "" {
			lines = append(lines, fmt.Sprintf("proxy_redirect %s %s;", state.proxyRedirectFrom, state.proxyRedirectTo))
		} else {
			warnings = append(warnings, "proxy-redirect-from requires proxy-redirect-to (or set proxy-redirect-from to off/default)")
		}
	} else if state.proxyRedirectTo != "" {
		warnings = append(warnings, "proxy-redirect-to requires proxy-redirect-from")
	}

	if state.proxyBuffersNumber != "" {
		if state.proxyBufferSize == "" {
			warnings = append(warnings, "proxy-buffers-number requires proxy-buffer-size to compute proxy_buffers")
		} else {
			lines = append(lines, fmt.Sprintf("proxy_buffers %s %s;", state.proxyBuffersNumber, state.proxyBufferSize))
		}
	}

	return lines, warnings
}

func buildNginxSnippetBlocks(lines []string, state nginxIngressSnippetState) []map[string]interface{} {
	snippets := make([]map[string]interface{}, 0, 2)
	if state.sslRedirectOff {
		snippets = append(snippets, map[string]interface{}{
			"context": "http",
			"value":   "ssl_redirect off;",
		})
	}

	serverLines := append([]string{}, lines...)
	if state.browserXssFilter {
		serverLines = append(serverLines, "add_header X-XSS-Protection \"1; mode=block\" always;")
	}
	if state.contentTypeNosniff {
		serverLines = append(serverLines, "add_header X-Content-Type-Options \"nosniff\" always;")
	}
	if strings.TrimSpace(state.referrerPolicy) != "" {
		serverLines = append(serverLines,
			fmt.Sprintf("add_header Referrer-Policy %q always;", escapeHeaderValue(state.referrerPolicy)))
	}
	if state.forceSSLRedirect {
		redirectTarget := "https://$server_name$request_uri"
		if state.preserveTrailingSlash {
			redirectTarget = "https://$server_name$request_uri"
		}
		serverLines = append(serverLines,
			"set $ingress_doperator_needs_redirect 0;",
			"if ($scheme != \"https\") { set $ingress_doperator_needs_redirect 1; }",
		)
		for _, header := range state.sslProxyHeaders {
			serverLines = append(serverLines,
				fmt.Sprintf(
					"if ($http_%s = %q) { set $ingress_doperator_needs_redirect 0; }",
					header.headerVar,
					header.value,
				),
			)
		}
		serverLines = append(serverLines,
			"add_header Strict-Transport-Security \"max-age=31536000\" always;",
			fmt.Sprintf("if ($ingress_doperator_needs_redirect = 1) { return 308 %s; }", redirectTarget),
		)
	}

	if len(serverLines) > 0 {
		snippets = append(snippets, map[string]interface{}{
			"context": "http.server",
			"value":   strings.Join(serverLines, "\n"),
		})
	}

	return snippets
}

func escapeHeaderValue(value string) string {
	return strings.ReplaceAll(value, "\"", "\\\"")
}

// NginxIngressSnippetWarningAnnotations returns full annotation keys that should emit warnings when ignored.
func NginxIngressSnippetWarningAnnotations(annotations map[string]string) []string {
	return collectSnippetWarnings(annotations)
}

// AutomaticSnippetsFilterName returns a stable name for annotation-based SnippetsFilter resources.
func AutomaticSnippetsFilterName(ingressName string) string {
	base := fmt.Sprintf("automatic-%s-annotations", ingressName)
	if len(base) <= maxK8sNameLength {
		return base
	}
	trimmed := base[:maxK8sNameLength]
	return strings.TrimRight(trimmed, "-")
}

// EnsureSnippetsFilterForIngress creates or updates a SnippetsFilter for the given Ingress.
// Returns true if the resource exists and can be referenced safely.
func EnsureSnippetsFilterForIngress(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	httpRoute *gatewayv1.HTTPRoute,
	owner client.Object,
	ingressNamespace string,
	ingressName string,
	filterName string,
	snippets []map[string]interface{},
) (bool, error) {
	logger := log.FromContext(ctx)
	if httpRoute == nil {
		return false, nil
	}
	if len(snippets) == 0 {
		return false, nil
	}
	crdName, ok := snippetsCRDNameForKind(SnippetsFilterKind)
	if !ok {
		return false, nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return false, err
	}

	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: SnippetsFilterKind}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(filterName)
	desired.SetNamespace(httpRoute.Namespace)

	if scheme != nil && owner != nil {
		if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
			return false, err
		}
	}

	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	desired.SetAnnotations(annotations)

	desired.Object["spec"] = map[string]interface{}{
		"snippets": snippets,
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err = c.Get(ctx, types.NamespacedName{Namespace: httpRoute.Namespace, Name: filterName}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating SnippetsFilter", "namespace", httpRoute.Namespace, "name", filterName)
			if err := c.Create(ctx, desired); err != nil {
				return false, fmt.Errorf("failed to create SnippetsFilter %s/%s: %w", httpRoute.Namespace, filterName, err)
			}
			return true, nil
		}
		return false, err
	}

	if !IsManagedByUs(existing) {
		logger.Info("SnippetsFilter exists but is not managed by us, skipping",
			"namespace", httpRoute.Namespace,
			"name", filterName)
		return false, nil
	}

	existing.SetAnnotations(desired.GetAnnotations())
	existing.SetOwnerReferences(desired.GetOwnerReferences())
	existing.Object["spec"] = desired.Object["spec"]
	logger.Info("Updating SnippetsFilter", "namespace", httpRoute.Namespace, "name", filterName)
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("failed to update SnippetsFilter %s/%s: %w", httpRoute.Namespace, filterName, err)
	}
	return true, nil
}

// EnsureSnippetsFilterCopyForHTTPRoute copies a SnippetsFilter from source namespace into the HTTPRoute namespace.
// Returns true if the destination resource was created/updated and can be referenced safely.
func EnsureSnippetsFilterCopyForHTTPRoute(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	sourceNamespace string,
	destNamespace string,
	ingressNamespace string,
	ingressName string,
	filterName string,
	ownerHTTPRouteName string,
) (bool, error) {
	logger := log.FromContext(ctx)
	crdName, ok := snippetsCRDNameForKind(SnippetsFilterKind)
	if !ok {
		return false, nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return false, err
	}

	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: SnippetsFilterKind}
	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: sourceNamespace, Name: filterName}, source); err != nil {
		if apierrors.IsNotFound(err) {
			if destNamespace == "" {
				return false, nil
			}
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(gvk)
			if err := c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: filterName}, existing); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			if !IsManagedByUs(existing) {
				return false, nil
			}
			logger.Info("Deleting SnippetsFilter copy (source missing)",
				"namespace", destNamespace,
				"name", filterName)
			if err := c.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("failed to delete SnippetsFilter %s/%s: %w", destNamespace, filterName, err)
			}
			return false, nil
		}
		return false, err
	}

	if sourceNamespace == destNamespace {
		return true, nil
	}

	spec := map[string]interface{}{}
	if rawSpec, ok := source.Object["spec"]; ok {
		if specMap, ok := rawSpec.(map[string]interface{}); ok && specMap != nil {
			copiedSpec := runtime.DeepCopyJSON(specMap)
			if copiedSpec != nil {
				spec = copiedSpec
			}
		}
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(filterName)
	desired.SetNamespace(destNamespace)
	desired.SetLabels(copyStringMap(source.GetLabels()))
	annotations := copyStringMap(source.GetAnnotations())
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	desired.SetAnnotations(annotations)
	desired.Object["spec"] = spec

	if scheme != nil && ownerHTTPRouteName != "" {
		owner := &gatewayv1.HTTPRoute{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: ownerHTTPRouteName}, owner); err == nil {
			if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
				return false, err
			}
		}
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err = c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: filterName}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating SnippetsFilter copy",
				"namespace", destNamespace,
				"name", filterName)
			if err := c.Create(ctx, desired); err != nil {
				return false, fmt.Errorf("failed to create SnippetsFilter %s/%s: %w", destNamespace, filterName, err)
			}
			return true, nil
		}
		return false, err
	}

	if !IsManagedByUs(existing) {
		logger.Info("SnippetsFilter exists but is not managed by us, skipping copy",
			"namespace", destNamespace,
			"name", filterName)
		return false, nil
	}

	existing.SetLabels(desired.GetLabels())
	existing.SetAnnotations(desired.GetAnnotations())
	existing.SetOwnerReferences(desired.GetOwnerReferences())
	existing.Object["spec"] = spec
	logger.Info("Updating SnippetsFilter copy",
		"namespace", destNamespace,
		"name", filterName)
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("failed to update SnippetsFilter %s/%s: %w", destNamespace, filterName, err)
	}
	return true, nil
}

// ValidateSnippetsFilterExists ensures a SnippetsFilter exists in the given namespace.
func ValidateSnippetsFilterExists(ctx context.Context, c client.Reader, namespace, name string) error {
	crdName, ok := snippetsCRDNameForKind(SnippetsFilterKind)
	if !ok {
		return nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return err
	}
	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: SnippetsFilterKind}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("SnippetsFilter %s/%s not found", namespace, name)
		}
		return err
	}
	return nil
}

// EnsureExtensionResource copies an ExtensionRef resource from sourceNamespace into destNamespace.
// Returns true if the destination resource was created/updated and can be referenced safely.
func EnsureExtensionResource(
	ctx context.Context,
	c client.Client,
	kind string,
	name string,
	sourceNamespace string,
	destNamespace string,
	ingressNamespace string,
	ingressName string,
	targetRef map[string]interface{},
) (bool, error) {
	logger := log.FromContext(ctx)
	crdName, ok := extensionCRDNameForKind(kind)
	if !ok {
		return false, nil
	}
	version, ok, err := getCRDVersion(ctx, c, crdName)
	if err != nil || !ok {
		return false, err
	}

	gvk := schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: kind}
	source := &unstructured.Unstructured{}
	source.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, types.NamespacedName{Namespace: sourceNamespace, Name: name}, source); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if sourceNamespace == destNamespace {
		return true, nil
	}

	spec := map[string]interface{}{}
	if rawSpec, ok := source.Object["spec"]; ok {
		if specMap, ok := rawSpec.(map[string]interface{}); ok && specMap != nil {
			copiedSpec := runtime.DeepCopyJSON(specMap)
			if copiedSpec != nil {
				spec = copiedSpec
			}
		}
	}
	if targetRef != nil {
		if spec == nil {
			spec = map[string]interface{}{}
		}
		spec["targetRef"] = targetRef
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(name)
	desired.SetNamespace(destNamespace)
	desired.SetLabels(copyStringMap(source.GetLabels()))
	annotations := copyStringMap(source.GetAnnotations())
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	annotations[SourceAnnotation] = fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	desired.SetAnnotations(annotations)
	desired.Object["spec"] = spec

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err = c.Get(ctx, types.NamespacedName{Namespace: destNamespace, Name: name}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating Snippets resource copy",
				"kind", kind,
				"namespace", destNamespace,
				"name", name)
			if err := c.Create(ctx, desired); err != nil {
				return false, fmt.Errorf("failed to create %s %s/%s: %w", kind, destNamespace, name, err)
			}
			return true, nil
		}
		return false, err
	}

	if !IsManagedByUs(existing) {
		logger.Info("Snippets resource exists but is not managed by us, skipping copy",
			"kind", kind,
			"namespace", destNamespace,
			"name", name)
		return false, nil
	}

	existing.SetLabels(desired.GetLabels())
	existing.SetAnnotations(desired.GetAnnotations())
	existing.Object["spec"] = spec
	logger.Info("Updating Snippets resource copy",
		"kind", kind,
		"namespace", destNamespace,
		"name", name)
	if err := c.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("failed to update %s %s/%s: %w", kind, destNamespace, name, err)
	}
	return true, nil
}

// AddExtensionRefFilterAfterExistingKind inserts an ExtensionRef filter after the last filter of the same kind.
func AddExtensionRefFilterAfterExistingKind(
	httpRoute *gatewayv1.HTTPRoute,
	group string,
	kind string,
	name string,
) {
	if httpRoute == nil || name == "" {
		return
	}

	kindValue := gatewayv1.Kind(kind)
	for i := range httpRoute.Spec.Rules {
		rule := &httpRoute.Spec.Rules[i]
		exists := false
		lastIdx := -1
		for idx, filter := range rule.Filters {
			if filter.Type != gatewayv1.HTTPRouteFilterExtensionRef || filter.ExtensionRef == nil {
				continue
			}
			if string(filter.ExtensionRef.Group) != group {
				continue
			}
			if string(filter.ExtensionRef.Kind) != kind {
				continue
			}
			if string(filter.ExtensionRef.Name) == name {
				exists = true
				break
			}
			lastIdx = idx
		}
		if exists {
			continue
		}

		ref := gatewayv1.LocalObjectReference{
			Group: gatewayv1.Group(group),
			Kind:  kindValue,
			Name:  gatewayv1.ObjectName(name),
		}
		newFilter := gatewayv1.HTTPRouteFilter{
			Type:         gatewayv1.HTTPRouteFilterExtensionRef,
			ExtensionRef: &ref,
		}

		if lastIdx == -1 || lastIdx == len(rule.Filters)-1 {
			rule.Filters = append(rule.Filters, newFilter)
			continue
		}

		rule.Filters = append(rule.Filters, gatewayv1.HTTPRouteFilter{})
		copy(rule.Filters[lastIdx+2:], rule.Filters[lastIdx+1:])
		rule.Filters[lastIdx+1] = newFilter
	}
}

// AddExtensionRefFilters appends ExtensionRef filters to every HTTPRoute rule.
func AddExtensionRefFilters(
	httpRoute *gatewayv1.HTTPRoute,
	group string,
	kind string,
	names []string,
) {
	if httpRoute == nil || len(names) == 0 {
		return
	}

	kindValue := gatewayv1.Kind(kind)

	for i := range httpRoute.Spec.Rules {
		rule := &httpRoute.Spec.Rules[i]
		existing := make(map[string]struct{})
		for _, filter := range rule.Filters {
			if filter.Type != gatewayv1.HTTPRouteFilterExtensionRef || filter.ExtensionRef == nil {
				continue
			}
			if string(filter.ExtensionRef.Group) != group {
				continue
			}
			if string(filter.ExtensionRef.Kind) != kind {
				continue
			}
			existing[string(filter.ExtensionRef.Name)] = struct{}{}
		}

		for _, name := range names {
			if _, ok := existing[name]; ok {
				continue
			}
			ref := gatewayv1.LocalObjectReference{
				Group: gatewayv1.Group(group),
				Kind:  kindValue,
				Name:  gatewayv1.ObjectName(name),
			}
			rule.Filters = append(rule.Filters, gatewayv1.HTTPRouteFilter{
				Type:         gatewayv1.HTTPRouteFilterExtensionRef,
				ExtensionRef: &ref,
			})
		}
	}
}

func snippetsCRDNameForKind(kind string) (string, bool) {
	return extensionCRDNameForKind(kind)
}

func extensionCRDNameForKind(kind string) (string, bool) {
	switch kind {
	case SnippetsFilterKind:
		return SnippetsFilterCRDName, true
	case SnippetsPolicyKind:
		return SnippetsPolicyCRDName, true
	case AuthenticationFilterKind:
		return AuthenticationFilterCRDName, true
	case RequestHeaderModifierFilterKind:
		return RequestHeaderModifierCRDName, true
	case RateLimitPolicyKind:
		return RateLimitPolicyCRDName, true
	default:
		return "", false
	}
}

func getCRDVersion(ctx context.Context, c client.Reader, crdName string) (string, bool, error) {
	if cached, ok := crdVersionCache.Load(crdName); ok {
		entry := cached.(crdVersionCacheEntry)
		return entry.version, true, nil
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, types.NamespacedName{Name: crdName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}

	version, ok := pickCRDVersion(crd)
	if !ok {
		return "", false, nil
	}

	crdVersionCache.Store(crdName, crdVersionCacheEntry{version: version})
	return version, true, nil
}

// GetCRDVersion returns the storage/served version for a CRD name if installed.
func GetCRDVersion(ctx context.Context, c client.Reader, crdName string) (string, bool, error) {
	return getCRDVersion(ctx, c, crdName)
}

func pickCRDVersion(crd *apiextensionsv1.CustomResourceDefinition) (string, bool) {
	for _, version := range crd.Spec.Versions {
		if version.Storage {
			return version.Name, true
		}
	}
	for _, version := range crd.Spec.Versions {
		if version.Served {
			return version.Name, true
		}
	}
	return "", false
}

func copyStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// SnippetsFilterGVK returns the GroupVersionKind used for SnippetsFilter watches.
func SnippetsFilterGVK() schema.GroupVersionKind {
	return ExtensionFilterGVK(SnippetsFilterKind, SnippetsFilterCRDName)
}

func ExtensionFilterGVK(kind, crdName string) schema.GroupVersionKind {
	version := "v1"
	if cached, ok := crdVersionCache.Load(crdName); ok {
		entry := cached.(crdVersionCacheEntry)
		if entry.version != "" {
			version = entry.version
		}
	}
	return schema.GroupVersionKind{Group: NginxGatewayGroup, Version: version, Kind: kind}
}
