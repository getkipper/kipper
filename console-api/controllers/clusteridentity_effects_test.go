package controllers

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/dexcfg"
)

// A dash migration keeps the same base domain, so the admin email is unchanged.
const oldConfig = "issuer: https://dex-acme.kipper.run/dex\n" +
	"staticClients:\n" +
	"- id: kipper-console\n  redirectURIs: [https://console-acme.kipper.run/callback]\n  secret: keep\n" +
	"- id: kipper-cli\n  public: true\n  redirectURIs: [http://localhost:18741/callback]\n" +
	"staticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"

const connectorConfig = oldConfig +
	"connectors:\n- type: github\n  id: github\n  name: GitHub\n  config:\n    redirectURI: https://dex-acme.kipper.run/dex/callback\n"

func i32(v int32) *int32 { return &v }

// fakeProber stands in for the live in-cluster OIDC probe.
type fakeProber struct {
	served bool
	err    error
}

func (f fakeProber) IssuerServed(_ context.Context, _ string) (bool, error) {
	return f.served, f.err
}

func oldHosts() *kipperv1.ResolvedHosts {
	return &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
}

func newHosts() *kipperv1.ResolvedHosts {
	return &kipperv1.ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run", Issuer: "https://dex--acme.kipper.run/dex"}
}

func dexConfigCM(cfg string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMapName, Namespace: dexNamespace},
		Data:       map[string]string{dexConfigKey: cfg},
	}
}

func dexDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: dexDeploymentName, Namespace: dexNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: i32(1)},
	}
}

func consoleAPIDeployment(issuer string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: consoleAPIDeploymentName, Namespace: kipperSystemNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32(1),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: consoleAPIContainerName,
				Env:  []corev1.EnvVar{{Name: "DEX_ISSUER", Value: issuer}, {Name: "SIDECAR_IMAGE", Value: "keep"}},
			}}}},
		},
	}
}

func dexSecrets() []client.Object {
	mk := func(ns string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dexOIDCClientSecretName, Namespace: ns},
			Data:       map[string][]byte{dexOIDCClientSecretKey: []byte("s3cr3t")},
		}
	}
	return []client.Object{mk(dexNamespace), mk(kipperSystemNamespace)}
}

// steadyFor snapshots a resolved host set as the steady identity it served
// under, the shape openTransition records and rollback restores.
func steadyFor(domain string, h *kipperv1.ResolvedHosts) *kipperv1.SteadyIdentity {
	return &kipperv1.SteadyIdentity{
		Domain: domain,
		Hosts:  &kipperv1.IdentityHosts{Console: h.Console, ConsoleAPI: h.ConsoleAPI, Dex: h.Dex},
	}
}

// transitioningCI is a CR paused mid-transition at the given phase, generation
// and observedGeneration aligned at 1 with a fixed nonce for approval math.
// Steady, FromIdentity, and ToIdentity carry the endpoint identities, as they
// do on any transition the reconciler opened itself (the target here is the
// spec's derived hosts, so ToIdentity has no overrides).
func transitioningCI(phase string, from, to *kipperv1.ResolvedHosts) *kipperv1.ClusterIdentity {
	return newCI(
		kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"},
		kipperv1.ClusterIdentityStatus{
			ActiveHosts:        from,
			Steady:             steadyFor("acme.kipper.run", from),
			ObservedGeneration: 1,
			Transition: &kipperv1.TransitionStatus{
				Phase:        phase,
				From:         from,
				FromIdentity: steadyFor("acme.kipper.run", from),
				ToIdentity:   &kipperv1.SteadyIdentity{Domain: "acme.kipper.run"},
				To:           to,
				Nonce:        "abcd",
			},
		},
	)
}

func issuerOf(t *testing.T, c client.Client) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: dexConfigMapName, Namespace: dexNamespace}, &cm); err != nil {
		t.Fatalf("get dex-config: %v", err)
	}
	cfg, err := dexcfg.Load(cm.Data[dexConfigKey])
	if err != nil {
		t.Fatalf("load dex-config: %v", err)
	}
	return cfg.Issuer()
}

// markReady simulates a Deployment finishing its rollout on the current
// generation. The fake client treats built-in Deployment status as a
// subresource, so it must be written through Status().
func markReady(t *testing.T, c client.Client, name, ns string) {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &dep); err != nil {
		return
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.UpdatedReplicas = 1
	dep.Status.AvailableReplicas = 1
	dep.Status.UnavailableReplicas = 0
	if err := c.Status().Update(context.Background(), &dep); err != nil {
		t.Fatalf("mark %s/%s ready: %v", ns, name, err)
	}
}

// requeued reports whether a reconcile asked to be re-run soon.
func requeued(res ctrl.Result) bool {
	return res.Requeue || res.RequeueAfter > 0 //nolint:staticcheck // the reconciler signals immediate requeue via the Requeue field
}

// drive reconciles repeatedly, marking both deployments ready after each pass, so
// multi-step rollouts converge. It stops when the transition clears or parks
// quietly (AwaitingApproval or Degraded).
func drive(t *testing.T, r *ClusterIdentityReconciler, c client.Client) *kipperv1.ClusterIdentity {
	t.Helper()
	const maxSteps = 16
	for i := 0; i < maxSteps; i++ {
		res := reconcileOnce(t, r)
		markReady(t, c, dexDeploymentName, dexNamespace)
		markReady(t, c, consoleAPIDeploymentName, kipperSystemNamespace)
		ci := getCI(t, c)
		if ci.Status.Transition == nil {
			return ci
		}
		if !requeued(res) && (ci.Status.Transition.Phase == phaseAwaitingApproval || ci.Status.Transition.Phase == phaseDegraded) {
			return ci
		}
	}
	t.Fatalf("transition did not settle within %d steps", maxSteps)
	return nil
}

func TestFullCutoverHappyPath(t *testing.T) {
	from := oldHosts()
	ci := newCI(kipperv1.ClusterIdentitySpec{Domain: "acme.kipper.run"}, kipperv1.ClusterIdentityStatus{ActiveHosts: from, Steady: steadyFor("acme.kipper.run", from)})
	objs := append([]client.Object{ci, dexConfigCM(oldConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	got := drive(t, r, c)
	if got.Status.Transition == nil || got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("expected to park at AwaitingApproval, got %+v", got.Status.Transition)
	}

	tr := got.Status.Transition
	got.Spec.CutoverApproval = kipperv1.CutoverApprovalHash(got.Status.ObservedGeneration, *tr.From, *tr.To, tr.Nonce)
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatalf("write approval: %v", err)
	}

	final := drive(t, r, c)
	if final.Status.Transition != nil {
		t.Fatalf("cutover should complete to steady, stuck at %+v", final.Status.Transition)
	}
	if final.Status.ActiveHosts == nil || final.Status.ActiveHosts.Dex != "dex--acme.kipper.run" {
		t.Fatalf("active identity should be the new hosts, got %+v", final.Status.ActiveHosts)
	}
	if final.Status.LastSteady == nil || final.Status.LastSteady.Hosts == nil || final.Status.LastSteady.Hosts.Dex != "dex-acme.kipper.run" {
		t.Fatalf("lastSteady should capture the previous hosts as the rollback target, got %+v", final.Status.LastSteady)
	}
	if final.Status.LastSteady.Domain != "acme.kipper.run" {
		t.Fatalf("lastSteady should carry the previous spec domain verbatim, got %q", final.Status.LastSteady.Domain)
	}
	if final.Status.Steady == nil || final.Status.Steady.Domain != "acme.kipper.run" {
		t.Fatalf("steady should be refreshed to the new spec identity, got %+v", final.Status.Steady)
	}
	if iss := issuerOf(t, c); iss != "https://dex--acme.kipper.run/dex" {
		t.Fatalf("issuer should be flipped to the new identity, got %q", iss)
	}

	var ca appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: consoleAPIDeploymentName, Namespace: kipperSystemNamespace}, &ca); err != nil {
		t.Fatalf("get console-api: %v", err)
	}
	env := ca.Spec.Template.Spec.Containers[0].Env
	if envValue(env, "DEX_ISSUER") != "https://dex--acme.kipper.run/dex" {
		t.Fatal("console-api DEX_ISSUER should be flipped to the new issuer")
	}
	if envValue(env, "SIDECAR_IMAGE") != "keep" {
		t.Fatal("unrelated console-api env must be preserved")
	}
	if ca.Spec.Strategy.RollingUpdate == nil || ca.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
		t.Fatal("console-api rollout must be pinned to maxUnavailable:0")
	}

	// The old console host is pruned after contraction.
	var ing networkingv1.Ingress
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kipper-system", Name: "console"}, &ing); err != nil {
		t.Fatalf("get console ingress: %v", err)
	}
	for _, ru := range ing.Spec.Rules {
		if ru.Host == "console-acme.kipper.run" {
			t.Fatal("old console host should be pruned after contraction")
		}
	}
}

func TestAwaitingApprovalAdvancesOnValidApproval(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseAwaitingApproval, from, to)
	ci.Spec.CutoverApproval = kipperv1.CutoverApprovalHash(1, *from, *to, "abcd")
	objs := append([]client.Object{ci, dexConfigCM(oldConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	reconcileOnce(t, r)

	if got := getCI(t, c); got.Status.Transition.Phase != phaseCuttingOver {
		t.Fatalf("valid approval + met preconditions should advance to CuttingOver, got %q", got.Status.Transition.Phase)
	}
}

func TestAwaitingApprovalParksOnWrongApproval(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseAwaitingApproval, from, to)
	ci.Spec.CutoverApproval = "not-the-hash"
	objs := append([]client.Object{ci, dexConfigCM(oldConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	reconcileOnce(t, r)

	if got := getCI(t, c); got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("a mismatched approval must not advance, got %q", got.Status.Transition.Phase)
	}
	if iss := issuerOf(t, c); iss != "https://dex-acme.kipper.run/dex" {
		t.Fatalf("no approval means no flip; issuer should stay old, got %q", iss)
	}
}

func TestConnectorsRequireAcknowledgement(t *testing.T) {
	from, to := oldHosts(), newHosts()
	approval := kipperv1.CutoverApprovalHash(1, *from, *to, "abcd")

	// Approved, but SSO connectors present and not acknowledged: must park.
	ci := transitioningCI(phaseAwaitingApproval, from, to)
	ci.Spec.CutoverApproval = approval
	objs := append([]client.Object{ci, dexConfigCM(connectorConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	reconcileOnce(t, r)
	if got := getCI(t, c); got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("unacknowledged SSO rehost must block the cutover, got %q", got.Status.Transition.Phase)
	}

	// An acknowledgement bound to a different Dex host is stale (given for an
	// earlier move) and must not unlock this one.
	ci2 := transitioningCI(phaseAwaitingApproval, from, to)
	ci2.Spec.CutoverApproval = approval
	ci2.Spec.AckSSOCallbacksFor = "dex.example.com"
	objs2 := append([]client.Object{ci2, dexConfigCM(connectorConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r2, c2 := reconcilerFor(objs2...)

	reconcileOnce(t, r2)
	if got := getCI(t, c2); got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("an acknowledgement for another host must block the cutover, got %q", got.Status.Transition.Phase)
	}

	// Acknowledge the actual target host: now it advances.
	ci3 := transitioningCI(phaseAwaitingApproval, from, to)
	ci3.Spec.CutoverApproval = approval
	ci3.Spec.AckSSOCallbacksFor = to.Dex
	objs3 := append([]client.Object{ci3, dexConfigCM(connectorConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r3, c3 := reconcilerFor(objs3...)

	reconcileOnce(t, r3)
	if got := getCI(t, c3); got.Status.Transition.Phase != phaseCuttingOver {
		t.Fatalf("acknowledged SSO rehost should advance to CuttingOver, got %q", got.Status.Transition.Phase)
	}
}

func TestVerifyingAutoRevertsOnRolloutFailure(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseVerifying, from, to)
	// Live cluster already flipped to the new issuer at cutover.
	dexCM := dexConfigCM("issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n")
	ca := consoleAPIDeployment("https://dex--acme.kipper.run/dex")
	ca.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentProgressing,
		Status: corev1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded",
	}}
	objs := append([]client.Object{ci, dexCM, dexDeployment(), ca}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	// The first pass only persists the Reverting phase; the reverse effects
	// run on later passes, once the phase write cannot be lost.
	reconcileOnce(t, r)
	if got := getCI(t, c); got.Status.Transition.Phase != phaseReverting {
		t.Fatalf("a failed rollout in Verifying must persist Reverting before any reverse effect, got %q", got.Status.Transition.Phase)
	}
	if iss := issuerOf(t, c); iss != "https://dex--acme.kipper.run/dex" {
		t.Fatalf("no reverse effect may run before the Reverting phase is persisted, got issuer %q", iss)
	}

	got := drive(t, r, c)
	if got.Status.Transition.Phase != phaseDegraded {
		t.Fatalf("a failed rollout in Verifying must revert and park Degraded, got %q", got.Status.Transition.Phase)
	}
	if iss := issuerOf(t, c); iss != "https://dex-acme.kipper.run/dex" {
		t.Fatalf("revert must restore the previous issuer, got %q", iss)
	}
	if envValue(mustGetConsoleAPI(t, c).Spec.Template.Spec.Containers[0].Env, "DEX_ISSUER") != "https://dex-acme.kipper.run/dex" {
		t.Fatal("revert must restore console-api DEX_ISSUER to the previous identity")
	}
}

// The approval and SSO-ack writes of a failed attempt advance the generation
// without changing the requested identity. That skew must not reopen a
// transition toward the target that just failed.
func TestDegradedParksWhenSpecStillNamesFailedTarget(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseDegraded, from, to)
	ci.Generation = 3

	r, c := reconcilerFor(ci, dexConfigCM(oldConfig))
	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.Transition == nil || got.Status.Transition.Phase != phaseDegraded {
		t.Fatalf("Degraded must stay parked while spec still names the failed target, got %+v", got.Status.Transition)
	}
}

// A failed base-domain move must revert every spec-derived value to the
// outgoing identity, not just the issuer: a reverted cluster with the new
// domain's cookie scope cannot log in on the restored old hosts.
func TestRevertRestoresCoherentOldIdentityOnBaseDomainMove(t *testing.T) {
	from := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	to := &kipperv1.ResolvedHosts{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com", Issuer: "https://dex.example.com/dex"}

	ci := newCI(
		kipperv1.ClusterIdentitySpec{
			Domain: "example.com",
			Hosts:  &kipperv1.IdentityHosts{Console: to.Console, ConsoleAPI: to.ConsoleAPI, Dex: to.Dex},
		},
		kipperv1.ClusterIdentityStatus{
			ActiveHosts:        from,
			Steady:             steadyFor("acme.kipper.run", from),
			ObservedGeneration: 1,
			Transition: &kipperv1.TransitionStatus{
				Phase:        phaseVerifying,
				From:         from,
				FromIdentity: steadyFor("acme.kipper.run", from),
				ToIdentity:   steadyFor("example.com", to),
				To:           to,
				Nonce:        "abcd",
			},
		},
	)
	stale := metav1.NewTime(time.Now().Add(-2 * cutoverDeadline))
	ci.Status.Transition.CutoverStartedAt = &stale

	flipped := "issuer: https://dex.example.com/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console.example.com/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@example.com, hash: H, username: admin}\n"
	ca := consoleAPIDeployment("https://dex.example.com/dex")
	ca.Spec.Template.Spec.Containers[0].Env = append(ca.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "CLUSTER_DOMAIN", Value: "example.com"})
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDeployment(), ca}, dexSecrets()...)
	r, c := reconcilerFor(objs...)
	r.Prober = fakeProber{served: false}

	got := drive(t, r, c)
	if got.Status.Transition.Phase != phaseDegraded {
		t.Fatalf("expected the deadline to revert and park Degraded, got %q", got.Status.Transition.Phase)
	}
	if iss := issuerOf(t, c); iss != "https://dex-acme.kipper.run/dex" {
		t.Fatalf("revert must restore the previous issuer, got %q", iss)
	}
	env := mustGetConsoleAPI(t, c).Spec.Template.Spec.Containers[0].Env
	if envValue(env, "DEX_ISSUER") != "https://dex-acme.kipper.run/dex" {
		t.Fatal("revert must restore console-api DEX_ISSUER to the previous identity")
	}
	if envValue(env, "CLUSTER_DOMAIN") != "acme.kipper.run" {
		t.Fatalf("revert must restore CLUSTER_DOMAIN to the outgoing identity's domain, got %q", envValue(env, "CLUSTER_DOMAIN"))
	}
}

// A spec edit that lands after the issuer flip does not reset the transition,
// so completion must record the transition's own target as steady, not the
// edited spec that was never applied.
func TestFinishRecordsTransitionTargetNotLiveSpec(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseContracting, from, to)
	ci.Spec.Domain = "third.example.com"
	ci.Generation = 2

	flipped := "issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDeployment(), consoleAPIDeployment("https://dex--acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.Transition != nil && got.Status.Transition.Phase == phaseContracting {
		// Contraction may need a second pass once the render applies.
		reconcileOnce(t, r)
		got = getCI(t, c)
	}
	if got.Status.Steady == nil || got.Status.Steady.Domain != "acme.kipper.run" {
		t.Fatalf("steady must record the transition's target identity, got %+v", got.Status.Steady)
	}
	if got.Status.LastSteady == nil || got.Status.LastSteady.Domain != "acme.kipper.run" || got.Status.LastSteady.Hosts.Dex != "dex-acme.kipper.run" {
		t.Fatalf("lastSteady must be the transition's FromIdentity, got %+v", got.Status.LastSteady)
	}
}

func TestDegradedResumesAfterSpecChange(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseDegraded, from, to)
	// Operator rolls back: spec now names the old identity via explicit overrides,
	// and the generation advances past observedGeneration.
	ci.Generation = 5
	ci.Spec.Hosts = &kipperv1.IdentityHosts{Console: from.Console, ConsoleAPI: from.ConsoleAPI, Dex: from.Dex}
	r, c := reconcilerFor(ci, dexConfigCM(oldConfig))

	reconcileOnce(t, r)

	if got := getCI(t, c); got.Status.Transition != nil {
		t.Fatalf("a spec change must clear the degraded transition, got %+v", got.Status.Transition)
	}
}

func TestContractionRespectsGracePeriod(t *testing.T) {
	from, to := oldHosts(), newHosts()
	flipped := "issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"

	ci := transitioningCI(phaseContracting, from, to)
	future := metav1.NewTime(time.Now().Add(time.Hour))
	ci.Spec.KeepOldHostsUntil = &future
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDeployment(), consoleAPIDeployment("https://dex--acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.Transition == nil || got.Status.Transition.Phase != phaseContracting {
		t.Fatalf("contraction must wait for the grace period, got %+v", got.Status.Transition)
	}
	// Old host still served during the grace period.
	var ing networkingv1.Ingress
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kipper-system", Name: "console"}, &ing); err != nil {
		t.Fatalf("get console ingress: %v", err)
	}
	served := map[string]bool{}
	for _, ru := range ing.Spec.Rules {
		served[ru.Host] = true
	}
	if !served["console-acme.kipper.run"] || !served["console--acme.kipper.run"] {
		t.Fatalf("grace period must keep both hosts serving, got %v", served)
	}
}

func TestResumeAtCuttingOverFlipsConsoleAPI(t *testing.T) {
	from, to := oldHosts(), newHosts()
	// Live state: Dex already flipped and Ready, console-api not yet flipped. This
	// is the crash-resume point right after Dex rolled.
	flipped := "issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback, https://console-acme.kipper.run/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"
	dexDep := dexDeployment()
	ci := transitioningCI(phaseCuttingOver, from, to)
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDep, consoleAPIDeployment("https://dex-acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	// Prime the Dex pod hash so dexReadyOnConfig sees it serving the flipped config.
	drive(t, r, c)

	if envValue(mustGetConsoleAPI(t, c).Spec.Template.Spec.Containers[0].Env, "DEX_ISSUER") != "https://dex--acme.kipper.run/dex" {
		t.Fatal("resuming at CuttingOver must flip console-api onto the new issuer")
	}
}

func TestBaseDomainMovePrunesOldAdmin(t *testing.T) {
	// Move from a *.kipper.run domain to a custom domain: the admin email changes,
	// so the new one is authorized at cutover and the old one pruned at contraction.
	from := &kipperv1.ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	to := &kipperv1.ResolvedHosts{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com", Issuer: "https://dex.example.com/dex"}

	ci := newCI(
		kipperv1.ClusterIdentitySpec{
			Domain: "example.com",
			Hosts:  &kipperv1.IdentityHosts{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com"},
		},
		kipperv1.ClusterIdentityStatus{
			ActiveHosts:        from,
			Steady:             steadyFor("acme.kipper.run", from),
			ObservedGeneration: 1,
			Transition:         &kipperv1.TransitionStatus{Phase: phaseContracting, From: from, FromIdentity: steadyFor("acme.kipper.run", from), To: to, Nonce: "abcd"},
		},
	)
	// Live config already flipped to the new domain (admin email moved at cutover),
	// and kipper-users holds both the old and new admin from the flip.
	flipped := "issuer: https://dex.example.com/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console.example.com/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@example.com, hash: H, username: admin}\n"
	users := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: kipperUsersConfigMapName, Namespace: kipperSystemNamespace},
		Data:       map[string]string{kipperUsersDataKey: `{"admin@acme.kipper.run":"admin","admin@example.com":"admin"}`},
	}
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDeployment(), consoleAPIDeployment("https://dex.example.com/dex"), users}, dexSecrets()...)
	r, c := reconcilerFor(objs...)

	drive(t, r, c)

	var got corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Name: kipperUsersConfigMapName, Namespace: kipperSystemNamespace}, &got); err != nil {
		t.Fatalf("get kipper-users: %v", err)
	}
	roles, err := unmarshalUsers(got.Data[kipperUsersDataKey])
	if err != nil {
		t.Fatalf("parse kipper-users: %v", err)
	}
	if _, ok := roles["admin@acme.kipper.run"]; ok {
		t.Fatalf("the old admin email should be pruned at contraction, still present: %v", roles)
	}
	if roles["admin@example.com"] != "admin" {
		t.Fatalf("the new admin email must remain authorized, got %v", roles)
	}
}

func TestVerifyingParksWhenIssuerNotServedLive(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseVerifying, from, to)
	started := metav1.NewTime(time.Now())
	ci.Status.Transition.CutoverStartedAt = &started
	flipped := "issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"
	dexDep := dexDeployment()
	dexDep.Status = appsv1.DeploymentStatus{ObservedGeneration: dexDep.Generation, UpdatedReplicas: 1, AvailableReplicas: 1}
	ca := consoleAPIDeployment("https://dex--acme.kipper.run/dex")
	ca.Status = appsv1.DeploymentStatus{ObservedGeneration: ca.Generation, UpdatedReplicas: 1, AvailableReplicas: 1}
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDep, ca}, dexSecrets()...)
	r, c := reconcilerFor(objs...)
	// Manifests are aligned, but Dex does not actually serve the new issuer.
	r.Prober = fakeProber{served: false}

	reconcileOnce(t, r)

	if got := getCI(t, c); got.Status.Transition.Phase != phaseVerifying {
		t.Fatalf("a not-yet-live issuer must not advance to Contracting, got %q", got.Status.Transition.Phase)
	}
}

func TestCutoverDeadlineGateRequiresFlip(t *testing.T) {
	ci := transitioningCI(phaseCuttingOver, oldHosts(), newHosts())
	// No CutoverStartedAt: the Dex issuer flip has not been written, so however
	// long a pre-flip blocker lingers the deadline never fires — the cluster stays
	// safely on the old identity.
	if cutoverDeadlineExceeded(ci) {
		t.Fatal("deadline must not fire before the issuer flip is written")
	}
	stale := metav1.NewTime(time.Now().Add(-2 * cutoverDeadline))
	ci.Status.Transition.CutoverStartedAt = &stale
	if !cutoverDeadlineExceeded(ci) {
		t.Fatal("deadline must fire once the post-flip clock has aged past it")
	}
}

func TestCutoverDeadlineAutoReverts(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseVerifying, from, to)
	// Flipped long ago and never verified: past the deadline.
	stale := metav1.NewTime(time.Now().Add(-2 * cutoverDeadline))
	ci.Status.Transition.CutoverStartedAt = &stale
	flipped := "issuer: https://dex--acme.kipper.run/dex\nstaticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\nstaticPasswords:\n- {email: admin@acme.kipper.run, hash: H, username: admin}\n"
	objs := append([]client.Object{ci, dexConfigCM(flipped), dexDeployment(), consoleAPIDeployment("https://dex--acme.kipper.run/dex")}, dexSecrets()...)
	r, c := reconcilerFor(objs...)
	r.Prober = fakeProber{served: false}

	got := drive(t, r, c)
	if got.Status.Transition.Phase != phaseDegraded {
		t.Fatalf("a cutover past its deadline must revert and park Degraded, got %q", got.Status.Transition.Phase)
	}
	if iss := issuerOf(t, c); iss != "https://dex-acme.kipper.run/dex" {
		t.Fatalf("deadline revert must restore the previous issuer, got %q", iss)
	}
}

func TestAwaitingApprovalPersistsBlockerCondition(t *testing.T) {
	from, to := oldHosts(), newHosts()
	ci := transitioningCI(phaseAwaitingApproval, from, to)
	ci.Spec.CutoverApproval = kipperv1.CutoverApprovalHash(1, *from, *to, "abcd")
	// Approved, but the dex-oidc-client Secret is absent, so the cutover is blocked.
	r, c := reconcilerFor(ci, dexConfigCM(oldConfig), dexDeployment(), consoleAPIDeployment("https://dex-acme.kipper.run/dex"))

	reconcileOnce(t, r)

	got := getCI(t, c)
	if got.Status.Transition.Phase != phaseAwaitingApproval {
		t.Fatalf("a missing client secret must block the cutover, got %q", got.Status.Transition.Phase)
	}
	var found bool
	for _, cond := range got.Status.Conditions {
		if cond.Type == condCutoverPreconditions {
			found = true
			if cond.Reason != "MissingClientSecret" {
				t.Fatalf("expected MissingClientSecret reason, got %q", cond.Reason)
			}
		}
	}
	if !found {
		t.Fatal("the blocking precondition must be persisted on the CR while parked at AwaitingApproval")
	}
}

func TestServingObjectWatchMapping(t *testing.T) {
	ingress := func(ns, n string) client.Object {
		return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}
	}
	cm := func(ns, n string) client.Object {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}
	}
	dep := func(ns, n string) client.Object {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}
	}
	ctx := context.Background()
	cases := []struct {
		name    string
		reqs    []reconcile.Request
		wantOne bool
	}{
		{"console ingress", mapServingIngress(ctx, ingress("kipper-system", "console")), true},
		{"dex ingress", mapServingIngress(ctx, ingress("dex", "dex")), true},
		{"unrelated ingress", mapServingIngress(ctx, ingress("kipper-system", "grafana")), false},
		{"dex-config cm", mapServingConfigMap(ctx, cm("dex", "dex-config")), true},
		{"kipper-users cm", mapServingConfigMap(ctx, cm("kipper-system", "kipper-users")), false},
		{"console-api deploy", mapServingDeployment(ctx, dep("kipper-system", "console-api")), true},
		{"console frontend deploy", mapServingDeployment(ctx, dep("kipper-system", "console")), false},
	}
	for _, tc := range cases {
		got := len(tc.reqs) == 1 && tc.reqs[0].Name == ClusterIdentityName
		if got != tc.wantOne {
			t.Errorf("%s: enqueued singleton=%v, want %v (reqs=%v)", tc.name, got, tc.wantOne, tc.reqs)
		}
	}
}

func mustGetConsoleAPI(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Name: consoleAPIDeploymentName, Namespace: kipperSystemNamespace}, &dep); err != nil {
		t.Fatalf("get console-api: %v", err)
	}
	return &dep
}
