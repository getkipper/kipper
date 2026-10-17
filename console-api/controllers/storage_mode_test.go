package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func localPlatformConfig() *kipperv1.PlatformConfig {
	return &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileNano, Storage: platform.StorageLocal},
	}
}

func TestClusterStorageMode(t *testing.T) {
	ctx := context.Background()
	none := crfake.NewClientBuilder().WithScheme(testScheme()).Build()
	mode, err := clusterStorageMode(ctx, none)
	require.NoError(t, err)
	assert.Equal(t, platform.StorageLonghorn, mode, "a cluster without a PlatformConfig predates local storage")

	legacy := localPlatformConfig()
	legacy.Spec.Storage = ""
	mode, err = clusterStorageMode(ctx, crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(legacy).Build())
	require.NoError(t, err)
	assert.Equal(t, platform.StorageLonghorn, mode)

	mode, err = clusterStorageMode(ctx, crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(localPlatformConfig()).Build())
	require.NoError(t, err)
	assert.Equal(t, platform.StorageLocal, mode)
}
