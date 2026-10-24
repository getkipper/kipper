package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func localClaim(namespace, name, class string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
	}
}

func retainedPV(name, service, state, usedBytes string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{platform.RetainedLocalCopyLabel: "true"},
		Annotations: map[string]string{
			platform.RetainedLocalCopyServiceAnnotation:   service,
			platform.RetainedLocalCopyStateAnnotation:     state,
			platform.RetainedLocalCopyUsedBytesAnnotation: usedBytes,
		},
	}}
}

func TestStatusListsLocalVolumesLeftOnALonghornCluster(t *testing.T) {
	marked := copiesStatusObject("Service", "shop", "db", 0, "", "", "")
	marked.SetAnnotations(map[string]string{platform.StorageMoveAnnotation: "kipper-storage-move-abc"})
	dyn := copiesStatusClient(
		marked,
		copiesStatusObject("Service", "shop", "cache", 0, "", "", ""),
		copiesStatusObject("Service", "blog", "pg", 0, "", "", ""),
		copiesStatusObject("Service", "blog", "moved", 0, "", "", ""),
	)
	client := fake.NewClientset(
		localClaim("shop", "data-db-0", platform.LocalPathClass),
		localClaim("shop", "data-cache-0", platform.LocalPathClass),
		localClaim("blog", "data-pg-0", platform.LocalPathClass),
		localClaim("blog", "data-moved-0", "longhorn-single"),
		localClaim("blog", "unrelated", platform.LocalPathClass),
		retainedPV("pvc-1", "shop/db", platform.RetainedLocalCopyMoving, "1073741824"),
		retainedPV("pvc-2", "blog/moved", platform.RetainedLocalCopyMoved, "2147483648"),
		retainedPV("pvc-3", "blog/old", platform.RetainedLocalCopyMoved, "1073741824"),
	)

	var out bytes.Buffer
	reportLocalVolumes(context.Background(), &out, client, dyn, time.Now())
	got := out.String()

	assert.Contains(t, got, "Moves to Longhorn not finished:\n    ⚠  shop/db\n")
	assert.Contains(t, got, "'kip service storage longhorn <name>'")
	assert.Contains(t, got, "--rollback")
	assert.Contains(t, got, "Service volumes still on local storage:\n    ·  blog/pg\n    ·  shop/cache\n")
	assert.NotContains(t, got, "·  shop/db\n", "a service in the middle of a move is listed once, as unfinished")
	assert.NotContains(t, got, "unrelated")
	assert.NotContains(t, got, "blog/moved\n    ·")
	assert.Contains(t, got, "Local copies kept after a move: 2 (3.0 GiB)")
	assert.Contains(t, got, "'kip cluster storage prune'")
}

func TestStatusShowsNothingWhenNoLocalVolumesAreLeft(t *testing.T) {
	dyn := copiesStatusClient(copiesStatusObject("Service", "shop", "db", 0, "", "", ""))
	client := fake.NewClientset(localClaim("shop", "data-db-0", "longhorn-single"))

	var out bytes.Buffer
	reportLocalVolumes(context.Background(), &out, client, dyn, time.Now())

	assert.Empty(t, out.String())
}

// movedPV is a moved copy of service whose move finished at movedAt onto the
// claim with UID destination.
func movedPV(name, service, movedAt, destination string) *corev1.PersistentVolume {
	pv := retainedPV(name, service, platform.RetainedLocalCopyMoved, "1073741824")
	pv.Annotations[platform.RetainedLocalCopyMovedAtAnnotation] = movedAt
	pv.Annotations["kipper.run/retained-local-copy-destination-uid"] = destination
	return pv
}

// longhornClaim is a bound Longhorn claim in shop.
func longhornClaim(name, uid string) *corev1.PersistentVolumeClaim {
	c := localClaim("shop", name, "longhorn-single")
	c.UID = types.UID(uid)
	c.Status.Phase = corev1.ClaimBound
	return c
}

// movedServicePod is service's pod in shop on its data claim, Ready since
// since, or not Ready when since is zero.
func movedServicePod(service string, since time.Time) *corev1.Pod {
	status := corev1.ConditionTrue
	if since.IsZero() {
		status = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: service + "-0"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + service + "-0"},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(since)},
		}},
	}
}

func TestStatusSaysWhyEachMovedCopyIsKept(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	backup := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]interface{}{"name": "daily-1", "namespace": "velero", "uid": "b1"},
		"status":   map[string]interface{}{"phase": "Completed", "startTimestamp": "2026-10-25T11:00:00Z", "expiration": "2026-11-24T11:00:00Z"},
	}}
	pvb := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": "PodVolumeBackup",
		"metadata": map[string]interface{}{"name": "daily-1-x", "namespace": "velero", "labels": map[string]interface{}{
			"velero.io/backup-name": "daily-1", "velero.io/backup-uid": "b1", "velero.io/pvc-uid": "c3",
		}},
		"spec":   map[string]interface{}{"pod": map[string]interface{}{"namespace": "shop", "name": "web-0"}, "volume": "data"},
		"status": map[string]interface{}{"phase": "Completed"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		manifest.ServiceGVR: "ServiceList", backupGVR: "BackupList", podVolumeBackupGVR: "PodVolumeBackupList",
	},
		copiesStatusObject("Service", "shop", "db", 0, "", "", ""),
		copiesStatusObject("Service", "shop", "pg", 0, "", "", ""),
		copiesStatusObject("Service", "shop", "web", 0, "", "", ""),
		copiesStatusObject("Service", "shop", "old", 0, "", "", ""),
		copiesStatusObject("Service", "shop", "conv", 2, "False", "Converting", "Longhorn is building a copy"),
		backup, pvb,
	)
	client := fake.NewClientset(
		longhornClaim("data-db-0", "c1"),
		longhornClaim("data-pg-0", "c2"),
		longhornClaim("data-web-0", "c3"),
		longhornClaim("data-old-0", "c4"),
		longhornClaim("data-conv-0", "c6"),
		movedServicePod("db", now.Add(-2*time.Hour)),
		movedServicePod("pg", now.Add(-30*time.Minute)),
		movedServicePod("web", now.Add(-2*time.Hour)),
		movedServicePod("old", now.Add(-2*time.Hour)),
		movedServicePod("conv", now.Add(-2*time.Hour)),
		movedPV("pvc-1", "shop/db", "2026-10-25T11:00:00Z", "c1"),
		movedPV("pvc-2", "shop/pg", "2026-10-25T09:00:00Z", "c2"),
		movedPV("pvc-3", "shop/web", "2026-10-25T09:00:00Z", "c3"),
		movedPV("pvc-4", "shop/old", "2026-10-23T09:00:00Z", "c4"),
		movedPV("pvc-6", "shop/conv", "2026-10-23T09:00:00Z", "c6"),
		movedPV("pvc-5", "shop/gone", "2026-10-25T09:00:00Z", "c5"),
	)

	var out bytes.Buffer
	reportLocalVolumes(context.Background(), &out, client, dyn, now)
	got := out.String()

	assert.Contains(t, got, "Local copies kept after a move: 6 (6.0 GiB)")
	assert.Contains(t, got, "·  shop/db: kept until a backup of db completes or 23h from now\n")
	assert.Contains(t, got, "·  shop/pg: kept: pg has not been Ready on Longhorn for an hour yet\n")
	assert.Contains(t, got, "·  shop/web: can be deleted: backup daily-1 holds its data from Longhorn\n")
	assert.Contains(t, got, "·  shop/old: can be deleted\n")
	assert.Contains(t, got, "·  shop/conv: kept: the copies of conv's data volume are changing (Converting)\n",
		"status agrees with prune while the copies of the new volume change")
	assert.Contains(t, got, "·  shop/gone: can be deleted: its service was deleted\n")
	assert.Contains(t, got, "'kip cluster storage prune' deletes a copy once its service has been Ready on Longhorn for an hour and either a backup made after the move holds its data or a day has passed since the move")
	assert.NotContains(t, got, "run well on Longhorn")
}
