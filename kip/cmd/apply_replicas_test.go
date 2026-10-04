package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func boundedSpec() map[string]interface{} {
	return map[string]interface{}{
		"image": "nginx",
		"autoscale": map[string]interface{}{
			"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70),
		},
	}
}

func seedReplicas(t *testing.T, dyn *dynamicfake.FakeDynamicClient, replicas int64) {
	t.Helper()
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "api", "namespace": "default"},
		"spec":     map[string]interface{}{"image": "nginx", "replicas": replicas},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func liveReplicasOf(t *testing.T, dyn *dynamicfake.FakeDynamicClient) interface{} {
	t.Helper()
	got, err := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	return got.Object["spec"].(map[string]interface{})["replicas"]
}

func TestApply_BoundsWithoutReplicasKeepAStoredCountWithinThem(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedReplicas(t, dyn, 3)
	res := appResource("api", boundedSpec())

	changes, err := scanChanges(context.Background(), dyn, "default", []manifest.Resource{res})
	require.NoError(t, err)
	for _, c := range changes {
		assert.NotEqual(t, "replicas", c.change.Path, "the diff must not offer to change a count the manifest does not declare")
	}

	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.NoError(t, err, "an omitted count is not a field the apply takes away")
	assert.Equal(t, int64(3), liveReplicasOf(t, dyn))
}

func TestApply_BoundsWithoutReplicasUseTheMinimumForAStoredCountOutsideThem(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedReplicas(t, dyn, 8)

	_, err := applyResource(context.Background(), dyn, "default", appResource("api", boundedSpec()), false, nil, false)

	require.NoError(t, err)
	assert.Equal(t, int64(2), liveReplicasOf(t, dyn))
}

func TestApply_BoundsWithoutReplicasCreateAtTheMinimum(t *testing.T) {
	dyn := fakeWorkloadDynamic()

	_, err := applyResource(context.Background(), dyn, "default", appResource("api", boundedSpec()), false, nil, false)

	require.NoError(t, err)
	assert.Equal(t, int64(2), liveReplicasOf(t, dyn))
}

func TestApply_AnExplicitCountIsKeptAsDeclared(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedReplicas(t, dyn, 3)
	spec := boundedSpec()
	spec["replicas"] = int64(4)

	_, err := applyResource(context.Background(), dyn, "default", appResource("api", spec), false, nil, false)

	require.NoError(t, err)
	assert.Equal(t, int64(4), liveReplicasOf(t, dyn))
}

func TestApply_WithoutBoundsAnOmittedCountIsStillRefused(t *testing.T) {
	for _, spec := range []map[string]interface{}{
		{"image": "nginx"},
		{"image": "nginx", "autoscale": map[string]interface{}{"enabled": false}},
	} {
		dyn := fakeWorkloadDynamic()
		seedReplicas(t, dyn, 3)

		_, err := applyResource(context.Background(), dyn, "default", appResource("api", spec), false, nil, false)

		require.Error(t, err, "without bounds an omitted count takes the stored one away, as before")
		assert.Equal(t, int64(3), liveReplicasOf(t, dyn))
	}
}
