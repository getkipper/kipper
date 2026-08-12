package controllers

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

// reconcileNameClaim keeps the workload's name reservation in step with the
// workload itself.
//
// It backfills, and it releases. Backfills, because a claim only exists for a
// workload created through a path that makes one: everything that predates
// reservations holds none, and a migration restore writes CRs directly. Until
// the claim exists the name is guarded by the weaker lookup, so making it here
// is what converts a cluster to the invariant as its workloads reconcile.
// Releases, because the claim is owned by the workload, so deleting the
// workload garbage-collects the claim and frees the name.
//
// A claim another kind holds is left alone. That collision is real and the
// caller is about to refuse the child objects over it; taking the claim would
// only hide which workload got there first.
//
// Failing to establish the reservation fails the pass. Not knowing whether the
// name is this workload's is not permission to use it, and a controller that
// treated a transient API error as ownership would build children under a name
// somebody else may hold. Only two things are not errors: a cluster with no
// WorkloadName resource at all, and losing the create to a claim that turns out
// to be this workload's own.
//
// It returns the kind holding the name when that is not this workload's, and
// the caller must then stop: a CR written straight to the API server (GitOps,
// kubectl, a restore) never passed a reservation, so the loser of that race is
// the only thing standing between a collision and two live workloads. An App and
// a Job do not contend on a child object, so nothing further down would refuse
// them, and the App's Service selects `app=<name>`, which the Job's pods carry.
func reconcileNameClaim(ctx context.Context, c client.Client, uncached client.Reader, scheme *runtime.Scheme, owner client.Object, kind string) (heldBy string, err error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Name: owner.GetName(), Namespace: owner.GetNamespace()}

	var claim kipperv1.WorkloadName
	err = c.Get(ctx, key, &claim)
	switch {
	case meta.IsNoMatchError(err):
		// console-api ships with its own CRDs and its reservation reconciler
		// watches this kind, so a cluster without it never gets as far as a
		// reconcile. Handling it is belt and braces rather than a supported
		// path: kip is the client that meets clusters it did not install, and
		// its own fallback is where that case is actually answered.
		return "", nil
	case apierrors.IsNotFound(err):
		// No reservation exists, which is every workload on a cluster the
		// moment it upgrades. Whoever reconciles first would otherwise take the
		// name, and controller scheduling order is not evidence of who held it:
		// a Job written straight to the API server could take a live App's name
		// simply by reconciling before it, and the App's refusal afterwards
		// would not remove the children it already has.

		if holder, holderErr := olderHolder(ctx, uncached, key, kind, owner); holderErr != nil {
			return "", holderErr
		} else if holder != "" {
			return holder, nil
		}

		claim = kipperv1.WorkloadName{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec:       kipperv1.WorkloadNameSpec{Kind: kind},
		}
		if refErr := controllerutil.SetControllerReference(owner, &claim, scheme); refErr != nil {
			return "", fmt.Errorf("owning the name reservation: %w", refErr)
		}
		createErr := c.Create(ctx, &claim)
		switch {
		case createErr == nil || meta.IsNoMatchError(createErr):
			return "", nil
		case !apierrors.IsAlreadyExists(createErr):
			// Failing open here would let a workload build its children without
			// ever establishing that the name is its own, which is the thing
			// this exists to prevent. An error stops the pass and retries.
			return "", fmt.Errorf("reserving the workload name: %w", createErr)
		}

		// AlreadyExists says somebody else won, not that this workload may
		// carry on. Both reconcilers read the claim as absent — a true race, or
		// the ordinary lag of the cache this Get reads — and only the create
		// settles it. Treating the loss as success is what would let both
		// workloads go live.
		// Read uncached, because the cache is exactly what cannot answer this.
		// A handler reserves the name with the direct client and then writes the
		// workload, so on a workload's first pass the claim it just lost to is
		// very often its own, and a cached read that has not caught up would
		// report no holder at all. Guessing there marked healthy workloads
		// failed for holding their own name.
		var winner kipperv1.WorkloadName
		if getErr := uncached.Get(ctx, key, &winner); getErr != nil {
			return "", fmt.Errorf("reading the workload holding this name: %w", getErr)
		}
		if winner.Spec.Kind == kind {
			return "", nil
		}
		return winner.Spec.Kind, nil
	case err != nil:
		// Same reason: not knowing who holds the name is not permission to use it.
		return "", fmt.Errorf("reading the workload name reservation: %w", err)
	case claim.Spec.Kind != kind:
		return claim.Spec.Kind, nil
	}

	// An existing claim of this kind may carry no owner reference at all: a
	// client reserves the name before writing the workload and never sets one,
	// so this is where every reservation acquires its owner. A restored claim
	// carries a reference to a UID from another cluster, which is the same
	// repair. Re-asserting it here is what makes the reservation die with its
	// workload.
	if metav1.IsControlledBy(&claim, owner) {
		return "", nil
	}
	if err := controllerutil.SetControllerReference(owner, &claim, scheme); err != nil {
		logger.Error(err, "adopting the name reservation", "workload", key.Name)
		return "", nil
	}
	// A failed adoption stops the pass. It is tempting to treat this as costing
	// the reservation nothing but its garbage collection, and that is wrong in
	// the case that matters: NotFound means reclamation deleted the claim
	// between the read above and here, so this workload now holds no
	// reservation at all and must not go on to build children. The next pass
	// creates the claim again, or finds that another kind won it.
	if err := c.Update(ctx, &claim); err != nil {
		return "", fmt.Errorf("adopting the name reservation: %w", err)
	}
	return "", nil
}

// objectOfKind returns an empty object of a workload kind to read into.
func objectOfKind(kind string) client.Object {
	switch kind {
	case "app":
		return &kipperv1.App{}
	case "function":
		return &kipperv1.Function{}
	default:
		return &kipperv1.Job{}
	}
}

// olderHolder returns the kind of another workload that already holds this name
// and has a better claim to it than owner, or "" when owner may take it.
//
// The rule itself is workload.Incumbent, shared with the client-side check, so
// a re-apply cannot award a name the controllers would award elsewhere.
func olderHolder(ctx context.Context, uncached client.Reader, key types.NamespacedName, kind string, owner client.Object) (string, error) {
	mine := owner.GetCreationTimestamp().Time
	for _, other := range workload.Kinds {
		if other == kind {
			continue
		}
		obj := objectOfKind(other)
		switch err := uncached.Get(ctx, key, obj); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return "", fmt.Errorf("checking whether %s holds this name: %w", workload.NameTakenError{Name: key.Name, Kind: other}.Holder(), err)
		}
		if workload.Incumbent(other, obj.GetCreationTimestamp().Time, kind, mine) {
			return other, nil
		}
	}
	return "", nil
}

// blockedMessage says a workload cannot have this name, and what that leaves
// behind.
//
// The second sentence is the part an operator upgrading into an existing
// collision needs. Kipper does not delete what a workload built before the rule
// arrived, because a minor version that tore down a running workload would be a
// worse failure than the collision. So the objects stay, and the message says
// so rather than reading as though stopping the reconcile had contained
// anything.
func blockedMessage(name, heldBy string) string {
	return workload.NameTakenError{Name: name, Kind: heldBy}.Error() +
		". This workload has stopped reconciling. Anything it already created keeps running until you delete or rename the workload"
}
