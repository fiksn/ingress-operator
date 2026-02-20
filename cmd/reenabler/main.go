/*
Copyright 2026.

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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap/zapcore"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fiksn/ingress-doperator/internal/controller"
	"github.com/fiksn/ingress-doperator/internal/utils"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

type trackedBool struct {
	value bool
	set   bool
}

func (b *trackedBool) Set(s string) error {
	b.set = true
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	b.value = v
	return nil
}

func (b *trackedBool) String() string {
	return strconv.FormatBool(b.value)
}

func (b *trackedBool) IsBoolFlag() bool { return true }

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	var namespace string
	var verbosity int
	var removeDerivedResources bool
	var restore trackedBool
	var restoreClass trackedBool
	var restoreExternalDNS trackedBool
	var dangerouslyDeleteIngresses bool
	var preventFurtherReconciliation bool

	flag.StringVar(&namespace, "namespace", "", "If set, only process Ingresses in this namespace")
	flag.BoolVar(&removeDerivedResources, "remove-derived-resources", false,
		"If true, remove managed HTTPRoutes and automatic SnippetsFilters derived from the Ingress")
	restore.value = true
	flag.Var(&restore, "restore",
		"If true, restore both ingress class and external-dns annotations")
	flag.Var(&restoreClass, "restore-class",
		"If true, restore ingress class settings saved by ingress-doperator")
	flag.Var(&restoreExternalDNS, "restore-external-dns",
		"If true, restore external-dns annotations saved by ingress-doperator")
	flag.BoolVar(&dangerouslyDeleteIngresses, "dangerously-delete-ingresses", false,
		"If true, delete disabled Ingresses managed by ingress-doperator")
	flag.BoolVar(&preventFurtherReconciliation, "prevent-further-reconciliation", false,
		"If true, mark restored Ingresses as disabled to stop future reconciles")
	flag.IntVar(&verbosity, "v", 0, "Log verbosity (0 = info, higher = more verbose)")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if verbosity > 0 {
		opts.Development = false
		opts.Level = zapcore.Level(-verbosity)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if dangerouslyDeleteIngresses {
		if restoreClass.set || restoreExternalDNS.set {
			setupLog.Error(fmt.Errorf("invalid flag combination"),
				"--dangerously-delete-ingresses cannot be combined with restore flags")
			os.Exit(1)
		}
		if removeDerivedResources {
			setupLog.Error(fmt.Errorf("invalid flag combination"),
				"--dangerously-delete-ingresses cannot be combined with --remove-derived-resources")
			os.Exit(1)
		}
		restore.value = false
	}

	if removeDerivedResources && (restoreClass.set || restoreExternalDNS.set || restore.value) {
		setupLog.Error(fmt.Errorf("invalid flag combination"),
			"--remove-derived-resources cannot be combined with restore flags")
		os.Exit(1)
	}

	if restoreClass.set || restoreExternalDNS.set {
		restore.value = false
	}

	if restore.value {
		restoreClass.value = true
		restoreExternalDNS.value = true
	}

	cfg := ctrl.GetConfigOrDie()
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	ctx := context.Background()
	if err := runReenabler(
		ctx,
		cli,
		namespace,
		removeDerivedResources,
		restoreClass.value,
		restoreExternalDNS.value,
		dangerouslyDeleteIngresses,
		preventFurtherReconciliation,
	); err != nil {
		setupLog.Error(err, "reenabler failed")
		os.Exit(1)
	}
}

func runReenabler(
	ctx context.Context,
	cli client.Client,
	namespace string,
	removeDerivedResources bool,
	restoreClass bool,
	restoreExternalDNS bool,
	dangerouslyDeleteIngresses bool,
	preventFurtherReconciliation bool,
) error {
	opts := reenablerOptions{
		removeDerivedResources:       removeDerivedResources,
		restoreClass:                 restoreClass,
		restoreExternalDNS:           restoreExternalDNS,
		dangerouslyDeleteIngresses:   dangerouslyDeleteIngresses,
		preventFurtherReconciliation: preventFurtherReconciliation,
	}

	ingresses, err := listIngresses(ctx, cli, namespace)
	if err != nil {
		return err
	}

	manager := utils.HTTPRouteManager{Client: cli}

	if opts.dangerouslyDeleteIngresses {
		if err := preflightDelete(ctx, cli, &manager, ingresses); err != nil {
			return err
		}
	}

	for i := range ingresses {
		ingress := &ingresses[i]
		if err := processIngress(ctx, cli, &manager, ingress, opts); err != nil {
			setupLog.Error(err, "failed to process ingress",
				"namespace", ingress.Namespace,
				"name", ingress.Name)
		}
	}

	return nil
}

type reenablerOptions struct {
	removeDerivedResources       bool
	restoreClass                 bool
	restoreExternalDNS           bool
	dangerouslyDeleteIngresses   bool
	preventFurtherReconciliation bool
}

func listIngresses(ctx context.Context, cli client.Client, namespace string) ([]networkingv1.Ingress, error) {
	list := &networkingv1.IngressList{}
	if namespace != "" {
		if err := cli.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
	} else {
		if err := cli.List(ctx, list); err != nil {
			return nil, err
		}
	}
	return list.Items, nil
}

func preflightDelete(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingresses []networkingv1.Ingress,
) error {
	for i := range ingresses {
		ingress := &ingresses[i]
		ok, reason, err := checkDeleteEligibility(ctx, cli, manager, ingress)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("refusing to delete ingress %s/%s: %s", ingress.Namespace, ingress.Name, reason)
		}
	}
	return nil
}

func processIngress(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
	opts reenablerOptions,
) error {
	disabled := isDisabledIngress(ingress)
	if opts.dangerouslyDeleteIngresses && shouldDeleteIngress(ingress) {
		return deleteIngressIfEligible(ctx, cli, manager, ingress)
	}
	if !shouldRestoreIngress(ingress, disabled, opts.restoreExternalDNS) {
		return nil
	}
	if opts.restoreExternalDNS &&
		ingress.Annotations != nil &&
		ingress.Annotations[controller.IngressDisabledAnnotation] == controller.IngressDisabledReasonExternalDNS {
		if err := disableExternalDNSForDerived(ctx, cli, manager, ingress); err != nil {
			return err
		}
	}
	if err := restoreIngressState(ctx, cli, ingress, disabled && opts.restoreClass, opts.restoreExternalDNS); err != nil {
		return err
	}
	if opts.preventFurtherReconciliation && (opts.restoreClass || opts.restoreExternalDNS) {
		if err := markIngressDisabled(ctx, cli, ingress, opts.restoreClass); err != nil {
			return err
		}
	}
	if disabled && opts.restoreClass && opts.removeDerivedResources {
		if err := removeManagedHTTPRoutes(ctx, manager, ingress); err != nil {
			return err
		}
		if err := removeAutomaticSnippetsFilter(ctx, cli, ingress); err != nil {
			return err
		}
		if err := removeManagedGatewaysIfEmpty(ctx, cli, ingress); err != nil {
			return err
		}
	} else if disabled && opts.restoreClass {
		setupLog.Info("Leaving derived resources in place (remove-derived-resources=false)",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
	}
	if disabled && opts.restoreClass {
		setupLog.Info("Re-enabled Ingress",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
	} else {
		setupLog.Info("Restored external-dns annotations for Ingress",
			"namespace", ingress.Namespace,
			"name", ingress.Name)
	}
	return nil
}

func markIngressDisabled(
	ctx context.Context,
	cli client.Client,
	ingress *networkingv1.Ingress,
	useNormal bool,
) error {
	if ingress == nil {
		return nil
	}
	updated := ingress.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	if useNormal {
		updated.Annotations[controller.IngressDisabledAnnotation] = controller.IngressDisabledReasonNormal
	} else {
		updated.Annotations[controller.IngressDisabledAnnotation] = controller.IngressDisabledReasonExternalDNS
	}
	return cli.Update(ctx, updated)
}

func deleteIngressIfEligible(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) error {
	ok, reason, err := checkDeleteEligibility(ctx, cli, manager, ingress)
	if err != nil {
		return err
	}
	if !ok {
		setupLog.Info("Skipping ingress deletion due to failed checks",
			"namespace", ingress.Namespace,
			"name", ingress.Name,
			"reason", reason)
		return nil
	}
	if err := cli.Delete(ctx, ingress); err != nil {
		return err
	}
	setupLog.Info("Deleted disabled Ingress",
		"namespace", ingress.Namespace,
		"name", ingress.Name)
	return nil
}

func shouldRestoreIngress(ingress *networkingv1.Ingress, disabled bool, restoreExternalDNS bool) bool {
	if disabled {
		return true
	}
	if !restoreExternalDNS {
		return false
	}
	return needsExternalDNSRestore(ingress)
}

func isDisabledIngress(ingress *networkingv1.Ingress) bool {
	if ingress == nil {
		return false
	}
	if ingress.Annotations == nil {
		return false
	}
	if ingress.Annotations[controller.IngressDisabledAnnotation] == "" {
		return false
	}
	if ingress.Spec.IngressClassName != nil &&
		*ingress.Spec.IngressClassName == controller.DisabledIngressClassName {
		return true
	}
	if ingress.Annotations[controller.IngressClassAnnotation] == controller.DisabledIngressClassName {
		return true
	}
	return false
}

func restoreIngressState(
	ctx context.Context,
	cli client.Client,
	ingress *networkingv1.Ingress,
	restoreClass bool,
	restoreExternalDNS bool,
) error {
	if ingress == nil {
		return nil
	}
	updated := ingress.DeepCopy()
	annotations := updated.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}

	modified := false
	if restoreClass {
		originalClassName := annotations[controller.OriginalIngressClassNameAnnotation]
		originalClassAnnotation := annotations[controller.OriginalIngressClassAnnotation]

		if originalClassName != "" {
			updated.Spec.IngressClassName = &originalClassName
		} else {
			updated.Spec.IngressClassName = nil
		}

		if originalClassAnnotation != "" {
			annotations[controller.IngressClassAnnotation] = originalClassAnnotation
		} else {
			delete(annotations, controller.IngressClassAnnotation)
		}

		delete(annotations, controller.IngressDisabledAnnotation)
		delete(annotations, controller.OriginalIngressClassNameAnnotation)
		delete(annotations, controller.OriginalIngressClassAnnotation)
		modified = true
	}

	if restoreExternalDNS {
		if originalHostname, ok := annotations[controller.OriginalExternalDNSHostname]; ok {
			if originalHostname != "" {
				annotations[controller.ExternalDNSHostnameAnnotation] = originalHostname
			} else {
				delete(annotations, controller.ExternalDNSHostnameAnnotation)
			}
			delete(annotations, controller.OriginalExternalDNSHostname)
			modified = true
		}

		if originalSource, ok := annotations[controller.OriginalExternalDNSIngressHostnameSource]; ok {
			if originalSource != "" {
				annotations[controller.ExternalDNSIngressHostnameSource] = originalSource
			} else {
				delete(annotations, controller.ExternalDNSIngressHostnameSource)
			}
			delete(annotations, controller.OriginalExternalDNSIngressHostnameSource)
			modified = true
		} else if _, exists := annotations[controller.ExternalDNSIngressHostnameSource]; exists {
			delete(annotations, controller.ExternalDNSIngressHostnameSource)
			modified = true
		}

		if annotations[controller.IngressDisabledAnnotation] == controller.IngressDisabledReasonExternalDNS {
			delete(annotations, controller.IngressDisabledAnnotation)
			modified = true
		}
	}

	updated.Annotations = annotations
	if !modified {
		return nil
	}
	return cli.Update(ctx, updated)
}

func needsExternalDNSRestore(ingress *networkingv1.Ingress) bool {
	if ingress == nil || ingress.Annotations == nil {
		return false
	}
	if _, ok := ingress.Annotations[controller.OriginalExternalDNSHostname]; ok {
		return true
	}
	if _, ok := ingress.Annotations[controller.OriginalExternalDNSIngressHostnameSource]; ok {
		return true
	}
	_, ok := ingress.Annotations[controller.ExternalDNSIngressHostnameSource]
	return ok
}

func shouldDeleteIngress(ingress *networkingv1.Ingress) bool {
	if ingress == nil || ingress.Annotations == nil {
		return false
	}
	disabledValue := ingress.Annotations[controller.IngressDisabledAnnotation]
	if disabledValue == "" {
		return false
	}
	switch disabledValue {
	case controller.IngressDisabledReasonNormal:
		return isDisabledIngress(ingress)
	case controller.IngressDisabledReasonExternalDNS:
		if ingress.Annotations[controller.ExternalDNSIngressHostnameSource] == "" {
			return false
		}
		if _, ok := ingress.Annotations[controller.OriginalExternalDNSHostname]; ok {
			return true
		}
		if _, ok := ingress.Annotations[controller.OriginalExternalDNSIngressHostnameSource]; ok {
			return true
		}
		return false
	default:
		return false
	}
}

func checkDeleteEligibility(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) (bool, string, error) {
	if ingress == nil {
		return false, "missing ingress", nil
	}
	if ingress.Annotations == nil || ingress.Annotations[controller.IngressDisabledAnnotation] == "" {
		return false, "missing ingress-doperator disabled annotation", nil
	}
	if !shouldDeleteIngress(ingress) {
		return false, "disabled annotation does not satisfy delete criteria", nil
	}

	hasRoute, hasGateway, err := hasManagedResources(ctx, cli, manager, ingress)
	if err != nil {
		return false, "", err
	}
	if !hasRoute && !hasGateway {
		return false, "missing managed HTTPRoute and Gateway", nil
	}
	if !hasRoute {
		return false, "missing managed HTTPRoute", nil
	}
	if !hasGateway {
		return false, "missing managed Gateway", nil
	}
	return true, "", nil
}

func removeManagedHTTPRoutes(
	ctx context.Context,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) error {
	if manager == nil || ingress == nil {
		return nil
	}
	routes, err := manager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return err
	}

	for _, route := range routes {
		httpRoute := route
		if !utils.IsManagedByUsForIngress(&httpRoute, ingress.Namespace, ingress.Name) {
			continue
		}
		if err := manager.Client.Delete(ctx, &httpRoute); err != nil {
			return err
		}
	}

	return nil
}

func removeManagedGatewaysIfEmpty(ctx context.Context, cli client.Client, ingress *networkingv1.Ingress) error {
	if ingress == nil {
		return nil
	}

	allRoutes := &gatewayv1.HTTPRouteList{}
	if err := cli.List(ctx, allRoutes); err != nil {
		return err
	}
	parentCounts := map[string]int{}
	for i := range allRoutes.Items {
		route := &allRoutes.Items[i]
		for _, parent := range route.Spec.ParentRefs {
			key, ok := parentRefKey(route.Namespace, parent)
			if !ok {
				continue
			}
			parentCounts[key]++
		}
	}

	gateways := &gatewayv1.GatewayList{}
	if err := cli.List(ctx, gateways); err != nil {
		return err
	}
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if !utils.IsManagedByUsWithIngress(gateway, ingress.Namespace, ingress.Name) {
			continue
		}
		key := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)
		if parentCounts[key] > 0 {
			continue
		}
		if err := cli.Delete(ctx, gateway); err != nil {
			return err
		}
		setupLog.Info("Deleted managed Gateway without HTTPRoutes",
			"namespace", gateway.Namespace,
			"name", gateway.Name)
	}

	return nil
}

func hasManagedResources(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) (bool, bool, error) {
	if manager == nil || ingress == nil {
		return false, false, nil
	}
	routes, err := manager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return false, false, err
	}
	hasRoute := false
	for _, route := range routes {
		httpRoute := route
		if utils.IsManagedByUsForIngress(&httpRoute, ingress.Namespace, ingress.Name) {
			hasRoute = true
			break
		}
	}
	if !hasRoute {
		return false, false, nil
	}

	gateways := &gatewayv1.GatewayList{}
	if err := cli.List(ctx, gateways); err != nil {
		return false, false, err
	}
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if utils.IsManagedByUsWithIngress(gateway, ingress.Namespace, ingress.Name) {
			return true, true, nil
		}
	}

	return hasRoute, false, nil
}

func disableExternalDNSForDerived(
	ctx context.Context,
	cli client.Client,
	manager *utils.HTTPRouteManager,
	ingress *networkingv1.Ingress,
) error {
	if ingress == nil || manager == nil {
		return nil
	}
	routes, err := manager.GetHTTPRoutesWithPrefix(ctx, ingress.Namespace, ingress.Name)
	if err != nil {
		return err
	}
	for _, route := range routes {
		httpRoute := route
		if !utils.IsManagedByUsForIngress(&httpRoute, ingress.Namespace, ingress.Name) {
			return fmt.Errorf("httproute %s/%s not managed by ingress-doperator", httpRoute.Namespace, httpRoute.Name)
		}
	}

	allRoutes := &gatewayv1.HTTPRouteList{}
	if err := cli.List(ctx, allRoutes); err != nil {
		return err
	}
	parentTotals := map[string]int{}
	parentManaged := map[string]int{}
	parentUpdated := map[string]int{}
	for i := range allRoutes.Items {
		route := &allRoutes.Items[i]
		managed := utils.IsManagedByUs(route)
		for _, parent := range route.Spec.ParentRefs {
			key, ok := parentRefKey(route.Namespace, parent)
			if !ok {
				continue
			}
			parentTotals[key]++
			if managed {
				parentManaged[key]++
			}
		}
	}

	if len(routes) == 0 {
		return fmt.Errorf("missing managed derived resources (httproutes=0)")
	}

	for _, route := range routes {
		httpRoute := route
		updated := httpRoute.DeepCopy()
		annotations, modified, warning := applyExternalDNSDisableAnnotations(updated.Annotations)
		if annotations[controller.IngressDisabledAnnotation] == "" {
			annotations[controller.IngressDisabledAnnotation] = controller.IngressDisabledReasonExternalDNS
			modified = true
		}
		updated.Annotations = annotations
		if warning {
			setupLog.Info("external-dns hostname annotation present on HTTPRoute; storing original value",
				"namespace", updated.Namespace,
				"name", updated.Name)
		}
		if modified {
			if err := cli.Update(ctx, updated); err != nil {
				return err
			}
			for _, parent := range updated.Spec.ParentRefs {
				key, ok := parentRefKey(updated.Namespace, parent)
				if ok {
					parentUpdated[key]++
				}
			}
		}
	}

	gateways := &gatewayv1.GatewayList{}
	if err := cli.List(ctx, gateways); err != nil {
		return err
	}
	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if !utils.IsManagedByUsWithIngress(gateway, ingress.Namespace, ingress.Name) {
			continue
		}
		key := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)
		if parentTotals[key] == 0 ||
			parentUpdated[key] != parentTotals[key] ||
			parentManaged[key] != parentTotals[key] {
			setupLog.Info("Skipping external-dns disable on Gateway; not all HTTPRoutes were updated this run",
				"namespace", gateway.Namespace,
				"name", gateway.Name)
			continue
		}
		updated := gateway.DeepCopy()
		annotations, modified, warning := applyExternalDNSDisableAnnotations(updated.Annotations)
		updated.Annotations = annotations
		if warning {
			setupLog.Info("external-dns hostname annotation present on Gateway; storing original value",
				"namespace", updated.Namespace,
				"name", updated.Name)
		}
		if modified {
			if err := cli.Update(ctx, updated); err != nil {
				return err
			}
		}
	}

	return nil
}

func applyExternalDNSDisableAnnotations(annotations map[string]string) (map[string]string, bool, bool) {
	if annotations == nil {
		annotations = map[string]string{}
	}
	modified := false
	warning := false

	if hostname, exists := annotations[controller.ExternalDNSHostnameAnnotation]; exists && hostname != "" {
		if _, saved := annotations[controller.OriginalExternalDNSHostname]; !saved {
			annotations[controller.OriginalExternalDNSHostname] = hostname
			modified = true
			warning = true
		}
	}

	if source, exists := annotations[controller.ExternalDNSIngressHostnameSource]; exists {
		if _, saved := annotations[controller.OriginalExternalDNSIngressHostnameSource]; !saved {
			annotations[controller.OriginalExternalDNSIngressHostnameSource] = source
			modified = true
		}
	}

	if annotations[controller.ExternalDNSIngressHostnameSource] != controller.ExternalDNSHostnameSourceAnnotationOnly {
		annotations[controller.ExternalDNSIngressHostnameSource] = controller.ExternalDNSHostnameSourceAnnotationOnly
		modified = true
	}

	return annotations, modified, warning
}

func parentRefKey(defaultNamespace string, ref gatewayv1.ParentReference) (string, bool) {
	if ref.Name == "" {
		return "", false
	}
	namespace := defaultNamespace
	if ref.Namespace != nil && *ref.Namespace != "" {
		namespace = string(*ref.Namespace)
	}
	return fmt.Sprintf("%s/%s", namespace, ref.Name), true
}

func removeAutomaticSnippetsFilter(ctx context.Context, cli client.Client, ingress *networkingv1.Ingress) error {
	if ingress == nil {
		return nil
	}
	filterName := utils.AutomaticSnippetsFilterName(ingress.Name)
	version, ok, err := utils.GetCRDVersion(ctx, cli, utils.SnippetsFilterCRDName)
	if err != nil || !ok {
		return err
	}
	filter := &unstructured.Unstructured{}
	filter.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   utils.NginxGatewayGroup,
		Version: version,
		Kind:    utils.SnippetsFilterKind,
	})
	if err := cli.Get(ctx, client.ObjectKey{Namespace: ingress.Namespace, Name: filterName}, filter); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !utils.IsManagedByUs(filter) {
		return nil
	}
	return cli.Delete(ctx, filter)
}
