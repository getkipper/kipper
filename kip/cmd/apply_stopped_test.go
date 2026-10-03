package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func seedStoppedApp(t *testing.T, dyn *dynamicfake.FakeDynamicClient, stopped map[string]interface{}) {
	t.Helper()
	_, err := dyn.Resource(deployer.AppGVR).Namespace("default").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "App",
		"metadata": map[string]interface{}{"name": "api", "namespace": "default"},
		"spec":     map[string]interface{}{"image": "nginx", "stopped": stopped},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func liveStopOf(t *testing.T, dyn *dynamicfake.FakeDynamicClient) map[string]interface{} {
	t.Helper()
	got, err := dyn.Resource(deployer.AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	stopped, _, _ := unstructured.NestedMap(got.Object, "spec", "stopped")
	return stopped
}

// An apply never starts an app silently: a manifest without the stop is a
// clear, refused without --force, even when the stop records nothing.
func TestApply_ManifestWithoutTheStopDoesNotStartTheApp(t *testing.T) {
	for _, stopped := range []map[string]interface{}{
		{"reason": "x", "by": "alice@example.com", "at": "2026-10-03T09:00:00Z"},
		{},
	} {
		dyn := fakeWorkloadDynamic()
		seedStoppedApp(t, dyn, stopped)

		_, err := applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx"}), false, nil, false)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "stopped")
		assert.NotNil(t, liveStopOf(t, dyn), "the app was started")

		_, err = applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx"}), true, nil, false)
		require.NoError(t, err)
		assert.Nil(t, liveStopOf(t, dyn), "--force starts it")
	}
}

func TestApply_KeepsWhoStoppedTheAppAndWhen(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	seedStoppedApp(t, dyn, map[string]interface{}{"reason": "old", "by": "alice@example.com", "at": "2026-10-01T08:00:00Z"})
	res := appResource("api", map[string]interface{}{"image": "nginx", "stopped": map[string]interface{}{"reason": "new"}})
	stampStops([]manifest.Resource{res}, "kip apply (bob)", time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))

	changes, err := scanChanges(context.Background(), dyn, "default", []manifest.Resource{res})
	require.NoError(t, err)
	for _, c := range changes {
		assert.NotContains(t, []string{"stopped.by", "stopped.at"}, c.change.Path, "the diff must not offer to rewrite who stopped the app")
	}

	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"reason": "new", "by": "alice@example.com", "at": "2026-10-01T08:00:00Z"}, liveStopOf(t, dyn))
}

func TestApply_AManifestStopsARunningApp(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	_, err := applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx"}), false, nil, false)
	require.NoError(t, err)
	res := appResource("api", map[string]interface{}{"image": "nginx", "stopped": map[string]interface{}{"reason": "parked"}})
	stampStops([]manifest.Resource{res}, "kip apply (bob)", time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))

	_, err = applyResource(context.Background(), dyn, "default", res, false, nil, false)

	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"reason": "parked", "by": "kip apply (bob)", "at": "2026-10-03T09:00:00Z"}, liveStopOf(t, dyn))
}

func TestStampStopsLeavesAppsWithoutAStopAlone(t *testing.T) {
	res := appResource("api", map[string]interface{}{"image": "nginx"})
	stampStops([]manifest.Resource{res}, "kip apply (bob)", time.Now())
	_, found, _ := unstructured.NestedMap(res.Object.Object, "spec", "stopped")
	assert.False(t, found)
}

// A cluster whose App schema predates stopping drops the block, and the app
// runs. The apply has to say so rather than report success.
func TestApply_ACLusterThatDropsTheStopIsReported(t *testing.T) {
	for _, existing := range []bool{false, true} {
		dyn := fakeWorkloadDynamic()
		if existing {
			_, err := applyResource(context.Background(), dyn, "default", appResource("api", map[string]interface{}{"image": "nginx"}), false, nil, false)
			require.NoError(t, err)
		}
		dyn.PrependReactor("*", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if write, ok := action.(interface{ GetObject() runtime.Object }); ok {
				if u, ok := write.GetObject().(*unstructured.Unstructured); ok {
					unstructured.RemoveNestedField(u.Object, "spec", "stopped")
				}
			}
			return false, nil, nil
		})

		_, err := applyResource(context.Background(), dyn, "default",
			appResource("api", map[string]interface{}{"image": "nginx", "stopped": map[string]interface{}{"reason": "x"}}), false, nil, false)

		require.Error(t, err, "existing=%v", existing)
		assert.Contains(t, err.Error(), "kip upgrade")
	}
}
