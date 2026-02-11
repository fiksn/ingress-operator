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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ManagedByAnnotation = "ingress-operator.fiction.si/managed-by"
	ManagedByValue      = "ingress-controller"
	SourceAnnotation    = "ingress-operator.fiction.si/source"
)

// IsManagedByUs checks if a resource is managed by the ingress operator
func IsManagedByUs(obj client.Object) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	return annotations[ManagedByAnnotation] == ManagedByValue
}

// IsManagedByUsForIngress checks if a resource is managed by the ingress operator
// AND was created for the specified ingress (exact match for single-source resources)
func IsManagedByUsForIngress(obj client.Object, ingressNamespace, ingressName string) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}

	// First check if it's managed by us
	if annotations[ManagedByAnnotation] != ManagedByValue {
		return false
	}

	// Then check if the source matches
	expectedSource := fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	return annotations[SourceAnnotation] == expectedSource
}

// IsManagedByUsWithIngress checks if a resource is managed by the ingress operator
// AND includes the specified ingress in its source list (for multi-source resources like shared Gateways)
func IsManagedByUsWithIngress(obj client.Object, ingressNamespace, ingressName string) bool {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}

	// First check if it's managed by us
	if annotations[ManagedByAnnotation] != ManagedByValue {
		return false
	}

	// Check if the ingress is in the comma-separated source list
	expectedSource := fmt.Sprintf("%s/%s", ingressNamespace, ingressName)
	sourceAnnotation := annotations[SourceAnnotation]
	if sourceAnnotation == "" {
		return false
	}

	// Check for exact match (single source) or presence in comma-separated list
	for _, source := range splitSources(sourceAnnotation) {
		if source == expectedSource {
			return true
		}
	}
	return false
}

// splitSources splits a comma-separated source annotation into individual sources
func splitSources(sources string) []string {
	var result []string
	for _, s := range strings.Split(sources, ",") {
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// CanUpdateResource checks if we can create or update a resource
// Returns true if the resource doesn't exist or is managed by us
func CanUpdateResource(
	ctx context.Context,
	c client.Client,
	obj client.Object,
	namespacedName types.NamespacedName,
) (bool, error) {
	logger := log.FromContext(ctx)

	// Try to get the existing resource
	err := c.Get(ctx, namespacedName, obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Resource doesn't exist, we can create it
			return true, nil
		}
		// Some other error occurred
		return false, err
	}

	// Resource exists, check if it's managed by us
	if !IsManagedByUs(obj) {
		logger.Info("Resource exists but is not managed by us, skipping",
			"resource", obj.GetObjectKind().GroupVersionKind().Kind,
			"namespace", namespacedName.Namespace,
			"name", namespacedName.Name)
		return false, nil
	}

	// Resource exists and is managed by us, we can update it
	return true, nil
}
