package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileStatefulSet_MinIOImage(t *testing.T) {
	for _, tc := range []struct {
		name, version, want string
	}{
		{"the default release", "", "ghcr.io/getkipper/minio:RELEASE.2025-09-07T16-13-09Z"},
		{"pinned to the release Kipper builds", "RELEASE.2025-09-07T16-13-09Z", "ghcr.io/getkipper/minio:RELEASE.2025-09-07T16-13-09Z"},
		{"pinned to another release", "RELEASE.2024-06-13T22-53-53Z", "minio/minio:RELEASE.2024-06-13T22-53-53Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := bareService("minio")
			svc.Spec.Version = tc.version
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
				Scheme: testScheme(),
			}
			require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

			var sts appsv1.StatefulSet
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
			assert.Equal(t, tc.want, sts.Spec.Template.Spec.Containers[0].Image)
		})
	}
}

// Model an upgrade from upstream images: migrate the release Kipper builds
// while preserving other version pins and the data mount.
func TestReconcileStatefulSet_MinIOImageOnAnExistingWorkload(t *testing.T) {
	for _, tc := range []struct {
		name, version, live, want string
	}{
		{"the default release moves to Kipper's registry", "", "minio/minio:RELEASE.2025-09-07T16-13-09Z", "ghcr.io/getkipper/minio:RELEASE.2025-09-07T16-13-09Z"},
		{"another release keeps its reference", "RELEASE.2024-06-13T22-53-53Z", "minio/minio:RELEASE.2024-06-13T22-53-53Z", "minio/minio:RELEASE.2024-06-13T22-53-53Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := bareService("minio")
			svc.Spec.Version = tc.version
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
				Scheme: testScheme(),
			}
			ctx := context.Background()
			key := types.NamespacedName{Namespace: "ns", Name: "db"}
			require.NoError(t, r.reconcileStatefulSet(ctx, svc))

			var sts appsv1.StatefulSet
			require.NoError(t, r.Get(ctx, key, &sts))
			mounts := sts.Spec.Template.Spec.Containers[0].VolumeMounts
			sts.Spec.Template.Spec.Containers[0].Image = tc.live
			require.NoError(t, r.Update(ctx, &sts))

			require.NoError(t, r.reconcileStatefulSet(ctx, svc))
			require.NoError(t, r.Get(ctx, key, &sts))
			assert.Equal(t, tc.want, sts.Spec.Template.Spec.Containers[0].Image)
			assert.Equal(t, mounts, sts.Spec.Template.Spec.Containers[0].VolumeMounts)
		})
	}
}
