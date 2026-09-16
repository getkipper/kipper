// Package workloadname reserves workload names, for every console-api path that
// creates one.
package workloadname

import (
	"context"
	goerrors "errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

// objectFor returns an empty object of a competing workload kind to read into.
func objectFor(kind string) crclient.Object {
	switch kind {
	case "app":
		return &kipperv1.App{}
	case "function":
		return &kipperv1.Function{}
	default:
		return &kipperv1.Job{}
	}
}

// Claim reserves name in namespace for a workload of kind creating.
//
// The reservation is the create: a WorkloadName is named after the workload, so
// exactly one caller creates it and everyone else is told it already exists.
// That is what makes this an invariant rather than a check — two creates racing
// each other cannot both win, where two reads racing each other both pass.
//
// A claim this kind already holds is ours, so an upsert or a re-apply proceeds.
// held reports whether the claim was already ours, which tells the caller not to
// release it if the workload write then fails.
func Claim(ctx context.Context, c crclient.Client, namespace, name, creating string) (held bool, uid types.UID, err error) {
	claim := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       kipperv1.WorkloadNameSpec{Kind: creating},
	}
	err = c.Create(ctx, claim)
	switch {
	case err == nil:
		return false, claim.UID, nil
	case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
		// No such resource on this cluster, rather than no such object.
		return false, "", workload.ClaimUnavailableError{Err: err}
	case !apierrors.IsAlreadyExists(err):
		return false, "", fmt.Errorf("reserving the name %q: %w", name, err)
	}

	var existing kipperv1.WorkloadName
	if getErr := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &existing); getErr != nil {
		return false, "", fmt.Errorf("reading the reservation for %q: %w", name, getErr)
	}
	if existing.Spec.Kind == creating {
		return true, existing.UID, nil
	}
	return false, "", workload.NameTakenError{Name: name, Kind: existing.Spec.Kind}
}

// Release rolls back only the reservation with the caller's UID, using a
// detached, time-limited context so cancellation still permits cleanup.
// Deletion errors leave a reservation requiring later cleanup.
func Release(ctx context.Context, c crclient.Client, namespace, name string, uid types.UID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = c.Delete(ctx, &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}, crclient.Preconditions{UID: &uid})
}

// EnsureFree checks competing workload kinds and permits a same-kind redeploy
// when that workload is incumbent. Reserve also acquires a WorkloadName claim;
// on clusters lacking that resource, this read-only check leaves a race between
// concurrent creates of different kinds.
func EnsureFree(ctx context.Context, c crclient.Client, namespace, name, creating string) error {
	key := types.NamespacedName{Name: name, Namespace: namespace}

	mine := objectFor(creating)
	held := false
	switch err := c.Get(ctx, key, mine); {
	case err == nil:
		held = true
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("checking whether the name %q is free: %w", name, err)
	}

	for _, kind := range workload.Kinds {
		if kind == creating {
			continue
		}
		other := objectFor(kind)
		switch err := c.Get(ctx, key, other); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("checking whether the name %q is free: %w", name, err)
		}
		// A name this kind already holds is normally its own, since every caller
		// here upserts. On a cluster that upgraded into an existing collision
		// neither side holds a reservation yet, and then the incumbent is
		// whichever workload is older — the same rule the controllers use to
		// settle it, so an ordinary re-apply of the newer one cannot take a name
		// the controllers would award to the older.
		if held && !workload.Incumbent(kind, other.GetCreationTimestamp().Time, creating, mine.GetCreationTimestamp().Time) {
			continue
		}
		return workload.NameTakenError{Name: name, Kind: kind}
	}
	return nil
}

// Reserve takes the name for a workload of kind creating.
//
// Both halves run, because they cover different gaps. The lookup catches a
// workload that predates reservations and so holds no claim, which is every
// workload on a cluster the moment it upgrades. The claim catches the race the
// lookup cannot: two creates of different kinds that both read before either
// writes. A cluster with no WorkloadName resource gets the lookup alone, which
// is the behaviour it already had.
//
// It returns a release function the caller runs when the workload write fails,
// so a name is not parked by a create that never happened. The function is a
// no-op when this call reserved nothing.
func Reserve(ctx context.Context, c crclient.Client, namespace, name, creating string) (release func(), err error) {
	held, err := heldBySameKind(ctx, c, namespace, name, creating)
	if err != nil {
		return func() {}, err
	}
	if err := EnsureFree(ctx, c, namespace, name, creating); err != nil {
		return func() {}, err
	}

	mine, uid, err := Claim(ctx, c, namespace, name, creating)
	switch {
	case err == nil:
		// Nothing to roll back when the claim was already this kind's, and
		// nothing that may be rolled back when the workload itself is already
		// there: the claim just made is that workload's own backfill, and
		// releasing it would undo the conversion an upgraded cluster depends on
		// while leaving the workload in place.
		if mine || held {
			return func() {}, nil
		}
		return func() { Release(ctx, c, namespace, name, uid) }, nil
	case isClaimUnavailable(err):
		return func() {}, nil
	default:
		return func() {}, err
	}
}

// isClaimUnavailable reports whether err says the cluster has no WorkloadName
// resource, rather than that the reservation failed.
func isClaimUnavailable(err error) bool {
	var unavailable workload.ClaimUnavailableError
	return goerrors.As(err, &unavailable)
}

// heldBySameKind reports whether the workload this reservation is for already
// exists, which decides whether a rollback may remove the reservation.
func heldBySameKind(ctx context.Context, c crclient.Client, namespace, name, creating string) (bool, error) {
	switch err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, objectFor(creating)); {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("checking whether %q already exists: %w", name, err)
	}
}
