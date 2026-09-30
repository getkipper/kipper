package controllers

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

type grafanaDeployState struct {
	authAnnotation bool
	rootURLHash    string
	generation     int64
	observed       int64
	replicas       int32
	updated        int32
	available      int32
	current        int32
}

func rolledOut(hash string) grafanaDeployState {
	return grafanaDeployState{authAnnotation: true, rootURLHash: hash, generation: 2, observed: 2, replicas: 1, updated: 1, available: 1, current: 1}
}

func grafanaDeployment(s grafanaDeployState) *appsv1.Deployment {
	ann := map[string]string{}
	if s.authAnnotation {
		ann[grafanaAuthAnnotation] = grafanaAuthVersion
	}
	if s.rootURLHash != "" {
		ann[grafanaRootURLAnnotation] = s.rootURLHash
	}
	replicas := s.replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: grafanaDeploymentName, Namespace: grafanaNamespace, Generation: s.generation},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: platform.GrafanaPodLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: platform.GrafanaPodLabels(), Annotations: ann},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "grafana", Image: "grafana"}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: s.observed,
			Replicas:           s.current,
			UpdatedReplicas:    s.updated,
			AvailableReplicas:  s.available,
		},
	}
}

// grafanaPolicy is the NetworkPolicy the route requires before it is published.
func grafanaPolicy() *networkingv1.NetworkPolicy {
	np := platform.GrafanaNetworkPolicyObject(platform.IngressPeer{})
	np.TypeMeta = metav1.TypeMeta{}
	return np
}

func steadyCI(domain string) *kipperv1.ClusterIdentity {
	return newCI(kipperv1.ClusterIdentitySpec{Domain: domain}, kipperv1.ClusterIdentityStatus{})
}

func grafanaIngressHosts(t *testing.T, c client.Client) []string {
	t.Helper()
	var list networkingv1.IngressList
	if err := c.List(context.Background(), &list, client.InNamespace(grafanaNamespace), client.MatchingLabels{serving.GrafanaRouteLabel: "true"}); err != nil {
		t.Fatalf("list grafana ingresses: %v", err)
	}
	seen := map[string]bool{}
	var hosts []string
	for _, ing := range list.Items {
		for _, rule := range ing.Spec.Rules {
			if !seen[rule.Host] {
				seen[rule.Host] = true
				hosts = append(hosts, rule.Host)
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

func readGrafanaServer(t *testing.T, c client.Client) (*corev1.ConfigMap, bool) {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{Namespace: grafanaNamespace, Name: grafanaServerConfigMapName}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get grafana-server: %v", err)
	}
	return &cm, true
}

func runGrafana(t *testing.T, r *ClusterIdentityReconciler, ci *kipperv1.ClusterIdentity) {
	t.Helper()
	if err := r.reconcileGrafana(context.Background(), ci); err != nil {
		t.Fatalf("reconcileGrafana: %v", err)
	}
}

const exampleRootURL = "https://grafana.example.com/"

func TestGrafanaRoute_PublishedOnceRolledOut(t *testing.T) {
	ci := steadyCI("example.com")
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash(exampleRootURL))))

	runGrafana(t, r, ci)

	if got := grafanaIngressHosts(t, c); len(got) != 1 || got[0] != "grafana.example.com" {
		t.Fatalf("route hosts = %v, want [grafana.example.com]", got)
	}
	cm, ok := readGrafanaServer(t, c)
	if !ok || cm.Data["GF_SERVER_ROOT_URL"] != exampleRootURL {
		t.Fatalf("grafana-server = %+v", cm)
	}
}

func TestGrafanaRoute_NotPublishedWithoutAuthConfiguration(t *testing.T) {
	// A --skip-system upgrade leaves the old chart: no auth-proxy, no route.
	s := rolledOut(grafanaRootURLHash(exampleRootURL))
	s.authAnnotation = false
	ci := steadyCI("example.com")
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(s))

	runGrafana(t, r, ci)

	if got := grafanaIngressHosts(t, c); len(got) != 0 {
		t.Fatalf("route published for a Grafana without auth-proxy: %v", got)
	}
}

func TestGrafanaRoute_WithdrawnDuringARollout(t *testing.T) {
	ci := steadyCI("example.com")
	hash := grafanaRootURLHash(exampleRootURL)
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(rolledOut(hash)))
	runGrafana(t, r, ci)
	if len(grafanaIngressHosts(t, c)) == 0 {
		t.Fatal("setup: route should be published")
	}

	for name, s := range map[string]grafanaDeployState{
		"new generation not observed": {authAnnotation: true, rootURLHash: hash, generation: 3, observed: 2, replicas: 1, updated: 1, available: 1, current: 1},
		"old pod still serving":       {authAnnotation: true, rootURLHash: hash, generation: 3, observed: 3, replicas: 1, updated: 1, available: 1, current: 2},
		"new pod not available":       {authAnnotation: true, rootURLHash: hash, generation: 3, observed: 3, replicas: 1, updated: 1, available: 0, current: 1},
		"scaled to zero":              {authAnnotation: true, rootURLHash: hash, generation: 3, observed: 3},
	} {
		t.Run(name, func(t *testing.T) {
			var dep appsv1.Deployment
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: grafanaNamespace, Name: grafanaDeploymentName}, &dep); err != nil {
				t.Fatal(err)
			}
			want := grafanaDeployment(s)
			dep.Spec = want.Spec
			dep.Generation = want.Generation
			if err := c.Update(context.Background(), &dep); err != nil {
				t.Fatal(err)
			}
			dep.Status = want.Status
			if err := c.Status().Update(context.Background(), &dep); err != nil {
				t.Fatal(err)
			}

			runGrafana(t, r, ci)

			if got := grafanaIngressHosts(t, c); len(got) != 0 {
				t.Errorf("route still published: %v", got)
			}
			if _, ok := readGrafanaServer(t, c); !ok {
				t.Error("grafana-server deleted during a rollout; the new pods need it")
			}
		})
	}
}

func TestGrafanaRoute_RootURLChangeRestartsGrafanaBeforePublishing(t *testing.T) {
	ci := steadyCI("example.com")
	stale := rolledOut(grafanaRootURLHash("https://grafana.old.example/"))
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(stale))

	runGrafana(t, r, ci)

	cm, ok := readGrafanaServer(t, c)
	if !ok || cm.Data["GF_SERVER_ROOT_URL"] != exampleRootURL {
		t.Fatalf("grafana-server not written before the restart: %+v", cm)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: grafanaNamespace, Name: grafanaDeploymentName}, &dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Annotations[grafanaRootURLAnnotation]; got != grafanaRootURLHash(exampleRootURL) {
		t.Errorf("restart annotation = %q, want the new root URL's hash", got)
	}
	if got := dep.Spec.Template.Annotations[grafanaAuthAnnotation]; got != grafanaAuthVersion {
		t.Errorf("restart patch dropped the auth annotation: %q", got)
	}
	if got := grafanaIngressHosts(t, c); len(got) != 0 {
		t.Errorf("route published before the pods have the new root URL: %v", got)
	}
}

func TestGrafanaRoute_RemovedWithGrafana(t *testing.T) {
	ci := steadyCI("example.com")
	dep := grafanaDeployment(rolledOut(grafanaRootURLHash(exampleRootURL)))
	r, c := reconcilerFor(ci, grafanaPolicy(), dep)
	runGrafana(t, r, ci)

	if err := c.Delete(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	runGrafana(t, r, ci)

	if got := grafanaIngressHosts(t, c); len(got) != 0 {
		t.Errorf("route left behind after monitoring was turned off: %v", got)
	}
	if _, ok := readGrafanaServer(t, c); ok {
		t.Error("grafana-server left behind after monitoring was turned off")
	}
}

func TestGrafanaRoute_PrunesHostsNoLongerServed(t *testing.T) {
	old := steadyCI("old.example")
	r, c := reconcilerFor(old, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash("https://grafana.old.example/"))))
	runGrafana(t, r, old)
	if got := grafanaIngressHosts(t, c); len(got) != 1 || got[0] != "grafana.old.example" {
		t.Fatalf("setup: hosts = %v", got)
	}

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: grafanaNamespace, Name: grafanaDeploymentName}, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Spec.Template.Annotations[grafanaRootURLAnnotation] = grafanaRootURLHash(exampleRootURL)
	if err := c.Update(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}
	runGrafana(t, r, steadyCI("example.com"))

	if got := grafanaIngressHosts(t, c); len(got) != 1 || got[0] != "grafana.example.com" {
		t.Errorf("hosts after the move = %v, want only grafana.example.com", got)
	}
}

func TestGrafanaRoute_ServesBothHostsDuringDualServe(t *testing.T) {
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "example.com"}, kipperv1.ClusterIdentityStatus{
		Transition: &kipperv1.TransitionStatus{
			Phase:        phaseDualServe,
			From:         &kipperv1.ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run"},
			To:           &kipperv1.ResolvedHosts{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com"},
			FromIdentity: &kipperv1.SteadyIdentity{Domain: "acme.kipper.run"},
			ToIdentity:   &kipperv1.SteadyIdentity{Domain: "example.com"},
		},
	})
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash("https://grafana--acme.kipper.run/"))))

	runGrafana(t, r, ci)

	got := grafanaIngressHosts(t, c)
	if len(got) != 2 || got[0] != "grafana--acme.kipper.run" || got[1] != "grafana.example.com" {
		t.Fatalf("hosts during DualServe = %v", got)
	}
	route := GrafanaRouteReader{Client: c}
	if u := route.URL(context.Background()); u != "https://grafana--acme.kipper.run" {
		t.Errorf("active URL during DualServe = %q, want the outgoing host", u)
	}
	hosts := route.Hosts(context.Background())
	sort.Strings(hosts)
	if len(hosts) != 2 {
		t.Errorf("gate hosts = %v, want both", hosts)
	}
}

func TestGrafanaRoute_ReaderReportsNothingWithoutARoute(t *testing.T) {
	_, c := reconcilerFor(steadyCI("example.com"))
	route := GrafanaRouteReader{Client: c}
	if h := route.Hosts(context.Background()); len(h) != 0 {
		t.Errorf("hosts = %v, want none", h)
	}
	if u := route.URL(context.Background()); u != "" {
		t.Errorf("url = %q, want empty", u)
	}
}

func TestReconcile_PublishesGrafanaRoute(t *testing.T) {
	ci := steadyCI("example.com")
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash(exampleRootURL))))

	reconcileOnce(t, r)

	if got := grafanaIngressHosts(t, c); len(got) != 1 {
		t.Fatalf("a full reconcile did not publish the Grafana route: %v", got)
	}
}

func TestGrafanaRouteCache_RefreshesAfterTTL(t *testing.T) {
	ci := steadyCI("example.com")
	r, c := reconcilerFor(ci, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash(exampleRootURL))))
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cache := NewGrafanaRouteCache(c, 10*time.Second)
	cache.now = func() time.Time { return now }

	if h := cache.Hosts(); len(h) != 0 {
		t.Fatalf("hosts before publishing = %v", h)
	}
	runGrafana(t, r, ci)

	now = now.Add(5 * time.Second)
	if h := cache.Hosts(); len(h) != 0 {
		t.Errorf("cache refreshed inside its TTL: %v", h)
	}
	now = now.Add(6 * time.Second)
	if h := cache.Hosts(); len(h) != 1 || h[0] != "grafana.example.com" {
		t.Errorf("hosts after the TTL = %v", h)
	}
	if u := cache.URL(); u != "https://grafana.example.com" {
		t.Errorf("url = %q", u)
	}
}

func reconcilerWithFaults(funcs interceptor.Funcs, objs ...client.Object) (*ClusterIdentityReconciler, client.Client) {
	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&kipperv1.ClusterIdentity{}).
		WithInterceptorFuncs(funcs).
		Build()
	return &ClusterIdentityReconciler{Client: c, Scheme: testScheme(), Prober: fakeProber{served: true}}, c
}

func startRollout(t *testing.T, c client.Client) {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: grafanaNamespace, Name: grafanaDeploymentName}, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.UpdatedReplicas = 0
	if err := c.Status().Update(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_WithdrawsGrafanaDespiteAnIdentityError(t *testing.T) {
	failConsole := false
	funcs := interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if failConsole && obj.GetName() == "console" && obj.GetNamespace() == "kipper-system" {
			return errors.New("identity write refused")
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}}
	ci := steadyCI("example.com")
	r, c := reconcilerWithFaults(funcs, ci, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash(exampleRootURL))))
	reconcileOnce(t, r)
	if len(grafanaIngressHosts(t, c)) == 0 {
		t.Fatal("setup: route should be published")
	}

	startRollout(t, c)
	failConsole = true
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ClusterIdentityName}})

	if err == nil {
		t.Error("the identity error must still be returned")
	}
	if got := grafanaIngressHosts(t, c); len(got) != 0 {
		t.Errorf("route still published after an unrelated identity error: %v", got)
	}
}

func TestGrafanaRoute_WithdrawnWhenTheRestartPatchFails(t *testing.T) {
	failPatch := false
	funcs := interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*appsv1.Deployment); ok && failPatch {
			return errors.New("patch refused")
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}}
	old := steadyCI("old.example")
	r, c := reconcilerWithFaults(funcs, old, grafanaPolicy(), grafanaDeployment(rolledOut(grafanaRootURLHash("https://grafana.old.example/"))))
	runGrafana(t, r, old)
	if len(grafanaIngressHosts(t, c)) == 0 {
		t.Fatal("setup: route should be published")
	}

	failPatch = true
	if err := r.reconcileGrafana(context.Background(), steadyCI("example.com")); err == nil {
		t.Error("the patch error must be returned")
	}
	if got := grafanaIngressHosts(t, c); len(got) != 0 {
		t.Errorf("route still published while its pods run the old root URL: %v", got)
	}
}

func TestGrafanaRoute_NotPublishedWithoutAWorkingPolicy(t *testing.T) {
	// Grafana trusts the identity headers, so without a policy that actually
	// selects its pods any pod could talk to it directly.
	unmatched := grafanaPolicy()
	unmatched.Spec.PodSelector.MatchLabels = map[string]string{"app.kubernetes.io/name": "grafana", "app.kubernetes.io/instance": "other-release"}

	for name, objs := range map[string][]client.Object{
		"no policy":                     {},
		"policy selects no Grafana pod": {unmatched},
	} {
		t.Run(name, func(t *testing.T) {
			ci := steadyCI("example.com")
			all := append([]client.Object{ci, grafanaDeployment(rolledOut(grafanaRootURLHash(exampleRootURL)))}, objs...)
			r, c := reconcilerFor(all...)

			runGrafana(t, r, ci)

			if got := grafanaIngressHosts(t, c); len(got) != 0 {
				t.Errorf("route published: %v", got)
			}
		})
	}
}
