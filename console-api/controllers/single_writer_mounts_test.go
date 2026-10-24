package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// claimsOf returns the claims a pod spec mounts.
func claimsOf(spec corev1.PodSpec) []string {
	var out []string
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return out
}

// TestDeploymentsAndJobsMountOnlySharedClaims protects against two nodes
// writing a single-writer volume after an unfenced pod replacement. Kubernetes
// can force-detach volumes after a six-minute deletion timeout on an unhealthy
// node. Kipper reserves single-writer claims for StatefulSets, while apps and
// functions mount shared ReadWriteMany claims from Volume objects.
func TestDeploymentsAndJobsMountOnlySharedClaims(t *testing.T) {
	mounts := []kipperv1.AppVolumeMount{{Name: "uploads", MountPath: "/data"}}

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "ns", UID: "uid-shop"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1.2.3", Port: 8080, Volumes: mounts},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).Build()
	dep, err := (&AppReconciler{Client: c, Scheme: testScheme()}).buildDeployment(context.Background(), app, nil, "gen-1", "")
	require.NoError(t, err)
	assert.Equal(t, []string{sharedClaimName("uploads")}, claimsOf(dep.Spec.Template.Spec))

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "ns", UID: "uid-fn"},
		Spec:       kipperv1.FunctionSpec{Image: "registry.example.com/fn:1", Volumes: mounts},
	}
	batch, err := batchPodSpec(fn, "cron", nil, nil, "gen-1")
	require.NoError(t, err)
	assert.Equal(t, []string{sharedClaimName("uploads")}, claimsOf(batch))

	fc := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fn).Build()
	require.NoError(t, (&FunctionReconciler{Client: fc, Scheme: testScheme()}).reconcileDeployment(context.Background(), fn, "", nil, "gen-1", renderedBindings{}))
	var fnDep appsv1.Deployment
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "nightly"}, &fnDep))
	assert.Equal(t, []string{sharedClaimName("uploads")}, claimsOf(fnDep.Spec.Template.Spec))

	vol := &kipperv1.Volume{ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "ns", UID: "uid-vol"}, Spec: kipperv1.VolumeSpec{Size: "1Gi"}}
	vc := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(vol).Build()
	claim, err := (&VolumeReconciler{Client: vc, Scheme: testScheme()}).reconcilePVC(context.Background(), vol, platform.StorageLonghorn)
	require.NoError(t, err)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, claim.Spec.AccessModes, "the shared claim apps and functions mount is ReadWriteMany")
}
