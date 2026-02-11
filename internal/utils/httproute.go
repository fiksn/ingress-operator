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
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	MaxHTTPRouteRules = 16 // Gateway API limit
)

// HTTPRouteManager handles HTTPRoute operations
type HTTPRouteManager struct {
	Client client.Client
}

// GetHTTPRoutesWithPrefix returns all HTTPRoutes with a given name prefix
func (m *HTTPRouteManager) GetHTTPRoutesWithPrefix(
	ctx context.Context,
	namespace string,
	prefix string,
) ([]gatewayv1.HTTPRoute, error) {
	routeList := &gatewayv1.HTTPRouteList{}

	if err := m.Client.List(ctx, routeList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	var result []gatewayv1.HTTPRoute
	for _, r := range routeList.Items {
		if strings.HasPrefix(r.Name, prefix) {
			result = append(result, r)
		}
	}

	return result, nil
}

// SplitHTTPRouteIfNeeded splits an HTTPRoute into multiple routes if it exceeds the Gateway API limit
func (m *HTTPRouteManager) SplitHTTPRouteIfNeeded(httpRoute *gatewayv1.HTTPRoute) []*gatewayv1.HTTPRoute {
	// If the HTTPRoute has <= MaxHTTPRouteRules, no split needed
	if len(httpRoute.Spec.Rules) <= MaxHTTPRouteRules {
		return []*gatewayv1.HTTPRoute{httpRoute}
	}

	logger := log.Log.WithName("SplitHTTPRouteIfNeeded")
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

		if partNum > 1 {
			part.Name = fmt.Sprintf("%s-%d", httpRoute.Name, partNum)
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

// ApplyHTTPRoutesAtomic handles applying HTTPRoutes with proper cleanup of obsolete split routes
// If there's only one HTTPRoute before and after, it does an atomic update
// If the count changed, it deletes all old HTTPRoutes first, then creates all new ones
func (m *HTTPRouteManager) ApplyHTTPRoutesAtomic(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	desiredRoutes []*gatewayv1.HTTPRoute,
	metricRecorder func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	// Get existing HTTPRoutes for this Ingress
	existingRoutes, err := m.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return fmt.Errorf("failed to get existing HTTPRoutes: %w", err)
	}

	existingCount := len(existingRoutes)
	desiredCount := len(desiredRoutes)

	logger.V(3).Info("HTTPRoute reconciliation",
		"ingress", ingress.Name,
		"existingCount", existingCount,
		"desiredCount", desiredCount)

	// Case 1: Single HTTPRoute before and after - atomic update
	if existingCount == 1 && desiredCount == 1 {
		logger.V(3).Info("Single HTTPRoute case - performing atomic update")
		return m.applyHTTPRoute(ctx, desiredRoutes[0], metricRecorder)
	}

	// Case 2: Count changed - delete all old, create all new
	logger.Info("HTTPRoute count changed - performing atomic replacement",
		"ingress", ingress.Name,
		"from", existingCount,
		"to", desiredCount)

	// First, delete all existing HTTPRoutes that we manage for this ingress
	for _, existingRoute := range existingRoutes {
		if IsManagedByUsForIngress(&existingRoute, ingress.Namespace, ingress.Name) {
			logger.Info("Deleting obsolete HTTPRoute",
				"namespace", existingRoute.Namespace,
				"name", existingRoute.Name)
			if err := m.Client.Delete(ctx, &existingRoute); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete obsolete HTTPRoute %s: %w", existingRoute.Name, err)
			}
			if metricRecorder != nil {
				metricRecorder("delete", existingRoute.Namespace, existingRoute.Name)
			}
		} else {
			logger.Info("Skipping deletion of HTTPRoute - not managed by us for this Ingress",
				"namespace", existingRoute.Namespace,
				"name", existingRoute.Name)
		}
	}

	// Then, create all new HTTPRoutes
	for _, desiredRoute := range desiredRoutes {
		if err := m.applyHTTPRoute(ctx, desiredRoute, metricRecorder); err != nil {
			return fmt.Errorf("failed to apply HTTPRoute %s: %w", desiredRoute.Name, err)
		}
	}

	logger.Info("HTTPRoute atomic replacement completed successfully",
		"ingress", ingress.Name,
		"routesCreated", desiredCount)

	return nil
}

// applyHTTPRoute creates or updates a single HTTPRoute
func (m *HTTPRouteManager) applyHTTPRoute(
	ctx context.Context,
	httpRoute *gatewayv1.HTTPRoute,
	metricRecorder func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	httpRouteNN := types.NamespacedName{
		Namespace: httpRoute.Namespace,
		Name:      httpRoute.Name,
	}
	existingHTTPRoute := &gatewayv1.HTTPRoute{}
	canManage, err := CanUpdateResource(ctx, m.Client, existingHTTPRoute, httpRouteNN)
	if err != nil {
		return err
	}

	if !canManage {
		logger.Info("Skipping HTTPRoute synthesis - resource exists and is not managed by us",
			"namespace", httpRouteNN.Namespace, "name", httpRouteNN.Name)
		return nil
	}

	// Check if HTTPRoute exists
	err = m.Client.Get(ctx, httpRouteNN, existingHTTPRoute)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new HTTPRoute
			logger.Info("Creating HTTPRoute", "namespace", httpRoute.Namespace, "name", httpRoute.Name)
			if err := m.Client.Create(ctx, httpRoute); err != nil {
				return fmt.Errorf("failed to create HTTPRoute: %w", err)
			}
			if metricRecorder != nil {
				metricRecorder("create", httpRoute.Namespace, httpRoute.Name)
			}
			return nil
		}
		return err
	}

	// Update existing HTTPRoute
	existingHTTPRoute.Annotations = httpRoute.Annotations
	existingHTTPRoute.Spec = httpRoute.Spec
	logger.Info("Updating HTTPRoute", "namespace", existingHTTPRoute.Namespace, "name", existingHTTPRoute.Name)
	if err := m.Client.Update(ctx, existingHTTPRoute); err != nil {
		return fmt.Errorf("failed to update HTTPRoute: %w", err)
	}
	if metricRecorder != nil {
		metricRecorder("update", existingHTTPRoute.Namespace, existingHTTPRoute.Name)
	}

	return nil
}

// ResolveNamedPorts resolves named ports in HTTPRoute by looking up the actual Services
func (m *HTTPRouteManager) ResolveNamedPorts(
	ctx context.Context,
	ingress *networkingv1.Ingress,
	httpRoute *gatewayv1.HTTPRoute,
) error {
	logger := log.FromContext(ctx)

	for i := range httpRoute.Spec.Rules {
		for j := range httpRoute.Spec.Rules[i].BackendRefs {
			backendRef := &httpRoute.Spec.Rules[i].BackendRefs[j]

			// Check if port is 0 (indicates named port)
			if backendRef.Port != nil && *backendRef.Port == 0 {
				serviceName := string(backendRef.Name)

				// Find the named port from the Ingress spec
				portName := m.findPortNameInIngress(ingress, serviceName)
				if portName == "" {
					logger.Info("Could not find port name in Ingress for service, using fallback port 80",
						"service", serviceName,
						"namespace", ingress.Namespace)
					fallbackPort := int32(80)
					backendRef.Port = &fallbackPort
					continue
				}

				// Look up the Service to resolve the named port
				resolvedPort, err := m.resolveServicePort(ctx, ingress.Namespace, serviceName, portName)
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
func (m *HTTPRouteManager) findPortNameInIngress(ingress *networkingv1.Ingress, serviceName string) string {
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
func (m *HTTPRouteManager) resolveServicePort(ctx context.Context, namespace, serviceName, portName string) (int32, error) {
	var service corev1.Service
	err := m.Client.Get(ctx, types.NamespacedName{
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
