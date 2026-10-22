package job

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHistoryListsOneOffRunsOfTheName(t *testing.T) {
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClient(collisionScheme())
	mgr := &Manager{Dynamic: dyn, Client: fake.NewSimpleClientset()}

	runName, err := mgr.Run(ctx, Options{Name: "burst1", Namespace: "shop-prod", Image: "busybox"})
	require.NoError(t, err)

	history, err := mgr.History(ctx, "shop-prod", "burst1")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, runName, history[0].Name)
	assert.Equal(t, "pending", history[0].Status)
}

func TestHistoryReportsTheOutcomeARunRecorded(t *testing.T) {
	ctx := context.Background()
	run := buildJobCR("burst1-20261022-061500", "shop-prod", "", "busybox", nil, "burst1")
	run.Object["status"] = map[string]any{"phase": "Completed", "lastResult": "Succeeded"}
	run.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
	other := buildJobCR("other-20261022-061500", "shop-prod", "", "busybox", nil, "other")
	mgr := &Manager{
		Dynamic: dynamicfake.NewSimpleDynamicClient(collisionScheme(), run, other),
		Client:  fake.NewSimpleClientset(),
	}

	history, err := mgr.History(ctx, "shop-prod", "burst1")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "burst1-20261022-061500", history[0].Name)
	assert.Equal(t, "completed", history[0].Status)
	assert.Equal(t, "1h0m0s ago", history[0].Last)
}

func TestHistoryListsAScheduledJobsRunsOnce(t *testing.T) {
	ctx := context.Background()
	scheduled := buildJobCR("nightly", "shop-prod", "0 2 * * *", "busybox", nil, "nightly")
	run := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "nightly-29000000", Namespace: "shop-prod",
		Labels: map[string]string{"app": "nightly", kipperLabel: kipperValue},
	}}
	mgr := &Manager{
		Dynamic: dynamicfake.NewSimpleDynamicClient(collisionScheme(), scheduled),
		Client:  fake.NewSimpleClientset(run),
	}

	history, err := mgr.History(ctx, "shop-prod", "nightly")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "nightly-29000000", history[0].Name)
}

func TestHistoryListsAConsoleRunOnce(t *testing.T) {
	ctx := context.Background()
	run := buildJobCR("db-migrate", "shop-prod", "", "busybox", nil, "db-migrate")
	run.Object["status"] = map[string]any{"phase": "Running"}
	native := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-migrate", Namespace: "shop-prod",
			Labels: map[string]string{"app": "db-migrate", kipperLabel: kipperValue},
		},
		Status: batchv1.JobStatus{Failed: 1, Active: 1},
	}
	mgr := &Manager{
		Dynamic: dynamicfake.NewSimpleDynamicClient(collisionScheme(), run),
		Client:  fake.NewSimpleClientset(native),
	}

	history, err := mgr.History(ctx, "shop-prod", "db-migrate")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "running", history[0].Status, "a retried attempt is not the run's outcome")
}
