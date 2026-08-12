package workload

import (
	"context"
	goerrors "errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/workload"
)

// ClaimGVR is the WorkloadName resource that reservations are made in.
var ClaimGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "workloadnames",
}

// claim reserves name in namespace for a workload of kind creating.
//
// The reservation is the create: a WorkloadName is named after the workload, so
// exactly one caller creates it and everyone else is told it already exists.
// Reading the other kinds cannot do this, because two creates racing each other
// both read first.
//
// held reports whether the claim was already this kind's, which tells the caller
// not to release it if the workload write then fails.
func claim(ctx context.Context, dyn dynamic.Interface, namespace, name, creating string) (held bool, uid types.UID, err error) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "WorkloadName",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec":       map[string]interface{}{"kind": creating},
	}}

	claims := dyn.Resource(ClaimGVR).Namespace(namespace)
	created, err := claims.Create(ctx, obj, metav1.CreateOptions{})
	switch {
	case err == nil:
		return false, created.GetUID(), nil
	case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
		// No such resource on this cluster, rather than no such object.
		return false, "", workload.ClaimUnavailableError{Err: err}
	case !apierrors.IsAlreadyExists(err):
		return false, "", fmt.Errorf("reserving the name %q: %w", name, err)
	}

	existing, getErr := claims.Get(ctx, name, metav1.GetOptions{})
	if getErr != nil {
		return false, "", fmt.Errorf("reading the reservation for %q: %w", name, getErr)
	}
	holder, _, _ := unstructured.NestedString(existing.Object, "spec", "kind")
	if holder == creating {
		return true, existing.GetUID(), nil
	}
	return false, "", workload.NameTakenError{Name: name, Kind: holder}
}

// Reserve takes the name for a workload of kind creating.
//
// Both halves run, because they cover different gaps. EnsureNameFree catches a
// workload that predates reservations and so holds no claim, which is every
// workload on a cluster the moment it upgrades. The claim catches the race the
// lookup cannot: two creates of different kinds that both read before either
// writes. A cluster with no WorkloadName resource gets the lookup alone, which
// is the behaviour it already had.
//
// It returns a release function the caller runs when the workload write fails,
// so a name is not parked by a create that never happened.
func Reserve(ctx context.Context, dyn dynamic.Interface, namespace, name, creating string) (release func(), err error) {
	held, err := heldBySameKind(ctx, dyn, namespace, name, creating)
	if err != nil {
		return func() {}, err
	}
	if err := EnsureNameFree(ctx, dyn, namespace, name, creating); err != nil {
		return func() {}, err
	}

	mine, uid, err := claim(ctx, dyn, namespace, name, creating)
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
		return func() { releaseClaim(ctx, dyn, namespace, name, uid) }, nil
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

// releaseClaim drops a reservation this caller made when the workload it was for
// could not be written.
//
// The delete is conditional on the uid this caller created, so it can only ever
// remove its own reservation: one deleted out of band and re-made by somebody
// else in the meantime would otherwise be deleted by this rollback, handing the
// name away while its new holder was still writing.
//
// It runs on a context detached from the caller's, because the usual reason a
// workload write failed is that the command was interrupted, and a rollback on
// that same context does nothing at all. A delete that fails leaves the
// follow-up case where a name is parked until someone removes it.
func releaseClaim(ctx context.Context, dyn dynamic.Interface, namespace, name string, uid types.UID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = dyn.Resource(ClaimGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
}

// heldBySameKind reports whether the workload this reservation is for already
// exists, which decides whether a rollback may remove the reservation.
func heldBySameKind(ctx context.Context, dyn dynamic.Interface, namespace, name, creating string) (bool, error) {
	switch _, err := dyn.Resource(gvrByKind[creating]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("checking whether %q already exists: %w", name, err)
	}
}
