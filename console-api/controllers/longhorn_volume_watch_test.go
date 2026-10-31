package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Size and timestamp updates are left to polling so routine volume activity
// does not trigger reconciliation.
func TestALonghornVolumeChangeWakesOnlyOnWhatTheControllersRead(t *testing.T) {
	base := longhornVolume("pvc-1", "ns", "data-db-0", 1)
	_ = unstructured.SetNestedField(base.Object, "attached", "status", "state")
	_ = unstructured.SetNestedField(base.Object, "healthy", "status", "robustness")
	_ = unstructured.SetNestedField(base.Object, int64(1<<30), "status", "actualSize")

	changed := func(edit func(*unstructured.Unstructured)) bool {
		next := base.DeepCopy()
		edit(next)
		return longhornVolumeChanged.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: next})
	}
	assert.False(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, int64(2<<30), "status", "actualSize")
		_ = unstructured.SetNestedField(v.Object, "2026-10-31T05:00:00Z", "status", "lastBackupAt")
	}), "size and timestamps")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, "faulted", "status", "robustness")
	}), "robustness")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, "detached", "status", "state")
	}), "state")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, int64(2), "spec", "numberOfReplicas")
	}), "copy count")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, "worker-1", "status", "currentNodeID")
	}), "attachment node")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedSlice(v.Object, []interface{}{map[string]interface{}{"type": "Scheduled", "status": "False"}}, "status", "conditions")
	}), "conditions")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, "data-db-1", "status", "kubernetesStatus", "pvcName")
	}), "claim")
	assert.True(t, changed(func(v *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(v.Object, "longhornio/longhorn-engine:v1.11.3", "status", "currentImage")
	}), "engine image")

	scheduled := base.DeepCopy()
	_ = unstructured.SetNestedSlice(scheduled.Object, []interface{}{map[string]interface{}{
		"type": "Scheduled", "status": "True", "reason": "", "message": "", "lastTransitionTime": "2026-10-31T04:00:00Z"}}, "status", "conditions")
	reworded := scheduled.DeepCopy()
	_ = unstructured.SetNestedSlice(reworded.Object, []interface{}{map[string]interface{}{
		"type": "Scheduled", "status": "True", "reason": "", "message": "replica scheduled", "lastTransitionTime": "2026-10-31T05:00:00Z"}}, "status", "conditions")
	assert.False(t, longhornVolumeChanged.Update(event.UpdateEvent{ObjectOld: scheduled, ObjectNew: reworded}), "a condition's message and time")
}

func TestALonghornVolumeWakesTheServiceThatOwnsItsClaim(t *testing.T) {
	for claim, want := range map[string][]reconcile.Request{
		"data-db-0":           {{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"}}},
		"db-dump":             {{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "db"}}},
		"shared-files":        nil,
		"data-db-1":           nil,
		"data-0":              nil,
		"shared-files-dump":   {{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "shared-files"}}},
		"data-warehouse-dump": {{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "data-warehouse"}}},
		"data-warehouse-0":    {{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "warehouse"}}},
		"":                    nil,
	} {
		got := serviceForLonghornVolume(context.Background(), longhornVolume("pvc-1", "ns", claim, 1))
		assert.Equal(t, want, got, claim)
	}
}

func TestALonghornVolumeWakesTheVolumeThatOwnsItsClaim(t *testing.T) {
	got := volumeForLonghornVolume(context.Background(), longhornVolume("pvc-1", "ns", "shared-files", 1))
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "files"}}}, got)
	assert.Nil(t, volumeForLonghornVolume(context.Background(), longhornVolume("pvc-1", "ns", "data-db-0", 1)))
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "uploads"}}},
		volumeForLonghornVolume(context.Background(), longhornVolume("pvc-1", "ns", "shared-uploads", 1)),
		"a Volume named uploads has the claim a project's shared uploads have")
}

func TestALonghornVolumeWakesTheProjectThatOwnsItsSharedUploads(t *testing.T) {
	owned := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod", UID: "ns-1", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-prod", UID: "ns-1"}}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(owned, shop).Build()
	mapper := projectForLonghornVolume(c)

	got := mapper(context.Background(), longhornVolume("pvc-1", "shop-prod", "shared-uploads", 1))
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "shop"}}}, got)
	assert.Nil(t, mapper(context.Background(), longhornVolume("pvc-1", "shop-prod", "data-db-0", 1)))
	assert.Nil(t, mapper(context.Background(), longhornVolume("pvc-1", "elsewhere", "shared-uploads", 1)), "a namespace that is gone")
}

// With the watch registered, a fault reaches the service as a volume event,
// so the dump check adds no timer of its own.
func TestAWatchedDumpVolumeNeedsNoTimer(t *testing.T) {
	for _, robustness := range []string{"faulted", "healthy"} {
		svc := faultService()
		r := copiesReconciler(svc, longhornPV(), dumpClaim(svc.UID), dumpLonghornVolume(robustness, "db-dump"))
		r.watchesLonghornVolumes = true

		retry, err := r.reportDumpVolume(context.Background(), svc)
		require.NoError(t, err)
		assert.Zero(t, retry, robustness)
	}
}
