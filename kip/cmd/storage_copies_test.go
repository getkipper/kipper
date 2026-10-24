package cmd

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/kip/internal/service"

	"github.com/getkipper/kipper/kip/internal/manifest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func offeringTwo(nodes int) copiesCluster {
	return copiesCluster{mode: platform.StorageLonghorn, offered: 2, replicaNodes: nodes, backup: platform.BackupExternal}
}

func TestOneCopyIsAlwaysAllowed(t *testing.T) {
	for _, c := range []copiesCluster{
		{mode: platform.StorageLocal, offered: 1, replicaNodes: 0},
		{mode: platform.StorageLonghorn, offered: 1, replicaNodes: unknownReplicaNodes},
	} {
		assert.NoError(t, refuseCopiesOnCreate(1, false, c))
		assert.NoError(t, refuseCopiesOnCreate(1, true, c))
	}
}

func TestCopiesOutsideOneAndTwoAreRefused(t *testing.T) {
	for _, copies := range []int32{0, 3, -1} {
		assert.ErrorContains(t, refuseCopiesOnCreate(copies, false, offeringTwo(2)), "1 or 2")
	}
}

func TestTwoCopiesOnLocalStorageAreRefused(t *testing.T) {
	c := offeringTwo(2)
	c.mode = platform.StorageLocal
	assert.ErrorContains(t, refuseCopiesOnCreate(2, false, c), "local storage")
}

func TestTwoCopiesNeedTheAdminsOffer(t *testing.T) {
	c := offeringTwo(2)
	c.offered = 1
	err := refuseCopiesOnCreate(2, false, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster storage --max-copies 2")
}

func TestAnUnreadableOfferLeavesTheDecisionToTheCluster(t *testing.T) {
	c := offeringTwo(2)
	c.offered = 0
	assert.NoError(t, refuseCopiesOnCreate(2, false, c))
}

func TestTwoCopiesOfASharedVolumeAreRefusedWhileRWXIsNotQualified(t *testing.T) {
	err := refuseCopiesOnCreate(2, true, offeringTwo(3))
	if platform.RWXCopiesQualified {
		assert.NoError(t, err)
		return
	}
	assert.ErrorContains(t, err, "shared volumes cannot have two copies")
}

func TestTwoCopiesNeedTwoReplicaNodes(t *testing.T) {
	err := refuseCopiesOnCreate(2, false, offeringTwo(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has 1")
	assert.Contains(t, err.Error(), "kip node storage <node> --copies")

	assert.NoError(t, refuseCopiesOnCreate(2, false, offeringTwo(2)))
	assert.NoError(t, refuseCopiesOnCreate(2, false, offeringTwo(unknownReplicaNodes)),
		"an unreadable node list leaves the decision to the controller")
}

func TestTwoCopiesWarnOnceAboutInClusterBackups(t *testing.T) {
	c := offeringTwo(2)
	c.backup = platform.BackupInCluster
	var out bytes.Buffer
	warnAboutCopies(&out, 2, c)
	assert.Contains(t, out.String(), "inside the cluster")
	assert.Equal(t, 1, bytes.Count(out.Bytes(), []byte("⚠")))

	out.Reset()
	warnAboutCopies(&out, 1, c)
	warnAboutCopies(&out, 2, offeringTwo(2))
	assert.Empty(t, out.String())
}

func copiesObject(spec, live *int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "db", "namespace": "shop"},
		"spec":       map[string]interface{}{"type": "postgres"},
	}}
	if spec != nil {
		_ = unstructured.SetNestedField(obj.Object, *spec, "spec", "copies")
	}
	if live != nil {
		_ = unstructured.SetNestedField(obj.Object, *live, "status", "storage", "copies")
	}
	return obj
}

func adopt(t *testing.T, obj *unstructured.Unstructured, want int32) (bool, *unstructured.Unstructured, error) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	changed, err := adoptCopies(context.Background(), dyn, manifest.ServiceGVR, "shop", "db", want)
	got, getErr := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, getErr)
	return changed, got, err
}

func TestAdoptCopiesWritesTheLiveCountOnAnUnmanagedObject(t *testing.T) {
	changed, got, err := adopt(t, copiesObject(nil, ptr.To[int64](3)), 3)
	require.Error(t, err, "3 is outside the range kip writes")
	assert.False(t, changed)
	_, written, _ := unstructured.NestedInt64(got.Object, "spec", "copies")
	assert.False(t, written)

	changed, got, err = adopt(t, copiesObject(nil, ptr.To[int64](1)), 1)
	require.NoError(t, err)
	assert.True(t, changed)
	copies, _, _ := unstructured.NestedInt64(got.Object, "spec", "copies")
	assert.EqualValues(t, 1, copies)
}

func TestAdoptCopiesRefusesAValueThatNeedsConversion(t *testing.T) {
	changed, got, err := adopt(t, copiesObject(nil, ptr.To[int64](1)), 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "live conversion")
	assert.False(t, changed)
	_, found, _ := unstructured.NestedInt64(got.Object, "spec", "copies")
	assert.False(t, found, "nothing is written")

	_, _, err = adopt(t, copiesObject(ptr.To[int64](1), ptr.To[int64](1)), 2)
	assert.ErrorContains(t, err, "live conversion")
}

func TestAdoptCopiesWaitsForTheLiveCount(t *testing.T) {
	_, _, err := adopt(t, copiesObject(nil, nil), 1)
	assert.ErrorContains(t, err, "not known yet")
}

func TestAdoptCopiesThatAreAlreadySetChangesNothing(t *testing.T) {
	changed, _, err := adopt(t, copiesObject(ptr.To[int64](2), ptr.To[int64](2)), 2)
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestServiceInfoShowsTheCopiesAndWhatHoldsThemBack(t *testing.T) {
	cr := healthyServiceCR("db", "redis")
	_ = unstructured.SetNestedField(cr.Object, int64(2), "spec", "copies")
	_ = unstructured.SetNestedSlice(cr.Object, []interface{}{map[string]interface{}{
		"type": "StorageCopies", "status": "False", "reason": "NotEnoughReplicaNodes",
		"message": "Two copies need two ready nodes that can hold copies, and the cluster has 1.",
	}}, "status", "conditions")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList", service.LonghornReplicaGVR: "ReplicaList"}, cr)
	client := k8sfake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "default"},
		Data:       map[string][]byte{"HOST": []byte("db.default.svc.cluster.local"), "PORT": []byte("6379")},
	})

	var out bytes.Buffer
	require.NoError(t, writeServiceInfo(context.Background(), &out, &service.Manager{Client: client, Dynamic: dyn}, "default", "db", false))
	assert.Contains(t, out.String(), "Copies:   2, not created yet")
	assert.Contains(t, out.String(), "the cluster has 1.")
}
