package controllers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func dumpReconciler(t *testing.T, svc *kipperv1.Service) (*ServiceReconciler, func() appsv1.StatefulSet) {
	t.Helper()
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	get := func() appsv1.StatefulSet {
		var sts appsv1.StatefulSet
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
		return sts
	}
	return r, get
}

func dumpService() *kipperv1.Service {
	svc := bareService("postgres")
	svc.UID = "svc-uid"
	svc.Spec.Storage = "10Gi"
	svc.Spec.ConsistentBackups = true
	return svc
}

func TestConsistentBackupsAddTheDumpVolumeAndHooks(t *testing.T) {
	svc := dumpService()
	r, get := dumpReconciler(t, svc)

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &pvc))
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, "10Gi", size.String(), "the dump can be as large as the data")
	assert.Equal(t, "db", pvc.Labels["app"], "kip service update --storage grows every claim with the app label")

	tpl := get().Spec.Template
	assert.Contains(t, tpl.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: dumpVolume, MountPath: dumpMountPath})
	assert.Equal(t, "db", tpl.Annotations[preHookContainer])
	assert.Equal(t, "Continue", tpl.Annotations[preHookOnError], "a failed dump must never drop the volume backup")
	assert.Equal(t, "40m", tpl.Annotations[preHookTimeout])
	var cmd []string
	require.NoError(t, json.Unmarshal([]byte(tpl.Annotations[preHookCommand]), &cmd))
	assert.Equal(t, []string{"/bin/sh", "-c", postgresDumpScript}, cmd)
	assert.Contains(t, tpl.Annotations[postHookCommand], "rm -rf /var/lib/kipper-dump/current")
}

func TestTurningConsistentBackupsOffRemovesWhatTheyAdded(t *testing.T) {
	svc := dumpService()
	r, get := dumpReconciler(t, svc)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	svc.Spec.ConsistentBackups = false
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	tpl := get().Spec.Template
	assert.Empty(t, tpl.Spec.Volumes)
	for _, k := range dumpHookAnnotations {
		assert.NotContains(t, tpl.Annotations, k)
	}
	for _, m := range tpl.Spec.Containers[0].VolumeMounts {
		assert.NotEqual(t, dumpVolume, m.Name)
	}
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &corev1.PersistentVolumeClaim{})))
}

func TestAServiceWithoutConsistentBackupsKeepsItsTemplate(t *testing.T) {
	svc := bareService("postgres")
	r, get := dumpReconciler(t, svc)
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	before := get().Spec.Template

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.Equal(t, before, get().Spec.Template, "an upgrade must not restart databases that did not ask for dumps")
	assert.Empty(t, before.Spec.Volumes)
	for _, k := range dumpHookAnnotations {
		assert.NotContains(t, before.Annotations, k)
	}
	assert.True(t, kerrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &corev1.PersistentVolumeClaim{})))
}

func TestOnlyPostgresGetsDumps(t *testing.T) {
	svc := dumpService()
	svc.Spec.Type = "mysql"
	r, get := dumpReconciler(t, svc)

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	assert.NotContains(t, get().Spec.Template.Annotations, preHookCommand)
}

func TestTheDumpVolumeGrowsWithTheDataButNeverShrinks(t *testing.T) {
	svc := dumpService()
	r, _ := dumpReconciler(t, svc)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	require.NoError(t, r.ensureDumpClaim(ctx, svc, "longhorn", resource.MustParse("20Gi")))
	require.NoError(t, r.ensureDumpClaim(ctx, svc, "longhorn", resource.MustParse("5Gi")))

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &pvc))
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, "20Gi", size.String())
}

func TestDumpHookTimeoutScalesWithTheData(t *testing.T) {
	assert.Equal(t, "31m", dumpHookTimeout(resource.MustParse("100Mi")))
	assert.Equal(t, "35m", dumpHookTimeout(resource.MustParse("5Gi")))
	assert.Equal(t, "530m", dumpHookTimeout(resource.MustParse("500Gi")))
}

func TestARestoredDumpClaimIsTakenBack(t *testing.T) {
	svc := dumpService()
	class := "longhorn"
	restored := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "db-dump", Namespace: "ns",
			Labels: map[string]string{kipperLabel: kipperValue, "app": "db", dumpForLabel: "db"}},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
	}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, restored).Build(), Scheme: testScheme()}

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc), "a restore strips owner references, and the service must keep reconciling")

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &pvc))
	assert.Equal(t, svc.UID, metav1.GetControllerOf(&pvc).UID)
}

func TestAForeignClaimOfTheDumpNameIsRefused(t *testing.T) {
	svc := dumpService()
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "db-dump", Namespace: "ns"}}
	r := &ServiceReconciler{Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, foreign).Build(), Scheme: testScheme()}

	var taken *nameTakenError
	require.ErrorAs(t, r.reconcileStatefulSet(context.Background(), svc), &taken)
}

func TestALocalDumpVolumeIsNotExpanded(t *testing.T) {
	svc := dumpService()
	r, _ := dumpReconciler(t, svc)
	ctx := context.Background()
	require.NoError(t, r.ensureDumpClaim(ctx, svc, "local-path", resource.MustParse("5Gi")))

	require.NoError(t, r.ensureDumpClaim(ctx, svc, "local-path", resource.MustParse("20Gi")), "local volumes cannot be expanded, so asking would fail every reconcile")

	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-dump"}, &pvc))
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, "5Gi", size.String())
}

func TestTLSAndConsistentBackupsLiveTogether(t *testing.T) {
	svc := dumpService()
	svc.Spec.TLS = "on"
	r := tlsTestReconciler(t, svc, issuedSecret(t, "cert-1"))
	ctx := context.Background()

	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	tpl := liveStatefulSet(t, r).Spec.Template
	assert.Contains(t, tpl.Spec.Containers[0].Args, "ssl=on")
	assert.Contains(t, tpl.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: dumpVolume, MountPath: dumpMountPath})
	assert.Equal(t, tlsVolume, tpl.Annotations[backupVolumesExcludesAnnotation], "the dump volume travels in the backup, the key copy does not")
	assert.NotEmpty(t, tpl.Annotations[preHookCommand])

	svc.Spec.TLS = "off"
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	tpl = liveStatefulSet(t, r).Spec.Template
	assert.NotContains(t, tpl.Spec.Containers[0].Args, "ssl=on")
	assert.NotEmpty(t, tpl.Annotations[preHookCommand], "turning TLS off keeps the dumps")
	assert.Contains(t, tpl.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: dumpVolume, MountPath: dumpMountPath})
}
