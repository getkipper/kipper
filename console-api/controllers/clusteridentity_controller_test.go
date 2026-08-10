package controllers

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

func newCI(spec kipperv1.ClusterIdentitySpec, status kipperv1.ClusterIdentityStatus) *kipperv1.ClusterIdentity {
	return &kipperv1.ClusterIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterIdentityName, Generation: 1},
		Spec:       spec,
		Status:     status,
	}
}

func reconcilerFor(objs ...client.Object) (*ClusterIdentityReconciler, client.Client) {
	c := crfake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&kipperv1.ClusterIdentity{}).
		Build()
	// The default prober reports the new issuer as live so verification passes;
	// tests that exercise the live-probe gate override r.Prober.
	return &ClusterIdentityReconciler{Client: c, Scheme: testScheme(), Prober: fakeProber{served: true}}, c
}

func reconcileOnce(t *testing.T, r *ClusterIdentityReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ClusterIdentityName}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getCI(t *testing.T, c client.Client) *kipperv1.ClusterIdentity {
	t.Helper()
	var ci kipperv1.ClusterIdentity
	if err := c.Get(context.Background(), types.NamespacedName{Name: ClusterIdentityName}, &ci); err != nil {
		t.Fatalf("get CI: %v", err)
	}
	return &ci
}

func TestReconcileAdoptsSpecAsSteady(t *testing.T) {
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{})
	r, c := reconcilerFor(ci)

	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.ActiveHosts == nil {
		t.Fatal("adoption should set activeHosts")
	}
	if got.Status.ActiveHosts.Console != "console--acme.kipper.run" || got.Status.ActiveHosts.Dex != "dex--acme.kipper.run" {
		t.Fatalf("adopted hosts wrong: %+v", got.Status.ActiveHosts)
	}
	if got.Status.Transition != nil {
		t.Fatal("adoption must not open a transition")
	}
	// The three Ingresses were applied.
	for _, name := range []struct{ ns, n string }{{"kipper-system", "console"}, {"kipper-system", "console-api"}, {"dex", "dex"}} {
		var ing networkingv1.Ingress
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: name.ns, Name: name.n}, &ing); err != nil {
			t.Fatalf("ingress %s/%s not applied: %v", name.ns, name.n, err)
		}
	}
}

func TestReconcileInitiatesTransitionOnDomainChange(t *testing.T) {
	// Already steady on single-dash hosts; spec asks for the double-dash identity.
	active := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{ActiveHosts: active, Steady: steadyFor("acme.kipper.run", active)})
	r, c := reconcilerFor(ci)

	res := reconcileOnce(t, r)
	if !requeued(res) {
		t.Fatal("initiating a transition should requeue")
	}
	got := getCI(t, c)
	if got.Status.Transition == nil {
		t.Fatal("a transition should be open")
	}
	tr := got.Status.Transition
	if tr.Phase != phaseDualServe {
		t.Fatalf("transition should start at DualServe, got %q", tr.Phase)
	}
	if tr.From == nil || tr.From.Console != "console-acme.kipper.run" {
		t.Fatalf("from should be the active hosts, got %+v", tr.From)
	}
	if tr.To == nil || tr.To.Console != "console--acme.kipper.run" {
		t.Fatalf("to should be the target hosts, got %+v", tr.To)
	}
	if tr.FromIdentity == nil || tr.FromIdentity.Domain != "acme.kipper.run" || tr.FromIdentity.Hosts == nil || tr.FromIdentity.Hosts.Dex != "dex-acme.kipper.run" {
		t.Fatalf("the transition must snapshot the outgoing steady identity, got %+v", tr.FromIdentity)
	}
	if len(tr.Nonce) != 32 || strings.Trim(tr.Nonce, "0123456789abcdef") != "" {
		t.Fatalf("nonce should be a 128-bit separator-free hex string, got %q", tr.Nonce)
	}
}

// A CR whose status has no recorded steady identity must refuse to open a
// transition: opening one would destroy the rollback information.
func TestInitiateTransitionRefusesWithoutSteadyIdentity(t *testing.T) {
	active := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{ActiveHosts: active})
	r, c := reconcilerFor(ci)

	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.Transition != nil {
		t.Fatalf("no transition may open without a steady identity to roll back to, got %+v", got.Status.Transition)
	}
	if reason := readyReason(got); reason != "NoSteadyIdentity" {
		t.Fatalf("the refusal must be visible on the Ready condition, got %q", reason)
	}
}

func TestReconcileSteadyIsIdempotent(t *testing.T) {
	active := &kipperv1.ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run", Issuer: "https://dex--acme.kipper.run/dex"}
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{ActiveHosts: active})
	r, c := reconcilerFor(ci)

	reconcileOnce(t, r)
	got := getCI(t, c)
	if got.Status.Transition != nil {
		t.Fatal("a converged cluster must not open a transition")
	}
	if c := readyReason(got); c != "Steady" {
		t.Fatalf("expected Ready reason Steady, got %q", c)
	}
}

func TestDriveDualServeAdvancesToAwaitingApproval(t *testing.T) {
	from := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	to := &kipperv1.ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run", Issuer: "https://dex--acme.kipper.run/dex"}
	ci := newCI(
		kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"},
		kipperv1.ClusterIdentityStatus{
			ActiveHosts: from,
			Transition:  &kipperv1.TransitionStatus{Phase: phaseDualServe, From: from, To: to, Nonce: "abcd"},
		},
	)
	r, c := reconcilerFor(ci)

	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("DualServe with applied ingresses should advance to AwaitingApproval, got %q", got.Status.Transition.Phase)
	}
	// The union console Ingress serves both the old and new hosts.
	var ing networkingv1.Ingress
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kipper-system", Name: "console"}, &ing); err != nil {
		t.Fatalf("console ingress not applied: %v", err)
	}
	hosts := map[string]bool{}
	for _, ru := range ing.Spec.Rules {
		hosts[ru.Host] = true
	}
	if !hosts["console-acme.kipper.run"] || !hosts["console--acme.kipper.run"] {
		t.Fatalf("dual-serve console ingress must serve both hosts, got %v", hosts)
	}
}

// CuttingOver refuses to flip the issuer while the dex-oidc-client Secret is
// missing: flipping into an empty client secret would break login, so the step
// trips auto-revert instead.
func TestCuttingOverWithoutClientSecretAutoReverts(t *testing.T) {
	from := oldHosts()
	to := newHosts()
	dexCM := dexConfigCM(oldConfig)
	ci := transitioningCI(phaseCuttingOver, from, to)
	r, c := reconcilerFor(ci, dexCM, dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex"))

	got := drive(t, r, c)
	if got.Status.Transition.Phase != phaseDegraded {
		t.Fatalf("missing client secret must revert and park Degraded, got %q", got.Status.Transition.Phase)
	}
	// The issuer was reverted (still old), never flipped forward.
	if iss := issuerOf(t, c); iss != "https://dex-acme.kipper.run/dex" {
		t.Fatalf("revert must keep the old issuer, got %q", iss)
	}
}

func TestApplyDexConfigRestartsDexOnChange(t *testing.T) {
	dexCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMapName, Namespace: dexNamespace},
		Data:       map[string]string{dexConfigKey: "issuer: https://dex-acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console-acme.kipper.run/callback]\n  secret: keep\n- id: kipper-cli\n  public: true\n  redirectURIs: [http://localhost:18741/callback]\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"},
	}
	dexDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: dexDeploymentName, Namespace: dexNamespace}}
	from := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run"}
	to := &kipperv1.ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run"}
	ci := newCI(
		kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"},
		kipperv1.ClusterIdentityStatus{
			ActiveHosts: from,
			Transition:  &kipperv1.TransitionStatus{Phase: phaseDualServe, From: from, To: to, Nonce: "abcd"},
		},
	)
	r, c := reconcilerFor(ci, dexCM, dexDep)

	reconcileOnce(t, r)

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: dexDeploymentName, Namespace: dexNamespace}, &dep); err != nil {
		t.Fatalf("get dex deployment: %v", err)
	}
	if dep.Spec.Template.Annotations[dexConfigHashAnnotation] == "" {
		t.Fatal("dex should have been restarted (config hash annotation set) after the dual-serve config change")
	}
}

// The issuer flip is refused outside a cutover: a bare-spec CR on a cluster
// whose live Dex serves a different issuer must not be steady-adopted into a
// flip. It errors and leaves the live issuer untouched and the CR unadopted.
func TestAdoptRefusesToFlipIssuerOutsideCutover(t *testing.T) {
	liveConfig := "issuer: https://dex-acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console-acme.kipper.run/callback]\n  secret: keep\n- id: kipper-cli\n  public: true\n  redirectURIs: [http://localhost:18741/callback]\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"
	dexCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMapName, Namespace: dexNamespace},
		Data:       map[string]string{dexConfigKey: liveConfig},
	}
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{})
	r, c := reconcilerFor(ci, dexCM)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: ClusterIdentityName}})
	if err == nil {
		t.Fatal("adoption that would flip the issuer must fail, not apply")
	}
	var cm corev1.ConfigMap
	if getErr := c.Get(context.Background(), types.NamespacedName{Name: dexConfigMapName, Namespace: dexNamespace}, &cm); getErr != nil {
		t.Fatalf("get dex-config: %v", getErr)
	}
	if cm.Data[dexConfigKey] != liveConfig {
		t.Fatalf("live dex-config issuer must be untouched, got:\n%s", cm.Data[dexConfigKey])
	}
	if got := getCI(t, c); got.Status.ActiveHosts != nil {
		t.Fatal("a refused adoption must not record activeHosts")
	}
}

// When the live Dex issuer already matches the spec target, adoption completes.
func TestAdoptWithMatchingLiveDexConfigSucceeds(t *testing.T) {
	liveConfig := "issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\n- id: kipper-cli\n  public: true\n  redirectURIs: [http://localhost:18741/callback]\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"
	dexCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMapName, Namespace: dexNamespace},
		Data:       map[string]string{dexConfigKey: liveConfig},
	}
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{})
	r, c := reconcilerFor(ci, dexCM)

	reconcileOnce(t, r)
	if got := getCI(t, c); got.Status.ActiveHosts == nil || got.Status.ActiveHosts.Dex != "dex--acme.kipper.run" {
		t.Fatalf("matching adoption should complete, got %+v", got.Status.ActiveHosts)
	}
}

// AwaitingApproval is a quiet parking state: it neither requeues nor rewrites
// status, so it does not hot-loop while it waits for the CLI to approve.
func TestAwaitingApprovalParksQuietly(t *testing.T) {
	from := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	to := &kipperv1.ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run", Issuer: "https://dex--acme.kipper.run/dex"}
	ci := newCI(
		kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"},
		kipperv1.ClusterIdentityStatus{
			ActiveHosts: from,
			Transition:  &kipperv1.TransitionStatus{Phase: phaseAwaitingApproval, From: from, To: to, Nonce: "abcd"},
		},
	)
	r, c := reconcilerFor(ci)

	res := reconcileOnce(t, r)
	if requeued(res) {
		t.Fatalf("AwaitingApproval must idle (no requeue), got %+v", res)
	}
	if got := getCI(t, c); got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("AwaitingApproval must stay parked, got %q", got.Status.Transition.Phase)
	}
}

// readyReason returns the Reason of the Ready condition, or "".
func readyReason(ci *kipperv1.ClusterIdentity) string {
	for _, c := range ci.Status.Conditions {
		if c.Type == condReady {
			return c.Reason
		}
	}
	return ""
}

// consoleAPIWithEnv is a live console-api Deployment carrying exactly the env a
// cluster in the field has, so tests can assert what a reconcile does to it.
func consoleAPIWithEnv(env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: consoleAPIDeploymentName, Namespace: kipperSystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: consoleAPIContainerName, Env: env}},
				},
			},
		},
	}
}

func deployedEnv(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: consoleAPIDeploymentName, Namespace: kipperSystemNamespace}, &dep); err != nil {
		t.Fatalf("get console-api deployment: %v", err)
	}
	out := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

// Steady state reconciles the env family too, so a cluster whose gateway config
// was blanked by an earlier transition (or never delivered by an upgrade) heals
// on the next reconcile instead of waiting for another host change.
func TestSteadyReconcileRestoresGatewayEnv(t *testing.T) {
	ci := newCI(kipperv1.ClusterIdentitySpec{
		Domain:  "acme.kipper.run",
		Gateway: &kipperv1.GatewaySpec{KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7"},
	}, kipperv1.ClusterIdentityStatus{})
	// The Deployment carries the damage: both vars present but empty.
	dep := consoleAPIWithEnv(
		corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: ""},
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: ""},
		corev1.EnvVar{Name: "SIDECAR_IMAGE", Value: "ghcr.io/example/sidecar:latest"},
	)
	r, c := reconcilerFor(ci, dep)

	reconcileOnce(t, r)

	env := deployedEnv(t, c)
	if env["KIPPER_RUN_DOMAIN"] != "acme.kipper.run" || env["CLUSTER_HOST"] != "203.0.113.7" {
		t.Errorf("steady reconcile must restore the gateway env, got %q / %q", env["KIPPER_RUN_DOMAIN"], env["CLUSTER_HOST"])
	}
	if env["SIDECAR_IMAGE"] != "ghcr.io/example/sidecar:latest" {
		t.Error("unrelated env must survive the patch")
	}
	if env["DEX_ISSUER"] != "https://dex--acme.kipper.run/dex" {
		t.Errorf("the host-derived family is applied too, got %q", env["DEX_ISSUER"])
	}
}

// A CR that cannot say what the gateway config is must leave the running values
// alone. This is the case every cluster installed before the CR carried a host
// is in, and blanking there is what stopped their heartbeats.
func TestSteadyReconcileLeavesUnknownGatewayEnvAlone(t *testing.T) {
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{})
	dep := consoleAPIWithEnv(
		corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: "acme.kipper.run"},
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	)
	r, c := reconcilerFor(ci, dep)

	reconcileOnce(t, r)

	env := deployedEnv(t, c)
	if env["KIPPER_RUN_DOMAIN"] != "acme.kipper.run" || env["CLUSTER_HOST"] != "203.0.113.7" {
		t.Errorf("a reconcile that does not know the gateway config must not erase it, got %q / %q",
			env["KIPPER_RUN_DOMAIN"], env["CLUSTER_HOST"])
	}
}

// register:false is the documented opt-out, so a reconcile must take the gateway
// config away rather than keep renewing a registration the operator refused.
func TestSteadyReconcileClearsGatewayEnvWhenRegistrationRefused(t *testing.T) {
	no := false
	ci := newCI(kipperv1.ClusterIdentitySpec{
		Domain: "acme.kipper.run",
		Gateway: &kipperv1.GatewaySpec{
			KipperRunDomain: "acme.kipper.run",
			ClusterHost:     "203.0.113.7",
			Register:        &no,
		},
	}, kipperv1.ClusterIdentityStatus{})
	dep := consoleAPIWithEnv(
		corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: "acme.kipper.run"},
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	)
	r, c := reconcilerFor(ci, dep)

	reconcileOnce(t, r)

	env := deployedEnv(t, c)
	if env["KIPPER_RUN_DOMAIN"] != "" || env["CLUSTER_HOST"] != "" {
		t.Errorf("an explicit opt-out must clear the gateway env, got %q / %q",
			env["KIPPER_RUN_DOMAIN"], env["CLUSTER_HOST"])
	}
}

// Every applied Ingress references the security-headers Middleware in its own
// namespace, and Traefik drops a router whose middleware is missing. A cluster
// installed before that Middleware existed therefore goes to a 404 on console,
// console-api and Dex the first time this reconciler re-applies its Ingresses —
// unless the reconciler applies the Middleware too.
func TestSteadyReconcileAppliesTheMiddlewareItsIngressesReference(t *testing.T) {
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{})
	r, c := reconcilerFor(ci)

	reconcileOnce(t, r)

	for _, ns := range []string{"kipper-system", "dex"} {
		var ing networkingv1.Ingress
		name := types.NamespacedName{Namespace: ns, Name: map[string]string{"kipper-system": "console", "dex": "dex"}[ns]}
		if err := c.Get(context.Background(), name, &ing); err != nil {
			t.Fatalf("ingress %s not applied: %v", name, err)
		}
		ref := ing.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]
		if ref == "" {
			t.Fatalf("ingress %s carries no middleware reference", name)
		}

		mw := serving.SecurityHeadersMiddleware(ns)
		if want := ns + "-" + mw.GetName() + "@kubernetescrd"; ref != want {
			t.Fatalf("ingress %s references %q, want %q", name, ref, want)
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(mw.GroupVersionKind())
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: mw.GetName()}, live); err != nil {
			t.Fatalf("middleware %s/%s referenced but not applied: %v", ns, mw.GetName(), err)
		}
		headers, found, err := unstructured.NestedMap(live.Object, "spec", "headers")
		if err != nil || !found {
			t.Fatalf("middleware %s/%s has no spec.headers (found=%v err=%v)", ns, mw.GetName(), found, err)
		}
		if headers["stsSeconds"] == nil {
			t.Errorf("middleware %s/%s must set HSTS, got %v", ns, mw.GetName(), headers)
		}
	}
}
