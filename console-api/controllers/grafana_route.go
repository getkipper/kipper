package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/uisession"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

const (
	grafanaNamespace           = platform.MonitoringNamespace
	grafanaDeploymentName      = platform.GrafanaDeploymentName
	grafanaServerConfigMapName = platform.GrafanaServerConfigMapName
	grafanaAuthAnnotation      = platform.GrafanaAuthAnnotation
	grafanaAuthVersion         = platform.GrafanaAuthVersion

	// grafanaRootURLAnnotation on the pod template is the hash of the root URL
	// the pods were started with; changing it restarts Grafana.
	grafanaRootURLAnnotation = "kipper.run/grafana-root-url"

	grafanaAuthCheckAddress = "http://console-api.kipper-system.svc.cluster.local:8080/auth/check/grafana"
)

var middlewareListGVK = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "MiddlewareList"}

// reconcileGrafana publishes Grafana's public route while a Grafana with the
// auth-proxy settings and the current root URL has fully rolled out, and
// withdraws it otherwise. grafana-server is kept for as long as Grafana exists,
// because pods being rolled out read it.
func (r *ClusterIdentityReconciler) reconcileGrafana(ctx context.Context, ci *kipperv1.ClusterIdentity) error {
	served, active := serving.GrafanaHosts(r.servingSpec(ci))

	var dep appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: grafanaNamespace, Name: grafanaDeploymentName}, &dep)
	if apierrors.IsNotFound(err) {
		if err := r.withdrawGrafanaRoute(ctx); err != nil {
			return err
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: grafanaNamespace, Name: grafanaServerConfigMapName}}
		return client.IgnoreNotFound(r.Delete(ctx, cm))
	}
	if err != nil {
		return err
	}

	rootURL := "https://" + active + "/"
	hash := grafanaRootURLHash(rootURL)
	ready := grafanaRolledOut(&dep) && dep.Spec.Template.Annotations[grafanaRootURLAnnotation] == hash &&
		r.grafanaIsolated(ctx, &dep)
	// Withdraw before any write that can fail, so a failure leaves no route
	// in front of a Grafana that is not ready for it.
	if !ready {
		if err := r.withdrawGrafanaRoute(ctx); err != nil {
			return err
		}
	}
	if err := r.serverSideApply(ctx, grafanaServerConfigMap(rootURL)); err != nil {
		return fmt.Errorf("applying %s: %w", grafanaServerConfigMapName, err)
	}
	if dep.Spec.Template.Annotations[grafanaRootURLAnnotation] != hash {
		before := dep.DeepCopy()
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[grafanaRootURLAnnotation] = hash
		if err := r.Patch(ctx, &dep, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("restarting grafana for the root URL: %w", err)
		}
	}
	if !ready {
		return nil
	}

	ings, mws := serving.RenderGrafanaRoute(serving.GrafanaRoute{
		Hosts:       served,
		Active:      active,
		AuthAddress: grafanaAuthCheckAddress,
		DenyAddress: denyAuthAddress,
		CookieName:  uisession.CookieName,
	})
	keepMW := map[string]bool{}
	for _, mw := range mws {
		if err := r.serverSideApply(ctx, mw); err != nil {
			return fmt.Errorf("applying middleware %s: %w", mw.GetName(), err)
		}
		keepMW[mw.GetName()] = true
	}
	keepIng := map[string]bool{}
	for i := range ings {
		ing := ings[i]
		if err := r.serverSideApply(ctx, &ing); err != nil {
			return fmt.Errorf("applying ingress %s: %w", ing.Name, err)
		}
		keepIng[ing.Name] = true
	}
	return r.pruneGrafanaRoute(ctx, keepIng, keepMW)
}

// grafanaIsolated checks that the named policy selects Grafana's pods.
// Policy rules are managed by PlatformConfig; this check does not validate
// those rules or verify network enforcement.
func (r *ClusterIdentityReconciler) grafanaIsolated(ctx context.Context, dep *appsv1.Deployment) bool {
	var np networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: grafanaNamespace, Name: platform.GrafanaNetworkPolicyName}, &np); err != nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(dep.Spec.Template.Labels))
}

// grafanaRolledOut requires the auth-proxy template marker and Deployment
// status showing all desired replicas updated and available.
func grafanaRolledOut(dep *appsv1.Deployment) bool {
	if dep.Spec.Template.Annotations[grafanaAuthAnnotation] != grafanaAuthVersion {
		return false
	}
	if dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	st := dep.Status
	return want > 0 && st.UpdatedReplicas == want && st.AvailableReplicas == want && st.Replicas == want
}

func grafanaRootURLHash(rootURL string) string {
	sum := sha256.Sum256([]byte(rootURL))
	return hex.EncodeToString(sum[:])[:16]
}

func grafanaServerConfigMap(rootURL string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: grafanaNamespace, Name: grafanaServerConfigMapName},
		Data:       map[string]string{"GF_SERVER_ROOT_URL": rootURL},
	}
}

func (r *ClusterIdentityReconciler) withdrawGrafanaRoute(ctx context.Context) error {
	return r.pruneGrafanaRoute(ctx, nil, nil)
}

// pruneGrafanaRoute deletes every Grafana route object not named in keep.
// Server-side apply never removes an object that is no longer rendered.
func (r *ClusterIdentityReconciler) pruneGrafanaRoute(ctx context.Context, keepIng, keepMW map[string]bool) error {
	selector := []client.ListOption{client.InNamespace(grafanaNamespace), client.MatchingLabels{serving.GrafanaRouteLabel: "true"}}

	var ings networkingv1.IngressList
	if err := r.List(ctx, &ings, selector...); err != nil {
		return err
	}
	for i := range ings.Items {
		if keepIng[ings.Items[i].Name] {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, &ings.Items[i])); err != nil {
			return fmt.Errorf("deleting ingress %s: %w", ings.Items[i].Name, err)
		}
	}

	var mws unstructured.UnstructuredList
	mws.SetGroupVersionKind(middlewareListGVK)
	if err := r.List(ctx, &mws, selector...); err != nil {
		return err
	}
	for i := range mws.Items {
		if keepMW[mws.Items[i].GetName()] {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, &mws.Items[i])); err != nil {
			return fmt.Errorf("deleting middleware %s: %w", mws.Items[i].GetName(), err)
		}
	}
	return nil
}

// GrafanaRouteReader reads the route published by reconcileGrafana.
type GrafanaRouteReader struct {
	Client client.Reader
}

func (g GrafanaRouteReader) ingresses(ctx context.Context) []networkingv1.Ingress {
	var list networkingv1.IngressList
	if err := g.Client.List(ctx, &list, client.InNamespace(grafanaNamespace), client.MatchingLabels{serving.GrafanaRouteLabel: "true"}); err != nil {
		return nil
	}
	return list.Items
}

// Hosts returns every host the Grafana route serves.
func (g GrafanaRouteReader) Hosts(ctx context.Context) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, ing := range g.ingresses(ctx) {
		for _, rule := range ing.Spec.Rules {
			if rule.Host != "" && !seen[rule.Host] {
				seen[rule.Host] = true
				hosts = append(hosts, rule.Host)
			}
		}
	}
	return hosts
}

// URL returns the address links should use, or "" while there is no route.
func (g GrafanaRouteReader) URL(ctx context.Context) string {
	for _, ing := range g.ingresses(ctx) {
		if ing.Annotations[serving.GrafanaActiveAnnotation] == "true" && len(ing.Spec.Rules) > 0 {
			return "https://" + ing.Spec.Rules[0].Host
		}
	}
	return ""
}

// GrafanaRouteCache holds GrafanaRouteReader results for a short TTL, so the
// forwardAuth gate does not read the API server on every Grafana request.
type GrafanaRouteCache struct {
	reader  GrafanaRouteReader
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	fetched time.Time
	hosts   []string
	url     string
}

func NewGrafanaRouteCache(c client.Reader, ttl time.Duration) *GrafanaRouteCache {
	return &GrafanaRouteCache{reader: GrafanaRouteReader{Client: c}, ttl: ttl, now: time.Now}
}

func (c *GrafanaRouteCache) refresh() {
	if !c.fetched.IsZero() && c.now().Sub(c.fetched) < c.ttl {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.hosts = c.reader.Hosts(ctx)
	c.url = c.reader.URL(ctx)
	c.fetched = c.now()
}

// Hosts returns the hosts the Grafana route serves.
func (c *GrafanaRouteCache) Hosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	return append([]string(nil), c.hosts...)
}

// URL returns the Grafana address links should use, or "" without a route.
func (c *GrafanaRouteCache) URL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
	return c.url
}
