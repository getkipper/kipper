package controllers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/dexcfg"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

// dualServePollInterval is how often DualServe re-checks that its Ingresses are
// serving. AwaitingApproval does not poll: it waits for the CLI to write an
// approval, which edits the spec and triggers a reconcile.
const dualServePollInterval = 10 * time.Second

// Namespaces and object names the reconciler drives. These match the installer's
// manifests; the reconciler and installer render the same objects.
const (
	dexNamespace      = "dex"
	dexConfigMapName  = "dex-config"
	dexConfigKey      = "config.yaml"
	dexDeploymentName = "dex"

	// servingFieldManager owns every object the render produces, which lets a
	// contraction prune old hosts simply by re-applying the render without them.
	servingFieldManager = "kipper-serving"

	// dexConfigHashAnnotation carries a hash of the rendered dex-config on the
	// Dex pod template, so the config change and its restart are one atomic
	// update and Dex restarts exactly once per content change.
	dexConfigHashAnnotation = "kipper.run/dex-config-hash"
)

// Condition types on status.conditions.
const (
	condIngressesReady = "IngressesReady"
	condReady          = "Ready"
)

// ClusterIdentityReconciler keeps the cluster's serving identity — the three
// Ingresses, the Dex config, and (at cutover) the console-api OIDC env — aligned
// with the ClusterIdentity CR, driving host changes through the no-lockout phase
// sequence. The spec is the desired end state; the transition lives in status
// and is written before each phase's mutations so a restart resumes rather than
// half-applies.
//
// The reconciler is the sole writer of the status subresource (G.5). A status
// write that conflicts returns the error, which requeues and re-reconciles from
// a fresh read; because every phase decision is re-derived from live state and
// the persisted phase, that requeue is the retry — re-mutating a stale cached
// object would be wrong here, so there is no inner retry loop around the writes.
type ClusterIdentityReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Prober performs the live in-cluster OIDC check during Verifying. Nil uses a
	// default HTTP prober against the in-cluster Dex Service.
	Prober oidcProber
	// Metrics reads the API server's /metrics so the cutover gate can confirm
	// the apiserver already trusts the new issuer (kip stages the
	// authentication config over SSH) before the in-cluster Dex flip. Nil
	// leaves the gate unenforced — production wires it in main.go; tests that
	// do not exercise the staged check leave it nil.
	Metrics MetricsReader
}

// MetricsReader returns the API server's /metrics text.
type MetricsReader interface {
	ReadMetrics(ctx context.Context) (string, error)
}

// prober returns the configured OIDC prober, or a default in-cluster HTTP one.
func (r *ClusterIdentityReconciler) prober() oidcProber {
	if r.Prober != nil {
		return r.Prober
	}
	return httpOIDCProber{client: &http.Client{Timeout: 5 * time.Second}}
}

// SetupWithManager registers the reconciler for the ClusterIdentity CR and for
// the objects it renders, so an out-of-band edit to a serving Ingress, the Dex
// config, or a managed Deployment is corrected within a reconcile.
//
// GenerationChangedPredicate on the CR suppresses triggers from the reconciler's
// own status writes, which it makes on every pass. The transition drives itself
// forward through explicit requeues (an immediate requeue between phases, a short
// poll while a phase waits), so it does not depend on CR status-change events;
// AwaitingApproval idles until the CLI edits the spec (writing the approval bumps
// the generation, which does pass the predicate). The owned-object watches map
// every serving object back to the singleton, and a steady re-apply of unchanged
// objects is a no-op that emits no further events, so drift repair does not loop.
func (r *ClusterIdentityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.ClusterIdentity{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&networkingv1.Ingress{}, handler.EnqueueRequestsFromMapFunc(mapServingIngress)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(mapServingConfigMap)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(mapServingDeployment)).
		Complete(r)
}

// singletonRequest enqueues the singleton reconcile when match is true.
func singletonRequest(match bool) []reconcile.Request {
	if !match {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ClusterIdentityName}}}
}

func mapServingIngress(_ context.Context, o client.Object) []reconcile.Request {
	ns, name := o.GetNamespace(), o.GetName()
	return singletonRequest(
		(ns == kipperSystemNamespace && (name == "console" || name == consoleAPIDeploymentName)) ||
			(ns == dexNamespace && name == dexDeploymentName))
}

func mapServingConfigMap(_ context.Context, o client.Object) []reconcile.Request {
	return singletonRequest(o.GetNamespace() == dexNamespace && o.GetName() == dexConfigMapName)
}

func mapServingDeployment(_ context.Context, o client.Object) []reconcile.Request {
	ns, name := o.GetNamespace(), o.GetName()
	return singletonRequest(
		(ns == dexNamespace && name == dexDeploymentName) ||
			(ns == kipperSystemNamespace && name == consoleAPIDeploymentName))
}

// Reconcile drives the singleton ClusterIdentity named "cluster".
func (r *ClusterIdentityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != ClusterIdentityName {
		return ctrl.Result{}, nil
	}
	var ci kipperv1.ClusterIdentity
	if err := r.Get(ctx, req.NamespacedName, &ci); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return r.reconcile(ctx, &ci)
}

func (r *ClusterIdentityReconciler) reconcile(ctx context.Context, ci *kipperv1.ClusterIdentity) (ctrl.Result, error) {
	target := serving.ResolveHosts(ci.Spec.Domain, overridesFromHosts(ci.Spec.Hosts))
	targetHosts := resolvedFromHostSet(target)

	switch {
	case ci.Status.ActiveHosts == nil && ci.Status.Transition == nil:
		// First reconcile: adopt the spec's identity as the steady state. The
		// installer already applied identical objects through the shared render,
		// so this changes nothing served — it only records ownership.
		return r.adopt(ctx, ci, targetHosts)

	case ci.Status.Transition == nil && hostsEqual(ci.Status.ActiveHosts, targetHosts):
		// Steady and converged: re-apply the render so any out-of-band edit
		// (kubectl surgery) reverts within a reconcile.
		return r.reconcileSteady(ctx, ci)

	case ci.Status.Transition == nil:
		// Steady but the spec now asks for a different identity: open a
		// transition. Persist it before any mutation so the next reconcile
		// resumes from a known phase.
		return r.initiateTransition(ctx, ci, targetHosts)

	default:
		return r.driveTransition(ctx, ci)
	}
}

// adopt records the spec's identity as steady without changing what is served.
func (r *ClusterIdentityReconciler) adopt(ctx context.Context, ci *kipperv1.ClusterIdentity, targetHosts *kipperv1.ResolvedHosts) (ctrl.Result, error) {
	if err := r.applySteady(ctx, ci); err != nil {
		return ctrl.Result{}, err
	}
	ci.Status.ActiveHosts = targetHosts
	ci.Status.Steady = specIdentity(ci)
	ci.Status.ObservedGeneration = ci.Generation
	setCondition(ci, condReady, metav1.ConditionTrue, "Adopted", "serving identity adopted from spec")
	return ctrl.Result{}, r.Status().Update(ctx, ci)
}

// reconcileSteady re-applies the steady render to correct drift. It also keeps
// status.steady current, so a transition opened by the next spec edit always
// has the pre-edit identity to snapshot.
func (r *ClusterIdentityReconciler) reconcileSteady(ctx context.Context, ci *kipperv1.ClusterIdentity) (ctrl.Result, error) {
	if err := r.applySteady(ctx, ci); err != nil {
		return ctrl.Result{}, err
	}
	ci.Status.Steady = specIdentity(ci)
	ci.Status.ObservedGeneration = ci.Generation
	setCondition(ci, condReady, metav1.ConditionTrue, "Steady", "serving the current identity")
	return ctrl.Result{}, r.Status().Update(ctx, ci)
}

// initiateTransition opens a DualServe transition toward the target, writing it
// to status before any object changes. It refuses when no steady identity has
// been recorded: without one there is nothing faithful to roll back to, and a
// transition that destroys its own rollback information must never open.
// Normal operation cannot hit this — adopt and every steady pass record the
// identity before a spec edit can land — so the park is purely defensive and
// is left by reverting spec to the serving identity for one steady pass.
func (r *ClusterIdentityReconciler) initiateTransition(ctx context.Context, ci *kipperv1.ClusterIdentity, targetHosts *kipperv1.ResolvedHosts) (ctrl.Result, error) {
	if ci.Status.Steady == nil {
		setCondition(ci, condReady, metav1.ConditionFalse, "NoSteadyIdentity", "refusing to open a host transition: no steady identity recorded to roll back to; revert spec to the serving identity, let the reconciler complete one steady pass, then retry the change")
		return ctrl.Result{}, r.Status().Update(ctx, ci)
	}
	return r.openTransition(ctx, ci, ci.Status.ActiveHosts, targetHosts, "Transitioning", "host change in progress: DualServe")
}

// driveTransition advances an in-flight transition by one step: it applies the
// current phase's effects (idempotently), gathers the observed signals, and lets
// the pure phase machine decide the next phase. Every mutation is level-driven
// and re-entrant, so the console-api rollout that restarts the reconciler's own
// pods mid-cutover resumes from the persisted phase rather than half-applying.
func (r *ClusterIdentityReconciler) driveTransition(ctx context.Context, ci *kipperv1.ClusterIdentity) (ctrl.Result, error) {
	t := ci.Status.Transition

	if t.Phase == phaseDegraded {
		return r.handleDegraded(ctx, ci)
	}

	// A pre-flip domain edit makes the transition target stale. Reset toward the
	// new target with a fresh nonce, which also invalidates any pending approval.
	if !identityIsNew(t.Phase) {
		target := resolvedFromHostSet(serving.ResolveHosts(ci.Spec.Domain, overridesFromHosts(ci.Spec.Hosts)))
		if !hostsEqual(t.To, target) {
			return r.resetTransition(ctx, ci, target)
		}
	}

	carry, err := r.dexCarry(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	objs, err := serving.Render(r.servingSpec(ci), carry)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rendering serving objects: %w", err)
	}

	signals, err := r.stepPhase(ctx, ci, objs, carry)
	if err != nil {
		return ctrl.Result{}, err
	}
	next := advance(t.Phase, signals)

	switch next {
	case "":
		return r.finishTransition(ctx, ci)
	case t.Phase:
		// Parked: the phase is waiting on evidence it does not yet have. Persist
		// the conditions gathered this pass so a blocker (a missing client secret,
		// an unacknowledged SSO rehost, an unreachable probe) is visible on the CR.
		// The generation predicate drops the resulting status-change event, so this
		// write does not re-trigger a reconcile. AwaitingApproval then idles until
		// the CLI edits the spec; active phases poll again shortly.
		if err := r.Status().Update(ctx, ci); err != nil {
			return ctrl.Result{}, err
		}
		if t.Phase == phaseAwaitingApproval {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: dualServePollInterval}, nil
	default:
		// The phase write lands before the next phase's effects run (they run
		// on the requeued pass), so a crash between the two resumes in the new
		// phase — this is what makes entering Reverting safe: the reverse
		// effects can never race a pass that still renders the new identity.
		t.Phase = next
		if next == phaseDegraded {
			setCondition(ci, condReady, metav1.ConditionFalse, "Degraded", "cutover verification failed; reverted to the previous identity, both host sets still serve")
		} else {
			setCondition(ci, condReady, metav1.ConditionFalse, "Transitioning", "host change in progress: "+next)
		}
		if err := r.Status().Update(ctx, ci); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
}

// servingSpec builds the render spec for the CR's current transition phase.
// Inside a transition the spec-derived values (cluster domain, cookie scope,
// admin email) are pinned to the transition's own endpoint snapshots rather
// than the live spec: post-flip phases render the target identity, and the
// revert phases render the outgoing identity, so neither a concurrent spec
// edit nor a revert can produce a mixed identity (old issuer with a new
// cookie domain, or vice versa).
func (r *ClusterIdentityReconciler) servingSpec(ci *kipperv1.ClusterIdentity) serving.Spec {
	t := ci.Status.Transition
	identity := specIdentity(ci)
	if t != nil {
		switch {
		case (t.Phase == phaseReverting || t.Phase == phaseDegraded) && t.FromIdentity != nil:
			identity = t.FromIdentity
		case identityIsNew(t.Phase) && t.ToIdentity != nil:
			identity = t.ToIdentity
		}
	}
	spec := serving.Spec{
		Domain:          identity.Domain,
		Overrides:       overridesFromHosts(identity.Hosts),
		KipperRunDomain: kipperRunDomainOf(ci),
		ClusterHost:     clusterHostOf(ci),
		GatewayDisabled: gatewayDisabled(ci),
	}
	if t != nil {
		spec.Transition = &serving.Transition{
			Phase: servingPhaseFor(t.Phase),
			From:  hostSetOf(t.From),
			To:    hostSetOf(t.To),
		}
	}
	return spec
}

// applySteady renders and applies the steady (no-transition) identity.
func (r *ClusterIdentityReconciler) applySteady(ctx context.Context, ci *kipperv1.ClusterIdentity) error {
	carry, err := r.dexCarry(ctx)
	if err != nil {
		return err
	}
	objs, err := serving.Render(serving.Spec{
		Domain:          ci.Spec.Domain,
		Overrides:       overridesFromHosts(ci.Spec.Hosts),
		KipperRunDomain: kipperRunDomainOf(ci),
		ClusterHost:     clusterHostOf(ci),
		GatewayDisabled: gatewayDisabled(ci),
	}, carry)
	if err != nil {
		return fmt.Errorf("rendering steady objects: %w", err)
	}
	// Check the issuer guard before touching any Ingress: applyDexConfig refuses
	// (with no write) when the rendered issuer differs from live, so a steady
	// re-apply or a first adoption whose spec disagrees with the live issuer
	// fails side-effect-free rather than repointing the Ingresses and then
	// stranding the issuer.
	if err := r.applyDexConfig(ctx, objs.DexConfigYAML, carry.ExistingDexConfig, false); err != nil {
		return err
	}
	if err := r.applyIngresses(ctx, objs.Ingresses); err != nil {
		return err
	}
	// Reconcile the env family in steady state too, not only while a transition
	// is in flight. It is the same patch the transition applies, so drift — an
	// upgraded Deployment missing the gateway pair, or a var a past transition
	// blanked — heals on any reconcile instead of waiting for the next host
	// change. mergeEnv no-ops when nothing differs, so a healthy cluster never
	// rolls its pods for this.
	return r.flipConsoleAPIEnv(ctx, objs.ConsoleAPIEnv)
}

// applyIngresses server-side applies each rendered Ingress under the shared
// field manager. Re-applying the same set is a no-op; dropping a host from the
// set prunes it, which is how contraction removes old hosts with no bespoke
// delete logic.
//
// The security-headers Middleware each Ingress references goes on first.
// Traefik drops a router whose middleware is missing, so applying an Ingress
// into a namespace that has no Middleware yet 404s the host — which is what a
// cluster installed before the Middleware existed would otherwise get the
// moment this reconciler first re-applies its Ingresses.
func (r *ClusterIdentityReconciler) applyIngresses(ctx context.Context, ingresses []networkingv1.Ingress) error {
	if err := r.applySecurityHeaders(ctx, ingresses); err != nil {
		return err
	}
	for i := range ingresses {
		ing := ingresses[i]
		if err := r.serverSideApply(ctx, &ing); err != nil {
			return fmt.Errorf("applying ingress %s/%s: %w", ing.Namespace, ing.Name, err)
		}
	}
	return nil
}

// applySecurityHeaders applies the security-headers Middleware once per
// namespace the given Ingresses live in.
func (r *ClusterIdentityReconciler) applySecurityHeaders(ctx context.Context, ingresses []networkingv1.Ingress) error {
	done := make(map[string]bool, len(ingresses))
	for i := range ingresses {
		ns := ingresses[i].Namespace
		if done[ns] {
			continue
		}
		done[ns] = true
		if err := r.serverSideApply(ctx, serving.SecurityHeadersMiddleware(ns)); err != nil {
			return fmt.Errorf("applying security-headers middleware in %s: %w", ns, err)
		}
	}
	return nil
}

// serverSideApply applies obj under the serving field manager, forcing
// ownership so the reconciler always wins over another manager. The installer
// applies these same objects client-side (`kubectl apply -f -`, so they carry
// kubectl's manager, not this one), and an operator may have edited one by
// hand; forced ownership is what makes adopting either of those fight-free
// instead of failing on a conflict.
func (r *ClusterIdentityReconciler) serverSideApply(ctx context.Context, obj client.Object) error {
	//nolint:staticcheck // SSA via client.Apply; the typed Apply-configuration API is not adopted in this codebase yet
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(servingFieldManager), client.ForceOwnership)
}

// applyDexConfig server-side applies the merged dex-config ConfigMap and, when
// its content changed, stamps the Dex pod template with the new content hash so
// Dex restarts to pick it up. An empty render (no live config to merge) is a
// no-op so a first reconcile before Dex is installed does not clear the config.
//
// Outside a cutover (allowIssuerFlip=false) it refuses to change the OIDC issuer.
// The issuer flip is the one session-invalidating change; routing it through the
// CuttingOver phase is what bounds and gates it. So a steady, adoption, or
// DualServe render whose issuer differs from the live config — the sign of a
// stale or hand-authored status/spec that would otherwise flip login unguarded —
// is refused here rather than applied. This makes the additive guarantee a
// property of the write path, not of trustworthy pre-seeded status.
func (r *ClusterIdentityReconciler) applyDexConfig(ctx context.Context, configYAML, liveConfig string, allowIssuerFlip bool) error {
	wrote, err := r.writeDexConfig(ctx, configYAML, liveConfig, allowIssuerFlip)
	if err != nil {
		return err
	}
	if !wrote {
		return nil
	}
	return r.restartDexIfChanged(ctx, configYAML)
}

// writeDexConfig server-side applies the merged dex-config ConfigMap and reports
// whether it wrote a (non-empty) config. It is the durable half of a Dex change:
// once it returns wrote=true the live config holds configYAML, so the cutover
// path starts its post-flip deadline clock here — before the separate Dex restart
// — so a restart that then fails is still bounded by the deadline. An empty render
// (no live config to merge) is a no-op so a first reconcile before Dex is
// installed does not clear the config.
//
// Outside a cutover (allowIssuerFlip=false) it refuses to change the OIDC issuer.
// The issuer flip is the one session-invalidating change; routing it through the
// CuttingOver phase is what bounds and gates it. So a steady, adoption, or
// DualServe render whose issuer differs from the live config — the sign of a
// stale or hand-authored status/spec that would otherwise flip login unguarded —
// is refused here rather than applied. This makes the additive guarantee a
// property of the write path, not of trustworthy pre-seeded status.
func (r *ClusterIdentityReconciler) writeDexConfig(ctx context.Context, configYAML, liveConfig string, allowIssuerFlip bool) (bool, error) {
	if configYAML == "" {
		return false, nil
	}
	if !allowIssuerFlip && liveConfig != "" {
		liveCfg, err := dexcfg.Load(liveConfig)
		if err != nil {
			return false, fmt.Errorf("reading live dex-config issuer: %w", err)
		}
		wantCfg, err := dexcfg.Load(configYAML)
		if err != nil {
			return false, fmt.Errorf("reading rendered dex-config issuer: %w", err)
		}
		if liveCfg.Issuer() != wantCfg.Issuer() {
			return false, fmt.Errorf("refusing to change the Dex issuer outside a cutover: live=%q want=%q; a host change must go through the transition sequence", liveCfg.Issuer(), wantCfg.Issuer())
		}
	}
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: dexConfigMapName, Namespace: dexNamespace},
		Data:       map[string]string{dexConfigKey: configYAML},
	}
	if err := r.serverSideApply(ctx, cm); err != nil {
		return false, fmt.Errorf("applying dex-config: %w", err)
	}
	return true, nil
}

// restartDexIfChanged stamps the Dex deployment's pod template with a hash of
// the config, triggering a rollout only when the content actually changed.
func (r *ClusterIdentityReconciler) restartDexIfChanged(ctx context.Context, configYAML string) error {
	sum := sha256.Sum256([]byte(configYAML))
	hash := hex.EncodeToString(sum[:])

	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: dexDeploymentName, Namespace: dexNamespace}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting dex deployment: %w", err)
	}
	if dep.Spec.Template.Annotations[dexConfigHashAnnotation] == hash {
		return nil
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[dexConfigHashAnnotation] = hash
	if err := r.Update(ctx, &dep); err != nil {
		return fmt.Errorf("restarting dex: %w", err)
	}
	log.FromContext(ctx).Info("restarted dex for config change", "hash", hash)
	return nil
}

// dexCarry reads the live dex-config so the render merges onto it, preserving
// SSO connectors, the client secret, and the admin bcrypt hash. A missing
// ConfigMap yields an empty carry (nothing to merge yet).
func (r *ClusterIdentityReconciler) dexCarry(ctx context.Context) (serving.Carry, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: dexConfigMapName, Namespace: dexNamespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return serving.Carry{}, nil
		}
		return serving.Carry{}, fmt.Errorf("reading dex-config: %w", err)
	}
	return serving.Carry{ExistingDexConfig: cm.Data[dexConfigKey]}, nil
}

// ingressesApplied reports whether the transition's Ingresses are all present.
// For *.kipper.run hosts a failed per-host certificate is not blocking (Caddy's
// wildcard terminates public TLS), so certificate state does not gate a
// kipper.run cluster here; custom-domain certificate gating lands with the
// cutover effects.
func (r *ClusterIdentityReconciler) ingressesApplied(ctx context.Context, objs serving.Objects) bool {
	for i := range objs.Ingresses {
		ing := objs.Ingresses[i]
		var got networkingv1.Ingress
		if err := r.Get(ctx, types.NamespacedName{Name: ing.Name, Namespace: ing.Namespace}, &got); err != nil {
			return false
		}
	}
	return true
}

// boolCondition maps a readiness boolean to a condition status.
func boolCondition(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// --- small helpers ---

// kipperRunDomainOf returns the gateway base domain to thread into the render.
func kipperRunDomainOf(ci *kipperv1.ClusterIdentity) string {
	if ci.Spec.Gateway != nil {
		return ci.Spec.Gateway.KipperRunDomain
	}
	return ""
}

// gatewayDisabled reports the CR's explicit opt-out. A gateway block with
// register:false means this cluster must not register, whatever it registered
// before, so the render clears the env rather than preserving it.
func gatewayDisabled(ci *kipperv1.ClusterIdentity) bool {
	g := ci.Spec.Gateway
	return g != nil && g.Register != nil && !*g.Register
}

// clusterHostOf returns the public host the gateway routes to. Empty means the
// CR does not carry it, and the render then leaves whatever the Deployment
// already serves as CLUSTER_HOST alone.
func clusterHostOf(ci *kipperv1.ClusterIdentity) string {
	if ci.Spec.Gateway != nil {
		return ci.Spec.Gateway.ClusterHost
	}
	return ""
}

// hostSetOf converts a CR ResolvedHosts into a render HostSet (issuer is
// re-derived by the render from the Dex host, so it is not carried here).
func hostSetOf(h *kipperv1.ResolvedHosts) serving.HostSet {
	if h == nil {
		return serving.HostSet{}
	}
	return serving.HostSet{Console: h.Console, ConsoleAPI: h.ConsoleAPI, Dex: h.Dex}
}

// hostsEqual compares two resolved host sets by their serving hosts (issuer is
// derived, so comparing hosts is sufficient).
func hostsEqual(a, b *kipperv1.ResolvedHosts) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Console == b.Console && a.ConsoleAPI == b.ConsoleAPI && a.Dex == b.Dex
}

// newNonce returns a 128-bit random hex string. Hex has no '|' or newline, so it
// never collides with the CutoverApprovalHash field separators.
func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating transition nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// setCondition upserts a status condition, stamping ObservedGeneration so a
// stale condition is distinguishable from a current one.
func setCondition(ci *kipperv1.ClusterIdentity, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ci.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ci.Generation,
	})
}
