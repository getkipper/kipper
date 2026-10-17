package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestVolumeReconciler_SyncsAppVolumes(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "uploads",
			Namespace: "project-test",
		},
		Spec: kipperv1.VolumeSpec{
			Size: "5Gi",
			Mounts: []kipperv1.VolumeMountTarget{
				{App: "my-app", MountPath: "/data/uploads"},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, vol).
		WithStatusSubresource(vol).
		Build()

	reconciler := &VolumeReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// App CR should now have the volume in Spec.Volumes
	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	require.Len(t, updated.Spec.Volumes, 1)
	assert.Equal(t, "uploads", updated.Spec.Volumes[0].Name)
	assert.Equal(t, "/data/uploads", updated.Spec.Volumes[0].MountPath)
}

func TestVolumeReconciler_RemovesAppVolumesOnUnmount(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "uploads", MountPath: "/data/uploads"},
	}

	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "uploads",
			Namespace: "project-test",
		},
		Spec: kipperv1.VolumeSpec{
			Size:   "5Gi",
			Mounts: []kipperv1.VolumeMountTarget{}, // unmounted
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, vol).
		WithStatusSubresource(vol).
		Build()

	reconciler := &VolumeReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	assert.Empty(t, updated.Spec.Volumes)
}

func TestVolumeReconciler_SkipsAlreadySyncedApp(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "uploads", MountPath: "/data/uploads"},
	}

	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "uploads",
			Namespace: "project-test",
		},
		Spec: kipperv1.VolumeSpec{
			Size: "5Gi",
			Mounts: []kipperv1.VolumeMountTarget{
				{App: "my-app", MountPath: "/data/uploads"},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, vol).
		WithStatusSubresource(vol).
		Build()

	reconciler := &VolumeReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	// Should still have exactly one volume, not duplicated
	assert.Len(t, updated.Spec.Volumes, 1)
}

// TestVolumeReconciler_UpdatesChangedMountPath covers a remount at a new
// path: the App CR entry must follow the Volume CR's spec.mounts instead of
// keeping the stale path forever.
func TestVolumeReconciler_UpdatesChangedMountPath(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "uploads", MountPath: "/data/uploads"},
	}

	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "uploads",
			Namespace: "project-test",
		},
		Spec: kipperv1.VolumeSpec{
			Size: "5Gi",
			Mounts: []kipperv1.VolumeMountTarget{
				{App: "my-app", MountPath: "/srv/uploads"},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, vol).
		WithStatusSubresource(vol).
		Build()

	reconciler := &VolumeReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	require.Len(t, updated.Spec.Volumes, 1)
	assert.Equal(t, "/srv/uploads", updated.Spec.Volumes[0].MountPath)
}

// TestVolumeReconciler_DeletionDetachesApps pins the delete-cascade safety:
// while the finalizer runs, every App still holding the volume must lose the
// entry, or pods keep using the PVC and pvc-protection leaves the claim (and
// its data) in Terminating forever.
func TestVolumeReconciler_DeletionDetachesApps(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "uploads", MountPath: "/data/uploads"},
	}

	now := metav1.Now()
	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "uploads",
			Namespace:         "project-test",
			DeletionTimestamp: &now,
			Finalizers:        []string{volumeFinalizer},
		},
		Spec: kipperv1.VolumeSpec{
			Size: "5Gi",
			Mounts: []kipperv1.VolumeMountTarget{
				{App: "my-app", MountPath: "/data/uploads"},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, vol).
		WithStatusSubresource(vol).
		Build()

	reconciler := &VolumeReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)
	assert.Empty(t, updated.Spec.Volumes, "deletion must detach the volume from every app")

	// With the finalizer released, the fake client completes the deletion.
	var gone kipperv1.Volume
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "uploads"}, &gone)
	assert.True(t, apierrors.IsNotFound(err), "the volume should be gone once the finalizer is released")
}

// A local-storage cluster has no ReadWriteMany class, so a shared volume gets
// no claim, is mounted into no app, and says why.
func TestVolumeReconciler_RefusesASharedVolumeOnLocalStorage(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "project-test"},
		Spec: kipperv1.VolumeSpec{
			Size:   "5Gi",
			Mounts: []kipperv1.VolumeMountTarget{{App: "my-app", MountPath: "/data/uploads"}},
		},
	}
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, vol, localPlatformConfig()).WithStatusSubresource(vol).Build()
	reconciler := &VolumeReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var pvc corev1.PersistentVolumeClaim
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "shared-uploads"}, &pvc)
	assert.True(t, apierrors.IsNotFound(err), "no ReadWriteMany claim is created on local storage")

	var updatedApp kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updatedApp))
	assert.Empty(t, updatedApp.Spec.Volumes, "an app is not given a volume that has no claim")

	var updated kipperv1.Volume
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "uploads"}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionSharedStorageUnsupported)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, platform.SharedVolumesUnsupported, cond.Message)
}
