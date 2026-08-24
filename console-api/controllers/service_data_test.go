package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/controller/pkg/datavolume"
)

func deletingService(annotations map[string]string) *kipperv1.Service {
	now := metav1.Now()
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test",
			DeletionTimestamp: &now,
			Finalizers:        []string{ServiceFinalizer},
			Annotations:       annotations,
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
}

func serviceWorkload(finalizers ...string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop-test", Finalizers: finalizers,
		Labels: map[string]string{kipperLabel: kipperValue},
	}}
}

func serviceClaim(name string, finalizers ...string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "shop-test", UID: types.UID(name),
		Labels:     map[string]string{"app": "db"},
		Finalizers: finalizers,
	}}
}

func reconcilerOver(objects ...crclient.Object) (*ServiceReconciler, crclient.Client) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithStatusSubresource(&kipperv1.Service{}).Build()
	return &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}, c
}

func finalize(t *testing.T, r *ServiceReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.NoError(t, err)
	return result
}

func serviceIsGone(t *testing.T, c crclient.Client) bool {
	t.Helper()
	var svc kipperv1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Name: "db", Namespace: "shop-test"}, &svc); err != nil {
		return true
	}
	for _, held := range svc.Finalizers {
		if held == ServiceFinalizer {
			return false
		}
	}
	return true
}

func claimIsThere(t *testing.T, c crclient.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "shop-test"}, &corev1.PersistentVolumeClaim{})
	return err == nil
}

// The console asks for the data to go by marking the service before deleting
// it, because the volume outlives the CR: nothing owns a claim a StatefulSet
// built from its template.
func TestServiceFinalizer_DestroysTheDataWhenAsked(t *testing.T) {
	mine := serviceClaim("data-db-0")
	borrowed := serviceClaim("db-uploads")
	other := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-cache-0", Namespace: "shop-test", Labels: map[string]string{"app": "cache"},
	}}
	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		serviceWorkload(), mine, borrowed, other,
	)

	for range 5 {
		if serviceIsGone(t, c) {
			break
		}
		finalize(t, r)
	}

	assert.True(t, serviceIsGone(t, c), "the finalizer was never released, so the service cannot finish deleting")
	assert.False(t, claimIsThere(t, c, "data-db-0"), "the volume survived a delete that asked for it to go")
	assert.True(t, claimIsThere(t, c, "db-uploads"), "a volume that only shares the label was destroyed")
	assert.True(t, claimIsThere(t, c, "data-cache-0"), "another service's volume was destroyed")

	var workload appsv1.StatefulSet
	assert.Error(t,
		c.Get(context.Background(), types.NamespacedName{Name: "db", Namespace: "shop-test"}, &workload),
		"the workload has to go before the volume, or its template writes the claim straight back")
}

// The claim cannot go while the StatefulSet is there to write it back from its
// volumeClaimTemplate, so the finalizer waits rather than deleting a volume that
// reappears empty with the data still on disk.
func TestServiceFinalizer_LeavesTheVolumeWhileTheWorkloadIsThere(t *testing.T) {
	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		serviceWorkload("kipper.run/test-hold"),
		serviceClaim("data-db-0"),
	)

	result := finalize(t, r)

	assert.NotZero(t, result.RequeueAfter, "nothing will come back to finish the delete")
	assert.False(t, serviceIsGone(t, c), "the finalizer was released with the volume still to destroy")
	assert.True(t, claimIsThere(t, c, "data-db-0"), "the volume went while the workload was still there to write it back")
}

// A volume the API server has accepted the delete of is not a volume that has
// gone: a finalizer holds it while the pod that mounted it is terminating. The
// service stays until the data has actually left.
func TestServiceFinalizer_WaitsForTheVolumeToActuallyGo(t *testing.T) {
	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		serviceClaim("data-db-0", "kubernetes.io/pvc-protection"),
	)

	result := finalize(t, r)

	assert.NotZero(t, result.RequeueAfter, "nothing will come back to see the volume out")
	assert.False(t, serviceIsGone(t, c), "the service finished deleting while its volume was still there")
}

// Nobody asked, so nothing is destroyed. This is what kubectl and a GitOps
// apply do, and the volume they leave is what a service of the same name picks
// up later.
func TestServiceFinalizer_KeepsTheVolumeWhenNobodyAsked(t *testing.T) {
	r, c := reconcilerOver(deletingService(nil), serviceWorkload(), serviceClaim("data-db-0"))

	finalize(t, r)

	assert.True(t, serviceIsGone(t, c), "an ordinary delete must not be held up")
	assert.True(t, claimIsThere(t, c, "data-db-0"), "the data was destroyed without anybody asking for it")
}

// Deleting the workload by name is what the owner reference would not do, so a
// StatefulSet that belongs to something else must stop the delete rather than be
// taken with it. Its volumes carry the same label and the same name shape, so
// destroying it destroys another owner's data.
func TestServiceFinalizer_LeavesAWorkloadThatBelongsToSomethingElse(t *testing.T) {
	controller := true
	foreign := serviceWorkload()
	foreign.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
		Name: "db", UID: types.UID("some-other-owner"), Controller: &controller,
	}}
	svc := deletingService(map[string]string{datavolume.DeleteAnnotation: "true"})
	svc.UID = types.UID("the-service-being-deleted")

	r, c := reconcilerOver(svc, foreign, serviceClaim("data-db-0"))

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})

	require.Error(t, err, "a workload owned by something else was taken without a word")
	assert.False(t, serviceIsGone(t, c), "the finalizer was released over a collision nobody has looked at")
	assert.True(t, claimIsThere(t, c, "data-db-0"), "another owner's volume was destroyed")

	var still appsv1.StatefulSet
	assert.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &still),
		"another owner's workload was deleted")
}

// A workload from before Service records existed carries no owner at all. What
// says it is Kipper's is the management label, which those workloads do carry,
// and that is enough when nothing claims to own it.
func TestServiceFinalizer_TakesAWorkloadThatNobodyOwns(t *testing.T) {
	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		serviceWorkload(), serviceClaim("data-db-0"),
	)

	for range 5 {
		if serviceIsGone(t, c) {
			break
		}
		finalize(t, r)
	}

	assert.True(t, serviceIsGone(t, c), "a service whose workload predates the CRs could never finish deleting")
	assert.False(t, claimIsThere(t, c, "data-db-0"))
}

// The delete names the volume the reconcile found, and pins the object it found
// under that name. Without the precondition the API server has nothing to check
// the name against.
func TestServiceFinalizer_PinsWhatItDeletes(t *testing.T) {
	pinned := map[string]types.UID{}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(
			deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
			serviceWorkload(), serviceClaim("data-db-0"),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				var options crclient.DeleteOptions
				options.ApplyOptions(opts)
				if options.Preconditions != nil && options.Preconditions.UID != nil {
					pinned[obj.GetName()] = *options.Preconditions.UID
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	for range 5 {
		if serviceIsGone(t, c) {
			break
		}
		finalize(t, r)
	}

	assert.Equal(t, types.UID("data-db-0"), pinned["data-db-0"],
		"the volume was deleted by name alone, so a name that came to mean something else would go with it")
}

// A refused delete keeps the service, because the volume is still there and
// releasing the finalizer would say the data went when it did not.
func TestServiceFinalizer_KeepsTheServiceWhenTheVolumeWillNotGo(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(
			deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
			serviceClaim("data-db-0"),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ crclient.WithWatch, obj crclient.Object, _ ...crclient.DeleteOption) error {
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "persistentvolumeclaims"}, obj.GetName(),
					fmt.Errorf("the UID does not match"))
			},
		}).Build()
	r := &ServiceReconciler{Client: c, Scheme: testScheme(), ShareGrants: share.NewGrantStore(k8sfake.NewClientset())}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})

	require.Error(t, err, "a volume the API server would not delete was passed over in silence")
	assert.False(t, serviceIsGone(t, c), "the service finished deleting with its volume still there")
}

// Kipper's own rule is that a resource without the management label is not its
// to touch. An unowned StatefulSet that Kipper never made carries the same name
// and the same conventional claim name as one it did, and deleting it takes
// somebody else's database with it.
func TestServiceFinalizer_LeavesAWorkloadKipperNeverMade(t *testing.T) {
	foreign := serviceWorkload()
	foreign.Labels = nil

	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		foreign, serviceClaim("data-db-0"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})

	require.Error(t, err, "a workload Kipper never made was deleted on the strength of its name")
	assert.False(t, serviceIsGone(t, c), "the finalizer was released over a collision nobody has looked at")
	assert.True(t, claimIsThere(t, c, "data-db-0"), "somebody else's volume was destroyed")

	var still appsv1.StatefulSet
	assert.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &still),
		"somebody else's workload was deleted")
}

// A service that cannot finish leaving sits there deleting for good. Which step
// stopped it lives in the controller's log, which an operator watching the
// service refuse to go has no way to read, so it goes on the object.
func TestServiceFinalizer_SaysWhyItCannotFinish(t *testing.T) {
	foreign := serviceWorkload()
	foreign.Labels = nil
	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		foreign, serviceClaim("data-db-0"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.Error(t, err)

	var stuck kipperv1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &stuck))

	blocked := meta.FindStatusCondition(stuck.Status.Conditions, kipperv1.ConditionCleanupComplete)
	require.NotNil(t, blocked, "nothing on the service says why it will not go")
	assert.Equal(t, metav1.ConditionFalse, blocked.Status)
	assert.Equal(t, "DataNotDestroyed", blocked.Reason)
	assert.Contains(t, blocked.Message, "not Kipper's", "the message does not carry the cause")
}

// Taking a workload over is what stamps the label that later authorises
// destroying its volume, so the rule has to hold before the first write and not
// only at the end. A service that adopted somebody's StatefulSet would become a
// service allowed to delete their database.
func TestReconcileStatefulSet_LeavesAWorkloadKipperNeverMade(t *testing.T) {
	foreign := serviceWorkload()
	foreign.Labels = nil
	foreign.Spec.Template.Spec.Containers = []corev1.Container{{Name: "theirs", Image: "theirs:1"}}

	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test",
			Finalizers: []string{ServiceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	r, c := reconcilerOver(live, foreign)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.Error(t, err, "somebody else's workload was taken over on the strength of its name")

	var still appsv1.StatefulSet
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &still))
	require.Len(t, still.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "theirs", still.Spec.Template.Spec.Containers[0].Name,
		"somebody else's workload was rewritten to run a Kipper service")
	assert.NotEqual(t, kipperValue, still.Labels[kipperLabel],
		"the management label was stamped on a workload Kipper never made, which is what lets the delete destroy it")
}

// A step that failed once and then ran leaves a reason that is no longer true. A
// service waiting normally for its workload to stop would otherwise keep telling
// an operator it is blocked on something that cleared passes ago.
func TestServiceFinalizer_TakesBackAReasonThatHasCleared(t *testing.T) {
	stuck := deletingService(map[string]string{datavolume.DeleteAnnotation: "true"})
	stuck.Status.Conditions = []metav1.Condition{{
		Type: kipperv1.ConditionCleanupComplete, Status: metav1.ConditionFalse,
		Reason: "DataNotDestroyed", Message: "something that has since cleared",
		LastTransitionTime: metav1.Now(),
	}}
	// Still there, so the cleanup is waiting rather than failing.
	r, c := reconcilerOver(stuck, serviceWorkload("kipper.run/test-hold"), serviceClaim("data-db-0"))

	result := finalize(t, r)
	assert.NotZero(t, result.RequeueAfter, "the cleanup is not waiting, so the test proves nothing")

	var waiting kipperv1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &waiting))
	assert.Nil(t, meta.FindStatusCondition(waiting.Status.Conditions, kipperv1.ConditionCleanupComplete),
		"the service still reports a blockage that cleared")
}

// A service held only by the data finalizer is one this build's handler marked
// and no controller has reconciled. It has to be released here, or the finalizer
// that exists to keep the data safe keeps the service instead.
func TestServiceFinalizer_ReleasesAServiceHeldOnlyByTheDataFinalizer(t *testing.T) {
	held := deletingService(map[string]string{datavolume.DeleteAnnotation: "true"})
	held.Finalizers = []string{DataFinalizer}
	r, c := reconcilerOver(held, serviceWorkload(), serviceClaim("data-db-0"))

	for range 5 {
		var svc kipperv1.Service
		if err := c.Get(context.Background(),
			types.NamespacedName{Name: "db", Namespace: "shop-test"}, &svc); err != nil {
			break
		}
		finalize(t, r)
	}

	var gone kipperv1.Service
	assert.Error(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &gone),
		"the data finalizer was never released, so the service cannot finish deleting")
	assert.False(t, claimIsThere(t, c, "data-db-0"), "the volume survived")
}

// The address is adopted by name like the workload, and the update writes this
// service's labels onto whatever stands there. The management label among them
// is what a later delete reads as proof the object is Kipper's, so stamping it
// on somebody else's Service launders exactly the evidence the delete trusts.
func TestReconcileHeadlessService_LeavesAnAddressKipperNeverMade(t *testing.T) {
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "theirs", Port: 1234}},
		},
	}
	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test", Finalizers: []string{ServiceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	r, c := reconcilerOver(live, serviceWorkload(), foreign)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.Error(t, err, "somebody else's address was taken over on the strength of its name")

	var still corev1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &still))
	assert.Equal(t, int32(1234), still.Spec.Ports[0].Port, "somebody else's routing was rewritten")
	assert.NotEqual(t, kipperValue, still.Labels[kipperLabel],
		"the management label was stamped on an address Kipper never made")
}

// A name somebody else holds is not a state a retry clears, so it belongs on the
// object like the other permanent refusals. Left in the log it is a service that
// sits at Pending with the reason where an operator cannot look.
func TestReconcile_SaysWhenTheNameIsTaken(t *testing.T) {
	foreign := serviceWorkload()
	foreign.Labels = nil
	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test", Finalizers: []string{ServiceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	r, c := reconcilerOver(live, foreign)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.Error(t, err)

	var blocked kipperv1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &blocked))
	taken := meta.FindStatusCondition(blocked.Status.Conditions, kipperv1.ConditionNameFree)
	require.NotNil(t, taken, "nothing on the service says its name is taken")
	assert.Equal(t, "NameTaken", taken.Reason)
	assert.Contains(t, taken.Message, "called something else", "the message offers no way out")
	assert.Equal(t, "Failed", blocked.Status.Phase)
}

// An operator who clears the collision the message named must not still be told
// the name is taken by a service that has been running since.
func TestReconcile_TakesBackTheNameRefusalOnceTheNameIsFree(t *testing.T) {
	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test", Finalizers: []string{ServiceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
		Status: kipperv1.ServiceStatus{
			Phase: "Failed",
			Conditions: []metav1.Condition{{
				Type: kipperv1.ConditionNameFree, Status: metav1.ConditionFalse,
				Reason: "NameTaken", Message: "the workload named db in shop-test is not Kipper's",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	// The collision has been cleared: nothing stands under the name now.
	r, c := reconcilerOver(live)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.NoError(t, err)

	var recovered kipperv1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &recovered))
	assert.Nil(t, meta.FindStatusCondition(recovered.Status.Conditions, kipperv1.ConditionNameFree),
		"a service that came up is still reported as having its name taken")
}

// A delete held up by a collision cannot be answered by renaming the service:
// it is already leaving. The reason has to carry the remedy that applies, or the
// operator is left with a service that will not go and nothing saying how to let
// it.
func TestServiceFinalizer_OffersARemedyADeletingServiceCanTake(t *testing.T) {
	foreign := serviceWorkload()
	foreign.Labels = nil
	r, c := reconcilerOver(
		deletingService(map[string]string{datavolume.DeleteAnnotation: "true"}),
		foreign, serviceClaim("data-db-0"),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.Error(t, err)

	var stuck kipperv1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &stuck))
	blocked := meta.FindStatusCondition(stuck.Status.Conditions, kipperv1.ConditionCleanupComplete)
	require.NotNil(t, blocked)
	assert.Contains(t, blocked.Message, datavolume.DeleteAnnotation,
		"the reason offers no way to let a service that is already leaving finish")
}

// Accepted is not adopted. A workload and an address from before these records
// existed carry no owner, and without one garbage collection has nothing to
// follow when the service goes, so an ordinary delete would leave them running
// against the data they were keeping.
func TestReconcile_AdoptsTheLegacyObjectsItAccepts(t *testing.T) {
	live := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "shop-test", UID: types.UID("the-service"),
			Finalizers: []string{ServiceFinalizer},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	address := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop-test",
		Labels: map[string]string{kipperLabel: kipperValue},
	}}
	r, c := reconcilerOver(live, serviceWorkload(), address)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "db", Namespace: "shop-test"},
	})
	require.NoError(t, err)

	var workload appsv1.StatefulSet
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &workload))
	owner := metav1.GetControllerOf(&workload)
	require.NotNil(t, owner, "the workload has no owner, so deleting the service leaves it running")
	assert.Equal(t, types.UID("the-service"), owner.UID)

	var adopted corev1.Service
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &adopted))
	owner = metav1.GetControllerOf(&adopted)
	require.NotNil(t, owner, "the address has no owner, so deleting the service leaves it behind")
	assert.Equal(t, types.UID("the-service"), owner.UID)
}

// Adoption only reaches a service the controller sees while it is alive. One
// deleted before that still has dependants nothing will collect, and leaving
// them means a workload still running against the volume the delete kept.
func TestServiceFinalizer_TakesTheDependantsNothingElseWill(t *testing.T) {
	address := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop-test",
		Labels: map[string]string{kipperLabel: kipperValue},
	}}
	theirs := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "cache", Namespace: "shop-test",
	}}
	// No delete-data mark: an ordinary delete, which keeps the volume.
	r, c := reconcilerOver(deletingService(nil), serviceWorkload(), address, theirs, serviceClaim("data-db-0"))

	finalize(t, r)

	assert.True(t, serviceIsGone(t, c), "the service was held up by cleanup that is not destructive")
	var workload appsv1.StatefulSet
	assert.Error(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &workload),
		"the workload nothing owns is still running against the volume this delete kept")
	var gone corev1.Service
	assert.Error(t, c.Get(context.Background(),
		types.NamespacedName{Name: "db", Namespace: "shop-test"}, &gone),
		"the address nothing owns was left behind")
	assert.True(t, claimIsThere(t, c, "data-db-0"), "an ordinary delete destroyed the data")
	assert.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "cache", Namespace: "shop-test"}, &corev1.Service{}),
		"another service's address was taken")
}
