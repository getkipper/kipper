package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/nsowner"
	"github.com/getkipper/kipper/controller/pkg/applink"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// linkPolicyName is the per-app NetworkPolicy carrying that app's cross-project
// egress. One policy per app rather than one shared per namespace, so it is
// owned by the app and removed with it.
func linkPolicyName(app string) string { return "kipper-link-" + app }

// linkRefreshInterval bounds how long a caller's egress can lag a change to a
// link target, or to the consent authorising it, when the watch that should
// have noticed dropped the event. A map function cannot report a failure or ask
// to be retried, so without this a transient cache error at the wrong moment
// would leave a revoked consent's policy standing indefinitely.
const linkRefreshInterval = 30 * time.Minute

// reconcileLinkPolicy opens egress from this app's pods to the apps it declares
// a link to in other namespaces.
//
// It exists because kipper-workload-egress denies everything inside the cluster
// beyond the app's own namespace: cross-project traffic is excepted by the
// RFC1918 ranges whether it is addressed by service or by pod, and the public
// route is excepted by the node addresses. Without an allowance there is no path
// between two projects at all.
//
// Only the caller's namespace needs one. That policy is egress-only, and Kipper
// writes no ingress policy over ordinary app pods, so the target namespace is
// untouched by a link. NetworkPolicy rules are additive, so this widens exactly
// what it names and nothing else.
//
// A link inside this app's own namespace produces no peer, because the workload
// policy already allows the whole namespace. It stays in the spec regardless:
// the list is what this app depends on, not a by-product of the policy.
func (r *AppReconciler) reconcileLinkPolicy(ctx context.Context, app *kipperv1.App) ([]ResolvedLink, error) {
	live, blocked, err := ResolveLinks(ctx, r.Client, app)
	if err != nil {
		return nil, err
	}
	if err := r.writeLinkPolicy(ctx, app, r.linkEgressRules(live)); err != nil {
		return nil, err
	}
	// Only now. The condition says which links carry traffic, and until the
	// allowance is accepted that is a claim about what was intended rather than
	// what is in force — an app whose policy the API server rejects would
	// otherwise report every link open on every retry, for as long as the
	// rejection lasted.
	r.setLinksOpenCondition(ctx, app, blocked)
	return live, nil
}

// writeLinkPolicy brings the app's egress policy to match the rules resolved
// for it, removing it entirely when nothing is allowed.
func (r *AppReconciler) writeLinkPolicy(ctx context.Context, app *kipperv1.App, peers []networkingv1.NetworkPolicyEgressRule) error {
	name := linkPolicyName(app.Name)
	if len(peers) == 0 {
		// Nothing to allow. Remove any policy a previous spec left behind rather
		// than leaving an allowance standing after the link that justified it is
		// gone.
		var existing networkingv1.NetworkPolicy
		switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &existing); {
		case errors.IsNotFound(err):
			return nil
		case err != nil:
			return err
		}
		return r.Delete(ctx, &existing)
	}

	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: app.Namespace,
			Labels: map[string]string{
				"app":       app.Name,
				kipperLabel: kipperValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": app.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      peers,
		},
	}
	if err := controllerutil.SetControllerReference(app, desired, r.Scheme); err != nil {
		return err
	}

	var existing networkingv1.NetworkPolicy
	switch err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: app.Namespace}, &existing); {
	case errors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	return r.Update(ctx, &existing)
}

// ResolvedLink is a declared link whose target was found and is usable, paired
// with the target it resolved to.
type ResolvedLink struct {
	Link   kipperv1.AppLink
	target kipperv1.App
	// sameNamespace links need no consent and get no egress rule, the workload
	// policy already allowing the namespace. They are still dependencies and
	// still get an address.
	sameNamespace bool
}

// resolveLinks answers, once, which of this app's declared links are live and
// why the rest are not.
//
// One resolver because two things are rendered from the answer — the egress the
// policy opens and the address the pod is given — and they have to be rendered
// from the same one. Asked separately they drifted: the policy loop skipped
// same-namespace links before looking for the target, so a dependency inside
// the app's own namespace that did not exist counted as carrying traffic while
// no address was produced for it, and the app reported every link open while
// one of them was dead.
func ResolveLinks(ctx context.Context, c crclient.Client, app *kipperv1.App) ([]ResolvedLink, []string, error) {
	logger := log.FromContext(ctx)
	var live []ResolvedLink
	var blocked []string

	// Deduplicated by target. The spec is writable directly, so a list that
	// repeats a target — by accident or to inflate the policy — is resolved
	// once, not once per entry.
	seen := map[string]bool{}
	byEnvKey := map[string]string{}
	for _, link := range app.Spec.Links {
		if link.App == "" || link.Namespace == "" {
			continue
		}
		key := link.Namespace + "/" + link.App
		if seen[key] {
			continue
		}
		seen[key] = true

		// Two targets of the same name in different namespaces are distinct
		// links and would open distinct allowances, but they name one variable
		// between them — the address is keyed by the app's name alone. Only one
		// value can reach the pod, so the second is refused rather than left to
		// win silently and leave the first declared, allowed, and unreachable.
		//
		// Neither supported writer can produce this, both replacing a link by
		// app name, but the spec is directly writable and the resolver is what
		// the cluster acts on.
		envKey := AppEnvKey(link.App)
		if owner, taken := byEnvKey[envKey]; taken {
			logger.Info("two links would use one address variable; the later one opens nothing",
				"app", app.Name, "variable", envKey, "kept", owner, "refused", key)
			blocked = append(blocked, fmt.Sprintf("%s (%s already uses %s)", key, owner, envKey))
			continue
		}
		byEnvKey[envKey] = key

		sameNamespace := link.Namespace == app.Namespace

		// The target's project has to have agreed. A link is written by the
		// calling side, and an app's own project cannot grant access to
		// somebody else's — the egress policy is what makes that backend
		// unreachable, and a direct route to it goes past the ingress and past
		// every control attached to a public route. A project reaching its own
		// apps has nobody to ask.
		if !sameNamespace {
			allowed, reason, cerr := linkIsConsentedTo(ctx, c, app.Namespace, link.Namespace)
			if cerr != nil {
				return nil, nil, cerr
			}
			if !allowed {
				logger.Info("link is not consented to by the target project; no egress opened for it",
					"app", app.Name, "target", link.App, "targetNamespace", link.Namespace, "reason", reason)
				blocked = append(blocked, fmt.Sprintf("%s (%s)", key, reason))
				continue
			}
		}

		var target kipperv1.App
		switch err := c.Get(ctx, types.NamespacedName{Name: link.App, Namespace: link.Namespace}, &target); {
		case errors.IsNotFound(err):
			logger.Info("link names an app that does not exist; no egress opened for it",
				"app", app.Name, "target", link.App, "targetNamespace", link.Namespace)
			blocked = append(blocked, fmt.Sprintf("%s (no such app)", key))
			continue
		case err != nil:
			return nil, nil, fmt.Errorf("reading link target %s/%s: %w", link.Namespace, link.App, err)
		}
		if target.Spec.Port == 0 {
			logger.Info("link target has no port; no egress opened for it",
				"app", app.Name, "target", link.App, "targetNamespace", link.Namespace)
			blocked = append(blocked, fmt.Sprintf("%s (target serves no port)", key))
			continue
		}

		live = append(live, ResolvedLink{Link: link, target: target, sameNamespace: sameNamespace})
	}
	return live, blocked, nil
}

// linkEgressRules builds one egress rule per live cross-namespace link, scoped
// to the target app's own pods and the port those pods listen on.
//
// A link inside this app's own namespace produces no rule: the workload policy
// already allows the whole namespace, and writing one would be an allowance
// nobody needs and nobody would think to remove.
func (r *AppReconciler) linkEgressRules(live []ResolvedLink) []networkingv1.NetworkPolicyEgressRule {
	var rules []networkingv1.NetworkPolicyEgressRule
	for _, resolved := range live {
		if resolved.sameNamespace {
			continue
		}
		// The pod's port, not its Service's. A peer here is a pod selector,
		// which resolves to pod addresses, so the rule is matched against
		// traffic the node has already translated — naming the Service's port
		// would match nothing that arrives. When the target runs the
		// instance-id sidecar its Service sends 3000 to a pod listening on
		// 13000, so 3000 is the wrong number to allow.
		//
		// That ordering is what kipper-workload-egress already relies on: its
		// DNS rule is a pod selector over kube-dns carrying port 53, and every
		// workload reaches DNS through the Service address. A cluster whose CNI
		// evaluated before translation would deny that peer outright, and
		// nothing here would resolve a name.
		port := intstr.FromInt32(r.serviceTargetPort(&resolved.target))
		tcp := corev1.ProtocolTCP
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": resolved.Link.Namespace},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": resolved.Link.App},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		})
	}
	return rules
}

// linkTargetIndex is the field index over spec.links. Without it a target that
// changes port, or is deleted and recreated, leaves every caller's allowance
// pointing at what used to be there — and a workload that later takes that name
// and port inherits the access.
const linkTargetIndex = "spec.links.target"

// LinkTargetKeys is the index extractor, exported so a test indexes exactly what
// production does rather than a second copy that can drift from it.
func LinkTargetKeys(o crclient.Object) []string {
	app, ok := o.(*kipperv1.App)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(app.Spec.Links))
	for _, link := range app.Spec.Links {
		if link.App == "" || link.Namespace == "" {
			continue
		}
		keys = append(keys, link.Namespace+"/"+link.App)
	}
	return keys
}

// IndexAppLinks registers the index the caller watch reads.
func IndexAppLinks(ctx context.Context, indexer crclient.FieldIndexer) error {
	return indexer.IndexField(ctx, &kipperv1.App{}, linkTargetIndex, LinkTargetKeys)
}

// enqueueCallersOfLinkTarget re-reconciles every app that links to the one that
// changed, so an allowance follows its target's port and is withdrawn when the
// target goes. A delete event arrives here too, which is the case that matters:
// the policy would otherwise keep selecting a name and port that anything could
// later occupy.
func (r *AppReconciler) enqueueCallersOfLinkTarget(ctx context.Context, obj crclient.Object) []reconcile.Request {
	var callers kipperv1.AppList
	if err := r.List(ctx, &callers, crclient.MatchingFields{
		linkTargetIndex: obj.GetNamespace() + "/" + obj.GetName(),
	}); err != nil {
		// A map function cannot return an error, so the event is considered
		// handled and never retried. Say so, and rely on the periodic backstop
		// in Reconcile to catch what this drops.
		log.FromContext(ctx).Error(err, "could not map a link target to its callers; their policies will be rebuilt by the periodic sweep",
			"target", obj.GetNamespace()+"/"+obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(callers.Items))
	for i := range callers.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: callers.Items[i].Name, Namespace: callers.Items[i].Namespace,
		}})
	}
	return reqs
}

// linkIsConsentedTo reports whether the project owning targetNS has agreed to be
// linked to from the project owning callerNS, and why not when it has not.
//
// Consent lives on the target's Project because that is where the authority to
// grant it sits. Reading it from the namespaces rather than from the link means
// a caller cannot name a project it does not belong to and have that believed:
// both ends are resolved through the shared owner lookup rather than from a
// label anyone who can write a namespace can set.
func linkIsConsentedTo(ctx context.Context, c crclient.Client, callerNS, targetNS string) (bool, string, error) {
	callerProject, err := projectOfNamespace(ctx, c, callerNS)
	if err != nil {
		return false, "", err
	}
	targetProject, err := projectOfNamespace(ctx, c, targetNS)
	if err != nil {
		return false, "", err
	}
	if callerProject == "" || targetProject == "" {
		return false, "one of the namespaces is not a Kipper project namespace", nil
	}
	if callerProject == targetProject {
		// A different environment of the same project. The project already owns
		// both ends, so there is nobody else to ask.
		//
		// Both ends resolved to the same project through the shared owner
		// lookup. Two projects resolving to one namespace name is refused where
		// a namespace is adopted rather than here, so this branch does not have
		// to defend against it.
		return true, "", nil
	}

	var project kipperv1.Project
	switch err := c.Get(ctx, types.NamespacedName{Name: targetProject}, &project); {
	case errors.IsNotFound(err):
		return false, "the target project does not exist", nil
	case err != nil:
		return false, "", fmt.Errorf("reading project %s: %w", targetProject, err)
	}
	for _, allowed := range project.Spec.AllowLinksFrom {
		if allowed == callerProject {
			return true, "", nil
		}
	}
	return false, "project " + targetProject + " does not allow links from " + callerProject, nil
}

// projectOfNamespace returns the project a namespace belongs to. An empty result
// means it is not a project's.
func projectOfNamespace(ctx context.Context, c crclient.Client, ns string) (string, error) {
	// Through the shared owner lookup, because this decides whether one
	// project's app may reach another's. Reading the label here trusted a value
	// anyone who can write a namespace can set, and consent between two tenants
	// is exactly the decision that must not rest on one.
	project, ok, err := nsowner.Of(ctx, c, ns)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return project, nil
}

// enqueueCallersOfProject re-reconciles every app whose links point into a
// project whose consent just changed.
//
// Revoking allowLinksFrom is a decision to close a path, and without this it
// closes nothing: the caller's policy is only rebuilt when the caller itself is
// reconciled, and a stable deployment may not be for days. Granting has the
// mirror problem — a link recorded before consent would sit inert until
// something unrelated happened to the caller.
func (r *AppReconciler) enqueueCallersOfProject(ctx context.Context, obj crclient.Object) []reconcile.Request {
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces, crclient.MatchingLabels{kipperlabels.Project: obj.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "could not map a project to its linked callers; the periodic sweep will catch them",
			"project", obj.GetName())
		return nil
	}

	seen := map[types.NamespacedName]bool{}
	var reqs []reconcile.Request
	for i := range namespaces.Items {
		var callers kipperv1.AppList
		// Every app linking to anything in this namespace, whichever app it
		// names: consent is granted per project, so withdrawing it withdraws
		// all of them at once.
		if err := r.List(ctx, &callers, crclient.MatchingFields{
			linkTargetNamespaceIndex: namespaces.Items[i].Name,
		}); err != nil {
			// Keep what earlier namespaces produced rather than dropping the
			// whole mapping: reconciling some callers of a revoked consent
			// beats reconciling none.
			log.FromContext(ctx).Error(err, "could not map a project to some of its linked callers; the periodic sweep will catch the rest",
				"project", obj.GetName(), "namespace", namespaces.Items[i].Name)
			continue
		}
		for j := range callers.Items {
			key := types.NamespacedName{Name: callers.Items[j].Name, Namespace: callers.Items[j].Namespace}
			if seen[key] {
				continue
			}
			seen[key] = true
			reqs = append(reqs, reconcile.Request{NamespacedName: key})
		}
	}
	return reqs
}

// linkTargetNamespaceIndex finds callers by the namespace they link into, which
// is the granularity consent is granted at.
const linkTargetNamespaceIndex = "spec.links.targetNamespace"

// LinkTargetNamespaceKeys is that index's extractor.
func LinkTargetNamespaceKeys(o crclient.Object) []string {
	app, ok := o.(*kipperv1.App)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var keys []string
	for _, link := range app.Spec.Links {
		// The same emptiness rule the renderer uses. A link missing either half
		// can never produce a rule, so indexing it would enqueue a caller that
		// has nothing to rebuild.
		if link.App == "" || link.Namespace == "" || seen[link.Namespace] {
			continue
		}
		seen[link.Namespace] = true
		keys = append(keys, link.Namespace)
	}
	return keys
}

// IndexAppLinkNamespaces registers it.
func IndexAppLinkNamespaces(ctx context.Context, indexer crclient.FieldIndexer) error {
	return indexer.IndexField(ctx, &kipperv1.App{}, linkTargetNamespaceIndex, LinkTargetNamespaceKeys)
}

// setLinksOpenCondition reports whether every link this app declares carries
// traffic. It says so on the app itself because that is where somebody looks
// after a connection is refused: the link is recorded, both surfaces show it,
// and otherwise the only account of why it opened nothing is a line in the
// controller log.
//
// An app declaring no links carries no condition — there is nothing to report.
// One whose links all opened carries it as true, so "no complaint" and "not
// evaluated yet" are distinguishable.
//
// The status write happens here rather than at the end of the reconcile. Link
// policy is reconciled first precisely because the steps after it can fail, and
// a condition left in memory until the end would go missing on exactly the app
// that could not finish reconciling — the one whose operator most needs to know
// why its link is dead. Only a real change is written, so a healthy app is not
// updated every pass, and a failed write is logged rather than returned: the
// policy it describes is already correct, and failing the reconcile over the
// note about it would undo nothing and retry everything.
func (r *AppReconciler) setLinksOpenCondition(ctx context.Context, app *kipperv1.App, blocked []string) {
	var changed bool
	switch {
	case len(app.Spec.Links) == 0:
		changed = apimeta.RemoveStatusCondition(&app.Status.Conditions, kipperv1.ConditionLinksOpen)
	case len(blocked) == 0:
		changed = apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionLinksOpen,
			Status:             metav1.ConditionTrue,
			Reason:             "AllLinksOpen",
			Message:            "every declared link carries traffic",
			ObservedGeneration: app.Generation,
		})
	default:
		sort.Strings(blocked)
		changed = apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionLinksOpen,
			Status:             metav1.ConditionFalse,
			Reason:             "LinkOpensNothing",
			Message:            "no traffic is allowed to " + joinWithinConditionMessage(blocked),
			ObservedGeneration: app.Generation,
		})
	}
	if !changed {
		return
	}
	if err := r.Status().Update(ctx, app); err != nil {
		log.FromContext(ctx).Error(err, "recording which of this app's links carry traffic", "app", app.Name)
	}
}

// linkEnvVars is the address of each app this one links to, as the pod should
// see it: TARGET_URL for every link that resolved.
//
// The port is the target's Service port, not the port its pods listen on. This
// is what the caller dials, and the Service is what translates — the egress
// allowance names the other one for the same reason, and the two are different
// numbers whenever the target runs the instance-id sidecar.
//
// A link that opens nothing gets no variable. The caller would only be given an
// address the policy refuses to carry it to, which fails further from the cause
// than a missing variable does, and the LinksOpen condition already says why.
func linkEnvVars(live []ResolvedLink) []corev1.EnvVar {
	vars := make([]corev1.EnvVar, 0, len(live))
	for _, resolved := range live {
		vars = append(vars, corev1.EnvVar{
			Name:  AppEnvKey(resolved.Link.App),
			Value: resolved.URL(),
		})
	}
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars
}

// MaxLinks is the most links one app may declare, matching the CRD's bound. A
// writer checks it before building an update, so reaching the limit reads as a
// limit rather than as the API server rejecting a spec nobody meant to write.
const MaxLinks = 64

// AppEnvKey is the variable a link injects the target's address as. The rule
// lives in controller/pkg/applink because kip names the same variable from a
// module that cannot import this one.
func AppEnvKey(app string) string { return applink.EnvKey(app) }

// URL is the address the caller reaches this target on: its Service, on the
// port the Service publishes. Not the port the target's pods listen on — the
// egress allowance names that one, and they differ whenever the target runs the
// instance-id sidecar.
func (l ResolvedLink) URL() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d",
		l.Link.App, l.Link.Namespace, l.target.Spec.Port)
}
