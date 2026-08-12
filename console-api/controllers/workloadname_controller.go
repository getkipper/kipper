package controllers

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// claimGrace is how long a reservation may stand without a workload before it
// is treated as abandoned.
//
// It only has to outlast the gap between reserving a name and writing the
// workload, which is one API call, plus the time a fresh workload takes to
// reach its controller and be adopted. Minutes rather than seconds, because
// deleting a reservation whose workload is merely slow would hand the name to
// somebody else while its rightful owner was still starting.
const claimGrace = 5 * time.Minute

// WorkloadNameReconciler reclaims workload name reservations that no workload
// holds.
//
// A reservation is created before the workload it is for, so a client that dies
// in between leaves one behind with no owner reference and no workload. Nothing
// else can clear it: garbage collection needs an owner, and the workload
// controllers only ever see workloads that exist. Left alone it is a permanent
// false owner, refusing the name to every other kind for ever, and the only
// remedy would be an administrator deleting an internal object by hand.
type WorkloadNameReconciler struct {
	client.Client
	// APIReader bypasses the cache for the one read that decides whether a name
	// is free. A cached read that has not caught up would report a workload
	// absent when it exists, and this is the one place that answer costs a
	// workload its name.
	APIReader client.Reader
	Scheme    *runtime.Scheme
}

// workloadReader returns the reader the existence check uses, preferring the
// uncached one.
func (r *WorkloadNameReconciler) workloadReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=kipper.run,resources=workloadnames,verbs=get;list;watch;delete

// Reconcile deletes a reservation that has outlived its grace period with no
// owner and no workload of the kind it names.
//
// Both conditions matter. An owner reference means the workload exists and
// garbage collection already covers it. A workload that exists without one is
// simply waiting for its own controller to adopt the reservation, which is a
// pass away, so the name is not free.
func (r *WorkloadNameReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var claim kipperv1.WorkloadName
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if len(claim.OwnerReferences) > 0 {
		return ctrl.Result{}, nil
	}

	if held := time.Since(claim.CreationTimestamp.Time); held < claimGrace {
		return ctrl.Result{RequeueAfter: claimGrace - held}, nil
	}

	held, err := r.workloadExists(ctx, claim.Spec.Kind, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if held {
		// Its controller adopts it on its own pass; this one has nothing to do.
		return ctrl.Result{}, nil
	}

	log.FromContext(ctx).Info("releasing a reservation no workload holds",
		"name", claim.Name, "namespace", claim.Namespace, "kind", claim.Spec.Kind)
	// Both preconditions, because they answer different questions. The uid stops
	// this from deleting a reservation that was replaced under the same name.
	// The resourceVersion stops it from acting on what it read: anything that
	// touched the claim since — an owner reference added by the workload that
	// turned up between the check and here, most of all — makes the delete fail
	// and the next pass see the truth.
	if err := r.Delete(ctx, &claim, client.Preconditions{
		UID:             &claim.UID,
		ResourceVersion: &claim.ResourceVersion,
	}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// workloadExists reports whether the workload a reservation names is present.
func (r *WorkloadNameReconciler) workloadExists(ctx context.Context, kind string, key types.NamespacedName) (bool, error) {
	var obj client.Object
	switch kind {
	case "app":
		obj = &kipperv1.App{}
	case "function":
		obj = &kipperv1.Function{}
	case "job":
		obj = &kipperv1.Job{}
	default:
		// A kind this build does not know. Refusing to act is the safe answer:
		// a reservation held by something newer is not this controller's to
		// release.
		return true, nil
	}

	err := r.workloadReader().Get(ctx, key, obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// SetupWithManager registers the reconciler.
func (r *WorkloadNameReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.WorkloadName{}).
		Complete(r)
}
