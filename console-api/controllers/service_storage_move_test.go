package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func movingService() *kipperv1.Service {
	svc := bareService("postgres")
	svc.Spec.ConsistentBackups = true
	svc.Annotations = map[string]string{platform.StorageMoveAnnotation: "kipper-storage-move-uid"}
	return svc
}

// A deleted StatefulSet or dump claim must not come back while
// 'kip service storage longhorn' moves the data, or the new claim would bind
// to the wrong class and a pod would write to a half-copied volume.
func TestAMovingServiceGetsNoWorkloadOrDumpClaim(t *testing.T) {
	svc := movingService()
	r := copiesReconciler(svc)
	err := r.reconcileStatefulSet(context.Background(), svc)
	requireGate(t, err, kipperv1.StorageCopiesConverting)
	assert.Contains(t, err.Error(), "kip service storage longhorn db")

	var sts appsv1.StatefulSet
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts)))
	var dump corev1.PersistentVolumeClaim
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &dump)))
}

func TestAMovingServiceKeepsItsStoppedWorkloadAndReportsTheMove(t *testing.T) {
	svc := movingService()
	stopped := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](0),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "old"}}}},
		},
	}
	r := copiesReconciler(svc, stopped)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"}})
	require.NoError(t, err)

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	assert.Equal(t, int32(0), *sts.Spec.Replicas)
	assert.Equal(t, "old", sts.Spec.Template.Spec.Containers[0].Image, "the workload is left exactly as the move set it")

	var got kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &got))
	assert.Equal(t, "Pending", got.Status.Phase)
	c := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, kipperv1.StorageCopiesConverting, c.Reason)
	assert.Contains(t, c.Message, "--rollback")
}
