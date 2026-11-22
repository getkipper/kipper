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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

func TestGrowingTheDataKeepsTheTemplateAndRaisesTheRunningPodsDumpTimeout(t *testing.T) {
	svc := dumpService()
	r, get := dumpReconciler(t, svc)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	sts := get()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "db-0", Namespace: "ns",
		Labels:      sts.Spec.Template.Labels,
		Annotations: map[string]string{preHookCommand: sts.Spec.Template.Annotations[preHookCommand], preHookTimeout: sts.Spec.Template.Annotations[preHookTimeout]},
	}}
	ownedByStatefulSet(sts, pod)
	require.NoError(t, r.Create(ctx, pod))
	before := sts.Spec.Template

	svc.Spec.Storage = "20Gi"
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	assert.Equal(t, before, get().Spec.Template, "kip promises a live expansion, so the template must not change and roll the database")
	var live corev1.Pod
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-0"}, &live))
	assert.Equal(t, "50m", live.Annotations[preHookTimeout], "Velero reads the hook timeout from the running pod")
}

func ownedByStatefulSet(sts appsv1.StatefulSet, pod *corev1.Pod) {
	isController := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: &isController,
	}}
}

func TestTurningConsistentBackupsOnSetsTheDumpTimeoutOnTheTemplate(t *testing.T) {
	svc := dumpService()
	svc.Spec.ConsistentBackups = false
	r, get := dumpReconciler(t, svc)
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	svc.Spec.ConsistentBackups = true
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	assert.Equal(t, "40m", get().Spec.Template.Annotations[preHookTimeout])
}

func timeoutPod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns", Labels: map[string]string{"app": "db"}, Annotations: annotations,
	}}
}

func timeoutStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: "sts-uid"},
		Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}},
	}
}

func TestSyncDumpTimeoutTouchesOnlyLiveHookedPodsOfTheStatefulSet(t *testing.T) {
	sts := timeoutStatefulSet()
	hooked := map[string]string{preHookCommand: "dump", preHookTimeout: "40m"}
	owned := timeoutPod("db-0", hooked)
	ownedByStatefulSet(*sts, owned)
	other := timeoutPod("db-other", map[string]string{preHookCommand: "dump", preHookTimeout: "40m"})
	ownedByStatefulSet(appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", UID: "older-sts-uid"}}, other)
	unhooked := timeoutPod("db-1", map[string]string{"unrelated": "x"})
	ownedByStatefulSet(*sts, unhooked)
	terminating := timeoutPod("db-2", map[string]string{preHookCommand: "dump", preHookTimeout: "40m"})
	ownedByStatefulSet(*sts, terminating)
	terminating.Finalizers = []string{"kipper.run/test"}
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(owned, other, unhooked, terminating).Build(),
		Scheme: testScheme(),
	}
	ctx := context.Background()

	require.NoError(t, r.syncDumpTimeout(ctx, sts, "50m"))

	annotations := func(name string) map[string]string {
		var pod corev1.Pod
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, &pod))
		return pod.Annotations
	}
	assert.Equal(t, "50m", annotations("db-0")[preHookTimeout])
	assert.Equal(t, "40m", annotations("db-other")[preHookTimeout], "a pod of another StatefulSet with the same labels stays as it is")
	assert.NotContains(t, annotations("db-1"), preHookTimeout, "a pod without the dump hook gets no timeout")
	assert.Equal(t, "40m", annotations("db-2")[preHookTimeout], "a terminating pod is left alone")
}

// conflictOnce makes the first pod update lose a race: another writer applies
// change to the stored pod first, and the update returns a conflict.
func conflictOnce(change func(*corev1.Pod)) (interceptor.Funcs, *int) {
	updates := 0
	raced := false
	return interceptor.Funcs{
		Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			updates++
			if !raced {
				raced = true
				var stored corev1.Pod
				if err := c.Get(ctx, crclient.ObjectKeyFromObject(obj), &stored); err != nil {
					return err
				}
				change(&stored)
				if err := c.Update(ctx, &stored); err != nil {
					return err
				}
				return kerrors.NewConflict(corev1.Resource("pods"), obj.GetName(), nil)
			}
			return c.Update(ctx, obj, opts...)
		},
	}, &updates
}

func TestSyncDumpTimeoutRetriesAConflictOnTheFreshPod(t *testing.T) {
	sts := timeoutStatefulSet()
	ctx := context.Background()
	run := func(t *testing.T, change func(*corev1.Pod)) (corev1.Pod, int) {
		pod := timeoutPod("db-0", map[string]string{preHookCommand: "dump", preHookTimeout: "40m"})
		ownedByStatefulSet(*sts, pod)
		funcs, updates := conflictOnce(change)
		r := &ServiceReconciler{
			Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(pod).WithInterceptorFuncs(funcs).Build(),
			Scheme: testScheme(),
		}
		require.NoError(t, r.syncDumpTimeout(ctx, sts, "50m"))
		var live corev1.Pod
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-0"}, &live))
		return live, *updates
	}

	t.Run("keeps the other writer's change", func(t *testing.T) {
		live, _ := run(t, func(p *corev1.Pod) { p.Annotations["other"] = "kept" })
		assert.Equal(t, "50m", live.Annotations[preHookTimeout])
		assert.Equal(t, "kept", live.Annotations["other"])
	})

	t.Run("leaves a pod that lost its pre-hook alone", func(t *testing.T) {
		live, updates := run(t, func(p *corev1.Pod) { delete(p.Annotations, preHookCommand) })
		assert.Equal(t, "40m", live.Annotations[preHookTimeout])
		assert.Equal(t, 1, updates, "no second update once the fresh pod no longer qualifies")
	})
}

// TestSyncDumpTimeoutSkipsAPodDeletedMeanwhile covers a pod deleted between the
// list and its update, before or after a conflict.
func TestSyncDumpTimeoutSkipsAPodDeletedMeanwhile(t *testing.T) {
	for name, answer := range map[string]func(ctx context.Context, c crclient.WithWatch, obj crclient.Object) error{
		"before the update": func(ctx context.Context, c crclient.WithWatch, obj crclient.Object) error {
			return c.Update(ctx, obj)
		},
		"after a conflict": func(_ context.Context, _ crclient.WithWatch, obj crclient.Object) error {
			return kerrors.NewConflict(corev1.Resource("pods"), obj.GetName(), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			sts := timeoutStatefulSet()
			ctx := context.Background()
			gone := timeoutPod("db-0", map[string]string{preHookCommand: "dump", preHookTimeout: "40m"})
			kept := timeoutPod("db-1", map[string]string{preHookCommand: "dump", preHookTimeout: "40m"})
			ownedByStatefulSet(*sts, gone)
			ownedByStatefulSet(*sts, kept)
			funcs := interceptor.Funcs{
				Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
					if obj.GetName() != "db-0" {
						return c.Update(ctx, obj, opts...)
					}
					if err := c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db-0"}}); err != nil && !kerrors.IsNotFound(err) {
						return err
					}
					return answer(ctx, c, obj)
				},
			}
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gone, kept).WithInterceptorFuncs(funcs).Build(),
				Scheme: testScheme(),
			}
			require.NoError(t, r.syncDumpTimeout(ctx, sts, "50m"))
			var live corev1.Pod
			require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "db-1"}, &live))
			assert.Equal(t, "50m", live.Annotations[preHookTimeout], "the remaining pods are still updated")
		})
	}
}
