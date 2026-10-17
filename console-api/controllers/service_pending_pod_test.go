package controllers

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const stsUID = types.UID("sts-uid")

func pendingServicePod(revision, node string, owner types.UID) *corev1.Pod {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-0", Namespace: "ns",
			Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: revision},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if owner != "" {
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db", UID: owner, Controller: &controller}}
	}
	return pod
}

func statefulSetAtRevision(updateRevision string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: stsUID},
		Status:     appsv1.StatefulSetStatus{UpdateRevision: updateRevision, CurrentRevision: "rev1"},
	}
}

// lagging marks the StatefulSet's status as not yet reflecting its spec.
func lagging(sts *appsv1.StatefulSet) *appsv1.StatefulSet {
	sts.Generation = 3
	sts.Status.ObservedGeneration = 2
	return sts
}

// podBlindClient rejects pod reads to simulate the manager's cache, which
// includes only the monitoring namespace.
func podBlindClient(base client.WithWatch) client.WithWatch {
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return stderrors.New("unable to get: ns/db-0 because of unknown namespace for the cache")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

func pendingPodReconciler(objects ...client.Object) (*ServiceReconciler, client.WithWatch) {
	base := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
	return &ServiceReconciler{Client: podBlindClient(base), APIReader: base, Scheme: testScheme()}, base
}

func TestReplaceUnschedulablePodDeletesAWaitingPodOnAStaleRevision(t *testing.T) {
	svc := bareService("postgres")
	r, base := pendingPodReconciler(svc, statefulSetAtRevision("rev2"), pendingServicePod("rev1", "", stsUID))

	lookAgain, err := r.replaceUnschedulablePod(context.Background(), svc)
	require.NoError(t, err)
	assert.False(t, lookAgain, "The controller watches StatefulSet changes, so successful deletion needs no explicit retry.")

	err = base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-0"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err), "The pod must be deleted so the StatefulSet can recreate it from the updated template.")
}

func TestReplaceUnschedulablePodLeavesOtherPodsAlone(t *testing.T) {
	cases := map[string]struct {
		pod *corev1.Pod
		sts *appsv1.StatefulSet
	}{
		"a pod already on the new revision":                     {pendingServicePod("rev2", "", stsUID), statefulSetAtRevision("rev2")},
		"a scheduled pod that is still starting":                {pendingServicePod("rev1", "server", stsUID), statefulSetAtRevision("rev2")},
		"a StatefulSet with no revision computed":               {pendingServicePod("rev1", "", stsUID), statefulSetAtRevision("")},
		"a pod of the same name that belongs to something else": {pendingServicePod("rev1", "", "someone-else"), statefulSetAtRevision("rev2")},
		"a pod of the same name that nothing owns":              {pendingServicePod("rev1", "", ""), statefulSetAtRevision("rev2")},
		"a StatefulSet whose status predates its spec":          {pendingServicePod("rev2", "", stsUID), lagging(statefulSetAtRevision("rev1"))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc := bareService("postgres")
			r, base := pendingPodReconciler(svc, tc.sts, tc.pod)
			lookAgain, err := r.replaceUnschedulablePod(context.Background(), svc)
			require.NoError(t, err)
			assert.False(t, lookAgain)
			assert.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-0"}, &corev1.Pod{}), "The pod must be preserved.")
		})
	}
}

// TestReplaceUnschedulablePodDeletesOnlyThePodItLookedAt verifies that deletion
// preconditions protect pods replaced or scheduled since the read.
func TestReplaceUnschedulablePodDeletesOnlyThePodItLookedAt(t *testing.T) {
	svc := bareService("postgres")
	pod := pendingServicePod("rev1", "", stsUID)
	base := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, statefulSetAtRevision("rev2"), pod).Build()
	var seen *metav1.Preconditions
	pinned := interceptor.NewClient(podBlindClient(base), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			seen = o.Preconditions
			return c.Delete(ctx, obj, opts...)
		},
	})
	r := &ServiceReconciler{Client: pinned, APIReader: base, Scheme: testScheme()}

	var fetched corev1.Pod
	require.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-0"}, &fetched))
	_, err := r.replaceUnschedulablePod(context.Background(), svc)
	require.NoError(t, err)

	require.NotNil(t, seen, "Deletion must include preconditions to protect pods changed since the read.")
	require.NotNil(t, seen.UID)
	require.NotNil(t, seen.ResourceVersion)
	assert.Equal(t, fetched.UID, *seen.UID)
	assert.Equal(t, fetched.ResourceVersion, *seen.ResourceVersion)
}

// TestReplaceUnschedulablePodAsksForAnotherLookAfterAConflict verifies that a
// conflict triggers a retry because this controller does not watch pods.
func TestReplaceUnschedulablePodAsksForAnotherLookAfterAConflict(t *testing.T) {
	svc := bareService("postgres")
	base := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, statefulSetAtRevision("rev2"), pendingServicePod("rev1", "", stsUID)).Build()
	conflicting := interceptor.NewClient(podBlindClient(base), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return errors.NewConflict(corev1.Resource("pods"), obj.GetName(), stderrors.New("the object has been modified"))
		},
	})
	r := &ServiceReconciler{Client: conflicting, APIReader: base, Scheme: testScheme()}
	lookAgain, err := r.replaceUnschedulablePod(context.Background(), svc)
	require.NoError(t, err)
	assert.True(t, lookAgain)
}

func TestReplaceUnschedulablePodToleratesAServiceWithoutAWorkloadYet(t *testing.T) {
	svc := bareService("postgres")
	r, _ := pendingPodReconciler(svc)
	_, err := r.replaceUnschedulablePod(context.Background(), svc)
	assert.NoError(t, err)
}
