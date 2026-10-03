package deployer

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
)

func liveStop(t *testing.T, dynClient *dynamicfake.FakeDynamicClient) map[string]interface{} {
	t.Helper()
	app, err := dynClient.Resource(AppGVR).Namespace("default").Get(context.Background(), "api", metav1.GetOptions{})
	require.NoError(t, err)
	stopped, _, _ := unstructured.NestedMap(app.Object, "spec", "stopped")
	return stopped
}

func TestStop(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

	t.Run("records the reason, who and when", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2)})

		res, err := d.Stop(context.Background(), "default", "api", StopOptions{Reason: strPtr("freeing memory"), By: "kip (alice)", At: at})

		require.NoError(t, err)
		assert.False(t, res.Already)
		assert.Equal(t, map[string]interface{}{"reason": "freeing memory", "by": "kip (alice)", "at": "2026-10-03T09:00:00Z"}, liveStop(t, dynClient))
	})

	t.Run("a stop for a migration says so", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1"})

		_, err := d.Stop(context.Background(), "default", "api", StopOptions{By: "kip", At: at, ForMigration: true})

		require.NoError(t, err)
		assert.Equal(t, true, liveStop(t, dynClient)["forMigration"])
	})

	t.Run("stopping again changes only the reason", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1",
			"stopped": map[string]interface{}{"reason": "old", "by": "bob@example.com", "at": "2026-10-01T08:00:00Z"}})

		res, err := d.Stop(context.Background(), "default", "api", StopOptions{Reason: strPtr("new"), By: "kip (alice)", At: at})

		require.NoError(t, err)
		assert.True(t, res.Already)
		assert.Equal(t, map[string]interface{}{"reason": "new", "by": "bob@example.com", "at": "2026-10-01T08:00:00Z"}, liveStop(t, dynClient))
	})

	t.Run("the migration freeze on an operator's stop keeps it an operator's stop", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1",
			"stopped": map[string]interface{}{"reason": "parked", "by": "bob@example.com", "at": "2026-10-01T08:00:00Z"}})

		res, err := d.Stop(context.Background(), "default", "api", StopOptions{By: "kip (alice)", At: at, ForMigration: true})

		require.NoError(t, err)
		assert.True(t, res.Already)
		assert.False(t, res.Freeze, "the operator's stop is what is stored")
		assert.Equal(t, map[string]interface{}{"reason": "parked", "by": "bob@example.com", "at": "2026-10-01T08:00:00Z"}, liveStop(t, dynClient),
			"a migration carries an operator's stop, so marking it as the freeze would start the app on the target")
	})

	t.Run("repeating the migration freeze reports a freeze", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "stopped": map[string]interface{}{"forMigration": true}})

		res, err := d.Stop(context.Background(), "default", "api", StopOptions{By: "kip", At: at, ForMigration: true})

		require.NoError(t, err)
		assert.True(t, res.Already)
		assert.True(t, res.Freeze)
	})

	t.Run("stopping again without a reason keeps the one there", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "stopped": map[string]interface{}{"reason": "parked"}})

		_, err := d.Stop(context.Background(), "default", "api", StopOptions{By: "kip", At: at})

		require.NoError(t, err)
		assert.Equal(t, "parked", liveStop(t, dynClient)["reason"])
	})

	t.Run("a cluster that drops the field is reported, not trusted", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1"})
		dynClient.PrependReactor("update", "apps", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if u, ok := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured); ok {
				unstructured.RemoveNestedField(u.Object, "spec", "stopped")
			}
			return false, nil, nil
		})

		_, err := d.Stop(context.Background(), "default", "api", StopOptions{By: "kip", At: at})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "kip upgrade")
	})

	t.Run("an unknown app", func(t *testing.T) {
		d, _ := testDeployer()
		_, err := d.Stop(context.Background(), "default", "ghost", StopOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestStart(t *testing.T) {
	t.Run("removes the stop", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2), "stopped": map[string]interface{}{"reason": "x"}})

		res, err := d.Start(context.Background(), "default", "api")

		require.NoError(t, err)
		assert.True(t, res.WasStopped)
		assert.False(t, res.RunsNoPods)
		assert.Nil(t, liveStop(t, dynClient))
	})

	t.Run("a running app is left as it is", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(2)})

		res, err := d.Start(context.Background(), "default", "api")

		require.NoError(t, err)
		assert.False(t, res.WasStopped)
	})

	t.Run("an app scaled to zero is not stopped, and start does not guess a count", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(0)})

		_, err := d.Start(context.Background(), "default", "api")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "kip app scale api --replicas")
	})

	t.Run("a stopped app at zero replicas runs no pods after the start", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(0), "stopped": map[string]interface{}{}})

		res, err := d.Start(context.Background(), "default", "api")

		require.NoError(t, err)
		assert.True(t, res.RunsNoPods)
	})

	t.Run("an autoscaled app at zero replicas starts at its minimum", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "replicas": int64(0), "stopped": map[string]interface{}{},
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(4)}})

		res, err := d.Start(context.Background(), "default", "api")

		require.NoError(t, err)
		assert.False(t, res.RunsNoPods)
	})
}

func TestRestartRefusesAStoppedApp(t *testing.T) {
	d, dynClient := testDeployer()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "stopped": map[string]interface{}{}})

	err := d.Restart(context.Background(), "default", "api")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip app start api")
}

func TestStopped(t *testing.T) {
	d, dynClient := testDeployer()
	seedApp(t, dynClient, map[string]interface{}{"image": "web:1", "stopped": map[string]interface{}{}})

	stopped, err := d.Stopped(context.Background(), "default", "api")

	require.NoError(t, err)
	assert.True(t, stopped)
}

func TestAppStatusCarriesTheStop(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "web"},
		"spec": map[string]interface{}{"image": "web:1", "replicas": int64(3),
			"stopped": map[string]interface{}{"reason": "freeing memory", "by": "kip (alice)", "at": "2026-10-03T09:00:00Z"}},
		"status": map[string]interface{}{"phase": "Stopped", "replicas": int64(0)},
	}}

	st := appStatusFromCR(cr)

	assert.Equal(t, "stopped", st.Status)
	assert.Equal(t, int32(3), st.Replicas, "the count it starts with")
	require.NotNil(t, st.Stopped)
	assert.Equal(t, StoppedInfo{Reason: "freeing memory", By: "kip (alice)", At: "2026-10-03T09:00:00Z"}, *st.Stopped)
}

func TestAppStatusOfAStoppedAppNotYetReconciled(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "web"},
		"spec":     map[string]interface{}{"image": "web:1", "stopped": map[string]interface{}{}},
	}}

	assert.Equal(t, "stopped", appStatusFromCR(cr).Status)
}

func strPtr(s string) *string { return &s }
