package controllers

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/internalpath"
)

// denyAuthAddress reaches the shared authz service across namespaces.
const denyAuthAddress = "http://kipper-authz.kipper-system.svc.cluster.local:8080/deny"

// Priority order: public exceptions > denied paths > ordinary app routes.
const (
	refusePriority = "100000"
	allowPriority  = "200000"

	routerPriorityAnnotation    = "traefik.ingress.kubernetes.io/router.priority"
	routerMiddlewaresAnnotation = "traefik.ingress.kubernetes.io/router.middlewares"

	routeIngressClass = "traefik"
)

// InternalPathsIngressName returns the name of the deny Ingress and Middleware.
// Ownership checks reject collisions with other apps before publishing the route.
func InternalPathsIngressName(app string) string { return app + "-internal-paths" }

// PublicPathsIngressName returns the name of the public-exception Ingress.
func PublicPathsIngressName(app string) string { return app + "-public-paths" }

// ingressPathRule describes an Exact or Prefix match. Loose rules use
// ImplementationSpecific for paths Kubernetes rejects in Exact/Prefix rules.
type ingressPathRule struct {
	path  string
	exact bool
	loose bool
}

// refusedPrefixes combines app declarations with enabled cluster defaults.
func refusedPrefixes(route *kipperv1.AppRoute, blockDefaults bool) []string {
	if route == nil {
		return nil
	}
	declared := route.InternalPaths
	if blockDefaults {
		declared = append(append([]string{}, internalpath.Default()...), declared...)
	}
	return internalpath.Clean(declared)
}

// Matching precedes strip-prefix: /domains-api + /actuator becomes
// /domains-api/actuator.
func routeBase(routePath string) string {
	if routePath == "" || routePath == "/" {
		return ""
	}
	return strings.TrimSuffix(routePath, "/")
}

// refusalPathRules matches each prefix exactly and at segment boundaries.
// It also covers semicolon path parameters that servlet backends strip
// (e.g. /actuator;x=1/metrics).
func refusalPathRules(routePath string, prefixes []string) []ingressPathRule {
	base := routeBase(routePath)
	rules := make([]ingressPathRule, 0, len(prefixes)*5)
	for _, p := range prefixes {
		if !expressible(base + p) {
			continue
		}
		rules = append(rules,
			ingressPathRule{path: base + p, exact: true},
			ingressPathRule{path: base + p + "/"},
			ingressPathRule{path: base + p + ";"},
			// Cover encoded separators that backends may decode. Kubernetes
			// requires ImplementationSpecific for paths containing %2F or %2f.
			ingressPathRule{path: base + p + "%2F", loose: true},
			ingressPathRule{path: base + p + "%2f", loose: true},
		)
	}
	return rules
}

// reopenedPathRules matches each public exception and its trailing-slash form
// exactly. Prefix matches would also expose descendants and traversal paths.
func reopenedPathRules(routePath string, paths []string) []ingressPathRule {
	base := routeBase(routePath)
	rules := make([]ingressPathRule, 0, len(paths)*2)
	for _, p := range paths {
		if !expressible(base + p) {
			continue
		}
		rules = append(rules,
			ingressPathRule{path: base + p, exact: true},
			ingressPathRule{path: base + p + "/", exact: true},
		)
	}
	return rules
}

// expressible validates the composed route path for Exact/Prefix rules.
// One invalid path causes Kubernetes to reject the entire Ingress.
func expressible(path string) bool {
	return internalpath.IngressPathAllowed(path) && internalpath.IngressPathAllowed(path+"/")
}

// unusablePaths identifies invalid declarations for route status reporting.
// Clean filters these out when building rules.
func unusablePaths(route *kipperv1.AppRoute) []string {
	if route == nil {
		return nil
	}
	var bad []string
	for _, declared := range [][]string{route.InternalPaths, route.PublicPaths} {
		kept := map[string]bool{}
		for _, p := range internalpath.Clean(declared) {
			kept[p] = true
		}
		for _, p := range declared {
			if !kept[strings.TrimSuffix(p, "/")] {
				bad = append(bad, p)
			}
		}
	}
	return bad
}

// reopenedPaths returns public exceptions covered by a refused prefix.
func reopenedPaths(route *kipperv1.AppRoute, refused []string) []string {
	if route == nil || len(route.PublicPaths) == 0 {
		return nil
	}
	var out []string
	for _, p := range internalpath.Clean(route.PublicPaths) {
		for _, prefix := range refused {
			if internalpath.Covers(prefix, p) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// RouteGuardEnabled reports whether cluster defaults apply. An absent setting
// means off, preserving existing routes on upgrade.
func RouteGuardEnabled(ctx context.Context, c client.Reader) (bool, error) {
	var pc kipperv1.PlatformConfig
	err := c.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return pc.Spec.RouteGuard != nil && pc.Spec.RouteGuard.BlockInternalPaths, nil
}

// RefusalInstalled checks whether both guard Ingresses match the requested policy,
// including ownership, host, class, priority, rules, and the deny middleware reference.
// Middleware contents are checked separately by guardEnforcing.
func RefusalInstalled(deny, allow *networkingv1.Ingress, app *kipperv1.App, host string, blockDefaults bool) bool {
	return refusalHolds(deny, allow, app, host, blockDefaults, true)
}

// refusalEnforced checks that every requested refusal is covered. Extra deny rules
// and missing public exceptions are safe while reconciliation catches up; stale
// public exceptions can expose paths and fail this check.
func refusalEnforced(deny, allow *networkingv1.Ingress, app *kipperv1.App, host string, blockDefaults bool) bool {
	return refusalHolds(deny, allow, app, host, blockDefaults, false)
}

func refusalHolds(deny, allow *networkingv1.Ingress, app *kipperv1.App, host string, blockDefaults, exact bool) bool {
	if app == nil || app.Spec.Route == nil {
		return false
	}
	route := app.Spec.Route
	refused := refusedPrefixes(route, blockDefaults)
	wantDeny := refusalPathRules(route.Path, refused)
	if len(wantDeny) == 0 {
		return false
	}
	if !guardMatches(deny, app, host, wantDeny, refusePriority, true, exact) {
		return false
	}
	if !chainRefuses(deny, app) {
		return false
	}

	wantAllow := reopenedPathRules(route.Path, reopenedPaths(route, refused))
	if len(wantAllow) == 0 {
		// Stale exceptions outrank the deny rules.
		return allow == nil
	}
	if allow == nil {
		// A missing exception keeps the path blocked until reconciliation.
		return !exact
	}
	return guardMatches(allow, app, host, wantAllow, allowPriority, exact, true)
}

// chainRefuses checks for this app's deny middleware reference.
// Compare whole references: another middleware name may contain the same suffix.
func chainRefuses(deny *networkingv1.Ingress, app *kipperv1.App) bool {
	want := app.Namespace + "-" + InternalPathsIngressName(app.Name) + "@kubernetescrd"
	for _, ref := range strings.Split(deny.Annotations[routerMiddlewaresAnnotation], ",") {
		if strings.TrimSpace(ref) == want {
			return true
		}
	}
	return false
}

// guardMatches checks ownership, ingress class, host, priority, and path rules.
// requireAll checks coverage; forbidExtra checks for stale rules. Enforcement
// allows extra deny rules, while policy convergence requires an exact match.
func guardMatches(ing *networkingv1.Ingress, app *kipperv1.App, host string, want []ingressPathRule,
	priority string, requireAll, forbidExtra bool) bool {

	if ing == nil || !metav1.IsControlledBy(ing, app) {
		return false
	}
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != routeIngressClass {
		return false
	}
	if ing.Annotations[routerPriorityAnnotation] != priority {
		return false
	}
	installed := map[ingressPathRule]bool{}
	for _, rule := range ing.Spec.Rules {
		if rule.Host != host || rule.HTTP == nil {
			return false
		}
		for _, p := range rule.HTTP.Paths {
			if p.PathType == nil {
				return false
			}
			installed[ingressPathRule{
				path:  p.Path,
				exact: *p.PathType == networkingv1.PathTypeExact,
				loose: *p.PathType == networkingv1.PathTypeImplementationSpecific,
			}] = true
		}
	}
	wanted := map[ingressPathRule]bool{}
	for _, rule := range want {
		wanted[rule] = true
		if requireAll && !installed[rule] {
			return false
		}
	}
	if forbidExtra {
		for rule := range installed {
			if !wanted[rule] {
				return false
			}
		}
	}
	return true
}

// withdrawSupersededPublication removes the old host before its guard moves.
// This briefly interrupts the old route but keeps its internal paths protected
// if publishing the new host fails.
func (r *AppReconciler) withdrawSupersededPublication(ctx context.Context, app *kipperv1.App, host string) error {
	var existing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedByWorkload(&existing, appOwner(app)) {
		return nil
	}

	// An existing rule for the new canonical host means the route can stay up.
	// Alias-only edits preserve that host; a canonical host change requires withdrawal,
	// even when the old host will become an alias.
	for _, rule := range existing.Spec.Rules {
		if rule.Host == host {
			return nil
		}
	}
	return r.deleteOwnedIngress(ctx, app)
}

// guardEnforcing checks installed Ingresses and middleware to decide whether
// the route can keep serving after a failed reconciliation.
func (r *AppReconciler) guardEnforcing(ctx context.Context, app *kipperv1.App, host string, blockDefaults bool) bool {
	deny, err := r.readGuardIngress(ctx, app, InternalPathsIngressName(app.Name))
	if err != nil {
		return false
	}
	allow, err := r.readGuardIngress(ctx, app, PublicPathsIngressName(app.Name))
	if err != nil {
		return false
	}
	if !refusalEnforced(deny, allow, app, host, blockDefaults) {
		return false
	}
	return r.denyMiddlewareIntact(ctx, app)
}

// readGuardIngress returns nil for an absent guard and propagates read errors.
// A failed read may hide a stale public exception that still overrides the guard.
func (r *AppReconciler) readGuardIngress(ctx context.Context, app *kipperv1.App, name string) (*networkingv1.Ingress, error) {
	var ing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &ing)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ing, nil
}

// denyMiddlewareIntact checks ownership and the forwardAuth target.
func (r *AppReconciler) denyMiddlewareIntact(ctx context.Context, app *kipperv1.App) bool {
	var mw unstructured.Unstructured
	mw.SetGroupVersionKind(middlewareGVK)
	if err := r.Get(ctx, types.NamespacedName{
		Name: InternalPathsIngressName(app.Name), Namespace: app.Namespace}, &mw); err != nil {
		return false
	}
	if !ownedByWorkload(&mw, appOwner(app)) {
		return false
	}
	address, _, _ := unstructured.NestedString(mw.Object, "spec", "forwardAuth", "address")
	return address == denyAuthAddress
}

// RefusedRoutePaths returns configured refused prefixes including the route base path.
func RefusedRoutePaths(route *kipperv1.AppRoute, blockDefaults bool) []string {
	return belowRoutePath(route, refusedPrefixes(route, blockDefaults))
}

// ReopenedRoutePaths returns covered public exceptions including the route base path.
func ReopenedRoutePaths(route *kipperv1.AppRoute, blockDefaults bool) []string {
	return belowRoutePath(route, reopenedPaths(route, refusedPrefixes(route, blockDefaults)))
}

func belowRoutePath(route *kipperv1.AppRoute, paths []string) []string {
	if route == nil || len(paths) == 0 {
		return nil
	}
	base := ""
	if route.Path != "" && route.Path != "/" {
		base = strings.TrimSuffix(route.Path, "/")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, base+p)
	}
	return out
}

// routeGuardChanged accepts creates, deletes, and changes to the guard setting.
func routeGuardChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return guardSetting(e.ObjectOld) != guardSetting(e.ObjectNew)
		},
	}
}

func guardSetting(obj client.Object) bool {
	pc, ok := obj.(*kipperv1.PlatformConfig)
	if !ok {
		return false
	}
	return pc.Spec.RouteGuard != nil && pc.Spec.RouteGuard.BlockInternalPaths
}

func (r *AppReconciler) enqueueAppsForRouteGuard(ctx context.Context, _ client.Object) []reconcile.Request {
	var apps kipperv1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range apps.Items {
		if apps.Items[i].Spec.Route == nil {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: apps.Items[i].Name, Namespace: apps.Items[i].Namespace,
		}})
	}
	return reqs
}

// reconcileInternalPathsMiddleware forwards denied paths to authz for a 404.
// If authz is unreachable, forwardAuth fails closed.
func (r *AppReconciler) reconcileInternalPathsMiddleware(ctx context.Context, app *kipperv1.App, refusing bool) error {
	mwName := InternalPathsIngressName(app.Name)
	if !refusing {
		return r.deleteOwnedMiddleware(ctx, app, mwName)
	}

	desired := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "traefik.io/v1alpha1",
			"kind":       "Middleware",
			"metadata": map[string]interface{}{
				"name":      mwName,
				"namespace": app.Namespace,
				"labels": map[string]interface{}{
					"app":       app.Name,
					kipperLabel: kipperValue,
				},
			},
			"spec": map[string]interface{}{
				"forwardAuth": map[string]interface{}{
					"address": denyAuthAddress,
					// Restrict forwarded headers to keep caller credentials out of authz.
					"authRequestHeaders": []interface{}{"X-Forwarded-Uri"},
				},
			},
		},
	}
	return r.applyOwnedMiddleware(ctx, app, desired)
}

// reconcileGuard applies the deny Ingress and higher-priority public exceptions.
func (r *AppReconciler) reconcileGuard(ctx context.Context, app *kipperv1.App, host string,
	tlsEntry networkingv1.IngressTLS, backend networkingv1.IngressBackend, routePath string,
	blockDefaults bool, appMiddlewares []string, rateLimitRef string) error {

	refused := refusedPrefixes(app.Spec.Route, blockDefaults)

	if bad := unusablePaths(app.Spec.Route); len(bad) > 0 {
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:   kipperv1.ConditionRouteReady,
			Status: metav1.ConditionFalse,
			Reason: "InternalPathUnusable",
			Message: fmt.Sprintf("these paths cannot become an ingress rule and refuse nothing: %s",
				strings.Join(bad, ", ")),
		})
	}

	if err := r.reconcileInternalPathsMiddleware(ctx, app, len(refused) > 0); err != nil {
		return err
	}
	if len(refused) == 0 {
		if err := r.deleteOwnedIngressNamed(ctx, app, InternalPathsIngressName(app.Name)); err != nil {
			return err
		}
		return r.deleteOwnedIngressNamed(ctx, app, PublicPathsIngressName(app.Name))
	}

	tls := []networkingv1.IngressTLS{tlsEntry}
	// Rate-limit denied requests before they reach authz.
	denyChain := []string{rateLimitRef, app.Namespace + "-" + InternalPathsIngressName(app.Name) + "@kubernetescrd"}
	refusals := refusalPathRules(routePath, refused)
	if len(refusals) == 0 {
		return fmt.Errorf("route path %q cannot carry an ingress rule, so %s cannot be refused",
			routePath, strings.Join(refused, ", "))
	}
	if err := r.guardNameIsOurs(ctx, app, InternalPathsIngressName(app.Name)); err != nil {
		return err
	}
	deny := r.guardIngress(app, InternalPathsIngressName(app.Name), host, tls, backend,
		refusals, denyChain, refusePriority)
	if err := r.applyOwnedIngress(ctx, app, deny); err != nil {
		return err
	}

	reopened := reopenedPaths(app.Spec.Route, refused)
	if len(reopened) == 0 {
		return r.deleteOwnedIngressNamed(ctx, app, PublicPathsIngressName(app.Name))
	}
	// Public exceptions use the app's full middleware chain, including authentication.
	allow := r.guardIngress(app, PublicPathsIngressName(app.Name), host, tls, backend,
		reopenedPathRules(routePath, reopened), appMiddlewares, allowPriority)
	return r.applyOwnedIngress(ctx, app, allow)
}

func (r *AppReconciler) guardNameIsOurs(ctx context.Context, app *kipperv1.App, name string) error {
	var existing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedByWorkload(&existing, appOwner(app)) {
		return fmt.Errorf("the Ingress %q in %s belongs to something else, so this app cannot refuse anything under that name",
			name, app.Namespace)
	}
	return nil
}

// guardIngress shares the app route's host and TLS with a separate chain and priority.
func (r *AppReconciler) guardIngress(app *kipperv1.App, name, host string, tls []networkingv1.IngressTLS,
	backend networkingv1.IngressBackend, rules []ingressPathRule, middlewares []string, priority string) *networkingv1.Ingress {

	exact := networkingv1.PathTypeExact
	prefix := networkingv1.PathTypePrefix
	loose := networkingv1.PathTypeImplementationSpecific
	paths := make([]networkingv1.HTTPIngressPath, 0, len(rules))
	for _, rule := range rules {
		pathType := &prefix
		switch {
		case rule.exact:
			pathType = &exact
		case rule.loose:
			pathType = &loose
		}
		paths = append(paths, networkingv1.HTTPIngressPath{
			Path:     rule.path,
			PathType: pathType,
			Backend:  backend,
		})
	}

	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: app.Namespace,
			Labels: map[string]string{
				"app":       app.Name,
				kipperLabel: kipperValue,
			},
			Annotations: map[string]string{
				routerMiddlewaresAnnotation: strings.Join(middlewares, ","),
				routerPriorityAnnotation:    priority,
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: strPtr(routeIngressClass),
			// Share TLS so the guard serves HTTPS. The app Ingress alone carries
			// the cert-manager annotation and manages the certificate.
			TLS: tls,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths},
				},
			}},
		},
	}
}

func (r *AppReconciler) applyOwnedIngress(ctx context.Context, app *kipperv1.App, desired *networkingv1.Ingress) error {
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.Ingress
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if err := adoptChild("Ingress", &existing, appOwner(app), r.Scheme); err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Annotations = desired.Annotations
	return r.Update(ctx, &existing)
}

func (r *AppReconciler) deleteOwnedIngressNamed(ctx context.Context, app *kipperv1.App, name string) error {
	var existing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedByWorkload(&existing, appOwner(app)) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}
