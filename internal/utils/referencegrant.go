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

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/fiksn/ingress-doperator/internal/translator"
)

// EnsureReferenceGrants creates ReferenceGrants for the given Ingresses
func EnsureReferenceGrants(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	trans *translator.Translator,
	ingresses []networkingv1.Ingress,
	recordMetric func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	// Get namespaces that need ReferenceGrants using translator
	namespacesWithTLS := trans.GetNamespacesWithTLS(ingresses)

	// Create ReferenceGrant in each namespace
	for _, namespace := range namespacesWithTLS {
		if err := ApplyReferenceGrant(ctx, c, scheme, trans, namespace, recordMetric); err != nil {
			logger.Error(err, "failed to apply ReferenceGrant", "namespace", namespace)
			return err
		}
	}

	return nil
}

// CleanupReferenceGrantIfNeeded removes the ingress from ReferenceGrant source list
// and deletes the ReferenceGrant if no sources remain
func CleanupReferenceGrantIfNeeded(
	ctx context.Context,
	c client.Client,
	ingressNamespace string,
	ingressName string,
) error {
	logger := log.FromContext(ctx)

	// Get the ReferenceGrant
	refGrant := &gatewayv1beta1.ReferenceGrant{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: ingressNamespace,
		Name:      translator.ReferenceGrantName,
	}, refGrant)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted, nothing to do
			return nil
		}
		return err
	}

	// Check if this ReferenceGrant was created for/includes this specific Ingress
	if !IsManagedByUsWithIngress(refGrant, ingressNamespace, ingressName) {
		logger.V(3).Info("ReferenceGrant not managed by us for this Ingress, skipping",
			"namespace", ingressNamespace,
			"name", translator.ReferenceGrantName,
			"ingress", ingressName)
		return nil
	}

	// Remove this ingress from the source list
	currentSource := refGrant.Annotations[SourceAnnotation]
	ingressSource := fmt.Sprintf("%s/%s", ingressNamespace, ingressName)

	sources := splitSources(currentSource)
	newSources := make([]string, 0, len(sources))
	for _, source := range sources {
		if source != ingressSource {
			newSources = append(newSources, source)
		}
	}

	// If no sources remain, delete the ReferenceGrant
	if len(newSources) == 0 {
		logger.Info("Deleting ReferenceGrant (no source Ingresses remain)",
			"namespace", ingressNamespace, "name", translator.ReferenceGrantName)
		if err := c.Delete(ctx, refGrant); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete ReferenceGrant: %w", err)
		}
		return nil
	}

	// Update source annotation with remaining sources
	newSource := strings.Join(newSources, ",")
	refGrant.Annotations[SourceAnnotation] = newSource
	logger.Info("Updating ReferenceGrant source annotation",
		"namespace", ingressNamespace,
		"name", translator.ReferenceGrantName,
		"removedIngress", ingressName,
		"remainingSources", newSource)
	if err := c.Update(ctx, refGrant); err != nil {
		return fmt.Errorf("failed to update ReferenceGrant: %w", err)
	}

	return nil
}

// ApplyReferenceGrant creates or updates a ReferenceGrant in the given namespace
func ApplyReferenceGrant(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	trans *translator.Translator,
	ingressNamespace string,
	recordMetric func(operation, namespace, name string),
) error {
	logger := log.FromContext(ctx)

	// Get all Ingresses with TLS in this namespace to track sources
	var ingressList networkingv1.IngressList
	if err := c.List(ctx, &ingressList, client.InNamespace(ingressNamespace)); err != nil {
		return fmt.Errorf("failed to list Ingresses: %w", err)
	}

	// Filter to only Ingresses with TLS
	ingressesWithTLS := make([]networkingv1.Ingress, 0)
	for _, ingress := range ingressList.Items {
		if len(ingress.Spec.TLS) > 0 {
			ingressesWithTLS = append(ingressesWithTLS, ingress)
		}
	}

	// Create ReferenceGrant using translator with source tracking
	refGrant := trans.CreateReferenceGrant(ingressNamespace, ingressesWithTLS)
	if scheme != nil && len(ingressesWithTLS) == 1 {
		owner := &gatewayv1.HTTPRoute{}
		ownerName := ingressesWithTLS[0].Name
		if err := c.Get(ctx, types.NamespacedName{Namespace: ingressNamespace, Name: ownerName}, owner); err == nil {
			if err := controllerutil.SetControllerReference(owner, refGrant, scheme); err != nil {
				return err
			}
		}
	}

	// Check if ReferenceGrant exists
	existing := &gatewayv1beta1.ReferenceGrant{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: ingressNamespace,
		Name:      translator.ReferenceGrantName,
	}, existing)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new ReferenceGrant
			logger.Info("Creating ReferenceGrant", "namespace", ingressNamespace, "name", translator.ReferenceGrantName)
			if err := c.Create(ctx, refGrant); err != nil {
				return fmt.Errorf("failed to create ReferenceGrant: %w", err)
			}
			// Record metric if callback provided
			if recordMetric != nil {
				recordMetric("create", ingressNamespace, translator.ReferenceGrantName)
			}
			return nil
		}
		return err
	}

	// Check if we manage this ReferenceGrant by verifying if ANY of the ingresses
	// we found are in the existing source list
	if !IsManagedByUs(existing) {
		logger.Info("ReferenceGrant exists but is not managed by us (no managed-by annotation), skipping",
			"namespace", ingressNamespace, "name", translator.ReferenceGrantName)
		return nil
	}

	// Verify at least one of our ingresses is in the existing source list
	existingSources := splitSources(existing.Annotations[SourceAnnotation])
	newSources := splitSources(refGrant.Annotations[SourceAnnotation])

	hasCommonSource := false
	for _, newSrc := range newSources {
		for _, existingSrc := range existingSources {
			if newSrc == existingSrc {
				hasCommonSource = true
				break
			}
		}
		if hasCommonSource {
			break
		}
	}

	if !hasCommonSource && len(existingSources) > 0 {
		logger.Info("ReferenceGrant managed by us but for different ingresses, skipping",
			"namespace", ingressNamespace,
			"name", translator.ReferenceGrantName,
			"existingSources", existing.Annotations[SourceAnnotation],
			"newSources", refGrant.Annotations[SourceAnnotation])
		return nil
	}

	// Update the ReferenceGrant with the complete current list
	existing.Spec = refGrant.Spec
	existing.Annotations = refGrant.Annotations // Update annotations including source
	logger.Info("Updating ReferenceGrant", "namespace", ingressNamespace, "name", translator.ReferenceGrantName)
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update ReferenceGrant: %w", err)
	}
	// Record metric if callback provided
	if recordMetric != nil {
		recordMetric("update", ingressNamespace, translator.ReferenceGrantName)
	}

	return nil
}
