package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A read-only volume cannot be cleared by restarting the container: the mount
// belongs to the pod, so kubelet has to detach and re-attach, which needs the
// pod recreated. That is why `kip service restart` exists, and it works by
// stamping the Service CR and letting the StatefulSet roll the pod. Apps have
// carried kipper.run/restartedAt since the beginning; services did not, so a
// crash-looping database could only be recovered with kubectl.

func TestReconcileStatefulSet_ProjectsRestartStamp(t *testing.T) {
	svc := bareService("postgres")
	svc.Annotations = map[string]string{"kipper.run/restartedAt": "2026-09-05T09:00:00Z"}
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	assert.Equal(t, "2026-09-05T09:00:00Z", sts.Spec.Template.Annotations["kipper.run/restartedAt"],
		"the stamp must reach the pod template, or nothing rolls and the command lies")
}

// The stamp has to survive an update of an existing StatefulSet, not only its
// creation. The reconciler copies a narrow set of fields onto the live object,
// and before this change template annotations were not among them, so a restart
// of a service that already existed changed nothing at all.
func TestReconcileStatefulSet_RestartStampReachesAnExistingWorkload(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	// First pass creates it with no stamp, as an ordinary service.
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	// The operator then runs kip service restart.
	svc.Annotations = map[string]string{"kipper.run/restartedAt": "2026-09-05T10:30:00Z"}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	assert.Equal(t, "2026-09-05T10:30:00Z", sts.Spec.Template.Annotations["kipper.run/restartedAt"],
		"a restart of an existing service must reach the running pod template")
}

// A second restart must differ from the first, or the pod template matches what
// is already running and the StatefulSet does not roll. The CLI uses a
// nanosecond stamp for this reason; the controller must not collapse it.
func TestReconcileStatefulSet_SecondRestartDiffersFromTheFirst(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	svc.Annotations = map[string]string{"kipper.run/restartedAt": "2026-09-05T10:30:00.111111111Z"}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	svc.Annotations["kipper.run/restartedAt"] = "2026-09-05T10:30:00.222222222Z"
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	assert.Equal(t, "2026-09-05T10:30:00.222222222Z", sts.Spec.Template.Annotations["kipper.run/restartedAt"])
}

// A service with no stamp must not gain an empty annotation, which would be
// noise on every StatefulSet in the cluster.
func TestReconcileStatefulSet_NoStampNoAnnotation(t *testing.T) {
	svc := bareService("redis")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	_, present := sts.Spec.Template.Annotations["kipper.run/restartedAt"]
	assert.False(t, present, "an unrestarted service must carry no restart stamp")
}

// Annotations the platform did not put there belong to whoever did, and a
// reconcile must not wipe them.
func TestReconcileStatefulSet_KeepsForeignTemplateAnnotations(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	if sts.Spec.Template.Annotations == nil {
		sts.Spec.Template.Annotations = map[string]string{}
	}
	sts.Spec.Template.Annotations["example.com/injected"] = "keep me"
	sts.ResourceVersion = ""
	require.NoError(t, r.Update(context.Background(), &sts))

	svc.Annotations = map[string]string{"kipper.run/restartedAt": "2026-09-05T11:00:00Z"}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var after appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &after))
	assert.Equal(t, "keep me", after.Spec.Template.Annotations["example.com/injected"],
		"a reconcile must not drop annotations it did not write")
	assert.Equal(t, "2026-09-05T11:00:00Z", after.Spec.Template.Annotations["kipper.run/restartedAt"])
}

var _ = metav1.ObjectMeta{}
