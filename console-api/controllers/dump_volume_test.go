package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

func faultService() *kipperv1.Service {
	svc := copiesService(1, true)
	svc.UID = "svc-uid"
	return svc
}

func dumpClaim(owner types.UID) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-dump", Namespace: "ns",
			Labels: map[string]string{kipperLabel: kipperValue, "app": "db", dumpForLabel: "db"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To(platform.LonghornSingleClass), VolumeName: "pv-dump",
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if owner != "" {
		pvc.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: kipperv1.GroupVersion.String(), Kind: "Service", Name: "db", UID: owner, Controller: ptr.To(true),
		}}
	}
	return pvc
}

// dumpPV binds the dump claim to a Longhorn volume whose name differs from
// the PV's, as a statically imported volume's does.
func dumpPV(claim, driver string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-dump"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef:               &corev1.ObjectReference{Namespace: "ns", Name: claim},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: "lh-dump"}},
		},
	}
}

func longhornPV() *corev1.PersistentVolume { return dumpPV("db-dump", "driver.longhorn.io") }

func dumpLonghornVolume(robustness, claim string) *unstructured.Unstructured {
	v := longhornVolume("lh-dump", "ns", claim, 1)
	_ = unstructured.SetNestedField(v.Object, robustness, "status", "robustness")
	return v
}

func dumpCondition(t *testing.T, r *ServiceReconciler) *metav1.Condition {
	t.Helper()
	var svc kipperv1.Service
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &svc))
	return apimeta.FindStatusCondition(svc.Status.Conditions, servicecatalog.ConditionDumpVolumeReady)
}

func TestAFaultedDumpVolumeIsReportedWithTheRecreateCommand(t *testing.T) {
	svc := faultService()
	r := copiesReconciler(svc, longhornPV(), dumpClaim(svc.UID), dumpLonghornVolume("faulted", "db-dump"))

	retry, err := r.reportDumpVolume(context.Background(), svc)
	require.NoError(t, err)
	assert.Equal(t, storageRefreshUnsettled, retry, "the Longhorn watch is not registered, so a faulted dump is read again on a timer")

	cond := dumpCondition(t, r)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Faulted", cond.Reason)
	assert.Contains(t, cond.Message, "kip service recreate-dump db")
	assert.Contains(t, cond.Message, "cannot start")
	assert.Contains(t, cond.Message, "A pod that is running keeps running until it restarts", "the pod may still be up when the fault is found")
	assert.Contains(t, cond.Message, "data volume unchanged")
	assert.NotContains(t, cond.Message, "healthy", "the dump's state says nothing about the data volume's health")
}

func TestADumpVolumeThatIsNotFaultedCarriesNoCondition(t *testing.T) {
	for _, robustness := range []string{"healthy", "degraded", "unknown", ""} {
		t.Run(robustness, func(t *testing.T) {
			svc := faultService()
			svc.Status.Conditions = []metav1.Condition{{
				Type: servicecatalog.ConditionDumpVolumeReady, Status: metav1.ConditionFalse, Reason: "Faulted", Message: "old",
				LastTransitionTime: metav1.Now(),
			}}
			r := copiesReconciler(svc, longhornPV(), dumpClaim(svc.UID), dumpLonghornVolume(robustness, "db-dump"))
			retry, err := r.reportDumpVolume(context.Background(), svc)
			require.NoError(t, err)
			assert.Equal(t, storageRefreshUnsettled, retry, "the Longhorn watch is not registered, so a dump volume that faults while the pod runs is found on this timer")
			assert.Nil(t, dumpCondition(t, r), "a recovered or recreated dump volume clears the report")
		})
	}
}

func TestDumpVolumeReportIgnoresVolumesItCannotProve(t *testing.T) {
	cases := map[string]struct {
		svc    func() *kipperv1.Service
		pv     *corev1.PersistentVolume
		claim  crclient.Object
		volume crclient.Object
	}{
		"PV bound to another claim": {
			svc: faultService, pv: dumpPV("other-claim", "driver.longhorn.io"),
			claim: dumpClaim("svc-uid"), volume: dumpLonghornVolume("faulted", "db-dump"),
		},
		"PV bound to an earlier claim of the same name": {
			svc: faultService, pv: func() *corev1.PersistentVolume {
				pv := longhornPV()
				pv.Spec.ClaimRef.UID = "earlier-claim-uid"
				return pv
			}(),
			claim: dumpClaim("svc-uid"), volume: dumpLonghornVolume("faulted", "db-dump"),
		},
		"PV of another driver": {
			svc: faultService, pv: dumpPV("db-dump", "other.csi.example.com"),
			claim: dumpClaim("svc-uid"), volume: dumpLonghornVolume("faulted", "db-dump"),
		},
		"consistent backups off": {
			svc:   func() *kipperv1.Service { s := faultService(); s.Spec.ConsistentBackups = false; return s },
			claim: dumpClaim("svc-uid"), volume: dumpLonghornVolume("faulted", "db-dump"),
		},
		"no dump claim": {svc: faultService, volume: dumpLonghornVolume("faulted", "db-dump")},
		"claim of another owner": {
			svc: faultService, claim: dumpClaim("other-uid"), volume: dumpLonghornVolume("faulted", "db-dump"),
		},
		"volume bound to another claim": {
			svc: faultService, claim: dumpClaim("svc-uid"), volume: dumpLonghornVolume("faulted", "other-claim"),
		},
		"no Longhorn volume": {svc: faultService, claim: dumpClaim("svc-uid")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc := tc.svc()
			objs := []crclient.Object{svc, tc.pv}
			if tc.pv == nil {
				objs[1] = longhornPV()
			}
			for _, o := range []crclient.Object{tc.claim, tc.volume} {
				if o != nil {
					objs = append(objs, o)
				}
			}
			r := copiesReconciler(objs...)
			_, err := r.reportDumpVolume(context.Background(), svc)
			require.NoError(t, err)
			assert.Nil(t, dumpCondition(t, r))
		})
	}
}

func TestADumpVolumeOnALocalClusterIsNeverReported(t *testing.T) {
	svc := faultService()
	claim := dumpClaim(svc.UID)
	claim.Spec.StorageClassName = ptr.To(platform.LocalPathClass)
	r := copiesReconciler(localPlatformConfig(), svc, claim)
	retry, err := r.reportDumpVolume(context.Background(), svc)
	require.NoError(t, err)
	assert.Zero(t, retry, "a local volume has no Longhorn state to poll")
	assert.Nil(t, dumpCondition(t, r))
}

func TestATerminatingDumpClaimIsCheckedAgainSoon(t *testing.T) {
	svc := faultService()
	claim := dumpClaim(svc.UID)
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	r := copiesReconciler(svc, longhornPV(), claim, dumpLonghornVolume("faulted", "db-dump"))

	retry, err := r.reportDumpVolume(context.Background(), svc)
	require.NoError(t, err)
	assert.Equal(t, pendingPodRetry, retry, "the replacement claim is created as soon as the old one is gone")
	assert.Nil(t, dumpCondition(t, r), "a claim on its way out is no longer the one to recreate")
}

func TestATerminatingDumpClaimDoesNotHoldTheStatefulSet(t *testing.T) {
	svc := faultService()
	claim := dumpClaim(svc.UID)
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	r := copiesReconciler(svc, claim)

	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))
	data, _ := claimClasses(t, r)
	assert.Equal(t, platform.LonghornSingleClass, data, "the StatefulSet is created while the old dump claim goes")
}

func TestReconcileReportsAFaultedDumpVolume(t *testing.T) {
	svc := faultService()
	r := copiesReconciler(svc, longhornPV(), dumpClaim(svc.UID), dumpLonghornVolume("faulted", "db-dump"))

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"}})
	require.NoError(t, err)
	cond := dumpCondition(t, r)
	require.NotNil(t, cond)
	assert.Equal(t, "Faulted", cond.Reason)
	assert.LessOrEqual(t, res.RequeueAfter, storageRefreshUnsettled)
}

// A copy change stops the service and starts it again, and a database with a
// faulted dump volume cannot start. Such a change waits before anything is
// stopped and names the command that replaces the dump volume. A change that
// has stopped the service already goes on.
func TestACopyChangeDoesNotStopAServiceWithAFaultedDumpVolume(t *testing.T) {
	svc := faultService()
	world := func(claim *corev1.PersistentVolumeClaim) *ServiceReconciler {
		return copiesReconciler(convWorld(append(twoCopies("worker-1"), svc, claim,
			attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"), lhAttachmentObj(""),
			longhornPV(), dumpClaim(svc.UID), dumpLonghornVolume("faulted", "db-dump"))...)...)
	}
	ctx := context.Background()

	r := world(dbDataClaim())
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	c := apimeta.FindStatusCondition(svc.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, c.Reason)
	assert.True(t, strings.HasPrefix(c.Message, "db keeps running."), c.Message)
	assert.Contains(t, c.Message, "kip service recreate-dump db")
	assert.Nil(t, stopOf(t, r.Client), "nothing is stopped for a change the service could not come back from")
	assert.Equal(t, int32(1), replicasOfDB(t, r.Client))

	r = world(stoppedClaim())
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	require.NoError(t, r.updateStatus(ctx, svc))
	c = apimeta.FindStatusCondition(svc.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, c)
	assert.NotContains(t, c.Message, "recreate-dump", "a change already under way is not undone")
}
