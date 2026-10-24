package cmd

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

const recreateNS = "shop"

func dumpServiceCR(svcType string, consistent bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "db", "namespace": recreateNS, "uid": "svc-uid"},
		"spec":       map[string]interface{}{"type": svcType, "consistentBackups": consistent},
	}}
}

func controlledBy(kind, name string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "v1", Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}}
}

func recreateClaim(name string, owner types.UID) *corev1.PersistentVolumeClaim {
	c := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: recreateNS, Name: name, UID: types.UID(name + "-uid"),
		Labels: map[string]string{"kipper.run/dump-for": "db"},
	}}
	if owner != "" {
		c.OwnerReferences = controlledBy("Service", "db", owner)
	}
	return c
}

func recreateStatefulSet(owner types.UID) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: recreateNS, Name: "db", UID: "sts-uid", OwnerReferences: controlledBy("Service", "db", owner),
	}}
}

func recreatePod(owner types.UID, claims ...string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: recreateNS, Name: "db-0", UID: "pod-uid", OwnerReferences: controlledBy("StatefulSet", "db", owner),
	}, Spec: corev1.PodSpec{NodeName: "worker-1"}}
	for _, c := range claims {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: c, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c},
		}})
	}
	return pod
}

type recreateFixture struct {
	cr         *unstructured.Unstructured
	objects    []runtime.Object
	velero     []runtime.Object
	robustness map[string]string
}

func veleroWork(resource, name, phase string, spec map[string]interface{}) *unstructured.Unstructured {
	kind := map[string]string{"backups": "Backup", "restores": "Restore", "podvolumerestores": "PodVolumeRestore", "datadownloads": "DataDownload"}[resource]
	version := "velero.io/v1"
	if resource == "datadownloads" {
		version = "velero.io/v2alpha1"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": version, "kind": kind,
		"metadata": map[string]interface{}{"name": name, "namespace": "velero"},
		"spec":     spec,
		"status":   map[string]interface{}{"phase": phase},
	}}
}

func healthyRecreateFixture() recreateFixture {
	return recreateFixture{
		cr: dumpServiceCR("postgres", true),
		objects: []runtime.Object{
			recreateClaim("db-dump", "svc-uid"), recreateClaim("data-db-0", ""),
			recreateStatefulSet("svc-uid"), recreatePod("sts-uid", "data-db-0", "db-dump"),
		},
		robustness: map[string]string{"db-dump": "faulted", "data-db-0": "healthy"},
	}
}

func (f recreateFixture) build() (*dumpRecreate, *k8sfake.Clientset) {
	client := k8sfake.NewSimpleClientset(f.objects...) //nolint:staticcheck
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			manifest.ServiceGVR: "ServiceList", veleroBackupGVR: "BackupList", veleroRestoreGVR: "RestoreList",
			podVolumeRestoreGVR: "PodVolumeRestoreList", dataDownloadGVR: "DataDownloadList",
		},
		append([]runtime.Object{f.cr}, f.velero...)...)
	return &dumpRecreate{
		client: client, dyn: dyn,
		robustness: func(_ context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
			return f.robustness[pvc.Name], nil
		},
		within: 200 * time.Millisecond, poll: 10 * time.Millisecond,
	}, client
}

func TestRecreateDumpAcceptsAFaultedDumpItCanProve(t *testing.T) {
	d, _ := healthyRecreateFixture().build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)
	assert.Equal(t, types.UID("db-dump-uid"), target.claim.UID)
	require.NotNil(t, target.pod)
	assert.False(t, target.dataFaulted)
}

func TestRecreateDumpAcceptsAnAbsentPod(t *testing.T) {
	f := healthyRecreateFixture()
	f.objects = f.objects[:3]
	d, _ := f.build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)
	assert.Nil(t, target.pod)
}

func TestRecreateDumpRefuses(t *testing.T) {
	cases := map[string]struct {
		change func(*recreateFixture)
		want   string
	}{
		"another service type": {
			change: func(f *recreateFixture) { f.cr = dumpServiceCR("mysql", true) },
			want:   "Postgres service with consistent backups",
		},
		"consistent backups off": {
			change: func(f *recreateFixture) { f.cr = dumpServiceCR("postgres", false) },
			want:   "Postgres service with consistent backups",
		},
		"a dump volume that is not faulted": {
			change: func(f *recreateFixture) { f.robustness["db-dump"] = "degraded" },
			want:   "is degraded",
		},
		"a dump volume Longhorn does not report": {
			change: func(f *recreateFixture) { delete(f.robustness, "db-dump") },
			want:   "is unknown",
		},
		"no dump claim": {
			change: func(f *recreateFixture) { f.objects = f.objects[1:] },
			want:   "has no dump volume",
		},
		"a dump claim not yet adopted": {
			change: func(f *recreateFixture) { f.objects[0] = recreateClaim("db-dump", "") },
			want:   "wait for the controller to adopt it",
		},
		"a dump claim of another owner": {
			change: func(f *recreateFixture) { f.objects[0] = recreateClaim("db-dump", "other-uid") },
			want:   "does not belong to service db",
		},
		"a StatefulSet of another owner": {
			change: func(f *recreateFixture) { f.objects[2] = recreateStatefulSet("other-uid") },
			want:   "StatefulSet db does not belong to service db",
		},
		"no StatefulSet": {
			change: func(f *recreateFixture) { f.objects = append(f.objects[:2], f.objects[3]) },
			want:   "has no StatefulSet",
		},
		"a pod of another owner": {
			change: func(f *recreateFixture) { f.objects[3] = recreatePod("other-uid", "data-db-0", "db-dump") },
			want:   "pod db-0 does not belong",
		},
		"a pod that does not mount the dump claim": {
			change: func(f *recreateFixture) { f.objects[3] = recreatePod("sts-uid", "data-db-0") },
			want:   "does not mount db-dump",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := healthyRecreateFixture()
			tc.change(&f)
			d, client := f.build()
			_, err := d.check(context.Background(), recreateNS, "db")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			for _, a := range client.Actions() {
				assert.NotEqual(t, "delete", a.GetVerb(), "a refusal changes nothing")
			}
		})
	}
}

func TestRecreateDumpNotesAFaultedDataVolume(t *testing.T) {
	f := healthyRecreateFixture()
	f.robustness["data-db-0"] = "faulted"
	d, _ := f.build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err, "the dump volume is still worth recreating")
	assert.True(t, target.dataFaulted)

	var out bytes.Buffer
	writeRecreateDumpPlan(&out, "db", target)
	assert.Contains(t, out.String(), "The data volume is faulted too")
	assert.Contains(t, out.String(), "left unchanged")
	assert.NotContains(t, out.String(), "healthy")
}

func TestRecreateDumpPlanNeverCallsTheDataVolumeHealthy(t *testing.T) {
	d, _ := healthyRecreateFixture().build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)
	var out bytes.Buffer
	writeRecreateDumpPlan(&out, "db", target)
	assert.Contains(t, out.String(), "deletes the dump volume db-dump and restarts the database pod")
	assert.Contains(t, out.String(), "data volume is left unchanged")
	assert.NotContains(t, out.String(), "healthy")
	assert.NotContains(t, out.String(), "faulted too")
}

// replaceOnDelete answers a claim deletion the way the controller does: the
// old claim is removed and a new one appears with the same name and a new UID.
func replaceOnDelete(client *k8sfake.Clientset) {
	client.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		gvr := corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")
		if err := client.Tracker().Delete(gvr, recreateNS, name); err != nil {
			return true, nil, err
		}
		fresh := recreateClaim(name, "svc-uid")
		fresh.UID = "new-claim-uid"
		return true, nil, client.Tracker().Add(fresh)
	})
}

func TestRecreateDumpDeletesWithPreconditionsAndWaitsForTheNewClaim(t *testing.T) {
	d, client := healthyRecreateFixture().build()
	replaceOnDelete(client)
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, d.recreate(context.Background(), &out, recreateNS, "db", target))
	assert.Contains(t, out.String(), "new dump volume")

	deleted := map[string]metav1.DeleteOptions{}
	for _, a := range client.Actions() {
		if del, ok := a.(k8stesting.DeleteActionImpl); ok {
			deleted[del.GetResource().Resource+"/"+del.GetName()] = del.GetDeleteOptions()
		}
	}
	require.Contains(t, deleted, "persistentvolumeclaims/db-dump")
	require.Contains(t, deleted, "pods/db-0")
	assert.NotContains(t, deleted, "persistentvolumeclaims/data-db-0", "the data volume is never touched")
	claimOpts, podOpts := deleted["persistentvolumeclaims/db-dump"], deleted["pods/db-0"]
	require.NotNil(t, claimOpts.Preconditions)
	assert.Equal(t, types.UID("db-dump-uid"), *claimOpts.Preconditions.UID)
	require.NotNil(t, podOpts.Preconditions)
	assert.Equal(t, types.UID("pod-uid"), *podOpts.Preconditions.UID)
	assert.Nil(t, podOpts.GracePeriodSeconds, "the pod gets its normal grace period")
}

func TestRecreateDumpReportsWhatIsLeftOnTimeout(t *testing.T) {
	d, client := healthyRecreateFixture().build()
	// A pod on a node that is down stays terminating, and pvc-protection keeps
	// its claim until the pod is removed.
	client.PrependReactor("delete", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)

	var out bytes.Buffer
	err = d.recreate(context.Background(), &out, recreateNS, "db", target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db-dump is still being deleted")
	assert.Contains(t, err.Error(), "pod db-0 on worker-1 is still there")
	assert.Contains(t, err.Error(), "kip node fence worker-1")
}

func TestServiceInfoWarnsAboutAFaultedDumpVolume(t *testing.T) {
	cr := healthyServiceCR("db", "redis")
	message := "The dump volume is faulted, so consistent dumps fail and the database pod cannot start until the dump volume is replaced. A pod that is running keeps running until it restarts. Recreating the dump volume leaves the data volume unchanged: kip service recreate-dump db"
	_ = unstructured.SetNestedSlice(cr.Object, []interface{}{map[string]interface{}{
		"type": "DumpVolumeReady", "status": "False", "reason": "Faulted", "message": message,
	}}, "status", "conditions")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList", service.LonghornReplicaGVR: "ReplicaList"}, cr)
	client := k8sfake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "default"},
		Data:       map[string][]byte{"HOST": []byte("db.default.svc.cluster.local"), "PORT": []byte("6379")},
	})

	var out bytes.Buffer
	require.NoError(t, writeServiceInfo(context.Background(), &out, &service.Manager{Client: client, Dynamic: dyn}, "default", "db", false))
	assert.Contains(t, out.String(), "Host:")
	assert.Contains(t, out.String(), "  ⚠  "+message)
}

func TestRecreateDumpChecksAgainAfterTheConfirmation(t *testing.T) {
	cases := map[string]func(*recreateFixture, *k8sfake.Clientset){
		"the dump volume recovered": func(f *recreateFixture, _ *k8sfake.Clientset) { f.robustness["db-dump"] = "healthy" },
		"the pod was replaced": func(_ *recreateFixture, client *k8sfake.Clientset) {
			pod := recreatePod("sts-uid", "data-db-0", "db-dump")
			pod.UID = "new-pod-uid"
			_, _ = client.CoreV1().Pods(recreateNS).Update(context.Background(), pod, metav1.UpdateOptions{})
		},
		"the claim was replaced": func(_ *recreateFixture, client *k8sfake.Clientset) {
			claim := recreateClaim("db-dump", "svc-uid")
			claim.UID = "new-claim-uid"
			_, _ = client.CoreV1().PersistentVolumeClaims(recreateNS).Update(context.Background(), claim, metav1.UpdateOptions{})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := healthyRecreateFixture()
			d, client := f.build()
			target, err := d.check(context.Background(), recreateNS, "db")
			require.NoError(t, err)
			change(&f, client)

			var out bytes.Buffer
			require.Error(t, d.recreate(context.Background(), &out, recreateNS, "db", target))
			for _, a := range client.Actions() {
				assert.NotEqual(t, "delete", a.GetVerb(), "what was confirmed is no longer what would be deleted")
			}
		})
	}
}

func TestRecreateDumpWaitsForBackupsAndRestoresOfTheNamespace(t *testing.T) {
	ns := func(names ...interface{}) []interface{} { return names }
	cases := map[string]struct {
		velero *unstructured.Unstructured
		want   string
	}{
		"a running backup of the namespace": {
			velero: veleroWork("backups", "nightly", "InProgress", map[string]interface{}{"includedNamespaces": ns(recreateNS)}),
			want:   "backup nightly is still running",
		},
		"a running backup of every namespace": {
			velero: veleroWork("backups", "nightly", "InProgress", map[string]interface{}{}),
			want:   "backup nightly is still running",
		},
		"a restore into the namespace": {
			velero: veleroWork("restores", "back", "InProgress", map[string]interface{}{"includedNamespaces": ns(recreateNS)}),
			want:   "restore back",
		},
		"a restore mapped into the namespace": {
			velero: veleroWork("restores", "copy", "WaitingForPluginOperations", map[string]interface{}{
				"includedNamespaces": ns("shop-old"), "namespaceMapping": map[string]interface{}{"shop-old": recreateNS},
			}),
			want: "restore copy",
		},
		"a volume restore outliving its finished restore": {
			velero: veleroWork("podvolumerestores", "back-x7", "InProgress", map[string]interface{}{
				"pod": map[string]interface{}{"namespace": recreateNS, "name": "db-0"},
			}),
			want: "volume restore back-x7",
		},
		"a data download into the namespace": {
			velero: veleroWork("datadownloads", "back-dd", "Accepted", map[string]interface{}{
				"targetVolume": map[string]interface{}{"namespace": recreateNS, "pvc": "data-db-0"},
			}),
			want: "data download back-dd",
		},
		"a restore of every namespace": {
			velero: veleroWork("restores", "all", "New", map[string]interface{}{"includedNamespaces": ns("*")}),
			want:   "restore all",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := healthyRecreateFixture()
			f.velero = []runtime.Object{tc.velero}
			d, _ := f.build()
			_, err := d.check(context.Background(), recreateNS, "db")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRecreateDumpIgnoresVeleroWorkElsewhereOrFinished(t *testing.T) {
	f := healthyRecreateFixture()
	f.velero = []runtime.Object{
		veleroWork("backups", "done", "Completed", map[string]interface{}{}),
		veleroWork("backups", "other", "InProgress", map[string]interface{}{"includedNamespaces": []interface{}{"blog-prod"}}),
		veleroWork("restores", "failed", "PartiallyFailed", map[string]interface{}{"includedNamespaces": []interface{}{recreateNS}}),
		veleroWork("podvolumerestores", "done-x1", "Completed", map[string]interface{}{
			"pod": map[string]interface{}{"namespace": recreateNS, "name": "db-0"},
		}),
		veleroWork("podvolumerestores", "drill-x1", "InProgress", map[string]interface{}{
			"pod": map[string]interface{}{"namespace": "kipper-restore-drill", "name": "db-0"},
		}),
		veleroWork("restores", "drill", "InProgress", map[string]interface{}{
			"includedNamespaces": []interface{}{recreateNS}, "namespaceMapping": map[string]interface{}{recreateNS: "kipper-restore-drill"},
		}),
	}
	d, _ := f.build()
	_, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err, "a drill reads the namespace's backup and writes elsewhere")
}

func TestRecreateDumpFailsClosedWhenVeleroCannotBeRead(t *testing.T) {
	for _, resource := range []string{"backups", "restores", "podvolumerestores", "datadownloads"} {
		t.Run(resource, func(t *testing.T) {
			d, _ := healthyRecreateFixture().build()
			d.dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "velero.io", Resource: resource}, "", fmt.Errorf("denied"))
			})
			_, err := d.check(context.Background(), recreateNS, "db")
			require.Error(t, err)
		})
	}
}

func TestRecreateDumpChecksForBackupsAgainAfterTheConfirmation(t *testing.T) {
	d, client := healthyRecreateFixture().build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)
	_, err = d.dyn.Resource(veleroBackupGVR).Namespace("velero").Create(context.Background(),
		veleroWork("backups", "manual", "InProgress", map[string]interface{}{}), metav1.CreateOptions{})
	require.NoError(t, err)

	var out bytes.Buffer
	require.ErrorContains(t, d.recreate(context.Background(), &out, recreateNS, "db", target), "backup manual is still running")
	for _, a := range client.Actions() {
		assert.NotEqual(t, "delete", a.GetVerb())
	}
}

func TestRecreateDumpChecksForVolumeRestoresAgainAfterTheConfirmation(t *testing.T) {
	d, client := healthyRecreateFixture().build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)
	_, err = d.dyn.Resource(podVolumeRestoreGVR).Namespace("velero").Create(context.Background(),
		veleroWork("podvolumerestores", "late-x1", "New", map[string]interface{}{
			"pod": map[string]interface{}{"namespace": recreateNS, "name": "db-0"},
		}), metav1.CreateOptions{})
	require.NoError(t, err)

	var out bytes.Buffer
	require.ErrorContains(t, d.recreate(context.Background(), &out, recreateNS, "db", target), "volume restore late-x1")
	for _, a := range client.Actions() {
		assert.NotEqual(t, "delete", a.GetVerb())
	}
}

// TestRecreateDumpRefusesDuringAStorageMove verifies that recreation leaves
// claims under the move's control until it ends, when the controller can
// create a new dump claim.
func TestRecreateDumpRefusesDuringAStorageMove(t *testing.T) {
	f := healthyRecreateFixture()
	f.cr.SetAnnotations(map[string]string{platform.StorageMoveAnnotation: "move-uid"})
	d, client := f.build()
	_, err := d.check(context.Background(), recreateNS, "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is moving its volume to Longhorn")
	_, getErr := client.CoreV1().PersistentVolumeClaims(recreateNS).Get(context.Background(), "db-dump", metav1.GetOptions{})
	assert.NoError(t, getErr, "the dump claim is left alone")
}

// TestRecreateDumpNamesAMoveThatStartedAfterTheCheck verifies that the timeout
// explains why the controller delays the new dump volume until the move ends.
func TestRecreateDumpNamesAMoveThatStartedAfterTheCheck(t *testing.T) {
	d, client := healthyRecreateFixture().build()
	target, err := d.check(context.Background(), recreateNS, "db")
	require.NoError(t, err)
	client.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		cr, err := d.dyn.Resource(manifest.ServiceGVR).Namespace(recreateNS).Get(context.Background(), "db", metav1.GetOptions{})
		require.NoError(t, err)
		cr.SetAnnotations(map[string]string{platform.StorageMoveAnnotation: "move-uid"})
		_, err = d.dyn.Resource(manifest.ServiceGVR).Namespace(recreateNS).Update(context.Background(), cr, metav1.UpdateOptions{})
		require.NoError(t, err)
		return false, nil, nil
	})

	var out bytes.Buffer
	err = d.recreate(context.Background(), &out, recreateNS, "db", target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the controller has not created a new db-dump yet")
	assert.Contains(t, err.Error(), "db started moving its volume to Longhorn, and the controller creates the new dump volume once the move ends")
}
