package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func fakeVolumeDynamic(t *testing.T, mounts []interface{}) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "Volume"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "VolumeList"}, &unstructured.UnstructuredList{})
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)

	spec := map[string]interface{}{"size": "5Gi"}
	if mounts != nil {
		spec["mounts"] = mounts
	}
	_, err := dyn.Resource(manifest.VolumeGVR).Namespace("blog-test").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Volume",
		"metadata":   map[string]interface{}{"name": "uploads", "namespace": "blog-test"},
		"spec":       spec,
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return dyn
}

func volumeMounts(t *testing.T, dyn *dynamicfake.FakeDynamicClient) []interface{} {
	t.Helper()
	vol, err := dyn.Resource(manifest.VolumeGVR).Namespace("blog-test").Get(context.Background(), "uploads", metav1.GetOptions{})
	require.NoError(t, err)
	mounts, _, err := unstructured.NestedSlice(vol.Object, "spec", "mounts")
	require.NoError(t, err)
	return mounts
}

// TestMountVolumeWritesVolumeCR pins the regression from the supplemento
// deploy: the mount must land on the Volume CR's spec.mounts, the
// authoritative list the volume reconciler propagates into the App CR and
// on into every future Deployment. The old direct Deployment patch was
// silently dropped on the next image update.
func TestMountVolumeWritesVolumeCR(t *testing.T) {
	dyn := fakeVolumeDynamic(t, nil)

	require.NoError(t, mountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp", "/data/uploads"))

	mounts := volumeMounts(t, dyn)
	require.Len(t, mounts, 1)
	assert.Equal(t, map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"}, mounts[0])
}

func TestMountVolumeIsIdempotentAndUpdatesPath(t *testing.T) {
	dyn := fakeVolumeDynamic(t, []interface{}{
		map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"},
		map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"},
	})

	// Remounting at the same path changes nothing.
	require.NoError(t, mountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp", "/data/uploads"))
	require.Len(t, volumeMounts(t, dyn), 2)

	// Remounting at a new path moves the mount instead of duplicating it.
	require.NoError(t, mountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp", "/srv/uploads"))
	mounts := volumeMounts(t, dyn)
	require.Len(t, mounts, 2)
	assert.Contains(t, mounts, map[string]interface{}{"app": "webapp", "mountPath": "/srv/uploads"})
	assert.Contains(t, mounts, map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"})
}

func TestMountVolumeMissingVolume(t *testing.T) {
	dyn := fakeVolumeDynamic(t, nil)

	err := mountVolume(context.Background(), dyn, "blog-test", "ghost", "webapp", "/data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `volume "ghost" not found`)
}

// TestMountVolumeRetriesOnConflict proves the read-modify-write survives a
// concurrent writer: the first update is rejected with a conflict, forcing
// a re-read that picks up the other writer's mount before re-applying.
func TestMountVolumeRetriesOnConflict(t *testing.T) {
	dyn := fakeVolumeDynamic(t, nil)

	conflicted := false
	dyn.PrependReactor("update", "volumes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		// Simulate a concurrent mount landing between our Get and Update.
		// The tracker is mutated directly — calling the fake client from
		// inside its own reactor would deadlock on the client's lock.
		tracker := dyn.Tracker()
		obj, err := tracker.Get(manifest.VolumeGVR, "blog-test", "uploads")
		if err != nil {
			return true, nil, err
		}
		vol := obj.(*unstructured.Unstructured)
		concurrent := map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"}
		if err := unstructured.SetNestedSlice(vol.Object, []interface{}{concurrent}, "spec", "mounts"); err != nil {
			return true, nil, err
		}
		if err := tracker.Update(manifest.VolumeGVR, vol, "blog-test"); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(manifest.VolumeGVR.GroupResource(), "uploads", nil)
	})

	require.NoError(t, mountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp", "/data/uploads"))
	assert.True(t, conflicted, "the conflict reactor must have fired")

	mounts := volumeMounts(t, dyn)
	require.Len(t, mounts, 2, "the concurrent mount must survive the retry")
	assert.Contains(t, mounts, map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"})
	assert.Contains(t, mounts, map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"})
}

// TestMountVolumeCollapsesDuplicateEntries guards against order-dependent
// remounts: with two stale entries for the same app, the reconciler applies
// the last one, so a remount must leave exactly one entry.
func TestMountVolumeCollapsesDuplicateEntries(t *testing.T) {
	dyn := fakeVolumeDynamic(t, []interface{}{
		map[string]interface{}{"app": "webapp", "mountPath": "/old-a"},
		map[string]interface{}{"app": "webapp", "mountPath": "/old-b"},
	})

	require.NoError(t, mountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp", "/data/uploads"))

	mounts := volumeMounts(t, dyn)
	require.Len(t, mounts, 1)
	assert.Equal(t, map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"}, mounts[0])
}

// TestUnmountVolumeRetriesOnConflict mirrors the mount conflict test: the
// retry re-reads, keeps the concurrently added mount, and removes only the
// requested app.
func TestUnmountVolumeRetriesOnConflict(t *testing.T) {
	dyn := fakeVolumeDynamic(t, []interface{}{
		map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"},
	})

	conflicted := false
	dyn.PrependReactor("update", "volumes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		tracker := dyn.Tracker()
		obj, err := tracker.Get(manifest.VolumeGVR, "blog-test", "uploads")
		if err != nil {
			return true, nil, err
		}
		vol := obj.(*unstructured.Unstructured)
		mounts := []interface{}{
			map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"},
			map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"},
		}
		if err := unstructured.SetNestedSlice(vol.Object, mounts, "spec", "mounts"); err != nil {
			return true, nil, err
		}
		if err := tracker.Update(manifest.VolumeGVR, vol, "blog-test"); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(manifest.VolumeGVR.GroupResource(), "uploads", nil)
	})

	require.NoError(t, unmountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp"))
	assert.True(t, conflicted, "the conflict reactor must have fired")

	mounts := volumeMounts(t, dyn)
	require.Len(t, mounts, 1, "the concurrent mount must survive the retry")
	assert.Equal(t, map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"}, mounts[0])
}

func TestUnmountVolumeRemovesOnlyTheNamedApp(t *testing.T) {
	dyn := fakeVolumeDynamic(t, []interface{}{
		map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"},
		map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"},
	})

	require.NoError(t, unmountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp"))

	mounts := volumeMounts(t, dyn)
	require.Len(t, mounts, 1)
	assert.Equal(t, map[string]interface{}{"app": "image-processor", "mountPath": "/data/input"}, mounts[0])
}

func TestUnmountLastMountClearsTheField(t *testing.T) {
	dyn := fakeVolumeDynamic(t, []interface{}{
		map[string]interface{}{"app": "webapp", "mountPath": "/data/uploads"},
	})

	require.NoError(t, unmountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp"))

	vol, err := dyn.Resource(manifest.VolumeGVR).Namespace("blog-test").Get(context.Background(), "uploads", metav1.GetOptions{})
	require.NoError(t, err)
	_, found, err := unstructured.NestedSlice(vol.Object, "spec", "mounts")
	require.NoError(t, err)
	assert.False(t, found, "an empty mount list should remove spec.mounts entirely")
}

func TestUnmountVolumeNotMounted(t *testing.T) {
	dyn := fakeVolumeDynamic(t, nil)

	err := unmountVolume(context.Background(), dyn, "blog-test", "uploads", "webapp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `volume "uploads" is not mounted`)
}
