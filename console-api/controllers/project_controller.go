package controllers

import (
	"context"
	goerrors "errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/nsowner"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/projectenv"
)

const projectFinalizer = kipperlabels.FinalizerProject

// errProjectReplaced ends a pass whose project has been replaced by a different
// one standing under the same name.
//
// Discovering the replacement is not success. The pass still holds the dead
// project's spec, and quota, member bindings, shared storage and the records
// all lie ahead of it, so carrying on writes that spec into namespaces the
// successor now holds. Returning it costs one requeue, which reads the
// successor and reconciles it as itself.
var errProjectReplaced = goerrors.New("the project was replaced while this pass ran")

// conditionEnvLimitExceeded flags a project whose environment count is above
// its effective cap, so namespaces beyond the limit are not created. The
// console surfaces it and it is the backstop for a direct CR create that slips
// past the CRD's CEL rule (which is a transition rule and so skips create).
const conditionEnvLimitExceeded = "EnvironmentLimitExceeded"

// conditionNamespaceConflict flags a project whose namespace is already claimed
// by a different project. Namespace names are not unique across projects —
// project "shop" environment "prod" and project "shop-prod" environment
// "default" both resolve to "shop-prod" — and adopting one another project owns
// puts this project's member RoleBindings in their namespace and relabels it,
// so each reconcile hands the other's members access. The namespace is left
// alone and the conflict is reported instead.
const conditionNamespaceConflict = "NamespaceConflict"

// The two reasons a namespace is refused need different repairs, so they are
// different reasons. Another project holding it is fixed by renaming one of
// them; nothing holding it is fixed by a person deciding whose it is. Sharing
// one reason sent an operator looking for a project that was not there.
const (
	reasonNamespaceOwnedByAnotherProject = "NamespaceOwnedByAnotherProject"
	reasonNamespaceNotLabelled           = "NamespaceNotLabelled"
)

// ProjectReconciler reconciles a Project CR.
type ProjectReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads straight from the API server, bypassing the manager
	// cache. The cache only watches pods in the monitoring namespace, so
	// pod listings in project namespaces must not go through it.
	APIReader client.Reader

	// Recorder emits events for over-limit projects. Nil in unit tests.
	Recorder record.EventRecorder
}

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var project kipperv1.Project
	if err := r.Get(ctx, req.NamespacedName, &project); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !project.DeletionTimestamp.IsZero() {
		logger.Info("cleaning up project resources", "project", project.Name)
		pruned, err := r.deleteProjectNamespaces(ctx, &project, false)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting project namespaces: %w", err)
		}
		if pruned == nil {
			// Already gone from the API server, so there is no finalizer left
			// to remove and the update below would only report that.
			return ctrl.Result{}, nil
		}
		controllerutil.RemoveFinalizer(&project, projectFinalizer)
		return ctrl.Result{}, r.Update(ctx, &project)
	}

	if !controllerutil.ContainsFinalizer(&project, projectFinalizer) {
		controllerutil.AddFinalizer(&project, projectFinalizer)
		if err := r.Update(ctx, &project); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Before any ownership-dependent write. Only the finalizer above comes
	// earlier on this path, and that one is about this object rather than about
	// a namespace, so a stale incarnation cannot land it: the API server holds
	// the UID immutable across an update. The deletion branch returns rather
	// than reaching here and carries its own check. The loop below creates namespaces
	// and their egress policy, and both are ownership-dependent:
	// claimable skips every project sharing this one's name, so a successor
	// standing in the name never registers as a rival and a stale pass is free
	// to create a namespace the successor never declared. Nothing then collects
	// it, because the claim is what makes a namespace collectable and a pass
	// that ends at publishClaim has written none.
	//
	// The window this leaves is one pass wide rather than one cache lag wide: a
	// project replaced between this read and the create still lands there.
	//
	// Deletion can finish inside that window too, and that one does not repair
	// itself. Another replica handling the delete finds no record of a namespace
	// this pass has not created yet, takes the finalizer off, and the project is
	// gone; this pass then creates the namespace, and publishClaim finds nothing
	// to write the claim onto. What is left is a namespace labelled for a
	// project that no longer exists, named by neither record, which no later
	// pass can collect because there is no project to run one. Closing it needs
	// the acquisition and the deletion to coordinate, which this design has no
	// means to do, so `kip upgrade` names those namespaces separately from the
	// ones merely waiting to be recorded, and says nothing will collect them.
	standing, err := r.confirmIncarnation(ctx, &project)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !standing {
		// Gone between the cache read and here, which the prune below and the
		// read at the top of this function both treat the same way. There is
		// nothing left to act on and nothing to retry.
		return ctrl.Result{}, nil
	}

	environments := ProjectEnvironments(&project)

	// The keep-list is every environment name, including any beyond the limit,
	// so pruning never deletes a live namespace merely because the tier was
	// lowered or an out-of-band CR edit exceeded the cap.
	envNames := make([]string, 0, len(environments))
	for _, env := range environments {
		envNames = append(envNames, env.Name)
	}

	// Enforce the environment cap by existence, not spec order: a namespace
	// that already exists is always reconciled (we never shrink a live
	// project), and new namespaces are created only while the total stays
	// within the effective limit. Deciding existence up front means a
	// reordered spec can never strand an existing namespace behind a new one.
	limit := project.EffectiveEnvLimit()
	type envPlan struct {
		env    kipperv1.ProjectEnvironment
		ns     string
		order  int
		exists bool
	}
	plans := make([]envPlan, 0, len(environments))
	existing := 0
	for i, env := range environments {
		ns := ResolveNamespace(project.Name, env.Name)
		exists, err := r.namespaceExists(ctx, ns)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking namespace %s: %w", ns, err)
		}
		if exists {
			existing++
		}
		plans = append(plans, envPlan{env: env, ns: ns, order: i, exists: exists})
	}

	created := 0
	var namespaces []string
	var claimed []kipperv1.NamespaceClaim
	var namespaceConflicts []*namespaceConflictError
	for _, pl := range plans {
		if !pl.exists {
			if existing+created >= limit {
				// Beyond the limit and not yet created: skip creation but keep
				// it in envNames so it is never pruned.
				continue
			}
			created++
		}

		uid, err := r.reconcileNamespace(ctx, &project, pl.ns, pl.env.Name, envNames, pl.order)
		if err != nil {
			// A collision is not retryable: the resolved name will keep
			// colliding until somebody renames one of the two projects. Report
			// it, skip that one namespace, and carry on with the rest of the
			// project rather than wedging the whole reconcile behind it.
			var conflict *namespaceConflictError
			if goerrors.As(err, &conflict) {
				namespaceConflicts = append(namespaceConflicts, conflict)
				continue
			}
			return ctrl.Result{}, fmt.Errorf("reconciling namespace %s: %w", pl.ns, err)
		}

		// Isolate the namespace before it is handed to members or workloads, so
		// it is never usable without its egress policy in place.
		if err := r.reconcileEgressPolicy(ctx, pl.ns); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling egress policy in %s: %w", pl.ns, err)
		}

		// A write that landed and came back with no UID leaves the pass unable
		// to name what it just took, so it stops here. Carrying on would grant
		// quota and member bindings inside a namespace no record says is this
		// project's, and recording the claim anyway would be worse still: an
		// empty UID matches no live namespace, so the project would not
		// recognise its own, while still reading as a claim to anything that
		// looks at the name alone.
		if uid == "" {
			return ctrl.Result{}, fmt.Errorf("namespace %s was written and came back with no UID", pl.ns)
		}

		// The claim is recorded here rather than at the end of the pass. Later
		// than the egress policy, because a namespace is not this project's to
		// hand out until it is isolated; earlier than everything else, because
		// quota, bindings, shared storage and pruning all come after this point
		// and any of them can fail, and a claim written only once they had all
		// succeeded would leave the namespace unclaimed for as long as one of
		// them kept failing. The add-environment flow waits on the claim, so
		// that is a hang rather than a delay.
		claimed = append(claimed, kipperv1.NamespaceClaim{Name: pl.ns, UID: uid})
		if err := r.publishClaim(ctx, &project, pl.ns, claimed); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording the claim to %s: %w", pl.ns, err)
		}

		if err := r.reconcileQuota(ctx, &project, pl.env, pl.ns); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling quota in %s: %w", pl.ns, err)
		}

		if err := r.reconcileMemberBindings(ctx, &project, pl.ns); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling member bindings in %s: %w", pl.ns, err)
		}

		if project.Spec.SharedStorage != nil && project.Spec.SharedStorage.Enabled {
			if err := r.reconcileSharedStorage(ctx, &project, pl.ns); err != nil {
				return ctrl.Result{}, fmt.Errorf("reconciling shared storage in %s: %w", pl.ns, err)
			}
		}

		namespaces = append(namespaces, pl.ns)
	}

	// The project this prune decided from, so the records kept below and the
	// namespaces deleted here answer to one spec. Reading it twice let a spec
	// edit land in between, and the two answers then disagreed: the delete kept
	// a namespace the record prune dropped, which strands it for good, because
	// cleanup reaches a labelled namespace only while the records still name it.
	pruned, err := r.deleteProjectNamespaces(ctx, &project, true)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("pruning removed environments: %w", err)
	}
	if pruned == nil {
		// The project went during the pass. There is nothing left to record it
		// on, and the records must not be pruned from a spec nobody has.
		return ctrl.Result{}, nil
	}

	// Granting is done. Everything above skips a namespace whose ownership it
	// cannot prove, which leaves the bindings already sitting there untouched:
	// a member removed from the project keeps their access in exactly the
	// namespace nobody is watching. This pass reaches those, and can only take
	// away.
	//
	// It runs after the loop rather than inside it because it is not per
	// namespace: it finds every binding carrying this project's label wherever
	// it is, including in namespaces that have dropped out of the project's
	// environments entirely.
	if err := r.revokeStaleMemberBindings(ctx, &project); err != nil {
		return ctrl.Result{}, fmt.Errorf("revoking stale member bindings: %w", err)
	}

	// The project is over limit whenever it declares more environments than its
	// cap, whether that is a fresh spec we capped or an existing project a
	// downgrade pushed over. Both surface the condition; creation is what the
	// loop above skips.
	r.setEnvLimitCondition(&project, len(environments) > limit, len(environments), limit)

	r.setNamespaceConflictCondition(&project, namespaceConflicts)
	project.Status.Phase = "Active"
	// What the project declares according to the API server, not what the pass
	// started from. Both records are pruned by that set, and a claim written
	// mid-pass syncs the live resourceVersion onto this copy, so the conflict
	// that used to stop a stale spec reaching this write no longer fires.
	// Pruning from a spec one watch event behind drops the record and the claim
	// to a namespace that is standing and declared, and those records are what
	// adoption and the deletion backstop read.
	declared := declaredEnvNames(pruned)
	project.Status.Namespaces = r.keepLiveNamespaces(ctx, project.Status.Namespaces, namespaces, declared, project.Name)
	project.Status.NamespaceClaims = r.keepLiveClaims(ctx, &project, claimed, declared)
	// What was projected, and from which generation. Written now and read a
	// release later, once every pod carries the field: an older pod's
	// whole-status write drops what its struct does not know, so a build that
	// trusted this today would be trusting a gap.
	project.Status.ProjectedMembers = append([]kipperv1.ProjectMember(nil), project.Spec.Members...)
	project.Status.ProjectedMembersGeneration = project.Generation
	if err := r.Status().Update(ctx, &project); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	// Requeue periodically so each namespace's egress except-list refreshes even
	// if a Node add/remove event was missed (a map-func List error can't be
	// retried), bounding how long a new node's IP stays reachable to this period
	// rather than the manager's multi-hour informer resync.
	return ctrl.Result{RequeueAfter: egressRefreshInterval}, nil
}

// confirmIncarnation reports whether the project this pass began from is still
// the one standing under its name.
//
// False means it has gone, and the caller ends the pass without an error,
// because a project that no longer exists is not a failure to retry. A different
// project standing in the name is errProjectReplaced instead: that one says the
// pass was about to act on somebody else's behalf.
//
// A read that fails stops the pass too. Creating namespaces for a project whose
// existence could not be confirmed is the thing this exists to prevent.
func (r *ProjectReconciler) confirmIncarnation(ctx context.Context, project *kipperv1.Project) (bool, error) {
	var current kipperv1.Project
	switch err := r.APIReader.Get(ctx, types.NamespacedName{Name: project.Name}, &current); {
	case errors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("confirming project %s still stands: %w", project.Name, err)
	}
	if current.UID != project.UID {
		return false, errProjectReplaced
	}
	return true, nil
}

// declaredEnvNames is every environment the project declares, including any
// beyond its tier's limit, which is the set the namespaces were resolved from.
func declaredEnvNames(project *kipperv1.Project) []string {
	environments := ProjectEnvironments(project)
	names := make([]string, 0, len(environments))
	for _, env := range environments {
		names = append(names, env.Name)
	}
	return names
}

// keepLiveNamespaces merges what this pass held into the namespaces already
// recorded, instead of replacing them.
//
// The list is the pre-claims record of what a project holds, and on a cluster
// upgrading from a release that wrote no claims it is the only evidence there
// is. Rebuilding it from what this pass reached meant one skipped namespace
// erased that evidence for good, and a skip is the ordinary case rather than a
// rare one: a namespace whose label somebody rewrote is skipped, and so is one
// whose pass failed. Everything then reads the record the skip destroyed.
// Nothing prunes the namespace, deleting the project strands it, and no later
// pass can take it back, because adoption reads the record too.
//
// What is kept is bounded the same way a claim is, and by the same rule: the
// object still has to be there, and the project still has to be asking for the
// namespace. Existence alone was not enough, because this record authorises
// deletion and an unbounded one outlives the project's right to the namespace.
// A project that stops declaring an environment whose namespace was relabelled
// away would hold that name for good; another project then adopts and claims the
// namespace, and the day somebody points the label back, the stale record
// authorises deleting a live namespace belonging to somebody else. The label
// cannot be the guard against that, because rewriting the label is the move the
// deletion gate exists to survive.
//
// A namespace that is already terminating is kept, so the record goes when the
// object does rather than one pass earlier, and a namespace that cannot be read
// is kept, because a read that failed is not evidence it is gone. Both for the
// same reasons as the claims.
func (r *ProjectReconciler) keepLiveNamespaces(ctx context.Context, recorded, held, envNames []string, project string) []string {
	wanted := make(map[string]struct{}, len(envNames))
	for _, env := range envNames {
		wanted[ResolveNamespace(project, env)] = struct{}{}
	}

	out := slices.Clone(held)
	for _, name := range recorded {
		if slices.Contains(out, name) {
			continue
		}
		var ns corev1.Namespace
		switch err := r.APIReader.Get(ctx, types.NamespacedName{Name: name}, &ns); {
		case errors.IsNotFound(err):
			continue
		case err != nil:
			out = append(out, name)
			continue
		}
		if _, want := wanted[name]; !want && ns.DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, name)
	}
	return out
}

// keepLiveClaims merges what this pass recorded into the claims already
// standing, instead of replacing them.
//
// Replacing dropped the claim to any namespace the pass did not reach, and the
// one thing that stops a pass reaching a namespace this project already holds is
// somebody rewriting its label. So the record whose whole promise is that it
// survives a relabel was erased by a relabel. Keeping is what makes the promise
// true.
//
// What is kept is bounded, because "never prune" is its own bug. A prior claim
// survives only while the object it names is still there under the same UID and
// the project is still asking for that namespace. Liveness on its own would let
// a claim outlive the project's right to the namespace: relabel it away, drop
// the environment from the spec, and a claim nothing will ever revisit sits
// there refusing the namespace to whoever does hold it.
//
// A namespace that is already terminating is kept too, so the claim goes when
// the object does rather than one pass earlier, while workloads are still
// winding down inside it.
//
// A namespace that cannot be read is kept. Dropping a claim is the direction
// that loses somebody their project, and a read that failed is not evidence the
// namespace is gone; the next pass prunes it once the answer is known.
func (r *ProjectReconciler) keepLiveClaims(ctx context.Context, project *kipperv1.Project, claimed []kipperv1.NamespaceClaim, envNames []string) []kipperv1.NamespaceClaim {
	wanted := make(map[string]struct{}, len(envNames))
	for _, env := range envNames {
		wanted[ResolveNamespace(project.Name, env)] = struct{}{}
	}
	recorded := make(map[string]struct{}, len(claimed))
	for _, claim := range claimed {
		recorded[claim.Name] = struct{}{}
	}

	out := slices.Clone(claimed)
	for _, claim := range project.Status.NamespaceClaims {
		if _, done := recorded[claim.Name]; done {
			continue
		}
		var ns corev1.Namespace
		switch err := r.APIReader.Get(ctx, types.NamespacedName{Name: claim.Name}, &ns); {
		case errors.IsNotFound(err):
			continue
		case err != nil:
			out = append(out, claim)
			continue
		}
		// A claim whose UID no longer matches is kept, not dropped, while the
		// project still asks for the name. It stops being a claim to the live
		// object and becomes the evidence that the live object is not this
		// project's, which is what the unlabelled cleanup path asks for: without
		// it only the name-only record answers, and it says yes about a
		// namespace somebody else recreated under a name this project used to
		// hold. Dropping it made the refusal the reconcile had just reported
		// last exactly one pass.
		//
		// Nothing lingers. A pass that genuinely adopts the replacement
		// republishes the claim against the new object, so this loop never sees
		// the old one; the object going away drops it above; and the project no
		// longer wanting the name drops it below.
		if _, want := wanted[claim.Name]; !want && ns.DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, claim)
	}
	return out
}

// namespaceExists reports whether the namespace already exists.
func (r *ProjectReconciler) namespaceExists(ctx context.Context, ns string) (bool, error) {
	var existing corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: ns}, &existing)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// setEnvLimitCondition records whether the project is over its environment cap
// and emits a warning event on the transition into the over-limit state.
func (r *ProjectReconciler) setEnvLimitCondition(project *kipperv1.Project, overLimit bool, count, limit int) {
	if !overLimit {
		apimeta.RemoveStatusCondition(&project.Status.Conditions, conditionEnvLimitExceeded)
		return
	}
	msg := fmt.Sprintf("project has %d environments but its limit is %d; namespaces beyond the limit are not created until a cluster admin raises the tier or the project's environment limit", count, limit)
	changed := apimeta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{
		Type:               conditionEnvLimitExceeded,
		Status:             metav1.ConditionTrue,
		Reason:             "OverLimit",
		Message:            msg,
		ObservedGeneration: project.Generation,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Event(project, corev1.EventTypeWarning, conditionEnvLimitExceeded, msg)
	}
}

// ownedNamespaces lists the namespaces this project is answerable for: the ones
// it has a record of having held, whether they still carry its label or have
// lost it altogether.
//
// A record is required, and the label alone is never one. This list is what
// pruning and project deletion delete from, and the label is writable by anyone
// who can write a namespace, so accepting it on its own makes pointing a
// victim's namespace at another project a way to have that namespace destroyed:
// the project it now names has no reason to keep a namespace it never declared,
// and its next pass deletes it with everything inside. A wrong answer elsewhere
// in this file discloses something; a wrong answer here cannot be undone.
//
// Two records count, and they differ in when they are written. A claim naming
// the object is published as early as it can be, which is once the namespace is
// proven this project's and isolated, so it covers a pass that adopted a
// namespace and then failed at anything after that. The status this project
// last wrote is the older record, and the one that is already there on a
// cluster upgrading from a build that wrote no claims. Neither is reachable by
// writing a label.
//
// The claim is matched on the object and not on the name, through the same rule
// the owner lookup answers from: a namespace deleted and recreated is a
// different namespace, and a claim naming the one that is gone must not
// authorise deleting its replacement.
//
// Both records are read a second time as the backstop. A namespace whose label
// was removed is invisible to the label query, so deleting the project would
// remove the finalizer and leave that namespace standing with its workloads and
// its member bindings and nothing left to collect them — the Project is
// cluster-scoped, so no owner reference reaches down to them. With no label to
// corroborate it, that path asks for the object and not just the name.
//
// A namespace whose label now names a different project is left out. It answers
// to them, and taking it on the strength of this project's own stale status
// would delete a live namespace out from under a project that legitimately
// holds it.
//
// Neither record is derived from the spec, and that is the point. Deriving
// candidates from what a project asks for would name namespaces it was refused
// — a pre-existing unlabelled "blog" that reconcileNamespace declines to adopt
// would be deleted along with a project called "blog" that never owned it. Both
// records say what the reconcile actually took.
//
// The reach is bounded by the records rather than by the pass. A namespace the
// project still declares an environment for stays in both records for as long
// as the object exists, whether or not the pass reached it, so one whose label
// somebody rewrote remains collectable until it goes or the project stops
// asking for the name. Past that the records let go: a namespace the project no
// longer declares and whose label now names somebody else is left alone,
// because keeping it would authorise deleting a live namespace another project
// legitimately holds.
func (r *ProjectReconciler) ownedNamespaces(ctx context.Context, project *kipperv1.Project) ([]corev1.Namespace, error) {
	// From the API server, like everything else this decision rests on: a
	// cached list can hold a namespace whose label has since moved, and the
	// answer here is a delete.
	var labelled corev1.NamespaceList
	if err := r.APIReader.List(ctx, &labelled, client.MatchingLabels{kipperlabels.Project: project.Name}); err != nil {
		return nil, err
	}

	// Every project, because this project's own records cannot tell whether
	// somebody else holds the live object, and a claim on it outranks any
	// name-only record of the name.
	//
	// Straight from the API server, not the cache. The interleaving this has to
	// survive is a rival publishing its claim and the namespace then being
	// relabelled, and those are two objects on two informer streams with no
	// ordering between them: the cache can show the relabel and not yet the
	// claim, and the answer here is a delete that cannot be undone. The UID
	// precondition does not help, because the object is the same one; what
	// changed is who holds it.
	var projects kipperv1.ProjectList
	if err := r.APIReader.List(ctx, &projects); err != nil {
		return nil, fmt.Errorf("reading projects to see who holds this project's namespaces: %w", err)
	}

	var owned []corev1.Namespace
	seen := make(map[string]struct{}, len(labelled.Items))
	for _, ns := range labelled.Items {
		if !nsowner.EverHeld(project.Status, ns.Name, ns.UID) {
			continue
		}
		if nsowner.ClaimedElsewhere(projects.Items, project.Name, ns.Name, ns.UID) {
			continue
		}
		owned = append(owned, ns)
		seen[ns.Name] = struct{}{}
	}

	// The backstop, for what the label query cannot see. Candidates come from
	// both records rather than only the namespace list: the claim is written far
	// earlier in the pass than the status is, so a pass that adopted a namespace
	// and then failed leaves a claim and no list entry, and stripping that
	// namespace's label would otherwise put it beyond every cleanup path there
	// is.
	//
	// One window is not covered, and both records are silent inside it. A
	// namespace is taken by the create or the label write, and the claim to it
	// is published two steps later, after the egress policy. A pass that fails
	// in between leaves the namespace labelled for this project and named by
	// neither record, so this function does not see it. The next pass takes the
	// update path and publishes the claim, which closes it; it stays open only
	// while the pass keeps failing, and it becomes permanent if the project
	// stops declaring that environment first, because then no pass ever reaches
	// the namespace again. Publishing the claim earlier is not the fix: it
	// resolves the namespace to the project, and handing it out before its
	// egress policy is in place is what that ordering exists to prevent.
	for _, name := range recordedNames(project.Status) {
		if _, ok := seen[name]; ok {
			continue
		}
		var ns corev1.Namespace
		switch err := r.APIReader.Get(ctx, types.NamespacedName{Name: name}, &ns); {
		case errors.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("reading namespace %s: %w", name, err)
		}
		if ns.Labels[kipperlabels.Project] != "" {
			continue
		}
		// Object identity, not the name, because there is no label here to
		// corroborate it. A namespace deleted and recreated unlabelled is
		// somebody else's until a pass adopts it, and the reconcile says so by
		// refusing it; the project's own claim naming the object that went away
		// is the evidence that this is not what it took. Collecting on the name
		// alone destroys a third party's namespace and contradicts the refusal
		// the operator has just been shown.
		if !nsowner.HoldsObject(project.Status, ns.Name, ns.UID) {
			continue
		}
		if nsowner.ClaimedElsewhere(projects.Items, project.Name, ns.Name, ns.UID) {
			continue
		}
		owned = append(owned, ns)
		seen[ns.Name] = struct{}{}
	}
	return owned, nil
}

// recordedNames is every namespace name this project's records mention, from
// either record and without duplicates.
func recordedNames(status kipperv1.ProjectStatus) []string {
	names := slices.Clone(status.Namespaces)
	for _, claim := range status.NamespaceClaims {
		if !slices.Contains(names, claim.Name) {
			names = append(names, claim.Name)
		}
	}
	return names
}

// deleteProjectNamespaces deletes the namespaces this project owns.
//
// keepDeclared keeps the ones the project still declares an environment for,
// which is pruning; false keeps none, which is what project deletion wants.
//
// The environments come from the project this function reads, not from a list
// the caller worked out, because the caller worked it out from the cache. An
// environment removed and immediately added back leaves one replica holding the
// intermediate spec, and a keep set built from it is missing a namespace the
// API server says to keep. Every namespace beyond the tier's limit is kept too,
// the same as before: they are declared, so pruning never removes a live
// namespace because somebody lowered a tier.
//
// It returns the project it decided from, so a caller that has to agree with
// this decision agrees with it exactly rather than reading again. Nil means the
// project has gone.
func (r *ProjectReconciler) deleteProjectNamespaces(ctx context.Context, project *kipperv1.Project, keepDeclared bool) (*kipperv1.Project, error) {
	// Re-read the project from the API server before deleting anything.
	//
	// Every input to this decision has to be current, not just the rival claims
	// below. The pass began from the cache, and a cache is per replica and
	// behind by an unbounded amount: an environment removed and immediately
	// added back leaves one replica holding the intermediate spec, and the keep
	// set built from it is missing a namespace the API server says to keep. The
	// rival check cannot save it, because the claim on that namespace is this
	// project's own and is skipped by name.
	//
	// A read that fails stops the pass. Deleting from a spec that could not be
	// confirmed is the thing this exists to prevent.
	var current kipperv1.Project
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: project.Name}, &current); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("re-reading project %s before deleting its namespaces: %w", project.Name, err)
	}
	// And it has to be the same object. A name outlives what carried it, so a
	// project deleted and recreated under this one's name is a different
	// project, and reading its records here would authorise deleting its
	// namespaces on the strength of a pass that was never about it. The
	// namespace preconditions cannot catch that: those namespaces are the live
	// ones, checked against the very records that say they may be taken. Both
	// other delete paths bind to the object they read; this is the same rule.
	//
	// And it ends the pass. On the pruning call the revoke sweep and the status
	// write still follow, and both act on the dead project's spec.
	if current.UID != project.UID {
		return nil, errProjectReplaced
	}
	project = &current

	// Keep by namespace rather than by environment name. Working an environment
	// name back out of a namespace has to guess which spelling of the default
	// one the project used — "" and "default" both resolve to the bare project
	// name — and guessing wrong here deletes a live namespace. Resolving the
	// declared names forwards asks the same rule that created them.
	keep := map[string]struct{}{}
	if keepDeclared {
		for _, env := range ProjectEnvironments(project) {
			keep[ResolveNamespace(project.Name, env.Name)] = struct{}{}
		}
	}

	namespaces, err := r.ownedNamespaces(ctx, project)
	if err != nil {
		return nil, err
	}

	for _, ns := range namespaces {
		if _, ok := keep[ns.Name]; ok {
			continue
		}
		// Bound to the object the records were checked against. A namespace can
		// finish deleting and be recreated under the same name between the read
		// above and here, and a delete by name alone would take the replacement
		// on the strength of a check made against its predecessor. That matters
		// most for the name-only record, which authorises cleanup without ever
		// naming an object. A precondition that no longer holds means the
		// authorised object has already gone.
		uid := ns.UID
		err := r.Delete(ctx, &ns, client.Preconditions{UID: &uid})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return nil, fmt.Errorf("deleting namespace %s: %w", ns.Name, err)
		}
		// A namespace's route host claims are cluster-scoped ConfigMaps in
		// kipper-system with no owner reference back to it, so they are not
		// garbage-collected with the namespace. They are not released here on
		// purpose: releasing while the namespace and its Ingresses are still
		// terminating would open a dual-serve window, and any delete path (this
		// one, the HTTP handler, an out-of-band delete) would have to be kept in
		// sync. Instead reserveHost reclaims a claim whose owner namespace is
		// gone, so an abandoned host is taken over by the next project that wants
		// it once the old one is fully deleted.
	}
	return project, nil
}

// reconcileNamespace creates or updates the namespace for one environment.
//
// envName is the environment as the project declares it, passed in rather than
// worked back out of the namespace name. Deriving it looked right for a
// suffixed environment and was wrong for the default one, whose namespace is
// the bare project name and so has no suffix to strip: the label came out as
// the project's name. Everything that reads the label back to build a hostname
// then disagreed with everything that used the declared name.
//
// It returns the UID of the object the write landed on, which is what the claim
// is then published against. Re-reading the name afterwards would bind the claim
// to whatever carries it at that moment rather than to the object this proved.
func (r *ProjectReconciler) reconcileNamespace(ctx context.Context, project *kipperv1.Project, ns, envName string, envNames []string, order int) (types.UID, error) {

	desired := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				kipperLabel:              kipperValue,
				"kipper.run/project":     project.Name,
				"kipper.run/environment": envName,
				"kipper.run/env-order":   fmt.Sprintf("%d", order),
				// Pod Security baseline blocks the pod shapes (privileged,
				// hostPath, hostNetwork) that turn a namespace editor into a
				// node attacker; restricted is surfaced as warnings and audit
				// so workloads can converge on it before it is ever enforced.
				// Enforcement bites at pod admission only: a pre-existing
				// violating workload keeps running and fails at its next
				// restart or reschedule, so rollout onto an existing cluster
				// requires a violation sweep of every project namespace
				// first, or the label lands as a delayed outage.
				"pod-security.kubernetes.io/enforce": "baseline",
				"pod-security.kubernetes.io/warn":    "restricted",
				"pod-security.kubernetes.io/audit":   "restricted",
			},
			Annotations: map[string]string{
				"kipper.run/environments": strings.Join(envNames, ","),
			},
		},
	}

	// Cannot set owner reference on cluster-scoped to namespace-scoped or vice versa
	// in certain configurations, so we rely on labels for association

	// From the API server. What comes back decides ownership twice over, through
	// claimable and through the label check below, and a cached copy can carry a
	// label that has since moved to another project.
	var existing corev1.Namespace
	err := r.APIReader.Get(ctx, types.NamespacedName{Name: ns}, &existing)
	if errors.IsNotFound(err) {
		// Checked before creating, not only before adopting. Two projects that
		// resolve to one name and find it absent would otherwise both create
		// it, and whichever lost the race would carry on as though it had not:
		// ownership decided by scheduling, which is the thing the collision
		// refusal exists to prevent.
		if err := r.claimable(ctx, project, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return "", err
		}
		if err := r.Create(ctx, desired); err != nil {
			return "", err
		}
		return desired.UID, nil
	}
	if err != nil {
		return "", err
	}

	// Whose namespace this is, decided from what projects recorded taking
	// rather than from the label on the object.
	//
	// The rule is one sentence: a project adopts a namespace no project claims,
	// whatever created it and whenever it appears, and never takes one another
	// project claims. Not "only what this reconcile created", which locks a
	// project out of namespaces a restore or `kip` made; not "seeded once while
	// the project has no claims", which refuses an environment added later. Both
	// were tried and both were wrong.
	if err := r.claimable(ctx, project, &existing); err != nil {
		return "", err
	}

	// A namespace is this project's to reconcile when this reconcile created it
	// or it is already labelled for this project. Anything else is somebody
	// else's, and adopting it relabels it, then hands this project's members
	// RoleBindings over whatever is already running there.
	//
	// An absent label is not permission. It means Kipper did not create this
	// namespace, so its contents belong to whoever did — a namespace made with
	// kubectl that happens to match a new project's name would otherwise have
	// its workloads and secrets exposed to that project's members.
	if owner := existing.Labels[kipperlabels.Project]; owner != project.Name {
		return "", &namespaceConflictError{namespace: ns, owner: owner, claimant: project.Name}
	}

	// Update labels and annotations
	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	for k, v := range desired.Annotations {
		existing.Annotations[k] = v
	}
	if err := r.Update(ctx, &existing); err != nil {
		return "", err
	}
	return existing.UID, nil
}

func (r *ProjectReconciler) reconcileSharedStorage(ctx context.Context, project *kipperv1.Project, ns string) error {
	pvcName := "shared-uploads"
	storageClass := "longhorn"

	size := "5Gi"
	if project.Spec.SharedStorage != nil && project.Spec.SharedStorage.Size != "" {
		size = project.Spec.SharedStorage.Size
	}

	desired := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
			Labels: map[string]string{
				kipperLabel:                kipperValue,
				"kipper.run/resource-type": "shared-volume",
				"kipper.run/project":       project.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}

	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: ns}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

// ResolveNamespace returns the namespace an environment of a project runs in.
//
// Exported because the API server has to reject a project whose namespaces would
// collide with another's, and it can only do that by resolving the same names
// this reconciler goes on to create. A second copy of the rule would guard names
// nothing uses the moment the two drifted.
func ResolveNamespace(projectName, envName string) string {
	if isDefaultEnvironment(envName) {
		return projectName
	}
	return projectName + "-" + envName
}

// isDefaultEnvironment reports whether an environment resolves to the project's
// own name rather than a suffixed one. It is the single condition behind both
// the namespace a project's environment gets and the subdomain its apps serve
// on, because those two answering differently is how a cluster ends up serving
// one hostname while every surface reports another.
func isDefaultEnvironment(env string) bool {
	return env == "" || env == "default"
}

// DefaultEnvironmentName is the environment this reconciler creates for a
// project that declares none. The rule is shared with the CLI, which mutates
// the same list from the other side.
const DefaultEnvironmentName = projectenv.Default

// ProjectEnvironments returns the environments a project actually has, which is
// its declared list or the default one when that list is empty.
//
// Exported for the same reason as ResolveNamespace: resolving a project to its
// namespaces takes both rules, and anything holding only one of them answers
// for a set the reconciler does not build. A collision check reading the
// declared list alone approves a project whose name collides with the namespace
// another project's defaulted environment already occupies.
func ProjectEnvironments(project *kipperv1.Project) []kipperv1.ProjectEnvironment {
	if len(project.Spec.Environments) == 0 {
		return []kipperv1.ProjectEnvironment{{Name: DefaultEnvironmentName}}
	}
	return project.Spec.Environments
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := IndexMemberBindings(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("indexing member bindings by project: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.Project{}).
		// Member RoleBindings are authorization objects: drift must trigger
		// repair, not wait for the next Project event.
		// The manager's client is handed to the mapper so a binding whose
		// project label has drifted is still routed: without a reader it can
		// only read the label, which is the one thing drift removes.
		Watches(&rbacv1.RoleBinding{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				return memberBindingProjects(ctx, mgr.GetClient(), obj)
			})).
		// A node added or removed changes every project's egress except-list.
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.enqueueProjectsForNode)).
		Complete(r)
}

// namespaceConflictError says a namespace is already claimed by another project.
// It is a distinct type so the reconcile can report it as a condition an
// operator can act on rather than as an ordinary retryable failure: retrying
// changes nothing, because the resolved name will collide until one of the two
// projects is renamed.
type namespaceConflictError struct {
	namespace string
	owner     string
	claimant  string
}

func (e *namespaceConflictError) Error() string {
	if e.owner == "" {
		return fmt.Sprintf("namespace %q already exists and was not created by Kipper, so project %q "+
			"will not take it over; whatever is running there would become visible to this project's "+
			"members. Rename the project, or remove the namespace if it is genuinely unused",
			e.namespace, e.claimant)
	}
	return fmt.Sprintf("namespace %q already belongs to project %q, so project %q cannot use it; "+
		"the two projects resolve to the same namespace name and one of them has to be renamed",
		e.namespace, e.owner, e.claimant)
}

// RefusedNamespaces reads the namespaces back out of a NamespaceConflict
// message.
//
// It sits next to the code that writes that message because the format is what
// defines it: the names come out of the quoted slots rather than out of the
// prose. A message names the project that wanted the namespace as well as the
// namespace itself, and a project's default environment resolves to a namespace
// carrying the project's own name, so anything matching against the whole
// message answers about the claimant and reports a refusal of something that
// was never refused.
//
// A message written by an older build quotes nothing, so nothing is read out of
// it and the caller waits for its claim instead of being told the reason early.
// That is the slower answer rather than the wrong one.
func RefusedNamespaces(message string) []string {
	var out []string
	if _, rest, ok := strings.Cut(message, `namespace "`); ok {
		if name, _, ok := strings.Cut(rest, `"`); ok {
			out = append(out, name)
		}
	}
	if _, rest, ok := strings.Cut(message, "same position: "); ok {
		for _, field := range strings.Split(rest, ",") {
			out = append(out, strings.Trim(strings.TrimSpace(field), `"`))
		}
	}
	return out
}

// setNamespaceConflictCondition records a collision, or clears it once the
// projects no longer overlap.
// setNamespaceConflictCondition reports every namespace this pass could not
// take. Reporting only the last one turns fixing a project with two colliding
// environments into a game of whack-a-mole: rename one, wait a reconcile, learn
// about the next.
func (r *ProjectReconciler) setNamespaceConflictCondition(project *kipperv1.Project, conflicts []*namespaceConflictError) {
	if len(conflicts) == 0 {
		apimeta.RemoveStatusCondition(&project.Status.Conditions, conditionNamespaceConflict)
		return
	}
	message := conflicts[0].Error()
	// The reason describes the first conflict, which is the one the message
	// leads with. A pass that hits both kinds reports the first and names the
	// rest, and the operator sees the second kind once the first is dealt with.
	reason := reasonNamespaceOwnedByAnotherProject
	if conflicts[0].owner == "" {
		reason = reasonNamespaceNotLabelled
	}
	if len(conflicts) > 1 {
		others := make([]string, 0, len(conflicts)-1)
		for _, conflict := range conflicts[1:] {
			others = append(others, fmt.Sprintf("%q", conflict.namespace))
		}
		message = fmt.Sprintf("%s. %d more namespaces are in the same position: %s",
			message, len(others), strings.Join(others, ", "))
	}
	changed := apimeta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{
		Type:               conditionNamespaceConflict,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: project.Generation,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Event(project, corev1.EventTypeWarning, conditionNamespaceConflict, message)
	}
}

// claimable reports whether this project may hold the namespace, and refuses
// with a conflict naming whoever does hold it.
//
// It reads the claims rather than the label because the label is what drifts:
// anyone who can write a namespace can rewrite it, and it is the thing an
// attacker rewrites. A claim is what a project's own reconcile recorded taking.
//
// Matching is on the UID as well as the name. A name outlives the object it
// named, so a namespace deleted and recreated is a different namespace, and a
// claim naming the old UID says nothing about the new one.
//
// Two projects resolving to one name, with nobody holding it, is refused to
// both. Handing it to whichever reconciles first decides ownership by a race,
// and the loser is a tenant whose workloads the winner's members can then read.
func (r *ProjectReconciler) claimable(ctx context.Context, project *kipperv1.Project, ns *corev1.Namespace) error {
	// From the API server. This decides whether one project may take a
	// namespace another holds, and a cached list can be missing the claim that
	// says somebody already has.
	var projects kipperv1.ProjectList
	if err := r.APIReader.List(ctx, &projects); err != nil {
		return fmt.Errorf("reading projects to see who holds %s: %w", ns.Name, err)
	}

	var rivalClaim, rivalDerives string
	for i := range projects.Items {
		other := &projects.Items[i]
		if other.Name == project.Name {
			continue
		}
		for _, claim := range other.Status.NamespaceClaims {
			if claim.Name == ns.Name && claim.UID == ns.UID {
				rivalClaim = other.Name
			}
		}
		for _, env := range ProjectEnvironments(other) {
			if ResolveNamespace(other.Name, env.Name) == ns.Name {
				rivalDerives = other.Name
			}
		}
	}

	if rivalClaim != "" {
		return &namespaceConflictError{namespace: ns.Name, owner: rivalClaim, claimant: project.Name}
	}

	// Nobody holds it. If this project already does, it is ours and the
	// derivation below does not matter: a collision resolved once stays
	// resolved rather than reopening every reconcile.
	for _, claim := range project.Status.NamespaceClaims {
		if claim.Name == ns.Name && claim.UID == ns.UID {
			return nil
		}
	}

	if rivalDerives != "" {
		// A collision the previous release already settled is adopted rather
		// than reopened. Refusing both is the right answer for a new one, where
		// deciding by whoever reconciles first decides ownership by a race. It
		// is the wrong answer for a pair that has been running for months: the
		// holder would be refused the namespace it is already in, no claim would
		// be written, the namespace would drop out of its own record, and once
		// the claim is what resolves ownership its members could not reach it.
		// The upgrade would break a cluster that worked.
		//
		// Adoption takes the evidence the previous release left, and takes both
		// halves of it. The label says which project this namespace answered to,
		// and this project's own record says it held it. A relabel supplies the
		// first and cannot supply the second, so the label alone still adopts
		// nothing. An object with no UID is the create path below, where nothing
		// exists yet and there is no settled anything to adopt.
		//
		// EverHeld is the cleanup rule rather than the resolver's, and for the
		// same reason: what is being decided is whether this project ever took
		// the name, not whether it holds the object it took last time. A
		// namespace recreated under a contested name it already held is still
		// its own.
		settled := ns.UID != "" &&
			ns.Labels[kipperlabels.Project] == project.Name &&
			nsowner.EverHeld(project.Status, ns.Name, ns.UID)
		if !settled {
			return &namespaceConflictError{namespace: ns.Name, owner: rivalDerives, claimant: project.Name}
		}
	}
	return nil
}

// publishClaim writes the claims recorded so far, without touching the rest of
// the status.
//
// The whole-status write at the end of the pass still happens and is still what
// prunes: this only ever adds a claim or repoints one at the object now
// carrying the name. A claim is worth recording the moment the
// namespace is proven this project's and isolated, because everything after
// that point can fail and the claim is what the add-environment flow waits on.
//
// A conflict is not an error. Another writer got there first, this pass will
// see their version on its next run, and the claim it wanted to add is
// derivable again from the same namespace.
func (r *ProjectReconciler) publishClaim(ctx context.Context, project *kipperv1.Project, ns string, claimed []kipperv1.NamespaceClaim) error {
	// From the API server, and the name says why: this writes status, so it has
	// to start from the status that is actually there. A cached copy carries a
	// superseded resourceVersion, and the conflict that follows is swallowed as
	// success, so the claim silently does not land.
	var live kipperv1.Project
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: project.Name}, &live); err != nil {
		return err
	}
	// And it has to be the same project. This deliberately replaces the pass's
	// copy with the live one before writing, so the ordinary resourceVersion
	// protection does not apply: it makes whatever now carries the name the
	// write target. A project deleted and recreated under this name would take
	// the old pass's claim as its own, and a claim authorises deletion
	// downstream. The delete path binds the same way.
	//
	// The mismatch ends the pass rather than skipping one write. Refusing the
	// claim and returning success leaves the caller to rewrite the successor's
	// member bindings from the dead project's member list, and to write its
	// quota and shared storage, all after the code has observed the
	// replacement.
	if live.UID != project.UID {
		return errProjectReplaced
	}
	claim := claimed[len(claimed)-1]
	replaced := false
	for i, existing := range live.Status.NamespaceClaims {
		if existing.Name != ns {
			continue
		}
		// A claim naming an object that is gone is worth nothing: the resolver
		// matches on the UID, so it answers "nobody owns this" until the claim
		// names the object that is actually there. Leaving it standing is what
		// the add-environment flow would then wait on.
		if existing.UID == claim.UID {
			return nil
		}
		live.Status.NamespaceClaims[i] = claim
		replaced = true
		break
	}
	if !replaced {
		live.Status.NamespaceClaims = append(live.Status.NamespaceClaims, claim)
	}
	if err := r.Status().Update(ctx, &live); err != nil {
		if errors.IsConflict(err) {
			return nil
		}
		return err
	}
	// The write moved the object on, so the copy this pass is still holding
	// carries a resourceVersion the API server has already superseded. Without
	// this the whole-status update at the end of the pass conflicts with a
	// change this same pass made.
	project.ResourceVersion = live.ResourceVersion
	project.Status.NamespaceClaims = live.Status.NamespaceClaims
	// The namespace list too, and for a reason the claims alone do not cover: a
	// pass one watch event behind its own previous write holds a stale list, and
	// the retention merge starts from what the pass holds. Syncing only the
	// resourceVersion would let that stale list go back over a newer one with
	// nothing to conflict against.
	project.Status.Namespaces = live.Status.Namespaces
	return nil
}
