package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func abandonedClaim(age time.Duration) *kipperv1.WorkloadName {
	return &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout", Namespace: "shop-prod", UID: "claim-uid",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
		},
		Spec: kipperv1.WorkloadNameSpec{Kind: "app"},
	}
}

func claimRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}}
}

// A client that dies between reserving a name and writing the workload leaves a
// reservation with no owner and no workload. Nothing else can clear it:
// garbage collection needs an owner, and the workload controllers only ever see
// workloads that exist. Left alone it refuses the name to every other kind for
// ever.
func TestWorkloadNameReconcile_ReleasesAReservationNoWorkloadHolds(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(abandonedClaim(claimGrace + time.Minute)).Build()
	r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), claimRequest())
	require.NoError(t, err)

	var claim kipperv1.WorkloadName
	err = c.Get(context.Background(), claimRequest().NamespacedName, &claim)
	assert.True(t, apierrors.IsNotFound(err), "the abandoned reservation still holds the name")
}

// A workload that exists is simply waiting for its own controller to adopt the
// reservation, which is a pass away. The name is not free.
func TestWorkloadNameReconcile_KeepsAReservationWhoseWorkloadExists(t *testing.T) {
	scheme := testScheme()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "checkout:v1", Port: 8080},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(abandonedClaim(claimGrace+time.Minute), app).Build()
	r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), claimRequest())
	require.NoError(t, err)

	var claim kipperv1.WorkloadName
	require.NoError(t, c.Get(context.Background(), claimRequest().NamespacedName, &claim),
		"a reservation whose workload exists was released")
}

// Deleting a reservation whose workload is merely slow would hand the name away
// while its rightful owner was still starting.
func TestWorkloadNameReconcile_WaitsOutTheGracePeriod(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(abandonedClaim(time.Minute)).Build()
	r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

	res, err := r.Reconcile(context.Background(), claimRequest())
	require.NoError(t, err)
	assert.Positive(t, res.RequeueAfter, "a young reservation must be looked at again, not dropped")

	var claim kipperv1.WorkloadName
	require.NoError(t, c.Get(context.Background(), claimRequest().NamespacedName, &claim),
		"a reservation was released before its workload had a chance to appear")
}

// An owned reservation dies with its workload through garbage collection, so
// this controller has no business touching it.
func TestWorkloadNameReconcile_LeavesAnOwnedReservationAlone(t *testing.T) {
	scheme := testScheme()
	owned := abandonedClaim(claimGrace + time.Minute)
	controller := true
	owned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
		Name: "checkout", UID: "app-uid", Controller: &controller,
	}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(owned).Build()
	r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), claimRequest())
	require.NoError(t, err)

	var claim kipperv1.WorkloadName
	require.NoError(t, c.Get(context.Background(), claimRequest().NamespacedName, &claim),
		"an owned reservation was released behind garbage collection's back")
}

// Version skew: a console-api that predates a fourth workload kind must not
// reclaim reservations it cannot understand. Refusing to act on a kind this
// build does not know is what keeps an older replica from deleting a newer
// one's names during a rollout.
func TestWorkloadNameReconcile_LeavesAKindThisBuildDoesNotKnow(t *testing.T) {
	scheme := testScheme()
	unknown := abandonedClaim(claimGrace + time.Minute)
	unknown.Spec.Kind = "app" // written below, because the CRD enum rejects others
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(unknown).Build()
	r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

	// The enum keeps an unknown kind out of the API, so the branch is exercised
	// where it actually lives.
	held, err := r.workloadExists(context.Background(), "quantumfunction", claimRequest().NamespacedName)
	require.NoError(t, err)
	assert.True(t, held,
		"a reservation of a kind this build does not know was treated as free to delete")
}

// Every kind's arm has to resolve to that kind's type, or reclamation silently
// stops working for it while the tests stay green.
func TestWorkloadNameReconcile_FindsEveryWorkloadKind(t *testing.T) {
	scheme := testScheme()
	key := claimRequest().NamespacedName
	meta := metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}

	for kind, obj := range map[string]crclient.Object{
		"app":      &kipperv1.App{ObjectMeta: meta, Spec: kipperv1.AppSpec{Image: "x:1", Port: 8080}},
		"function": &kipperv1.Function{ObjectMeta: meta, Spec: kipperv1.FunctionSpec{Image: "x:1", Port: 8080}},
		"job":      &kipperv1.Job{ObjectMeta: meta, Spec: kipperv1.JobSpec{Image: "x:1"}},
	} {
		t.Run(kind, func(t *testing.T) {
			c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build()
			r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

			held, err := r.workloadExists(context.Background(), kind, key)
			require.NoError(t, err)
			assert.True(t, held, "reclamation cannot see a %s, so it would free its name", kind)
		})
	}
}

// The preconditions are what keep reclamation from acting on what it read. A
// claim adopted or replaced between the check and the delete must survive.
func TestWorkloadNameReconcile_DeletesOnlyTheClaimItInspected(t *testing.T) {
	scheme := testScheme()
	claim := abandonedClaim(claimGrace + time.Minute)
	var saw crclient.Preconditions

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				for _, o := range opts {
					if p, ok := o.(crclient.Preconditions); ok {
						saw = p
					}
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &WorkloadNameReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), claimRequest())
	require.NoError(t, err)

	require.NotNil(t, saw.UID, "the delete carried no uid, so it can remove a replacement")
	require.NotNil(t, saw.ResourceVersion,
		"the delete carried no resourceVersion, so it can remove a claim adopted since the check")
}
