package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/datavolume"
)

// DataFinalizer holds a deleting Service until its data has been destroyed.
//
// It is separate from ServiceFinalizer, and separate on purpose. During a
// rolling upgrade a request can reach this build's handler while the previous
// build's controller still holds the lease, and that controller knows
// ServiceFinalizer: it would finish its own cleanup, take the finalizer off, and
// let the service go with the volume still there and the console already saying
// the data was destroyed. A finalizer it has never heard of it leaves alone, so
// the service waits, visibly, for a controller that understands what was asked.
const DataFinalizer = "kipper.run/service-data"

// destroyDataInterval is how often the finalizer comes back to see whether the
// workload, and then the volume, has gone.
const destroyDataInterval = 2 * time.Second

// destroyData removes the volumes the service kept its data on, and reports
// whether that has finished.
//
// It runs across as many reconciles as it takes rather than blocking one: the
// workload has to stop before the volume can go, and a database pod takes as
// long as it takes. Nothing is carried between them, because while the
// finalizer is held the service's name cannot be taken by anything else.
func (r *ServiceReconciler) destroyData(ctx context.Context, svc *kipperv1.Service) (bool, error) {
	gone, err := r.workloadIsGone(ctx, svc)
	if err != nil || !gone {
		return false, err
	}

	var claims corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &claims,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{datavolume.LabelKey: svc.Name},
	); err != nil {
		return false, fmt.Errorf("listing the volumes of %s: %w", svc.Name, err)
	}

	destroyed := true
	for i := range claims.Items {
		claim := &claims.Items[i]
		if !datavolume.Belongs(svc.Name, claim.Name) {
			continue
		}
		// Still listed is still there, whether the delete has been asked for yet
		// or a finalizer is holding it while the pod that mounted it stops.
		destroyed = false
		if !claim.DeletionTimestamp.IsZero() {
			continue
		}
		// The UID is pinned so the API server refuses the call if the name has
		// come to stand for something else in the meantime.
		if err := r.Delete(ctx, claim, client.Preconditions{UID: &claim.UID}); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("deleting volume %s: %w", claim.Name, err)
		}
	}
	return destroyed, nil
}

// objectIsOurs refuses an object of this service's name that is somebody else's.
//
// Everything a service owns is found by name, so every path that writes to one
// has to ask this first. An owner settles it. Without one the management label
// has to, because a StatefulSet called db with a claim called data-db-0 is what
// anybody's database looks like, and the services made before these records
// existed have no owner but do carry the label.
//
// It runs before the first write and not only at deletion, because adopting an
// object is what stamps the label, and a label this stamped would then be the
// evidence that lets the delete destroy it.
func objectIsOurs(kind string, object metav1.Object, svc *kipperv1.Service) error {
	owner := metav1.GetControllerOf(object)
	switch {
	case owner != nil && owner.UID != svc.UID:
		return &nameTakenError{Kind: kind, Name: svc.Name, Namespace: svc.Namespace,
			Holder: fmt.Sprintf("%s %s", owner.Kind, owner.Name)}
	case owner == nil && object.GetLabels()[kipperLabel] != kipperValue:
		return &nameTakenError{Kind: kind, Name: svc.Name, Namespace: svc.Namespace}
	}
	return nil
}

// isOurs is objectIsOurs as a question, for callers that have somewhere else to
// go when the answer is no.
func isOurs(object metav1.Object, svc *kipperv1.Service) bool {
	return objectIsOurs("object", object, svc) == nil
}

// nameTakenError refuses an object of the service's name that belongs to
// something else. No retry clears it: the object is somebody's and this service
// has to be called something else, so it is one of the states an operator has to
// see rather than one the controller can work through.
type nameTakenError struct {
	Kind      string
	Name      string
	Namespace string
	Holder    string
}

func (e *nameTakenError) Error() string {
	if e.Holder != "" {
		return fmt.Sprintf("the %s named %s in %s belongs to %s, so it is not this service's; the service has to be called something else",
			e.Kind, e.Name, e.Namespace, e.Holder)
	}
	return fmt.Sprintf("the %s named %s in %s is not Kipper's; the service has to be called something else, or that object removed if it is a leftover",
		e.Kind, e.Name, e.Namespace)
}

// workloadIsGone deletes the service's StatefulSet and reports whether it has
// left.
//
// The volume cannot go first. The StatefulSet controller writes a claim back
// from its volumeClaimTemplate as soon as one is deleted, leaving a fresh empty
// volume and the data exactly where it was. The workload is deleted here rather
// than left to the owner reference, because that only fires once the CR is fully
// gone and this has to happen while the finalizer still holds it.
//
// Deleting by name is what the owner reference would not do, so the ownership it
// would have checked is checked here instead.
func (r *ServiceReconciler) workloadIsGone(ctx context.Context, svc *kipperv1.Service) (bool, error) {
	var workload appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &workload)
	switch {
	case errors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("reading the workload of %s: %w", svc.Name, err)
	}

	if err := objectIsOurs("workload", &workload, svc); err != nil {
		return false, err
	}

	if workload.DeletionTimestamp.IsZero() {
		uid := workload.UID
		err := r.Delete(ctx, &workload, client.Preconditions{UID: &uid})
		if err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("deleting the workload of %s: %w", svc.Name, err)
		}
	}
	return false, nil
}
