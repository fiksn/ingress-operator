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

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/fiksn/ingress-operator/internal/translator"
)

const (
	IgnoreIngressAnnotation = "ingress-operator.fiction.si/ignore-ingress"
	AllowIngressAnnotation  = "ingress-operator.fiction.si/allow-ingress"
	WebhookAnnotation       = "ingress-operator.fiction.si/webhook"
)

// +kubebuilder:webhook:path=/mutate-v1-ingress,mutating=true,failurePolicy=ignore,groups="networking.k8s.io",resources=ingresses,verbs=create;update,versions=v1,name=mingress.fiction.si,admissionReviewVersions=v1,sideEffects=None

// IngressMutator handles Ingress mutations
type IngressMutator struct {
	Client     client.Client
	decoder    admission.Decoder
	Translator *translator.Translator
}

// Handle performs the mutation
func (m *IngressMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)
	logger.Info("Webhook called", "namespace", req.Namespace, "name", req.Name)

	ingress := &networkingv1.Ingress{}
	err := m.decoder.Decode(req, ingress)
	if err != nil {
		logger.Error(err, "failed to decode ingress")
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if this Ingress should be ignored (skip all processing)
	if ingress.Annotations != nil && ingress.Annotations[IgnoreIngressAnnotation] == fmt.Sprintf("%t", true) {
		logger.Info("Ingress has ignore annotation, skipping mutation")
		return admission.Allowed("ignored")
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
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Create the Gateway resource
	if err := m.Client.Create(ctx, gateway); err != nil {
		// If already exists, update it
		if err := m.Client.Update(ctx, gateway); err != nil {
			logger.Error(err, "failed to create/update Gateway")
			// Don't fail the admission, just log
		} else {
			logger.Info("Updated Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
		}
	} else {
		logger.Info("Created Gateway", "name", gateway.Name, "namespace", gateway.Namespace)
	}

	// Create the HTTPRoute resource
	if err := m.Client.Create(ctx, httpRoute); err != nil {
		// If already exists, update it
		if err := m.Client.Update(ctx, httpRoute); err != nil {
			logger.Error(err, "failed to create/update HTTPRoute")
			// Don't fail the admission, just log
		} else {
			logger.Info("Updated HTTPRoute", "name", httpRoute.Name, "namespace", httpRoute.Namespace)
		}
	} else {
		logger.Info("Created HTTPRoute", "name", httpRoute.Name, "namespace", httpRoute.Namespace)
	}

	// By default, reject the Ingress after creating Gateway/HTTPRoute
	// This prevents the Ingress from being stored in the cluster
	if !allowIngress {
		logger.Info("Rejecting Ingress creation (Gateway/HTTPRoute created instead)",
			"ingress", ingress.Name,
			"gateway", gateway.Name,
			"httpRoute", httpRoute.Name)
		return admission.Denied("Ingress resources are not allowed - Gateway and HTTPRoute have been created instead. " +
			"Use annotation 'ingress-operator.fiction.si/allow-ingress=true' to allow Ingress creation.")
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
