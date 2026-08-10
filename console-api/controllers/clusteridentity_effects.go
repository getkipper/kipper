package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/hopcert"
	"github.com/getkipper/kipper/controller/pkg/authncfg"
	"github.com/getkipper/kipper/controller/pkg/dexcfg"
	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/rollout"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

// cutoverDeadline bounds the post-flip window. If a transition that has flipped
// the Dex issuer has not verified within it — a stuck rollout, a deleted or
// renamed Deployment, or a persistently failing write — the reconciler
// auto-reverts to the previous identity rather than leaving Dex on the new
// issuer with no path forward (G.1).
const cutoverDeadline = 10 * time.Minute

// dexInClusterBase reaches Dex through its in-cluster Service rather than the
// public host, whose certificate and DNS may not resolve from inside the cluster
// (particularly on *.kipper.run, where backend certs do not issue through the
// edge). The discovery document it returns still advertises the public issuer.
const dexInClusterBase = "http://dex.dex.svc.cluster.local:5556/dex"

// Objects the cutover effects touch beyond the Dex config and Ingresses.
const (
	kipperSystemNamespace    = "kipper-system"
	consoleAPIDeploymentName = "console-api"
	consoleAPIContainerName  = "console-api"

	// kipperUsers is the role store the console-api RoleStore reads. The cutover
	// authorizes the (possibly new) admin email here before Dex serves the new
	// issuer, so a base-domain move never authenticates the operator as a subject
	// that is not yet authorized (G.10).
	kipperUsersConfigMapName = "kipper-users"
	kipperUsersDataKey       = "users"
	roleAdmin                = "admin"

	// dexOIDCClientSecret must exist and be non-empty in both the dex namespace
	// (Dex reads it via secretEnv) and kipper-system (console-api reads it via the
	// DEX_CLIENT_SECRET secretRef) before the cutover flips either onto it.
	dexOIDCClientSecretName = "dex-oidc-client" //nolint:gosec // G101: a Secret object name, not a credential
	dexOIDCClientSecretKey  = "secret"
)

// Condition types the cutover half of the machine reports.
const (
	condOIDCAligned          = "OIDCAligned"
	condExternalCallbacks    = "ExternalCallbacks"
	condCutoverPreconditions = "CutoverPreconditions"
)

// openTransition writes a fresh DualServe transition (with a new nonce, which
// invalidates any pending approval) before any object changes, so the next
// reconcile resumes from a known phase. Both a first spec edit and a mid-flight
// target change funnel through here.
func (r *ClusterIdentityReconciler) openTransition(ctx context.Context, ci *kipperv1.ClusterIdentity, from, to *kipperv1.ResolvedHosts, reason, msg string) (ctrl.Result, error) {
	nonce, err := newNonce()
	if err != nil {
		return ctrl.Result{}, err
	}
	ci.Status.Transition = &kipperv1.TransitionStatus{
		Phase: phaseDualServe,
		From:  from,
		// status.steady still holds the pre-edit spec identity here: the spec
		// has already moved to the new domain, so this snapshot is the only
		// faithful record of what a rollback must restore. The target spec is
		// snapshotted alongside it, so post-flip renders and completion are
		// pinned to what was requested, not to whatever spec later says.
		FromIdentity: ci.Status.Steady.DeepCopy(),
		ToIdentity:   specIdentity(ci),
		To:           to,
		Nonce:        nonce,
	}
	ci.Status.ObservedGeneration = ci.Generation
	setCondition(ci, condReady, metav1.ConditionFalse, reason, msg)
	if err := r.Status().Update(ctx, ci); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// resetTransition restarts an in-flight, pre-flip transition toward a new target
// with a fresh nonce (a domain edit landed mid-transition).
func (r *ClusterIdentityReconciler) resetTransition(ctx context.Context, ci *kipperv1.ClusterIdentity, target *kipperv1.ResolvedHosts) (ctrl.Result, error) {
	return r.openTransition(ctx, ci, ci.Status.ActiveHosts, target, "Transitioning", "transition target changed; restarting at DualServe")
}

// stepPhase applies the current phase's effects idempotently and reports the
// signals the pure phase machine decides on. Every effect is level-driven, so a
// reconciler restart (including the one the console-api rollout triggers on the
// reconciler's own pods mid-cutover) re-runs the same step and converges rather
// than half-applying.
func (r *ClusterIdentityReconciler) stepPhase(ctx context.Context, ci *kipperv1.ClusterIdentity, objs serving.Objects, carry serving.Carry) (phaseSignals, error) {
	switch ci.Status.Transition.Phase {
	case phaseDualServe, phaseAwaitingApproval:
		return r.stepAdditive(ctx, ci, objs, carry)
	case phaseCuttingOver:
		return r.stepCuttingOver(ctx, ci, objs, carry)
	case phaseVerifying:
		return r.stepVerifying(ctx, ci)
	case phaseContracting:
		return r.stepContracting(ctx, ci, objs, carry)
	case phaseReverting:
		return r.stepReverting(ctx, ci, objs, carry)
	}
	return phaseSignals{}, nil
}

// stepAdditive applies the additive dual-serve render (union hosts, old issuer)
// and gathers the pre-flip gate signals. Nothing session-affecting changes here.
func (r *ClusterIdentityReconciler) stepAdditive(ctx context.Context, ci *kipperv1.ClusterIdentity, objs serving.Objects, carry serving.Carry) (phaseSignals, error) {
	if err := r.applyIngresses(ctx, objs.Ingresses); err != nil {
		return phaseSignals{}, err
	}
	if err := r.applyDexConfig(ctx, objs.DexConfigYAML, carry.ExistingDexConfig, false); err != nil {
		return phaseSignals{}, err
	}
	ready := r.ingressesApplied(ctx, objs)
	setCondition(ci, condIngressesReady, boolCondition(ready), "Applied", "transition Ingresses applied")
	return phaseSignals{
		servingReady:     ready,
		approvalMatches:  r.approvalMatches(ci),
		preconditionsMet: r.preconditionsMet(ctx, ci, carry),
	}, nil
}

// stepCuttingOver performs the one session-invalidating change in a fixed order:
// authorize the new admin email, flip Dex onto the new issuer and wait for it to
// serve, then flip console-api onto the new issuer. The old hosts keep serving
// throughout (union Ingresses); only the identity moves.
func (r *ClusterIdentityReconciler) stepCuttingOver(ctx context.Context, ci *kipperv1.ClusterIdentity, objs serving.Objects, carry serving.Carry) (phaseSignals, error) {
	// Past the post-flip deadline, stop retrying and revert. This converts a
	// stuck rollout, a missing Deployment, or a persistently failing write into a
	// bounded auto-revert instead of an indefinite park or error loop (G.1).
	if cutoverDeadlineExceeded(ci) {
		setCondition(ci, condReady, metav1.ConditionFalse, "CutoverDeadlineExceeded", "cutover did not complete within the deadline")
		return phaseSignals{verifyFailed: true}, nil
	}
	// AwaitingApproval gated on the client secret, but it could have been deleted
	// between the gate and here. Flipping into an empty secret breaks login, so
	// fail (auto-revert) rather than proceed.
	if !r.dexSecretPresent(ctx) {
		setCondition(ci, condCutoverPreconditions, metav1.ConditionFalse, "MissingClientSecret", "dex-oidc-client Secret went missing during cutover")
		return phaseSignals{verifyFailed: true}, nil
	}
	// 0. Refuse the flip unless the API server already trusts the new issuer.
	//    kip stages the union authentication config over SSH; without it, the
	//    moment Dex serves the new issuer every operator's newly minted token
	//    (new iss) is rejected. Park — set the condition, start nothing, leave
	//    CutoverStartedAt unset — so re-staging (kip cluster domain --sync)
	//    unparks it. This never trips the revert path.
	if ok, msg := r.authnConfigStaged(ctx, ci); !ok {
		setCondition(ci, condCutoverPreconditions, metav1.ConditionFalse, "AuthnConfigNotStaged", msg)
		return phaseSignals{}, nil
	}
	// 1. Authorize the new admin email before Dex serves the new issuer (G.10). A
	//    dash migration keeps the same email, so this is a no-op there.
	if err := r.ensureKipperUser(ctx, objs.AdminEmail); err != nil {
		return phaseSignals{}, err
	}
	// 2. Flip the Dex issuer, rehost callbacks/connectors, restart Dex. The
	//    ConfigMap write is the durable flip; the moment it lands the post-flip
	//    deadline clock starts and is persisted, before the separate Dex restart.
	//    A pre-flip blocker (a missing secret, a failing ingress or ConfigMap
	//    write) leaves the clock unstarted, so it parks safely on the old identity
	//    rather than tripping the deadline; a restart or later step that fails
	//    after the write is bounded by the now-persisted deadline.
	if err := r.applyIngresses(ctx, objs.Ingresses); err != nil {
		return phaseSignals{}, err
	}
	wrote, err := r.writeDexConfig(ctx, objs.DexConfigYAML, carry.ExistingDexConfig, true)
	if err != nil {
		return phaseSignals{}, err
	}
	if wrote {
		if err := r.startCutoverClock(ctx, ci); err != nil {
			return phaseSignals{}, err
		}
		if err := r.restartDexIfChanged(ctx, objs.DexConfigYAML); err != nil {
			return phaseSignals{}, err
		}
	}
	dexDep, err := r.getDeployment(ctx, dexDeploymentName, dexNamespace)
	if err != nil {
		return phaseSignals{}, err
	}
	if rolloutFailed(dexDep) {
		return phaseSignals{verifyFailed: true}, nil
	}
	if !dexReadyOnConfig(dexDep, objs.DexConfigYAML) {
		// Dex has not finished rolling onto the new issuer. console-api must never
		// validate against an issuer Dex is not yet serving, so hold here.
		setCondition(ci, condOIDCAligned, metav1.ConditionFalse, "AwaitingDex", "waiting for Dex to serve the new issuer")
		return phaseSignals{}, nil
	}
	// 3. Flip console-api's OIDC env. maxUnavailable:0 keeps the old pod serving
	//    until the new one is Ready, so the reconciler (which runs inside
	//    console-api) does not cut its own session out from under the rollout.
	if err := r.flipConsoleAPIEnv(ctx, objs.ConsoleAPIEnv); err != nil {
		return phaseSignals{}, err
	}
	caDep, err := r.getDeployment(ctx, consoleAPIDeploymentName, kipperSystemNamespace)
	if err != nil {
		return phaseSignals{}, err
	}
	if rolloutFailed(caDep) {
		return phaseSignals{verifyFailed: true}, nil
	}
	return phaseSignals{cutoverRolloutsReady: rolloutReady(dexDep) && rolloutReady(caDep)}, nil
}

// stepVerifying proves the new issuer is actually live before contraction prunes
// the old hosts. It requires: both rollouts Ready, the Dex config and console-api
// env both on the new issuer, and a live in-cluster probe of Dex's discovery
// document and JWKS on the new issuer. A definitively failed rollout
// (ProgressDeadlineExceeded) or the post-flip deadline trips auto-revert; an
// unreachable probe parks and retries until it succeeds or the deadline fires.
// The console-api side is proven by its rollout going Ready on the new env: a pod
// that could not initialise its OIDC verifier against the new issuer never
// becomes Ready. External, client-facing verification is the CLI's pre-approval
// probe, separate from this in-cluster gate.
func (r *ClusterIdentityReconciler) stepVerifying(ctx context.Context, ci *kipperv1.ClusterIdentity) (phaseSignals, error) {
	if cutoverDeadlineExceeded(ci) {
		setCondition(ci, condReady, metav1.ConditionFalse, "CutoverDeadlineExceeded", "cutover did not verify within the deadline")
		return phaseSignals{verifyFailed: true}, nil
	}
	dexDep, err := r.getDeployment(ctx, dexDeploymentName, dexNamespace)
	if err != nil {
		return phaseSignals{}, err
	}
	caDep, err := r.getDeployment(ctx, consoleAPIDeploymentName, kipperSystemNamespace)
	if err != nil {
		return phaseSignals{}, err
	}
	if rolloutFailed(dexDep) || rolloutFailed(caDep) {
		return phaseSignals{verifyFailed: true}, nil
	}
	to := ci.Status.Transition.To
	if !r.oidcAligned(ctx, ci) || !rolloutReady(dexDep) || !rolloutReady(caDep) || to == nil {
		setCondition(ci, condOIDCAligned, metav1.ConditionFalse, "Verifying", "waiting for Dex and console-api to serve the new issuer")
		return phaseSignals{}, nil
	}
	live, err := r.prober().IssuerServed(ctx, to.Issuer)
	if err != nil {
		// Dex is not reachable yet on the new issuer. Not a definitive failure:
		// park and retry, and let the deadline auto-revert if it never comes up. A
		// probe error is a transient signal, not a reconcile error.
		setCondition(ci, condOIDCAligned, metav1.ConditionFalse, "ProbeUnreachable", "in-cluster OIDC probe could not reach Dex: "+err.Error())
		return phaseSignals{}, nil //nolint:nilerr // an unreachable probe parks and retries; it is not a reconcile failure
	}
	setCondition(ci, condOIDCAligned, boolCondition(live), "Verifying", "confirming Dex serves the new issuer live")
	return phaseSignals{verifyOK: live}, nil
}

// startCutoverClock stamps the post-flip deadline start and persists it, once.
// It is called right after the Dex issuer flip is written, so the deadline only
// ever bounds the window in which Dex is (or should be) serving the new issuer.
func (r *ClusterIdentityReconciler) startCutoverClock(ctx context.Context, ci *kipperv1.ClusterIdentity) error {
	if ci.Status.Transition.CutoverStartedAt != nil {
		return nil
	}
	now := metav1.Now()
	ci.Status.Transition.CutoverStartedAt = &now
	return r.Status().Update(ctx, ci)
}

// cutoverDeadlineExceeded reports whether a flipped transition has run past the
// post-flip deadline without verifying. It returns false until the issuer flip
// has been written (CutoverStartedAt is set), so a pre-flip blocker parks
// indefinitely on the safe old identity rather than tripping the deadline.
func cutoverDeadlineExceeded(ci *kipperv1.ClusterIdentity) bool {
	t := ci.Status.Transition
	if t == nil || t.CutoverStartedAt == nil {
		return false
	}
	return time.Now().After(t.CutoverStartedAt.Add(cutoverDeadline))
}

// oidcProber performs a live, in-cluster check that Dex serves a given issuer.
type oidcProber interface {
	// IssuerServed reports whether the in-cluster Dex endpoint advertises
	// wantIssuer in its discovery document and serves a non-empty JWKS.
	IssuerServed(ctx context.Context, wantIssuer string) (bool, error)
}

// httpOIDCProber probes Dex over the in-cluster Service.
type httpOIDCProber struct{ client *http.Client }

func (p httpOIDCProber) IssuerServed(ctx context.Context, wantIssuer string) (bool, error) {
	disco, err := p.getJSON(ctx, dexInClusterBase+"/.well-known/openid-configuration")
	if err != nil {
		return false, err
	}
	if iss, _ := disco["issuer"].(string); iss != wantIssuer {
		return false, nil
	}
	// Fetch keys through the in-cluster path (the advertised jwks_uri is the
	// public host). A non-empty key set proves Dex can sign and publish tokens.
	keys, err := p.getJSON(ctx, dexInClusterBase+"/keys")
	if err != nil {
		return false, err
	}
	set, _ := keys["keys"].([]interface{})
	return len(set) > 0, nil
}

func (p httpOIDCProber) getJSON(ctx context.Context, url string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body close on a read-only GET
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", url, err)
	}
	return out, nil
}

// stepContracting prunes the old hosts once any keepOldHostsUntil grace period
// has elapsed. During the grace period it keeps both host sets serving on the new
// issuer (the Verifying render) so external clients still resolve the old hosts.
func (r *ClusterIdentityReconciler) stepContracting(ctx context.Context, ci *kipperv1.ClusterIdentity, objs serving.Objects, carry serving.Carry) (phaseSignals, error) {
	if !contractGraceElapsed(ci) {
		holdSpec := r.servingSpec(ci)
		if holdSpec.Transition != nil {
			holdSpec.Transition.Phase = serving.PhaseVerifying
		}
		held, err := serving.Render(holdSpec, carry)
		if err != nil {
			return phaseSignals{}, fmt.Errorf("rendering grace-period hold: %w", err)
		}
		if err := r.applyIngresses(ctx, held.Ingresses); err != nil {
			return phaseSignals{}, err
		}
		if err := r.applyDexConfig(ctx, held.DexConfigYAML, carry.ExistingDexConfig, true); err != nil {
			return phaseSignals{}, err
		}
		return phaseSignals{contractReady: false}, nil
	}
	// Grace elapsed: prune to the new identity only.
	if err := r.applyIngresses(ctx, objs.Ingresses); err != nil {
		return phaseSignals{}, err
	}
	if err := r.applyDexConfig(ctx, objs.DexConfigYAML, carry.ExistingDexConfig, true); err != nil {
		return phaseSignals{}, err
	}
	if err := r.pruneOldAdminEmail(ctx, ci, objs.AdminEmail); err != nil {
		return phaseSignals{}, err
	}
	return phaseSignals{contractReady: true}, nil
}

// stepReverting applies the safe-revert render (old identity, both host sets
// still serving) and reports whether Dex and console-api have actually rolled
// back onto it. The Reverting phase was persisted before any of these effects
// ran, so a crash mid-revert resumes here instead of re-rendering the new
// identity. A reverse rollout that hangs parks and retries — there is no safer
// state to fall back to, and the conditions keep the blocker visible.
// kipper-users is left untouched: the old admin email was never removed, and
// the new one added at cutover can no longer authenticate once the issuer
// reverts, so both entries are harmless and nobody is locked out.
func (r *ClusterIdentityReconciler) stepReverting(ctx context.Context, ci *kipperv1.ClusterIdentity, objs serving.Objects, carry serving.Carry) (phaseSignals, error) {
	if err := r.applyIngresses(ctx, objs.Ingresses); err != nil {
		return phaseSignals{}, err
	}
	if err := r.applyDexConfig(ctx, objs.DexConfigYAML, carry.ExistingDexConfig, true); err != nil {
		return phaseSignals{}, err
	}
	dexDep, err := r.getDeployment(ctx, dexDeploymentName, dexNamespace)
	if err != nil {
		return phaseSignals{}, err
	}
	if !dexReadyOnConfig(dexDep, objs.DexConfigYAML) {
		// Mirror the forward order: console-api must never validate against an
		// issuer Dex is not serving, so its env flips back only once Dex is
		// Ready on the previous config.
		setCondition(ci, condOIDCAligned, metav1.ConditionFalse, "Reverting", "waiting for Dex to serve the previous issuer again")
		return phaseSignals{}, nil
	}
	if err := r.flipConsoleAPIEnv(ctx, objs.ConsoleAPIEnv); err != nil {
		return phaseSignals{}, err
	}
	caDep, err := r.getDeployment(ctx, consoleAPIDeploymentName, kipperSystemNamespace)
	if err != nil {
		return phaseSignals{}, err
	}
	if !rolloutReady(caDep) {
		setCondition(ci, condOIDCAligned, metav1.ConditionFalse, "Reverting", "waiting for console-api to roll back onto the previous issuer")
		return phaseSignals{}, nil
	}
	setCondition(ci, condOIDCAligned, metav1.ConditionFalse, "Reverted", "issuer restored to the previous identity")
	return phaseSignals{revertComplete: true}, nil
}

// handleDegraded parks until an operator supersedes the failed transition by
// pointing spec at a different identity — a fresh target, or a --rollback that
// patches spec back to status.lastSteady. The comparison is against the spec's
// resolved target, not the generation counter: the approval and SSO
// acknowledgement the failed attempt consumed advance the generation without
// changing the requested identity, and re-evaluating on those would reopen a
// transition toward the very target that just failed.
func (r *ClusterIdentityReconciler) handleDegraded(ctx context.Context, ci *kipperv1.ClusterIdentity) (ctrl.Result, error) {
	target := resolvedFromHostSet(serving.ResolveHosts(ci.Spec.Domain, overridesFromHosts(ci.Spec.Hosts)))
	if !hostsEqual(target, ci.Status.Transition.To) {
		ci.Status.Transition = nil
		ci.Status.ObservedGeneration = ci.Generation
		setCondition(ci, condReady, metav1.ConditionFalse, "Reevaluating", "spec changed after a degraded cutover; re-evaluating")
		if err := r.Status().Update(ctx, ci); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

// finishTransition records the new identity as steady and the previous one as the
// rollback target, then clears the transition. lastSteady is the transition's
// FromIdentity — the exact spec (domain plus overrides) that was serving before
// the change — so a later --rollback restores that spec verbatim.
func (r *ClusterIdentityReconciler) finishTransition(ctx context.Context, ci *kipperv1.ClusterIdentity) (ctrl.Result, error) {
	t := ci.Status.Transition
	ci.Status.ActiveHosts = t.To
	ci.Status.LastSteady = t.FromIdentity
	// The steady record is the identity this transition made active, which is
	// its target snapshot. The live spec may already say something newer (an
	// edit that landed after the flip); that edit gets its own transition on
	// the next reconcile, with this snapshot as its faithful FromIdentity.
	ci.Status.Steady = t.ToIdentity
	ci.Status.Transition = nil
	ci.Status.ObservedGeneration = ci.Generation
	setCondition(ci, condReady, metav1.ConditionTrue, "Steady", "serving the new identity")
	return ctrl.Result{}, r.Status().Update(ctx, ci)
}

// specIdentity snapshots the spec's identity (domain plus host overrides) as a
// steady identity.
func specIdentity(ci *kipperv1.ClusterIdentity) *kipperv1.SteadyIdentity {
	return &kipperv1.SteadyIdentity{Domain: ci.Spec.Domain, Hosts: ci.Spec.Hosts.DeepCopy()}
}

// approvalMatches reports whether spec.cutoverApproval matches the hash of the
// pending transition. A stale or replayed approval never matches: the hash binds
// observedGeneration, both endpoints, and the per-transition nonce.
func (r *ClusterIdentityReconciler) approvalMatches(ci *kipperv1.ClusterIdentity) bool {
	if ci.Spec.CutoverApproval == "" {
		return false
	}
	t := ci.Status.Transition
	if t == nil || t.From == nil || t.To == nil {
		return false
	}
	want := kipperv1.CutoverApprovalHash(ci.Status.ObservedGeneration, *t.From, *t.To, t.Nonce)
	return ci.Spec.CutoverApproval == want
}

// preconditionsMet gates the issuer flip. The dex-oidc-client Secret must be
// present and non-empty in both namespaces, and when SSO connectors will be
// rehosted the operator must have acknowledged updating each provider's callback.
func (r *ClusterIdentityReconciler) preconditionsMet(ctx context.Context, ci *kipperv1.ClusterIdentity, carry serving.Carry) bool {
	if !r.dexSecretPresent(ctx) {
		setCondition(ci, condCutoverPreconditions, metav1.ConditionFalse, "MissingClientSecret", "dex-oidc-client Secret is absent or empty in the dex or kipper-system namespace")
		return false
	}
	if connectorsPresent(carry) && !ssoAckMatches(ci) {
		setCondition(ci, condExternalCallbacks, metav1.ConditionFalse, "NeedsAck", "SSO connectors will be rehosted to the new issuer; set ackSSOCallbacksFor to the new Dex host once each provider's callback URL is updated")
		return false
	}
	if ok, msg := r.authnConfigStaged(ctx, ci); !ok {
		setCondition(ci, condCutoverPreconditions, metav1.ConditionFalse, "AuthnConfigNotStaged", msg)
		return false
	}
	setCondition(ci, condExternalCallbacks, metav1.ConditionTrue, "Acknowledged", "no SSO connectors to rehost, or callbacks acknowledged")
	setCondition(ci, condCutoverPreconditions, metav1.ConditionTrue, "Ready", "cutover preconditions met")
	return true
}

// authnConfigStaged reports whether the API server already trusts the new
// issuer — the union of the old and new Dex hosts must be the active
// authentication configuration before the in-cluster Dex flip, or newly
// issued tokens (new iss) would be rejected and every operator locked out.
// kip stages that config over SSH (kip cluster domain / --sync); this gate
// refuses the flip until the apiserver publishes the staged hash. It fails
// hopCA reads the cluster's certificate authority, the anchor the API server was
// given for a gateway-fronted issuer. Absent means no anchor was ever written,
// which is the case for a cluster that only ever served a custom domain, and
// renders as an empty anchor exactly as kip would render it.
func (r *ClusterIdentityReconciler) hopCA(ctx context.Context) (string, error) {
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: hopcert.CASecretName, Namespace: hopcert.Namespace}, &secret)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// The same bundle kip writes: the active signer plus any authority retained
	// across a rollover. Rendering only the signer here would hash differently
	// from the file on disk and park every cutover mid-transaction.
	return string(hopca.Bundle(secret.Data[corev1.TLSCertKey], secret.Data[hopcert.RetainedCAKey])), nil
}

// closed: an unreadable metric or a mismatch parks the cutover. When no
// MetricsReader is wired (tests) the gate is inert.
func (r *ClusterIdentityReconciler) authnConfigStaged(ctx context.Context, ci *kipperv1.ClusterIdentity) (bool, string) {
	if r.Metrics == nil {
		return true, ""
	}
	t := ci.Status.Transition
	if t == nil || t.From == nil || t.To == nil {
		return true, ""
	}
	// The anchor is part of the rendered bytes, so this must read the same CA
	// kip wrote the file with or the hashes can never match. An unreadable CA
	// parks the cutover rather than comparing against a config nobody has.
	caPEM, err := r.hopCA(ctx)
	if err != nil {
		return false, fmt.Sprintf("could not read the cluster certificate authority (%v); the API server's authentication config cannot be verified", err)
	}
	want := authncfg.Hash(authncfg.Render(caPEM, authncfg.HostsFor(t.From.Dex, t.To.Dex)...))
	metrics, merr := r.Metrics.ReadMetrics(ctx)
	if merr != nil {
		return false, fmt.Sprintf("could not read the API server authentication config state (%v); run 'kip cluster domain --sync' from a machine with SSH access to the server to stage the new issuer", merr)
	}
	if !authncfg.ActiveHashMatches(metrics, want) {
		return false, "the API server does not yet trust the new login issuer; run 'kip cluster domain --sync' from a machine with SSH access to the server to stage /etc/rancher/k3s/authn-config.yaml before the cutover"
	}
	return true, ""
}

// ssoAckMatches reports whether the operator's SSO callback acknowledgement
// names the pending transition's target Dex host. Binding the acknowledgement
// to the host keeps it single-use: a move to a different host can never ride
// on an acknowledgement given for an earlier one.
func ssoAckMatches(ci *kipperv1.ClusterIdentity) bool {
	t := ci.Status.Transition
	return t != nil && t.To != nil && t.To.Dex != "" && ci.Spec.AckSSOCallbacksFor == t.To.Dex
}

// dexSecretPresent reports whether the Dex OIDC client secret is populated in
// both the dex and kipper-system namespaces.
func (r *ClusterIdentityReconciler) dexSecretPresent(ctx context.Context) bool {
	for _, ns := range []string{dexNamespace, kipperSystemNamespace} {
		var s corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: dexOIDCClientSecretName, Namespace: ns}, &s); err != nil {
			return false
		}
		if len(s.Data[dexOIDCClientSecretKey]) == 0 {
			return false
		}
	}
	return true
}

// connectorsPresent reports whether the live Dex config declares SSO connectors
// that a rehost would touch. A config that cannot be parsed is treated as having
// connectors, so acknowledgment is required rather than silently skipped.
func connectorsPresent(carry serving.Carry) bool {
	if carry.ExistingDexConfig == "" {
		return false
	}
	cfg, err := dexcfg.Load(carry.ExistingDexConfig)
	if err != nil {
		return true
	}
	return len(cfg.Connectors()) > 0
}

// ensureKipperUser authorizes email as an admin in the kipper-users role store,
// creating the store if it does not exist yet. It preserves every other entry
// and only writes when the email is not already an admin.
func (r *ClusterIdentityReconciler) ensureKipperUser(ctx context.Context, email string) error {
	if email == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm corev1.ConfigMap
		err := r.Get(ctx, types.NamespacedName{Name: kipperUsersConfigMapName, Namespace: kipperSystemNamespace}, &cm)
		if apierrors.IsNotFound(err) {
			data, merr := marshalUsers(map[string]string{email: roleAdmin})
			if merr != nil {
				return merr
			}
			return r.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: kipperUsersConfigMapName, Namespace: kipperSystemNamespace},
				Data:       map[string]string{kipperUsersDataKey: data},
			})
		}
		if err != nil {
			return err
		}
		users, err := unmarshalUsers(cm.Data[kipperUsersDataKey])
		if err != nil {
			return err
		}
		if users[email] == roleAdmin {
			return nil
		}
		users[email] = roleAdmin
		data, err := marshalUsers(users)
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[kipperUsersDataKey] = data
		return r.Update(ctx, &cm)
	})
}

// removeKipperUser drops email from the role store if present. A missing store is
// not an error.
func (r *ClusterIdentityReconciler) removeKipperUser(ctx context.Context, email string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: kipperUsersConfigMapName, Namespace: kipperSystemNamespace}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		users, err := unmarshalUsers(cm.Data[kipperUsersDataKey])
		if err != nil {
			return err
		}
		if _, ok := users[email]; !ok {
			return nil
		}
		delete(users, email)
		data, err := marshalUsers(users)
		if err != nil {
			return err
		}
		cm.Data[kipperUsersDataKey] = data
		return r.Update(ctx, &cm)
	})
}

// pruneOldAdminEmail removes the previous admin email from the role store at
// contraction, but only on a base-domain move where the email actually changed. A
// dash migration keeps the same email, so nothing is pruned. The old email comes
// from the transition's FromIdentity domain — the same derivation the render used
// when that identity was serving — because recovering a base domain from a Dex
// hostname is ambiguous under host overrides.
func (r *ClusterIdentityReconciler) pruneOldAdminEmail(ctx context.Context, ci *kipperv1.ClusterIdentity, newEmail string) error {
	t := ci.Status.Transition
	if t == nil || t.FromIdentity == nil || t.FromIdentity.Domain == "" || newEmail == "" {
		return nil
	}
	oldEmail := "admin@" + t.FromIdentity.Domain
	if oldEmail == newEmail {
		return nil
	}
	return r.removeKipperUser(ctx, oldEmail)
}

// flipConsoleAPIEnv patches the host-derived OIDC env family onto the console-api
// Deployment, leaving unrelated env untouched, and pins a surge-only rollout so
// the old pod keeps serving until the new one is Ready (G.4).
func (r *ClusterIdentityReconciler) flipConsoleAPIEnv(ctx context.Context, envs []corev1.EnvVar) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var dep appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: consoleAPIDeploymentName, Namespace: kipperSystemNamespace}, &dep); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		changed := ensureSurgeOnlyStrategy(&dep)
		idx := containerIndex(dep.Spec.Template.Spec.Containers, consoleAPIContainerName)
		if idx < 0 {
			return fmt.Errorf("console-api container not found in its deployment")
		}
		if mergeEnv(&dep.Spec.Template.Spec.Containers[idx], envs) {
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Update(ctx, &dep)
	})
}

// oidcAligned reports whether both Dex (config issuer) and console-api
// (DEX_ISSUER env) present the transition's target issuer.
func (r *ClusterIdentityReconciler) oidcAligned(ctx context.Context, ci *kipperv1.ClusterIdentity) bool {
	to := ci.Status.Transition.To
	if to == nil {
		return false
	}
	carry, err := r.dexCarry(ctx)
	if err != nil || carry.ExistingDexConfig == "" {
		return false
	}
	cfg, err := dexcfg.Load(carry.ExistingDexConfig)
	if err != nil || cfg.Issuer() != to.Issuer {
		return false
	}
	caDep, err := r.getDeployment(ctx, consoleAPIDeploymentName, kipperSystemNamespace)
	if err != nil || caDep == nil {
		return false
	}
	idx := containerIndex(caDep.Spec.Template.Spec.Containers, consoleAPIContainerName)
	if idx < 0 {
		return false
	}
	return envValue(caDep.Spec.Template.Spec.Containers[idx].Env, "DEX_ISSUER") == to.Issuer
}

// getDeployment fetches a Deployment, returning (nil, nil) when it does not exist.
func (r *ClusterIdentityReconciler) getDeployment(ctx context.Context, name, namespace string) (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting deployment %s/%s: %w", namespace, name, err)
	}
	return &dep, nil
}

// dexReadyOnConfig reports whether Dex has rolled out and is serving the given
// config: the pod template carries this content's hash and the rollout is Ready.
func dexReadyOnConfig(dep *appsv1.Deployment, configYAML string) bool {
	if dep == nil {
		return false
	}
	sum := sha256.Sum256([]byte(configYAML))
	if dep.Spec.Template.Annotations[dexConfigHashAnnotation] != hex.EncodeToString(sum[:]) {
		return false
	}
	return rolloutReady(dep)
}

// rolloutReady and rolloutFailed answer the cutover gate's readiness questions.
// The definitions live in controller/pkg/rollout so kip's upgrade gate cannot
// drift from what a cutover considers rolled out.
func rolloutReady(dep *appsv1.Deployment) bool  { return rollout.Ready(dep) }
func rolloutFailed(dep *appsv1.Deployment) bool { return rollout.Failed(dep) }

// contractGraceElapsed reports whether the optional keepOldHostsUntil grace
// period has passed (or was never set).
func contractGraceElapsed(ci *kipperv1.ClusterIdentity) bool {
	until := ci.Spec.KeepOldHostsUntil
	if until == nil {
		return true
	}
	return !time.Now().Before(until.Time)
}

// --- env / container helpers ---

// mergeEnv overwrites each named env var on the container, appending any that are
// absent and leaving unrelated vars in place. It reports whether anything changed.
func mergeEnv(c *corev1.Container, envs []corev1.EnvVar) bool {
	changed := false
	for _, e := range envs {
		idx := envIndex(c.Env, e.Name)
		if idx < 0 {
			c.Env = append(c.Env, e)
			changed = true
			continue
		}
		if !reflect.DeepEqual(c.Env[idx], e) {
			c.Env[idx] = e
			changed = true
		}
	}
	return changed
}

// ensureSurgeOnlyStrategy sets a maxUnavailable:0, maxSurge:1 rolling update so a
// rollout never drops below full capacity. It reports whether it changed the spec.
func ensureSurgeOnlyStrategy(dep *appsv1.Deployment) bool {
	zero := intstr.FromInt(0)
	one := intstr.FromInt(1)
	cur := dep.Spec.Strategy
	if cur.Type == appsv1.RollingUpdateDeploymentStrategyType && cur.RollingUpdate != nil &&
		cur.RollingUpdate.MaxUnavailable != nil && cur.RollingUpdate.MaxUnavailable.IntValue() == 0 &&
		cur.RollingUpdate.MaxSurge != nil && cur.RollingUpdate.MaxSurge.IntValue() == 1 {
		return false
	}
	dep.Spec.Strategy = appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one},
	}
	return true
}

func containerIndex(containers []corev1.Container, name string) int {
	for i := range containers {
		if containers[i].Name == name {
			return i
		}
	}
	return -1
}

func envIndex(env []corev1.EnvVar, name string) int {
	for i := range env {
		if env[i].Name == name {
			return i
		}
	}
	return -1
}

func envValue(env []corev1.EnvVar, name string) string {
	if i := envIndex(env, name); i >= 0 {
		return env[i].Value
	}
	return ""
}

// --- kipper-users JSON codec ---

func unmarshalUsers(raw string) (map[string]string, error) {
	m := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parsing kipper-users: %w", err)
	}
	return m, nil
}

func marshalUsers(m map[string]string) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encoding kipper-users: %w", err)
	}
	return string(b), nil
}
