package migration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func zeroDeployment(generation, observed int64) *appsv1.Deployment {
	zero := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod", UID: "deploy-uid", Generation: generation},
		Spec:       appsv1.DeploymentSpec{Replicas: &zero, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: observed},
	}
}

func replicaSet(desired, actual int32, generation, observed int64) *appsv1.ReplicaSet {
	controller := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "shop-prod", UID: "rs-uid", Generation: generation, Labels: map[string]string{"app": "web"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "deploy-uid", Controller: &controller}},
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: &desired},
		Status: appsv1.ReplicaSetStatus{Replicas: actual, ObservedGeneration: observed},
	}
}

func podOf(rs types.UID, terminating bool) *corev1.Pod {
	controller := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc-1", Namespace: "shop-prod", Labels: map[string]string{"app": "web"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc", UID: rs, Controller: &controller}},
	}}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestAppFrozen(t *testing.T) {
	app := shopApp("web", nil)
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		want    bool
	}{
		{"scaled down and gone", []runtime.Object{zeroDeployment(3, 3), replicaSet(0, 0, 4, 4)}, true},
		{"a running app whose Deployment is about to be recreated", nil, false},
		{"the Deployment is gone but its pods are not yet", []runtime.Object{podOf("rs-uid", false)}, false},
		{"the Deployment is gone but its ReplicaSet is not yet", []runtime.Object{replicaSet(1, 1, 1, 1)}, false},
		{"the Deployment controller has not seen the scale-down", []runtime.Object{zeroDeployment(3, 2), replicaSet(0, 0, 4, 4)}, false},
		{"a ReplicaSet still wants pods", []runtime.Object{zeroDeployment(3, 3), replicaSet(1, 0, 4, 4)}, false},
		{"a ReplicaSet still has pods", []runtime.Object{zeroDeployment(3, 3), replicaSet(0, 1, 4, 4)}, false},
		{"a ReplicaSet controller has not seen the scale-down", []runtime.Object{zeroDeployment(3, 3), replicaSet(0, 0, 4, 3)}, false},
		{"a pod is still terminating", []runtime.Object{zeroDeployment(3, 3), replicaSet(0, 0, 4, 4), podOf("rs-uid", true)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, appFrozen(context.Background(), fake.NewSimpleClientset(tc.objects...), app))
		})
	}
}

func TestWriteFreezeWarning(t *testing.T) {
	running := shopApp("web", nil)
	ns := projectNamespace("shop-prod", "shop")
	ns.Labels["kipper.run/environment"] = "prod"
	h := &Handler{
		Client: fake.NewSimpleClientset(ns, zeroDeployment(1, 0)),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(running, ownerOf("shop-prod")).Build(),
	}

	step := h.writeFreezeWarning(context.Background(), []string{"shop"}, 0)

	require.NotNil(t, step)
	assert.Contains(t, step.Detail, "shop-prod/web")
	assert.Contains(t, step.ManualSteps, "kip app stop web --for-migration --project shop --environment prod")
	assert.Contains(t, step.ManualSteps, "kip app start web --project shop --environment prod")
}

func TestWriteFreezeWarning_QuietOnceEveryAppIsFrozen(t *testing.T) {
	stopped := shopApp("web", &kipperv1.AppStopped{ForMigration: true})
	h := &Handler{
		Client: fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"), zeroDeployment(1, 1)),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(stopped, ownerOf("shop-prod")).Build(),
	}

	assert.Nil(t, h.writeFreezeWarning(context.Background(), []string{"shop"}, 0))
}

// Apps that finish draining within the wait budget need no write-freeze warning.
func TestWriteFreezeWarning_WaitsForAStoppedAppToDrain(t *testing.T) {
	stopped := shopApp("web", &kipperv1.AppStopped{ForMigration: true})
	client := fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"), zeroDeployment(1, 1), replicaSet(0, 1, 1, 1))
	h := &Handler{
		Client:   client,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(stopped, ownerOf("shop-prod")).Build(),
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = client.AppsV1().ReplicaSets("shop-prod").Update(context.Background(), replicaSet(0, 0, 1, 1), metav1.UpdateOptions{})
	}()

	assert.Nil(t, h.writeFreezeWarning(context.Background(), []string{"shop"}, 5*time.Second))

}

// Without a Deployment only the App says whether pods come back: a stopped one
// gets a Deployment at zero.
func TestAppFrozen_StoppedAppWithoutADeployment(t *testing.T) {
	stopped := shopApp("web", &kipperv1.AppStopped{})
	assert.True(t, appFrozen(context.Background(), fake.NewSimpleClientset(), stopped))
	assert.False(t, appFrozen(context.Background(), fake.NewSimpleClientset(podOf("rs-uid", true)), stopped))
}
