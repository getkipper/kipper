package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestReconcileStatefulSet_StorageClassFollowsTheClusterMode(t *testing.T) {
	for name, tc := range map[string]struct {
		extra []crclient.Object
		want  string
	}{
		"longhorn cluster": {nil, platform.LonghornSingleClass},
		"local cluster":    {[]crclient.Object{localPlatformConfig()}, platform.LocalPathClass},
	} {
		t.Run(name, func(t *testing.T) {
			svc := bareService("postgres")
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(append(tc.extra, svc)...).Build(),
				Scheme: testScheme(),
			}
			require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

			var sts appsv1.StatefulSet
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
			require.Len(t, sts.Spec.VolumeClaimTemplates, 1)
			assert.Equal(t, tc.want, *sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName)
		})
	}
}
