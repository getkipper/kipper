package controllers

import (
	"context"
	goerrors "errors"
	"fmt"
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
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/projectenv"
)

const projectFinalizer = kipperlabels.FinalizerProject

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
		if err := r.deleteProjectNamespaces(ctx, &project, nil); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting project namespaces: %w", err)
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

		if err := r.reconcileNamespace(ctx, &project, pl.ns, pl.env.Name, envNames, pl.order); err != nil {
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

		// The claim is recorded here rather than at the end of the pass. Later
		// than the egress policy, because a namespace is not this project's to
		// hand out until it is isolated; earlier than everything else, because
		// a claim written only after quota, bindings and pruning had all
		// succeeded would leave the namespace unclaimed whenever any of them
		// failed, and the add-environment flow waits on the claim.
		if uid := nsUID(ctx, r, pl.ns); uid != "" {
			claimed = append(claimed, kipperv1.NamespaceClaim{Name: pl.ns, UID: uid})
			// Published now rather than with the rest of the status at the end
			// of the pass. Quota, bindings, shared storage and pruning all come
			// after this point and any of them can fail, and a claim written
			// only once they had all succeeded would leave the namespace
			// unclaimed for as long as one of them kept failing. The
			// add-environment flow waits on the claim, so that is a hang rather
			// than a delay.
			if err := r.publishClaim(ctx, &project, pl.ns, claimed); err != nil {
				return ctrl.Result{}, fmt.Errorf("recording the claim to %s: %w", pl.ns, err)
			}
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

	if err := r.deleteProjectNamespaces(ctx, &project, envNames); err != nil {
		return ctrl.Result{}, fmt.Errorf("pruning removed environments: %w", err)
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
	project.Status.Namespaces = namespaces
	// What was projected, and from which generation. Written now and read a
	// release later, once every pod carries the field: an older pod's
	// whole-status write drops what its struct does not know, so a build that
	// trusted this today would be trusting a gap.
	project.Status.NamespaceClaims = claimed
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
// carrying its label, plus any it recorded in status that have since lost their
// label altogether.
//
// The label is the record and the status is the backstop. A namespace whose
// label was removed is invisible to the label query, so deleting the project
// would remove the finalizer and leave that namespace standing with its
// workloads and its member bindings and nothing left to collect them — the
// Project is cluster-scoped, so no owner reference reaches down to them.
//
// A namespace whose label now names a different project is left out. It answers
// to them, and taking it on the strength of this project's own stale status
// would delete a live namespace out from under a project that legitimately
// holds it.
//
// Status is the only extra source, because it is the only record that this
// project ever managed the namespace: the reconcile writes a namespace there
// once it has held it as its own. Deriving candidates from the spec instead
// would name namespaces the project asked for but was refused — a pre-existing
// unlabelled "blog" that reconcileNamespace declines to adopt would be deleted
// along with a project called "blog" that never owned it.
//
// The reach is one reconcile: the pass that finds a namespace unlabelled
// reports a conflict and leaves it out of status, so the record of it is gone
// by the next pass and only a label restore brings it back. Recovering from
// that needs an inventory the reconcile does not rewrite, which is a wider
// change than this one — see plans/link-eod-fix-plan-2026-07-29.md.
func (r *ProjectReconciler) ownedNamespaces(ctx context.Context, project *kipperv1.Project) ([]corev1.Namespace, error) {
	var labelled corev1.NamespaceList
	if err := r.List(ctx, &labelled, client.MatchingLabels{kipperlabels.Project: project.Name}); err != nil {
		return nil, err
	}

	owned := labelled.Items
	seen := make(map[string]struct{}, len(owned))
	for _, ns := range owned {
		seen[ns.Name] = struct{}{}
	}

	for _, recorded := range project.Status.Namespaces {
		if _, ok := seen[recorded]; ok {
			continue
		}
		var ns corev1.Namespace
		switch err := r.Get(ctx, types.NamespacedName{Name: recorded}, &ns); {
		case errors.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("reading namespace %s: %w", recorded, err)
		}
		if ns.Labels[kipperlabels.Project] != "" {
			continue
		}
		owned = append(owned, ns)
		seen[ns.Name] = struct{}{}
	}
	return owned, nil
}

// deleteProjectNamespaces deletes the namespaces this project owns that are not
// listed in keepEnvs. Pass a nil keepEnvs to delete every one of them (used
// during project deletion).
func (r *ProjectReconciler) deleteProjectNamespaces(ctx context.Context, project *kipperv1.Project, keepEnvs []string) error {
	// Keep by namespace rather than by environment name. Working an environment
	// name back out of a namespace has to guess which spelling of the default
	// one the project used — "" and "default" both resolve to the bare project
	// name — and guessing wrong here deletes a live namespace. Resolving the
	// declared names forwards asks the same rule that created them.
	keep := make(map[string]struct{}, len(keepEnvs))
	for _, env := range keepEnvs {
		keep[ResolveNamespace(project.Name, env)] = struct{}{}
	}

	namespaces, err := r.ownedNamespaces(ctx, project)
	if err != nil {
		return err
	}

	for _, ns := range namespaces {
		if _, ok := keep[ns.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, &ns); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting namespace %s: %w", ns.Name, err)
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
	return nil
}

// reconcileNamespace creates or updates the namespace for one environment.
//
// envName is the environment as the project declares it, passed in rather than
// worked back out of the namespace name. Deriving it looked right for a
// suffixed environment and was wrong for the default one, whose namespace is
// the bare project name and so has no suffix to strip: the label came out as
// the project's name. Everything that reads the label back to build a hostname
// then disagreed with everything that used the declared name.
func (r *ProjectReconciler) reconcileNamespace(ctx context.Context, project *kipperv1.Project, ns, envName string, envNames []string, order int) error {

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

	var existing corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: ns}, &existing)
	if errors.IsNotFound(err) {
		// Checked before creating, not only before adopting. Two projects that
		// resolve to one name and find it absent would otherwise both create
		// it, and whichever lost the race would carry on as though it had not:
		// ownership decided by scheduling, which is the thing the collision
		// refusal exists to prevent.
		if err := r.claimable(ctx, project, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return err
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
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
		return err
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
		return &namespaceConflictError{namespace: ns, owner: owner, claimant: project.Name}
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
	return r.Update(ctx, &existing)
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
		return fmt.Sprintf("namespace %s already exists and was not created by Kipper, so project %s "+
			"will not take it over; whatever is running there would become visible to this project's "+
			"members. Rename the project, or remove the namespace if it is genuinely unused",
			e.namespace, e.claimant)
	}
	return fmt.Sprintf("namespace %s already belongs to project %s, so project %s cannot use it; "+
		"the two projects resolve to the same namespace name and one of them has to be renamed",
		e.namespace, e.owner, e.claimant)
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
			others = append(others, conflict.namespace)
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
	var projects kipperv1.ProjectList
	if err := r.List(ctx, &projects); err != nil {
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
		return &namespaceConflictError{namespace: ns.Name, owner: rivalDerives, claimant: project.Name}
	}
	return nil
}

// nsUID reads the namespace's UID, which is what makes a claim about an object
// rather than about a name.
//
// An unreadable namespace yields an empty UID and the caller records no claim
// at all. A claim carrying an empty UID would be worse than none: it matches no
// live namespace, so the project would not recognise its own, while still
// reading as a claim to anything that looks at the name alone.
func nsUID(ctx context.Context, r *ProjectReconciler, name string) types.UID {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		return ""
	}
	return ns.UID
}

// publishClaim writes the claims recorded so far, without touching the rest of
// the status.
//
// The whole-status write at the end of the pass still happens and is still what
// prunes: this only ever adds. A claim is worth recording the moment the
// namespace is proven this project's and isolated, because everything after
// that point can fail and the claim is what the add-environment flow waits on.
//
// A conflict is not an error. Another writer got there first, this pass will
// see their version on its next run, and the claim it wanted to add is
// derivable again from the same namespace.
func (r *ProjectReconciler) publishClaim(ctx context.Context, project *kipperv1.Project, ns string, claimed []kipperv1.NamespaceClaim) error {
	var live kipperv1.Project
	if err := r.Get(ctx, types.NamespacedName{Name: project.Name}, &live); err != nil {
		return err
	}
	for _, existing := range live.Status.NamespaceClaims {
		if existing.Name == ns {
			return nil
		}
	}
	live.Status.NamespaceClaims = append(live.Status.NamespaceClaims, claimed[len(claimed)-1])
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
	return nil
}
