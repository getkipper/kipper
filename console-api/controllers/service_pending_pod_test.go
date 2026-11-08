package controllers

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/stranded"

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

// runningServicePod creates a scheduled, running pod with the given readiness.
func runningServicePod(revision string, ready bool) *corev1.Pod {
	pod := pendingServicePod(revision, "server", stsUID)
	pod.Status.Phase = corev1.PodRunning
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "db", Ready: ready}}
	return pod
}

// onLostNode is a pod the node controller marked not ready after losing its
// node, while the kubelet's last report still has the container ready.
func onLostNode(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "db", Ready: true}}
	return pod
}

func serverNode(ready corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "server"},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func statefulSetAtRevision(updateRevision string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: stsUID},
		Status:     appsv1.StatefulSetStatus{UpdateRevision: updateRevision, CurrentRevision: "rev1"},
	}
}

func terminating(pod *corev1.Pod) *corev1.Pod {
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"kipper.run/test"}
	return pod
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

func TestReplaceStuckPodDeletesAWaitingPodOnAStaleRevision(t *testing.T) {
	svc := bareService("postgres")
	r, base := pendingPodReconciler(svc, statefulSetAtRevision("rev2"), pendingServicePod("rev1", "", stsUID))

	lookAgain, err := r.replaceStuckPod(context.Background(), svc)
	require.NoError(t, err)
	assert.False(t, lookAgain, "The controller watches StatefulSet changes, so successful deletion needs no explicit retry.")

	err = base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-0"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err), "The pod must be deleted so the StatefulSet can recreate it from the updated template.")
}

// A database that runs but never answers its readiness probe blocks the
// StatefulSet's rollout, so a restart must replace it.
func TestReplaceStuckPodDeletesARunningPodThatIsNotReadyOnAStaleRevision(t *testing.T) {
	svc := bareService("postgres")
	r, base := pendingPodReconciler(svc, statefulSetAtRevision("rev2"), runningServicePod("rev1", false), serverNode(corev1.ConditionTrue))

	_, err := r.replaceStuckPod(context.Background(), svc)
	require.NoError(t, err)

	err = base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-0"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err), "The pod must be deleted so the StatefulSet can recreate it from the updated template.")
}

func TestReplaceStuckPodLeavesOtherPodsAlone(t *testing.T) {
	cases := map[string]struct {
		pod  *corev1.Pod
		sts  *appsv1.StatefulSet
		node *corev1.Node
	}{
		"a pod already on the new revision":                     {pendingServicePod("rev2", "", stsUID), statefulSetAtRevision("rev2"), nil},
		"a scheduled pod that is still starting":                {pendingServicePod("rev1", "server", stsUID), statefulSetAtRevision("rev2"), nil},
		"a StatefulSet with no revision computed":               {pendingServicePod("rev1", "", stsUID), statefulSetAtRevision(""), nil},
		"a pod of the same name that belongs to something else": {pendingServicePod("rev1", "", "someone-else"), statefulSetAtRevision("rev2"), nil},
		"a pod of the same name that nothing owns":              {pendingServicePod("rev1", "", ""), statefulSetAtRevision("rev2"), nil},
		"a StatefulSet whose status predates its spec":          {pendingServicePod("rev2", "", stsUID), lagging(statefulSetAtRevision("rev1")), nil},
		"a ready pod the StatefulSet rolls itself":              {runningServicePod("rev1", true), statefulSetAtRevision("rev2"), nil},
		"a pod on the new revision that is not ready yet":       {runningServicePod("rev2", false), statefulSetAtRevision("rev2"), nil},
		"a pod that is already terminating":                     {terminating(runningServicePod("rev1", false)), statefulSetAtRevision("rev2"), nil},
		"a pod on a node that lost contact":                     {onLostNode(runningServicePod("rev1", true)), statefulSetAtRevision("rev2"), nil},
		// A lost node may last have reported its container as unready too.
		"an unready pod on a node that is not ready": {runningServicePod("rev1", false), statefulSetAtRevision("rev2"), serverNode(corev1.ConditionUnknown)},
		"an unready pod on a fenced node":            {runningServicePod("rev1", false), statefulSetAtRevision("rev2"), serverNode(corev1.ConditionTrue, corev1.Taint{Key: stranded.TaintKey, Effect: corev1.TaintEffectNoExecute})},
		"an unready pod on a node that is gone":      {runningServicePod("rev1", false), statefulSetAtRevision("rev2"), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc := bareService("postgres")
			node := tc.node
			if node == nil {
				node = serverNode(corev1.ConditionTrue)
			}
			r, base := pendingPodReconciler(svc, tc.sts, tc.pod, node)
			lookAgain, err := r.replaceStuckPod(context.Background(), svc)
			require.NoError(t, err)
			assert.False(t, lookAgain)
			assert.NoError(t, base.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-0"}, &corev1.Pod{}), "The pod must be preserved.")
		})
	}
}

// TestReplaceStuckPodDeletesOnlyThePodItLookedAt verifies that deletion
// preconditions protect pods replaced or scheduled since the read.
func TestReplaceStuckPodDeletesOnlyThePodItLookedAt(t *testing.T) {
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
	_, err := r.replaceStuckPod(context.Background(), svc)
	require.NoError(t, err)

	require.NotNil(t, seen, "Deletion must include preconditions to protect pods changed since the read.")
	require.NotNil(t, seen.UID)
	require.NotNil(t, seen.ResourceVersion)
	assert.Equal(t, fetched.UID, *seen.UID)
	assert.Equal(t, fetched.ResourceVersion, *seen.ResourceVersion)
}

// TestReplaceStuckPodAsksForAnotherLookAfterAConflict verifies that a
// conflict triggers a retry because this controller does not watch pods.
func TestReplaceStuckPodAsksForAnotherLookAfterAConflict(t *testing.T) {
	svc := bareService("postgres")
	base := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, statefulSetAtRevision("rev2"), pendingServicePod("rev1", "", stsUID)).Build()
	conflicting := interceptor.NewClient(podBlindClient(base), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return errors.NewConflict(corev1.Resource("pods"), obj.GetName(), stderrors.New("the object has been modified"))
		},
	})
	r := &ServiceReconciler{Client: conflicting, APIReader: base, Scheme: testScheme()}
	lookAgain, err := r.replaceStuckPod(context.Background(), svc)
	require.NoError(t, err)
	assert.True(t, lookAgain)
}

func TestReplaceStuckPodToleratesAServiceWithoutAWorkloadYet(t *testing.T) {
	svc := bareService("postgres")
	r, _ := pendingPodReconciler(svc)
	_, err := r.replaceStuckPod(context.Background(), svc)
	assert.NoError(t, err)
}
